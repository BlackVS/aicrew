//go:build realaimem

package realaimem

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The network and staleness faults (crew-execution b4b-1): F1, lost aimem
// replies; F2, lost aicrewd replies; F5, stale steps. Each fault is injected
// by the fault proxy or by a gate in the timed aimem, and belongs to a
// skip-the-fault case: with AICREW_E2E_SKIP_FAULT=<case> it is left out, and
// the scenario must then fail.

// --- gates in the timed aimem ---------------------------------------------

// gateName names an aimem command for the timed aimem's gates: its first
// two arguments, as "reservation-claim" or "mcp-".
func gateName(cmd, op string) string { return cmd + "-" + op }

// holdNext pauses the next such aimem command before it runs, unless the
// run skips case c.
func (h *harness) holdNext(c, name string) bool {
	if skipFault(c) {
		h.report.write(map[string]any{"type": "fault", "case": c, "gate": name, "skipped": true})
		return false
	}
	h.report.write(map[string]any{"type": "fault", "case": c, "gate": name, "action": "hold"})
	if err := os.WriteFile(filepath.Join(h.gate, "hold-"+name), nil, 0o600); err != nil {
		h.t.Fatal(err)
	}
	return true
}

func (h *harness) waitPaused(name string) {
	h.waitFor("aimem to pause at "+name, 2*time.Minute, func() bool {
		_, err := os.Stat(filepath.Join(h.gate, "paused-"+name))
		return err == nil
	})
	os.Remove(filepath.Join(h.gate, "paused-"+name))
}

func (h *harness) release(name string) {
	if err := os.WriteFile(filepath.Join(h.gate, "go-"+name), nil, 0o600); err != nil {
		h.t.Fatal(err)
	}
}

// captureNext keeps the next such command's standard input in an
// owner-only run file, never copied to the artifacts: a proof is a secret.
func (h *harness) captureNext(name string) {
	os.Remove(filepath.Join(h.gate, "captured-"+name))
	if err := os.WriteFile(filepath.Join(h.gate, "capture-"+name), nil, 0o600); err != nil {
		h.t.Fatal(err)
	}
}

func (h *harness) captured(sc *scenario, name string) map[string]json.RawMessage {
	sc.t.Helper()
	raw, err := os.ReadFile(filepath.Join(h.gate, "captured-"+name))
	if err != nil {
		sc.t.Fatalf("nothing captured for %s: %v", name, err)
	}
	var body map[string]json.RawMessage
	if err := json.Unmarshal(raw, &body); err != nil {
		sc.t.Fatalf("the captured %s body is not JSON", name)
	}
	// The proof is a secret: register it now, before a later capture
	// replaces this file, so that F7 scans for every proof the run saw.
	var proof string
	if json.Unmarshal(body["coordination_proof"], &proof) == nil && proof != "" {
		h.knowSecret(proof)
		h.outMu.Lock()
		h.proofs = append(h.proofs, proof)
		h.outMu.Unlock()
	}
	return body
}

// aimemCalls are the timed aimem's calls since w, as "cmd op exit".
func (h *harness) aimemCalls(w window) []string {
	var out []string
	for _, l := range tail(lines(h.timingLog), w.timing) {
		var e struct {
			Cmd, Op string
			Exit    int
		}
		if json.Unmarshal([]byte(l), &e) == nil {
			out = append(out, fmt.Sprintf("%s %s %d", e.Cmd, e.Op, e.Exit))
		}
	}
	return out
}

func contains(xs []string, x string) bool {
	for _, y := range xs {
		if y == x {
			return true
		}
	}
	return false
}

// --- the persisted terminal evidence (fold-in of 01a0f6d4-2170) ------------

