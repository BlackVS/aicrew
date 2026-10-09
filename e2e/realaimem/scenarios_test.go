//go:build realaimem

package realaimem

import (
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	_ "modernc.org/sqlite"
)

// TestRealAimem is b4a: the harness (H) and the flows S1–S5, in order, on
// one isolated hub.
func TestRealAimem(t *testing.T) {
	h := newHarness(t)
	sc := h.report.scenario(t, "H")
	h.bootstrap(memberSpec{"coord", "coordinator"}, memberSpec{"worker", "worker"}, memberSpec{"indep", "independent"})
	sc.check("the hub serves TLS it terminates, and aicrew is its verified peer", h.hubID != "")
	sc.check("three members joined through aicrew-agent join", len(h.members) == 3)
	for _, m := range h.members {
		sc.check(m.name+"'s launcher serves its step channel", m.launcher.cmd.ProcessState == nil)
		if m.checkNote != "" {
			sc.check(m.name+"'s join: "+m.checkNote, true)
		}
	}
	// Each launcher reports its home's capabilities as its session starts;
	// the offers below need the worker's.
	h.waitFor("the members' capability reports", 60*time.Second, func() bool {
		for _, m := range h.members {
			if h.capabilityOf(sc, m.agentID) != "write" {
				return false
			}
		}
		return true
	})
	sc.check("each member's launcher reported its capabilities", true)
	sc.finish()

	t.Run("S1_offer_flow", func(t *testing.T) { h.s1OfferFlow(t) })
	t.Run("S2_independent_claim", func(t *testing.T) { h.s2IndependentClaim(t) })
	t.Run("S3_stop", func(t *testing.T) { h.s3Stop(t) })
	t.Run("S4_never_accepted", func(t *testing.T) { h.s4NeverAccepted(t) })
	t.Run("S5_dependencies", func(t *testing.T) { h.s5Dependencies(t) })
	t.Run("G1_grants", func(t *testing.T) { h.g1Grants(t) })
	t.Run("G2_capabilities", func(t *testing.T) { h.g2Capabilities(t) })
	t.Run("E1_escalation", func(t *testing.T) { h.e1Escalation(t) })
	t.Run("F1_lost_aimem_replies", func(t *testing.T) { h.f1LostAimemReplies(t) })
	t.Run("F2_lost_aicrewd_replies", func(t *testing.T) { h.f2LostAicrewdReplies(t) })
	t.Run("F3_restarts", func(t *testing.T) { h.f3Restarts(t) })
	t.Run("F4_competing_steps", func(t *testing.T) { h.f4CompetingSteps(t) })
	t.Run("F6_recovery", func(t *testing.T) { h.f6Recovery(t) })
	// F5 crashes the coordinator. Its never-sent offer once held the
	// worker's capacity until the offer's proof expired; since the resume
	// ends that proof (01a0f758-c827), F5 frees the worker within its own
	// run, and its place after F3, F4 and F6 is kept only to avoid churn.
	// F7 scans everything, so it runs last.
	t.Run("F5_stale_steps", func(t *testing.T) { h.f5StaleSteps(t) })
	t.Run("F7_secrets", func(t *testing.T) { h.f7Secrets(t) })
	if c := os.Getenv("AICREW_E2E_SKIP_FAULT"); c != "" {
		h.report.verdict(c)
	}
}

// --- steps ----------------------------------------------------------------

// StepAnswerLite is the step channel's answer as the step command prints it.
type StepAnswerLite struct {
	OK     bool            `json:"ok"`
	Status string          `json:"status"`
	Result json.RawMessage `json:"result"`
	Error  *struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
	NextAction string `json:"next_action"`
}

// stepResult is a reservation step's result.
type stepResult struct {
	AttemptID  string `json:"attempt_id"`
	RequestKey string `json:"request_key"`
	Operation  string `json:"operation"`
	Report     struct {
		Outcome string `json:"outcome"`
		Code    string `json:"code"`
	} `json:"report"`
	Settled bool   `json:"settled"`
	Outcome string `json:"outcome"`
}

