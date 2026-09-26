package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// Execution attempts, offer path (docs/CREW-CONTRACT.md, "Execution
// capacity" and "Attempts and the aimem reservation").
//
// An attempt is aicrew's record of one task offered to one worker. Aimem
// decides who holds the task; the attempt follows its receipts. Every step
// that touches the reservation goes in three stages:
//
//  1. A keyed local command records the intent: the pending operation, a
//     stable request key and, for an offer, the worker's capacity.
//  2. The reservation port is called with no transaction open.
//  3. A settle transaction applies the outcome with its audit record and,
//     for a committed step, a lifecycle message.
//
// Only a committed receipt or a refusal that is not retryable is a known
// outcome. Anything else (a transport error, a timeout, a retryable refusal,
// an unresolved receipt) leaves the attempt reconciling: it keeps the
// worker's capacity and no further transition is sent until a receipt
// lookup with the same key resolves it. A timeout or a missing local record
// never releases or adopts a hold.
//
// Steps on one attempt, including reconciliation, run one at a time within
// the Store (inflight.go), so a reconciliation never races a call in flight.

var (
	// ErrAgentBusy refuses an offer to a worker whose one execution
	// capacity is taken by an open attempt in any team.
	ErrAgentBusy = errors.New("agent_busy")
	// ErrAttemptState refuses a transition the attempt's state does not
	// allow, including any transition while it is reconciling.
	ErrAttemptState = errors.New("attempt_state")
	// ErrOfferStale refuses accepting an offer after the team's coordinator
	// changed, or after the worker's session context changed since the offer
	// was issued; the current coordinator releases or re-issues it.
	ErrOfferStale = errors.New("offer_stale")
	// ErrOfferExpired refuses accepting an expired offer. Expiry releases
	// nothing by itself.
	ErrOfferExpired = errors.New("offer_expired")
	// ErrOfferDeclined refuses accepting an offer the worker declined.
	ErrOfferDeclined = errors.New("offer_declined")
	// ErrProcessChanged refuses accepting an offer whose recorded process
	// is no longer the selected one; the offer is reconciled or re-issued,
	// never silently re-pinned.
	ErrProcessChanged = errors.New("process_changed")
	// ErrProcessMismatch refuses accepting with instructions other than the
	// ones recorded on the offer.
	ErrProcessMismatch = errors.New("process_mismatch")
	// ErrOutcomeUnknown reports that a reservation call's outcome is not
	// known; the attempt is reconciling.
	ErrOutcomeUnknown = errors.New("reservation_outcome_unknown")
)

type AttemptState string

const (
	AttemptOffering    AttemptState = "offering"
	AttemptOffered     AttemptState = "offered"
	AttemptAccepting   AttemptState = "accepting"
	AttemptRunning     AttemptState = "running"
	AttemptReleasing   AttemptState = "releasing"
	AttemptReconciling AttemptState = "reconciling"
	AttemptClosed      AttemptState = "closed"
)

// ProcessIdentity names an immutable process version: its repository, the
// commit and the manifest within it.
type ProcessIdentity struct {
	Repository string `json:"repository"`
	Commit     string `json:"commit"`
	Manifest   string `json:"manifest"`
}

// TrustedProcess is a project's selected process as read from aimem by a
// trusted internal caller: its identity and the digest of the role
// instructions a worker receives. Only such a reader may construct one;
// nothing an external caller sends may populate it. Reading and verifying
// the authoritative selection arrives with crew-execution b.
type TrustedProcess struct {
	Identity          ProcessIdentity `json:"identity"`
	InstructionDigest string          `json:"instruction_digest"`
}

func (p TrustedProcess) valid() bool {
	return validRefs(p.Identity.Repository, p.Identity.Commit, p.Identity.Manifest, p.InstructionDigest)
}

// Attempt is one task offered to one worker.
type Attempt struct {
	ID                    string  `json:"id"`
	TeamID                string  `json:"team_id"`
	Task                  TaskRef `json:"task"`
	WorkerAgentID         string  `json:"worker_agent_id"`
	CoordinatorAgentID    string  `json:"coordinator_agent_id"`
	CoordinatorSessionID  string  `json:"coordinator_session_id"`
	CoordinatorGeneration int64   `json:"coordinator_generation"`
	// WorkerSessionID and WorkerGeneration are the worker's session context
	// when the offer was issued; empty if it had no active session then.
	WorkerSessionID  string `json:"worker_session_id,omitempty"`
	WorkerGeneration int64  `json:"worker_generation,omitempty"`
	// WorkerSessionFloor is the worker's latest session number in the team
	// when the offer was issued, or -1 if unknown (an offer from before it
	// was recorded). Only the next session may accept an offline offer.
	WorkerSessionFloor int64 `json:"worker_session_floor"`
	// The work lifecycle of a running attempt (work.go).
	Phase                AttemptPhase   `json:"phase,omitempty"`
	PendingIntent        string         `json:"pending_intent,omitempty"`
	PendingDetail        string         `json:"pending_detail,omitempty"`
	PendingEvidence      string         `json:"pending_evidence,omitempty"`
	AcceptedResult       int64          `json:"accepted_result,omitempty"`
	AcceptedBySession    string         `json:"accepted_by_session,omitempty"`
	AcceptedByGeneration int64          `json:"accepted_by_generation,omitempty"`
	FinalizedResult      int64          `json:"finalized_result,omitempty"`
	TerminalEvidence     string         `json:"terminal_evidence,omitempty"`
	State                AttemptState   `json:"state"`
	CloseReason          string         `json:"close_reason,omitempty"`
	Declined             bool           `json:"declined"`
	BaseCommit           string         `json:"base_commit"`
	Branch               string         `json:"branch"`
	Process              TrustedProcess `json:"process"`
	OfferExpiresAt       time.Time      `json:"offer_expires_at"`
	TaskRevision         int64          `json:"task_revision"`
	ReservationID        string         `json:"reservation_id,omitempty"`
	Fence                string         `json:"fence,omitempty"`
	LastReceiptID        string         `json:"last_receipt_id,omitempty"`
	LastRefusal          string         `json:"last_refusal,omitempty"`
	PendingOp            ReservationOp  `json:"pending_op,omitempty"`
	PendingKey           string         `json:"pending_key,omitempty"`
	PendingFrom          AttemptState   `json:"pending_from,omitempty"`
	Revision             int64          `json:"revision"`
	CreatedAt            time.Time      `json:"created_at"`
	UpdatedAt            time.Time      `json:"updated_at"`
}

