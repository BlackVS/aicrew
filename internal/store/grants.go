package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// ErrProjectNotGranted refuses a step or a message for a project the
// team's hub does not grant the team.
var ErrProjectNotGranted = errors.New("project_not_granted")

// The states of a team's grants, as team.read last answered.
const (
	// GrantsEnabled: the hub holds the team's profile, enabled, with its
	// grants.
	GrantsEnabled = "enabled"
	// GrantsDisabled: the hub's operator disabled the team's profile; it
	// grants nothing.
	GrantsDisabled = "disabled"
	// GrantsNotRegistered: the hub holds no profile for the team.
	GrantsNotRegistered = "not_registered"
)

// TeamGrant is one project the team's hub grants it, as team.read last
// answered: the project, its repository and the process it selects.
type TeamGrant struct {
	HubID      string           `json:"hub_id"`
	ProjectID  string           `json:"project_id"`
	Repository *GrantRepository `json:"repository,omitempty"`
	Process    *GrantProcess    `json:"process,omitempty"`
}

// GrantRepository is a granted project's repository binding on the hub.
type GrantRepository struct {
	Kind   string `json:"kind"`
	URL    string `json:"url"`
	Host   string `json:"host,omitempty"`
	Access string `json:"access,omitempty"`
}

// GrantProcess is a granted project's selected process pin on the hub.
type GrantProcess struct {
	Repo     string `json:"repo"`
	Commit   string `json:"commit"`
	Manifest string `json:"manifest"`
}

// TeamGrantsRead is one team.read of a team: the state and grants the hub
// answered, and when the read was sent.
type TeamGrantsRead struct {
	// HubID is the hub read: the team's.
	HubID  string
	State  string
	Grants []TeamGrant
	At     time.Time
}

const maxGrantField = 2048

func (r TeamGrantsRead) validate() error {
	switch r.State {
	case GrantsEnabled:
	case GrantsDisabled, GrantsNotRegistered:
		if len(r.Grants) > 0 {
			return fmt.Errorf("%w: a %s team has no grants", ErrInvalid, r.State)
		}
	default:
		return fmt.Errorf("%w: grants state %q", ErrInvalid, r.State)
	}
	if r.At.IsZero() || !refPattern.MatchString(r.HubID) {
		return fmt.Errorf("%w: a team read names its hub and when it was sent", ErrInvalid)
	}
	seen := map[ProjectRef]bool{}
	for _, g := range r.Grants {
		p := ProjectRef{HubID: g.HubID, ProjectID: g.ProjectID}
		if !g.Valid() || g.HubID != r.HubID || seen[p] {
			return fmt.Errorf("%w: granted project %q/%q", ErrInvalid, p.HubID, p.ProjectID)
		}
		seen[p] = true
	}
	return nil
}

// Valid reports whether a grant can be recorded: hub and project IDs of the
// shape a task names them by, and fields of at most 2048 bytes. A hub's
// project that is not one can never be offered or claimed.
func (g TeamGrant) Valid() bool {
	if !refPattern.MatchString(g.HubID) || !refPattern.MatchString(g.ProjectID) {
		return false
	}
	fields := []string{}
	if g.Repository != nil {
		fields = append(fields, g.Repository.Kind, g.Repository.URL, g.Repository.Host, g.Repository.Access)
	}
	if g.Process != nil {
		fields = append(fields, g.Process.Repo, g.Process.Commit, g.Process.Manifest)
	}
	for _, f := range fields {
		if len(f) > maxGrantField {
			return false
		}
	}
	return true
}

