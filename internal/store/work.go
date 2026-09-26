package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// The work lifecycle of a running attempt (docs/CREW-CONTRACT.md,
// "Lifecycle"). A running attempt holds the worker's capacity; its phase
// tracks the work under that hold:
//
//	working -> blocked -> working            (fenced update: block, resume)
//	working -> submitted                     (fenced update: submit a result)
//	submitted/accepted -> rework -> working  (local review, then resume)
//	submitted/accepted -> accepted           (local review of the exact result)
//	accepted -> finalized, attempt closed    (finalize DONE with delivery evidence)
//
// Steps that touch the reservation follow the attempts.go ordering: intent,
// port call, settle, and reconciliation by receipt. Reviews are local only;
// accepting a result is not delivery. Results are an append-only history,
// and an acceptance names one result: rework clears it and a new result
// needs its own review.
//
// Integration adapter obligation (crew-execution b): aicrew sends only the
// fields it owns (ReservationRequest.Owned). The adapter reads aimem's
// current task, keeps every other field as it is, and submits the complete
// content with the expected revision and the reservation fence. A revision
// conflict is refused, never overwritten; the attempt then reconciles and
// refreshes its revision before a new request, and a pending request is
// never changed.

type AttemptPhase string

const (
	PhaseWorking   AttemptPhase = "working"
	PhaseBlocked   AttemptPhase = "blocked"
	PhaseSubmitted AttemptPhase = "submitted"
	PhaseRework    AttemptPhase = "rework"
	PhaseAccepted  AttemptPhase = "accepted"
	PhaseFinalized AttemptPhase = "finalized"
)

// WorkIntent is the purpose of a fenced update.
type WorkIntent string

const (
	IntentBlock  WorkIntent = "block"
	IntentResume WorkIntent = "resume"
	IntentSubmit WorkIntent = "submit"
)

// ReviewDecision is a coordinator's decision on a submitted result.
type ReviewDecision string

const (
	ReviewAccept ReviewDecision = "accept"
	ReviewRework ReviewDecision = "rework"
)

// WorkUpdate is the worker's update. Detail is the blocker for block and the
// result reference for submit.
type WorkUpdate struct {
	SessionID  string     `json:"session_id"`
	Generation int64      `json:"generation"`
	Intent     WorkIntent `json:"intent"`
	Detail     string     `json:"detail,omitempty"`
}

// ResultReview is the coordinator's decision on one submitted result.
type ResultReview struct {
	SessionID  string         `json:"session_id"`
	Generation int64          `json:"generation"`
	ResultSeq  int64          `json:"result_seq"`
	Decision   ReviewDecision `json:"decision"`
}

// FinalizeRequest finalizes the accepted result as DONE. Delivery comes from
// a trusted internal caller (delivery.go).
type FinalizeRequest struct {
	SessionID  string          `json:"session_id"`
	Generation int64           `json:"generation"`
	ResultSeq  int64           `json:"result_seq"`
	Delivery   TrustedDelivery `json:"delivery"`
}

// AttemptResult is one submitted result. Results are never changed.
type AttemptResult struct {
	Seq         int64     `json:"seq"`
	ResultRef   string    `json:"result_ref"`
	RequestKey  string    `json:"request_key"`
	ReceiptID   string    `json:"receipt_id"`
	SubmittedAt time.Time `json:"submitted_at"`
}

const (
	opUpdateWork   = "attempt.update"
	opReviewResult = "attempt.review"
	opFinalizeWork = "attempt.finalize"
)