// terminalEvidence is the terminal evidence of the task's last finalize, as
// aimem persisted it in its reservation event. No hub route reads the
// events, so the hub's project store is read, read-only.
func (h *harness) terminalEvidence(sc *scenario, task string) []string {
	sc.t.Helper()
	var dbs []string
	filepath.Walk(filepath.Join(h.root, "hub", "aimem"), func(p string, info os.FileInfo, err error) error {
		if err == nil && !info.IsDir() && strings.HasSuffix(p, ".db") {
			dbs = append(dbs, p)
		}
		return nil
	})
	for _, p := range dbs {
		db, err := sql.Open("sqlite", "file:"+p+"?mode=ro&_pragma=busy_timeout(5000)")
		if err != nil {
			continue
		}
		var body string
		err = db.QueryRow(`SELECT body FROM task_reservation_events WHERE task_id = ? AND operation = 'finalize'
			ORDER BY sequence DESC LIMIT 1`, task).Scan(&body)
		db.Close()
		if err != nil {
			continue
		}
		var ev struct {
			TerminalEvidence []string `json:"terminal_evidence"`
		}
		if json.Unmarshal([]byte(body), &ev) != nil {
			sc.t.Fatalf("task %s's finalize event is not JSON", task)
		}
		return ev.TerminalEvidence
	}
	sc.t.Fatalf("no finalize event for task %s in the hub's stores %v", task, dbs)
	return nil
}

// endsWithIdentity reports whether terminal evidence ends with the attempt's
// identity reference.
func endsWithIdentity(evidence []string, identity string) bool {
	return len(evidence) > 0 && evidence[len(evidence)-1] == identity
}

// checkTerminalIdentity asserts the identity is the last terminal evidence
// aimem persisted, and that the candidate note alone would not satisfy
// that check.
func (h *harness) checkTerminalIdentity(sc *scenario, task, identity string) {
	sc.t.Helper()
	ev := h.terminalEvidence(sc, task)
	sc.check("aimem persisted terminal evidence ending with the attempt's identity", endsWithIdentity(ev, identity), ev)
	if len(ev) > 0 {
		sc.check("the check fails on evidence without the identity, whatever the candidate note says",
			!endsWithIdentity(ev[:len(ev)-1], identity))
	}
}

// --- helpers for the fault flows ---------------------------------------------

// attemptOf is the newest attempt of task in aicrew's store.
func (h *harness) attemptOf(sc *scenario, task string) attemptRow {
	sc.t.Helper()
	all := h.attemptsOfTask(sc, task)
	sc.require("aicrew has an attempt for task "+task, len(all) > 0)
	return all[len(all)-1]
}

// settledNotCommitted waits for aicrew to settle a step as not committed:
// its pending key cleared, with cond on the attempt.
func (h *harness) settledNotCommitted(sc *scenario, what, id string, cond func(attemptRow) bool) {
	sc.t.Helper()
	deadline := time.Now().Add(2 * time.Minute)
	for {
		a := h.attempt(sc, id)
		if a.PendingKey == "" && cond(a) {
			sc.check("aicrew settles "+what+" as not committed", true)
			return
		}
		if time.Now().After(deadline) {
			sc.check("aicrew settles "+what+" as not committed", false, a)
			sc.t.FailNow()
		}
		time.Sleep(time.Second)
	}
}

// recoverStep runs `aicrew-agent step recover` for mem.
func (h *harness) recoverStep(sc *scenario, mem *member) StepAnswerLite {
	sc.t.Helper()
	ans, _ := h.step(sc, mem, "recover", "", "", nil)
	return ans
}

// sessionFile is the member's current aimem team-session file.
func (h *harness) sessionFile(mem *member) string {
	raw, err := os.ReadFile(filepath.Join(mem.home, "state", "aicrew-session.json"))
	if err != nil {
		h.t.Fatal(err)
	}
	var st struct {
		AimemFile string `json:"aimem_session_file"`
	}
	if json.Unmarshal(raw, &st) != nil || st.AimemFile == "" {
		h.t.Fatalf("%s has no aimem session file", mem.name)
	}
	return st.AimemFile
}

