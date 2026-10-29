package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/BlackVS/aicrew/internal/agent"
	"github.com/BlackVS/aicrew/internal/forge"
)

const (
	tAttempt = "01a10000-0000-7000-8000-00000000a001"
	tTask    = "01a10000-0000-7000-8000-00000000b001"
	tWorker  = "01a10000-0000-7000-8000-00000000c001"
	tBase    = "1111111111111111111111111111111111111111"
	tHead    = "2222222222222222222222222222222222222222"
	tRepo    = "https://sim-gitea:3000/operator/pilot.git"
	tPull    = "https://sim-gitea:3000/operator/pilot/pulls/7"
)

// fakeTools answers the scripted member's commands and MCP calls from
// recorded answers, and records every call.
type fakeTools struct {
	t        *testing.T
	home     string
	inbox    []map[string]any
	attempts []agent.OpenAttempt
	pull     *agent.PullReport // what forge pr show answers
	// labelFails makes forge pr label fail.
	labelFails bool
	tasks      map[string]map[string]any
	calls      []string
	bodies     map[string]map[string]any // step bodies by op
	stdin      map[string]string         // stdin by command
}

func newFake(t *testing.T) *fakeTools {
	return &fakeTools{t: t, home: t.TempDir(), tasks: map[string]map[string]any{},
		bodies: map[string]map[string]any{}, stdin: map[string]string{}}
}

func (f *fakeTools) member() *member {
	return &member{home: f.home, in: f.home, run: f.run, mcp: f.mcp,
		now: func() time.Time { return time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC) }}
}

func answer(result any) []byte {
	raw, _ := json.Marshal(result)
	out, _ := json.Marshal(agent.StepAnswer{OK: true, Status: "done", Result: raw})
	return out
}

func flagOf(args []string, name string) string {
	for i := range args[:max(len(args)-1, 0)] {
		if args[i] == name {
			return args[i+1]
		}
	}
	return ""
}

func (f *fakeTools) run(_ context.Context, dir, stdin, name string, args ...string) ([]byte, int, error) {
	line := name + " " + strings.Join(args, " ")
	if name == "aicrew-agent" && args[0] == "step" {
		line = name + " step " + args[1]
		var body map[string]any
		if b := flagOf(args, "--body"); b != "" {
			if err := json.Unmarshal([]byte(b), &body); err != nil {
				f.t.Fatalf("step %s: body %q: %v", args[1], b, err)
			}
		}
		f.bodies[args[1]] = body
		if r := flagOf(args, "--repository"); r != "" {
			line += " --repository " + r
		}
	}
	f.calls = append(f.calls, line)
	if stdin != "" {
		f.stdin[line] = stdin
	}
	if name == "git" {
		return nil, 0, nil
	}
	switch {
	case args[0] == "inbox" && len(args) > 1 && args[1] == "--ack":
		return nil, 0, nil
	case args[0] == "inbox":
		return answer(map[string]any{"messages": f.inbox}), 0, nil
	case args[0] == "attempts":
		return answer(agent.OpenAttempts{Attempts: f.attempts}), 0, nil
	case args[0] == "step" && args[1] == "offer":
		return answer(map[string]string{"attempt_id": tAttempt, "request_key": "k1"}), 0, nil
	case args[0] == "step":
		return answer(map[string]string{"attempt_id": flagOf(args, "--attempt")}), 0, nil
	case args[0] == "clone":
		if err := os.MkdirAll(filepath.Join(f.home, "worktrees", flagOf(args, "--attempt")), 0o755); err != nil {
			f.t.Fatal(err)
		}
		return []byte(`{}`), 0, nil
	case args[0] == "forge" && args[2] == "open":
		out, _ := json.Marshal(agent.PullReport{Pull: forge.Pull{Number: 7, URL: tPull, State: "open", HeadCommit: tHead}})
		return out, 0, nil
	case args[0] == "forge" && args[2] == "label" && f.labelFails:
		return []byte("forge pr label: the forge refused"), 1, nil
	case args[0] == "forge" && args[2] == "label":
		out, _ := json.Marshal(agent.PullReport{Pull: forge.Pull{Number: 7, URL: tPull, Labels: []string{"review-this"}}})
		return out, 0, nil
	case args[0] == "forge" && args[2] == "show":
		out, _ := json.Marshal(f.pull)
		return out, 0, nil
	}
	f.t.Fatalf("unexpected command: %s (in %s)", line, dir)
	return nil, 0, nil
}