// step runs `aicrew-agent step` for mem, through its launcher, and reports
// its latency.
func (h *harness) step(sc *scenario, mem *member, op, attempt, task string, body any) (StepAnswerLite, int) {
	sc.t.Helper()
	args := []string{filepath.Join(h.bin, "aicrew-agent"), "step", op, "-home", mem.home}
	if attempt != "" {
		args = append(args, "-attempt", attempt)
	}
	if task != "" {
		args = append(args, "-task", task)
	}
	var stdin []byte
	if body != nil {
		stdin, _ = json.Marshal(body)
		args = append(args, "-body", "-")
	}
	w := h.mark()
	r := h.run(mem.env, stdin, args...)
	var ans StepAnswerLite
	if err := json.Unmarshal([]byte(r.stdout), &ans); err != nil {
		sc.t.Fatalf("step %s by %s exited %d, not an answer: %s %s", op, mem.name, r.code, r.stdout, r.stderr)
	}
	h.latency(sc, w, mem.name, op, r.code, ans)
	return ans, r.code
}

// inboxMessage is one message `aicrew-agent inbox -json` delivered.
type inboxMessage struct {
	ID        string `json:"id"`
	Kind      string `json:"kind"`
	AttemptID string `json:"attempt_id"`
	Task      *struct {
		TaskID string `json:"task_id"`
	} `json:"task"`
	// Offer is an offer's details, on the message that announces it.
	Offer *struct {
		Repository repositoryBody `json:"repository"`
		Process    struct {
			Repo     string `json:"repo"`
			Commit   string `json:"commit"`
			Manifest string `json:"manifest"`
		} `json:"process"`
		InstructionDigest string `json:"instruction_digest"`
	} `json:"offer"`
	Text string `json:"text"`
}

// inbox reads mem's inbox through its launcher, as its client does.
func (h *harness) inbox(sc *scenario, mem *member) []inboxMessage {
	sc.t.Helper()
	r := h.run(mem.env, nil, filepath.Join(h.bin, "aicrew-agent"), "inbox", "-home", mem.home, "-limit", "100", "-json")
	var ans StepAnswerLite
	var page struct {
		Messages []inboxMessage `json:"messages"`
	}
	if json.Unmarshal([]byte(r.stdout), &ans) != nil || !ans.OK || json.Unmarshal(ans.Result, &page) != nil {
		sc.t.Fatalf("inbox of %s exited %d: %s %s", mem.name, r.code, r.stdout, r.stderr)
	}
	return page.Messages
}

// ackInbox acknowledges mem's messages ids through its launcher.
func (h *harness) ackInbox(sc *scenario, mem *member, ids []string) bool {
	sc.t.Helper()
	r := h.run(mem.env, nil, filepath.Join(h.bin, "aicrew-agent"), "inbox", "-home", mem.home, "-ack", strings.Join(ids, ","))
	return r.code == 0
}

// committed runs a reservation step that must commit, and returns its
// result.
func (h *harness) committed(sc *scenario, mem *member, op, attempt, task string, body any) stepResult {
	sc.t.Helper()
	ans, code := h.step(sc, mem, op, attempt, task, body)
	var res stepResult
	_ = json.Unmarshal(ans.Result, &res)
	sc.require(fmt.Sprintf("%s's %s commits", mem.name, op), code == 0 && ans.OK && res.Settled && res.Outcome == "committed",
		describe(ans))
	return res
}

// local runs a local step that must succeed.
func (h *harness) local(sc *scenario, mem *member, op, attempt string, body any) {
	sc.t.Helper()
	ans, code := h.step(sc, mem, op, attempt, "", body)
	sc.require(fmt.Sprintf("%s's %s succeeds", mem.name, op), code == 0 && ans.OK, describe(ans))
}

// describe is an answer as JSON, for a failed assertion.
func describe(ans StepAnswerLite) string {
	b, _ := json.Marshal(ans)
	return string(b)
}

// repositoryBody is an offer's or a claim's repository.
type repositoryBody struct {
	Kind          string `json:"kind"`
	URL           string `json:"url"`
	Access        string `json:"access"`
	DefaultBranch string `json:"default_branch"`
	BaseCommit    string `json:"base_commit"`
	Branch        string `json:"branch"`
}

