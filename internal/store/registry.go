package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"time"
)

// Role is a team member's coordination role. Roles grant no aimem authority.
type Role string

const (
	RoleCoordinator Role = "coordinator"
	RoleWorker      Role = "worker"
	RoleIndependent Role = "independent"
)

func (r Role) valid() bool {
	return r == RoleCoordinator || r == RoleWorker || r == RoleIndependent
}

// Profile is changeable, declared data about an agent's model and client.
// It is recorded for audit and display and never used for authorization.
type Profile struct {
	Model         string `json:"model"`
	Client        string `json:"client"`
	ClientVersion string `json:"client_version"`
}

// LinkedActor is the verified aimem identity of an agent. Nothing in this
// increment sets it; it stays nil until proof integration exists.
type LinkedActor struct {
	HubID  string `json:"hub_id"`
	UserID string `json:"user_id"`
}

type Agent struct {
	ID        string       `json:"id"`
	Label     string       `json:"label"`
	Profile   Profile      `json:"profile"`
	Linked    *LinkedActor `json:"linked,omitempty"`
	Revision  int64        `json:"revision"`
	CreatedAt time.Time    `json:"created_at"`
	UpdatedAt time.Time    `json:"updated_at"`
}

// ProjectRef names an aimem project by its hub and stable project ID. A
// team's project list states intended scope; it grants nothing.
type ProjectRef struct {
	HubID     string `json:"hub_id"`
	ProjectID string `json:"project_id"`
}

type Team struct {
	ID        string       `json:"id"`
	Name      string       `json:"name"`
	Projects  []ProjectRef `json:"projects"`
	Revision  int64        `json:"revision"`
	CreatedAt time.Time    `json:"created_at"`
	UpdatedAt time.Time    `json:"updated_at"`
}

type Membership struct {
	TeamID    string    `json:"team_id"`
	AgentID   string    `json:"agent_id"`
	Role      Role      `json:"role"`
	Revision  int64     `json:"revision"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

const (
	opCreateAgent     = "agent.create"
	opRenameAgent     = "agent.rename"
	opSetAgentProfile = "agent.set_profile"
	opCreateTeam      = "team.create"
	opRenameTeam      = "team.rename"
	opSetTeamProjects = "team.set_projects"
	opAddMember       = "member.add"
	opSetMemberRole   = "member.set_role"
	opRemoveMember    = "member.remove"
)

// Labels follow the workspace convention: lowercase letters, digits and '-',
// at most 32 characters, starting with a letter or digit.
var labelPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,31}$`)

// Hub and project IDs are opaque stable identifiers from aimem.
var refPattern = regexp.MustCompile(`^[A-Za-z0-9._:-]{1,128}$`)

func validateLabel(what, label string) error {
	if !labelPattern.MatchString(label) {
		return fmt.Errorf("%w: %s %q must be 1-32 lowercase letters, digits or '-'", ErrInvalid, what, label)
	}
	return nil
}

func (p Profile) validate() error {
	for name, v := range map[string]string{"model": p.Model, "client": p.Client, "client_version": p.ClientVersion} {
		if len(v) > 128 {
			return fmt.Errorf("%w: profile %s longer than 128 bytes", ErrInvalid, name)
		}
		for _, r := range v {
			if r < ' ' || r == 0x7f {
				return fmt.Errorf("%w: profile %s contains control characters", ErrInvalid, name)
			}
		}
	}
	return nil
}

// normalizeProjects validates, de-duplicates and sorts a project list so that
// equal sets produce equal idempotency digests.
func normalizeProjects(in []ProjectRef) ([]ProjectRef, error) {
	seen := make(map[ProjectRef]bool, len(in))
	out := make([]ProjectRef, 0, len(in))
	for _, p := range in {
		if !refPattern.MatchString(p.HubID) || !refPattern.MatchString(p.ProjectID) {
			return nil, fmt.Errorf("%w: project ref %q/%q", ErrInvalid, p.HubID, p.ProjectID)
		}
		if !seen[p] {
			seen[p] = true
			out = append(out, p)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].HubID != out[j].HubID {
			return out[i].HubID < out[j].HubID
		}
		return out[i].ProjectID < out[j].ProjectID
	})
	return out, nil
}

