package store

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// An unacknowledged hold (task 01a1fa19) is one hold from its count's start
// to its clear: the first report tells the team's coordinators, a set that
// grew and a further turn refresh the row and tell no one, and the team
// state shows the messages, the turns, the last summary and the count's
// start. A new count after a clear is a new hold. A coordinator's own hold
// tells no one, so it cannot wake the held member.
func TestUnacknowledgedHold(t *testing.T) {
	ctx := context.Background()
	e := newClaimStopEnv(t)
	e.runningClaim(t, "task-1")
	told := func(agent string) int {
		t.Helper()
		var n int
		if err := e.s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM message_recipients r JOIN messages m
			ON m.id = r.message_id WHERE r.agent_id = ? AND m.kind = 'lifecycle' AND m.text LIKE '%is held%'`,
			agent).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
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
	report := func(tok string, r HoldReport) HoldResult {
		t.Helper()
		got, err := e.s.ReportHoldWithToken(ctx, tok, r)
		if err != nil {
			t.Fatalf("report %+v: %v", r, err)
		}
		return got
	}

	start := e.now.Add(-25 * time.Minute)
	first := HoldReport{Kind: HoldUnacknowledged, Messages: []string{"m1"}, Turns: 3,
		Summary: "The delivery check was denied; I cannot finalize.", StuckSince: start, NextCheck: e.now.Add(24 * time.Hour)}
	if got := report(e.indepTk, first); !got.Announced || told(e.lead) != 1 {
		t.Fatalf("the first report: %+v, told %d", got, told(e.lead))
	}
	var text string
	if err := e.s.db.QueryRowContext(ctx, `SELECT text FROM messages WHERE text LIKE '%is held%'`).Scan(&text); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"3 turns in a row", "m1", first.Summary, "pauses and then resumes"} {
		if !strings.Contains(text, want) {
			t.Fatalf("the coordinators' message %q does not say %q", text, want)
		}
	}
	h := hold(e.indep)
	if h == nil || h.Kind != HoldUnacknowledged || h.Turns != 3 || h.Summary != first.Summary ||
		strings.Join(h.Messages, ",") != "m1" || !h.StuckSince.Equal(start) {
		t.Fatalf("the team state's hold: %+v", h)
	}
	since := h.Since

	*e.now = e.now.Add(20 * time.Minute)
	grown := first
	grown.Messages, grown.Turns, grown.Summary = []string{"m1", "m2"}, 4, "Still denied."
	grown.NextCheck = e.now.Add(24 * time.Hour)
	if got := report(e.indepTk, grown); got.Announced || told(e.lead) != 1 {
		t.Fatalf("a set that grew was told again: %+v, told %d", got, told(e.lead))
	}
	if h := hold(e.indep); h == nil || h.Turns != 4 || len(h.Messages) != 2 || h.Summary != "Still denied." ||
		!h.Since.Equal(since) {
		t.Fatalf("the refreshed hold: %+v (since %v)", h, since)
	}

	// A clear, an acknowledgement or a pause, and a new count later: a new
	// hold, told again.
	report(e.indepTk, HoldReport{})
	if hold(e.indep) != nil {
		t.Fatal("the cleared hold still shows")
	}
	again := first
	again.StuckSince = e.now.Add(time.Minute)
	if got := report(e.indepTk, again); !got.Announced || told(e.lead) != 2 {
		t.Fatalf("a new count: %+v, told %d", got, told(e.lead))
	}

	// The coordinator's own: kept, shown, told to no one.
	if got := report(e.leadTok, first); !got.Announced || told(e.lead) != 2 || hold(e.lead) == nil {
		t.Fatalf("the coordinator's own hold: %+v, told %d", got, told(e.lead))
	}
	if n := told(e.indep); n != 0 {
		t.Fatalf("the held member was told %d times", n)
	}

	eleven := make([]string, MaxHoldMessages+1)
	for i := range eleven {
		eleven[i] = "m" + string(rune('a'+i))
	}
	bad := func(edit func(*HoldReport)) HoldReport {
		r := first
		r.Messages = append([]string(nil), first.Messages...)
		edit(&r)
		return r
	}
	for name, r := range map[string]HoldReport{
		"no message":          bad(func(r *HoldReport) { r.Messages = nil }),
		"too many messages":   bad(func(r *HoldReport) { r.Messages = eleven }),
		"a message twice":     bad(func(r *HoldReport) { r.Messages = []string{"m1", "m1"} }),
		"an empty message id": bad(func(r *HoldReport) { r.Messages = []string{""} }),
		"no turn":             bad(func(r *HoldReport) { r.Turns = 0 }),
		"too many turns":      bad(func(r *HoldReport) { r.Turns = MaxHoldTurns + 1 }),
		"an escape":           bad(func(r *HoldReport) { r.Summary = "ok \x1b[2Jcleared" }),
		"a newline":           bad(func(r *HoldReport) { r.Summary = "one\ntwo" }),
		"a C1 control":        bad(func(r *HoldReport) { r.Summary = "one\u009btwo" }),
		"a long summary":      bad(func(r *HoldReport) { r.Summary = strings.Repeat("x", MaxHoldSummary+1) }),
		"not UTF-8":           bad(func(r *HoldReport) { r.Summary = "\xff" }),
		"no count start":      bad(func(r *HoldReport) { r.StuckSince = time.Time{} }),
		"an attempt":          bad(func(r *HoldReport) { r.AttemptID = "a1" }),
		"no next check":       bad(func(r *HoldReport) { r.NextCheck = time.Time{} }),
		"a budget with turns": {Kind: HoldDailyBudget, SpentUSD: 1, LimitUSD: 1, NextCheck: first.NextCheck, Turns: 3},
		"a login with messages": {Kind: HoldNeedsLogin, NextCheck: first.NextCheck, Command: "claude",
			Messages: []string{"m1"}},
		"a clear with a message": {Messages: []string{"m1"}},
	} {
		if _, err := e.s.ReportHoldWithToken(ctx, e.indepTk, r); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: %v", name, err)
		}
	}
	if _, err := e.s.ReportHoldWithToken(ctx, e.indepTk, bad(func(r *HoldReport) {
		r.Summary = strings.Repeat("é", MaxHoldSummary/2)
	})); err != nil {
		t.Errorf("a summary of exactly %d bytes: %v", MaxHoldSummary, err)
	}
}

// An unacknowledged hold's identity is its count's start, whatever its
// messages and turns.
func TestUnacknowledgedHoldIdentity(t *testing.T) {
	start := time.Date(2026, 11, 20, 17, 42, 0, 0, time.UTC)
	a := HoldReport{Kind: HoldUnacknowledged, Messages: []string{"m1"}, Turns: 3, StuckSince: start}
	b := HoldReport{Kind: HoldUnacknowledged, Messages: []string{"m1", "m2"}, Turns: 9, StuckSince: start}
	if holdIdentity(a, start) != holdIdentity(b, start.Add(48*time.Hour)) {
		t.Fatal("a grown set across days is another hold")
	}
	b.StuckSince = start.Add(time.Second)
	if holdIdentity(a, start) == holdIdentity(b, start) {
		t.Fatal("a new count is the same hold")
	}
}
