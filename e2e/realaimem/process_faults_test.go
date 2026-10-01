//go:build realaimem

package realaimem

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// The process, competition, recovery and secrets faults (crew-execution
// b4b-2): F3, restarts; F4, competing steps; F6, recovery through the read
// scope; F7, secrets. Every fault belongs to a skip-the-fault case, and each
// case's control reaches the same assertion as the fault and fails it.

// --- shared helpers ---------------------------------------------------------

// stepRaw runs `aicrew-agent step` without judging its answer: for a step
// whose launcher, aicrewd or hub the scenario disturbs on purpose.
func (h *harness) stepRaw(mem *member, op, attempt, task string, body any) (StepAnswerLite, int) {
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
	r := h.run(mem.env, stdin, args...)
	var ans StepAnswerLite
	_ = json.Unmarshal([]byte(r.stdout), &ans)
	return ans, r.code
}

// refusedAtBegin reports whether a step's answer is aicrewd's refusal at
// begin with one of codes: a refused status and its error code.
func refusedAtBegin(ans StepAnswerLite, codes ...string) bool {
	return ans.Status == "refused" && !ans.OK && ans.Error != nil && contains(codes, ans.Error.Code)
}

// aimemConflictCodes are aimem's own refusals of a mutation that lost to
// another (internal/server/reservations.go at the pin). The driver's
// not_committed, made from a receipt after a lost reply, is not one: it
// proves no commit, not a conflict.
var aimemConflictCodes = []string{"reservation_conflict", "revision_conflict", "stale_fence"}

// refusedByAimem reports whether a step's answer is aimem's conflict refusal
// of the mutation: the report refused with one of aimem's conflict codes,
// and not committed.
func refusedByAimem(ans StepAnswerLite) bool {
	var sr stepResult
	if json.Unmarshal(ans.Result, &sr) != nil {
		return false
	}
	return (ans.Status == "refused" || ans.Status == "pending") && sr.Report.Outcome == "refused" &&
		contains(aimemConflictCodes, sr.Report.Code) && sr.Outcome != "committed"
}

// pendingStepKey is the request key of mem's recorded step on task.
func (h *harness) pendingStepKey(sc *scenario, mem *member, task string) string {
	sc.t.Helper()
	files, _ := filepath.Glob(filepath.Join(mem.home, "state", "steps", "step-*.json"))
	for _, f := range files {
		raw, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		var rec struct {
			Request struct {
				TaskID string `json:"task_id"`
			} `json:"request"`
			Step *struct {
				RequestKey string `json:"request_key"`
			} `json:"step"`
		}
		if json.Unmarshal(raw, &rec) == nil && rec.Request.TaskID == task && rec.Step != nil {
			return rec.Step.RequestKey
		}
	}
	sc.t.Fatalf("%s has no recorded step on task %s", mem.name, task)
	return ""
}

// activeProofControl replays a live proof under its own step's key: the
// independent member's claim, paused before it is sent. aimem accepts it,
// so the rejection check F5 applies to an inactive proof fails on it
// (01a0f766-3c36). The claim is then let through and released.
func (h *harness) activeProofControl(sc *scenario) (int, string) {
	sc.t.Helper()
	indep, coord := h.members["indep"], h.members["coord"]
	task := h.createTask(sc, "F5 active-proof control")
	name := gateName("reservation", "claim")
	h.captureNext(name)
	h.holdNext("", name)
	done := make(chan struct{})
	go func() {
		h.stepRaw(indep, "claim", "", "", h.claimBody(task))
		close(done)
	}()
	h.waitPaused(name)
	body := h.captured(sc, name)
	code, refusal := h.replayClaimKey(sc, indep, task, body, h.pendingStepKey(sc, indep, task.ID))
	h.release(name)
	<-done
	if a := h.attemptsOfTask(sc, task.ID); len(a) == 1 && a[0].State == "running" {
		h.local(sc, coord, "stop", a[0].ID, map[string]string{"reason": "control done"})
		h.local(sc, indep, "confirm-stop", a[0].ID, map[string]any{})
		h.committed(sc, indep, "release", a[0].ID, task.ID, map[string]string{"target": "READY"})
	}
	return code, refusal
}