// replayClaim sends body as mem's own `aimem reservation claim` under a new
// key, with the task's current revision: a replay of a proof, not a step.
func (h *harness) replayClaim(sc *scenario, mem *member, task taskRef, body map[string]json.RawMessage) (int, string) {
	return h.replayClaimKey(sc, mem, task, body, newKey())
}

// replayClaimKey sends body as mem's own `aimem reservation claim` under
// key, with the task's current revision.
func (h *harness) replayClaimKey(sc *scenario, mem *member, task taskRef, body map[string]json.RawMessage, key string) (int, string) {
	sc.t.Helper()
	body["expected_revision"], _ = json.Marshal(h.readTask(sc, task.ID).Revision)
	raw, _ := json.Marshal(body)
	env := append(append([]string(nil), mem.env...), "AIMEM_TEAM_SESSION="+h.sessionFile(mem))
	r := h.run(env, raw, h.aimem(), "reservation", "claim", "--task", task.ID, "--key", key)
	var env2 struct {
		Code string `json:"code"`
	}
	_ = json.Unmarshal([]byte(r.stdout), &env2)
	return r.code, env2.Code
}

// --- F1: lost aimem replies --------------------------------------------------

func reservationRoute(op string) string {
	return `^/v1/projects/[^/]+/tasks/[^/]+/reservation/` + op + `$`
}

// lostReply arms the hub proxy to drop the reply of op's mutation, runs the
// step, and asserts the driver reconciled it by its receipt.
func (h *harness) lostReply(sc *scenario, mem *member, op, stepOp, attempt, task string, body any) stepResult {
	sc.t.Helper()
	rule := h.hubProxy.arm(&faultRule{Case: "F1-reply", Action: dropReply, Method: "POST", Path: route(reservationRoute(op))})
	w := h.mark()
	res := h.committed(sc, mem, stepOp, attempt, task, body)
	calls := h.aimemCalls(w)
	sc.checkCase("F1-reply", "F1: the "+op+"'s reply was dropped after aimem committed it", rule.wasFired())
	sc.checkCase("F1-reply", "F1: aimem's CLI answered the lost "+op+" reply with exit 5, and the receipt reconciled it",
		contains(calls, "reservation "+op+" 5") && contains(calls, "reservation receipt 0"), calls)
	return res
}

// lostRequest arms the hub proxy to drop op's request before the hub sees
// it, runs the step, and returns its answer; the caller waits for the
// settle.
func (h *harness) lostRequest(sc *scenario, mem *member, op, stepOp, attempt, task string, body any) StepAnswerLite {
	sc.t.Helper()
	rule := h.hubProxy.arm(&faultRule{Case: "F1-request", Action: dropRequest, Method: "POST", Path: route(reservationRoute(op))})
	ans, _ := h.step(sc, mem, stepOp, attempt, task, body)
	var res stepResult
	_ = json.Unmarshal(ans.Result, &res)
	sc.checkCase("F1-request", "F1: the "+op+"'s request was dropped before the hub", rule.wasFired())
	sc.checkCase("F1-request", "F1: the dropped "+op+" did not commit", res.Outcome != "committed", describe(ans))
	return ans
}

