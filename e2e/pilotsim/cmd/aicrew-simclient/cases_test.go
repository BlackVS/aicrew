package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/BlackVS/aicrew/internal/agent"
	"github.com/BlackVS/aicrew/internal/forge"
)

// The member's behaviour in the pilot cases (scenario).

func (f *fakeTools) memberWith(sc scenario) *member {
	m := f.member()
	m.sc = sc
	return m
}

func TestReadScenario(t *testing.T) {
	dir := t.TempDir()
	if sc, err := readScenario(filepath.Join(dir, "none.json")); err != nil || sc != (scenario{}) {
		t.Fatalf("a missing file = %+v, %v", sc, err)
	}
	for body, ok := range map[string]bool{
		`{"stall": true, "cost_usd": 3.5}`: true,
		`{"result": "model"}`:              true,
		`{"result": "limit"}`:              false,
		`{"stall": `:                       false,
	} {
		path := filepath.Join(dir, "member.json")
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := readScenario(path); (err == nil) != ok {
			t.Errorf("%s: err = %v", body, err)
		}
	}
}

func TestScenarioClientErrors(t *testing.T) {
	for result, status := range map[string]float64{"login": 401, "model": 404} {
		f := newFake(t)
		var out bytes.Buffer
		m := f.memberWith(scenario{Result: result, CostUSD: 1.25})
		if code := turnRun(context.Background(), f.home, strings.NewReader(workerBrief("running", "working")), &out, m); code != 1 {
			t.Fatalf("%s: turnRun = %d", result, code)
		}
		ev := lastResult(t, out.String())
		if ev["api_error_status"] != status || ev["is_error"] != true || ev["total_cost_usd"] != 1.25 {
			t.Fatalf("%s: the result = %v", result, ev)
		}
		if len(f.calls) != 0 {
			t.Fatalf("%s: the turn ran commands: %v", result, f.calls)
		}
	}
}

func TestScenarioCostOnASuccess(t *testing.T) {
	f := newFake(t)
	var out bytes.Buffer
	if code := turnRun(context.Background(), f.home, strings.NewReader(coordinatorBrief), &out,
		f.memberWith(scenario{CostUSD: 7})); code != 0 {
		t.Fatalf("turnRun = %d: %s", code, out.String())
	}
	if ev := lastResult(t, out.String()); ev["total_cost_usd"] != float64(7) || ev["is_error"] != false {
		t.Fatalf("the result = %v", ev)
	}
}

func TestUnreadableScenarioFailsTheTurn(t *testing.T) {
	f := newFake(t)
	m := f.member()
	m.scErr = os.ErrPermission
	var out bytes.Buffer
	if code := turnRun(context.Background(), f.home, strings.NewReader(coordinatorBrief), &out, m); code != 1 {
		t.Fatalf("turnRun = %d", code)
	}
	if len(f.calls) != 0 {
		t.Fatalf("the turn ran commands: %v", f.calls)
	}
}

func TestScenarioStallLeavesEverything(t *testing.T) {
	f := newFake(t)
	writeSection(t, f.home)
	before := readHandoff(t, f.home)
	var out bytes.Buffer
	if code := turnRun(context.Background(), f.home, strings.NewReader(workerBrief("running", "working")), &out,
		f.memberWith(scenario{Stall: true})); code != 0 {
		t.Fatalf("turnRun = %d: %s", code, out.String())
	}
	if len(f.calls) != 0 || readHandoff(t, f.home) != before {
		t.Fatalf("a stalled turn acted: %v", f.calls)
	}
}

func TestScenarioNeverAck(t *testing.T) {
	f := newFake(t)
	f.inbox = []map[string]any{{"id": "m1", "kind": "lifecycle", "attempt_id": tAttempt,
		"task": map[string]string{"task_id": tTask}, "offer": map[string]string{"instruction_digest": "d1"}}}
	var out bytes.Buffer
	if code := turnRun(context.Background(), f.home, strings.NewReader(workerBrief("offered", "offered")), &out,
		f.memberWith(scenario{NeverAck: true})); code != 0 {
		t.Fatalf("turnRun = %d: %s", code, out.String())
	}
	if !f.called("aicrew-agent step accept") || f.called("aicrew-agent inbox --ack") {
		t.Fatalf("calls = %v", f.calls)
	}
}

func answerMessage(attempt string) map[string]any {
	return map[string]any{"id": "m9", "kind": "action", "attempt_id": attempt,
		"action": map[string]any{"subjects": []map[string]any{{"kind": "answer", "attempt_id": attempt}}}}
}

