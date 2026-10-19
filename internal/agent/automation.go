package agent

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/BlackVS/aicrew/internal/automation"
	"github.com/BlackVS/aicrew/internal/forge"
	"github.com/BlackVS/aicrew/internal/toolchain"
)

// Workflow scripts (docs/DESIGN-TURNS.md, 10.3; increment 2b). The launcher
// runs a script in a child process, `aicrew-agent automation run`, under an
// OS memory limit, since the interpreter has none. The child holds no
// credential: its environment is empty, its arguments are the command's
// name, and its standard input carries the script, the event, the allow
// list, the limits and a token that names this one run. Every host call
// comes back over the home's step socket as an "automation" call with that
// token, and the launcher serves it: pr() from the forge with the home's
// credential, attempt() from the session status, run() in the attempt's
// worktree under the turn's allow list, step() into the home's log, and
// comment() on the attempt's pull request. A token is valid only while its
// run lives.

// AutomationLogFile is the home file, relative to the home, that records
// each script run's steps and its outcome, one JSON line each, redacted.
const AutomationLogFile = "logs/automations.jsonl"

// AutomationMemory is the memory limit of a script's process: the child's
// committed memory on Windows (a job object), its data segment beyond what
// it held at its start on Linux (RLIMIT_DATA), and its Go runtime's memory
// on macOS and the other platforms (a watchdog, with RLIMIT_DATA set as
// well; see memlimit_other.go).
const AutomationMemory = 512 << 20

// automationGrace is how long past the script's own wall-clock limit the
// launcher waits for its process before killing it.
const automationGrace = 15 * time.Second

// maxAutomationAnswer bounds an answer the child reads on the socket: a
// pr() result holds reviews' bodies and log excerpts, each bounded.
const maxAutomationAnswer = 2 << 20

// maxAutomationIO bounds what the launcher reads of the child's output and
// of a run() command's output before it is bounded as text.
const (
	maxChildOutput   = 256 << 10
	maxChildStderr   = 8 << 10
	maxCommandOutput = 4 * automation.MaxRunOutput
)

// The kinds of the steps the launcher records for a run itself.
const (
	// stepAutomationFailed is a run that failed: the script's own fail(),
	// a limit, a refused command, a host error, or its process ending.
	stepAutomationFailed = "automation_failed"
)

// AutomationRun is one script to run for an attempt.
type AutomationRun struct {
	AttemptID string
	// Pull is the attempt's pull request on the forge of its worktree's
	// origin; 0 when it has none yet, and pr() and comment() then fail.
	Pull   int64
	Name   string
	Source []byte
	Event  automation.Event
	// Point is the extension point the run is at, when it is (dispatch.go);
	// recorded with the run.
	Point string
	// Memory is the process's memory limit; 0 is AutomationMemory.
	Memory int64
	// Limits are the interpreter's; zero values are 10.3's.
	Limits automation.Limits
}

// AutomationResult is how a run ended. A run either handled the event,
// asked for a turn, or failed; a failure is recorded as a step and is
// never retried here.
type AutomationResult struct {
	RunID string `json:"run_id"`
	// Turn is the turn the script asked for, or nil.
	Turn *automation.Turn `json:"turn,omitempty"`
	// Failed is why the run failed, redacted; "" when it ended normally.
	Failed string `json:"failed,omitempty"`
	// Invalid says the script's text is at fault (automation.ErrInvalid).
	Invalid bool `json:"invalid,omitempty"`
	// Memory says the process passed its memory limit.
	Memory bool `json:"memory,omitempty"`
	// Steps are the steps the run recorded, each bounded, for the report
	// to aicrewd (dispatch.go).
	Steps []automationStep `json:"steps,omitempty"`
}

// automationRequest is what the launcher writes to the child's standard
// input.
type automationRequest struct {
	Version int              `json:"version"`
	Socket  string           `json:"socket"`
	Token   string           `json:"token"`
	Name    string           `json:"name"`
	Source  string           `json:"source"`
	Event   automation.Event `json:"event"`
	Allow   []string         `json:"allow"`
	Deny    []string         `json:"deny"`
	Memory  int64            `json:"memory"`
	Steps   uint64           `json:"steps,omitempty"`
	Wall    time.Duration    `json:"wall,omitempty"`
}

// automationReply is what the child writes to its standard output.
type automationReply struct {
	Turn    *automation.Turn `json:"turn,omitempty"`
	Error   string           `json:"error,omitempty"`
	Invalid bool             `json:"invalid,omitempty"`
}

// automationCall is one host call on the socket: the run's token and the
// call with its arguments.
type automationCall struct {
	Token string   `json:"token"`
	Call  string   `json:"call"`
	Cmd   string   `json:"cmd,omitempty"`
	Args  []string `json:"args,omitempty"`
	Kind  string   `json:"kind,omitempty"`
	Text  string   `json:"text,omitempty"`
	// label's lists, and submit's result reference.
	Add    []string `json:"add,omitempty"`
	Remove []string `json:"remove,omitempty"`
	URL    string   `json:"url,omitempty"`
}

// The kinds of the steps the launcher records for a script's transitions
// (increment 2d-c), beside the script's own.
const (
	stepHostLabel  = "host_label"
	stepHostReady  = "host_ready"
	stepHostSubmit = "host_submit"
	stepHostRerun  = "host_rerun"
	// stepHostUpdateBase is update_base()'s outcome (increment 2e).
	stepHostUpdateBase = "host_update_base"
)

// liveRun is a run the launcher serves host calls for.
type liveRun struct {
	ctx      context.Context
	id       string
	attempt  string
	pull     int64
	script   string
	point    string
	worktree string
	allow    automation.AllowList
	// steps are the steps recorded so far, bounded, for the run's report;
	// submit is the result reference submit() asked for, sent once the
	// script ends without failing (submitResult).
	stepsMu sync.Mutex
	steps   []automationStep
	submit  string
}

// AutomationForge is what a script's host asks of the forge;
// forge.Client is the real one.
type AutomationForge interface {
	GetPullContent(ctx context.Context, host string, k forge.Kind, token, path string, n int64) (forge.Pull, forge.PullContent, error)
	CommentPull(ctx context.Context, host string, k forge.Kind, token, path string, n int64, body string) (forge.Comment, error)
	AddLabels(ctx context.Context, host string, k forge.Kind, token, path string, n int64, labels []string) error
	RemoveLabels(ctx context.Context, host string, k forge.Kind, token, path string, n int64, labels []string) error
	ReadyPull(ctx context.Context, host string, k forge.Kind, token, path string, n int64) (forge.Pull, error)
	RerunFailed(ctx context.Context, host string, k forge.Kind, token, path string, checks []forge.Check) ([]string, error)
}

// newAutomationForge is the forge a script's host uses; tests replace it.
var newAutomationForge = func() AutomationForge { return forge.NewClient() }

