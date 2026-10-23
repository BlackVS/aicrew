package agent

import (
	"bytes"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"
)

// Each hold the launcher gives its member is reported to aicrewd once, with
// the amount, the limit and the next check, and the first admitted turn
// clears it: a spent daily budget, a spent attempt budget, the provider's
// limit with or without its reset time, and a missing login with the
// command that logs in. A failed turn's backoff is not reported.
func TestTurnLoopReportsHolds(t *testing.T) {
	e := newLoopEnv(t)
	writeAgentDoc(t, e.home, map[string]any{"turns": map[string]any{"daily_budget_usd": 1, "attempt_budget_usd": 100}})
	e.host.pending = []PendingMessage{msg("m1", "message")}
	e.next = func(TurnSpec, *fakeHost) TurnResult { return TurnResult{Outcome: TurnFinished, CostUSD: 1.2} }
	e.tick(t)
	for range 3 {
		e.tick(t)
	}
	// The day's budget, once, however often the loop finds it spent.
	if len(e.specs) != 1 || len(e.host.holds) != 1 {
		t.Fatalf("%d turns, holds %+v", len(e.specs), e.host.holds)
	}
	day := e.host.holds[0]
	if day.Kind != holdDailyBudget || day.SpentUSD != 1.2 || day.LimitUSD != 1 ||
		!day.NextCheck.Equal(nextDay(e.clock)) || day.AttemptID != "" {
		t.Fatalf("the holds: %+v", e.host.holds)
	}
	e.clock = nextDay(e.clock)
	e.host.holds = nil
	e.next = func(TurnSpec, *fakeHost) TurnResult { return TurnResult{Outcome: TurnFinished} }
	e.tick(t)
	if len(e.specs) != 2 || len(e.host.holds) != 1 || e.host.holds[0] != (holdReport{}) {
		t.Fatalf("the next day's turn: %d turns, holds %+v", len(e.specs), e.host.holds)
	}

	e = newLoopEnv(t)
	writeAgentDoc(t, e.home, map[string]any{"turns": map[string]any{"attempt_budget_usd": 1}})
	e.host.attempt = &memberAttempt{ID: "att-1", State: "running", Phase: "submitted", Task: TaskRef{TaskID: "task-1"}}
	e.host.pending = []PendingMessage{msg("m1", "message")}
	e.next = func(TurnSpec, *fakeHost) TurnResult { return TurnResult{Outcome: TurnFinished, CostUSD: 1.5} }
	e.tick(t)
	e.tick(t)
	if h := e.host.holds; len(h) != 1 || h[0].Kind != holdAttemptBudget || h[0].AttemptID != "att-1" || h[0].SpentUSD != 1.5 ||
		h[0].LimitUSD != 1 || !h[0].NextCheck.Equal(e.clock.Add(time.Hour)) {
		t.Fatalf("an attempt over its budget: %+v", e.host.holds)
	}

	reset := time.Date(2026, 10, 12, 14, 0, 0, 0, time.UTC)
	for _, c := range []struct {
		res  TurnResult
		want holdReport
		// backoff is how long after the turn a hold with no reset time ends.
		backoff time.Duration
	}{
		{TurnResult{Outcome: TurnLimitReached, LimitKind: "provider", ResetAt: reset},
			holdReport{Kind: holdProviderLimit, AttemptID: "att-1", NextCheck: reset, ResetAt: reset}, 0},
		{TurnResult{Outcome: TurnLimitReached, LimitKind: "provider"},
			holdReport{Kind: holdProviderLimit, AttemptID: "att-1"}, time.Hour},
		{TurnResult{Outcome: TurnNeedsLogin},
			holdReport{Kind: holdNeedsLogin, AttemptID: "att-1", Command: loginCommand}, time.Hour},
		{TurnResult{Outcome: TurnFailed}, holdReport{}, 0},
		{TurnResult{Outcome: TurnLimitReached, LimitKind: "budget"}, holdReport{}, 0},
	} {
		e := newLoopEnv(t)
		e.host.attempt = &memberAttempt{ID: "att-1", State: "running", Phase: "working", Task: TaskRef{TaskID: "task-1"}}
		e.host.pending = []PendingMessage{msg("m1", "message")}
		e.next = func(TurnSpec, *fakeHost) TurnResult { return c.res }
		e.tick(t)
		want := c.want
		if c.backoff != 0 {
			want.NextCheck = e.clock.Add(c.backoff)
		}
		got := e.host.holds
		if want == (holdReport{}) {
			if len(got) != 0 {
				t.Errorf("%s/%s: holds %+v", c.res.Outcome, c.res.LimitKind, got)
			}
			continue
		}
		if len(got) != 1 || got[0] != want {
			t.Errorf("%s/%s: holds %+v, want %+v", c.res.Outcome, c.res.LimitKind, got, want)
		}
	}
}

