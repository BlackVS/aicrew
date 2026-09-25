package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

var (
	// ErrContextStale refuses a session command whose session is no longer
	// active or whose generation has moved on. It changes nothing.
	ErrContextStale = errors.New("context_stale")
	// ErrIdentityLinkRequired refuses a session for an agent without a
	// verified aimem identity link.
	ErrIdentityLinkRequired = errors.New("identity_link_required")
	// ErrSessionActive refuses a second active session for one membership.
	ErrSessionActive = errors.New("session already active")
	// ErrCoordinatorActive refuses a second active coordinator session in a
	// team.
	ErrCoordinatorActive = errors.New("coordinator session already active")
)

// SessionState is the lifecycle state of a team session. Only an active
// session accepts commands; every other state is final.
type SessionState string

const (
	SessionActive  SessionState = "active"
	SessionLeft    SessionState = "left"    // the member left
	SessionStopped SessionState = "stopped" // an operator stopped it
	SessionEnded   SessionState = "ended"   // role change or membership removal
)

// Session is one running team context for one member. Generation advances on
// resume and when the session ends; commands carrying an older generation are
// refused. A coordinator session also holds the team-wide coordinator
// generation current when it started or last resumed.
type Session struct {
	ID                    string       `json:"id"`
	TeamID                string       `json:"team_id"`
	AgentID               string       `json:"agent_id"`
	Role                  Role         `json:"role"`
	State                 SessionState `json:"state"`
	Generation            int64        `json:"generation"`
	CoordinatorGeneration int64        `json:"coordinator_generation"`
	LastSeenAt            time.Time    `json:"last_seen_at"`
	CreatedAt             time.Time    `json:"created_at"`
	UpdatedAt             time.Time    `json:"updated_at"`
}

const (
	opStartSession  = "session.start"
	opResumeSession = "session.resume"
	opHeartbeat     = "session.heartbeat"
	opLeaveSession  = "session.leave"
	opStopSession   = "session.stop"
)

type sessionStart struct {
	TeamID string `json:"team_id"`
}

// StartSession starts the caller's session in a team. The caller must be the
// member's own agent, the agent must have a verified identity link, and the
// membership must have no active session. A coordinator also needs the team
// to have no active coordinator session.
func (s *Store) StartSession(ctx context.Context, c Caller, key, teamID string) (Session, error) {
	var out Session
	err := s.run(ctx, c, command{
		op: opStartSession, scope: teamID, key: key, input: sessionStart{TeamID: teamID},
		authorize: requireAgent, replayCheck: sessionStillAt,
		apply: func(ctx context.Context, tx *sql.Tx, now time.Time) (any, error) {
			agent, err := getAgent(ctx, tx, c.id)
			if err != nil {
				return nil, err
			}
			if agent.Linked == nil {
				return nil, fmt.Errorf("agent %s: %w", agent.ID, ErrIdentityLinkRequired)
			}
			m, err := getMembership(ctx, tx, teamID, c.id)
			if errors.Is(err, ErrNotFound) {
				return nil, fmt.Errorf("%w: agent %s is not a member of team %s", ErrForbidden, c.id, teamID)
			}
			if err != nil {
				return nil, err
			}
			if _, err := activeSession(ctx, tx, teamID, c.id); err == nil {
				return nil, fmt.Errorf("team %s agent %s: %w", teamID, c.id, ErrSessionActive)
			} else if !errors.Is(err, ErrNotFound) {
				return nil, err
			}
			var coordGen int64
			if m.Role == RoleCoordinator {
				var active int
				if err := tx.QueryRowContext(ctx,
					`SELECT COUNT(*) FROM sessions WHERE team_id = ? AND state = 'active' AND role = 'coordinator'`,
					teamID).Scan(&active); err != nil {
					return nil, fmt.Errorf("check coordinator: %w", err)
				}
				if active > 0 {
					return nil, fmt.Errorf("team %s: %w", teamID, ErrCoordinatorActive)
				}
				if coordGen, err = bumpCoordinatorGeneration(ctx, tx, teamID); err != nil {
					return nil, err
				}
			}
			id, err := newID(now)
			if err != nil {
				return nil, err
			}
			at := formatTime(now)
			if _, err := tx.ExecContext(ctx,
				`INSERT INTO sessions (id, team_id, agent_id, role, state, generation, coordinator_generation,
				                       last_seen_at, created_at, updated_at)
				 VALUES (?, ?, ?, ?, 'active', 1, ?, ?, ?, ?)`,
				id, teamID, c.id, string(m.Role), coordGen, at, at, at); err != nil {
				return nil, fmt.Errorf("insert session: %w", err)
			}
			return getSession(ctx, tx, id)
		},
	}, &out)
	return out, err
}