func (h *harness) f1LostAimemReplies(t *testing.T) {
	sc := h.report.scenario(t, "F1")
	coord, worker := h.members["coord"], h.members["worker"]
	task := h.createTask(sc, "F1 lost replies")
	offer := func() map[string]any {
		cur := h.readTask(sc, task.ID)
		return h.offerBody(taskRef{ID: task.ID, Revision: cur.Revision}, worker, time.Now().Add(time.Hour))
	}

	// claim: the request lost, then the reply lost.
	h.lostRequest(sc, coord, "claim", "offer", "", "", offer())
	first := h.attemptOf(sc, task.ID)
	h.settledNotCommitted(sc, "the lost claim", first.ID, func(a attemptRow) bool { return a.State == "closed" })
	sc.check("aimem holds nothing after the lost claim request", holdState(h.hold(sc, task.ID)) != "held")
	id := h.lostReply(sc, coord, "claim", "offer", "", "", offer()).AttemptID
	sc.check("aimem holds the task after the lost claim reply", holdState(h.hold(sc, task.ID)) == "held")

	// transfer.
	h.lostRequest(sc, worker, "transfer", "accept", id, task.ID, map[string]string{"instruction_digest": instructionHash})
	h.settledNotCommitted(sc, "the lost transfer", id, func(a attemptRow) bool { return a.State == "offered" })
	h.lostReply(sc, worker, "transfer", "accept", id, task.ID, map[string]string{"instruction_digest": instructionHash})
	sc.check("aicrew: the attempt runs after the lost transfer reply", h.attempt(sc, id).State == "running")

	// update: a lost request cannot be decided while the hold has not
	// moved; the holder supersedes it, and that update's reply is lost.
	pr := "https://forge.example.test/e2e/pull/f1"
	ans := h.lostRequest(sc, worker, "update", "work", id, task.ID, map[string]string{"intent": "submit", "detail": pr})
	var res stepResult
	_ = json.Unmarshal(ans.Result, &res)
	sc.check("the lost update stays pending, undecided", h.attempt(sc, id).PendingKey != "", h.attempt(sc, id))
	h.lostReply(sc, worker, "update", "work", id, task.ID,
		map[string]string{"intent": "submit", "detail": pr, "supersedes": res.RequestKey})
	sc.check("aimem: the superseding update was submitted", h.readTask(sc, task.ID).State == "REVIEW")

	// finalize.
	h.local(sc, coord, "review", id, map[string]any{"result_seq": 1, "decision": "accept"})
	h.local(sc, coord, "confirm-delivery", id, map[string]any{"result_seq": 1, "evidence": deliveryEvidence})
	h.lostRequest(sc, worker, "finalize", "finalize", id, task.ID, map[string]any{"result_seq": 1})
	h.settledNotCommitted(sc, "the lost finalize", id, func(a attemptRow) bool { return a.State == "running" })
	h.lostReply(sc, worker, "finalize", "finalize", id, task.ID, map[string]any{"result_seq": 1})
	sc.check("aimem: the task is DONE after the lost finalize reply", h.readTask(sc, task.ID).State == "DONE")
	h.checkTerminalIdentity(sc, task.ID, identityRef(id, worker))

	// release, as the coordinator's withdrawal of a new offer.
	task2 := h.createTask(sc, "F1 lost release")
	id2 := h.committed(sc, coord, "offer", "", "", h.offerBody(task2, worker, time.Now().Add(time.Hour))).AttemptID
	h.lostRequest(sc, coord, "release", "withdraw", id2, task2.ID, map[string]any{})
	h.settledNotCommitted(sc, "the lost release", id2, func(a attemptRow) bool { return a.State == "offered" })
	h.lostReply(sc, coord, "release", "withdraw", id2, task2.ID, map[string]any{})
	sc.check("aimem holds nothing after the lost release reply", holdState(h.hold(sc, task2.ID)) != "held")
	sc.check("aicrew: the worker's capacity is free", h.openWorkOf(sc, worker.agentID) == 0)
}

// --- F2: lost aicrewd replies -------------------------------------------------

