package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/BlackVS/aicrew/internal/automation"
	"github.com/BlackVS/aicrew/internal/forge"
	"github.com/BlackVS/aicrew/internal/store"
)

// childReport is where the test binary, run as a script's process, writes
// what it was given: in its working directory, the launcher's private
// state directory.
const childReport = "automation-child.json"

// automationChildMain is the test binary as `aicrew-agent automation run`.
// It records its arguments, its environment and its request, then runs as
// the real command does.
func automationChildMain() int {
	in, _ := io.ReadAll(io.LimitReader(os.Stdin, 4<<20))
	b, _ := json.Marshal(map[string]any{"args": os.Args[1:], "env": os.Environ(), "stdin": string(in)})
	_ = os.WriteFile(childReport, b, 0o600)
	return AutomationChild(context.Background(), bytes.NewReader(in), os.Stdout, os.Stderr)
}

// automationForge is the forge of the run's pull request.
type automationForge struct {
	mu       sync.Mutex
	token    string
	pull     forge.Pull
	content  forge.PullContent
	comments []string
}

func (f *automationForge) GetPullContent(_ context.Context, host string, k forge.Kind, token, path string, n int64) (forge.Pull, forge.PullContent, error) {
	if token != f.token || host != "git.example.test" || k != forge.Gitea || path != "crew/project-t" || n != f.pull.Number {
		return forge.Pull{}, forge.PullContent{}, forge.ErrNotFound
	}
	return f.pull, f.content, nil
}

func (f *automationForge) CommentPull(_ context.Context, host string, k forge.Kind, token, path string, n int64, body string) (forge.Comment, error) {
	if token != f.token || host != "git.example.test" || n != f.pull.Number {
		return forge.Comment{}, forge.ErrNotFound
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.comments = append(f.comments, body)
	return forge.Comment{ID: 1}, nil
}

// automationEnv is a member's launcher with a claimed attempt, its real
// worktree, a forge credential and the forge of its pull request.
type automationEnv struct {
	*stepEnv
	srv      *StepServer
	id       string
	head     string
	worktree string
	forge    *automationForge
}

const automationForgeToken = "member-forge-token-automation"

func setupAutomation(t *testing.T) *automationEnv {
	t.Helper()
	s := setupStepsWith(t, crewOptions{role: store.RoleIndependent, steps: true, shortHome: true})
	id := s.claimed(t)
	provisionTestForge(t, s.cfg.Home, "git.example.test", automationForgeToken)
	srv := s.serve(t)

	// The worktree: the pull request's head, and an origin/main that has
	// moved on since, so the pull request is behind its base.
	wt := filepath.Join(s.cfg.Home, "worktrees", id)
	if err := os.MkdirAll(wt, 0o700); err != nil {
		t.Fatal(err)
	}
	git(t, wt, "init", "-q", "-b", "work/task-1")
	git(t, wt, "remote", "add", "origin", "https://git.example.test/crew/project-t.git")
	os.WriteFile(filepath.Join(wt, "a.txt"), []byte("a\n"), 0o600)
	git(t, wt, "add", "a.txt")
	git(t, wt, "commit", "-q", "-m", "work")
	head := git(t, wt, "rev-parse", "HEAD")
	os.WriteFile(filepath.Join(wt, "b.txt"), []byte("b\n"), 0o600)
	git(t, wt, "add", "b.txt")
	git(t, wt, "commit", "-q", "-m", "base moved")
	git(t, wt, "update-ref", "refs/remotes/origin/main", "HEAD")
	git(t, wt, "reset", "-q", "--hard", head)

	f := &automationForge{token: automationForgeToken,
		pull: forge.Pull{Number: 7, State: forge.PullOpen, Draft: true, Head: "work/task-1", HeadCommit: head, Base: "main",
			Labels: []string{"review-this"}, Mergeable: true},
		content: forge.PullContent{
			Checks: []forge.Check{{Name: "go-test", State: forge.ChecksFailure, Log: &forge.JobLog{Excerpt: "FAIL TestX"}},
				{Name: "lint", State: forge.ChecksSuccess}},
			Comments: []forge.PullComment{
				{Body: "an older local review", Review: &forge.ReviewHeader{Reviewer: forge.ReviewerLocal, Head: "old"}},
				{Body: "[hands-bot review] reviewed at head " + head + "\n\nBLOCKERS\n- [BLOCKER] [CONFIRMED] the parser drops a line\n" +
					"\nOBSERVATIONS\n- [OBSERVATION] fine\n\nVERDICT\nRETURN_TO_IMPLEMENTATION",
					Review: &forge.ReviewHeader{Reviewer: forge.ReviewerExternal, Head: head, AtHead: true, Verdict: "RETURN_TO_IMPLEMENTATION"}},
				{Body: "the newest local review", Review: &forge.ReviewHeader{Reviewer: forge.ReviewerLocal, Head: head, AtHead: true,
					Verdict: "READY_FOR_HUMAN_MERGE"}},
			}}}
	old := newAutomationForge
	newAutomationForge = func() AutomationForge { return f }
	t.Cleanup(func() { newAutomationForge = old })

	st := readTurnState(s.cfg.Home)
	st.attempt(id).CostUSD = 1.5
	if err := writeTurnState(s.cfg.Home, st); err != nil {
		t.Fatal(err)
	}
	return &automationEnv{stepEnv: s, srv: srv, id: id, head: head, worktree: wt, forge: f}
}

func (a *automationEnv) run(t *testing.T, src string, r AutomationRun) AutomationResult {
	t.Helper()
	r.AttemptID, r.Source = a.id, []byte(src)
	if r.Name == "" {
		r.Name = "test.star"
	}
	if r.Pull == 0 {
		r.Pull = 7
	}
	res, err := a.srv.RunAutomation(context.Background(), r)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	return res
}

// records are the home's automation records of a run.
func (a *automationEnv) records(t *testing.T, run string) []automationRecord {
	t.Helper()
	raw, _ := os.ReadFile(filepath.Join(a.cfg.Home, filepath.FromSlash(AutomationLogFile)))
	var out []automationRecord
	for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		var r automationRecord
		if json.Unmarshal([]byte(line), &r) == nil && r.Run == run {
			out = append(out, r)
		}
	}
	return out
}

