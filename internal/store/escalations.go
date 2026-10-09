package store

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode/utf8"
)

// Escalations (docs/DESIGN-CONTROL-PLANE.md, section 7.5; task 5598). The
// team's current coordinator raises a question it cannot settle within its
// role; the architect, the operator's own session, answers it through the
// operator API with an architect credential, or the operator with the
// operator credential. The answer is recorded once, and in the same
// transaction a lifecycle message carrying it goes to the coordinator and
// to the member the request names as blocked: their inbox, which wakes them.
// This store is the record; a task comment is only its mirror, and a comment
// without a recorded answer authorizes nothing.

// ErrEscalationAnswered refuses a second answer to an escalation.
var ErrEscalationAnswered = errors.New("escalation_answered")

const (
	opRaiseEscalation  = "escalation.raise"
	opAnswerEscalation = "escalation.answer"
	opIssueArchitect   = "architect.credential.issue"
	opRevokeArchitect  = "architect.credential.revoke"

	maxEscalationQuestion = 1 << 10
	maxEscalationContext  = 8 << 10
	maxEscalationOption   = 512
	maxEscalationDecision = 1 << 10
	maxEscalationReason   = 4 << 10
	maxEscalationList     = 200

	// ArchitectCredentialLifetime bounds an architect credential.
	ArchitectCredentialLifetime = 90 * 24 * time.Hour
	architectPrefix             = "aar_"
	maxArchitectLabel           = 64
)

var architectShape = regexp.MustCompile(`^aar_[0-9a-f]{64}$`)

// EscalationCategories are OPERATOR-SEAT's categories (§2); its floor is
// architecture, wire, security, risk, merge and deploy.
var EscalationCategories = []string{"architecture", "wire", "security", "risk", "merge", "deploy", "scope",
	"cross_repo", "intent", "process", "implementation", "environment", "retry"}

// EscalationUrgencies say who waits how long.
var EscalationUrgencies = []string{"now", "today", "next_session"}

// EscalationOption is one answer the coordinator offers, with its
// consequence.
type EscalationOption struct {
	Option      string `json:"option"`
	Consequence string `json:"consequence"`
}

// EscalationRequest is what the coordinator raises (OPERATOR-SEAT §3).
type EscalationRequest struct {
	Task           TaskRef            `json:"task"`
	AttemptID      string             `json:"attempt_id,omitempty"`
	Category       string             `json:"category"`
	Question       string             `json:"question"`
	Context        string             `json:"context,omitempty"`
	Options        []EscalationOption `json:"options"`
	Recommendation string             `json:"recommendation"`
	// Blocked is the member that waits for the answer, if any; the answer
	// reaches it besides the coordinator.
	Blocked string `json:"blocked,omitempty"`
	Urgency string `json:"urgency"`
}

// EscalationAnswer is the architect's or operator's decision.
type EscalationAnswer struct {
	Decision   string    `json:"decision"`
	Rationale  string    `json:"rationale"`
	AnsweredBy string    `json:"answered_by,omitempty"`
	AnsweredAt time.Time `json:"answered_at,omitzero"`
}

// Escalation is one request and, once given, its answer.
type Escalation struct {
	ID          string `json:"id"`
	TeamID      string `json:"team_id"`
	Coordinator string `json:"coordinator_agent_id"`
	EscalationRequest
	CreatedAt time.Time         `json:"created_at"`
	Answer    *EscalationAnswer `json:"answer,omitempty"`
}

// EscalationDetail is what an answer's lifecycle message carries.
type EscalationDetail struct {
	ID        string `json:"id"`
	Category  string `json:"category"`
	Question  string `json:"question"`
	Decision  string `json:"decision"`
	Rationale string `json:"rationale"`
}

func validText(s string, max int, what string, required bool) error {
	if !utf8.ValidString(s) || strings.ContainsRune(s, 0) {
		return fmt.Errorf("%w: %s is not valid UTF-8", ErrInvalid, what)
	}
	if required && strings.TrimSpace(s) == "" {
		return fmt.Errorf("%w: %s is required", ErrInvalid, what)
	}
	if len(s) > max {
		return fmt.Errorf("%w: %s is longer than %d bytes", ErrInvalid, what, max)
	}
	return nil
}

