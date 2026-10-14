package store

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

// boardMessages are the board.changed announcements a member has pending.
func boardMessages(t *testing.T, s *Store, teamID string, m crewMember) []InboxItem {
	t.Helper()
	items, err := pendingMessages(context.Background(), s.db, teamID, m.agent.ID, maxInboxPage)
	if err != nil {
		t.Fatal(err)
	}
	var out []InboxItem
	for _, it := range items {
		if it.Board != nil {
			out = append(out, it)
		}
	}
	return out
}

func readyChange(taskID string, rev int64, at time.Time) BoardChange {
	return BoardChange{ProjectID: "project-a", TaskID: taskID, Revision: rev, From: "BACKLOG", To: "READY", At: at}
}

// The first page sees the granted project for the first time: its changes
// until then are history and announce nothing. A task that becomes READY
// afterwards reaches the coordinator, and only the coordinator, once,
// naming the task and the change; the cursor is stored with it.
func TestBoardAnnouncesReadyOnce(t *testing.T) {
	ctx := context.Background()
	s, _ := openTemp(t)
	now := clock(s)
	e := newExecTeam(t, s)
	rc := ReconcilerCaller()

	if cur, err := s.BoardCursor(ctx, rc, "hub-a"); err != nil || cur != "" {
		t.Fatalf("before the first read: %q %v", cur, err)
	}
	history := BoardPage{Changes: []BoardChange{readyChange("old-1", 2, now.Add(-time.Hour)),
		readyChange("old-2", 5, now.Add(-time.Minute))}, Cursor: "c1"}
	if n, err := s.RecordBoardPage(ctx, rc, "hub-a", history); err != nil || n != 0 {
		t.Fatalf("the board's history: %d announced, %v", n, err)
	}
	if got := boardMessages(t, s, e.tm.ID, e.lead); len(got) != 0 {
		t.Fatalf("history announced: %+v", got)
	}

	*now = now.Add(time.Minute)
	page := BoardPage{From: "c1", Changes: []BoardChange{readyChange("task-9", 4, *now),
		{ProjectID: "project-a", TaskID: "task-8", Revision: 2, From: "READY", To: "IN_PROGRESS", At: *now},
		{ProjectID: "project-other", TaskID: "task-7", Revision: 1, To: "READY", At: *now}}, Cursor: "c2"}
	if n, err := s.RecordBoardPage(ctx, rc, "hub-a", page); err != nil || n != 1 {
		t.Fatalf("a READY task: %d announced, %v", n, err)
	}
	got := boardMessages(t, s, e.tm.ID, e.lead)
	if len(got) != 1 || got[0].Kind != KindLifecycle || got[0].Task == nil || got[0].Task.TaskID != "task-9" ||
		got[0].Task.HubID != "hub-a" || got[0].Board.To != "READY" || got[0].Board.From != "BACKLOG" || got[0].Board.Revision != 4 ||
		got[0].Text != "Task task-9 of project project-a moved from BACKLOG to READY on the board (revision 4)." {
		t.Fatalf("the coordinator's announcements: %+v", got)
	}
	if w := boardMessages(t, s, e.tm.ID, e.builder); len(w) != 0 {
		t.Fatalf("the worker got %+v", w)
	}
	if cur, _ := s.BoardCursor(ctx, rc, "hub-a"); cur != "c2" {
		t.Fatalf("cursor %q", cur)
	}

	// The same page again, as after a restart that read from the old
	// cursor: refused, and nothing is announced twice.
	if _, err := s.RecordBoardPage(ctx, rc, "hub-a", page); !errors.Is(err, ErrBoardCursorMoved) {
		t.Fatalf("a page from a cursor already moved past: %v", err)
	}
	if got := boardMessages(t, s, e.tm.ID, e.lead); len(got) != 1 {
		t.Fatalf("after the replay: %d announcements", len(got))
	}
}

