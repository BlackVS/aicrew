package store

import (
	"context"
	"errors"
	"math"
	"testing"
)

// A turn's report raises its attempt's total and its member's total for
// aicrewd's day; a replay changes nothing; the day is the hub's date at the
// first receipt and the member's own, whichever home reports it.
func TestTurnReports(t *testing.T) {
	ctx := context.Background()
	e := newClaimStopEnv(t)
	a, _ := e.runningClaim(t, "task-1")
	today := e.now.UTC().Format("2006-01-02")

	totals := func(tok, attempt string) TurnTotals {
		t.Helper()
		got, err := e.s.TurnTotalsWithToken(ctx, tok, attempt)
		if err != nil {
			t.Fatalf("the totals: %v", err)
		}
		return got
	}
	if got := totals(e.indepTk, a.ID); got != (TurnTotals{Day: today, AttemptID: a.ID}) {
		t.Fatalf("before any report: %+v", got)
	}
	first := TurnReport{ID: "turn-1", AttemptID: a.ID, CostUSD: 1.25, CostSource: "price_list"}
	if got, err := e.s.ReportTurnWithToken(ctx, e.indepTk, first); err != nil || got != first {
		t.Fatalf("the report: %+v, %v", got, err)
	}
	if _, err := e.s.ReportTurnWithToken(ctx, e.indepTk, TurnReport{ID: "turn-2", CostUSD: 0.5, CostSource: "client_total"}); err != nil {
		t.Fatalf("a turn outside an attempt: %v", err)
	}
	if got := totals(e.indepTk, a.ID); got.DayUSD != 1.75 || got.AttemptUSD != 1.25 {
		t.Fatalf("after two reports: %+v", got)
	}

	// A replay, even with another cost, answers the turn as recorded and
	// counts nothing; another member's replay of the id is refused.
	if got, err := e.s.ReportTurnWithToken(ctx, e.indepTk, TurnReport{ID: "turn-1", AttemptID: a.ID, CostUSD: 9,
		CostSource: "price_list"}); err != nil || got != first {
		t.Fatalf("the replay: %+v, %v", got, err)
	}
	if _, err := e.s.ReportTurnWithToken(ctx, e.workerTok, TurnReport{ID: "turn-1", CostUSD: 1, CostSource: "price_list"}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("another member's report of the turn: %v", err)
	}
	if got := totals(e.indepTk, a.ID); got.DayUSD != 1.75 || got.AttemptUSD != 1.25 {
		t.Fatalf("after the replays: %+v", got)
	}

	// A turn counts on the hub's date when its report first arrived: a turn
	// recorded yesterday is in the attempt's total and not in today's.
	// Another member's day is its own.
	if _, err := e.s.ReportTurnWithToken(ctx, e.indepTk, TurnReport{ID: "turn-3", AttemptID: a.ID, CostUSD: 2,
		CostSource: "price_list"}); err != nil {
		t.Fatal(err)
	}
	var day string
	if err := e.s.db.QueryRow(`SELECT day FROM turn_reports WHERE id = 'turn-3'`).Scan(&day); err != nil || day != today {
		t.Fatalf("the turn's day: %q, %v", day, err)
	}
	yesterday := e.now.UTC().AddDate(0, 0, -1).Format("2006-01-02")
	if _, err := e.s.db.Exec(`UPDATE turn_reports SET day = ? WHERE id = 'turn-3'`, yesterday); err != nil {
		t.Fatal(err)
	}
	if got := totals(e.indepTk, a.ID); got.Day != today || got.DayUSD != 1.75 || got.AttemptUSD != 3.25 {
		t.Fatalf("with a turn of yesterday: %+v", got)
	}
	if got := totals(e.workerTok, ""); got.DayUSD != 0 || got.AttemptID != "" {
		t.Fatalf("another member's day: %+v", got)
	}

	// The attempt is its worker's alone, to report on and to read.
	if _, err := e.s.ReportTurnWithToken(ctx, e.workerTok, TurnReport{ID: "turn-4", AttemptID: a.ID, CostUSD: 1,
		CostSource: "price_list"}); !errors.Is(err, ErrNoAttempt) {
		t.Fatalf("a report on another member's attempt: %v", err)
	}
	if _, err := e.s.TurnTotalsWithToken(ctx, e.leadTok, a.ID); !errors.Is(err, ErrNoAttempt) {
		t.Fatalf("a read of another member's attempt: %v", err)
	}
	if _, err := e.s.TurnTotalsWithToken(ctx, "not-a-token", ""); err == nil {
		t.Fatal("a read without a valid token was answered")
	}

	// A closed attempt still takes its worker's report of a late turn.
	if _, err := e.s.db.Exec(`UPDATE attempts SET state = 'closed' WHERE id = ?`, a.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := e.s.ReportTurnWithToken(ctx, e.indepTk, TurnReport{ID: "turn-5", AttemptID: a.ID, CostUSD: 0.25,
		CostSource: "price_list"}); err != nil {
		t.Fatalf("a report on the closed attempt: %v", err)
	}

	for name, r := range map[string]TurnReport{
		"no id":           {CostSource: "price_list"},
		"a negative cost": {ID: "x", CostUSD: -1, CostSource: "price_list"},
		"a NaN cost":      {ID: "x", CostUSD: math.NaN(), CostSource: "price_list"},
		"an infinite one": {ID: "x", CostUSD: math.Inf(1), CostSource: "price_list"},
		"a huge cost":     {ID: "x", CostUSD: MaxTurnCostUSD + 1, CostSource: "price_list"},
		"no source":       {ID: "x", CostUSD: 1},
		"a bad source":    {ID: "x", CostUSD: 1, CostSource: "Price List"},
	} {
		if _, err := e.s.ReportTurnWithToken(ctx, e.indepTk, r); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: %v", name, err)
		}
	}
}
