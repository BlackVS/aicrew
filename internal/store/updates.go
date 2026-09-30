package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"time"
)

// Work updates as member-driven steps (crew-execution b1b-3). A work update
// (block, submit, resume) is a fenced update of the holder's own hold: it
// needs no coordination fact, so it has no proof, and nothing can void it.
// Its settle follows coordination.v1's proofless finality rule: a read-scope
// "none" for the update's key stays unresolved until the hold's fence or task
// revision is observed past the request's, and the receipt is read after that
// observation.
//
// An update aimem refused without moving the fence or revision (for example
// for authorization or validation) would never meet that rule. The holder
// may then supersede it (D-b1b-4 (a)): the same update under a new request
// key, at the same expected revision and fence, so aimem's revision check
// lets at most one of the keys commit. Every key is an alias of one step:
// settle checks them all and records the step's outcome under each.

const opSupersedeUpdate = "attempt.supersede_update"

// supersededKey reports whether key is a superseded alias of attempt's
// pending update step.
func (s *Store) supersededKey(ctx context.Context, attemptID, key string) (bool, error) {
	var n int
	err := s.snapshot(ctx, func(q querier) error {
		return q.QueryRowContext(ctx, `SELECT COUNT(*) FROM superseded_updates WHERE attempt_id = ? AND request_key = ?`,
			attemptID, key).Scan(&n)
	})
	if err != nil {
		return false, fmt.Errorf("read superseded keys: %w", err)
	}
	return n > 0, nil
}

