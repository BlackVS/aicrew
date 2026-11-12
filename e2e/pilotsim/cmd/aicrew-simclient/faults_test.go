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

// stepReply is one scripted answer of `aicrew-agent step`.
type stepReply struct {
	out  []byte
	code int
}

func stepAnswer(status string, code int, res any, stepErr *agent.StepError) stepReply {
	raw, _ := json.Marshal(res)
	out, _ := json.Marshal(agent.StepAnswer{OK: code == 0, Status: status, Result: raw, Error: stepErr})
	return stepReply{out: out, code: code}
}

// notCommitted is the answer to a step aicrewd settled as not committed.
func notCommitted(op string) stepReply {
	return stepAnswer(agent.StepRefused, 3, agent.StepResult{AttemptID: tAttempt, RequestKey: "k-lost", Operation: op,
		Report: agent.StepReport{Outcome: "refused", Code: "not_committed"}, Settled: true, Outcome: "not_committed"}, nil)
}

// pending is the answer to a step that is sent and not settled.
func pending(op string) stepReply {
	return stepAnswer(agent.StepPending, 4, agent.StepResult{AttemptID: tAttempt, RequestKey: "k-pending", Operation: op,
		Report: agent.StepReport{Outcome: "unknown"}}, nil)
}

func offerInbox() []map[string]any {
	return []map[string]any{{"id": "m1", "kind": "lifecycle", "attempt_id": tAttempt,
		"task":  map[string]string{"hub_id": "H1", "project_id": "pilot", "task_id": tTask},
		"offer": map[string]any{"instruction_digest": "sha256:abc"}, "text": "An offer."}}
}

const offeredBrief = "/crew-turn Attempt " + tAttempt + ": offered/offered.\n\n## Who and where\n\n- Role: worker; team T1; agent W.\n"

// A step aimem did not commit ends the turn green, with its messages left
// for the next turn, which begins the step again (case F1).
func TestWorkerRetriesAnAcceptThatDidNotCommit(t *testing.T) {
	f := newFake(t)
	f.inbox = offerInbox()
	f.steps["accept"] = []stepReply{notCommitted("transfer")}
	var out bytes.Buffer
	if code := turnRun(context.Background(), f.home, strings.NewReader(offeredBrief), &out, f.member()); code != 0 {
		t.Fatalf("turn 1 = %d: %s", code, out.String())
	}
	if f.called("aicrew-agent inbox --ack") {
		t.Fatalf("turn 1 acked the offer it did not accept: %v", f.calls)
	}
	if doc := readHandoff(t, f.home); !strings.Contains(doc,
		"The step accept did not commit (not_committed not_committed); 1 message(s) left unacknowledged") {
		t.Fatalf("handoff:\n%s", doc)
	}
	f.calls = nil
	out.Reset()
	if code := turnRun(context.Background(), f.home, strings.NewReader(offeredBrief), &out, f.member()); code != 0 {
		t.Fatalf("turn 2 = %d: %s", code, out.String())
	}
	if !f.called("aicrew-agent step accept") || !f.called("aicrew-agent inbox --ack m1") {
		t.Fatalf("turn 2 did not accept again and ack: %v", f.calls)
	}
}

// A pending step is not built on either.
func TestPendingAcceptIsRetried(t *testing.T) {
	f := newFake(t)
	f.inbox = offerInbox()
	f.steps["accept"] = []stepReply{pending("transfer")}
	var out bytes.Buffer
	if code := turnRun(context.Background(), f.home, strings.NewReader(offeredBrief), &out, f.member()); code != 0 {
		t.Fatalf("turn = %d: %s", code, out.String())
	}
	if f.called("aicrew-agent inbox --ack") || !strings.Contains(readHandoff(t, f.home), "The step accept is pending, undecided;") {
		t.Fatalf("calls %v\n%s", f.calls, readHandoff(t, f.home))
	}
}

// Any other refusal still fails the turn.
func TestOtherRefusalsFailTheTurn(t *testing.T) {
	f := newFake(t)
	f.inbox = offerInbox()
	f.steps["accept"] = []stepReply{stepAnswer(agent.StepRefused, 3, nil, &agent.StepError{Code: "attempt_state", Message: "no"})}
	var out bytes.Buffer
	if code := turnRun(context.Background(), f.home, strings.NewReader(offeredBrief), &out, f.member()); code != 1 ||
		!strings.Contains(out.String(), "attempt_state: no") {
		t.Fatalf("turn = %d: %s", code, out.String())
	}
}