// RunAutomation runs one script for an attempt in a child process and
// serves its host calls until it ends. It returns an error only when the
// run could not start; a script that fails is a result, recorded as a step.
func (s *StepServer) RunAutomation(ctx context.Context, r AutomationRun) (AutomationResult, error) {
	if !attemptIDShape(r.AttemptID) {
		return AutomationResult{}, errors.New("a script runs for an attempt: name its ID")
	}
	worktree := filepath.Join(s.home, "worktrees", r.AttemptID)
	if fi, err := os.Stat(worktree); err != nil || !fi.IsDir() {
		return AutomationResult{}, fmt.Errorf("the attempt %s has no worktree in this home", r.AttemptID)
	}
	names, _, err := teamToolchains(s.home)
	if err != nil {
		return AutomationResult{}, fmt.Errorf("read the team's toolchains: %w", err)
	}
	allow, deny := turnAllowRules(s.home, names)
	token, err := runToken()
	if err != nil {
		return AutomationResult{}, err
	}
	memory := r.Memory
	if memory <= 0 {
		memory = AutomationMemory
	}
	wall := r.Limits.Wall
	if wall <= 0 {
		wall = automation.MaxWall
	}
	runCtx, cancel := context.WithTimeout(ctx, wall+automationGrace)
	defer cancel()
	run := &liveRun{ctx: runCtx, id: newUUID(), attempt: r.AttemptID, pull: r.Pull, script: r.Name, point: r.Point, worktree: worktree,
		allow: automation.NewAllowList(allow, deny)}
	s.runsMu.Lock()
	if s.runs == nil {
		s.runs = map[string]*liveRun{}
	}
	s.runs[token] = run
	s.runsMu.Unlock()
	defer func() {
		s.runsMu.Lock()
		delete(s.runs, token)
		s.runsMu.Unlock()
	}()

	sock, err := filepath.Abs(StepSocket(s.home))
	if err != nil {
		return AutomationResult{}, err
	}
	in, _ := json.Marshal(automationRequest{Version: 1, Socket: sock, Token: token, Name: r.Name, Source: string(r.Source),
		Event: r.Event, Allow: allow, Deny: deny, Memory: memory, Steps: r.Limits.Steps, Wall: wall})
	cmd := exec.CommandContext(runCtx, selfExecutable(), "automation", "run")
	// No credential reaches the child: an empty environment (Windows adds
	// only SYSTEMROOT), the private state directory, and nothing in its
	// arguments.
	cmd.Env = []string{}
	cmd.Dir = filepath.Dir(sock)
	cmd.Stdin = bytes.NewReader(in)
	var out, errb capped
	out.n, errb.n = maxChildOutput, maxChildStderr
	fatal := make(chan struct{})
	errb.fatal = fatal
	cmd.Stdout, cmd.Stderr = &out, &errb
	cmd.WaitDelay = time.Second
	bindToLauncher(cmd)
	if err := cmd.Start(); err != nil {
		return AutomationResult{}, fmt.Errorf("start the script's process: %w", err)
	}
	if release, err := afterStart(cmd); err != nil {
		s.log.Warn("the script's process will not be stopped if the launcher is killed", "error", err.Error())
	} else {
		defer release()
	}
	// A process that reported a fatal error is ended after a moment: on
	// Windows, a Go runtime that ran out of memory inside its job object
	// was seen to stay alive after its report, held by the system's crash
	// handling, until the run's wall clock.
	ended := make(chan struct{})
	go func() {
		select {
		case <-fatal:
		case <-ended:
			return
		}
		select {
		case <-time.After(fatalGrace):
			_ = cmd.Process.Kill()
		case <-ended:
		}
	}()
	waitErr := cmd.Wait()
	close(ended)

	res := AutomationResult{RunID: run.id}
	var reply automationReply
	decoded := json.Unmarshal(out.b.Bytes(), &reply) == nil
	switch {
	case errors.Is(runCtx.Err(), context.DeadlineExceeded):
		res.Failed = fmt.Sprintf("the run passed its wall-clock limit of %s, and its process was stopped", wall)
	case memoryFailure(errb.b.String()) != "":
		res.Failed, res.Memory = fmt.Sprintf("the script's process passed its memory limit of %d MiB (%s)", memory>>20,
			memoryFailure(errb.b.String())), true
	case decoded && reply.Error != "":
		res.Failed, res.Invalid = reply.Error, reply.Invalid
	case decoded && waitErr == nil:
		res.Turn = reply.Turn
	case runCtx.Err() != nil:
		res.Failed = "the run was stopped: " + runCtx.Err().Error()
	default:
		res.Failed = "the script's process ended without an answer: " + processEnd(waitErr, errb.b.String())
	}
	run.stepsMu.Lock()
	submit := run.submit
	run.stepsMu.Unlock()
	if submit != "" && res.Failed == "" {
		// The submit the script asked for, after it ended: the before-submit
		// point and the work step take their own time, outside the script's
		// wall clock, and a submit that fails fails the run.
		if err := s.submitResult(ctx, run, submit); err != nil {
			res.Failed = "the submit of " + submit + ": " + err.Error()
		}
	}
	if res.Failed != "" {
		res.Failed = redactTurnText(s.home, res.Failed)
	}
	s.recordRun(run, r.Event, res)
	run.stepsMu.Lock()
	res.Steps = run.steps
	run.stepsMu.Unlock()
	return res, nil
}

// memoryFailure is the line of the child's standard error that says it
// ran out of memory, or "": Go's own fatal error when the OS refuses an
// allocation, the watchdog's message, or, in a race-detector build (the
// tests'), ThreadSanitizer's own refused allocation. A mapping the limit
// refused somewhere else counts too: on Linux the data limit holds every
// private writable mapping, a thread's stack and the C library's thread
// state with the heap, so a child near the limit whose runtime needs
// another OS thread dies there first, with "pthread_create failed" (a cgo
// build), "failed to create new OS thread" (without cgo), or a signal
// where the C library's thread start touched what it did not get. The
// script is interpreted and memory-safe and the child runs nothing else,
// so a segmentation violation, an abort or a bus error under the limit is
// the limit acting; the line is kept in the text for the reader.
func memoryFailure(stderr string) string {
	for line := range strings.Lines(stderr) {
		for _, sign := range []string{"out of memory", "cannot allocate memory", "cannot map pages", memoryWatchdogMessage,
			"ThreadSanitizer failed to allocate", "pthread_create failed", "failed to create new OS thread",
			"SIGSEGV: segmentation violation", "SIGABRT: abort", "SIGBUS: bus error"} {
			if strings.Contains(line, sign) {
				return strings.TrimSpace(line)
			}
		}
	}
	return ""
}

// processEnd is how the child ended: its exit status and the first line of
// what it wrote to its standard error.
func processEnd(err error, stderr string) string {
	s := "it exited normally"
	if err != nil {
		s = err.Error()
	}
	if line, _, _ := strings.Cut(strings.TrimSpace(stderr), "\n"); line != "" {
		s += ": " + line
	}
	return s
}

