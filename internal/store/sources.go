package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/BlackVS/aicrew/internal/events"
)

// The timer and aicrewd sources of the event core (docs/DESIGN-TURNS.md,
// 12.1; increment 1d). aicrewd journals, in the transaction that makes the
// change:
// - attempt.changed: an attempt's phase change, a worker's blocked report
//   among them, whose reason and start are also kept on the attempt
//   (attempt_blocks) until it leaves the blocked phase;
// - member.changed: a member's session starting, resuming or ending.
// A stored deadline (timers) journals timer.due once, on the first delivery
// tick after it passes. The default blocked-wake subscription wakes the
// team's coordinators on a blocked report, with the reason and the
// attempt's last observed pull request.

// The attempt and member subjects.
func attemptSubject(id string) string     { return "attempt:" + id }
func memberSubject(agentID string) string { return "member:" + agentID }

// maxEventReason bounds the blocked reason an attempt.changed event
// carries, as JSON encodes it, so its payload stays within
// maxEventPayload whatever the reason's characters.
const maxEventReason = 4 << 10

// fitReason is reason, or its longest start that JSON encodes, with a
// trailing ellipsis, within maxEventReason bytes: an escaped character
// takes up to six.
func fitReason(reason string) string {
	encoded := func(s string) int { b, _ := json.Marshal(s); return len(b) }
	if encoded(reason) <= maxEventReason {
		return reason
	}
	lo, hi := 0, len(reason)
	for lo < hi {
		mid := (lo + hi + 1) / 2
		if encoded(strings.ToValidUTF8(reason[:mid], "")+"…") <= maxEventReason {
			lo = mid
		} else {
			hi = mid - 1
		}
	}
	return strings.ToValidUTF8(reason[:lo], "") + "…"
}

// sourceAicrewd and sourceTimer are the sources of this file's events.
const (
	sourceAicrewd = "aicrewd"
	sourceTimer   = "timer"
)

// schemaV34 keeps a worker's blocked report on its attempt (pilot finding,
// 01a13ef0-158a), apart from the hub's grant block in attempts.blocked_*,
// and adds aicrewd's stored deadlines.
var schemaV34 = []string{
	`CREATE TABLE attempt_blocks (
		attempt_id TEXT PRIMARY KEY REFERENCES attempts (id),
		reason     TEXT NOT NULL,
		since      TEXT NOT NULL
	)`,
	`CREATE TABLE timers (
		key      TEXT PRIMARY KEY,
		team_id  TEXT NOT NULL,
		subject  TEXT NOT NULL,
		payload  TEXT NOT NULL,
		due_at   TEXT NOT NULL,
		arm      INTEGER NOT NULL DEFAULT 1,
		fired_at TEXT NOT NULL DEFAULT ''
	)`,
	`CREATE INDEX timers_due ON timers (fired_at, due_at)`,
}

// releaseSubs is the release's subscriptions, which every source matches
// its events against.
var releaseSubs = events.Defaults()

// matches is the deliveries of an event of kind to team, given its facts.
func matches(kind, teamID string, f events.Facts) []match {
	var out []match
	for _, sub := range releaseSubs.Matching(kind) {
		if roles := sub.Roles(f); len(roles) > 0 {
			out = append(out, match{subscription: sub.Name, teamID: teamID, roles: roles})
		}
	}
	return out
}

// attemptEvent is an attempt.changed event's payload.
type attemptEvent struct {
	AttemptID string       `json:"attempt_id"`
	Task      TaskRef      `json:"task"`
	Worker    string       `json:"worker_agent_id"`
	From      AttemptPhase `json:"from"`
	To        AttemptPhase `json:"to"`
	// Reason is a blocked report's reason.
	Reason string `json:"reason,omitempty"`
}

