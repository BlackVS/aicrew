package store

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// Each hold a member's launcher reports tells the team's coordinators once:
// the same hold reported again refreshes it and sends nothing, a new one
// is told, a spent budget is journaled as budget.spent, a cleared hold
// leaves the team state, and a hold raised again after a clear is not told
// twice. A coordinator's own hold tells no one.
func TestMemberHolds(t *testing.T) {
	ctx := context.Background()
	e := newClaimStopEnv(t)
	a, _ := e.runningClaim(t, "task-1")
	next := e.now.Add(time.Hour)
	count := func(query string, args ...any) int {
		t.Helper()
		var n int
		if err := e.s.db.QueryRowContext(ctx, query, args...).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	told := func(agent string) int {
		return count(`SELECT COUNT(*) FROM message_recipients r JOIN messages m ON m.id = r.message_id
			WHERE r.agent_id = ? AND m.kind = 'lifecycle' AND m.text LIKE '%is held%'`, agent)
	}
	spent := func() int { return count(`SELECT COUNT(*) FROM events WHERE kind = 'budget.spent'`) }
	report := func(tok string, r HoldReport) HoldResult {
		t.Helper()
		got, err := e.s.ReportHoldWithToken(ctx, tok, r)
		if err != nil {
			t.Fatalf("report %+v: %v", r, err)
		}
		return got
	}
	hold := func(agent string) *MemberHold {
		t.Helper()
		op, err := OperatorCaller("op")
		if err != nil {
			t.Fatal(err)
		}
		states, err := e.s.TeamStates(ctx, op, e.teamID)
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range states[0].Members {
			if m.AgentID == agent {
				return m.Hold
			}
		}
		t.Fatalf("no member %s", agent)
		return nil
	}

	steps := []struct {
		name      string
		r         HoldReport
		announced bool
		told      int
		spent     int
		text      string
	}{
		{"the attempt's budget", HoldReport{Kind: HoldAttemptBudget, AttemptID: a.ID, SpentUSD: 10.25, LimitUSD: 10, NextCheck: next},
			true, 1, 1, "attempt " + a.ID + "'s budget is spent, 10.25 USD of 10.00 USD"},
		{"the same, refreshed", HoldReport{Kind: HoldAttemptBudget, AttemptID: a.ID, SpentUSD: 10.5, LimitUSD: 10, NextCheck: next.Add(time.Hour)},
			false, 1, 1, ""},
		{"the daily budget", HoldReport{Kind: HoldDailyBudget, SpentUSD: 30, LimitUSD: 25, NextCheck: next},
			true, 2, 2, "the daily budget is spent, 30.00 USD of 25.00 USD"},
		{"the provider's limit with its reset", HoldReport{Kind: HoldProviderLimit, AttemptID: a.ID, NextCheck: next, ResetAt: next},
			true, 3, 2, "the provider's limit is reached until " + next.Format(time.RFC3339)},
		{"the same reset again", HoldReport{Kind: HoldProviderLimit, AttemptID: a.ID, NextCheck: next, ResetAt: next},
			false, 3, 2, ""},
		{"the provider's limit with none", HoldReport{Kind: HoldProviderLimit, AttemptID: a.ID, NextCheck: next},
			true, 4, 2, "with no reset time given"},
		{"a retry hitting it again", HoldReport{Kind: HoldProviderLimit, AttemptID: a.ID, NextCheck: next.Add(2 * time.Hour)},
			false, 4, 2, ""},
		{"a missing login", HoldReport{Kind: HoldNeedsLogin, AttemptID: a.ID, NextCheck: next, Command: "run `claude`, then `/login`"},
			true, 5, 2, "To log in: run `claude`, then `/login`."},
	}
	var since time.Time
	for i, st := range steps {
		got := report(e.indepTk, st.r)
		if got.Announced != st.announced || got.Hold == nil || got.Hold.Kind != st.r.Kind {
			t.Fatalf("%s: %+v", st.name, got)
		}
		if n := told(e.lead); n != st.told {
			t.Fatalf("%s: the coordinator was told %d times, want %d", st.name, n, st.told)
		}
		if n := spent(); n != st.spent {
			t.Fatalf("%s: %d budget.spent events, want %d", st.name, n, st.spent)
		}
		if st.text != "" && count(`SELECT COUNT(*) FROM messages WHERE text LIKE ?`, "%"+st.text+"%") != 1 {
			t.Fatalf("%s: no message says %q", st.name, st.text)
		}
		h := hold(e.indep)
		if h == nil || h.HoldReport.Kind != st.r.Kind || h.SpentUSD != st.r.SpentUSD || !h.NextCheck.Equal(st.r.NextCheck) {
			t.Fatalf("%s: the team state's hold %+v", st.name, h)
		}
		if i == 0 {
			since = h.Since
		}
		if i == 1 && !h.Since.Equal(since) {
			t.Fatalf("a refresh moved the hold's start: %v, was %v", h.Since, since)
		}
		*e.now = e.now.Add(time.Minute)
	}
	if n := told(e.indep); n != 0 {
		t.Fatalf("the held member was told %d times", n)
	}
	if n := count(`SELECT COUNT(*) FROM message_recipients r JOIN messages m ON m.id = r.message_id
		WHERE r.agent_id = ? AND m.text LIKE '%is held%'`, e.indep); n != 0 {
		t.Fatalf("the held member got %d hold messages", n)
	}

	// The first admitted turn clears the hold; the login failing again the
	// same day is the hold already told.
	if got := report(e.indepTk, HoldReport{}); got.Hold != nil || got.Announced {
		t.Fatalf("the clear: %+v", got)
	}
	if h := hold(e.indep); h != nil {
		t.Fatalf("a cleared hold shows: %+v", h)
	}
	if got := report(e.indepTk, steps[len(steps)-1].r); got.Announced || told(e.lead) != 5 || hold(e.indep) == nil {
		t.Fatalf("the hold raised again: %+v, told %d", got, told(e.lead))
	}

	// The coordinator's own hold is kept and tells no one.
	if got := report(e.leadTok, HoldReport{Kind: HoldDailyBudget, SpentUSD: 5, LimitUSD: 5, NextCheck: next}); !got.Announced ||
		told(e.lead) != 5 || hold(e.lead) == nil {
		t.Fatalf("the coordinator's hold: %+v", got)
	}

	if _, err := e.s.ReportHoldWithToken(ctx, e.workerTok, steps[0].r); !errors.Is(err, ErrNoAttempt) {
		t.Fatalf("another member's attempt: %v", err)
	}
	for name, r := range map[string]HoldReport{
		"an attempt's budget without it": {Kind: HoldAttemptBudget, SpentUSD: 1, LimitUSD: 1, NextCheck: next},
		"a login without its command":    {Kind: HoldNeedsLogin, NextCheck: next},
		"a limit with amounts":           {Kind: HoldProviderLimit, SpentUSD: 1, NextCheck: next},
		"a budget with a reset":          {Kind: HoldDailyBudget, NextCheck: next, ResetAt: next},
		"a clear with a field":           {AttemptID: a.ID},
		"no next check":                  {Kind: HoldDailyBudget, SpentUSD: 1, LimitUSD: 1},
		"a long command":                 {Kind: HoldNeedsLogin, NextCheck: next, Command: strings.Repeat("x", maxHoldCommand+1)},
	} {
		if _, err := e.s.ReportHoldWithToken(ctx, e.indepTk, r); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: %v", name, err)
		}
	}
}

// A daily budget's hold is another one on another UTC day; a provider's
// limit without a reset time is one hold a day per attempt.
func TestHoldIdentity(t *testing.T) {
	day := time.Date(2026, 10, 23, 23, 59, 0, 0, time.UTC)
	daily := HoldReport{Kind: HoldDailyBudget}
	if holdIdentity(daily, day) == holdIdentity(daily, day.Add(2*time.Minute)) {
		t.Fatal("the daily budget's hold is the same across midnight")
	}
	limit := HoldReport{Kind: HoldProviderLimit, AttemptID: "a1"}
	if holdIdentity(limit, day.Add(-time.Hour)) != holdIdentity(limit, day) ||
		holdIdentity(limit, day) == holdIdentity(HoldReport{Kind: HoldProviderLimit, AttemptID: "a2"}, day) {
		t.Fatal("a provider's limit without a reset time")
	}
	budget := HoldReport{Kind: HoldAttemptBudget, AttemptID: "a1"}
	if holdIdentity(budget, day) != holdIdentity(budget, day.Add(48*time.Hour)) {
		t.Fatal("an attempt's budget is one hold whatever the day")
	}
}
