package server

import (
	"net/http"

	"github.com/BlackVS/aicrew/internal/opapi"
	"github.com/BlackVS/aicrew/internal/store"
)

// The operator's read of an attempt's handoff (280c): the worker's session
// doc as its launcher last pushed it (handoff.go), for the operator
// credential or an architect credential, in any state of the attempt.

func (s *Server) registerAttemptHandoff() {
	s.handle(http.MethodGet, opapi.AttemptHandoffPath, s.adminRoute("attempt.handoff", true, s.attemptHandoff))
}

// attemptHandoff is GET opapi.AttemptHandoffPath?id=ATTEMPT: the operator's
// or an architect's read of an attempt's handoff, in any state of the
// attempt.
func (s *Server) attemptHandoff(w http.ResponseWriter, r *http.Request, op store.Caller) (string, string) {
	q := r.URL.Query()
	for name := range q {
		if name != "id" {
			adminRefuse(w, http.StatusBadRequest, opapi.CodeInvalid, "the only parameter is id")
			return opapi.CodeInvalid, ""
		}
	}
	id := q.Get("id")
	h, err := s.store.AttemptHandoff(r.Context(), op, id)
	if err != nil {
		return adminFail(w, err), ""
	}
	writeJSON(w, http.StatusOK, opapi.AttemptHandoff{AttemptID: h.AttemptID, TeamID: h.TeamID, Handoff: h.Body,
		UpdatedAt: h.UpdatedAt})
	return "ok", id
}