func TestWorkerResumesOnTheAnswer(t *testing.T) {
	f := newFake(t)
	writeSection(t, f.home)
	var out bytes.Buffer
	if code := turnRun(context.Background(), f.home, strings.NewReader(workerBrief("running", "blocked")), &out, f.member()); code != 0 {
		t.Fatalf("turn 1 = %d: %s", code, out.String())
	}
	if f.called("aicrew-agent step") {
		t.Fatalf("a blocked attempt without an answer made a step: %v", f.calls)
	}
	f.calls, f.inbox = nil, []map[string]any{answerMessage("01a10000-0000-7000-8000-0000000000ff"), answerMessage(tAttempt)}
	out.Reset()
	if code := turnRun(context.Background(), f.home, strings.NewReader(workerBrief("running", "blocked")), &out, f.member()); code != 0 {
		t.Fatalf("turn 2 = %d: %s", code, out.String())
	}
	if got := f.bodies["work"]; got["intent"] != "resume" {
		t.Fatalf("work body = %v; calls %v", got, f.calls)
	}
	if !f.called("aicrew-agent clone") {
		t.Fatalf("the resumed attempt did no work: %v", f.calls)
	}
}

func TestCoordinatorAnswersABlockedReport(t *testing.T) {
	f := newFake(t)
	f.attempts = []agent.OpenAttempt{{ID: tAttempt, State: "running", Phase: "blocked", Task: agent.TaskRef{TaskID: tTask}}}
	f.inbox = []map[string]any{{"id": "m1", "kind": "action", "attempt_id": tAttempt,
		"action": map[string]any{"subjects": []map[string]any{{"kind": "blocked", "attempt_id": tAttempt}}}}}
	var out bytes.Buffer
	if code := turnRun(context.Background(), f.home, strings.NewReader(coordinatorBrief), &out, f.member()); code != 0 {
		t.Fatalf("turnRun = %d: %s", code, out.String())
	}
	if got := f.bodies["answer"]; got["text"] != stallAnswer {
		t.Fatalf("answer body = %v; calls %v", got, f.calls)
	}
}

const tEscalation = "01a1e5c0-0000-7000-8000-000000000001"

func blockedReport() []map[string]any {
	return []map[string]any{{"id": "m1", "kind": "action", "attempt_id": tAttempt,
		"action": map[string]any{"subjects": []map[string]any{{"kind": "blocked", "attempt_id": tAttempt}}}}}
}

func TestCoordinatorEscalatesABlockedReport(t *testing.T) {
	f := newFake(t)
	f.attempts = []agent.OpenAttempt{{ID: tAttempt, State: "running", Phase: "blocked", WorkerAgentID: "w1",
		Task: agent.TaskRef{TaskID: tTask}}}
	f.inbox = blockedReport()
	var out bytes.Buffer
	if code := turnRun(context.Background(), f.home, strings.NewReader(coordinatorBrief), &out,
		f.memberWith(scenario{EscalateBlocked: true})); code != 0 {
		t.Fatalf("turnRun = %d: %s", code, out.String())
	}
	if f.called("aicrew-agent step answer") {
		t.Fatalf("the coordinator answered instead of escalating: %v", f.calls)
	}
	var body map[string]any
	if err := json.Unmarshal([]byte(f.stdin["aicrew-agent escalate --body -"]), &body); err != nil {
		t.Fatalf("escalate body: %v; calls %v", err, f.calls)
	}
	if body["blocked"] != "w1" || body["attempt_id"] != tAttempt || body["category"] != "scope" {
		t.Fatalf("escalate body = %v", body)
	}
	if !f.called("mcp add_task_comment") || !f.called("aicrew-agent inbox --ack") {
		t.Fatalf("no mirror or ack: %v", f.calls)
	}
}

func TestCoordinatorEscalatesOncePerTask(t *testing.T) {
	f := newFake(t)
	f.attempts = []agent.OpenAttempt{{ID: tAttempt, State: "running", Phase: "blocked", Task: agent.TaskRef{TaskID: tTask}}}
	f.escalations = []map[string]any{{"id": tEscalation, "task": map[string]string{"task_id": tTask}}}
	f.inbox = blockedReport()
	var out bytes.Buffer
	if code := turnRun(context.Background(), f.home, strings.NewReader(coordinatorBrief), &out,
		f.memberWith(scenario{EscalateBlocked: true})); code != 0 {
		t.Fatalf("turnRun = %d: %s", code, out.String())
	}
	if f.called("aicrew-agent escalate --body -") || f.called("aicrew-agent step answer") {
		t.Fatalf("calls = %v", f.calls)
	}
	// The open one is mirrored again under its own key: a turn that raised
	// it may have failed before its mirror.
	if !f.called("mcp add_task_comment") {
		t.Fatalf("the open escalation was not mirrored: %v", f.calls)
	}
}

