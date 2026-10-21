package agent

import (
	"errors"
	"fmt"
	"testing"
)

// newCostEnv is a worker's loop with an open attempt and a message due.
func newCostEnv(t *testing.T) *loopEnv {
	t.Helper()
	e := newLoopEnv(t)
	e.host.attempt = &memberAttempt{ID: "att-1", State: "running", Phase: "working", Task: TaskRef{TaskID: "task-1"}}
	e.host.pending = []PendingMessage{msg("m1", "message")}
	e.next = func(_ TurnSpec, h *fakeHost) TurnResult {
		h.pending = nil
		return TurnResult{Outcome: TurnFinished, CostUSD: 0.5, CostSource: CostPriceList}
	}
	return e
}

// After each turn the launcher reports the turn's cost to aicrewd, named
// as the turn's record names it.
func TestTurnCostReported(t *testing.T) {
	e := newCostEnv(t)
	e.tick(t)
	recs := e.records(t)
	want := []turnReport{{ID: recs[0].ID, AttemptID: "att-1", CostUSD: 0.5, CostSource: CostPriceList}}
	if len(recs) != 1 || fmt.Sprint(e.host.reports) != fmt.Sprint(want) {
		t.Fatalf("the reports: %+v, want %+v", e.host.reports, want)
	}
	if st := readTurnState(e.home); len(st.Unreported) != 0 {
		t.Fatalf("a reported turn is still kept: %+v", st.Unreported)
	}
}

// A report that does not reach aicrewd is kept and sent before the next
// turn, in order; one aicrewd refuses for good is dropped.
func TestTurnCostResent(t *testing.T) {
	e := newCostEnv(t)
	e.host.reportErr = &TransportError{Err: errors.New("aicrewd is not reached")}
	e.tick(t)
	if st := readTurnState(e.home); len(st.Unreported) != 1 || len(e.host.reports) != 0 {
		t.Fatalf("after a lost report: %+v, %+v", st.Unreported, e.host.reports)
	}
	e.host.reportErr = nil
	e.host.pending = []PendingMessage{msg("m2", "message")}
	e.next = func(_ TurnSpec, h *fakeHost) TurnResult {
		h.pending = nil
		if len(h.reports) != 1 {
			t.Errorf("the turn started before the backlog was sent: %+v", h.reports)
		}
		return TurnResult{Outcome: TurnFinished, CostUSD: 0.25, CostSource: CostPriceList}
	}
	e.tick(t)
	recs := e.records(t)
	if len(recs) != 2 || len(e.host.reports) != 2 || e.host.reports[0].ID != recs[0].ID || e.host.reports[1].ID != recs[1].ID {
		t.Fatalf("the resend: %+v", e.host.reports)
	}

	// A session's refusal keeps the report; a malformed report's drops it.
	e.next = newCostEnv(t).next
	e.host.reportErr = &Refusal{Status: 401, Code: "invalid_token"}
	e.host.pending = []PendingMessage{msg("m3", "message")}
	e.tick(t)
	if st := readTurnState(e.home); len(st.Unreported) != 1 {
		t.Fatalf("a report refused for the session is not kept: %+v", st.Unreported)
	}
	e.host.reportErr = &Refusal{Status: 400, Code: "invalid_request"}
	e.host.pending = []PendingMessage{msg("m4", "message")}
	e.tick(t)
	if st := readTurnState(e.home); len(st.Unreported) != 0 {
		t.Fatalf("a refused report is kept: %+v", st.Unreported)
	}
}

