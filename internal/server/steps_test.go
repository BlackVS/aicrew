package server

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/BlackVS/aicrew/internal/store"
)

// The fake aimem's read scope: what it committed under each proof.

func (f *fakeAimem) ReceiptByProof(_ context.Context, digest string) (store.ScopeReceiptLookup, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if r, ok := f.receipts[digest]; ok {
		return r, nil
	}
	return store.ScopeReceiptLookup{State: store.ScopeNone}, nil
}

func (f *fakeAimem) ReceiptByKey(context.Context, store.TaskRef, store.ReservationOp, string) (store.ScopeReceiptLookup, error) {
	return store.ScopeReceiptLookup{State: store.ScopeNone}, nil
}

func (f *fakeAimem) HoldStatus(context.Context, store.TaskRef) (store.ScopeHold, error) {
	return store.ScopeHold{State: store.ScopeNone}, nil
}

// send is the acting member's own aimem connection sending the step it
// began: aimem verifies the proof through aicrew's coordination route, then
// commits and keeps the receipt in its read scope. It returns aimem's refusal
// code, or "".
func (f *fakeAimem) send(t *testing.T, task store.TaskRef, st store.Step) string {
	t.Helper()
	res, err := f.Mutate(context.Background(), st.Operation, store.ReservationRequest{Task: task, RequestKey: st.RequestKey,
		ExpectedRevision: st.ExpectedRevision, ReservationID: st.ReservationID, Fence: st.Fence, Holder: st.Holder,
		CoordinationProof: st.CoordinationProof, TerminalEvidence: st.TerminalEvidence})
	var refusal *store.ReservationRefusal
	if errors.As(err, &refusal) {
		return refusal.Code
	} else if err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.receipts[p1(st.CoordinationProof)] = store.ScopeReceiptLookup{State: store.ScopeCommitted, Receipt: &store.ScopeReceipt{
		ID: res.Receipt.ID, Operation: string(st.Operation), TaskID: task.TaskID, RequestKeyDigest: k1(st.RequestKey),
		ReservationID: f.holds[task.TaskID].id, Fence: res.Reservation.Fence, TaskRevision: res.TaskRevision,
		MemberUserID: "user-member", VerifiedMode: "team", CommittedAt: time.Now().UTC().Format(time.RFC3339)}}
	return ""
}

