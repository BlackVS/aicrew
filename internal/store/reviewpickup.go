package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/BlackVS/aicrew/internal/events"
)

// The external reviewer's labels (agent/serve.go and agent/dispatch.go name
// the same): the request, and the one it swaps the request for when it
// picks the pull request up.
const (
	labelReviewRequest = "review-this"
	labelReviewing     = "hands-reviewing"
)

// ReviewPickupAfter is how long a review request may wait at the head for
// the external reviewer to pick it up before the coordinators are told
// (DESIGN-TURNS.md 4.2, external-review).
const ReviewPickupAfter = 30 * time.Minute

// timerReviewPickup is a review pickup timer's payload kind.
const timerReviewPickup = "review_pickup"

// PullWait is a review request waiting for the external reviewer at a pull
// request's head: since the observation where it began.
type PullWait struct {
	Pull  int64     `json:"pull"`
	URL   string    `json:"url"`
	Head  string    `json:"head"`
	Since time.Time `json:"since"`
}

// reviewWaiting reports whether the observation o, with the content cf,
// waits for a pickup: open, `review-this` on it, no `hands-reviewing`, and no
// external review at the head.
func reviewWaiting(o PullObservation, cf pullContentFacts) bool {
	return o.State == "open" && slices.Contains(o.Labels, labelReviewRequest) && !slices.Contains(o.Labels, labelReviewing) &&
		!(cf.Review != nil && sameCommit(cf.Review.Head, o.HeadCommit))
}

// reviewWait is the wait at the observation o, and whether it began there:
// the wait before it (in pf) goes on at the same pull request and head, and
// a wait that ended, at another head or of another pull request begins
// anew. A request removed and added again between two observations is not
// seen.
func reviewWait(pf *pullContentFacts, o PullObservation, cf pullContentFacts, now time.Time) (*PullWait, bool) {
	if !reviewWaiting(o, cf) {
		return nil, false
	}
	if pf != nil && pf.Wait != nil && pf.Wait.Pull == o.Number && pf.Wait.Head == o.HeadCommit {
		return pf.Wait, false
	}
	return &PullWait{Pull: o.Number, URL: o.URL, Head: o.HeadCommit, Since: now}, true
}

// reviewPickup is a review pickup timer's payload.
type reviewPickup struct {
	Kind      string   `json:"kind"`
	AttemptID string   `json:"attempt_id"`
	Wait      PullWait `json:"wait"`
}

// armReviewPickup sets the attempt's review pickup timer for the wait w. One
// timer per attempt: a wait that begins anew moves its deadline.
func armReviewPickup(ctx context.Context, tx *sql.Tx, a Attempt, w PullWait) error {
	return setTimer(ctx, tx, "review-pickup/"+a.ID, a.TeamID, pullSubject(a.ID),
		reviewPickup{Kind: timerReviewPickup, AttemptID: a.ID, Wait: w}, w.Since.Add(ReviewPickupAfter))
}

// wakeTimer wakes on passed deadlines; a review pickup is the only one.
func wakeTimer(ctx context.Context, tx *sql.Tx, evs []queuedEvent, roles []string, now time.Time) (*wakeOut, error) {
	var last reviewPickup
	for _, ev := range evs {
		var p reviewPickup
		if err := json.Unmarshal(ev.Payload, &p); err != nil {
			return nil, fmt.Errorf("read a timer event: %w", err)
		}
		if p.Kind != timerReviewPickup {
			return nil, fmt.Errorf("the wake handler takes no %q timer", p.Kind)
		}
		if last.AttemptID != "" && p.AttemptID != last.AttemptID {
			return nil, errors.New("the queue holds timers of two attempts")
		}
		last = p
	}
	if !slices.Contains(roles, events.RoleCoordinator) {
		return nil, nil
	}
	return wakeReviewPickup(ctx, tx, last, now)
}

// wakeReviewPickup tells the coordinators of a review request that waited
// past ReviewPickupAfter, if the wait it was set for still holds: the
// attempt runs and its last observation waits since then at that head.
func wakeReviewPickup(ctx context.Context, tx *sql.Tx, p reviewPickup, now time.Time) (*wakeOut, error) {
	a, err := getAttempt(ctx, tx, p.AttemptID)
	if errors.Is(err, ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if a.State != AttemptRunning {
		return nil, nil
	}
	pf, err := readPullFacts(ctx, tx, a.ID)
	if err != nil {
		return nil, err
	}
	if pf == nil || pf.Wait == nil || pf.Wait.Pull != p.Wait.Pull || pf.Wait.Head != p.Wait.Head ||
		!pf.Wait.Since.Equal(p.Wait.Since) {
		return nil, nil
	}
	coords, err := activeCoordinators(ctx, tx, a.TeamID)
	if err != nil {
		return nil, err
	}
	var recipients []string
	for _, c := range coords {
		if c != a.WorkerAgentID {
			recipients = append(recipients, c)
		}
	}
	if len(recipients) == 0 {
		return nil, nil
	}
	worker, err := getAgent(ctx, tx, a.WorkerAgentID)
	if err != nil {
		return nil, err
	}
	w := pf.Wait
	text := fmt.Sprintf("action.needed: the external reviewer has not picked up pull request #%d (%s) of %s's attempt %s "+
		"for task %s: `%s` has waited at head %s since %s (%d minutes), with no `%s` and no review at the head. "+
		"The worker waits on its gates and is not woken; check the external reviewer, or escalate.",
		w.Pull, w.URL, worker.Label, a.ID, taskName(a.Task), labelReviewRequest, short(w.Head),
		w.Since.UTC().Format(time.RFC3339), int(now.Sub(w.Since)/time.Minute), labelReviewing)
	task := a.Task
	sub := ActionSubject{Kind: "review_pickup", AttemptID: a.ID, Task: &task, Wait: w, Text: boundedText(text)}
	if o, err := lastObservation(ctx, tx, a.ID); err == nil && o != nil {
		sub.Pull = &ForgeDetail{Pull: o.Number, URL: o.URL, State: o.State, HeadCommit: o.HeadCommit,
			Labels: o.Labels, Checks: o.Checks, Comments: o.Comments}
	}
	return &wakeOut{recipients: recipients, subject: sub}, nil
}
