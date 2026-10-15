package agent

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"math"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/BlackVS/aicrew/internal/store"
	"github.com/BlackVS/aicrew/internal/toolchain"
)

// fakeHost is the launcher's session as the loop sees it.
type fakeHost struct {
	mu      sync.Mutex
	pending []PendingMessage
	paused  *PauseNote
	attempt *memberAttempt
	role    string
	steps   int64
	calls   []StepCall
	polls   int
}

func (h *fakeHost) PendingNow(context.Context) ([]PendingMessage, *PauseNote, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]PendingMessage(nil), h.pending...), h.paused, nil
}

func (h *fakeHost) Status(context.Context) (memberStatus, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	var st memberStatus
	st.Session.AgentID, st.Session.TeamID, st.Session.Role = "agent-1", "team-1", h.role
	if h.attempt != nil {
		a := *h.attempt
		st.Attempt = &a
	}
	return st, nil
}

func (h *fakeHost) StepsDone() int64 {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.steps
}

func (h *fakeHost) Do(_ context.Context, call StepCall) StepAnswer {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.calls = append(h.calls, call)
	if call.Op == "work" && h.attempt != nil {
		h.attempt.Phase = "blocked"
	}
	return StepAnswer{OK: true, Status: StepDone}
}

func (h *fakeHost) PollPulls(context.Context) { h.mu.Lock(); h.polls++; h.mu.Unlock() }

// loopEnv is a turn loop over a fake host, a fake client and a fake clock.
type loopEnv struct {
	home  string
	host  *fakeHost
	loop  *TurnLoop
	clock time.Time
	specs []TurnSpec
	// next is what the fake client's next turn does: its result, and a
	// change to the host (the member's work).
	next func(spec TurnSpec, h *fakeHost) TurnResult
	head string
}

func newLoopEnv(t *testing.T) *loopEnv {
	t.Helper()
	e := &loopEnv{home: t.TempDir(), host: &fakeHost{role: "worker"}, clock: time.Date(2026, 10, 12, 9, 0, 0, 0, time.UTC)}
	e.next = func(TurnSpec, *fakeHost) TurnResult {
		return TurnResult{Outcome: TurnFinished, CostUSD: 0.5, Denials: []string{}}
	}
	e.loop = &TurnLoop{Home: e.home, Client: "claude", Host: e.host, Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
		Now:  func() time.Time { return e.clock },
		Head: func(context.Context, string) string { return e.head },
		Run: func(_ context.Context, spec TurnSpec) TurnResult {
			e.specs = append(e.specs, spec)
			return e.next(spec, e.host)
		}}
	return e
}

// tick advances the clock past the turn gap and ticks once.
func (e *loopEnv) tick(t *testing.T) {
	t.Helper()
	e.clock = e.clock.Add(turnGap + time.Second)
	e.loop.Tick(context.Background())
}

func (e *loopEnv) records(t *testing.T) []TurnRecord {
	t.Helper()
	f, err := os.Open(filepath.Join(e.home, filepath.FromSlash(TurnLogFile)))
	if err != nil {
		return nil
	}
	defer f.Close()
	var out []TurnRecord
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		var r TurnRecord
		if err := json.Unmarshal(sc.Bytes(), &r); err != nil {
			t.Fatal(err)
		}
		out = append(out, r)
	}
	return out
}

func msg(id, kind string) PendingMessage { return PendingMessage{ID: id, Kind: kind} }

