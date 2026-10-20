package server

import (
	"errors"
	"net/http"

	"github.com/BlackVS/aicrew/internal/store"
)

// The attempt's handoff (docs/CREW-CONTRACT.md, "The attempt's handoff";
// store/handoff.go): the worker's launcher pushes its session doc to its
// running attempt after a turn, and a home without a copy reads it back.
// Both routes are the attempt's worker's only; neither is a step, so
// neither takes an Idempotency-Key: a push replaces the doc, and a
// repeated push leaves the same doc.

// handoffBody is a push: the doc, at most store.MaxHandoffBytes, in a body
// within the server's body bound.
type handoffBody struct {
	Handoff string `json:"handoff"`
}

// putHandoff is POST AttemptsPath/{id}/handoff.
func (s *Server) putHandoff(w http.ResponseWriter, r *http.Request) {
	id, _, _ := attemptPath(requestPath(r))
	var in handoffBody
	token, _, ok := s.stepRequestMax(w, r, false, &in, MaxBodyBytes)
	if !ok {
		return
	}
	h, err := s.store.PutHandoffWithToken(r.Context(), token, id, in.Handoff)
	if err != nil {
		s.refuseSession(w, r, handoffRefusal(err), false, 0)
		return
	}
	writeJSON(w, http.StatusOK, store.Handoff{AttemptID: h.AttemptID, UpdatedAt: h.UpdatedAt})
}

// getHandoff is GET AttemptsPath/{id}/handoff: the doc, or none.
func (s *Server) getHandoff(w http.ResponseWriter, r *http.Request) {
	id, _, _ := attemptPath(requestPath(r))
	token := sessionToken(r)
	if _, err := s.store.AuthenticateSessionToken(r.Context(), token); err != nil {
		s.refuseSession(w, r, refusalCode(err), false, 0)
		return
	}
	if len(r.URL.Query()) > 0 {
		s.refuseSession(w, r, "invalid_request", false, 0)
		return
	}
	h, err := s.store.HandoffWithToken(r.Context(), token, id)
	if err != nil {
		s.refuseSession(w, r, handoffRefusal(err), false, 0)
		return
	}
	writeJSON(w, http.StatusOK, h)
}

func handoffRefusal(err error) string {
	switch {
	case errors.Is(err, store.ErrNoAttempt):
		return "no_attempt"
	case errors.Is(err, store.ErrAttemptState):
		return "attempt_state"
	}
	return refusalCode(err)
}
