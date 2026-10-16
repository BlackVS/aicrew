package store

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"testing"
	"time"
)

// settle lets the event queues go quiet and runs them, as aicrewd's loop
// does, and returns the runs.
func settle(t *testing.T, s *Store, now *time.Time) []DeliverRun {
	t.Helper()
	*now = now.Add(EventQuiet)
	runs, err := s.DeliverEvents(context.Background(), ReconcilerCaller())
	if err != nil {
		t.Fatal(err)
	}
	return runs
}

func deliver(t *testing.T, s *Store) []DeliverRun {
	t.Helper()
	runs, err := s.DeliverEvents(context.Background(), ReconcilerCaller())
	if err != nil {
		t.Fatal(err)
	}
	return runs
}

// Four changes of one pull request within two minutes, each less than
// EventQuiet after the one before, are one handler run and one message,
// which carries every change; nothing is delivered while they keep coming.
func TestEventBurstWakesOnce(t *testing.T) {
	ctx := context.Background()
	e := newClaimStopEnv(t)
	a, _ := e.runningClaim(t, "task-1")
	o := observed(a.Branch)
	if _, err := e.s.ObservePullWithToken(ctx, e.indepTk, o); err != nil {
		t.Fatal(err)
	}
	steps := []func(){
		func() { o.Labels = []string{"review-this", "hands-reviewing"} },
		func() { o.Comments = 1 },
		func() { o.Labels = append(o.Labels, "hands-reviewed") },
		func() { o.Checks = "success" },
	}
	start := *e.now
	for i, step := range steps {
		step()
		if res, err := e.s.ObservePullWithToken(ctx, e.indepTk, o); err != nil || res.EventID == "" {
			t.Fatalf("change %d: %+v, %v", i, res, err)
		}
		if runs := deliver(t, e.s); len(runs) != 0 {
			t.Fatalf("a run %v after the burst began: %+v", e.now.Sub(start), runs)
		}
		*e.now = e.now.Add(EventQuiet - 5*time.Second)
	}
	if e.now.Sub(start) > 2*time.Minute {
		t.Fatalf("the burst took %v", e.now.Sub(start))
	}
	runs := settle(t, e.s, e.now)
	if len(runs) != 1 || runs[0].Events != 4 || runs[0].Outcome != "handled" || runs[0].Subject != "pull:"+a.ID {
		t.Fatalf("the burst's runs: %+v", runs)
	}
	evs := forgeEvents(t, e.s, e.indepTk)
	want := "label hands-reviewing added|1 new comment(s)|label hands-reviewed added|CI concluded success at 0123456789ab"
	if len(evs) != 1 || strings.Join(evs[0].Forge.Changes, "|") != want || evs[0].Forge.Checks != "success" {
		t.Fatalf("the burst's messages: %+v", evs)
	}
	if runs := settle(t, e.s, e.now); len(runs) != 0 {
		t.Fatalf("a second run: %+v", runs)
	}
}

// Events that never go quiet are still delivered once the oldest has waited
// EventMaxWait.
func TestEventMaxWait(t *testing.T) {
	ctx := context.Background()
	e := newClaimStopEnv(t)
	a, _ := e.runningClaim(t, "task-1")
	o := observed(a.Branch)
	if _, err := e.s.ObservePullWithToken(ctx, e.indepTk, o); err != nil {
		t.Fatal(err)
	}
	start := *e.now
	for i := int64(1); e.now.Sub(start) < EventMaxWait; i++ {
		o.Comments = i
		if _, err := e.s.ObservePullWithToken(ctx, e.indepTk, o); err != nil {
			t.Fatal(err)
		}
		*e.now = e.now.Add(20 * time.Second)
	}
	if runs := deliver(t, e.s); len(runs) != 1 || runs[0].Events < 6 {
		t.Fatalf("after %v of events: %+v", e.now.Sub(start), runs)
	}
}