// A message gives one turn that starts a conversation; the prompt names
// the messages' kinds and ids, never their text. Messages that keep
// arriving within the turn gap coalesce, and nothing pending gives no turn.
// The next message's turn resumes the member's conversation, which rotates
// on a new day.
func TestTurnLoopMessages(t *testing.T) {
	e := newLoopEnv(t)
	e.host.role = "coordinator"
	e.tick(t)
	if len(e.specs) != 0 {
		t.Fatal("a turn with nothing pending")
	}
	e.host.pending = []PendingMessage{msg("m1", "lifecycle"), msg("m2", "message")}
	e.next = func(TurnSpec, *fakeHost) TurnResult {
		e.host.pending = nil // the turn read and acknowledged them
		return TurnResult{Outcome: TurnFinished, CostUSD: 0.5, Summary: "done; the token was ghp_0123456789abcdef"}
	}
	e.tick(t)
	if len(e.specs) != 1 || e.specs[0].Resume || e.specs[0].Prompt != "/crew-turn 2 new message(s) in your inbox (1 lifecycle, 1 message; ids m1, m2)" {
		t.Fatalf("the first turn: %+v", e.specs)
	}
	first := e.specs[0].Conversation
	e.host.pending = []PendingMessage{msg("m3", "message")}
	e.clock = e.clock.Add(time.Second) // within the gap: coalesced
	e.loop.Tick(context.Background())
	if len(e.specs) != 1 {
		t.Fatal("a second turn within the turn gap")
	}
	e.tick(t)
	if len(e.specs) != 2 || !e.specs[1].Resume || e.specs[1].Conversation != first {
		t.Fatalf("the second turn did not resume the conversation: %+v", e.specs[1])
	}
	e.host.pending = []PendingMessage{msg("m4", "message")}
	e.clock = e.clock.Add(24 * time.Hour)
	e.tick(t)
	if len(e.specs) != 3 || e.specs[2].Resume {
		t.Fatalf("a new day did not start a fresh conversation: %+v", e.specs[2])
	}
	recs := e.records(t)
	if len(recs) != 3 || recs[0].Outcome != TurnFinished || recs[0].Conversation != first || recs[0].Trigger == "" ||
		strings.Contains(recs[0].Summary, "ghp_0123456789abcdef") || !strings.Contains(recs[0].Summary, "[redacted]") {
		t.Fatalf("the records: %+v", recs)
	}
	if e.host.polls == 0 {
		t.Error("the loop never watched the pull requests")
	}
}

// No turn while the member or its team is paused.
func TestTurnLoopPaused(t *testing.T) {
	e := newLoopEnv(t)
	e.host.pending = []PendingMessage{msg("m1", "message")}
	e.host.paused = &PauseNote{Reason: "upgrade"}
	e.tick(t)
	if len(e.specs) != 0 {
		t.Fatal("a turn while paused")
	}
	e.host.paused = nil
	e.tick(t)
	if len(e.specs) != 1 {
		t.Fatal("no turn after the resume")
	}
}

// A worker whose attempt is still in work after its turn gets continuation
// turns in the attempt's own conversation, up to the limit; a submitted
// attempt gets none; a new attempt starts a fresh conversation.
func TestTurnLoopContinuation(t *testing.T) {
	e := newLoopEnv(t)
	writeAgentDoc(t, e.home, map[string]any{"turns": map[string]any{"max_continuations": 2}})
	e.host.attempt = &memberAttempt{ID: "att-1", State: "running", Phase: "working", Task: TaskRef{TaskID: "task-1"}}
	e.host.pending = []PendingMessage{msg("offer", "lifecycle")}
	commit := 0
	e.next = func(TurnSpec, *fakeHost) TurnResult {
		e.host.pending = nil
		commit++
		e.head = strings.Repeat("a", commit) // each turn commits
		return TurnResult{Outcome: TurnFinished, CostUSD: 0.1}
	}
	for range 5 {
		e.tick(t)
	}
	if len(e.specs) != 3 {
		t.Fatalf("%d turns, want the message's and two continuations", len(e.specs))
	}
	conv := e.specs[0].Conversation
	for i, s := range e.specs[1:] {
		if !s.Resume || s.Conversation != conv || !strings.Contains(s.Prompt, "a continuation of attempt att-1") {
			t.Fatalf("continuation %d: %+v", i, s)
		}
	}
	// A message resets nothing of the limit, but gets its turn.
	e.host.pending = []PendingMessage{msg("review", "lifecycle")}
	e.tick(t)
	if len(e.specs) != 4 {
		t.Fatal("a message got no turn after the continuations were spent")
	}
	// The worker submits: no continuation.
	e.host.attempt = &memberAttempt{ID: "att-2", State: "running", Phase: "working", Task: TaskRef{TaskID: "task-2"}}
	e.host.pending = []PendingMessage{msg("offer-2", "lifecycle")}
	e.next = func(TurnSpec, *fakeHost) TurnResult {
		e.host.pending = nil
		e.host.attempt.Phase = "submitted"
		return TurnResult{Outcome: TurnFinished}
	}
	e.tick(t)
	e.tick(t)
	if len(e.specs) != 5 || e.specs[4].Resume || e.specs[4].Conversation == conv {
		t.Fatalf("a new attempt's turn: %+v (%d turns)", e.specs[len(e.specs)-1], len(e.specs))
	}
}

