package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/BlackVS/aicrew/internal/events"
)

// Automation runs (docs/DESIGN-TURNS.md, 9.1, 10.3 and 12.4; increment
// 2d-a). The worker's launcher runs the effective automations at the
// extension points of its attempt's pull request and reports each run to
// aicrewd as a fact on the attempt: the point, the entry, where it came from
// and at which commit, the head, how it ended and the steps it recorded.
// aicrewd keeps the runs (attempt_automation_runs) and journals one
// automation.run event per run. The default automation-wake subscription
// wakes the team's coordinators on a failed run, naming the handler, the
// subject and the error, and the attempt's worker on a run that asked for
// a model turn, with the turn's content. A run that handled its point
// wakes no one. aicrewd never retries a run: the launcher decides what
// runs, and only at the next edge of a point.

// How a run ended.
const (
	RunHandled = "handled" // main returned None: the point is handled
	RunTurn    = "turn"    // main returned turn(kind, content)
	RunFailed  = "failed"  // fail(), a limit, a refused or failing command, a host error
)

// The bounds of a run report.
const (
	maxRunSteps    = 20
	maxRunStepText = 4 << 10
	maxRunError    = 4 << 10
	maxRunContent  = 16 << 10
	maxRunCommand  = 32
	maxRunArg      = 256
	// runBudget is the room a run's event payload may take.
	runBudget = maxEventPayload - 512
)

var (
	runIDShape   = regexp.MustCompile(`^[A-Za-z0-9-]{1,64}$`)
	runNameShape = regexp.MustCompile(`^[a-z][a-z0-9-]{0,62}$`)
	runKindShape = regexp.MustCompile(`^[a-z0-9_]{1,64}$`)
)

// AutomationRunReport is one run of an automation at an extension point,
// as the worker's launcher reports it.
type AutomationRunReport struct {
	AttemptID string `json:"attempt_id"`
	// RunID is the launcher's ID of the run, unique per attempt: a report
	// sent twice records one run.
	RunID string `json:"run_id"`
	// Point is the extension point the run was for, and Name the entry's
	// name; Source and Commit say where the entry came from (process,
	// repository, release or command) and at which commit it was read.
	Point  string `json:"point"`
	Name   string `json:"name"`
	Source string `json:"source"`
	Commit string `json:"commit,omitempty"`
	// Script is a script entry's path; Command a declared command's
	// argument list.
	Script  string   `json:"script,omitempty"`
	Command []string `json:"command,omitempty"`
	// Pull, URL and Head name the pull request and the head the run was
	// at; BaseOnly says the head was a base-only update of the one before.
	Pull     int64  `json:"pull"`
	URL      string `json:"url"`
	Head     string `json:"head"`
	BaseOnly bool   `json:"base_only,omitempty"`
	// Outcome is RunHandled, RunTurn or RunFailed. Turn and Content are
	// the turn a script asked for; Error is why a run failed; Exit is a
	// command's exit status.
	Outcome string `json:"outcome"`
	Turn    string `json:"turn,omitempty"`
	Content string `json:"content,omitempty"`
	Error   string `json:"error,omitempty"`
	Exit    *int   `json:"exit,omitempty"`
	// Steps are the steps the run recorded, each bounded.
	Steps []AutomationStep `json:"steps,omitempty"`
}

// AutomationStep is a step a run recorded.
type AutomationStep struct {
	Kind string `json:"kind"`
	Text string `json:"text"`
}

// AutomationRunResult is what aicrewd did with a report: whether it
// recorded the run (false for one it had already), and the event it
// journaled, if any.
type AutomationRunResult struct {
	AttemptID string `json:"attempt_id"`
	Recorded  bool   `json:"recorded"`
	EventID   string `json:"event_id,omitempty"`
}

// sourceAutomation is the source of this file's events.
const sourceAutomation = "automation"

// schemaV36 keeps each automation run the worker's launcher reported for
// an attempt's pull request.
var schemaV36 = []string{
	`CREATE TABLE attempt_automation_runs (
		attempt_id TEXT NOT NULL REFERENCES attempts (id),
		run_id     TEXT NOT NULL,
		point      TEXT NOT NULL,
		name       TEXT NOT NULL,
		source     TEXT NOT NULL,
		commit_id  TEXT NOT NULL DEFAULT '',
		head       TEXT NOT NULL DEFAULT '',
		outcome    TEXT NOT NULL,
		report     TEXT NOT NULL,
		at         TEXT NOT NULL,
		PRIMARY KEY (attempt_id, run_id)
	)`,
}