type sessionRef struct {
	SessionID string `json:"session_id"`
}

// ResumeSession takes over the caller's active session under a new
// generation, fencing any holder of the old one. It does not need the old
// generation: the previous process may be gone. A coordinator session also
// advances the team-wide coordinator generation.
func (s *Store) ResumeSession(ctx context.Context, c Caller, key, sessionID string) (Session, error) {
	var out Session
	err := s.run(ctx, c, command{
		op: opResumeSession, scope: sessionID, key: key, input: sessionRef{SessionID: sessionID},
		authorize: requireAgent, replayCheck: sessionStillAt,
		apply: func(ctx context.Context, tx *sql.Tx, now time.Time) (any, error) {
			sess, err := ownSession(ctx, tx, c, sessionID)
			if err != nil {
				return nil, err
			}
			if sess.State != SessionActive {
				return nil, fmt.Errorf("session %s is %s: %w", sessionID, sess.State, ErrContextStale)
			}
			agent, err := getAgent(ctx, tx, c.id)
			if err != nil {
				return nil, err
			}
			if agent.Linked == nil {
				return nil, fmt.Errorf("agent %s: %w", agent.ID, ErrIdentityLinkRequired)
			}
			coordGen := sess.CoordinatorGeneration
			if sess.Role == RoleCoordinator {
				if coordGen, err = bumpCoordinatorGeneration(ctx, tx, sess.TeamID); err != nil {
					return nil, err
				}
			}
			at := formatTime(now)
			if _, err := tx.ExecContext(ctx,
				`UPDATE sessions SET generation = generation + 1, coordinator_generation = ?,
				        last_seen_at = ?, updated_at = ?
				 WHERE id = ? AND state = 'active'`,
				coordGen, at, at, sessionID); err != nil {
				return nil, fmt.Errorf("resume session: %w", err)
			}
			return getSession(ctx, tx, sessionID)
		},
	}, &out)
	return out, err
}

type sessionAt struct {
	SessionID  string `json:"session_id"`
	Generation int64  `json:"generation"`
}

// Heartbeat records that the session is alive. Liveness is informational:
// nothing expires a session, frees work or changes its generation when
// heartbeats stop.
func (s *Store) Heartbeat(ctx context.Context, c Caller, key, sessionID string, generation int64) (Session, error) {
	var out Session
	err := s.run(ctx, c, command{
		op: opHeartbeat, scope: sessionID, key: key, input: sessionAt{SessionID: sessionID, Generation: generation},
		authorize: requireAgent, replayCheck: sessionStillAt,
		check: func(ctx context.Context, tx *sql.Tx) error {
			_, err := currentSession(ctx, tx, c, sessionID, generation)
			return err
		},
		apply: func(ctx context.Context, tx *sql.Tx, now time.Time) (any, error) {
			at := formatTime(now)
			if _, err := tx.ExecContext(ctx,
				`UPDATE sessions SET last_seen_at = ?, updated_at = ? WHERE id = ?`,
				at, at, sessionID); err != nil {
				return nil, fmt.Errorf("heartbeat: %w", err)
			}
			return getSession(ctx, tx, sessionID)
		},
	}, &out)
	return out, err
}