// A step whose begin failed on the channel is finished by step recover, and
// the turn goes on with the recovered step (case F2-begin).
func TestCoordinatorRecoversAFailedOffer(t *testing.T) {
	f := newFake(t)
	f.tasks[tTask] = map[string]any{"id": tTask, "revision": float64(2), "state": "READY", "next_action": offerNext}
	ref := map[string]string{"hub_id": "H1", "project_id": "pilot", "task_id": tTask}
	f.inbox = []map[string]any{{"id": "m1", "kind": "action.needed", "text": "Task created.",
		"action": map[string]any{"subjects": []map[string]any{{"kind": "board", "task": ref}}}}}
	f.members = teamOf(tWorker)
	f.steps["offer"] = []stepReply{stepAnswer(agent.StepFailed, 1, nil,
		&agent.StepError{Code: "unavailable", Message: "EOF", Retryable: true})}
	// An older claim record that did not commit comes first; it is not the
	// offer's, and neither is the other operation's.
	f.steps["recover"] = []stepReply{stepAnswer(agent.StepRefused, 3, []agent.StepResult{
		{AttemptID: "A-old", Operation: "claim", Settled: true, Outcome: "not_committed"},
		{AttemptID: "A-old2", Operation: "update", Settled: true, Outcome: "committed"},
		{AttemptID: tAttempt, Operation: "claim", Settled: true, Outcome: "committed"},
	}, nil)}
	var out bytes.Buffer
	if code := turnRun(context.Background(), f.home, strings.NewReader(coordinatorBrief), &out, f.member()); code != 0 {
		t.Fatalf("turn = %d: %s", code, out.String())
	}
	doc := readHandoff(t, f.home)
	if !strings.Contains(doc, "Step offer failed (unavailable); step recover finished it: committed.") ||
		!strings.Contains(doc, "Offered task "+tTask+" to "+tWorker+": attempt "+tAttempt+".") {
		t.Fatalf("handoff:\n%s", doc)
	}
	if !f.called("aicrew-agent inbox --ack m1") {
		t.Fatalf("calls %v", f.calls)
	}
}

// step recover that finishes no step of the operation fails the turn.
func TestRecoverWithoutTheStepFailsTheTurn(t *testing.T) {
	f := newFake(t)
	f.steps["finalize"] = []stepReply{stepAnswer(agent.StepFailed, 1, nil,
		&agent.StepError{Code: "unavailable", Message: "EOF", Retryable: true})}
	f.steps["recover"] = []stepReply{stepAnswer(agent.StepDone, 0, []agent.StepResult{}, nil)}
	_, err := f.member().step(context.Background(), "finalize", tAttempt, tTask, map[string]any{"result_seq": 1})
	if err == nil || !strings.Contains(err.Error(), "finished no finalize step") {
		t.Fatalf("err = %v", err)
	}
	f.steps["finalize"] = []stepReply{stepAnswer(agent.StepFailed, 1, nil,
		&agent.StepError{Code: "unavailable", Message: "EOF", Retryable: true})}
	f.steps["recover"] = []stepReply{stepAnswer(agent.StepPending, 4, []agent.StepResult{
		{AttemptID: tAttempt, Operation: "finalize", Report: agent.StepReport{Outcome: "unknown"}}}, nil)}
	_, err = f.member().step(context.Background(), "finalize", tAttempt, tTask, map[string]any{"result_seq": 1})
	if later, ok := err.(*retryLater); !ok || !later.pending {
		t.Fatalf("err = %v", err)
	}
}