// repositoryFor is the repository every step on task names: the hub's,
// based on the process commit.
func (h *harness) repositoryFor(task string) repositoryBody {
	return repositoryBody{Kind: repoKind, URL: h.forge.repoURL(), Access: "write", DefaultBranch: "main", BaseCommit: processCommit,
		Branch: "work/" + task}
}

func (h *harness) offerBody(task taskRef, worker *member, expires time.Time) map[string]any {
	return map[string]any{"worker_agent_id": worker.agentID, "task": h.taskRefBody(task.ID),
		"expected_revision": task.Revision, "repository": h.repositoryFor(task.ID),
		"process":            map[string]string{"repo": processRepo, "commit": processCommit, "manifest": processManifest},
		"instruction_digest": instructionHash, "expires_at": expires.UTC().Format(time.RFC3339)}
}

func (h *harness) claimBody(task taskRef) map[string]any {
	return map[string]any{"task": h.taskRefBody(task.ID), "expected_revision": task.Revision,
		"repository":         h.repositoryFor(task.ID),
		"process":            map[string]string{"repo": processRepo, "commit": processCommit, "manifest": processManifest},
		"instruction_digest": instructionHash}
}

func (h *harness) taskRefBody(id string) map[string]string {
	return map[string]string{"hub_id": h.hubID, "project_id": projectID, "task_id": id}
}

var deliveryEvidence = []map[string]string{
	{"kind": "reviewed_head", "ref": "https://forge.example.test/e2e/pull/1#review-1"},
	{"kind": "human_merge", "ref": "https://forge.example.test/e2e/commit/merge-1"},
	{"kind": "post_merge_ci", "ref": "https://forge.example.test/e2e/actions/runs/1"},
}

func identityRef(attemptID string, worker *member) string {
	return "aicrew attempt " + attemptID + " by member " + worker.agentID
}

// --- aimem's side -----------------------------------------------------------

// taskRef is an aimem task's ID and the revision last read.
type taskRef struct {
	ID       string
	Revision int64
}

// aimemTask is a task as the hub's admin reads it.
type aimemTask struct {
	ID            string           `json:"id"`
	Revision      int64            `json:"revision"`
	State         string           `json:"state"`
	Blocker       string           `json:"blocker"`
	Dependencies  []string         `json:"dependencies"`
	CandidateRefs []map[string]any `json:"candidate_refs"`
	EvidenceRefs  []map[string]any `json:"evidence_refs"`
	Coordination  json.RawMessage  `json:"coordination"`
	Rest          map[string]any   `json:"-"`
	raw           []byte
}

func newKey() string {
	var b [12]byte
	rand.Read(b[:])
	return "e2e-" + hex.EncodeToString(b[:])
}

func taskContent(title string, deps []string) map[string]any {
	if deps == nil {
		deps = []string{}
	}
	return map[string]any{"title": title, "objective": "End-to-end task " + title, "acceptance_criteria": "Verified by the harness",
		"non_goals": "None", "state": "READY", "blocker": "", "dependencies": deps, "candidate_refs": []any{},
		"evidence_refs": []any{}, "next_action": "Offer it", "archived": false}
}

// createTask creates a READY task in the project.
func (h *harness) createTask(sc *scenario, title string, deps ...string) taskRef {
	sc.t.Helper()
	var task aimemTask
	status := h.hubJSON(http.MethodPost, "/v1/projects/"+projectID+"/tasks", map[string]string{"Idempotency-Key": newKey()},
		taskContent(title, deps), &task)
	sc.require("aimem creates task "+title, status == http.StatusCreated && task.ID != "", status)
	return taskRef{ID: task.ID, Revision: task.Revision}
}

// readTask reads a task as the hub's admin.
func (h *harness) readTask(sc *scenario, id string) aimemTask {
	sc.t.Helper()
	var raw json.RawMessage
	status := h.hubJSON(http.MethodGet, "/v1/tasks/"+id, nil, nil, &raw)
	sc.require("aimem reads task "+id, status == http.StatusOK, status)
	var task aimemTask
	if err := json.Unmarshal(raw, &task); err != nil {
		sc.t.Fatal(err)
	}
	task.raw = raw
	return task
}

