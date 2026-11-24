package main

// The turn mode (docs/DESIGN-PILOT-SIMULATION.md, sections 2.2 to 2.4): the
// launcher runs `claude -p --output-format stream-json ... --session-id ID`
// in the home with `/crew-turn <brief>` on standard input. The scripted
// member reads its role and attempt from the brief and its messages from
// `aicrew-agent inbox --json`, acts only through the product's surfaces
// (aicrew-agent's steps, clone and forge commands, git in its worktree, the
// home's aimem MCP server), acks the messages it read, records the turn in
// docs/HANDOFF.md, and ends with one stream-json result line.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/BlackVS/aicrew/internal/agent"
	"github.com/BlackVS/aicrew/internal/automation"
	"github.com/BlackVS/aicrew/internal/forge"
	"github.com/BlackVS/aicrew/internal/store"
)

// turnMode reports the launcher's turn: print mode with a session ID.
func turnMode(args []string) bool {
	return printMode(args) && slices.Contains(args, "--session-id")
}

// brief is what the scripted member reads of its brief (internal/agent,
// brief.go): the first line and the "Who and where" section.
type brief struct {
	Attempt, State, Phase string
	Role, Team, Agent     string
	Task, Project, Hub    string
	Branch, Base, Repo    string
	// Continuation is a turn the turn loop started with no new message:
	// the brief's "Why this turn" is a continuation of the attempt.
	Continuation bool
}

var (
	briefFirst  = regexp.MustCompile(`^Attempt (\S+): ([^/\s]+)/([^.\s]+)\.$`)
	briefRole   = regexp.MustCompile(`(?m)^- Role: (\S+); team (\S*); agent (\S*)\.$`)
	briefTask   = regexp.MustCompile(`(?m)^- Attempt (\S+) on task (\S+) \(project (\S+), hub (\S+)\)\.$`)
	briefBranch = regexp.MustCompile("(?m)^- Branch (\\S+) from base commit (\\S+); worktree `")
	briefRepo   = regexp.MustCompile(`(?m)^- Repository (\S+) \(default branch \S+\)\.$`)
	briefCont   = regexp.MustCompile(`(?m)^## Why this turn\n\na continuation of attempt `)
)

// parseBrief reads the brief after the /crew-turn command.
func parseBrief(prompt string) (brief, error) {
	text, ok := strings.CutPrefix(prompt, "/crew-turn ")
	if !ok {
		return brief{}, errors.New("the prompt is not /crew-turn")
	}
	first, _, _ := strings.Cut(text, "\n")
	var b brief
	switch m := briefFirst.FindStringSubmatch(first); {
	case m != nil:
		b.Attempt, b.State, b.Phase = m[1], m[2], m[3]
	case first != "No open attempt.":
		return brief{}, fmt.Errorf("the brief's first line is neither an attempt nor none: %q", first)
	}
	m := briefRole.FindStringSubmatch(text)
	if m == nil {
		return brief{}, errors.New("the brief names no role")
	}
	b.Role, b.Team, b.Agent = m[1], m[2], m[3]
	if b.Attempt == "" {
		return b, nil
	}
	if m := briefTask.FindStringSubmatch(text); m != nil && m[1] == b.Attempt {
		b.Task, b.Project, b.Hub = m[2], m[3], m[4]
	}
	if m := briefBranch.FindStringSubmatch(text); m != nil {
		b.Branch, b.Base = m[1], m[2]
	}
	if m := briefRepo.FindStringSubmatch(text); m != nil {
		b.Repo = m[1]
	}
	b.Continuation = briefCont.MatchString(text)
	return b, nil
}

// message is what the scripts read of an inbox message (internal/store,
// InboxItem).
type message struct {
	ID        string         `json:"id"`
	Kind      string         `json:"kind"`
	AttemptID string         `json:"attempt_id"`
	Task      *agent.TaskRef `json:"task"`
	Project   *struct {
		HubID     string `json:"hub_id"`
		ProjectID string `json:"project_id"`
	} `json:"project"`
	Offer *struct {
		InstructionDigest string `json:"instruction_digest"`
	} `json:"offer"`
	// Escalation is an answered escalation's detail, on the answer's
	// lifecycle message.
	Escalation *struct {
		ID       string `json:"id"`
		Decision string `json:"decision"`
	} `json:"escalation"`
	Action *struct {
		Subjects []struct {
			Kind      string         `json:"kind"`
			AttemptID string         `json:"attempt_id"`
			Task      *agent.TaskRef `json:"task"`
		} `json:"subjects"`
	} `json:"action"`
	Text string `json:"text"`
}

// runFunc runs a command in dir with stdin and returns its standard output
// and exit code; err is a command that could not run.
type runFunc func(ctx context.Context, dir, stdin, name string, args ...string) ([]byte, int, error)

// mcpFunc calls one tool of the home's aimem MCP server and returns its text.
type mcpFunc func(ctx context.Context, tool string, args map[string]any) (string, error)

// member is one turn of the scripted member.
type member struct {
	home string
	// settings is the turn's --settings file, which holds the allow list a
	// model's commands would run under.
	settings string
	run      runFunc
	mcp      mcpFunc
	now      func() time.Time
	sc       scenario
	// scErr is an unreadable scenario, which fails the turn rather than
	// running the plain cycle in its place.
	scErr error
	did   []string // what the turn did, in order
}

// scenarioFile is where the runner puts the member's scenario, outside the
// home: what a model would do differently in a pilot case
// (docs/DESIGN-PILOT-SIMULATION.md, 6.2). The runner changes it only while
// the member is paused and its turn has ended.
const scenarioFile = "/sim/scenario/member.json"

// scenario is the member's behaviour in the pilot cases; the zero scenario
// is the plain cycle.
type scenario struct {
	// NeverAck leaves every message unacknowledged (P1).
	NeverAck bool `json:"never_ack"`
	// PushDuringReview makes the worker push one more commit while
	// hands-reviewing is on its pull request (P2).
	PushDuringReview bool `json:"push_during_review"`
	// Stall makes the worker's turns in a working attempt do nothing: no
	// step, no commit and no handoff change (P4).
	Stall bool `json:"stall"`
	// Result ends every turn with a client error instead of the script:
	// "login" (the provider's 401) or "model" (its 404) (P3).
	Result string `json:"result"`
	// CostUSD is the client's running total the result reports (P3).
	CostUSD float64 `json:"cost_usd"`
	// EscalateBlocked makes the coordinator raise an escalation on a
	// blocked attempt instead of answering it, and give the worker the
	// architect's decision once the answer arrives (E1).
	EscalateBlocked bool `json:"escalate_blocked"`
	// SubmitItself keeps the worker's own submit, before its review, where
	// its guidance leaves the submit to the launcher: case F1's faults are
	// on the worker's own submit (01a206f8-24d0).
	SubmitItself bool `json:"submit_itself"`
	// SubmitAnyway makes the worker submit its pull request once more after
	// the launcher submitted it, as a worker that ignores its guidance
	// would, and record the answer (01a206f8-24d0).
	SubmitAnyway bool `json:"submit_anyway"`
}

// readScenario reads path; a missing file is the zero scenario.
func readScenario(path string) (scenario, error) {
	var sc scenario
	raw, err := os.ReadFile(path)
	switch {
	case errors.Is(err, os.ErrNotExist):
		return sc, nil
	case err != nil:
		return sc, err
	}
	if err := json.Unmarshal(raw, &sc); err != nil {
		return sc, fmt.Errorf("%s: %w", path, err)
	}
	if sc.Result != "" && sc.Result != "login" && sc.Result != "model" {
		return sc, fmt.Errorf("%s: result %q is neither login nor model", path, sc.Result)
	}
	return sc, nil
}

func (m *member) note(format string, a ...any) { m.did = append(m.did, fmt.Sprintf(format, a...)) }

// agentJSON runs `aicrew-agent ARGS --json` in dir and decodes its answer;
// args that name --json themselves place it before their positional
// arguments. Exit 4 (recorded, not settled) is not an error: `step
// recover` settles it.
func (m *member) agentJSON(ctx context.Context, dir, stdin string, out any, args ...string) error {
	if !slices.Contains(args, "--json") {
		args = append(args, "--json")
	}
	raw, code, err := m.run(ctx, dir, stdin, "aicrew-agent", args...)
	if err != nil {
		return fmt.Errorf("aicrew-agent %s: %w", args[0], err)
	}
	if code != 0 && code != 4 {
		return fmt.Errorf("aicrew-agent %s exited %d: %s", strings.Join(args[:min(2, len(args))], " "), code,
			strings.TrimSpace(string(raw)))
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("aicrew-agent %s: the answer is not JSON: %w", args[0], err)
	}
	return nil
}

// retryLater is a step whose outcome the turn cannot build on: aimem did
// not commit it, or it is pending, undecided. The turn ends without
// acknowledging its messages, so a later turn reads them again and begins
// the step again (cases F1 and F2, docs/E2E-PILOT-SIM.md).
type retryLater struct {
	op, why string
	pending bool
}

func (e *retryLater) Error() string { return "step " + e.op + " " + e.why }

// reservationOp is the aimem operation a member's step sends.
var reservationOp = map[string]string{"offer": "claim", "claim": "claim", "accept": "transfer", "work": "update",
	"finalize": "finalize", "withdraw": "release", "release": "release"}

