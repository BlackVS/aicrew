package agent

import (
	"context"
	"encoding/json"
	"errors"
	"time"
)

// A member's hold on aicrewd (CREW-CONTRACT.md, "A member's hold"). The
// launcher reports the hold it gives its member, no turn while a budget is
// spent, the provider's limit is reached or the client's login is missing,
// and clears it when it admits a turn again: aicrewd shows the hold in the
// team state and tells the team's coordinators once per hold. The report
// is kept in the turn state until aicrewd has it, and sent again at the
// next tick when it did not arrive. A hold that is only the launcher's own,
// after a failed turn or totals it could not read, is not reported.

const holdsPath = "/v1/crew/holds"

// The holds' kinds, as aicrewd names them.
const (
	holdAttemptBudget = "attempt_budget"
	holdDailyBudget   = "daily_budget"
	holdProviderLimit = "provider_limit"
	holdNeedsLogin    = "needs_login"
)

// loginCommand is how a member's client logs in again (DESIGN-CONTROL-PLANE
// 3.3), for the coordinator's message and the team state.
const loginCommand = "run `claude` as the member's OS user, then `/login`"

// holdReport is a hold as aicrewd takes it; the zero report clears it.
type holdReport struct {
	Kind      string    `json:"kind"`
	AttemptID string    `json:"attempt_id,omitempty"`
	SpentUSD  float64   `json:"spent_usd,omitempty"`
	LimitUSD  float64   `json:"limit_usd,omitempty"`
	NextCheck time.Time `json:"next_check,omitzero"`
	ResetAt   time.Time `json:"reset_at,omitzero"`
	Command   string    `json:"command,omitempty"`
}

// setHold makes r, nil for none, the hold aicrewd is to have; a change is
// sent at the next sync.
func setHold(st *turnState, r *holdReport) {
	if r != nil && r.same(holdReport{}) {
		r = nil
	}
	if r == st.HoldReport || (r != nil && st.HoldReport != nil && r.same(*st.HoldReport)) {
		return
	}
	st.HoldReport, st.HoldPending = r, true
}

// same reports whether r and o are the same hold: their times are compared
// as instants, since one read back from the state has lost its monotonic
// reading and may name another location.
func (r holdReport) same(o holdReport) bool {
	return r.Kind == o.Kind && r.AttemptID == o.AttemptID && r.SpentUSD == o.SpentUSD && r.LimitUSD == o.LimitUSD &&
		r.NextCheck.Equal(o.NextCheck) && r.ResetAt.Equal(o.ResetAt) && r.Command == o.Command
}

// syncHold sends the hold aicrewd is to have, when it does not have it yet.
// An aicrewd older than the route keeps no holds: the hold stays this
// home's, said once per launcher. A report aicrewd refuses as malformed or
// for an attempt that is not the member's is dropped and logged; any other
// failure keeps it for the next tick.
func (l *TurnLoop) syncHold(ctx context.Context, st *turnState) {
	if !st.HoldPending {
		return
	}
	r := holdReport{}
	if st.HoldReport != nil {
		r = *st.HoldReport
	}
	err := l.Host.ReportHold(ctx, r)
	var refusal *Refusal
	switch {
	case err == nil:
	case errors.As(err, &refusal) && refusal.Code == "not_found":
		if !l.holdsUnsupported {
			l.Log.Warn("aicrewd keeps no holds; the coordinators are not told of this member's holds: upgrade aicrewd")
			l.holdsUnsupported = true
		}
	case errors.As(err, &refusal) && (refusal.Code == "invalid_request" || refusal.Code == "no_attempt"):
		l.Log.Warn("aicrewd refused the member's hold; it is dropped", "kind", r.Kind, "error", err.Error())
	default:
		l.Log.Warn("the member's hold was not reported to aicrewd; it is sent again at the next tick", "kind", r.Kind,
			"error", err.Error())
		return
	}
	st.HoldPending = false
	if err := writeTurnState(l.Home, *st); err != nil {
		l.Log.Warn("record the turn state", "error", err.Error())
	}
}

// outcomeHold is the hold a turn's outcome reports, nil for none: the
// provider's limit and a missing login. A failed turn's backoff is the
// launcher's own.
func outcomeHold(st turnState, res TurnResult, attemptID string) *holdReport {
	switch {
	case res.Outcome == TurnLimitReached && res.LimitKind != "budget":
		r := &holdReport{Kind: holdProviderLimit, AttemptID: attemptID, NextCheck: st.HoldUntil}
		if st.HoldUntil.Equal(res.ResetAt) {
			r.ResetAt = res.ResetAt
		}
		return r
	case res.Outcome == TurnNeedsLogin:
		return &holdReport{Kind: holdNeedsLogin, AttemptID: attemptID, NextCheck: st.HoldUntil, Command: loginCommand}
	}
	return nil
}

// ReportHold reports the member's hold to aicrewd, as the launcher's
// session.
func (s *StepServer) ReportHold(ctx context.Context, r holdReport) error {
	body, err := json.Marshal(r)
	if err != nil {
		return err
	}
	_, err = s.local.LocalStep(ctx, newKey("hold"), s.driver.Session.stepToken(), holdsPath, body)
	return err
}