// RecordTeamGrants replaces a team's grants snapshot with a team.read's
// answer, unless a read sent later is already recorded: a slow read never
// undoes a newer one. In the same transaction it blocks the team's open
// attempts on the hub whose project the read no longer grants, and unblocks
// those it grants again (applyBlocks). It reports whether the read was recorded. The
// operator and aicrewd's own reconciler may record; the snapshot is
// aicrewd's copy of the hub's grants and changes nothing in aimem.
func (s *Store) RecordTeamGrants(ctx context.Context, c Caller, teamID string, read TeamGrantsRead) (bool, error) {
	if c.kind != callerReconciler && !(c.kind == callerOperator && c.id != "") {
		return false, fmt.Errorf("%w: only the operator or aicrewd records a team's grants", ErrForbidden)
	}
	if err := read.validate(); err != nil {
		return false, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, fmt.Errorf("begin: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck // no-op after commit
	var last string
	err = tx.QueryRowContext(ctx, `SELECT grants_read_at FROM teams WHERE id = ?`, teamID).Scan(&last)
	if errors.Is(err, sql.ErrNoRows) {
		return false, fmt.Errorf("team %s: %w", teamID, ErrNotFound)
	}
	if err != nil {
		return false, fmt.Errorf("read team: %w", err)
	}
	if last != "" {
		prev, err := parseTime(last)
		if err != nil {
			return false, err
		}
		if read.At.Before(prev) {
			return false, nil
		}
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM team_grants WHERE team_id = ?`, teamID); err != nil {
		return false, fmt.Errorf("clear team grants: %w", err)
	}
	for _, g := range read.Grants {
		var repo GrantRepository
		var proc GrantProcess
		if g.Repository != nil {
			repo = *g.Repository
		}
		if g.Process != nil {
			proc = *g.Process
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO team_grants (team_id, hub_id, project_id, has_repository, repository_kind, repository_url,
			  repository_host, repository_access, has_process, process_repo, process_commit, process_manifest)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			teamID, g.HubID, g.ProjectID, g.Repository != nil, repo.Kind, repo.URL, repo.Host, repo.Access,
			g.Process != nil, proc.Repo, proc.Commit, proc.Manifest); err != nil {
			return false, fmt.Errorf("insert team grant: %w", err)
		}
	}
	if _, err := tx.ExecContext(ctx, `UPDATE teams SET grants_state = ?, grants_read_at = ? WHERE id = ?`,
		read.State, formatTime(read.At), teamID); err != nil {
		return false, fmt.Errorf("record team grants: %w", err)
	}
	if err := applyBlocks(ctx, tx, teamID, read, s.now()); err != nil {
		return false, err
	}
	if err := tx.Commit(); err != nil {
		return false, fmt.Errorf("commit: %w", err)
	}
	return true, nil
}

// requireTeamGrant refuses a project the team's grants snapshot does not
// hold.
func requireTeamGrant(ctx context.Context, q querier, teamID string, p ProjectRef) error {
	var one int
	err := q.QueryRowContext(ctx,
		`SELECT 1 FROM team_grants WHERE team_id = ? AND hub_id = ? AND project_id = ?`,
		teamID, p.HubID, p.ProjectID).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("%w: the team's hub does not grant project %s/%s", ErrProjectNotGranted, p.HubID, p.ProjectID)
	}
	if err != nil {
		return fmt.Errorf("read team grants: %w", err)
	}
	return nil
}

// teamGrants reads a team's grants snapshot, ordered by hub and project.
func teamGrants(ctx context.Context, q querier, teamID string) ([]TeamGrant, error) {
	rows, err := q.QueryContext(ctx,
		`SELECT hub_id, project_id, has_repository, repository_kind, repository_url, repository_host, repository_access,
		        has_process, process_repo, process_commit, process_manifest
		 FROM team_grants WHERE team_id = ? ORDER BY hub_id, project_id`, teamID)
	if err != nil {
		return nil, fmt.Errorf("read team grants: %w", err)
	}
	defer rows.Close()
	out := []TeamGrant{}
	for rows.Next() {
		var (
			g             TeamGrant
			hasRepo, hasP bool
			repo          GrantRepository
			proc          GrantProcess
		)
		if err := rows.Scan(&g.HubID, &g.ProjectID, &hasRepo, &repo.Kind, &repo.URL, &repo.Host, &repo.Access,
			&hasP, &proc.Repo, &proc.Commit, &proc.Manifest); err != nil {
			return nil, err
		}
		if hasRepo {
			g.Repository = &repo
		}
		if hasP {
			g.Process = &proc
		}
		out = append(out, g)
	}
	return out, rows.Err()
}