// Three turns of an attempt in work with no step and no new commit stall
// it: the loop blocks it through the work step and gives it no more
// continuation turns; a message gives a turn again.
func TestTurnLoopStall(t *testing.T) {
	e := newLoopEnv(t)
	e.host.attempt = &memberAttempt{ID: "att-1", State: "running", Phase: "working", Task: TaskRef{TaskID: "task-1"}}
	e.host.pending = []PendingMessage{msg("offer", "lifecycle")}
	e.head = "c1"
	e.next = func(TurnSpec, *fakeHost) TurnResult {
		e.host.pending = nil
		return TurnResult{Outcome: TurnFinished}
	}
	for range 6 {
		e.tick(t)
	}
	if len(e.specs) != 3 || len(e.host.calls) != 1 {
		t.Fatalf("%d turns and %d steps, want 3 turns and one block", len(e.specs), len(e.host.calls))
	}
	var body struct{ Intent, Detail string }
	c := e.host.calls[0]
	if json.Unmarshal(c.Body, &body) != nil || c.Op != "work" || c.AttemptID != "att-1" || c.TaskID != "task-1" ||
		body.Intent != "block" || !strings.Contains(body.Detail, "stalled") {
		t.Fatalf("the stall's step: %+v %s", c, c.Body)
	}
	// A step in a turn is progress.
	e2 := newLoopEnv(t)
	e2.host.attempt = &memberAttempt{ID: "att-1", State: "running", Phase: "working", Task: TaskRef{TaskID: "task-1"}}
	e2.host.pending = []PendingMessage{msg("offer", "lifecycle")}
	e2.next = func(TurnSpec, *fakeHost) TurnResult {
		e2.host.pending = nil
		e2.host.steps++
		return TurnResult{Outcome: TurnFinished}
	}
	for range 5 {
		e2.tick(t)
	}
	if len(e2.host.calls) != 0 || len(e2.specs) != 5 {
		t.Fatalf("a worker that steps each turn stalled: %d turns, %d blocks", len(e2.specs), len(e2.host.calls))
	}
}