// step runs one attempt step. A step that did not commit, or is pending, is
// a *retryLater with its answer. A step the channel failed (aicrewd's or
// the launcher's answer lost) is finished with `step recover`, which
// replays it under its recorded key.
func (m *member) step(ctx context.Context, op, attempt, task string, body any, extra ...string) (agent.StepAnswer, error) {
	ans, code, err := m.stepOnce(ctx, op, attempt, task, body, extra...)
	if err != nil {
		return ans, err
	}
	var res agent.StepResult
	_ = json.Unmarshal(ans.Result, &res)
	switch {
	case code == 0:
		return ans, nil
	case code == 4:
		return ans, &retryLater{op: op, why: "is pending, undecided", pending: true}
	case code == 3 && ans.Status == agent.StepRefused && res.Settled && res.Outcome != "committed":
		return ans, &retryLater{op: op, why: fmt.Sprintf("did not commit (%s %s)", res.Outcome, res.Report.Code)}
	case code == 1 && ans.Error != nil && ans.Error.Retryable:
		return m.recoverStep(ctx, op, attempt, ans)
	}
	why := ans.Status
	if ans.Error != nil {
		why = ans.Error.Code + ": " + ans.Error.Message
	}
	return ans, fmt.Errorf("step %s exited %d: %s", op, code, why)
}

// recoverStep finishes the recorded steps after op failed on the channel,
// and answers for op's own: the recovered step of its operation, and of
// its attempt when the step names one (every step but offer and claim).
func (m *member) recoverStep(ctx context.Context, op, attempt string, failed agent.StepAnswer) (agent.StepAnswer, error) {
	ans, code, err := m.stepOnce(ctx, "recover", "", "", nil)
	if err != nil {
		return ans, err
	}
	var results []agent.StepResult
	_ = json.Unmarshal(ans.Result, &results)
	// Recover finishes every recorded step: an older one of the same
	// operation that did not commit may be among them, so a committed
	// result is the failed step's, then a pending one.
	rank := func(r agent.StepResult) int {
		switch {
		case r.Settled && r.Outcome == "committed":
			return 3
		case !r.Settled:
			return 2
		}
		return 1
	}
	var best *agent.StepResult
	for i, r := range results {
		if r.Operation == reservationOp[op] && (attempt == "" || r.AttemptID == attempt) && (best == nil || rank(r) > rank(*best)) {
			best = &results[i]
		}
	}
	if best == nil {
		return ans, fmt.Errorf("step %s failed (%s), and step recover (exit %d) finished no %s step", op, failed.Error.Code,
			code, reservationOp[op])
	}
	raw, _ := json.Marshal(best)
	one := agent.StepAnswer{OK: true, Status: agent.StepDone, Result: raw}
	switch rank(*best) {
	case 3:
		m.note("Step %s failed (%s); step recover finished it: committed.", op, failed.Error.Code)
		return one, nil
	case 2:
		return one, &retryLater{op: op, why: "is pending after step recover", pending: true}
	}
	return one, &retryLater{op: op, why: fmt.Sprintf("did not commit after step recover (%s %s)", best.Outcome, best.Report.Code)}
}

// stepOnce runs `aicrew-agent step OP` and decodes its answer.
func (m *member) stepOnce(ctx context.Context, op, attempt, task string, body any, extra ...string) (agent.StepAnswer, int, error) {
	args := []string{"step", op}
	if attempt != "" {
		args = append(args, "--attempt", attempt)
	}
	if task != "" {
		args = append(args, "--task", task)
	}
	if body != nil {
		raw, _ := json.Marshal(body)
		args = append(args, "--body", string(raw))
	}
	args = append(args, extra...)
	raw, code, err := m.run(ctx, m.home, "", "aicrew-agent", args...)
	if err != nil {
		return agent.StepAnswer{}, code, fmt.Errorf("step %s: %w", op, err)
	}
	var ans agent.StepAnswer
	if err := json.Unmarshal(raw, &ans); err != nil {
		return ans, code, fmt.Errorf("step %s: the answer is not JSON (exit %d): %.400s", op, code, strings.TrimSpace(string(raw)))
	}
	return ans, code, nil
}

// inbox reads the member's unacknowledged messages.
func (m *member) inbox(ctx context.Context) ([]message, error) {
	var ans agent.StepAnswer
	if err := m.agentJSON(ctx, m.home, "", &ans, "inbox"); err != nil {
		return nil, err
	}
	var res struct {
		Messages []message `json:"messages"`
	}
	if err := json.Unmarshal(ans.Result, &res); err != nil {
		return nil, fmt.Errorf("inbox: %w", err)
	}
	return res.Messages, nil
}

// ack acknowledges the messages the turn read: each is handled, or not the
// script's to act on.
func (m *member) ack(ctx context.Context, msgs []message) error {
	if len(msgs) == 0 {
		return nil
	}
	ids := make([]string, len(msgs))
	for i, msg := range msgs {
		ids[i] = msg.ID
	}
	_, code, err := m.run(ctx, m.home, "", "aicrew-agent", "inbox", "--ack", strings.Join(ids, ","))
	if err != nil || code != 0 {
		return fmt.Errorf("inbox --ack exited %d: %v", code, err)
	}
	m.note("Acked %d message(s).", len(ids))
	return nil
}

// turn runs one turn: the role's script, the acks and the handoff.
func (m *member) turn(ctx context.Context, b brief) error {
	if m.sc.Stall && b.Role == "worker" && b.State == "running" && b.Phase == "working" {
		return nil // no step, no commit, the handoff as it was (P4)
	}
	msgs, err := m.inbox(ctx)
	if err != nil {
		return err
	}
	switch b.Role {
	case "worker":
		err = m.worker(ctx, b, msgs)
	case "coordinator":
		err = m.coordinator(ctx, msgs)
	case "independent":
		// The runner drives the independent member's steps, as b4's
		// harness does (cases F2-settle, F3-launcher, F4 and F5): its turns
		// only acknowledge what reached it.
	default:
		err = fmt.Errorf("no script for the role %q", b.Role)
	}
	var later *retryLater
	switch {
	case errors.As(err, &later):
		m.note("The %s; %d message(s) left unacknowledged, for a later turn to begin it again.", later.Error(), len(msgs))
		err = nil
	case err == nil && m.sc.NeverAck && len(msgs) > 0:
		m.note("Left %d message(s) unacknowledged.", len(msgs))
	case err == nil:
		err = m.ack(ctx, msgs)
	}
	if err == nil && len(m.did) == 0 {
		m.note("Nothing to do.")
	}
	// What a failed turn did is recorded too: the next turn goes on from it.
	if len(m.did) > 0 {
		if herr := m.recordHandoff(b); err == nil {
			err = herr
		}
	}
	return err
}

// ---- the worker ----------------------------------------------------------------

func (m *member) worker(ctx context.Context, b brief, msgs []message) error {
	if b.Attempt == "" || b.State == "offered" {
		// The brief names the offer that is open: older offers in the
		// inbox were withdrawn or closed since (the fault cases leave
		// such offers behind while the worker is held).
		for _, msg := range msgs {
			if msg.Offer == nil || msg.AttemptID == "" || msg.Task == nil || (b.Attempt != "" && msg.AttemptID != b.Attempt) {
				continue
			}
			if _, err := m.step(ctx, "accept", msg.AttemptID, msg.Task.TaskID,
				map[string]string{"instruction_digest": msg.Offer.InstructionDigest}); err != nil {
				return err
			}
			m.note("Accepted the offer of attempt %s on task %s.", msg.AttemptID, msg.Task.TaskID)
			return nil
		}
		return nil
	}
	if b.State == "running" && b.Phase == "blocked" {
		if !answered(msgs, b.Attempt) {
			m.note("Attempt %s is blocked: waiting for the coordinator's answer.", b.Attempt)
			return nil
		}
		if _, err := m.step(ctx, "work", b.Attempt, b.Task, map[string]string{"intent": "resume"}); err != nil {
			return err
		}
		m.note("Resumed attempt %s on the coordinator's answer.", b.Attempt)
	} else if b.State == "running" && b.Phase == "accepted" {
		launcher, err := m.launcherDelivers()
		if err != nil {
			return err
		}
		if launcher {
			return m.finalizeConfirmed(ctx, b, msgs)
		}
		m.note("Attempt %s is accepted: the coordinator confirms its delivery and finalizes it.", b.Attempt)
		return nil
	} else if b.State == "running" && b.Phase == "submitted" && m.sc.SubmitAnyway &&
		!strings.Contains(m.section(), submittedAnyway) {
		return m.submitAnyway(ctx, b)
	} else if b.State != "running" || (b.Phase != "working" && b.Phase != "rework") {
		m.note("Attempt %s is %s/%s: nothing for the worker to do.", b.Attempt, b.State, b.Phase)
		return nil
	}
	if b.Phase == "rework" {
		if _, err := m.step(ctx, "work", b.Attempt, b.Task, map[string]string{"intent": "resume"}); err != nil {
			return err
		}
		m.note("Resumed attempt %s for the coordinator's rework.", b.Attempt)
	}
	if err := m.admitsTests(); err != nil {
		return err
	}
	wt := filepath.Join(m.home, "worktrees", b.Attempt)
	if _, err := os.Stat(wt); errors.Is(err, os.ErrNotExist) {
		var out any
		if err := m.agentJSON(ctx, m.home, "", &out, "clone", "--repository", b.Repo, "--attempt", b.Attempt,
			"--base", b.Base, "--branch", b.Branch); err != nil {
			return err
		}
		m.note("Cloned %s and made the worktree on %s from %s.", b.Repo, b.Branch, b.Base)
	}
	// Once per attempt, recorded apart from the worktree: a turn whose probe
	// failed after the clone leaves no record, so the next turn probes again
	// before it commits.
	probed := filepath.Join(m.home, "state", "simclient-go-"+b.Attempt)
	if _, err := os.Stat(probed); err != nil {
		if err := m.goVersion(ctx, wt); err != nil {
			return err
		}
		if err := os.MkdirAll(filepath.Dir(probed), 0o700); err != nil {
			return fmt.Errorf("record the go probe: %w", err)
		}
		if err := os.WriteFile(probed, nil, 0o600); err != nil {
			return fmt.Errorf("record the go probe: %w", err)
		}
	}
	n := recordedPull(m.section())
	if n == 0 {
		return m.openPull(ctx, b, wt)
	}
	return m.followPull(ctx, b, wt, n)
}

