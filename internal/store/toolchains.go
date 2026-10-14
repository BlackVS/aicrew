package store

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/BlackVS/aicrew/internal/toolchain"
)

// A team's toolchains (pilot finding #18): the operator selects one named
// list per team and project, and a worker's turn admits its rules besides
// the common ones. The project need not be granted yet; a selection for a
// project the hub does not grant is kept and carried once it is.

type toolchainSet struct {
	TeamID    string `json:"team_id"`
	ProjectID string `json:"project_id"`
	Toolchain string `json:"toolchain"`
}

// SetTeamToolchain selects the named toolchain for the team's project, or
// clears the selection when name is "". It advances the team's revision
// when the selection changes. Operator only.
func (s *Store) SetTeamToolchain(ctx context.Context, c Caller, key, teamID, projectID, name string) (Team, error) {
	in := toolchainSet{TeamID: teamID, ProjectID: projectID, Toolchain: name}
	var out Team
	err := s.run(ctx, c, command{
		op: opSetToolchain, scope: teamID, key: key, input: in, authorize: requireOperator,
		validate: func() error {
			if !refPattern.MatchString(projectID) {
				return fmt.Errorf("%w: project %q", ErrInvalid, projectID)
			}
			if name != "" && !toolchain.Valid(name) {
				return fmt.Errorf("%w: toolchain %q is not one of %v", ErrInvalid, name, toolchain.Names())
			}
			return nil
		},
		apply: func(ctx context.Context, tx *sql.Tx, now time.Time) (any, error) {
			t, err := getTeam(ctx, tx, teamID)
			if err != nil {
				return nil, err
			}
			if t.Toolchains[projectID] == name {
				return t, nil
			}
			if name == "" {
				_, err = tx.ExecContext(ctx, `DELETE FROM team_toolchains WHERE team_id = ? AND project_id = ?`,
					teamID, projectID)
			} else {
				_, err = tx.ExecContext(ctx,
					`INSERT INTO team_toolchains (team_id, project_id, toolchain, updated_at) VALUES (?, ?, ?, ?)
					 ON CONFLICT (team_id, project_id) DO UPDATE SET toolchain = excluded.toolchain,
					   updated_at = excluded.updated_at`, teamID, projectID, name, formatTime(now))
			}
			if err != nil {
				return nil, fmt.Errorf("set toolchain: %w", err)
			}
			if _, err := tx.ExecContext(ctx, `UPDATE teams SET revision = revision + 1, updated_at = ? WHERE id = ?`,
				formatTime(now), teamID); err != nil {
				return nil, fmt.Errorf("set toolchain: %w", err)
			}
			return getTeam(ctx, tx, teamID)
		},
	}, &out)
	return out, err
}

// teamToolchains are the team's selected toolchains by project, or nil.
func teamToolchains(ctx context.Context, q querier, teamID string) (map[string]string, error) {
	rows, err := q.QueryContext(ctx, `SELECT project_id, toolchain FROM team_toolchains WHERE team_id = ?`, teamID)
	if err != nil {
		return nil, fmt.Errorf("read toolchains: %w", err)
	}
	defer rows.Close()
	var out map[string]string
	for rows.Next() {
		var project, name string
		if err := rows.Scan(&project, &name); err != nil {
			return nil, fmt.Errorf("read toolchains: %w", err)
		}
		if out == nil {
			out = map[string]string{}
		}
		out[project] = name
	}
	return out, rows.Err()
}
