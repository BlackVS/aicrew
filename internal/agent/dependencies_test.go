package agent

import (
	"context"
	"database/sql"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/BlackVS/aicrew/internal/store"
)

// setupOfferSteps is a step channel whose agent is its team's coordinator,
// offering task-1 to the worker "lead".
func setupOfferSteps(t *testing.T) *stepEnv {
	t.Helper()
	s := setupStepsWith(t, crewOptions{role: store.RoleCoordinator, steps: true, shortHome: true})
	s.serve(t)
	return s
}

// setDependencies gives task-1 the dependencies deps, each an aimem task in
// its state at its revision; a dependency without a state is not in aimem.
func (s *stepEnv) setDependencies(t *testing.T, deps []string, states map[string]string, revisions map[string]int64) {
	t.Helper()
	f := loadFakeReservations(s.root)
	f.Tasks["task-1"].Content["dependencies"], _ = json.Marshal(deps)
	for _, id := range deps {
		state, ok := states[id]
		if !ok {
			continue
		}
		st, _ := json.Marshal(state)
		f.Tasks[id] = &fakeTaskState{Project: "project-t", Revision: revisions[id],
			Content: map[string]json.RawMessage{"title": json.RawMessage(`"dependency"`), "state": st}}
	}
	f.save(s.root)
}