// Each outcome holds the member as 1.6 and 3.4 say: a provider limit until
// its reset, a login failure for an hour and then longer, a failure for a
// minute; a finished or stopped turn holds nothing. A failed resume starts
// the next conversation fresh.
func TestTurnLoopOutcomes(t *testing.T) {
	for _, c := range []struct {
		res  TurnResult
		hold time.Duration
	}{
		{TurnResult{Outcome: TurnFinished}, 0},
		{TurnResult{Outcome: TurnStopped}, 0},
		{TurnResult{Outcome: TurnLimitReached, LimitKind: "budget"}, 0},
		{TurnResult{Outcome: TurnLimitReached, LimitKind: "provider"}, time.Hour},
		{TurnResult{Outcome: TurnNeedsLogin}, time.Hour},
		{TurnResult{Outcome: TurnFailed}, time.Minute},
	} {
		e := newLoopEnv(t)
		e.host.pending = []PendingMessage{msg("m1", "message")}
		e.next = func(TurnSpec, *fakeHost) TurnResult { return c.res }
		e.tick(t)
		st := readTurnState(e.home)
		if got := st.HoldUntil.Sub(e.clock); (c.hold == 0 && !st.HoldUntil.IsZero()) || (c.hold != 0 && got != c.hold) {
			t.Errorf("%s/%s: held for %s, want %s", c.res.Outcome, c.res.LimitKind, got, c.hold)
		}
		if recs := e.records(t); len(recs) != 1 || recs[0].Outcome != c.res.Outcome {
			t.Errorf("%s: records %+v", c.res.Outcome, recs)
		}
	}
	// A provider limit with its reset time holds until then; a second login
	// failure doubles the hold.
	e := newLoopEnv(t)
	e.host.pending = []PendingMessage{msg("m1", "message")}
	reset := e.clock.Add(3 * time.Hour)
	e.next = func(TurnSpec, *fakeHost) TurnResult {
		return TurnResult{Outcome: TurnLimitReached, LimitKind: "provider", ResetAt: reset}
	}
	e.tick(t)
	if st := readTurnState(e.home); !st.HoldUntil.Equal(reset) {
		t.Fatalf("held until %s, want the reset %s", st.HoldUntil, reset)
	}
	e.tick(t)
	if len(e.specs) != 1 {
		t.Fatal("a turn before the reset")
	}
	e.clock = reset
	e.next = func(TurnSpec, *fakeHost) TurnResult { return TurnResult{Outcome: TurnNeedsLogin} }
	e.tick(t)
	e.clock = e.clock.Add(time.Hour)
	e.tick(t)
	if st := readTurnState(e.home); st.HoldUntil.Sub(e.clock) != 2*time.Hour || len(e.specs) != 3 {
		t.Fatalf("a second login failure: held until %s (%d turns)", st.HoldUntil, len(e.specs))
	}
	// A failed resume: the next turn starts fresh.
	e = newLoopEnv(t)
	e.host.pending = []PendingMessage{msg("m1", "message")}
	e.tick(t)
	e.next = func(TurnSpec, *fakeHost) TurnResult { return TurnResult{Outcome: TurnFailed, Error: "no conversation"} }
	e.tick(t)
	e.next = func(TurnSpec, *fakeHost) TurnResult { return TurnResult{Outcome: TurnFinished} }
	e.clock = e.clock.Add(time.Minute)
	e.tick(t)
	if len(e.specs) != 3 || !e.specs[1].Resume || e.specs[2].Resume {
		t.Fatalf("after a failed resume: %+v", e.specs)
	}
}

// A spent daily budget gives no turn until the next day; a spent attempt
// budget none for that attempt.
func TestTurnLoopBudgets(t *testing.T) {
	e := newLoopEnv(t)
	writeAgentDoc(t, e.home, map[string]any{"turns": map[string]any{"daily_budget_usd": 1, "attempt_budget_usd": 100}})
	e.host.pending = []PendingMessage{msg("m1", "message")}
	e.next = func(TurnSpec, *fakeHost) TurnResult { return TurnResult{Outcome: TurnFinished, CostUSD: 0.6} }
	for range 4 {
		e.tick(t)
	}
	if len(e.specs) != 2 {
		t.Fatalf("%d turns within a $1 day at $0.60 a turn", len(e.specs))
	}
	if st := readTurnState(e.home); !st.HoldUntil.Equal(nextDay(e.clock)) {
		t.Fatalf("held until %s, want the next day", st.HoldUntil)
	}
	e.clock = nextDay(e.clock)
	e.tick(t)
	if len(e.specs) != 3 {
		t.Fatal("no turn on the next day")
	}

	e = newLoopEnv(t)
	writeAgentDoc(t, e.home, map[string]any{"turns": map[string]any{"attempt_budget_usd": 1}})
	e.host.attempt = &memberAttempt{ID: "att-1", State: "running", Phase: "submitted", Task: TaskRef{TaskID: "task-1"}}
	e.host.pending = []PendingMessage{msg("m1", "message")}
	e.next = func(TurnSpec, *fakeHost) TurnResult { return TurnResult{Outcome: TurnFinished, CostUSD: 1.5} }
	e.tick(t)
	e.tick(t)
	if len(e.specs) != 1 || !strings.Contains(readTurnState(e.home).Hold, "att-1") {
		t.Fatalf("an attempt over its budget: %d turns, hold %q", len(e.specs), readTurnState(e.home).Hold)
	}
}

