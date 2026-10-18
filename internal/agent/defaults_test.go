package agent

import (
	"context"
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/BlackVS/aicrew/defaults"
	"github.com/BlackVS/aicrew/internal/automation"
	"github.com/BlackVS/aicrew/internal/forge"
	"github.com/BlackVS/aicrew/internal/store"
)

// The release's defaults through the launcher (increment 2d-d): with the
// embedded defaults, a process manifest that names nothing of its own and
// the fake forge for the host calls, the first sight of the head runs
// review-request (review-this is added), a CI failure on the runner runs
// ci-rerun (the failed job is re-run once), and both reviews READY with CI
// green run gates-green: the pull request is marked ready, the attempt is
// submitted, and the worker gets no gates wake, because the launcher's
// observation named gates-green among its automations. An observation
// with both gates green while an external review is in flight takes no
// edge, so the first one without the label still marks ready and submits.
func TestDefaultsThroughTheLauncher(t *testing.T) {
	ctx := context.Background()
	a := setupAutomationSources(t)
	// The fixture swaps the release's defaults for fakes; this test is
	// about the real ones.
	releaseAutomations = func() ([]automation.Entry, error) {
		raw, err := fs.ReadFile(defaults.Automations, defaults.AutomationsFile)
		if err != nil {
			return nil, err
		}
		return automation.ParseDefaults(raw)
	}
	releaseFS = defaults.Automations
	pin := a.commit(t, a.process, a.processBare, "main", map[string]string{
		"process/manifest.json": `{"name": "delivery", "automations": []}`})
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
	// The host calls name the pull request from the worktree's origin.
	git(t, wt, "remote", "set-url", "origin", "https://git.example.test/crew/project-t.git")
	head := git(t, wt, "rev-parse", "HEAD")
	url := "https://git.example.test/crew/project-t/pulls/4"

	// The poll's view of the pull request, and the forge the host calls act on.
	fake := &fakeStatus{token: "member-forge-token", st: forge.PullStatus{Pull: forge.Pull{Number: 4, URL: url,
		State: forge.PullOpen, Head: "work/task-1", HeadCommit: head, Base: "main", Labels: []string{}}, Checks: forge.ChecksPending}}
	oldAPI := newPullStatusAPI
	newPullStatusAPI = func() PullStatusAPI { return fake }
	t.Cleanup(func() { newPullStatusAPI = oldAPI })
	f := &automationForge{token: "member-forge-token",
		pull: forge.Pull{Number: 4, URL: url, State: forge.PullOpen, Draft: true, Title: "WIP: the change", Head: "work/task-1",
			HeadCommit: head, Base: "main", Labels: []string{}, Mergeable: true},
		content: forge.PullContent{Checks: []forge.Check{{Name: "go-test", State: forge.ChecksSuccess}}}}
	oldForge := newAutomationForge
	newAutomationForge = func() AutomationForge { return f }
	t.Cleanup(func() { newAutomationForge = oldForge })

	// 1. The first sight of the head: review-request asks for the review.
	var answers []byte
	obs, _ := json.Marshal(observation("git.example.test", "crew/project-t", fake.st))
	if ans := s.call(t, &answers, StepCall{Op: "watch-pull", Body: obs}); !ans.OK {
		t.Fatalf("the baseline: %+v", ans)
	}
	srv.waitPoints()
	recs := dispatchRecords(t, s.cfg.Home)
	if push := runsAt(recs, automation.PointAfterPush); len(push) != 1 || push["review-request.star"].Outcome != "handled" {
		t.Fatalf("after-push: %+v\nrecords: %+v\n%s", push, recs, s.logs.String())
	}
	if !slices.Equal(f.labels, []string{"+review-this"}) {
		t.Fatalf("review-request's labels: %v", f.labels)
	}

	// 2. CI fails on the runner: ci-rerun re-runs the job once.
	f.mu.Lock()
	f.content.Checks = []forge.Check{{Name: "go-test", State: forge.ChecksFailure, JobID: 11, RunnerFailure: true}}
	f.mu.Unlock()
	fake.set(func(st *forge.PullStatus) { st.Checks = forge.ChecksFailure })
	srv.pollPulls(ctx)
	srv.waitPoints()
	recs = dispatchRecords(t, s.cfg.Home)
	if ci := runsAt(recs, automation.PointCIFailed); len(ci) != 1 || ci["ci-rerun.star"].Outcome != "handled" ||
		len(f.reruns) != 1 || !slices.Equal(f.reruns[0], []string{"go-test"}) {
		t.Fatalf("ci-failed: %+v, re-runs %v", ci, f.reruns)
	}

	// 3. Both reviews READY at the head and CI green, first with an external
	// review in flight (the label beside the READY comment, as during the
	// reviewer's label swap or after a re-request): the gates edge is not
	// taken, nothing is marked ready or submitted, and the head is not
	// consumed. Then the label gone: gates-green marks the pull request
	// ready and submits the attempt; the worker is not woken for the
	// gates, since the launcher does it.
	review := func(reviewer, verdict string) forge.PullComment {
		return forge.PullComment{Kind: forge.CommentIssue, Author: reviewer, Body: "reviewed at head " + head + "\n\nVERDICT\n" + verdict,
			Review: &forge.ReviewHeader{Reviewer: reviewer, Head: head[:12], AtHead: true, Verdict: verdict}}
	}
	f.mu.Lock()
	f.content.Checks = []forge.Check{{Name: "go-test", State: forge.ChecksSuccess}}
	f.mu.Unlock()
	fake.mu.Lock()
	fake.content = forge.PullContent{Comments: []forge.PullComment{review(forge.ReviewerExternal, verdictReady), review(forge.ReviewerLocal, verdictReady)},
		Checks: []forge.Check{{Name: "go-test", State: forge.ChecksSuccess}}}
	fake.mu.Unlock()
	fake.set(func(st *forge.PullStatus) {
		st.Checks, st.Comments, st.Labels = forge.ChecksSuccess, 2, []string{"hands-reviewing"}
	})
	srv.pollPulls(ctx)
	srv.waitPoints()
	recs = dispatchRecords(t, s.cfg.Home)
	if gates := runsAt(recs, automation.PointGatesGreen); len(gates) != 0 || f.ready != 0 {
		t.Fatalf("gates-green while an external review is in flight: %+v, ready %d", gates, f.ready)
	}
	fake.set(func(st *forge.PullStatus) { st.Labels = []string{"hands-reviewed"} })
	srv.pollPulls(ctx)
	srv.waitPoints()
	recs = dispatchRecords(t, s.cfg.Home)
	if gates := runsAt(recs, automation.PointGatesGreen); len(gates) != 1 || gates["gates-green.star"].Outcome != "handled" {
		t.Fatalf("gates-green: %+v\n%s", gates, s.logs.String())
	}
	if f.ready != 1 || f.pull.Draft || f.pull.Title != "the change" {
		t.Errorf("the pull request after gates-green: ready %d, draft %v, title %q", f.ready, f.pull.Draft, f.pull.Title)
	}
	st, err := srv.Status(ctx)
	if err != nil || st.Attempt == nil || st.Attempt.Phase != "submitted" {
		t.Fatalf("the attempt after gates-green: %+v, %v", st.Attempt, err)
	}
	page := s.call(t, &answers, StepCall{Op: "inbox"})
	var inbox struct {
		Messages []store.InboxItem `json:"messages"`
	}
	if err := json.Unmarshal(page.Result, &inbox); err != nil {
		t.Fatal(err)
	}
	for _, m := range inbox.Messages {
		if m.Action == nil {
			continue
		}
		for _, sub := range m.Action.Subjects {
			if sub.Kind == "pull" && slices.Contains(sub.Needs, store.NeedGates) {
				t.Fatalf("the worker was woken for the gates the launcher performed: %+v", sub)
			}
		}
	}
}