// journalAttemptChange journals a's change from one phase to another, and
// keeps or clears its blocked report.
func journalAttemptChange(ctx context.Context, tx *sql.Tx, a Attempt, to AttemptPhase, reason string, now time.Time) error {
	if to == PhaseBlocked {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO attempt_blocks (attempt_id, reason, since) VALUES (?, ?, ?)
			 ON CONFLICT (attempt_id) DO UPDATE SET reason = excluded.reason, since = excluded.since`,
			a.ID, reason, formatTime(now)); err != nil {
			return fmt.Errorf("record the blocked report: %w", err)
		}
	} else if _, err := tx.ExecContext(ctx, `DELETE FROM attempt_blocks WHERE attempt_id = ?`, a.ID); err != nil {
		return fmt.Errorf("clear the blocked report: %w", err)
	}
	if to == a.Phase {
		return nil
	}
	if to != PhaseBlocked {
		reason = ""
	}
	// A reason may be as long as a message; the event carries its start, and
	// the attempt keeps it whole.
	reason = fitReason(reason)
	key, err := newID(now)
	if err != nil {
		return err
	}
	_, _, err = journal(ctx, tx, sourceAicrewd, "attempt/"+a.ID+"/"+key, attemptSubject(a.ID), events.KindAttemptChanged,
		attemptEvent{AttemptID: a.ID, Task: a.Task, Worker: a.WorkerAgentID, From: a.Phase, To: to, Reason: reason},
		now, now, matches(events.KindAttemptChanged, a.TeamID, events.Facts{Blocked: to == PhaseBlocked}))
	return err
}

// memberEvent is a member.changed event's payload.
type memberEvent struct {
	AgentID    string `json:"agent_id"`
	Role       Role   `json:"role"`
	SessionID  string `json:"session_id"`
	Generation int64  `json:"generation"`
	// Change is started, resumed, or the state the session ended in.
	Change string `json:"change"`
}

// journalMemberChange journals a change of sess.
func journalMemberChange(ctx context.Context, tx *sql.Tx, sess Session, change string, now time.Time) error {
	key := fmt.Sprintf("session/%s/%d/%s", sess.ID, sess.Generation, change)
	_, _, err := journal(ctx, tx, sourceAicrewd, key, memberSubject(sess.AgentID), events.KindMemberChanged,
		memberEvent{AgentID: sess.AgentID, Role: sess.Role, SessionID: sess.ID, Generation: sess.Generation, Change: change},
		now, now, matches(events.KindMemberChanged, sess.TeamID, events.Facts{}))
	return err
}

// setTimer stores a deadline: when due passes, the delivery loop journals
// a timer.due event about subject with payload, once. A timer set again
// under its key is a new arm: before it fires it moves to the new deadline;
// after, it fires again, even at the same deadline.
func setTimer(ctx context.Context, tx *sql.Tx, key, teamID, subject string, payload any, due time.Time) error {
	raw, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("encode the timer: %w", err)
	}
	if len(raw) > maxEventPayload {
		return fmt.Errorf("%w: the timer's payload is %d bytes, more than %d", ErrInvalid, len(raw), maxEventPayload)
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO timers (key, team_id, subject, payload, due_at) VALUES (?, ?, ?, ?, ?)
		 ON CONFLICT (key) DO UPDATE SET team_id = excluded.team_id, subject = excluded.subject,
		   payload = excluded.payload, due_at = excluded.due_at, arm = timers.arm + 1, fired_at = ''`,
		key, teamID, subject, string(raw), formatTime(due)); err != nil {
		return fmt.Errorf("store the timer: %w", err)
	}
	return nil
}

