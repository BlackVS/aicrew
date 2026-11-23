package agent

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/BlackVS/aicrew/defaults"
	"github.com/BlackVS/aicrew/internal/automation"
	"github.com/BlackVS/aicrew/internal/forge"
	"github.com/BlackVS/aicrew/internal/store"
)

// failingReports is aicrewd's local step API with the report of an
// observation failing on demand, as when aicrewd cannot be reached after
// the forge read.
type failingReports struct {
	LocalAPI
	mu   sync.Mutex
	fail bool
}

func (f *failingReports) LocalStep(ctx context.Context, key, token, path string, body []byte) (json.RawMessage, error) {
	f.mu.Lock()
	fail := f.fail
	f.mu.Unlock()
	if fail && path == pullsPath {
		return nil, errors.New("aicrewd could not be reached")
	}
	return f.LocalAPI.LocalStep(ctx, key, token, path, body)
}

func (f *failingReports) set(fail bool) {
	f.mu.Lock()
	f.fail = fail
	f.mu.Unlock()
}

// The worker's wait on its launcher's gates-green submit reads only an
// observation aicrewd took, through the real poll and the release's
// defaults (01a206f8-24d0, hands review of #286): a newer review in a
// poll whose report failed is in the last observation but not in the
// points, so the poll waits on nothing, whichever review it is, and a
// reported observation followed by a failed one does not wait either.
func TestGatesGreenWaitReadsOnlyReportedObservations(t *testing.T) {
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
	git(t, wt, "remote", "set-url", "origin", "https://git.example.test/crew/project-t.git")
	head := git(t, wt, "rev-parse", "HEAD")
	url := "https://git.example.test/crew/project-t/pulls/4"

	review := func(reviewer, verdict string) forge.PullComment {
		return forge.PullComment{Kind: forge.CommentIssue, Author: reviewer, Body: "reviewed at head " + head + "\n\nVERDICT\n" + verdict,
			Review: &forge.ReviewHeader{Reviewer: reviewer, Head: head[:12], AtHead: true, Verdict: verdict}}
	}
	ready := []forge.PullComment{review(forge.ReviewerExternal, verdictReady), review(forge.ReviewerLocal, verdictReady)}
	checks := []forge.Check{{Name: "go-test", State: forge.ChecksSuccess}}
	fake := &fakeStatus{token: "member-forge-token", st: forge.PullStatus{Pull: forge.Pull{Number: 4, URL: url,
		State: forge.PullOpen, Head: "work/task-1", HeadCommit: head, Base: "main", Labels: []string{"hands-reviewed"}},
		Checks: forge.ChecksSuccess, Comments: int64(len(ready))}, content: forge.PullContent{Comments: ready, Checks: checks}}
	oldAPI := newPullStatusAPI
	newPullStatusAPI = func() PullStatusAPI { return fake }
	t.Cleanup(func() { newPullStatusAPI = oldAPI })
	f := &automationForge{token: "member-forge-token",
		pull: forge.Pull{Number: 4, URL: url, State: forge.PullOpen, Draft: true, Title: "WIP: the change", Head: "work/task-1",
			HeadCommit: head, Base: "main", Labels: []string{}, Mergeable: true},
		content: forge.PullContent{Checks: checks}}
	oldForge := newAutomationForge
	newAutomationForge = func() AutomationForge { return f }
	t.Cleanup(func() { newAutomationForge = oldForge })
	reports := &failingReports{LocalAPI: srv.local}
	srv.local = reports

	// comments shows the forge's comments and has the next poll read them.
	comments := func(cs ...forge.PullComment) {
		fake.mu.Lock()
		fake.content.Comments = cs
		fake.mu.Unlock()
		fake.set(func(st *forge.PullStatus) { st.Comments = int64(len(cs)) })
	}
	poll := func() {
		srv.pollPulls(ctx)
		srv.waitPoints()
	}
	waits := func() bool { return srv.GatesGreenWait(id, head, time.Now()) }

	// The forge pr command's baseline puts the pull request on the watch.
	var answers []byte
	obs, _ := json.Marshal(observation("git.example.test", "crew/project-t", fake.st))
	if ans := s.call(t, &answers, StepCall{Op: "watch-pull", Body: obs}); !ans.OK {
		t.Fatalf("the baseline: %+v", ans)
	}
	srv.waitPoints()

	// Both reviews READY at the head and CI green, reported: the wait.
	poll()
	if !waits() {
		t.Fatalf("no wait on a reported green head\n%s", s.logs.String())
	}
	// The same status again, aicrewd unreachable: the poll reads the status
	// alone and carries the reported content on, never the mark, so there
	// is no wait on it.
	reports.set(true)
	reads := fake.contentReads()
	poll()
	if fake.contentReads() != reads || !srv.Observed()[0].Content {
		t.Fatalf("the poll of an unchanged pull request: content reads %d, had %d; %+v", fake.contentReads(), reads,
			srv.Observed())
	}
	if waits() {
		t.Error("a reported observation followed by a failed one of the same status: the worker waits")
	}
	reports.set(false)

	for _, c := range []struct {
		name  string
		newer forge.PullComment
	}{
		{"a newer blocking external review", review(forge.ReviewerExternal, "RETURN_TO_IMPLEMENTATION")},
		{"the worker's own newer review, not READY", review(forge.ReviewerLocal, "RETURN_TO_IMPLEMENTATION")},
	} {
		// The forge shows the newer review; aicrewd takes no report.
		reports.set(true)
		comments(append(append([]forge.PullComment{}, ready...), c.newer)...)
		poll()
		obs := srv.Observed()
		if len(obs) != 1 || obs[0].reported || obs[0].Comments != int64(len(ready)+1) {
			t.Fatalf("%s: the last observation is not the poll's, unreported: %+v", c.name, obs)
		}
		if waits() {
			t.Errorf("%s in a report that failed: the worker waits on the launcher", c.name)
		}
		// aicrewd back, the forge back to both READY: the wait again.
		reports.set(false)
		comments(ready...)
		poll()
		if !waits() {
			t.Fatalf("%s: no wait once a report succeeded again", c.name)
		}
	}

	// A reported observation followed by a failed one with its labels
	// changed: no wait.
	reports.set(true)
	fake.set(func(st *forge.PullStatus) { st.Labels = []string{"hands-reviewed", "pilot"} })
	poll()
	if waits() {
		t.Error("a reported observation followed by a failed one: the worker waits")
	}
}