// UpdateWork blocks, resumes or submits the caller's running attempt as its
// holder.
func (s *Store) UpdateWork(ctx context.Context, c Caller, port Reservations, key, attemptID string, in WorkUpdate) (Attempt, error) {
	cmd := command{
		op: opUpdateWork, scope: attemptID, key: key, input: struct {
			AttemptID string `json:"attempt_id"`
			WorkUpdate
		}{attemptID, in},
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
			if err := workable(a); err != nil {
				return nil, err
			}
			allowed := map[WorkIntent][]AttemptPhase{
				IntentBlock:  {PhaseWorking},
				IntentResume: {PhaseBlocked, PhaseRework},
				IntentSubmit: {PhaseWorking},
			}[in.Intent]
			if allowed == nil {
				return nil, fmt.Errorf("%w: unknown work intent %q", ErrInvalid, in.Intent)
			}
			if !phaseIn(a.Phase, allowed) {
				return nil, fmt.Errorf("attempt %s is %s: %w", a.ID, a.Phase, ErrAttemptState)
			}
			detail := ""
			switch in.Intent {
			case IntentBlock:
				if err := validateMessageText(in.Detail); err != nil {
					return nil, fmt.Errorf("%w: a block needs a blocker", ErrInvalid)
				}
				detail = in.Detail
			case IntentSubmit:
				if !validRefs(in.Detail) {
					return nil, fmt.Errorf("%w: a submit needs a result reference", ErrInvalid)
				}
				detail = in.Detail
			}
			text, err := workMessage(ctx, tx, a, in.Intent, detail)
			if err != nil {
				return nil, err
			}
			if err := validateMessageText(text); err != nil {
				return nil, fmt.Errorf("%w: the %s detail is too long for its team message", ErrInvalid, in.Intent)
			}
			return startWorkIntent(ctx, tx, a, ReservationUpdate, AttemptRunning, string(in.Intent), detail, "", text, now)
		},
	}
	return s.transition(ctx, c, port, cmd, attemptID)
}