// errTestsDenied is a worker's turn whose settings would deny the
// project's tests: the operator selected no toolchain for the project.
var errTestsDenied = errors.New("the turn's settings deny `go test ./...`: no toolchain is selected for the " +
	"project (`aicrew team toolchain`, docs/BRING-UP.md)")

// admitsTests checks that the turn's settings admit the project's test
// command, `go test ./...`, as the turn's allow and deny rules judge it,
// before the worker commits: a model would run it here. The simulation
// runs no tests; it proves the selection reaches the turn.
func (m *member) admitsTests() error {
	if m.settings == "" {
		return errors.New("the turn names no --settings file")
	}
	raw, err := os.ReadFile(m.settings)
	if err != nil {
		return fmt.Errorf("the turn's settings: %w", err)
	}
	var s struct {
		Permissions struct {
			Allow []string `json:"allow"`
			Deny  []string `json:"deny"`
		} `json:"permissions"`
	}
	if err := json.Unmarshal(raw, &s); err != nil {
		return fmt.Errorf("the turn's settings %s: %w", m.settings, err)
	}
	if automation.NewAllowList(s.Permissions.Allow, s.Permissions.Deny).Admit("go", []string{"test", "./..."}) != nil {
		return errTestsDenied
	}
	return nil
}

// goVersion runs `go version` in the worktree, in the turn's environment:
// the worker's host has the toolchain's tool.
func (m *member) goVersion(ctx context.Context, wt string) error {
	out, code, err := m.run(ctx, wt, "", "go", "version")
	if err != nil || code != 0 {
		return fmt.Errorf("`go version` did not run in the worktree: exit %d, %v: %s", code, err,
			strings.TrimSpace(string(out)))
	}
	m.note("Ran `go version` in the worktree: %s.", strings.TrimSpace(string(out)))
	return nil
}

// answered says whether the messages hold the coordinator's answer to the
// attempt's blocked report.
func answered(msgs []message, attempt string) bool {
	for _, msg := range msgs {
		if msg.Action == nil {
			continue
		}
		for _, s := range msg.Action.Subjects {
			if s.Kind == "answer" && s.AttemptID == attempt {
				return true
			}
		}
	}
	return false
}

// openPull commits the task's change, pushes it with the home's credential,
// opens the pull request and asks for the external review.
func (m *member) openPull(ctx context.Context, b brief, wt string) error {
	file := filepath.Join("sim", b.Task+".md")
	if err := os.MkdirAll(filepath.Join(wt, "sim"), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(wt, file), []byte("Task "+b.Task+", attempt "+b.Attempt+".\n"), 0o644); err != nil {
		return err
	}
	if err := m.commitAndPush(ctx, wt, b.Branch, file, "sim: task "+b.Task); err != nil {
		return err
	}
	var opened agent.PullReport
	text := "The pilot simulation's change for task " + b.Task + " (attempt " + b.Attempt + ").\n"
	stdin, body := text, "-"
	if m.reviewCommand() {
		// This release's guidance and wait: the text from a file in the
		// worktree. A home of an earlier release counts such a file as
		// uncommitted work and does not wait on its gates, so there the
		// stand-in posts on standard input, as it did then.
		path, err := bodyFile(wt, pullBodyFile, text)
		if err != nil {
			return err
		}
		stdin, body = "", path
	}
	if err := m.agentJSON(ctx, wt, stdin, &opened, "forge", "pr", "open", "--title", "sim: task "+b.Task,
		"--body-file", body); err != nil {
		return err
	}
	m.note("Pull request #%d: %s, opened at head %s.", opened.Pull.Number, opened.Pull.URL, opened.Pull.HeadCommit)
	return m.requestReview(ctx, wt, opened.Pull.Number, false)
}

// commitAndPush commits file in the worktree and pushes the branch with the
// home's credential.
func (m *member) commitAndPush(ctx context.Context, wt, branch, file, subject string) error {
	for _, args := range [][]string{
		{"add", filepath.ToSlash(file)},
		{"-c", "user.name=sim-worker", "-c", "user.email=sim-worker@example.test", "-c", "commit.gpgsign=false",
			"commit", "-q", "-m", subject},
		{"push", "-q", "origin", branch},
	} {
		if out, code, err := m.run(ctx, wt, "", "git", args...); err != nil || code != 0 {
			return fmt.Errorf("git %s exited %d: %v %s", args[len(args)-1], code, err, strings.TrimSpace(string(out)))
		}
	}
	return nil
}

// rework answers why the head cannot be submitted: a new commit, then a
// new request for the external review unless one is still running.
func (m *member) rework(ctx context.Context, b brief, wt string, n int64, why string) error {
	file := filepath.Join("sim", b.Task+".md")
	f, err := os.OpenFile(filepath.Join(wt, file), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	_, err = f.WriteString("Reworked: " + why + ".\n")
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return err
	}
	if err := m.commitAndPush(ctx, wt, b.Branch, file, "sim: rework of task "+b.Task); err != nil {
		return err
	}
	m.note("Reworked #%d: %s; pushed a new commit.", n, why)
	if why == pushedInReview {
		// The reviewer is reading the old head: the request for the new
		// one waits until its label is off (P2).
		return nil
	}
	return m.requestReview(ctx, wt, n, true)
}

// requestReview asks for the external review: the review-this label, with a
// stale hands-reviewed taken off first.
func (m *member) requestReview(ctx context.Context, wt string, n int64, stale bool) error {
	var labelled agent.PullReport
	if stale {
		if err := m.agentJSON(ctx, wt, "", &labelled, "forge", "pr", "label", "remove", "--pr", fmt.Sprint(n), "--json",
			"hands-reviewed"); err != nil {
			return err
		}
	}
	if err := m.agentJSON(ctx, wt, "", &labelled, "forge", "pr", "label", "add", "--pr", fmt.Sprint(n), "--json",
		"review-this"); err != nil {
		return err
	}
	m.note("Asked for the external review of #%d (review-this)%s.", n, untold(labelled.Watch))
	return nil
}

// followPull reads the pull request and submits it once the external review
// is READY at its head and its checks passed; until then it waits.
func (m *member) followPull(ctx context.Context, b brief, wt string, n int64) error {
	var shown agent.PullReport
	if err := m.agentJSON(ctx, wt, "", &shown, "forge", "pr", "show", "--pr", fmt.Sprint(n)); err != nil {
		return err
	}
	p := shown.Pull
	verdict, checks := pullGates(shown.Content)
	asked := slices.Contains(p.Labels, "review-this") || slices.Contains(p.Labels, "hands-reviewing")
	switch {
	case p.State != forge.PullOpen:
		m.note("Pull request #%d is %s.", n, p.State)
	case m.sc.PushDuringReview && slices.Contains(p.Labels, "hands-reviewing") &&
		!strings.Contains(m.section(), pushedInReview):
		return m.rework(ctx, b, wt, n, pushedInReview) // once per attempt
	case asked:
		mark := ""
		if b.Continuation && m.gateWaitHolds(ctx, wt, n, p.HeadCommit, checks, shown.Content) {
			mark = continuationWait
		}
		m.note("Waiting on the external review of #%d at %s (labels %s)%s%s.", n, p.HeadCommit, strings.Join(p.Labels, ", "),
			untold(shown.Watch), mark)
	case checks == forge.ChecksFailure:
		return m.rework(ctx, b, wt, n, "the checks failed at "+p.HeadCommit)
	case verdict == "RETURN_TO_IMPLEMENTATION":
		return m.rework(ctx, b, wt, n, "the external review returned it")
	case verdict == "":
		// No review at this head: a hands-reviewed label is an older head's.
		return m.requestReview(ctx, wt, n, slices.Contains(p.Labels, "hands-reviewed"))
	case verdict == "READY_FOR_HUMAN_MERGE" && checks == forge.ChecksSuccess:
		// The guidance's order where the launcher submits: the worker's own
		// review, and no submit of its own (01a206f8-24d0).
		if m.reviewCommand() && !m.sc.SubmitItself {
			launcher, err := m.gatesGreenInSet(ctx)
			if err != nil {
				return err
			}
			if launcher {
				if ownReviewAt(shown.Content) {
					m.note("Waiting for the launcher's submit of #%d at %s.", n, p.HeadCommit)
					return nil
				}
				if err := m.postOwnReview(ctx, wt, n, p.HeadCommit); err != nil {
					return err
				}
				m.note("Posted the review of #%d at %s; the launcher submits it, since gates-green is in the set.", n,
					p.HeadCommit)
				return nil
			}
		}
		body := map[string]string{"intent": "submit", "detail": p.URL}
		key := pendingSubmit(m.section())
		if key != "" {
			// An update aimem never decided stays pending while the hold has
			// not moved: the holder supersedes it (case F1).
			body["supersedes"] = key
		}
		ans, err := m.step(ctx, "work", b.Attempt, b.Task, body)
		var later *retryLater
		if errors.As(err, &later) && later.pending {
			var res agent.StepResult
			if json.Unmarshal(ans.Result, &res) == nil && res.RequestKey != "" {
				m.note("Submit of #%d is pending, undecided: request key %s.", n, res.RequestKey)
				return nil
			}
		}
		if err != nil {
			return err
		}
		m.note("Submitted #%d (%s): the external review is READY at %s and the checks passed.", n, p.URL, p.HeadCommit)
		if key != "" {
			m.note("The submit superseded the pending one, request key %s.", key)
		}
		// The worker's own review comes after its submit: a READY local
		// review before it would let the launcher's gates-green automation
		// submit the attempt beside the stand-in's own step. Once the
		// attempt is submitted, the automation does nothing. A home of an
		// earlier release has no review command, and its worker posts no
		// review of its own, as the stand-in did then.
		if m.reviewCommand() && !ownReviewAt(shown.Content) {
			if err := m.postOwnReview(ctx, wt, n, p.HeadCommit); err != nil {
				return err
			}
		}
	default:
		m.note("Pull request #%d at %s: external verdict %q, checks %q; waiting.", n, p.HeadCommit, verdict, checks)
	}
	return nil
}

