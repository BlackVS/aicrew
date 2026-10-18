package agent

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/BlackVS/aicrew/internal/automation"
	"github.com/BlackVS/aicrew/internal/forge"
	"github.com/BlackVS/aicrew/internal/store"
)

// A point fires once for its edge: after-push for a head, after-ci for a
// head and a conclusion, ci-failed for a failed head, gates-green for a
// head whose two reviews are READY while CI passed, after-merge for the
// merge. An edge in flight, or fired already, does not fire again.
func TestPointEdges(t *testing.T) {
	open := func(head, checks string) PullObservation {
		return PullObservation{Number: 4, State: forge.PullOpen, HeadCommit: head, Checks: checks}
	}
	ready := func(head string) map[string]pointReview {
		return map[string]pointReview{forge.ReviewerLocal: {Head: head[:12], Verdict: verdictReady},
			forge.ReviewerExternal: {Head: head, Verdict: verdictReady}}
	}
	reviewing := func(head, checks string) PullObservation {
		o := open(head, checks)
		o.Labels = []string{labelReviewing}
		return o
	}
	h1, h2 := "0123456789abcdef0123456789abcdef01234567", "89abcdef0123456789abcdef0123456789abcdef"
	p := &pullPoints{Attempt: "att-1", Fired: map[string][]string{}}
	inflight := map[string]bool{}
	fired := func(want ...string) {
		t.Helper()
		if len(want) == 0 {
			want = nil
		}
		got := p.edges(lastO, lastF, inflight, "k")
		if !slices.Equal(got, want) {
			t.Fatalf("%s: fired %v, want %v", lastWhat, got, want)
		}
		for _, pt := range got {
			p.Fired[pt] = append(p.Fired[pt], edgeOf(pt, lastO, p.Head))
		}
	}
	say := func(what string, o PullObservation, f pullFacts) { lastWhat, lastO, lastF = what, o, f }

	say("a new head", open(h1, forge.ChecksPending), pullFacts{Base: "main"})
	fired(automation.PointAfterPush)
	say("the same head again", open(h1, forge.ChecksPending), pullFacts{})
	fired()
	say("CI failed", open(h1, forge.ChecksFailure), pullFacts{})
	fired(automation.PointAfterCI, automation.PointCIFailed)
	say("CI failed, read again", open(h1, forge.ChecksFailure), pullFacts{})
	fired()
	say("CI passed after a re-run", open(h1, forge.ChecksSuccess), pullFacts{})
	fired(automation.PointAfterCI)
	say("CI failed once more: an edge already consumed", open(h1, forge.ChecksFailure), pullFacts{})
	fired()
	say("and passed once more", open(h1, forge.ChecksSuccess), pullFacts{})
	fired()
	say("the external review READY alone", open(h1, forge.ChecksSuccess),
		pullFacts{Reviews: map[string]pointReview{forge.ReviewerExternal: {Head: h1, Verdict: verdictReady}}})
	fired()
	say("both reviews READY, CI green, while an external review is in flight", reviewing(h1, forge.ChecksSuccess), pullFacts{Reviews: ready(h1)})
	fired()
	say("both reviews READY, CI green, the label gone", open(h1, forge.ChecksSuccess), pullFacts{})
	fired(automation.PointGatesGreen)
	say("the same, polled again", open(h1, forge.ChecksSuccess), pullFacts{})
	fired()
	say("a review with a BLOCKER", open(h1, forge.ChecksSuccess), pullFacts{Reviews: map[string]pointReview{
		forge.ReviewerLocal: {Head: h1, Verdict: verdictReady}, forge.ReviewerExternal: {Head: h1, Verdict: "RETURN", Blocker: true}}})
	fired()
	say("a new head with CI already green and stale reviews", open(h2, forge.ChecksSuccess), pullFacts{})
	fired(automation.PointAfterPush, automation.PointAfterCI)
	if p.Previous != h1 || p.Head != h2 || p.Base != "main" {
		t.Fatalf("the heads: %+v", p)
	}
	inflight["k "+automation.PointGatesGreen] = true
	say("gates green while that point is in flight", open(h2, forge.ChecksSuccess), pullFacts{Reviews: ready(h2)})
	fired()
	delete(inflight, "k "+automation.PointGatesGreen)
	say("gates green once the run ended", open(h2, forge.ChecksSuccess), pullFacts{})
	fired(automation.PointGatesGreen)
	// Many heads later, a force-push back to the first: its edge is still
	// consumed.
	for i := 0; i < 20; i++ {
		say("another head", open(fmt.Sprintf("%040d", i), forge.ChecksPending), pullFacts{})
		fired(automation.PointAfterPush)
	}
	say("back to the first head", open(h1, forge.ChecksPending), pullFacts{})
	fired()
	say("and to the second", open(h2, forge.ChecksSuccess), pullFacts{})
	fired()
	say("merged", PullObservation{Number: 4, State: forge.PullMerged, HeadCommit: h2, Checks: forge.ChecksSuccess}, pullFacts{})
	fired(automation.PointAfterMerge)
	say("merged, polled again", PullObservation{Number: 4, State: forge.PullMerged, HeadCommit: h2, Checks: forge.ChecksSuccess}, pullFacts{})
	fired()
}

var (
	lastWhat string
	lastO    PullObservation
	lastF    pullFacts
)

