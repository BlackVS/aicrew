package agent

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// `aicrew-agent serve` (docs/DESIGN-CONTROL-PLANE.md, sections 1.3 to 1.10;
// increment A3): the member's own turn loop. It holds the team session as
// `run` does and runs one headless turn of the client at a time, instead of
// one interactive client and its Stop hook:
//   - a turn starts when the member has unacknowledged messages, or when a
//     worker's attempt is still in work after its last turn (continuation);
//   - at most one turn start per turnGap, so a burst of messages coalesces;
//   - no turn while the team or the member is paused, a limit or a login
//     failure holds the member, or a budget is spent;
//   - three turns of an attempt with no step and no new commit stall it: the
//     loop blocks the attempt through the work step, which tells the
//     coordinator, and gives it no more continuation turns until a message;
//   - each turn resumes the attempt's or the member's conversation (D4) and
//     is recorded in logs/turns.jsonl.
// While idle it also watches the attempt's pull requests (forge events).

// sessionReadPath is aicrewd's session status, which names the pause and
// the member's open attempt.
const sessionReadPath = "/v1/crew/session"

// memberStatus is the part of the session status the loop reads.
type memberStatus struct {
	Session struct {
		AgentID string `json:"agent_id"`
		TeamID  string `json:"team_id"`
		Role    string `json:"role"`
	} `json:"session"`
	Paused  *PauseNote     `json:"paused"`
	Attempt *memberAttempt `json:"attempt"`
}

// memberAttempt is the member's open attempt as its worker.
type memberAttempt struct {
	ID     string  `json:"id"`
	State  string  `json:"state"`
	Phase  string  `json:"phase"`
	Task   TaskRef `json:"task"`
	Branch string  `json:"branch"`
	// BaseCommit is the commit the attempt's work started from.
	BaseCommit string `json:"base_commit"`
}

// inWork reports whether the attempt is running and being worked on: not
// submitted, blocked, accepted or finalized.
func (a *memberAttempt) inWork() bool {
	return a != nil && a.State == "running" && (a.Phase == "working" || a.Phase == "rework")
}

// turnHost is what the loop asks of the launcher's session; the step
// server is the real one.
type turnHost interface {
	PendingNow(ctx context.Context) ([]PendingMessage, *PauseNote, error)
	Status(ctx context.Context) (memberStatus, error)
	StepsDone() int64
	Do(ctx context.Context, call StepCall) StepAnswer
	PollPulls(ctx context.Context)
}

// TurnLoop is one home's turn loop.
type TurnLoop struct {
	Home   string
	Client string // the client's executable
	Env    []string
	Host   turnHost
	Run    func(context.Context, TurnSpec) TurnResult
	Log    *slog.Logger
	Now    func() time.Time
	// Head reads the commit a worktree is at; "" when it cannot.
	Head func(ctx context.Context, worktree string) string

	lastStart time.Time
	nextPoll  time.Time
}

// Loop runs ticks every wakePoll until ctx ends. A turn left running by an
// earlier loop is recorded as interrupted first.
func (l *TurnLoop) Loop(ctx context.Context) error {
	st := readTurnState(l.Home)
	if st.Running != nil {
		r := *st.Running
		r.EndedAt, r.Outcome, r.Error = l.Now(), TurnInterrupted, "the turn loop restarted while the turn ran"
		if err := appendTurnRecord(l.Home, r); err != nil {
			l.Log.Warn("record the interrupted turn", "error", err.Error())
		}
		st.Running = nil
		if err := writeTurnState(l.Home, st); err != nil {
			return err
		}
	}
	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		l.Tick(ctx)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(wakePoll):
		}
	}
}

