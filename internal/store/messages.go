package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"
)

// Team inbox (docs/CREW-CONTRACT.md, "Inbox, receipts and audit").
//
// Each team has one ordered message log. A message's recipients are fixed
// when it is sent, and each recipient has its own delivery and
// acknowledgement record. Sending, reading and acknowledging all require the
// caller's own active session at its current generation.
//
// There is no client cursor. A read always starts at the caller's oldest
// unacknowledged visible message, so a message delivered but never
// acknowledged, even across a restart, is delivered again, and no page limit
// or acknowledgement order can skip one.

// ErrNotDelivered refuses acknowledging a message that was not delivered to
// the caller: unknown, addressed to someone else, or never read.
var ErrNotDelivered = errors.New("message_not_delivered")

const (
	opSendMessage = "inbox.send"
	opAckMessages = "inbox.ack"

	maxMessageText = 16 << 10 // bytes
	// maxInboxPageBytes bounds a page of the inbox as encoded JSON, so a
	// client reads any valid page with a fixed limit: messages are added
	// while they fit, and the first always is (a message's JSON stays under
	// about 100 KiB even when every byte of its 16 KiB text is escaped).
	maxInboxPageBytes = 128 << 10
	maxInboxPage      = 100
	maxAckIDs         = 100
	maxRefLen         = 256
)

type MessageKind string

const (
	// KindMessage is written by a member. It informs; it never assigns work.
	KindMessage MessageKind = "message"
	// KindLifecycle announces a transition, in that transition's transaction.
	KindLifecycle MessageKind = "lifecycle"
)

// TaskRef names an aimem task a message is about. It grants nothing.
type TaskRef struct {
	HubID     string `json:"hub_id"`
	ProjectID string `json:"project_id"`
	TaskID    string `json:"task_id"`
}

// Message is one entry of a team's log.
type Message struct {
	ID     string      `json:"id"`
	TeamID string      `json:"team_id"`
	Seq    int64       `json:"seq"`
	Kind   MessageKind `json:"kind"`
	// The sender as it was at send time: agent, session, generation and the
	// profile it reported. A lifecycle message may have no session.
	SenderAgentID    string      `json:"sender_agent_id"`
	SenderSessionID  string      `json:"sender_session_id,omitempty"`
	SenderGeneration int64       `json:"sender_generation,omitempty"`
	SenderProfile    Profile     `json:"sender_profile"`
	To               string      `json:"to,omitempty"` // one recipient, or the team
	Project          *ProjectRef `json:"project,omitempty"`
	Task             *TaskRef    `json:"task,omitempty"`
	// AttemptID is the attempt a lifecycle message announces; a member's
	// own message names none.
	AttemptID string `json:"attempt_id,omitempty"`
	// Offer is the offer an offer's announcement makes: what the worker
	// needs to verify the pin and start its worktree.
	Offer     *OfferDetail `json:"offer,omitempty"`
	Text      string       `json:"text"`
	CreatedAt time.Time    `json:"created_at"`
}

// OfferDetail is an offer as its announcement carries it, in the shape of
// the offer route's body.
type OfferDetail struct {
	BaseCommit        string       `json:"base_commit"`
	Branch            string       `json:"branch"`
	Process           OfferProcess `json:"process"`
	InstructionDigest string       `json:"instruction_digest"`
	ExpiresAt         time.Time    `json:"expires_at"`
}

// OfferProcess is the offer's process pin, as the offer route takes it.
type OfferProcess struct {
	Repo     string `json:"repo"`
	Commit   string `json:"commit"`
	Manifest string `json:"manifest"`
}

// InboxItem is a message as delivered to one recipient.
type InboxItem struct {
	Message
	Deliveries       int64     `json:"deliveries"`
	FirstDeliveredAt time.Time `json:"first_delivered_at"`
}

// NewMessage is a member's message. Without To it goes to every other active
// member of the team; with Project it is scoped to that team project.
type NewMessage struct {
	SessionID  string      `json:"session_id"`
	Generation int64       `json:"generation"`
	To         string      `json:"to,omitempty"`
	Project    *ProjectRef `json:"project,omitempty"`
	Task       *TaskRef    `json:"task,omitempty"`
	Text       string      `json:"text"`
}

// AckResult lists the messages this acknowledgement recorded and those that
// were already acknowledged.
type AckResult struct {
	Acknowledged []string `json:"acknowledged"`
	Already      []string `json:"already"`
}

type ackRequest struct {
	SessionID  string   `json:"session_id"`
	Generation int64    `json:"generation"`
	IDs        []string `json:"ids"`
}

