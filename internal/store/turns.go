package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
	"regexp"
	"time"
)

// The turns' costs (docs/DESIGN-CONTROL-PLANE.md, section 3.5; task
// 01a157c8-b360). A member's launcher reports each turn's cost after the
// turn, and checks its budgets before the next against the totals aicrewd
// keeps, so a member that moves to a new home keeps its spend. This is the
// first part of increment A4's turn report: one row per turn, with the
// fields the budgets need. A4 adds the rest of the record, the
// one-running-turn rule and the operator's reads.
//
// A turn counts on aicrewd's UTC date when its report first arrives, for
// the member that reported it, and on its attempt. A report of a turn
// already recorded changes nothing.

// MaxTurnCostUSD bounds one turn's reported cost.
const MaxTurnCostUSD = 10000

// costSourceShape is a turn's cost source, such as price_list.
var costSourceShape = regexp.MustCompile(`^[a-z][a-z_]{0,31}$`)

// schemaV38 keeps the turns' reports.
var schemaV38 = []string{
	`CREATE TABLE turn_reports (
		id          TEXT PRIMARY KEY,
		team_id     TEXT NOT NULL,
		agent_id    TEXT NOT NULL,
		attempt_id  TEXT NOT NULL,
		day         TEXT NOT NULL,
		cost_usd    REAL NOT NULL,
		cost_source TEXT NOT NULL,
		session_id  TEXT NOT NULL,
		received_at TEXT NOT NULL
	)`,
	`CREATE INDEX turn_reports_member_day ON turn_reports (team_id, agent_id, day)`,
	`CREATE INDEX turn_reports_attempt ON turn_reports (attempt_id)`,
}

// TurnReport is a turn's report, named as the launcher's turn record names
// its fields. AttemptID is "" for a turn outside an attempt.
type TurnReport struct {
	ID         string  `json:"id"`
	AttemptID  string  `json:"attempt_id,omitempty"`
	CostUSD    float64 `json:"cost_usd"`
	CostSource string  `json:"cost_source"`
}

// TurnTotals is a member's spend as aicrewd counts it: on Day, aicrewd's
// UTC date now, and on AttemptID when the read names one.
type TurnTotals struct {
	Day        string  `json:"day"`
	DayUSD     float64 `json:"day_usd"`
	AttemptID  string  `json:"attempt_id,omitempty"`
	AttemptUSD float64 `json:"attempt_usd"`
}

func dayKey(t time.Time) string { return t.UTC().Format("2006-01-02") }

func (r TurnReport) validate() error {
	switch {
	case !validRefs(r.ID):
		return fmt.Errorf("%w: the turn's id", ErrInvalid)
	case len(r.AttemptID) > maxRefLen:
		return fmt.Errorf("%w: the turn's attempt", ErrInvalid)
	case math.IsNaN(r.CostUSD) || r.CostUSD < 0 || r.CostUSD > MaxTurnCostUSD:
		return fmt.Errorf("%w: the turn's cost is from 0 to %d USD", ErrInvalid, MaxTurnCostUSD)
	case !costSourceShape.MatchString(r.CostSource):
		return fmt.Errorf("%w: the turn's cost source", ErrInvalid)
	}
	return nil
}

// ReportTurnWithToken records the turn the token's member reports. A turn
// it names an attempt of must be one the member is the worker of, in any
// state. A turn already recorded for the member is answered with its
// record unchanged; one recorded for another member is refused.
func (s *Store) ReportTurnWithToken(ctx context.Context, token string, r TurnReport) (TurnReport, error) {
	if err := r.validate(); err != nil {
		return TurnReport{}, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return TurnReport{}, fmt.Errorf("begin: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck // no-op after commit
	now := s.now()
	b, err := tokenBinding(ctx, tx, token, now)
	if err != nil {
		return TurnReport{}, err
	}
	var team, agent string
	var have TurnReport
	err = tx.QueryRowContext(ctx, `SELECT team_id, agent_id, attempt_id, cost_usd, cost_source FROM turn_reports WHERE id = ?`,
		r.ID).Scan(&team, &agent, &have.AttemptID, &have.CostUSD, &have.CostSource)
	switch {
	case err == nil && (team != b.TeamID || agent != b.AgentID):
		return TurnReport{}, fmt.Errorf("%w: turn %s is another member's", ErrInvalid, r.ID)
	case err == nil:
		have.ID = r.ID
		return have, nil
	case !errors.Is(err, sql.ErrNoRows):
		return TurnReport{}, fmt.Errorf("read the turn: %w", err)
	}
	if r.AttemptID != "" {
		if _, err := workerAttempt(ctx, tx, b, r.AttemptID); err != nil {
			return TurnReport{}, err
		}
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO turn_reports (id, team_id, agent_id, attempt_id, day, cost_usd, cost_source, session_id, received_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		r.ID, b.TeamID, b.AgentID, r.AttemptID, dayKey(now), r.CostUSD, r.CostSource, b.SessionID, formatTime(now)); err != nil {
		return TurnReport{}, fmt.Errorf("record the turn: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return TurnReport{}, fmt.Errorf("commit: %w", err)
	}
	return r, nil
}

// TurnTotalsWithToken is the token's member's spend today and, when
// attemptID is set, on that attempt, which must be one the member is the
// worker of.
func (s *Store) TurnTotalsWithToken(ctx context.Context, token, attemptID string) (TurnTotals, error) {
	if len(attemptID) > maxRefLen {
		return TurnTotals{}, fmt.Errorf("%w: the totals' attempt", ErrInvalid)
	}
	var t TurnTotals
	err := s.snapshot(ctx, func(q querier) error {
		now := s.now()
		b, err := tokenBinding(ctx, q, token, now)
		if err != nil {
			return err
		}
		t = TurnTotals{Day: dayKey(now)}
		if err := q.QueryRowContext(ctx,
			`SELECT COALESCE(SUM(cost_usd), 0) FROM turn_reports WHERE team_id = ? AND agent_id = ? AND day = ?`,
			b.TeamID, b.AgentID, t.Day).Scan(&t.DayUSD); err != nil {
			return fmt.Errorf("read the day's total: %w", err)
		}
		if attemptID == "" {
			return nil
		}
		a, err := workerAttempt(ctx, q, b, attemptID)
		if err != nil {
			return err
		}
		t.AttemptID = a.ID
		if err := q.QueryRowContext(ctx,
			`SELECT COALESCE(SUM(cost_usd), 0) FROM turn_reports WHERE attempt_id = ? AND team_id = ? AND agent_id = ?`,
			a.ID, b.TeamID, b.AgentID).Scan(&t.AttemptUSD); err != nil {
			return fmt.Errorf("read the attempt's total: %w", err)
		}
		return nil
	})
	if err != nil {
		return TurnTotals{}, err
	}
	return t, nil
}