// Tick decides whether the member is due a turn now and runs it.
func (l *TurnLoop) Tick(ctx context.Context) {
	now := l.Now()
	if !now.Before(l.nextPoll) {
		l.Host.PollPulls(ctx)
		l.nextPoll = l.Now().Add(forgePoll)
	}
	st := readTurnState(l.Home)
	if now.Before(st.HoldUntil) {
		return
	}
	pending, paused, err := l.Host.PendingNow(ctx)
	if err != nil || paused != nil {
		return
	}
	status, err := l.Host.Status(ctx)
	if err != nil || status.Paused != nil {
		return
	}
	attempt := status.Attempt
	if attempt != nil && attempt.State == "closed" {
		attempt = nil
	}
	limits := ReadTurnLimits(l.Home)
	var trigger string
	continuation := false
	switch {
	case len(pending) > 0:
		trigger = pendingSummary(pending)
	case attempt.inWork() && st.Continue == attempt.ID:
		a := st.attempt(attempt.ID)
		if a.Stalled || a.Continuations >= limits.MaxContinuations {
			return
		}
		trigger, continuation = "a continuation of attempt "+attempt.ID, true
	default:
		return
	}
	if now.Sub(l.lastStart) < turnGap {
		return
	}
	day := dayOf(now)
	if st.Days[day] >= limits.DailyBudgetUSD {
		l.hold(&st, nextDay(now), "the daily budget is spent")
		return
	}
	if attempt != nil && st.attempt(attempt.ID).CostUSD >= limits.AttemptBudgetUSD {
		l.hold(&st, now.Add(time.Hour), "attempt "+attempt.ID+"'s budget is spent")
		return
	}
	l.lastStart = now
	l.turn(ctx, &st, status, attempt, trigger, continuation, limits)
}

// settingsFile is the per-turn settings of a member in role. A coordinator's
// file that is missing, in a home not rejoined since an upgrade, falls back
// to the common one, which admits less, and says so: a missing --settings
// file would fail every turn. A worker whose team selects a toolchain gets
// WorkerToolchainSettingsFile, written now from the team record; a
// coordinator never does.
func (l *TurnLoop) settingsFile(role string) string {
	f := TurnSettingsFor(role)
	if f == TurnSettingsFile {
		if role == "worker" {
			return l.workerSettings()
		}
		return f
	}
	if _, err := os.Stat(filepath.Join(l.Home, filepath.FromSlash(f))); err != nil {
		l.Log.Warn("the role's per-turn settings are missing: the turn runs with the common ones; rerun "+
			"`aicrew-agent join --home HOME` to write them", "file", f)
		return TurnSettingsFile
	}
	return f
}

// workerSettings writes a worker's settings with its team's toolchains and
// names the file, or names the common one when the team selects none or the
// file cannot be written.
func (l *TurnLoop) workerSettings() string {
	names, unknown, err := teamToolchains(l.Home)
	if err != nil {
		l.Log.Warn("the team record was not read: the turn runs with the common settings", "error", err.Error())
		return TurnSettingsFile
	}
	if len(unknown) > 0 {
		l.Log.Warn("the team selects a toolchain this aicrew-agent does not know; upgrade it", "toolchains",
			strings.Join(unknown, ","))
	}
	path := filepath.Join(l.Home, filepath.FromSlash(WorkerToolchainSettingsFile))
	if len(names) == 0 {
		// A selection cleared since the last turn leaves no file behind.
		_ = os.Remove(path)
		return TurnSettingsFile
	}
	if err := writeAtomic(path, []byte(workerToolchainSettings(l.Home, names))); err != nil {
		l.Log.Warn("the worker's toolchain settings were not written: the turn runs with the common ones",
			"error", err.Error())
		return TurnSettingsFile
	}
	return WorkerToolchainSettingsFile
}

// hold gives the member no turn before until, and says why once.
func (l *TurnLoop) hold(st *turnState, until time.Time, why string) {
	if st.Hold != why {
		l.Log.Warn("no turn: "+why, "until", until.Format(time.RFC3339))
	}
	st.HoldUntil, st.Hold = until, why
	if err := writeTurnState(l.Home, *st); err != nil {
		l.Log.Warn("record the turn state", "error", err.Error())
	}
}