// The journal survives a crash between a source's write and the delivery:
// the restarted store delivers the pending events once. A board page read
// again is the same events, and queues nothing twice.
func TestEventsReplayAfterRestart(t *testing.T) {
	ctx := context.Background()
	s, path := openTemp(t)
	now := clock(s)
	e := newExecTeam(t, s)
	rc := ReconcilerCaller()
	if _, err := s.RecordBoardPage(ctx, rc, "hub-a", BoardPage{Changes: []BoardChange{}, Cursor: "c1"}); err != nil {
		t.Fatal(err)
	}
	*now = now.Add(time.Minute)
	page := BoardPage{From: "c1", Changes: []BoardChange{readyChange("task-9", 4, *now)}, Cursor: "c2"}
	if n, err := s.RecordBoardPage(ctx, rc, "hub-a", page); err != nil || n != 1 {
		t.Fatalf("a READY task: %d queued, %v", n, err)
	}
	// The feed is reset and read again from its start: the change is the
	// same event, already journaled.
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	n, err := s.journalBoardChange(ctx, tx, "hub-a", page.Changes[0], *now)
	if err != nil || n != 0 {
		t.Fatalf("the same change again: %d queued, %v", n, err)
	}
	tx.Rollback()
	// aicrewd stops before its loop delivers.
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s2, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	later := now.Add(time.Hour)
	s2.now = func() time.Time { return later }
	if runs := deliver(t, s2); len(runs) != 1 || runs[0].Events != 1 || runs[0].MessageID == "" {
		t.Fatalf("after the restart: %+v", runs)
	}
	if runs := deliver(t, s2); len(runs) != 0 {
		t.Fatalf("a second delivery: %+v", runs)
	}
	if got := boardMessages(t, s2, e.tm.ID, e.lead); len(got) != 1 || got[0].Task.TaskID != "task-9" {
		t.Fatalf("the coordinator's announcements: %+v", got)
	}
}

// A handler that fails three times in a row parks the queue's events and
// tells the coordinators once; a release whose subscriptions differ replays
// them.
func TestEventHandlerParks(t *testing.T) {
	ctx := context.Background()
	s, _ := openTemp(t)
	now := clock(s)
	e := newExecTeam(t, s)
	rc := ReconcilerCaller()
	if _, err := s.RecordBoardPage(ctx, rc, "hub-a", BoardPage{Changes: []BoardChange{}, Cursor: "c1"}); err != nil {
		t.Fatal(err)
	}
	*now = now.Add(time.Minute)
	if _, err := s.RecordBoardPage(ctx, rc, "hub-a", BoardPage{From: "c1",
		Changes: []BoardChange{readyChange("task-9", 4, *now)}, Cursor: "c2"}); err != nil {
		t.Fatal(err)
	}
	wake := s.handlers["wake"]
	s.handlers["wake"] = func(context.Context, *sql.Tx, queue, []queuedEvent, time.Time) (string, error) {
		return "", errors.New("the handler broke\non two lines")
	}
	for i, want := range []string{"failed", "failed", "parked"} {
		runs := settle(t, s, now)
		if len(runs) != 1 || runs[0].Outcome != want || runs[0].Error != "the handler broke\non two lines" {
			t.Fatalf("run %d: %+v", i, runs)
		}
	}
	if runs := settle(t, s, now); len(runs) != 0 {
		t.Fatalf("a run after the park: %+v", runs)
	}
	items, err := pendingMessages(ctx, s.db, e.tm.ID, e.lead.agent.ID, maxInboxPage)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 || !strings.Contains(items[0].Text, "Events are parked") ||
		!strings.Contains(items[0].Text, "(last error: the handler broke on two lines)") {
		t.Fatalf("the coordinator's messages: %+v", items)
	}
	// The release with a fixed handler and changed subscriptions.
	s.handlers["wake"] = wake
	s.subs.Version = "a-new-version"
	if runs := deliver(t, s); len(runs) != 1 || runs[0].Outcome != "handled" {
		t.Fatalf("the replay: %+v", runs)
	}
	if got := boardMessages(t, s, e.tm.ID, e.lead); len(got) != 1 || got[0].Task.TaskID != "task-9" {
		t.Fatalf("after the replay: %+v", got)
	}
}

