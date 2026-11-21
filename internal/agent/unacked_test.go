package agent

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// unackedEnv is a member with no attempt and one message its turns never
// acknowledge.
func unackedEnv(t *testing.T) *loopEnv {
	t.Helper()
	e := newLoopEnv(t)
	e.host.role = "coordinator"
	e.host.pending = []PendingMessage{msg("m1", "action.needed")}
	e.next = func(TurnSpec, *fakeHost) TurnResult {
		return TurnResult{Outcome: TurnFinished, CostUSD: 0.24, Summary: "The delivery check was denied.\x1b[2J"}
	}
	return e
}

// ticksTo ticks until the clock reaches at, one turn gap at a time.
func (e *loopEnv) ticksTo(t *testing.T, at time.Time) {
	t.Helper()
	for e.clock.Add(turnGap + time.Second).Before(at) {
		e.tick(t)
	}
}

func lastHold(e *loopEnv) holdReport {
	e.host.mu.Lock()
	defer e.host.mu.Unlock()
	if len(e.host.holds) == 0 {
		return holdReport{}
	}
	return e.host.holds[len(e.host.holds)-1]
}

// Turns that finish and leave the same message unacknowledged wait 5 and
// then 20 minutes, and after the third the member is held and the hold is
// reported with the message, the count, the last summary on one line and
// the next check a day on. While held, no message gives no turn until the
// day's re-check, and a new message gives one turn no sooner than 20
// minutes after the last: it does not restart the count, and the grown set
// is the same hold (the same count start).
func TestUnackedWaitsAndHold(t *testing.T) {
	e := unackedEnv(t)
	e.tick(t)
	if len(e.specs) != 1 {
		t.Fatalf("%d turns, want the first", len(e.specs))
	}
	first := readTurnState(e.home).Unacked
	if first == nil || first.Turns != 1 || strings.Join(first.Messages, ",") != "m1" {
		t.Fatalf("the count after one turn: %+v", first)
	}
	end1 := first.LastEnded
	e.ticksTo(t, end1.Add(defaultUnackedWait))
	if len(e.specs) != 1 {
		t.Fatalf("a turn %s after the first, before its wait", e.clock.Sub(end1))
	}
	e.tick(t)
	if len(e.specs) != 2 {
		t.Fatalf("%d turns after the first wait", len(e.specs))
	}
	if !strings.Contains(e.specs[1].Prompt, "This is turn 2 on message(s) m1") ||
		!strings.Contains(e.specs[1].Prompt, "The delivery check was denied.") ||
		strings.Contains(e.specs[1].Prompt, "\x1b") {
		t.Fatalf("the second turn's brief does not name the earlier turn:\n%s", e.specs[1].Prompt)
	}
	end2 := readTurnState(e.home).Unacked.LastEnded
	e.ticksTo(t, end2.Add(unackedCeiling*defaultUnackedWait))
	if len(e.specs) != 2 {
		t.Fatal("a third turn before the second wait")
	}
	e.tick(t)
	if len(e.specs) != 3 {
		t.Fatalf("%d turns after the second wait", len(e.specs))
	}
	st := readTurnState(e.home)
	h := lastHold(e)
	if h.Kind != holdUnacknowledged || h.Turns != 3 || strings.Join(h.Messages, ",") != "m1" ||
		h.Summary != "The delivery check was denied. [2J" || !h.StuckSince.Equal(first.Since) ||
		!h.NextCheck.Equal(st.Unacked.LastEnded.Add(unackedRecheck)) {
		t.Fatalf("the hold reported: %+v", h)
	}
	end3 := st.Unacked.LastEnded
	e.host.mu.Lock()
	reported := len(e.host.holds)
	e.host.mu.Unlock()

	// Held: no turn for the old message within the day.
	e.ticksTo(t, end3.Add(time.Hour))
	if len(e.specs) != 3 {
		t.Fatalf("%d turns while held", len(e.specs))
	}
	if h := lastHold(e); h.Kind != holdUnacknowledged {
		t.Fatalf("the hold was cleared while held: %+v", h)
	}

	// A new message: one turn, 20 minutes after the last (already past),
	// and the count goes on.
	e.host.pending = append(e.host.pending, msg("m2", "message"))
	e.tick(t)
	if len(e.specs) != 4 {
		t.Fatalf("%d turns after a new message to a held member", len(e.specs))
	}
	st = readTurnState(e.home)
	h = lastHold(e)
	if st.Unacked.Turns != 4 || h.Turns != 4 || strings.Join(h.Messages, ",") != "m1,m2" ||
		!h.StuckSince.Equal(first.Since) {
		t.Fatalf("after the new message's turn: %+v, hold %+v", st.Unacked, h)
	}
	// The held member's turn does not clear the hold aicrewd shows.
	e.host.mu.Lock()
	for _, r := range e.host.holds[reported:] {
		if r.same(holdReport{}) {
			t.Errorf("the hold was cleared for a held member's turn: %+v", e.host.holds[reported:])
		}
	}
	e.host.mu.Unlock()
	end4 := st.Unacked.LastEnded
	e.host.pending = append(e.host.pending, msg("m3", "message"))
	e.ticksTo(t, end4.Add(unackedCeiling*defaultUnackedWait))
	if len(e.specs) != 4 {
		t.Fatal("a second new message gave a turn sooner than 20 minutes after the last")
	}
	e.tick(t)
	if len(e.specs) != 5 {
		t.Fatalf("%d turns 20 minutes after the last", len(e.specs))
	}

	// No new message: the day's re-check.
	end5 := readTurnState(e.home).Unacked.LastEnded
	e.clock = end5.Add(unackedRecheck - time.Minute)
	e.tick(t)
	if len(e.specs) != 5 {
		t.Fatal("a turn before the day's re-check")
	}
	e.clock = end5.Add(unackedRecheck)
	e.tick(t)
	if len(e.specs) != 6 {
		t.Fatalf("%d turns at the day's re-check", len(e.specs))
	}
}