// setTaskState edits a task's state as a plain admin edit (no reservation).
func (h *harness) setTaskState(sc *scenario, id, state string) taskRef {
	sc.t.Helper()
	cur := h.readTask(sc, id)
	var body map[string]any
	_ = json.Unmarshal(cur.raw, &body)
	content := map[string]any{"expected_revision": cur.Revision}
	for _, f := range []string{"title", "objective", "acceptance_criteria", "non_goals", "blocker", "dependencies",
		"candidate_refs", "evidence_refs", "next_action", "archived"} {
		content[f] = body[f]
	}
	content["state"] = state
	var task aimemTask
	status := h.hubJSON(http.MethodPut, "/v1/tasks/"+id, map[string]string{"Idempotency-Key": newKey()}, content, &task)
	sc.require(fmt.Sprintf("aimem moves task %s to %s", id, state), status == http.StatusOK && task.State == state, status)
	return taskRef{ID: id, Revision: task.Revision}
}

// hold reads a task's reservation hold through the recovery reader, as the
// hub's admin.
func (h *harness) hold(sc *scenario, task string) map[string]any {
	sc.t.Helper()
	r := h.run(h.hostEnv(), nil, h.aimem(), "reservation", "recover", "status", "--task", task,
		"--hub", h.hubURL, "--admin-token-file", h.adminFile, "--hub-ca-file", h.caFile)
	var out map[string]any
	if r.code != 0 || json.Unmarshal([]byte(r.stdout), &out) != nil {
		sc.t.Fatalf("recover status %s: %d %s %s", task, r.code, r.stdout, r.stderr)
	}
	return out
}

// receiptCommitted reads, as the hub's admin, aimem's receipt for a step's
// request key, and reports whether it is committed.
func (h *harness) receiptCommitted(sc *scenario, task, op, key string) bool {
	sc.t.Helper()
	sum := sha256.Sum256([]byte(key))
	k1 := "k1_" + base64.RawURLEncoding.EncodeToString(sum[:])
	r := h.run(h.hostEnv(), nil, h.aimem(), "reservation", "recover", "receipt", op, "--task", task, "--key-digest", k1,
		"--hub", h.hubURL, "--admin-token-file", h.adminFile, "--hub-ca-file", h.caFile)
	var out map[string]any
	if r.code != 0 || json.Unmarshal([]byte(r.stdout), &out) != nil {
		sc.t.Logf("recover receipt %s %s: %d %s %s", op, task, r.code, r.stdout, r.stderr)
		return false
	}
	// The recovery reader lists the task's committed receipts under the key.
	receipts, _ := out["receipts"].([]any)
	if len(receipts) != 1 {
		sc.t.Logf("recover receipt %s %s: %s", op, task, r.stdout)
	}
	return len(receipts) == 1
}

// holdState is the hold's state: held, closed, or none.
func holdState(hold map[string]any) string {
	for _, k := range []string{"state", "status"} {
		if s, ok := hold[k].(string); ok {
			return s
		}
	}
	if hd, ok := hold["hold"].(map[string]any); ok {
		return holdState(hd)
	}
	return ""
}

// --- aicrew's side ----------------------------------------------------------

// aicrewDB is a read-only view of aicrewd's store.
func (h *harness) aicrewDB(sc *scenario) *sql.DB {
	sc.t.Helper()
	db, err := sql.Open("sqlite", "file:"+h.storePath+"?mode=ro&_pragma=busy_timeout(5000)")
	if err != nil {
		sc.t.Fatal(err)
	}
	return db
}

// attemptRow is the part of an attempt the scenarios assert.
type attemptRow struct {
	ID, State, Phase, CloseReason, Worker, PendingKey string
}