// A turn left running by an earlier loop is recorded as interrupted when the
// loop starts again.
func TestTurnLoopInterrupted(t *testing.T) {
	e := newLoopEnv(t)
	st := readTurnState(e.home)
	st.Running = &TurnRecord{ID: "turn-1", Trigger: "1 new message(s)", StartedAt: e.clock, Denials: []string{}}
	if err := writeTurnState(e.home, st); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_ = e.loop.Loop(ctx)
	recs := e.records(t)
	if len(recs) != 1 || recs[0].ID != "turn-1" || recs[0].Outcome != TurnInterrupted || readTurnState(e.home).Running != nil {
		t.Fatalf("records %+v", recs)
	}
}

// The limits come from agent.json's turns object, with the design's
// defaults for what it leaves out or names out of range.
func TestReadTurnLimits(t *testing.T) {
	home := t.TempDir()
	if l := ReadTurnLimits(home); l.TurnTimeout != 30*time.Minute || l.TurnBudgetUSD != 2 || l.AttemptBudgetUSD != 20 ||
		l.DailyBudgetUSD != 50 || l.MaxContinuations != 20 || l.Model != "" {
		t.Fatalf("defaults: %+v", l)
	}
	writeAgentDoc(t, home, map[string]any{"turns": map[string]any{"turn_timeout": "45m", "turn_budget_usd": 1.5,
		"max_continuations": 0, "model": "claude-sonnet-5-5", "daily_budget_usd": -1}})
	if l := ReadTurnLimits(home); l.TurnTimeout != 45*time.Minute || l.TurnBudgetUSD != 1.5 || l.MaxContinuations != 0 ||
		l.Model != "claude-sonnet-5-5" || l.DailyBudgetUSD != 50 {
		t.Fatalf("set: %+v", l)
	}
	writeAgentDoc(t, home, map[string]any{"turns": map[string]any{"turn_timeout": "1s", "model": "--dangerously"}})
	if l := ReadTurnLimits(home); l.TurnTimeout != 30*time.Minute || l.Model != "" {
		t.Fatalf("out of range: %+v", l)
	}
}

// A turn that opens an attempt (a claim, an accepted offer) is the
// attempt's first turn: its record names the attempt, its cost is the
// attempt's, and the attempt's continuation resumes its conversation.
func TestTurnLoopAttemptOpenedInATurn(t *testing.T) {
	e := newLoopEnv(t)
	writeAgentDoc(t, e.home, map[string]any{"turns": map[string]any{"attempt_budget_usd": 1}})
	e.host.role = "independent"
	e.host.pending = []PendingMessage{msg("m1", "message")}
	e.next = func(TurnSpec, *fakeHost) TurnResult {
		e.host.pending = nil
		e.host.attempt = &memberAttempt{ID: "att-9", State: "running", Phase: "working", Task: TaskRef{TaskID: "task-9"}}
		e.host.steps++ // the claim
		return TurnResult{Outcome: TurnFinished, CostUSD: 0.4}
	}
	e.tick(t)
	e.next = func(TurnSpec, *fakeHost) TurnResult { return TurnResult{Outcome: TurnFinished, CostUSD: 0.7} }
	e.tick(t)
	if len(e.specs) != 2 || !e.specs[1].Resume || e.specs[1].Conversation != e.specs[0].Conversation {
		t.Fatalf("the continuation did not resume the claiming turn's conversation: %+v", e.specs)
	}
	recs := e.records(t)
	if recs[0].AttemptID != "att-9" || recs[0].Task == nil || recs[0].Task.TaskID != "task-9" {
		t.Fatalf("the claiming turn's record: %+v", recs[0])
	}
	st := readTurnState(e.home)
	if a := st.Attempts["att-9"]; a == nil || a.CostUSD != 1.1 || st.Member.ID != "" {
		t.Fatalf("the attempt's state: %+v, member %+v", st.Attempts["att-9"], st.Member)
	}
	e.tick(t)
	if len(e.specs) != 2 {
		t.Fatal("a turn after the attempt's budget was spent")
	}
}