// casUpdate reports ErrNotFound or ErrRevisionConflict when an update matched
// no row, distinguishing a missing record from a stale expected revision.
func casUpdate(ctx context.Context, tx *sql.Tx, res sql.Result, existsQuery string, args ...any) error {
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 1 {
		return nil
	}
	var one int
	err = tx.QueryRowContext(ctx, existsQuery, args...).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	return ErrRevisionConflict
}

// --- agents ---

type NewAgent struct {
	Label   string  `json:"label"`
	Profile Profile `json:"profile"`
}

// CreateAgent registers an agent. Operator only. The agent has no linked
// aimem identity.
func (s *Store) CreateAgent(ctx context.Context, c Caller, key string, in NewAgent) (Agent, error) {
	var out Agent
	err := s.run(ctx, c, command{
		op: opCreateAgent, key: key, input: in, authorize: requireOperator,
		validate: func() error {
			if err := validateLabel("agent label", in.Label); err != nil {
				return err
			}
			return in.Profile.validate()
		},
		apply: func(ctx context.Context, tx *sql.Tx, now time.Time) (any, error) {
			id, err := newID(now)
			if err != nil {
				return nil, err
			}
			at := formatTime(now)
			if _, err := tx.ExecContext(ctx,
				`INSERT INTO agents (id, label, model, client, client_version, revision, created_at, updated_at)
				 VALUES (?, ?, ?, ?, ?, 1, ?, ?)`,
				id, in.Label, in.Profile.Model, in.Profile.Client, in.Profile.ClientVersion, at, at); err != nil {
				return nil, fmt.Errorf("insert agent: %w", err)
			}
			return getAgent(ctx, tx, id)
		},
	}, &out)
	return out, err
}

type agentRename struct {
	AgentID          string `json:"agent_id"`
	ExpectedRevision int64  `json:"expected_revision"`
	Label            string `json:"label"`
}

// RenameAgent changes an agent's label. Operator only. The label is not an
// identity: the agent ID and any linked actor are unchanged.
func (s *Store) RenameAgent(ctx context.Context, c Caller, key, agentID string, expectedRevision int64, label string) (Agent, error) {
	in := agentRename{AgentID: agentID, ExpectedRevision: expectedRevision, Label: label}
	var out Agent
	err := s.run(ctx, c, command{
		op: opRenameAgent, scope: agentID, key: key, input: in, authorize: requireOperator,
		validate: func() error { return validateLabel("agent label", label) },
		apply: func(ctx context.Context, tx *sql.Tx, now time.Time) (any, error) {
			res, err := tx.ExecContext(ctx,
				`UPDATE agents SET label = ?, revision = revision + 1, updated_at = ? WHERE id = ? AND revision = ?`,
				label, formatTime(now), agentID, expectedRevision)
			if err != nil {
				return nil, fmt.Errorf("rename agent: %w", err)
			}
			if err := casUpdate(ctx, tx, res, `SELECT 1 FROM agents WHERE id = ?`, agentID); err != nil {
				return nil, fmt.Errorf("rename agent %s: %w", agentID, err)
			}
			return getAgent(ctx, tx, agentID)
		},
	}, &out)
	return out, err
}

type agentProfileUpdate struct {
	AgentID          string  `json:"agent_id"`
	ExpectedRevision int64   `json:"expected_revision"`
	Profile          Profile `json:"profile"`
}

// SetAgentProfile replaces an agent's declared model and client data.
// Operator only in this increment.
func (s *Store) SetAgentProfile(ctx context.Context, c Caller, key, agentID string, expectedRevision int64, p Profile) (Agent, error) {
	in := agentProfileUpdate{AgentID: agentID, ExpectedRevision: expectedRevision, Profile: p}
	var out Agent
	err := s.run(ctx, c, command{
		op: opSetAgentProfile, scope: agentID, key: key, input: in, authorize: requireOperator,
		validate: p.validate,
		apply: func(ctx context.Context, tx *sql.Tx, now time.Time) (any, error) {
			res, err := tx.ExecContext(ctx,
				`UPDATE agents SET model = ?, client = ?, client_version = ?, revision = revision + 1, updated_at = ?
				 WHERE id = ? AND revision = ?`,
				p.Model, p.Client, p.ClientVersion, formatTime(now), agentID, expectedRevision)
			if err != nil {
				return nil, fmt.Errorf("set agent profile: %w", err)
			}
			if err := casUpdate(ctx, tx, res, `SELECT 1 FROM agents WHERE id = ?`, agentID); err != nil {
				return nil, fmt.Errorf("set agent profile %s: %w", agentID, err)
			}
			return getAgent(ctx, tx, agentID)
		},
	}, &out)
	return out, err
}

