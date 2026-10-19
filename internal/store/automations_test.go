package store

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
)

// actionItems is the action.needed messages of kind waiting in the
// token's inbox.
func actionItems(t *testing.T, s *Store, token, kind string) []InboxItem {
	t.Helper()
	items, err := s.ReadInboxWithToken(context.Background(), token, 100)
	if err != nil {
		t.Fatal(err)
	}
	var out []InboxItem
	for _, it := range items {
		if it.Action != nil && len(it.Action.Subjects) > 0 && it.Action.Subjects[0].Kind == kind {
			out = append(out, it)
		}
	}
	return out
}

func runReport(attemptID, runID, outcome string) AutomationRunReport {
	return AutomationRunReport{AttemptID: attemptID, RunID: runID, Point: "after-push", Name: "repo-check",
		Source: "command", Command: []string{"bash", "scripts/check-repo.sh"}, Pull: 7,
		URL: "https://git.example.test/crew/project-a/pulls/7", Head: head1, Outcome: outcome}
}

// The worker's launcher reports each automation run at an extension point
// of its attempt's pull request. A run that handled its point is recorded
// and wakes no one; a failed run wakes the team's coordinators once with
// the handler, the subject and the error; a run that asked for a turn
// wakes the worker with the turn's content. A report sent twice records
// one run.
func TestAutomationRunReports(t *testing.T) {
	ctx := context.Background()
	e := newClaimStopEnv(t)
	a, _ := e.runningClaim(t, "task-1")

	handled := runReport(a.ID, "run-1", RunHandled)
	res, err := e.s.RecordAutomationRunWithToken(ctx, e.indepTk, handled)
	if err != nil || !res.Recorded || res.AttemptID != a.ID || res.EventID == "" {
		t.Fatalf("a handled run: %+v, %v", res, err)
	}
	if runs := settle(t, e.s, e.now); len(runs) != 0 {
		t.Fatalf("a handled run woke someone: %+v", runs)
	}
	if got := actionItems(t, e.s, e.leadTok, "automation"); len(got) != 0 {
		t.Fatalf("a handled run reached the coordinator: %+v", got)
	}

	exit := 2
	failed := runReport(a.ID, "run-2", RunFailed)
	failed.Error, failed.Exit = "exit status 2", &exit
	failed.Steps = []AutomationStep{{Kind: "command", Text: "gofmt: internal/x.go is not formatted\nFAIL"}}
	if res, err = e.s.RecordAutomationRunWithToken(ctx, e.indepTk, failed); err != nil || !res.Recorded || res.EventID == "" {
		t.Fatalf("a failed run: %+v, %v", res, err)
	}
	// The same report again: one run, no second event.
	if res, err = e.s.RecordAutomationRunWithToken(ctx, e.indepTk, failed); err != nil || res.Recorded || res.EventID != "" {
		t.Fatalf("the failed run reported twice: %+v, %v", res, err)
	}
	runs := settle(t, e.s, e.now)
	if len(runs) != 1 || runs[0].Subscription != "automation-wake" || runs[0].Subject != "pull:"+a.ID || runs[0].Events != 1 {
		t.Fatalf("the failed run's delivery: %+v", runs)
	}
	got := actionItems(t, e.s, e.leadTok, "automation")
	if len(got) != 1 || got[0].Kind != KindActionNeeded || got[0].AttemptID != a.ID || len(got[0].Action.Subjects) != 1 {
		t.Fatalf("the coordinator's message: %+v", got)
	}
	sub := got[0].Action.Subjects[0]
	if len(sub.Runs) != 1 || sub.Runs[0].RunID != "run-2" || sub.Runs[0].Outcome != RunFailed || sub.Runs[0].Exit == nil {
		t.Fatalf("the subject's runs: %+v", sub.Runs)
	}
	for _, want := range []string{
		"action.needed: the automation repo-check (the command bash scripts/check-repo.sh) failed at after-push for pull request #7 " +
			"(https://git.example.test/crew/project-a/pulls/7) at 0123456789ab: exit status 2 (exit 2)",
		"A failed run is never retried",
		"\n  - step command:\n  | gofmt: internal/x.go is not formatted\n  | FAIL",
	} {
		if !strings.Contains(got[0].Text, want) {
			t.Errorf("the text lacks %q:\n%s", want, got[0].Text)
		}
	}
	if w := actionItems(t, e.s, e.indepTk, "automation"); len(w) != 0 {
		t.Fatalf("the worker was woken for a failed run: %+v", w)
	}
	ackTok(t, e.s, e.leadTok, got)

	turn := runReport(a.ID, "run-3", RunTurn)
	turn.Source, turn.Command, turn.Script, turn.Commit = "process", nil, "process/triage.star", "3f2a9c1e5b7d4a6c8e0f1a2b3c4d5e6f7a8b9c0d"
	turn.Turn, turn.Content = "rework", "The check found two findings; decide which to fix."
	if res, err = e.s.RecordAutomationRunWithToken(ctx, e.indepTk, turn); err != nil || !res.Recorded {
		t.Fatalf("a turn: %+v, %v", res, err)
	}
	if runs := settle(t, e.s, e.now); len(runs) != 1 {
		t.Fatalf("the turn's delivery: %+v", runs)
	}
	w := actionItems(t, e.s, e.indepTk, "automation")
	if len(w) != 1 || !strings.Contains(w[0].Text, "action.needed: the automation repo-check (process process/triage.star at 3f2a9c1e5b7d) "+
		"at after-push for pull request #7 (https://git.example.test/crew/project-a/pulls/7) at 0123456789ab asks for a rework turn:"+
		"\n  | The check found two findings; decide which to fix.") || w[0].Action.Subjects[0].Runs[0].Turn != "rework" {
		t.Fatalf("the worker's message: %+v", w)
	}
	if c := actionItems(t, e.s, e.leadTok, "automation"); len(c) != 0 {
		t.Fatalf("the coordinator was woken for a turn: %+v", c)
	}

	// The record on the attempt.
	var n int
	if err := e.s.db.QueryRow(`SELECT count(*) FROM attempt_automation_runs WHERE attempt_id = ? AND outcome = 'failed'`, a.ID).Scan(&n); err != nil || n != 1 {
		t.Fatalf("the failed runs recorded: %d, %v", n, err)
	}
}