func (h *harness) attempt(sc *scenario, id string) attemptRow {
	sc.t.Helper()
	db := h.aicrewDB(sc)
	defer db.Close()
	var a attemptRow
	if err := db.QueryRow(`SELECT id, state, phase, close_reason, worker_agent_id, pending_key FROM attempts WHERE id = ?`, id).
		Scan(&a.ID, &a.State, &a.Phase, &a.CloseReason, &a.Worker, &a.PendingKey); err != nil {
		sc.t.Fatalf("attempt %s: %v", id, err)
	}
	return a
}

func (h *harness) attemptsOfTask(sc *scenario, task string) []attemptRow {
	sc.t.Helper()
	db := h.aicrewDB(sc)
	defer db.Close()
	rows, err := db.Query(`SELECT id, state, phase, close_reason, worker_agent_id, pending_key FROM attempts WHERE task_id = ?`, task)
	if err != nil {
		sc.t.Fatal(err)
	}
	defer rows.Close()
	var out []attemptRow
	for rows.Next() {
		var a attemptRow
		if err := rows.Scan(&a.ID, &a.State, &a.Phase, &a.CloseReason, &a.Worker, &a.PendingKey); err != nil {
			sc.t.Fatal(err)
		}
		out = append(out, a)
	}
	return out
}

// attemptRepository is the repository aicrew recorded on an attempt.
func (h *harness) attemptRepository(sc *scenario, id string) repositoryBody {
	sc.t.Helper()
	db := h.aicrewDB(sc)
	defer db.Close()
	var r repositoryBody
	if err := db.QueryRow(`SELECT repository_kind, repository_url, repository_access, default_branch, base_commit, branch
		FROM attempts WHERE id = ?`, id).Scan(&r.Kind, &r.URL, &r.Access, &r.DefaultBranch, &r.BaseCommit, &r.Branch); err != nil {
		sc.t.Fatal(err)
	}
	return r
}

// openWorkOf counts an agent's open attempts: its capacity.
func (h *harness) openWorkOf(sc *scenario, agent string) int {
	sc.t.Helper()
	db := h.aicrewDB(sc)
	defer db.Close()
	var n int
	if err := db.QueryRow(`SELECT count(*) FROM attempts WHERE worker_agent_id = ? AND state != 'closed'`, agent).Scan(&n); err != nil {
		sc.t.Fatal(err)
	}
	return n
}

// offerAudit is the dependency evidence the offer of task recorded.
func (h *harness) offerAudit(sc *scenario, task string) []map[string]any {
	sc.t.Helper()
	db := h.aicrewDB(sc)
	defer db.Close()
	rows, err := db.Query(`SELECT input FROM audit WHERE operation = 'attempt.offer' ORDER BY id`)
	if err != nil {
		sc.t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var input string
		_ = rows.Scan(&input)
		var in struct {
			Task struct {
				TaskID string `json:"task_id"`
			} `json:"task"`
			Evidence []map[string]any `json:"dependency_evidence"`
		}
		if json.Unmarshal([]byte(input), &in) == nil && in.Task.TaskID == task {
			return in.Evidence
		}
	}
	return nil
}

// --- S1–S5 --------------------------------------------------------------------