// turn runs one turn and records it.
func (l *TurnLoop) turn(ctx context.Context, st *turnState, status memberStatus, attempt *memberAttempt, trigger string,
	continuation bool, limits TurnLimits) {
	now := l.Now()
	conv := &st.Member
	var a *attemptTurns
	if attempt != nil {
		a = st.attempt(attempt.ID)
		conv = &a.Conversation
	} else if conv.ID != "" && (conv.Turns >= rotateTurns || conv.Context > rotateContext || conv.Day != dayOf(now)) {
		*conv = conversation{} // rotate: the next turn starts from the handoff
	}
	resume := conv.ID != ""
	if !resume {
		*conv = conversation{ID: newUUID(), Day: dayOf(now)}
	}
	rec := TurnRecord{ID: newUUID(), AgentID: status.Session.AgentID, TeamID: status.Session.TeamID, Trigger: trigger,
		Role: status.Session.Role, Client: "claude", Model: limits.Model, Conversation: conv.ID, Resumed: resume,
		StartedAt: now, Denials: []string{}}
	if attempt != nil {
		task := attempt.Task
		rec.AttemptID, rec.Task = attempt.ID, &task
	}
	st.Running = &rec
	if err := writeTurnState(l.Home, *st); err != nil {
		l.Log.Warn("record the turn state", "error", err.Error())
	}
	head := ""
	if attempt != nil {
		head = l.head(ctx, attempt.ID)
	}
	before := l.Host.StepsDone()
	l.Log.Info("turn started", "turn", rec.ID, "trigger", trigger, "conversation", conv.ID, "resumed", resume)
	transcript, closeTranscript := openTranscript(l.Home, rec.ID)
	res := l.Run(ctx, TurnSpec{Client: l.Client, Home: l.Home, Env: l.Env, Prompt: "/crew-turn " + trigger,
		Settings: l.settingsFile(status.Session.Role), Conversation: conv.ID, Resume: resume, Model: limits.Model, BudgetUSD: limits.TurnBudgetUSD,
		Timeout: limits.TurnTimeout, Transcript: transcript})
	closeTranscript()
	rec.EndedAt = l.Now()
	rec.DurationMS = rec.EndedAt.Sub(rec.StartedAt).Milliseconds()
	rec.Outcome, rec.Error, rec.LimitKind, rec.ResetAt = res.Outcome, res.Error, res.LimitKind, res.ResetAt
	rec.Usage, rec.CostBasis, rec.Summary, rec.ClientTotalUSD = res.Usage, res.CostBasis, res.Summary, res.ClientTotalUSD
	if len(res.Models) > 0 {
		rec.Model = strings.Join(res.Models, ",")
	}
	rec.CostUSD, rec.CostSource = l.turnCost(*conv, resume, res)
	if rec.CostSource == CostPriceList {
		rec.PriceList = PriceListVersion
	}
	if res.Denials != nil {
		rec.Denials = res.Denials
	}
	rec.Steps = int(l.Host.StepsDone() - before)
	after, afterErr := l.Host.Status(ctx)
	if afterErr == nil && attempt == nil && after.Attempt != nil && after.Attempt.State != "closed" {
		// The turn opened an attempt (an accepted offer, a claim): the
		// turn is the attempt's first, and its conversation the attempt's
		// (D4), with its cost charged to it.
		attempt = after.Attempt
		task := attempt.Task
		rec.AttemptID, rec.Task = attempt.ID, &task
		a = st.attempt(attempt.ID)
		a.Conversation = *conv
		st.Member = conversation{}
		conv = &a.Conversation
	}
	if err := appendTurnRecord(l.Home, rec); err != nil {
		l.Log.Warn("record the turn", "error", err.Error())
	}
	l.Log.Info("turn ended", "turn", rec.ID, "outcome", rec.Outcome, "cost_usd", rec.CostUSD, "steps", rec.Steps,
		"denials", len(rec.Denials))

	st.Running = nil
	attemptID := ""
	if attempt != nil {
		attemptID = attempt.ID
	}
	conv.Turns++
	if res.Context > 0 {
		conv.Context = res.Context
	}
	if res.ClientTotalUSD > 0 {
		total := res.ClientTotalUSD
		conv.ClientTotalUSD = &total
	}
	if res.Outcome == TurnFailed && resume {
		*conv = conversation{} // a conversation the client cannot resume starts fresh
	}
	st.spend(dayOf(now), attemptID, rec.CostUSD)
	l.afterOutcome(st, res)
	if a != nil {
		if !continuation {
			a.Stalled = false
		} else {
			a.Continuations++
		}
		newHead := l.head(ctx, attempt.ID)
		if rec.Steps > 0 || (newHead != "" && newHead != head) {
			a.NoProgress = 0
		} else if res.Outcome == TurnFinished {
			a.NoProgress++
		}
		a.Head = newHead
	}
	st.Continue = ""
	if afterErr == nil && after.Attempt.inWork() {
		st.Continue = after.Attempt.ID
		if a != nil && after.Attempt.ID == attemptID && a.NoProgress >= stallTurns && !a.Stalled {
			l.stall(ctx, after.Attempt, a)
			st.Continue = ""
		}
	}
	if err := writeTurnState(l.Home, *st); err != nil {
		l.Log.Warn("record the turn state", "error", err.Error())
	}
}