// offerRef and attemptRef are the work references aimem records for the
// coordinator's offer hold and the worker's hold.
func (a Attempt) offerRef() string   { return "aicrew-offer-" + a.ID }
func (a Attempt) attemptRef() string { return "aicrew-attempt-" + a.ID }

// OfferRequest offers a task to a named worker. ExpectedRevision, Process
// and the task reference come from a trusted internal caller that read them
// from aimem; the store records them and does not re-read them.
type OfferRequest struct {
	SessionID        string         `json:"session_id"`
	Generation       int64          `json:"generation"`
	WorkerAgentID    string         `json:"worker_agent_id"`
	Task             TaskRef        `json:"task"`
	ExpectedRevision int64          `json:"expected_revision"`
	BaseCommit       string         `json:"base_commit"`
	Branch           string         `json:"branch"`
	Process          TrustedProcess `json:"process"`
	ExpiresAt        time.Time      `json:"expires_at"`
}

// AcceptRequest accepts an offer. Selected is the project's currently
// selected process, from a trusted internal caller. InstructionDigest is the
// digest of the instructions the worker verified. A match shows the worker
// has the recorded instructions; it does not show they were read or
// understood.
type AcceptRequest struct {
	SessionID         string         `json:"session_id"`
	Generation        int64          `json:"generation"`
	Selected          TrustedProcess `json:"selected"`
	InstructionDigest string         `json:"instruction_digest"`
}

type attemptAt struct {
	AttemptID  string `json:"attempt_id"`
	SessionID  string `json:"session_id"`
	Generation int64  `json:"generation"`
}

const (
	opOfferTask    = "attempt.offer"
	opAcceptOffer  = "attempt.accept"
	opDeclineOffer = "attempt.decline"
	opReleaseOffer = "attempt.release"
	opSettle       = "attempt.settle"

	maxOfferLifetime = 7 * 24 * time.Hour
)

func (r OfferRequest) validate() error {
	if r.SessionID == "" || r.Generation < 1 || r.WorkerAgentID == "" || r.ExpectedRevision < 1 {
		return fmt.Errorf("%w: an offer needs a session, generation, worker and expected task revision", ErrInvalid)
	}
	if !validRefs(r.Task.HubID, r.Task.ProjectID, r.Task.TaskID, r.BaseCommit, r.Branch) {
		return fmt.Errorf("%w: an offer needs a task, a base commit and a branch", ErrInvalid)
	}
	if !r.Process.valid() {
		return fmt.Errorf("%w: an offer needs the process repository, commit, manifest and instruction digest", ErrInvalid)
	}
	return nil
}