// S1: an offer with its dependencies DONE, accepted, submitted, reviewed,
// its delivery confirmed and finalized DONE, through the real hub.
func (h *harness) s1OfferFlow(t *testing.T) {
	sc := h.report.scenario(t, "S1")
	coord, worker := h.members["coord"], h.members["worker"]
	dep := h.createTask(sc, "S1 dependency")
	dep = h.setTaskState(sc, dep.ID, "DONE")
	task := h.createTask(sc, "S1 offer flow", dep.ID)

	offer := h.committed(sc, coord, "offer", "", "", h.offerBody(task, worker, time.Now().Add(time.Hour)))
	id := offer.AttemptID
	keys := map[string]string{"claim": offer.RequestKey}
	sc.check("aicrew: the attempt is offered", h.attempt(sc, id).State == "offered", h.attempt(sc, id))
	ev := h.offerAudit(sc, task.ID)
	sc.check("aicrew's audit holds the dependency evidence", len(ev) == 1 && ev[0]["task_id"] == dep.ID &&
		ev[0]["state"] == "DONE" && ev[0]["revision"] == float64(dep.Revision), ev)
	sc.check("aimem holds the task for the offer", holdState(h.hold(sc, task.ID)) == "held", h.hold(sc, task.ID))

	// The worker finds the offer in its own inbox (pilot G1) and accepts by
	// the attempt ID it names there; nothing is relayed.
	found, ids := inboxMessage{}, []string{}
	for _, m := range h.inbox(sc, worker) {
		ids = append(ids, m.ID)
		if m.Kind == "lifecycle" && m.Task != nil && m.Task.TaskID == task.ID && m.AttemptID != "" {
			found = m
		}
	}
	sc.check("the worker's inbox names the offered attempt", found.AttemptID == id, found.AttemptID, id)
	// The offer's details come with it (pilot G2): the worker starts its
	// worktree from them, and computes the instruction digest itself from
	// the pinned manifest before accepting.
	o := found.Offer
	sc.require("the worker's inbox carries the offer's details", o != nil && o.Repository == h.repositoryFor(task.ID) &&
		o.Process.Repo == processRepo && o.Process.Commit == processCommit && o.Process.Manifest == processManifest, found)
	sc.check("aicrew: the attempt records the offer's repository", h.attemptRepository(sc, id) == h.repositoryFor(task.ID))
	digest := h.processDigest(o.Process.Commit, o.Process.Manifest)
	sc.check("the worker's own digest of the pinned manifest equals the offer's", digest == o.InstructionDigest,
		digest, o.InstructionDigest)
	sc.check("the worker acknowledges what it read", h.ackInbox(sc, worker, ids))
	keys["transfer"] = h.committed(sc, worker, "accept", found.AttemptID, task.ID, map[string]string{"instruction_digest": digest}).RequestKey
	sc.check("aicrew: the attempt runs", h.attempt(sc, id).State == "running", h.attempt(sc, id))

	pr := "https://forge.example.test/e2e/pull/1"
	keys["update"] = h.committed(sc, worker, "work", id, task.ID, map[string]string{"intent": "submit", "detail": pr}).RequestKey
	got := h.readTask(sc, task.ID)
	sc.check("aimem: the task is in REVIEW", got.State == "REVIEW", got.State)
	noted := false
	for _, r := range got.CandidateRefs {
		noted = noted || (r["ref"] == pr && r["note"] == identityRef(id, worker))
	}
	sc.check("aimem: the result reference names the attempt and the member", noted, got.CandidateRefs)

	h.local(sc, coord, "review", id, map[string]any{"result_seq": 1, "decision": "accept"})
	h.local(sc, coord, "confirm-delivery", id, map[string]any{"result_seq": 1, "evidence": deliveryEvidence})
	keys["finalize"] = h.committed(sc, worker, "finalize", id, task.ID, map[string]any{"result_seq": 1}).RequestKey

	got = h.readTask(sc, task.ID)
	sc.check("aimem: the task is DONE", got.State == "DONE", got.State)
	for _, op := range []string{"claim", "transfer", "update", "finalize"} {
		sc.check("aimem: the "+op+"'s receipt is committed under aicrew's request key", keys[op] != "" &&
			h.receiptCommitted(sc, task.ID, op, keys[op]), keys[op])
	}
	// aimem committed the finalize under its C5-w3 digest check; the evidence
	// it persisted must end with the attempt's identity (01a0f6d4-2170).
	h.checkTerminalIdentity(sc, task.ID, identityRef(id, worker))
	a := h.attempt(sc, id)
	sc.check("aicrew: the attempt is closed as finalized", a.State == "closed" && a.PendingKey == "", a)
	sc.check("aicrew: the worker's capacity is free", h.openWorkOf(sc, worker.agentID) == 0)
	sc.check("aimem: the hold is closed by the holder's finalize", holdState(h.hold(sc, task.ID)) != "held", h.hold(sc, task.ID))
}

