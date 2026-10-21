package server

import (
	"net/http"
	"testing"
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