// seen holds the idempotency keys triage_task took, across a test's turns.
var seen = map[string]bool{}

func (f *fakeTools) mcp(_ context.Context, tool string, args map[string]any) (string, error) {
	f.calls = append(f.calls, "mcp "+tool)
	switch tool {
	case "get_task":
		t, ok := f.tasks[args["id"].(string)]
		if !ok {
			return "", fmt.Errorf("no task %v", args["id"])
		}
		raw, _ := json.Marshal(t)
		return string(raw), nil
	case "triage_task":
		t := f.tasks[args["id"].(string)]
		if t["revision"] != float64(args["expected_revision"].(int64)) {
			return "", fmt.Errorf("stale revision %v", args["expected_revision"])
		}
		if seen[args["idempotency_key"].(string)] {
			return "", fmt.Errorf("idempotency key already used")
		}
		seen[args["idempotency_key"].(string)] = true
		t["state"], t["revision"], t["next_action"] = args["state"], t["revision"].(float64)+1, args["next_action"]
		raw, _ := json.Marshal(t)
		return string(raw), nil
	case "add_task_comment":
		return `{"sequence":1}`, nil
	case "project_repository":
		return `{"project":"pilot","repository":{"kind":"gitea","url":"` + tRepo + `","access":"write"}}`, nil
	}
	return "", fmt.Errorf("unexpected tool %s", tool)
}

func (f *fakeTools) called(prefix string) bool {
	for _, c := range f.calls {
		if strings.HasPrefix(c, prefix) {
			return true
		}
	}
	return false
}

func workerBrief(state, phase string) string {
	return "/crew-turn Attempt " + tAttempt + ": " + state + "/" + phase + ".\n\n## Who and where\n\n" +
		"- Role: worker; team T1; agent " + tWorker + ".\n- Home: /home/member/agent.\n" +
		"- Attempt " + tAttempt + " on task " + tTask + " (project pilot, hub H1).\n" +
		"- Branch work/" + tTask + " from base commit " + tBase + "; worktree `worktrees/" + tAttempt + "`.\n" +
		"- Repository " + tRepo + " (default branch main).\n\n## The task\n\n..."
}

const coordinatorBrief = "/crew-turn No open attempt.\n\n## Who and where\n\n- Role: coordinator; team T1; agent A0.\n" +
	"- Home: /home/member/agent.\n\n## The task\n\nNo task: no open attempt.\n"

// section is the launcher's CURRENT ATTEMPT section as it opens it.
func writeSection(t *testing.T, home string) {
	t.Helper()
	doc := "# Handoff\n\nThe agent's header.\n\n## CURRENT ATTEMPT\n\nAttempt `" + tAttempt + "` on task " + tTask +
		".\n\n### Done\n\n### Decisions\n\n### Next\n\n### Open questions\n\n## Archive\n\n### Attempt X (archived)\nold\n"
	if err := os.MkdirAll(filepath.Join(home, "docs"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, "docs", "HANDOFF.md"), []byte(doc), 0o600); err != nil {
		t.Fatal(err)
	}
}

func readHandoff(t *testing.T, home string) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(home, "docs", "HANDOFF.md"))
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

// lastResult is the stream's last event, which must be the result.
func lastResult(t *testing.T, out string) map[string]any {
	t.Helper()
	lines := strings.Split(strings.TrimSpace(out), "\n")
	var ev map[string]any
	if err := json.Unmarshal([]byte(lines[len(lines)-1]), &ev); err != nil || ev["type"] != "result" {
		t.Fatalf("the last line is not a result event: %q", lines[len(lines)-1])
	}
	return ev
}