// ReviewResult records the current coordinator's decision on the latest
// submitted result. It is local: accepting a result is not delivery. The
// acceptance names the result and the reviewing session and generation.
func (s *Store) ReviewResult(ctx context.Context, c Caller, key, attemptID string, in ResultReview) (Attempt, error) {
	var out Attempt
	cmd := command{
		op: opReviewResult, scope: attemptID, key: key, input: struct {
			AttemptID string `json:"attempt_id"`
			ResultReview
		}{attemptID, in},
		authorize: requireAgent, replayCheck: sessionCurrent(c, in.SessionID, in.Generation),
		check: func(ctx context.Context, tx *sql.Tx) error {
			_, err := currentCoordinator(ctx, tx, c, in.SessionID, in.Generation)
			return err
		},
		apply: func(ctx context.Context, tx *sql.Tx, now time.Time) (any, error) {
			sess, err := getSession(ctx, tx, in.SessionID)
			if err != nil {
				return nil, err
			}
			a, err := getAttempt(ctx, tx, attemptID)
			if err != nil {
				return nil, err
			}
			if a.TeamID != sess.TeamID {
				return nil, fmt.Errorf("attempt %s: %w", attemptID, ErrNotFound)
			}
			if err := workable(a); err != nil {
				return nil, err
			}
			if !phaseIn(a.Phase, []AttemptPhase{PhaseSubmitted, PhaseAccepted}) {
				return nil, fmt.Errorf("attempt %s is %s: %w", a.ID, a.Phase, ErrAttemptState)
			}
			if err := requireLatestResult(ctx, tx, a, in.ResultSeq); err != nil {
				return nil, err
			}
			switch in.Decision {
			case ReviewAccept:
				err = updateAttempt(ctx, tx, a.ID, now,
					`phase = 'accepted', accepted_result = ?, accepted_by_session = ?, accepted_by_generation = ?`,
					in.ResultSeq, sess.ID, sess.Generation)
				if err == nil {
					err = announce(ctx, tx, a, sess.AgentID, "%s accepted result %d of task %s.",
						now, labelOf(sess.AgentID), in.ResultSeq, taskName(a.Task))
				}
			case ReviewRework:
				err = updateAttempt(ctx, tx, a.ID, now,
					`phase = 'rework', `+acceptanceCleared)
				if err == nil {
					err = announce(ctx, tx, a, sess.AgentID, "%s returned result %d of task %s for rework.",
						now, labelOf(sess.AgentID), in.ResultSeq, taskName(a.Task))
				}
			default:
				return nil, fmt.Errorf("%w: unknown review decision %q", ErrInvalid, in.Decision)
			}
			if err != nil {
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

// FinalizeWork finalizes the accepted result as DONE. The holder may
// finalize its own accepted result. A coordinator may finalize only from the
// session and generation that recorded the acceptance: a successor, or the
// same coordinator after a resume, reviews the result itself first.
func (s *Store) FinalizeWork(ctx context.Context, c Caller, port Reservations, key, attemptID string, in FinalizeRequest) (Attempt, error) {
	cmd := command{
		op: opFinalizeWork, scope: attemptID, key: key, input: struct {
			AttemptID string `json:"attempt_id"`
			FinalizeRequest
		}{attemptID, in},
		authorize: requireAgent, replayCheck: sessionCurrent(c, in.SessionID, in.Generation),
		check: func(ctx context.Context, tx *sql.Tx) error {
			_, err := currentSession(ctx, tx, c, in.SessionID, in.Generation)
			return err
		},
		apply: func(ctx context.Context, tx *sql.Tx, now time.Time) (any, error) {
			sess, err := getSession(ctx, tx, in.SessionID)
			if err != nil {
				return nil, err
			}
			a, err := getAttempt(ctx, tx, attemptID)
			if err != nil {
				return nil, err
			}
			if a.TeamID != sess.TeamID {
				return nil, fmt.Errorf("attempt %s: %w", attemptID, ErrNotFound)
			}
			if err := workable(a); err != nil {
				return nil, err
			}
			if a.Phase != PhaseAccepted {
				return nil, fmt.Errorf("attempt %s is %s: %w", a.ID, a.Phase, ErrAttemptState)
			}
			// An acceptance always names the latest result: accepting needs
			// the latest result, rework clears the acceptance, and a new
			// result can only follow rework. So this binds the finalize to
			// the exact submitted result.
			if in.ResultSeq != a.AcceptedResult {
				return nil, fmt.Errorf("attempt %s: result %d is not the accepted result: %w", a.ID, in.ResultSeq, ErrAttemptState)
			}
			switch {
			case a.WorkerAgentID == c.id:
				// The holder finalizes its own accepted result.
			case sess.Role == RoleCoordinator:
				if sess.ID != a.AcceptedBySession || sess.Generation != a.AcceptedByGeneration {
					return nil, fmt.Errorf("%w: this coordinator session has not reviewed the accepted result of attempt %s",
						ErrForbidden, a.ID)
				}
			default:
				return nil, fmt.Errorf("%w: only the holder or the reviewing coordinator may finalize attempt %s",
					ErrForbidden, a.ID)
			}
			if err := in.Delivery.check(); err != nil {
				return nil, err
			}
			evidence, err := json.Marshal(in.Delivery.Evidence)
			if err != nil {
				return nil, fmt.Errorf("encode delivery evidence: %w", err)
			}
			return startWorkIntent(ctx, tx, a, ReservationFinalize, AttemptRunning, "", "", string(evidence), "", now)
		},
	}
	return s.transition(ctx, c, port, cmd, attemptID)
}

// AttemptResults lists an attempt's submitted results in order.
func (s *Store) AttemptResults(ctx context.Context, attemptID string) ([]AttemptResult, error) {
	var out []AttemptResult
	err := s.snapshot(ctx, func(q querier) error {
		rows, err := q.QueryContext(ctx,
			`SELECT seq, result_ref, request_key, receipt_id, submitted_at FROM attempt_results
			 WHERE attempt_id = ? ORDER BY seq`, attemptID)
		if err != nil {
			return fmt.Errorf("read results: %w", err)
		}
		defer rows.Close()
		out = []AttemptResult{}
		for rows.Next() {
			var (
				r  AttemptResult
				at string
			)
			if err := rows.Scan(&r.Seq, &r.ResultRef, &r.RequestKey, &r.ReceiptID, &at); err != nil {
				return err
			}
			if r.SubmittedAt, err = parseTime(at); err != nil {
				return err
			}
			out = append(out, r)
		}
		return rows.Err()
	})
	return out, err
}

// idleRunning requires a running attempt with no step in flight.
func idleRunning(a Attempt) error {
	if a.State != AttemptRunning || a.PendingKey != "" {
		return fmt.Errorf("attempt %s is %s: %w", a.ID, a.State, ErrAttemptState)
	}
	return nil
}

// workable requires an idle running attempt that is not being stopped: a
// requested stop refuses every further work step.
func workable(a Attempt) error {
	if err := idleRunning(a); err != nil {
		return err
	}
	if a.Stop != StopNone {
		return fmt.Errorf("attempt %s: a stop is %s: %w", a.ID, a.Stop, ErrAttemptState)
	}
	return nil
}

// acceptanceCleared forgets a recorded acceptance; acceptanceVoided also
// returns an accepted result to submitted.
const (
	acceptanceCleared = `accepted_result = 0, accepted_by_session = '', accepted_by_generation = 0`
	acceptanceVoided  = `phase = CASE phase WHEN 'accepted' THEN 'submitted' ELSE phase END, ` + acceptanceCleared
)

func phaseIn(p AttemptPhase, allowed []AttemptPhase) bool {
	for _, a := range allowed {
		if p == a {
			return true
		}
	}
	return false
}

// requireLatestResult requires seq to name the attempt's latest result.
func requireLatestResult(ctx context.Context, q querier, a Attempt, seq int64) error {
	latest, err := latestResult(ctx, q, a.ID)
	if err != nil {
		return err
	}
	if seq < 1 || seq != latest {
		return fmt.Errorf("attempt %s: result %d is not the latest result (%d): %w", a.ID, seq, latest, ErrAttemptState)
	}
	return nil
}

func latestResult(ctx context.Context, q querier, attemptID string) (int64, error) {
	var n int64
	if err := q.QueryRowContext(ctx,
		`SELECT COALESCE(MAX(seq), 0) FROM attempt_results WHERE attempt_id = ?`, attemptID).Scan(&n); err != nil {
		return 0, fmt.Errorf("read latest result: %w", err)
	}
	return n, nil
}

// startWorkIntent records a pending step on a running attempt with its
// intent, detail, delivery evidence and checked team message.
func startWorkIntent(ctx context.Context, tx *sql.Tx, a Attempt, op ReservationOp, state AttemptState,
	intent, detail, evidence, message string, now time.Time) (Attempt, error) {
	if _, err := startIntent(ctx, tx, a, op, state, now); err != nil {
		return Attempt{}, err
	}
	if err := updateAttempt(ctx, tx, a.ID, now,
		`pending_intent = ?, pending_detail = ?, pending_evidence = ?, pending_message = ?`,
		intent, detail, evidence, message); err != nil {
		return Attempt{}, err
	}
	return getAttempt(ctx, tx, a.ID)
}

// taskStateFor is the aimem task state a work step leads to.
func taskStateFor(op ReservationOp, intent WorkIntent) string {
	switch {
	case op == ReservationFinalize:
		return "DONE"
	case intent == IntentBlock:
		return "BLOCKED"
	case intent == IntentSubmit:
		return "REVIEW"
	default:
		return "IN_PROGRESS"
	}
}

// applyWorkOutcome applies a committed update or finalize.
func applyWorkOutcome(ctx context.Context, tx *sql.Tx, a Attempt, r ReservationResult, now time.Time) error {
	const clearPending = pendingColumnsCleared
	if a.PendingOp == ReservationFinalize {
		if err := updateAttempt(ctx, tx, a.ID, now,
			`state = 'closed', close_reason = 'finalized', phase = 'finalized', finalized_result = accepted_result,
			 terminal_evidence = pending_evidence, reservation_id = '', fence = ?, task_revision = ?, last_receipt_id = ?, `+clearPending,
			r.Reservation.Fence, r.TaskRevision, r.Receipt.ID); err != nil {
			return err
		}
		return announce(ctx, tx, a, "", "Task %s was finalized as done with result %d.",
			now, taskName(a.Task), a.AcceptedResult)
	}
	phase := map[WorkIntent]AttemptPhase{
		IntentBlock: PhaseBlocked, IntentResume: PhaseWorking, IntentSubmit: PhaseSubmitted,
	}[WorkIntent(a.PendingIntent)]
	if phase == "" {
		return fmt.Errorf("attempt %s: unknown pending intent %q", a.ID, a.PendingIntent)
	}
	if WorkIntent(a.PendingIntent) == IntentSubmit {
		seq, err := latestResult(ctx, tx, a.ID)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO attempt_results (attempt_id, seq, result_ref, request_key, receipt_id, submitted_at)
			 VALUES (?, ?, ?, ?, ?, ?)`,
			a.ID, seq+1, a.PendingDetail, a.PendingKey, r.Receipt.ID, formatTime(now)); err != nil {
			return fmt.Errorf("record result: %w", err)
		}
	}
	// The step may be settled from a reconciliation, so the running state
	// is set here, not assumed.
	if err := updateAttempt(ctx, tx, a.ID, now,
		`state = 'running', phase = ?, fence = ?, task_revision = ?, last_receipt_id = ?, `+clearPending,
		string(phase), r.Reservation.Fence, r.TaskRevision, r.Receipt.ID); err != nil {
		return err
	}
	// The message was built and checked when the step was recorded, and is
	// posted as it was then, so a later rename cannot make it too long.
	task := a.Task
	_, err := postLifecycle(ctx, tx, a.TeamID, a.WorkerAgentID, a.PendingMessage, &task, now)
	return err
}

// workMessage is the lifecycle message a committed work update posts. The
// update's intent builds and checks it, and records it with the step, so a
// step aimem commits can always be settled.
func workMessage(ctx context.Context, q querier, a Attempt, intent WorkIntent, detail string) (string, error) {
	worker, err := getAgent(ctx, q, a.WorkerAgentID)
	if err != nil {
		return "", err
	}
	switch intent {
	case IntentBlock:
		return fmt.Sprintf("%s blocked task %s: %s", worker.Label, taskName(a.Task), detail), nil
	case IntentSubmit:
		return fmt.Sprintf("%s submitted a result for task %s: %s", worker.Label, taskName(a.Task), detail), nil
	default:
		return fmt.Sprintf("%s resumed work on task %s.", worker.Label, taskName(a.Task)), nil
	}
}

// refreshRevision records a newer task revision seen in the caller's hold
// status, only while no step is pending, so a later request is sent with
// the revision aimem now has. A pending request is never changed.
func (s *Store) refreshRevision(ctx context.Context, c Caller, a Attempt, revision int64) (Attempt, error) {
	key, err := newID(s.now())
	if err != nil {
		return a, err
	}
	var out Attempt
	err = s.run(ctx, c, command{
		op: "attempt.refresh_revision", scope: a.ID, key: key, authorize: anyCaller,
		input: struct {
			AttemptID string `json:"attempt_id"`
			Revision  int64  `json:"revision"`
		}{a.ID, revision},
		check: func(ctx context.Context, tx *sql.Tx) error {
			cur, err := getAttempt(ctx, tx, a.ID)
			if err != nil {
				return err
			}
			if cur.PendingKey != "" || cur.TaskRevision >= revision {
				return errSettled
			}
			return nil
		},
		apply: func(ctx context.Context, tx *sql.Tx, now time.Time) (any, error) {
			if err := updateAttempt(ctx, tx, a.ID, now, `task_revision = ?`, revision); err != nil {
				return nil, err
			}
			return getAttempt(ctx, tx, a.ID)
		},
	}, &out)
	if errors.Is(err, errSettled) {
		return s.GetAttempt(ctx, a.ID)
	}
	return out, err
}
