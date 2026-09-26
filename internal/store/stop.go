package store

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// The stop of a running attempt (docs/CREW-CONTRACT.md, "Lifecycle"). The
// team's current coordinator or the operator requests it, the worker
// confirms it, and the worker, as holder, releases the task to READY, or to
// BLOCKED with a blocker:
//
//	running -> stop requested        (local: coordinator or operator)
//	stop requested -> stopped        (local: the worker confirms)
//	stopped -> closed                (release: intent, port call, settle)
//
// A request and a confirmation send nothing to aimem: the attempt keeps its
// hold and the worker's capacity until aimem commits the release. Nothing
// but the worker's own confirmation confirms a stop: not elapsed time, a
// lost connection, a coordinator change, or the worker's session being
// resumed or replaced. A stop cannot be withdrawn, and there is no
// cancellation shortcut around it.
//
// While a stop is requested no work update, review or finalize is accepted.
// A step already in flight still settles or reconciles, and settling it
// never clears the stop: the stop lives apart from the work phase. A
// requested stop voids a recorded acceptance, unless a finalize of it is in
// flight; if that finalize is not committed, the acceptance is voided then.

// StopState is how far a running attempt's stop has gone.
type StopState string

const (
	StopNone      StopState = ""
	StopRequested StopState = "requested"
	StopConfirmed StopState = "confirmed"
)

// ReleaseTarget is the task state a stopped attempt releases the task to.
type ReleaseTarget string

const (
	ReleaseReady   ReleaseTarget = "READY"
	ReleaseBlocked ReleaseTarget = "BLOCKED"
)

// StopRequest asks for the stop of a running attempt. A coordinator names
// its session and generation; the operator leaves both empty.
type StopRequest struct {
	SessionID  string `json:"session_id,omitempty"`
	Generation int64  `json:"generation,omitempty"`
	Reason     string `json:"reason"`
}

// StopRelease releases a stopped attempt's task as its holder.
type StopRelease struct {
	SessionID  string        `json:"session_id"`
	Generation int64         `json:"generation"`
	Target     ReleaseTarget `json:"target"`
	Blocker    string        `json:"blocker,omitempty"`
}

const (
	opRequestStop    = "attempt.stop"
	opConfirmStop    = "attempt.confirm_stop"
	opReleaseStopped = "attempt.release_stopped"
)

// stoppablePhases are the work phases a stop may be requested in.
var stoppablePhases = []AttemptPhase{PhaseWorking, PhaseBlocked, PhaseSubmitted, PhaseRework, PhaseAccepted}