// A change made by one of aicrew's own steps (a committed step records its
// revision) is not announced, however many steps ran before the read; a later change of the same task made outside aicrew is,
// whatever its state, because the team has an attempt for it.
func TestBoardAnnouncesChangesOfAnAttemptsTask(t *testing.T) {
	ctx := context.Background()
	s, _ := openTemp(t)
	now := clock(s)
	e := newExecTeam(t, s)
	rc := ReconcilerCaller()
	if _, err := s.RecordBoardPage(ctx, rc, "hub-a", BoardPage{Changes: []BoardChange{}, Cursor: "c1"}); err != nil {
		t.Fatal(err)
	}
	*now = now.Add(time.Minute)
	// Two steps of the attempt before the board is read: the worker blocks,
	// then resumes. Each one's revision is aicrew's own, not only the last,
	// which is all the attempt itself keeps.
	running := e.accept(t, "a1", e.offer(t, "o1", "task-1"))
	blocked := e.mustWork(t, "w1", running, IntentBlock, "waiting on the schema")
	a := e.mustWork(t, "w2", blocked, IntentResume, "")
	if blocked.TaskRevision <= running.TaskRevision || a.TaskRevision <= blocked.TaskRevision {
		t.Fatalf("the steps' task revisions: accept %d, block %d, resume %d", running.TaskRevision, blocked.TaskRevision, a.TaskRevision)
	}
	own1 := BoardChange{ProjectID: "project-a", TaskID: "task-1", Revision: blocked.TaskRevision, From: "IN_PROGRESS", To: "BLOCKED", At: *now}
	own2 := BoardChange{ProjectID: "project-a", TaskID: "task-1", Revision: a.TaskRevision, From: "BLOCKED", To: "IN_PROGRESS", At: *now}
	rev := a.TaskRevision + 1
	cancelled := BoardChange{ProjectID: "project-a", TaskID: "task-1", Revision: rev, From: "IN_PROGRESS",
		To: "CANCELLED", At: now.Add(time.Second)}
	page := BoardPage{From: "c1", Changes: []BoardChange{own1, own2, cancelled}, Cursor: "c2"}
	if n, err := s.RecordBoardPage(ctx, rc, "hub-a", page); err != nil || n != 1 {
		t.Fatalf("%d announced, %v", n, err)
	}
	got := boardMessages(t, s, e.tm.ID, e.lead)
	if len(got) != 1 || got[0].Board.To != "CANCELLED" || got[0].Board.Revision != rev ||
		got[0].Text != fmt.Sprintf("Task task-1 of project project-a moved from IN_PROGRESS to CANCELLED on the board (revision %d)."+
			" The team has an attempt for it.", rev) {
		t.Fatalf("announcements: %+v", got)
	}
}

// A project granted after the first read is first seen then: its history
// is not announced, its later changes are. A cursor_ahead reset makes
// everything until then history, so nothing is announced twice.
func TestBoardNewGrantAndReset(t *testing.T) {
	ctx := context.Background()
	s, _ := openTemp(t)
	now := clock(s)
	e := newExecTeam(t, s)
	rc := ReconcilerCaller()
	if _, err := s.RecordBoardPage(ctx, rc, "hub-a", BoardPage{Changes: []BoardChange{}, Cursor: "c1"}); err != nil {
		t.Fatal(err)
	}
	*now = now.Add(time.Hour)
	read := TeamGrantsRead{HubID: "hub-a", State: GrantsEnabled, At: *now, Grants: []TeamGrant{
		{HubID: "hub-a", ProjectID: "project-a"}, {HubID: "hub-a", ProjectID: "project-b"}}}
	if _, err := s.RecordTeamGrants(ctx, rc, e.tm.ID, read); err != nil {
		t.Fatal(err)
	}
	b := func(task string, rev int64, at time.Time) BoardChange {
		return BoardChange{ProjectID: "project-b", TaskID: task, Revision: rev, From: "BACKLOG", To: "READY", At: at}
	}
	page := BoardPage{From: "c1", Changes: []BoardChange{b("b-old", 3, now.Add(-time.Minute)), b("b-new", 4, now.Add(time.Second))}, Cursor: "c2"}
	if n, err := s.RecordBoardPage(ctx, rc, "hub-a", page); err != nil || n != 1 {
		t.Fatalf("a new grant: %d announced, %v", n, err)
	}
	if got := boardMessages(t, s, e.tm.ID, e.lead); len(got) != 1 || got[0].Task.TaskID != "b-new" {
		t.Fatalf("announcements: %+v", got)
	}

	*now = now.Add(time.Minute)
	if err := s.ResetBoardCursor(ctx, rc, "hub-a"); err != nil {
		t.Fatal(err)
	}
	if cur, _ := s.BoardCursor(ctx, rc, "hub-a"); cur != "" {
		t.Fatalf("cursor after the reset: %q", cur)
	}
	again := BoardPage{Changes: []BoardChange{b("b-new", 4, now.Add(-time.Minute+time.Second))}, Cursor: "r1"}
	if n, err := s.RecordBoardPage(ctx, rc, "hub-a", again); err != nil || n != 0 {
		t.Fatalf("the feed read again after a restore: %d announced, %v", n, err)
	}
}