// On a task its worker holds, the mirror waits for the hold, the turn goes
// on, and a later blocked report with the answer raises nothing more (the
// pilot simulation's first E1 run raised a second escalation there).
func TestEscalationOnAHeldTask(t *testing.T) {
	f := newFake(t)
	f.commentHeld = true
	f.attempts = []agent.OpenAttempt{{ID: tAttempt, State: "running", Phase: "blocked", WorkerAgentID: "w1",
		Task: agent.TaskRef{TaskID: tTask}}}
	f.inbox = blockedReport()
	var out bytes.Buffer
	if code := turnRun(context.Background(), f.home, strings.NewReader(coordinatorBrief), &out,
		f.memberWith(scenario{EscalateBlocked: true})); code != 0 {
		t.Fatalf("turn 1 = %d: %s", code, out.String())
	}
	if h := readHandoff(t, f.home); !strings.Contains(h, "Escalation "+tEscalation+": its mirror on task "+tTask+" waits") {
		t.Fatalf("the handoff does not say the mirror waits:\n%s", h)
	}
	// The answer arrives; the escalation is no longer open.
	f.calls = nil
	f.inbox = append(blockedReport(), map[string]any{"id": "m2", "kind": "lifecycle", "attempt_id": tAttempt,
		"task":       map[string]string{"task_id": tTask},
		"escalation": map[string]string{"id": tEscalation, "decision": "go on"}})
	out.Reset()
	if code := turnRun(context.Background(), f.home, strings.NewReader(coordinatorBrief), &out,
		f.memberWith(scenario{EscalateBlocked: true})); code != 0 {
		t.Fatalf("turn 2 = %d: %s", code, out.String())
	}
	if f.called("aicrew-agent escalate") || f.bodies["answer"]["text"] != "go on" {
		t.Fatalf("turn 2: calls %v, answer %v", f.calls, f.bodies["answer"])
	}
	// Without the answer in the turn, the handoff's record of the raise
	// stops a second one too.
	f.calls, f.inbox = nil, blockedReport()
	out.Reset()
	if code := turnRun(context.Background(), f.home, strings.NewReader(coordinatorBrief), &out,
		f.memberWith(scenario{EscalateBlocked: true})); code != 0 || f.called("aicrew-agent escalate") {
		t.Fatalf("turn 3 = %d: calls %v", code, f.calls)
	}
}