// LeaveSession ends the caller's session at its current generation. Later
// increments refuse leave while the member has outstanding work; this
// increment has none.
func (s *Store) LeaveSession(ctx context.Context, c Caller, key, sessionID string, generation int64) (Session, error) {
	var out Session
	err := s.run(ctx, c, command{
		op: opLeaveSession, scope: sessionID, key: key, input: sessionAt{SessionID: sessionID, Generation: generation},
		authorize: requireAgent, replayCheck: sessionStillAt,
		check: func(ctx context.Context, tx *sql.Tx) error {
			_, err := currentSession(ctx, tx, c, sessionID, generation)
			return err
		},
		apply: func(ctx context.Context, tx *sql.Tx, now time.Time) (any, error) {
			sess, err := getSession(ctx, tx, sessionID)
			if err != nil {
				return nil, err
			}
			if err := endSession(ctx, tx, sess, SessionLeft, now); err != nil {
				return nil, err
			}
			return getSession(ctx, tx, sessionID)
		},
	}, &out)
	return out, err
}

// StopSession ends an active session. Operator only.
func (s *Store) StopSession(ctx context.Context, c Caller, key, sessionID string) (Session, error) {
	var out Session
	err := s.run(ctx, c, command{
		op: opStopSession, scope: sessionID, key: key, input: sessionRef{SessionID: sessionID},
		authorize: requireOperator, replayCheck: sessionStillAt,
		apply: func(ctx context.Context, tx *sql.Tx, now time.Time) (any, error) {
			sess, err := getSession(ctx, tx, sessionID)
			if err != nil {
				return nil, err
			}
			if sess.State != SessionActive {
				return nil, fmt.Errorf("session %s is %s: %w", sessionID, sess.State, ErrContextStale)
			}
			if err := endSession(ctx, tx, sess, SessionStopped, now); err != nil {
				return nil, err
			}
			return getSession(ctx, tx, sessionID)
		},
	}, &out)
	return out, err
}

// GetSession reads a session by ID.
func (s *Store) GetSession(ctx context.Context, id string) (Session, error) {
	var sess Session
	err := s.snapshot(ctx, func(q querier) error {
		var err error
		sess, err = getSession(ctx, q, id)
		return err
	})
	return sess, err
}

// sessionStillAt admits a replayed session result only while the session
// still has the generation recorded in it, so an old result never tells a
// caller it holds a generation that has since been superseded.
func sessionStillAt(ctx context.Context, tx *sql.Tx, result string) error {
	var recorded Session
	if err := json.Unmarshal([]byte(result), &recorded); err != nil {
		return fmt.Errorf("decode recorded session: %w", err)
	}
	current, err := getSession(ctx, tx, recorded.ID)
	if err != nil {
		return err
	}
	if current.Generation != recorded.Generation {
		return fmt.Errorf("session %s is at generation %d, not %d: %w",
			current.ID, current.Generation, recorded.Generation, ErrContextStale)
	}
	return nil
}

// ownSession reads a session and requires it to belong to the calling agent.
func ownSession(ctx context.Context, q querier, c Caller, sessionID string) (Session, error) {
	sess, err := getSession(ctx, q, sessionID)
	if err != nil {
		return Session{}, err
	}
	if sess.AgentID != c.id {
		return Session{}, fmt.Errorf("%w: session %s belongs to another agent", ErrForbidden, sessionID)
	}
	return sess, nil
}

// currentSession requires the caller's own session to be active at exactly
// the given generation.
func currentSession(ctx context.Context, q querier, c Caller, sessionID string, generation int64) (Session, error) {
	sess, err := ownSession(ctx, q, c, sessionID)
	if err != nil {
		return Session{}, err
	}
	if sess.State != SessionActive || sess.Generation != generation {
		return Session{}, fmt.Errorf("session %s is %s at generation %d, not active at %d: %w",
			sessionID, sess.State, sess.Generation, generation, ErrContextStale)
	}
	return sess, nil
}

