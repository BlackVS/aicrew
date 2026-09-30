package store

import (
	"context"
	"database/sql"
	"fmt"
	"strconv"
	"time"
)

// The reconciler (crew-execution b3b). aicrewd settles the steps members left
// pending, and closes attempts whose reservation aimem closed outside aicrew,
// with no member online. It acts only on aimem's read scope, as the
// reconciler caller:
//   - a pending step is settled by the same rules as a member's settle, but
//     with no member's report, so nothing is voided: it settles committed
//     only on a committed receipt for that exact step (by its proof or, for
//     an update, its request key), and not committed only when the scope's
//     rules make a "none" final;
//   - an open attempt closes as recovered only on the scope's closure
//     evidence for its exact reservation, with a closing fence past the
//     fence aicrew confirmed.
//
// It never begins a step, confirms a delivery, reviews, or writes to aimem;
// every other command refuses its caller.

const opCloseRecovered = "attempt.close_recovered"

// ReconcileCandidate is an attempt the reconciler looks at: one with a
// pending step to settle, or an open attempt holding a reservation.
type ReconcileCandidate struct {
	AttemptID string
	Pending   bool
	Task      TaskRef
	UpdatedAt time.Time
}

// ReconcileCandidates lists the attempts with a pending step, then the open
// attempts that hold a confirmed reservation and have no step pending, each
// oldest first.
func (s *Store) ReconcileCandidates(ctx context.Context) ([]ReconcileCandidate, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, pending_key != '', task_hub_id, task_project_id, task_id, updated_at FROM attempts
		 WHERE pending_key != ''
		    OR (state IN ('offered', 'running') AND reservation_id != '' AND pending_key = '')
		 ORDER BY pending_key = '', updated_at, id`)
	if err != nil {
		return nil, fmt.Errorf("list reconcile candidates: %w", err)
	}
	defer rows.Close()
	var out []ReconcileCandidate
	for rows.Next() {
		var c ReconcileCandidate
		var updated string
		if err := rows.Scan(&c.AttemptID, &c.Pending, &c.Task.HubID, &c.Task.ProjectID, &c.Task.TaskID, &updated); err != nil {
			return nil, fmt.Errorf("read reconcile candidate: %w", err)
		}
		if c.UpdatedAt, err = parseTime(updated); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// ReconcileStep settles attemptID's pending step as the reconciler, from
// reader's answers alone. It voids nothing, so a member still sending the
// step is never cut off: a proof that lives keeps the step pending.
func (s *Store) ReconcileStep(ctx context.Context, reader ReservationReader, attemptID string) (Attempt, Settlement, error) {
	if reader == nil {
		return Attempt{}, Settlement{}, fmt.Errorf("%w: the reconciler needs the read scope", ErrInvalid)
	}
	a, err := s.GetAttempt(ctx, attemptID)
	if err != nil {
		return Attempt{}, Settlement{}, err
	}
	if a.PendingKey == "" {
		return a, Settlement{}, nil
	}
	return s.settleStep(ctx, ReconcilerCaller(), reader, attemptID, a.PendingKey, StepReport{reconciled: true}, nil)
}

// recoveredInput is the audited input of a recovered closure: the scope's
// closure evidence and the reservation it closes.
type recoveredInput struct {
	AttemptID     string `json:"attempt_id"`
	ReservationID string `json:"reservation_id"`
	Fence         string `json:"fence"`
	ClosingFence  string `json:"closing_fence"`
	ClosedBy      string `json:"closed_by"`
	ClosedAt      string `json:"closed_at"`
}

// CloseRecovered closes attemptID as recovered on the read scope's closure
// evidence (C5c-w): hold must be "closed" for exactly the attempt's
// reservation, with a closing fence greater than the fence aicrew
// confirmed. Anything else changes nothing. The rule is checked again inside
// the closing transaction, against the attempt as it is then. It returns
// whether the attempt closed.
func (s *Store) CloseRecovered(ctx context.Context, attemptID string, hold ScopeHold) (Attempt, bool, error) {
	c := ReconcilerCaller()
	unlock, err := s.lockAttempt(ctx, attemptID)
	if err != nil {
		return Attempt{}, false, err
	}
	defer unlock()
	a, err := s.GetAttempt(ctx, attemptID)
	if err != nil {
		return Attempt{}, false, err
	}
	if !closesRecovered(a, hold) {
		return a, false, nil
	}
	if s.beforeRecoveredClose != nil {
		s.beforeRecoveredClose()
	}
	key, err := newID(s.now())
	if err != nil {
		return a, false, err
	}
	in := recoveredInput{AttemptID: a.ID, ReservationID: a.ReservationID, Fence: a.Fence,
		ClosingFence: hold.ClosingFence, ClosedBy: hold.ClosedBy, ClosedAt: hold.ClosedAt}
	var out Attempt
	err = s.run(ctx, c, command{
		op: opCloseRecovered, scope: a.ID, key: key, input: in, authorize: requireReconciler,
		check: func(ctx context.Context, tx *sql.Tx) error {
			cur, err := getAttempt(ctx, tx, a.ID)
			if err != nil {
				return err
			}
			if !closesRecovered(cur, hold) {
				return errSettled
			}
			return nil
		},
		apply: func(ctx context.Context, tx *sql.Tx, now time.Time) (any, error) {
			if err := updateAttempt(ctx, tx, a.ID, now,
				`state = 'closed', close_reason = 'recovered', reservation_id = '', fence = ?,
				 recovered_by = ?, recovered_fence = ?, recovered_at = ?`,
				hold.ClosingFence, hold.ClosedBy, hold.ClosingFence, hold.ClosedAt); err != nil {
				return nil, err
			}
			if err := endProofs(ctx, tx, a.ID, now); err != nil {
				return nil, err
			}
			return getAttempt(ctx, tx, a.ID)
		},
	}, &out)
	if err == errSettled {
		cur, gerr := s.GetAttempt(ctx, a.ID)
		return cur, false, gerr
	}
	if err != nil {
		return a, false, err
	}
	return out, true, nil
}

// closesRecovered is the closure rule: an open attempt with no step
// pending, and the scope's "closed" for exactly its reservation with a
// closing fence past its confirmed fence. A "none", a "held", another
// reservation's closure or an unchanged fence never closes it.
func closesRecovered(a Attempt, hold ScopeHold) bool {
	if a.State == AttemptClosed || a.PendingKey != "" || a.ReservationID == "" {
		return false
	}
	if a.State != AttemptOffered && a.State != AttemptRunning {
		return false
	}
	if hold.State != ScopeClosed || hold.ReservationID != a.ReservationID {
		return false
	}
	switch hold.ClosedBy {
	case "holder_release", "holder_finalize", "recovery_release", "recovery_cancel":
	default:
		return false
	}
	closing, err := strconv.ParseUint(hold.ClosingFence, 10, 63)
	if err != nil {
		return false
	}
	fence, err := strconv.ParseUint(a.Fence, 10, 63)
	return err == nil && closing > fence
}