// afterOutcome holds the member after a turn that hit a limit, a login
// failure or a failure (1.6, 3.4): until the provider's reset time, or for a
// backoff that doubles from an hour (a minute after a failure) up to 8
// hours (30 minutes after a failure). A finished turn clears the backoff.
func (l *TurnLoop) afterOutcome(st *turnState, res TurnResult) {
	now := l.Now()
	switch {
	case res.Outcome == TurnFinished || res.Outcome == TurnStopped || (res.Outcome == TurnLimitReached && res.LimitKind == "budget"):
		st.Backoff, st.Hold, st.HoldUntil = 0, "", time.Time{}
	case res.Outcome == TurnLimitReached && res.ResetAt.After(now):
		st.HoldUntil, st.Hold, st.Backoff = res.ResetAt, "the provider's limit", 0
	case res.Outcome == TurnLimitReached || res.Outcome == TurnNeedsLogin:
		st.Backoff = nextBackoff(st.Backoff, time.Hour, 8*time.Hour)
		st.HoldUntil, st.Hold = now.Add(st.Backoff), res.Outcome
	default:
		st.Backoff = nextBackoff(st.Backoff, time.Minute, 30*time.Minute)
		st.HoldUntil, st.Hold = now.Add(st.Backoff), res.Outcome
	}
	if st.Hold != "" {
		l.Log.Warn("no turn before "+st.HoldUntil.Format(time.RFC3339), "reason", st.Hold, "error", res.Error)
	}
}

func nextBackoff(cur, base, ceiling time.Duration) time.Duration {
	if cur < base {
		return base
	}
	return min(cur*2, ceiling)
}

// stall blocks the attempt through the work step: three turns made no step
// and no commit. The block's lifecycle message tells the coordinator, which
// decides what the stall means, as it decides any block.
func (l *TurnLoop) stall(ctx context.Context, attempt *memberAttempt, a *attemptTurns) {
	a.Stalled = true
	body, _ := json.Marshal(map[string]string{"intent": "block", "detail": fmt.Sprintf(
		"The turn loop stalled this attempt: %d turns in a row made no step and no new commit.", stallTurns)})
	ans := l.Host.Do(ctx, StepCall{Op: "work", AttemptID: attempt.ID, TaskID: attempt.Task.TaskID, Body: body})
	if !ans.OK {
		msg := ""
		if ans.Error != nil {
			msg = ans.Error.Message
		}
		l.Log.Warn("the stalled attempt was not blocked; it gets no continuation turns until a message",
			"attempt", attempt.ID, "error", msg)
		return
	}
	l.Log.Warn("the attempt stalled and was blocked", "attempt", attempt.ID)
}