// OfferTask offers a task to a worker of the caller's team. The caller must
// be the team's current coordinator. The worker's capacity is taken with the
// intent, before aimem is asked to claim the task for the offer.
func (s *Store) OfferTask(ctx context.Context, c Caller, port Reservations, key string, in OfferRequest) (Attempt, error) {
	cmd := command{
		op: opOfferTask, scope: in.SessionID, key: key, input: in,
		authorize: requireAgent, validate: in.validate,
		replayCheck: sessionCurrent(c, in.SessionID, in.Generation),
	}
	release, err := s.flights.acquire(ctx, c, cmd, s.flightWait)
	if err != nil {
		return Attempt{}, err
	}
	defer release()
	cmd.check = func(ctx context.Context, tx *sql.Tx) error {
		_, err := currentCoordinator(ctx, tx, c, in.SessionID, in.Generation)
		return err
	}
	cmd.apply = func(ctx context.Context, tx *sql.Tx, now time.Time) (any, error) {
		sess, err := getSession(ctx, tx, in.SessionID)
		if err != nil {
			return nil, err
		}
		if !in.ExpiresAt.After(now) || in.ExpiresAt.After(now.Add(maxOfferLifetime)) {
			return nil, fmt.Errorf("%w: an offer must expire within %s from now", ErrInvalid, maxOfferLifetime)
		}
		if in.WorkerAgentID == sess.AgentID {
			return nil, fmt.Errorf("%w: a coordinator cannot offer a task to itself", ErrInvalid)
		}
		m, err := getMembership(ctx, tx, sess.TeamID, in.WorkerAgentID)
		if errors.Is(err, ErrNotFound) || (err == nil && m.Role != RoleWorker) {
			return nil, fmt.Errorf("%w: %s is not an active worker of the team", ErrInvalid, in.WorkerAgentID)
		} else if err != nil {
			return nil, err
		}
		if busy, err := openWork(ctx, tx, in.WorkerAgentID, ""); err != nil {
			return nil, err
		} else if busy {
			return nil, fmt.Errorf("agent %s: %w", in.WorkerAgentID, ErrAgentBusy)
		}
		var workerSession string
		var workerGeneration int64
		if ws, err := activeSession(ctx, tx, sess.TeamID, in.WorkerAgentID); err == nil {
			workerSession, workerGeneration = ws.ID, ws.Generation
		} else if !errors.Is(err, ErrNotFound) {
			return nil, err
		}
		floor, err := lastSessionOrdinal(ctx, tx, sess.TeamID, in.WorkerAgentID)
		if err != nil {
			return nil, err
		}
		id, err := newID(now)
		if err != nil {
			return nil, err
		}
		at := formatTime(now)
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO attempts (id, team_id, task_hub_id, task_project_id, task_id, worker_agent_id,
			        coordinator_agent_id, coordinator_session_id, coordinator_generation, state,
			        base_commit, branch, process_repository, process_commit, process_manifest, instruction_digest,
			        offer_expires_at, task_revision, pending_op, pending_key, pending_from, intents,
			        worker_session_id, worker_generation, worker_session_floor, revision, created_at, updated_at)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, 'offering', ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 'closed', 1, ?, ?, ?, 1, ?, ?)`,
			id, sess.TeamID, in.Task.HubID, in.Task.ProjectID, in.Task.TaskID, in.WorkerAgentID,
			sess.AgentID, sess.ID, sess.CoordinatorGeneration,
			in.BaseCommit, in.Branch, in.Process.Identity.Repository, in.Process.Identity.Commit,
			in.Process.Identity.Manifest, in.Process.InstructionDigest,
			formatTime(in.ExpiresAt), in.ExpectedRevision, string(ReservationClaim), requestKey(id, ReservationClaim, 1),
			workerSession, workerGeneration, floor, at, at); err != nil {
			return nil, fmt.Errorf("insert attempt: %w", err)
		}
		return getAttempt(ctx, tx, id)
	}
	return s.transition(ctx, c, port, cmd, "")
}

// AcceptOffer accepts an offer as its named worker. It checks the recorded
// process against the current selection and the worker's instructions, then
// asks aimem to transfer the hold from the offer to the worker's attempt.
// The worker starts work only once the transfer is confirmed (running).
func (s *Store) AcceptOffer(ctx context.Context, c Caller, port Reservations, key, attemptID string, in AcceptRequest) (Attempt, error) {
	cmd := command{
		op: opAcceptOffer, scope: attemptID, key: key, input: struct {
			AttemptID string `json:"attempt_id"`
			AcceptRequest
		}{attemptID, in},
		authorize:   requireAgent,
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
			if err := acceptable(ctx, tx, a, in.SessionID, now); err != nil {
				return nil, err
			}
			if in.Selected.Identity != a.Process.Identity || in.Selected.InstructionDigest != a.Process.InstructionDigest {
				return nil, fmt.Errorf("attempt %s: %w: reconcile or re-issue the offer", a.ID, ErrProcessChanged)
			}
			if in.InstructionDigest != a.Process.InstructionDigest {
				return nil, fmt.Errorf("attempt %s: %w", a.ID, ErrProcessMismatch)
			}
			return startIntent(ctx, tx, a, ReservationTransfer, AttemptAccepting, now)
		},
	}
	return s.transition(ctx, c, port, cmd, attemptID)
}

// DeclineOffer records the worker's decline. It is local only: the hold
// stays with the offer until the team's coordinator releases it.
func (s *Store) DeclineOffer(ctx context.Context, c Caller, key, attemptID, sessionID string, generation int64) (Attempt, error) {
	var out Attempt
	cmd := command{
		op: opDeclineOffer, scope: attemptID, key: key,
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
			if a.State != AttemptOffered || a.Declined {
				return nil, fmt.Errorf("attempt %s is %s: %w", a.ID, a.State, ErrAttemptState)
			}
			if err := updateAttempt(ctx, tx, a.ID, now, `declined = 1`); err != nil {
				return nil, err
			}
			if err := announce(ctx, tx, a, a.WorkerAgentID, "%s declined the offer of task %s.", now, labelOf(a.WorkerAgentID), taskName(a.Task)); err != nil {
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

// ReleaseOffer releases an offer that was not accepted: withdrawn by the
// coordinator, declined by the worker, or expired. The caller must be the
// team's current coordinator, which may be a successor of the one that made
// the offer.
func (s *Store) ReleaseOffer(ctx context.Context, c Caller, port Reservations, key, attemptID, sessionID string, generation int64) (Attempt, error) {
	cmd := command{
		op: opReleaseOffer, scope: attemptID, key: key,
		input:     attemptAt{AttemptID: attemptID, SessionID: sessionID, Generation: generation},
		authorize: requireAgent, replayCheck: sessionCurrent(c, sessionID, generation),
		check: func(ctx context.Context, tx *sql.Tx) error {
			_, err := currentCoordinator(ctx, tx, c, sessionID, generation)
			return err
		},
		apply: func(ctx context.Context, tx *sql.Tx, now time.Time) (any, error) {
			sess, err := getSession(ctx, tx, sessionID)
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
			if a.State != AttemptOffered {
				return nil, fmt.Errorf("attempt %s is %s: %w", a.ID, a.State, ErrAttemptState)
			}
			return startIntent(ctx, tx, a, ReservationRelease, AttemptReleasing, now)
		},
	}
	return s.transition(ctx, c, port, cmd, attemptID)
}

// GetAttempt reads an attempt by ID.
func (s *Store) GetAttempt(ctx context.Context, id string) (Attempt, error) {
	var a Attempt
	err := s.snapshot(ctx, func(q querier) error {
		var err error
		a, err = getAttempt(ctx, q, id)
		return err
	})
	return a, err
}

// transition runs a keyed intent command and then its reservation call. A
// retry of a recorded intent does not call aimem again: it reconciles with a
// receipt lookup for the same request key. attemptID is empty for an offer,
// whose attempt the intent creates.
func (s *Store) transition(ctx context.Context, c Caller, port Reservations, cmd command, attemptID string) (Attempt, error) {
	if port == nil {
		return Attempt{}, fmt.Errorf("%w: no reservation service", ErrInvalid)
	}
	if attemptID != "" {
		unlock, err := s.lockAttempt(ctx, attemptID)
		if err != nil {
			return Attempt{}, err
		}
		defer unlock()
	}
	retry, err := s.receiptExists(ctx, c, cmd)
	if err != nil {
		return Attempt{}, err
	}
	var intent Attempt
	if err := s.run(ctx, c, cmd, &intent); err != nil {
		return Attempt{}, err
	}
	if attemptID == "" {
		unlock, err := s.lockAttempt(ctx, intent.ID)
		if err != nil {
			return Attempt{}, err
		}
		defer unlock()
	}
	cur, err := s.GetAttempt(ctx, intent.ID)
	if err != nil {
		return Attempt{}, err
	}
	if cur.PendingKey != intent.PendingKey || cur.PendingKey == "" {
		// Settled already, by an earlier call or a reconciliation. Report
		// this command's own step, never the attempt's later state, so a
		// retried refused step is not mistaken for a success.
		return s.stepOutcome(ctx, cur, intent.PendingKey)
	}
	if retry {
		return s.reconcilePending(ctx, c, port, cur)
	}
	res, err := port.Mutate(ctx, cur.PendingOp, reservationRequest(cur))
	return s.settle(ctx, c, cur, classify(cur, res, err))
}

// ReconcileAttempt conforms an attempt to aimem. A pending step is resolved
// with a receipt lookup for its request key; an offered or running attempt
// is checked against the caller's hold status. The caller must be the
// operator or an active member of the attempt's team.
func (s *Store) ReconcileAttempt(ctx context.Context, c Caller, port Reservations, attemptID string) (Attempt, error) {
	if port == nil {
		return Attempt{}, fmt.Errorf("%w: no reservation service", ErrInvalid)
	}
	unlock, err := s.lockAttempt(ctx, attemptID)
	if err != nil {
		return Attempt{}, err
	}
	defer unlock()
	a, err := s.GetAttempt(ctx, attemptID)
	if err != nil {
		return Attempt{}, err
	}
	if err := s.mayReconcile(ctx, c, a); err != nil {
		return Attempt{}, err
	}
	switch {
	case a.PendingKey != "":
		return s.reconcilePending(ctx, c, port, a)
	case a.State == AttemptOffered || a.State == AttemptRunning:
		st, err := port.Status(ctx, a.Task)
		if err != nil {
			return a, fmt.Errorf("attempt %s: status: %w: %w", a.ID, ErrOutcomeUnknown, err)
		}
		ref := a.offerRef()
		if a.State == AttemptRunning {
			ref = a.attemptRef()
		}
		switch {
		case st.State == "held" && st.OwnWorkRef == ref && st.ReservationID == a.ReservationID:
			if st.TaskRevision > a.TaskRevision {
				return s.refreshRevision(ctx, c, a, st.TaskRevision)
			}
			return a, nil
		case st.State == "none":
			return s.settle(ctx, c, a, callOutcome{kind: outcomeRecovered})
		default:
			return a, fmt.Errorf("attempt %s: aimem shows a different hold; operator recovery is required: %w",
				a.ID, ErrOutcomeUnknown)
		}
	default:
		return a, nil
	}
}

// stepOutcome reports the recorded outcome of one settled step.
func (s *Store) stepOutcome(ctx context.Context, cur Attempt, key string) (Attempt, error) {
	var outcome, refusal string
	err := s.snapshot(ctx, func(q querier) error {
		return q.QueryRowContext(ctx, `SELECT outcome, refusal FROM attempt_steps WHERE request_key = ?`, key).
			Scan(&outcome, &refusal)
	})
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return cur, fmt.Errorf("attempt %s: step %s has no recorded outcome: %w", cur.ID, key, ErrOutcomeUnknown)
	case err != nil:
		return cur, fmt.Errorf("read step outcome: %w", err)
	case outcome == string(outcomeCommitted):
		return cur, nil
	default:
		return cur, fmt.Errorf("attempt %s: step %s was %s %s: %w", cur.ID, key, outcome, refusal, ErrAttemptState)
	}
}

func (s *Store) reconcilePending(ctx context.Context, c Caller, port Reservations, a Attempt) (Attempt, error) {
	lookup, err := port.Receipt(ctx, a.Task, a.PendingOp, a.PendingKey)
	switch {
	case err != nil:
		return s.settle(ctx, c, a, callOutcome{kind: outcomeUnknown, detail: err.Error()})
	case lookup.State == ReceiptCommitted && lookup.Result != nil:
		return s.settle(ctx, c, a, classify(a, *lookup.Result, nil))
	case lookup.State == ReceiptNotCommitted:
		return s.settle(ctx, c, a, callOutcome{kind: outcomeNotCommitted})
	default:
		return s.settle(ctx, c, a, callOutcome{kind: outcomeUnknown, detail: "receipt " + lookup.State})
	}
}

func (s *Store) mayReconcile(ctx context.Context, c Caller, a Attempt) error {
	if c.kind == callerOperator && c.id != "" {
		return nil
	}
	if err := requireAgent(c); err != nil {
		return err
	}
	return s.snapshot(ctx, func(q querier) error {
		if _, err := getMembership(ctx, q, a.TeamID, c.id); errors.Is(err, ErrNotFound) {
			return fmt.Errorf("%w: only the operator or a team member may reconcile attempt %s", ErrForbidden, a.ID)
		} else if err != nil {
			return err
		}
		return nil
	})
}

// lockAttempt serializes the steps on one attempt within the Store.
func (s *Store) lockAttempt(ctx context.Context, attemptID string) (func(), error) {
	return s.flights.acquire(ctx, Caller{}, command{op: "attempt", scope: attemptID, key: "steps"}, s.flightWait)
}

type outcomeKind string

const (
	outcomeCommitted    outcomeKind = "committed"
	outcomeRefused      outcomeKind = "refused"
	outcomeNotCommitted outcomeKind = "not_committed"
	outcomeUnknown      outcomeKind = "unknown"
	outcomeRecovered    outcomeKind = "recovered"
)

type callOutcome struct {
	kind    outcomeKind
	result  ReservationResult
	refusal *ReservationRefusal
	detail  string
}

// classify turns a reservation call's reply into a known or unknown outcome.
// A committed receipt counts only if it is for this attempt's pending
// request and names the hold this step expects.
func classify(a Attempt, res ReservationResult, err error) callOutcome {
	var refusal *ReservationRefusal
	switch {
	case errors.As(err, &refusal) && !refusal.Retryable:
		return callOutcome{kind: outcomeRefused, refusal: refusal}
	case err != nil:
		return callOutcome{kind: outcomeUnknown, detail: err.Error()}
	case res.Receipt.State != ReceiptCommitted || res.Receipt.RequestKey != a.PendingKey ||
		res.Receipt.Operation != string(a.PendingOp):
		return callOutcome{kind: outcomeUnknown, detail: "the receipt does not match the pending request"}
	}
	r := res.Reservation
	var ok bool
	switch a.PendingOp {
	case ReservationClaim:
		ok = r.Active && r.ID != "" && r.OwnWorkRef == a.offerRef()
	case ReservationTransfer:
		ok = r.Active && r.ID == a.ReservationID && r.OwnWorkRef == a.attemptRef()
	case ReservationRelease, ReservationFinalize:
		ok = !r.Active
	case ReservationUpdate:
		// An update keeps the hold and its fence.
		ok = r.Active && r.ID == a.ReservationID && r.OwnWorkRef == a.attemptRef() && r.Fence == a.Fence
	}
	if !ok || r.Fence == "" || res.TaskRevision < 1 {
		return callOutcome{kind: outcomeUnknown, detail: "the committed reservation does not match the pending request"}
	}
	return callOutcome{kind: outcomeCommitted, result: res}
}

func requestKey(attemptID string, op ReservationOp, n int64) string {
	return fmt.Sprintf("aicrew-%s-%s-%d", attemptID, op, n)
}

func reservationRequest(a Attempt) ReservationRequest {
	req := ReservationRequest{
		Task: a.Task, RequestKey: a.PendingKey, ExpectedRevision: a.TaskRevision,
		CoordinationProof: "aicrew-coordination-" + a.PendingKey,
	}
	switch a.PendingOp {
	case ReservationClaim:
		req.Holder = &ReservationHolder{Mode: "external", WorkRef: a.offerRef()}
	case ReservationTransfer:
		req.ReservationID, req.Fence = a.ReservationID, a.Fence
		req.Holder = &ReservationHolder{Mode: "external", WorkRef: a.attemptRef()}
	case ReservationRelease:
		req.ReservationID, req.Fence = a.ReservationID, a.Fence
		req.Reason = "withdrawn offer"
		if a.Declined {
			req.Reason = "declined offer"
		}
	case ReservationUpdate:
		// The holder updates its own hold; no coordination reference.
		req.ReservationID, req.Fence, req.Intent, req.CoordinationProof = a.ReservationID, a.Fence, a.PendingIntent, ""
		owned := &OwnedTaskFields{State: taskStateFor(a.PendingOp, WorkIntent(a.PendingIntent))}
		switch WorkIntent(a.PendingIntent) {
		case IntentBlock:
			owned.Blocker = a.PendingDetail
		case IntentSubmit:
			owned.ResultRef = a.PendingDetail
		}
		req.Owned = owned
	case ReservationFinalize:
		req.ReservationID, req.Fence, req.Reason = a.ReservationID, a.Fence, "reviewed delivery"
		var evidence []Evidence
		if err := json.Unmarshal([]byte(a.PendingEvidence), &evidence); err == nil {
			for _, e := range evidence {
				req.TerminalEvidence = append(req.TerminalEvidence, e.Ref)
			}
		}
		req.Owned = &OwnedTaskFields{State: taskStateFor(a.PendingOp, ""), Evidence: evidence}
	}
	return req
}

// startIntent records a pending reservation step on an attempt.
func startIntent(ctx context.Context, tx *sql.Tx, a Attempt, op ReservationOp, state AttemptState, now time.Time) (Attempt, error) {
	n, err := nextIntent(ctx, tx, a.ID)
	if err != nil {
		return Attempt{}, err
	}
	if err := updateAttempt(ctx, tx, a.ID, now,
		`state = ?, pending_op = ?, pending_key = ?, pending_from = ?, intents = ?`,
		string(state), string(op), requestKey(a.ID, op, n), string(a.State), n); err != nil {
		return Attempt{}, err
	}
	return getAttempt(ctx, tx, a.ID)
}

func nextIntent(ctx context.Context, tx *sql.Tx, id string) (int64, error) {
	var n int64
	if err := tx.QueryRowContext(ctx, `SELECT intents + 1 FROM attempts WHERE id = ?`, id).Scan(&n); err != nil {
		return 0, fmt.Errorf("read attempt intents: %w", err)
	}
	return n, nil
}

type settleInput struct {
	AttemptID  string      `json:"attempt_id"`
	PendingKey string      `json:"pending_key"`
	Outcome    outcomeKind `json:"outcome"`
	ReceiptID  string      `json:"receipt_id,omitempty"`
	Refusal    string      `json:"refusal,omitempty"`
	Detail     string      `json:"detail,omitempty"`
}

var errSettled = errors.New("already settled")

// settle applies an outcome to an attempt in one transaction with its audit
// record. It changes nothing if the attempt has moved on since the outcome's
// step was recorded.
func (s *Store) settle(ctx context.Context, c Caller, a Attempt, o callOutcome) (Attempt, error) {
	in := settleInput{AttemptID: a.ID, PendingKey: a.PendingKey, Outcome: o.kind, ReceiptID: o.result.Receipt.ID, Detail: o.detail}
	if o.refusal != nil {
		in.Refusal = o.refusal.Code
	}
	key, err := newID(s.now())
	if err != nil {
		return a, err
	}
	var out Attempt
	err = s.run(ctx, c, command{
		op: opSettle, scope: a.ID, key: key, input: in, authorize: anyCaller,
		check: func(ctx context.Context, tx *sql.Tx) error {
			cur, err := getAttempt(ctx, tx, a.ID)
			if err != nil {
				return err
			}
			if cur.PendingKey != a.PendingKey || cur.State != a.State {
				return errSettled
			}
			if o.kind == outcomeUnknown && cur.State == AttemptReconciling {
				return errSettled
			}
			return nil
		},
		apply: func(ctx context.Context, tx *sql.Tx, now time.Time) (any, error) {
			return applyOutcome(ctx, tx, a, o, now)
		},
	}, &out)
	if errors.Is(err, errSettled) {
		cur, gerr := s.GetAttempt(ctx, a.ID)
		if gerr != nil {
			return a, gerr
		}
		if cur.State == AttemptReconciling {
			return cur, fmt.Errorf("attempt %s: %w", a.ID, ErrOutcomeUnknown)
		}
		return cur, nil
	}
	if err != nil {
		return a, err
	}
	switch o.kind {
	case outcomeRefused:
		return out, fmt.Errorf("attempt %s: %w", a.ID, o.refusal)
	case outcomeUnknown:
		return out, fmt.Errorf("attempt %s: %w: %s", a.ID, ErrOutcomeUnknown, o.detail)
	}
	return out, nil
}

func applyOutcome(ctx context.Context, tx *sql.Tx, a Attempt, o callOutcome, now time.Time) (Attempt, error) {
	const clearPending = pendingColumnsCleared
	var err error
	switch o.kind {
	case outcomeUnknown:
		err = updateAttempt(ctx, tx, a.ID, now, `state = 'reconciling'`)
	case outcomeRecovered:
		err = updateAttempt(ctx, tx, a.ID, now, `state = 'closed', close_reason = 'recovered'`)
		if err == nil {
			err = announce(ctx, tx, a, "", "Aimem no longer holds task %s for %s, so the attempt was closed as recovered.",
				now, taskName(a.Task), labelOf(a.WorkerAgentID))
		}
	case outcomeRefused, outcomeNotCommitted:
		reason := "not_committed"
		if o.refusal != nil {
			reason = o.refusal.Code
		}
		if a.PendingOp == ReservationClaim {
			err = updateAttempt(ctx, tx, a.ID, now,
				`state = 'closed', close_reason = ?, last_refusal = ?, `+clearPending, "claim "+reason, reason)
		} else {
			err = updateAttempt(ctx, tx, a.ID, now,
				`state = ?, last_refusal = ?, `+clearPending, string(a.PendingFrom), reason)
		}
	case outcomeCommitted:
		r := o.result
		switch a.PendingOp {
		case ReservationClaim:
			err = updateAttempt(ctx, tx, a.ID, now,
				`state = 'offered', reservation_id = ?, fence = ?, task_revision = ?, last_receipt_id = ?, `+clearPending,
				r.Reservation.ID, r.Reservation.Fence, r.TaskRevision, r.Receipt.ID)
			if err == nil {
				err = announce(ctx, tx, a, a.CoordinatorAgentID, "%s offered task %s to %s.",
					now, labelOf(a.CoordinatorAgentID), taskName(a.Task), labelOf(a.WorkerAgentID))
			}
		case ReservationTransfer:
			err = updateAttempt(ctx, tx, a.ID, now,
				`state = 'running', phase = 'working', fence = ?, task_revision = ?, last_receipt_id = ?, `+clearPending,
				r.Reservation.Fence, r.TaskRevision, r.Receipt.ID)
			if err == nil {
				err = announce(ctx, tx, a, a.WorkerAgentID, "%s accepted task %s and started work.",
					now, labelOf(a.WorkerAgentID), taskName(a.Task))
			}
		case ReservationRelease:
			reason := "withdrawn"
			if a.Declined {
				reason = "declined"
			}
			err = updateAttempt(ctx, tx, a.ID, now,
				`state = 'closed', close_reason = ?, reservation_id = '', fence = ?, task_revision = ?, last_receipt_id = ?, `+clearPending,
				reason, r.Reservation.Fence, r.TaskRevision, r.Receipt.ID)
			if err == nil {
				err = announce(ctx, tx, a, "", "The offer of task %s to %s was released.",
					now, taskName(a.Task), labelOf(a.WorkerAgentID))
			}
		case ReservationUpdate, ReservationFinalize:
			err = applyWorkOutcome(ctx, tx, a, r, now)
		}
	}
	if err != nil {
		return Attempt{}, err
	}
	if err := recordStep(ctx, tx, a, o, now); err != nil {
		return Attempt{}, err
	}
	return getAttempt(ctx, tx, a.ID)
}

// pendingColumnsCleared resets every pending-step column.
const pendingColumnsCleared = `pending_op = '', pending_key = '', pending_from = '', pending_intent = '',
	pending_detail = '', pending_evidence = ''`

// recordStep keeps the known outcome of a pending step by its request key.
func recordStep(ctx context.Context, tx *sql.Tx, a Attempt, o callOutcome, now time.Time) error {
	if a.PendingKey == "" || (o.kind != outcomeCommitted && o.kind != outcomeRefused && o.kind != outcomeNotCommitted) {
		return nil
	}
	refusal := ""
	if o.refusal != nil {
		refusal = o.refusal.Code
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO attempt_steps (request_key, attempt_id, operation, outcome, refusal, receipt_id, settled_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?)`,
		a.PendingKey, a.ID, string(a.PendingOp), string(o.kind), refusal, o.result.Receipt.ID, formatTime(now)); err != nil {
		return fmt.Errorf("record step outcome: %w", err)
	}
	return nil
}