// A run for the launcher's own event (forge.changed) names the event in
// place of a point: it is recorded with an empty point, and a failed one
// wakes the coordinators with the event in the text and the run's events in
// the subject.
func TestAutomationEventRun(t *testing.T) {
	ctx := context.Background()
	e := newClaimStopEnv(t)
	a, _ := e.runningClaim(t, "task-1")
	r := runReport(a.ID, "run-ev", RunFailed)
	r.Point, r.Events, r.Source, r.Command, r.Script, r.Error = "", []string{"forge.changed"}, "release", nil, "review-request.star", "the script failed: boom"
	if res, err := e.s.RecordAutomationRunWithToken(ctx, e.indepTk, r); err != nil || !res.Recorded || res.EventID == "" {
		t.Fatalf("an event run: %+v, %v", res, err)
	}
	settle(t, e.s, e.now)
	got := actionItems(t, e.s, e.leadTok, "automation")
	if len(got) != 1 || !strings.Contains(got[0].Text, "the automation repo-check (release review-request.star) failed on forge.changed for pull request #7") ||
		len(got[0].Action.Subjects[0].Runs) != 1 || len(got[0].Action.Subjects[0].Runs[0].Events) != 1 ||
		got[0].Action.Subjects[0].Runs[0].Events[0] != "forge.changed" || got[0].Action.Subjects[0].Runs[0].Point != "" {
		t.Fatalf("the coordinator's message: %+v", got)
	}
	var point string
	if err := e.s.db.QueryRow(`SELECT point FROM attempt_automation_runs WHERE attempt_id = ? AND run_id = 'run-ev'`, a.ID).Scan(&point); err != nil || point != "" {
		t.Fatalf("the row's point: %q, %v", point, err)
	}
}

