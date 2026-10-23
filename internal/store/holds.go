package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
	"time"

	"github.com/BlackVS/aicrew/internal/events"
)

// A member's hold (docs/CREW-CONTRACT.md, "A member's hold"; DESIGN-CONTROL-
// PLANE 3.3 and 3.4). A member's launcher holds it, giving it no turn, while
// a budget is spent, the provider's limit is reached or the client's login
// is missing, and reports each hold here, and clears it when it admits a
// turn again. aicrewd keeps the member's current hold for the team state
// and tells the team's coordinators once per hold: a report of the hold it
// already announced refreshes the row and sends nothing. A spent budget is
// also journaled as budget.spent, which no default subscription handles.

// The holds' kinds.
const (
	HoldAttemptBudget = "attempt_budget"
	HoldDailyBudget   = "daily_budget"
	HoldProviderLimit = "provider_limit"
	HoldNeedsLogin    = "needs_login"
)

// maxHoldCommand bounds a needs_login hold's command.
const maxHoldCommand = 512

// schemaV39 keeps each member's current hold. A cleared hold keeps its row,
// with an empty kind, so the hold it announced last is not announced again.
var schemaV39 = []string{
	`CREATE TABLE member_holds (
		team_id    TEXT NOT NULL,
		agent_id   TEXT NOT NULL,
		kind       TEXT NOT NULL,
		attempt_id TEXT NOT NULL,
		spent_usd  REAL NOT NULL,
		limit_usd  REAL NOT NULL,
		next_check TEXT NOT NULL,
		reset_at   TEXT NOT NULL,
		command    TEXT NOT NULL,
		identity   TEXT NOT NULL,
		announced  TEXT NOT NULL,
		since      TEXT NOT NULL,
		updated_at TEXT NOT NULL,
		PRIMARY KEY (team_id, agent_id)
	)`,
}

// HoldReport is a launcher's report of its member's hold; an empty Kind
// clears it. The amounts are a budget's, the command a needs_login's.
type HoldReport struct {
	Kind      string    `json:"kind"`
	AttemptID string    `json:"attempt_id,omitempty"`
	SpentUSD  float64   `json:"spent_usd,omitempty"`
	LimitUSD  float64   `json:"limit_usd,omitempty"`
	NextCheck time.Time `json:"next_check,omitzero"`
	ResetAt   time.Time `json:"reset_at,omitzero"`
	Command   string    `json:"command,omitempty"`
}

// MemberHold is a member's current hold: the report, and when this hold
// began.
type MemberHold struct {
	HoldReport
	Since time.Time `json:"since"`
}

// HoldResult answers a hold's report: the member's hold after it, none
// after a clear, and whether this report announced it.
type HoldResult struct {
	Hold      *MemberHold `json:"hold,omitempty"`
	Announced bool        `json:"announced"`
}

func (r HoldReport) validate() error {
	budget := r.Kind == HoldAttemptBudget || r.Kind == HoldDailyBudget
	amount := func(v float64) bool { return !math.IsNaN(v) && v >= 0 && v <= MaxTurnCostUSD*100 }
	switch {
	case r.Kind == "":
		if r != (HoldReport{}) {
			return fmt.Errorf("%w: a clear names nothing but the empty kind", ErrInvalid)
		}
		return nil
	case !budget && r.Kind != HoldProviderLimit && r.Kind != HoldNeedsLogin:
		return fmt.Errorf("%w: the hold's kind is one of %s, %s, %s or %s", ErrInvalid, HoldAttemptBudget, HoldDailyBudget,
			HoldProviderLimit, HoldNeedsLogin)
	case len(r.AttemptID) > maxRefLen || (r.AttemptID != "" && !validRefs(r.AttemptID)):
		return fmt.Errorf("%w: the hold's attempt", ErrInvalid)
	case r.Kind == HoldAttemptBudget && r.AttemptID == "":
		return fmt.Errorf("%w: an attempt's budget names the attempt", ErrInvalid)
	case !amount(r.SpentUSD) || !amount(r.LimitUSD):
		return fmt.Errorf("%w: the hold's amounts are from 0 to %d USD", ErrInvalid, MaxTurnCostUSD*100)
	case !budget && (r.SpentUSD != 0 || r.LimitUSD != 0):
		return fmt.Errorf("%w: only a budget's hold has amounts", ErrInvalid)
	case r.NextCheck.IsZero():
		return fmt.Errorf("%w: the hold's next check", ErrInvalid)
	case r.Kind != HoldProviderLimit && !r.ResetAt.IsZero():
		return fmt.Errorf("%w: only a provider's limit has a reset time", ErrInvalid)
	case r.Kind != HoldNeedsLogin && r.Command != "":
		return fmt.Errorf("%w: only a needs_login hold has a command", ErrInvalid)
	case r.Kind == HoldNeedsLogin && (len(r.Command) > maxHoldCommand || validateMessageText(r.Command) != nil):
		return fmt.Errorf("%w: a needs_login hold names the command that logs in, at most %d bytes", ErrInvalid, maxHoldCommand)
	}
	return nil
}