func (h *harness) f2LostAicrewdReplies(t *testing.T) {
	sc := h.report.scenario(t, "F2")
	coord, worker, indep := h.members["coord"], h.members["worker"], h.members["indep"]

	// A begin's reply lost: the step fails, and recover replays the begin
	// under the same Idempotency-Key, reaching the same step.
	task := h.createTask(sc, "F2 lost begin")
	rule := h.aicrewdProxy.arm(&faultRule{Case: "F2-begin", Action: dropReply, Method: "POST", Path: route(`^/v1/crew/attempts$`)})
	ans, code := h.step(sc, coord, "offer", "", "", h.offerBody(task, worker, time.Now().Add(time.Hour)))
	sc.checkCase("F2-begin", "F2: the offer's begin reply was dropped", rule.wasFired())
	sc.checkCase("F2-begin", "F2: the client saw the begin fail", code != 0 && !ans.OK, describe(ans))
	h.recoverStep(sc, coord)
	all := h.attemptsOfTask(sc, task.ID)
	sc.checkCase("F2-begin", "F2: the replayed begin reached the one attempt the lost reply began", len(all) == 1 && all[0].State == "offered", all)
	sc.check("aimem holds the task once", holdState(h.hold(sc, task.ID)) == "held")
	if len(all) == 1 {
		h.committed(sc, coord, "withdraw", all[0].ID, task.ID, map[string]any{})
	}

	// A settle's reply lost: the settle is retried, settled once.
	task2 := h.createTask(sc, "F2 lost settle")
	rule = h.aicrewdProxy.arm(&faultRule{Case: "F2-settle", Action: dropReply, Method: "POST", Path: route(`^/v1/crew/attempts/[^/]+/settle$`)})
	ans, code = h.step(sc, indep, "claim", "", "", h.claimBody(task2))
	sc.checkCase("F2-settle", "F2: the claim's settle reply was dropped", rule.wasFired())
	sc.checkCase("F2-settle", "F2: the client saw the settle fail", code != 0 && !ans.OK, describe(ans))
	h.recoverStep(sc, indep)
	a := h.attemptOf(sc, task2.ID)
	sc.check("F2: the retried settle left the claim running, settled once", a.State == "running" && a.PendingKey == "", a)
	pending, _ := h.step(sc, indep, "pending", "", "", nil)
	sc.check("F2: no step is left recorded", string(pending.Result) == "null", string(pending.Result))
	h.local(sc, coord, "stop", a.ID, map[string]string{"reason": "F2 done"})
	h.local(sc, indep, "confirm-stop", a.ID, map[string]any{})
	h.committed(sc, indep, "release", a.ID, task2.ID, map[string]string{"target": "READY"})
}

// --- F5: stale steps ---------------------------------------------------------------