// The backlog keeps the last maxUnreported turns; older ones are dropped.
func TestTurnCostBacklogBound(t *testing.T) {
	e := newCostEnv(t)
	st := readTurnState(e.home)
	st.Days[dayOf(e.clock)] = 1
	st.attempt("att-1")
	for i := range maxUnreported {
		st.Unreported = append(st.Unreported, turnReport{ID: fmt.Sprintf("old-%d", i), CostUSD: 0.1, CostSource: CostPriceList})
	}
	if err := writeTurnState(e.home, st); err != nil {
		t.Fatal(err)
	}
	e.host.reportErr = &TransportError{Err: errors.New("aicrewd is not reached")}
	e.tick(t)
	got := readTurnState(e.home).Unreported
	if len(got) != maxUnreported || got[0].ID != "old-1" || got[len(got)-1].ID != e.records(t)[0].ID {
		t.Fatalf("the backlog: %d, first %+v", len(got), got[0])
	}
}

// A home whose state has no figure for the day or the attempt (a new home
// of the member) takes aicrewd's totals before the admission: it refuses a
// turn on a budget aicrewd counts as spent, and admits one under it. A home
// with figures of its own does not ask.
func TestTurnCostNewHomeTakesTotals(t *testing.T) {
	limits := ReadTurnLimits(t.TempDir())
	for _, c := range []struct {
		name   string
		totals func(day string) turnTotals
		turns  int
		hold   string
	}{
		{"the day spent", func(day string) turnTotals { return turnTotals{Day: day, DayUSD: limits.DailyBudgetUSD} }, 0,
			"the daily budget is spent"},
		{"the attempt spent", func(day string) turnTotals {
			return turnTotals{Day: day, DayUSD: 1, AttemptUSD: limits.AttemptBudgetUSD}
		}, 0, "attempt att-1's budget is spent"},
		{"under both", func(day string) turnTotals { return turnTotals{Day: day, DayUSD: 1, AttemptUSD: 2} }, 1, ""},
		{"another day on the hub", func(string) turnTotals { return turnTotals{Day: "2000-01-01", DayUSD: 999} }, 1, ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			e := newCostEnv(t)
			e.host.totals = c.totals(dayOf(e.clock.Add(turnGap + 1e9)))
			e.tick(t)
			st := readTurnState(e.home)
			if len(e.specs) != c.turns || st.Hold != c.hold || e.host.totalsReads != 1 {
				t.Fatalf("%d turns, hold %q, %d reads", len(e.specs), st.Hold, e.host.totalsReads)
			}
			if c.turns == 1 && c.name == "under both" {
				if d := st.Days[dayOf(e.clock)]; d != 1.5 || st.Attempts["att-1"].CostUSD != 2.5 {
					t.Fatalf("the cached totals after the turn: day %v, attempt %v", d, st.Attempts["att-1"].CostUSD)
				}
			}
			if c.turns == 1 {
				e.host.pending = []PendingMessage{msg("m2", "message")}
				e.tick(t)
				if len(e.specs) != 2 || e.host.totalsReads != 1 {
					t.Fatalf("the home asked again with figures of its own: %d turns, %d reads", len(e.specs), e.host.totalsReads)
				}
			}
		})
	}
}

// A new home that cannot read aicrewd's totals has no turn yet.
func TestTurnCostTotalsUnread(t *testing.T) {
	e := newCostEnv(t)
	e.host.totalsErr = &TransportError{Err: errors.New("aicrewd is not reached")}
	e.tick(t)
	if st := readTurnState(e.home); len(e.specs) != 0 || st.Hold != "the budgets' totals were not read from aicrewd" {
		t.Fatalf("%d turns, hold %q", len(e.specs), st.Hold)
	}
}

// Against an aicrewd older than the turns' routes the home's own figures
// stand: the turn runs, and its report is kept for an upgraded aicrewd.
func TestTurnCostOlderAicrewd(t *testing.T) {
	e := newCostEnv(t)
	e.host.totalsErr = &Refusal{Status: 404, Code: "not_found"}
	e.host.reportErr = &Refusal{Status: 404, Code: "not_found"}
	e.tick(t)
	st := readTurnState(e.home)
	if len(e.specs) != 1 || st.Hold != "" || len(st.Unreported) != 1 {
		t.Fatalf("%d turns, hold %q, %d kept", len(e.specs), st.Hold, len(st.Unreported))
	}
}
