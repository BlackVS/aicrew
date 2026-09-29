package agent

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/BlackVS/aicrew/internal/store"
)

// stepEnv is a running aicrewd with aimem's read scope over the fake aimem's
// state, the agent in its team session through the engine, a coordinator
// acting through the store, and one task in the fake aimem.
type stepEnv struct {
	*crewEnv
	e         *Engine
	d         *Driver
	coord     store.Caller
	coordSess store.Session
	agent     store.Caller
}

var testProcess = map[string]string{"repo": "https://git.example/team/process.git",
	"commit": "3f2a9c1e5b7d4a6c8e0f1a2b3c4d5e6f7a8b9c0d", "manifest": "process/manifest.json"}

const testDigest = "sha256:worker-instructions-v1"

func setupSteps(t *testing.T, role store.Role) *stepEnv {
	t.Helper()
	ctx := context.Background()
	c := setupCrewWith(t, crewOptions{role: role, steps: true})
	t.Setenv("AICREW_FAKE_AICREW_DB", c.storePath)
	f := loadFakeReservations(c.root)
	content := map[string]any{"title": "Example task", "objective": "Deliver the example", "acceptance_criteria": "Reviewed",
		"non_goals": "No deploy", "state": "READY", "blocker": "", "dependencies": []string{},
		"candidate_refs": []any{}, "evidence_refs": []any{}, "next_action": "Pick it up", "archived": false}
	raw := map[string]json.RawMessage{}
	for k, v := range content {
		raw[k], _ = json.Marshal(v)
	}
	f.Tasks["task-1"] = &fakeTaskState{Project: "project-t", Revision: 3, Content: raw}
	f.save(c.root)

	lead, err := c.store.CreateAgent(ctx, c.operator, "agent-lead", store.NewAgent{Label: "lead"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.store.AddMember(ctx, c.operator, "member-lead", c.teamID, lead.ID, store.RoleCoordinator); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", c.storePath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE agents SET linked_hub_id = 'hub-test', linked_user_id = 'user-lead', linked_token_id = 'tok-lead' WHERE id = ?`, lead.ID); err != nil {
		t.Fatal(err)
	}
	db.Close()
	coord, _ := store.AgentCaller(lead.ID)
	coordSess, err := c.store.StartSession(ctx, coord, "start-lead", c.teamID)
	if err != nil {
		t.Fatal(err)
	}
	e := c.engine(t)
	if err := e.Start(ctx); err != nil {
		t.Fatal(err)
	}
	crew, err := NewCrew(c.cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	d := NewDriver(c.cfg.Home, crew, ExecAimem{Command: c.cfg.AimemCommand}, e, slog.New(slog.NewTextHandler(c.logs, nil)))
	d.Sleep = func(context.Context, time.Duration) error { return nil }
	agent, _ := store.AgentCaller(c.agentID)
	return &stepEnv{crewEnv: c, e: e, d: d, coord: coord, coordSess: coordSess, agent: agent}
}

func (s *stepEnv) task() map[string]any {
	return map[string]any{"hub_id": "hub-test", "project_id": "project-t", "task_id": "task-1"}
}

func (s *stepEnv) request(t *testing.T, path string, body any) StepRequest {
	t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	return StepRequest{Path: path, Body: raw, TaskID: "task-1"}
}

func (s *stepEnv) run(t *testing.T, path string, body any) (StepResult, error) {
	t.Helper()
	return s.d.Run(context.Background(), s.request(t, path, body))
}

func (s *stepEnv) claimBody() map[string]any {
	return map[string]any{"task": s.task(), "expected_revision": 3, "base_commit": "base-1", "branch": "work/task-1",
		"process": testProcess, "instruction_digest": testDigest}
}

// claimed claims task-1 through the driver.
func (s *stepEnv) claimed(t *testing.T) string {
	t.Helper()
	r, err := s.run(t, attemptsPath+"/claim", s.claimBody())
	if err != nil || !r.Settled || r.Outcome != "committed" || r.Report.Outcome != "committed" {
		t.Fatalf("claim: %+v %v", r, err)
	}
	return r.AttemptID
}

func (s *stepEnv) work(t *testing.T, id string, intent, detail string) (StepResult, error) {
	t.Helper()
	return s.run(t, attemptsPath+"/"+id+"/work", map[string]string{"intent": intent, "detail": detail})
}

func (s *stepEnv) attempt(t *testing.T, id string) store.Attempt {
	t.Helper()
	a, err := s.store.GetAttempt(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func (s *stepEnv) fakeTask(t *testing.T) *fakeTaskState {
	t.Helper()
	return loadFakeReservations(s.root).Tasks["task-1"]
}

func contentString(t *testing.T, ft *fakeTaskState, field string) string {
	t.Helper()
	var v string
	json.Unmarshal(ft.Content[field], &v)
	return v
}

func (s *stepEnv) setFault(t *testing.T, cmd string, faults ...string) {
	t.Helper()
	f := loadFakeReservations(s.root)
	f.Faults[cmd] = faults
	f.save(s.root)
}

// mutations lists the fake's mutation calls with their request keys.
func (s *stepEnv) mutations(t *testing.T) []fakeCall {
	t.Helper()
	var out []fakeCall
	for _, c := range fakeCalls(t, s.root) {
		if c.Event == "mutate" {
			out = append(out, c)
		}
	}
	return out
}

func (s *stepEnv) mismatches(t *testing.T) []string {
	t.Helper()
	var out []string
	for _, c := range fakeCalls(t, s.root) {
		if strings.HasPrefix(c.Event, "mismatch: ") {
			out = append(out, c.Event)
		}
	}
	return out
}

func (s *stepEnv) resultSeq(t *testing.T, id string) int64 {
	t.Helper()
	res, err := s.store.AttemptResults(context.Background(), id)
	if err != nil || len(res) == 0 {
		t.Fatalf("results: %v %v", res, err)
	}
	return res[len(res)-1].Seq
}

func (s *stepEnv) reviewAndConfirm(t *testing.T, id string) int64 {
	t.Helper()
	ctx := context.Background()
	seq := s.resultSeq(t, id)
	if _, err := s.store.ReviewResult(ctx, s.coord, "review-"+id, id, store.ResultReview{SessionID: s.coordSess.ID,
		Generation: s.coordSess.Generation, ResultSeq: seq, Decision: store.ReviewAccept}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.store.ConfirmDelivery(ctx, s.coord, "confirm-"+id, id, store.DeliveryConfirmation{SessionID: s.coordSess.ID,
		Generation: s.coordSess.Generation, ResultSeq: seq, Evidence: []store.Evidence{
			{Kind: "reviewed_head", Ref: "https://forge.example/pull/7#review-1"},
			{Kind: "human_merge", Ref: "https://forge.example/commit/merge-7"},
			{Kind: "post_merge_ci", Ref: "https://forge.example/actions/runs/7"}}}); err != nil {
		t.Fatal(err)
	}
	return seq
}

// The driver runs claim, update and finalize with no model: each step's
// begin goes through aicrewd, the fake aimem checks every body against the
// pending step and commits it, and aicrewd settles it from the read scope.
// The task's content keeps every field aicrew does not own, exactly as read.
func TestDriverClaimSubmitFinalize(t *testing.T) {
	s := setupSteps(t, store.RoleIndependent)
	id := s.claimed(t)
	// A member changes a field aicrew does not own, at the same revision:
	// the update must carry it through untouched.
	f := loadFakeReservations(s.root)
	f.Tasks["task-1"].Content["next_action"] = json.RawMessage(`"changed by the member"`)
	f.save(s.root)

	r, err := s.work(t, id, "submit", "https://forge.example/pull/7")
	if err != nil || !r.Settled || r.Outcome != "committed" {
		t.Fatalf("submit: %+v %v", r, err)
	}
	ft := s.fakeTask(t)
	if contentString(t, ft, "state") != "REVIEW" || contentString(t, ft, "next_action") != "changed by the member" ||
		!strings.Contains(string(ft.Content["candidate_refs"]), "https://forge.example/pull/7") ||
		contentString(t, ft, "title") != "Example task" {
		t.Fatalf("the task after submit: %v", ft.Content)
	}
	if a := s.attempt(t, id); a.Phase != store.PhaseSubmitted {
		t.Fatalf("attempt after submit: %+v", a)
	}
	seq := s.reviewAndConfirm(t, id)
	r, err = s.run(t, attemptsPath+"/"+id+"/finalize", map[string]any{"result_seq": seq})
	if err != nil || !r.Settled || r.Outcome != "committed" {
		t.Fatalf("finalize: %+v %v", r, err)
	}
	if a := s.attempt(t, id); a.State != store.AttemptClosed || a.CloseReason != "finalized" {
		t.Fatalf("attempt after finalize: %+v", a)
	}
	if ft := s.fakeTask(t); contentString(t, ft, "state") != "DONE" {
		t.Fatalf("the task after finalize: %v", ft.Content)
	}
	if m := s.mismatches(t); len(m) != 0 {
		t.Fatalf("the fake refused a body: %v", m)
	}
	if p, err := s.d.Pending(); err != nil || len(p) != 0 {
		t.Fatalf("settled steps left records: %v %v", p, err)
	}
}

// The holder releases its stopped attempt under the stopped fact, with the
// target and blocker its begin returned.
func TestDriverStopRelease(t *testing.T) {
	ctx := context.Background()
	s := setupSteps(t, store.RoleIndependent)
	id := s.claimed(t)
	if _, err := s.store.RequestStop(ctx, s.coord, "stop-1", id, store.StopRequest{SessionID: s.coordSess.ID,
		Generation: s.coordSess.Generation, Reason: "priorities changed"}); err != nil {
		t.Fatal(err)
	}
	b, err := s.store.AuthenticateSessionToken(ctx, s.e.token)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.store.ConfirmStop(ctx, s.agent, "confirm-1", id, b.SessionID, b.Generation); err != nil {
		t.Fatal(err)
	}
	r, err := s.run(t, attemptsPath+"/"+id+"/release", map[string]string{"target": "BLOCKED", "blocker": "waiting on design"})
	if err != nil || !r.Settled || r.Outcome != "committed" {
		t.Fatalf("release: %+v %v", r, err)
	}
	if a := s.attempt(t, id); a.State != store.AttemptClosed || a.CloseReason != "stopped" {
		t.Fatalf("attempt after release: %+v", a)
	}
	if ft := s.fakeTask(t); contentString(t, ft, "state") != "BLOCKED" || contentString(t, ft, "blocker") != "waiting on design" {
		t.Fatalf("the task after release: %v", ft.Content)
	}
}

// The worker accepts an offer: the driver sends the transfer.
func TestDriverAcceptTransfer(t *testing.T) {
	s := setupSteps(t, store.RoleWorker)
	id := s.offered(t)
	r, err := s.run(t, attemptsPath+"/"+id+"/accept", map[string]string{"instruction_digest": testDigest})
	if err != nil || !r.Settled || r.Outcome != "committed" {
		t.Fatalf("accept: %+v %v", r, err)
	}
	if got := s.attempt(t, id); got.State != store.AttemptRunning {
		t.Fatalf("attempt after accept: %+v", got)
	}
}

// offered has the coordinator offer task-1 to the agent, its own client
// sending the claim, and settles it.
func (s *stepEnv) offered(t *testing.T) string {
	t.Helper()
	ctx := context.Background()
	a, st, err := s.store.BeginOffer(ctx, s.coord, "offer-1", store.OfferRequest{SessionID: s.coordSess.ID,
		Generation: s.coordSess.Generation, WorkerAgentID: s.agentID,
		Task:             store.TaskRef{HubID: "hub-test", ProjectID: "project-t", TaskID: "task-1"},
		ExpectedRevision: 3, BaseCommit: "base-1", Branch: "work/task-1",
		Process: store.TrustedProcess{InstructionDigest: testDigest, Identity: store.ProcessIdentity{
			Repository: testProcess["repo"], Commit: testProcess["commit"], Manifest: testProcess["manifest"]}},
		ExpiresAt: time.Now().Add(time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	// The coordinator's own client sends the claim.
	body, _ := json.Marshal(map[string]any{"expected_revision": st.ExpectedRevision, "holder": st.Holder,
		"coordination_proof": st.CoordinationProof})
	if code, out, err := s.d.Aimem.Reservation(ctx, s.e.aimemFile, body, "claim", "--task", "task-1", "--key", st.RequestKey); err != nil || code != 0 {
		t.Fatalf("the offer's claim: %d %s %v %v", code, out, err, s.mismatches(t))
	}
	if _, set, err := s.store.SettleStep(ctx, s.coord, fileReader{root: s.root}, a.ID, st.RequestKey,
		store.StepReport{Outcome: store.HintCommitted}); err != nil || !set.Settled {
		t.Fatalf("settle the offer: %+v %v", set, err)
	}
	return a.ID
}

// aimem's exit codes, one by one, on work updates: 3 is a refusal with its
// code; 4 is retried under the same key; 5 is reconciled with the receipt
// (committed, not committed, unresolved); 2 is reported unknown.
func TestDriverExitCodes(t *testing.T) {
	s := setupSteps(t, store.RoleIndependent)
	id := s.claimed(t)
	block := func(detail string) StepResult {
		t.Helper()
		r, err := s.work(t, id, "block", detail)
		if err != nil {
			t.Fatalf("block: %v", err)
		}
		return r
	}
	resume := func() {
		t.Helper()
		if r, err := s.work(t, id, "resume", ""); err != nil || !r.Settled || r.Outcome != "committed" {
			t.Fatalf("resume: %+v %v", r, err)
		}
	}
	keyOf := func(r StepResult) []string {
		var keys []string
		for _, c := range s.mutations(t) {
			if flagValue(c.Args, "--key") == r.RequestKey {
				keys = append(keys, r.RequestKey)
			}
		}
		return keys
	}

	s.setFault(t, "update", "3:task_conflict")
	if r := block("x"); r.Report != (StepReport{Outcome: "refused", Code: "task_conflict"}) || r.Settled {
		t.Fatalf("exit 3: %+v", r)
	}
	// A refused update stays pending (it is never voided; superseding it is
	// the member's next step). The next cases start from a fresh attempt.
	s = setupSteps(t, store.RoleIndependent)
	id = s.claimed(t)

	s.setFault(t, "update", "4", "4")
	r := block("waiting")
	if r.Report.Outcome != "committed" || !r.Settled || len(keyOf(r)) != 3 {
		t.Fatalf("exit 4 twice, then 0: %+v, %d sends", r, len(keyOf(r)))
	}
	resume()

	s.setFault(t, "update", "4", "4", "4", "4")
	if r := block("waiting"); r.Report.Outcome != "unknown" || len(keyOf(r)) != s.d.Retries+1 {
		t.Fatalf("exit 4 past the retries: %+v, %d sends", r, len(keyOf(r)))
	}
}

// Exit 5: the receipt answers. A committed step lost on the way back is
// reported committed; one never committed is refused as not committed; an
// unresolved one is unknown. Exit 2 is unknown too.
func TestDriverReconcilesLostReplies(t *testing.T) {
	cases := []struct {
		name, fault, receipt string
		want                 StepReport
		settled              bool
	}{
		{"committed then lost", "5c", "", StepReport{Outcome: "committed"}, true},
		{"lost before commit", "5", "", StepReport{Outcome: "refused", Code: "not_committed"}, false},
		{"receipt unresolved", "5", "unresolved", StepReport{Outcome: "unknown"}, false},
		{"receipt lost too", "5", "5", StepReport{Outcome: "unknown"}, false},
		{"usage", "2", "", StepReport{Outcome: "unknown"}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := setupSteps(t, store.RoleIndependent)
			id := s.claimed(t)
			s.setFault(t, "update", c.fault)
			if c.receipt != "" {
				s.setFault(t, "receipt", c.receipt)
			}
			r, err := s.work(t, id, "block", "waiting")
			if err != nil || r.Report != c.want || r.Settled != c.settled {
				t.Fatalf("%+v %v", r, err)
			}
		})
	}
}

// D-b2-2: the task moved between the begin and the driver's read, so the
// driver sends nothing and reports the step refused as stale.
func TestDriverStaleRevisionSendsNothing(t *testing.T) {
	s := setupSteps(t, store.RoleIndependent)
	id := s.claimed(t)
	before := len(s.mutations(t))
	s.setFault(t, "mcp", "bump")
	r, err := s.work(t, id, "submit", "https://forge.example/pull/7")
	// The task's revision moved past the step's, so aicrewd settles the
	// unsent update as not committed at once, and it can be begun again.
	if err != nil || r.Report != (StepReport{Outcome: "refused", Code: "stale_revision"}) || !r.Settled || r.Outcome != "not_committed" {
		t.Fatalf("a stale read: %+v %v", r, err)
	}
	if len(s.mutations(t)) != before {
		t.Fatal("the driver sent a body read at another revision")
	}
	if ft := s.fakeTask(t); contentString(t, ft, "state") != "READY" {
		t.Fatalf("the task was written: %v", ft.Content)
	}
}

// 01a0eb60-c28f: a body that does not carry exactly what the begin returned is
// refused by the fake aimem. Seeded faults drop or alter each field the
// member must send, and each is caught.
func TestDriverSeededPayloadFaults(t *testing.T) {
	faults := []struct {
		name, intent, detail string
		fault                func(body map[string]any)
	}{
		{"intent dropped", "submit", "https://forge.example/pull/7", func(b map[string]any) { delete(b, "intent") }},
		{"intent altered", "block", "waiting", func(b map[string]any) { b["intent"] = "resume" }},
		{"state altered", "block", "waiting", func(b map[string]any) { setContent(b, "state", "IN_PROGRESS") }},
		{"blocker dropped", "block", "waiting", func(b map[string]any) { setContent(b, "blocker", "") }},
		{"result reference dropped", "submit", "https://forge.example/pull/7", func(b map[string]any) {
			setContent(b, "candidate_refs", []any{})
		}},
		{"unowned field altered", "submit", "https://forge.example/pull/7", func(b map[string]any) {
			setContent(b, "title", "another title")
		}},
		{"revision altered", "block", "waiting", func(b map[string]any) { b["expected_revision"] = 99 }},
	}
	for _, f := range faults {
		t.Run(f.name, func(t *testing.T) {
			s := setupSteps(t, store.RoleIndependent)
			id := s.claimed(t)
			s.d.composed = func(op string, b map[string]any) {
				if op == "update" {
					f.fault(b)
				}
			}
			r, err := s.work(t, id, f.intent, f.detail)
			if err != nil || r.Report != (StepReport{Outcome: "refused", Code: "payload_mismatch"}) || len(s.mismatches(t)) != 1 {
				t.Fatalf("the seeded fault went unnoticed: %+v %v %v", r, err, s.mismatches(t))
			}
		})
	}
}

func setContent(b map[string]any, field string, v any) {
	c := b["content"].(map[string]json.RawMessage)
	c[field], _ = json.Marshal(v)
}

// A driver killed at any point finishes the step when restarted, with the
// same begin key and the same aimem request key, and never a fresh one.
func TestDriverRecovers(t *testing.T) {
	for _, point := range []string{"begun", "answered", "sent"} {
		t.Run(point, func(t *testing.T) {
			s := setupSteps(t, store.RoleIndependent)
			id := s.claimed(t)
			s.d.crash = func(p string) bool { return p == point }
			if _, err := s.work(t, id, "submit", "https://forge.example/pull/7"); !errors.Is(err, errCrashed) {
				t.Fatalf("the crash: %v", err)
			}
			pending, err := s.d.Pending()
			if err != nil || len(pending) != 1 {
				t.Fatalf("records after the crash: %v %v", pending, err)
			}
			beginKey, stepKey := pending[0].BeginKey, pending[0].Step.RequestKey
			s.d.crash = nil
			results, err := s.d.Recover(context.Background())
			if err != nil || len(results) != 1 || !results[0].Settled || results[0].Outcome != "committed" ||
				results[0].RequestKey != stepKey {
				t.Fatalf("recover: %+v %v", results, err)
			}
			for _, c := range s.mutations(t) {
				if flagValue(c.Args, "--task") == "task-1" && c.Args[0] == "update" && flagValue(c.Args, "--key") != stepKey {
					t.Fatalf("a fresh key reached aimem: %v", c.Args)
				}
			}
			if a := s.attempt(t, id); a.Phase != store.PhaseSubmitted {
				t.Fatalf("attempt after recovery: %+v", a)
			}
			if p, _ := s.d.Pending(); len(p) != 0 {
				t.Fatalf("records after recovery: %v", p)
			}
			if point != "sent" && strings.Count(s.logs.String(), beginKey) != 0 {
				t.Fatal("the begin key reached the log")
			}
		})
	}
	// A crash before the begin's reply: the record names only the begin,
	// and recovery begins it again under the same key.
	s := setupSteps(t, store.RoleIndependent)
	p := &pendingStep{BeginKey: "step-0123456789abcdef0123456789abcdef", Request: s.request(t, attemptsPath+"/claim", s.claimBody()),
		Phase: PhaseBegin}
	if err := s.d.save(p); err != nil {
		t.Fatal(err)
	}
	results, err := s.d.Recover(context.Background())
	if err != nil || len(results) != 1 || !results[0].Settled || results[0].Report.Outcome != "committed" {
		t.Fatalf("recover a begin: %+v %v", results, err)
	}
}

// No secret at rest or in an argument: the proofs and the session token
// never reach the step records, the logs or aimem's argv.
func TestDriverKeepsNoSecret(t *testing.T) {
	s := setupSteps(t, store.RoleIndependent)
	// A claim stopped after aimem answered keeps its record, with the
	// proof's digest, until it is recovered.
	s.d.crash = func(p string) bool { return p == "sent" }
	if _, err := s.run(t, attemptsPath+"/claim", s.claimBody()); !errors.Is(err, errCrashed) {
		t.Fatalf("the crash: %v", err)
	}
	if p, err := s.d.Pending(); err != nil || len(p) != 1 || p[0].ProofSHA256 == "" {
		t.Fatalf("the claim's record: %v %v", p, err)
	}
	proofs, _ := os.ReadFile(filepath.Join(s.root, "proofs.secret"))
	secrets := strings.Fields(string(proofs))
	if len(secrets) == 0 {
		t.Fatal("no proof was seen")
	}
	s.noSecrets(t, append(secrets, s.e.token)...)
	for _, c := range s.mutations(t) {
		if c.Args[0] != "update" && !c.StdinPipe {
			t.Fatalf("a coordinated step's proof was not on stdin: %v", c.Args)
		}
	}
}

// A step reported but still pending keeps its record; Recover settles it
// again, and keeps it while aicrewd still has no final answer.
func TestDriverKeepsAPendingStep(t *testing.T) {
	s := setupSteps(t, store.RoleIndependent)
	id := s.claimed(t)
	s.setFault(t, "update", "3:task_conflict")
	r, err := s.work(t, id, "block", "waiting")
	if err != nil || r.Settled {
		t.Fatalf("a refused update: %+v %v", r, err)
	}
	pending, err := s.d.Pending()
	if err != nil || len(pending) != 1 || pending[0].Phase != PhaseSent || pending[0].Step.RequestKey != r.RequestKey {
		t.Fatalf("the pending record: %v %v", pending, err)
	}
	sends := len(s.mutations(t))
	results, err := s.d.Recover(context.Background())
	if err != nil || len(results) != 1 || results[0].Settled || results[0].RequestKey != r.RequestKey {
		t.Fatalf("recover: %+v %v", results, err)
	}
	if len(s.mutations(t)) != sends {
		t.Fatal("recovering a reported step sent it again")
	}
	if p, _ := s.d.Pending(); len(p) != 1 {
		t.Fatalf("records after recovery: %v", p)
	}
}

// An offer never accepted returns the task READY, with a reason, and keeps
// every other field.
func TestComposeNeverAcceptedRelease(t *testing.T) {
	task := &TaskDoc{Revision: 5, Fields: map[string]json.RawMessage{"title": json.RawMessage(`"T"`),
		"state": json.RawMessage(`"IN_PROGRESS"`), "next_action": json.RawMessage(`"N"`), "id": json.RawMessage(`"task-1"`)}}
	body, err := composeBody(&Step{Operation: "release", ExpectedRevision: 5, ReservationID: "r-1", Fence: "1"}, "acp1_x", task)
	if err != nil {
		t.Fatal(err)
	}
	c := body["content"].(map[string]json.RawMessage)
	if body["reason"] != "offer released" || string(c["state"]) != `"READY"` || string(c["title"]) != `"T"` ||
		string(c["next_action"]) != `"N"` || c["id"] != nil || body["coordination_proof"] != "acp1_x" {
		t.Fatalf("a never-accepted release: %v", body)
	}
}

// A begin aicrewd refuses keeps no record: nothing began, and a restart must
// not be held up by it. Recover finishes the other steps past a failing one.
func TestDriverDropsARefusedBegin(t *testing.T) {
	s := setupSteps(t, store.RoleIndependent)
	s.claimed(t)
	_, err := s.run(t, attemptsPath+"/claim", s.claimBody())
	var ref *Refusal
	if !errors.As(err, &ref) || ref.Code != "task_busy" {
		t.Fatalf("a second claim of the task: %v", err)
	}
	if p, _ := s.d.Pending(); len(p) != 0 {
		t.Fatalf("a refused begin kept a record: %v", p)
	}
	// A record whose begin fails for good sits beside one that recovers.
	bad := &pendingStep{BeginKey: "step-00000000000000000000000000000001",
		Request: s.request(t, attemptsPath+"/claim", s.claimBody()), Phase: PhaseBegin}
	if err := s.d.save(bad); err != nil {
		t.Fatal(err)
	}
	s.setFault(t, "update", "3:task_conflict")
	id := s.attemptOfTask(t)
	s.work(t, id, "block", "waiting") // a refused update: recorded, pending
	results, err := s.d.Recover(context.Background())
	if !errors.As(err, &ref) || ref.Code != "task_busy" || len(results) != 1 {
		t.Fatalf("recover past a failing step: %+v %v", results, err)
	}
	if p, _ := s.d.Pending(); len(p) != 2 {
		t.Fatalf("records after recovery: %d, want the refused begin's and the pending update's", len(p))
	}
}

func (s *stepEnv) attemptOfTask(t *testing.T) string {
	t.Helper()
	db, err := sql.Open("sqlite", s.storePath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var id string
	if err := db.QueryRow(`SELECT id FROM attempts WHERE task_id = 'task-1' AND state != 'closed'`).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

// A begin that aicrewd committed, whose reply was lost, keeps its record
// through a recovery aicrewd refuses (here, the session token has lapsed): the
// refusal says nothing about the lost begin. The next recovery finishes the
// step under the same begin key.
func TestDriverKeepsALostBeginThroughARefusal(t *testing.T) {
	ctx := context.Background()
	s := setupSteps(t, store.RoleIndependent)
	p := &pendingStep{BeginKey: "step-00000000000000000000000000000002",
		Request: s.request(t, attemptsPath+"/claim", s.claimBody()), Phase: PhaseBegin}
	if err := s.d.save(p); err != nil {
		t.Fatal(err)
	}
	// The begin commits; its reply is lost.
	if _, _, err := s.d.Crew.BeginStep(ctx, p.BeginKey, s.e.token, p.Request.Path, p.Request.Body); err != nil {
		t.Fatal(err)
	}
	token := s.e.token
	s.e.token = "acs1_" + strings.Repeat("0", 43) // a token aicrewd refuses
	_, err := s.d.Recover(ctx)
	var ref *Refusal
	if !errors.As(err, &ref) || ref.Code != "invalid_token" {
		t.Fatalf("recover with a lapsed token: %v", err)
	}
	if pending, _ := s.d.Pending(); len(pending) != 1 || pending[0].BeginKey != p.BeginKey {
		t.Fatalf("the lost begin's record was dropped: %v", pending)
	}
	s.e.token = token
	results, err := s.d.Recover(ctx)
	if err != nil || len(results) != 1 || !results[0].Settled || results[0].Outcome != "committed" {
		t.Fatalf("recover the lost begin: %+v %v", results, err)
	}
}

// The fake refuses a transfer that does not name the current hold at its
// fence: seeded faults drop the reservation or alter the fence.
func TestDriverSeededTransferFaults(t *testing.T) {
	for name, fault := range map[string]func(b map[string]any){
		"reservation dropped": func(b map[string]any) { delete(b, "reservation_id") },
		"fence altered":       func(b map[string]any) { b["fence"] = "9" },
	} {
		t.Run(name, func(t *testing.T) {
			s := setupSteps(t, store.RoleWorker)
			id := s.offered(t)
			s.d.composed = func(op string, b map[string]any) {
				if op == "transfer" {
					fault(b)
				}
			}
			r, err := s.run(t, attemptsPath+"/"+id+"/accept", map[string]string{"instruction_digest": testDigest})
			if err != nil || r.Report != (StepReport{Outcome: "refused", Code: "payload_mismatch"}) || len(s.mismatches(t)) != 1 {
				t.Fatalf("the seeded transfer fault went unnoticed: %+v %v %v", r, err, s.mismatches(t))
			}
		})
	}
}