func (h *harness) f5StaleSteps(t *testing.T) {
	sc := h.report.scenario(t, "F5")
	coord, worker := h.members["coord"], h.members["worker"]

	// A held task cannot go stale under its holder: aimem refuses a plain
	// edit of it (an observed contract, so the driver's own stale_revision
	// check guards only against the member's other clients). The real stale
	// step is one begun at a revision the task leaves before the send: the
	// coordinator's offer, its claim paused, while the admin edits the
	// still unheld task. aimem refuses the claim, aicrew settles the offer
	// not committed, and the offer begun again at the new revision commits.
	task := h.createTask(sc, "F5 stale revision")
	held := h.createTask(sc, "F5 held task")
	hid := h.committed(sc, coord, "offer", "", "", h.offerBody(held, worker, time.Now().Add(time.Hour))).AttemptID
	sc.check("aimem refuses a plain edit of a held task", h.tryEditNextAction(sc, held.ID, "edited while held") == 409)
	h.committed(sc, coord, "withdraw", hid, held.ID, map[string]any{})

	paused := h.holdNext("F5-stale", gateName("reservation", "claim"))
	t.Cleanup(func() { h.release(gateName("reservation", "claim")) })
	type out struct {
		ans  StepAnswerLite
		code int
	}
	done := make(chan out, 1)
	w := h.mark()
	go func() {
		a, c := h.step(sc, coord, "offer", "", "", h.offerBody(task, worker, time.Now().Add(time.Hour)))
		done <- out{a, c}
	}()
	if paused {
		h.waitPaused(gateName("reservation", "claim"))
		sc.require("the admin edits the unheld task mid-step", h.tryEditNextAction(sc, task.ID, "edited mid-step") == 200)
		h.release(gateName("reservation", "claim"))
	}
	first := <-done
	calls := h.aimemCalls(w)
	sc.checkCase("F5-stale", "F5: aimem refused the claim begun at the old revision", contains(calls, "reservation claim 3"), calls, describe(first.ans))
	stale := h.attemptOf(sc, task.ID)
	h.settledNotCommitted(sc, "the stale offer", stale.ID, func(a attemptRow) bool { return a.State == "closed" })
	cur := h.readTask(sc, task.ID)
	again := h.committed(sc, coord, "offer", "", "", h.offerBody(taskRef{ID: task.ID, Revision: cur.Revision}, worker,
		time.Now().Add(time.Hour))).AttemptID
	sc.check("F5: the offer begun again at the new revision holds the task", holdState(h.hold(sc, task.ID)) == "held")
	h.committed(sc, coord, "withdraw", again, task.ID, map[string]any{})

	// A proof replayed after its step settled, under a new key: aimem asks
	// aicrewd, which answers the fact inactive.
	task2 := h.createTask(sc, "F5 replayed proof")
	h.captureNext(gateName("reservation", "claim"))
	id2 := h.committed(sc, coord, "offer", "", "", h.offerBody(task2, worker, time.Now().Add(time.Hour))).AttemptID
	body := h.captured(sc, gateName("reservation", "claim"))
	h.committed(sc, coord, "withdraw", id2, task2.ID, map[string]any{})
	if !skipFault("F5-replay") {
		code, refusal := h.replayClaim(sc, coord, task2, body)
		sc.checkCase("F5-replay", "F5: aimem refuses a settled step's proof replayed under a new key", code == 3 && refusal != "", code, refusal)
	} else {
		// The active-proof control: the same rejection check, on a live
		// proof replayed under its own step's key, which aimem accepts.
		code, refusal := h.activeProofControl(sc)
		sc.checkCase("F5-replay", "F5: aimem refuses a settled step's proof replayed under a new key", code == 3 && refusal != "",
			code, refusal)
	}
	sc.check("aimem holds nothing after the replay", holdState(h.hold(sc, task2.ID)) != "held")

	// A member that resumes to a new generation: its old proof is inactive.
	task3 := h.createTask(sc, "F5 resumed member")
	h.captureNext(gateName("reservation", "claim"))
	resumed := h.holdNext("F5-resume", gateName("reservation", "claim"))
	// The launcher dies under this step: its answer is the channel's EOF.
	offer3, _ := json.Marshal(h.offerBody(task3, worker, time.Now().Add(time.Hour)))
	offer3Done := make(chan struct{})
	go func() {
		h.run(coord.env, offer3, filepath.Join(h.bin, "aicrew-agent"), "step", "offer", "-home", coord.home, "-body", "-")
		close(offer3Done)
	}()
	if resumed {
		h.waitPaused(gateName("reservation", "claim"))
		h.crashLauncher(coord)
		body3 := h.captured(sc, gateName("reservation", "claim"))
		h.restartLauncher(coord)
		code, refusal := h.replayClaim(sc, coord, task3, body3)
		sc.checkCase("F5-resume", "F5: the old generation's proof is refused after the member resumed", code == 3 && refusal != "", code, refusal)
		// The offer is the old generation's: the resumed coordinator may not
		// continue it, and aicrewd settles the never-sent offer itself.
		rec := h.recoverStep(sc, coord)
		sc.check("F5: the new generation may not continue the old generation's offer", !rec.OK, describe(rec))
		sc.check("aimem holds nothing for the old generation's offer", holdState(h.hold(sc, task3.ID)) != "held")
		// The resume ended the old generation's proof (01a0f758-c827), so
		// aicrewd's reconciler settles the never-sent offer not committed
		// once the grace after that end has passed, well before the proof
		// would have expired, and the worker's capacity is free again.
		a := h.attemptOf(sc, task3.ID)
		settled := h.waitAttempt(sc, a.ID, 45*time.Second, func(r attemptRow) bool { return r.PendingKey == "" && r.State == "closed" })
		got := h.attempt(sc, a.ID)
		sc.check("F5: aicrewd settles the resumed coordinator's never-sent offer not committed within 45 s",
			settled && got.CloseReason == "claim not_committed", got)
		sc.check("aicrew: the worker's capacity is free after the resumed coordinator's offer", h.openWorkOf(sc, worker.agentID) == 0)
	} else {
		// The active-proof control: the member did not resume, so its
		// proof is live under its own generation and step key. The offer
		// above ran through unpaused first.
		<-offer3Done
		code, refusal := h.activeProofControl(sc)
		sc.checkCase("F5-resume", "F5: the old generation's proof is refused after the member resumed", code == 3 && refusal != "",
			code, refusal)
	}

	// aicrewd's coordination answer delayed past aimem's 2 s call budget:
	// aimem refuses it as retryable, and the driver's retry commits.
	// The independent member's claim, whose capacity the resumed offer above
	// does not hold.
	indep := h.members["indep"]
	task4 := h.createTask(sc, "F5 slow coordination")
	rule := h.aicrewdProxy.arm(&faultRule{Case: "F5-delay", Action: delayReply, Method: "POST",
		Path: route(`^/v1/crew/coordination$`), Delay: 3 * time.Second})
	w = h.mark()
	id4 := h.committed(sc, indep, "claim", "", "", h.claimBody(task4)).AttemptID
	calls = h.aimemCalls(w)
	sc.checkCase("F5-delay", "F5: aicrewd's coordination answer was delayed past aimem's budget", rule.wasFired())
	sc.checkCase("F5-delay", "F5: aimem refused the slow claim as retryable, and the retry committed",
		contains(calls, "reservation claim 4") && contains(calls, "reservation claim 0"), calls)
	h.local(sc, coord, "stop", id4, map[string]string{"reason": "F5 done"})
	h.local(sc, indep, "confirm-stop", id4, map[string]any{})
	h.committed(sc, indep, "release", id4, task4.ID, map[string]string{"target": "READY"})
}