// pullGates is the external review's verdict at the pull request's head
// ("" for none) and its checks' combined state ("" for none).
func pullGates(c *forge.PullContent) (verdict, checks string) {
	if c == nil {
		return "", ""
	}
	for _, cm := range c.Comments {
		if r := cm.Review; r != nil && r.Reviewer == forge.ReviewerExternal && r.AtHead {
			verdict = r.Verdict // the latest at the head wins
		}
	}
	for _, ch := range c.Checks {
		switch {
		case ch.State == forge.ChecksFailure:
			return verdict, forge.ChecksFailure
		case ch.State != forge.ChecksSuccess:
			checks = forge.ChecksPending
		case checks == "":
			checks = forge.ChecksSuccess
		}
	}
	return verdict, checks
}

var (
	pullLine    = regexp.MustCompile(`(?m)^- \S+ Pull request #(\d+): `)
	submitLines = regexp.MustCompile(`(?m)^- \S+ (?:Submit of #\d+ is pending, undecided: request key (\S+)\.|Submitted #\d+ )`)
)

// pendingSubmit is the request key of the worker's last submit when the
// section records it pending and no submit after it; "" otherwise.
func pendingSubmit(section string) string {
	all := submitLines.FindAllStringSubmatch(section, -1)
	if len(all) == 0 {
		return ""
	}
	return all[len(all)-1][1]
}

// recordedPull is the pull request number the worker recorded in its
// section's Done, 0 for none.
func recordedPull(section string) int64 {
	var n int64
	if m := pullLine.FindStringSubmatch(section); m != nil {
		fmt.Sscan(m[1], &n)
	}
	return n
}

// ---- the coordinator -------------------------------------------------------------

var taskID = regexp.MustCompile(`\b[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}\b`)

// candidates are the tasks the messages name: a board change's, or those a
// READY list names in its text, in the order read.
func candidates(msgs []message) []agent.TaskRef {
	var out []agent.TaskRef
	add := func(t agent.TaskRef) {
		if t.TaskID != "" && !slices.Contains(out, t) {
			out = append(out, t)
		}
	}
	for _, msg := range msgs {
		if msg.Action != nil {
			for _, s := range msg.Action.Subjects {
				if s.Kind == "board" && s.Task != nil {
					add(*s.Task)
				}
			}
		}
		if msg.Kind == "lifecycle" && msg.Project != nil && msg.AttemptID == "" {
			for _, id := range taskID.FindAllString(msg.Text, -1) {
				add(agent.TaskRef{HubID: msg.Project.HubID, ProjectID: msg.Project.ProjectID, TaskID: id})
			}
		}
	}
	return out
}

func (m *member) coordinator(ctx context.Context, msgs []message) error {
	tasks, results, blocked := candidates(msgs), submits(msgs), blockedReports(msgs)
	var ans agent.StepAnswer
	if err := m.agentJSON(ctx, m.home, "", &ans, "attempts"); err != nil {
		return err
	}
	var open agent.OpenAttempts
	if err := json.Unmarshal(ans.Result, &open); err != nil {
		return fmt.Errorf("attempts: %w", err)
	}
	phase := map[string]agent.OpenAttempt{}
	for _, a := range open.Attempts {
		phase[a.ID] = a
	}
	for _, r := range results {
		if err := m.reviewResult(ctx, r, phase[r.attempt]); err != nil {
			return err
		}
	}
	for _, id := range blocked {
		a, ok := phase[id]
		if !ok || a.Phase != "blocked" {
			continue
		}
		if m.sc.EscalateBlocked {
			if err := m.escalateBlocked(ctx, a, msgs); err != nil {
				return err
			}
			continue
		}
		if _, err := m.step(ctx, "answer", id, "", map[string]string{"text": stallAnswer}); err != nil {
			return err
		}
		m.note("Answered the blocked report of attempt %s.", id)
	}
	for _, ea := range escalationAnswers(msgs) {
		if a, ok := phase[ea.attempt]; ok && a.Phase == "blocked" {
			if _, err := m.step(ctx, "answer", ea.attempt, "", map[string]string{"text": ea.decision}); err != nil {
				return err
			}
			m.note("Gave the blocked attempt %s the architect's answer to escalation %s.", ea.attempt, ea.id)
		}
	}
	// The stand-in follows its home's guidance. Where the launcher checks
	// the delivery (task 01a1fa19-19a4), an accepted attempt's turn has
	// nothing to run, and the worker finalizes. A home of an earlier release
	// tells its coordinator to confirm the delivery and finalize in a turn:
	// every accepted attempt is checked each turn, so a delivery
	// verify-delivery left pending is confirmed on a later turn.
	launcher, err := m.launcherDelivers()
	if err != nil {
		return err
	}
	if !launcher {
		for _, a := range open.Attempts {
			if a.Phase == "accepted" {
				if err := m.deliver(ctx, a); err != nil {
					return err
				}
			}
		}
	} else if err := m.launcherDelivered(ctx, open, msgs); err != nil {
		return err
	}
	for _, t := range tasks {
		if slices.ContainsFunc(open.Attempts, func(a agent.OpenAttempt) bool { return a.Task.TaskID == t.TaskID }) {
			m.note("Task %s has an open attempt.", t.TaskID)
			continue
		}
		if err := m.triageAndOffer(ctx, t); err != nil {
			return err
		}
	}
	return nil
}

// boardTask is what the coordinator reads of a task.
type boardTask struct {
	Revision   int64  `json:"revision"`
	State      string `json:"state"`
	NextAction string `json:"next_action"`
}

// offerNext is the next action the coordinator's triage sets; a READY task
// that has it is triaged already.
const offerNext = "Offer it to the team's worker."

func (m *member) readTask(ctx context.Context, id string) (boardTask, error) {
	text, err := m.mcp(ctx, "get_task", map[string]any{"id": id})
	if err != nil {
		return boardTask{}, err
	}
	var t boardTask
	if err := json.Unmarshal([]byte(text), &t); err != nil || t.Revision < 1 {
		return boardTask{}, fmt.Errorf("get_task %s: not a task: %.200s", id, text)
	}
	return t, nil
}

// triageAndOffer triages a BACKLOG or READY task to READY, with its next
// action and a comment, and offers it.
func (m *member) triageAndOffer(ctx context.Context, t agent.TaskRef) error {
	task, err := m.readTask(ctx, t.TaskID)
	if err != nil {
		return err
	}
	if task.State == "BACKLOG" || (task.State == "READY" && task.NextAction != offerNext) {
		key := fmt.Sprintf("sim-triage-%s-%d", t.TaskID, task.Revision)
		text, err := m.mcp(ctx, "triage_task", map[string]any{"id": t.TaskID, "expected_revision": task.Revision,
			"state": "READY", "next_action": offerNext, "idempotency_key": key})
		if err != nil {
			return err
		}
		if err := json.Unmarshal([]byte(text), &task); err != nil || task.State != "READY" {
			return fmt.Errorf("triage_task %s: the task is not READY: %.200s", t.TaskID, text)
		}
		if _, err := m.mcp(ctx, "add_task_comment", map[string]any{"id": t.TaskID,
			"body":            "Triaged READY by the scripted coordinator: the simulation's task has no dependency and fits one attempt.",
			"idempotency_key": key + "-comment"}); err != nil {
			return err
		}
		m.note("Triaged task %s: READY (revision %d).", t.TaskID, task.Revision)
	}
	if task.State != "READY" {
		m.note("Task %s is %s: not offered.", t.TaskID, task.State)
		return nil
	}
	return m.offer(ctx, t, task.Revision)
}

