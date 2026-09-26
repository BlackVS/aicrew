package store

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// Independent claim (docs/CREW-CONTRACT.md, "Independent claim"). An
// independent member of a team claims a task in one of the team's projects
// for itself. The claim is an attempt like an offered one, with no
// coordinator: the intent records it as claiming and takes the member's one
// execution capacity, then aimem is asked to claim the task with an
// external holder that names this exact attempt, under the member's own
// verified context. A committed claim runs the attempt; from there the work,
// review, finalize and stop rules are those of any running attempt. A final
// refusal, or a call that was not committed, closes the attempt and frees the
// capacity; any other outcome reconciles and keeps both.

// ClaimRequest claims a task for the caller. ExpectedRevision, Process and
// the task reference come from a trusted internal caller that read them from
// aimem; InstructionDigest is the digest of the instructions the claimer
// verified, which must be the pinned process's.
type ClaimRequest struct {
	SessionID         string         `json:"session_id"`
	Generation        int64          `json:"generation"`
	Task              TaskRef        `json:"task"`
	ExpectedRevision  int64          `json:"expected_revision"`
	BaseCommit        string         `json:"base_commit"`
	Branch            string         `json:"branch"`
	Process           TrustedProcess `json:"process"`
	InstructionDigest string         `json:"instruction_digest"`
}

const opClaimTask = "attempt.claim"

func (r ClaimRequest) validate() error {
	if r.SessionID == "" || r.Generation < 1 || r.ExpectedRevision < 1 {
		return fmt.Errorf("%w: a claim needs a session, generation and expected task revision", ErrInvalid)
	}
	if !validRefs(r.Task.HubID, r.Task.ProjectID, r.Task.TaskID, r.BaseCommit, r.Branch) {
		return fmt.Errorf("%w: a claim needs a task, a base commit and a branch", ErrInvalid)
	}
	if !r.Process.valid() || !validRefs(r.InstructionDigest) {
		return fmt.Errorf("%w: a claim needs the process pin and the digest of the verified instructions", ErrInvalid)
	}
	return nil
}

// ClaimTask claims a task for the caller, an independent member of the
// session's team. The member's capacity is taken with the intent, before
// aimem is asked to claim the task for the attempt.
func (s *Store) ClaimTask(ctx context.Context, c Caller, port Reservations, key string, in ClaimRequest) (Attempt, error) {
	cmd := command{
		op: opClaimTask, scope: in.SessionID, key: key, input: in,
		authorize: requireAgent, validate: in.validate,
		replayCheck: sessionCurrent(c, in.SessionID, in.Generation),
	}
	release, err := s.flights.acquire(ctx, c, cmd, s.flightWait)
	if err != nil {
		return Attempt{}, err
	}
	defer release()
	cmd.check = func(ctx context.Context, tx *sql.Tx) error {
		sess, err := currentSession(ctx, tx, c, in.SessionID, in.Generation)
		if err != nil {
			return err
		}
		if sess.Role != RoleIndependent {
			return fmt.Errorf("%w: only an independent member claims tasks; a %s receives them by offer", ErrForbidden, sess.Role)
		}
		return nil
	}
	cmd.apply = func(ctx context.Context, tx *sql.Tx, now time.Time) (any, error) {
		sess, err := getSession(ctx, tx, in.SessionID)
		if err != nil {
			return nil, err
		}
		if err := requireTeamProject(ctx, tx, sess.TeamID, ProjectRef{HubID: in.Task.HubID, ProjectID: in.Task.ProjectID}); err != nil {
			return nil, err
		}
		if in.InstructionDigest != in.Process.InstructionDigest {
			return nil, fmt.Errorf("claim of task %s: %w", taskName(in.Task), ErrProcessMismatch)
		}
		if busy, err := openWork(ctx, tx, c.id, ""); err != nil {
			return nil, err
		} else if busy {
			return nil, fmt.Errorf("agent %s: %w", c.id, ErrAgentBusy)
		}
		id, err := newID(now)
		if err != nil {
			return nil, err
		}
		at := formatTime(now)
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO attempts (id, team_id, task_hub_id, task_project_id, task_id, worker_agent_id, origin,
			        coordinator_agent_id, coordinator_session_id, coordinator_generation, state,
			        base_commit, branch, process_repository, process_commit, process_manifest, instruction_digest,
			        offer_expires_at, task_revision, pending_op, pending_key, pending_from, intents,
			        worker_session_id, worker_generation, revision, created_at, updated_at)
			 VALUES (?, ?, ?, ?, ?, ?, 'claim', NULL, '', 0, 'claiming', ?, ?, ?, ?, ?, ?, '', ?, ?, ?, 'closed', 1,
			         ?, ?, 1, ?, ?)`,
			id, sess.TeamID, in.Task.HubID, in.Task.ProjectID, in.Task.TaskID, c.id,
			in.BaseCommit, in.Branch, in.Process.Identity.Repository, in.Process.Identity.Commit,
			in.Process.Identity.Manifest, in.Process.InstructionDigest,
			in.ExpectedRevision, string(ReservationClaim), requestKey(id, ReservationClaim, 1),
			sess.ID, sess.Generation, at, at); err != nil {
			return nil, fmt.Errorf("insert attempt: %w", err)
		}
		return getAttempt(ctx, tx, id)
	}
	return s.transition(ctx, c, port, cmd, "")
}

// applyClaimRun applies a committed independent claim: the claimer holds the
// task itself and starts work.
func applyClaimRun(ctx context.Context, tx *sql.Tx, a Attempt, r ReservationResult, now time.Time) error {
	if err := updateAttempt(ctx, tx, a.ID, now,
		`state = 'running', phase = 'working', reservation_id = ?, fence = ?, task_revision = ?, last_receipt_id = ?, `+
			pendingColumnsCleared, r.Reservation.ID, r.Reservation.Fence, r.TaskRevision, r.Receipt.ID); err != nil {
		return err
	}
	return announce(ctx, tx, a, a.WorkerAgentID, "%s claimed task %s and started work.",
		now, labelOf(a.WorkerAgentID), taskName(a.Task))
}