func TestParseBrief(t *testing.T) {
	b, err := parseBrief(workerBrief("running", "working"))
	want := brief{Attempt: tAttempt, State: "running", Phase: "working", Role: "worker", Team: "T1", Agent: tWorker,
		Task: tTask, Project: "pilot", Hub: "H1", Branch: "work/" + tTask, Base: tBase, Repo: tRepo}
	if err != nil || b != want {
		t.Fatalf("parseBrief = %+v, %v; want %+v", b, err, want)
	}
	b, err = parseBrief(coordinatorBrief)
	if err != nil || b != (brief{Role: "coordinator", Team: "T1", Agent: "A0"}) {
		t.Fatalf("parseBrief(coordinator) = %+v, %v", b, err)
	}
	for name, in := range map[string]string{
		"not a turn":      "hello",
		"no role":         "/crew-turn No open attempt.\n\n## Who and where\n",
		"first line":      "/crew-turn Something else.\n- Role: worker; team T; agent A.\n",
		"attempt no role": "/crew-turn Attempt X: running/working.\n",
	} {
		if _, err := parseBrief(in); err == nil {
			t.Errorf("%s: parseBrief accepted %q", name, in)
		}
	}
}

func TestTurnResultLine(t *testing.T) {
	f := newFake(t)
	var out bytes.Buffer
	if code := turnRun(context.Background(), f.home, strings.NewReader(coordinatorBrief), &out, f.member()); code != 0 {
		t.Fatalf("turnRun = %d: %s", code, out.String())
	}
	ev := lastResult(t, out.String())
	if ev["subtype"] != "success" || ev["is_error"] != false || ev["total_cost_usd"] != float64(0) ||
		ev["result"] != "Nothing to do." {
		t.Fatalf("result = %v", ev)
	}
	// A brief the script cannot read fails the turn with an error result.
	out.Reset()
	if code := turnRun(context.Background(), f.home, strings.NewReader("hello"), &out, f.member()); code == 0 {
		t.Fatal("turnRun accepted a prompt that is no turn")
	}
	if ev := lastResult(t, out.String()); ev["is_error"] != true || ev["subtype"] != "error_during_execution" {
		t.Fatalf("result = %v", ev)
	}
}

func TestWorkerAcceptsTheOffer(t *testing.T) {
	f := newFake(t)
	f.inbox = []map[string]any{
		{"id": "m1", "kind": "lifecycle", "attempt_id": tAttempt,
			"task":  map[string]string{"hub_id": "H1", "project_id": "pilot", "task_id": tTask},
			"offer": map[string]any{"instruction_digest": "sha256:abc"}, "text": "An offer."},
		{"id": "m2", "kind": "message", "text": "Unrelated."},
	}
	brief := "/crew-turn Attempt " + tAttempt + ": offered/offered.\n\n## Who and where\n\n- Role: worker; team T1; agent W.\n"
	var out bytes.Buffer
	if code := turnRun(context.Background(), f.home, strings.NewReader(brief), &out, f.member()); code != 0 {
		t.Fatalf("turnRun = %d: %s", code, out.String())
	}
	if got := f.bodies["accept"]; got["instruction_digest"] != "sha256:abc" {
		t.Fatalf("accept body = %v", got)
	}
	if !f.called("aicrew-agent inbox --ack m1,m2") {
		t.Fatalf("the messages were not acked: %v", f.calls)
	}
}