// Only a step that ran counts as progress: a request the channel refuses
// before anything is recorded does not, and a committed step does.
func TestStepsDoneCountsStepsThatRan(t *testing.T) {
	s := setupStepsWith(t, crewOptions{role: store.RoleIndependent, steps: true, shortHome: true})
	srv := s.serve(t)
	var answers []byte
	for _, c := range []StepCall{
		{Op: "work", AttemptID: "01a12ff0-5afa-7000-95ab-3277b0539633", Body: []byte(`{"intent":"block","detail":"x"}`)},
		{Op: "review"},
		{Op: "fly"},
	} {
		if ans := s.call(t, &answers, c); ans.OK {
			t.Fatalf("%s was not refused: %+v", c.Op, ans)
		}
	}
	if n := srv.StepsDone(); n != 0 {
		t.Fatalf("refused requests counted %d steps", n)
	}
	body, _ := json.Marshal(s.claimBody())
	if ans := s.call(t, &answers, StepCall{Op: "claim", Body: body}); !ans.OK {
		t.Fatalf("the claim: %+v", ans)
	}
	if n := srv.StepsDone(); n != 1 {
		t.Fatalf("a committed claim counted %d steps", n)
	}
}

// A coordinator's turn runs under the coordinator's per-turn settings, and
// any other member's under the common ones (pilot finding #15). A home not
// rejoined since the upgrade, without the coordinator's file, falls back to
// the common one rather than pass a missing file.
func TestTurnLoopSettingsByRole(t *testing.T) {
	for role, want := range map[string]string{"coordinator": CoordinatorTurnSettingsFile, "worker": TurnSettingsFile,
		"independent": TurnSettingsFile, "coordinator without its file": TurnSettingsFile} {
		e := newLoopEnv(t)
		e.host.role, _, _ = strings.Cut(role, " ")
		if role == "coordinator" {
			os.MkdirAll(filepath.Join(e.home, ".claude"), 0o700)
			os.WriteFile(filepath.Join(e.home, filepath.FromSlash(CoordinatorTurnSettingsFile)), []byte("{}"), 0o600)
		}
		e.host.pending = []PendingMessage{msg("m1", "lifecycle")}
		e.tick(t)
		if len(e.specs) != 1 || e.specs[0].Settings != want {
			t.Errorf("%s: %d turns, settings %+v", role, len(e.specs), e.specs)
		}
		if args := ClaudeTurnArgs(e.specs[0]); !slices.Contains(args, want) {
			t.Errorf("%s: the turn's arguments %q", role, args)
		}
	}
}