func TestDeliveryPostsTheWaitingMirror(t *testing.T) {
	f := newFake(t)
	deliveryHome(t, f)
	h, err := os.OpenFile(filepath.Join(f.home, filepath.FromSlash(agent.HandoffFile)), os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.WriteString("- 2026-10-29T11:00:00Z Escalation " + tEscalation + ": its mirror on task " + tTask +
		" waits for the task's hold to end.\n"); err != nil {
		t.Fatal(err)
	}
	h.Close()
	f.verify = `{"verdict": "confirmed", "evidence": [{"kind": "reviewed_head", "ref": "R"}]}`
	var out bytes.Buffer
	if code := turnRun(context.Background(), f.home, strings.NewReader(coordinatorBrief), &out, f.member()); code != 0 {
		t.Fatalf("turnRun = %d: %s", code, out.String())
	}
	if len(f.comments) != 1 || !strings.HasPrefix(f.comments[0], "[escalation.request "+tEscalation+"]") {
		t.Fatalf("comments = %q", f.comments)
	}
	if !strings.Contains(readHandoff(t, f.home), "Mirrored escalation "+tEscalation+" on task "+tTask) {
		t.Fatalf("the handoff does not record the mirror")
	}
}

func TestCoordinatorActsOnTheEscalationAnswer(t *testing.T) {
	f := newFake(t)
	f.attempts = []agent.OpenAttempt{{ID: tAttempt, State: "running", Phase: "blocked", Task: agent.TaskRef{TaskID: tTask}}}
	f.inbox = []map[string]any{{"id": "m2", "kind": "lifecycle", "attempt_id": tAttempt,
		"task":       map[string]string{"task_id": tTask},
		"escalation": map[string]string{"id": tEscalation, "decision": "go on"}}}
	var out bytes.Buffer
	if code := turnRun(context.Background(), f.home, strings.NewReader(coordinatorBrief), &out,
		f.memberWith(scenario{EscalateBlocked: true})); code != 0 {
		t.Fatalf("turnRun = %d: %s", code, out.String())
	}
	if got := f.bodies["answer"]; got["text"] != "go on" {
		t.Fatalf("answer body = %v; calls %v", got, f.calls)
	}
	if !f.called("aicrew-agent inbox --ack") {
		t.Fatalf("the answer was not acked: %v", f.calls)
	}
}

func TestCoordinatorChecksAcceptedAttemptsWithoutAMessage(t *testing.T) {
	f := newFake(t)
	f.attempts = []agent.OpenAttempt{{ID: tAttempt, State: "running", Phase: "accepted", Task: agent.TaskRef{TaskID: tTask}}}
	var out bytes.Buffer
	// No result is recorded, so the delivery check fails the turn: it ran.
	if code := turnRun(context.Background(), f.home, strings.NewReader(coordinatorBrief), &out, f.member()); code != 1 {
		t.Fatalf("turnRun = %d: %s", code, out.String())
	}
	if !f.called("aicrew-agent attempts") {
		t.Fatalf("calls = %v", f.calls)
	}
}

// openedPull runs the worker's first turn, which opens pull request #7.
func openedPull(t *testing.T, f *fakeTools) {
	t.Helper()
	writeSection(t, f.home)
	var out bytes.Buffer
	if code := turnRun(context.Background(), f.home, strings.NewReader(workerBrief("running", "working")), &out, f.member()); code != 0 {
		t.Fatalf("turn 1 = %d: %s", code, out.String())
	}
	f.calls = nil
}

func TestWorkerReworksFailedChecks(t *testing.T) {
	f := newFake(t)
	openedPull(t, f)
	f.pull = &agent.PullReport{Pull: forge.Pull{Number: 7, URL: tPull, State: "open", HeadCommit: tHead,
		Labels: []string{"hands-reviewed"}}, Content: &forge.PullContent{
		Checks: []forge.Check{{Name: "sim-ci", State: forge.ChecksFailure}},
		Comments: []forge.PullComment{{Author: "sim-hands", Review: &forge.ReviewHeader{Reviewer: forge.ReviewerExternal,
			Head: tHead, AtHead: true, Verdict: "READY_FOR_HUMAN_MERGE"}}}}}
	var out bytes.Buffer
	if code := turnRun(context.Background(), f.home, strings.NewReader(workerBrief("running", "working")), &out, f.member()); code != 0 {
		t.Fatalf("turn 2 = %d: %s", code, out.String())
	}
	if !f.called("git push") || !f.called("aicrew-agent forge pr label add --pr 7 --json review-this") ||
		f.called("aicrew-agent step") {
		t.Fatalf("calls = %v", f.calls)
	}
	if doc := readHandoff(t, f.home); !strings.Contains(doc, "Reworked #7: the checks failed at "+tHead) {
		t.Fatalf("the handoff:\n%s", doc)
	}
}

func TestWorkerPushesDuringTheReviewOnce(t *testing.T) {
	f := newFake(t)
	openedPull(t, f)
	f.pull = &agent.PullReport{Pull: forge.Pull{Number: 7, URL: tPull, State: "open", HeadCommit: tHead,
		Labels: []string{"hands-reviewing"}}, Content: &forge.PullContent{}}
	sc := scenario{PushDuringReview: true}
	var out bytes.Buffer
	if code := turnRun(context.Background(), f.home, strings.NewReader(workerBrief("running", "working")), &out, f.memberWith(sc)); code != 0 {
		t.Fatalf("turn 2 = %d: %s", code, out.String())
	}
	if !f.called("git push") || f.called("aicrew-agent forge pr label") {
		t.Fatalf("the push during the review: %v", f.calls)
	}
	f.calls = nil
	out.Reset()
	if code := turnRun(context.Background(), f.home, strings.NewReader(workerBrief("running", "working")), &out, f.memberWith(sc)); code != 0 {
		t.Fatalf("turn 3 = %d: %s", code, out.String())
	}
	if f.called("git push") {
		t.Fatalf("a second push during the review: %v", f.calls)
	}
}