// offer offers the task to a worker that verified the project's repository
// at write access (`aicrew-agent members`), with the repository filled from
// the forge and the process pin and its digest from the hub.
func (m *member) offer(ctx context.Context, t agent.TaskRef, revision int64) error {
	text, err := m.mcp(ctx, "project_repository", map[string]any{"project": t.ProjectID})
	if err != nil {
		return err
	}
	var repo struct {
		Repository *struct {
			URL string `json:"url"`
		} `json:"repository"`
	}
	if err := json.Unmarshal([]byte(text), &repo); err != nil || repo.Repository == nil || repo.Repository.URL == "" {
		return fmt.Errorf("project_repository %s names no repository: %.200s", t.ProjectID, text)
	}
	worker, err := m.writer(ctx, repo.Repository.URL)
	if err != nil {
		return err
	}
	body := map[string]any{
		"worker_agent_id": worker, "task": t, "expected_revision": revision,
		"repository": map[string]string{"access": "write", "branch": "work/" + t.TaskID},
		"expires_at": m.now().Add(2 * time.Hour).UTC().Format(time.RFC3339),
	}
	ans, err := m.step(ctx, "offer", "", "", body, "--repository", repo.Repository.URL, "--process")
	if err != nil {
		return err
	}
	var res struct {
		AttemptID string `json:"attempt_id"`
	}
	_ = json.Unmarshal(ans.Result, &res)
	m.note("Offered task %s to %s: attempt %s.", t.TaskID, worker, res.AttemptID)
	return nil
}

// writer is the agent ID of the first worker whose last capability report
// verified the repository at write access.
func (m *member) writer(ctx context.Context, repoURL string) (string, error) {
	var ans agent.StepAnswer
	if err := m.agentJSON(ctx, m.home, "", &ans, "members"); err != nil {
		return "", err
	}
	var team agent.TeamMembers
	if err := json.Unmarshal(ans.Result, &team); err != nil {
		return "", fmt.Errorf("aicrew-agent members: %w", err)
	}
	for _, mb := range team.Members {
		if mb.Role != "worker" {
			continue
		}
		for _, c := range mb.Capabilities {
			for _, r := range c.Repositories {
				if r.URL == repoURL && r.Access == "write" {
					return mb.AgentID, nil
				}
			}
		}
	}
	return "", fmt.Errorf("no worker verified %s at write access (aicrew-agent members)", repoURL)
}

// ---- the coordinator: review and delivery ------------------------------------------

// submitted is one submit announcement: the attempt, the message and the
// pull request it names.
type submitted struct {
	attempt, message, pull string
}

var (
	submitText = regexp.MustCompile(` submitted a result for task .*: (https://\S+/pulls/\d+)$`)
	resultLine = regexp.MustCompile(`(?m)Result (\d+) of attempt (\S+) \(message (\S+)\): (\S+)\.$`)
)

// submits are the submit announcements among the messages.
func submits(msgs []message) []submitted {
	var out []submitted
	for _, msg := range msgs {
		if msg.Kind != "lifecycle" || msg.AttemptID == "" {
			continue
		}
		if s := submitText.FindStringSubmatch(msg.Text); s != nil {
			out = append(out, submitted{attempt: msg.AttemptID, message: msg.ID, pull: s[1]})
		}
	}
	return out
}

// pushedInReview is the rework of P2: one commit pushed while the external
// review runs.
const pushedInReview = "pushed while the external review ran"

// stallAnswer is the coordinator's answer to a blocked report.
const stallAnswer = "Go on with the task as offered; nothing else blocks it."

// escalateBlocked is the coordinator of case E1: a blocked attempt is a
// question for the architect. It raises one escalation on the attempt,
// naming its worker as blocked, and mirrors the request on the task. It
// raises none when the handoff records a raise on the attempt, when the
// turn's messages carry an answer for it (an answered escalation is no
// longer open), or when an open one already asks about the task (ROLES.md,
// "How to escalate").
func (m *member) escalateBlocked(ctx context.Context, a agent.OpenAttempt, msgs []message) error {
	if slices.ContainsFunc(escalationAnswers(msgs), func(e escalationAnswer) bool { return e.attempt == a.ID }) {
		return nil
	}
	if id := m.raisedOn(a.ID); id != "" {
		m.note("Escalation %s already asks about the blocked attempt %s.", id, a.ID)
		return nil
	}
	raw, code, err := m.run(ctx, m.home, "", "aicrew-agent", "escalations", "--json")
	if err != nil || code != 0 {
		return fmt.Errorf("aicrew-agent escalations exited %d: %v %.200s", code, err, raw)
	}
	var open struct {
		Escalations []struct {
			ID   string        `json:"id"`
			Task agent.TaskRef `json:"task"`
		} `json:"escalations"`
	}
	if err := json.Unmarshal(raw, &open); err != nil {
		return fmt.Errorf("aicrew-agent escalations: %w", err)
	}
	for _, e := range open.Escalations {
		if e.Task.TaskID == a.Task.TaskID {
			// A turn that raised it and failed before the mirror leaves the
			// mirror to this one; the comment's key makes a second one a no-op.
			return m.mirrorRequest(ctx, a, e.ID)
		}
	}
	body, _ := json.Marshal(map[string]any{
		"task": a.Task, "attempt_id": a.ID, "category": "scope", "urgency": "now",
		"question": escalationQuestion,
		"context":  "The worker's attempt was blocked by the launcher's stall rule; the coordinator cannot tell whether the task still holds.",
		"options": []map[string]string{
			{"option": "go on", "consequence": "the worker resumes the task as offered"},
			{"option": "cancel the attempt", "consequence": "the task goes back to the board"}},
		"recommendation": "go on", "blocked": a.WorkerAgentID})
	raw, code, err = m.run(ctx, m.home, string(body), "aicrew-agent", "escalate", "--body", "-")
	if err != nil {
		return fmt.Errorf("aicrew-agent escalate: %w", err)
	}
	var ans agent.StepAnswer
	if jerr := json.Unmarshal(raw, &ans); jerr != nil || code != 0 || !ans.OK {
		return fmt.Errorf("aicrew-agent escalate exited %d: %.400s", code, strings.TrimSpace(string(raw)))
	}
	var esc struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(ans.Result, &esc); err != nil || esc.ID == "" {
		return fmt.Errorf("aicrew-agent escalate: no escalation in %.200s", ans.Result)
	}
	m.note("Raised escalation %s on the blocked attempt %s.", esc.ID, a.ID)
	return m.mirrorRequest(ctx, a, esc.ID)
}

// mirrorRequest mirrors escalation id on the attempt's task. A task its
// worker holds takes no comment from the coordinator: the mirror then waits
// until the hold ends, and the handoff says so (DESIGN-CONTROL-PLANE.md,
// section 7.7); deliver posts it after the finalize.
func (m *member) mirrorRequest(ctx context.Context, a agent.OpenAttempt, id string) error {
	_, err := m.mcp(ctx, "add_task_comment", map[string]any{"id": a.Task.TaskID,
		"body":            fmt.Sprintf("[escalation.request %s]\n\n%s Raised for the architect; attempt %s was blocked.", id, escalationQuestion, a.ID),
		"idempotency_key": "sim-escalation-request-" + id})
	switch {
	case err != nil && strings.Contains(err.Error(), "task_held"):
		m.note("Escalation %s: its mirror on task %s waits for the task's hold to end.", id, a.Task.TaskID)
		return nil
	case err != nil:
		return err
	}
	m.note("Mirrored escalation %s on task %s.", id, a.Task.TaskID)
	return nil
}

var (
	raisedLine  = regexp.MustCompile(`(?m)Raised escalation (\S+) on the blocked attempt (\S+)\.$`)
	waitingLine = regexp.MustCompile(`(?m)Escalation (\S+): its mirror on task (\S+) waits for the task's hold to end\.$`)
)

// notes are the handoff's lines and this turn's.
func (m *member) notes() string {
	raw, _ := os.ReadFile(m.handoffPath())
	return string(raw) + "\n" + strings.Join(m.did, "\n")
}

// raisedOn is the escalation the handoff records raised on the attempt, ""
// for none.
func (m *member) raisedOn(attempt string) string {
	for _, s := range raisedLine.FindAllStringSubmatch(m.notes(), -1) {
		if s[2] == attempt {
			return s[1]
		}
	}
	return ""
}

// postWaitingMirrors posts the mirrors that waited for the hold on the
// attempt's task, once the attempt is finalized.
func (m *member) postWaitingMirrors(ctx context.Context, a agent.OpenAttempt) error {
	notes := m.notes()
	for _, s := range waitingLine.FindAllStringSubmatch(notes, -1) {
		if s[2] != a.Task.TaskID || strings.Contains(notes, "Mirrored escalation "+s[1]+" ") {
			continue
		}
		if err := m.mirrorRequest(ctx, a, s[1]); err != nil {
			return err
		}
	}
	return nil
}

// escalationQuestion is the question of case E1's escalation.
const escalationQuestion = "The worker's attempt is blocked: go on with the task as offered, or cancel the attempt?"

// escalationAnswer is an answered escalation's message to a member.
type escalationAnswer struct{ id, attempt, decision string }