// RequestStop records a stop request for a running attempt. The caller must
// be the operator or the team's current coordinator, which may be a
// successor of the one that made the offer. A step in flight does not
// prevent the request.
func (s *Store) RequestStop(ctx context.Context, c Caller, key, attemptID string, in StopRequest) (Attempt, error) {
	var out Attempt
	byOperator := requireOperator(c) == nil
	cmd := command{
		op: opRequestStop, scope: attemptID, key: key, input: struct {
			AttemptID string `json:"attempt_id"`
			StopRequest
		}{attemptID, in},
		authorize: func(c Caller) error {
			if byOperator {
				return nil
			}
			return requireAgent(c)
		},
		validate: func() error {
			if byOperator && (in.SessionID != "" || in.Generation != 0) {
				return fmt.Errorf("%w: the operator requests a stop without a session", ErrInvalid)
			}
			if err := validateMessageText(in.Reason); err != nil {
				return fmt.Errorf("%w: a stop needs a reason", ErrInvalid)
			}
			return nil
		},
		check: func(ctx context.Context, tx *sql.Tx) error {
			if byOperator {
				return nil
			}
			_, err := currentCoordinator(ctx, tx, c, in.SessionID, in.Generation)
			return err
		},
		apply: func(ctx context.Context, tx *sql.Tx, now time.Time) (any, error) {
			a, err := getAttempt(ctx, tx, attemptID)
			if err != nil {
				return nil, err
			}
			who, actorID := "The operator", ""
			if !byOperator {
				sess, err := getSession(ctx, tx, in.SessionID)
				if err != nil {
					return nil, err
				}
				if a.TeamID != sess.TeamID {
					return nil, fmt.Errorf("attempt %s: %w", attemptID, ErrNotFound)
				}
				// A worker never requests its own stop, even after becoming
				// the team's coordinator; the operator still may.
				if a.WorkerAgentID == sess.AgentID {
					return nil, fmt.Errorf("%w: the worker of attempt %s cannot request its own stop", ErrForbidden, a.ID)
				}
				if who, err = agentLabel(ctx, tx, sess.AgentID); err != nil {
					return nil, err
				}
				actorID = sess.AgentID
			}
			if a.State == AttemptClosed || a.Stop != StopNone || !phaseIn(a.Phase, stoppablePhases) {
				return nil, fmt.Errorf("attempt %s is %s (%s): %w", a.ID, a.State, a.Phase, ErrAttemptState)
			}
			worker, err := agentLabel(ctx, tx, a.WorkerAgentID)
			if err != nil {
				return nil, err
			}
			// A reason too long for its message is refused when the message
			// is posted, in this transaction.
			text := fmt.Sprintf("%s requested a stop of task %s for %s: %s", who, taskName(a.Task), worker, in.Reason)
			set := `stop = 'requested', stop_by = ?, stop_session = ?, stop_reason = ?, stop_at = ?`
			if a.PendingOp != ReservationFinalize {
				set += `, ` + acceptanceVoided
			}
			if err := updateAttempt(ctx, tx, a.ID, now, set, c.String(), in.SessionID, in.Reason, formatTime(now)); err != nil {
				return nil, err
			}
			task := a.Task
			if _, err := postLifecycle(ctx, tx, a.TeamID, actorID, text, &task, now); err != nil {
				return nil, err
			}
			return getAttempt(ctx, tx, a.ID)
		},
	}
	if !byOperator {
		cmd.replayCheck = sessionCurrent(c, in.SessionID, in.Generation)
	}
	// The request is one local transaction and does not wait for a
	// reservation call in flight on the attempt (the attempt's step lock),
	// so a stop can always be recorded. The settle of that call reads the
	// stop inside its own transaction (applyOutcome).
	return out, s.run(ctx, c, cmd, &out)
}

// ConfirmStop records the worker's confirmation that it stopped work on the
// attempt. Only the attempt's worker confirms, from its current session,
// with no step in flight. The hold and the capacity stay.
func (s *Store) ConfirmStop(ctx context.Context, c Caller, key, attemptID, sessionID string, generation int64) (Attempt, error) {
	var out Attempt
	cmd := command{
		op: opConfirmStop, scope: attemptID, key: key,
		input:     attemptAt{AttemptID: attemptID, SessionID: sessionID, Generation: generation},
		authorize: requireAgent, replayCheck: sessionCurrent(c, sessionID, generation),
		check: func(ctx context.Context, tx *sql.Tx) error {
			_, err := currentSession(ctx, tx, c, sessionID, generation)
			return err
		},
		apply: func(ctx context.Context, tx *sql.Tx, now time.Time) (any, error) {
			a, err := attemptForWorker(ctx, tx, c, attemptID, sessionID)
			if err != nil {
				return nil, err
			}
			if err := idleRunning(a); err != nil {
				return nil, err
			}
			if a.Stop != StopRequested {
				return nil, fmt.Errorf("attempt %s: no stop is awaiting confirmation: %w", a.ID, ErrAttemptState)
			}
			if err := updateAttempt(ctx, tx, a.ID, now, `stop = 'confirmed'`); err != nil {
				return nil, err
			}
			if err := announce(ctx, tx, a, a.WorkerAgentID, "%s confirmed the stop of task %s.",
				now, labelOf(a.WorkerAgentID), taskName(a.Task)); err != nil {
				return nil, err
			}
			return getAttempt(ctx, tx, a.ID)
		},
	}
	unlock, err := s.lockAttempt(ctx, attemptID)
	if err != nil {
		return out, err
	}
	defer unlock()
	return out, s.run(ctx, c, cmd, &out)
}

