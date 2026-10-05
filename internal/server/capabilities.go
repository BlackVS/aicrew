package server

import (
	"net/http"
	"time"

	"github.com/BlackVS/aicrew/internal/store"
)

// The member's requirements and capabilities (docs/proposals/
// PILOT-1-FOLLOWUPS.md, 3.6), over the session API.
const (
	RequirementsPath = "/v1/crew/requirements"
	CapabilitiesPath = "/v1/crew/capabilities"
)

func (s *Server) registerCapabilities() {
	s.handle(http.MethodGet, RequirementsPath, s.requirements)
	s.handle(http.MethodGet, CapabilitiesPath, s.teamCapabilities)
	s.handle(http.MethodPost, CapabilitiesPath, s.reportCapabilities)
}

// requirementView is a granted project and the repository the hub binds to
// it: what a member's home has to verify.
type requirementView struct {
	HubID      string `json:"hub_id"`
	ProjectID  string `json:"project_id"`
	Repository struct {
		Kind   string `json:"kind"`
		URL    string `json:"url"`
		Access string `json:"access"`
	} `json:"repository"`
}

// requirements is GET /v1/crew/requirements: the token's team's granted
// projects with their repositories. The team's hub is read first, so a
// member that verifies right after a grant sees it; a hub that does not
// answer leaves the last snapshot.
func (s *Server) requirements(w http.ResponseWriter, r *http.Request) {
	token := sessionToken(r)
	b, err := s.store.AuthenticateSessionToken(r.Context(), token)
	if err != nil {
		s.refuseSession(w, r, refusalCode(err), false, 0)
		return
	}
	if t, err := s.store.GetTeam(r.Context(), b.TeamID); err == nil && t.Hub != "" {
		s.readTeam(r.Context(), t)
	}
	grants, err := s.store.RequirementsWithToken(r.Context(), token)
	if err != nil {
		s.refuseSession(w, r, refusalCode(err), false, 0)
		return
	}
	out := struct {
		Projects []requirementView `json:"projects"`
	}{Projects: []requirementView{}}
	for _, g := range grants {
		v := requirementView{HubID: g.HubID, ProjectID: g.ProjectID}
		v.Repository.Kind, v.Repository.URL, v.Repository.Access = g.Repository.Kind, g.Repository.URL, g.Repository.Access
		out.Projects = append(out.Projects, v)
	}
	writeJSON(w, http.StatusOK, out)
}

// reportCapabilities is POST /v1/crew/capabilities: the member's home's
// verified capabilities, which replace its earlier report.
func (s *Server) reportCapabilities(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Capabilities []store.Capability `json:"capabilities"`
	}
	token, _, ok := s.stepRequest(w, r, false, &in)
	if !ok {
		return
	}
	at, err := s.store.ReportCapabilitiesWithToken(r.Context(), token, in.Capabilities)
	if err != nil {
		s.refuseSession(w, r, refusalCode(err), false, 0)
		return
	}
	writeJSON(w, http.StatusOK, struct {
		ReportedAt time.Time `json:"reported_at"`
	}{at})
}

// teamCapabilities is GET /v1/crew/capabilities: the token's team's members
// with their last reports, for planning who can take a task.
func (s *Server) teamCapabilities(w http.ResponseWriter, r *http.Request) {
	members, err := s.store.TeamCapabilitiesWithToken(r.Context(), sessionToken(r))
	if err != nil {
		s.refuseSession(w, r, refusalCode(err), false, 0)
		return
	}
	writeJSON(w, http.StatusOK, struct {
		Members []store.MemberCapabilities `json:"members"`
	}{members})
}