// fireTimers journals a timer.due event for each timer whose deadline has
// passed, and marks it fired in the same transaction. A tick with no timer
// due writes nothing.
func (s *Store) fireTimers(ctx context.Context, now time.Time) error {
	var any bool
	if err := s.rdb.QueryRowContext(ctx,
		`SELECT EXISTS (SELECT 1 FROM timers WHERE fired_at = '' AND due_at <= ?)`, formatTime(now)).Scan(&any); err != nil {
		return fmt.Errorf("read the timers: %w", err)
	}
	if !any {
		return nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck // no-op after commit
	rows, err := tx.QueryContext(ctx,
		`SELECT key, team_id, subject, payload, due_at, arm FROM timers WHERE fired_at = '' AND due_at <= ? ORDER BY due_at, key`,
		formatTime(now))
	if err != nil {
		return fmt.Errorf("read the timers: %w", err)
	}
	type due struct {
		key, team, subject, payload, at string
		arm                             int64
	}
	var fired []due
	for rows.Next() {
		var d due
		if err := rows.Scan(&d.key, &d.team, &d.subject, &d.payload, &d.at, &d.arm); err != nil {
			rows.Close()
			return err
		}
		fired = append(fired, d)
	}
	if err := rows.Close(); err != nil {
		return err
	}
	for _, d := range fired {
		at, err := parseTime(d.at)
		if err != nil {
			return err
		}
		// The event's key holds the arm, so a timer set again fires again,
		// at any deadline.
		if _, _, err := journal(ctx, tx, sourceTimer, fmt.Sprintf("%s#%d", d.key, d.arm), d.subject, events.KindTimerDue,
			json.RawMessage(d.payload), at, now, matches(events.KindTimerDue, d.team, events.Facts{})); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE timers SET fired_at = ? WHERE key = ?`, formatTime(now), d.key); err != nil {
			return fmt.Errorf("mark the timer fired: %w", err)
		}
	}
	return tx.Commit()
}

// workBlock is the attempt's blocked report, or nil.
func workBlock(ctx context.Context, q querier, attemptID string) (*AttemptBlock, error) {
	var reason, since string
	err := q.QueryRowContext(ctx, `SELECT reason, since FROM attempt_blocks WHERE attempt_id = ?`, attemptID).Scan(&reason, &since)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read the blocked report: %w", err)
	}
	t, err := parseTime(since)
	if err != nil {
		return nil, err
	}
	return &AttemptBlock{Reason: reason, Since: t}, nil
}

// wakeAttempt wakes the coordinators on an attempt's blocked report, with
// the reason and the attempt's last observed pull request, so the
// coordinator can answer or escalate without reading the forge.
func wakeAttempt(ctx context.Context, tx *sql.Tx, evs []queuedEvent, roles []string) (*wakeOut, error) {
	var last attemptEvent
	var reports []string
	for i, ev := range evs {
		var e attemptEvent
		if err := json.Unmarshal(ev.Payload, &e); err != nil {
			return nil, fmt.Errorf("read an attempt event: %w", err)
		}
		if i > 0 && e.AttemptID != last.AttemptID {
			return nil, errors.New("the queue holds events of two attempts")
		}
		last = e
		if e.To == PhaseBlocked {
			reports = append(reports, e.Reason)
		}
	}
	a, err := getAttempt(ctx, tx, last.AttemptID)
	if err != nil {
		return nil, err
	}
	var recipients []string
	for _, r := range roles {
		switch r {
		case events.RoleCoordinator:
			coords, err := activeCoordinators(ctx, tx, a.TeamID)
			if err != nil {
				return nil, err
			}
			for _, c := range coords {
				if c != a.WorkerAgentID {
					recipients = append(recipients, c)
				}
			}
		case events.RoleWorker:
			recipients = append(recipients, a.WorkerAgentID)
		}
	}
	if len(recipients) == 0 {
		return nil, nil
	}
	worker, err := getAgent(ctx, tx, a.WorkerAgentID)
	if err != nil {
		return nil, err
	}
	reason := strings.Join(reports, "; ")
	text := fmt.Sprintf("action.needed: %s reported task %s blocked", worker.Label, taskName(a.Task))
	if reason != "" {
		text += ": " + reason
	}
	if !strings.HasSuffix(text, ".") {
		text += "."
	}
	text += " " + pullFacts(ctx, tx, a.ID)
	if a.Phase != PhaseBlocked {
		text += fmt.Sprintf(" The attempt has since moved to %s.", a.Phase)
	} else {
		text += " Answer what is yours, or escalate with these facts attached."
	}
	task := a.Task
	sub := ActionSubject{Kind: "blocked", AttemptID: a.ID, Task: &task,
		Block: &BlockedSubject{Worker: a.WorkerAgentID, Reason: reason}, Text: boundedText(text)}
	if o, err := lastObservation(ctx, tx, a.ID); err == nil && o != nil {
		sub.Pull = &ForgeDetail{Pull: o.Number, URL: o.URL, State: o.State, HeadCommit: o.HeadCommit,
			Labels: o.Labels, Checks: o.Checks, Comments: o.Comments}
	}
	return &wakeOut{recipients: recipients, subject: sub}, nil
}

// pullFacts is the attempt's last observed pull request, in words.
func pullFacts(ctx context.Context, tx *sql.Tx, attemptID string) string {
	o, err := lastObservation(ctx, tx, attemptID)
	if err != nil || o == nil {
		return "No pull request of the attempt has been observed."
	}
	labels := "none"
	if len(o.Labels) > 0 {
		labels = strings.Join(o.Labels, ", ")
	}
	return fmt.Sprintf("Its pull request #%d (%s) is %s at %s; labels: %s; CI: %s; %d comment(s). "+
		"`aicrew-agent forge pr show --pr %d` prints the comments and checks.",
		o.Number, o.URL, o.State, short(o.HeadCommit), labels, o.Checks, o.Comments, o.Number)
}

// boundedText is text cut to what a message may carry.
func boundedText(text string) string {
	if len(text) <= maxMessageText {
		return text
	}
	return strings.ToValidUTF8(text[:maxMessageText-3], "") + "..."
}