// escalationAnswers are the answers to escalations among the messages, in
// the order read.
func escalationAnswers(msgs []message) []escalationAnswer {
	var out []escalationAnswer
	for _, msg := range msgs {
		if msg.Escalation != nil && msg.Escalation.Decision != "" && msg.AttemptID != "" {
			out = append(out, escalationAnswer{id: msg.Escalation.ID, attempt: msg.AttemptID, decision: msg.Escalation.Decision})
		}
	}
	return out
}

// blockedReports are the attempts whose worker reported them blocked, in
// the order read.
func blockedReports(msgs []message) []string {
	var out []string
	for _, msg := range msgs {
		if msg.Action == nil {
			continue
		}
		for _, s := range msg.Action.Subjects {
			if s.Kind == "blocked" && s.AttemptID != "" && !slices.Contains(out, s.AttemptID) {
				out = append(out, s.AttemptID)
			}
		}
	}
	return out
}

// result is a submitted result as the coordinator recorded it.
type result struct {
	seq           int64
	message, pull string
}

// results are the attempt's results the handoff and this turn record, in
// order. No member surface names a result's sequence number (task
// 01a188f9-4d9a), so the coordinator counts the attempt's submit
// announcements: the nth is result n.
func (m *member) results(attempt string) []result {
	raw, _ := os.ReadFile(m.handoffPath())
	var out []result
	for _, s := range resultLine.FindAllStringSubmatch(string(raw)+"\n"+strings.Join(m.did, "\n"), -1) {
		if s[2] == attempt {
			var n int64
			fmt.Sscan(s[1], &n)
			out = append(out, result{seq: n, message: s[3], pull: s[4]})
		}
	}
	return out
}

// pullRef is a pull request URL's repository and number.
func pullRef(url string) (repo, number string, err error) {
	repo, number, ok := strings.Cut(url, "/pulls/")
	if !ok || number == "" {
		return "", "", fmt.Errorf("%s is not a pull request URL", url)
	}
	return repo + ".git", number, nil
}

func (m *member) showPull(ctx context.Context, url string) (agent.PullReport, error) {
	var shown agent.PullReport
	repo, n, err := pullRef(url)
	if err == nil {
		err = m.agentJSON(ctx, m.home, "", &shown, "forge", "pr", "show", "--repository", repo, "--pr", n)
	}
	return shown, err
}

// reviewResult records a submitted result, posts the local review at its
// pull request's full head SHA and accepts it.
func (m *member) reviewResult(ctx context.Context, r submitted, a agent.OpenAttempt) error {
	rs := m.results(r.attempt)
	seq := int64(len(rs)) + 1
	if i := slices.IndexFunc(rs, func(x result) bool { return x.message == r.message }); i >= 0 {
		seq = rs[i].seq
	} else {
		m.note("Result %d of attempt %s (message %s): %s.", seq, r.attempt, r.message, r.pull)
	}
	if a.Phase != "submitted" {
		m.note("Attempt %s is %s/%s: result %d is not reviewed.", r.attempt, a.State, a.Phase, seq)
		return nil
	}
	shown, err := m.showPull(ctx, r.pull)
	if err != nil {
		return err
	}
	head := shown.Pull.HeadCommit
	repo, n, _ := pullRef(r.pull)
	var posted agent.PullReport
	if m.reviewCommand() {
		// The coordinator has no worktree on the worker's branch: --head
		// names the commit it read, which the command checks against the
		// forge's.
		if err := m.agentJSON(ctx, m.home, reviewBody(coordinatorReview), &posted, "forge", "pr", "review",
			"--repository", repo, "--pr", n, "--head", head, "--body-file", "-"); err != nil {
			return err
		}
		m.note("Posted the local review of %s at head %s with forge pr review --head: READY_FOR_HUMAN_MERGE.", r.pull, head)
	} else {
		// A home of an earlier release posts the review, header and all,
		// as a comment.
		if err := m.agentJSON(ctx, m.home, countedHeader+head+"\n\n"+reviewBody(coordinatorReview), &posted, "forge",
			"pr", "comment", "--repository", repo, "--pr", n, "--body-file", "-"); err != nil {
			return err
		}
		m.note("Posted the local review of %s at head %s with forge pr comment: READY_FOR_HUMAN_MERGE.", r.pull, head)
	}
	if _, err := m.step(ctx, "review", r.attempt, "", map[string]any{"result_seq": seq, "decision": "accept"}); err != nil {
		return err
	}
	m.note("Accepted result %d of attempt %s.", seq, r.attempt)
	return nil
}

// The reviewers whose reviews the stand-ins write, named in the
// attribution line.
const (
	coordinatorReview = "the pilot simulation's scripted coordinator"
	workerReview      = "the pilot simulation's scripted worker"
)

// reviewBody is a review in the oh-code-review format without its header
// line, which `forge pr review` writes, attributed to by.
func reviewBody(by string) string {
	return `CURRENT OBJECTIVE
The pilot simulation's task: one file, added through a reviewed pull request.

BLOCKERS
- None

CURRENT-SCOPE IMPROVEMENTS
- None

FOLLOW-UPS
- None

OBSERVATIONS
- None

REJECTED
- None

RISK ACCEPTED
- None

VERDICT
READY_FOR_HUMAN_MERGE

RISK
LOW

Review: oh-code-review (high) via ` + by + `
`
}

// countedHeader is the header line the gates read, before the head.
const countedHeader = "[oh-code-review (high) review] reviewed at head "

// reviewGuidance is the wording of a home's docs/ROLES.md whose release has
// `forge pr review` (task 01a1fa07-5db2); the releases before have none.
const reviewGuidance = "`review --pr N --body-file FILE [--head SHA]` for a review"

// gatesGreenInSet says whether `aicrew-agent check` lists gates-green in
// the home's effective automations, not disabled: the guidance's test for
// leaving the submit to the launcher.
func (m *member) gatesGreenInSet(ctx context.Context) (bool, error) {
	var rep agent.CheckReport
	if err := m.agentJSON(ctx, m.home, "", &rep, "check", "--home", m.home); err != nil {
		return false, err
	}
	if rep.Automations == nil || rep.Automations.AutomationSet == nil {
		return false, nil
	}
	for _, e := range rep.Automations.Entries {
		if e.Name == store.AutomationGatesGreen {
			return e.Source != automation.SourceDisabled, nil
		}
	}
	return false, nil
}

// submittedAnyway begins the note of the worker's submit after the
// launcher's (SubmitAnyway).
const submittedAnyway = "Submitted anyway"

// submitAnyway submits the attempt's pull request once more after the
// launcher submitted it, and records the step's answer as it came.
func (m *member) submitAnyway(ctx context.Context, b brief) error {
	n := recordedPull(m.section())
	wt := filepath.Join(m.home, "worktrees", b.Attempt)
	var shown agent.PullReport
	if err := m.agentJSON(ctx, wt, "", &shown, "forge", "pr", "show", "--pr", fmt.Sprint(n)); err != nil {
		return err
	}
	ans, code, err := m.stepOnce(ctx, "work", b.Attempt, b.Task, map[string]string{"intent": "submit",
		"detail": shown.Pull.URL})
	if err != nil {
		return err
	}
	said := ans.NextAction
	if ans.Error != nil {
		said = ans.Error.Code + ": " + ans.Error.Message
	}
	m.note("%s #%d (%s) after the launcher: exit %d, %s: %s", submittedAnyway, n, shown.Pull.URL, code, ans.Status, said)
	return nil
}

// reviewCommand says whether the home's own guidance names `forge pr
// review`, as the stand-in follows its home's guidance: the upgrade
// scenario's first cycle runs on the previous release's homes. A home
// whose guidance cannot be read is taken as an earlier one.
func (m *member) reviewCommand() bool {
	raw, err := os.ReadFile(filepath.Join(m.home, "docs", "ROLES.md"))
	return err == nil && bytes.Contains(raw, []byte(reviewGuidance))
}

// ownHeader is the header the first team's worker wrote on its own review of
// pull request #279, which no gate counts.
func ownHeader(head string) string { return "[worker review] reviewed at head " + head }

// postOwnReview posts the worker's own review of pull request n at head, as
// the first team's worker did on #279: first with a header of its own
// through `forge pr comment`, which must refuse it and name the review
// command, then through `forge pr review` in the worktree, which writes
// the header the gates read at the worktree's commit.
func (m *member) postOwnReview(ctx context.Context, wt string, n int64, head string) error {
	text := reviewBody(workerReview)
	comment, err := bodyFile(wt, commentBodyFile, ownHeader(head)+"\n\n"+text)
	if err != nil {
		return err
	}
	review, err := bodyFile(wt, reviewBodyFile, text)
	if err != nil {
		return err
	}
	out, code, err := m.run(ctx, wt, "", "aicrew-agent", "forge", "pr", "comment",
		"--pr", fmt.Sprint(n), "--body-file", comment, "--json")
	if err != nil {
		return fmt.Errorf("aicrew-agent forge pr comment: %w", err)
	}
	if code == 0 || !strings.Contains(string(out), "aicrew-agent forge pr review") {
		return fmt.Errorf("forge pr comment took a review with the header %q: exit %d: %s", ownHeader(head), code,
			strings.TrimSpace(string(out)))
	}
	m.note("forge pr comment refused the worker's review of #%d with the header %q (exit %d).", n, ownHeader(head), code)
	var posted agent.PullReport
	if err := m.agentJSON(ctx, wt, "", &posted, "forge", "pr", "review", "--pr", fmt.Sprint(n), "--body-file", review); err != nil {
		return err
	}
	m.note("Posted the worker's review of #%d at head %s with forge pr review: READY_FOR_HUMAN_MERGE.", n, head)
	return nil
}