// An acknowledgement by the member clears the count: outside a turn, or in
// one. The next turn then has no wait and no "turn N" in its brief.
func TestUnackedClearedByAck(t *testing.T) {
	e := unackedEnv(t)
	e.tick(t)
	e.clock = readTurnState(e.home).Unacked.LastEnded.Add(defaultUnackedWait)
	e.tick(t)
	if n := readTurnState(e.home).Unacked.Turns; n != 2 {
		t.Fatalf("%d turns counted", n)
	}
	e.host.ack("m1")
	e.host.pending = append(e.host.pending, msg("m2", "message"))
	e.tick(t)
	if len(e.specs) != 3 || strings.Contains(e.specs[2].Prompt, "This is turn") {
		t.Fatalf("after an acknowledgement: %d turns", len(e.specs))
	}
	if u := readTurnState(e.home).Unacked; u == nil || u.Turns != 1 || strings.Join(u.Messages, ",") != "m2" {
		t.Fatalf("a new count for the new message: %+v", u)
	}

	// A turn that acknowledges its message leaves no count.
	e.next = func(_ TurnSpec, h *fakeHost) TurnResult {
		h.ack("m2")
		return TurnResult{Outcome: TurnFinished}
	}
	e.clock = readTurnState(e.home).Unacked.LastEnded.Add(defaultUnackedWait)
	e.tick(t)
	if u := readTurnState(e.home).Unacked; u != nil {
		t.Fatalf("a turn that acknowledged: %+v", u)
	}
}

// A pause clears the count and the hold aicrewd shows; after the resume the
// message gets a turn at once.
func TestUnackedClearedByPause(t *testing.T) {
	e := unackedEnv(t)
	for range 3 {
		if u := readTurnState(e.home).Unacked; u != nil {
			e.clock = u.LastEnded.Add(unackedCeiling * defaultUnackedWait)
		}
		e.tick(t)
	}
	if h := lastHold(e); len(e.specs) != 3 || h.Kind != holdUnacknowledged {
		t.Fatalf("%d turns, hold %+v", len(e.specs), h)
	}
	e.host.paused = &PauseNote{Reason: "the operator"}
	e.tick(t)
	if st := readTurnState(e.home); st.Unacked != nil || st.HoldReport != nil || !lastHold(e).same(holdReport{}) {
		t.Fatalf("the pause did not clear the count and the hold: %+v, hold %+v", st.Unacked, lastHold(e))
	}
	e.host.paused = nil
	e.tick(t)
	if len(e.specs) != 4 {
		t.Fatalf("%d turns after the resume", len(e.specs))
	}
}