// Only aicrewd delivers events, and an event's payload is bounded.
func TestEventRefusals(t *testing.T) {
	ctx := context.Background()
	s, _ := openTemp(t)
	if _, err := s.DeliverEvents(ctx, operator(t)); !errors.Is(err, ErrForbidden) {
		t.Errorf("an operator delivering: %v", err)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	big := strings.Repeat("x", maxEventPayload)
	if _, _, err := journal(ctx, tx, sourceForge, "k", "pull:x", "forge.changed", big, s.now(), s.now(), nil); !errors.Is(err, ErrInvalid) {
		t.Errorf("a payload over the bound: %v", err)
	}
}

// Changes of one task in one page are one message, in the feed's order: it
// carries the last delivered change, and its text names each transition.
func TestEventsKeepThePagesOrder(t *testing.T) {
	ctx := context.Background()
	s, _ := openTemp(t)
	now := clock(s)
	e := newExecTeam(t, s)
	rc := ReconcilerCaller()
	if _, err := s.RecordBoardPage(ctx, rc, "hub-a", BoardPage{Changes: []BoardChange{}, Cursor: "c1"}); err != nil {
		t.Fatal(err)
	}
	*now = now.Add(time.Minute)
	var changes []BoardChange
	for i, to := range []string{"READY", "IN_PROGRESS", "BLOCKED", "READY", "CANCELLED"} {
		from := "BACKLOG"
		if i > 0 {
			from = changes[i-1].To
		}
		changes = append(changes, BoardChange{ProjectID: "project-a", TaskID: "task-5", Revision: int64(10 + i), From: from, To: to,
			At: now.Add(time.Duration(i) * time.Second)})
	}
	if n, err := s.RecordBoardPage(ctx, rc, "hub-a", BoardPage{From: "c1", Changes: changes, Cursor: "c2"}); err != nil || n != 2 {
		t.Fatalf("%d queued, %v", n, err)
	}
	settle(t, s, now)
	got := boardMessages(t, s, e.tm.ID, e.lead)
	// The team has no attempt for the task: only its arrivals at READY
	// are delivered.
	want := "Task task-5 of project project-a moved from BACKLOG to READY on the board (revision 10). " +
		"Task task-5 of project project-a moved from BLOCKED to READY on the board (revision 13)."
	if len(got) != 1 || got[0].Board.Revision != 13 || got[0].Board.From != "BLOCKED" || got[0].Text != want {
		t.Fatalf("the announcements: %+v", got)
	}
}

// A run's message keeps every change of its events in order, repeats
// included: two comments added one by one are two changes, and a
// conclusion that flips back ends the list as it ended on the forge.
func TestEventBurstKeepsRepeatedChanges(t *testing.T) {
	ctx := context.Background()
	e := newClaimStopEnv(t)
	a, _ := e.runningClaim(t, "task-1")
	o := observed(a.Branch)
	if _, err := e.s.ObservePullWithToken(ctx, e.indepTk, o); err != nil {
		t.Fatal(err)
	}
	for _, step := range []func(){
		func() { o.Comments = 1 },
		func() { o.Comments = 2 },
		func() { o.Checks = "success" },
		func() { o.Checks = "failure" },
		func() { o.Checks = "success" },
	} {
		step()
		if res, err := e.s.ObservePullWithToken(ctx, e.indepTk, o); err != nil || res.EventID == "" {
			t.Fatalf("%+v, %v", res, err)
		}
		*e.now = e.now.Add(time.Second)
	}
	if runs := settle(t, e.s, e.now); len(runs) != 1 || runs[0].Events != 5 {
		t.Fatalf("runs: %+v", runs)
	}
	evs := forgeEvents(t, e.s, e.indepTk)
	want := "1 new comment(s)|1 new comment(s)|CI concluded success at 0123456789ab|" +
		"CI concluded failure at 0123456789ab|CI concluded success at 0123456789ab"
	if len(evs) != 1 || strings.Join(evs[0].Forge.Changes, "|") != want || evs[0].Forge.Checks != "success" {
		t.Fatalf("the message: %+v", evs)
	}
}

// A queue selected as due is checked again in its run: an event that
// arrived in between ends the quiet, and the run waits for the next one.
func TestEventQueueRecheckedBeforeRun(t *testing.T) {
	ctx := context.Background()
	e := newClaimStopEnv(t)
	a, _ := e.runningClaim(t, "task-1")
	o := observed(a.Branch)
	if _, err := e.s.ObservePullWithToken(ctx, e.indepTk, o); err != nil {
		t.Fatal(err)
	}
	o.Comments = 1
	if _, err := e.s.ObservePullWithToken(ctx, e.indepTk, o); err != nil {
		t.Fatal(err)
	}
	*e.now = e.now.Add(EventQuiet)
	e.s.beforeQueueRun = func() {
		e.s.beforeQueueRun = nil
		o.Comments = 2
		if _, err := e.s.ObservePullWithToken(ctx, e.indepTk, o); err != nil {
			t.Error(err)
		}
	}
	if runs := deliver(t, e.s); len(runs) != 0 {
		t.Fatalf("a run although an event had just arrived: %+v", runs)
	}
	if got := forgeEvents(t, e.s, e.indepTk); len(got) != 0 {
		t.Fatalf("messages: %+v", got)
	}
	if runs := settle(t, e.s, e.now); len(runs) != 1 || runs[0].Events != 2 {
		t.Fatalf("the next run: %+v", runs)
	}
}