func (r AutomationRunReport) validate() error {
	bad := func(what string) error { return fmt.Errorf("%w: the run report's %s", ErrInvalid, what) }
	switch {
	case !validRefs(r.AttemptID):
		return bad("attempt")
	case !runIDShape.MatchString(r.RunID):
		return bad("run_id")
	case !runNameShape.MatchString(r.Point) || !runNameShape.MatchString(r.Name) || !runNameShape.MatchString(r.Source):
		return bad("point, name or source")
	case len(r.Commit) > maxRefLen || len(r.Script) > maxRefLen || len(r.Command) > maxRunCommand:
		return bad("commit, script or command")
	case r.Pull < 1 || !strings.HasPrefix(r.URL, "https://") || len(r.URL) > maxPullURL || strings.ContainsAny(r.URL, " \t\r\n"):
		return bad("pull request")
	case r.Head != "" && !observedCommit.MatchString(r.Head):
		return bad("head")
	case r.Outcome != RunHandled && r.Outcome != RunTurn && r.Outcome != RunFailed:
		return bad("outcome")
	case r.Outcome == RunFailed && strings.TrimSpace(r.Error) == "":
		return bad("failure, which names no error")
	case r.Outcome == RunTurn && !runKindShape.MatchString(r.Turn):
		return bad("turn kind")
	case r.Outcome != RunTurn && (r.Turn != "" || r.Content != ""):
		return bad("turn, which only a turn outcome has")
	case len(r.Steps) > maxRunSteps:
		return bad("steps")
	}
	for _, a := range r.Command {
		if a == "" || len(a) > maxRunArg || !utf8.ValidString(a) {
			return bad("command")
		}
	}
	if err := validText(r.Error, maxRunError, "the run report's error", false); err != nil {
		return err
	}
	if err := validText(r.Content, maxRunContent, "the run report's turn content", false); err != nil {
		return err
	}
	for _, st := range r.Steps {
		if !runKindShape.MatchString(st.Kind) {
			return bad("step kind")
		}
		if err := validText(st.Text, maxRunStepText, "the run report's step text", false); err != nil {
			return err
		}
	}
	return nil
}

