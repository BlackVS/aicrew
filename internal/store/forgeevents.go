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

	"github.com/BlackVS/aicrew/internal/forge"
)

// Forge events (docs/CREW-CONTRACT.md, "Forge events"; a minimal A6 of
// docs/DESIGN-CONTROL-PLANE.md). aicrewd holds no forge credential: the
// worker's launcher reads the pull request of its running attempt with the
// home's own credential and reports what it observed. aicrewd keeps the last
// observation of each attempt's pull request and, once per observed change,
// writes a forge.event lifecycle message: to the worker always, and to the
// team's coordinators when the change bears on a submit (the attempt's
// result is submitted, or the pull request was merged or closed). The first
// observation of a pull request is the baseline, written by the member's own
// forge pr command, and announces nothing: the member saw it.

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
}

// ForgeDetail is what a forge.event message carries: the pull request as
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
// the pull request of, whether it was the baseline, and the forge.event it
// wrote, if any.
type ObserveResult struct {
	AttemptID string   `json:"attempt_id"`
	Baseline  bool     `json:"baseline"`
	Changes   []string `json:"changes"`
	MessageID string   `json:"message_id,omitempty"`
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
	return nil
}

// ObservePullWithToken records the token's member's observation of the pull
// request of one of its running attempts, matched by repository and head
// branch. The first observation of a pull request, and every one marked as
// a baseline, is recorded and announces nothing; a later one that differs is
// recorded with one forge.event message, and one that does not changes
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
	labels, _ := json.Marshal(o.Labels)
	if prev != nil && prev.Number == o.Number && prev.URL == o.URL && prev.State == o.State &&
		prev.HeadCommit == o.HeadCommit && slices.Equal(prev.Labels, o.Labels) && prev.Checks == o.Checks &&
		prev.Comments == o.Comments {
		return res, tx.Commit()
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
	res.Changes = changes
	if len(changes) > 0 {
		recipients := []string{a.WorkerAgentID}
		to := a.WorkerAgentID
		if submit || a.Phase == PhaseSubmitted {
			coords, err := activeCoordinators(ctx, tx, a.TeamID)
			if err != nil {
				return ObserveResult{}, err
			}
			for _, c := range coords {
				if c != a.WorkerAgentID {
					recipients, to = append(recipients, c), ""
				}
			}
		}
		task := a.Task
		m, err := insertMessage(ctx, tx, Message{TeamID: a.TeamID, Kind: KindLifecycle, To: to, Task: &task, AttemptID: a.ID,
			Text: forgeText(o, changes), Forge: &ForgeDetail{Pull: o.Number, URL: o.URL, State: o.State, HeadCommit: o.HeadCommit,
				Labels: o.Labels, Checks: o.Checks, Comments: o.Comments, Changes: changes}}, recipients, now)
		if err != nil {
			return ObserveResult{}, err
		}
		res.MessageID = m.ID
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
// or a result follows.
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
	if cur.Checks != prev.Checks && (cur.Checks == forge.ChecksSuccess || cur.Checks == forge.ChecksFailure) {
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

// forgeText is a forge.event's text: what changed, and how to read it.
func forgeText(o PullObservation, changes []string) string {
	return fmt.Sprintf("forge.event: pull request #%d (%s): %s. Read it with `aicrew-agent forge pr show --pr %d` "+
		"and act on it within your role.", o.Number, o.URL, strings.Join(changes, "; "), o.Number)
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
