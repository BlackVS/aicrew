package server

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/BlackVS/aicrew/internal/store"
)

// The worker pushes its session doc to its running attempt and reads it
// back over the attempt's handoff route; no other member reaches it, a
// body over the bound or an extra field is refused, and a requested stop
// refuses the push while the read still answers.
func TestAttemptHandoffRoute(t *testing.T) {
	e := setupCoordination(t)
	id := e.runningOffer(t)
	path := AttemptsPath + "/" + id + "/handoff"

	got := e.get(t, e.worker.token, path)
	if got.status != http.StatusOK || got.body["attempt_id"] != id || got.body["handoff"] != nil || got.body["updated_at"] != nil {
		t.Fatalf("no handoff yet: %d %s", got.status, got.raw)
	}
	doc := "# Handoff\n\n## CURRENT ATTEMPT\n\n### Next\n\nOpen the pull request.\n"
	got = e.call(t, e.worker.token, path, "", map[string]string{"handoff": doc})
	if got.status != http.StatusOK || got.body["attempt_id"] != id || got.body["updated_at"] == nil || got.body["handoff"] != nil {
		t.Fatalf("the push: %d %s", got.status, got.raw)
	}
	got = e.get(t, e.worker.token, path)
	if got.status != http.StatusOK || got.body["handoff"] != doc || got.body["updated_at"] == nil {
		t.Fatalf("the read: %d %s", got.status, got.raw)
	}

	for _, m := range []member{e.lead, e.indep} {
		refused(t, e.call(t, m.token, path, "", map[string]string{"handoff": doc}), http.StatusNotFound, "no_attempt")
		refused(t, e.get(t, m.token, path), http.StatusNotFound, "no_attempt")
	}
	refused(t, e.call(t, "", path, "", map[string]string{"handoff": doc}), http.StatusUnauthorized, "invalid_token")
	refused(t, e.get(t, "", path), http.StatusUnauthorized, "invalid_token")
	refused(t, e.get(t, e.worker.token, path+"?x=1"), http.StatusBadRequest, "invalid_request")
	refused(t, e.call(t, e.worker.token, path, "", map[string]any{"handoff": doc, "extra": 1}), http.StatusBadRequest,
		"invalid_request")
	refused(t, e.call(t, e.worker.token, path, "", map[string]string{"handoff": strings.Repeat("x", store.MaxHandoffBytes+1)}),
		http.StatusBadRequest, "invalid_request")
	refused(t, e.call(t, e.worker.token, path, "", map[string]string{"handoff": ""}), http.StatusBadRequest, "invalid_request")

	if _, err := e.store.RequestStopWithToken(context.Background(), "stop-1", e.lead.token, id, "priorities changed"); err != nil {
		t.Fatal(err)
	}
	refused(t, e.call(t, e.worker.token, path, "", map[string]string{"handoff": doc + "more\n"}), http.StatusConflict, "attempt_state")
	if got = e.get(t, e.worker.token, path); got.status != http.StatusOK || got.body["handoff"] != doc {
		t.Fatalf("the read while a stop is requested: %d %s", got.status, got.raw)
	}
}