// S2: an independent claim, submitted, and finalized by the reviewing
// coordinator; the identity names the worker, not the finalizer.
func (h *harness) s2IndependentClaim(t *testing.T) {
	sc := h.report.scenario(t, "S2")
	coord, indep := h.members["coord"], h.members["indep"]
	task := h.createTask(sc, "S2 independent claim")
	claim := h.committed(sc, indep, "claim", "", "", h.claimBody(task))
	id := claim.AttemptID
	sc.check("aicrew: the claim runs", h.attempt(sc, id).State == "running", h.attempt(sc, id))
	h.committed(sc, indep, "work", id, task.ID, map[string]string{"intent": "submit", "detail": "https://forge.example.test/e2e/pull/2"})
	h.local(sc, coord, "review", id, map[string]any{"result_seq": 1, "decision": "accept"})
	h.local(sc, coord, "confirm-delivery", id, map[string]any{"result_seq": 1, "evidence": deliveryEvidence})
	h.committed(sc, coord, "finalize", id, task.ID, map[string]any{"result_seq": 1})
	got := h.readTask(sc, task.ID)
	sc.check("aimem: the task is DONE", got.State == "DONE", got.State)
	h.checkTerminalIdentity(sc, task.ID, identityRef(id, indep))
	ev := h.terminalEvidence(sc, task.ID)
	sc.check("aimem: the terminal evidence names the worker, not the finalizer", !contains(ev, identityRef(id, coord)), ev)
	sc.check("aicrew: the attempt is closed", h.attempt(sc, id).State == "closed", h.attempt(sc, id))
}

// S3: a stop the worker confirms, released to BLOCKED with a blocker, and
// another released to READY.
func (h *harness) s3Stop(t *testing.T) {
	sc := h.report.scenario(t, "S3")
	coord, worker := h.members["coord"], h.members["worker"]
	for _, target := range []map[string]string{{"target": "BLOCKED", "blocker": "waiting on design"}, {"target": "READY"}} {
		task := h.createTask(sc, "S3 stop to "+target["target"])
		offer := h.committed(sc, coord, "offer", "", "", h.offerBody(task, worker, time.Now().Add(time.Hour)))
		id := offer.AttemptID
		h.committed(sc, worker, "accept", id, task.ID, map[string]string{"instruction_digest": instructionHash})
		h.local(sc, coord, "stop", id, map[string]string{"reason": "priorities changed"})
		h.local(sc, worker, "confirm-stop", id, map[string]any{})
		h.committed(sc, worker, "release", id, task.ID, target)
		got := h.readTask(sc, task.ID)
		sc.check("aimem: the stopped task is "+target["target"], got.State == target["target"] && got.Blocker == target["blocker"], got.State, got.Blocker)
		a := h.attempt(sc, id)
		sc.check("aicrew: the attempt is closed as stopped", a.State == "closed" && a.CloseReason == "stopped", a)
		sc.check("aicrew: the worker's capacity is free", h.openWorkOf(sc, worker.agentID) == 0)
	}
}

// S4: offers never accepted, released by the coordinator: withdrawn,
// declined, and expired.
func (h *harness) s4NeverAccepted(t *testing.T) {
	sc := h.report.scenario(t, "S4")
	coord, worker := h.members["coord"], h.members["worker"]
	released := func(what string, task taskRef, id string) {
		h.committed(sc, coord, "withdraw", id, task.ID, map[string]any{})
		got := h.readTask(sc, task.ID)
		sc.check("aimem: the "+what+" offer's task is READY again", got.State == "READY", got.State)
		sc.check("aimem: no hold remains after the "+what+" offer", holdState(h.hold(sc, task.ID)) != "held", h.hold(sc, task.ID))
		a := h.attempt(sc, id)
		sc.check("aicrew: the "+what+" offer is closed", a.State == "closed", a)
		sc.check("aicrew: the worker's capacity is free after the "+what+" offer", h.openWorkOf(sc, worker.agentID) == 0)
	}

	task := h.createTask(sc, "S4 withdrawn")
	id := h.committed(sc, coord, "offer", "", "", h.offerBody(task, worker, time.Now().Add(time.Hour))).AttemptID
	released("withdrawn", task, id)

	task = h.createTask(sc, "S4 declined")
	id = h.committed(sc, coord, "offer", "", "", h.offerBody(task, worker, time.Now().Add(time.Hour))).AttemptID
	h.local(sc, worker, "decline", id, map[string]any{})
	released("declined", task, id)

	task = h.createTask(sc, "S4 expired")
	expires := time.Now().Add(20 * time.Second)
	id = h.committed(sc, coord, "offer", "", "", h.offerBody(task, worker, expires)).AttemptID
	time.Sleep(time.Until(expires) + 2*time.Second)
	ans, _ := h.step(sc, worker, "accept", id, task.ID, map[string]string{"instruction_digest": instructionHash})
	sc.check("aicrew refuses accepting an expired offer", !ans.OK, ans)
	released("expired", task, id)
}