// label is a placeholder resolved to an agent's label inside the
// transaction, so lifecycle messages name members by label, not by ID.
type label string

func labelOf(agentID string) label { return label(agentID) }

func taskName(t TaskRef) string { return t.HubID + "/" + t.ProjectID + "/" + t.TaskID }

// announce writes a lifecycle message to the attempt's team.
func announce(ctx context.Context, tx *sql.Tx, a Attempt, actorID, format string, now time.Time, args ...any) error {
	for i, arg := range args {
		if l, ok := arg.(label); ok {
			ag, err := getAgent(ctx, tx, string(l))
			if err != nil {
				return err
			}
			args[i] = ag.Label
		}
	}
	task := a.Task
	_, err := postLifecycle(ctx, tx, a.TeamID, actorID, fmt.Sprintf(format, args...), &task, now)
	return err
}

func updateAttempt(ctx context.Context, tx *sql.Tx, id string, now time.Time, set string, args ...any) error {
	args = append(args, formatTime(now), id)
	res, err := tx.ExecContext(ctx,
		`UPDATE attempts SET `+set+`, revision = revision + 1, updated_at = ? WHERE id = ?`, args...)
	if err != nil {
		return fmt.Errorf("update attempt: %w", err)
	}
	if n, err := res.RowsAffected(); err != nil {
		return err
	} else if n != 1 {
		return fmt.Errorf("attempt %s: %w", id, ErrNotFound)
	}
	return nil
}

