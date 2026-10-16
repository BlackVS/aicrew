package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/BlackVS/aicrew/internal/events"
	"github.com/BlackVS/aicrew/internal/forge"
)

// Forge events (docs/CREW-CONTRACT.md, "Forge events"; a minimal A6 of
// docs/DESIGN-CONTROL-PLANE.md). aicrewd holds no forge credential: the
// worker's launcher reads the pull request of its running attempt with the
// home's own credential and reports what it observed. aicrewd keeps the last
// observation of each attempt's pull request and, once per observed change,
// journals a forge.changed event (events.go) in the same transaction. The
// default forge-wake subscription queues it for the worker always, and for
// the team's coordinators when the change bears on a submit (the attempt's
// result is submitted, or the pull request was merged or closed); its run
// writes the action.needed message (actions.go) for a change that needs a
// decision. The first observation of a pull
// request is the baseline, written by the member's own forge pr command, and
// announces nothing: the member saw it.

// ErrNoAttempt refuses an observation of a pull request that is no running
// attempt's of the member: its repository and head branch match none.
var ErrNoAttempt = errors.New("no_attempt")

// PullObservation is a pull request as the worker's launcher read it.
type PullObservation struct {
	// Host and Repository are the forge host and the repository's path
	// (owner/name), as the member's checkout names them.
	Host       string   `json:"host"`
	Repository string   `json:"repository"`
	Number     int64    `json:"number"`
	URL        string   `json:"url"`
	State      string   `json:"state"`
	Head       string   `json:"head"`
	HeadCommit string   `json:"head_commit"`
	Labels     []string `json:"labels"`
	Checks     string   `json:"checks"`
	Comments   int64    `json:"comments"`
	// Baseline marks the report of the member's own forge pr command: it is
	// recorded and announces nothing, since the member has just seen it.
	Baseline bool `json:"baseline,omitempty"`
	// Content marks an observation for which the launcher also read the
	// pull request's content (increment 1c): the external review at the
	// head, the failed checks with their excerpts, and the passed checks.
	// Without it the content facts recorded before stand.
	Content bool          `json:"content,omitempty"`
	Review  *PullReview   `json:"review,omitempty"`
	Failed  []FailedCheck `json:"failed,omitempty"`
	Passed  []PassedCheck `json:"passed,omitempty"`
}

// ForgeDetail is the pull request a forge subject carries: the pull request as
// observed, and what changed since the observation before it.
type ForgeDetail struct {
	Pull       int64    `json:"pull"`
	URL        string   `json:"url"`
	State      string   `json:"state"`
	HeadCommit string   `json:"head_commit"`
	Labels     []string `json:"labels"`
	Checks     string   `json:"checks"`
	Comments   int64    `json:"comments"`
	Changes    []string `json:"changes"`
}

// ObserveResult is what aicrewd did with an observation: the attempt it is
// the pull request of, whether it was the baseline, what changed, and the
// event it journaled, if any. The action.needed message follows when the
// event's queue runs (events.go).
type ObserveResult struct {
	AttemptID string   `json:"attempt_id"`
	Baseline  bool     `json:"baseline"`
	Changes   []string `json:"changes"`
	// Needs is what the change needs a decision on (actions.go); empty for
	// one that only reports progress.
	Needs   []string `json:"needs,omitempty"`
	EventID string   `json:"event_id,omitempty"`
}

const (
	maxPullLabels = 50
	maxLabelLen   = 100
	maxPullURL    = 2048
)

var (
	observedCommit = regexp.MustCompile(`^[0-9a-f]{7,64}$`)
	pullStates     = map[string]bool{forge.PullOpen: true, forge.PullClosed: true, forge.PullMerged: true}
	checkStates    = map[string]bool{forge.ChecksNone: true, forge.ChecksPending: true, forge.ChecksSuccess: true, forge.ChecksFailure: true}
)