func (r StopRelease) validate() error {
	switch r.Target {
	case ReleaseReady:
		if r.Blocker != "" {
			return fmt.Errorf("%w: a release to READY has no blocker", ErrInvalid)
		}
	case ReleaseBlocked:
		if err := validateMessageText(r.Blocker); err != nil {
			return fmt.Errorf("%w: a release to BLOCKED needs a blocker", ErrInvalid)
		}
	default:
		return fmt.Errorf("%w: a stopped attempt releases its task to READY or BLOCKED, not %q", ErrInvalid, r.Target)
	}
	return nil
}

// ReleaseStopped releases the task of a stopped attempt as its holder. The
// attempt closes and the capacity is freed only when aimem commits the
// release; any other outcome keeps both.
func (s *Store) ReleaseStopped(ctx context.Context, c Caller, port Reservations, key, attemptID string, in StopRelease) (Attempt, error) {
	cmd := command{
		op: opReleaseStopped, scope: attemptID, key: key, input: struct {
			AttemptID string `json:"attempt_id"`
			StopRelease
		}{attemptID, in},
		authorize: requireAgent, validate: in.validate,
		replayCheck: sessionCurrent(c, in.SessionID, in.Generation),
		check: func(ctx context.Context, tx *sql.Tx) error {
			_, err := currentSession(ctx, tx, c, in.SessionID, in.Generation)
			return err
		},
		apply: func(ctx context.Context, tx *sql.Tx, now time.Time) (any, error) {
			a, err := attemptForWorker(ctx, tx, c, attemptID, in.SessionID)
			if err != nil {
				return nil, err
			}
			if err := idleRunning(a); err != nil {
				return nil, err
			}
			if a.Stop != StopConfirmed {
				return nil, fmt.Errorf("attempt %s: the worker has not confirmed a stop: %w", a.ID, ErrAttemptState)
			}
			worker, err := agentLabel(ctx, tx, a.WorkerAgentID)
			if err != nil {
				return nil, err
			}
			text := fmt.Sprintf("%s released stopped task %s to READY.", worker, taskName(a.Task))
			if in.Target == ReleaseBlocked {
				text = fmt.Sprintf("%s released stopped task %s as BLOCKED: %s", worker, taskName(a.Task), in.Blocker)
			}
			if err := validateMessageText(text); err != nil {
				return nil, fmt.Errorf("%w: the blocker is too long for its team message", ErrInvalid)
			}
			return startWorkIntent(ctx, tx, a, ReservationRelease, AttemptReleasing, string(in.Target), in.Blocker, "", text, now)
		},
	}
	return s.transition(ctx, c, port, cmd, attemptID)
}

// applyStopRelease applies a committed release of a stopped attempt: the
// attempt closes, the capacity is freed and the message checked with the
// step is posted.
func applyStopRelease(ctx context.Context, tx *sql.Tx, a Attempt, r ReservationResult, now time.Time) error {
	if err := updateAttempt(ctx, tx, a.ID, now,
		`state = 'closed', close_reason = 'stopped', reservation_id = '', fence = ?, task_revision = ?, last_receipt_id = ?, `+
			pendingColumnsCleared, r.Reservation.Fence, r.TaskRevision, r.Receipt.ID); err != nil {
		return err
	}
	task := a.Task
	_, err := postLifecycle(ctx, tx, a.TeamID, a.WorkerAgentID, a.PendingMessage, &task, now)
	return err
}

func agentLabel(ctx context.Context, q querier, agentID string) (string, error) {
	ag, err := getAgent(ctx, q, agentID)
	if err != nil {
		return "", err
	}
	return ag.Label, nil
}