// stepKeys lists the keys of a's pending update step: its own, then the
// ones it superseded, oldest first.
func (s *Store) stepKeys(ctx context.Context, a Attempt) ([]string, error) {
	keys := []string{a.PendingKey}
	err := s.snapshot(ctx, func(q querier) error {
		rows, err := q.QueryContext(ctx,
			`SELECT request_key FROM superseded_updates WHERE attempt_id = ? ORDER BY superseded_at, request_key`, a.ID)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var k string
			if err := rows.Scan(&k); err != nil {
				return err
			}
			keys = append(keys, k)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, fmt.Errorf("read superseded keys: %w", err)
	}
	return keys, nil
}

// fenceNumber reads a fence as its number; a malformed one is -1, never past
// anything.
func fenceNumber(f string) int64 {
	n, err := strconv.ParseInt(f, 10, 64)
	if err != nil {
		return -1
	}
	return n
}

// updateOvertaken reports whether the hold status shows a's update overtaken:
// its hold's fence or task revision past the request's.
func updateOvertaken(a Attempt, h ScopeHold) bool {
	switch {
	case h.ReservationID != a.ReservationID:
		return false
	case h.State == ScopeHeld:
		return fenceNumber(h.Fence) > fenceNumber(a.Fence) || h.TaskRevision > a.TaskRevision
	case h.State == ScopeClosed:
		return fenceNumber(h.ClosingFence) > fenceNumber(a.Fence)
	}
	return false
}

// settleUpdate settles a's pending update step by the proofless rule. The
// hold status is read first and the receipts after it, so a "none" is final
// only when it was read after the evidence that the update was overtaken.
func (s *Store) settleUpdate(ctx context.Context, c Caller, reader ReservationReader, a Attempt, g guard) (Attempt, Settlement, error) {
	pending := Settlement{RetryAfter: NoneFinalAfter}
	if reader == nil {
		return a, pending, nil
	}
	keys, err := s.stepKeys(ctx, a)
	if err != nil {
		return a, Settlement{}, err
	}
	hold, err := reader.HoldStatus(ctx, a.Task)
	if err != nil {
		return a, pending, fmt.Errorf("attempt %s: read scope: %w: %w", a.ID, ErrOutcomeUnknown, err)
	}
	overtaken := updateOvertaken(a, hold)
	finals, err := s.scanFinals(ctx, a.ID)
	if err != nil {
		return a, Settlement{}, err
	}
	for _, k := range keys {
		if finals[keyLookup(k)] {
			continue
		}
		look, err := reader.ReceiptByKey(ctx, a.Task, ReservationUpdate, requestKeyDigest(k))
		if err != nil {
			return a, pending, fmt.Errorf("attempt %s: read scope: %w: %w", a.ID, ErrOutcomeUnknown, err)
		}
		switch look.State {
		case ScopeCommitted:
			res, err := resultFromReceipt(a, look.Receipt, k)
			if err != nil {
				return a, pending, err
			}
			out, err := s.settleGuarded(ctx, c, a, callOutcome{kind: outcomeCommitted, result: res}, g)
			return out, Settlement{Settled: true, Outcome: string(outcomeCommitted)}, err
		case ScopeNone:
			// Read after the hold was seen past the request, this key's
			// "none" is final (none_finality's proofless rule).
			if overtaken {
				if err := s.markScanFinal(ctx, a.ID, keyLookup(k), s.now()); err != nil {
					return a, pending, err
				}
			}
		default:
			return a, pending, fmt.Errorf("attempt %s: read scope answered %q: %w", a.ID, look.State, ErrOutcomeUnknown)
		}
	}
	if !overtaken {
		return a, pending, nil
	}
	out, err := s.settleGuarded(ctx, c, a, callOutcome{kind: outcomeNotCommitted}, g)
	return out, Settlement{Settled: true, Outcome: string(outcomeNotCommitted)}, err
}

// MaxSupersededKeys bounds the keys an update step can supersede, so that
// settling it takes at most one hold read and MaxSupersededKeys+1 receipt
// reads: well within one read window of the reconciler (crew-execution b3b).
const MaxSupersededKeys = 8

// ErrSupersedeLimit refuses a supersede past MaxSupersededKeys.
var ErrSupersedeLimit = errors.New("supersede_limit")

// supersedeUpdateCommand gives the holder's pending update step a new request
// key. The update itself does not change: the same intent and detail, at the
// same expected revision and fence. The old key becomes an alias of the step.
func supersedeUpdateCommand(c Caller, key, attemptID string, in WorkUpdate, supersedes string) command {
	return command{
		op: opSupersedeUpdate, scope: attemptID, key: key, input: struct {
			AttemptID  string `json:"attempt_id"`
			Supersedes string `json:"supersedes"`
			WorkUpdate
		}{attemptID, supersedes, in},
		authorize: requireAgent, replayCheck: sessionCurrent(c, in.SessionID, in.Generation),
		check: func(ctx context.Context, tx *sql.Tx) error {
			_, err := currentSession(ctx, tx, c, in.SessionID, in.Generation)
			return err
		},
		apply: func(ctx context.Context, tx *sql.Tx, now time.Time) (any, error) {
			a, err := attemptForWorker(ctx, tx, c, attemptID, in.SessionID)
			if err != nil {
				return nil, err
			}
			if a.PendingOp != ReservationUpdate || a.PendingKey != supersedes {
				return nil, fmt.Errorf("attempt %s: %s is not its pending update: %w", a.ID, supersedes, ErrAttemptState)
			}
			if string(in.Intent) != a.PendingIntent || in.Detail != a.PendingDetail {
				return nil, fmt.Errorf("%w: a superseding update repeats the pending update's intent and detail", ErrInvalid)
			}
			var aliases int
			if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM superseded_updates WHERE attempt_id = ?`,
				a.ID).Scan(&aliases); err != nil {
				return nil, fmt.Errorf("count superseded keys: %w", err)
			}
			if aliases >= MaxSupersededKeys {
				return nil, fmt.Errorf("attempt %s: the update already superseded %d keys: %w", a.ID, aliases, ErrSupersedeLimit)
			}
			n, err := nextIntent(ctx, tx, a.ID)
			if err != nil {
				return nil, err
			}
			if _, err := tx.ExecContext(ctx,
				`INSERT INTO superseded_updates (request_key, attempt_id, superseded_at) VALUES (?, ?, ?)`,
				a.PendingKey, a.ID, formatTime(now)); err != nil {
				return nil, fmt.Errorf("record superseded key: %w", err)
			}
			if err := updateAttempt(ctx, tx, a.ID, now, `pending_key = ?, intents = ?`,
				requestKey(a.ID, ReservationUpdate, n), n); err != nil {
				return nil, err
			}
			return getAttempt(ctx, tx, a.ID)
		},
	}
}

// BeginWorkWithToken begins the holder's work update, as the token's session:
// block with a blocker, submit with a result reference, or resume. Its begin
// response carries the values the member sends aimem, and no proof.
func (s *Store) BeginWorkWithToken(ctx context.Context, key, token, attemptID string, intent WorkIntent,
	detail string) (Attempt, Step, error) {
	t, c, err := s.tokenCaller(ctx, token)
	if err != nil {
		return Attempt{}, Step{}, err
	}
	var proof string
	cmd, g := s.withToken(intentOnly(updateCommand(c, key, attemptID,
		WorkUpdate{SessionID: t.sessionID, Generation: t.generation, Intent: intent, Detail: detail})), token, t)
	return s.begin(ctx, c, cmd, attemptID, &proof, "", t.sessionID, t.generation, g)
}

// SupersedeWorkWithToken supersedes the holder's pending update step with a
// new request key, as the token's session. intent and detail must repeat the
// pending update's.
func (s *Store) SupersedeWorkWithToken(ctx context.Context, key, token, attemptID, supersedes string, intent WorkIntent,
	detail string) (Attempt, Step, error) {
	t, c, err := s.tokenCaller(ctx, token)
	if err != nil {
		return Attempt{}, Step{}, err
	}
	var proof string
	cmd, g := s.withToken(intentOnly(supersedeUpdateCommand(c, key, attemptID,
		WorkUpdate{SessionID: t.sessionID, Generation: t.generation, Intent: intent, Detail: detail}, supersedes)), token, t)
	return s.begin(ctx, c, cmd, attemptID, &proof, "", t.sessionID, t.generation, g)
}