func (r EscalationRequest) validate() error {
	if !validRefs(r.Task.HubID, r.Task.ProjectID, r.Task.TaskID) {
		return fmt.Errorf("%w: an escalation names its task: hub, project and task ID", ErrInvalid)
	}
	if len(r.AttemptID) > maxRefLen || len(r.Blocked) > maxRefLen {
		return fmt.Errorf("%w: an attempt or member ID is too long", ErrInvalid)
	}
	if !slices.Contains(EscalationCategories, r.Category) {
		return fmt.Errorf("%w: category is one of %s", ErrInvalid, strings.Join(EscalationCategories, ", "))
	}
	if !slices.Contains(EscalationUrgencies, r.Urgency) {
		return fmt.Errorf("%w: urgency is one of %s", ErrInvalid, strings.Join(EscalationUrgencies, ", "))
	}
	if err := validText(r.Question, maxEscalationQuestion, "the question", true); err != nil {
		return err
	}
	if err := validText(r.Context, maxEscalationContext, "the context", false); err != nil {
		return err
	}
	if err := validText(r.Recommendation, maxEscalationQuestion, "the recommendation", true); err != nil {
		return err
	}
	if len(r.Options) < 2 || len(r.Options) > 4 {
		return fmt.Errorf("%w: an escalation offers two to four options", ErrInvalid)
	}
	for _, o := range r.Options {
		if err := validText(o.Option, maxEscalationOption, "an option", true); err != nil {
			return err
		}
		if err := validText(o.Consequence, maxEscalationOption, "an option's consequence", true); err != nil {
			return err
		}
	}
	return nil
}

func (a EscalationAnswer) validate() error {
	if err := validText(a.Decision, maxEscalationDecision, "the decision", true); err != nil {
		return err
	}
	return validText(a.Rationale, maxEscalationReason, "the rationale", true)
}

