package server

import (
	"errors"
	"net/http"

	"github.com/BlackVS/aicrew/internal/store"
)

// Forge events (docs/CREW-CONTRACT.md, "Forge events"): the worker's
// launcher reports what it observed of its running attempt's pull request,
// read with the home's own forge credential; aicrewd records it and, once
// per observed change, journals an event, which a change that needs a
// decision turns into an action.needed message to the attempt's worker
// and, when it bears on a submit, to the team's coordinators. aicrewd
// itself reads no forge.

// PullsPath is where a member reports a pull request it observed, and
// AutomationsPath where its launcher reports an automation's run at one of
// the pull request's extension points (store/automations.go).
const (
	PullsPath       = "/v1/crew/pulls"
	AutomationsPath = "/v1/crew/automations"

	// maxObserveBody is an observation with its content (a review's body,
	// the failed checks' excerpts, a failed check's output): the server's
	// own bound, which the launcher cuts an observation to fit
	// (agent/forgewatch.go fitObservation). The step routes' bound would
	// refuse one.
	maxObserveBody = MaxBodyBytes
)

func (s *Server) registerForgeEvents() {
	s.handle(http.MethodPost, PullsPath, s.observePull)
	s.handle(http.MethodPost, AutomationsPath, s.recordAutomationRun)
}

// observePull is POST /v1/crew/pulls: the token's member's observation of
// the pull request of one of its running attempts.
func (s *Server) observePull(w http.ResponseWriter, r *http.Request) {
	var in store.PullObservation
	token, _, ok := s.stepRequestMax(w, r, false, &in, maxObserveBody)
	if !ok {
		return
	}
	res, err := s.store.ObservePullWithToken(r.Context(), token, in)
	if err != nil {
		code := refusalCode(err)
		if errors.Is(err, store.ErrNoAttempt) {
			code = "no_attempt"
		}
		s.refuseSession(w, r, code, false, 0)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

// recordAutomationRun is POST /v1/crew/automations: the token's member's
// report of an automation run at an extension point of its running
// attempt's pull request.
func (s *Server) recordAutomationRun(w http.ResponseWriter, r *http.Request) {
	var in store.AutomationRunReport
	token, _, ok := s.stepRequestMax(w, r, false, &in, maxReportBody)
	if !ok {
		return
	}
	res, err := s.store.RecordAutomationRunWithToken(r.Context(), token, in)
	if err != nil {
		code := refusalCode(err)
		if errors.Is(err, store.ErrNoAttempt) {
			code = "no_attempt"
		}
		s.refuseSession(w, r, code, false, 0)
		return
	}
	writeJSON(w, http.StatusOK, res)
}