// currentCoordinator requires the caller's own active coordinator session at
// its current generation. That makes it the team's current coordinator: a
// team has at most one active coordinator session, and starting, resuming or
// ending one advances the session's and the team's coordinator generation
// together (session.go).
func currentCoordinator(ctx context.Context, q querier, c Caller, sessionID string, generation int64) (Session, error) {
	sess, err := currentSession(ctx, q, c, sessionID, generation)
	if err != nil {
		return Session{}, err
	}
	if sess.Role != RoleCoordinator {
		return Session{}, fmt.Errorf("%w: session %s is not the team's coordinator", ErrForbidden, sessionID)
	}
	return sess, nil
}

// attemptForWorker reads an attempt in the session's team whose worker is
// the caller.
func attemptForWorker(ctx context.Context, tx *sql.Tx, c Caller, attemptID, sessionID string) (Attempt, error) {
	sess, err := getSession(ctx, tx, sessionID)
	if err != nil {
		return Attempt{}, err
	}
	a, err := getAttempt(ctx, tx, attemptID)
	if err != nil {
		return Attempt{}, err
	}
	if a.TeamID != sess.TeamID || a.WorkerAgentID != c.id {
		return Attempt{}, fmt.Errorf("attempt %s: %w", attemptID, ErrNotFound)
	}
	return a, nil
}

