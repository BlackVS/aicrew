package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
)

// The pull requests that wait for the human merge (docs/DESIGN-CONTROL-PLANE.md,
// section 4.2; increment C3). An attempt waits for the merge when the
// coordinator reviewed and accepted its latest result and no delivery of
// that result is confirmed yet: the merge, a human click on the forge, is
// what the coordinator confirms next. The list is read from aicrewd's own
// records, with no forge call. The worker's last observation of the pull
// request (forge events), when there is one, gives its head and its gates
// as last seen.

// PendingMerge is one accepted result that waits for the human merge.
type PendingMerge struct {
	TeamID      string    `json:"team_id"`
	TeamName    string    `json:"team_name"`
	AttemptID   string    `json:"attempt_id"`
	Task        TaskRef   `json:"task"`
	WorkerID    string    `json:"worker_agent_id"`
	WorkerLabel string    `json:"worker_label"`
	ResultSeq   int64     `json:"result_seq"`
	ResultRef   string    `json:"result_ref"`
	SubmittedAt string    `json:"submitted_at"`
	Branch      string    `json:"branch"`
	Pull        *PullSeen `json:"pull,omitempty"`
}

// PullSeen is the pull request as the worker's launcher last observed it.
type PullSeen struct {
	Number     int64    `json:"number"`
	URL        string   `json:"url"`
	State      string   `json:"state"`
	HeadCommit string   `json:"head_commit"`
	Labels     []string `json:"labels"`
	Checks     string   `json:"checks"`
	Comments   int64    `json:"comments"`
	ObservedAt string   `json:"observed_at"`
}

// maxPendingMerges bounds the list.
const maxPendingMerges = 200

// PendingMerges lists the accepted results that wait for the human merge,
// of one team or of every team (teamID ""), oldest first. The operator and
// an architect credential may read it.
func (s *Store) PendingMerges(ctx context.Context, c Caller, teamID string) ([]PendingMerge, error) {
	if err := requireEscalationReader(c); err != nil {
		return nil, err
	}
	if len(teamID) > maxRefLen {
		return nil, fmt.Errorf("%w: the team's ID is too long", ErrInvalid)
	}
	out := []PendingMerge{}
	err := s.snapshot(ctx, func(q querier) error {
		if teamID != "" {
			if _, err := getTeam(ctx, q, teamID); err != nil {
				return err
			}
		}
		rows, err := q.QueryContext(ctx,
			`SELECT a.id, a.team_id, t.name, a.worker_agent_id, COALESCE(g.label, ''), a.accepted_result,
			        r.result_ref, r.submitted_at, a.branch
			 FROM attempts a
			 JOIN teams t ON t.id = a.team_id
			 JOIN attempt_results r ON r.attempt_id = a.id AND r.seq = a.accepted_result
			 LEFT JOIN agents g ON g.id = a.worker_agent_id
			 WHERE a.state = ? AND a.phase = ? AND a.accepted_result > 0 AND a.delivery_result != a.accepted_result
			   AND (? = '' OR a.team_id = ?)
			 ORDER BY r.submitted_at, a.id LIMIT ?`,
			string(AttemptRunning), string(PhaseAccepted), teamID, teamID, maxPendingMerges)
		if err != nil {
			return fmt.Errorf("list pending merges: %w", err)
		}
		for rows.Next() {
			var m PendingMerge
			if err := rows.Scan(&m.AttemptID, &m.TeamID, &m.TeamName, &m.WorkerID, &m.WorkerLabel, &m.ResultSeq,
				&m.ResultRef, &m.SubmittedAt, &m.Branch); err != nil {
				rows.Close()
				return err
			}
			out = append(out, m)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		for i := range out {
			a, err := getAttempt(ctx, q, out[i].AttemptID)
			if err != nil {
				return err
			}
			out[i].Task = a.Task
			if out[i].Pull, err = pullSeen(ctx, q, a.ID); err != nil {
				return err
			}
		}
		return nil
	})
	return out, err
}

// pullSeen is the attempt's last observed pull request, or nil.
func pullSeen(ctx context.Context, q querier, attemptID string) (*PullSeen, error) {
	var (
		p      PullSeen
		labels string
	)
	err := q.QueryRowContext(ctx,
		`SELECT number, url, state, head_commit, labels, checks, comments, observed_at FROM attempt_pulls WHERE attempt_id = ?`,
		attemptID).Scan(&p.Number, &p.URL, &p.State, &p.HeadCommit, &labels, &p.Checks, &p.Comments, &p.ObservedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read the attempt's pull request: %w", err)
	}
	if err := json.Unmarshal([]byte(labels), &p.Labels); err != nil {
		return nil, fmt.Errorf("read the attempt's pull request labels: %w", err)
	}
	return &p, nil
}
