package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// The operator's control of a team (docs/DESIGN-CONTROL-PLANE.md, section
// 4.2 and decision D12; increment C1). A pause is a scheduler state, not a
// message a model has to obey: while a team or a member is paused, the
// member's pending read holds its messages back, so its wake starts no turn
// on them, and its session status names the pause for the turn loop to read
// before a turn. A pause never touches attempts, reservations or sessions,
// and a turn already running ends normally. A nudge is the operator's note
// to one member: a lifecycle message carrying it, which wakes the member
// like any other message once it is not paused.

const (
	opPause = "control.pause"
	opNudge = "control.nudge"

	maxPauseReason = 512
	maxNudgeText   = 4 << 10
)

// Pause is a team's or a member's pause. AgentID is empty for the team's.
type Pause struct {
	TeamID   string    `json:"team_id"`
	AgentID  string    `json:"agent_id,omitempty"`
	Reason   string    `json:"reason,omitempty"`
	PausedBy string    `json:"paused_by"`
	PausedAt time.Time `json:"paused_at"`
}

// PauseResult is a pause or resume: the pause now in force for the target,
// or none, and whether this call changed it.
type PauseResult struct {
	TeamID  string `json:"team_id"`
	AgentID string `json:"agent_id,omitempty"`
	Paused  *Pause `json:"paused,omitempty"`
	Changed bool   `json:"changed"`
}

// OperatorDetail is the operator's note a nudge carries; no other message
// carries one.
type OperatorDetail struct {
	Note string `json:"note"`
	By   string `json:"by"`
}

// SetPause pauses (paused set) or resumes the team, or one of its active
// members when agentID is set. It is idempotent: pausing what is paused, or
// resuming what is not, changes nothing and says so. The reason is kept with
// the pause. The operator only.
func (s *Store) SetPause(ctx context.Context, c Caller, key, teamID, agentID string, paused bool, reason string) (PauseResult, error) {
	in := struct {
		TeamID  string `json:"team_id"`
		AgentID string `json:"agent_id,omitempty"`
		Paused  bool   `json:"paused"`
		Reason  string `json:"reason,omitempty"`
	}{teamID, agentID, paused, reason}
	var out PauseResult
	err := s.run(ctx, c, command{
		op: opPause, scope: teamID, key: key, input: in, authorize: requireOperator,
		validate: func() error {
			if teamID == "" || len(teamID) > maxRefLen || len(agentID) > maxRefLen {
				return fmt.Errorf("%w: name the team, and the member for a member's pause", ErrInvalid)
			}
			if !paused && reason != "" {
				return fmt.Errorf("%w: a resume takes no reason", ErrInvalid)
			}
			return validText(reason, maxPauseReason, "the reason", false)
		},
		apply: func(ctx context.Context, tx *sql.Tx, now time.Time) (any, error) {
			if _, err := getTeam(ctx, tx, teamID); err != nil {
				return nil, err
			}
			if agentID != "" {
				if _, err := getMembership(ctx, tx, teamID, agentID); errors.Is(err, ErrNotFound) {
					return nil, fmt.Errorf("%w: %s is not an active member of the team", ErrNotFound, agentID)
				} else if err != nil {
					return nil, err
				}
			}
			cur, err := getPause(ctx, tx, teamID, agentID)
			if err != nil {
				return nil, err
			}
			res := PauseResult{TeamID: teamID, AgentID: agentID, Paused: cur}
			switch {
			case paused && cur == nil:
				p := Pause{TeamID: teamID, AgentID: agentID, Reason: reason, PausedBy: c.String(), PausedAt: now}
				if _, err := tx.ExecContext(ctx,
					`INSERT INTO pauses (team_id, agent_id, reason, paused_by, paused_at) VALUES (?, ?, ?, ?, ?)`,
					teamID, agentID, reason, p.PausedBy, formatTime(now)); err != nil {
					return nil, fmt.Errorf("record pause: %w", err)
				}
				res.Paused, res.Changed = &p, true
			case !paused && cur != nil:
				if _, err := tx.ExecContext(ctx, `DELETE FROM pauses WHERE team_id = ? AND agent_id = ?`, teamID, agentID); err != nil {
					return nil, fmt.Errorf("end pause: %w", err)
				}
				res.Paused, res.Changed = nil, true
			}
			return res, nil
		},
	}, &out)
	return out, err
}