// endSession moves an active session to a final state and advances its
// generation; ending a coordinator session advances the team's coordinator
// generation too.
func endSession(ctx context.Context, tx *sql.Tx, sess Session, state SessionState, now time.Time) error {
	res, err := tx.ExecContext(ctx,
		`UPDATE sessions SET state = ?, generation = generation + 1, updated_at = ?
		 WHERE id = ? AND state = 'active'`,
		string(state), formatTime(now), sess.ID)
	if err != nil {
		return fmt.Errorf("end session: %w", err)
	}
	if n, err := res.RowsAffected(); err != nil {
		return err
	} else if n != 1 {
		return fmt.Errorf("session %s: %w", sess.ID, ErrContextStale)
	}
	if sess.Role == RoleCoordinator {
		if _, err := bumpCoordinatorGeneration(ctx, tx, sess.TeamID); err != nil {
			return err
		}
	}
	return nil
}

// endActiveSession ends the member's active session, if any, as ended.
func endActiveSession(ctx context.Context, tx *sql.Tx, teamID, agentID string, now time.Time) error {
	sess, err := activeSession(ctx, tx, teamID, agentID)
	if errors.Is(err, ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	return endSession(ctx, tx, sess, SessionEnded, now)
}

func bumpCoordinatorGeneration(ctx context.Context, tx *sql.Tx, teamID string) (int64, error) {
	if _, err := tx.ExecContext(ctx,
		`UPDATE teams SET coordinator_generation = coordinator_generation + 1 WHERE id = ?`, teamID); err != nil {
		return 0, fmt.Errorf("advance coordinator generation: %w", err)
	}
	var gen int64
	if err := tx.QueryRowContext(ctx,
		`SELECT coordinator_generation FROM teams WHERE id = ?`, teamID).Scan(&gen); err != nil {
		return 0, fmt.Errorf("read coordinator generation: %w", err)
	}
	return gen, nil
}

const sessionColumns = `id, team_id, agent_id, role, state, generation, coordinator_generation,
	last_seen_at, created_at, updated_at`

func getSession(ctx context.Context, q querier, id string) (Session, error) {
	sess, err := scanSession(q.QueryRowContext(ctx, `SELECT `+sessionColumns+` FROM sessions WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return Session{}, fmt.Errorf("session %s: %w", id, ErrNotFound)
	}
	return sess, err
}

func activeSession(ctx context.Context, q querier, teamID, agentID string) (Session, error) {
	sess, err := scanSession(q.QueryRowContext(ctx,
		`SELECT `+sessionColumns+` FROM sessions WHERE team_id = ? AND agent_id = ? AND state = 'active'`,
		teamID, agentID))
	if errors.Is(err, sql.ErrNoRows) {
		return Session{}, fmt.Errorf("active session %s/%s: %w", teamID, agentID, ErrNotFound)
	}
	return sess, err
}

func scanSession(sc scanner) (Session, error) {
	var (
		s                          Session
		role, state                string
		lastSeen, created, updated string
	)
	if err := sc.Scan(&s.ID, &s.TeamID, &s.AgentID, &role, &state, &s.Generation, &s.CoordinatorGeneration,
		&lastSeen, &created, &updated); err != nil {
		return Session{}, err
	}
	s.Role, s.State = Role(role), SessionState(state)
	var err error
	if s.LastSeenAt, err = parseTime(lastSeen); err != nil {
		return Session{}, err
	}
	if s.CreatedAt, err = parseTime(created); err != nil {
		return Session{}, err
	}
	if s.UpdatedAt, err = parseTime(updated); err != nil {
		return Session{}, err
	}
	return s, nil
}