// acceptable checks that an offer can still be accepted from the worker's
// session. The offer is bound to the worker's session context when it was
// issued: the same session at the same generation, or, if the worker had no
// active session then, the first session the worker started after the offer
// (the next in its session history), still at its first generation. Any
// later advance (resume, credential rotation) or a replacement session makes
// the offer stale for good; so does an offer whose eligible session is
// unknown.
func acceptable(ctx context.Context, tx *sql.Tx, a Attempt, sessionID string, now time.Time) error {
	switch {
	case a.State != AttemptOffered:
		return fmt.Errorf("attempt %s is %s: %w", a.ID, a.State, ErrAttemptState)
	case a.Declined:
		return fmt.Errorf("attempt %s: %w", a.ID, ErrOfferDeclined)
	case !now.Before(a.OfferExpiresAt):
		return fmt.Errorf("attempt %s: %w", a.ID, ErrOfferExpired)
	}
	team, err := getTeam(ctx, tx, a.TeamID)
	if err != nil {
		return err
	}
	if team.CoordinatorGeneration != a.CoordinatorGeneration {
		return fmt.Errorf("attempt %s: the coordinator changed: %w", a.ID, ErrOfferStale)
	}
	sess, err := getSession(ctx, tx, sessionID)
	if err != nil {
		return err
	}
	if a.WorkerSessionID != "" {
		if sess.ID != a.WorkerSessionID || sess.Generation != a.WorkerGeneration {
			return fmt.Errorf("attempt %s: the worker's session changed since the offer: %w", a.ID, ErrOfferStale)
		}
		return nil
	}
	// Session ordinals start at 1, so an unknown floor (-1) matches no
	// session: such an offer is refused until it is released and re-issued.
	ordinal, err := sessionOrdinal(ctx, tx, sess.ID)
	if err != nil {
		return err
	}
	if ordinal != a.WorkerSessionFloor+1 || sess.Generation != firstGeneration {
		return fmt.Errorf("attempt %s: only the worker's first session after the offer, at its first generation, may accept: %w",
			a.ID, ErrOfferStale)
	}
	return nil
}