func TestWorkerOpensAndSubmitsThePullRequest(t *testing.T) {
	f := newFake(t)
	writeSection(t, f.home)
	m := f.member()
	var out bytes.Buffer
	// Turn 1: no worktree, no pull request: clone, commit, push, open, review-this.
	if code := turnRun(context.Background(), f.home, strings.NewReader(workerBrief("running", "working")), &out, m); code != 0 {
		t.Fatalf("turn 1 = %d: %s", code, out.String())
	}
	for _, want := range []string{
		"aicrew-agent clone --repository " + tRepo + " --attempt " + tAttempt + " --base " + tBase + " --branch work/" + tTask,
		"git add sim/" + tTask + ".md", "git push -q origin work/" + tTask,
		"aicrew-agent forge pr open --title sim: task " + tTask + " --body-file -",
		"aicrew-agent forge pr label add --pr 7 --json review-this",
	} {
		if !f.called(want) {
			t.Fatalf("turn 1 did not run %q: %v", want, f.calls)
		}
	}
	if f.called("aicrew-agent step") {
		t.Fatalf("turn 1 made a step: %v", f.calls)
	}
	doc := readHandoff(t, f.home)
	if recordedPull(doc) != 7 || !strings.Contains(doc, "### Next\n\nTurn of 2026-10-29T12:00:00Z: Asked for the external review of #7") ||
		!strings.HasSuffix(doc, "## Archive\n\n### Attempt X (archived)\nold\n") || !strings.HasPrefix(doc, "# Handoff\n") {
		t.Fatalf("handoff after turn 1:\n%s", doc)
	}

	// Turn 2: the review is in flight: wait, with no step, and a changed section.
	f.calls = nil
	f.pull = &agent.PullReport{Pull: forge.Pull{Number: 7, URL: tPull, State: "open", HeadCommit: tHead,
		Labels: []string{"hands-reviewing"}}, Content: &forge.PullContent{}}
	m = f.member()
	m.now = func() time.Time { return time.Date(2026, 10, 29, 12, 0, 10, 0, time.UTC) }
	before := readHandoff(t, f.home)
	out.Reset()
	if code := turnRun(context.Background(), f.home, strings.NewReader(workerBrief("running", "working")), &out, m); code != 0 {
		t.Fatalf("turn 2 = %d: %s", code, out.String())
	}
	if f.called("aicrew-agent step") || f.called("aicrew-agent clone") || f.called("git ") {
		t.Fatalf("turn 2 acted while the review runs: %v", f.calls)
	}
	if after := readHandoff(t, f.home); after == before || !strings.Contains(after, "Waiting on the external review of #7") {
		t.Fatalf("turn 2 left the section unchanged or did not say it waits:\n%s", after)
	}

	// Turn 3: a READY review at an older head does not count.
	f.calls = nil
	review := func(head string, atHead bool, verdict string) forge.PullComment {
		return forge.PullComment{Author: "sim-hands", Review: &forge.ReviewHeader{Reviewer: forge.ReviewerExternal, Head: head,
			AtHead: atHead, Verdict: verdict}}
	}
	f.pull = &agent.PullReport{Pull: forge.Pull{Number: 7, URL: tPull, State: "open", HeadCommit: tHead,
		Labels: []string{"hands-reviewed"}}, Content: &forge.PullContent{
		Checks:   []forge.Check{{Name: "sim-ci", State: forge.ChecksSuccess}},
		Comments: []forge.PullComment{review(tBase, false, "READY_FOR_HUMAN_MERGE")}}}
	out.Reset()
	if code := turnRun(context.Background(), f.home, strings.NewReader(workerBrief("running", "working")), &out, f.member()); code != 0 {
		t.Fatalf("turn 3 = %d: %s", code, out.String())
	}
	if f.called("aicrew-agent step") {
		t.Fatalf("turn 3 submitted on a review of another head: %v", f.calls)
	}

	// Turn 4: READY at the head and the checks passed: submit the pull request.
	f.calls = nil
	f.pull.Content.Comments = append(f.pull.Content.Comments, review(tHead, true, "READY_FOR_HUMAN_MERGE"))
	out.Reset()
	if code := turnRun(context.Background(), f.home, strings.NewReader(workerBrief("running", "working")), &out, f.member()); code != 0 {
		t.Fatalf("turn 4 = %d: %s", code, out.String())
	}
	if got := f.bodies["work"]; got["intent"] != "submit" || got["detail"] != tPull {
		t.Fatalf("work body = %v; calls %v", got, f.calls)
	}
}

func TestWorkerRequestsTheReviewAfterAFailedTurn(t *testing.T) {
	f := newFake(t)
	writeSection(t, f.home)
	f.labelFails = true
	var out bytes.Buffer
	// The label call fails after the pull request is open: the turn fails,
	// and its handoff still records the pull request.
	if code := turnRun(context.Background(), f.home, strings.NewReader(workerBrief("running", "working")), &out, f.member()); code == 0 {
		t.Fatalf("the turn did not fail: %s", out.String())
	}
	if recordedPull(readHandoff(t, f.home)) != 7 {
		t.Fatalf("the failed turn did not record its pull request:\n%s", readHandoff(t, f.home))
	}
	// The next turn finds no review requested and asks for it, opening nothing.
	f.labelFails, f.calls = false, nil
	f.pull = &agent.PullReport{Pull: forge.Pull{Number: 7, URL: tPull, State: "open", HeadCommit: tHead},
		Content: &forge.PullContent{}}
	out.Reset()
	if code := turnRun(context.Background(), f.home, strings.NewReader(workerBrief("running", "working")), &out, f.member()); code != 0 {
		t.Fatalf("the next turn = %d: %s", code, out.String())
	}
	if !f.called("aicrew-agent forge pr label add --pr 7 --json review-this") || f.called("aicrew-agent forge pr open") ||
		f.called("git ") {
		t.Fatalf("the next turn did not just ask for the review: %v", f.calls)
	}
}