// settledByReconciler reports whether aicrewd's reconciler settled a step of
// the attempt: its audit names the reconciler as the settle's caller.
func (h *harness) settledByReconciler(sc *scenario, attempt string) bool {
	sc.t.Helper()
	db := h.aicrewDB(sc)
	defer db.Close()
	var n int
	if err := db.QueryRow(`SELECT count(*) FROM audit WHERE operation = 'attempt.settle' AND caller_kind = 'reconciler'
		AND scope = ?`, attempt).Scan(&n); err != nil {
		sc.t.Fatal(err)
	}
	return n > 0
}

// killAicrewd kills aicrewd outright and starts it again on its store.
func (h *harness) killAicrewd() int {
	p := h.aicrewdProc
	_ = syscall.Kill(p.cmd.Process.Pid, syscall.SIGKILL)
	<-p.done
	p.done <- nil
	h.aicrewdProc = h.start("aicrewd", p.cmd.Env, p.cmd.Dir, p.cmd.Args...)
	h.aicrewdLog = h.aicrewdProc.log
	h.waitFor("aicrewd's listener after the kill", 30*time.Second, func() bool {
		resp, err := h.httpClient.Get(h.aicrewdURL + "/healthz")
		if err != nil {
			return false
		}
		resp.Body.Close()
		return resp.StatusCode == http.StatusOK
	})
	return h.aicrewdProc.cmd.Process.Pid
}

// restartHub stops the hub (killed outright, or stopped) and starts it
// again on its state.
func (h *harness) stopHub(kill bool) {
	p := h.hubProc
	if kill {
		_ = killGroup(p)
		<-p.done
		p.done <- nil
	} else {
		_ = p.stop(30 * time.Second)
	}
}

func (h *harness) startHub() int {
	p := h.hubProc
	h.hubProc = h.start("aimem-hub", p.cmd.Env, p.cmd.Dir, p.cmd.Args...)
	h.waitFor("the hub after its restart", 60*time.Second, func() bool {
		resp, err := h.httpClient.Get(h.hubURL + "/v1/status")
		if err != nil {
			return false
		}
		resp.Body.Close()
		return resp.StatusCode == http.StatusOK
	})
	return h.hubProc.cmd.Process.Pid
}