// TurnTranscriptDir holds each turn's raw stream, relative to the home:
// logs/turns/<turn id>.jsonl, owner-only. It is the home's own log and is
// never sent anywhere (4.3, D7).
const TurnTranscriptDir = "logs/turns"

// openTranscript opens the turn's transcript file, or nil when it cannot.
func openTranscript(home, turnID string) (io.Writer, func()) {
	dir := filepath.Join(home, filepath.FromSlash(TurnTranscriptDir))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, func() {}
	}
	f, err := os.OpenFile(filepath.Join(dir, turnID+".jsonl"), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, func() {}
	}
	return f, func() { f.Close() }
}

// head is the commit the attempt's worktree is at, or "".
func (l *TurnLoop) head(ctx context.Context, attemptID string) string {
	if l.Head == nil {
		return ""
	}
	return l.Head(ctx, filepath.Join(l.Home, "worktrees", attemptID))
}

// worktreeHead reads a worktree's HEAD with git.
func worktreeHead(ctx context.Context, worktree string) string {
	if _, err := os.Stat(worktree); err != nil {
		return ""
	}
	out, err := execGit(ctx, worktree, gitEnv, "rev-parse", "HEAD")
	if err != nil {
		return ""
	}
	return strings.TrimSpace(out)
}

// pendingSummary names what started a turn: the messages' kinds and ids,
// never their text.
func pendingSummary(pending []PendingMessage) string {
	kinds := map[string]int{}
	var ids []string
	for _, p := range pending {
		kinds[p.Kind]++
		if len(ids) < 10 {
			ids = append(ids, p.ID)
		}
	}
	var parts []string
	for _, k := range []string{"action.needed", "lifecycle", "message"} {
		if n := kinds[k]; n > 0 {
			parts = append(parts, fmt.Sprintf("%d %s", n, k))
		}
	}
	return fmt.Sprintf("%d new message(s) in your inbox (%s; ids %s)", len(pending), strings.Join(parts, ", "),
		strings.Join(ids, ", "))
}

func nextDay(t time.Time) time.Time {
	y, m, d := t.UTC().Date()
	return time.Date(y, m, d+1, 0, 0, 0, 0, time.UTC)
}

// newUUID is a random version 4 UUID, the form Claude Code takes for a
// conversation's ID.
func newUUID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// The step server is the loop's host.