// A worker whose team selects a toolchain for a project runs its turn under
// the common rules and the toolchain's, which the loop writes from the team
// record (pilot finding #18). A coordinator, and an independent reviewer,
// keep their own files; a worker whose team selects none, or only names
// this build does not know, keeps the common one.
func TestTurnLoopWorkerToolchain(t *testing.T) {
	record := func(t *testing.T, home string, chains ...string) {
		t.Helper()
		rec := TeamRecord{}
		for i, c := range chains {
			rec.Projects = append(rec.Projects, TeamProject{HubID: "hub-a", ProjectID: fmt.Sprintf("p%d", i), Toolchain: c})
		}
		if err := SaveTeam(home, rec); err != nil {
			t.Fatal(err)
		}
	}
	cases := []struct {
		name, role string
		chains     []string
		want       string
	}{
		{"dev-go", "worker", []string{"dev-go", "", "dev-go"}, WorkerToolchainSettingsFile},
		{"none selected", "worker", []string{""}, TurnSettingsFile},
		{"no record", "worker", nil, TurnSettingsFile},
		{"unknown", "worker", []string{"dev-cobol"}, TurnSettingsFile},
		{"coordinator", "coordinator", []string{"dev-go"}, CoordinatorTurnSettingsFile},
		{"independent", "independent", []string{"dev-go"}, TurnSettingsFile},
	}
	for _, c := range cases {
		e := newLoopEnv(t)
		e.host.role = c.role
		os.MkdirAll(filepath.Join(e.home, ".claude"), 0o700)
		os.WriteFile(filepath.Join(e.home, filepath.FromSlash(CoordinatorTurnSettingsFile)), []byte("{}"), 0o600)
		if c.chains != nil {
			record(t, e.home, c.chains...)
		}
		e.host.pending = []PendingMessage{msg("m1", "lifecycle")}
		e.tick(t)
		if len(e.specs) != 1 || e.specs[0].Settings != c.want {
			t.Errorf("%s: %d turns, settings %+v", c.name, len(e.specs), e.specs)
			continue
		}
		raw, err := os.ReadFile(filepath.Join(e.home, filepath.FromSlash(WorkerToolchainSettingsFile)))
		if c.want != WorkerToolchainSettingsFile {
			if err == nil {
				t.Errorf("%s: the worker's toolchain file was written", c.name)
			}
			continue
		}
		var doc struct {
			Permissions struct{ Allow, Deny []string }
		}
		if err != nil || json.Unmarshal(raw, &doc) != nil {
			t.Fatalf("%s: %v %s", c.name, err, raw)
		}
		common := turnAllow(e.home, false)
		if !slices.Equal(doc.Permissions.Allow[:len(common)], common) ||
			!slices.Equal(doc.Permissions.Allow[len(common):], toolchain.Rules("dev-go")) {
			t.Errorf("%s: allow %q", c.name, doc.Permissions.Allow)
		}
		var common2 struct {
			Permissions struct{ Deny []string }
		}
		_ = json.Unmarshal([]byte(turnSettings(e.home, false)), &common2)
		if !slices.Equal(doc.Permissions.Deny, common2.Permissions.Deny) || len(doc.Permissions.Deny) == 0 {
			t.Errorf("%s: deny %q", c.name, doc.Permissions.Deny)
		}
	}
	// A selection cleared since the last turn removes the worker's file.
	e := newLoopEnv(t)
	e.host.role = "worker"
	record(t, e.home, "dev-go")
	e.host.pending = []PendingMessage{msg("m1", "lifecycle")}
	e.tick(t)
	record(t, e.home, "")
	e.clock = e.clock.Add(time.Hour)
	e.host.pending = []PendingMessage{msg("m2", "lifecycle")}
	e.tick(t)
	if _, err := os.Stat(filepath.Join(e.home, filepath.FromSlash(WorkerToolchainSettingsFile))); len(e.specs) != 2 ||
		e.specs[1].Settings != TurnSettingsFile || err == nil {
		t.Errorf("after a clear: %+v, the file %v", e.specs, err)
	}
	// The coordinator's and the common files never carry a toolchain.
	home := t.TempDir()
	for _, coordinator := range []bool{false, true} {
		if s := turnSettings(home, coordinator); strings.Contains(s, "go test") {
			t.Errorf("coordinator %v: %s", coordinator, s)
		}
	}
}