func (m NewMessage) validate() error {
	if m.SessionID == "" || m.Generation < 1 {
		return fmt.Errorf("%w: a session and generation are required", ErrInvalid)
	}
	if err := validateMessageText(m.Text); err != nil {
		return err
	}
	if len(m.To) > maxRefLen {
		return fmt.Errorf("%w: recipient is too long", ErrInvalid)
	}
	if p := m.Project; p != nil && !validRefs(p.HubID, p.ProjectID) {
		return fmt.Errorf("%w: project needs a hub and a project ID", ErrInvalid)
	}
	if t := m.Task; t != nil && !validRefs(t.HubID, t.ProjectID, t.TaskID) {
		return fmt.Errorf("%w: task needs a hub, a project and a task ID", ErrInvalid)
	}
	return nil
}

// validateMessageText is the one text rule for member and lifecycle
// messages alike. Lifecycle text is built inside a transition, where the
// command path's input check does not see it, so the rule checks UTF-8 too.
func validateMessageText(text string) error {
	if !utf8.ValidString(text) {
		return fmt.Errorf("%w: message text is not valid UTF-8", ErrInvalid)
	}
	if strings.TrimSpace(text) == "" || len(text) > maxMessageText {
		return fmt.Errorf("%w: message text must be non-blank and at most %d bytes", ErrInvalid, maxMessageText)
	}
	return nil
}

func validRefs(refs ...string) bool {
	for _, r := range refs {
		if r == "" || len(r) > maxRefLen {
			return false
		}
	}
	return true
}

// sessionCurrent is a replay check: a recorded result is returned only while
// the session that produced it is still active at the same generation.
func sessionCurrent(c Caller, sessionID string, generation int64) func(context.Context, *sql.Tx, string) error {
	return func(ctx context.Context, tx *sql.Tx, _ string) error {
		_, err := currentSession(ctx, tx, c, sessionID, generation)
		return err
	}
}

// SendMessage adds a member's message to its team's log. The message goes to
// the named active member, or to every other active member; either way the
// recipients are fixed now. A retry with the same key returns the original
// message while the session is still at the same generation.
func (s *Store) SendMessage(ctx context.Context, c Caller, key string, in NewMessage) (Message, error) {
	var out Message
	err := s.run(ctx, c, command{
		op: opSendMessage, scope: in.SessionID, key: key, input: in,
		authorize: requireAgent, validate: in.validate,
		replayCheck: sessionCurrent(c, in.SessionID, in.Generation),
		check: func(ctx context.Context, tx *sql.Tx) error {
			_, err := currentSession(ctx, tx, c, in.SessionID, in.Generation)
			return err
		},
		apply: func(ctx context.Context, tx *sql.Tx, now time.Time) (any, error) {
			sess, err := getSession(ctx, tx, in.SessionID)
			if err != nil {
				return nil, err
			}
			recipients, err := memberRecipients(ctx, tx, sess.TeamID, sess.AgentID, in.To)
			if err != nil {
				return nil, err
			}
			if in.Project != nil {
				if err := requireTeamProject(ctx, tx, sess.TeamID, *in.Project); err != nil {
					return nil, err
				}
			}
			sender, err := getAgent(ctx, tx, sess.AgentID)
			if err != nil {
				return nil, err
			}
			return insertMessage(ctx, tx, Message{
				TeamID: sess.TeamID, Kind: KindMessage,
				SenderAgentID: sender.ID, SenderSessionID: sess.ID, SenderGeneration: sess.Generation,
				SenderProfile: sender.Profile,
				To:            in.To, Project: in.Project, Task: in.Task, Text: in.Text,
			}, recipients, now)
		},
	}, &out)
	return out, err
}

// postLifecycle writes a lifecycle message inside the transaction of the
// transition it announces, so both commit or neither does. It goes to every
// active member except the actor. Crew-execution's transitions call it.
func postLifecycle(ctx context.Context, tx *sql.Tx, teamID, actorAgentID, text string, task *TaskRef, attemptID string,
	now time.Time) (Message, error) {
	return postLifecycleMessage(ctx, tx, Message{TeamID: teamID, Kind: KindLifecycle, SenderAgentID: actorAgentID,
		Task: task, AttemptID: attemptID, Text: text}, now)
}

