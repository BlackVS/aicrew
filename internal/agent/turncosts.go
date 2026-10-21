package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
)

// The turns' costs on aicrewd (CREW-CONTRACT.md, "The turns' costs"). The
// budgets are checked against state/turns.json, a cache of what aicrewd
// holds: after each turn the launcher reports the turn's cost, and a home
// whose state has no figure for the day or the attempt takes aicrewd's
// totals first, so a member that moves to a new home keeps its spend. A
// report that does not reach aicrewd stays in the state and is sent again
// before the next turn.

const (
	turnsPath      = "/v1/crew/turns"
	turnTotalsPath = "/v1/crew/turns/totals"
)

// maxUnreported bounds the reports kept for a resend; older ones are
// dropped, so a long outage cannot grow the state without limit.
const maxUnreported = 50

// turnReport is a turn's report, named as TurnRecord names its fields.
type turnReport struct {
	ID         string  `json:"id"`
	AttemptID  string  `json:"attempt_id,omitempty"`
	CostUSD    float64 `json:"cost_usd"`
	CostSource string  `json:"cost_source"`
}

// turnTotals is the member's spend as aicrewd counts it: on Day, aicrewd's
// UTC date, and on the attempt the read named.
type turnTotals struct {
	Day        string  `json:"day"`
	DayUSD     float64 `json:"day_usd"`
	AttemptUSD float64 `json:"attempt_usd"`
}

// queueReport adds the turn's report to the state's backlog, dropping the
// oldest beyond maxUnreported.
func (l *TurnLoop) queueReport(st *turnState, rec TurnRecord) {
	source := rec.CostSource
	if source == "" {
		source = CostUnpriced
	}
	st.Unreported = append(st.Unreported, turnReport{ID: rec.ID, AttemptID: rec.AttemptID, CostUSD: rec.CostUSD,
		CostSource: source})
	if n := len(st.Unreported) - maxUnreported; n > 0 {
		l.Log.Warn("turn costs not reported to aicrewd are dropped: the backlog is full", "dropped", n,
			"oldest", st.Unreported[0].ID)
		st.Unreported = append([]turnReport(nil), st.Unreported[n:]...)
	}
}

// reportTurns sends the backlog in order until one is not recorded. A
// report aicrewd refuses as malformed or for an attempt that is not the
// member's is dropped and logged; any other failure, a session's refusal
// among them, keeps it for the next turn.
func (l *TurnLoop) reportTurns(ctx context.Context, st *turnState) {
	for len(st.Unreported) > 0 {
		r := st.Unreported[0]
		if err := l.Host.ReportTurn(ctx, r); err != nil {
			var refusal *Refusal
			if !errors.As(err, &refusal) || (refusal.Code != "invalid_request" && refusal.Code != "no_attempt") {
				l.Log.Warn("the turn's cost was not reported to aicrewd; it is sent again before the next turn",
					"turn", r.ID, "error", err.Error())
				return
			}
			l.Log.Warn("aicrewd refused the turn's cost; it is dropped", "turn", r.ID, "error", err.Error())
		}
		st.Unreported = st.Unreported[1:]
	}
	st.Unreported = nil
}

// pullTotals takes aicrewd's totals into a state that has no figure for
// day or for the attempt, keeping the larger of each. It reports whether
// the state can be trusted for the admission.
func (l *TurnLoop) pullTotals(ctx context.Context, st *turnState, day string, attempt *memberAttempt, knownAttempt bool) bool {
	_, knownDay := st.Days[day]
	if knownDay && (attempt == nil || knownAttempt) {
		return true
	}
	id := ""
	if attempt != nil {
		id = attempt.ID
	}
	t, err := l.Host.TurnTotals(ctx, id)
	var refusal *Refusal
	switch {
	case errors.As(err, &refusal) && refusal.Code == "not_found":
		// An aicrewd older than the route keeps no totals: the home's own
		// figures stand.
		l.Log.Warn("aicrewd keeps no turn costs; the budgets are checked against this home's figures alone")
		t = turnTotals{}
	case err != nil:
		l.Log.Warn("the budgets' totals were not read from aicrewd", "error", err.Error())
		return false
	}
	if t.Day != day {
		// The hub's date is another (a turn at midnight): the local figure
		// stands for this day.
		t.DayUSD = 0
	}
	st.Days[day] = max(st.Days[day], t.DayUSD)
	if attempt != nil {
		a := st.attempt(id)
		a.CostUSD = max(a.CostUSD, t.AttemptUSD)
	}
	if err := writeTurnState(l.Home, *st); err != nil {
		l.Log.Warn("record the turn state", "error", err.Error())
	}
	return true
}

// ReportTurn reports a turn's cost to aicrewd, as the launcher's session.
func (s *StepServer) ReportTurn(ctx context.Context, r turnReport) error {
	body, err := json.Marshal(r)
	if err != nil {
		return err
	}
	_, err = s.local.LocalStep(ctx, newKey("turn"), s.driver.Session.stepToken(), turnsPath, body)
	return err
}

// TurnTotals reads the member's totals from aicrewd: the day's and, when
// attemptID is set, the attempt's.
func (s *StepServer) TurnTotals(ctx context.Context, attemptID string) (turnTotals, error) {
	path := turnTotalsPath
	if attemptID != "" {
		path += "?attempt_id=" + url.QueryEscape(attemptID)
	}
	raw, err := s.local.Read(ctx, s.driver.Session.stepToken(), path)
	if err != nil {
		return turnTotals{}, err
	}
	var t turnTotals
	if err := json.Unmarshal(raw, &t); err != nil {
		return turnTotals{}, fmt.Errorf("the totals are not the expected JSON: %w", err)
	}
	return t, nil
}