// The files the worker writes its pull request's text and its reviews into,
// in its worktree and never added, as the managed guidance has a worker post
// them: `--body-file FILE` from the worktree, the only place a turn may write
// them (01a20b66-0f78). The launcher's wait on the gates takes such a file
// for no change. Only a home whose guidance names `forge pr review` (this
// release's) gets them: the review is posted only there, and openPull
// posts on standard input elsewhere.
const (
	pullBodyFile    = "pr-body.md"
	commentBodyFile = "comment.md"
	reviewBodyFile  = "review.md"
)

// bodyFile writes text to name in the worktree wt and returns its path.
func bodyFile(wt, name, text string) (string, error) {
	path := filepath.Join(wt, name)
	if err := os.WriteFile(path, []byte(text), 0o644); err != nil {
		return "", fmt.Errorf("write the body file %s: %w", name, err)
	}
	return path, nil
}

// continuationWait marks the note of a continuation turn that found the
// pull request waiting on the external review where the launcher's wait on
// the gates applies (gateWaitHolds): a worker that waits on its gates gets
// no such turn, which cycle.py checks. The wait on the launcher's submit is
// never marked: it is bounded, and past the bound a continuation is right.
const continuationWait = " (a continuation turn)"

// gateWaitHolds reports whether the launcher's wait on the gates applies to
// pull request n at head as the turn reads it, by a condition narrower than
// the launcher's (waitingOnGates, internal/agent/serve.go), so that a mark
// never falls on a continuation the launcher was right to give:
//   - no check failed and no external review is at the head (the launcher
//     skips a failed check and a blocking review);
//   - the worktree's HEAD is head and no tracked file is changed: the work
//     is committed and pushed (an untracked file, such as the body files,
//     is no change);
//   - the last note on n is not a push made while the external review ran
//     (P2), nor a request for the review whose report says the launcher
//     was not told: the launcher's observation can still hold the earlier
//     head or lack the label.
func (m *member) gateWaitHolds(ctx context.Context, wt string, n int64, head, checks string, c *forge.PullContent) bool {
	if checks == forge.ChecksFailure || externalAtHead(c) || m.unreported(n) {
		return false
	}
	out, code, err := m.run(ctx, wt, "", "git", "rev-parse", "HEAD")
	if err != nil || code != 0 || strings.TrimSpace(string(out)) != head {
		return false
	}
	out, code, err = m.run(ctx, wt, "", "git", "status", "--porcelain", "--untracked-files=no")
	return err == nil && code == 0 && strings.TrimSpace(string(out)) == ""
}

// externalAtHead reports whether c holds an external review at the head.
func externalAtHead(c *forge.PullContent) bool {
	if c == nil {
		return false
	}
	return slices.ContainsFunc(c.Comments, func(cm forge.PullComment) bool {
		return cm.Review != nil && cm.Review.Reviewer == forge.ReviewerExternal && cm.Review.AtHead
	})
}

// launcherNotTold is what a note on a forge pr command adds when the
// command's report says the launcher was not told of the pull request
// (its watch is not agent.WatchOn): the launcher's observation then waits
// for its next poll.
const launcherNotTold = "; the launcher was not told: "

// untold is launcherNotTold with the report's watch, or "" where the
// launcher was told.
func untold(watch string) string {
	if watch == agent.WatchOn {
		return ""
	}
	return launcherNotTold + watch
}

// unreported reports whether the section's last note on pull request n is
// one the launcher's observation may not hold yet: the push made while the
// external review ran (rework, P2), which no forge pr command followed, or
// a command whose report says the launcher was not told.
func (m *member) unreported(n int64) bool {
	tag := fmt.Sprintf("#%d", n)
	last := ""
	for _, line := range strings.Split(m.section(), "\n") {
		if strings.HasPrefix(line, "- ") && (strings.Contains(line, tag+" ") || strings.Contains(line, tag+":")) {
			last = line
		}
	}
	return strings.Contains(last, "Reworked "+tag+": "+pushedInReview+";") || strings.Contains(last, launcherNotTold)
}

// ownReviewAt reports whether c holds a local review at the head.
func ownReviewAt(c *forge.PullContent) bool {
	if c == nil {
		return false
	}
	for _, cm := range c.Comments {
		if r := cm.Review; r != nil && r.Reviewer == forge.ReviewerLocal && r.AtHead {
			return true
		}
	}
	return false
}

// externalReviewer is the account of the simulation's external reviewer,
// whose reviews verify-delivery counts as the external one.
const externalReviewer = "sim-hands"

// deliver confirms the delivery of the accepted result once its pull
// request is merged, with the evidence verify-delivery returns, and
// finalizes it.
func (m *member) deliver(ctx context.Context, a agent.OpenAttempt) error {
	rs := m.results(a.ID)
	if len(rs) == 0 {
		return fmt.Errorf("attempt %s is accepted, but the handoff records no result of it", a.ID)
	}
	r := rs[len(rs)-1]
	shown, err := m.showPull(ctx, r.pull)
	if err != nil {
		return err
	}
	if shown.Pull.State != forge.PullMerged {
		m.note("Pull request %s is %s: no delivery to confirm yet.", r.pull, shown.Pull.State)
		return nil
	}
	v, err := m.verifyDelivery(ctx, r.pull)
	if err != nil {
		return err
	}
	if v.Verdict != "confirmed" {
		m.note("verify-delivery: %s for %s; the delivery is not confirmed.", v.Verdict, r.pull)
		return nil
	}
	m.note("verify-delivery confirmed %s with %d evidence entries.", r.pull, len(v.Evidence))
	if _, err := m.step(ctx, "confirm-delivery", a.ID, "", map[string]any{"result_seq": r.seq, "evidence": v.Evidence}); err != nil {
		return err
	}
	m.note("Confirmed the delivery of result %d of attempt %s.", r.seq, a.ID)
	if _, err := m.step(ctx, "finalize", a.ID, a.Task.TaskID, map[string]any{"result_seq": r.seq}); err != nil {
		return err
	}
	m.note("Finalized attempt %s: task %s is DONE.", a.ID, a.Task.TaskID)
	return m.postWaitingMirrors(ctx, a)
}

// delivery is what the coordinator reads of verify-delivery's answer.
type delivery struct {
	Verdict  string `json:"verdict"`
	Evidence []struct {
		Kind string `json:"kind"`
		Ref  string `json:"ref"`
	} `json:"evidence"`
}

// verifyDelivery runs the installed verify-delivery helper against the pull
// request with the home's own forge credential, naming the coordinator's
// account and the external reviewer's as the reviews' authors.
func (m *member) verifyDelivery(ctx context.Context, url string) (delivery, error) {
	var v delivery
	host, _, ok := strings.Cut(strings.TrimPrefix(url, "https://"), "/")
	if !ok {
		return v, fmt.Errorf("%s is not an https URL", url)
	}
	cred, err := m.forgeCred(host)
	if err != nil {
		return v, err
	}
	helper, err := m.skillFile("verify-delivery", "verify_delivery.py")
	if err != nil {
		return v, err
	}
	out, code, err := m.run(ctx, m.home, "", "env", "GITEA_TOKEN_FILE="+cred.file, "python3", helper,
		"--pr", url, "--api-base", "https://"+host+"/api/v1",
		"--review-author", "local="+cred.account, "--review-author", "external="+externalReviewer)
	if err != nil {
		return v, fmt.Errorf("verify-delivery: %w", err)
	}
	if jerr := json.Unmarshal(out, &v); jerr != nil || v.Verdict == "" {
		return v, fmt.Errorf("verify-delivery exited %d without a verdict: %.400s", code, strings.TrimSpace(string(out)))
	}
	return v, nil
}

// forgeCredential is the home's own credential for a forge host, as
// join recorded it in agent.json.
type forgeCredential struct{ file, account string }