func (o PullObservation) validate() error {
	bad := func(what string) error { return fmt.Errorf("%w: the observation's %s", ErrInvalid, what) }
	if h, err := forge.NormalizeHost(o.Host); err != nil || h != o.Host {
		return bad("host")
	}
	if o.Repository == "" || len(o.Repository) > maxRefLen || strings.ContainsAny(o.Repository, " \t\r\n") {
		return bad("repository")
	}
	if o.Number < 1 || !pullStates[o.State] || !checkStates[o.Checks] || o.Comments < 0 {
		return bad("number, state, checks or comments")
	}
	if !strings.HasPrefix(o.URL, "https://") || len(o.URL) > maxPullURL || strings.ContainsAny(o.URL, " \t\r\n") {
		return bad("url")
	}
	if !validRefs(o.Head) || (o.HeadCommit != "" && !observedCommit.MatchString(o.HeadCommit)) {
		return bad("head")
	}
	if len(o.Labels) > maxPullLabels {
		return bad("labels")
	}
	for _, l := range o.Labels {
		if err := validText(l, maxLabelLen, "a label", true); err != nil {
			return err
		}
		if strings.IndexFunc(l, unicode.IsControl) >= 0 {
			return bad("labels")
		}
	}
	if !o.Content && (o.Review != nil || len(o.Failed) > 0 || len(o.Passed) > 0) {
		return bad("content without the content mark")
	}
	if r := o.Review; r != nil && (len(r.Body) > maxObservedLen || len(r.URL) > maxPullURL || len(r.Author) > maxCheckField ||
		len(r.Verdict) > maxCheckField || (r.Head != "" && !observedCommit.MatchString(r.Head)) || !utf8.ValidString(r.Body)) {
		return bad("review")
	}
	if len(o.Failed) > 4*maxFailed || len(o.Passed) > 4*maxPassed {
		return bad("checks")
	}
	for _, f := range o.Failed {
		if len(f.Name) > maxCheckField || len(f.URL) > maxPullURL || len(f.Excerpt) > maxObservedLen ||
			len(f.NoLog) > maxCheckField || len(f.Conclusion) > maxCheckField || !utf8.ValidString(f.Excerpt) {
			return bad("failed check")
		}
	}
	for _, p := range o.Passed {
		if len(p.Name) > maxCheckField || len(p.URL) > maxPullURL {
			return bad("passed check")
		}
	}
	return nil
}