// GetAgent reads an agent by ID.
func (s *Store) GetAgent(ctx context.Context, id string) (Agent, error) {
	return getAgent(ctx, s.db, id)
}

// ResolveAgent finds the one agent with the given label. It returns
// ErrAmbiguous rather than choosing when several agents share the label.
func (s *Store) ResolveAgent(ctx context.Context, label string) (Agent, error) {
	id, err := resolveLabel(ctx, s.db, `SELECT id FROM agents WHERE label = ? LIMIT 2`, "agent", label)
	if err != nil {
		return Agent{}, err
	}
	return getAgent(ctx, s.db, id)
}

type querier interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}

func getAgent(ctx context.Context, q querier, id string) (Agent, error) {
	var (
		a                  Agent
		hubID, userID      sql.NullString
		createdAt, updated string
	)
	err := q.QueryRowContext(ctx,
		`SELECT id, label, model, client, client_version, linked_hub_id, linked_user_id, revision, created_at, updated_at
		 FROM agents WHERE id = ?`, id).
		Scan(&a.ID, &a.Label, &a.Profile.Model, &a.Profile.Client, &a.Profile.ClientVersion,
			&hubID, &userID, &a.Revision, &createdAt, &updated)
	if errors.Is(err, sql.ErrNoRows) {
		return Agent{}, fmt.Errorf("agent %s: %w", id, ErrNotFound)
	}
	if err != nil {
		return Agent{}, fmt.Errorf("read agent: %w", err)
	}
	if hubID.Valid {
		a.Linked = &LinkedActor{HubID: hubID.String, UserID: userID.String}
	}
	if a.CreatedAt, err = parseTime(createdAt); err != nil {
		return Agent{}, err
	}
	if a.UpdatedAt, err = parseTime(updated); err != nil {
		return Agent{}, err
	}
	return a, nil
}

func resolveLabel(ctx context.Context, q querier, query, what, label string) (string, error) {
	if err := validateLabel(what+" label", label); err != nil {
		return "", err
	}
	rows, err := q.QueryContext(ctx, query, label)
	if err != nil {
		return "", fmt.Errorf("resolve %s: %w", what, err)
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return "", err
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return "", err
	}
	switch len(ids) {
	case 0:
		return "", fmt.Errorf("%s %q: %w", what, label, ErrNotFound)
	case 1:
		return ids[0], nil
	default:
		return "", fmt.Errorf("%s %q: %w", what, label, ErrAmbiguous)
	}
}

// --- teams ---

type NewTeam struct {
	Name     string       `json:"name"`
	Projects []ProjectRef `json:"projects"`
}

// CreateTeam registers a team with its intended project scope. Operator only.
func (s *Store) CreateTeam(ctx context.Context, c Caller, key string, in NewTeam) (Team, error) {
	var out Team
	err := s.run(ctx, c, command{
		op: opCreateTeam, key: key, input: &in, authorize: requireOperator,
		validate: func() error {
			if err := validateLabel("team name", in.Name); err != nil {
				return err
			}
			var err error
			in.Projects, err = normalizeProjects(in.Projects)
			return err
		},
		apply: func(ctx context.Context, tx *sql.Tx, now time.Time) (any, error) {
			id, err := newID(now)
			if err != nil {
				return nil, err
			}
			at := formatTime(now)
			if _, err := tx.ExecContext(ctx,
				`INSERT INTO teams (id, name, revision, created_at, updated_at) VALUES (?, ?, 1, ?, ?)`,
				id, in.Name, at, at); err != nil {
				return nil, fmt.Errorf("insert team: %w", err)
			}
			if err := insertProjects(ctx, tx, id, in.Projects); err != nil {
				return nil, err
			}
			return getTeam(ctx, tx, id)
		},
	}, &out)
	return out, err
}