// Only aicrewd's reconciler reads and records the board, and a page must
// name the cursor to read on from.
func TestBoardCallers(t *testing.T) {
	ctx := context.Background()
	s, _ := openTemp(t)
	op := operator(t)
	if _, err := s.BoardCursor(ctx, op, "hub-a"); !errors.Is(err, ErrForbidden) {
		t.Errorf("the operator reads the cursor: %v", err)
	}
	if _, err := s.RecordBoardPage(ctx, op, "hub-a", BoardPage{Cursor: "c"}); !errors.Is(err, ErrForbidden) {
		t.Errorf("the operator records: %v", err)
	}
	if err := s.ResetBoardCursor(ctx, op, "hub-a"); !errors.Is(err, ErrForbidden) {
		t.Errorf("the operator resets: %v", err)
	}
	rc := ReconcilerCaller()
	if _, err := s.RecordBoardPage(ctx, rc, "hub-a", BoardPage{}); !errors.Is(err, ErrInvalid) {
		t.Errorf("a page without a cursor: %v", err)
	}
	bad := BoardPage{Cursor: "c", Changes: []BoardChange{{ProjectID: "project-a", TaskID: "t", To: "READY", At: time.Now()}}}
	if _, err := s.RecordBoardPage(ctx, rc, "hub-a", bad); !errors.Is(err, ErrInvalid) {
		t.Errorf("a change without a revision: %v", err)
	}
}

// readyNotices are the coordinator's pending messages that list READY tasks
// at a session start.
func readyNotices(t *testing.T, s *Store, teamID string, m crewMember) []InboxItem {
	t.Helper()
	items, err := pendingMessages(context.Background(), s.db, teamID, m.agent.ID, maxInboxPage)
	if err != nil {
		t.Fatal(err)
	}
	var out []InboxItem
	for _, it := range items {
		if it.Kind == KindLifecycle && strings.HasPrefix(it.Text, "At this coordinator session's start") {
			out = append(out, it)
		}
	}
	return out
}