// tickStopping ticks once, and the loop is stopping during the turn, which
// ends stopped.
func (e *loopEnv) tickStopping(t *testing.T) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	next := e.next
	e.next = func(TurnSpec, *fakeHost) TurnResult {
		cancel()
		return TurnResult{Outcome: TurnStopped, Error: "the turn loop is stopping", LoopStopping: true}
	}
	defer func() { e.next = next }()
	e.clock = e.clock.Add(turnGap + time.Second)
	e.loop.Tick(ctx)
}

// A turn cut by its own budget, or stopped by its time limit, that leaves
// the set unchanged counts; one stopped because the loop is stopping, or a
// failed one, does not, and the failed turn's backoff is as it was.
func TestUnackedOutcomes(t *testing.T) {
	e := unackedEnv(t)
	e.next = func(TurnSpec, *fakeHost) TurnResult {
		return TurnResult{Outcome: TurnLimitReached, LimitKind: "budget"}
	}
	e.tick(t)
	if u := readTurnState(e.home).Unacked; u == nil || u.Turns != 1 {
		t.Fatalf("a budget-cut turn: %+v", u)
	}

	e = unackedEnv(t)
	e.next = func(TurnSpec, *fakeHost) TurnResult {
		return TurnResult{Outcome: TurnStopped, Error: "the turn's time limit (30m0s) passed"}
	}
	e.tick(t)
	if u := readTurnState(e.home).Unacked; u == nil || u.Turns != 1 {
		t.Fatalf("a turn stopped by its time limit: %+v", u)
	}

	e = unackedEnv(t)
	e.tickStopping(t)
	e.tickStopping(t)
	if u := readTurnState(e.home).Unacked; u != nil || len(e.specs) != 2 {
		t.Fatalf("turns stopped by the loop: %+v, %d turns", u, len(e.specs))
	}

	e = unackedEnv(t)
	e.next = func(TurnSpec, *fakeHost) TurnResult { return TurnResult{Outcome: TurnFailed, Error: "exit 1"} }
	e.tick(t)
	st := readTurnState(e.home)
	if st.Unacked != nil || st.Backoff != time.Minute || !st.HoldUntil.Equal(e.clock.Add(time.Minute)) {
		t.Fatalf("a failed turn: %+v, backoff %s until %s", st.Unacked, st.Backoff, st.HoldUntil)
	}
}

// A worker in an attempt in work, with a message it leaves unacknowledged,
// is neither slowed nor held: its turns keep the continuation's cadence and
// the stall rules.
func TestUnackedSparesAttemptInWork(t *testing.T) {
	e := newLoopEnv(t)
	e.host.attempt = &memberAttempt{ID: "att-1", State: "running", Phase: "working", Task: TaskRef{TaskID: "task-1"}}
	e.host.pending = []PendingMessage{msg("m1", "message")}
	e.next = func(TurnSpec, *fakeHost) TurnResult {
		e.head += "a" // a new commit each turn: no stall
		return TurnResult{Outcome: TurnFinished}
	}
	for range 4 {
		e.tick(t)
	}
	if len(e.specs) != 4 {
		t.Fatalf("%d turns in four ticks", len(e.specs))
	}
	st := readTurnState(e.home)
	if st.Unacked != nil || st.HoldReport != nil {
		t.Fatalf("an attempt in work was counted: %+v, hold %+v", st.Unacked, st.HoldReport)
	}
	if strings.Contains(e.specs[3].Prompt, "This is turn") {
		t.Fatal("the brief of an attempt's turn names a count")
	}

	// A count left from before the attempt does not slow its turns.
	st = readTurnState(e.home)
	st.Unacked = &unackedState{Messages: []string{"m1"}, Turns: 2, Since: e.clock, LastEnded: e.clock}
	if err := writeTurnState(e.home, st); err != nil {
		t.Fatal(err)
	}
	e.tick(t)
	if len(e.specs) != 5 {
		t.Fatal("an earlier count slowed an attempt in work")
	}
}