// firstGeneration is a new session's generation (session.go).
const firstGeneration = 1

// openWork reports whether an agent has open work: an attempt it works on
// that is not closed, or, as coordinator, an offer not yet running. With a
// team ID it looks only at that team.
func openWork(ctx context.Context, q querier, agentID, teamID string) (bool, error) {
	query := `SELECT 1 FROM attempts WHERE state != 'closed'
		AND (worker_agent_id = ? OR (coordinator_agent_id = ? AND state != 'running'))`
	args := []any{agentID, agentID}
	if teamID != "" {
		query += ` AND team_id = ?`
		args = append(args, teamID)
	}
	var one int
	err := q.QueryRowContext(ctx, query+` LIMIT 1`, args...).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("check open work: %w", err)
	}
	return true, nil
}

const attemptColumns = `id, team_id, task_hub_id, task_project_id, task_id, worker_agent_id, coordinator_agent_id,
	coordinator_session_id, coordinator_generation, state, close_reason, declined, base_commit, branch,
	process_repository, process_commit, process_manifest, instruction_digest, offer_expires_at, task_revision,
	reservation_id, fence, last_receipt_id, last_refusal, pending_op, pending_key, pending_from,
	worker_session_id, worker_generation, worker_session_floor, phase, pending_intent, pending_detail,
	pending_evidence, accepted_result, accepted_by_session, accepted_by_generation, finalized_result,
	terminal_evidence, revision, created_at, updated_at`

