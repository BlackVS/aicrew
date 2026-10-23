package server

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/BlackVS/aicrew/internal/opapi"
)

// A member reports its turns' costs and reads its totals: a replay counts
// nothing, an attempt is its worker's alone, and a malformed report or
// query is refused.
func TestTurnRoutes(t *testing.T) {
	e := setupCoordination(t)
	id := e.runningOffer(t)
	report := map[string]any{"id": "turn-1", "attempt_id": id, "cost_usd": 1.5, "cost_source": "price_list"}

	for range 2 {
		got := e.call(t, e.worker.token, TurnsPath, "", report)
		if got.status != http.StatusOK || got.body["id"] != "turn-1" || got.body["cost_usd"] != 1.5 {
			t.Fatalf("the report: %d %s", got.status, got.raw)
		}
	}
	got := e.call(t, e.worker.token, TurnsPath, "", map[string]any{"id": "turn-2", "cost_usd": 0.5, "cost_source": "client_total"})
	if got.status != http.StatusOK {
		t.Fatalf("a turn outside an attempt: %d %s", got.status, got.raw)
	}
	got = e.get(t, e.worker.token, TurnTotalsPath+"?attempt_id="+id)
	if got.status != http.StatusOK || got.body["day_usd"] != 2.0 || got.body["attempt_usd"] != 1.5 || got.body["attempt_id"] != id ||
		got.body["day"] == nil {
		t.Fatalf("the totals: %d %s", got.status, got.raw)
	}
	if got = e.get(t, e.worker.token, TurnTotalsPath); got.status != http.StatusOK || got.body["day_usd"] != 2.0 || got.body["attempt_id"] != nil {
		t.Fatalf("the day's total: %d %s", got.status, got.raw)
	}

	refused(t, e.call(t, e.lead.token, TurnsPath, "", map[string]any{"id": "turn-3", "attempt_id": id, "cost_usd": 1,
		"cost_source": "price_list"}), http.StatusNotFound, "no_attempt")
	refused(t, e.get(t, e.indep.token, TurnTotalsPath+"?attempt_id="+id), http.StatusNotFound, "no_attempt")
	refused(t, e.call(t, e.lead.token, TurnsPath, "", report), http.StatusBadRequest, "invalid_request")
	refused(t, e.call(t, "", TurnsPath, "", report), http.StatusUnauthorized, "invalid_token")
	refused(t, e.get(t, "", TurnTotalsPath), http.StatusUnauthorized, "invalid_token")
	refused(t, e.get(t, e.worker.token, TurnTotalsPath+"?x=1"), http.StatusBadRequest, "invalid_request")
	refused(t, e.get(t, e.worker.token, TurnTotalsPath+"?attempt_id="), http.StatusBadRequest, "invalid_request")
	for name, body := range map[string]map[string]any{
		"an extra field":  {"id": "turn-4", "cost_usd": 1, "cost_source": "price_list", "extra": 1},
		"a negative cost": {"id": "turn-4", "cost_usd": -1, "cost_source": "price_list"},
		"no source":       {"id": "turn-4", "cost_usd": 1},
	} {
		t.Run(name, func(t *testing.T) {
			refused(t, e.call(t, e.worker.token, TurnsPath, "", body), http.StatusBadRequest, "invalid_request")
		})
	}
}

// A member's launcher reports its hold: the coordinator is told once, the
// team state shows it with the amounts and the next check, and a clear
// removes it; a malformed report or another member's attempt is refused.
func TestHoldRoutes(t *testing.T) {
	e := setupCoordination(t)
	id := e.runningOffer(t)
	next := time.Now().Add(time.Hour).UTC().Truncate(time.Second)
	report := map[string]any{"kind": "attempt_budget", "attempt_id": id, "spent_usd": 10.5, "limit_usd": 10,
		"next_check": next.Format(time.RFC3339)}
	for i, announced := range []bool{true, false} {
		got := e.call(t, e.worker.token, HoldsPath, "", report)
		if got.status != http.StatusOK || got.body["announced"] != announced || got.body["hold"] == nil {
			t.Fatalf("report %d: %d %s", i, got.status, got.raw)
		}
	}
	held := func(m inboxMessage) bool { return m.Kind == "lifecycle" && strings.Contains(m.Text, "is held") }
	var told []inboxMessage
	for _, m := range inboxOf(t, e.get(t, e.lead.token, InboxPath+"?limit=100")) {
		if held(m) {
			told = append(told, m)
		}
	}
	if len(told) != 1 || told[0].AttemptID != id || !strings.Contains(told[0].Text, "10.50 USD of 10.00 USD") {
		t.Fatalf("the coordinator's messages: %+v", told)
	}
	for _, m := range inboxOf(t, e.get(t, e.indep.token, InboxPath+"?limit=100")) {
		if held(m) {
			t.Fatalf("a member other than the coordinators was told: %+v", m)
		}
	}
	hold := func() *opapi.MemberHold {
		var list []opapi.TeamState
		if got := e.adminCall(t, http.MethodGet, opapi.TeamStatesPath, e.opToken, nil, nil, &list); got.status != http.StatusOK {
			t.Fatalf("team states: %d %s", got.status, got.raw)
		}
		for _, m := range list[0].Members {
			if m.AgentID == e.worker.agent.ID {
				return m.Hold
			}
		}
		t.Fatal("no worker in the team state")
		return nil
	}
	if h := hold(); h == nil || h.Kind != "attempt_budget" || h.AttemptID != id || h.SpentUSD != 10.5 || h.LimitUSD != 10 ||
		!h.NextCheck.Equal(next) || h.Since.IsZero() {
		t.Fatalf("the team state's hold: %+v", h)
	}
	if got := e.call(t, e.worker.token, HoldsPath, "", map[string]any{"kind": ""}); got.status != http.StatusOK {
		t.Fatalf("clear: %d %s", got.status, got.raw)
	}
	if h := hold(); h != nil {
		t.Fatalf("a cleared hold shows: %+v", h)
	}

	refused(t, e.call(t, e.lead.token, HoldsPath, "", report), http.StatusNotFound, "no_attempt")
	refused(t, e.call(t, "", HoldsPath, "", report), http.StatusUnauthorized, "invalid_token")
	for name, body := range map[string]map[string]any{
		"an extra field":       {"kind": "daily_budget", "spent_usd": 1, "limit_usd": 1, "next_check": next, "extra": 1},
		"an unknown kind":      {"kind": "tired", "next_check": next},
		"no next check":        {"kind": "daily_budget", "spent_usd": 1, "limit_usd": 1},
		"a clear with a field": {"kind": "", "attempt_id": id},
		"a login's amounts":    {"kind": "needs_login", "spent_usd": 1, "next_check": next, "command": "claude"},
	} {
		t.Run(name, func(t *testing.T) {
			refused(t, e.call(t, e.worker.token, HoldsPath, "", body), http.StatusBadRequest, "invalid_request")
		})
	}
}