// One queue holding a failed run and a turn wakes each role with its own
// message: the coordinators read the failed run only, the worker the turn
// only, and the queue is delivered once (the 2d-a follow-up 2a2b).
func TestAutomationWakeSplitPerRole(t *testing.T) {
	ctx := context.Background()
	e := newClaimStopEnv(t)
	a, _ := e.runningClaim(t, "task-1")

	exit := 2
	failed := runReport(a.ID, "run-2", RunFailed)
	failed.Error, failed.Exit = "exit status 2", &exit
	turn := runReport(a.ID, "run-3", RunTurn)
	turn.Source, turn.Command, turn.Script = "process", nil, "process/triage.star"
	turn.Turn, turn.Content = "rework", "Decide which finding to fix."
	turn.Conflict = &ConflictDetail{Base: "main", Files: []string{"work.txt"}, Hunks: "<<<<<<< HEAD\nwork\n=======\nbase\n>>>>>>> main\n"}
	for _, r := range []AutomationRunReport{failed, turn} {
		if res, err := e.s.RecordAutomationRunWithToken(ctx, e.indepTk, r); err != nil || !res.Recorded {
			t.Fatalf("%s: %+v, %v", r.RunID, res, err)
		}
	}
	runs := settle(t, e.s, e.now)
	if len(runs) != 1 || runs[0].Subscription != "automation-wake" || runs[0].Events != 2 || runs[0].MessageID == "" {
		t.Fatalf("the queue's delivery: %+v", runs)
	}
	coord := actionItems(t, e.s, e.leadTok, "automation")
	if len(coord) != 1 || len(coord[0].Action.Subjects) != 1 || len(coord[0].Action.Subjects[0].Runs) != 1 ||
		coord[0].Action.Subjects[0].Runs[0].RunID != "run-2" || coord[0].ID != runs[0].MessageID {
		t.Fatalf("the coordinator's message: %+v", coord)
	}
	if !strings.Contains(coord[0].Text, "failed at after-push") || strings.Contains(coord[0].Text, "asks for a") {
		t.Errorf("the coordinator's text:\n%s", coord[0].Text)
	}
	worker := actionItems(t, e.s, e.indepTk, "automation")
	if len(worker) != 1 || len(worker[0].Action.Subjects) != 1 || len(worker[0].Action.Subjects[0].Runs) != 1 ||
		worker[0].Action.Subjects[0].Runs[0].RunID != "run-3" || worker[0].ID == coord[0].ID {
		t.Fatalf("the worker's message: %+v", worker)
	}
	if !strings.Contains(worker[0].Text, "asks for a rework turn:\n  | Decide which finding to fix.\n  The update from main conflicts in work.txt;") ||
		strings.Contains(worker[0].Text, "failed at") {
		t.Errorf("the worker's text:\n%s", worker[0].Text)
	}
	// The turn's conflict is the subject's, hunks and all; the
	// coordinators' subject has none.
	if c := worker[0].Action.Subjects[0].Conflict; c == nil || c.Base != "main" || !slices.Equal(c.Files, []string{"work.txt"}) ||
		!strings.Contains(c.Hunks, ">>>>>>> main") || coord[0].Action.Subjects[0].Conflict != nil {
		t.Errorf("the subjects' conflicts: worker %+v, coordinator %+v", c, coord[0].Action.Subjects[0].Conflict)
	}
	// Nothing is delivered twice.
	if again := settle(t, e.s, e.now); len(again) != 0 {
		t.Fatalf("a second delivery: %+v", again)
	}
}

