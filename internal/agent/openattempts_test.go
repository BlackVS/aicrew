package agent

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/BlackVS/aicrew/internal/store"
)

// seedOffer writes an outstanding offer to the member, made under the
// coordinator generation gen. The member is its own worker here only so the
// list has a label to show: the store's offer rules are not under test.
func (c *crewEnv) seedOffer(t *testing.T, id string, gen int64) {
	t.Helper()
	db, err := sql.Open("sqlite", c.storePath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	now := time.Now().UTC()
	if _, err := db.Exec(`INSERT INTO attempts (id, team_id, task_hub_id, task_project_id, task_id, worker_agent_id, origin,
		coordinator_agent_id, coordinator_session_id, coordinator_generation, state, base_commit, branch, process_repository, process_commit,
		process_manifest, instruction_digest, offer_expires_at, task_revision, revision, created_at, updated_at, phase)
		VALUES (?, ?, 'hub-test', 'project-a', 'task-3dc2', ?, 'offer', ?, 'old-session', ?, 'offered', 'base', 'work/task-3dc2',
		'github.com/example/process', 'abc1234', 'process.yaml', 'sha256:x', ?, 1, 1, ?, ?, '')`,
		id, c.teamID, c.agentID, c.agentID, gen, now.Add(time.Hour).Format("2006-01-02T15:04:05.000000000Z07:00"),
		now.Format("2006-01-02T15:04:05.000000000Z07:00"), now.Format("2006-01-02T15:04:05.000000000Z07:00")); err != nil {
		t.Fatal(err)
	}
}

// At a coordinator's session start the launcher records the team's open
// attempts from aicrewd in the home file the first instruction names: an
// offer made under an earlier coordinator session is listed as stale, with
// the withdraw-and-offer-again action (pilot finding 2026-10-11).
func TestCoordinatorStartRecordsOpenAttempts(t *testing.T) {
	c := setupCrewWith(t, crewOptions{role: store.RoleCoordinator})
	ctx := context.Background()
	e := c.engine(t)
	if err := e.Start(ctx); err != nil {
		t.Fatal(err)
	}
	c.seedOffer(t, "attempt-stale", 0)

	o, ok, err := e.RecordOpenAttempts(ctx)
	if err != nil || !ok {
		t.Fatalf("record the open attempts: ok %v, %v\n%s", ok, err, c.logs)
	}
	if len(o.Attempts) != 1 || !o.Attempts[0].Stale || o.Attempts[0].ID != "attempt-stale" {
		t.Fatalf("open attempts = %+v, want the stale offer", o.Attempts)
	}
	b, err := os.ReadFile(filepath.Join(c.cfg.Home, filepath.FromSlash(OpenAttemptsFile)))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"# Open attempts at this session's start", "`attempt-stale`", "task `hub-test/project-a/task-3dc2`",
		"stale: made under an earlier coordinator session", "Next: Withdraw the offer, then offer the task again",
		"\"Reconcile first\"", "`aicrew-agent attempts`"} {
		if !strings.Contains(string(b), want) {
			t.Errorf("%s lacks %q:\n%s", OpenAttemptsFile, want, b)
		}
	}
	if !strings.Contains(FirstInstruction(), OpenAttemptsFile) {
		t.Errorf("the first instruction does not name %s", OpenAttemptsFile)
	}
}

// A worker's start records nothing, and the file a failed read leaves says
// so instead of showing an older list.
func TestOpenAttemptsOnlyForTheCoordinator(t *testing.T) {
	c := setupCrew(t)
	ctx := context.Background()
	e := c.engine(t)
	if err := e.Start(ctx); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := e.RecordOpenAttempts(ctx); ok || err != nil {
		t.Fatalf("a worker's start: ok %v, %v; want nothing recorded", ok, err)
	}
	if _, err := os.Stat(filepath.Join(c.cfg.Home, filepath.FromSlash(OpenAttemptsFile))); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a worker's home has %s: %v", OpenAttemptsFile, err)
	}
	failed := RenderOpenAttempts(OpenAttempts{}, time.Now(), errors.New("aicrewd is unreachable"))
	if !strings.Contains(failed, "could not be read: aicrewd is unreachable. Run `aicrew-agent attempts`.") ||
		strings.Contains(failed, "no open attempts") {
		t.Errorf("a failed read renders:\n%s", failed)
	}
}

// The launcher's start of the step channel, which run makes before it
// starts the client, records a coordinator's open attempts.
func TestServeStepsRecordsACoordinatorsOpenAttempts(t *testing.T) {
	s := setupStepsWith(t, crewOptions{role: store.RoleCoordinator, steps: true, shortHome: true})
	defer swapAccess(fakeAccess{})()
	e := s.d.Session.(*Engine)
	srv := serveSteps(e)
	if srv == nil {
		t.Fatal("the step channel is not served")
	}
	defer srv.Close()
	b, err := os.ReadFile(filepath.Join(s.cfg.Home, filepath.FromSlash(OpenAttemptsFile)))
	if err != nil || !strings.Contains(string(b), "The team has no open attempts.") {
		t.Fatalf("%s after the step channel started: %q %v", OpenAttemptsFile, b, err)
	}
}