// RaiseEscalationWithToken records an escalation raised by the token's
// session, which must be its team's current coordinator. The task's project
// must be granted to the team; a named attempt must be the team's, on the
// same task; a named blocked member must be an active member of the team. A
// retry with the same key returns the original while the session is current.
func (s *Store) RaiseEscalationWithToken(ctx context.Context, key, token string, in EscalationRequest) (Escalation, error) {
	t, c, err := s.tokenCaller(ctx, token)
	if err != nil {
		return Escalation{}, err
	}
	input := struct {
		SessionID  string `json:"session_id"`
		Generation int64  `json:"generation"`
		EscalationRequest
	}{t.sessionID, t.generation, in}
	cmd, _ := s.withToken(command{
		op: opRaiseEscalation, scope: t.sessionID, key: key, input: input,
		authorize: requireAgent, validate: in.validate,
		replayCheck: sessionCurrent(c, t.sessionID, t.generation),
		check: func(ctx context.Context, tx *sql.Tx) error {
			_, err := currentCoordinator(ctx, tx, c, t.sessionID, t.generation)
			return err
		},
		apply: func(ctx context.Context, tx *sql.Tx, now time.Time) (any, error) {
			sess, err := getSession(ctx, tx, t.sessionID)
			if err != nil {
				return nil, err
			}
			if err := requireTeamGrant(ctx, tx, sess.TeamID, ProjectRef{HubID: in.Task.HubID, ProjectID: in.Task.ProjectID}); err != nil {
				return nil, err
			}
			if in.AttemptID != "" {
				a, err := getAttempt(ctx, tx, in.AttemptID)
				if errors.Is(err, ErrNotFound) || (err == nil && (a.TeamID != sess.TeamID || a.Task != in.Task)) {
					return nil, fmt.Errorf("%w: attempt %s is not the team's attempt on this task", ErrInvalid, in.AttemptID)
				}
				if err != nil {
					return nil, err
				}
			}
			if in.Blocked != "" {
				if in.Blocked == sess.AgentID {
					return nil, fmt.Errorf("%w: the coordinator receives the answer anyway; name a blocked member only", ErrInvalid)
				}
				if _, err := getMembership(ctx, tx, sess.TeamID, in.Blocked); errors.Is(err, ErrNotFound) {
					return nil, fmt.Errorf("%w: %s is not an active member of the team", ErrInvalid, in.Blocked)
				} else if err != nil {
					return nil, err
				}
			}
			id, err := newID(now)
			if err != nil {
				return nil, err
			}
			req, _ := json.Marshal(in)
			if _, err := tx.ExecContext(ctx,
				`INSERT INTO escalations (id, team_id, coordinator_agent_id, session_id, task_hub_id, task_project_id,
				        task_id, attempt_id, blocked_agent_id, request, created_at)
				 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
				id, sess.TeamID, sess.AgentID, sess.ID, in.Task.HubID, in.Task.ProjectID, in.Task.TaskID,
				in.AttemptID, in.Blocked, string(req), formatTime(now)); err != nil {
				return nil, fmt.Errorf("insert escalation: %w", err)
			}
			return Escalation{ID: id, TeamID: sess.TeamID, Coordinator: sess.AgentID, EscalationRequest: in, CreatedAt: now}, nil
		},
	}, token, t)
	var out Escalation
	err = s.run(ctx, c, cmd, &out)
	return out, err
}

// AnswerEscalation records the answer to an open escalation, once, as the
// operator or an architect credential; the answer names which. In the same
// transaction a lifecycle message carrying the answer goes to the team's
// coordinator, which may have changed since the request, and to the blocked
// member while it is still an active member. A second answer is
// ErrEscalationAnswered; a retry of the same answer with the same key
// returns it.
func (s *Store) AnswerEscalation(ctx context.Context, c Caller, key, id string, in EscalationAnswer) (Escalation, error) {
	in.AnsweredBy, in.AnsweredAt = "", time.Time{}
	var out Escalation
	err := s.run(ctx, c, command{
		op: opAnswerEscalation, scope: id, key: key, input: struct {
			ID string `json:"id"`
			EscalationAnswer
		}{id, in},
		authorize: requireEscalationReader,
		validate: func() error {
			if id == "" || len(id) > maxRefLen {
				return fmt.Errorf("%w: name the escalation's ID", ErrInvalid)
			}
			return in.validate()
		},
		apply: func(ctx context.Context, tx *sql.Tx, now time.Time) (any, error) {
			e, err := getEscalation(ctx, tx, id)
			if err != nil {
				return nil, err
			}
			if e.Answer != nil {
				return nil, fmt.Errorf("%w: escalation %s was answered at %s", ErrEscalationAnswered, id, formatTime(e.Answer.AnsweredAt))
			}
			in.AnsweredBy, in.AnsweredAt = c.String(), now
			ans, _ := json.Marshal(in)
			if _, err := tx.ExecContext(ctx,
				`UPDATE escalations SET answer = ?, answered_by = ?, answered_at = ? WHERE id = ? AND answered_at = ''`,
				string(ans), in.AnsweredBy, formatTime(now), id); err != nil {
				return nil, fmt.Errorf("record answer: %w", err)
			}
			e.Answer = &in
			// The team's coordinator now, which may not be the one that
			// raised it, and the blocked member, while still a member.
			recipients, err := activeCoordinators(ctx, tx, e.TeamID)
			if err != nil {
				return nil, err
			}
			if e.Blocked != "" && !slices.Contains(recipients, e.Blocked) {
				if _, err := getMembership(ctx, tx, e.TeamID, e.Blocked); err == nil {
					recipients = append(recipients, e.Blocked)
				} else if !errors.Is(err, ErrNotFound) {
					return nil, err
				}
			}
			task := e.Task
			text := fmt.Sprintf("Escalation %s on task %s is answered (%s): %s", e.ID, task.TaskID, e.Category,
				truncateText(in.Decision, maxEscalationDecision))
			if _, err := insertMessage(ctx, tx, Message{TeamID: e.TeamID, Kind: KindLifecycle, Task: &task,
				AttemptID: e.AttemptID, Text: text, Escalation: &EscalationDetail{ID: e.ID, Category: e.Category,
					Question: e.Question, Decision: in.Decision, Rationale: in.Rationale}}, recipients, now); err != nil {
				return nil, err
			}
			return e, nil
		},
	}, &out)
	return out, err
}

func truncateText(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n] + "…"
}

// ListEscalations returns a team's escalations, or every team's when
// teamID is empty, newest first and at most 200, the open ones only when
// open is set. The operator or an architect credential.
func (s *Store) ListEscalations(ctx context.Context, c Caller, teamID string, open bool) ([]Escalation, error) {
	if err := requireEscalationReader(c); err != nil {
		return nil, err
	}
	q := `SELECT ` + escalationColumns + ` FROM escalations WHERE (? = '' OR team_id = ?)`
	if open {
		q += ` AND answered_at = ''`
	}
	q += ` ORDER BY created_at DESC, id DESC LIMIT ?`
	out := []Escalation{}
	err := s.snapshot(ctx, func(qr querier) error {
		rows, err := qr.QueryContext(ctx, q, teamID, teamID, maxEscalationList)
		if err != nil {
			return fmt.Errorf("list escalations: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			e, err := scanEscalation(rows)
			if err != nil {
				return err
			}
			out = append(out, e)
		}
		return rows.Err()
	})
	return out, err
}

// GetEscalation reads one escalation. The operator or an architect
// credential.
func (s *Store) GetEscalation(ctx context.Context, c Caller, id string) (Escalation, error) {
	if err := requireEscalationReader(c); err != nil {
		return Escalation{}, err
	}
	var e Escalation
	err := s.snapshot(ctx, func(q querier) error {
		var err error
		e, err = getEscalation(ctx, q, id)
		return err
	})
	return e, err
}

const escalationColumns = `id, team_id, coordinator_agent_id, request, created_at, answer`

func scanEscalation(r rowScanner) (Escalation, error) {
	var e Escalation
	var req, created, ans string
	if err := r.Scan(&e.ID, &e.TeamID, &e.Coordinator, &req, &created, &ans); err != nil {
		return Escalation{}, err
	}
	if err := json.Unmarshal([]byte(req), &e.EscalationRequest); err != nil {
		return Escalation{}, fmt.Errorf("read escalation %s: %w", e.ID, err)
	}
	var err error
	if e.CreatedAt, err = parseTime(created); err != nil {
		return Escalation{}, err
	}
	if ans != "" {
		var a EscalationAnswer
		if err := json.Unmarshal([]byte(ans), &a); err != nil {
			return Escalation{}, fmt.Errorf("read the answer of escalation %s: %w", e.ID, err)
		}
		e.Answer = &a
	}
	return e, nil
}

func getEscalation(ctx context.Context, q querier, id string) (Escalation, error) {
	e, err := scanEscalation(q.QueryRowContext(ctx, `SELECT `+escalationColumns+` FROM escalations WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return Escalation{}, fmt.Errorf("%w: escalation %s", ErrNotFound, id)
	}
	return e, err
}

// ArchitectCredential is an architect credential's metadata; never the
// bearer. It authorizes the escalation routes only (decision D10).
type ArchitectCredential struct {
	ID        string    `json:"id"`
	Label     string    `json:"label"`
	CreatedAt time.Time `json:"created_at"`
	ExpiresAt time.Time `json:"expires_at"`
	RevokedAt time.Time `json:"revoked_at,omitzero"`
}

// Active reports whether the credential authenticates at now.
func (a ArchitectCredential) Active(now time.Time) bool {
	return a.RevokedAt.IsZero() && now.Before(a.ExpiresAt)
}

var architectLabelShape = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,63}$`)

// IssueArchitectCredential issues an architect credential. The operator
// only. It returns the metadata and the bearer, which exists nowhere else: a
// replay of the same key returns the metadata and an empty bearer.
func (s *Store) IssueArchitectCredential(ctx context.Context, c Caller, key, label string) (ArchitectCredential, string, error) {
	var out ArchitectCredential
	var bearer string
	err := s.run(ctx, c, command{
		op: opIssueArchitect, scope: label, key: key, input: struct {
			Label string `json:"label"`
		}{label},
		authorize: requireOperator,
		validate: func() error {
			if !architectLabelShape.MatchString(label) {
				return fmt.Errorf("%w: a label is 1 to %d characters of lowercase letters, digits and '-'", ErrInvalid, maxArchitectLabel)
			}
			return nil
		},
		apply: func(ctx context.Context, tx *sql.Tx, now time.Time) (any, error) {
			var raw [32]byte
			if _, err := rand.Read(raw[:]); err != nil {
				return nil, err
			}
			secret := architectPrefix + hex.EncodeToString(raw[:])
			id, err := newID(now)
			if err != nil {
				return nil, err
			}
			cred := ArchitectCredential{ID: id, Label: label, CreatedAt: now, ExpiresAt: now.Add(ArchitectCredentialLifetime)}
			if _, err := tx.ExecContext(ctx,
				`INSERT INTO architect_credentials (id, label, digest, created_at, expires_at) VALUES (?, ?, ?, ?, ?)`,
				cred.ID, label, secretDigest(secret), formatTime(cred.CreatedAt), formatTime(cred.ExpiresAt)); err != nil {
				return nil, fmt.Errorf("insert architect credential: %w", err)
			}
			bearer = secret
			return cred, nil
		},
	}, &out)
	if err != nil {
		return ArchitectCredential{}, "", err
	}
	return out, bearer, nil
}

// RevokeArchitectCredential revokes one credential. The operator only.
// Revoking a revoked credential changes nothing.
func (s *Store) RevokeArchitectCredential(ctx context.Context, c Caller, key, id string) (ArchitectCredential, error) {
	var out ArchitectCredential
	err := s.run(ctx, c, command{
		op: opRevokeArchitect, scope: id, key: key, input: struct {
			ID string `json:"id"`
		}{id},
		authorize: requireOperator,
		validate: func() error {
			if id == "" || len(id) > maxRefLen {
				return fmt.Errorf("%w: name the credential's ID", ErrInvalid)
			}
			return nil
		},
		apply: func(ctx context.Context, tx *sql.Tx, now time.Time) (any, error) {
			if _, err := tx.ExecContext(ctx,
				`UPDATE architect_credentials SET revoked_at = ? WHERE id = ? AND revoked_at = ''`, formatTime(now), id); err != nil {
				return nil, fmt.Errorf("revoke architect credential: %w", err)
			}
			return getArchitectCredential(ctx, tx, `id = ?`, id)
		},
	}, &out)
	return out, err
}

// ListArchitectCredentials lists the architect credentials' metadata. The
// operator only.
func (s *Store) ListArchitectCredentials(ctx context.Context, c Caller) ([]ArchitectCredential, error) {
	if err := requireOperator(c); err != nil {
		return nil, err
	}
	out := []ArchitectCredential{}
	err := s.snapshot(ctx, func(q querier) error {
		rows, err := q.QueryContext(ctx, `SELECT `+architectColumns+` FROM architect_credentials ORDER BY created_at, id`)
		if err != nil {
			return fmt.Errorf("list architect credentials: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			a, err := scanArchitectCredential(rows)
			if err != nil {
				return err
			}
			out = append(out, a)
		}
		return rows.Err()
	})
	return out, err
}

// AuthenticateArchitect returns the caller an active architect credential
// acts as, or ErrUnauthenticated: an unknown, revoked or expired bearer
// answers alike.
func (s *Store) AuthenticateArchitect(ctx context.Context, bearer string) (Caller, error) {
	if !architectShape.MatchString(bearer) {
		return Caller{}, ErrUnauthenticated
	}
	var a ArchitectCredential
	err := s.snapshot(ctx, func(q querier) error {
		var err error
		a, err = getArchitectCredential(ctx, q, `digest = ?`, secretDigest(bearer))
		return err
	})
	if errors.Is(err, ErrNotFound) || (err == nil && !a.Active(s.now())) {
		return Caller{}, ErrUnauthenticated
	}
	if err != nil {
		return Caller{}, err
	}
	return Caller{kind: callerArchitect, id: a.ID}, nil
}

const architectColumns = `id, label, created_at, expires_at, revoked_at`

func scanArchitectCredential(r rowScanner) (ArchitectCredential, error) {
	var a ArchitectCredential
	var created, expires, revoked string
	if err := r.Scan(&a.ID, &a.Label, &created, &expires, &revoked); err != nil {
		return ArchitectCredential{}, err
	}
	var err error
	if a.CreatedAt, err = parseTime(created); err != nil {
		return ArchitectCredential{}, err
	}
	if a.ExpiresAt, err = parseTime(expires); err != nil {
		return ArchitectCredential{}, err
	}
	if revoked != "" {
		if a.RevokedAt, err = parseTime(revoked); err != nil {
			return ArchitectCredential{}, err
		}
	}
	return a, nil
}

func getArchitectCredential(ctx context.Context, q querier, where string, arg any) (ArchitectCredential, error) {
	a, err := scanArchitectCredential(q.QueryRowContext(ctx, `SELECT `+architectColumns+` FROM architect_credentials WHERE `+where, arg))
	if errors.Is(err, sql.ErrNoRows) {
		return ArchitectCredential{}, fmt.Errorf("%w: architect credential", ErrNotFound)
	}
	return a, err
}

// activeCoordinators are the team's members whose role is coordinator.
func activeCoordinators(ctx context.Context, tx *sql.Tx, teamID string) ([]string, error) {
	rows, err := tx.QueryContext(ctx,
		`SELECT agent_id FROM memberships WHERE team_id = ? AND removed = 0 AND role = 'coordinator' ORDER BY agent_id`, teamID)
	if err != nil {
		return nil, fmt.Errorf("list coordinators: %w", err)
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