type teamRename struct {
	TeamID           string `json:"team_id"`
	ExpectedRevision int64  `json:"expected_revision"`
	Name             string `json:"name"`
}

// RenameTeam changes a team's name. Operator only.
func (s *Store) RenameTeam(ctx context.Context, c Caller, key, teamID string, expectedRevision int64, name string) (Team, error) {
	in := teamRename{TeamID: teamID, ExpectedRevision: expectedRevision, Name: name}
	var out Team
	err := s.run(ctx, c, command{
		op: opRenameTeam, scope: teamID, key: key, input: in, authorize: requireOperator,
		validate: func() error { return validateLabel("team name", name) },
		apply: func(ctx context.Context, tx *sql.Tx, now time.Time) (any, error) {
			res, err := tx.ExecContext(ctx,
				`UPDATE teams SET name = ?, revision = revision + 1, updated_at = ? WHERE id = ? AND revision = ?`,
				name, formatTime(now), teamID, expectedRevision)
			if err != nil {
				return nil, fmt.Errorf("rename team: %w", err)
			}
			if err := casUpdate(ctx, tx, res, `SELECT 1 FROM teams WHERE id = ?`, teamID); err != nil {
				return nil, fmt.Errorf("rename team %s: %w", teamID, err)
			}
			return getTeam(ctx, tx, teamID)
		},
	}, &out)
	return out, err
}

type teamProjectsUpdate struct {
	TeamID           string       `json:"team_id"`
	ExpectedRevision int64        `json:"expected_revision"`
	Projects         []ProjectRef `json:"projects"`
}

// SetTeamProjects replaces a team's intended project scope. Operator only.
// The list grants no access to any project.
func (s *Store) SetTeamProjects(ctx context.Context, c Caller, key, teamID string, expectedRevision int64, projects []ProjectRef) (Team, error) {
	in := teamProjectsUpdate{TeamID: teamID, ExpectedRevision: expectedRevision, Projects: projects}
	var out Team
	err := s.run(ctx, c, command{
		op: opSetTeamProjects, scope: teamID, key: key, input: &in, authorize: requireOperator,
		validate: func() error {
			var err error
			in.Projects, err = normalizeProjects(in.Projects)
			return err
		},
		apply: func(ctx context.Context, tx *sql.Tx, now time.Time) (any, error) {
			res, err := tx.ExecContext(ctx,
				`UPDATE teams SET revision = revision + 1, updated_at = ? WHERE id = ? AND revision = ?`,
				formatTime(now), teamID, expectedRevision)
			if err != nil {
				return nil, fmt.Errorf("update team: %w", err)
			}
			if err := casUpdate(ctx, tx, res, `SELECT 1 FROM teams WHERE id = ?`, teamID); err != nil {
				return nil, fmt.Errorf("set team projects %s: %w", teamID, err)
			}
			if _, err := tx.ExecContext(ctx, `DELETE FROM team_projects WHERE team_id = ?`, teamID); err != nil {
				return nil, fmt.Errorf("clear team projects: %w", err)
			}
			if err := insertProjects(ctx, tx, teamID, in.Projects); err != nil {
				return nil, err
			}
			return getTeam(ctx, tx, teamID)
		},
	}, &out)
	return out, err
}