// An attempt the stall rules stalled no longer spares its member: a
// message its turns leave unacknowledged is counted and waits.
func TestUnackedCountsStalledAttempt(t *testing.T) {
	e := unackedEnv(t)
	e.host.role = "worker"
	e.host.attempt = &memberAttempt{ID: "att-1", State: "running", Phase: "working", Task: TaskRef{TaskID: "task-1"}}
	st := readTurnState(e.home)
	st.attempt("att-1").Stalled = true
	if err := writeTurnState(e.home, st); err != nil {
		t.Fatal(err)
	}
	e.tick(t)
	if u := readTurnState(e.home).Unacked; len(e.specs) != 1 || u == nil || u.Turns != 1 {
		t.Fatalf("a stalled attempt's member: %d turns, count %+v", len(e.specs), u)
	}
	// The message's turn ended the stall, as a message does: the turns
	// that follow are the attempt's continuations, bounded by the stall
	// rules again.
}

// A held member's turn stopped by the loop is not counted, but the next one
// still waits: the waits run from the latest turn on the messages.
func TestUnackedStoppedTurnWhileHeld(t *testing.T) {
	e := unackedEnv(t)
	for range 3 {
		if u := readTurnState(e.home).Unacked; u != nil {
			e.clock = u.LastEnded.Add(unackedCeiling * defaultUnackedWait)
		}
		e.tick(t)
	}
	e.clock = readTurnState(e.home).Unacked.LastEnded.Add(unackedRecheck - turnGap - time.Second)
	e.tickStopping(t)
	if u := readTurnState(e.home).Unacked; len(e.specs) != 4 || u.Turns != 3 || !u.LastEnded.Equal(e.clock) {
		t.Fatalf("the re-check's stopped turn: %d turns, count %+v", len(e.specs), u)
	}
	e.host.pending = append(e.host.pending, msg("m2", "message"))
	e.tick(t)
	if len(e.specs) != 4 {
		t.Fatal("a turn at once after a held member's stopped turn")
	}
}

// The count survives a restart of the launcher: it is in the turn state.
func TestUnackedSurvivesRestart(t *testing.T) {
	e := unackedEnv(t)
	e.tick(t)
	e.loop = &TurnLoop{Home: e.home, Client: "claude", Host: e.host, Log: e.loop.Log, Now: e.loop.Now,
		Head: e.loop.Head, Clean: e.loop.Clean, Run: e.loop.Run}
	e.tick(t)
	if len(e.specs) != 1 {
		t.Fatal("a restarted launcher forgot the wait")
	}
}

// The waits come from agent.json's turns.unacked_wait, from 10 seconds to
// an hour; out of range is the default.
func TestUnackedWaitSetting(t *testing.T) {
	for in, want := range map[string]time.Duration{"10s": 10 * time.Second, "1h": time.Hour, "9s": defaultUnackedWait,
		"2h": defaultUnackedWait, "x": defaultUnackedWait, "": defaultUnackedWait} {
		home := t.TempDir()
		writeAgentDoc(t, home, map[string]any{"turns": map[string]any{"unacked_wait": in}})
		if got := ReadTurnLimits(home).UnackedWait; got != want {
			t.Errorf("unacked_wait %q: %s, want %s", in, got, want)
		}
	}
}

// A hold's summary is redacted, on one line with no control character, and
// at most maxHoldSummary bytes on a rune boundary.
func TestHoldSummary(t *testing.T) {
	home := t.TempDir()
	if got := holdSummary(home, "a\x1b[31m\nb\u009b c\x00 ghp_abcdefghijklmnop"); got != "a [31m b c [redacted]" {
		t.Fatalf("%q", got)
	}
	long := holdSummary(home, strings.Repeat("é", maxHoldSummary))
	if len(long) > maxHoldSummary || !strings.HasSuffix(long, "…") {
		t.Fatalf("a long summary: %d bytes", len(long))
	}
}

