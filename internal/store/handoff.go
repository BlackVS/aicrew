package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// The attempt's handoff (docs/DESIGN-TURNS.md, section 3; increment 3c).
// The worker's session doc, its home's docs/HANDOFF.md, is the memory a
// fresh turn continues from. The home is a cache: after a turn the
// worker's launcher pushes the doc to its running attempt, and a home that
// has no copy (a new home of the same member on another host) takes the
// attempt's. aicrewd keeps one doc per attempt, replaced by each push, and
// reads nothing in it: the launcher redacts and bounds it before it sends
// it. A push is refused while a stop is requested and once the attempt is
// no longer running; the launcher keeps its local copy then.

// MaxHandoffBytes bounds the doc an attempt keeps.
const MaxHandoffBytes = 32 << 10

// schemaV37 keeps each attempt's handoff, apart from the attempt's row so
// that no attempt read carries it.
var schemaV37 = []string{
	`CREATE TABLE attempt_handoffs (
		attempt_id TEXT PRIMARY KEY REFERENCES attempts (id),
		body       TEXT NOT NULL,
		session_id TEXT NOT NULL,
		updated_at TEXT NOT NULL
	)`,
}

// Handoff is an attempt's handoff as aicrewd keeps it. An attempt that has
// none has an empty Body and a zero UpdatedAt.
type Handoff struct {
	AttemptID string    `json:"attempt_id"`
	Body      string    `json:"handoff,omitempty"`
	UpdatedAt time.Time `json:"updated_at,omitzero"`
}

// workerAttempt is the attempt id when the token's member is its worker.
// Any other attempt, or none, is ErrNoAttempt, so a member learns nothing
// of another's attempts.
func workerAttempt(ctx context.Context, q querier, b TokenBinding, id string) (Attempt, error) {
	a, err := getAttempt(ctx, q, id)
	switch {
	case errors.Is(err, ErrNotFound) || (err == nil && (a.TeamID != b.TeamID || a.WorkerAgentID != b.AgentID)):
		return Attempt{}, fmt.Errorf("%w: attempt %s is not yours", ErrNoAttempt, id)
	case err != nil:
		return Attempt{}, err
	}
	return a, nil
}

// PutHandoffWithToken replaces the handoff of the token's member's running
// attempt with body. The same body pushed again changes nothing but the
// time.
func (s *Store) PutHandoffWithToken(ctx context.Context, token, attemptID, body string) (Handoff, error) {
	if !validRefs(attemptID) {
		return Handoff{}, fmt.Errorf("%w: the handoff's attempt", ErrInvalid)
	}
	if err := validText(body, MaxHandoffBytes, "the handoff", true); err != nil {
		return Handoff{}, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Handoff{}, fmt.Errorf("begin: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck // no-op after commit
	now := s.now()
	b, err := tokenBinding(ctx, tx, token, now)
	if err != nil {
		return Handoff{}, err
	}
	a, err := workerAttempt(ctx, tx, b, attemptID)
	switch {
	case err != nil:
		return Handoff{}, err
	case a.State != AttemptRunning:
		return Handoff{}, fmt.Errorf("%w: attempt %s is %s, not running", ErrAttemptState, a.ID, a.State)
	case a.Stop != StopNone:
		return Handoff{}, fmt.Errorf("%w: attempt %s: a stop is %s", ErrAttemptState, a.ID, a.Stop)
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO attempt_handoffs (attempt_id, body, session_id, updated_at) VALUES (?, ?, ?, ?)
		 ON CONFLICT (attempt_id) DO UPDATE SET body = excluded.body, session_id = excluded.session_id,
		 updated_at = excluded.updated_at`,
		a.ID, body, b.SessionID, formatTime(now)); err != nil {
		return Handoff{}, fmt.Errorf("record the handoff: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return Handoff{}, fmt.Errorf("commit: %w", err)
	}
	return Handoff{AttemptID: a.ID, Body: body, UpdatedAt: now.UTC()}, nil
}

// HandoffWithToken is the handoff of an attempt the token's member is the
// worker of, in any state; an attempt without one has an empty Body.
func (s *Store) HandoffWithToken(ctx context.Context, token, attemptID string) (Handoff, error) {
	if !validRefs(attemptID) {
		return Handoff{}, fmt.Errorf("%w: the handoff's attempt", ErrInvalid)
	}
	var h Handoff
	err := s.snapshot(ctx, func(q querier) error {
		b, err := tokenBinding(ctx, q, token, s.now())
		if err != nil {
			return err
		}
		a, err := workerAttempt(ctx, q, b, attemptID)
		if err != nil {
			return err
		}
		h = Handoff{AttemptID: a.ID}
		var at string
		err = q.QueryRowContext(ctx, `SELECT body, updated_at FROM attempt_handoffs WHERE attempt_id = ?`, a.ID).Scan(&h.Body, &at)
		switch {
		case errors.Is(err, sql.ErrNoRows):
			return nil
		case err != nil:
			return fmt.Errorf("read the handoff: %w", err)
		}
		if h.UpdatedAt, err = parseTime(at); err != nil {
			return fmt.Errorf("read the handoff's time: %w", err)
		}
		return nil
	})
	if err != nil {
		return Handoff{}, err
	}
	return h, nil
}