func insertProjects(ctx context.Context, tx *sql.Tx, teamID string, projects []ProjectRef) error {
	for _, p := range projects {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO team_projects (team_id, hub_id, project_id) VALUES (?, ?, ?)`,
			teamID, p.HubID, p.ProjectID); err != nil {
			return fmt.Errorf("insert team project: %w", err)
		}
	}
	return nil
}

// GetTeam reads a team and its project list by ID.
func (s *Store) GetTeam(ctx context.Context, id string) (Team, error) {
	return getTeam(ctx, s.db, id)
}

// ResolveTeam finds the one team with the given name, or returns
// ErrAmbiguous when several share it.
func (s *Store) ResolveTeam(ctx context.Context, name string) (Team, error) {
	id, err := resolveLabel(ctx, s.db, `SELECT id FROM teams WHERE name = ? LIMIT 2`, "team", name)
	if err != nil {
		return Team{}, err
	}
	return getTeam(ctx, s.db, id)
}

func getTeam(ctx context.Context, q querier, id string) (Team, error) {
	var (
		t                  Team
		createdAt, updated string
	)
	err := q.QueryRowContext(ctx,
		`SELECT id, name, revision, created_at, updated_at FROM teams WHERE id = ?`, id).
		Scan(&t.ID, &t.Name, &t.Revision, &createdAt, &updated)
	if errors.Is(err, sql.ErrNoRows) {
		return Team{}, fmt.Errorf("team %s: %w", id, ErrNotFound)
	}
	if err != nil {
		return Team{}, fmt.Errorf("read team: %w", err)
	}
	if t.CreatedAt, err = parseTime(createdAt); err != nil {
		return Team{}, err
	}
	if t.UpdatedAt, err = parseTime(updated); err != nil {
		return Team{}, err
	}
	rows, err := q.QueryContext(ctx,
		`SELECT hub_id, project_id FROM team_projects WHERE team_id = ? ORDER BY hub_id, project_id`, id)
	if err != nil {
		return Team{}, fmt.Errorf("read team projects: %w", err)
	}
	defer rows.Close()
	t.Projects = []ProjectRef{}
	for rows.Next() {
		var p ProjectRef
		if err := rows.Scan(&p.HubID, &p.ProjectID); err != nil {
			return Team{}, err
		}
		t.Projects = append(t.Projects, p)
	}
	return t, rows.Err()
}

// --- memberships ---

type memberAdd struct {
	TeamID  string `json:"team_id"`
	AgentID string `json:"agent_id"`
	Role    Role   `json:"role"`
}

// AddMember adds an agent to a team with a role. Operator only. The role
// grants no aimem authority.
func (s *Store) AddMember(ctx context.Context, c Caller, key, teamID, agentID string, role Role) (Membership, error) {
	in := memberAdd{TeamID: teamID, AgentID: agentID, Role: role}
	var out Membership
	err := s.run(ctx, c, command{
		op: opAddMember, scope: teamID, key: key, input: in, authorize: requireOperator,
		validate: func() error {
			if !role.valid() {
				return fmt.Errorf("%w: role %q", ErrInvalid, role)
			}
			return nil
		},
		apply: func(ctx context.Context, tx *sql.Tx, now time.Time) (any, error) {
			if _, err := getTeam(ctx, tx, teamID); err != nil {
				return nil, err
			}
			if _, err := getAgent(ctx, tx, agentID); err != nil {
				return nil, err
			}
			if _, err := getMembership(ctx, tx, teamID, agentID); err == nil {
				return nil, fmt.Errorf("membership %s/%s: %w", teamID, agentID, ErrExists)
			} else if !errors.Is(err, ErrNotFound) {
				return nil, err
			}
			at := formatTime(now)
			if _, err := tx.ExecContext(ctx,
				`INSERT INTO memberships (team_id, agent_id, role, revision, created_at, updated_at)
				 VALUES (?, ?, ?, 1, ?, ?)`,
				teamID, agentID, string(role), at, at); err != nil {
				return nil, fmt.Errorf("insert membership: %w", err)
			}
			return getMembership(ctx, tx, teamID, agentID)
		},
	}, &out)
	return out, err
}

type memberRoleUpdate struct {
	TeamID           string `json:"team_id"`
	AgentID          string `json:"agent_id"`
	ExpectedRevision int64  `json:"expected_revision"`
	Role             Role   `json:"role"`
}

// SetMemberRole changes a member's role. Operator only.
func (s *Store) SetMemberRole(ctx context.Context, c Caller, key, teamID, agentID string, expectedRevision int64, role Role) (Membership, error) {
	in := memberRoleUpdate{TeamID: teamID, AgentID: agentID, ExpectedRevision: expectedRevision, Role: role}
	var out Membership
	err := s.run(ctx, c, command{
		op: opSetMemberRole, scope: teamID, key: key, input: in, authorize: requireOperator,
		validate: func() error {
			if !role.valid() {
				return fmt.Errorf("%w: role %q", ErrInvalid, role)
			}
			return nil
		},
		apply: func(ctx context.Context, tx *sql.Tx, now time.Time) (any, error) {
			res, err := tx.ExecContext(ctx,
				`UPDATE memberships SET role = ?, revision = revision + 1, updated_at = ?
				 WHERE team_id = ? AND agent_id = ? AND revision = ?`,
				string(role), formatTime(now), teamID, agentID, expectedRevision)
			if err != nil {
				return nil, fmt.Errorf("set member role: %w", err)
			}
			if err := casUpdate(ctx, tx, res,
				`SELECT 1 FROM memberships WHERE team_id = ? AND agent_id = ?`, teamID, agentID); err != nil {
				return nil, fmt.Errorf("set member role %s/%s: %w", teamID, agentID, err)
			}
			return getMembership(ctx, tx, teamID, agentID)
		},
	}, &out)
	return out, err
}

type memberRemove struct {
	TeamID           string `json:"team_id"`
	AgentID          string `json:"agent_id"`
	ExpectedRevision int64  `json:"expected_revision"`
}

// RemoveMember removes a membership. Operator only. The audit log keeps the
// history. Later increments add the outstanding-work check the contract
// requires before removal; this increment has no sessions or work.
func (s *Store) RemoveMember(ctx context.Context, c Caller, key, teamID, agentID string, expectedRevision int64) error {
	in := memberRemove{TeamID: teamID, AgentID: agentID, ExpectedRevision: expectedRevision}
	return s.run(ctx, c, command{
		op: opRemoveMember, scope: teamID, key: key, input: in, authorize: requireOperator,
		apply: func(ctx context.Context, tx *sql.Tx, now time.Time) (any, error) {
			res, err := tx.ExecContext(ctx,
				`DELETE FROM memberships WHERE team_id = ? AND agent_id = ? AND revision = ?`,
				teamID, agentID, expectedRevision)
			if err != nil {
				return nil, fmt.Errorf("remove member: %w", err)
			}
			if err := casUpdate(ctx, tx, res,
				`SELECT 1 FROM memberships WHERE team_id = ? AND agent_id = ?`, teamID, agentID); err != nil {
				return nil, fmt.Errorf("remove member %s/%s: %w", teamID, agentID, err)
			}
			return in, nil
		},
	}, nil)
}

// ListMembers returns a team's memberships ordered by creation.
func (s *Store) ListMembers(ctx context.Context, teamID string) ([]Membership, error) {
	if _, err := getTeam(ctx, s.db, teamID); err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT team_id, agent_id, role, revision, created_at, updated_at FROM memberships
		 WHERE team_id = ? ORDER BY created_at, agent_id`, teamID)
	if err != nil {
		return nil, fmt.Errorf("list members: %w", err)
	}
	defer rows.Close()
	members := []Membership{}
	for rows.Next() {
		m, err := scanMembership(rows)
		if err != nil {
			return nil, err
		}
		members = append(members, m)
	}
	return members, rows.Err()
}

func getMembership(ctx context.Context, q querier, teamID, agentID string) (Membership, error) {
	row := q.QueryRowContext(ctx,
		`SELECT team_id, agent_id, role, revision, created_at, updated_at FROM memberships
		 WHERE team_id = ? AND agent_id = ?`, teamID, agentID)
	m, err := scanMembership(row)
	if errors.Is(err, sql.ErrNoRows) {
		return Membership{}, fmt.Errorf("membership %s/%s: %w", teamID, agentID, ErrNotFound)
	}
	return m, err
}

type scanner interface{ Scan(dest ...any) error }

func scanMembership(sc scanner) (Membership, error) {
	var (
		m                  Membership
		role               string
		createdAt, updated string
	)
	if err := sc.Scan(&m.TeamID, &m.AgentID, &role, &m.Revision, &createdAt, &updated); err != nil {
		return Membership{}, err
	}
	m.Role = Role(role)
	var err error
	if m.CreatedAt, err = parseTime(createdAt); err != nil {
		return Membership{}, err
	}
	if m.UpdatedAt, err = parseTime(updated); err != nil {
		return Membership{}, err
	}
	return m, nil
}