// waitAttempt waits for cond on the attempt, up to limit, and reports it.
func (h *harness) waitAttempt(sc *scenario, id string, limit time.Duration, cond func(attemptRow) bool) bool {
	deadline := time.Now().Add(limit)
	for {
		if cond(h.attempt(sc, id)) {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(time.Second)
	}
}

// running starts a running offer of a new task to the worker.
func (h *harness) running(sc *scenario, title string) (taskRef, string) {
	sc.t.Helper()
	coord, worker := h.members["coord"], h.members["worker"]
	task := h.createTask(sc, title)
	id := h.committed(sc, coord, "offer", "", "", h.offerBody(task, worker, time.Now().Add(time.Hour))).AttemptID
	h.committed(sc, worker, "accept", id, task.ID, map[string]string{"instruction_digest": instructionHash})
	return task, id
}

// stopAndRelease ends a running attempt through the stop protocol.
func (h *harness) stopAndRelease(sc *scenario, holder *member, id, task string) {
	sc.t.Helper()
	h.local(sc, h.members["coord"], "stop", id, map[string]string{"reason": "scenario done"})
	h.local(sc, holder, "confirm-stop", id, map[string]any{})
	h.committed(sc, holder, "release", id, task, map[string]string{"target": "READY"})
}

// --- F3: restarts --------------------------------------------------------------

func (h *harness) f3Restarts(t *testing.T) {
	sc := h.report.scenario(t, "F3")
	coord, worker, indep := h.members["coord"], h.members["worker"], h.members["indep"]

	// The member's launcher is killed after aimem committed its claim and
	// before the settle: aicrewd's reconciler settles alone.
	task := h.createTask(sc, "F3 launcher killed")
	name := "after-" + gateName("reservation", "claim")
	held := h.holdNext("F3-launcher", name)
	done := make(chan struct{})
	go func() {
		h.stepRaw(indep, "claim", "", "", h.claimBody(task))
		close(done)
	}()
	if held {
		h.waitPaused(name)
		h.crashLauncher(indep)
	}
	<-done
	a := h.attemptOf(sc, task.ID)
	settled := h.waitAttempt(sc, a.ID, 45*time.Second, func(a attemptRow) bool { return a.State == "running" && a.PendingKey == "" })
	sc.checkCase("F3-launcher", "F3: aicrewd's reconciler settled the claim alone, within 45 s, with the member's launcher down",
		settled && h.settledByReconciler(sc, a.ID), h.attempt(sc, a.ID))
	if held {
		h.restartLauncher(indep)
		h.recoverStep(sc, indep)
	}
	pending, _ := h.step(sc, indep, "pending", "", "", nil)
	sc.check("F3: the restarted launcher has no recorded step left", string(pending.Result) == "null", string(pending.Result))
	h.stopAndRelease(sc, indep, a.ID, task.ID)

	// aicrewd killed outright between begin and send: the step commits and
	// settles against the restarted aicrewd.
	task2 := h.createTask(sc, "F3 aicrewd killed")
	name = gateName("reservation", "claim")
	before := h.aicrewdProc.cmd.Process.Pid
	held = h.holdNext("F3-aicrewd", name)
	type out struct {
		ans  StepAnswerLite
		code int
	}
	res := make(chan out, 1)
	go func() {
		ans, code := h.stepRaw(coord, "offer", "", "", h.offerBody(task2, worker, time.Now().Add(time.Hour)))
		res <- out{ans, code}
	}()
	if held {
		h.waitPaused(name)
		h.killAicrewd()
		h.release(name)
	}
	r := <-res
	var sr stepResult
	_ = json.Unmarshal(r.ans.Result, &sr)
	after := h.aicrewdProc.cmd.Process.Pid
	sc.checkCase("F3-aicrewd", "F3: the offer committed and settled against the restarted aicrewd",
		after != before && sr.Settled && sr.Outcome == "committed", before, after, describe(r.ans))
	id2 := h.attemptOf(sc, task2.ID).ID

	// The hub killed outright between steps: its holds persist, and the
	// next step works.
	holdBefore := h.hold(sc, task2.ID)
	hubBefore := h.hubProc.cmd.Process.Pid
	hubAfter := hubBefore
	if !skipFault("F3-hub") {
		h.report.write(map[string]any{"type": "fault", "case": "F3-hub", "action": "kill_hub"})
		h.stopHub(true)
		hubAfter = h.startHub()
	} else {
		h.report.write(map[string]any{"type": "fault", "case": "F3-hub", "skipped": true})
	}
	holdAfter := h.hold(sc, task2.ID)
	sc.checkCase("F3-hub", "F3: the hub restarted with its hold intact",
		hubAfter != hubBefore && holdState(holdAfter) == "held" && fmt.Sprint(holdAfter) == fmt.Sprint(holdBefore), holdBefore, holdAfter)
	h.committed(sc, coord, "withdraw", id2, task2.ID, map[string]any{})
	sc.check("F3: the step after the hub's restart released the hold", holdState(h.hold(sc, task2.ID)) != "held")
}

// --- F4: competing steps -----------------------------------------------------------

func (h *harness) f4CompetingSteps(t *testing.T) {
	sc := h.report.scenario(t, "F4")
	coord, worker, indep := h.members["coord"], h.members["worker"], h.members["indep"]

	// A claim of a task with an open attempt is refused.
	task := h.createTask(sc, "F4 second step")
	id := h.committed(sc, coord, "offer", "", "", h.offerBody(task, worker, time.Now().Add(time.Hour))).AttemptID
	target := task
	if skipFault("F4-second") {
		target = h.createTask(sc, "F4 second step, control")
	}
	ans, code := h.step(sc, indep, "claim", "", "", h.claimBody(target))
	sc.checkCase("F4-second", "F4: a claim of a task with an open attempt is refused task_busy at begin",
		code == 3 && refusedAtBegin(ans, "task_busy") && len(h.attemptsOfTask(sc, task.ID)) == 1, describe(ans))
	if target != task {
		if a := h.attemptsOfTask(sc, target.ID); len(a) == 1 && a[0].State == "running" {
			h.stopAndRelease(sc, indep, a[0].ID, target.ID)
		}
	}

	// A busy worker is refused.
	task2 := h.createTask(sc, "F4 busy worker")
	if skipFault("F4-busy") {
		h.committed(sc, coord, "withdraw", id, task.ID, map[string]any{})
	}
	ans, code = h.step(sc, coord, "offer", "", "", h.offerBody(task2, worker, time.Now().Add(time.Hour)))
	sc.checkCase("F4-busy", "F4: an offer to a busy worker is refused agent_busy at begin", code == 3 && refusedAtBegin(ans, "agent_busy"),
		describe(ans))
	for _, tk := range []taskRef{task, task2} {
		for _, a := range h.attemptsOfTask(sc, tk.ID) {
			if a.State == "offered" {
				h.committed(sc, coord, "withdraw", a.ID, tk.ID, map[string]any{})
			}
		}
	}

	// An offer and an independent claim race for one task.
	race := h.createTask(sc, "F4 race")
	other := race
	if skipFault("F4-race") {
		other = h.createTask(sc, "F4 race, control")
	}
	type out struct {
		ans  StepAnswerLite
		code int
	}
	offerRes, claimRes := make(chan out, 1), make(chan out, 1)
	go func() {
		a, c := h.stepRaw(coord, "offer", "", "", h.offerBody(race, worker, time.Now().Add(time.Hour)))
		offerRes <- out{a, c}
	}()
	go func() {
		a, c := h.stepRaw(indep, "claim", "", "", h.claimBody(other))
		claimRes <- out{a, c}
	}()
	o, c := <-offerRes, <-claimRes
	committed := func(r out) bool {
		var sr stepResult
		_ = json.Unmarshal(r.ans.Result, &sr)
		return r.code == 0 && sr.Settled && sr.Outcome == "committed"
	}
	// A step lost before commit settles as the driver's not_committed: no
	// commit, and no conflict either.
	lost := StepAnswerLite{Status: "refused",
		Result: json.RawMessage(`{"report":{"outcome":"refused","code":"not_committed"},"settled":true,"outcome":"not_committed"}`)}
	raceCase := "F4-race"
	if skipFault("F4-lost") {
		// The control: the loser's answer becomes a lost step's, and the
		// race's own assertion must refuse it.
		raceCase = "F4-lost"
		if !committed(o) {
			o.ans, o.code = lost, 3
		} else {
			c.ans, c.code = lost, 3
		}
	}
	wins := 0
	var loser *out
	for _, r := range []out{o, c} {
		if committed(r) {
			wins++
		} else {
			r := r
			loser = &r
		}
	}
	// The loser must be a conflict refusal: aicrew's at begin, or aimem's
	// at the claim. A failure, a non-answer or a lost step is none of them.
	loserRefused := loser != nil && (refusedAtBegin(loser.ans, "task_busy", "agent_busy") || refusedByAimem(loser.ans))
	sc.check("F4: a step lost before commit, or a failure, is not taken for a conflict refusal",
		!refusedByAimem(lost) && !refusedAtBegin(lost, "task_busy", "agent_busy") && !refusedByAimem(StepAnswerLite{Status: "failed"}))
	open := 0
	for _, a := range h.attemptsOfTask(sc, race.ID) {
		if a.State != "closed" {
			open++
		}
	}
	sc.checkCase(raceCase, "F4: of an offer and a claim racing for one task, exactly one commits, the other is refused, one attempt is open",
		wins == 1 && loserRefused && open == 1 && holdState(h.hold(sc, race.ID)) == "held", describe(o.ans), describe(c.ans))
	for _, tk := range []taskRef{race, other} {
		for _, a := range h.attemptsOfTask(sc, tk.ID) {
			switch {
			case a.State == "offered":
				h.committed(sc, coord, "withdraw", a.ID, tk.ID, map[string]any{})
			case a.State == "running":
				h.stopAndRelease(sc, indep, a.ID, tk.ID)
			}
		}
		if tk == other {
			break
		}
	}
}

// --- F6: recovery through the read scope ---------------------------------------------

// recoverHold has the hub's admin recover the task's hold: release (to
// READY) or cancel (to CANCELLED), with an attestation.
func (h *harness) recoverHold(sc *scenario, task, verb string) {
	sc.t.Helper()
	hold := h.hold(sc, task)
	cur := h.readTask(sc, task)
	var content map[string]any
	_ = json.Unmarshal(cur.raw, &content)
	tc := map[string]any{}
	for _, f := range []string{"title", "objective", "acceptance_criteria", "non_goals", "blocker", "dependencies",
		"candidate_refs", "evidence_refs", "next_action", "archived"} {
		tc[f] = content[f]
	}
	tc["state"] = map[string]string{"release": "READY", "cancel": "CANCELLED"}[verb]
	body := map[string]any{"reservation_id": hold["reservation_id"], "fence": hold["fence"], "expected_revision": cur.Revision,
		"content": tc, "reason": "e2e recovery " + verb,
		"evidence": map[string]any{"kind": "attestation", "attestation_id": "e2e-" + newKey(),
			"statement": "The end-to-end harness recovers this hold to test aicrew's reconciliation."}}
	raw, _ := json.Marshal(body)
	r := h.run(h.hostEnv(), raw, h.aimem(), "reservation", "recover", verb, "--task", task, "--key", newKey(),
		"--hub", h.hubURL, "--admin-token-file", h.adminFile, "--hub-ca-file", h.caFile)
	sc.require("aimem's admin recovers the hold ("+verb+")", r.code == 0, r.stdout, r.stderr)
}

// recovered reports an attempt's recovered closure from aicrew's store.
func (h *harness) recovered(sc *scenario, id string) (reason, by, fence string) {
	sc.t.Helper()
	db := h.aicrewDB(sc)
	defer db.Close()
	if err := db.QueryRow(`SELECT close_reason, recovered_by, recovered_fence FROM attempts WHERE id = ?`, id).
		Scan(&reason, &by, &fence); err != nil {
		sc.t.Fatal(err)
	}
	return reason, by, fence
}

func (h *harness) f6Recovery(t *testing.T) {
	sc := h.report.scenario(t, "F6")
	worker := h.members["worker"]

	for _, verb := range []string{"release", "cancel"} {
		c := "F6-" + verb
		task, id := h.running(sc, "F6 recover "+verb)
		before := h.attempt(sc, id)
		if !skipFault(c) {
			h.report.write(map[string]any{"type": "fault", "case": c, "action": "recover_" + verb})
			h.recoverHold(sc, task.ID, verb)
		} else {
			h.report.write(map[string]any{"type": "fault", "case": c, "skipped": true})
		}
		closed := h.waitAttempt(sc, id, 60*time.Second, func(a attemptRow) bool { return a.State == "closed" })
		reason, by, fence := h.recovered(sc, id)
		_ = before
		sc.checkCase(c, "F6: aicrewd closed the attempt as recovered after aimem's recover "+verb,
			closed && reason == "recovered" && by == "recovery_"+verb && fence != "", reason, by, fence)
		if !closed {
			h.stopAndRelease(sc, worker, id, task.ID)
		}
	}

	// A held hold is never closed: across 4 ticks the running attempt stays.
	task, id := h.running(sc, "F6 held")
	stayed := !h.waitAttempt(sc, id, 65*time.Second, func(a attemptRow) bool { return a.State == "closed" })
	sc.check("F6: a held hold is not closed across 4 ticks", stayed, h.attempt(sc, id))

	// With the hub unreachable for aicrewd, nothing closes; after its
	// restart, the recovered attempt is closed. aicrewd's reads are held
	// back from before the recovery, so the order is deterministic.
	c := "F6-unreachable"
	var block *faultRule
	if !skipFault(c) {
		block = h.hubProxy.arm(&faultRule{Case: c, Action: dropRequest, Method: "GET", Path: route(`^/v1/identity/peers/`), Persist: true})
	} else {
		h.report.write(map[string]any{"type": "fault", "case": c, "skipped": true})
	}
	h.recoverHold(sc, task.ID, "release")
	if block != nil {
		h.stopHub(false)
	}
	stayedOpen := !h.waitAttempt(sc, id, 65*time.Second, func(a attemptRow) bool { return a.State == "closed" })
	sc.checkCase(c, "F6: nothing closes while the hub is unreachable for aicrewd", stayedOpen, h.attempt(sc, id))
	if block != nil {
		h.startHub()
		h.hubProxy.disarm(block)
	}
	closed := h.waitAttempt(sc, id, 60*time.Second, func(a attemptRow) bool { return a.State == "closed" })
	reason, by, _ := h.recovered(sc, id)
	sc.check("F6: once the hub is back, the attempt is closed as recovered", closed && reason == "recovered" && by == "recovery_release",
		reason, by)
}

// --- F7: secrets --------------------------------------------------------------------------

func (h *harness) f7Secrets(t *testing.T) {
	sc := h.report.scenario(t, "F7")
	// The secrets the run made, besides those recorded as they were made
	// (every captured proof included, as it was read): any capture still
	// on disk that no scenario read, and the members' aimem session handles.
	captured, _ := filepath.Glob(filepath.Join(h.gate, "captured-*"))
	for _, f := range captured {
		var body map[string]any
		if raw, err := os.ReadFile(f); err == nil && json.Unmarshal(raw, &body) == nil {
			if p, ok := body["coordination_proof"].(string); ok && p != "" {
				h.knowSecret(p)
			}
		}
	}
	for _, m := range h.members {
		files, _ := filepath.Glob(filepath.Join(m.dir, "aimem", "aicrew-sessions", "*.json"))
		for _, f := range files {
			var sf map[string]any
			if raw, err := os.ReadFile(f); err == nil && json.Unmarshal(raw, &sf) == nil {
				if hd, ok := sf["handle"].(string); ok {
					h.knowSecret(hd)
				}
			}
		}
	}
	h.outMu.Lock()
	secrets := append([]string(nil), h.secrets...)
	outputs := append([]string(nil), h.outputs...)
	proofs := append([]string(nil), h.proofs...)
	h.outMu.Unlock()
	sc.require("F7: the run knows its secrets", len(secrets) >= 8, len(secrets))
	// Every captured proof is among the secrets scanned for, the earliest
	// as well as the last; the scan finds the earliest one where it lies.
	known := 0
	for _, p := range proofs {
		if contains(secrets, p) {
			known++
		}
	}
	sc.check(fmt.Sprintf("F7: all %d proofs the run captured are among the secrets scanned for", len(proofs)), known == len(proofs))
	if len(proofs) > 0 {
		sc.check("F7: the scan finds the run's earliest captured proof", len(leaksIn(map[string]string{"probe": "x " + proofs[0]}, secrets)) == 1)
	}

	if skipFault("F7-leak") {
		// The control: a secret planted in a scanned log must be found.
		h.report.write(map[string]any{"type": "fault", "case": "F7-leak", "action": "plant"})
		os.WriteFile(filepath.Join(h.root, "logs", "planted.log"), []byte("planted "+secrets[0]+"\n"), 0o600)
	}
	sources := map[string]string{"command outputs": strings.Join(outputs, "\n")}
	logs, _ := filepath.Glob(filepath.Join(h.root, "logs", "*.log"))
	for _, f := range append(logs, h.timingLog, h.report.path) {
		b, _ := os.ReadFile(f)
		sources[filepath.Base(f)] = string(b)
	}
	db := h.aicrewDB(sc)
	rows, err := db.Query(`SELECT input, result FROM audit`)
	if err != nil {
		sc.t.Fatal(err)
	}
	var audit strings.Builder
	for rows.Next() {
		var in, res string
		_ = rows.Scan(&in, &res)
		audit.WriteString(in + "\n" + res + "\n")
	}
	rows.Close()
	db.Close()
	sources["aicrew audit"] = audit.String()

	leaks := leaksIn(sources, secrets)
	sc.checkCase("F7-leak", fmt.Sprintf("F7: none of the run's %d secrets appears in %d scanned sources", len(secrets), len(sources)),
		len(leaks) == 0, leaks)
}

// leaksIn names each secret found in each source.
func leaksIn(sources map[string]string, secrets []string) []string {
	var leaks []string
	for name, text := range sources {
		for i, s := range secrets {
			if strings.Contains(text, s) {
				leaks = append(leaks, fmt.Sprintf("secret #%d in %s", i, name))
			}
		}
	}
	return leaks
}
