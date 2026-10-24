package store

import (
	"context"
	"fmt"
	"time"
)

// OpenAttempt is one attempt of the team that is not closed, as the team's
// coordinator reconciles it at the start of its session (pilot finding
// 2026-10-11): an offer it may have to withdraw, or an attempt still running.
type OpenAttempt struct {
	ID            string       `json:"id"`
	State         AttemptState `json:"state"`
	Phase         AttemptPhase `json:"phase,omitempty"`
	Task          TaskRef      `json:"task"`
	WorkerAgentID string       `json:"worker_agent_id"`
	WorkerLabel   string       `json:"worker_label"`
	// CoordinatorGeneration is the coordinator generation an offer was made
	// under; a claimed attempt has none.
	CoordinatorGeneration int64     `json:"coordinator_generation,omitempty"`
	OfferExpiresAt        time.Time `json:"offer_expires_at,omitzero"`
	// Stale, Expired and Declined say why an offer can no longer be
	// accepted: made under an earlier coordinator generation, past its
	// expiry, or declined by its worker.
	Stale    bool `json:"stale"`
	Expired  bool `json:"expired"`
	Declined bool `json:"declined"`
	// NextAction is what the coordinator does about the attempt now.
	NextAction string    `json:"next_action"`
	CreatedAt  time.Time `json:"created_at"`
	// HandoffUpdatedAt is when the worker's launcher last pushed the
	// attempt's handoff; zero when it has none.
	HandoffUpdatedAt time.Time `json:"handoff_updated_at,omitzero"`
}

// OpenAttempts is the team's open attempts and its current coordinator
// generation.
type OpenAttempts struct {
	CoordinatorGeneration int64         `json:"coordinator_generation"`
	Attempts              []OpenAttempt `json:"attempts"`
}

// The next actions an open attempt names.
const (
	nextWithdrawReoffer = "Withdraw the offer, then offer the task again if it is still to be done."
	nextWaitAccept      = "Wait for the worker to accept or decline; withdraw the offer if it is no longer wanted."
	nextFollowRunning   = "Follow the running attempt: review a submitted result, or answer a block."
	nextSettlePending   = "A step is pending on this attempt. The member that began it finishes it with `aicrew-agent step recover`: run that if it is yours, otherwise wait for it."
)

// OpenAttemptsWithToken lists the open attempts of the token's team, oldest
// first. Only the team's coordinator may: it is the one that withdraws,
// re-offers and reviews them.
func (s *Store) OpenAttemptsWithToken(ctx context.Context, token string) (OpenAttempts, error) {
	var out OpenAttempts
	err := s.snapshot(ctx, func(q querier) error {
		now := s.now()
		b, err := tokenBinding(ctx, q, token, now)
		if err != nil {
			return err
		}
		if b.Role != RoleCoordinator {
			return fmt.Errorf("%w: only the team's coordinator lists its open attempts", ErrForbidden)
		}
		team, err := getTeam(ctx, q, b.TeamID)
		if err != nil {
			return err
		}
		rows, err := q.QueryContext(ctx,
			`SELECT id FROM attempts WHERE team_id = ? AND state != 'closed' ORDER BY created_at, id`, b.TeamID)
		if err != nil {
			return fmt.Errorf("list open attempts: %w", err)
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
		out = OpenAttempts{CoordinatorGeneration: team.CoordinatorGeneration, Attempts: []OpenAttempt{}}
		for _, id := range ids {
			a, err := getAttempt(ctx, q, id)
			if err != nil {
				return err
			}
			worker, err := getAgent(ctx, q, a.WorkerAgentID)
			if err != nil {
				return err
			}
			o := openAttempt(a, worker.Label, team.CoordinatorGeneration, now)
			if o.HandoffUpdatedAt, err = handoffTime(ctx, q, a.ID); err != nil {
				return err
			}
			out.Attempts = append(out.Attempts, o)
		}
		return nil
	})
	return out, err
}

func openAttempt(a Attempt, workerLabel string, coordGen int64, now time.Time) OpenAttempt {
	o := OpenAttempt{ID: a.ID, State: a.State, Phase: a.Phase, Task: a.Task, WorkerAgentID: a.WorkerAgentID,
		WorkerLabel: workerLabel, CoordinatorGeneration: a.CoordinatorGeneration, OfferExpiresAt: a.OfferExpiresAt,
		Declined: a.Declined, CreatedAt: a.CreatedAt}
	switch a.State {
	case AttemptOffered:
		o.Stale = a.CoordinatorGeneration != coordGen
		o.Expired = !now.Before(a.OfferExpiresAt)
		o.NextAction = nextWaitAccept
		if o.Stale || o.Expired || o.Declined {
			o.NextAction = nextWithdrawReoffer
		}
	case AttemptRunning:
		o.NextAction = nextFollowRunning
	default:
		o.NextAction = nextSettlePending
	}
	return o
}