// automationRecord is one line of AutomationLogFile.
type automationRecord struct {
	At      time.Time `json:"at"`
	Run     string    `json:"run"`
	Attempt string    `json:"attempt"`
	Script  string    `json:"script"`
	// A step: its kind and text.
	Kind string `json:"kind,omitempty"`
	Text string `json:"text,omitempty"`
	// The run's end: handled, turn or failed, with the turn's kind.
	Outcome string   `json:"outcome,omitempty"`
	Turn    string   `json:"turn,omitempty"`
	Events  []string `json:"events,omitempty"`
	// Kinds are the event kinds the run was for: the point's name, or the
	// launcher's own event (forge.changed).
	Kinds []string `json:"kinds,omitempty"`
	// A run at an extension point (dispatch.go): the point, and for a
	// declared command its argument list and exit status.
	Point   string   `json:"point,omitempty"`
	Command []string `json:"command,omitempty"`
	Exit    *int     `json:"exit,omitempty"`
}

// recordRun records how a run ended, and a failed run as a step too.
func (s *StepServer) recordRun(run *liveRun, ev automation.Event, res AutomationResult) {
	rec := automationRecord{At: time.Now().UTC(), Run: run.id, Attempt: run.attempt, Script: run.script, Point: run.point, Events: ev.IDs, Kinds: ev.Kinds}
	switch {
	case res.Failed != "":
		s.recordStep(run, stepAutomationFailed, res.Failed)
		rec.Outcome = "failed"
	case res.Turn != nil:
		rec.Outcome, rec.Turn = "turn", res.Turn.Kind
	default:
		rec.Outcome = "handled"
	}
	if err := appendAutomationRecord(s.home, rec); err != nil {
		s.log.Warn("a script run was not recorded", "error", err.Error())
	}
}

// recordStep records a step of the attempt in AutomationLogFile, and keeps
// it on the run, bounded, for the run's report.
func (s *StepServer) recordStep(run *liveRun, kind, text string) error {
	text = redactText(s.home, text)
	run.stepsMu.Lock()
	if len(run.steps) < maxReportSteps {
		run.steps = append(run.steps, automationStep{Kind: kind, Text: bound(text, maxReportStepText)})
	}
	run.stepsMu.Unlock()
	return appendAutomationRecord(s.home, automationRecord{At: time.Now().UTC(), Run: run.id, Attempt: run.attempt,
		Script: run.script, Point: run.point, Kind: kind, Text: text})
}

// redactText is s without the home's forge tokens and the known token
// shapes.
func redactText(home, s string) string {
	return tokenShapes.ReplaceAllString(RedactForgeTokens(home, s), "[redacted]")
}

func appendAutomationRecord(home string, r automationRecord) error {
	b, err := json.Marshal(r)
	if err != nil {
		return err
	}
	path := filepath.Join(home, filepath.FromSlash(AutomationLogFile))
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.Write(append(b, '\n'))
	return err
}

// runToken is a new run's token: 32 random bytes.
func runToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// turnAllowRules are the Bash rules of the home's worker turn with the
// named toolchains', and the turn's deny rules (TurnAllowList).
func turnAllowRules(home string, toolchains []string) (allow, deny []string) {
	allow = turnAllow(home, false)
	for _, n := range toolchains {
		allow = append(allow, toolchain.Rules(n)...)
	}
	return allow, denyRules()
}

// capped keeps the first n bytes written to it and drops the rest. When
// fatal is set, it is closed once what was kept reports a fatal error.
type capped struct {
	b     bytes.Buffer
	n     int
	fatal chan struct{}
}

func (c *capped) Write(p []byte) (int, error) {
	if room := c.n - c.b.Len(); room > 0 {
		c.b.Write(p[:min(len(p), room)])
	}
	if c.fatal != nil && bytes.Contains(c.b.Bytes(), []byte("fatal error:")) {
		close(c.fatal)
		c.fatal = nil
	}
	return len(p), nil
}

// fatalGrace is how long a process that reported a fatal error has to
// exit before the launcher ends it.
const fatalGrace = 2 * time.Second

// serveAutomation answers one host call of a live run. It runs beside the
// steps, never under their lock: a run may be started from within a step.
func (s *StepServer) serveAutomation(call StepCall) StepAnswer {
	var in automationCall
	if json.Unmarshal(call.Body, &in) != nil || in.Token == "" {
		return refuse("invalid_request", "A host call names its run's token.", "Only a script's process makes host calls.")
	}
	s.runsMu.Lock()
	run := s.runs[in.Token]
	s.runsMu.Unlock()
	if run == nil {
		return refuse("no_run", "No script run holds this token.", "Only a running script's process makes host calls.")
	}
	ctx := run.ctx
	switch in.Call {
	case "pr":
		p, err := s.hostPR(ctx, run)
		if err != nil {
			return s.hostErr(err)
		}
		return done(redactPR(hostRedactor(s.home), p))
	case "attempt":
		a, err := s.hostAttempt(ctx, run)
		if err != nil {
			return s.hostErr(err)
		}
		return done(a)
	case "run":
		res, err := s.hostRun(ctx, run, in.Cmd, in.Args)
		if err != nil {
			return s.hostErr(err)
		}
		return done(res)
	case "step":
		if !automationKind.MatchString(in.Kind) || len(in.Text) > automation.MaxText {
			return refuse("invalid_request", "A step's kind is lowercase letters, digits and '_', and its text at most 16 KiB.", "")
		}
		if err := s.recordStep(run, in.Kind, in.Text); err != nil {
			return s.hostErr(fmt.Errorf("record the step: %w", err))
		}
		return done(struct{}{})
	case "comment":
		if err := s.hostComment(ctx, run, in.Text); err != nil {
			return s.hostErr(err)
		}
		return done(struct{}{})
	case "label":
		if err := s.hostLabel(ctx, run, in.Add, in.Remove); err != nil {
			return s.hostErr(err)
		}
		return done(struct{}{})
	case "ready":
		if err := s.hostReady(ctx, run); err != nil {
			return s.hostErr(err)
		}
		return done(struct{}{})
	case "submit":
		if err := s.hostSubmit(ctx, run, in.URL); err != nil {
			return s.hostErr(err)
		}
		return done(struct{}{})
	case "rerun":
		res, err := s.hostRerun(ctx, run)
		if err != nil {
			return s.hostErr(err)
		}
		return done(res)
	case "update_base":
		res, err := s.hostUpdateBase(ctx, run)
		if err != nil {
			return s.hostErr(err)
		}
		return done(res)
	}
	return refuse("invalid_request", "Unknown host call "+in.Call+".", "Use pr, attempt, run, step, comment, label, ready, submit, rerun or update_base.")
}

// hostRedactor removes the home's forge tokens and the known token shapes
// from a text, the tokens read once. Every text a host call gives the
// child passes through it: what the forge, a review, a job's log or a
// command says may quote a credential.
func hostRedactor(home string) func(string) string {
	var toks []string
	if doc, _, err := readAgentDoc(home); err == nil {
		for host := range doc.forgeEntries() {
			if _, tok, err := forgeTokenFor(home, doc, host); err == nil && tok != "" {
				toks = append(toks, tok)
			}
		}
	}
	return func(s string) string {
		for _, t := range toks {
			s = strings.ReplaceAll(s, t, redacted)
		}
		return tokenShapes.ReplaceAllString(s, "[redacted]")
	}
}