// postLifecycleMessage writes a lifecycle message m to every active member
// but its actor.
func postLifecycleMessage(ctx context.Context, tx *sql.Tx, m Message, now time.Time) (Message, error) {
	if err := validateMessageText(m.Text); err != nil {
		return Message{}, err
	}
	actorAgentID := m.SenderAgentID
	recipients, err := activeMembersExcept(ctx, tx, m.TeamID, actorAgentID)
	if err != nil {
		return Message{}, err
	}
	if actorAgentID != "" {
		actor, err := getAgent(ctx, tx, actorAgentID)
		if err != nil {
			return Message{}, err
		}
		m.SenderProfile = actor.Profile
	}
	return insertMessage(ctx, tx, m, recipients, now)
}

// memberRecipients returns the recipients of a member's message: the named
// active member, or every other active member of the team.
func memberRecipients(ctx context.Context, tx *sql.Tx, teamID, senderID, to string) ([]string, error) {
	if to == "" {
		recipients, err := activeMembersExcept(ctx, tx, teamID, senderID)
		if err == nil && len(recipients) == 0 {
			err = fmt.Errorf("%w: the team has no other active member", ErrInvalid)
		}
		return recipients, err
	}
	if to == senderID {
		return nil, fmt.Errorf("%w: a message cannot be sent to its sender", ErrInvalid)
	}
	if _, err := getMembership(ctx, tx, teamID, to); errors.Is(err, ErrNotFound) {
		return nil, fmt.Errorf("%w: recipient %s is not an active member of the team", ErrInvalid, to)
	} else if err != nil {
		return nil, err
	}
	return []string{to}, nil
}