func getAttempt(ctx context.Context, q querier, id string) (Attempt, error) {
	var (
		a                         Attempt
		state, op, from, phase    string
		declined                  int
		expires, created, updated string
	)
	err := q.QueryRowContext(ctx, `SELECT `+attemptColumns+` FROM attempts WHERE id = ?`, id).Scan(
		&a.ID, &a.TeamID, &a.Task.HubID, &a.Task.ProjectID, &a.Task.TaskID, &a.WorkerAgentID, &a.CoordinatorAgentID,
		&a.CoordinatorSessionID, &a.CoordinatorGeneration, &state, &a.CloseReason, &declined, &a.BaseCommit, &a.Branch,
		&a.Process.Identity.Repository, &a.Process.Identity.Commit, &a.Process.Identity.Manifest,
		&a.Process.InstructionDigest, &expires, &a.TaskRevision, &a.ReservationID, &a.Fence, &a.LastReceiptID,
		&a.LastRefusal, &op, &a.PendingKey, &from, &a.WorkerSessionID, &a.WorkerGeneration,
		&a.WorkerSessionFloor, &phase, &a.PendingIntent, &a.PendingDetail, &a.PendingEvidence,
		&a.AcceptedResult, &a.AcceptedBySession, &a.AcceptedByGeneration, &a.FinalizedResult,
		&a.TerminalEvidence, &a.Revision, &created, &updated)
	if errors.Is(err, sql.ErrNoRows) {
		return Attempt{}, fmt.Errorf("attempt %s: %w", id, ErrNotFound)
	}
	if err != nil {
		return Attempt{}, fmt.Errorf("read attempt: %w", err)
	}
	a.State, a.PendingOp, a.PendingFrom, a.Declined = AttemptState(state), ReservationOp(op), AttemptState(from), declined == 1
	a.Phase = AttemptPhase(phase)
	for _, t := range []struct {
		dst *time.Time
		src string
	}{{&a.OfferExpiresAt, expires}, {&a.CreatedAt, created}, {&a.UpdatedAt, updated}} {
		if *t.dst, err = parseTime(t.src); err != nil {
			return Attempt{}, err
		}
	}
	return a, nil
}
