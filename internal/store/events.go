package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/BlackVS/aicrew/internal/events"
)

// The event core (docs/DESIGN-TURNS.md, section 12; increment 1b). A source
// writes an event to the journal in the transaction that moves its own
// cursor: the board feed's page with its cursor, a pull request observation
// with the attempt's last observation. The same transaction matches the
// event to the subscriptions (internal/events) and queues one delivery per
// subscription and team whose recipients' conditions hold, with the roles
// they name. An event seen twice, from a replayed feed page, is the same
// row: the journal is keyed by source and key.
//
// DeliverEvents, run by aicrewd's loop, hands each queue (a subscription, a
// team and a subject) to its handler once its events have been quiet for
// EventQuiet, or once its oldest has waited EventMaxWait: a burst of events
// about one subject becomes one handler run, and one message. A run reads
// the queue's pending events in order and is one transaction with the
// message it writes and the record of what it handled, so a crash replays
// the run whole or not at all. A handler that fails three times in a row
// parks the queue's events and tells the team's coordinators; a release
// whose subscription file differs replays them.

// The event sources.
const (
	sourceBoard = "board"
	sourceForge = "forge"
)

// maxEventPayload bounds an event's payload.
const maxEventPayload = 16 << 10

// Delivery timing (DESIGN-TURNS 12.4).
const (
	// EventQuiet is how long a queue's events must be quiet before its
	// handler runs: events closer together than this become one run.
	EventQuiet = 30 * time.Second
	// EventMaxWait bounds how long a queue's oldest event waits for a run
	// while events keep arriving.
	EventMaxWait = 2 * time.Minute
	// maxHandlerFailures is how many runs of a queue in a row may fail
	// before its events are parked.
	maxHandlerFailures = 3
)

// The states of a delivery and the outcomes of a run.
const (
	deliveryPending = "pending"
	deliveryHandled = "handled"
	deliveryParked  = "parked"

	runHandled = "handled"
	runFailed  = "failed"
	runParked  = "parked"
)

// schemaV33 adds the event journal, its deliveries and the record of each
// handler run (docs/DESIGN-TURNS.md, 12.4). Nothing is journaled before it:
// the board's and the forge's earlier changes were announced directly.
var schemaV33 = []string{
	`CREATE TABLE events (
		id          TEXT PRIMARY KEY,
		source      TEXT NOT NULL,
		key         TEXT NOT NULL,
		subject     TEXT NOT NULL,
		kind        TEXT NOT NULL,
		payload     TEXT NOT NULL,
		at          TEXT NOT NULL,
		recorded_at TEXT NOT NULL,
		UNIQUE (source, key)
	)`,
	`CREATE TABLE event_deliveries (
		event_id     TEXT NOT NULL REFERENCES events (id),
		subscription TEXT NOT NULL,
		team_id      TEXT NOT NULL,
		subject      TEXT NOT NULL,
		roles        TEXT NOT NULL,
		state        TEXT NOT NULL CHECK (state IN ('pending', 'handled', 'parked')),
		recorded_at  TEXT NOT NULL,
		run_id       TEXT NOT NULL DEFAULT '',
		PRIMARY KEY (event_id, subscription, team_id)
	)`,
	`CREATE INDEX event_deliveries_queue ON event_deliveries (state, subscription, team_id, subject)`,
	`CREATE TABLE event_runs (
		id           TEXT PRIMARY KEY,
		subscription TEXT NOT NULL,
		team_id      TEXT NOT NULL,
		subject      TEXT NOT NULL,
		version      TEXT NOT NULL,
		outcome      TEXT NOT NULL CHECK (outcome IN ('handled', 'failed', 'parked')),
		events       INTEGER NOT NULL,
		message_id   TEXT NOT NULL DEFAULT '',
		error        TEXT NOT NULL DEFAULT '',
		at           TEXT NOT NULL
	)`,
	`CREATE INDEX event_runs_queue ON event_runs (subscription, team_id, subject, at)`,
}

// match is one subscription's delivery of an event to a team, with the
// roles whose conditions hold.
type match struct {
	subscription string
	teamID       string
	roles        []string
}