// ObservePullWithToken records the token's member's observation of the pull
// request of one of its running attempts, matched by repository and head
// branch. The first observation of a pull request, and every one marked as
// a baseline, is recorded and announces nothing; a later one that differs is
// recorded with one forge.changed event, and one that does not changes
// nothing, so a repeated report announces nothing twice.
func (s *Store) ObservePullWithToken(ctx context.Context, token string, o PullObservation) (ObserveResult, error) {
	if o.Labels == nil {
		o.Labels = []string{}
	}
	if err := o.validate(); err != nil {
		return ObserveResult{}, err
	}
	o.Labels = slices.Clone(o.Labels)
	slices.Sort(o.Labels)
	o.Labels = slices.Compact(o.Labels)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return ObserveResult{}, fmt.Errorf("begin: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck // no-op after commit
	now := s.now()
	b, err := tokenBinding(ctx, tx, token, now)
	if err != nil {
		return ObserveResult{}, err
	}
	a, err := pullAttempt(ctx, tx, b, o)
	if err != nil {
		return ObserveResult{}, err
	}
	res := ObserveResult{AttemptID: a.ID, Changes: []string{}}
	prev, err := lastObservation(ctx, tx, a.ID)
	if err != nil {
		return ObserveResult{}, err
	}
	pf, err := readPullFacts(ctx, tx, a.ID)
	if err != nil {
		return ObserveResult{}, err
	}
	// The content facts: this observation's when the launcher read the
	// content, else those recorded before.
	cf := pullContentFacts{Head: o.HeadCommit, Checks: o.Checks}
	switch {
	case o.Content:
		cf.Review, cf.Failed, cf.Passed = o.Review, o.Failed, o.Passed
	case pf != nil:
		// The review names its head, which pullNeeds checks; the checks are
		// the head's and the conclusion's they were read with, and stand
		// only while both are unchanged.
		cf.Review = pf.Review
		if pf.Head == o.HeadCommit && pf.Checks == o.Checks {
			cf.Failed, cf.Passed = pf.Failed, pf.Passed
		}
	}
	boundContent(&cf)
	newReview := o.Content && cf.Review != nil && (pf == nil || pf.Review == nil || pf.Review.URL != cf.Review.URL ||
		pf.Review.Head != cf.Review.Head || pf.Review.Verdict != cf.Review.Verdict)
	labels, _ := json.Marshal(o.Labels)
	if prev != nil && prev.Number == o.Number && prev.URL == o.URL && prev.State == o.State &&
		prev.HeadCommit == o.HeadCommit && slices.Equal(prev.Labels, o.Labels) && prev.Checks == o.Checks &&
		prev.Comments == o.Comments && !newReview {
		return res, tx.Commit()
	}
	if err := writePullFacts(ctx, tx, a.ID, cf); err != nil {
		return ObserveResult{}, err
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO attempt_pulls (attempt_id, number, url, state, head_commit, labels, checks, comments, observed_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT (attempt_id) DO UPDATE SET number = excluded.number, url = excluded.url, state = excluded.state,
		   head_commit = excluded.head_commit, labels = excluded.labels, checks = excluded.checks,
		   comments = excluded.comments, observed_at = excluded.observed_at`,
		a.ID, o.Number, o.URL, o.State, o.HeadCommit, string(labels), o.Checks, o.Comments, formatTime(now)); err != nil {
		return ObserveResult{}, fmt.Errorf("record the pull request: %w", err)
	}
	if prev == nil || prev.Number != o.Number || o.Baseline {
		res.Baseline = true
		return res, tx.Commit()
	}
	changes, submit := pullChanges(*prev, o)
	needs := pullNeeds(*prev, pf, o, cf)
	res.Changes, res.Needs = changes, needs
	if len(changes) > 0 || len(needs) > 0 {
		key, err := newID(now)
		if err != nil {
			return ObserveResult{}, err
		}
		ms := matches(events.KindForgeChanged, a.TeamID,
			events.Facts{Submit: submit, Submitted: a.Phase == PhaseSubmitted, Action: len(needs) > 0})
		// The event carries the content its needs call for, bounded; the
		// observation's own copy of the content is left out.
		ev := forgeEvent{AttemptID: a.ID, Observation: o, Changes: changes, Needs: needs}
		ev.Observation.Content, ev.Observation.Review, ev.Observation.Failed, ev.Observation.Passed = false, nil, nil, nil
		for _, n := range needs {
			switch n {
			case NeedReview, NeedGates:
				ev.Review = cf.Review
				if n == NeedGates {
					ev.Passed = cf.Passed
				}
			case NeedCI:
				ev.Failed = cf.Failed
			}
		}
		// Each change is its own event: the observation it compares with is
		// gone once this one is recorded.
		if res.EventID, _, err = journal(ctx, tx, sourceForge, a.ID+"/"+key, pullSubject(a.ID), events.KindForgeChanged,
			ev, now, now, ms); err != nil {
			return ObserveResult{}, err
		}
	}
	return res, tx.Commit()
}

// pullAttempt is the member's running attempt whose repository and branch
// are the pull request's.
func pullAttempt(ctx context.Context, tx *sql.Tx, b TokenBinding, o PullObservation) (Attempt, error) {
	rows, err := tx.QueryContext(ctx,
		`SELECT id FROM attempts WHERE team_id = ? AND worker_agent_id = ? AND state = ? AND branch = ? ORDER BY id`,
		b.TeamID, b.AgentID, string(AttemptRunning), o.Head)
	if err != nil {
		return Attempt{}, fmt.Errorf("read the member's attempts: %w", err)
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return Attempt{}, err
		}
		ids = append(ids, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return Attempt{}, err
	}
	for _, id := range ids {
		a, err := getAttempt(ctx, tx, id)
		if err != nil {
			return Attempt{}, err
		}
		host, path, err := forge.Repository(a.Repository.URL)
		if err == nil && host == o.Host && strings.EqualFold(path, o.Repository) {
			return a, nil
		}
	}
	return Attempt{}, fmt.Errorf("%w: pull request %d of %s/%s is from branch %s, which is no running attempt of yours",
		ErrNoAttempt, o.Number, o.Host, o.Repository, o.Head)
}

// lastObservation is the attempt's recorded pull request, or nil.
func lastObservation(ctx context.Context, tx *sql.Tx, attemptID string) (*PullObservation, error) {
	var (
		o      PullObservation
		labels string
	)
	err := tx.QueryRowContext(ctx,
		`SELECT number, url, state, head_commit, labels, checks, comments FROM attempt_pulls WHERE attempt_id = ?`, attemptID).
		Scan(&o.Number, &o.URL, &o.State, &o.HeadCommit, &labels, &o.Checks, &o.Comments)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read the attempt's pull request: %w", err)
	}
	if err := json.Unmarshal([]byte(labels), &o.Labels); err != nil {
		return nil, fmt.Errorf("read the attempt's pull request labels: %w", err)
	}
	return &o, nil
}

// pullChanges is what changed between two observations of one pull request
// that a member acts on, and whether it bears on a submit whatever the
// attempt's phase: a merge or close. A label removed, a new head commit or
// a CI run starting is recorded and announces nothing: the member made it,
// or a result follows. A concluded CI is announced once per head commit.
func pullChanges(prev, cur PullObservation) (changes []string, submit bool) {
	if cur.State != prev.State {
		switch cur.State {
		case forge.PullMerged:
			changes = append(changes, "the pull request was merged")
		case forge.PullClosed:
			changes = append(changes, "the pull request was closed without a merge")
		default:
			changes = append(changes, "the pull request was reopened")
		}
		submit = true
	}
	for _, l := range cur.Labels {
		if !slices.Contains(prev.Labels, l) {
			changes = append(changes, "label "+l+" added")
		}
	}
	// A conclusion is the head's: one on a new head is news even when it
	// equals the previous head's.
	if (cur.Checks != prev.Checks || cur.HeadCommit != prev.HeadCommit) &&
		(cur.Checks == forge.ChecksSuccess || cur.Checks == forge.ChecksFailure) {
		changes = append(changes, fmt.Sprintf("CI concluded %s at %s", cur.Checks, short(cur.HeadCommit)))
	}
	if n := cur.Comments - prev.Comments; n > 0 {
		changes = append(changes, fmt.Sprintf("%d new comment(s)", n))
	}
	return changes, submit
}

func short(commit string) string {
	if len(commit) > 12 {
		return commit[:12]
	}
	return commit
}

// encodeForge is a message's forge detail as stored, "" for none.
func encodeForge(d *ForgeDetail) (string, error) {
	if d == nil {
		return "", nil
	}
	b, err := json.Marshal(d)
	if err != nil {
		return "", fmt.Errorf("encode forge event: %w", err)
	}
	return string(b), nil
}
