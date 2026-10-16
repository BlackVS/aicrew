package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/BlackVS/aicrew/internal/events"
)

// The board wake (docs/DESIGN-CONTROL-PLANE.md, A1; docs/CREW-CONTRACT.md,
// "Board changes"). aicrewd reads each hub's board feed (aimem's board.read)
// and journals each change of a granted project as a board.changed event
// (events.go), queued for each team granted the project with the facts the
// subscriptions' conditions read: the default board-wake subscription wakes
// a team's coordinators when a task becomes READY, or changes state while
// the team has an attempt for it, unless aicrew made the change itself. The
// feed's cursor is stored in the transaction that journals the events, so a
// change is journaled once, across restarts.
//
// A project's changes from before aicrewd first saw it granted are history:
// they are read, and announced to no one. That covers the first read, which
// has no cursor and starts at each project's first change, and a project
// granted later, whose history the feed then returns from its start.

// ErrBoardCursorMoved refuses recording a page read from a cursor that is
// no longer the stored one: another read recorded first. Nothing changed;
// read again from the stored cursor.
var ErrBoardCursorMoved = errors.New("board_cursor_moved")

// BoardChange is one entry of a hub's board feed.
type BoardChange struct {
	ProjectID          string
	TaskID             string
	Revision           int64
	From, To           string
	At                 time.Time
	RequiredCapability string
}

// BoardPage is one read of a hub's board feed: the cursor it was read from
// ("" for none), its changes, and the cursor to read on from.
type BoardPage struct {
	From    string
	Changes []BoardChange
	Cursor  string
}

// BoardDetail is the board change a board.changed announcement carries.
type BoardDetail struct {
	Revision           int64     `json:"revision"`
	From               string    `json:"from"`
	To                 string    `json:"to"`
	At                 time.Time `json:"at"`
	RequiredCapability string    `json:"required_capability,omitempty"`
}

// boardReady is the state whose arrival is announced for any task.
const boardReady = "READY"

const maxBoardCursor = 4096

// BoardCursor returns the hub's stored board cursor, "" before the first
// read. aicrewd's reconciler only.
func (s *Store) BoardCursor(ctx context.Context, c Caller, hubID string) (string, error) {
	if c.kind != callerReconciler {
		return "", fmt.Errorf("%w: only aicrewd reads the board", ErrForbidden)
	}
	var cur string
	err := s.db.QueryRowContext(ctx, `SELECT cursor FROM board_cursors WHERE hub_id = ?`, hubID).Scan(&cur)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("read the board cursor: %w", err)
	}
	return cur, nil
}

