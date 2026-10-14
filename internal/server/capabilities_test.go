package server

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/BlackVS/aicrew/internal/opapi"
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

// A member reads every project granted to its team (3a4b): one the hub
// grants without a repository too, with its repository null, which the
// requirements leave out; and nothing without a valid session token.
func TestTeamProjects(t *testing.T) {
	e := setupCoordination(t)
	e.hub.mu.Lock()
	e.hub.bare = []string{"project-docs"}
	e.hub.mu.Unlock()
	got := e.get(t, e.worker.token, ProjectsPath)
	var out struct {
		Projects []projectView `json:"projects"`
	}
	if got.status != http.StatusOK || json.Unmarshal([]byte(got.raw), &out) != nil || len(out.Projects) != 2 {
		t.Fatalf("projects: %d %s", got.status, got.raw)
	}
	byID := map[string]projectView{}
	for _, p := range out.Projects {
		byID[p.ProjectID] = p
	}
	if docs, ok := byID["project-docs"]; !ok || docs.HubID != coordHub || docs.Repository != nil ||
		!strings.Contains(got.raw, `"repository":null`) {
		t.Fatalf("the project without a repository: %s", got.raw)
	}
	if ex := byID["project-example"]; ex.Repository == nil || ex.Repository.Access != "write" {
		t.Fatalf("the project with a repository: %s", got.raw)
	}
	req := e.get(t, e.worker.token, RequirementsPath)
	if req.status != http.StatusOK || strings.Contains(req.raw, "project-docs") {
		t.Fatalf("the requirements name a project without a repository: %s", req.raw)
	}
	refused(t, e.get(t, "", ProjectsPath), http.StatusUnauthorized, "invalid_token")
	if strings.Contains(got.raw, "toolchain") {
		t.Fatalf("a project without a toolchain names one: %s", got.raw)
	}

	// The operator selects a toolchain for one project (pilot finding #18);
	// the members read it with the project. An unknown name is refused.
	var tm opapi.Team
	if got := e.adminCall(t, http.MethodPost, opapi.TeamToolchainPath, e.opToken, nil, opapi.TeamToolchainRequest{
		TeamID: e.team.ID, ProjectID: "project-example", Toolchain: "dev-go"}, &tm); got.status != http.StatusOK ||
		tm.Toolchains["project-example"] != "dev-go" {
		t.Fatalf("select: %d %s", got.status, got.raw)
	}
	adminRefused(t, e.adminCall(t, http.MethodPost, opapi.TeamToolchainPath, e.opToken, nil, opapi.TeamToolchainRequest{
		TeamID: e.team.ID, ProjectID: "project-example", Toolchain: "dev-cobol"}, nil), http.StatusBadRequest, opapi.CodeInvalid)
	got = e.get(t, e.lead.token, ProjectsPath)
	out.Projects = nil
	if got.status != http.StatusOK || json.Unmarshal([]byte(got.raw), &out) != nil {
		t.Fatalf("projects: %d %s", got.status, got.raw)
	}
	for _, p := range out.Projects {
		if want := map[string]string{"project-example": "dev-go"}[p.ProjectID]; p.Toolchain != want {
			t.Fatalf("project %s carries toolchain %q, want %q: %s", p.ProjectID, p.Toolchain, want, got.raw)
		}
	}
	var cleared opapi.Team
	if got := e.adminCall(t, http.MethodPost, opapi.TeamToolchainPath, e.opToken, nil, opapi.TeamToolchainRequest{
		TeamID: e.team.ID, ProjectID: "project-example"}, &cleared); got.status != http.StatusOK || cleared.Toolchains != nil {
		t.Fatalf("clear: %d %s", got.status, got.raw)
	}
	if got := e.get(t, e.worker.token, ProjectsPath); strings.Contains(got.raw, "toolchain") {
		t.Fatalf("a cleared toolchain is still carried: %s", got.raw)
	}
}