// A coordinator session that starts or resumes is told the READY tasks of
// its granted projects that have no open attempt of the team (pilot finding
// #17), from the states the board feed recorded, history included. A task
// with an open attempt, a task no longer READY, a project not granted and a
// worker's session add nothing; a closed attempt's task is listed again.
func TestReadyTasksAtTheCoordinatorsStart(t *testing.T) {
	ctx := context.Background()
	s, _ := openTemp(t)
	now := clock(s)
	e := newExecTeam(t, s)
	rc := ReconcilerCaller()
	resume := func(key string) {
		t.Helper()
		sess, err := s.ResumeSession(ctx, e.lead.caller, key, e.lead.sess.ID)
		if err != nil {
			t.Fatal(err)
		}
		e.lead.sess = sess
	}

	resume("resume-0")
	if got := readyNotices(t, s, e.tm.ID, e.lead); len(got) != 0 {
		t.Fatalf("an empty board: %+v", got)
	}
	history := BoardPage{Changes: []BoardChange{readyChange("task-1", 2, now.Add(-time.Hour)),
		readyChange("task-2", 2, now.Add(-time.Hour)), readyChange("task-3", 2, now.Add(-time.Hour)),
		{ProjectID: "project-a", TaskID: "task-3", Revision: 3, From: "READY", To: "IN_PROGRESS", At: now.Add(-time.Minute)},
		readyChange("task-4", 5, now.Add(-time.Minute))}, Cursor: "c1"}
	if n, err := s.RecordBoardPage(ctx, rc, "hub-a", history); err != nil || n != 0 {
		t.Fatalf("history: %d announced, %v", n, err)
	}
	// A later page that repeats an older revision does not move a task back.
	stale := BoardPage{From: "c1", Changes: []BoardChange{readyChange("task-3", 2, now.Add(-time.Hour))}, Cursor: "c2"}
	if _, err := s.RecordBoardPage(ctx, rc, "hub-a", stale); err != nil {
		t.Fatal(err)
	}
	*now = now.Add(time.Minute)
	if _, err := s.RecordBoardPage(ctx, rc, "hub-a", BoardPage{From: "c2", Changes: []BoardChange{
		{ProjectID: "project-other", TaskID: "task-9", Revision: 1, To: "READY", At: *now}}, Cursor: "c3"}); err != nil {
		t.Fatal(err)
	}
	a := e.offer(t, "o2", "task-2")

	resume("resume-1")
	got := readyNotices(t, s, e.tm.ID, e.lead)
	want := "At this coordinator session's start, 2 tasks are READY on the board in project project-a with no open attempt" +
		" of the team: task-1, task-4."
	if len(got) != 1 || got[0].Text != want || got[0].Project == nil || got[0].Project.ProjectID != "project-a" ||
		got[0].Project.HubID != "hub-a" || got[0].Task != nil {
		t.Fatalf("at the resume: %+v", got)
	}
	if w := readyNotices(t, s, e.tm.ID, e.builder); len(w) != 0 {
		t.Fatalf("the worker was told: %+v", w)
	}
	if _, err := s.ResumeSession(ctx, e.builder.caller, "resume-builder", e.builder.sess.ID); err != nil {
		t.Fatal(err)
	}
	if n := readyNotices(t, s, e.tm.ID, e.lead); len(n) != 1 {
		t.Fatalf("a worker's resume told the coordinator: %+v", n)
	}

	// Released, the offer's task is listed again at a new session's start.
	if _, err := e.release(t, "r2", a); err != nil {
		t.Fatal(err)
	}
	if _, err := s.StopSession(ctx, operator(t), "stop-lead", e.lead.sess.ID); err != nil {
		t.Fatal(err)
	}
	var err error
	if e.lead.sess, err = s.StartSession(ctx, e.lead.caller, "start-lead-2", e.tm.ID); err != nil {
		t.Fatal(err)
	}
	got = readyNotices(t, s, e.tm.ID, e.lead)
	if len(got) != 2 || !strings.HasSuffix(got[1].Text, ": task-1, task-2, task-4.") {
		t.Fatalf("at the new session's start: %+v", got)
	}

	// A hub restore drops the recorded states with the cursor.
	if err := s.ResetBoardCursor(ctx, rc, "hub-a"); err != nil {
		t.Fatal(err)
	}
	resume("resume-2")
	if n := readyNotices(t, s, e.tm.ID, e.lead); len(n) != 2 {
		t.Fatalf("after a reset, before the feed is read again: %d notices", len(n))
	}
}

// One message names at most maxReadyListed tasks and counts the rest.
func TestReadyTasksTextBound(t *testing.T) {
	tasks := make([]string, maxReadyListed+3)
	for i := range tasks {
		tasks[i] = fmt.Sprintf("t%d", i)
	}
	text := readyTasksText("p", tasks)
	if !strings.HasSuffix(text, ", t49 and 3 more.") || strings.Contains(text, "t50") ||
		!strings.Contains(text, "53 tasks are READY") {
		t.Fatalf("text %q", text)
	}
	if one := readyTasksText("p", []string{"t"}); one != "At this coordinator session's start, 1 task is READY on the board in project p with no open attempt of the team: t." {
		t.Fatalf("one task: %q", one)
	}
}