// Nudge sends the operator's note to one active member of the team: one
// lifecycle message carrying it, to that member alone. A retry with the
// same key sends nothing more. The operator only.
func (s *Store) Nudge(ctx context.Context, c Caller, key, teamID, agentID, note string) (Message, error) {
	in := struct {
		TeamID  string `json:"team_id"`
		AgentID string `json:"agent_id"`
		Note    string `json:"note"`
	}{teamID, agentID, note}
	var out Message
	err := s.run(ctx, c, command{
		op: opNudge, scope: teamID, key: key, input: in, authorize: requireOperator,
		validate: func() error {
			if teamID == "" || agentID == "" || len(teamID) > maxRefLen || len(agentID) > maxRefLen {
				return fmt.Errorf("%w: name the team and the member", ErrInvalid)
			}
			return validText(note, maxNudgeText, "the note", true)
		},
		apply: func(ctx context.Context, tx *sql.Tx, now time.Time) (any, error) {
			if _, err := getMembership(ctx, tx, teamID, agentID); errors.Is(err, ErrNotFound) {
				return nil, fmt.Errorf("%w: %s is not an active member of team %s", ErrNotFound, agentID, teamID)
			} else if err != nil {
				return nil, err
			}
			return insertMessage(ctx, tx, Message{TeamID: teamID, Kind: KindLifecycle, To: agentID,
				Text: "A note from the operator: " + note, Operator: &OperatorDetail{Note: note, By: c.String()}},
				[]string{agentID}, now)
		},
	}, &out)
	return out, err
}

// pauseOf is the pause in force for a member: the team's, which wins, or
// the member's own, or nil.
func pauseOf(ctx context.Context, q querier, teamID, agentID string) (*Pause, error) {
	if p, err := getPause(ctx, q, teamID, ""); p != nil || err != nil {
		return p, err
	}
	return getPause(ctx, q, teamID, agentID)
}

// PauseFor is the pause in force for a member of a team, or nil.
func (s *Store) PauseFor(ctx context.Context, teamID, agentID string) (*Pause, error) {
	var p *Pause
	err := s.snapshot(ctx, func(q querier) error {
		var err error
		p, err = pauseOf(ctx, q, teamID, agentID)
		return err
	})
	return p, err
}

func getPause(ctx context.Context, q querier, teamID, agentID string) (*Pause, error) {
	p := Pause{TeamID: teamID, AgentID: agentID}
	var at string
	err := q.QueryRowContext(ctx, `SELECT reason, paused_by, paused_at FROM pauses WHERE team_id = ? AND agent_id = ?`,
		teamID, agentID).Scan(&p.Reason, &p.PausedBy, &at)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read pause: %w", err)
	}
	if p.PausedAt, err = parseTime(at); err != nil {
		return nil, err
	}
	return &p, nil
}

// MemberState is one current member of a team as the operator's team state
// shows it: what the store knows. The server adds what it saw of the
// member's session and derives the state.
type MemberState struct {
	AgentID string  `json:"agent_id"`
	Label   string  `json:"label"`
	Role    Role    `json:"role"`
	Profile Profile `json:"profile"`
	// Session is the member's active session, if any.
	Session *Session `json:"session,omitempty"`
	// Paused is the member's own pause; the team's is on TeamState.
	Paused *Pause `json:"paused,omitempty"`
	// OpenAttempt is the newest attempt the member works on that is not
	// closed, if any.
	OpenAttempt *MemberAttempt `json:"open_attempt,omitempty"`
	// Unacknowledged counts the messages waiting in the member's inbox,
	// delivered or not.
	Unacknowledged int `json:"unacknowledged"`
}

// MemberAttempt is a member's open attempt in the team state.
type MemberAttempt struct {
	ID    string       `json:"id"`
	State AttemptState `json:"state"`
	Phase AttemptPhase `json:"phase,omitempty"`
	Task  TaskRef      `json:"task"`
	// Branch is the attempt's branch, which the turn loop reads progress
	// from (a new commit).
	Branch string `json:"branch,omitempty"`
	// UpdatedAt is when the attempt last changed, for the team state. The
	// member's session status, which carries this struct, leaves it out.
	UpdatedAt time.Time `json:"-"`
}