// PendingNow reads the member's pending messages once, with the pause.
func (s *StepServer) PendingNow(ctx context.Context) ([]PendingMessage, *PauseNote, error) {
	raw, err := s.local.Read(ctx, s.driver.Session.stepToken(), inboxPendingPath)
	if err != nil {
		return nil, nil, err
	}
	var out struct {
		Pending []PendingMessage `json:"pending"`
		Paused  *PauseNote       `json:"paused"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, nil, err
	}
	return out.Pending, out.Paused, nil
}

// Status reads the session status: the pause and the open attempt.
func (s *StepServer) Status(ctx context.Context) (memberStatus, error) {
	raw, err := s.local.Read(ctx, s.driver.Session.stepToken(), sessionReadPath)
	if err != nil {
		return memberStatus{}, err
	}
	var st memberStatus
	if err := json.Unmarshal(raw, &st); err != nil {
		return memberStatus{}, err
	}
	return st, nil
}

// StepsDone is how many attempt steps the channel has run.
func (s *StepServer) StepsDone() int64 { return s.steps.Load() }

// Do runs one step call as the channel does, one step at a time.
func (s *StepServer) Do(ctx context.Context, call StepCall) StepAnswer {
	s.mu.Lock()
	defer s.mu.Unlock()
	cctx, cancel := context.WithTimeout(ctx, stepTimeout)
	defer cancel()
	call.Version = 1
	return s.handle(cctx, call)
}

// PollPulls reads and reports the watched pull requests (forgewatch.go).
func (s *StepServer) PollPulls(ctx context.Context) { s.pollPulls(ctx) }

// ErrNotTurnsHome refuses serve on a home whose wake mode is not turns.
var ErrNotTurnsHome = errors.New("this home is not in turns mode")

// ServeHome runs the home's turn loop under its team session until a stop
// (an interrupt or SIGTERM) or the session's end, then leaves the session
// as `run` does when its client exits. A turn running at the stop is ended
// and recorded as stopped. client is the Claude Code executable.
func ServeHome(ctx context.Context, e *Engine, client string, signals <-chan os.Signal) error {
	if ReadWakeSettings(e.Cfg.Home).Mode != WakeTurns {
		return fmt.Errorf("%w: run `aicrew-agent join --home %s --wake turns` first, so that its managed settings "+
			"have no Stop hook; an interactive member keeps `aicrew-agent run`", ErrNotTurnsHome, quoteArg(e.Cfg.Home))
	}
	if err := start(ctx, e, signals); err != nil {
		return err
	}
	steps := serveSteps(e)
	if steps == nil {
		return errors.Join(errors.New("the step channel is not served, so no turn can act"),
			e.Leave(context.WithoutCancel(ctx)))
	}
	defer steps.Close()
	home, err := filepath.Abs(e.Cfg.Home)
	if err != nil {
		home = e.Cfg.Home
	}
	runCtx, stopRun := context.WithCancel(context.WithoutCancel(ctx))
	defer stopRun()
	ran := make(chan error, 1)
	go func() { ran <- e.Run(runCtx) }()
	loopCtx, stopLoop := context.WithCancel(ctx)
	defer stopLoop()
	loop := &TurnLoop{Home: home, Client: client, Host: steps, Run: RunClaudeTurn, Log: e.Log, Now: time.Now,
		Head: worktreeHead, Env: clientEnviron(e, home)}
	looped := make(chan error, 1)
	go func() { looped <- loop.Loop(loopCtx) }()
	e.Log.Info("the turn loop runs", "home", home)
	select {
	case <-signals:
		e.Log.Info("stop requested; ending the turn loop")
		stopLoop()
		<-looped
		stopRun()
		return <-ran
	case err := <-ran:
		// The session ended (an operator stop, a removal): no turn can act.
		stopLoop()
		<-looped
		if err != nil {
			e.Log.Warn("the team session is no longer held", "error", err.Error())
		}
		return err
	}
}

// turnCost is a turn's own cost and its source (pricing.go). A turn the
// price list priced keeps that cost, and a client's running total, when the
// conversation's previous total is known, checks it. A turn it did not
// price costs the difference of the running total since the conversation's
// previous turn: the whole total for a fresh conversation, and unknown for
// a resumed one whose previous total was not kept. Another client's
// adapter reports its turn's own cost.
func (l *TurnLoop) turnCost(conv conversation, resume bool, res TurnResult) (float64, string) {
	if res.CostSource != CostPriceList && res.CostSource != CostClientTotal {
		return res.CostUSD, res.CostSource
	}
	prev, known := 0.0, !resume
	if resume && conv.ClientTotalUSD != nil {
		prev, known = *conv.ClientTotalUSD, true
	}
	delta, deltaOK := math.Round((res.ClientTotalUSD-prev)*1e6)/1e6, known && res.ClientTotalUSD >= prev
	if res.CostSource == CostPriceList {
		if deltaOK && math.Abs(delta-res.CostUSD) > 0.01 {
			l.Log.Warn("the price list and the client's running total disagree on a turn's cost",
				"price_list_usd", res.CostUSD, "client_total_delta_usd", delta, "price_list", PriceListVersion)
		}
		return res.CostUSD, CostPriceList
	}
	if !deltaOK {
		return 0, CostUnpriced
	}
	return delta, CostClientTotal
}
