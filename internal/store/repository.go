package store

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"strings"
	"time"
)

// AttemptRepository is the repository an attempt's work happens in, beside
// the attempt's base commit and branch (docs/proposals/PILOT-1-FOLLOWUPS.md,
// 3.5): the forge's API dialect, the clone URL and the access the work
// needs, as the hub binds them to the project, and the default branch the
// base commit was read from. All four are recorded when the attempt is
// created and never change.
type AttemptRepository struct {
	Kind          string `json:"kind"`
	URL           string `json:"url"`
	Access        string `json:"access"`
	DefaultBranch string `json:"default_branch"`
}

// RepositoryKinds are the forge dialects a hub binds a project's repository
// with.
var RepositoryKinds = map[string]bool{"github": true, "gitea": true, "gitlab": true}

const maxRepositoryURL = 2048

func (r AttemptRepository) valid() bool {
	if !RepositoryKinds[r.Kind] || (r.Access != "read" && r.Access != "write") || !validRefs(r.DefaultBranch) {
		return false
	}
	if len(r.URL) > maxRepositoryURL || strings.ContainsAny(r.URL, " \t\r\n") {
		return false
	}
	u, err := url.Parse(r.URL)
	return err == nil && u.Scheme == "https" && u.Host != "" && u.User == nil && u.RawQuery == "" && u.Fragment == ""
}

// Reasons an open attempt is blocked: its team's hub no longer grants the
// attempt's project, or no longer holds an enabled profile for the team.
const (
	BlockedGrantRevoked    = "grant_revoked"
	BlockedProfileDisabled = "profile_disabled"
)

// AttemptBlock is why an open attempt is blocked, and since when. It is a
// condition beside the attempt's state, never a state of its own: every
// step of the state goes on as before, aimem decides each one, and aicrewd
// never touches the hold.
type AttemptBlock struct {
	Reason string    `json:"reason"`
	Since  time.Time `json:"since"`
}

// blockReason is the reason a team read blocks an open attempt of the hub's
// project, or "" when the read grants it.
func blockReason(read TeamGrantsRead, projectID string) string {
	if read.State != GrantsEnabled {
		return BlockedProfileDisabled
	}
	for _, g := range read.Grants {
		if g.ProjectID == projectID {
			return ""
		}
	}
	return BlockedGrantRevoked
}

// applyBlocks sets or clears the block of each open attempt of the team on
// the read's hub, from the team read just recorded, and tells the team of
// each change with a lifecycle message.
func applyBlocks(ctx context.Context, tx *sql.Tx, teamID string, read TeamGrantsRead, now time.Time) error {
	rows, err := tx.QueryContext(ctx,
		`SELECT id, blocked_reason FROM attempts WHERE team_id = ? AND task_hub_id = ? AND state != 'closed' ORDER BY id`,
		teamID, read.HubID)
	if err != nil {
		return fmt.Errorf("read open attempts: %w", err)
	}
	type open struct{ id, reason string }
	var found []open
	for rows.Next() {
		var o open
		if err := rows.Scan(&o.id, &o.reason); err != nil {
			rows.Close()
			return err
		}
		found = append(found, o)
	}
	if err := rows.Close(); err != nil {
		return err
	}
	for _, o := range found {
		a, err := getAttempt(ctx, tx, o.id)
		if err != nil {
			return err
		}
		reason := blockReason(read, a.Task.ProjectID)
		if reason == o.reason {
			continue
		}
		since := ""
		if reason != "" {
			since = formatTime(now)
		}
		if _, err := tx.ExecContext(ctx, `UPDATE attempts SET blocked_reason = ?, blocked_since = ? WHERE id = ?`,
			reason, since, o.id); err != nil {
			return fmt.Errorf("block attempt: %w", err)
		}
		var text string
		switch reason {
		case BlockedGrantRevoked:
			text = "The attempt on task %s is blocked: the hub no longer grants the team project %s. " +
				"The hub's operator grants it again, or the attempt is closed."
		case BlockedProfileDisabled:
			text = "The attempt on task %s is blocked: the hub's team profile is disabled, so project %s is out of reach. " +
				"The hub's operator re-enables it, or the attempt is closed."
		default:
			text = "The attempt on task %s is no longer blocked: the hub grants the team project %s again."
		}
		if err := announce(ctx, tx, a, "", text, now, taskName(a.Task), a.Task.ProjectID); err != nil {
			return err
		}
	}
	return nil
}
