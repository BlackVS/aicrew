package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/BlackVS/aicrew/internal/automation"
	"github.com/BlackVS/aicrew/internal/forge"
	"github.com/BlackVS/aicrew/internal/store"
)

// Two declared checks at after-push, each passing once its file is at the
// head.
const prePushManifest = `{
  "name": "delivery",
  "automations": [
    {"name": "check-ok", "source": {"command": ["git", "cat-file", "-e", "HEAD:check-ok"]}, "point": "after-push"},
    {"name": "second", "source": {"command": ["git", "cat-file", "-e", "HEAD:second-ok"]}, "point": "after-push"}
  ]
}`

// prePushWakes are the worker's action.needed messages whose pull subject
// needs pre_push, read through the launcher's inbox op, waited for.
func prePushWakes(t *testing.T, s *stepEnv, answers *[]byte, want int, wait time.Duration) []store.InboxItem {
	t.Helper()
	deadline := time.Now().Add(wait)
	for {
		page := s.call(t, answers, StepCall{Op: "inbox"})
		var inbox struct {
			Messages []store.InboxItem `json:"messages"`
		}
		if json.Unmarshal(page.Result, &inbox) != nil {
			t.Fatalf("the inbox: %s", page.Result)
		}
		var out []store.InboxItem
		for _, m := range inbox.Messages {
			if m.Action == nil {
				continue
			}
			for _, sub := range m.Action.Subjects {
				if sub.Kind == "pull" && sliceHas(sub.Needs, store.NeedPrePush) {
					out = append(out, m)
				}
			}
		}
		if len(out) >= want || time.Now().After(deadline) {
			return out
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func sliceHas(xs []string, x string) bool {
	for _, v := range xs {
		if v == x {
			return true
		}
	}
	return false
}

// A declared check at after-push that exits nonzero is the check's verdict
// on the head (2d-b): its run is handled, with its exit recorded, and the
// coordinators are not woken for it; the worker is, with the command, the
// exit and the output in the message's pre_push field, and the other
// failed check named. The failure is kept for the head: a poll at the same
// head wakes no one again, and a head where the checks pass clears it.
func TestFailedCheckWakesTheWorker(t *testing.T) {
	ctx := context.Background()
	a := setupAutomationSources(t)
	pin := a.commit(t, a.process, a.processBare, "main", map[string]string{"process/manifest.json": prePushManifest})
	s := setupStepsWith(t, crewOptions{role: store.RoleIndependent, steps: true, shortHome: true})
	body := s.claimBody()
	body["process"] = map[string]string{"repo": testProcessRepo, "commit": pin, "manifest": "process/manifest.json"}
	r, err := s.run(t, attemptsPath+"/claim", body)
	if err != nil || r.Outcome != "committed" {
		t.Fatalf("claim: %+v %v", r, err)
	}
	id := r.AttemptID
	provisionTestForge(t, s.cfg.Home, "git.example.test", "member-forge-token")
	srv := s.serve(t)

	wt := filepath.Join(s.cfg.Home, "worktrees", id)
	os.MkdirAll(filepath.Dir(wt), 0o700)
	git(t, filepath.Dir(wt), "clone", "-q", "-b", "work/task-1", a.projectBare, wt)
	git(t, wt, "config", "user.email", "setup@example")
	git(t, wt, "config", "user.name", "setup")
	head1 := git(t, wt, "rev-parse", "HEAD")

	fake := &fakeStatus{token: "member-forge-token", st: forge.PullStatus{Pull: forge.Pull{Number: 4,
		URL: "https://git.example.test/crew/project-t/pulls/4", State: forge.PullOpen, Head: "work/task-1", HeadCommit: head1,
		Base: "main", Labels: []string{"review-this"}}, Checks: forge.ChecksPending}}
	oldAPI := newPullStatusAPI
	newPullStatusAPI = func() PullStatusAPI { return fake }
	t.Cleanup(func() { newPullStatusAPI = oldAPI })

	// 1. The baseline fires after-push: both checks fail at the head.
	var answers []byte
	obs, _ := json.Marshal(observation("git.example.test", "crew/project-t", fake.st))
	if ans := s.call(t, &answers, StepCall{Op: "watch-pull", Body: obs}); !ans.OK {
		t.Fatalf("the baseline: %+v", ans)
	}
	srv.waitPoints()
	recs := dispatchRecords(t, s.cfg.Home)
	push := runsAt(recs, automation.PointAfterPush)
	if check := push["check-ok"]; len(push) != 2 || check.Outcome != "handled" || check.Exit == nil || *check.Exit == 0 ||
		!strings.Contains(stepOf(recs, check.Run, stepCommand), "check-ok") {
		t.Fatalf("the failed check's run: %+v\n%s", push, s.logs.String())
	}
	wakes := prePushWakes(t, s, &answers, 1, 15*time.Second)
	if len(wakes) != 1 {
		t.Fatalf("the worker's wakes: %+v\n%s", wakes, s.logs.String())
	}
	sub := wakes[0].Action.Subjects[0]
	if sub.AttemptID != id || strings.Join(sub.Needs, ",") != store.NeedPrePush || sub.PrePush == nil ||
		sub.PrePush.Command != "git cat-file -e HEAD:check-ok" || sub.PrePush.Exit == 0 || sub.PrePush.Cut ||
		!strings.Contains(sub.PrePush.Output, "check-ok") || strings.Join(sub.PrePush.Others, ",") != "second" {
		t.Fatalf("the pre_push subject: %+v", sub)
	}
	if text := wakes[0].Text; !strings.Contains(text, "A declared check failed after the push: `git cat-file -e HEAD:check-ok` (exit") ||
		!strings.Contains(text, "\n  | ") || !strings.Contains(text, "Other checks failed at this point too: second") {
		t.Fatalf("the message's text:\n%s", text)
	}
	if w := automationWakes(t, s, 0); len(w) != 0 {
		t.Fatalf("a failed check woke the coordinator: %+v", w)
	}
	key := "git.example.test/crew/project-t#4"
	if pp := srv.prePushAt(key, head1); pp == nil || pp.Command != "git cat-file -e HEAD:check-ok" {
		t.Fatalf("the kept failure: %+v", pp)
	}

	// 2. A poll at the same head, and one after a restart, carry the
	// failure again and wake no one again.
	srv.pollPulls(ctx)
	srv.waitPoints()
	srv.Close()
	srv = s.serve(t)
	srv.pollPulls(ctx)
	srv.waitPoints()
	if n := len(prePushWakes(t, s, &answers, 2, 3*time.Second)); n != 1 {
		t.Fatalf("the failure at one head woke the worker %d times", n)
	}

	// 3. A new head where one check still fails: a new wake, naming no
	// other.
	os.WriteFile(filepath.Join(wt, "check-ok"), []byte("ok\n"), 0o600)
	git(t, wt, "add", "check-ok")
	git(t, wt, "commit", "-q", "-m", "the first check passes")
	git(t, wt, "push", "-q", "origin", "HEAD:refs/heads/work/task-1")
	head2 := git(t, wt, "rev-parse", "HEAD")
	fake.set(func(st *forge.PullStatus) { st.HeadCommit = head2 })
	srv.pollPulls(ctx)
	srv.waitPoints()
	wakes = prePushWakes(t, s, &answers, 2, 15*time.Second)
	if len(wakes) != 2 || wakes[1].Action.Subjects[0].PrePush == nil || wakes[1].Action.Subjects[0].PrePush.Command != "git cat-file -e HEAD:second-ok" ||
		len(wakes[1].Action.Subjects[0].PrePush.Others) != 0 || wakes[1].Action.Subjects[0].Pull.HeadCommit != head2 {
		t.Fatalf("the wake at the second head: %+v", wakes)
	}

	// 4. A head where both pass: the failure is cleared and no one is woken.
	os.WriteFile(filepath.Join(wt, "second-ok"), []byte("ok\n"), 0o600)
	git(t, wt, "add", "second-ok")
	git(t, wt, "commit", "-q", "-m", "the second check passes")
	git(t, wt, "push", "-q", "origin", "HEAD:refs/heads/work/task-1")
	head3 := git(t, wt, "rev-parse", "HEAD")
	fake.set(func(st *forge.PullStatus) { st.HeadCommit = head3 })
	srv.pollPulls(ctx)
	srv.waitPoints()
	recs = dispatchRecords(t, s.cfg.Home)
	if n := countRuns(recs, automation.PointAfterPush); n != 6 {
		t.Fatalf("after-push ran %d checks for three heads, want 6", n)
	}
	if check := runsAt(recs, automation.PointAfterPush)["second"]; check.Outcome != "handled" || check.Exit == nil || *check.Exit != 0 {
		t.Fatalf("the passing check's run: %+v", check)
	}
	if pp := srv.prePushAt(key, head3); pp != nil {
		t.Fatalf("a failure kept at a passing head: %+v", pp)
	}
	var pts pointsDoc
	raw, _ := os.ReadFile(pointsPathIn(s.cfg.Home))
	if json.Unmarshal(raw, &pts) != nil || pts.Pulls[key] == nil || pts.Pulls[key].PrePush != nil {
		t.Fatalf("the points' state: %s", raw)
	}
	if n := len(prePushWakes(t, s, &answers, 3, 3*time.Second)); n != 2 {
		t.Fatalf("a passing head woke the worker: %d wakes", n)
	}
}

// An observation with the most content the launcher reads (a review body of
// 32 KiB, five excerpts of 8 KiB, 120 passed checks, a failed check's
// output of 8 KiB, all of characters JSON escapes) is cut to what aicrewd's
// request bound takes, each cut marked; a small one is sent whole.
func TestObservationFitsTheRoute(t *testing.T) {
	o := PullObservation{Host: "git.example.test", Repository: "crew/project-t", Number: 4, State: forge.PullOpen,
		HeadCommit: strings.Repeat("a", 40), Labels: []string{}, Checks: forge.ChecksFailure, Content: true,
		Review:  &ObservedReview{Author: "hands-bot", Head: strings.Repeat("a", 40), AtHead: true, Body: strings.Repeat("\"", 32<<10)},
		PrePush: &PrePushFailure{Command: "bash scripts/check-repo.sh", Exit: 1, Output: strings.Repeat("\"", 8<<10)}}
	for i := 0; i < 5; i++ {
		o.Failed = append(o.Failed, ObservedFailure{Name: "job", Excerpt: strings.Repeat("\"", 8<<10)})
	}
	for i := 0; i < 120; i++ {
		o.Passed = append(o.Passed, ObservedPass{Name: "ok", URL: "https://ci.example/" + strings.Repeat("u", 200)})
	}
	body := fitObservation(&o)
	var back PullObservation
	if len(body) > maxObservationEncoded || json.Unmarshal(body, &back) != nil || !back.Review.Cut || back.Review.Body == "" ||
		!back.PrePush.Cut || back.PrePush.Output == "" || len(back.Failed) == 0 || !back.Failed[0].Cut || len(back.Passed) != 30 {
		t.Fatalf("the cut observation: %d bytes, %+v", len(body), back)
	}
	small := PullObservation{Host: "git.example.test", Repository: "crew/project-t", Number: 4, State: forge.PullOpen, Labels: []string{},
		Checks: forge.ChecksPending, PrePush: &PrePushFailure{Command: "git status", Exit: 1, Output: "dirty"}}
	whole, _ := json.Marshal(small)
	if got := fitObservation(&small); string(got) != string(whole) {
		t.Fatalf("a small observation was changed: %s", got)
	}
}

// A failed check as the manifest admits it can exceed what aicrewd's
// observation takes: 32 words of 256 bytes joined, a NUL in the output, more
// other failed checks than the route's forty. The launcher cuts the failure
// to the contract before sending it, so the worker is woken all the same
// (external review of #170), launcher to store through the real route; the
// message then names the first ten of the other checks, the store's bound.
func TestFailedCheckPayloadFitsTheContract(t *testing.T) {
	a := setupAutomationSources(t)
	// The first entry: a shell alias that prints a NUL and exits 3, with
	// 28 words of 250 bytes after it (ignored by the function); then 42
	// checks that fail.
	words := []string{"git", "-c", "alias.nul=!f() { printf 'a\\000b'; exit 3; }; f", "nul"}
	for i := 0; len(words) < automation.MaxCommandArgs; i++ {
		words = append(words, strings.Repeat(string(rune('a'+i%26)), 250))
	}
	quoted, _ := json.Marshal(words)
	entries := []string{fmt.Sprintf(`{"name": "nul-and-long", "source": {"command": %s}, "point": "after-push"}`, quoted)}
	for i := 0; i < 42; i++ {
		entries = append(entries, fmt.Sprintf(`{"name": "missing-%02d", "source": {"command": ["git", "cat-file", "-e", "HEAD:missing-%02d"]}, "point": "after-push"}`, i, i))
	}
	manifest := `{"name": "delivery", "automations": [` + strings.Join(entries, ",\n") + `]}`
	pin := a.commit(t, a.process, a.processBare, "main", map[string]string{"process/manifest.json": manifest})
	s := setupStepsWith(t, crewOptions{role: store.RoleIndependent, steps: true, shortHome: true})
	body := s.claimBody()
	body["process"] = map[string]string{"repo": testProcessRepo, "commit": pin, "manifest": "process/manifest.json"}
	r, err := s.run(t, attemptsPath+"/claim", body)
	if err != nil || r.Outcome != "committed" {
		t.Fatalf("claim: %+v %v", r, err)
	}
	id := r.AttemptID
	provisionTestForge(t, s.cfg.Home, "git.example.test", "member-forge-token")
	srv := s.serve(t)
	wt := filepath.Join(s.cfg.Home, "worktrees", id)
	os.MkdirAll(filepath.Dir(wt), 0o700)
	git(t, filepath.Dir(wt), "clone", "-q", "-b", "work/task-1", a.projectBare, wt)
	head1 := git(t, wt, "rev-parse", "HEAD")
	fake := &fakeStatus{token: "member-forge-token", st: forge.PullStatus{Pull: forge.Pull{Number: 4,
		URL: "https://git.example.test/crew/project-t/pulls/4", State: forge.PullOpen, Head: "work/task-1", HeadCommit: head1,
		Base: "main", Labels: []string{}}, Checks: forge.ChecksPending}}
	oldAPI := newPullStatusAPI
	newPullStatusAPI = func() PullStatusAPI { return fake }
	t.Cleanup(func() { newPullStatusAPI = oldAPI })

	var answers []byte
	obs, _ := json.Marshal(observation("git.example.test", "crew/project-t", fake.st))
	if ans := s.call(t, &answers, StepCall{Op: "watch-pull", Body: obs}); !ans.OK {
		t.Fatalf("the baseline: %+v", ans)
	}
	srv.waitPoints()
	recs := dispatchRecords(t, s.cfg.Home)
	if n := countRuns(recs, automation.PointAfterPush); n != 43 {
		t.Fatalf("after-push ran %d checks, want 43\n%s", n, s.logs.String())
	}
	if first := runsAt(recs, automation.PointAfterPush)["nul-and-long"]; first.Outcome != "handled" || first.Exit == nil || *first.Exit != 3 {
		t.Fatalf("the first check's run: %+v", first)
	}
	wakes := prePushWakes(t, s, &answers, 1, 15*time.Second)
	if len(wakes) != 1 {
		t.Fatalf("the worker's wakes: %+v\n%s", wakes, s.logs.String())
	}
	p := wakes[0].Action.Subjects[0].PrePush
	if p == nil || len(p.Command) > 1<<10 || !strings.HasSuffix(p.Command, " [cut]") || !strings.HasPrefix(p.Command, "git -c alias.nul=") ||
		p.Exit != 3 || p.Output != "ab" || len(p.Others) != 10 || p.Others[0] != "missing-00" || p.Others[9] != "missing-09" {
		t.Fatalf("the pre_push subject: %+v", p)
	}
	if strings.Contains(s.logs.String(), "report the failed check") {
		t.Fatalf("a report was refused:\n%s", s.logs.String())
	}
}