// A page of open escalations fits the reader's bound in the worst case the
// store accepts: a full page of maximal questions that all need escaping,
// with maximal contexts the list leaves out. Every escalation is reached by
// following next through the real HTTP reader.
func TestOpenEscalationPagesFitTheReader(t *testing.T) {
	ctx := context.Background()
	s := setupStepsWith(t, crewOptions{role: store.RoleCoordinator, steps: true, shortHome: true})
	e := s.d.Session.(*Engine)
	token := e.stepToken()
	n := store.OpenEscalationPageSize + 3
	for i := range n {
		in := store.EscalationRequest{Task: store.TaskRef{HubID: "hub-test", ProjectID: "project-t", TaskID: "task-1"},
			Category: "scope", Urgency: "today", Question: strings.Repeat("<", 1<<10-4) + fmt.Sprintf("%04d", i),
			Context: strings.Repeat("&", 8<<10), Recommendation: "add it",
			Options: []store.EscalationOption{{Option: "add it", Consequence: "c"}, {Option: "drop it", Consequence: "c"}}}
		if _, err := s.store.RaiseEscalationWithToken(ctx, fmt.Sprintf("raise-%d", i), token, in); err != nil {
			t.Fatal(err)
		}
	}
	reader := e.Crew.(attemptsReader)
	seen, after := 0, ""
	for pages := 0; ; pages++ {
		path := escalationsPath
		if after != "" {
			path += "?after=" + after
		}
		raw, err := reader.Read(ctx, token, path)
		if err != nil {
			t.Fatalf("page %d: %v", pages, err)
		}
		if len(raw) >= maxInboxReply {
			t.Fatalf("page %d is %d bytes, at or over the reader's %d", pages, len(raw), maxInboxReply)
		}
		var page struct {
			Escalations []json.RawMessage `json:"escalations"`
			Next        string            `json:"next"`
		}
		if err := json.Unmarshal(raw, &page); err != nil {
			t.Fatal(err)
		}
		seen += len(page.Escalations)
		if page.Next == "" {
			break
		}
		after = page.Next
	}
	if seen != n {
		t.Fatalf("followed %d escalations, want %d", seen, n)
	}
}

// A running attempt's line names when its handoff was last pushed, or that
// none was; an offer names neither.
func TestOpenAttemptLineHandoff(t *testing.T) {
	at := time.Date(2026, 10, 24, 5, 30, 0, 0, time.FixedZone("x", 3600))
	a := OpenAttempt{ID: "att-1", State: "running", Phase: "working", Task: TaskRef{HubID: "h", ProjectID: "p", TaskID: "t"},
		WorkerLabel: "worker", NextAction: "Follow it."}
	if got, want := a.Line(), "- Attempt `att-1`: task `h/p/t`, worker worker, running (working), no handoff pushed. Next: Follow it."; got != want {
		t.Errorf("without a handoff: %q, want %q", got, want)
	}
	a.HandoffUpdatedAt = at
	if got := a.Line(); !strings.Contains(got, ", running (working), handoff pushed 2026-10-24T04:30:00Z. Next:") {
		t.Errorf("with a handoff: %q", got)
	}
	o := OpenAttempt{ID: "att-2", State: "offered", WorkerLabel: "worker", NextAction: "Wait."}
	if got := o.Line(); strings.Contains(got, "handoff") {
		t.Errorf("an offer names a handoff: %q", got)
	}
}

// A submitted attempt's line names its latest result: the result_seq the
// coordinator's steps take, the reference and the pull request's head
// (4d9a).
func TestOpenAttemptLineLatestResult(t *testing.T) {
	a := OpenAttempt{ID: "att-1", State: "running", Phase: "submitted", Task: TaskRef{HubID: "h", ProjectID: "p", TaskID: "t"},
		WorkerLabel: "worker", NextAction: "Review it.",
		LatestResult: &LatestResult{ResultSeq: 2, Kind: "pull_request", Ref: "https://forge.example/o/r/pulls/7", Head: "abc123"}}
	if got := a.Line(); !strings.Contains(got, ", latest result_seq 2: https://forge.example/o/r/pulls/7 at head abc123. Next:") {
		t.Errorf("a pull request result: %q", got)
	}
	a.LatestResult = &LatestResult{ResultSeq: 1, Kind: "reference", Ref: "https://example.invalid/r"}
	if got := a.Line(); !strings.Contains(got, ", latest result_seq 1: https://example.invalid/r. Next:") {
		t.Errorf("another reference: %q", got)
	}
}