// TeamState is a team's pause and its current members' state.
type TeamState struct {
	TeamID   string        `json:"team_id"`
	TeamName string        `json:"team_name"`
	Paused   *Pause        `json:"paused,omitempty"`
	Members  []MemberState `json:"members"`
}

// GetTeamState reads a team's state in one snapshot. The operator only.
func (s *Store) GetTeamState(ctx context.Context, c Caller, teamID string) (TeamState, error) {
	if err := requireOperator(c); err != nil {
		return TeamState{}, err
	}
	var out TeamState
	err := s.snapshot(ctx, func(q querier) error {
		var err error
		out, err = teamStateIn(ctx, q, teamID)
		return err
	})
	return out, err
}

// maxTeamStates bounds the teams one TeamStates read returns.
const maxTeamStates = 100

// TeamStates reads the state of one team, or of every team (at most
// maxTeamStates, by name) when teamID is empty, in one snapshot: the read
// the architect seat makes (/arch-status). The operator and an architect
// credential.
func (s *Store) TeamStates(ctx context.Context, c Caller, teamID string) ([]TeamState, error) {
	if err := requireEscalationReader(c); err != nil {
		return nil, err
	}
	if len(teamID) > maxRefLen {
		return nil, fmt.Errorf("%w: the team's ID is too long", ErrInvalid)
	}
	out := []TeamState{}
	err := s.snapshot(ctx, func(q querier) error {
		ids := []string{teamID}
		if teamID == "" {
			rows, err := q.QueryContext(ctx, `SELECT id FROM teams ORDER BY name, id LIMIT ?`, maxTeamStates)
			if err != nil {
				return fmt.Errorf("list teams: %w", err)
			}
			ids = ids[:0]
			for rows.Next() {
				var id string
				if err := rows.Scan(&id); err != nil {
					rows.Close()
					return err
				}
				ids = append(ids, id)
			}
			if err := errors.Join(rows.Err(), rows.Close()); err != nil {
				return err
			}
		}
		for _, id := range ids {
			st, err := teamStateIn(ctx, q, id)
			if err != nil {
				return err
			}
			out = append(out, st)
		}
		return nil
	})
	return out, err
}