// Every host call of 10.3 reaches the launcher over the step socket from
// the script's own process: pr() from the forge with the home's
// credential, attempt() from the session status, run() in the attempt's
// worktree, step() into the home's log and comment() on the pull request.
// The turn the script returns comes back to the launcher.
func TestAutomationHostCalls(t *testing.T) {
	a := setupAutomation(t)
	src := fmt.Sprintf(`
def main(event):
    p = pr()
    want = (7, %q, "main", "work/task-1", True, True, True, ["review-this"])
    got = (p.number, p.head, p.base, p.branch, p.behind, p.draft, p.mergeable, p.labels)
    if got != want:
        fail("pr: %%s" %% (got,))
    if len(p.reviews) != 2:
        fail("reviews: %%s" %% p.reviews)
    local, external = p.reviews[0], p.reviews[1]
    if local.reviewer != "local" or local.body != "the newest local review" or local.verdict != "READY_FOR_HUMAN_MERGE":
        fail("local review: %%s" %% local)
    if external.reviewer != "external" or external.head != p.head or external.verdict != "RETURN_TO_IMPLEMENTATION":
        fail("external review: %%s" %% external)
    if external.findings != ["[BLOCKER] [CONFIRMED] the parser drops a line", "[OBSERVATION] fine"]:
        fail("findings: %%s" %% external.findings)
    if [(c.name, c.conclusion, c.excerpt) for c in p.checks] != [("go-test", "failure", "FAIL TestX"), ("lint", "success", "")]:
        fail("checks: %%s" %% p.checks)
    a = attempt()
    got = (a.id, a.phase, a.task.id, a.base_commit, a.spent_usd, a.budget_usd)
    if got != (%q, "working", "task-1", "base-1", 1.5, 20.0):
        fail("attempt: %%s" %% (got,))
    r = run("git", ["rev-parse", "HEAD"])
    if r.exit != 0 or r.output.strip() != p.head:
        fail("run: %%s" %% r)
    r = run("git", ["rev-parse", "--verify", "no-such-ref"])
    if r.exit == 0 or "fatal" not in r.output:
        fail("a failing command: %%s" %% r)
    step("checked", "the head is " + p.head)
    comment("Base-only update.")
    return turn("fix", "the external review has a blocker")
`, a.head, a.id)
	res := a.run(t, src, AutomationRun{Event: automation.Event{Kinds: []string{"forge.changed"}, IDs: []string{"ev-1"}}})
	if res.Failed != "" || res.Turn == nil || *res.Turn != (automation.Turn{Kind: "fix", Content: "the external review has a blocker"}) {
		t.Fatalf("the run: %+v", res)
	}
	if !slices.Equal(a.forge.comments, []string{"Base-only update."}) {
		t.Errorf("the forge has the comments %q", a.forge.comments)
	}
	recs := a.records(t, res.RunID)
	if len(recs) != 2 || recs[0].Kind != "checked" || recs[0].Text != "the head is "+a.head || recs[0].Attempt != a.id ||
		recs[1].Outcome != "turn" || recs[1].Turn != "fix" || !slices.Equal(recs[1].Events, []string{"ev-1"}) {
		t.Errorf("the records: %+v", recs)
	}
	// The run's token is gone with the run: a host call with it is refused.
	a.srv.runsMu.Lock()
	n := len(a.srv.runs)
	a.srv.runsMu.Unlock()
	if n != 0 {
		t.Errorf("%d runs still live", n)
	}
}

