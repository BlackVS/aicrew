package server

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"
)

// A member reads its team's requirements and reports its capabilities, which
// the team then lists; a report that is not one is refused, and a request
// without a valid session token reads nothing.
func TestRequirementsAndCapabilities(t *testing.T) {
	e := setupCoordination(t)
	got := e.get(t, e.worker.token, RequirementsPath)
	var req struct {
		Projects []requirementView `json:"projects"`
	}
	if got.status != http.StatusOK || json.Unmarshal([]byte(got.raw), &req) != nil || len(req.Projects) != 1 ||
		req.Projects[0].ProjectID != "project-example" || req.Projects[0].Repository.URL != coordRepo.URL ||
		req.Projects[0].Repository.Access != "write" {
		t.Fatalf("requirements: %d %s", got.status, got.raw)
	}
	report := map[string]any{"capabilities": []map[string]any{{"host": "git.example.test", "kind": "gitea", "account": "indep",
		"repositories": []map[string]string{{"url": coordRepo.URL, "access": "read"}}}}}
	if got := e.call(t, e.indep.token, CapabilitiesPath, "", report); got.status != http.StatusOK || got.body["reported_at"] == nil {
		t.Fatalf("report: %d %s", got.status, got.raw)
	}
	refused(t, e.call(t, e.indep.token, CapabilitiesPath, "", map[string]any{"capabilities": []map[string]any{{"host": "h"}}}),
		http.StatusBadRequest, "invalid_request")
	refused(t, e.call(t, e.indep.token, CapabilitiesPath, "", map[string]any{"other": 1}), http.StatusBadRequest, "invalid_request")
	refused(t, e.call(t, "", CapabilitiesPath, "", report), http.StatusUnauthorized, "invalid_token")
	refused(t, e.get(t, "", RequirementsPath), http.StatusUnauthorized, "invalid_token")

	var team struct {
		Members []struct {
			AgentID      string `json:"agent_id"`
			Role         string `json:"role"`
			ReportedAt   *time.Time
			Capabilities []struct {
				Repositories []struct {
					Access string `json:"access"`
				} `json:"repositories"`
			} `json:"capabilities"`
		} `json:"members"`
	}
	got = e.get(t, e.lead.token, CapabilitiesPath)
	if got.status != http.StatusOK || json.Unmarshal([]byte(got.raw), &team) != nil || len(team.Members) != 3 {
		t.Fatalf("team capabilities: %d %s", got.status, got.raw)
	}
	for _, m := range team.Members {
		if m.AgentID == e.indep.agent.ID && (len(m.Capabilities) != 1 || m.Capabilities[0].Repositories[0].Access != "read") {
			t.Fatalf("the independent member's row: %+v", m)
		}
	}
}

// An offer goes only to a worker whose last report verified the offer's
// repository at the offer's access: no report, or read access for a write
// offer, is capability_missing naming the host and the access, and nothing
// begins. A claim is not checked: the claimer verifies with its own
// credential.
func TestOfferNeedsTheWorkersCapability(t *testing.T) {
	e := setupCoordination(t)
	expires := time.Now().Add(time.Hour)
	report := func(access string) {
		t.Helper()
		var caps []map[string]any
		if access != "" {
			caps = []map[string]any{{"host": "git.example.test", "kind": "gitea", "account": "worker",
				"repositories": []map[string]string{{"url": coordRepo.URL, "access": access}}}}
		}
		if got := e.call(t, e.worker.token, CapabilitiesPath, "", map[string]any{"capabilities": caps}); got.status != http.StatusOK {
			t.Fatalf("report %q: %d %s", access, got.status, got.raw)
		}
	}
	for _, access := range []string{"", "read"} {
		report(access)
		got := e.call(t, e.lead.token, AttemptsPath, "offer-"+access, e.offerBody("task-1", expires))
		refused(t, got, http.StatusConflict, "capability_missing")
		if msg, _ := got.body["message"].(string); !strings.Contains(msg, "write access") || !strings.Contains(msg, "git.example.test") {
			t.Fatalf("the refusal does not name the host and the access: %s", got.raw)
		}
	}
	stepOf(t, e.call(t, e.indep.token, ClaimPath, "claim-1", e.claimBody("task-2")))
	report("write")
	stepOf(t, e.call(t, e.lead.token, AttemptsPath, "offer-w", e.offerBody("task-1", expires)))
}