// redactPR is p with every text redacted.
func redactPR(r func(string) string, p automation.PR) automation.PR {
	list := func(ss []string) []string {
		out := make([]string, 0, len(ss))
		for _, s := range ss {
			out = append(out, r(s))
		}
		return out
	}
	p.URL, p.Head, p.Base, p.Branch, p.Labels = r(p.URL), r(p.Head), r(p.Base), r(p.Branch), list(p.Labels)
	reviews := make([]automation.Review, 0, len(p.Reviews))
	for _, rv := range p.Reviews {
		reviews = append(reviews, automation.Review{Reviewer: r(rv.Reviewer), Head: r(rv.Head), Verdict: r(rv.Verdict),
			Findings: list(rv.Findings), Body: r(rv.Body)})
	}
	checks := make([]automation.Check, 0, len(p.Checks))
	for _, c := range p.Checks {
		checks = append(checks, automation.Check{Name: r(c.Name), Conclusion: r(c.Conclusion), Excerpt: r(c.Excerpt), RunnerFailure: c.RunnerFailure})
	}
	p.Reviews, p.Checks = reviews, checks
	return p
}

// automationKind is a step's kind (automation.stepKind).
var automationKind = regexp.MustCompile(`^[a-z0-9_]{1,64}$`)

// hostErr is a host call's failure, redacted: the script sees it in its
// error, and the error may land in a record.
func (s *StepServer) hostErr(err error) StepAnswer {
	return StepAnswer{Status: StepFailed, Error: &StepError{Code: "host_failed", Message: redactText(s.home, err.Error())}}
}

// pullTarget of a run: the repository of its worktree's origin, and the
// home's credential for its host.
func (s *StepServer) runTarget(ctx context.Context, run *liveRun) (pullTarget, error) {
	if run.pull <= 0 {
		return pullTarget{}, errors.New("the attempt has no pull request yet")
	}
	return (&ForgePR{Home: s.home, Dir: run.worktree}).target(ctx)
}

// hostPR is pr(): the pull request with its content, read with the home's
// credential, and whether its base has commits the head lacks, as the
// worktree's last fetch saw it.
func (s *StepServer) hostPR(ctx context.Context, run *liveRun) (automation.PR, error) {
	t, err := s.runTarget(ctx, run)
	if err != nil {
		return automation.PR{}, err
	}
	p, c, err := newAutomationForge().GetPullContent(ctx, t.host, t.kind(), t.token, t.path, run.pull)
	if err != nil {
		return automation.PR{}, fmt.Errorf("read pull request %d of %s on %s: %w", run.pull, t.path, t.host, err)
	}
	out := automation.PR{Number: p.Number, URL: p.URL, Head: p.HeadCommit, Base: p.Base, Branch: p.Head, Draft: p.Draft,
		Mergeable: p.Mergeable, Labels: p.Labels, Behind: behindBase(ctx, run.worktree, p.Base, p.HeadCommit)}
	// The newest review of each reviewer, at any head: a script compares
	// its head with the pull request's.
	seen := map[string]bool{}
	for i := len(c.Comments) - 1; i >= 0; i-- {
		cm := c.Comments[i]
		if cm.Review == nil || seen[cm.Review.Reviewer] {
			continue
		}
		seen[cm.Review.Reviewer] = true
		out.Reviews = append(out.Reviews, automation.Review{Reviewer: cm.Review.Reviewer, Head: cm.Review.Head,
			Verdict: cm.Review.Verdict, Findings: reviewFindings(cm.Body), Body: cm.Body})
	}
	for _, ch := range c.Checks {
		k := automation.Check{Name: ch.Name, Conclusion: ch.State, RunnerFailure: ch.RunnerFailure}
		if ch.Log != nil {
			k.Excerpt = ch.Log.Excerpt
		}
		out.Checks = append(out.Checks, k)
	}
	return out, nil
}

// hostLabel is label(add, remove): the labels removed, then added, on the
// attempt's pull request with the home's credential, each checked again
// whatever the child checked, and recorded as a step.
func (s *StepServer) hostLabel(ctx context.Context, run *liveRun, add, remove []string) error {
	if len(add) > automation.MaxLabels || len(remove) > automation.MaxLabels || len(add)+len(remove) == 0 {
		return fmt.Errorf("label names 1 to %d labels to add and to remove", automation.MaxLabels)
	}
	for _, l := range slices.Concat(remove, add) {
		if !automation.LabelName(l) {
			return fmt.Errorf("%q is not a label name", l)
		}
	}
	t, err := s.runTarget(ctx, run)
	if err != nil {
		return err
	}
	if t.holds(slices.Concat(remove, add)...) {
		return errHoldsToken
	}
	f := newAutomationForge()
	if len(remove) > 0 {
		if err := f.RemoveLabels(ctx, t.host, t.kind(), t.token, t.path, run.pull, remove); err != nil {
			return fmt.Errorf("remove %s from pull request %d of %s on %s: %w", strings.Join(remove, ", "), run.pull, t.path, t.host, err)
		}
	}
	if len(add) > 0 {
		if err := f.AddLabels(ctx, t.host, t.kind(), t.token, t.path, run.pull, add); err != nil {
			return fmt.Errorf("add %s to pull request %d of %s on %s: %w", strings.Join(add, ", "), run.pull, t.path, t.host, err)
		}
	}
	var what []string
	if len(remove) > 0 {
		what = append(what, "removed "+strings.Join(remove, ", "))
	}
	if len(add) > 0 {
		what = append(what, "added "+strings.Join(add, ", "))
	}
	return s.recordStep(run, stepHostLabel, strings.Join(what, "; "))
}

// hostReady is ready(): the attempt's pull request leaves its draft state
// and loses its draft prefix, with the home's credential.
func (s *StepServer) hostReady(ctx context.Context, run *liveRun) error {
	t, err := s.runTarget(ctx, run)
	if err != nil {
		return err
	}
	p, err := newAutomationForge().ReadyPull(ctx, t.host, t.kind(), t.token, t.path, run.pull)
	if err != nil {
		return fmt.Errorf("mark pull request %d of %s on %s ready: %w", run.pull, t.path, t.host, err)
	}
	return s.recordStep(run, stepHostReady, fmt.Sprintf("pull request %d is ready: %s", p.Number, p.Title))
}

// hostSubmit is submit(url): the attempt's result is to be submitted with
// url as the result reference. The call checks and keeps the request; the
// submit itself is sent by submitResult once the script ended without
// failing, since the before-submit point and the work step take their own
// time, outside the script's wall clock. A run at the before-submit point
// itself is refused: the step that runs it submits.
func (s *StepServer) hostSubmit(ctx context.Context, run *liveRun, u string) error {
	if !automation.ResultURL(u) {
		return errors.New("the result reference is not an http(s) URL on one line")
	}
	if run.point == automation.PointBeforeSubmit {
		return errors.New("a run at before-submit cannot submit: the step that runs it does")
	}
	st, err := s.Status(ctx)
	if err != nil {
		return fmt.Errorf("read the session status: %w", err)
	}
	if st.Attempt == nil || st.Attempt.ID != run.attempt {
		return fmt.Errorf("the attempt %s is no longer the member's open attempt", run.attempt)
	}
	if t, err := s.runTarget(ctx, run); err == nil && t.holds(u) {
		return errHoldsToken
	}
	run.stepsMu.Lock()
	run.submit = u
	run.stepsMu.Unlock()
	return nil
}