// RecordBoardPage records one page of the hub's board feed: in one
// transaction it journals the page's events, queues their deliveries and
// stores its cursor, if the stored cursor is still the one the page was read
// from (ErrBoardCursorMoved otherwise). It returns how many deliveries it
// queued: each becomes a coordinator's announcement when DeliverEvents runs
// its queue. aicrewd's reconciler only.
func (s *Store) RecordBoardPage(ctx context.Context, c Caller, hubID string, page BoardPage) (int, error) {
	if c.kind != callerReconciler {
		return 0, fmt.Errorf("%w: only aicrewd records the board", ErrForbidden)
	}
	if !refPattern.MatchString(hubID) || page.Cursor == "" || len(page.Cursor) > maxBoardCursor {
		return 0, fmt.Errorf("%w: a board page names its hub and the cursor to read on from", ErrInvalid)
	}
	for _, ch := range page.Changes {
		if !refPattern.MatchString(ch.ProjectID) || !refPattern.MatchString(ch.TaskID) || ch.Revision < 1 || ch.To == "" || ch.At.IsZero() {
			return 0, fmt.Errorf("%w: a board change names its project, task, revision, state and time", ErrInvalid)
		}
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("begin: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck // no-op after commit
	var stored string
	err = tx.QueryRowContext(ctx, `SELECT cursor FROM board_cursors WHERE hub_id = ?`, hubID).Scan(&stored)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return 0, fmt.Errorf("read the board cursor: %w", err)
	}
	if stored != page.From {
		return 0, ErrBoardCursorMoved
	}
	now := s.now()
	since, err := boardSince(ctx, tx, hubID, now)
	if err != nil {
		return 0, err
	}
	announced := 0
	for _, ch := range page.Changes {
		first, granted := since[ch.ProjectID]
		if !granted {
			continue
		}
		if err := recordBoardTask(ctx, tx, hubID, ch); err != nil {
			return 0, err
		}
		if ch.At.Before(first) {
			continue
		}
		n, err := s.journalBoardChange(ctx, tx, hubID, ch, now)
		if err != nil {
			return 0, err
		}
		announced += n
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO board_cursors (hub_id, cursor, updated_at) VALUES (?, ?, ?)
		 ON CONFLICT (hub_id) DO UPDATE SET cursor = excluded.cursor, updated_at = excluded.updated_at`,
		hubID, page.Cursor, formatTime(now)); err != nil {
		return 0, fmt.Errorf("store the board cursor: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("commit: %w", err)
	}
	return announced, nil
}

// ResetBoardCursor drops the hub's stored cursor, after the hub answered
// that it is past its feed (its state was restored), and makes every
// granted project's changes until now history: the feed is read again from
// its start, and nothing announced before is announced again. The tasks'
// recorded states are dropped with it. aicrewd's reconciler only.
func (s *Store) ResetBoardCursor(ctx context.Context, c Caller, hubID string) error {
	if c.kind != callerReconciler {
		return fmt.Errorf("%w: only aicrewd resets the board", ErrForbidden)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck // no-op after commit
	now := formatTime(s.now())
	if _, err := tx.ExecContext(ctx, `DELETE FROM board_cursors WHERE hub_id = ?`, hubID); err != nil {
		return fmt.Errorf("drop the board cursor: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `UPDATE board_projects SET since = ? WHERE hub_id = ?`, now, hubID); err != nil {
		return fmt.Errorf("reset the board's projects: %w", err)
	}
	// A restored hub's revisions may run behind the ones recorded: its
	// tasks' states are read again from its feed.
	if _, err := tx.ExecContext(ctx, `DELETE FROM board_tasks WHERE hub_id = ?`, hubID); err != nil {
		return fmt.Errorf("drop the board's task states: %w", err)
	}
	return tx.Commit()
}

// boardSince records the first sight of each project some team holds a
// grant for on the hub, and returns every granted project's first sight.
// A project no team is granted is not in it.
func boardSince(ctx context.Context, tx *sql.Tx, hubID string, now time.Time) (map[string]time.Time, error) {
	if _, err := tx.ExecContext(ctx,
		`INSERT OR IGNORE INTO board_projects (hub_id, project_id, since)
		 SELECT DISTINCT hub_id, project_id, ? FROM team_grants WHERE hub_id = ?`, formatTime(now), hubID); err != nil {
		return nil, fmt.Errorf("record the board's projects: %w", err)
	}
	rows, err := tx.QueryContext(ctx,
		`SELECT p.project_id, p.since FROM board_projects p
		 WHERE p.hub_id = ? AND EXISTS (SELECT 1 FROM team_grants g WHERE g.hub_id = p.hub_id AND g.project_id = p.project_id)`,
		hubID)
	if err != nil {
		return nil, fmt.Errorf("read the board's projects: %w", err)
	}
	defer rows.Close()
	out := map[string]time.Time{}
	for rows.Next() {
		var project, since string
		if err := rows.Scan(&project, &since); err != nil {
			return nil, err
		}
		t, err := parseTime(since)
		if err != nil {
			return nil, err
		}
		out[project] = t
	}
	return out, rows.Err()
}

// journalBoardChange journals ch as a board.changed event, queued for each
// team granted its project with the facts of that team: whether one of
// aicrew's own committed steps made the change (a step records its task
// revision), whether the task became READY, and whether the team has an
// attempt for it. It returns how many deliveries it queued.
func (s *Store) journalBoardChange(ctx context.Context, tx *sql.Tx, hubID string, ch BoardChange, now time.Time) (int, error) {
	task := TaskRef{HubID: hubID, ProjectID: ch.ProjectID, TaskID: ch.TaskID}
	var own int
	err := tx.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM attempt_steps s JOIN attempts a ON a.id = s.attempt_id
		 WHERE a.task_hub_id = ? AND a.task_project_id = ? AND a.task_id = ? AND s.outcome = 'committed' AND s.task_revision = ?`,
		task.HubID, task.ProjectID, task.TaskID, ch.Revision).Scan(&own)
	if err != nil {
		return 0, fmt.Errorf("read the task's attempts: %w", err)
	}
	teams, err := boardTeams(ctx, tx, task)
	if err != nil {
		return 0, err
	}
	var ms []match
	for _, t := range teams {
		ms = append(ms, s.matches(events.KindBoardChanged, t.id,
			events.Facts{OwnChange: own > 0, Ready: ch.To == boardReady, TeamAttempt: t.attempt})...)
	}
	// The key holds the change's time as well as its revision: a hub
	// restored from a backup may number a later change as an earlier one.
	key := fmt.Sprintf("%s/%s/%s@%d@%s", hubID, ch.ProjectID, ch.TaskID, ch.Revision, formatTime(ch.At))
	_, n, err := journal(ctx, tx, sourceBoard, key, taskSubject(task), events.KindBoardChanged,
		boardEvent{Task: task, Change: BoardDetail{Revision: ch.Revision, From: ch.From, To: ch.To, At: ch.At,
			RequiredCapability: ch.RequiredCapability}}, ch.At, now, ms)
	return n, err
}

// boardTeam is a team granted a task's project, and whether it has an
// attempt for the task.
type boardTeam struct {
	id      string
	attempt bool
}

func boardTeams(ctx context.Context, tx *sql.Tx, task TaskRef) ([]boardTeam, error) {
	rows, err := tx.QueryContext(ctx,
		`SELECT g.team_id, EXISTS (SELECT 1 FROM attempts a WHERE a.team_id = g.team_id AND a.task_hub_id = g.hub_id
		        AND a.task_project_id = g.project_id AND a.task_id = ?)
		 FROM team_grants g WHERE g.hub_id = ? AND g.project_id = ? ORDER BY g.team_id`,
		task.TaskID, task.HubID, task.ProjectID)
	if err != nil {
		return nil, fmt.Errorf("read the project's teams: %w", err)
	}
	defer rows.Close()
	var out []boardTeam
	for rows.Next() {
		var t boardTeam
		if err := rows.Scan(&t.id, &t.attempt); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// boardText is an announcement's text, built from the change's fields only:
// never from a task's content.
func boardText(ch BoardChange, attempt bool) string {
	text := fmt.Sprintf("Task %s of project %s moved from %s to %s on the board (revision %d).",
		ch.TaskID, ch.ProjectID, ch.From, ch.To, ch.Revision)
	if ch.From == "" {
		text = fmt.Sprintf("Task %s of project %s was created on the board in %s (revision %d).",
			ch.TaskID, ch.ProjectID, ch.To, ch.Revision)
	}
	if attempt {
		text += " The team has an attempt for it."
	}
	return text
}