// S5: the dependency check against the real hub. An open dependency is
// refused before anything reaches aimem; once it is DONE the offer commits;
// and a dependency reopened between the driver's read and the claim is
// refused by aimem, which stays authoritative.
func (h *harness) s5Dependencies(t *testing.T) {
	sc := h.report.scenario(t, "S5")
	coord, worker := h.members["coord"], h.members["worker"]

	dep := h.createTask(sc, "S5 open dependency")
	task := h.createTask(sc, "S5 blocked by a dependency", dep.ID)
	ans, code := h.step(sc, coord, "offer", "", "", h.offerBody(task, worker, time.Now().Add(time.Hour)))
	sc.check("an open dependency is refused dependencies_open", code == 3 && ans.Error != nil && ans.Error.Code == "dependencies_open" &&
		strings.Contains(ans.Error.Message, dep.ID), ans)
	sc.check("aicrew: nothing began", len(h.attemptsOfTask(sc, task.ID)) == 0)
	sc.check("aimem: nothing was sent", holdState(h.hold(sc, task.ID)) != "held" && h.readTask(sc, task.ID).Revision == task.Revision)

	h.setTaskState(sc, dep.ID, "DONE")
	id := h.committed(sc, coord, "offer", "", "", h.offerBody(task, worker, time.Now().Add(time.Hour))).AttemptID
	sc.check("once the dependency is DONE, aimem holds the task", holdState(h.hold(sc, task.ID)) == "held")
	h.committed(sc, coord, "withdraw", id, task.ID, map[string]any{})

	// The race: pause the coordinator's aimem before it sends the claim,
	// reopen the dependency, and let the claim go.
	dep2 := h.createTask(sc, "S5 dependency reopened")
	dep2 = h.setTaskState(sc, dep2.ID, "DONE")
	task2 := h.createTask(sc, "S5 dependency race", dep2.ID)
	if err := os.WriteFile(filepath.Join(h.gate, "hold-reservation-claim"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	type stepOut struct {
		ans  StepAnswerLite
		code int
	}
	done := make(chan stepOut, 1)
	go func() {
		a, c := h.step(sc, coord, "offer", "", "", h.offerBody(task2, worker, time.Now().Add(time.Hour)))
		done <- stepOut{a, c}
	}()
	h.waitFor("the coordinator's claim to pause", 60*time.Second, func() bool {
		_, err := os.Stat(filepath.Join(h.gate, "paused-reservation-claim"))
		return err == nil
	})
	h.setTaskState(sc, dep2.ID, "IN_PROGRESS")
	os.Remove(filepath.Join(h.gate, "paused-reservation-claim"))
	if err := os.WriteFile(filepath.Join(h.gate, "go-reservation-claim"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	out := <-done
	var res stepResult
	_ = json.Unmarshal(out.ans.Result, &res)
	sc.check("aimem refuses the claim the driver read as ready", res.Report.Outcome == "refused" && res.Outcome != "committed", out.ans)
	attempts := h.attemptsOfTask(sc, task2.ID)
	sc.require("aicrew began one offer for the race", len(attempts) == 1, attempts)
	race := attempts[0].ID
	h.waitFor("aicrew to settle the refused offer as not committed", 3*time.Minute, func() bool {
		return h.attempt(sc, race).State == "closed"
	})
	sc.check("aimem: no hold for the refused offer", holdState(h.hold(sc, task2.ID)) != "held", h.hold(sc, task2.ID))
	sc.check("aicrew: the worker's capacity is free", h.openWorkOf(sc, worker.agentID) == 0)
}