// A Claude Code turn's total_cost_usd is the conversation's running total
// (docs/DESIGN-TURNS.md, 2.1): a resumed turn's record and the attempt's
// spend take only the turn's own cost, the difference since the previous
// turn, or the price list's cost when it priced the turn. A resumed
// conversation whose previous total was not kept leaves the turn unpriced
// rather than charging the whole total again.
func TestTurnLoopTurnOwnCost(t *testing.T) {
	e := newLoopEnv(t)
	writeAgentDoc(t, e.home, map[string]any{"turns": map[string]any{"attempt_budget_usd": 100}})
	e.host.attempt = &memberAttempt{ID: "att-1", State: "running", Phase: "working", Task: TaskRef{TaskID: "task-1"}}
	e.host.pending = []PendingMessage{msg("m1", "message")}
	results := []TurnResult{
		{Outcome: TurnFinished, CostSource: CostClientTotal, ClientTotalUSD: 1.2, Models: []string{"m"}},
		{Outcome: TurnFinished, CostSource: CostClientTotal, ClientTotalUSD: 2.56, Models: []string{"m"}},
		{Outcome: TurnFinished, CostSource: CostPriceList, CostUSD: 0.13, ClientTotalUSD: 2.69,
			Models: []string{"claude-opus-5-5"}},
	}
	e.next = func(TurnSpec, *fakeHost) TurnResult {
		e.host.pending = nil
		e.host.steps++ // progress, so no stall
		r := results[0]
		results = results[1:]
		return r
	}
	for range 3 {
		e.tick(t)
	}
	if len(e.specs) != 3 || e.specs[0].Resume || !e.specs[1].Resume || !e.specs[2].Resume {
		t.Fatalf("three turns of one conversation: %+v", e.specs)
	}
	recs := e.records(t)
	want := []struct {
		cost   float64
		source string
		model  string
	}{{1.2, CostClientTotal, "m"}, {1.36, CostClientTotal, "m"}, {0.13, CostPriceList, "claude-opus-5-5"}}
	for i, w := range want {
		r := recs[i]
		if r.CostUSD != w.cost || r.CostSource != w.source || r.Model != w.model || r.Role != "worker" {
			t.Errorf("turn %d: cost %v from %q, model %q, role %q; want %v from %q", i+1, r.CostUSD, r.CostSource,
				r.Model, r.Role, w.cost, w.source)
		}
	}
	if recs[1].ClientTotalUSD != 2.56 || recs[2].PriceList != PriceListVersion || recs[1].PriceList != "" {
		t.Errorf("the running total and the price list are recorded as such: %+v %+v", recs[1], recs[2])
	}
	st := readTurnState(e.home)
	if a := st.Attempts["att-1"]; a == nil || math.Abs(a.CostUSD-2.69) > 1e-9 {
		t.Fatalf("the attempt spent %+v, want the sum of the turns' own costs, 2.69", st.Attempts["att-1"])
	}
	if day := st.Days[dayOf(e.clock)]; math.Abs(day-2.69) > 1e-9 {
		t.Fatalf("the day spent %v, want 2.69", day)
	}

	// A state from before the running total was kept: the resumed turn's
	// own cost is unknown, and the whole total is not charged again.
	a := st.Attempts["att-1"]
	a.Conversation.ClientTotalUSD = nil
	if err := writeTurnState(e.home, st); err != nil {
		t.Fatal(err)
	}
	e.host.pending = []PendingMessage{msg("m2", "message")}
	e.next = func(TurnSpec, *fakeHost) TurnResult {
		e.host.pending = nil
		e.host.steps++
		return TurnResult{Outcome: TurnFinished, CostSource: CostClientTotal, ClientTotalUSD: 3.0, Models: []string{"m"}}
	}
	e.tick(t)
	recs = e.records(t)
	if last := recs[len(recs)-1]; last.CostUSD != 0 || last.CostSource != CostUnpriced || last.ClientTotalUSD != 3.0 {
		t.Fatalf("an unknown previous total: %+v", last)
	}
	if st := readTurnState(e.home); math.Abs(st.Attempts["att-1"].CostUSD-2.69) > 1e-9 ||
		*st.Attempts["att-1"].Conversation.ClientTotalUSD != 3.0 {
		t.Fatalf("after an unpriced turn: %+v", st.Attempts["att-1"])
	}
}