func TestPullGates(t *testing.T) {
	at := func(v string) forge.PullComment {
		return forge.PullComment{Review: &forge.ReviewHeader{Reviewer: forge.ReviewerExternal, AtHead: true, Verdict: v}}
	}
	local := forge.PullComment{Review: &forge.ReviewHeader{Reviewer: forge.ReviewerLocal, AtHead: true,
		Verdict: "READY_FOR_HUMAN_MERGE"}}
	for name, c := range map[string]struct {
		content        *forge.PullContent
		verdict, check string
	}{
		"nothing":      {nil, "", ""},
		"no checks":    {&forge.PullContent{Comments: []forge.PullComment{at("READY_FOR_HUMAN_MERGE")}}, "READY_FOR_HUMAN_MERGE", ""},
		"local only":   {&forge.PullContent{Comments: []forge.PullComment{local}}, "", ""},
		"latest wins":  {&forge.PullContent{Comments: []forge.PullComment{at("READY_FOR_HUMAN_MERGE"), at("RETURN_TO_IMPLEMENTATION")}}, "RETURN_TO_IMPLEMENTATION", ""},
		"one pending":  {&forge.PullContent{Checks: []forge.Check{{State: forge.ChecksSuccess}, {State: forge.ChecksPending}}}, "", forge.ChecksPending},
		"one failed":   {&forge.PullContent{Checks: []forge.Check{{State: forge.ChecksPending}, {State: forge.ChecksFailure}}}, "", forge.ChecksFailure},
		"all passed":   {&forge.PullContent{Checks: []forge.Check{{State: forge.ChecksSuccess}, {State: forge.ChecksSuccess}}}, "", forge.ChecksSuccess},
		"pending last": {&forge.PullContent{Checks: []forge.Check{{State: forge.ChecksSuccess}, {State: "none"}}}, "", forge.ChecksPending},
	} {
		if v, ch := pullGates(c.content); v != c.verdict || ch != c.check {
			t.Errorf("%s: pullGates = %q, %q; want %q, %q", name, v, ch, c.verdict, c.check)
		}
	}
}

func TestCoordinatorTriagesAndOffers(t *testing.T) {
	f := newFake(t)
	f.tasks[tTask] = map[string]any{"id": tTask, "revision": float64(1), "state": "BACKLOG"}
	other := "01a10000-0000-7000-8000-00000000b002"
	f.attempts = []agent.OpenAttempt{{ID: "A9", Task: agent.TaskRef{TaskID: other}}}
	ref := map[string]string{"hub_id": "H1", "project_id": "pilot", "task_id": tTask}
	f.inbox = []map[string]any{
		{"id": "m1", "kind": "action.needed", "text": "Task created.",
			"action": map[string]any{"subjects": []map[string]any{{"kind": "board", "task": ref}}}},
		{"id": "m2", "kind": "lifecycle", "project": map[string]string{"hub_id": "H1", "project_id": "pilot"},
			"text": "2 READY tasks are in project pilot: " + other + ", " + tTask + "."},
	}
	in := `{"worker_agent_id":"` + tWorker + `","process":{"repo":"https://sim-gitea:3000/operator/process.git",` +
		`"commit":"` + tBase + `","manifest":"process/manifest.json"},"instruction_digest":"sha256:abc"}`
	if err := os.WriteFile(filepath.Join(f.home, "offer.json"), []byte(in), 0o600); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if code := turnRun(context.Background(), f.home, strings.NewReader(coordinatorBrief), &out, f.member()); code != 0 {
		t.Fatalf("turnRun = %d: %s", code, out.String())
	}
	if f.tasks[tTask]["state"] != "READY" || !f.called("mcp add_task_comment") {
		t.Fatalf("the task was not triaged: %v; calls %v", f.tasks[tTask], f.calls)
	}
	body := f.bodies["offer"]
	task, _ := body["task"].(map[string]any)
	proc, _ := body["process"].(map[string]any)
	repo, _ := body["repository"].(map[string]any)
	if body["worker_agent_id"] != tWorker || body["expected_revision"] != float64(2) || task["task_id"] != tTask ||
		task["hub_id"] != "H1" || proc["commit"] != tBase || body["instruction_digest"] != "sha256:abc" ||
		repo["branch"] != "work/"+tTask || repo["access"] != "write" || body["expires_at"] != "2026-10-29T14:00:00Z" {
		t.Fatalf("offer body = %v", body)
	}
	if !f.called("aicrew-agent step offer --repository " + tRepo) {
		t.Fatalf("the offer did not name the project's repository: %v", f.calls)
	}
	offers := 0
	for _, c := range f.calls {
		if strings.HasPrefix(c, "aicrew-agent step offer") {
			offers++
		}
	}
	if offers != 1 || strings.Contains(strings.Join(f.calls, "\n"), other) {
		t.Fatalf("the task with an open attempt was read or offered: %v", f.calls)
	}
	if !f.called("aicrew-agent inbox --ack m1,m2") {
		t.Fatalf("the messages were not acked: %v", f.calls)
	}
	if doc := readHandoff(t, f.home); !strings.Contains(doc, "- 2026-10-29T12:00:00Z Offered task "+tTask) {
		t.Fatalf("the coordinator's handoff:\n%s", doc)
	}
}

