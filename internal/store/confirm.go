package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// Confirmed delivery (crew-execution b1b, seq179). Aicrew never queries a
// forge: a team member confirms that an accepted result was delivered, and
// aicrew records the evidence that member gathered and enforces the rule.
// Finalizing as DONE needs the confirmed delivery of the accepted result,
// and its terminal evidence is the confirmed record's, never what the
// finalizer sends. Evidence a worker supplies without confirmation unlocks
// nothing.
//
// Who may confirm is one predicate, deliveryConfirmer: today the team's
// current coordinator, who is not the attempt's worker. A dedicated verifier
// role would change only that predicate.

// ErrDeliveryUnconfirmed refuses finalizing a result whose delivery no team
// member has confirmed.
var ErrDeliveryUnconfirmed = errors.New("delivery_unconfirmed")

// maxDeliveryEvidence is the most references a finalize may carry to aimem.
const maxDeliveryEvidence = 16

// deliveryCleared forgets a confirmed delivery; it is part of
// acceptanceCleared, so rework or a stop voids the confirmation with the
// acceptance.
const deliveryCleared = `delivery_result = 0, delivery_evidence = '', delivery_by_agent = '', delivery_by_session = '',
	delivery_by_generation = 0, delivery_at = ''`

const opConfirmDelivery = "attempt.confirm_delivery"

// DeliveryConfirmation is a team member's confirmation that the accepted
// result ResultSeq was delivered, with the evidence it gathered.
type DeliveryConfirmation struct {
	SessionID  string     `json:"session_id"`
	Generation int64      `json:"generation"`
	ResultSeq  int64      `json:"result_seq"`
	Evidence   []Evidence `json:"evidence"`
}

// deliveryConfirmer checks, in the command's transaction, that the caller's
// session may confirm the delivery of attempt a: the team's current
// coordinator, who is not the attempt's worker.
func deliveryConfirmer(ctx context.Context, tx *sql.Tx, c Caller, sessionID string, generation int64, a Attempt) (Session, error) {
	sess, err := currentCoordinator(ctx, tx, c, sessionID, generation)
	if err != nil {
		return Session{}, err
	}
	if a.TeamID != sess.TeamID {
		return Session{}, fmt.Errorf("attempt %s: %w", a.ID, ErrNotFound)
	}
	if a.WorkerAgentID == sess.AgentID {
		return Session{}, fmt.Errorf("%w: the worker of attempt %s cannot confirm the delivery of its own result", ErrForbidden, a.ID)
	}
	return sess, nil
}

// ConfirmDelivery records the caller's confirmation that the accepted result
// was delivered. A confirmation replaces an earlier one for the same result
// while no finalize is in flight.
func (s *Store) ConfirmDelivery(ctx context.Context, c Caller, key, attemptID string, in DeliveryConfirmation) (Attempt, error) {
	return s.localStep(ctx, c, confirmDeliveryCommand(c, key, attemptID, in), attemptID)
}

func confirmDeliveryCommand(c Caller, key, attemptID string, in DeliveryConfirmation) command {
	return command{
		op: opConfirmDelivery, scope: attemptID, key: key, input: struct {
			AttemptID string `json:"attempt_id"`
			DeliveryConfirmation
		}{attemptID, in},
		authorize: requireAgent, replayCheck: sessionCurrent(c, in.SessionID, in.Generation),
		validate: func() error {
			if len(in.Evidence) > maxDeliveryEvidence {
				return fmt.Errorf("%w: at most %d delivery references", ErrInvalid, maxDeliveryEvidence)
			}
			return TrustedDelivery{Required: DevelopmentDelivery, Evidence: in.Evidence}.check()
		},
		check: func(ctx context.Context, tx *sql.Tx) error {
			_, err := currentSession(ctx, tx, c, in.SessionID, in.Generation)
			return err
		},
		apply: func(ctx context.Context, tx *sql.Tx, now time.Time) (any, error) {
			a, err := getAttempt(ctx, tx, attemptID)
			if err != nil {
				return nil, err
			}
			sess, err := deliveryConfirmer(ctx, tx, c, in.SessionID, in.Generation, a)
			if err != nil {
				return nil, err
			}
			if err := workable(a); err != nil {
				return nil, err
			}
			if a.Phase != PhaseAccepted || in.ResultSeq != a.AcceptedResult {
				return nil, fmt.Errorf("attempt %s: result %d is not the accepted result: %w", a.ID, in.ResultSeq, ErrAttemptState)
			}
			evidence, err := json.Marshal(in.Evidence)
			if err != nil {
				return nil, fmt.Errorf("encode delivery evidence: %w", err)
			}
			if err := updateAttempt(ctx, tx, a.ID, now,
				`delivery_result = ?, delivery_evidence = ?, delivery_by_agent = ?, delivery_by_session = ?,
				 delivery_by_generation = ?, delivery_at = ?`,
				in.ResultSeq, string(evidence), sess.AgentID, sess.ID, sess.Generation, formatTime(now)); err != nil {
				return nil, err
			}
			if err := announce(ctx, tx, a, sess.AgentID, "%s confirmed the delivery of result %d of task %s.",
				now, labelOf(sess.AgentID), in.ResultSeq, taskName(a.Task)); err != nil {
				return nil, err
			}
			return getAttempt(ctx, tx, a.ID)
		},
	}
}