// teamStateIn reads one team's state inside a snapshot.
func teamStateIn(ctx context.Context, q querier, teamID string) (TeamState, error) {
	t, err := getTeam(ctx, q, teamID)
	if err != nil {
		return TeamState{}, err
	}
	out := TeamState{TeamID: t.ID, TeamName: t.Name, Members: []MemberState{}}
	if out.Paused, err = getPause(ctx, q, teamID, ""); err != nil {
		return TeamState{}, err
	}
	members, err := listMembers(ctx, q, teamID)
	if err != nil {
		return TeamState{}, err
	}
	for _, m := range members {
		a, err := getAgent(ctx, q, m.AgentID)
		if err != nil {
			return TeamState{}, err
		}
		ms := MemberState{AgentID: m.AgentID, Label: a.Label, Role: m.Role, Profile: a.Profile}
		if sess, err := activeSession(ctx, q, teamID, m.AgentID); err == nil {
			ms.Session = &sess
		} else if !errors.Is(err, ErrNotFound) {
			return TeamState{}, err
		}
		if ms.Paused, err = getPause(ctx, q, teamID, m.AgentID); err != nil {
			return TeamState{}, err
		}
		if ms.OpenAttempt, err = memberOpenAttempt(ctx, q, teamID, m.AgentID); err != nil {
			return TeamState{}, err
		}
		if err := q.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM message_recipients r JOIN messages m ON m.id = r.message_id
			 WHERE r.agent_id = ? AND m.team_id = ? AND r.acknowledged_at IS NULL
			   AND (m.project_id = '' OR EXISTS (SELECT 1 FROM team_grants g
			        WHERE g.team_id = m.team_id AND g.hub_id = m.project_hub_id AND g.project_id = m.project_id))`,
			m.AgentID, teamID).Scan(&ms.Unacknowledged); err != nil {
			return TeamState{}, fmt.Errorf("count unacknowledged messages: %w", err)
		}
		out.Members = append(out.Members, ms)
	}
	return out, nil
}

func memberOpenAttempt(ctx context.Context, q querier, teamID, agentID string) (*MemberAttempt, error) {
	var id string
	err := q.QueryRowContext(ctx,
		`SELECT id FROM attempts WHERE team_id = ? AND worker_agent_id = ? AND state != 'closed'
		 ORDER BY created_at DESC, id DESC LIMIT 1`, teamID, agentID).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read open attempt: %w", err)
	}
	a, err := getAttempt(ctx, q, id)
	if err != nil {
		return nil, err
	}
	return &MemberAttempt{ID: a.ID, State: a.State, Phase: a.Phase, Task: a.Task, Branch: a.Branch, UpdatedAt: a.UpdatedAt}, nil
}

// MemberAttemptFor is the member's newest attempt as its worker that is not
// closed, or nil: what the turn loop reads to decide a worker's
// continuation (docs/DESIGN-CONTROL-PLANE.md, 1.3 b).
func (s *Store) MemberAttemptFor(ctx context.Context, teamID, agentID string) (*MemberAttempt, error) {
	var a *MemberAttempt
	err := s.snapshot(ctx, func(q querier) error {
		var err error
		a, err = memberOpenAttempt(ctx, q, teamID, agentID)
		return err
	})
	return a, err
}

// Member states (docs/DESIGN-CONTROL-PLANE.md, section 1.6). needs_login
// and limit_reached come with the provider profile (A5), and the runner's
// own report replaces this derivation with the scheduler (B2).
const (
	StateWorking      = "working"
	StateWaiting      = "waiting"
	StateNeedsLogin   = "needs_login"
	StateLimitReached = "limit_reached"
	StatePaused       = "paused"
	StateOffline      = "offline"
)

// Until the turns and the runner exist, the state follows from what aicrewd
// sees of a member's session: its wake reads the pending list every few
// seconds while it waits, and a turn calls aicrewd through its launcher.
const (
	// WaitingWindow is how recently a pending read makes a member waiting.
	WaitingWindow = 15 * time.Second
	// WorkingWindow is how long a session not heard from still counts as
	// working: the design's default turn limit.
	WorkingWindow = 30 * time.Minute
)

// DeriveState is a member's state: paused while the team or the member is
// paused; offline without an active session or when nothing was heard from
// it within WorkingWindow; waiting when its wake read the pending list
// within WaitingWindow; working otherwise. lastSeen is the latest time the
// session was heard from, lastWaited its latest pending read; either may be
// zero.
func DeriveState(paused, session bool, lastSeen, lastWaited, now time.Time) string {
	switch {
	case paused:
		return StatePaused
	case !session:
		return StateOffline
	case !lastWaited.IsZero() && now.Sub(lastWaited) <= WaitingWindow:
		return StateWaiting
	case lastSeen.IsZero() || now.Sub(lastSeen) > WorkingWindow:
		return StateOffline
	default:
		return StateWorking
	}
}

// StallWindow is how long an open attempt may stay unchanged while its
// member is working or waiting before the member looks stalled: twice the
// working window, so one long turn alone never marks it.
const StallWindow = 2 * WorkingWindow

// LooksStalled reports whether a member in state, whose open attempt is in
// phase and last changed at attemptUpdated (zero without an open attempt),
// looks stalled at now: working or waiting, with an open attempt unchanged
// for longer than StallWindow. A paused or offline member is not stalled:
// its state says why it does nothing. Nor is one whose attempt waits on
// someone else with a channel of its own: an accepted result waits for the
// human merge (the merges list), a blocked one for an escalation's answer,
// and a finalized one is done.
func LooksStalled(state string, phase AttemptPhase, attemptUpdated, now time.Time) bool {
	switch phase {
	case PhaseAccepted, PhaseBlocked, PhaseFinalized:
		return false
	}
	return (state == StateWorking || state == StateWaiting) && !attemptUpdated.IsZero() &&
		now.Sub(attemptUpdated) > StallWindow
}

// encodeOperator is a message's operator column.
func encodeOperator(d *OperatorDetail) (string, error) {
	if d == nil {
		return "", nil
	}
	b, err := json.Marshal(d)
	if err != nil {
		return "", fmt.Errorf("encode operator note: %w", err)
	}
	return string(b), nil
}
