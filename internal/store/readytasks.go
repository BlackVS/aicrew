package store

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"
)

// READY tasks at a coordinator's start (pilot finding #17, 2026-10-14). A
// board.changed announcement is written once; a coordinator that
// acknowledged it in a turn that could not act, and then restarted, had
// nothing in its inbox to start a turn. So when a coordinator session
// starts or resumes, aicrewd writes one lifecycle message for each granted
// project that has READY tasks without an open attempt of the team, listing
// them. aicrewd knows each task's state from the board feed it reads
// (board.go): every page records its tasks' latest states, history too.

// maxReadyListed bounds the tasks one message names; the rest are counted.
const maxReadyListed = 50

// readyTasksText is the message's text, built from the tasks' IDs alone.
func readyTasksText(projectID string, tasks []string) string {
	listed := tasks
	more := ""
	if len(listed) > maxReadyListed {
		listed, more = listed[:maxReadyListed], fmt.Sprintf(" and %d more", len(tasks)-maxReadyListed)
	}
	noun := "tasks are"
	if len(tasks) == 1 {
		noun = "task is"
	}
	return fmt.Sprintf("At this coordinator session's start, %d %s READY on the board in project %s with no open attempt of the team: %s%s.",
		len(tasks), noun, projectID, strings.Join(listed, ", "), more)
}

// recordBoardTask keeps ch as its task's latest state, unless a later
// revision of the task is already recorded.
func recordBoardTask(ctx context.Context, tx *sql.Tx, hubID string, ch BoardChange) error {
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO board_tasks (hub_id, project_id, task_id, revision, state, at) VALUES (?, ?, ?, ?, ?, ?)
		 ON CONFLICT (hub_id, project_id, task_id) DO UPDATE SET revision = excluded.revision, state = excluded.state, at = excluded.at
		 WHERE excluded.revision > board_tasks.revision`,
		hubID, ch.ProjectID, ch.TaskID, ch.Revision, ch.To, formatTime(ch.At)); err != nil {
		return fmt.Errorf("record the task's board state: %w", err)
	}
	return nil
}

// noticeReadyTasks runs in the transaction that starts or resumes a
// coordinator session of the team: for each project the team is granted
// with READY tasks that have no open attempt of the team, it writes one
// message to the team's coordinators listing them.
func noticeReadyTasks(ctx context.Context, tx *sql.Tx, teamID string, now time.Time) error {
	rows, err := tx.QueryContext(ctx,
		`SELECT b.hub_id, b.project_id, b.task_id FROM board_tasks b
		 JOIN team_grants g ON g.team_id = ? AND g.hub_id = b.hub_id AND g.project_id = b.project_id
		 WHERE b.state = ? AND NOT EXISTS (SELECT 1 FROM attempts a WHERE a.team_id = g.team_id AND a.task_hub_id = b.hub_id
		       AND a.task_project_id = b.project_id AND a.task_id = b.task_id AND a.state != 'closed')
		 ORDER BY b.hub_id, b.project_id, b.task_id`, teamID, boardReady)
	if err != nil {
		return fmt.Errorf("list the READY tasks: %w", err)
	}
	var projects []ProjectRef
	tasks := map[ProjectRef][]string{}
	for rows.Next() {
		var p ProjectRef
		var task string
		if err := rows.Scan(&p.HubID, &p.ProjectID, &task); err != nil {
			rows.Close()
			return err
		}
		if tasks[p] == nil {
			projects = append(projects, p)
		}
		tasks[p] = append(tasks[p], task)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	if len(projects) == 0 {
		return nil
	}
	recipients, err := activeCoordinators(ctx, tx, teamID)
	if err != nil || len(recipients) == 0 {
		return err
	}
	for _, p := range projects {
		project := p
		if _, err := insertMessage(ctx, tx, Message{TeamID: teamID, Kind: KindLifecycle, Project: &project,
			Text: readyTasksText(p.ProjectID, tasks[p])}, recipients, now); err != nil {
			return err
		}
	}
	return nil
}