// submitResult sends the submit a script asked for: the before-submit point
// first (dispatch.go; the run's caller holds the points' run lock, so the
// point fires without taking it), then the holder's own work step with u
// as the result reference, so aicrewd sees the submit the member would
// send. A committed step is recorded as a step of the run.
func (s *StepServer) submitResult(ctx context.Context, run *liveRun, u string) error {
	st, err := s.Status(ctx)
	if err != nil {
		return fmt.Errorf("read the session status: %w", err)
	}
	if st.Attempt == nil || st.Attempt.ID != run.attempt {
		return fmt.Errorf("the attempt %s is no longer the member's open attempt", run.attempt)
	}
	s.fireBeforeSubmit(ctx, run.attempt)
	body, _ := json.Marshal(map[string]string{"intent": "submit", "detail": u})
	res, err := s.driver.Run(ctx, StepRequest{Path: attemptsPath + "/" + url.PathEscape(run.attempt) + "/work", Body: body,
		TaskID: st.Attempt.Task.TaskID})
	if err != nil {
		return fmt.Errorf("the submit step: %w", err)
	}
	if !res.Settled || res.Outcome == "committed" {
		s.steps.Add(1)
	}
	if !res.Settled {
		return errors.New("the submit step is pending; `aicrew-agent step recover` finishes it")
	}
	if res.Outcome != "committed" {
		return fmt.Errorf("the submit step did not commit: %s", res.Outcome)
	}
	return s.recordStep(run, stepHostSubmit, "the result "+u+" is submitted")
}

// hostRerun is rerun(): the failed jobs of the pull request's head re-run
// once per head, with the home's credential. The head is claimed in the
// points' state before the forge is asked, and the claimed heads are kept
// as long as the pull request's points are, so a second call at the head,
// in this run or a later one, or after the branch came back to the head,
// re-runs nothing;
// a forge without a re-run call, or a head with no failed job it names,
// re-runs nothing and leaves the head unclaimed.
func (s *StepServer) hostRerun(ctx context.Context, run *liveRun) (automation.RerunResult, error) {
	t, err := s.runTarget(ctx, run)
	if err != nil {
		return automation.RerunResult{}, err
	}
	f := newAutomationForge()
	p, c, err := f.GetPullContent(ctx, t.host, t.kind(), t.token, t.path, run.pull)
	if err != nil {
		return automation.RerunResult{}, fmt.Errorf("read pull request %d of %s on %s: %w", run.pull, t.path, t.host, err)
	}
	jobs := 0
	for _, ch := range c.Checks {
		if ch.State == forge.ChecksFailure && ch.JobID != 0 {
			jobs++
		}
	}
	if jobs == 0 || p.HeadCommit == "" {
		return s.rerunResult(run, automation.RerunResult{Reason: "no failed job at the head that the forge can re-run"})
	}
	key := pullKey(t.host, t.path, run.pull)
	// mark claims head among the pull request's re-run heads, or releases
	// it; changed is false when it was so already.
	mark := func(head string, claim bool) (changed bool, err error) {
		s.points.mu.Lock()
		defer s.points.mu.Unlock()
		doc := readPoints(s.home)
		cur := doc.Pulls[key]
		if cur == nil {
			cur = &pullPoints{Attempt: run.attempt, Number: run.pull, URL: p.URL}
			doc.Pulls[key] = cur
		}
		i := slices.Index(cur.Reruns, head)
		if claim == (i >= 0) {
			return false, nil
		}
		if claim {
			cur.Reruns = append(cur.Reruns, head)
		} else {
			cur.Reruns = slices.Delete(cur.Reruns, i, i+1)
		}
		return true, writePoints(s.home, doc, key)
	}
	claimed, err := mark(p.HeadCommit, true)
	if err != nil {
		return automation.RerunResult{}, fmt.Errorf("record the re-run: %w", err)
	}
	if !claimed {
		return s.rerunResult(run, automation.RerunResult{Reason: "the failed jobs were re-run at this head already"})
	}
	names, err := f.RerunFailed(ctx, t.host, t.kind(), t.token, t.path, c.Checks)
	if errors.Is(err, forge.ErrNoRerun) {
		// Nothing was re-run: the head is given back.
		if _, werr := mark(p.HeadCommit, false); werr != nil {
			s.log.Warn("automations: the re-run's state was not written", "error", werr.Error())
		}
		return s.rerunResult(run, automation.RerunResult{Reason: err.Error()})
	}
	if err != nil {
		return automation.RerunResult{}, fmt.Errorf("re-run the failed jobs of pull request %d of %s on %s: %w", run.pull, t.path, t.host, err)
	}
	return s.rerunResult(run, automation.RerunResult{Reran: true, Jobs: names})
}

// rerunResult records res as a step and returns it.
func (s *StepServer) rerunResult(run *liveRun, res automation.RerunResult) (automation.RerunResult, error) {
	text := "re-ran nothing: " + res.Reason
	if res.Reran {
		text = "re-ran " + strings.Join(res.Jobs, ", ")
	}
	return res, s.recordStep(run, stepHostRerun, text)
}

// baseUpdateNote is the delta note update_base() posts after a base-only
// update (docs/DESIGN-TURNS.md 10.4): the old and the new head, the
// identity both have, and the command that computes it.
const baseUpdateNote = `Base-only update: %s -> %s, merged from %s.
Patch identity unchanged: %s
(git diff $(git merge-base origin/%s HEAD) HEAD | git patch-id --stable)
The reviews at %s stand; CI runs on %s.`

// The bounds of a conflict update_base() returns.
const (
	maxConflictFiles = automation.MaxConflictFiles
	maxConflictHunks = automation.MaxText
)