const dispatchManifest = `{
  "name": "delivery",
  "automations": [
    {"name": "push-note", "source": {"process": "process/push-note.star"}, "point": "after-push"},
    {"name": "push-check", "source": {"command": ["git", "status", "--short"]}, "point": "after-push"},
    {"name": "env-probe", "source": {"command": ["git", "-c", "alias.dumpenv=!env", "dumpenv"]}, "point": "after-push"},
    {"name": "refused", "source": {"command": ["curl", "https://example.test"]}, "point": "after-push"},
    {"name": "ci-note", "source": {"process": "process/ci-note.star"}, "point": "after-ci"},
    {"name": "gates", "source": {"process": "process/gates.star"}, "point": "gates-green"},
    {"name": "submit", "source": {"process": "process/submit.star"}, "point": "before-submit"},
    {"name": "merged", "source": {"process": "process/merged.star"}, "point": "after-merge"}
  ]
}`

func starlark(body string) string { return automation.HeaderPrefix + `{"host_api": 1}` + "\n" + body }

// dispatchRecords are the home's automation records, in order.
func dispatchRecords(t *testing.T, home string) []automationRecord {
	t.Helper()
	raw, _ := os.ReadFile(filepath.Join(home, filepath.FromSlash(AutomationLogFile)))
	var out []automationRecord
	for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		var r automationRecord
		if line != "" && json.Unmarshal([]byte(line), &r) == nil {
			out = append(out, r)
		}
	}
	return out
}

// runsAt are the records of the runs' ends at point, by entry name.
func runsAt(recs []automationRecord, point string) map[string]automationRecord {
	out := map[string]automationRecord{}
	for _, r := range recs {
		if r.Point == point && r.Outcome != "" {
			out[r.Script] = r
		}
	}
	return out
}

// stepOf is the text of the step kind of run.
func stepOf(recs []automationRecord, run, kind string) string {
	for _, r := range recs {
		if r.Run == run && r.Kind == kind {
			return r.Text
		}
	}
	return ""
}