// holdIdentity is what makes a hold another one: the attempt for its
// budget, the UTC day for the daily budget, and for a provider's limit or
// a missing login the attempt with the reset time, or with the day when
// the provider gave none. A provider's limit hit again by each retry
// within a day is one hold.
func holdIdentity(r HoldReport, now time.Time) string {
	switch r.Kind {
	case HoldAttemptBudget:
		return r.Kind + "/" + r.AttemptID
	case HoldDailyBudget:
		return r.Kind + "/" + dayKey(now)
	case HoldProviderLimit:
		if !r.ResetAt.IsZero() {
			return r.Kind + "/" + r.AttemptID + "/" + formatTime(r.ResetAt)
		}
	}
	return r.Kind + "/" + r.AttemptID + "/" + dayKey(now)
}

// ReportHoldWithToken records the hold the token's member reports, or
// clears it. A hold's attempt must be one the member is the worker of. A
// hold not announced before writes one lifecycle message to the team's
// coordinators, the member aside, and a spent budget's budget.spent event.
func (s *Store) ReportHoldWithToken(ctx context.Context, token string, r HoldReport) (HoldResult, error) {
	if err := r.validate(); err != nil {
		return HoldResult{}, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return HoldResult{}, fmt.Errorf("begin: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck // no-op after commit
	now := s.now()
	b, err := tokenBinding(ctx, tx, token, now)
	if err != nil {
		return HoldResult{}, err
	}
	var kind, identity, announced, since string
	err = tx.QueryRowContext(ctx, `SELECT kind, identity, announced, since FROM member_holds WHERE team_id = ? AND agent_id = ?`,
		b.TeamID, b.AgentID).Scan(&kind, &identity, &announced, &since)
	exists := err == nil
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return HoldResult{}, fmt.Errorf("read the hold: %w", err)
	}
	if r.Kind == "" {
		if exists {
			if _, err := tx.ExecContext(ctx,
				`UPDATE member_holds SET kind = '', attempt_id = '', spent_usd = 0, limit_usd = 0, next_check = '', reset_at = '',
				 command = '', identity = '', since = '', updated_at = ? WHERE team_id = ? AND agent_id = ?`,
				formatTime(now), b.TeamID, b.AgentID); err != nil {
				return HoldResult{}, fmt.Errorf("clear the hold: %w", err)
			}
		}
		return HoldResult{}, tx.Commit()
	}
	var task *TaskRef
	if r.AttemptID != "" {
		a, err := workerAttempt(ctx, tx, b, r.AttemptID)
		if err != nil {
			return HoldResult{}, err
		}
		task = &a.Task
	}
	ident := holdIdentity(r, now)
	start := now
	if kind != "" && identity == ident {
		if start, err = parseTime(since); err != nil {
			return HoldResult{}, fmt.Errorf("read the hold's start: %w", err)
		}
	}
	announce := ident != announced
	if announce {
		announced = ident
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO member_holds (team_id, agent_id, kind, attempt_id, spent_usd, limit_usd, next_check, reset_at, command,
		   identity, announced, since, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT (team_id, agent_id) DO UPDATE SET kind = excluded.kind, attempt_id = excluded.attempt_id,
		   spent_usd = excluded.spent_usd, limit_usd = excluded.limit_usd, next_check = excluded.next_check,
		   reset_at = excluded.reset_at, command = excluded.command, identity = excluded.identity,
		   announced = excluded.announced, since = excluded.since, updated_at = excluded.updated_at`,
		b.TeamID, b.AgentID, r.Kind, r.AttemptID, r.SpentUSD, r.LimitUSD, formatTime(r.NextCheck), optionalTime(r.ResetAt),
		r.Command, ident, announced, formatTime(start), formatTime(now)); err != nil {
		return HoldResult{}, fmt.Errorf("record the hold: %w", err)
	}
	out := HoldResult{Hold: &MemberHold{HoldReport: r, Since: start}, Announced: announce}
	if announce {
		if err := announceHold(ctx, tx, b, r, task, ident, now); err != nil {
			return HoldResult{}, err
		}
	}
	if err := tx.Commit(); err != nil {
		return HoldResult{}, fmt.Errorf("commit: %w", err)
	}
	return out, nil
}

// budgetEvent is a budget.spent event's payload: the member and its hold.
type budgetEvent struct {
	AgentID string     `json:"agent_id"`
	Hold    HoldReport `json:"hold"`
}

// announceHold writes a new hold's lifecycle message to the team's
// coordinators, the member aside, and journals a spent budget.
func announceHold(ctx context.Context, tx *sql.Tx, b TokenBinding, r HoldReport, task *TaskRef, ident string, now time.Time) error {
	if r.Kind == HoldAttemptBudget || r.Kind == HoldDailyBudget {
		if _, _, err := journal(ctx, tx, sourceAicrewd, "hold/"+b.AgentID+"/"+ident, memberSubject(b.AgentID),
			events.KindBudgetSpent, budgetEvent{AgentID: b.AgentID, Hold: r}, now, now,
			matches(events.KindBudgetSpent, b.TeamID, events.Facts{})); err != nil {
			return err
		}
	}
	coords, err := activeCoordinators(ctx, tx, b.TeamID)
	if err != nil {
		return err
	}
	var to []string
	for _, c := range coords {
		if c != b.AgentID {
			to = append(to, c)
		}
	}
	if len(to) == 0 {
		return nil
	}
	agent, err := getAgent(ctx, tx, b.AgentID)
	if err != nil {
		return err
	}
	_, err = insertMessage(ctx, tx, Message{TeamID: b.TeamID, Kind: KindLifecycle, SenderAgentID: b.AgentID,
		SenderProfile: agent.Profile, Task: task, AttemptID: r.AttemptID, Text: holdText(agent.Label, b.AgentID, r)}, to, now)
	return err
}

// holdText is a hold's lifecycle message.
func holdText(label, agentID string, r HoldReport) string {
	who := agentID
	if label != "" {
		who = label + " (" + agentID + ")"
	}
	next := r.NextCheck.UTC().Format(time.RFC3339)
	switch r.Kind {
	case HoldAttemptBudget:
		return fmt.Sprintf("Member %s is held: attempt %s's budget is spent, %.2f USD of %.2f USD. "+
			"Its launcher checks the budget again at %s.", who, r.AttemptID, r.SpentUSD, r.LimitUSD, next)
	case HoldDailyBudget:
		return fmt.Sprintf("Member %s is held: the daily budget is spent, %.2f USD of %.2f USD. "+
			"Its launcher checks the budget again at %s.", who, r.SpentUSD, r.LimitUSD, next)
	case HoldProviderLimit:
		if !r.ResetAt.IsZero() {
			return fmt.Sprintf("Member %s is held: the provider's limit is reached until %s. "+
				"Its launcher tries a turn again at %s.", who, r.ResetAt.UTC().Format(time.RFC3339), next)
		}
		return fmt.Sprintf("Member %s is held: the provider's limit is reached, with no reset time given. "+
			"Its launcher tries a turn again at %s.", who, next)
	}
	return fmt.Sprintf("Member %s is held: the client's provider login is missing or expired. To log in: %s. "+
		"Its launcher tries a turn again at %s.", who, r.Command, next)
}

// memberHold reads a member's current hold, nil when it has none.
func memberHold(ctx context.Context, q querier, teamID, agentID string) (*MemberHold, error) {
	var h MemberHold
	var next, reset, since string
	err := q.QueryRowContext(ctx,
		`SELECT kind, attempt_id, spent_usd, limit_usd, next_check, reset_at, command, since FROM member_holds
		 WHERE team_id = ? AND agent_id = ? AND kind != ''`, teamID, agentID).
		Scan(&h.Kind, &h.AttemptID, &h.SpentUSD, &h.LimitUSD, &next, &reset, &h.Command, &since)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read the hold: %w", err)
	}
	if h.NextCheck, err = parseTime(next); err != nil {
		return nil, fmt.Errorf("read the hold's next check: %w", err)
	}
	if h.Since, err = parseTime(since); err != nil {
		return nil, fmt.Errorf("read the hold's start: %w", err)
	}
	if reset != "" {
		if h.ResetAt, err = parseTime(reset); err != nil {
			return nil, fmt.Errorf("read the hold's reset time: %w", err)
		}
	}
	return &h, nil
}

// optionalTime is t formatted, or "" for the zero time.
func optionalTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return formatTime(t)
}