// tryEditNextAction is an admin edit that would move the task's revision
// and not its state; it returns the hub's status.
func (h *harness) tryEditNextAction(sc *scenario, id, next string) int {
	sc.t.Helper()
	cur := h.readTask(sc, id)
	var body map[string]any
	_ = json.Unmarshal(cur.raw, &body)
	content := map[string]any{"expected_revision": cur.Revision}
	for _, f := range []string{"title", "objective", "acceptance_criteria", "non_goals", "state", "blocker", "dependencies",
		"candidate_refs", "evidence_refs", "archived"} {
		content[f] = body[f]
	}
	content["next_action"] = next
	return h.hubJSON("PUT", "/v1/tasks/"+id, map[string]string{"Idempotency-Key": newKey()}, content, nil)
}

// crashLauncher kills mem's launcher and everything it started, as a crash.
func (h *harness) crashLauncher(mem *member) {
	p := mem.launcher
	_ = killGroup(p)
	<-p.done
	p.done <- nil
}

// restartLauncher starts mem's launcher again: it resumes the recorded
// session, under a new generation.
func (h *harness) restartLauncher(mem *member) {
	// The client is a stand-in, not Claude Code: no first instruction (-no-start).
	mem.launcher = h.start("launcher-"+mem.name, mem.env, mem.home, filepath.Join(h.bin, "aicrew-agent"),
		"run", "-client", "claude", "-home", mem.home, "-no-start", "--", "86400")
	h.waitFor(mem.name+"'s step channel after the restart", 60*time.Second, func() bool {
		r := h.run(mem.env, nil, filepath.Join(h.bin, "aicrew-agent"), "step", "pending", "-home", mem.home)
		return r.code == 0
	})
}