// step recover finishes the recorded steps of every attempt: a committed
// step of the same operation from an older attempt of the task is not the
// failed step's, whose own did not commit.
func TestRecoverTakesTheStepsOwnAttempt(t *testing.T) {
	f := newFake(t)
	f.steps["finalize"] = []stepReply{stepAnswer(agent.StepFailed, 1, nil,
		&agent.StepError{Code: "unavailable", Message: "EOF", Retryable: true})}
	f.steps["recover"] = []stepReply{stepAnswer(agent.StepRefused, 3, []agent.StepResult{
		{AttemptID: "A-old", Operation: "finalize", Settled: true, Outcome: "committed"},
		{AttemptID: tAttempt, Operation: "finalize", Settled: true, Outcome: "not_committed",
			Report: agent.StepReport{Code: "conflict"}},
	}, nil)}
	ans, err := f.member().step(context.Background(), "finalize", tAttempt, tTask, map[string]any{"result_seq": 1})
	later, ok := err.(*retryLater)
	if !ok || later.pending || !strings.Contains(later.why, "did not commit after step recover") {
		t.Fatalf("err = %v", err)
	}
	var res agent.StepResult
	if json.Unmarshal(ans.Result, &res) != nil || res.AttemptID != tAttempt {
		t.Fatalf("answer = %s", ans.Result)
	}
}

// A submit aimem leaves undecided is recorded with its request key; the next
// turn's submit supersedes it (case F1, update).
func TestWorkerSupersedesAPendingSubmit(t *testing.T) {
	f := newFake(t)
	writeSection(t, f.home)
	doc := strings.Replace(readHandoff(t, f.home), "### Done\n", "### Done\n\n- 2026-10-29T11:00:00Z Pull request #7: "+tPull+
		", opened at head "+tHead+".\n", 1)
	if err := os.WriteFile(filepath.Join(f.home, "docs", "HANDOFF.md"), []byte(doc), 0o600); err != nil {
		t.Fatal(err)
	}
	f.pull = &agent.PullReport{Pull: forge.Pull{Number: 7, URL: tPull, State: "open", HeadCommit: tHead,
		Labels: []string{"hands-reviewed"}}, Content: &forge.PullContent{
		Checks: []forge.Check{{Name: "sim-ci", State: forge.ChecksSuccess}},
		Comments: []forge.PullComment{{Author: "sim-hands", Review: &forge.ReviewHeader{Reviewer: forge.ReviewerExternal,
			Head: tHead, AtHead: true, Verdict: "READY_FOR_HUMAN_MERGE"}}}}}
	f.steps["work"] = []stepReply{pending("update")}
	var out bytes.Buffer
	if code := turnRun(context.Background(), f.home, strings.NewReader(workerBrief("running", "working")), &out, f.member()); code != 0 {
		t.Fatalf("turn 1 = %d: %s", code, out.String())
	}
	if _, ok := f.bodies["work"]["supersedes"]; ok {
		t.Fatalf("the first submit superseded: %v", f.bodies["work"])
	}
	doc = readHandoff(t, f.home)
	if !strings.Contains(doc, "Submit of #7 is pending, undecided: request key k-pending.") || pendingSubmit(doc) != "k-pending" {
		t.Fatalf("handoff:\n%s", doc)
	}
	f.calls = nil
	out.Reset()
	if code := turnRun(context.Background(), f.home, strings.NewReader(workerBrief("running", "working")), &out, f.member()); code != 0 {
		t.Fatalf("turn 2 = %d: %s", code, out.String())
	}
	if got := f.bodies["work"]; got["supersedes"] != "k-pending" || got["intent"] != "submit" || got["detail"] != tPull {
		t.Fatalf("turn 2's submit: %v", got)
	}
	doc = readHandoff(t, f.home)
	if !strings.Contains(doc, "The submit superseded the pending one, request key k-pending.") || pendingSubmit(doc) != "" {
		t.Fatalf("handoff:\n%s", doc)
	}
}

func TestPendingSubmit(t *testing.T) {
	for sec, want := range map[string]string{
		"":                         "",
		"- t Pull request #7: x\n": "",
		"- t Submit of #7 is pending, undecided: request key k1.\n":                                                          "k1",
		"- t Submit of #7 is pending, undecided: request key k1.\n- t Submitted #7 (u): ok\n":                                "",
		"- t Submitted #7 (u): ok\n- t Submit of #7 is pending, undecided: request key k2.\n":                                "k2",
		"- t Submit of #7 is pending, undecided: request key k1.\n- t Submit of #7 is pending, undecided: request key k3.\n": "k3",
	} {
		if got := pendingSubmit(sec); got != want {
			t.Errorf("%q: %q, want %q", sec, got, want)
		}
	}
}