// heldEnv is unackedEnv after three turns: the member is held on m1.
func heldEnv(t *testing.T) *loopEnv {
	t.Helper()
	e := unackedEnv(t)
	for range 3 {
		if u := readTurnState(e.home).Unacked; u != nil {
			e.clock = u.LastEnded.Add(unackedCeiling * defaultUnackedWait)
		}
		e.tick(t)
	}
	if u := readTurnState(e.home).Unacked; len(e.specs) != 3 || u == nil || u.Turns != 3 {
		t.Fatalf("not held: %d turns, %+v", len(e.specs), u)
	}
	return e
}

// A held member's turn that acknowledges only a later note made no
// progress on the message the count began with: it counts, and the hold
// keeps its start, so the coordinators are not told again.
func TestUnackedLaterAckIsNoProgress(t *testing.T) {
	e := heldEnv(t)
	since := readTurnState(e.home).Unacked.Since
	for i, id := range []string{"n1", "n2", "n3"} {
		e.host.pending = append(e.host.pending, msg(id, "message"))
		e.next = func(_ TurnSpec, h *fakeHost) TurnResult {
			h.ack(id)
			return TurnResult{Outcome: TurnFinished}
		}
		e.clock = readTurnState(e.home).Unacked.LastEnded.Add(unackedCeiling * defaultUnackedWait)
		e.tick(t)
		u := readTurnState(e.home).Unacked
		if len(e.specs) != 4+i || u == nil || u.Turns != 4+i || !u.Since.Equal(since) ||
			strings.Join(u.Stuck, ",") != "m1" {
			t.Fatalf("note %s: %d turns, count %+v", id, len(e.specs), u)
		}
	}
	// No new note: the day's wait.
	e.ticksTo(t, readTurnState(e.home).Unacked.LastEnded.Add(time.Hour))
	if len(e.specs) != 6 {
		t.Fatalf("%d turns after three notes, want 6", len(e.specs))
	}
}

// A pending read that fails after a turn counts the turn as one that
// acknowledged nothing; the next read that succeeds clears the count when
// a message it began with is gone.
func TestUnackedFailedReadAfterTurn(t *testing.T) {
	e := unackedEnv(t)
	e.next = func(_ TurnSpec, h *fakeHost) TurnResult {
		h.ack("m1")
		h.pendingErr = errors.New("aicrewd is restarting")
		return TurnResult{Outcome: TurnFinished}
	}
	e.tick(t)
	if u := readTurnState(e.home).Unacked; u == nil || u.Turns != 1 {
		t.Fatalf("a turn whose read failed: %+v", u)
	}
	e.host.pendingErr = nil
	e.tick(t)
	if u := readTurnState(e.home).Unacked; u != nil {
		t.Fatalf("the next read did not clear the count: %+v", u)
	}

	// The same without the acknowledgement: the count stands and waits.
	e = unackedEnv(t)
	e.next = func(_ TurnSpec, h *fakeHost) TurnResult {
		h.pendingErr = errors.New("aicrewd is restarting")
		return TurnResult{Outcome: TurnFinished}
	}
	e.tick(t)
	e.host.pendingErr = nil
	e.next = func(TurnSpec, *fakeHost) TurnResult { return TurnResult{Outcome: TurnFinished} }
	e.tick(t)
	if u := readTurnState(e.home).Unacked; len(e.specs) != 1 || u == nil || u.Turns != 1 {
		t.Fatalf("after a failed read: %d turns, count %+v", len(e.specs), u)
	}
}