// A hold aicrewd did not take is sent again at the next tick, while the
// member is still held; an aicrewd older than the route answers not_found,
// which the launcher says once and leaves the hold its own; a refused
// report is dropped.
func TestTurnLoopHoldDelivery(t *testing.T) {
	var logs syncBuffer
	e := newLoopEnv(t)
	e.loop.Log = slog.New(slog.NewTextHandler(&logs, nil))
	writeAgentDoc(t, e.home, map[string]any{"turns": map[string]any{"daily_budget_usd": 1}})
	e.host.pending = []PendingMessage{msg("m1", "message")}
	e.next = func(TurnSpec, *fakeHost) TurnResult { return TurnResult{Outcome: TurnFinished, CostUSD: 2} }
	e.tick(t)
	e.host.holdErr = errors.New("connection refused")
	e.tick(t)
	if st := readTurnState(e.home); !st.HoldPending || st.HoldReport == nil || st.HoldReport.Kind != holdDailyBudget {
		t.Fatalf("an undelivered hold: %+v", st)
	}
	e.host.holdErr = nil
	e.tick(t)
	if st := readTurnState(e.home); st.HoldPending || len(e.host.holds) != 1 || e.host.holds[0].Kind != holdDailyBudget {
		t.Fatalf("the hold sent again: %+v, holds %+v", st, e.host.holds)
	}

	// An older aicrewd: said once, and the member stays held all the same.
	e.clock = nextDay(e.clock)
	e.host.holdErr = &Refusal{Status: 404, Code: "not_found"}
	e.tick(t) // the turn, and the clear aicrewd does not take
	e.clock = nextDay(e.clock)
	e.tick(t)
	e.tick(t) // the day's budget spent again
	if n := strings.Count(logs.String(), "aicrewd keeps no holds"); n != 1 || len(e.specs) != 3 {
		t.Fatalf("an older aicrewd: said %d times, %d turns", n, len(e.specs))
	}
	if st := readTurnState(e.home); st.HoldPending || st.HoldUntil.IsZero() {
		t.Fatalf("an older aicrewd's hold: %+v", st)
	}

	e.clock = nextDay(e.clock)
	e.host.holdErr = &Refusal{Status: 400, Code: "invalid_request"}
	e.tick(t)
	e.tick(t)
	if st := readTurnState(e.home); st.HoldPending || !strings.Contains(logs.String(), "it is dropped") {
		t.Fatalf("a refused hold: %+v", st)
	}
}

// A hold read back from the state is the same hold as the one computed
// now for the same instant, whatever its location or monotonic reading.
func TestSetHoldComparesInstants(t *testing.T) {
	now := time.Now()
	st := turnState{HoldReport: &holdReport{Kind: holdDailyBudget, SpentUSD: 2, LimitUSD: 1,
		NextCheck: now.Round(0).In(time.FixedZone("x", 3600))}}
	setHold(&st, &holdReport{Kind: holdDailyBudget, SpentUSD: 2, LimitUSD: 1, NextCheck: now})
	if st.HoldPending {
		t.Fatal("the same hold is sent again")
	}
	setHold(&st, &holdReport{Kind: holdDailyBudget, SpentUSD: 3, LimitUSD: 1, NextCheck: now})
	if !st.HoldPending || st.HoldReport.SpentUSD != 3 {
		t.Fatalf("a changed hold: %+v", st)
	}
	st.HoldPending = false
	setHold(&st, &holdReport{})
	if !st.HoldPending || st.HoldReport != nil {
		t.Fatalf("a clear: %+v", st)
	}
}

// syncBuffer is a log sink the loop and the test share.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}
func (s *syncBuffer) String() string { s.mu.Lock(); defer s.mu.Unlock(); return s.b.String() }