// call sends one step request as a member's client. A nil body sends none.
func (e *coordEnv) call(t *testing.T, token, path, key string, body any) reply {
	t.Helper()
	var raw []byte
	if body != nil {
		raw, _ = json.Marshal(body)
	}
	req, err := http.NewRequest(http.MethodPost, e.url(path), bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if key != "" {
		req.Header.Set("Idempotency-Key", key)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := e.client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	out := reply{status: resp.StatusCode, header: resp.Header, raw: string(b)}
	_ = json.Unmarshal(b, &out.body)
	return out
}

func (e *coordEnv) offerBody(taskID string, expires time.Time) map[string]any {
	return map[string]any{"worker_agent_id": e.worker.agent.ID, "task": e.task(taskID), "expected_revision": 3,
		"base_commit": "base-1", "branch": "work/" + taskID,
		"process": map[string]string{"repo": coordPin.Identity.Repository, "commit": coordPin.Identity.Commit,
			"manifest": coordPin.Identity.Manifest},
		"instruction_digest": coordPin.InstructionDigest, "expires_at": expires.UTC().Format(time.RFC3339Nano)}
}

// stepOf decodes a begin reply strictly as coordination.v1's begin response.
func stepOf(t *testing.T, got reply) store.Step {
	t.Helper()
	if got.status != http.StatusOK {
		t.Fatalf("begin: %d %s", got.status, got.raw)
	}
	var st store.Step
	if err := strictDecode([]byte(got.raw), &st); err != nil || st.CoordinationProof == "" || st.RequestKey == "" {
		t.Fatalf("begin reply %s: %v", got.raw, err)
	}
	return st
}

func attemptOf(t *testing.T, got reply) string {
	t.Helper()
	id, ok := strings.CutPrefix(got.header.Get("Location"), AttemptsPath+"/")
	if !ok || !attemptIDShape.MatchString(id) {
		t.Fatalf("Location %q", got.header.Get("Location"))
	}
	return id
}

// offerAndSettle offers taskID to the worker, sends the claim as the
// coordinator's client and settles it.
func (e *coordEnv) offerAndSettle(t *testing.T, key, taskID string, expires time.Time) (string, store.Step) {
	t.Helper()
	got := e.call(t, e.lead.token, AttemptsPath, key, e.offerBody(taskID, expires))
	st, id := stepOf(t, got), attemptOf(t, got)
	if code := e.aimem.send(t, e.task(taskID), st); code != "" {
		t.Fatalf("aimem refused the offer: %s %v", code, e.aimem.refused)
	}
	settled(t, e.settleAs(t, e.lead, id, st, "committed", ""), "committed", "offered")
	return id, st
}

func (e *coordEnv) settleAs(t *testing.T, m member, id string, st store.Step, outcome, code string) reply {
	t.Helper()
	body := map[string]string{"request_key": st.RequestKey, "outcome": outcome}
	if code != "" {
		body["code"] = code
	}
	return e.call(t, m.token, AttemptsPath+"/"+id+"/settle", "", body)
}

func settled(t *testing.T, got reply, outcome, state string) map[string]any {
	t.Helper()
	attempt, _ := got.body["attempt"].(map[string]any)
	if got.status != http.StatusOK || got.body["settled"] != true || got.body["outcome"] != outcome || attempt["state"] != state {
		t.Fatalf("settle: %d %s, want %s and %s", got.status, got.raw, outcome, state)
	}
	return attempt
}

func pending(t *testing.T, got reply) {
	t.Helper()
	if got.status != http.StatusAccepted || got.body["settled"] != false || got.header.Get("Retry-After") != "10" {
		t.Fatalf("settle: %d %s (Retry-After %q), want pending", got.status, got.raw, got.header.Get("Retry-After"))
	}
}

func (e *coordEnv) accept(t *testing.T, id, key string) store.Step {
	t.Helper()
	return stepOf(t, e.call(t, e.worker.token, AttemptsPath+"/"+id+"/accept", key,
		map[string]string{"instruction_digest": coordPin.InstructionDigest}))
}

// seenKinds lists the fact kinds aimem accepted, in order.
func (e *coordEnv) seenKinds() string {
	var kinds []string
	for _, f := range e.aimem.seen {
		kinds = append(kinds, f.fact.Kind)
	}
	return strings.Join(kinds, ",")
}

// The offer family end to end, as members' clients drive it. Each begin
// goes through the route; the member's own aimem connection sends the step,
// and aimem verifies its proof through the real coordination route; settle
// confirms the outcome from aimem's read scope. aicrewd has no mutating port
// and sends aimem nothing.
func TestStepRoutesEndToEnd(t *testing.T) {
	e := setupCoordination(t)
	task := e.task("task-1")
	body := e.offerBody("task-1", time.Now().Add(time.Hour))
	got := e.call(t, e.lead.token, AttemptsPath, "offer-1", body)
	first, id := stepOf(t, got), attemptOf(t, got)
	var keys map[string]json.RawMessage
	if err := json.Unmarshal([]byte(got.raw), &keys); err != nil {
		t.Fatal(err)
	}
	var names []string
	for k := range keys {
		names = append(names, k)
	}
	sort.Strings(names)
	if strings.Join(names, ",") != "coordination_proof,expected_revision,holder,operation,request_key" ||
		first.Operation != store.ReservationClaim || first.ExpectedRevision != 3 || first.Holder.Mode != "external" {
		t.Fatalf("offer begin %s", got.raw)
	}

	// A retried begin gets a replacement proof for the same step; the
	// first proof ends, so aimem refuses it.
	retry := e.call(t, e.lead.token, AttemptsPath, "offer-1", body)
	second := stepOf(t, retry)
	if attemptOf(t, retry) != id || second.RequestKey != first.RequestKey || second.CoordinationProof == first.CoordinationProof {
		t.Fatalf("retried begin %s", retry.raw)
	}
	if code := e.aimem.send(t, task, first); code != "coordination_rejected" {
		t.Fatalf("the replaced proof: %q", code)
	}
	if code := e.aimem.send(t, task, second); code != "" {
		t.Fatalf("the replacement proof: %q %v", code, e.aimem.refused)
	}
	settled(t, e.settleAs(t, e.lead, id, second, "committed", ""), "committed", "offered")
	refused(t, e.call(t, e.lead.token, AttemptsPath, "offer-1", body), http.StatusConflict, "step_settled")
	settled(t, e.settleAs(t, e.lead, id, second, "committed", ""), "committed", "offered")

	// The worker accepts; its client loses aimem's reply and reports an
	// unknown outcome: the read scope settles it.
	acc := e.accept(t, id, "accept-1")
	hold := e.aimem.holds["task-1"]
	if acc.Operation != store.ReservationTransfer || acc.ReservationID != hold.id || acc.Fence != "1" ||
		acc.Holder == nil || acc.Holder.WorkRef == first.Holder.WorkRef {
		t.Fatalf("accept begin %+v", acc)
	}
	if code := e.aimem.send(t, task, acc); code != "" {
		t.Fatalf("aimem refused the transfer: %s %v", code, e.aimem.refused)
	}
	settled(t, e.settleAs(t, e.worker, id, acc, "unknown", ""), "committed", "running")
	if kinds := e.seenKinds(); kinds != "offer,accepted_attempt" {
		t.Fatalf("aimem accepted %s", kinds)
	}
	for _, f := range e.aimem.seen {
		if f.fact.Process == nil {
			t.Fatalf("the %s fact has no process pin", f.fact.Kind)
		}
	}

	// The proofs were in the begin replies only: not in the logs, not in
	// the store.
	var stored []byte
	for _, suffix := range []string{"", "-wal"} {
		b, err := os.ReadFile(e.storePath + suffix)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			t.Fatal(err)
		}
		stored = append(stored, b...)
	}
	for _, p := range []string{first.CoordinationProof, second.CoordinationProof, acc.CoordinationProof} {
		if strings.Contains(e.logs.String(), p) || bytes.Contains(stored, []byte(p)) {
			t.Fatal("a coordination proof reached the logs or the store")
		}
	}
}

// Decline is local; the coordinator then withdraws the declined offer. The
// coordinator's client goes offline after sending the release, and another
// member's settle confirms it from the read scope alone.
func TestStepRoutesDeclineThenWithdraw(t *testing.T) {
	e := setupCoordination(t)
	id, _ := e.offerAndSettle(t, "offer-1", "task-1", time.Now().Add(time.Hour))
	got := e.call(t, e.worker.token, AttemptsPath+"/"+id+"/decline", "decline-1", nil)
	if got.status != http.StatusOK || got.body["declined"] != true || got.body["state"] != "offered" {
		t.Fatalf("decline: %d %s", got.status, got.raw)
	}
	if again := e.call(t, e.worker.token, AttemptsPath+"/"+id+"/decline", "decline-1", map[string]any{}); again.raw != got.raw {
		t.Fatalf("replayed decline: %s", again.raw)
	}
	rel := stepOf(t, e.call(t, e.lead.token, AttemptsPath+"/"+id+"/withdraw", "withdraw-1", nil))
	if rel.Operation != store.ReservationRelease || rel.ReservationID != e.aimem.holds["task-1"].id || rel.Fence != "1" ||
		rel.Holder != nil {
		t.Fatalf("withdraw begin %+v", rel)
	}
	if code := e.aimem.send(t, e.task("task-1"), rel); code != "" {
		t.Fatalf("aimem refused the release: %s %v", code, e.aimem.refused)
	}
	if a := settled(t, e.settleAs(t, e.worker, id, rel, "unknown", ""), "committed", "closed"); a["close_reason"] != "declined" {
		t.Fatalf("closed as %v", a["close_reason"])
	}
	if kinds := e.seenKinds(); kinds != "offer,never_accepted" {
		t.Fatalf("aimem accepted %s", kinds)
	}
}

// An expired offer cannot be accepted; the coordinator releases it.
func TestStepRoutesExpiredOffer(t *testing.T) {
	e := setupCoordination(t)
	expires := time.Now().Add(time.Second)
	id, _ := e.offerAndSettle(t, "offer-1", "task-1", expires)
	time.Sleep(time.Until(expires) + 50*time.Millisecond)
	refused(t, e.call(t, e.worker.token, AttemptsPath+"/"+id+"/accept", "accept-1",
		map[string]string{"instruction_digest": coordPin.InstructionDigest}), http.StatusConflict, "offer_expired")
	rel := stepOf(t, e.call(t, e.lead.token, AttemptsPath+"/"+id+"/withdraw", "withdraw-1", nil))
	if code := e.aimem.send(t, e.task("task-1"), rel); code != "" {
		t.Fatalf("aimem refused the release: %s %v", code, e.aimem.refused)
	}
	if a := settled(t, e.settleAs(t, e.lead, id, rel, "committed", ""), "committed", "closed"); a["close_reason"] != "withdrawn" {
		t.Fatalf("closed as %v", a["close_reason"])
	}
}

// A report is only a hint. A committed report with no receipt waits; a
// refusal voids the step's proof at once, so aimem refuses any late use, and
// the step still waits out the grace period.
func TestStepRoutesRefusedStepWaits(t *testing.T) {
	e := setupCoordination(t)
	id, _ := e.offerAndSettle(t, "offer-1", "task-1", time.Now().Add(time.Hour))
	e.aimem.current.Commit = strings.Repeat("e", 40) // the project's selection moved on
	acc := e.accept(t, id, "accept-1")
	if code := e.aimem.send(t, e.task("task-1"), acc); code != "process_mismatch" {
		t.Fatalf("aimem answered %q", code)
	}
	pending(t, e.settleAs(t, e.worker, id, acc, "committed", ""))
	e.aimem.current = coordPin.Identity
	pending(t, e.settleAs(t, e.worker, id, acc, "refused", "process_mismatch"))
	if code := e.aimem.send(t, e.task("task-1"), acc); code != "coordination_rejected" {
		t.Fatalf("a voided proof: aimem answered %q", code)
	}
	refused(t, e.settleAs(t, e.worker, id, acc, "refused", ""), http.StatusBadRequest, "invalid_request")
}

// Without aimem's read scope, no settle ever settles: a report is never
// trusted (D-b1a-2).
func TestStepRoutesWithoutReader(t *testing.T) {
	e := setupCoordinationWith(t, false)
	got := e.call(t, e.lead.token, AttemptsPath, "offer-1", e.offerBody("task-1", time.Now().Add(time.Hour)))
	st, id := stepOf(t, got), attemptOf(t, got)
	if code := e.aimem.send(t, e.task("task-1"), st); code != "" {
		t.Fatalf("aimem refused the offer: %s", code)
	}
	pending(t, e.settleAs(t, e.lead, id, st, "committed", ""))
}

// otherTeam is a second team on the same hub with one coordinator in
// session there. That agent is also a member of the first team, so only its
// token's team keeps it from acting on the first team's attempts.
func (e *coordEnv) otherTeam(t *testing.T) member {
	t.Helper()
	ctx := context.Background()
	team, err := e.store.CreateTeam(ctx, e.op, "team-2", store.NewTeam{Name: "crew-2",
		Projects: []store.ProjectRef{{HubID: coordHub, ProjectID: "project-example"}}})
	if err != nil {
		t.Fatal(err)
	}
	a, err := e.store.CreateAgent(ctx, e.op, "agent-other", store.NewAgent{Label: "other"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.store.AddMember(ctx, e.op, "member-other", team.ID, a.ID, store.RoleCoordinator); err != nil {
		t.Fatal(err)
	}
	if _, err := e.store.AddMember(ctx, e.op, "member-other-1", e.team.ID, a.ID, store.RoleWorker); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", e.storePath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`UPDATE agents SET linked_hub_id = ?, linked_user_id = 'user-other', linked_token_id = 'token-other' WHERE id = ?`,
		coordHub, a.ID); err != nil {
		t.Fatal(err)
	}
	entry, secrets := enterAs(t, e.running, a, "other", team.ID)
	return member{agent: a, sess: entry.Session, token: secrets.Token.Reveal()}
}

// resume resumes m's session under a new generation and returns its new
// token; the old token dies.
func (e *coordEnv) resume(t *testing.T, m member) string {
	t.Helper()
	ctx := context.Background()
	entries++
	ch, err := e.store.IssueAgentChallenge(ctx, store.Caller{}, fmt.Sprintf("resume-challenge-%d", entries), m.agent.ID)
	if err != nil {
		t.Fatal(err)
	}
	receipt := fmt.Sprintf("amr1_%s-%043d", m.agent.Label, entries)
	v := &fakeVerifier{ids: map[string]store.VerifiedIdentity{
		receipt: {HubID: coordHub, UserID: "user-" + m.agent.Label, TokenID: "token-" + m.agent.Label}}}
	_, secrets, err := e.store.ResumeSessionWithProof(ctx, v, fmt.Sprintf("resume-%d", entries), ch.ID, store.NewSecret(receipt),
		m.sess.ID, coordService)
	if err != nil {
		t.Fatal(err)
	}
	return secrets.Token.Reveal()
}

// Every step route acts only for its token's session, in its role and team,
// and refuses in the session API's envelope.
func TestStepRouteRefusals(t *testing.T) {
	e := setupCoordination(t)
	accept := func(id string) string { return AttemptsPath + "/" + id + "/accept" }
	digest := map[string]string{"instruction_digest": coordPin.InstructionDigest}
	body := e.offerBody("task-1", time.Now().Add(time.Hour))

	for _, path := range []string{AttemptsPath, accept("a-1"), AttemptsPath + "/a-1/decline", AttemptsPath + "/a-1/withdraw",
		AttemptsPath + "/a-1/settle"} {
		refused(t, e.call(t, "", path, "k-1", map[string]any{}), http.StatusUnauthorized, "invalid_token")
		refused(t, e.call(t, "not-a-token", path, "k-1", map[string]any{}), http.StatusUnauthorized, "invalid_token")
	}
	refused(t, e.call(t, e.lead.token, AttemptsPath, "", body), http.StatusBadRequest, "invalid_request")
	refused(t, e.call(t, e.lead.token, AttemptsPath, "k-1", map[string]any{"task": "x"}), http.StatusBadRequest, "invalid_request")
	refused(t, e.call(t, e.worker.token, AttemptsPath, "offer-w", body), http.StatusForbidden, "attempt_forbidden")

	id, _ := e.offerAndSettle(t, "offer-1", "task-1", time.Now().Add(time.Hour))
	refused(t, e.call(t, e.lead.token, AttemptsPath, "offer-2", body), http.StatusConflict, "task_busy")
	refused(t, e.call(t, e.lead.token, accept(id), "accept-l", digest), http.StatusForbidden, "attempt_forbidden")
	refused(t, e.call(t, e.worker.token, accept(id), "accept-x", map[string]string{"instruction_digest": "sha256:other"}),
		http.StatusConflict, "instruction_mismatch")
	refused(t, e.settleAs(t, e.worker, id, store.Step{RequestKey: "no-such-step"}, "unknown", ""), http.StatusNotFound, "step_unknown")

	other := e.otherTeam(t)
	refused(t, e.settleAs(t, other, id, store.Step{RequestKey: "no-such-step"}, "unknown", ""), http.StatusForbidden, "attempt_forbidden")
	refused(t, e.call(t, other.token, AttemptsPath+"/"+id+"/withdraw", "withdraw-o", nil), http.StatusForbidden, "attempt_forbidden")

	// A resumed session's old token is dead, on a new step and on a replay.
	old := e.lead.token
	e.lead.token = e.resume(t, e.lead)
	refused(t, e.call(t, old, AttemptsPath+"/"+id+"/withdraw", "withdraw-1", nil), http.StatusUnauthorized, "invalid_token")
	refused(t, e.call(t, old, AttemptsPath, "offer-1", body), http.StatusUnauthorized, "invalid_token")

	// Paths and methods: an ID of another shape or an unknown action is
	// not a route; another method is refused in the envelope.
	for _, path := range []string{AttemptsPath + "/../accept", AttemptsPath + "/%61-1/accept", AttemptsPath + "/a-1/start",
		AttemptsPath + "/a-1/accept/x", AttemptsPath + "/"} {
		if got := e.call(t, e.lead.token, path, "k-1", map[string]any{}); got.status != http.StatusNotFound {
			t.Fatalf("%s: %d %s", path, got.status, got.raw)
		}
	}
	req, _ := http.NewRequest(http.MethodGet, e.url(accept(id)), nil)
	resp, err := e.client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	var env map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&env)
	resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed || env["code"] != "method_not_allowed" || env["correlation_id"] == nil {
		t.Fatalf("GET: %d %v", resp.StatusCode, env)
	}
	status, raw := e.head(t, "POST "+accept(id)+" HTTP/1.1\r\nHost: x\r\nContent-Length: 70000\r\n\r\n")
	if status != http.StatusRequestEntityTooLarge || !bytes.Contains(raw, []byte(`"correlation_id"`)) {
		t.Fatalf("oversized: %d %s", status, raw)
	}
	for _, line := range strings.Split(e.logs.String(), "\n") {
		if strings.Contains(line, id) {
			t.Fatalf("an attempt ID reached the log: %s", line)
		}
	}
}

func (e *coordEnv) claimBody(taskID string) map[string]any {
	return map[string]any{"task": e.task(taskID), "expected_revision": 3, "base_commit": "base-1", "branch": "work/" + taskID,
		"process": map[string]string{"repo": coordPin.Identity.Repository, "commit": coordPin.Identity.Commit,
			"manifest": coordPin.Identity.Manifest},
		"instruction_digest": coordPin.InstructionDigest}
}

// The independent claim and the stop family end to end, as members' clients
// drive them. aimem verifies the claim's pin through the coordination route;
// aicrew records it as verified only once the claim settles. The stop is
// requested by the coordinator, confirmed by the holder, and released by the
// holder under the stopped fact with the values the begin returned; the
// holder then goes offline, and the coordinator's settle confirms the
// release from the read scope alone.
func TestClaimAndStopRoutesEndToEnd(t *testing.T) {
	e := setupCoordination(t)
	got := e.call(t, e.indep.token, ClaimPath, "claim-1", e.claimBody("task-1"))
	claim, id := stepOf(t, got), attemptOf(t, got)
	if claim.Operation != store.ReservationClaim || claim.Holder == nil || claim.Holder.WorkRef != "aicrew-attempt-"+id ||
		claim.ReservationID != "" || claim.TargetState != "" {
		t.Fatalf("claim begin %s", got.raw)
	}
	if code := e.aimem.send(t, e.task("task-1"), claim); code != "" {
		t.Fatalf("aimem refused the claim: %s %v", code, e.aimem.refused)
	}
	if f := e.last(t).fact; f.Kind != "independent_claim" || f.Process == nil || f.Process.Commit != coordPin.Identity.Commit {
		t.Fatalf("the claim's fact %+v", f)
	}
	running := settled(t, e.settleAs(t, e.indep, id, claim, "committed", ""), "committed", "running")
	if r, _ := running["process_verified_receipt"].(string); r == "" {
		t.Fatalf("the claim settled without a verified pin: %v", running)
	}

	stop := func(m member, key string) reply {
		return e.call(t, m.token, AttemptsPath+"/"+id+"/stop", key, map[string]string{"reason": "priorities changed"})
	}
	refused(t, stop(e.indep, "stop-self"), http.StatusForbidden, "attempt_forbidden")
	if got := stop(e.lead, "stop-1"); got.status != http.StatusOK || got.body["stop"] != "requested" {
		t.Fatalf("stop: %d %s", got.status, got.raw)
	}
	confirm := func(m member) reply { return e.call(t, m.token, AttemptsPath+"/"+id+"/confirm-stop", "confirm-1", nil) }
	refused(t, confirm(e.lead), http.StatusForbidden, "attempt_forbidden")
	if got := confirm(e.indep); got.status != http.StatusOK || got.body["stop"] != "confirmed" {
		t.Fatalf("confirm: %d %s", got.status, got.raw)
	}
	rel := stepOf(t, e.call(t, e.indep.token, AttemptsPath+"/"+id+"/release", "release-1",
		map[string]string{"target": "BLOCKED", "blocker": "waiting on design"}))
	if rel.Operation != store.ReservationRelease || rel.ReservationID != e.aimem.holds["task-1"].id || rel.Holder != nil ||
		rel.TargetState != "BLOCKED" || rel.Reason != "stopped" || rel.Blocker != "waiting on design" {
		t.Fatalf("release begin %+v", rel)
	}
	if code := e.aimem.send(t, e.task("task-1"), rel); code != "" {
		t.Fatalf("aimem refused the release: %s %v", code, e.aimem.refused)
	}
	if a := settled(t, e.settleAs(t, e.lead, id, rel, "unknown", ""), "committed", "closed"); a["close_reason"] != "stopped" {
		t.Fatalf("closed as %v", a["close_reason"])
	}
	if kinds := e.seenKinds(); kinds != "independent_claim,stopped" {
		t.Fatalf("aimem accepted %s", kinds)
	}
}

// A claim under a pin that is no longer the project's selection is refused by
// aimem, and never verified: the refusal voids its proof.
func TestClaimRouteStalePin(t *testing.T) {
	e := setupCoordination(t)
	e.aimem.current.Commit = strings.Repeat("e", 40)
	got := e.call(t, e.indep.token, ClaimPath, "claim-1", e.claimBody("task-1"))
	claim, id := stepOf(t, got), attemptOf(t, got)
	if code := e.aimem.send(t, e.task("task-1"), claim); code != "process_mismatch" {
		t.Fatalf("aimem answered %q", code)
	}
	pending(t, e.settleAs(t, e.indep, id, claim, "refused", "process_mismatch"))
	if a, err := e.store.GetAttempt(context.Background(), id); err != nil || a.ProcessVerifiedReceipt != "" {
		t.Fatalf("a refused claim: %+v %v", a, err)
	}
	e.aimem.current = coordPin.Identity
	if code := e.aimem.send(t, e.task("task-1"), claim); code != "coordination_rejected" {
		t.Fatalf("a voided claim proof: aimem answered %q", code)
	}
}

// The claim and stop routes act only in their roles.
func TestClaimAndStopRouteRefusals(t *testing.T) {
	e := setupCoordination(t)
	refused(t, e.call(t, e.worker.token, ClaimPath, "claim-w", e.claimBody("task-1")), http.StatusForbidden, "attempt_forbidden")
	refused(t, e.call(t, e.indep.token, ClaimPath, "", e.claimBody("task-1")), http.StatusBadRequest, "invalid_request")
	got := e.call(t, e.indep.token, ClaimPath, "claim-1", e.claimBody("task-1"))
	id := attemptOf(t, got)
	refused(t, e.call(t, e.indep.token, ClaimPath, "claim-2", e.claimBody("task-1")), http.StatusConflict, "task_busy")
	refused(t, e.call(t, e.indep.token, AttemptsPath+"/"+id+"/release", "release-0", map[string]string{"target": "READY"}),
		http.StatusConflict, "attempt_state")
	refused(t, e.call(t, e.indep.token, AttemptsPath+"/"+id+"/release", "release-x", map[string]string{"target": "DONE"}),
		http.StatusBadRequest, "invalid_request")
	refused(t, e.call(t, e.lead.token, AttemptsPath+"/"+id+"/stop", "stop-x", map[string]string{"reason": ""}),
		http.StatusBadRequest, "invalid_request")
	for _, path := range []string{ClaimPath, AttemptsPath + "/" + id + "/stop", AttemptsPath + "/" + id + "/confirm-stop",
		AttemptsPath + "/" + id + "/release"} {
		refused(t, e.call(t, "", path, "k-1", map[string]any{}), http.StatusUnauthorized, "invalid_token")
	}
}

var confirmedEvidence = []store.Evidence{
	{Kind: "reviewed_head", Ref: "https://forge.example/pull/7#review-1"},
	{Kind: "human_merge", Ref: "https://forge.example/commit/merge-7"},
	{Kind: "post_merge_ci", Ref: "https://forge.example/actions/runs/7"},
}

// runningOffer offers task-1 to the worker, which accepts; both steps are
// sent and settled through the routes.
func (e *coordEnv) runningOffer(t *testing.T) string {
	t.Helper()
	id, _ := e.offerAndSettle(t, "offer-1", "task-1", time.Now().Add(time.Hour))
	acc := e.accept(t, id, "accept-1")
	if code := e.aimem.send(t, e.task("task-1"), acc); code != "" {
		t.Fatalf("aimem refused the transfer: %s %v", code, e.aimem.refused)
	}
	settled(t, e.settleAs(t, e.worker, id, acc, "committed", ""), "committed", "running")
	return id
}

// submit submits a result as the worker, through the one-shot update path
// until b1b-3 serves it as a step, and returns the result's sequence.
func (e *coordEnv) submit(t *testing.T, id, key string) int64 {
	t.Helper()
	ctx := context.Background()
	b, err := e.store.AuthenticateSessionToken(ctx, e.worker.token)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.store.UpdateWork(ctx, e.worker.caller, e.aimem, key, id, store.WorkUpdate{SessionID: b.SessionID,
		Generation: b.Generation, Intent: store.IntentSubmit, Detail: "https://forge.example/pull/7"}); err != nil {
		t.Fatalf("submit: %v %v", err, e.aimem.refused)
	}
	res, err := e.store.AttemptResults(ctx, id)
	if err != nil || len(res) == 0 {
		t.Fatalf("results: %v %v", res, err)
	}
	return res[len(res)-1].Seq
}

// Review, confirmed delivery and finalize end to end. The coordinator reviews
// the result and confirms its delivery with the evidence it gathered; the
// finalize names the result only, and its begin response carries the
// confirmed evidence as the terminal evidence the member sends. aimem
// verifies the accepted_for_finalization fact and, as its ledger requires,
// the terminal evidence.
func TestReviewDeliveryFinalizeEndToEnd(t *testing.T) {
	e := setupCoordination(t)
	id := e.runningOffer(t)
	seq := e.submit(t, id, "submit-1")
	path := func(action string) string { return AttemptsPath + "/" + id + "/" + action }
	review := map[string]any{"result_seq": seq, "decision": "accept"}
	refused(t, e.call(t, e.worker.token, path("review"), "review-self", review), http.StatusForbidden, "attempt_forbidden")
	if got := e.call(t, e.lead.token, path("review"), "review-1", review); got.status != http.StatusOK ||
		got.body["phase"] != "accepted" || got.body["accepted_result"] != float64(seq) {
		t.Fatalf("review: %d %s", got.status, got.raw)
	}

	fin := map[string]any{"result_seq": seq}
	refused(t, e.call(t, e.worker.token, path("finalize"), "fin-1", fin), http.StatusConflict, "delivery_unconfirmed")
	// Evidence the finalizer supplies itself unlocks nothing: a finalize
	// carries no evidence.
	refused(t, e.call(t, e.worker.token, path("finalize"), "fin-2",
		map[string]any{"result_seq": seq, "terminal_evidence": []string{"https://forge.example/pull/7"}}),
		http.StatusBadRequest, "invalid_request")
	confirm := map[string]any{"result_seq": seq, "evidence": confirmedEvidence}
	refused(t, e.call(t, e.worker.token, path("confirm-delivery"), "confirm-self", confirm), http.StatusForbidden, "attempt_forbidden")
	refused(t, e.call(t, e.lead.token, path("confirm-delivery"), "confirm-thin",
		map[string]any{"result_seq": seq, "evidence": confirmedEvidence[:2]}), http.StatusBadRequest, "invalid_request")
	if got := e.call(t, e.lead.token, path("confirm-delivery"), "confirm-1", confirm); got.status != http.StatusOK ||
		got.body["delivery_result"] != float64(seq) {
		t.Fatalf("confirm-delivery: %d %s", got.status, got.raw)
	}

	// The reviewing coordinator finalizes.
	st := stepOf(t, e.call(t, e.lead.token, path("finalize"), "fin-3", fin))
	want := []string{confirmedEvidence[0].Ref, confirmedEvidence[1].Ref, confirmedEvidence[2].Ref}
	if st.Operation != store.ReservationFinalize || st.TargetState != "DONE" || st.Reason == "" ||
		strings.Join(st.TerminalEvidence, "|") != strings.Join(want, "|") {
		t.Fatalf("finalize begin %+v", st)
	}
	if code := e.aimem.send(t, e.task("task-1"), st); code != "" {
		t.Fatalf("aimem refused the finalize: %s %v", code, e.aimem.refused)
	}
	if a := settled(t, e.settleAs(t, e.worker, id, st, "unknown", ""), "committed", "closed"); a["close_reason"] != "finalized" {
		t.Fatalf("closed as %v", a["close_reason"])
	}
	if kinds := e.seenKinds(); kinds != "offer,accepted_attempt,accepted_for_finalization" {
		t.Fatalf("aimem accepted %s", kinds)
	}
}

// A finalize sent without its terminal evidence is refused by aimem, as its
// ledger requires: the evidence travels only in the begin response.
func TestFinalizeWithoutEvidenceIsRefusedByAimem(t *testing.T) {
	e := setupCoordination(t)
	id := e.runningOffer(t)
	seq := e.submit(t, id, "submit-1")
	path := func(action string) string { return AttemptsPath + "/" + id + "/" + action }
	if got := e.call(t, e.lead.token, path("review"), "review-1", map[string]any{"result_seq": seq, "decision": "accept"}); got.status != http.StatusOK {
		t.Fatalf("review: %d %s", got.status, got.raw)
	}
	if got := e.call(t, e.lead.token, path("confirm-delivery"), "confirm-1",
		map[string]any{"result_seq": seq, "evidence": confirmedEvidence}); got.status != http.StatusOK {
		t.Fatalf("confirm: %d %s", got.status, got.raw)
	}
	st := stepOf(t, e.call(t, e.worker.token, path("finalize"), "fin-1", map[string]any{"result_seq": seq}))
	st.TerminalEvidence = nil
	if code := e.aimem.send(t, e.task("task-1"), st); code != "invalid_request" {
		t.Fatalf("a finalize without its evidence: aimem answered %q", code)
	}
}