func (s *stepEnv) offerBody(t *testing.T, extra map[string]any) json.RawMessage {
	t.Helper()
	body := map[string]any{"worker_agent_id": s.coordSess.AgentID, "task": s.task(), "expected_revision": 3,
		"repository": testRepository, "process": testProcess, "instruction_digest": testDigest,
		"expires_at": time.Now().Add(time.Hour).UTC().Format(time.RFC3339)}
	for k, v := range extra {
		body[k] = v
	}
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// offerAudits are the dependency evidence of each offer aicrewd recorded.
func (s *stepEnv) offerAudits(t *testing.T) [][]DependencyEvidence {
	t.Helper()
	db, err := sql.Open("sqlite", s.storePath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	rows, err := db.Query(`SELECT input FROM audit WHERE operation = 'attempt.offer' ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out [][]DependencyEvidence
	for rows.Next() {
		var input string
		if err := rows.Scan(&input); err != nil {
			t.Fatal(err)
		}
		var in struct {
			Evidence []DependencyEvidence `json:"dependency_evidence"`
		}
		if err := json.Unmarshal([]byte(input), &in); err != nil {
			t.Fatalf("audit input %s: %v", input, err)
		}
		out = append(out, in.Evidence)
	}
	return out
}

func (s *stepEnv) attemptsOfTask(t *testing.T) int {
	t.Helper()
	db, err := sql.Open("sqlite", s.storePath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var n int
	if err := db.QueryRow(`SELECT count(*) FROM attempts WHERE task_id = 'task-1'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// An offer whose task has a dependency that is not DONE, or that the
// member's aimem connection cannot read, is refused before anything is
// recorded or begun: unknown is not DONE.
func TestOfferWithOpenDependenciesIsRefused(t *testing.T) {
	s := setupOfferSteps(t)
	var answers []byte
	s.setDependencies(t, []string{"dep-a", "dep-b", "dep-c"},
		map[string]string{"dep-a": "DONE", "dep-b": "IN_PROGRESS"}, map[string]int64{"dep-a": 4, "dep-b": 2})
	ans := s.call(t, &answers, StepCall{Op: "offer", Body: s.offerBody(t, nil)})
	if ans.OK || ans.Status != StepRefused || ans.Error == nil || ans.Error.Code != "dependencies_open" ||
		!strings.Contains(ans.Error.Message, "dep-b (IN_PROGRESS)") || !strings.Contains(ans.Error.Message, "dep-c (unreadable)") ||
		strings.Contains(ans.Error.Message, "dep-a") {
		t.Fatalf("an offer with open dependencies: %+v %+v", ans, ans.Error)
	}
	if pending, err := s.d.Pending(); err != nil || len(pending) != 0 {
		t.Fatalf("a refused offer left a record: %v %v", pending, err)
	}
	if n := s.attemptsOfTask(t); n != 0 || len(s.offerAudits(t)) != 0 {
		t.Fatalf("a refused offer began: %d attempts", n)
	}
	if calls := s.mutations(t); len(calls) != 0 {
		t.Fatalf("a refused offer reached aimem: %v", calls)
	}

	// An offered task the member's aimem cannot read has unknown
	// dependencies, which are not DONE.
	unread := map[string]any{"hub_id": "hub-test", "project_id": "project-t", "task_id": "task-9"}
	ans = s.call(t, &answers, StepCall{Op: "offer", Body: s.offerBody(t, map[string]any{"task": unread})})
	if ans.OK || ans.Error == nil || ans.Error.Code != "dependencies_open" || !strings.Contains(ans.Error.Message, "task-9") {
		t.Fatalf("an offer of an unreadable task: %+v %+v", ans, ans.Error)
	}
	if n := s.attemptsOfTask(t); n != 0 || len(s.offerAudits(t)) != 0 {
		t.Fatalf("an offer of an unreadable task began: %d attempts", n)
	}
}

// An offer whose dependencies are all DONE is begun with the evidence of the
// driver's read, which aicrewd records in the offer's audit; evidence the
// caller supplies is replaced by the driver's own.
func TestOfferCarriesItsDependencyEvidence(t *testing.T) {
	s := setupOfferSteps(t)
	var answers []byte
	s.setDependencies(t, []string{"dep-a", "dep-b"}, map[string]string{"dep-a": "DONE", "dep-b": "DONE"},
		map[string]int64{"dep-a": 4, "dep-b": 9})
	forged := []DependencyEvidence{{TaskID: "dep-z", State: "DONE", Revision: 1}}
	ans := s.call(t, &answers, StepCall{Op: "offer", Body: s.offerBody(t, map[string]any{"dependency_evidence": forged})})
	if r := stepResult(t, ans); !ans.OK || !r.Settled || r.Outcome != "committed" {
		t.Fatalf("the offer: %+v", ans)
	}
	want := []DependencyEvidence{{TaskID: "dep-a", State: "DONE", Revision: 4}, {TaskID: "dep-b", State: "DONE", Revision: 9}}
	if got := s.offerAudits(t); len(got) != 1 || !reflect.DeepEqual(got[0], want) {
		t.Fatalf("the offer's audit evidence: %+v", got)
	}

	// A task with no dependencies is offered with empty evidence.
	s.setDependencies(t, []string{}, nil, nil)
	ev, err := s.d.Dependencies(context.Background(), "task-1")
	if err != nil || ev == nil || len(ev) != 0 {
		t.Fatalf("no dependencies: %v %v", ev, err)
	}
}

// A recovered offer resends the evidence it was begun with: the recorded
// request carries it, and nothing is read again.
func TestARecoveredOfferResendsItsEvidence(t *testing.T) {
	s := setupOfferSteps(t)
	var answers []byte
	s.setDependencies(t, []string{"dep-a"}, map[string]string{"dep-a": "DONE"}, map[string]int64{"dep-a": 4})
	s.d.crash = func(point string) bool { return point == "begun" }
	if ans := s.call(t, &answers, StepCall{Op: "offer", Body: s.offerBody(t, nil)}); ans.OK {
		t.Fatalf("the crashed offer: %+v", ans)
	}
	pending, err := s.d.Pending()
	if err != nil || len(pending) != 1 || !strings.Contains(string(pending[0].Request.Body), `"dep-a"`) {
		t.Fatalf("the recorded offer: %v %v", pending, err)
	}
	// The dependency moves on; the recovery does not read it again.
	s.setDependencies(t, []string{"dep-a"}, map[string]string{"dep-a": "DONE"}, map[string]int64{"dep-a": 5})
	s.d.crash = nil
	if ans := s.call(t, &answers, StepCall{Op: "recover"}); !ans.OK {
		t.Fatalf("recover: %+v", ans)
	}
	want := []DependencyEvidence{{TaskID: "dep-a", State: "DONE", Revision: 4}}
	if got := s.offerAudits(t); len(got) != 1 || !reflect.DeepEqual(got[0], want) {
		t.Fatalf("the recovered offer's audit evidence: %+v", got)
	}
	if a := s.attempt(t, s.attemptOfTask(t)); a.State != store.AttemptOffered {
		t.Fatalf("the recovered offer: %+v", a)
	}
}

// aimem stays authoritative: an offer whose dependencies the driver read as
// DONE is not committed when aimem refuses the claim. Like any refusal, it
// is reported to aicrewd, which settles it once the proof's grace is past.
func TestAimemRefusesAnOfferTheDriverReadAsReady(t *testing.T) {
	s := setupOfferSteps(t)
	var answers []byte
	s.setDependencies(t, []string{"dep-a"}, map[string]string{"dep-a": "DONE"}, map[string]int64{"dep-a": 4})
	s.setFault(t, "claim", "3:dependencies_open")
	ans := s.call(t, &answers, StepCall{Op: "offer", Body: s.offerBody(t, nil)})
	r := stepResult(t, ans)
	if ans.Status == StepDone || r.Outcome == "committed" || r.Report.Outcome != "refused" || r.Report.Code != "dependencies_open" {
		t.Fatalf("an offer aimem refused: %+v %+v", ans, r)
	}
	if f := loadFakeReservations(s.root); f.Holds["task-1"] != nil && f.Holds["task-1"].Active {
		t.Fatalf("aimem holds the refused offer's task: %+v", f.Holds["task-1"])
	}
}

// setTaskState sets task-1's state in the fake aimem.
func (s *stepEnv) setTaskState(t *testing.T, state string) {
	t.Helper()
	f := loadFakeReservations(s.root)
	f.Tasks["task-1"].Content["state"], _ = json.Marshal(state)
	f.save(s.root)
}

// An offer of a task that is not READY is refused before anything is
// recorded or begun, naming the coordinator's triage; once the task is
// triaged to READY, the same offer is begun (PILOT-1 §5).
func TestOfferOfATaskNotReadyIsRefused(t *testing.T) {
	s := setupOfferSteps(t)
	var answers []byte
	s.setTaskState(t, "BACKLOG")
	ans := s.call(t, &answers, StepCall{Op: "offer", Body: s.offerBody(t, nil)})
	if ans.OK || ans.Status != StepRefused || ans.Error == nil || ans.Error.Code != "task_not_ready" ||
		!strings.Contains(ans.Error.Message, "BACKLOG") || !strings.Contains(ans.Error.NextAction, "triage_task") {
		t.Fatalf("an offer of a BACKLOG task: %+v %+v", ans, ans.Error)
	}
	if pending, err := s.d.Pending(); err != nil || len(pending) != 0 {
		t.Fatalf("a refused offer left a record: %v %v", pending, err)
	}
	if n := s.attemptsOfTask(t); n != 0 || len(s.mutations(t)) != 0 {
		t.Fatalf("a refused offer began: %d attempts, calls %v", n, s.mutations(t))
	}
	s.setTaskState(t, "READY")
	ans = s.call(t, &answers, StepCall{Op: "offer", Body: s.offerBody(t, nil)})
	if r := stepResult(t, ans); !ans.OK || !r.Settled || r.Outcome != "committed" {
		t.Fatalf("the offer after triage: %+v", ans)
	}
}

// aimem's own task_not_ready (the task left READY after the launcher read
// it) is named in the answer, with the triage the step's role may do; a
// step still pending keeps its recover instruction.
func TestAimemTaskNotReadyNamesTheTriage(t *testing.T) {
	s := setupOfferSteps(t)
	var answers []byte
	s.setFault(t, "claim", "3:task_not_ready")
	ans := s.call(t, &answers, StepCall{Op: "offer", Body: s.offerBody(t, nil)})
	if r := stepResult(t, ans); ans.Status == StepDone || r.Report.Code != "task_not_ready" ||
		!strings.Contains(ans.NextAction, "not READY") || !strings.Contains(ans.NextAction, "triage_task") ||
		ans.Status != StepPending || !strings.Contains(ans.NextAction, "aicrew-agent step recover") {
		t.Fatalf("an offer aimem refused as not READY: %+v %+v", ans, r)
	}

	c := setupStepsWith(t, crewOptions{role: store.RoleIndependent, steps: true, shortHome: true})
	c.serve(t)
	c.setFault(t, "claim", "3:task_not_ready")
	claim, _ := json.Marshal(c.claimBody())
	ans = c.call(t, &answers, StepCall{Op: "claim", Body: claim})
	if r := stepResult(t, ans); ans.Status == StepDone || r.Report.Code != "task_not_ready" ||
		!strings.Contains(ans.NextAction, "Only the coordinator may triage") {
		t.Fatalf("a claim aimem refused as not READY: %+v %+v", ans, r)
	}
}

// A submitted result's reference names the attempt and the member that
// submitted it (1aad G2), in the note of the typed reference aimem keeps.
func TestASubmittedResultNamesItsAttemptAndMember(t *testing.T) {
	s := setupSteps(t, store.RoleIndependent)
	id := s.claimed(t)
	if r, err := s.work(t, id, "submit", "https://forge.example/pull/7"); err != nil || !r.Settled || r.Outcome != "committed" {
		t.Fatalf("submit: %+v %v %v", r, err, s.mismatches(t))
	}
	var refs []map[string]string
	if err := json.Unmarshal(s.fakeTask(t).Content["candidate_refs"], &refs); err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"kind": "url", "ref": "https://forge.example/pull/7",
		"note": "aicrew attempt " + id + " by member " + s.agentID}
	if len(refs) != 1 || !reflect.DeepEqual(refs[0], want) {
		t.Fatalf("the task's candidate_refs: %v", refs)
	}
}

// A resend of the same reference with the same note adds nothing; another
// attempt's submission of the same reference is recorded with its own note.
func TestWithRefKeepsOneEntryPerSubmission(t *testing.T) {
	once, err := withRef(nil, "https://forge.example/pull/7", "aicrew attempt a1 by member m1")
	if err != nil {
		t.Fatal(err)
	}
	again, err := withRef(once, "https://forge.example/pull/7", "aicrew attempt a1 by member m1")
	if err != nil || string(again) != string(once) {
		t.Fatalf("a resend changed the references: %s", again)
	}
	other, err := withRef(once, "https://forge.example/pull/7", "aicrew attempt a2 by member m1")
	var refs []map[string]string
	if err != nil || json.Unmarshal(other, &refs) != nil || len(refs) != 2 || refs[1]["note"] != "aicrew attempt a2 by member m1" {
		t.Fatalf("another attempt's submission: %s %v", other, err)
	}
}