// journal writes an event and queues its deliveries, in the caller's
// transaction, and returns the event's ID and how many deliveries it
// queued. An event already in the journal (the same source and key) is not
// written again and queues nothing: it returns "" and 0.
func journal(ctx context.Context, tx *sql.Tx, source, key, subject, kind string, payload any, at, now time.Time,
	ms []match) (string, int, error) {
	raw, err := json.Marshal(payload)
	if err != nil {
		return "", 0, fmt.Errorf("encode the event: %w", err)
	}
	if len(raw) > maxEventPayload {
		return "", 0, fmt.Errorf("%w: the event's payload is %d bytes, more than %d", ErrInvalid, len(raw), maxEventPayload)
	}
	id, err := newID(now)
	if err != nil {
		return "", 0, err
	}
	res, err := tx.ExecContext(ctx,
		`INSERT INTO events (id, source, key, subject, kind, payload, at, recorded_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT (source, key) DO NOTHING`,
		id, source, key, subject, kind, string(raw), formatTime(at), formatTime(now))
	if err != nil {
		return "", 0, fmt.Errorf("journal the event: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return "", 0, nil
	}
	for _, m := range ms {
		roles, _ := json.Marshal(m.roles)
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO event_deliveries (event_id, subscription, team_id, subject, roles, state, recorded_at)
			 VALUES (?, ?, ?, ?, ?, ?, ?)`,
			id, m.subscription, m.teamID, subject, string(roles), deliveryPending, formatTime(now)); err != nil {
			return "", 0, fmt.Errorf("queue the event: %w", err)
		}
	}
	return id, len(ms), nil
}

// queue is one subscription's deliveries to one team about one subject.
type queue struct {
	subscription, teamID, subject string
}

// queuedEvent is a pending delivery with its event.
type queuedEvent struct {
	ID      string
	Kind    string
	Payload json.RawMessage
	At      time.Time
	Roles   []string
}

// handlerFunc handles a queue's pending events in the run's transaction
// and returns what to send, or nil for nothing; the runner writes it
// (actions.go, writeWakes).
type handlerFunc func(ctx context.Context, tx *sql.Tx, q queue, evs []queuedEvent, now time.Time) (*wakeOut, error)

// DeliverRun is one handler run DeliverEvents made.
type DeliverRun struct {
	Subscription string `json:"subscription"`
	TeamID       string `json:"team_id"`
	Subject      string `json:"subject"`
	Events       int    `json:"events"`
	Outcome      string `json:"outcome"`
	MessageID    string `json:"message_id,omitempty"`
	Error        string `json:"error,omitempty"`
}

// SetEventTiming sets how long a queue's events must be quiet before its
// run, and how long its oldest may wait; a value of zero or less restores
// EventQuiet or EventMaxWait. Tests use it to deliver at once.
func (s *Store) SetEventTiming(quiet, maxWait time.Duration) {
	if quiet <= 0 {
		quiet = EventQuiet
	}
	if maxWait <= 0 {
		maxWait = EventMaxWait
	}
	s.eventQuiet, s.eventMaxWait = quiet, maxWait
}

// DeliverEvents runs the handler of every queue that is due, and returns
// the runs it made. Parked events whose run recorded another subscription
// version are replayed first. A call with nothing to do writes nothing.
// aicrewd's reconciler only.
func (s *Store) DeliverEvents(ctx context.Context, c Caller) ([]DeliverRun, error) {
	if c.kind != callerReconciler {
		return nil, fmt.Errorf("%w: only aicrewd delivers events", ErrForbidden)
	}
	var stale bool
	if err := s.rdb.QueryRowContext(ctx,
		`SELECT EXISTS (SELECT 1 FROM event_deliveries d JOIN event_runs r ON r.id = d.run_id
		 WHERE d.state = ? AND r.outcome = ? AND r.version != ?)`,
		deliveryParked, runParked, s.subs.Version).Scan(&stale); err != nil {
		return nil, fmt.Errorf("read the parked events: %w", err)
	}
	if stale {
		if _, err := s.db.ExecContext(ctx,
			`UPDATE event_deliveries SET state = ?, run_id = '' WHERE state = ? AND run_id IN
			 (SELECT id FROM event_runs WHERE outcome = ? AND version != ?)`,
			deliveryPending, deliveryParked, runParked, s.subs.Version); err != nil {
			return nil, fmt.Errorf("replay parked events: %w", err)
		}
	}
	now := s.now()
	if err := s.fireTimers(ctx, now); err != nil {
		return nil, err
	}
	due, err := s.dueQueues(ctx, now)
	if err != nil {
		return nil, err
	}
	// A team's due queues run together, so the subjects they wake the same
	// members for become one message.
	var teams []string
	byTeam := map[string][]queue{}
	for _, q := range due {
		if _, ok := byTeam[q.teamID]; !ok {
			teams = append(teams, q.teamID)
		}
		byTeam[q.teamID] = append(byTeam[q.teamID], q)
	}
	runs := []DeliverRun{}
	for _, team := range teams {
		if s.beforeQueueRun != nil {
			s.beforeQueueRun()
		}
		rs, err := s.runTeam(ctx, team, byTeam[team], now)
		runs = append(runs, rs...)
		if err != nil {
			return runs, err
		}
	}
	return runs, nil
}

// runTeam runs a team's due queues in one transaction and writes their
// messages together. When a handler fails, nothing of the batch is kept,
// and each queue runs on its own, so the failure is recorded against its
// queue alone.
func (s *Store) runTeam(ctx context.Context, teamID string, qs []queue, now time.Time) ([]DeliverRun, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("begin: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck // no-op after commit
	type ran struct {
		q   queue
		evs []queuedEvent
		out *wakeOut
	}
	var done []ran
	var outs []*wakeOut
	for _, q := range qs {
		evs, err := s.dueEvents(ctx, tx, q, now)
		if err != nil {
			return nil, err
		}
		if len(evs) == 0 {
			continue
		}
		out, herr := s.handle(ctx, tx, q, evs, now)
		if herr != nil {
			tx.Rollback() //nolint:errcheck // each queue runs again on its own
			var runs []DeliverRun
			for _, q := range qs {
				r, err := s.runQueue(ctx, q, now)
				if err != nil {
					return runs, err
				}
				if r.Events > 0 {
					runs = append(runs, r)
				}
			}
			return runs, nil
		}
		done = append(done, ran{q, evs, out})
		outs = append(outs, out)
	}
	ids, err := writeWakes(ctx, tx, teamID, outs, now)
	if err != nil {
		return nil, err
	}
	var runs []DeliverRun
	for _, d := range done {
		msgID := ""
		if d.out != nil {
			msgID = ids[d.out]
		}
		runID, err := insertRun(ctx, tx, d.q, s.subs.Version, runHandled, len(d.evs), msgID, "", now)
		if err != nil {
			return nil, err
		}
		if err := markDeliveries(ctx, tx, d.q, d.evs, deliveryHandled, runID); err != nil {
			return nil, err
		}
		runs = append(runs, DeliverRun{Subscription: d.q.subscription, TeamID: d.q.teamID, Subject: d.q.subject,
			Events: len(d.evs), Outcome: runHandled, MessageID: msgID})
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit: %w", err)
	}
	return runs, nil
}

// dueEvents is q's pending events, in the run's transaction, if q is still
// due: an event that arrived after the queue was selected may have ended
// its quiet.
func (s *Store) dueEvents(ctx context.Context, tx *sql.Tx, q queue, now time.Time) ([]queuedEvent, error) {
	var oldest, newest sql.NullString
	if err := tx.QueryRowContext(ctx,
		`SELECT MIN(recorded_at), MAX(recorded_at) FROM event_deliveries
		 WHERE state = ? AND subscription = ? AND team_id = ? AND subject = ?`,
		deliveryPending, q.subscription, q.teamID, q.subject).Scan(&oldest, &newest); err != nil {
		return nil, fmt.Errorf("read the queue: %w", err)
	}
	if !oldest.Valid {
		return nil, nil
	}
	if due, err := s.due(oldest.String, newest.String, now); err != nil || !due {
		return nil, err
	}
	return pendingEvents(ctx, tx, q)
}

// dueQueues is the queues with pending events that are due a run.
func (s *Store) dueQueues(ctx context.Context, now time.Time) ([]queue, error) {
	rows, err := s.rdb.QueryContext(ctx,
		`SELECT subscription, team_id, subject, MIN(recorded_at), MAX(recorded_at) FROM event_deliveries
		 WHERE state = ? GROUP BY subscription, team_id, subject ORDER BY MIN(recorded_at)`, deliveryPending)
	if err != nil {
		return nil, fmt.Errorf("read the event queues: %w", err)
	}
	defer rows.Close()
	var out []queue
	for rows.Next() {
		var q queue
		var oldest, newest string
		if err := rows.Scan(&q.subscription, &q.teamID, &q.subject, &oldest, &newest); err != nil {
			return nil, err
		}
		due, err := s.due(oldest, newest, now)
		if err != nil {
			return nil, err
		}
		if due {
			out = append(out, q)
		}
	}
	return out, rows.Err()
}

// due reports whether a queue whose pending events were recorded from
// oldest to newest is due a run at now: quiet for eventQuiet, or waiting
// for eventMaxWait.
func (s *Store) due(oldest, newest string, now time.Time) (bool, error) {
	o, err := parseTime(oldest)
	if err != nil {
		return false, err
	}
	n, err := parseTime(newest)
	if err != nil {
		return false, err
	}
	return now.Sub(n) >= s.eventQuiet || now.Sub(o) >= s.eventMaxWait, nil
}

// runQueue runs q's handler on its pending events in one transaction, if
// the queue is still due: an event that arrived after the queue was
// selected may have ended its quiet. A failure is recorded in a
// transaction of its own, and the third in a row parks the events.
func (s *Store) runQueue(ctx context.Context, q queue, now time.Time) (DeliverRun, error) {
	r := DeliverRun{Subscription: q.subscription, TeamID: q.teamID, Subject: q.subject}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return r, fmt.Errorf("begin: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck // no-op after commit
	evs, err := s.dueEvents(ctx, tx, q, now)
	if err != nil {
		return r, err
	}
	r.Events = len(evs)
	if len(evs) == 0 {
		return r, nil
	}
	out, herr := s.handle(ctx, tx, q, evs, now)
	if herr != nil {
		tx.Rollback() //nolint:errcheck // the failure is recorded on its own
		return s.failRun(ctx, q, evs, herr, now)
	}
	ids, err := writeWakes(ctx, tx, q.teamID, []*wakeOut{out}, now)
	if err != nil {
		return r, err
	}
	msgID := ""
	if out != nil {
		msgID = ids[out]
	}
	runID, err := insertRun(ctx, tx, q, s.subs.Version, runHandled, len(evs), msgID, "", now)
	if err != nil {
		return r, err
	}
	if err := markDeliveries(ctx, tx, q, evs, deliveryHandled, runID); err != nil {
		return r, err
	}
	if err := tx.Commit(); err != nil {
		return r, fmt.Errorf("commit: %w", err)
	}
	r.Outcome, r.MessageID = runHandled, msgID
	return r, nil
}

// handle runs the queue's subscription's handler.
func (s *Store) handle(ctx context.Context, tx *sql.Tx, q queue, evs []queuedEvent, now time.Time) (*wakeOut, error) {
	sub, ok := s.subs.Named(q.subscription)
	if !ok {
		return nil, fmt.Errorf("no subscription is called %s in this release", q.subscription)
	}
	h, ok := s.handlers[sub.Handler]
	if !ok {
		return nil, fmt.Errorf("no handler is called %s in this release", sub.Handler)
	}
	return h(ctx, tx, q, evs, now)
}

// failRun records a failed run of q; the third failure in a row parks the
// queue's pending events and tells the team's coordinators.
func (s *Store) failRun(ctx context.Context, q queue, evs []queuedEvent, herr error, now time.Time) (DeliverRun, error) {
	r := DeliverRun{Subscription: q.subscription, TeamID: q.teamID, Subject: q.subject, Events: len(evs), Error: herr.Error()}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return r, fmt.Errorf("begin: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck // no-op after commit
	rows, err := tx.QueryContext(ctx,
		`SELECT outcome FROM event_runs WHERE subscription = ? AND team_id = ? AND subject = ? ORDER BY at DESC, id DESC LIMIT ?`,
		q.subscription, q.teamID, q.subject, maxHandlerFailures-1)
	if err != nil {
		return r, fmt.Errorf("read the queue's runs: %w", err)
	}
	failed := 0
	for rows.Next() {
		var outcome string
		if err := rows.Scan(&outcome); err != nil {
			rows.Close()
			return r, err
		}
		if outcome != runFailed {
			break
		}
		failed++
	}
	rows.Close()
	r.Outcome = runFailed
	if failed+1 >= maxHandlerFailures {
		r.Outcome = runParked
	}
	msg := oneLineError(herr)
	runID, err := insertRun(ctx, tx, q, s.subs.Version, r.Outcome, len(evs), "", msg, now)
	if err != nil {
		return r, err
	}
	if r.Outcome == runParked {
		// The events read before the failure are parked; one that arrived
		// since stays pending and starts the count again.
		if err := markDeliveries(ctx, tx, q, evs, deliveryParked, runID); err != nil {
			return r, err
		}
		coords, err := activeCoordinators(ctx, tx, q.teamID)
		if err != nil {
			return r, err
		}
		if len(coords) > 0 {
			m, err := insertMessage(ctx, tx, Message{TeamID: q.teamID, Kind: KindLifecycle,
				Text: fmt.Sprintf("Events are parked: the %s handler of subscription %s failed %d times in a row on %s (last error: %s). "+
					"Its %d event(s) are kept and replayed when a release changes the subscriptions.",
					s.subscriptionHandler(q.subscription), q.subscription, maxHandlerFailures, q.subject, msg, len(evs))},
				coords, now)
			if err != nil {
				return r, err
			}
			r.MessageID = m.ID
		}
	}
	return r, tx.Commit()
}

func (s *Store) subscriptionHandler(name string) string {
	if sub, ok := s.subs.Named(name); ok {
		return sub.Handler
	}
	return "unknown"
}

// oneLineError is err's text as one line of at most 300 bytes.
func oneLineError(err error) string {
	msg := strings.Join(strings.Fields(err.Error()), " ")
	if len(msg) > 300 {
		msg = strings.ToValidUTF8(msg[:300], "") + "…"
	}
	return msg
}

// pendingEvents is q's pending events in the order they were journaled:
// the journal's row order, since events of one page share their time and
// an ID minted within one millisecond is not ordered.
func pendingEvents(ctx context.Context, tx *sql.Tx, q queue) ([]queuedEvent, error) {
	rows, err := tx.QueryContext(ctx,
		`SELECT e.id, e.kind, e.payload, e.at, d.roles FROM event_deliveries d JOIN events e ON e.id = d.event_id
		 WHERE d.state = ? AND d.subscription = ? AND d.team_id = ? AND d.subject = ?
		 ORDER BY e.rowid`,
		deliveryPending, q.subscription, q.teamID, q.subject)
	if err != nil {
		return nil, fmt.Errorf("read the queue: %w", err)
	}
	defer rows.Close()
	var out []queuedEvent
	for rows.Next() {
		var ev queuedEvent
		var payload, at, roles string
		if err := rows.Scan(&ev.ID, &ev.Kind, &payload, &at, &roles); err != nil {
			return nil, err
		}
		if ev.At, err = parseTime(at); err != nil {
			return nil, err
		}
		if err := json.Unmarshal([]byte(roles), &ev.Roles); err != nil {
			return nil, fmt.Errorf("read a delivery's roles: %w", err)
		}
		ev.Payload = json.RawMessage(payload)
		out = append(out, ev)
	}
	return out, rows.Err()
}

func insertRun(ctx context.Context, tx *sql.Tx, q queue, version, outcome string, n int, msgID, errText string,
	now time.Time) (string, error) {
	id, err := newID(now)
	if err != nil {
		return "", err
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO event_runs (id, subscription, team_id, subject, version, outcome, events, message_id, error, at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		id, q.subscription, q.teamID, q.subject, version, outcome, n, msgID, errText, formatTime(now)); err != nil {
		return "", fmt.Errorf("record the run: %w", err)
	}
	return id, nil
}

func markDeliveries(ctx context.Context, tx *sql.Tx, q queue, evs []queuedEvent, state, runID string) error {
	for _, ev := range evs {
		if _, err := tx.ExecContext(ctx,
			`UPDATE event_deliveries SET state = ?, run_id = ? WHERE event_id = ? AND subscription = ? AND team_id = ? AND state = ?`,
			state, runID, ev.ID, q.subscription, q.teamID, deliveryPending); err != nil {
			return fmt.Errorf("record the delivery: %w", err)
		}
	}
	return nil
}

// The subjects events are about.
func taskSubject(t TaskRef) string        { return "task:" + t.HubID + "/" + t.ProjectID + "/" + t.TaskID }
func pullSubject(attemptID string) string { return "pull:" + attemptID }

// boardEvent is a board.changed event's payload.
type boardEvent struct {
	Task   TaskRef     `json:"task"`
	Change BoardDetail `json:"change"`
}

// forgeEvent is a forge.changed event's payload: the observation and what
// changed since the one before it.
type forgeEvent struct {
	AttemptID   string          `json:"attempt_id"`
	Observation PullObservation `json:"observation"`
	Changes     []string        `json:"changes"`
	// Needs is what the change needs a decision on, and Review, Failed,
	// Passed and PrePush the content those needs call for (actions.go).
	Needs   []string        `json:"needs,omitempty"`
	Review  *PullReview     `json:"review,omitempty"`
	Failed  []FailedCheck   `json:"failed,omitempty"`
	Passed  []PassedCheck   `json:"passed,omitempty"`
	PrePush *PrePushFailure `json:"pre_push,omitempty"`
	// Omitted says what fitForgeEvent left out for room, "" for nothing.
	Omitted string `json:"omitted,omitempty"`
}

// wake is the handler of the event core: one action.needed subject per
// run, to the recipients the run's events name, with the content the
// decision needs (actions.go). It reads the current facts (the team's
// coordinators, the attempt's worker), not those of the moment each event
// was written.
func (s *Store) wake(ctx context.Context, tx *sql.Tx, q queue, evs []queuedEvent, now time.Time) (*wakeOut, error) {
	var roles []string
	for _, ev := range evs {
		if ev.Kind != evs[0].Kind {
			return nil, fmt.Errorf("the queue holds events of kinds %s and %s", evs[0].Kind, ev.Kind)
		}
		for _, r := range ev.Roles {
			if !slices.Contains(roles, r) {
				roles = append(roles, r)
			}
		}
	}
	switch evs[0].Kind {
	case events.KindBoardChanged:
		return wakeBoard(ctx, tx, q, evs, roles)
	case events.KindForgeChanged:
		return wakeForge(ctx, tx, evs, roles)
	case events.KindAttemptChanged:
		return wakeAttempt(ctx, tx, evs, roles)
	case events.KindAttemptAnswered:
		return wakeAnswer(ctx, tx, evs, roles)
	case events.KindAutomationRun:
		return wakeAutomation(ctx, tx, evs)
	case events.KindTimerDue:
		return wakeTimer(ctx, tx, evs, roles, now)
	}
	return nil, fmt.Errorf("the wake handler takes no %s event", evs[0].Kind)
}

func wakeBoard(ctx context.Context, tx *sql.Tx, q queue, evs []queuedEvent, roles []string) (*wakeOut, error) {
	if !slices.Contains(roles, events.RoleCoordinator) {
		return nil, nil // a board change has no worker of its own
	}
	recipients, err := activeCoordinators(ctx, tx, q.teamID)
	if err != nil || len(recipients) == 0 {
		return nil, err
	}
	var last boardEvent
	var texts []string
	for i, ev := range evs {
		var b boardEvent
		if err := json.Unmarshal(ev.Payload, &b); err != nil {
			return nil, fmt.Errorf("read a board event: %w", err)
		}
		if i > 0 && b.Task != last.Task {
			return nil, errors.New("the queue holds events of two tasks")
		}
		last = b
		ch := BoardChange{ProjectID: b.Task.ProjectID, TaskID: b.Task.TaskID, Revision: b.Change.Revision,
			From: b.Change.From, To: b.Change.To}
		texts = append(texts, boardText(ch, false))
	}
	var attempt bool
	if err := tx.QueryRowContext(ctx,
		`SELECT EXISTS (SELECT 1 FROM attempts WHERE team_id = ? AND task_hub_id = ? AND task_project_id = ? AND task_id = ?)`,
		q.teamID, last.Task.HubID, last.Task.ProjectID, last.Task.TaskID).Scan(&attempt); err != nil {
		return nil, fmt.Errorf("read the task's attempts: %w", err)
	}
	text := strings.Join(texts, " ")
	if attempt {
		text += " The team has an attempt for it."
	}
	task, project := last.Task, ProjectRef{HubID: last.Task.HubID, ProjectID: last.Task.ProjectID}
	change := last.Change
	return &wakeOut{recipients: recipients, project: &project, board: &change,
		subject: ActionSubject{Kind: "board", Task: &task, Board: &change, Text: text}}, nil
}

func wakeForge(ctx context.Context, tx *sql.Tx, evs []queuedEvent, roles []string) (*wakeOut, error) {
	var last forgeEvent
	changes, needs := []string{}, []string{}
	var parsed []forgeEvent
	for i, ev := range evs {
		var f forgeEvent
		if err := json.Unmarshal(ev.Payload, &f); err != nil {
			return nil, fmt.Errorf("read a forge event: %w", err)
		}
		if i > 0 && f.AttemptID != last.AttemptID {
			return nil, errors.New("the queue holds events of two attempts")
		}
		last = f
		parsed = append(parsed, f)
		// Every event's changes, in order, repeats included: two events that
		// each add a comment, or a conclusion that flips and flips back, are
		// distinct changes even when their words are equal.
		changes = append(changes, f.Changes...)
		for _, n := range f.Needs {
			if !slices.Contains(needs, n) {
				needs = append(needs, n)
			}
		}
	}
	// The run's needs and content are those that still hold now: at the
	// pull request as aicrewd last recorded it, which may be newer than the
	// run's last event, since a change that needs no decision (CI running
	// again) is recorded without queuing anything. A later review
	// supersedes an earlier one, and checks read for another head are not
	// this head's (hands reviews of #165).
	final := last.Observation
	cur, err := lastObservation(ctx, tx, last.AttemptID)
	if err != nil {
		return nil, err
	}
	if cur != nil {
		final = *cur
	}
	pf, err := readPullFacts(ctx, tx, last.AttemptID)
	if err != nil {
		return nil, err
	}
	var content pullContentFacts
	var omitted string
	for _, f := range parsed {
		if f.Review != nil {
			content.Review = f.Review
		}
		if f.Observation.HeadCommit != final.HeadCommit {
			continue
		}
		if f.Failed != nil {
			content.Failed = f.Failed
		}
		if f.Passed != nil {
			content.Passed = f.Passed
		}
		if f.Omitted != "" {
			omitted = f.Omitted
		}
	}
	if pf != nil && pf.Review != nil {
		content.Review = pf.Review // the newest review recorded, cut as stored
	}
	if pf != nil && pf.Head == final.HeadCommit {
		// The failed check as last observed at the head: nil once a run
		// there passed, so the need clears.
		content.PrePush, content.Streak = pf.PrePush, pf.Streak
	}
	needs = holdingNeeds(needs, final, content)
	if len(needs) == 0 {
		return nil, nil
	}
	a, err := getAttempt(ctx, tx, last.AttemptID)
	if err != nil {
		return nil, err
	}
	var recipients []string
	if slices.Contains(roles, events.RoleWorker) {
		recipients = append(recipients, a.WorkerAgentID)
	}
	if slices.Contains(roles, events.RoleCoordinator) {
		coords, err := activeCoordinators(ctx, tx, a.TeamID)
		if err != nil {
			return nil, err
		}
		for _, c := range coords {
			if !slices.Contains(recipients, c) {
				recipients = append(recipients, c)
			}
		}
	}
	if len(recipients) == 0 {
		return nil, nil
	}
	task := a.Task
	detail := &ForgeDetail{Pull: final.Number, URL: final.URL, State: final.State, HeadCommit: final.HeadCommit,
		Labels: final.Labels, Checks: final.Checks, Comments: final.Comments, Changes: changes}
	sub := ActionSubject{Kind: "pull", AttemptID: a.ID, Task: &task, Needs: needs, Pull: detail,
		Text: pullText(final, needs, content, omitted)}
	if slices.Contains(needs, NeedReview) || slices.Contains(needs, NeedGates) {
		sub.Review = content.Review
	}
	if slices.Contains(needs, NeedCI) {
		sub.Failed = content.Failed
	}
	if slices.Contains(needs, NeedGates) {
		sub.Passed = content.Passed
	}
	if slices.Contains(needs, NeedPrePush) {
		sub.PrePush = content.PrePush
	}
	return &wakeOut{recipients: recipients, forge: detail, subject: sub}, nil
}