func (m *member) forgeCred(host string) (forgeCredential, error) {
	raw, err := os.ReadFile(filepath.Join(m.home, "agent.json"))
	if err != nil {
		return forgeCredential{}, err
	}
	var doc struct {
		Forge map[string]struct {
			Account string `json:"account"`
			File    string `json:"file"`
		} `json:"forge"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return forgeCredential{}, fmt.Errorf("agent.json: %w", err)
	}
	e, ok := doc.Forge[host]
	if !ok || e.File == "" || e.Account == "" {
		return forgeCredential{}, fmt.Errorf("the home has no forge credential for %s", host)
	}
	return forgeCredential{file: filepath.Join(m.home, "creds", e.File), account: e.Account}, nil
}

// skillFile is a file of an installed skill, from the directories the
// client reads skills in: the home's, then the user's.
func (m *member) skillFile(skill, file string) (string, error) {
	dirs := []string{filepath.Join(m.home, ".claude", "skills")}
	if uh, err := os.UserHomeDir(); err == nil {
		dirs = append(dirs, filepath.Join(uh, ".claude", "skills"))
	}
	for _, d := range dirs {
		p := filepath.Join(d, skill, file)
		if _, err := os.Stat(p); err == nil {
			return p, nil
		}
	}
	return "", fmt.Errorf("no installed %s skill (%s)", skill, strings.Join(dirs, ", "))
}

// The wordings of the home's docs/ROLES.md on the confirm-delivery step:
// launcherGuidance where the launcher checks the delivery (task
// 01a1fa19-19a4), turnGuidance in the releases before, where the
// coordinator's turn does.
const (
	launcherGuidance = "Your launcher takes this step itself"
	turnGuidance     = "After a person merged the accepted result, record the evidence"
)

// launcherDelivers says whether the home's own guidance gives the delivery
// check to the launcher (true) or to the coordinator's turn (false). A home
// whose guidance has neither wording fails the turn: the stand-in would
// otherwise guess.
func (m *member) launcherDelivers() (bool, error) {
	raw, err := os.ReadFile(filepath.Join(m.home, "docs", "ROLES.md"))
	if err != nil {
		return false, fmt.Errorf("the home's guidance on the delivery: %w", err)
	}
	return guidanceDelivers(raw)
}

// guidanceDelivers reads a docs/ROLES.md for who checks the delivery.
func guidanceDelivers(roles []byte) (bool, error) {
	l, t := bytes.Contains(roles, []byte(launcherGuidance)), bytes.Contains(roles, []byte(turnGuidance))
	if l == t {
		return false, fmt.Errorf("the home's docs/ROLES.md says neither or both of %q and %q: the stand-in cannot "+
			"tell who checks the delivery", launcherGuidance, turnGuidance)
	}
	return l, nil
}

// launcherDelivered is the coordinator's part where the launcher checks the
// delivery: an accepted attempt's turn has nothing to run, and the mirrors
// that waited for the worker's hold on the task go up once the worker
// finalized it.
func (m *member) launcherDelivered(ctx context.Context, open agent.OpenAttempts, msgs []message) error {
	for _, a := range open.Attempts {
		if a.Phase == "accepted" {
			m.note("Attempt %s is accepted: its delivery is the launcher's check.", a.ID)
		}
	}
	for _, msg := range msgs {
		if msg.Kind == "lifecycle" && msg.AttemptID != "" && msg.Task != nil && finalizedLine.MatchString(msg.Text) {
			m.note("Attempt %s was finalized: task %s is DONE.", msg.AttemptID, msg.Task.TaskID)
			if err := m.postWaitingMirrors(ctx, agent.OpenAttempt{ID: msg.AttemptID, Task: *msg.Task}); err != nil {
				return err
			}
		}
	}
	return nil
}

// finalizedLine is aicrewd's announcement of a finalized attempt.
var finalizedLine = regexp.MustCompile(`^Task \S+ was finalized as done with result \d+\.$`)

// confirmedLine is aicrewd's announcement of a confirmed delivery.
var confirmedLine = regexp.MustCompile(` confirmed the delivery of result (\d+) of task `)

// finalizeConfirmed finalizes the worker's accepted attempt once a message
// says its delivery was confirmed, with that message's result.
func (m *member) finalizeConfirmed(ctx context.Context, b brief, msgs []message) error {
	for _, msg := range msgs {
		s := confirmedLine.FindStringSubmatch(msg.Text)
		if msg.Kind != "lifecycle" || msg.AttemptID != b.Attempt || s == nil {
			continue
		}
		var n int64
		fmt.Sscan(s[1], &n)
		if _, err := m.step(ctx, "finalize", b.Attempt, b.Task, map[string]any{"result_seq": n}); err != nil {
			return err
		}
		m.note("Finalized attempt %s: task %s is DONE.", b.Attempt, b.Task)
		return nil
	}
	m.note("Attempt %s is accepted: waiting for the confirmed delivery.", b.Attempt)
	return nil
}

// ---- the handoff -------------------------------------------------------------------

const (
	sectionHeading = "## CURRENT ATTEMPT"
	archiveHeading = "## Archive"
)

func (m *member) handoffPath() string {
	return filepath.Join(m.home, filepath.FromSlash(agent.HandoffFile))
}

// section is the CURRENT ATTEMPT section of the home's handoff, "" for none.
func (m *member) section() string {
	raw, err := os.ReadFile(m.handoffPath())
	if err != nil {
		return ""
	}
	_, sec, _ := splitSection(string(raw))
	return sec
}

// splitSection splits the doc around its CURRENT ATTEMPT section.
func splitSection(doc string) (before, section, after string) {
	i := strings.Index(doc, sectionHeading+"\n")
	if i < 0 {
		return doc, "", ""
	}
	rest := doc[i:]
	j := strings.Index(rest, "\n"+archiveHeading+"\n")
	if j < 0 {
		return doc[:i], rest, ""
	}
	return doc[:i], rest[:j+1], rest[j+1:]
}

// recordHandoff writes the turn into the handoff: a worker's lines go under
// the section's Done, and its Next says what the turn leaves; a coordinator,
// which has no section, appends them to the file.
func (m *member) recordHandoff(b brief) error {
	path := m.handoffPath()
	raw, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	at := m.now().UTC().Format(time.RFC3339)
	var lines strings.Builder
	for _, d := range m.did {
		fmt.Fprintf(&lines, "- %s %s\n", at, d)
	}
	doc := string(raw)
	before, sec, after := splitSection(doc)
	if b.Role == "worker" && sec != "" {
		sec = editSection(sec, lines.String(), "Turn of "+at+": "+m.did[len(m.did)-1])
		doc = before + sec + after
	} else {
		if doc != "" && !strings.HasSuffix(doc, "\n") {
			doc += "\n"
		}
		doc += lines.String()
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	return os.WriteFile(path, []byte(doc), 0o600)
}

// editSection appends done under "### Done" and makes next the body of
// "### Next"; a heading the section lacks is added at its end.
func editSection(sec, done, next string) string {
	sec = withSub(sec, "### Done", func(body string) string { return body + "\n" + done })
	return withSub(sec, "### Next", func(string) string { return next })
}

// withSub replaces the body of the subsection under heading with f of its
// current body, both without their surrounding blank lines; the result
// keeps one blank line after the heading and before the next one.
func withSub(sec, heading string, f func(string) string) string {
	i := strings.Index(sec, "\n"+heading+"\n")
	if i < 0 {
		return strings.TrimRight(sec, "\n") + "\n\n" + heading + "\n\n" + strings.Trim(f(""), "\n") + "\n"
	}
	start := i + 1 + len(heading)
	end := len(sec)
	if j := strings.Index(sec[start:], "\n### "); j >= 0 {
		end = start + j + 1
	}
	body := "\n\n" + strings.Trim(f(strings.Trim(sec[start:end], "\n")), "\n") + "\n"
	if end < len(sec) {
		body += "\n"
	}
	return sec[:start] + body + sec[end:]
}

// ---- the client's side -------------------------------------------------------------

// turnRun is one turn as the launcher runs it: the brief from stdin, the
// script, and the stream-json events with the result last. A script that
// fails ends the turn with an error result, which the launcher records as
// a failed turn.
func turnRun(ctx context.Context, dir string, stdin io.Reader, stdout io.Writer, m *member) int {
	raw, err := io.ReadAll(io.LimitReader(stdin, 4<<20))
	var b brief
	if err == nil {
		b, err = parseBrief(strings.TrimSpace(string(raw)))
	}
	if err == nil {
		err = m.scErr
	}
	// A client error the scenario names ends the turn before the script, as
	// the provider's answer would (P3).
	status := map[string]int{"login": 401, "model": 404}[m.sc.Result]
	if err == nil && status == 0 {
		err = m.turn(ctx, b)
	}
	emit(stdout, map[string]any{"type": "system", "subtype": "init", "cwd": dir, "model": simModel,
		"mcp_servers": []mcpEntry{}, "skills": []string{}})
	switch {
	case err != nil:
		emit(stdout, map[string]any{"type": "result", "subtype": "error_during_execution", "is_error": true,
			"result": strings.Join(m.did, " "), "errors": []string{err.Error()}, "total_cost_usd": m.sc.CostUSD})
		return 1
	case status != 0:
		emit(stdout, map[string]any{"type": "result", "subtype": "error_during_execution", "is_error": true,
			"result": "", "api_error_status": status, "errors": []string{"the scenario's " + m.sc.Result + " error"},
			"total_cost_usd": m.sc.CostUSD})
		return 1
	}
	emit(stdout, map[string]any{"type": "result", "subtype": "success", "is_error": false,
		"result": strings.Join(m.did, " "), "total_cost_usd": m.sc.CostUSD})
	return 0
}

// newMember is the real member: commands on PATH and the home's aimem MCP
// server as its .mcp.json starts it.
func newMember(dir string) *member {
	home := os.Getenv(agent.HomeEnv)
	if home == "" {
		home = dir
	}
	sc, scErr := readScenario(scenarioFile)
	return &member{home: home, sc: sc, scErr: scErr, run: execRun, now: time.Now,
		mcp: func(ctx context.Context, tool string, args map[string]any) (string, error) {
			s, ok, err := aimemServer(home)
			if err != nil || !ok {
				return "", fmt.Errorf("the home's aimem MCP server: %v", err)
			}
			return callTool(ctx, s, tool, args)
		}}
}

func execRun(ctx context.Context, dir, stdin, name string, args ...string) ([]byte, int, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = dir
	cmd.Stdin = strings.NewReader(stdin)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		if len(out) == 0 {
			out = stderr.Bytes()
		}
		return out, exit.ExitCode(), nil
	}
	return out, 0, err
}