// No credential reaches the script's process: its arguments are the
// command's name, its environment is empty (Windows adds SYSTEMROOT), and
// its request holds no token of the home, the session or the forge.
func TestAutomationChildHoldsNoCredential(t *testing.T) {
	a := setupAutomation(t)
	t.Setenv("AICREW_TEST_SECRET", "secret-in-the-launchers-environment")
	res := a.run(t, "def main(event):\n    pr()\n    attempt()\n", AutomationRun{})
	if res.Failed != "" {
		t.Fatalf("the run: %+v", res)
	}
	raw, err := os.ReadFile(filepath.Join(filepath.Dir(StepSocket(a.cfg.Home)), childReport))
	if err != nil {
		t.Fatal(err)
	}
	var rep struct {
		Args  []string `json:"args"`
		Env   []string `json:"env"`
		Stdin string   `json:"stdin"`
	}
	if err := json.Unmarshal(raw, &rep); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(rep.Args, []string{"automation", "run"}) {
		t.Errorf("the arguments: %q", rep.Args)
	}
	for _, kv := range rep.Env {
		name, _, _ := strings.Cut(kv, "=")
		if runtime.GOOS != "windows" || !strings.EqualFold(name, "SYSTEMROOT") {
			t.Errorf("the environment holds %s", name)
		}
	}
	sess, _ := os.ReadFile(a.e.AimemFile())
	for name, secret := range map[string]string{"the forge token": automationForgeToken, "the launcher's environment": "secret-in-the-launchers-environment",
		"the session file's path": a.e.AimemFile()} {
		if strings.Contains(string(raw), secret) {
			t.Errorf("the child was given %s", name)
		}
	}
	if len(sess) > 0 && strings.Contains(string(raw), strings.TrimSpace(string(sess))) {
		t.Error("the child was given the session file's content")
	}
	var req automationRequest
	if json.Unmarshal([]byte(rep.Stdin), &req) != nil || req.Token == "" || req.Memory != AutomationMemory {
		t.Errorf("the request: %s", rep.Stdin)
	}
}

// A refused command fails the script before the launcher runs it, and the
// failure is a step; the launcher refuses it too whatever the child says,
// and a call without a live run's token.
func TestAutomationRefusedRun(t *testing.T) {
	a := setupAutomation(t)
	res := a.run(t, "def main(event):\n    run(\"curl\", [\"https://example.test\"])\n", AutomationRun{})
	if !strings.Contains(res.Failed, "outside the turn's allow list") || res.Invalid || res.Memory {
		t.Fatalf("the run: %+v", res)
	}
	recs := a.records(t, res.RunID)
	if len(recs) != 2 || recs[0].Kind != stepAutomationFailed || !strings.Contains(recs[0].Text, "curl") || recs[1].Outcome != "failed" {
		t.Errorf("the records: %+v", recs)
	}

	run := &liveRun{ctx: context.Background(), id: "run-x", attempt: a.id, pull: 7, worktree: a.worktree,
		allow: TurnAllowList(a.cfg.Home, nil)}
	a.srv.runsMu.Lock()
	a.srv.runs = map[string]*liveRun{"token-x": run}
	a.srv.runsMu.Unlock()
	call := func(token string, c automationCall) StepAnswer {
		c.Token = token
		body, _ := json.Marshal(c)
		ans, err := CallStep(context.Background(), a.cfg.Home, StepCall{Op: automationOp, Body: body})
		if err != nil {
			t.Fatal(err)
		}
		return ans
	}
	if ans := call("token-x", automationCall{Call: "run", Cmd: "curl", Args: []string{"https://example.test"}}); ans.OK ||
		!strings.Contains(ans.Error.Message, "outside the turn's allow list") {
		t.Errorf("the launcher ran a refused command: %+v", ans)
	}
	if ans := call("token-x", automationCall{Call: "run", Cmd: "git", Args: []string{"rev-parse", "HEAD"}}); !ans.OK {
		t.Errorf("an admitted command: %+v", ans)
	}
	if ans := call("token-y", automationCall{Call: "run", Cmd: "git", Args: []string{"rev-parse", "HEAD"}}); ans.OK ||
		ans.Error.Code != "no_run" {
		t.Errorf("a call without a live run's token: %+v", ans)
	}
	if ans := call("token-x", automationCall{Call: "comment", Text: "leaks " + automationForgeToken}); ans.OK ||
		len(a.forge.comments) != 0 || strings.Contains(ans.Error.Message, automationForgeToken) {
		t.Errorf("a comment holding the forge token: %+v %q", ans, a.forge.comments)
	}
}

