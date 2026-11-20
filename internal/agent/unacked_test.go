package agent

import (
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

// An acknowledgement clears the count: by the member outside a turn (the
// pending read no longer lists the message), or in a turn. The next turn
// then has no wait and no "turn N" in its brief.
func TestUnackedClearedByAck(t *testing.T) {
	e := unackedEnv(t)
	e.tick(t)
	e.clock = readTurnState(e.home).Unacked.LastEnded.Add(defaultUnackedWait)
	e.tick(t)
	if n := readTurnState(e.home).Unacked.Turns; n != 2 {
		t.Fatalf("%d turns counted", n)
	}
	e.host.pending = []PendingMessage{msg("m2", "message")}
	e.tick(t)
	if len(e.specs) != 3 || strings.Contains(e.specs[2].Prompt, "This is turn") {
		t.Fatalf("after an acknowledgement: %d turns", len(e.specs))
	}
	if u := readTurnState(e.home).Unacked; u == nil || u.Turns != 1 || strings.Join(u.Messages, ",") != "m2" {
		t.Fatalf("a new count for the new message: %+v", u)
	}

	// A turn that acknowledges its message leaves no count.
	e.next = func(_ TurnSpec, h *fakeHost) TurnResult {
		h.pending = nil
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

// A turn cut by its own budget that leaves the set unchanged counts; a
// stopped or failed one does not, and the failed turn's backoff is as it
// was.
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
	e.next = func(TurnSpec, *fakeHost) TurnResult { return TurnResult{Outcome: TurnStopped} }
	e.tick(t)
	e.tick(t)
	if u := readTurnState(e.home).Unacked; u != nil || len(e.specs) != 2 {
		t.Fatalf("stopped turns: %+v, %d turns", u, len(e.specs))
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

// A held member's turn that is stopped is not counted, but the next one
// still waits: the waits run from the latest turn on the messages.
func TestUnackedStoppedTurnWhileHeld(t *testing.T) {
	e := unackedEnv(t)
	for range 3 {
		if u := readTurnState(e.home).Unacked; u != nil {
			e.clock = u.LastEnded.Add(unackedCeiling * defaultUnackedWait)
		}
		e.tick(t)
	}
	e.next = func(TurnSpec, *fakeHost) TurnResult { return TurnResult{Outcome: TurnStopped} }
	e.clock = readTurnState(e.home).Unacked.LastEnded.Add(unackedRecheck)
	e.tick(t)
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