func TestCoordinatorTriagesAReadyTask(t *testing.T) {
	f := newFake(t)
	f.tasks[tTask] = map[string]any{"id": tTask, "revision": float64(3), "state": "READY"}
	ref := map[string]string{"hub_id": "H1", "project_id": "pilot", "task_id": tTask}
	f.inbox = []map[string]any{{"id": "m1", "kind": "action.needed", "text": "Task is READY.",
		"action": map[string]any{"subjects": []map[string]any{{"kind": "board", "task": ref}}}}}
	in := `{"worker_agent_id":"` + tWorker + `","process":{},"instruction_digest":"sha256:abc"}`
	if err := os.WriteFile(filepath.Join(f.home, "offer.json"), []byte(in), 0o600); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if code := turnRun(context.Background(), f.home, strings.NewReader(coordinatorBrief), &out, f.member()); code != 0 {
		t.Fatalf("turnRun = %d: %s", code, out.String())
	}
	if !f.called("mcp triage_task") || f.bodies["offer"]["expected_revision"] != float64(4) {
		t.Fatalf("the READY task was not triaged before its offer: %v; offer %v", f.calls, f.bodies["offer"])
	}
	// A turn that failed after the triage is retried: the task is triaged
	// already, so the retry offers it without a second triage.
	f.calls = nil
	out.Reset()
	if code := turnRun(context.Background(), f.home, strings.NewReader(coordinatorBrief), &out, f.member()); code != 0 {
		t.Fatalf("the retried turn = %d: %s", code, out.String())
	}
	if f.called("mcp triage_task") || !f.called("aicrew-agent step offer") {
		t.Fatalf("the retry triaged again or did not offer: %v", f.calls)
	}
}

func TestCoordinatorLeavesOtherStates(t *testing.T) {
	f := newFake(t)
	f.tasks[tTask] = map[string]any{"id": tTask, "revision": float64(3), "state": "IN_PROGRESS"}
	ref := map[string]string{"hub_id": "H1", "project_id": "pilot", "task_id": tTask}
	f.inbox = []map[string]any{{"id": "m1", "kind": "action.needed", "text": "Task moved.",
		"action": map[string]any{"subjects": []map[string]any{{"kind": "board", "task": ref}}}}}
	var out bytes.Buffer
	if code := turnRun(context.Background(), f.home, strings.NewReader(coordinatorBrief), &out, f.member()); code != 0 {
		t.Fatalf("turnRun = %d: %s", code, out.String())
	}
	if f.called("mcp triage_task") || f.called("aicrew-agent step") {
		t.Fatalf("a task in progress was triaged or offered: %v", f.calls)
	}
}

func TestEditSectionAddsMissingHeadings(t *testing.T) {
	got := editSection("## CURRENT ATTEMPT\n\nAttempt `A`.\n", "- did\n", "next")
	want := "## CURRENT ATTEMPT\n\nAttempt `A`.\n\n### Done\n\n- did\n\n### Next\n\nnext\n"
	if got != want {
		t.Fatalf("editSection =\n%q\nwant\n%q", got, want)
	}
}