// automationWakes are the action.needed messages with an automation
// subject in the coordinator's inbox, waited for.
func automationWakes(t *testing.T, s *stepEnv, want int) []store.ActionSubject {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for {
		items, err := s.store.ReadInbox(context.Background(), s.coord, s.coordSess.ID, s.coordSess.Generation, 100)
		if err != nil {
			t.Fatal(err)
		}
		var out []store.ActionSubject
		for _, it := range items {
			if it.Action != nil {
				for _, sub := range it.Action.Subjects {
					if sub.Kind == "automation" {
						out = append(out, sub)
					}
				}
			}
		}
		if len(out) >= want || time.Now().After(deadline) {
			return out
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// The launcher dispatches the extension points of its attempt's pull
// request from what it observes, and runs the effective automations at
// each: a declared command in the worktree under the allow list, with the
// client's environment and no credential, recorded with its exit and
// output; a script in its own process, told the point and whether the head
// is a base-only update. Each run is reported to aicrewd, which wakes the
// coordinators on a failed run and the worker on a turn. A point fires once
// for its edge, across a restart too.
func TestPointsDispatch(t *testing.T) {
	ctx := context.Background()
	a := setupAutomationSources(t)
	pin := a.commit(t, a.process, a.processBare, "main", map[string]string{
		"process/manifest.json":  dispatchManifest,
		"process/push-note.star": starlark("def main(event):\n    step(\"push\", \"kinds=%s base_only=%s\" % (\",\".join(event.kinds), event.base_only))\n    return None\n"),
		"process/ci-note.star":   starlark("def main(event):\n    step(\"ci\", \"kinds=%s ids=%s\" % (\",\".join(event.kinds), \",\".join(event.ids)))\n    return None\n"),
		"process/gates.star":     starlark("def main(event):\n    fail(\"the gates script fails on purpose\")\n"),
		"process/submit.star":    starlark("def main(event):\n    step(\"submit\", \"before the submit at %s\" % event.ids[0])\n    return None\n"),
		"process/merged.star":    starlark("def main(event):\n    return turn(\"rework\", \"merged: decide the follow-up\")\n"),
	})
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
	srv.env = append(os.Environ(), "AICREW_CLIENT_MARK=client-env-mark")

	// The worktree: the attempt's branch, cloned from the project.
	wt := filepath.Join(s.cfg.Home, "worktrees", id)
	os.MkdirAll(filepath.Dir(wt), 0o700)
	git(t, filepath.Dir(wt), "clone", "-q", "-b", "work/task-1", a.projectBare, wt)
	git(t, wt, "config", "user.email", "setup@example")
	git(t, wt, "config", "user.name", "setup")
	head1 := git(t, wt, "rev-parse", "HEAD")

	fake := &fakeStatus{token: "member-forge-token", st: forge.PullStatus{Pull: forge.Pull{Number: 4,
		URL: "https://git.example.test/crew/project-t/pulls/4", State: forge.PullOpen, Head: "work/task-1", HeadCommit: head1,
		Base: "main", Labels: []string{}}, Checks: forge.ChecksPending}}
	oldAPI := newPullStatusAPI
	newPullStatusAPI = func() PullStatusAPI { return fake }
	t.Cleanup(func() { newPullStatusAPI = oldAPI })

	// 1. The forge pr command's baseline is the first sight of the head:
	// after-push fires, and the four entries at it run.
	var answers []byte
	obs, _ := json.Marshal(observation("git.example.test", "crew/project-t", fake.st))
	if ans := s.call(t, &answers, StepCall{Op: "watch-pull", Body: obs}); !ans.OK {
		t.Fatalf("the baseline: %+v", ans)
	}
	srv.waitPoints()
	recs := dispatchRecords(t, s.cfg.Home)
	push := runsAt(recs, automation.PointAfterPush)
	if len(push) != 4 {
		raw, _ := os.ReadFile(filepath.Join(s.cfg.Home, filepath.FromSlash(AutomationLogFile)))
		t.Fatalf("the runs at after-push: %+v\n%s\n%s", push, raw, s.logs.String())
	}
	if note := push["process/push-note.star"]; note.Outcome != "handled" ||
		stepOf(recs, note.Run, "push") != "kinds=after-push base_only=False" {
		t.Errorf("push-note: %+v, step %q", note, stepOf(recs, note.Run, "push"))
	}
	if check := push["push-check"]; check.Outcome != "handled" || check.Exit == nil || *check.Exit != 0 ||
		!slices.Equal(check.Command, []string{"git", "status", "--short"}) {
		t.Errorf("push-check: %+v", check)
	}
	probe := push["env-probe"]
	env := stepOf(recs, probe.Run, stepCommand)
	if probe.Outcome != "handled" || !strings.Contains(env, "AICREW_CLIENT_MARK=client-env-mark") {
		t.Fatalf("env-probe: %+v\n%s", probe, env)
	}
	sess, _ := os.ReadFile(s.e.AimemFile())
	for name, secret := range map[string]string{"the forge token": "member-forge-token", "the step token": s.e.stepToken(),
		"the session file's content": strings.TrimSpace(string(sess))} {
		if secret != "" && strings.Contains(env, secret) {
			t.Errorf("the command's environment holds %s", name)
		}
	}
	if refused := push["refused"]; refused.Outcome != "failed" ||
		!strings.Contains(stepOf(recs, refused.Run, stepAutomationFailed), "outside the turn's allow list") {
		t.Errorf("refused: %+v", refused)
	}
	// The failed run woke the coordinator with the handler, the subject
	// and the error; the handled runs woke no one.
	wakes := automationWakes(t, s, 1)
	if len(wakes) != 1 || len(wakes[0].Runs) != 1 || wakes[0].Runs[0].Name != "refused" || wakes[0].AttemptID != id ||
		!strings.Contains(wakes[0].Text, "the automation refused (the command curl https://example.test) failed at after-push for pull request #4") ||
		!strings.Contains(wakes[0].Text, "outside the turn's allow list") {
		t.Fatalf("the coordinator's wakes: %+v", wakes)
	}

	// 2. The same status polled again: nothing fires; nor after a restart.
	srv.pollPulls(ctx)
	srv.waitPoints()
	if n := len(dispatchRecords(t, s.cfg.Home)); n != len(recs) {
		t.Fatalf("a poll of an unchanged pull request ran something: %d records, had %d", n, len(recs))
	}
	srv.Close()
	srv = s.serve(t)
	srv.env = append(os.Environ(), "AICREW_CLIENT_MARK=client-env-mark")
	if ans := s.call(t, &answers, StepCall{Op: "watch-pull", Body: obs}); !ans.OK {
		t.Fatalf("the baseline after the restart: %+v", ans)
	}
	srv.waitPoints()
	if n := len(dispatchRecords(t, s.cfg.Home)); n != len(recs) {
		t.Fatalf("a restarted launcher fired after-push again: %d records, had %d", n, len(recs))
	}

	// 3. CI fails at the head: after-ci and ci-failed fire (ci-note at the
	// first, nothing declared at the second); CI passes after a re-run:
	// after-ci fires for the new conclusion.
	fake.set(func(st *forge.PullStatus) { st.Checks = forge.ChecksFailure })
	srv.pollPulls(ctx)
	srv.waitPoints()
	recs = dispatchRecords(t, s.cfg.Home)
	// The release default base-update runs at after-ci too.
	ci := runsAt(recs, automation.PointAfterCI)
	if note := ci["process/ci-note.star"]; len(ci) != 2 || note.Outcome != "handled" ||
		stepOf(recs, note.Run, "ci") != "kinds=after-ci ids=git.example.test/crew/project-t#4@"+head1 {
		t.Fatalf("ci-note after a failure: %+v, step %q", ci, stepOf(recs, note.Run, "ci"))
	}
	fake.set(func(st *forge.PullStatus) { st.Checks = forge.ChecksSuccess })
	srv.pollPulls(ctx)
	srv.waitPoints()
	if n := countRuns(dispatchRecords(t, s.cfg.Home), automation.PointAfterCI); n != 4 {
		t.Fatalf("after-ci ran %d entries for two conclusions, want 4", n)
	}

	// 4. Reviews: the external review READY alone fires nothing; with the
	// local review READY too, gates-green fires, and its failing script
	// wakes the coordinator.
	review := func(reviewer, head, verdict string) forge.PullComment {
		return forge.PullComment{Kind: forge.CommentIssue, Author: reviewer, Body: "reviewed at head " + head + "\n\nVERDICT\n" + verdict,
			Review: &forge.ReviewHeader{Reviewer: reviewer, Head: head[:12], AtHead: true, Verdict: verdict}}
	}
	fake.mu.Lock()
	fake.content = forge.PullContent{Comments: []forge.PullComment{review(forge.ReviewerExternal, head1, verdictReady)},
		Checks: []forge.Check{{Name: "build", State: forge.ChecksSuccess}}}
	fake.mu.Unlock()
	fake.set(func(st *forge.PullStatus) { st.Comments = 1 })
	srv.pollPulls(ctx)
	srv.waitPoints()
	if n := countRuns(dispatchRecords(t, s.cfg.Home), automation.PointGatesGreen); n != 0 {
		t.Fatalf("gates-green fired on the external review alone")
	}
	fake.mu.Lock()
	fake.content.Comments = append(fake.content.Comments, review(forge.ReviewerLocal, head1, verdictReady))
	fake.mu.Unlock()
	fake.set(func(st *forge.PullStatus) { st.Comments = 2 })
	srv.pollPulls(ctx)
	srv.waitPoints()
	recs = dispatchRecords(t, s.cfg.Home)
	if gates := runsAt(recs, automation.PointGatesGreen); len(gates) != 2 || gates["process/gates.star"].Outcome != "failed" ||
		gates["gates-green.star"].Outcome != "handled" {
		t.Fatalf("gates-green: %+v", gates)
	}
	wakes = automationWakes(t, s, 2)
	if len(wakes) != 2 || wakes[1].Runs[0].Name != "gates" || !strings.Contains(wakes[1].Text, "the gates script fails on purpose") {
		t.Fatalf("the gates wake: %+v", wakes)
	}

	// 5. A base-only update: main moves, the branch is rebased onto it,
	// the patch identity is unchanged, and the after-push event says so.
	a.commit(t, a.project, a.projectBare, "main", map[string]string{"moved.txt": "the base moved\n"})
	git(t, wt, "fetch", "-q", "origin")
	git(t, wt, "rebase", "-q", "origin/main")
	git(t, wt, "push", "-q", "-f", "origin", "HEAD:refs/heads/work/task-1")
	head2 := git(t, wt, "rev-parse", "HEAD")
	fake.mu.Lock()
	for _, c := range fake.content.Comments {
		c.Review.AtHead = false // the reviews name the old head
	}
	fake.mu.Unlock()
	fake.set(func(st *forge.PullStatus) { st.HeadCommit, st.Checks, st.Comments = head2, forge.ChecksPending, 2 })
	srv.pollPulls(ctx)
	srv.waitPoints()
	recs = dispatchRecords(t, s.cfg.Home)
	if got := lastStep(recs, "push"); got != "kinds=after-push base_only=True" {
		raw, _ := os.ReadFile(pointsPathIn(s.cfg.Home))
		t.Fatalf("after-push at the rebased head: %q\n%s\n%s", got, raw, s.logs.String())
	}
	// A head that changes the patch: not base-only.
	os.WriteFile(filepath.Join(wt, "more.txt"), []byte("more\n"), 0o600)
	git(t, wt, "add", "more.txt")
	git(t, wt, "commit", "-q", "-m", "more")
	git(t, wt, "push", "-q", "origin", "HEAD:refs/heads/work/task-1")
	head3 := git(t, wt, "rev-parse", "HEAD")
	fake.set(func(st *forge.PullStatus) { st.HeadCommit = head3 })
	srv.pollPulls(ctx)
	srv.waitPoints()
	recs = dispatchRecords(t, s.cfg.Home)
	if got := lastStep(recs, "push"); got != "kinds=after-push base_only=False" {
		t.Fatalf("after-push at a changed head: %q", got)
	}
	if n := countRuns(recs, automation.PointAfterPush); n != 12 {
		t.Fatalf("after-push ran %d entries for three heads, want 12", n)
	}
	var pts pointsDoc
	raw, _ := os.ReadFile(pointsPathIn(s.cfg.Home))
	if json.Unmarshal(raw, &pts) != nil || pts.Pulls["git.example.test/crew/project-t#4"] == nil ||
		pts.Pulls["git.example.test/crew/project-t#4"].Base != "main" || len(pts.Pulls["git.example.test/crew/project-t#4"].Identity) != 2 {
		t.Fatalf("the points' state: %s", raw)
	}

	// 6. A report aicrewd would refuse as sent, twenty steps whose JSON
	// escaping multiplies their bytes and a NUL in the error, is cut to
	// what the route takes, and the failed run still wakes the coordinator.
	big := automationReport{AttemptID: id, RunID: newUUID(), Point: automation.PointAfterPush, Name: "big", Source: "process",
		Script: "process/big.star", Pull: 4, URL: fake.st.URL, Head: head3, Outcome: runFailed, Error: "check\x00 failed"}
	for i := 0; i < 20; i++ {
		big.Steps = append(big.Steps, automationStep{Kind: "check", Text: strings.Repeat("<", 1024)})
	}
	srv.reportRun(ctx, big)
	var bigWake *store.AutomationRunReport
	for deadline := time.Now().Add(15 * time.Second); bigWake == nil && time.Now().Before(deadline); time.Sleep(100 * time.Millisecond) {
		// A wake may carry several runs of the queue (a run of the point
		// reported just before this one, delivered together).
		for _, w := range automationWakes(t, s, 0) {
			for i := range w.Runs {
				if w.Runs[i].Name == "big" {
					bigWake = &w.Runs[i]
				}
			}
		}
	}
	if bigWake == nil || bigWake.Error != "check failed" || len(bigWake.Steps) == 0 || len(bigWake.Steps[0].Text) > 512 {
		t.Fatalf("the big report's wake: %+v", bigWake)
	}
	// A command the manifest admits (32 words of 256 bytes) that the allow
	// list refuses: its words alone exceed the route once escaped, and the
	// failed run still reaches the coordinator, the words cut and marked.
	wide := automationReport{AttemptID: id, Point: automation.PointAfterPush, Name: "wide", Source: automation.SourceCommand,
		Pull: 4, URL: fake.st.URL, Head: head3}
	var words []string
	for i := 0; i < automation.MaxCommandArgs; i++ {
		words = append(words, strings.Repeat("<", 256))
	}
	wide.Command = words
	srv.runCommand(ctx, &wide, words)
	if wide.Outcome != runFailed {
		t.Fatalf("the wide command: %+v", wide)
	}
	srv.reportRun(ctx, wide)
	var wideWake *store.AutomationRunReport
	for deadline := time.Now().Add(15 * time.Second); wideWake == nil && time.Now().Before(deadline); time.Sleep(100 * time.Millisecond) {
		for _, w := range automationWakes(t, s, 0) {
			for i := range w.Runs {
				if w.Runs[i].Name == "wide" {
					wideWake = &w.Runs[i]
				}
			}
		}
	}
	if wideWake == nil || len(wideWake.Command) < 2 || wideWake.Command[len(wideWake.Command)-1] != "[cut]" || len(wideWake.Command[0]) > 64 ||
		!strings.Contains(wideWake.Error, "outside the turn's allow list") {
		t.Fatalf("the wide command's wake: %+v", wideWake)
	}

	// 7. The submit: before-submit runs in the work step, before the
	// submit is sent.
	submit, _ := json.Marshal(map[string]string{"intent": "submit", "detail": "https://git.example.test/crew/project-t/pulls/4"})
	s.call(t, &answers, StepCall{Op: "work", AttemptID: id, TaskID: "task-1", Body: submit})
	recs = dispatchRecords(t, s.cfg.Home)
	if sub := runsAt(recs, automation.PointBeforeSubmit); len(sub) != 1 || sub["process/submit.star"].Outcome != "handled" ||
		lastStep(recs, "submit") != "before the submit at git.example.test/crew/project-t#4@"+head3 {
		t.Fatalf("before-submit: %+v, step %q", sub, lastStep(recs, "submit"))
	}

	// 8. Merged: after-merge fires, and its turn reaches the worker.
	fake.set(func(st *forge.PullStatus) { st.State = forge.PullMerged })
	srv.pollPulls(ctx)
	srv.waitPoints()
	recs = dispatchRecords(t, s.cfg.Home)
	if merged := runsAt(recs, automation.PointAfterMerge); len(merged) != 1 || merged["process/merged.star"].Outcome != "turn" {
		t.Fatalf("after-merge: %+v", merged)
	}
	var worker []store.ActionSubject
	for deadline := time.Now().Add(15 * time.Second); time.Now().Before(deadline) && len(worker) == 0; time.Sleep(100 * time.Millisecond) {
		page := s.call(t, &answers, StepCall{Op: "inbox"})
		var inbox struct {
			Messages []store.InboxItem `json:"messages"`
		}
		json.Unmarshal(page.Result, &inbox)
		for _, m := range inbox.Messages {
			if m.Action != nil && m.Action.Subjects[0].Kind == "automation" {
				worker = append(worker, m.Action.Subjects[0])
			}
		}
	}
	if len(worker) != 1 || worker[0].Runs[0].Turn != "rework" || !strings.Contains(worker[0].Text, "asks for a rework turn:\n  | merged: decide the follow-up") {
		t.Fatalf("the worker's wake: %+v", worker)
	}

	// 9. An after-merge run the launcher's exit cut short: the merge took
	// the pull request off the watch list, the kept state still owes the
	// point, and the next launcher's first poll runs it.
	key := "git.example.test/crew/project-t#4"
	pts = pointsDoc{}
	raw, _ = os.ReadFile(pointsPathIn(s.cfg.Home))
	if json.Unmarshal(raw, &pts) != nil || pts.Pulls[key] == nil || pts.Pulls[key].State != forge.PullMerged {
		t.Fatalf("the points' state after the merge: %s", raw)
	}
	delete(pts.Pulls[key].Fired, automation.PointAfterMerge)
	raw, _ = json.Marshal(pts)
	os.WriteFile(pointsPathIn(s.cfg.Home), raw, 0o600)
	if len(readWatched(s.cfg.Home)) != 0 {
		t.Fatal("the merged pull request is still watched")
	}
	srv.Close()
	srv = s.serve(t)
	srv.pollPulls(ctx)
	srv.waitPoints()
	if n := countRuns(dispatchRecords(t, s.cfg.Home), automation.PointAfterMerge); n != 2 {
		t.Fatalf("after-merge ran %d times after the replay, want 2", n)
	}
	srv.pollPulls(ctx)
	srv.waitPoints()
	if n := countRuns(dispatchRecords(t, s.cfg.Home), automation.PointAfterMerge); n != 2 {
		t.Fatalf("after-merge ran %d times after a second poll, want 2", n)
	}

	if strings.Contains(string(answers), "member-forge-token") || strings.Contains(s.logs.String(), "member-forge-token") {
		t.Fatal("the forge token reached an answer or the log")
	}
}

func countRuns(recs []automationRecord, point string) int {
	n := 0
	for _, r := range recs {
		if r.Point == point && r.Outcome != "" {
			n++
		}
	}
	return n
}

func lastStep(recs []automationRecord, kind string) string {
	out := ""
	for _, r := range recs {
		if r.Kind == kind {
			out = r.Text
		}
	}
	return out
}

// A declared command runs code from the attempt's head and receives no
// credential through its environment or through git: its environment
// holds none (TestPointsDispatch), the clone's credential helper is reset
// for its process, so `git credential fill` and any network git fail, and
// the declared command itself is never the helper command.
func TestCommandHasNoCredentialPath(t *testing.T) {
	ctx := context.Background()
	s := setupStepsWith(t, crewOptions{role: store.RoleIndependent, steps: true, shortHome: true})
	srv := s.serve(t)
	wt := filepath.Join(s.cfg.Home, "worktrees", "att-cred")
	os.MkdirAll(wt, 0o700)
	git(t, wt, "init", "-q", "-b", "work/task-1")
	// The clone's helper, as Clone configures it; here one that answers.
	git(t, wt, "config", "--local", "--add", "credential.helper", "")
	git(t, wt, "config", "--local", "--add", "credential.helper", "!f() { echo username=u; echo password=helper-secret; }; f")
	git(t, wt, "config", "--local", "alias.probe", "!printf 'protocol=https\\nhost=git.example.test\\n\\n' | git credential fill")
	// The probe has teeth: run outside the command's environment, the
	// helper answers.
	direct := exec.Command("git", "probe")
	direct.Dir = wt
	direct.Env = append(os.Environ(), gitEnv...)
	if out, _ := direct.CombinedOutput(); !strings.Contains(string(out), "password=helper-secret") {
		t.Fatalf("the probe without the reset: %s", out)
	}

	rep := automationReport{AttemptID: "att-cred", Point: automation.PointAfterPush, Name: "probe", Source: automation.SourceCommand,
		Pull: 4, URL: "https://git.example.test/crew/project-t/pulls/4", Head: strings.Repeat("a", 40)}
	srv.runCommand(ctx, &rep, []string{"git", "probe"})
	out := ""
	if len(rep.Steps) > 0 {
		out = rep.Steps[0].Text
	}
	// At after-push a nonzero exit is the check's verdict for the worker
	// (2d-b): the run is handled, and the failure it keeps holds no secret
	// either.
	if rep.Outcome != runHandled || rep.check == nil || strings.Contains(out, "helper-secret") ||
		strings.Contains(rep.check.failure.Output, "helper-secret") || rep.Exit == nil || *rep.Exit == 0 {
		t.Fatalf("the probe as a declared command: %s %q exit %v\n%s", rep.Outcome, rep.Error, rep.Exit, out)
	}
	// At any other point it is a failed run, with the same silence.
	rep = automationReport{AttemptID: "att-cred", Point: automation.PointBeforeSubmit, Name: "probe", Source: automation.SourceCommand,
		Pull: 4, URL: "https://git.example.test/crew/project-t/pulls/4", Head: strings.Repeat("a", 40)}
	srv.runCommand(ctx, &rep, []string{"git", "probe"})
	if rep.Outcome != runFailed || rep.Exit == nil || *rep.Exit == 0 || (len(rep.Steps) > 0 && strings.Contains(rep.Steps[0].Text, "helper-secret")) {
		t.Fatalf("the probe at before-submit: %s %q exit %v", rep.Outcome, rep.Error, rep.Exit)
	}

	// The helper command itself is refused: by name, and under any other
	// spelling of the program by the allow list, which admits bare names.
	for prog, want := range map[string]string{"aicrew-agent": "never the credential helper",
		"aicrew-agent.exe": "outside the turn's allow list", "/opt/aicrew/aicrew-agent": "outside the turn's allow list"} {
		rep := automationReport{AttemptID: "att-cred", Point: automation.PointAfterPush, Name: "helper", Source: automation.SourceCommand,
			Pull: 4, URL: "https://git.example.test/crew/project-t/pulls/4", Head: strings.Repeat("a", 40)}
		srv.runCommand(ctx, &rep, []string{prog, "git-credential", "--home", s.cfg.Home, "get"})
		if rep.Outcome != runFailed || !strings.Contains(rep.Error, want) {
			t.Fatalf("%s git-credential: %s %q", prog, rep.Outcome, rep.Error)
		}
	}
	// An argument that would set a helper or an askpass after the reset is
	// refused before any process starts, under any spelling git or the
	// environment reads (2a00); an ordinary setting still runs.
	for _, c := range []struct {
		args []string
		want string
	}{
		{[]string{"git", "-c", "credential.helper=store", "fetch"}, "credential.helper"},
		{[]string{"git", "-c", "Credential.Helper=store", "fetch"}, "credential.helper"},
		{[]string{"git", "-c", "credential.https://git.example.test.helper=store", "fetch"}, "credential.helper"},
		{[]string{"git", "--config-env=core.askPass=PROMPT", "fetch"}, "core.askPass"},
		{[]string{"git", "--config-env", "credential.helper=HELPER", "fetch"}, "credential.helper"},
		{[]string{"git", "config", "credential.helper", "store"}, "credential.helper"},
		{[]string{"git", "GIT_ASKPASS=/bin/echo", "fetch"}, "GIT_ASKPASS"},
		{[]string{"git", "GIT_CONFIG_KEY_0=credential.helper", "fetch"}, "credential.helper"},
		{[]string{"git", "GIT_CONFIG_PARAMETERS='core.askpass'='/bin/echo'", "fetch"}, "core.askPass"},
	} {
		rep := automationReport{AttemptID: "att-cred", Point: automation.PointAfterPush, Name: "setting", Source: automation.SourceCommand,
			Pull: 4, URL: "https://git.example.test/crew/project-t/pulls/4", Head: strings.Repeat("a", 40)}
		srv.runCommand(ctx, &rep, c.args)
		if rep.Outcome != runFailed || !strings.Contains(rep.Error, "cannot set "+c.want) || rep.Exit != nil || len(rep.Steps) != 0 {
			t.Fatalf("%q: %s %q exit %v steps %d", c.args, rep.Outcome, rep.Error, rep.Exit, len(rep.Steps))
		}
	}
	for _, args := range [][]string{{"git", "-c", "alias.x=status", "x"}, {"git", "-c", "core.pager=cat", "status", "--short"}} {
		rep := automationReport{AttemptID: "att-cred", Point: automation.PointAfterPush, Name: "plain", Source: automation.SourceCommand,
			Pull: 4, URL: "https://git.example.test/crew/project-t/pulls/4", Head: strings.Repeat("a", 40)}
		srv.runCommand(ctx, &rep, args)
		if rep.Outcome != runHandled || rep.Exit == nil || *rep.Exit != 0 {
			t.Fatalf("%q: %s %q exit %v", args, rep.Outcome, rep.Error, rep.Exit)
		}
	}
	for _, line := range dispatchRecords(t, s.cfg.Home) {
		if strings.Contains(line.Text, "helper-secret") {
			t.Fatal("the secret reached the automation log")
		}
	}
}

// The edges a point fired for are kept for as long as the pull request
// is: after many heads, and a restart, a return to an old head fires
// nothing. The edges are recorded by the production path (firePoint, for
// a pull request whose attempt is not the member's open one, so no
// automation runs).
func TestPointsKeepEveryEdge(t *testing.T) {
	ctx := context.Background()
	s := setupStepsWith(t, crewOptions{role: store.RoleIndependent, steps: true, shortHome: true})
	srv := s.serve(t)
	key := "git.example.test/crew/project-t#9"
	heads := make([]string, 24)
	for i := range heads {
		heads[i] = fmt.Sprintf("%040d", i+1)
		o := PullObservation{Host: "git.example.test", Repository: "crew/project-t", Number: 9, State: forge.PullOpen, HeadCommit: heads[i]}
		p := &pullPoints{Attempt: "att-x", Number: 9, Head: heads[i]}
		srv.firePoint(ctx, key, p, automation.PointAfterPush, o)
	}
	srv.Close()
	srv = s.serve(t)
	doc := readPoints(s.cfg.Home)
	p := doc.Pulls[key]
	if p == nil || len(p.Fired[automation.PointAfterPush]) != len(heads) {
		t.Fatalf("the kept edges after a restart: %+v", p)
	}
	for _, h := range []string{heads[0], heads[1], heads[len(heads)-1]} {
		o := PullObservation{Number: 9, State: forge.PullOpen, HeadCommit: h}
		if got := p.edges(o, pullFacts{}, nil, key); len(got) != 0 {
			t.Fatalf("a return to the old head %s fires %v", h, got)
		}
	}
	o := PullObservation{Number: 9, State: forge.PullOpen, HeadCommit: strings.Repeat("f", 40)}
	if got := p.edges(o, pullFacts{}, nil, key); !slices.Equal(got, []string{automation.PointAfterPush}) {
		t.Fatalf("a new head fires %v", got)
	}
}

// sleepHelperMain is the test binary as a declared command that takes its
// time: it writes marker and sleeps until the launcher stops it. A marker
// already there is the run after a restart, which ends at once.
func sleepHelperMain(marker string) int {
	if _, err := os.Stat(marker); err == nil {
		return 0
	}
	if err := os.WriteFile(marker, []byte("started\n"), 0o600); err != nil {
		return 3
	}
	time.Sleep(20 * time.Second)
	return 0
}

// A run the launcher's exit cut (the 2d-a follow-up 2a39; DESIGN-TURNS
// 12.4, at least once): after-push fires a declared command that takes its
// time, Close cuts it, and the point keeps no edge, the home's log no end
// record and aicrewd no report; the next launcher's first poll fires the
// point again and the command runs to its end.
func TestPointCutByShutdown(t *testing.T) {
	ctx := context.Background()
	a := setupAutomationSources(t)
	dir := t.TempDir()
	marker := filepath.ToSlash(filepath.Join(dir, "started"))
	// The test binary on PATH as aicrew-agent, which the turn's allow list
	// admits by name: a declared command is a bare command name.
	bin := filepath.Join(dir, "bin")
	os.MkdirAll(bin, 0o755)
	self, dst := selfPath(t), filepath.Join(dir, "bin", "aicrew-agent")
	if runtime.GOOS == "windows" {
		dst += ".exe"
	}
	// A running executable's hard link cannot be removed on Windows.
	if runtime.GOOS == "windows" || os.Link(self, dst) != nil {
		raw, err := os.ReadFile(self)
		if err != nil || os.WriteFile(dst, raw, 0o755) != nil {
			t.Fatal("cannot place the stand-in")
		}
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	manifest, _ := json.Marshal(map[string]any{"name": "delivery", "automations": []map[string]any{
		{"name": "slow", "source": map[string]any{"command": []string{"aicrew-agent", "sleep-helper", marker}}, "point": "after-push"}}})
	pin := a.commit(t, a.process, a.processBare, "main", map[string]string{"process/manifest.json": string(manifest)})
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
	head := git(t, wt, "rev-parse", "HEAD")
	fake := &fakeStatus{token: "member-forge-token", st: forge.PullStatus{Pull: forge.Pull{Number: 4,
		URL: "https://git.example.test/crew/project-t/pulls/4", State: forge.PullOpen, Head: "work/task-1", HeadCommit: head,
		Base: "main", Labels: []string{}}, Checks: forge.ChecksPending}}
	oldAPI := newPullStatusAPI
	newPullStatusAPI = func() PullStatusAPI { return fake }
	t.Cleanup(func() { newPullStatusAPI = oldAPI })
	key := pullKey("git.example.test", "crew/project-t", 4)
	fired := func() []string {
		if p := readPoints(s.cfg.Home).Pulls[key]; p != nil {
			return p.Fired[automation.PointAfterPush]
		}
		return nil
	}
	// The runs aicrewd recorded for the attempt, read from its store.
	reported := func() int {
		db, err := sql.Open("sqlite", s.storePath+"?_pragma=query_only(1)&_pragma=busy_timeout(10000)")
		if err != nil {
			t.Fatal(err)
		}
		defer db.Close()
		var n int
		if err := db.QueryRow(`SELECT count(*) FROM attempt_automation_runs WHERE attempt_id = ?`, id).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}

	// 1. The baseline fires after-push; the command starts and takes its
	// time.
	var answers []byte
	obs, _ := json.Marshal(observation("git.example.test", "crew/project-t", fake.st))
	if ans := s.call(t, &answers, StepCall{Op: "watch-pull", Body: obs}); !ans.OK {
		t.Fatalf("the baseline: %+v", ans)
	}
	for deadline := time.Now().Add(20 * time.Second); ; time.Sleep(20 * time.Millisecond) {
		if _, err := os.Stat(marker); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the command did not start: %+v\n%s", dispatchRecords(t, s.cfg.Home), s.logs.String())
		}
	}

	// 2. The launcher's exit cuts it: no edge, no end record, no report.
	srv.Close()
	recs := dispatchRecords(t, s.cfg.Home)
	if ends := countRuns(recs, automation.PointAfterPush); ends != 0 || len(fired()) != 0 || reported() != 0 {
		t.Fatalf("after the cut: %d ends, fired %v, reported %d\n%s", ends, fired(), reported(), s.logs.String())
	}
	if readPoints(s.cfg.Home).Pulls[key] == nil {
		t.Fatal("the pull request left the points' state")
	}
	// Nor was a report attempted: the cut is not a result to send.
	if strings.Contains(s.logs.String(), "automations: report a run") {
		t.Fatalf("a report of the cut run was attempted:\n%s", s.logs.String())
	}

	// 3. The next launcher's first poll fires the point again, and the
	// command runs to its end.
	srv = s.serve(t)
	srv.pollPulls(ctx)
	srv.waitPoints()
	recs = dispatchRecords(t, s.cfg.Home)
	slow := runsAt(recs, automation.PointAfterPush)["slow"]
	if ends := countRuns(recs, automation.PointAfterPush); slow.Outcome != "handled" || slow.Exit == nil || *slow.Exit != 0 || ends != 1 ||
		!slices.Equal(fired(), []string{head}) || reported() != 1 {
		t.Fatalf("after the restart: %+v, %d ends, fired %v, reported %d\n%s", slow, ends, fired(), reported(), s.logs.String())
	}
}

// A point that could not be prepared records no edge (the 2d-a follow-up):
// with the release's defaults unreadable, after-push fires, runs nothing
// and is logged once over two polls; once the defaults read again the next
// poll fires it and its entries run, and the edge is recorded then.
func TestPreparationErrorKeepsTheEdge(t *testing.T) {
	ctx := context.Background()
	a := setupAutomationSources(t)
	orig := releaseAutomations
	var failing atomic.Bool
	failing.Store(true)
	releaseAutomations = func() ([]automation.Entry, error) {
		if failing.Load() {
			return nil, errors.New("the release's defaults are unreadable on purpose")
		}
		return orig()
	}
	t.Cleanup(func() { releaseAutomations = orig })
	s := setupStepsWith(t, crewOptions{role: store.RoleIndependent, steps: true, shortHome: true})
	body := s.claimBody()
	body["process"] = map[string]string{"repo": testProcessRepo, "commit": a.pin, "manifest": "process/manifest.json"}
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
	head := git(t, wt, "rev-parse", "HEAD")
	fake := &fakeStatus{token: "member-forge-token", st: forge.PullStatus{Pull: forge.Pull{Number: 4,
		URL: "https://git.example.test/crew/project-t/pulls/4", State: forge.PullOpen, Head: "work/task-1", HeadCommit: head,
		Base: "main", Labels: []string{}}, Checks: forge.ChecksPending}}
	oldAPI := newPullStatusAPI
	newPullStatusAPI = func() PullStatusAPI { return fake }
	t.Cleanup(func() { newPullStatusAPI = oldAPI })
	key := pullKey("git.example.test", "crew/project-t", 4)
	fired := func() []string {
		if p := readPoints(s.cfg.Home).Pulls[key]; p != nil {
			return p.Fired[automation.PointAfterPush]
		}
		return nil
	}
	logged := func() int { return strings.Count(s.logs.String(), "the point was not prepared") }

	var answers []byte
	obs, _ := json.Marshal(observation("git.example.test", "crew/project-t", fake.st))
	if ans := s.call(t, &answers, StepCall{Op: "watch-pull", Body: obs}); !ans.OK {
		t.Fatalf("the baseline: %+v", ans)
	}
	srv.waitPoints()
	if recs := dispatchRecords(t, s.cfg.Home); len(recs) != 0 || len(fired()) != 0 || logged() != 1 {
		t.Fatalf("after-push with the set unreadable: records %+v, fired %v, logged %d\n%s", recs, fired(), logged(), s.logs.String())
	}
	srv.pollPulls(ctx)
	srv.waitPoints()
	if recs := dispatchRecords(t, s.cfg.Home); len(recs) != 0 || len(fired()) != 0 || logged() != 1 {
		t.Fatalf("the second poll, still unreadable: records %+v, fired %v, logged %d", recs, fired(), logged())
	}
	failing.Store(false)
	srv.pollPulls(ctx)
	srv.waitPoints()
	recs := dispatchRecords(t, s.cfg.Home)
	push := runsAt(recs, automation.PointAfterPush)
	if len(push) != 1 || push["automations/check.star"].Outcome != "handled" || !slices.Equal(fired(), []string{head}) || logged() != 1 {
		t.Fatalf("after-push once the set reads: %+v, fired %v, logged %d\n%s", push, fired(), logged(), s.logs.String())
	}
}