// RecordAutomationRunWithToken records the token's member's report of an
// automation run at an extension point of its running attempt's pull
// request, and journals it as one automation.run event. A run already
// recorded (the same attempt and run_id) records nothing and journals
// nothing, so a repeated report wakes no one twice.
func (s *Store) RecordAutomationRunWithToken(ctx context.Context, token string, r AutomationRunReport) (AutomationRunResult, error) {
	if err := r.validate(); err != nil {
		return AutomationRunResult{}, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return AutomationRunResult{}, fmt.Errorf("begin: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck // no-op after commit
	now := s.now()
	b, err := tokenBinding(ctx, tx, token, now)
	if err != nil {
		return AutomationRunResult{}, err
	}
	a, err := getAttempt(ctx, tx, r.AttemptID)
	switch {
	case errors.Is(err, ErrNotFound) || (err == nil && (a.TeamID != b.TeamID || a.WorkerAgentID != b.AgentID)):
		return AutomationRunResult{}, fmt.Errorf("%w: attempt %s is not yours", ErrNoAttempt, r.AttemptID)
	case err != nil:
		return AutomationRunResult{}, err
	case a.State != AttemptRunning:
		return AutomationRunResult{}, fmt.Errorf("%w: attempt %s is %s, not running", ErrAttemptState, r.AttemptID, a.State)
	}
	res := AutomationRunResult{AttemptID: a.ID}
	raw, err := json.Marshal(r)
	if err != nil {
		return AutomationRunResult{}, fmt.Errorf("encode the run: %w", err)
	}
	ins, err := tx.ExecContext(ctx,
		`INSERT INTO attempt_automation_runs (attempt_id, run_id, point, name, source, commit_id, head, outcome, report, at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?) ON CONFLICT (attempt_id, run_id) DO NOTHING`,
		a.ID, r.RunID, r.Point, r.Name, r.Source, r.Commit, r.Head, r.Outcome, string(raw), formatTime(now))
	if err != nil {
		return AutomationRunResult{}, fmt.Errorf("record the run: %w", err)
	}
	if n, _ := ins.RowsAffected(); n == 0 {
		return res, tx.Commit()
	}
	res.Recorded = true
	ev := automationEvent{AttemptID: a.ID, Run: r}
	fitAutomationEvent(&ev)
	ms := matches(events.KindAutomationRun, a.TeamID, events.Facts{Failed: r.Outcome == RunFailed, Turn: r.Outcome == RunTurn})
	if res.EventID, _, err = journal(ctx, tx, sourceAutomation, a.ID+"/"+r.RunID, pullSubject(a.ID), events.KindAutomationRun,
		ev, now, now, ms); err != nil {
		return AutomationRunResult{}, err
	}
	return res, tx.Commit()
}

// automationEvent is an automation.run event's payload: the run as
// reported, its texts cut to the payload's room.
type automationEvent struct {
	AttemptID string              `json:"attempt_id"`
	Run       AutomationRunReport `json:"run"`
	// Cut says a text was cut for room: the attempt's record holds it
	// whole.
	Cut bool `json:"cut,omitempty"`
}

// fitAutomationEvent cuts ev's texts until its payload fits runBudget: the
// steps' texts first, then the turn's content, the error, and at last the
// steps themselves.
func fitAutomationEvent(ev *automationEvent) {
	size := func() int { b, _ := json.Marshal(ev); return len(b) }
	if size() <= runBudget {
		return
	}
	ev.Cut = true
	for i := range ev.Run.Steps {
		ev.Run.Steps[i].Text, _ = fitText(ev.Run.Steps[i].Text, 512)
	}
	if size() > runBudget {
		ev.Run.Content, _ = fitText(ev.Run.Content, 4<<10)
	}
	if size() > runBudget {
		ev.Run.Error, _ = fitText(ev.Run.Error, 2<<10)
	}
	for len(ev.Run.Steps) > 0 && size() > runBudget {
		ev.Run.Steps = ev.Run.Steps[:len(ev.Run.Steps)/2]
	}
}

// wakeAutomation wakes the roles the runs' events name: the coordinators
// for the failed runs, the worker for the runs that asked for a turn, with
// each run's content. The subject lists every run of the queue that still
// calls for one.
func wakeAutomation(ctx context.Context, tx *sql.Tx, evs []queuedEvent, roles []string) (*wakeOut, error) {
	var last automationEvent
	var runs []AutomationRunReport
	var texts []string
	cut := false
	for i, ev := range evs {
		var e automationEvent
		if err := json.Unmarshal(ev.Payload, &e); err != nil {
			return nil, fmt.Errorf("read an automation event: %w", err)
		}
		if i > 0 && e.AttemptID != last.AttemptID {
			return nil, errors.New("the queue holds events of two attempts")
		}
		last = e
		if e.Run.Outcome == RunHandled {
			continue
		}
		runs = append(runs, e.Run)
		texts = append(texts, runText(e.Run))
		cut = cut || e.Cut
	}
	if len(runs) == 0 {
		return nil, nil
	}
	a, err := getAttempt(ctx, tx, last.AttemptID)
	if err != nil {
		return nil, err
	}
	var recipients []string
	if slices.Contains(roles, events.RoleWorker) {
		recipients = append(recipients, a.WorkerAgentID)
	}
	if slices.Contains(roles, events.RoleCoordinator) {
		coords, err := activeCoordinators(ctx, tx, a.TeamID)
		if err != nil {
			return nil, err
		}
		for _, c := range coords {
			if !slices.Contains(recipients, c) {
				recipients = append(recipients, c)
			}
		}
	}
	if len(recipients) == 0 {
		return nil, nil
	}
	text := strings.Join(texts, "\n")
	if cut {
		text += "\n- A text was cut for room; the attempt's run record holds it whole."
	}
	task := a.Task
	sub := ActionSubject{Kind: "automation", AttemptID: a.ID, Task: &task, Runs: runs, Text: boundedText(text)}
	return &wakeOut{recipients: recipients, subject: sub}, nil
}

// runText is one run in words: a failed run with its handler, subject and
// error, a turn with its content, each step indented under it.
func runText(r AutomationRunReport) string {
	var b strings.Builder
	what := r.Name
	switch r.Source {
	case "command":
		what += " (the command " + strings.Join(r.Command, " ") + ")"
	default:
		what += " (" + r.Source + " " + r.Script
		if r.Commit != "" {
			what += " at " + short(r.Commit)
		}
		what += ")"
	}
	where := fmt.Sprintf("at %s for pull request #%d (%s) at %s", r.Point, r.Pull, r.URL, short(r.Head))
	switch r.Outcome {
	case RunFailed:
		fmt.Fprintf(&b, "action.needed: the automation %s failed %s: %s", what, where, strings.TrimSpace(r.Error))
		if r.Exit != nil {
			fmt.Fprintf(&b, " (exit %d)", *r.Exit)
		}
		b.WriteString(" A failed run is never retried: fix the script or the command, or disable the entry in the process manifest.")
	case RunTurn:
		fmt.Fprintf(&b, "action.needed: the automation %s %s asks for a %s turn:", what, where, r.Turn)
		indentInto(&b, r.Content)
	}
	for _, st := range r.Steps {
		fmt.Fprintf(&b, "\n  - step %s:", st.Kind)
		indentInto(&b, st.Text)
	}
	return b.String()
}
