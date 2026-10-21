package server

import (
	"errors"
	"net/http"

	"github.com/BlackVS/aicrew/internal/store"
)

// The turns' costs (docs/CREW-CONTRACT.md, "The turns' costs";
// store/turns.go): a member's launcher reports each turn's cost, and reads
// its totals before a turn in a home that has none. Neither route is a
// step: a report is idempotent by its turn's id, and needs no
// Idempotency-Key.
const (
	TurnsPath      = "/v1/crew/turns"
	TurnTotalsPath = "/v1/crew/turns/totals"
)

// maxTurnReportBody bounds a turn's report.
const maxTurnReportBody = 4 << 10

func (s *Server) registerTurns() {
	s.handle(http.MethodPost, TurnsPath, s.reportTurn)
	s.handle(http.MethodGet, TurnTotalsPath, s.turnTotals)
}

// reportTurn is POST TurnsPath with a store.TurnReport.
func (s *Server) reportTurn(w http.ResponseWriter, r *http.Request) {
	var in store.TurnReport
	token, _, ok := s.stepRequestMax(w, r, false, &in, maxTurnReportBody)
	if !ok {
		return
	}
	out, err := s.store.ReportTurnWithToken(r.Context(), token, in)
	if err != nil {
		s.refuseSession(w, r, turnRefusal(err), false, 0)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// turnTotals is GET TurnTotalsPath[?attempt_id=ID].
func (s *Server) turnTotals(w http.ResponseWriter, r *http.Request) {
	token := sessionToken(r)
	if _, err := s.store.AuthenticateSessionToken(r.Context(), token); err != nil {
		s.refuseSession(w, r, refusalCode(err), false, 0)
		return
	}
	var attempt string
	for name, v := range r.URL.Query() {
		if name != "attempt_id" || len(v) != 1 || v[0] == "" {
			s.refuseSession(w, r, "invalid_request", false, 0)
			return
		}
		attempt = v[0]
	}
	t, err := s.store.TurnTotalsWithToken(r.Context(), token, attempt)
	if err != nil {
		s.refuseSession(w, r, turnRefusal(err), false, 0)
		return
	}
	writeJSON(w, http.StatusOK, t)
}

func turnRefusal(err error) string {
	if errors.Is(err, store.ErrNoAttempt) {
		return "no_attempt"
	}
	return refusalCode(err)
}