// A script that allocates past the memory limit is ended by it, on each
// platform's mechanism, and the failure is recorded; so is a script that
// passes its step limit, and one whose text is invalid.
func TestAutomationLimits(t *testing.T) {
	a := setupAutomation(t)
	src := "def main(event):\n    keep = []\n    for i in range(64):\n        keep.append(\"x\" * (32 << 20))\n    return turn(\"never\", \"x\")\n"
	res := a.run(t, src, AutomationRun{Memory: 128 << 20})
	if !res.Memory || !strings.Contains(res.Failed, "memory limit of 128 MiB") || res.Turn != nil {
		t.Fatalf("past the memory limit: %+v", res)
	}
	t.Logf("%s: %s", runtime.GOOS, res.Failed)
	if recs := a.records(t, res.RunID); len(recs) != 2 || recs[0].Kind != stepAutomationFailed || recs[1].Outcome != "failed" {
		t.Errorf("the records: %+v", recs)
	}
	// The same script under the limit runs to its end: the limit, not the
	// process, ended it.
	if res := a.run(t, strings.Replace(src, "range(64)", "range(1)", 1), AutomationRun{Memory: 128 << 20}); res.Failed != "" ||
		res.Turn == nil {
		t.Errorf("within the memory limit: %+v", res)
	}

	res = a.run(t, "def main(event):\n    for i in range(1000000):\n        pass\n", AutomationRun{Limits: automation.Limits{Steps: 1000}})
	if !strings.Contains(res.Failed, "too many steps") || res.Memory {
		t.Errorf("past the step limit: %+v", res)
	}
	res = a.run(t, "def main(event):\n    while True:\n        pass\n", AutomationRun{})
	if !res.Invalid || res.Failed == "" {
		t.Errorf("an invalid script: %+v", res)
	}
}

// What a host call gives the script is redacted: a job's log, a review or
// a command's output that quotes the home's forge credential reaches the
// child without it (hands' BLOCKER on #167).
func TestAutomationHostAnswersAreRedacted(t *testing.T) {
	a := setupAutomation(t)
	leak := "token " + automationForgeToken + " leaked"
	a.forge.content.Checks[0].Log.Excerpt = leak
	a.forge.content.Comments[1].Body += "\n- [FOLLOW_UP] " + leak + "\n"
	os.WriteFile(filepath.Join(a.worktree, "c.txt"), []byte("c\n"), 0o600)
	git(t, a.worktree, "add", "c.txt")
	git(t, a.worktree, "commit", "-q", "-m", leak)

	a.srv.runsMu.Lock()
	a.srv.runs = map[string]*liveRun{"token-r": {ctx: context.Background(), id: "run-r", attempt: a.id, pull: 7,
		worktree: a.worktree, allow: TurnAllowList(a.cfg.Home, nil)}}
	a.srv.runsMu.Unlock()
	for _, c := range []automationCall{{Call: "pr"}, {Call: "run", Cmd: "git", Args: []string{"log", "-1", "--format=%B"}}} {
		c.Token = "token-r"
		body, _ := json.Marshal(c)
		ans, err := CallStep(context.Background(), a.cfg.Home, StepCall{Op: automationOp, Body: body})
		if err != nil || !ans.OK {
			t.Fatalf("%s: %+v %v", c.Call, ans, err)
		}
		raw, _ := json.Marshal(ans)
		if strings.Contains(string(raw), automationForgeToken) || !strings.Contains(string(raw), redacted) {
			t.Errorf("%s: the answer is not redacted: %s", c.Call, raw)
		}
	}

	// Through the child too: the script sees the redacted text.
	src := fmt.Sprintf(`
def main(event):
    p = pr()
    texts = [p.checks[0].excerpt, p.reviews[1].body] + p.reviews[1].findings + [run("git", ["log", "-1", "--format=%%B"]).output]
    for t in texts:
        if %q in t:
            fail("a credential reached the script")
    if %q not in p.checks[0].excerpt:
        fail("not redacted: " + p.checks[0].excerpt)
`, automationForgeToken, redacted)
	if res := a.run(t, src, AutomationRun{}); res.Failed != "" {
		t.Fatalf("the run: %+v", res)
	}
}