func activeMembersExcept(ctx context.Context, tx *sql.Tx, teamID, agentID string) ([]string, error) {
	rows, err := tx.QueryContext(ctx,
		`SELECT agent_id FROM memberships WHERE team_id = ? AND removed = 0 AND agent_id != ?
		 ORDER BY agent_id`, teamID, agentID)
	if err != nil {
		return nil, fmt.Errorf("list recipients: %w", err)
	}
	defer rows.Close()
	ids := []string{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

func requireTeamProject(ctx context.Context, q querier, teamID string, p ProjectRef) error {
	var one int
	err := q.QueryRowContext(ctx,
		`SELECT 1 FROM team_projects WHERE team_id = ? AND hub_id = ? AND project_id = ?`,
		teamID, p.HubID, p.ProjectID).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("%w: project %s/%s is not in the team's project set", ErrInvalid, p.HubID, p.ProjectID)
	}
	return err
}

// insertMessage appends m to its team's log with the next sequence number.
// Writers are serialized, so the sequence has no gaps or repeats; the unique
// (team, sequence) index backs that up.
func insertMessage(ctx context.Context, tx *sql.Tx, m Message, recipients []string, now time.Time) (Message, error) {
	id, err := newID(now)
	if err != nil {
		return Message{}, err
	}
	m.ID, m.CreatedAt = id, now
	if err := tx.QueryRowContext(ctx,
		`SELECT COALESCE(MAX(seq), 0) + 1 FROM messages WHERE team_id = ?`, m.TeamID).Scan(&m.Seq); err != nil {
		return Message{}, fmt.Errorf("next message sequence: %w", err)
	}
	var project ProjectRef
	if m.Project != nil {
		project = *m.Project
	}
	var task TaskRef
	if m.Task != nil {
		task = *m.Task
	}
	offer := ""
	if m.Offer != nil {
		b, err := json.Marshal(m.Offer)
		if err != nil {
			return Message{}, fmt.Errorf("encode offer: %w", err)
		}
		offer = string(b)
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO messages (id, team_id, seq, kind, sender_agent_id, sender_session_id, sender_generation,
		        sender_model, sender_client, sender_client_version, to_agent_id, project_hub_id, project_id,
		        task_hub_id, task_project_id, task_id, attempt_id, offer, text, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		m.ID, m.TeamID, m.Seq, string(m.Kind), m.SenderAgentID, m.SenderSessionID, m.SenderGeneration,
		m.SenderProfile.Model, m.SenderProfile.Client, m.SenderProfile.ClientVersion, m.To,
		project.HubID, project.ProjectID, task.HubID, task.ProjectID, task.TaskID, m.AttemptID, offer,
		m.Text, formatTime(now)); err != nil {
		return Message{}, fmt.Errorf("insert message: %w", err)
	}
	for _, r := range recipients {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO message_recipients (message_id, agent_id) VALUES (?, ?)`, m.ID, r); err != nil {
			return Message{}, fmt.Errorf("insert recipient: %w", err)
		}
	}
	return m, nil
}

// ReadInbox delivers the caller's oldest unacknowledged messages in its
// session's team, at most limit of them (1 to 100), in sequence order, and
// records the delivery. Acknowledged messages and messages scoped to a
// project no longer in the team's set are not returned. A message stays in
// every later read until it is acknowledged.
func (s *Store) ReadInbox(ctx context.Context, c Caller, sessionID string, generation int64, limit int) ([]InboxItem, error) {
	return s.readInbox(ctx, c, sessionID, generation, limit, nil)
}

// ReadInboxWithToken reads the inbox of the token's session, as ReadInbox
// does; the token is rechecked inside the read's transaction.
func (s *Store) ReadInboxWithToken(ctx context.Context, token string, limit int) ([]InboxItem, error) {
	t, c, err := s.tokenCaller(ctx, token)
	if err != nil {
		return nil, err
	}
	return s.readInbox(ctx, c, t.sessionID, t.generation, limit, s.requireToken(token, t.sessionID, t.generation, nil))
}

// readInbox is ReadInbox, with check run first inside its transaction.
func (s *Store) readInbox(ctx context.Context, c Caller, sessionID string, generation int64, limit int,
	check func(context.Context, *sql.Tx) error) ([]InboxItem, error) {
	if err := requireAgent(c); err != nil {
		return nil, err
	}
	if limit < 1 || limit > maxInboxPage {
		return nil, fmt.Errorf("%w: limit must be 1-%d", ErrInvalid, maxInboxPage)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("begin: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck // no-op after commit
	if check != nil {
		if err := check(ctx, tx); err != nil {
			return nil, err
		}
	}
	sess, err := currentSession(ctx, tx, c, sessionID, generation)
	if err != nil {
		return nil, err
	}
	items, err := pendingMessages(ctx, tx, sess.TeamID, sess.AgentID, limit)
	if err != nil {
		return nil, err
	}
	// Only what fits the page is delivered; the rest stays first in line.
	items = fitPage(items, maxInboxPageBytes)
	now := s.now()
	at := formatTime(now)
	for i := range items {
		if _, err := tx.ExecContext(ctx,
			`UPDATE message_recipients
			 SET deliveries = deliveries + 1, first_delivered_at = COALESCE(first_delivered_at, ?), last_delivered_at = ?
			 WHERE message_id = ? AND agent_id = ?`, at, at, items[i].ID, sess.AgentID); err != nil {
			return nil, fmt.Errorf("record delivery: %w", err)
		}
		items[i].Deliveries++
		if items[i].FirstDeliveredAt.IsZero() {
			if items[i].FirstDeliveredAt, err = parseTime(at); err != nil {
				return nil, err
			}
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit: %w", err)
	}
	return items, nil
}

// fitPage keeps the leading items whose JSON fits budget bytes, and always
// the first.
func fitPage(items []InboxItem, budget int) []InboxItem {
	total := 0
	for i, it := range items {
		b, _ := json.Marshal(it)
		if i > 0 && total+len(b) > budget {
			return items[:i]
		}
		total += len(b)
	}
	return items
}

func pendingMessages(ctx context.Context, q querier, teamID, agentID string, limit int) ([]InboxItem, error) {
	rows, err := q.QueryContext(ctx,
		`SELECT m.id, m.team_id, m.seq, m.kind, m.sender_agent_id, m.sender_session_id, m.sender_generation,
		        m.sender_model, m.sender_client, m.sender_client_version, m.to_agent_id,
		        m.project_hub_id, m.project_id, m.task_hub_id, m.task_project_id, m.task_id, m.attempt_id, m.offer,
		        m.text, m.created_at, r.deliveries, r.first_delivered_at
		 FROM message_recipients r JOIN messages m ON m.id = r.message_id
		 WHERE r.agent_id = ? AND m.team_id = ? AND r.acknowledged_at IS NULL
		   AND (m.project_id = '' OR EXISTS (SELECT 1 FROM team_projects p
		        WHERE p.team_id = m.team_id AND p.hub_id = m.project_hub_id AND p.project_id = m.project_id))
		 ORDER BY m.seq LIMIT ?`, agentID, teamID, limit)
	if err != nil {
		return nil, fmt.Errorf("read inbox: %w", err)
	}
	defer rows.Close()
	items := []InboxItem{}
	for rows.Next() {
		var (
			it             InboxItem
			kind, created  string
			project        ProjectRef
			task           TaskRef
			offer          string
			firstDelivered sql.NullString
		)
		if err := rows.Scan(&it.ID, &it.TeamID, &it.Seq, &kind, &it.SenderAgentID, &it.SenderSessionID,
			&it.SenderGeneration, &it.SenderProfile.Model, &it.SenderProfile.Client,
			&it.SenderProfile.ClientVersion, &it.To, &project.HubID, &project.ProjectID,
			&task.HubID, &task.ProjectID, &task.TaskID, &it.AttemptID, &offer, &it.Text, &created,
			&it.Deliveries, &firstDelivered); err != nil {
			return nil, err
		}
		it.Kind = MessageKind(kind)
		if project.ProjectID != "" {
			it.Project = &project
		}
		if task.TaskID != "" {
			it.Task = &task
		}
		if offer != "" {
			var o OfferDetail
			if err := json.Unmarshal([]byte(offer), &o); err != nil {
				return nil, fmt.Errorf("read the offer of message %s: %w", it.ID, err)
			}
			it.Offer = &o
		}
		if it.CreatedAt, err = parseTime(created); err != nil {
			return nil, err
		}
		if firstDelivered.Valid {
			if it.FirstDeliveredAt, err = parseTime(firstDelivered.String); err != nil {
				return nil, err
			}
		}
		items = append(items, it)
	}
	return items, rows.Err()
}

// AckMessages acknowledges messages delivered to the caller in its session's
// team. Every ID must have been delivered to the caller, or nothing is
// recorded; an ID acknowledged before is reported, not an error.
func (s *Store) AckMessages(ctx context.Context, c Caller, key, sessionID string, generation int64, ids []string) (AckResult, error) {
	var out AckResult
	err := s.run(ctx, c, ackCommand(c, key, sessionID, generation, ids), &out)
	return out, err
}

// AckWithToken acknowledges messages delivered to the token's session, as
// AckMessages does; the token is rechecked inside the command.
func (s *Store) AckWithToken(ctx context.Context, key, token string, ids []string) (AckResult, error) {
	t, c, err := s.tokenCaller(ctx, token)
	if err != nil {
		return AckResult{}, err
	}
	cmd, _ := s.withToken(ackCommand(c, key, t.sessionID, t.generation, ids), token, t)
	var out AckResult
	err = s.run(ctx, c, cmd, &out)
	return out, err
}

func ackCommand(c Caller, key, sessionID string, generation int64, ids []string) command {
	in := ackRequest{SessionID: sessionID, Generation: generation, IDs: ids}
	return command{
		op: opAckMessages, scope: sessionID, key: key, input: in,
		authorize: requireAgent,
		validate: func() error {
			if len(ids) == 0 || len(ids) > maxAckIDs {
				return fmt.Errorf("%w: acknowledge 1-%d messages at a time", ErrInvalid, maxAckIDs)
			}
			seen := map[string]bool{}
			for _, id := range ids {
				if id == "" || seen[id] {
					return fmt.Errorf("%w: message IDs must be non-empty and distinct", ErrInvalid)
				}
				seen[id] = true
			}
			return nil
		},
		replayCheck: sessionCurrent(c, sessionID, generation),
		check: func(ctx context.Context, tx *sql.Tx) error {
			_, err := currentSession(ctx, tx, c, sessionID, generation)
			return err
		},
		apply: func(ctx context.Context, tx *sql.Tx, now time.Time) (any, error) {
			sess, err := getSession(ctx, tx, sessionID)
			if err != nil {
				return nil, err
			}
			res := AckResult{Acknowledged: []string{}, Already: []string{}}
			for _, id := range ids {
				var (
					deliveries int64
					acked      sql.NullString
				)
				err := tx.QueryRowContext(ctx,
					`SELECT r.deliveries, r.acknowledged_at FROM message_recipients r
					 JOIN messages m ON m.id = r.message_id
					 WHERE r.message_id = ? AND r.agent_id = ? AND m.team_id = ?`,
					id, sess.AgentID, sess.TeamID).Scan(&deliveries, &acked)
				if errors.Is(err, sql.ErrNoRows) || (err == nil && deliveries == 0) {
					return nil, fmt.Errorf("message %s: %w", id, ErrNotDelivered)
				}
				if err != nil {
					return nil, fmt.Errorf("read recipient: %w", err)
				}
				if acked.Valid {
					res.Already = append(res.Already, id)
					continue
				}
				if _, err := tx.ExecContext(ctx,
					`UPDATE message_recipients SET acknowledged_at = ? WHERE message_id = ? AND agent_id = ?`,
					formatTime(now), id, sess.AgentID); err != nil {
					return nil, fmt.Errorf("acknowledge: %w", err)
				}
				res.Acknowledged = append(res.Acknowledged, id)
			}
			return res, nil
		},
	}
}
