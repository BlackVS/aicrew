package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// Stale offers (pilot finding 2026-10-11). An offer is bound to the
// coordinator generation it was made under and to the worker's session
// context (acceptable, attempts.go). Once either moves on, the offer can
// never be accepted, and only the team's coordinator can release it. aicrewd
// tells the coordinator so, with a lifecycle message that wakes it:
//   - when a coordinator session starts or resumes, for each offer made under
//     an earlier coordinator generation;
//   - when a worker's accept is refused offer_stale, once per attempt.

// staleOfferRemedy is what every stale-offer message tells the coordinator.
const staleOfferRemedy = "Withdraw it and offer the task again."

func staleOfferText(a Attempt, worker, why string) string {
	return fmt.Sprintf("The offer of task %s to %s (attempt %s) is stale: %s. %s",
		taskName(a.Task), worker, a.ID, why, staleOfferRemedy)
}

// staleOfferMark is the part of a stale-offer message that names its
// attempt; a message containing it means the coordinator was told.
func staleOfferMark(attemptID string) string { return "(attempt " + attemptID + ") is stale: " }

// postStaleOffer writes a stale-offer message about a to the team's
// coordinators.
func postStaleOffer(ctx context.Context, tx *sql.Tx, a Attempt, why string, now time.Time) error {
	recipients, err := activeCoordinators(ctx, tx, a.TeamID)
	if err != nil || len(recipients) == 0 {
		return err
	}
	worker, err := getAgent(ctx, tx, a.WorkerAgentID)
	if err != nil {
		return err
	}
	task := a.Task
	_, err = insertMessage(ctx, tx, Message{TeamID: a.TeamID, Kind: KindLifecycle, Task: &task, AttemptID: a.ID,
		Text: staleOfferText(a, worker.Label, why)}, recipients, now)
	return err
}

// noticeFencedOffers runs in the transaction that advanced the team's
// coordinator generation to gen by starting or resuming a coordinator
// session: every outstanding offer made under an earlier generation is
// stale now, and the coordinator is told about each.
func noticeFencedOffers(ctx context.Context, tx *sql.Tx, teamID string, gen int64, now time.Time) error {
	rows, err := tx.QueryContext(ctx,
		`SELECT id FROM attempts WHERE team_id = ? AND state = 'offered' AND coordinator_generation < ?
		 ORDER BY created_at, id`, teamID, gen)
	if err != nil {
		return fmt.Errorf("list fenced offers: %w", err)
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		ids = append(ids, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for _, id := range ids {
		a, err := getAttempt(ctx, tx, id)
		if err != nil {
			return err
		}
		if err := postStaleOffer(ctx, tx, a,
			"it was made under an earlier coordinator session and can no longer be accepted", now); err != nil {
			return err
		}
	}
	return nil
}

// noticeStaleAccept passes refused through, and when it is an accept refused
// offer_stale, tells the coordinator in a transaction of its own: the refusal
// records nothing. The message is written once per attempt, so a worker
// retrying the accept does not repeat it.
func (s *Store) noticeStaleAccept(ctx context.Context, attemptID string, refused error) error {
	if !errors.Is(refused, ErrOfferStale) {
		return refused
	}
	if err := s.postStaleAccept(ctx, attemptID); err != nil {
		return fmt.Errorf("%w (the coordinator could not be told: %v)", refused, err)
	}
	return refused
}

func (s *Store) postStaleAccept(ctx context.Context, attemptID string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck // no-op after commit
	a, err := getAttempt(ctx, tx, attemptID)
	if err != nil {
		return err
	}
	if a.State != AttemptOffered {
		return nil
	}
	var told int
	if err := tx.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM messages WHERE team_id = ? AND attempt_id = ? AND kind = 'lifecycle' AND instr(text, ?) > 0`,
		a.TeamID, a.ID, staleOfferMark(a.ID)).Scan(&told); err != nil {
		return fmt.Errorf("find stale-offer message: %w", err)
	}
	if told > 0 {
		return nil
	}
	if err := postStaleOffer(ctx, tx, a, "its worker's accept was refused offer_stale", s.now()); err != nil {
		return err
	}
	return tx.Commit()
}