// The messages that had their turn are those it was admitted with: one that
// arrives while the held member's turn runs still gets its own turn, and
// one whose turn failed has not had it.
func TestUnackedArrivalDuringTurn(t *testing.T) {
	e := heldEnv(t)
	e.host.pending = append(e.host.pending, msg("m2", "message"))
	e.clock = readTurnState(e.home).Unacked.LastEnded.Add(unackedCeiling * defaultUnackedWait)
	e.next = func(_ TurnSpec, h *fakeHost) TurnResult {
		h.pending = append(h.pending, msg("m3", "message"))
		return TurnResult{Outcome: TurnFinished}
	}
	e.tick(t)
	if len(e.specs) != 4 {
		t.Fatalf("%d turns for m2", len(e.specs))
	}
	e.next = func(TurnSpec, *fakeHost) TurnResult { return TurnResult{Outcome: TurnFinished} }
	e.ticksTo(t, readTurnState(e.home).Unacked.LastEnded.Add(unackedCeiling*defaultUnackedWait))
	e.tick(t)
	if len(e.specs) != 5 {
		t.Fatalf("%d turns: m3, which arrived during m2's turn, had none", len(e.specs))
	}

	e = heldEnv(t)
	e.host.pending = append(e.host.pending, msg("m2", "message"))
	e.clock = readTurnState(e.home).Unacked.LastEnded.Add(unackedCeiling * defaultUnackedWait)
	e.next = func(TurnSpec, *fakeHost) TurnResult { return TurnResult{Outcome: TurnFailed, Error: "exit 1"} }
	e.tick(t)
	e.next = func(TurnSpec, *fakeHost) TurnResult { return TurnResult{Outcome: TurnFinished} }
	e.ticksTo(t, readTurnState(e.home).Unacked.LastEnded.Add(unackedCeiling*defaultUnackedWait))
	e.tick(t)
	if len(e.specs) != 5 {
		t.Fatalf("%d turns: m2's failed turn used up its turn", len(e.specs))
	}
}

// A pause during a failed turn's backoff clears the count; the backoff
// stands.
func TestUnackedPauseDuringBackoff(t *testing.T) {
	e := heldEnv(t)
	e.host.pending = append(e.host.pending, msg("m2", "message"))
	e.clock = readTurnState(e.home).Unacked.LastEnded.Add(unackedCeiling * defaultUnackedWait)
	e.next = func(TurnSpec, *fakeHost) TurnResult { return TurnResult{Outcome: TurnFailed, Error: "exit 1"} }
	e.tick(t)
	until := readTurnState(e.home).HoldUntil
	if !e.clock.Before(until) {
		t.Fatal("no backoff after the failed turn")
	}
	e.host.paused = &PauseNote{Reason: "the operator"}
	e.tick(t)
	e.host.paused = nil
	st := readTurnState(e.home)
	if st.Unacked != nil || !st.HoldUntil.Equal(until) || !lastHold(e).same(holdReport{}) {
		t.Fatalf("a pause during the backoff: %+v, until %s, hold %+v", st.Unacked, st.HoldUntil, lastHold(e))
	}
}

// A brief whose inbox read failed still names the turn and the earlier
// outcomes.
func TestUnackedBriefWithoutInbox(t *testing.T) {
	e := unackedEnv(t)
	e.host.inboxErr = errors.New("the inbox is unavailable")
	e.tick(t)
	e.clock = readTurnState(e.home).Unacked.LastEnded.Add(defaultUnackedWait)
	e.tick(t)
	if len(e.specs) != 2 || !strings.Contains(e.specs[1].Prompt, "This is turn 2 on message(s) m1") ||
		!strings.Contains(e.specs[1].Prompt, "The delivery check was denied.") {
		t.Fatalf("the fallback brief: %d turns\n%s", len(e.specs), e.specs[len(e.specs)-1].Prompt)
	}
}

// The hold names at most aicrewd's bound of turns.
func TestUnackedHoldTurnsBound(t *testing.T) {
	st := turnState{Unacked: &unackedState{Messages: []string{"m1"}, Stuck: []string{"m1"}, Turns: maxHoldTurns + 5}}
	if h := unackedHold(st); h == nil || h.Turns != maxHoldTurns {
		t.Fatalf("%+v", h)
	}
}

