package server

import (
	"net/http"

	"github.com/BlackVS/aicrew/internal/opapi"
	"github.com/BlackVS/aicrew/internal/store"
)

// The pull requests waiting for the human merge (docs/DESIGN-CONTROL-PLANE.md,
// section 4.2; increment C3), for the operator and an architect credential.
// aicrewd reads its own records: no forge call.

func (s *Server) registerMerges() {
	s.handle(http.MethodGet, opapi.MergesPath, s.adminRoute("merges.list", true, s.listMerges))
}

// listMerges is GET /v1/admin/merges[?team=ID].
func (s *Server) listMerges(w http.ResponseWriter, r *http.Request, op store.Caller) (string, string) {
	q := r.URL.Query()
	for name := range q {
		if name != "team" {
			adminRefuse(w, http.StatusBadRequest, opapi.CodeInvalid, "the only parameter is team")
			return opapi.CodeInvalid, ""
		}
	}
	list, err := s.store.PendingMerges(r.Context(), op, q.Get("team"))
	if err != nil {
		return adminFail(w, err), ""
	}
	out := make([]opapi.PendingMerge, 0, len(list))
	for _, m := range list {
		pm := opapi.PendingMerge{TeamID: m.TeamID, TeamName: m.TeamName, AttemptID: m.AttemptID,
			Task:     opapi.EscalationTask{HubID: m.Task.HubID, ProjectID: m.Task.ProjectID, TaskID: m.Task.TaskID},
			WorkerID: m.WorkerID, WorkerLabel: m.WorkerLabel, ResultSeq: m.ResultSeq, ResultRef: m.ResultRef,
			SubmittedAt: m.SubmittedAt, Branch: m.Branch}
		if p := m.Pull; p != nil {
			pm.Pull = &opapi.PullSeen{Number: p.Number, URL: p.URL, State: p.State, HeadCommit: p.HeadCommit,
				Labels: p.Labels, Checks: p.Checks, Comments: p.Comments, ObservedAt: p.ObservedAt}
		}
		out = append(out, pm)
	}
	writeJSON(w, http.StatusOK, out)
	return "ok", ""
}
