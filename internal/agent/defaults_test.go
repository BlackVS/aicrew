package agent

import (
	"context"
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/BlackVS/aicrew/defaults"
	"github.com/BlackVS/aicrew/internal/automation"
	"github.com/BlackVS/aicrew/internal/events"
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

// The repository-source case of the 2d-a follow-up, from 2d-d's review: the
// observation names gates-green from the release and the manifest, while
// the set at the point also reads a repository-sourced script at the base
// branch. With the base unreadable at the gates-green edge the point
// records no edge and is logged once over two polls, and the worker gets
// no gates wake, since the names promise the transition; once the base
// reads, the next poll runs the point: the pull request is ready and the
// attempt submitted.
func TestRepositorySourceBaseUnreadable(t *testing.T) {
	ctx := context.Background()
	a := setupAutomationSources(t)
	releaseAutomations = func() ([]automation.Entry, error) {
		raw, err := fs.ReadFile(defaults.Automations, defaults.AutomationsFile)
		if err != nil {
			return nil, err
		}
		return automation.ParseDefaults(raw)
	}
	releaseFS = defaults.Automations
	pin := a.commit(t, a.process, a.processBare, "main", map[string]string{
		"process/manifest.json": `{"name": "delivery", "automations": [` +
			`{"name": "project-check", "source": {"repository": "automations/check.star"}, "point": "after-push"}]}`})
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
	git(t, wt, "remote", "set-url", "origin", "https://git.example.test/crew/project-t.git")
	head := git(t, wt, "rev-parse", "HEAD")
	url := "https://git.example.test/crew/project-t/pulls/4"
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
	key := pullKey("git.example.test", "crew/project-t", 4)
	fired := func(point string) []string {
		if p := readPoints(s.cfg.Home).Pulls[key]; p != nil {
			return p.Fired[point]
		}
		return nil
	}
	// The lines logged for the gates-green point (after-ci fires at the
	// same poll and is logged on its own).
	logged := func() int { return strings.Count(s.logs.String(), "at the next observation"+`" point=gates-green`) }
	gatesWakes := func() int {
		var answers []byte
		page := s.call(t, &answers, StepCall{Op: "inbox"})
		var inbox struct {
			Messages []store.InboxItem `json:"messages"`
		}
		if err := json.Unmarshal(page.Result, &inbox); err != nil {
			t.Fatal(err)
		}
		n := 0
		for _, m := range inbox.Messages {
			if m.Action == nil {
				continue
			}
			for _, sub := range m.Action.Subjects {
				if sub.Kind == "pull" && slices.Contains(sub.Needs, store.NeedGates) {
					n++
				}
			}
		}
		return n
	}

	// 1. The base readable: after-push runs the release's review-request
	// and the repository's script.
	var answers []byte
	obs, _ := json.Marshal(observation("git.example.test", "crew/project-t", fake.st))
	if ans := s.call(t, &answers, StepCall{Op: "watch-pull", Body: obs}); !ans.OK {
		t.Fatalf("the baseline: %+v", ans)
	}
	srv.waitPoints()
	if push := runsAt(dispatchRecords(t, s.cfg.Home), automation.PointAfterPush); len(push) != 2 ||
		push["review-request.star"].Outcome != "handled" || push["automations/check.star"].Outcome != "handled" {
		t.Fatalf("after-push: %+v\n%s", push, s.logs.String())
	}

	// 2. Both gates green while the base branch cannot be read: the
	// gates-green edge fires, the set cannot be read, no edge is recorded,
	// the error is logged once over two polls, and no gates wake reaches
	// the worker, since the observation names gates-green.
	review := func(reviewer, verdict string) forge.PullComment {
		return forge.PullComment{Kind: forge.CommentIssue, Author: reviewer, Body: "reviewed at head " + head + "\n\nVERDICT\n" + verdict,
			Review: &forge.ReviewHeader{Reviewer: reviewer, Head: head[:12], AtHead: true, Verdict: verdict}}
	}
	fake.mu.Lock()
	fake.content = forge.PullContent{Comments: []forge.PullComment{review(forge.ReviewerExternal, verdictReady), review(forge.ReviewerLocal, verdictReady)},
		Checks: []forge.Check{{Name: "go-test", State: forge.ChecksSuccess}}}
	fake.mu.Unlock()
	fake.set(func(st *forge.PullStatus) {
		st.Checks, st.Comments, st.Labels, st.Base = forge.ChecksSuccess, 2, []string{"hands-reviewed"}, "nowhere"
	})
	for i := 1; i <= 2; i++ {
		srv.pollPulls(ctx)
		srv.waitPoints()
		if gates := runsAt(dispatchRecords(t, s.cfg.Home), automation.PointGatesGreen); len(gates) != 0 ||
			len(fired(automation.PointGatesGreen)) != 0 || logged() != 1 || f.ready != 0 || gatesWakes() != 0 {
			t.Fatalf("poll %d with the base unreadable: gates %+v, fired %v, logged %d, ready %d, wakes %d\n%s",
				i, gates, fired(automation.PointGatesGreen), logged(), f.ready, gatesWakes(), s.logs.String())
		}
	}
	if !strings.Contains(s.logs.String(), "nowhere") {
		t.Errorf("the log does not name the base branch that could not be read:\n%s", s.logs.String())
	}

	// 3. The base readable again: the next poll runs gates-green, the
	// pull request is ready and the attempt submitted.
	fake.set(func(st *forge.PullStatus) { st.Base = "main" })
	srv.pollPulls(ctx)
	srv.waitPoints()
	if gates := runsAt(dispatchRecords(t, s.cfg.Home), automation.PointGatesGreen); len(gates) != 1 ||
		gates["gates-green.star"].Outcome != "handled" || !slices.Equal(fired(automation.PointGatesGreen), []string{head}) {
		t.Fatalf("gates-green once the base reads: %+v, fired %v\n%s", gates, fired(automation.PointGatesGreen), s.logs.String())
	}
	if f.ready != 1 || f.pull.Draft || f.pull.Title != "the change" {
		t.Errorf("the pull request: ready %d, draft %v, title %q", f.ready, f.pull.Draft, f.pull.Title)
	}
	if st, err := srv.Status(ctx); err != nil || st.Attempt == nil || st.Attempt.Phase != "submitted" {
		t.Fatalf("the attempt: %+v, %v", st.Attempt, err)
	}
	if gatesWakes() != 0 {
		t.Errorf("the worker was woken for the gates the launcher performed")
	}
}

// The base-update default through the launcher on real git (the scheduling
// follow-up of 2e): CI green at a head behind its base while an external
// review is in flight runs the default at after-ci, which steps and
// updates nothing, and the after-ci edge is consumed. The observation
// that removed hands-reviewing at the same head is a forge.changed the
// default subscribes to: it runs update_base(), the merge of the base is
// pushed to the origin with the delta note and the attempt's worktree
// fast-forwarded, so the update the review held is not left for the next
// head.
func TestBaseUpdateFollowsTheReviewThroughTheLauncher(t *testing.T) {
	ctx := context.Background()
	a := setupAutomationSources(t)
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
	git(t, wt, "config", "user.name", "member")
	git(t, wt, "config", "user.email", "member@example.test")
	git(t, wt, "remote", "set-url", "origin", testProjectRepo)
	head := git(t, wt, "rev-parse", "HEAD")
	// The base moves on after the branch was pushed: the pull request is
	// behind it, and the launcher's clone does not know.
	base := a.commit(t, a.project, a.projectBare, "main", map[string]string{"moved.txt": "the base moved\n"})
	origin := func() string { return git(t, a.projectBare, "rev-parse", "refs/heads/work/task-1") }
	url := "https://git.example.test/crew/project-t/pulls/4"
	fake := &fakeStatus{token: "member-forge-token", st: forge.PullStatus{Pull: forge.Pull{Number: 4, URL: url,
		State: forge.PullOpen, Head: "work/task-1", HeadCommit: head, Base: "main", Labels: []string{"hands-reviewing"}}, Checks: forge.ChecksSuccess}}
	oldAPI := newPullStatusAPI
	newPullStatusAPI = func() PullStatusAPI { return fake }
	t.Cleanup(func() { newPullStatusAPI = oldAPI })
	f := &automationForge{token: "member-forge-token",
		pull: forge.Pull{Number: 4, URL: url, State: forge.PullOpen, Draft: true, Title: "WIP: the change", Head: "work/task-1",
			HeadCommit: head, Base: "main", Labels: []string{"hands-reviewing"}, Mergeable: true},
		content: forge.PullContent{Checks: []forge.Check{{Name: "go-test", State: forge.ChecksSuccess}}}}
	oldForge := newAutomationForge
	newAutomationForge = func() AutomationForge { return f }
	t.Cleanup(func() { newAutomationForge = oldForge })

	// 1. CI green at the head while the external review is in flight:
	// after-ci runs the default, which steps and pushes nothing.
	var answers []byte
	obs, _ := json.Marshal(observation("git.example.test", "crew/project-t", fake.st))
	if ans := s.call(t, &answers, StepCall{Op: "watch-pull", Body: obs}); !ans.OK {
		t.Fatalf("the baseline: %+v", ans)
	}
	srv.waitPoints()
	recs := dispatchRecords(t, s.cfg.Home)
	if ci := runsAt(recs, automation.PointAfterCI); len(ci) != 1 || ci["base-update.star"].Outcome != "handled" ||
		lastStep(recs, "base_update") != "an external review is in progress; the update of this head follows its result" ||
		countKind(recs, events.KindForgeChanged) != 0 || origin() != head {
		t.Fatalf("after-ci with the review in flight: %+v, step %q, origin %s (head %s)\n%s",
			ci, lastStep(recs, "base_update"), origin(), head, s.logs.String())
	}

	// 2. The review ends at the same head: the observation is a
	// forge.changed (review-request runs on it too and does nothing), the
	// default runs update_base(), and the origin's branch is the merge of
	// the base into the head, with the delta note.
	f.mu.Lock()
	f.pull.Labels = []string{"hands-reviewed"}
	f.mu.Unlock()
	fake.set(func(st *forge.PullStatus) { st.Labels = []string{"hands-reviewed"} })
	srv.pollPulls(ctx)
	srv.waitPoints()
	recs = dispatchRecords(t, s.cfg.Home)
	var changed []automationRecord
	for _, rec := range recs {
		if rec.Outcome != "" && rec.Script == "base-update.star" && slices.Contains(rec.Kinds, events.KindForgeChanged) {
			changed = append(changed, rec)
		}
	}
	if len(changed) != 1 || changed[0].Outcome != "handled" || len(runsAt(recs, automation.PointAfterCI)) != 1 {
		t.Fatalf("the review's end: forge.changed runs %+v\n%s", changed, s.logs.String())
	}
	updated := origin()
	if updated == head || git(t, a.projectBare, "rev-parse", updated+"^1") != head || git(t, a.projectBare, "rev-parse", updated+"^2") != base {
		t.Fatalf("the origin's branch after the review ended: %s (head %s, base %s)\n%s", updated, head, base, s.logs.String())
	}
	if len(f.comments) != 1 || !strings.Contains(f.comments[0], head[:12]) || !strings.Contains(f.comments[0], updated[:12]) {
		t.Errorf("the delta note: %q", f.comments)
	}
	if got := git(t, wt, "rev-parse", "HEAD"); got != updated {
		t.Errorf("the attempt's worktree is at %s, not the update %s", got, updated)
	}
}