// A turn stopped by its time limit counts even when the loop begins to stop
// while the turn is recorded: the cause is the runner's, not the context's.
func TestUnackedTimeoutThenShutdown(t *testing.T) {
	e := unackedEnv(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	e.next = func(_ TurnSpec, h *fakeHost) TurnResult {
		cancel()
		h.pendingErr = errors.New("the loop is stopping")
		return TurnResult{Outcome: TurnStopped, Error: "the turn's time limit (30m0s) passed"}
	}
	e.clock = e.clock.Add(turnGap + time.Second)
	e.loop.Tick(ctx)
	if u := readTurnState(e.home).Unacked; u == nil || u.Turns != 1 {
		t.Fatalf("a timed-out turn during a shutdown: %+v", u)
	}
}

// During a failed turn's backoff, a read that no longer lists a message the
// count began with clears the count and the hold; the backoff stands.
func TestUnackedGoneDuringBackoff(t *testing.T) {
	e := heldEnv(t)
	e.host.pending = append(e.host.pending, msg("m2", "message"))
	e.clock = readTurnState(e.home).Unacked.LastEnded.Add(unackedCeiling * defaultUnackedWait)
	e.next = func(_ TurnSpec, h *fakeHost) TurnResult {
		h.ack("m1")
		h.pendingErr = errors.New("aicrewd is restarting")
		return TurnResult{Outcome: TurnFailed, Error: "exit 1"}
	}
	e.tick(t)
	st := readTurnState(e.home)
	if st.Unacked == nil || !e.clock.Before(st.HoldUntil) {
		t.Fatalf("after the failed turn: %+v, until %s", st.Unacked, st.HoldUntil)
	}
	until := st.HoldUntil
	e.host.pendingErr = nil
	e.tick(t)
	st = readTurnState(e.home)
	if st.Unacked != nil || !st.HoldUntil.Equal(until) || !lastHold(e).same(holdReport{}) {
		t.Fatalf("a read without m1 during the backoff: %+v, until %s, hold %+v", st.Unacked, st.HoldUntil, lastHold(e))
	}
}

// A later note the held member's turn acknowledged is not in the next
// brief's streak: the brief names the messages the count began with.
func TestUnackedMessagesNamed(t *testing.T) {
	e := heldEnv(t)
	e.host.pending = append(e.host.pending, msg("n1", "message"), msg("n2", "message"))
	e.clock = readTurnState(e.home).Unacked.LastEnded.Add(unackedCeiling * defaultUnackedWait)
	e.next = func(_ TurnSpec, h *fakeHost) TurnResult {
		h.ack("n1")
		return TurnResult{Outcome: TurnFinished}
	}
	e.tick(t)
	if h := lastHold(e); strings.Join(h.Messages, ",") != "m1,n1,n2" || h.Turns != 4 {
		t.Fatalf("the hold after turn 4: %+v", h)
	}
	e.host.pending = append(e.host.pending, msg("n3", "message"))
	e.clock = readTurnState(e.home).Unacked.LastEnded.Add(unackedCeiling * defaultUnackedWait)
	e.tick(t)
	p := e.specs[len(e.specs)-1].Prompt
	if len(e.specs) != 5 || !strings.Contains(p, "This is turn 5 on message(s) m1:") {
		t.Fatalf("the fifth brief: %d turns\n%s", len(e.specs), p)
	}
}

// A held member's turn on a new note that hits the provider's limit did
// not serve the note: its next turn is due after the ceiling, not a day on.
func TestUnackedLimitLeavesNoteNew(t *testing.T) {
	e := heldEnv(t)
	e.host.pending = append(e.host.pending, msg("n1", "message"))
	e.clock = readTurnState(e.home).Unacked.LastEnded.Add(unackedCeiling * defaultUnackedWait)
	e.next = func(TurnSpec, *fakeHost) TurnResult {
		return TurnResult{Outcome: TurnLimitReached, LimitKind: "usage", ResetAt: e.clock.Add(time.Minute)}
	}
	e.tick(t)
	if len(e.specs) != 4 {
		t.Fatalf("%d turns for the note", len(e.specs))
	}
	e.next = func(TurnSpec, *fakeHost) TurnResult { return TurnResult{Outcome: TurnFinished} }
	e.ticksTo(t, readTurnState(e.home).Unacked.LastEnded.Add(unackedCeiling*defaultUnackedWait+time.Minute))
	e.tick(t)
	if len(e.specs) != 5 {
		t.Fatalf("%d turns: the limited turn served the note", len(e.specs))
	}
}