// hostUpdateBase is update_base(): the pull request's branch brought up to
// date with its base by a merge, pushed without force when the patch
// identity is unchanged, with the delta note as a comment. It works in a
// temporary detached worktree of the attempt's clone at the branch's head
// as the origin has it, so a turn's uncommitted work in the attempt's
// worktree is never touched; the clone's credential helper serves the
// fetch and the push, and the script holds nothing. A conflict aborts the
// merge and is returned with the base, the files and the hunks, bounded;
// a changed identity is returned with no push; a push the origin refuses
// (the branch moved) is returned with its reason, the new head being an
// event of its own; an up-to-date branch is returned as not updated. The
// attempt's worktree is fast-forwarded to the new head when it is clean
// and at the old head. The outcome is one step, host_update_base.
func (s *StepServer) hostUpdateBase(ctx context.Context, run *liveRun) (automation.UpdateBaseResult, error) {
	t, err := s.runTarget(ctx, run)
	if err != nil {
		return automation.UpdateBaseResult{}, err
	}
	f := newAutomationForge()
	p, _, err := f.GetPullContent(ctx, t.host, t.kind(), t.token, t.path, run.pull)
	if err != nil {
		return automation.UpdateBaseResult{}, fmt.Errorf("read pull request %d of %s on %s: %w", run.pull, t.path, t.host, err)
	}
	base, branch := p.Base, p.Head
	for _, b := range []string{base, branch} {
		if !branchShape.MatchString(b) || strings.Contains(b, "..") {
			return automation.UpdateBaseResult{}, fmt.Errorf("pull request %d names the branch %q, which is not a branch name", run.pull, b)
		}
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	git := automationGit
	if git == nil {
		git = execGit
	}
	wt := run.worktree
	baseRef, branchRef := "refs/remotes/origin/"+base, "refs/remotes/origin/"+branch
	if _, err := git(ctx, wt, gitEnv, "fetch", "--quiet", "origin", "+refs/heads/"+base+":"+baseRef, "+refs/heads/"+branch+":"+branchRef); err != nil {
		return automation.UpdateBaseResult{}, fmt.Errorf("fetch %s and %s: %w", base, branch, err)
	}
	old, err := git(ctx, wt, gitEnv, "rev-parse", "--verify", "--quiet", branchRef+"^{commit}")
	if old = strings.TrimSpace(old); err != nil || !baseShape.MatchString(old) {
		return automation.UpdateBaseResult{}, fmt.Errorf("the origin has no branch %s", branch)
	}
	// An update in progress: cut after its push, the origin's head is its
	// recorded new head and the rest is completed here, without another
	// merge or push; at any other head the record is stale and goes.
	key := pullKey(t.host, t.path, run.pull)
	if rec := s.readBaseUpdate(key); rec != nil {
		if old == rec.New {
			return s.finishBaseUpdate(ctx, run, t, f, key, p.URL, rec, git, "completed the update cut after its push")
		}
		if err := s.writeBaseUpdate(key, run, p.URL, nil); err != nil {
			return automation.UpdateBaseResult{}, fmt.Errorf("clear the stale record of an update: %w", err)
		}
	}
	if _, err := git(ctx, wt, gitEnv, "merge-base", "--is-ancestor", baseRef, old); err == nil {
		return s.updateBaseResult(run, automation.UpdateBaseResult{OldHead: old, Reason: "the branch is up to date with " + base})
	}
	before := patchIdentityAt(ctx, git, wt, baseRef, old)
	if before == "" {
		return automation.UpdateBaseResult{}, fmt.Errorf("the patch identity of %s against %s could not be computed", branch, base)
	}
	// The temporary worktree: a stale one of an earlier run goes first.
	dir := wt + ".base-update"
	removeWorktree := func(ctx context.Context) {
		git(ctx, wt, gitEnv, "worktree", "remove", "--force", dir)
		os.RemoveAll(dir)
		git(ctx, wt, gitEnv, "worktree", "prune")
	}
	removeWorktree(ctx)
	if _, err := git(ctx, wt, gitEnv, "worktree", "add", "--quiet", "--detach", dir, old); err != nil {
		return automation.UpdateBaseResult{}, fmt.Errorf("make the update's worktree: %w", err)
	}
	defer removeWorktree(context.WithoutCancel(ctx))
	if _, err := git(ctx, dir, gitEnv, "merge", "--quiet", "--no-edit", "-m", "Merge "+base+" into "+branch+" (base-only update)", baseRef); err != nil {
		c := conflictIn(ctx, git, dir, base)
		git(ctx, dir, gitEnv, "merge", "--abort")
		if c == nil {
			return automation.UpdateBaseResult{}, fmt.Errorf("merge %s into %s: %w", base, branch, err)
		}
		return s.updateBaseResult(run, automation.UpdateBaseResult{OldHead: old, Conflict: c,
			Reason: fmt.Sprintf("merging %s into %s conflicts in %s", base, branch, strings.Join(c.Files, ", "))})
	}
	head, err := git(ctx, dir, gitEnv, "rev-parse", "HEAD")
	if head = strings.TrimSpace(head); err != nil || !baseShape.MatchString(head) {
		return automation.UpdateBaseResult{}, fmt.Errorf("the merged head: %w", err)
	}
	after := patchIdentityAt(ctx, git, dir, baseRef, head)
	if after != before {
		return s.updateBaseResult(run, automation.UpdateBaseResult{OldHead: old, IdentityChanged: true,
			Reason: fmt.Sprintf("merging %s into %s changes the patch identity from %s to %s; update the branch by hand", base, branch, before, after)})
	}
	// The record goes before the push, so a run cut after it is completed
	// at the next call; a push that fails leaves the record, which the next
	// call completes when the origin applied the push after all, and
	// clears otherwise.
	rec := &pointBaseUpdate{Old: old, New: head, Base: base, Identity: after}
	if err := s.writeBaseUpdate(key, run, p.URL, rec); err != nil {
		return automation.UpdateBaseResult{}, fmt.Errorf("record the update before its push: %w", err)
	}
	if _, err := git(ctx, dir, gitEnv, "push", "--quiet", "origin", "HEAD:refs/heads/"+branch); err != nil {
		return s.updateBaseResult(run, automation.UpdateBaseResult{OldHead: old,
			Reason: fmt.Sprintf("the push of %s was refused, the branch moved during the update: %s", branch, err)})
	}
	rec.Pushed = true
	s.noteBaseUpdate(key, run, p.URL, rec)
	git(ctx, wt, gitEnv, "update-ref", branchRef, head, old)
	return s.finishBaseUpdate(ctx, run, t, f, key, p.URL, rec, git, "")
}

// finishBaseUpdate is an update's effects after its push, from its
// record: the attempt's worktree fast-forwarded, the delta note posted
// unless it was, the step recorded, and the record cleared once the note
// is posted (a note the forge refused leaves it, for the next call);
// reason, when set, is the completion of an update a run left cut.
func (s *StepServer) finishBaseUpdate(ctx context.Context, run *liveRun, t pullTarget, f AutomationForge, key, url string,
	rec *pointBaseUpdate, git GitRunner, reason string) (automation.UpdateBaseResult, error) {
	res := automation.UpdateBaseResult{Updated: true, OldHead: rec.Old, NewHead: rec.New, Reason: reason}
	fastForward(ctx, git, run.worktree, rec.Old, rec.New)
	if !rec.Commented {
		note := fmt.Sprintf(baseUpdateNote, rec.Old, rec.New, rec.Base, rec.Identity, rec.Base, rec.Old, rec.New)
		if _, err := f.CommentPull(ctx, t.host, t.kind(), t.token, t.path, run.pull, note); err != nil {
			res.Reason = fmt.Sprintf("the delta note was not posted on pull request %d of %s on %s: %s", run.pull, t.path, t.host, err)
			return s.updateBaseResult(run, res)
		}
		rec.Commented = true
		s.noteBaseUpdate(key, run, url, rec)
	}
	out, err := s.updateBaseResult(run, res)
	if err != nil {
		return out, err
	}
	if err := s.writeBaseUpdate(key, run, url, nil); err != nil {
		return out, fmt.Errorf("clear the record of the completed update: %w", err)
	}
	return out, nil
}

// readBaseUpdate is the base update in progress for the pull request key,
// nil when none.
func (s *StepServer) readBaseUpdate(key string) *pointBaseUpdate {
	s.points.mu.Lock()
	defer s.points.mu.Unlock()
	if p := readPoints(s.home).Pulls[key]; p != nil && p.BaseUpdate != nil {
		rec := *p.BaseUpdate
		return &rec
	}
	return nil
}

// writeBaseUpdate records rec as the base update in progress for the pull
// request key, or clears it when rec is nil.
func (s *StepServer) writeBaseUpdate(key string, run *liveRun, url string, rec *pointBaseUpdate) error {
	s.points.mu.Lock()
	defer s.points.mu.Unlock()
	doc := readPoints(s.home)
	cur := doc.Pulls[key]
	if cur == nil {
		if rec == nil {
			return nil
		}
		cur = &pullPoints{Attempt: run.attempt, Number: run.pull, URL: url}
		doc.Pulls[key] = cur
	}
	cur.BaseUpdate = rec
	return writePoints(s.home, doc, key)
}

// noteBaseUpdate writes rec after an effect; a write that fails is logged,
// the origin's head being what the next call reads.
func (s *StepServer) noteBaseUpdate(key string, run *liveRun, url string, rec *pointBaseUpdate) {
	if err := s.writeBaseUpdate(key, run, url, rec); err != nil {
		s.log.Warn("the base update's record was not written", "error", err.Error())
	}
}

// conflictIn is the conflict a failed merge left in dir: the unmerged
// files and their hunks, bounded; nil when the merge failed with none.
func conflictIn(ctx context.Context, git GitRunner, dir, base string) *automation.Conflict {
	names, err := git(ctx, dir, gitEnv, "diff", "--name-only", "--diff-filter=U")
	if err != nil || strings.TrimSpace(names) == "" {
		return nil
	}
	c := &automation.Conflict{Base: base}
	for _, n := range strings.Split(strings.TrimSpace(names), "\n") {
		if len(c.Files) < maxConflictFiles {
			c.Files = append(c.Files, n)
		}
	}
	hunks, _ := git(ctx, dir, gitEnv, "diff", "--diff-filter=U")
	c.Hunks, _ = automation.Bound(hunks, maxConflictHunks)
	return c
}

// fastForward moves the worktree to head when it is at old with no change
// to a tracked file; an untracked file is no reason to leave the branch
// behind the head it was pushed to.
func fastForward(ctx context.Context, git GitRunner, worktree, old, head string) {
	if st, err := git(ctx, worktree, gitEnv, "status", "--porcelain", "--untracked-files=no"); err != nil || strings.TrimSpace(st) != "" {
		return
	}
	if at, err := git(ctx, worktree, gitEnv, "rev-parse", "HEAD"); err != nil || strings.TrimSpace(at) != old {
		return
	}
	git(ctx, worktree, gitEnv, "merge", "--quiet", "--ff-only", head)
}

// updateBaseResult redacts res, records it as a step and returns it.
func (s *StepServer) updateBaseResult(run *liveRun, res automation.UpdateBaseResult) (automation.UpdateBaseResult, error) {
	red := hostRedactor(s.home)
	res.Reason = red(res.Reason)
	text := "not updated: " + res.Reason
	switch {
	case res.Conflict != nil:
		res.Conflict.Hunks = red(res.Conflict.Hunks)
	case res.Updated:
		text = fmt.Sprintf("%s -> %s", res.OldHead, res.NewHead)
		if res.Reason != "" {
			text += "; " + res.Reason
		}
	}
	return res, s.recordStep(run, stepHostUpdateBase, text)
}

// findingLine is a review's finding: a list item that opens with its
// disposition in brackets, as oh-code-review and the external reviewer
// write them.
var findingLine = regexp.MustCompile(`^\s*(?:[-*]|\d+\.)\s+\[(BLOCKER|CURRENT_SCOPE_IMPROVEMENT|FOLLOW_UP|OBSERVATION|REJECTED|RISK_ACCEPTED)\]`)

// The bounds of a review's findings in pr().
const (
	maxFindings    = 50
	maxFindingLine = 1000
)

// reviewFindings are the finding lines of a review's body, each without
// its list marker.
func reviewFindings(body string) []string {
	var out []string
	for _, line := range strings.Split(body, "\n") {
		if !findingLine.MatchString(line) || len(out) == maxFindings {
			continue
		}
		line = strings.TrimSpace(line)
		line = strings.TrimSpace(strings.TrimLeft(line, "-*0123456789."))
		line, _ = automation.Bound(line, maxFindingLine)
		out = append(out, line)
	}
	return out
}

// behindBase reports whether the base branch, as the worktree's origin
// last fetched it, has commits that head lacks. Unknown reads as false.
func behindBase(ctx context.Context, worktree, base, head string) bool {
	if base == "" || head == "" || strings.HasPrefix(base, "-") || strings.HasPrefix(head, "-") {
		return false
	}
	_, err := execGit(ctx, worktree, gitEnv, "merge-base", "--is-ancestor", "refs/remotes/origin/"+base, head)
	var exit *exec.ExitError
	return errors.As(err, &exit) && exit.ExitCode() == 1
}

// hostAttempt is attempt(): the attempt's state from the session status,
// with what its turns cost and its budget.
func (s *StepServer) hostAttempt(ctx context.Context, run *liveRun) (automation.Attempt, error) {
	st, err := s.Status(ctx)
	if err != nil {
		return automation.Attempt{}, fmt.Errorf("read the session status: %w", err)
	}
	if st.Attempt == nil || st.Attempt.ID != run.attempt {
		return automation.Attempt{}, fmt.Errorf("the attempt %s is no longer the member's open attempt", run.attempt)
	}
	a := automation.Attempt{ID: st.Attempt.ID, Phase: st.Attempt.Phase, TaskID: st.Attempt.Task.TaskID,
		BaseCommit: st.Attempt.BaseCommit, BudgetUSD: ReadTurnLimits(s.home).AttemptBudgetUSD}
	if t := readTurnState(s.home).Attempts[run.attempt]; t != nil {
		a.SpentUSD = t.CostUSD
	}
	return a, nil
}

// hostRun is run(): one command, no shell, in the attempt's worktree, under
// the turn's allow list, which the launcher checks again whatever the
// child checked. Its output, standard output and standard error together,
// comes back as bounded text.
func (s *StepServer) hostRun(ctx context.Context, run *liveRun, name string, args []string) (automation.RunResult, error) {
	if err := run.allow.Admit(name, args); err != nil {
		return automation.RunResult{}, err
	}
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = run.worktree
	cmd.Env = s.commandEnv()
	var out capped
	out.n = maxCommandOutput
	cmd.Stdout, cmd.Stderr = &out, &out
	cmd.WaitDelay = time.Second
	err := cmd.Run()
	var exit *exec.ExitError
	switch {
	case errors.As(err, &exit):
	case err != nil:
		return automation.RunResult{}, err
	}
	text, _ := automation.Bound(hostRedactor(s.home)(out.b.String()), automation.MaxRunOutput)
	return automation.RunResult{Exit: cmd.ProcessState.ExitCode(), Output: text}, nil
}

// commandEnv is the environment a run() command gets: the turn's, as the
// launcher gives its client.
func (s *StepServer) commandEnv() []string {
	env := s.env
	if env == nil {
		env = append(os.Environ(), gitEnv...)
	}
	return withoutCredentialHelper(env)
}

// withoutCredentialHelper is env with git's credential helpers reset for
// the process: an empty credential.helper given as GIT_CONFIG_* applies
// after every config file and empties the list the clone configured, so
// `git credential fill`, and any network git, finds no helper and fails:
// a declared command runs code from the attempt's head, and receives no
// credential through its environment or through git (dispatch.go). The
// code it runs keeps the member's OS-user access, as a worker turn's
// code does.
func withoutCredentialHelper(env []string) []string {
	n := 0
	out := make([]string, 0, len(env)+3)
	for _, kv := range env {
		if v, ok := strings.CutPrefix(kv, "GIT_CONFIG_COUNT="); ok {
			n, _ = strconv.Atoi(v)
			continue
		}
		out = append(out, kv)
	}
	return append(out, fmt.Sprintf("GIT_CONFIG_COUNT=%d", n+1),
		fmt.Sprintf("GIT_CONFIG_KEY_%d=credential.helper", n), fmt.Sprintf("GIT_CONFIG_VALUE_%d=", n))
}

// hostComment is comment(): body on the attempt's pull request, with the
// home's credential.
func (s *StepServer) hostComment(ctx context.Context, run *liveRun, body string) error {
	if strings.TrimSpace(body) == "" || len(body) > automation.MaxText {
		return fmt.Errorf("the body is empty or more than %d bytes", automation.MaxText)
	}
	t, err := s.runTarget(ctx, run)
	if err != nil {
		return err
	}
	if t.holds(body) {
		return errHoldsToken
	}
	if _, err := newAutomationForge().CommentPull(ctx, t.host, t.kind(), t.token, t.path, run.pull, body); err != nil {
		return fmt.Errorf("comment on pull request %d of %s on %s: %w", run.pull, t.path, t.host, err)
	}
	return nil
}

// The child's side: `aicrew-agent automation run`.

// AutomationChild runs the script stdin carries under the memory limit it
// names, its host calls going to the launcher over the step socket, and
// writes how it ended to stdout. It returns the process's exit code.
func AutomationChild(ctx context.Context, stdin io.Reader, stdout, stderr io.Writer) int {
	raw, err := io.ReadAll(io.LimitReader(stdin, 2<<20))
	var req automationRequest
	if err == nil {
		err = json.Unmarshal(raw, &req)
	}
	if err != nil || req.Version != 1 || req.Token == "" || req.Socket == "" || req.Memory <= 0 {
		fmt.Fprintln(stderr, "aicrew-agent automation run: the launcher starts this process and writes its request to its standard input")
		return 2
	}
	// The limit comes first: no part of the script runs without it.
	if err := limitMemory(req.Memory); err != nil {
		return reply(stdout, automationReply{Error: "the memory limit could not be set: " + err.Error()})
	}
	host := &socketHost{socket: req.Socket, token: req.Token}
	turn, err := automation.Run(ctx, automation.Script{Name: req.Name, Source: []byte(req.Source), Host: host,
		Allow: automation.NewAllowList(req.Allow, req.Deny), Limits: automation.Limits{Steps: req.Steps, Wall: req.Wall}}, req.Event)
	if err != nil {
		return reply(stdout, automationReply{Error: err.Error(), Invalid: errors.Is(err, automation.ErrInvalid)})
	}
	return reply(stdout, automationReply{Turn: turn})
}

func reply(w io.Writer, r automationReply) int {
	b, _ := json.Marshal(r)
	if _, err := w.Write(append(b, '\n')); err != nil {
		return 1
	}
	return 0
}

// socketHost is the script's host in the child: each call goes to the
// launcher over the step socket, with the run's token.
type socketHost struct {
	socket, token string
}

func (h *socketHost) call(ctx context.Context, c automationCall, out any) error {
	c.Token = h.token
	body, _ := json.Marshal(c)
	var d net.Dialer
	conn, err := d.DialContext(ctx, "unix", h.socket)
	if err != nil {
		return errors.New("the launcher is not reachable")
	}
	defer conn.Close()
	if dl, ok := ctx.Deadline(); ok {
		conn.SetDeadline(dl)
	}
	b, _ := json.Marshal(StepCall{Version: 1, Op: automationOp, Body: body})
	if _, err := conn.Write(append(b, '\n')); err != nil {
		return fmt.Errorf("the step channel: %w", err)
	}
	var ans StepAnswer
	if err := json.NewDecoder(io.LimitReader(conn, maxAutomationAnswer)).Decode(&ans); err != nil {
		return fmt.Errorf("the step channel's answer: %w", err)
	}
	if !ans.OK {
		if ans.Error != nil {
			return errors.New(ans.Error.Message)
		}
		return errors.New("the launcher refused the call")
	}
	if out == nil {
		return nil
	}
	return json.Unmarshal(ans.Result, out)
}

// automationOp is the step channel's op for a script's host calls.
const automationOp = "automation"

func (h *socketHost) PR(ctx context.Context) (automation.PR, error) {
	var p automation.PR
	return p, h.call(ctx, automationCall{Call: "pr"}, &p)
}

func (h *socketHost) Attempt(ctx context.Context) (automation.Attempt, error) {
	var a automation.Attempt
	return a, h.call(ctx, automationCall{Call: "attempt"}, &a)
}

func (h *socketHost) Run(ctx context.Context, cmd string, args []string) (automation.RunResult, error) {
	var r automation.RunResult
	return r, h.call(ctx, automationCall{Call: "run", Cmd: cmd, Args: args}, &r)
}

func (h *socketHost) Step(ctx context.Context, kind, text string) error {
	return h.call(ctx, automationCall{Call: "step", Kind: kind, Text: text}, nil)
}

func (h *socketHost) Comment(ctx context.Context, body string) error {
	return h.call(ctx, automationCall{Call: "comment", Text: body}, nil)
}

func (h *socketHost) Label(ctx context.Context, add, remove []string) error {
	return h.call(ctx, automationCall{Call: "label", Add: add, Remove: remove}, nil)
}

func (h *socketHost) Ready(ctx context.Context) error {
	return h.call(ctx, automationCall{Call: "ready"}, nil)
}

func (h *socketHost) Submit(ctx context.Context, u string) error {
	return h.call(ctx, automationCall{Call: "submit", URL: u}, nil)
}

func (h *socketHost) Rerun(ctx context.Context) (automation.RerunResult, error) {
	var r automation.RerunResult
	return r, h.call(ctx, automationCall{Call: "rerun"}, &r)
}

func (h *socketHost) UpdateBase(ctx context.Context) (automation.UpdateBaseResult, error) {
	var r automation.UpdateBaseResult
	return r, h.call(ctx, automationCall{Call: "update_base"}, &r)
}