// A report of another member's attempt, of a closed attempt, or with a
// shape the store does not take is refused and records nothing; a report
// whose texts would not fit an event is cut, not refused.
func TestAutomationRunRefusalsAndBounds(t *testing.T) {
	ctx := context.Background()
	e := newClaimStopEnv(t)
	a, _ := e.runningClaim(t, "task-1")
	if _, err := e.s.RecordAutomationRunWithToken(ctx, e.workerTok, runReport(a.ID, "run-x", RunHandled)); !errors.Is(err, ErrNoAttempt) {
		t.Fatalf("another member's report: %v", err)
	}
	if _, err := e.s.RecordAutomationRunWithToken(ctx, e.indepTk, runReport("no-such-attempt", "run-x", RunHandled)); !errors.Is(err, ErrNoAttempt) {
		t.Fatalf("an unknown attempt: %v", err)
	}
	for name, change := range map[string]func(r *AutomationRunReport){
		"an unknown outcome":          func(r *AutomationRunReport) { r.Outcome = "done" },
		"a failure without an error":  func(r *AutomationRunReport) { r.Outcome = RunFailed },
		"a turn without a kind":       func(r *AutomationRunReport) { r.Outcome = RunTurn },
		"content on a handled run":    func(r *AutomationRunReport) { r.Content = "x" },
		"a conflict on a handled run": func(r *AutomationRunReport) { r.Conflict = &ConflictDetail{Base: "main", Files: []string{"a"}} },
		"a conflict without files": func(r *AutomationRunReport) {
			r.Outcome, r.Turn, r.Conflict = RunTurn, "fix", &ConflictDetail{Base: "main"}
		},
		"a conflict with a two-line base": func(r *AutomationRunReport) {
			r.Outcome, r.Turn, r.Conflict = RunTurn, "fix", &ConflictDetail{Base: "ma\nin", Files: []string{"a"}}
		},
		"a conflict with a two-line file": func(r *AutomationRunReport) {
			r.Outcome, r.Turn, r.Conflict = RunTurn, "fix", &ConflictDetail{Base: "main", Files: []string{"a\nb"}}
		},
		"a conflict with too many files": func(r *AutomationRunReport) {
			r.Outcome, r.Turn, r.Conflict = RunTurn, "fix", &ConflictDetail{Base: "main", Files: make([]string, maxConflictFiles+1)}
		},
		"a bad run id":                 func(r *AutomationRunReport) { r.RunID = "run 1" },
		"a bad point":                  func(r *AutomationRunReport) { r.Point = "After Push" },
		"neither a point nor events":   func(r *AutomationRunReport) { r.Point = "" },
		"an unknown event kind":        func(r *AutomationRunReport) { r.Point, r.Events = "", []string{"forge.moved"} },
		"a long command":               func(r *AutomationRunReport) { r.Command = make([]string, maxRunCommand+1) },
		"an http url":                  func(r *AutomationRunReport) { r.URL = "http://git.example.test/x" },
		"a head that is not a commit":  func(r *AutomationRunReport) { r.Head = "main" },
		"too many steps":               func(r *AutomationRunReport) { r.Steps = make([]AutomationStep, maxRunSteps+1) },
		"a step kind with a space":     func(r *AutomationRunReport) { r.Steps = []AutomationStep{{Kind: "a b", Text: "x"}} },
		"an error longer than allowed": func(r *AutomationRunReport) { r.Outcome, r.Error = RunFailed, strings.Repeat("e", maxRunError+1) },
	} {
		r := runReport(a.ID, "run-bad", RunHandled)
		change(&r)
		if _, err := e.s.RecordAutomationRunWithToken(ctx, e.indepTk, r); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: %v", name, err)
		}
	}
	var n int
	if err := e.s.db.QueryRow(`SELECT count(*) FROM attempt_automation_runs`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("a refused report was recorded: %d, %v", n, err)
	}

	// Twenty steps of 4 KiB each: the event is cut to its room, the
	// coordinator's message says so, and the record keeps the whole.
	big := runReport(a.ID, "run-big", RunFailed)
	big.Error = strings.Repeat("e", maxRunError)
	for i := 0; i < maxRunSteps; i++ {
		big.Steps = append(big.Steps, AutomationStep{Kind: "command", Text: strings.Repeat("s", maxRunStepText)})
	}
	if res, err := e.s.RecordAutomationRunWithToken(ctx, e.indepTk, big); err != nil || !res.Recorded || res.EventID == "" {
		t.Fatalf("a large report: %+v, %v", res, err)
	}
	settle(t, e.s, e.now)
	got := actionItems(t, e.s, e.leadTok, "automation")
	if len(got) != 1 || !strings.Contains(got[0].Text, "A text was cut for room") || len(got[0].Text) > maxMessageText {
		t.Fatalf("the cut message: %d items, %d bytes", len(got), len(got[0].Text))
	}
	var stored string
	if err := e.s.db.QueryRow(`SELECT report FROM attempt_automation_runs WHERE run_id = 'run-big'`).Scan(&stored); err != nil ||
		strings.Count(stored, strings.Repeat("s", maxRunStepText)) != maxRunSteps {
		t.Fatalf("the record was cut: %d bytes, %v", len(stored), err)
	}

	// A conflict naming fifty files of 400-byte paths, which the report
	// accepts, is more than an event holds: the event's file names are
	// cut and the list halved until it fits, the worker's subject still
	// carries the conflict with its base and some of its files, the
	// message says a text was cut, and the record keeps all fifty.
	wide := runReport(a.ID, "run-wide", RunTurn)
	wide.Turn, wide.Content = "fix", "Updating work/task-1 from main conflicts."
	wide.Conflict = &ConflictDetail{Base: "main", Hunks: strings.Repeat("<", 1024)}
	for i := 0; i < maxConflictFiles; i++ {
		wide.Conflict.Files = append(wide.Conflict.Files, strings.Repeat("d", 380)+"/"+strings.Repeat("f", 15)+".go"+string(rune('a'+i%26)))
	}
	if res, err := e.s.RecordAutomationRunWithToken(ctx, e.indepTk, wide); err != nil || !res.Recorded || res.EventID == "" {
		t.Fatalf("a wide conflict: %+v, %v", res, err)
	}
	settle(t, e.s, e.now)
	worker := actionItems(t, e.s, e.indepTk, "automation")
	if len(worker) != 1 || len(worker[0].Action.Subjects) != 1 || !strings.Contains(worker[0].Text, "A text was cut for room") {
		t.Fatalf("the worker's cut message: %d items: %+v", len(worker), worker)
	}
	c := worker[0].Action.Subjects[0].Conflict
	if c == nil || c.Base != "main" || len(c.Files) == 0 || len(c.Files) > maxConflictFiles || len(c.Files[0]) > 128 ||
		!strings.HasPrefix(c.Files[0], "dddd") || !strings.Contains(c.Hunks, "<<<") {
		t.Fatalf("the worker's conflict: %d files, %+v", len(c.Files), c)
	}
	if err := e.s.db.QueryRow(`SELECT report FROM attempt_automation_runs WHERE run_id = 'run-wide'`).Scan(&stored); err != nil ||
		strings.Count(stored, strings.Repeat("d", 380)) != maxConflictFiles {
		t.Fatalf("the wide record was cut: %d bytes, %v", len(stored), err)
	}
}
