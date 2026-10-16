package agent

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/BlackVS/aicrew/internal/forge"
	"github.com/BlackVS/aicrew/internal/forge/fakeforge"
	"github.com/BlackVS/aicrew/internal/store"
)

// fakeStatus is a forge whose one pull request's status the test changes.
type fakeStatus struct {
	mu    sync.Mutex
	st    forge.PullStatus
	reads int
	token string
}

func (f *fakeStatus) GetPullStatus(_ context.Context, host string, k forge.Kind, token, path string, n int64) (forge.PullStatus, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reads++
	if token != f.token || host != "git.example.test" || k != forge.Gitea || path != "crew/project-t" || n != f.st.Number {
		return forge.PullStatus{}, forge.ErrNotFound
	}
	return f.st, nil
}

func (f *fakeStatus) readCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.reads
}

func (f *fakeStatus) set(change func(st *forge.PullStatus)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	change(&f.st)
}

// The launcher watches the pull request a forge pr command reported for the
// member's running attempt: the report is the baseline, and the member's
// wait reads the pull request with the home's own credential, reports a
// reviewer's change and wakes on the forge.event it becomes. A pull
// request of no attempt is refused and not watched; a merged one is
// reported and then no longer watched.
func TestWaitWakesOnAForgeEvent(t *testing.T) {
	shortWakePoll(t)
	oldPoll := forgePoll
	forgePoll = 100 * time.Millisecond
	t.Cleanup(func() { forgePoll = oldPoll })
	s := setupStepsWith(t, crewOptions{role: store.RoleIndependent, steps: true, shortHome: true})
	id := s.claimed(t)
	provisionTestForge(t, s.cfg.Home, "git.example.test", "member-forge-token")
	fake := &fakeStatus{token: "member-forge-token", st: forge.PullStatus{Pull: forge.Pull{Number: 4,
		URL: "https://git.example.test/crew/project-t/pulls/4", State: forge.PullOpen, Head: "work/task-1",
		HeadCommit: "0123456789abcdef0123456789abcdef01234567", Labels: []string{"review-this"}}, Checks: forge.ChecksPending}}
	oldAPI := newPullStatusAPI
	newPullStatusAPI = func() PullStatusAPI { return fake }
	t.Cleanup(func() { newPullStatusAPI = oldAPI })
	s.serve(t)

	var answers []byte
	fake.mu.Lock()
	obs, _ := json.Marshal(observation("git.example.test", "crew/project-t", fake.st))
	fake.mu.Unlock()
	ans := s.call(t, &answers, StepCall{Op: "watch-pull", Body: obs})
	var res store.ObserveResult
	if !ans.OK || json.Unmarshal(ans.Result, &res) != nil || !res.Baseline || res.AttemptID != id {
		t.Fatalf("the baseline: %+v %s", ans, ans.Result)
	}
	if w := readWatched(s.cfg.Home); len(w) != 1 || w[0].Number != 4 {
		t.Fatalf("watched: %+v", w)
	}
	one, _ := json.Marshal(map[string]int{"seconds": 1})
	var wake WakeResult
	ans = s.call(t, &answers, StepCall{Op: "wait-inbox", Body: one})
	if json.Unmarshal(ans.Result, &wake) != nil || len(wake.Pending) != 0 || fake.readCount() == 0 {
		t.Fatalf("an unchanged pull request woke the member: %s (reads %d)", ans.Result, fake.readCount())
	}

	go func() {
		time.Sleep(300 * time.Millisecond)
		fake.set(func(st *forge.PullStatus) {
			st.Labels, st.Checks, st.Comments = []string{"hands-reviewed"}, forge.ChecksSuccess, 1
		})
	}()
	ten, _ := json.Marshal(map[string]int{"seconds": 10})
	start := time.Now()
	ans = s.call(t, &answers, StepCall{Op: "wait-inbox", Body: ten})
	if json.Unmarshal(ans.Result, &wake) != nil || len(wake.Pending) != 1 || wake.Pending[0].Kind != "lifecycle" ||
		wake.Pending[0].AttemptID != id || time.Since(start) > 5*time.Second {
		t.Fatalf("a review during the wait: %s after %s", ans.Result, time.Since(start))
	}
	page := s.call(t, &answers, StepCall{Op: "inbox"})
	var inbox struct {
		Messages []store.InboxItem `json:"messages"`
	}
	if json.Unmarshal(page.Result, &inbox) != nil || len(inbox.Messages) != 1 || inbox.Messages[0].Forge == nil ||
		strings.Join(inbox.Messages[0].Forge.Changes, "|") != "label hands-reviewed added|CI concluded success at 0123456789ab|1 new comment(s)" {
		t.Fatalf("the forge event: %s", page.Result)
	}

	// Merged: reported once more, then no longer watched.
	fake.set(func(st *forge.PullStatus) { st.State = forge.PullMerged })
	s.call(t, &answers, StepCall{Op: "wait-inbox", Body: one})
	if w := readWatched(s.cfg.Home); len(w) != 0 {
		t.Fatalf("a merged pull request is still watched: %+v", w)
	}
	// A pull request from another branch is no attempt's: refused, not watched.
	other := observation("git.example.test", "crew/project-t", forge.PullStatus{Pull: forge.Pull{Number: 5,
		URL: "https://git.example.test/crew/project-t/pulls/5", State: forge.PullOpen, Head: "work/other"}, Checks: forge.ChecksNone})
	raw, _ := json.Marshal(other)
	if ans := s.call(t, &answers, StepCall{Op: "watch-pull", Body: raw}); ans.OK || ans.Error == nil || ans.Error.Code != "no_attempt" {
		t.Fatalf("another branch: %+v", ans)
	}
	if w := readWatched(s.cfg.Home); len(w) != 0 {
		t.Fatalf("a refused pull request is watched: %+v", w)
	}
	if strings.Contains(string(answers), "member-forge-token") || strings.Contains(s.logs.String(), "member-forge-token") {
		t.Fatal("the forge token reached an answer or the log")
	}
}

// A forge pr command reports the pull request it acted on, read after the
// command, to the launcher, and says whether it is watched; without a
// launcher it says so, and the command still succeeds.
func TestForgePRWatch(t *testing.T) {
	ctx := context.Background()
	f := fakeforge.New(t, "gitea")
	home := t.TempDir()
	provisionTestForge(t, home, f.Host, fakeforge.Token)
	var calls []StepCall
	pr := ForgePR{Home: home, Repository: "https://" + f.Host + "/team/app.git", API: &forge.Client{HTTP: f.Client},
		Step: func(_ context.Context, _ string, c StepCall) (StepAnswer, error) {
			calls = append(calls, c)
			return StepAnswer{OK: true, Status: StepDone, Result: json.RawMessage(`{}`)}, nil
		}}
	rep, err := pr.Open(ctx, OpenPull{Title: "Fix", Head: "feat/x", Base: "main"})
	if err != nil || rep.Watch != WatchOn || len(calls) != 1 || calls[0].Op != "watch-pull" {
		t.Fatalf("open: %+v %v %+v", rep, err, calls)
	}
	if rep, err = pr.Comment(ctx, 1, "A note."); err != nil || rep.Watch != WatchOn {
		t.Fatalf("comment: %+v %v", rep, err)
	}
	var o PullObservation
	if json.Unmarshal(calls[1].Body, &o) != nil || o.Number != 1 || o.Comments != 1 || o.Host != f.Host || o.Repository != "team/app" ||
		o.Head != "feat/x" || o.Checks != forge.ChecksNone || !o.Baseline {
		t.Fatalf("the comment's observation: %s", calls[1].Body)
	}
	if strings.Contains(string(calls[0].Body)+string(calls[1].Body), fakeforge.Token) {
		t.Fatal("an observation carried the token")
	}
	pr.Step = func(context.Context, string, StepCall) (StepAnswer, error) {
		return StepAnswer{Status: StepRefused, Error: &StepError{Code: "no_attempt", Message: "No running attempt of yours."}}, nil
	}
	if rep, err = pr.Show(ctx, 1); err != nil || rep.Watch != "not watched: No running attempt of yours." {
		t.Fatalf("no attempt: %+v %v", rep, err)
	}
	pr.Step = nil
	if rep, err = pr.Show(ctx, 1); err != nil || rep.Watch != watchNoLauncher {
		t.Fatalf("no launcher: %+v %v", rep, err)
	}
	pr.Step = func(context.Context, string, StepCall) (StepAnswer, error) {
		return StepAnswer{}, errors.New("broken pipe")
	}
	if rep, err = pr.Show(ctx, 1); err != nil || !strings.HasPrefix(rep.Watch, "not watched: ") {
		t.Fatalf("a broken channel: %+v %v", rep, err)
	}
}

// A merged or closed pull request is reported, and then no longer watched:
// the command says so instead of "watched" (01a1301d-55e3).
func TestForgePRWatchEnds(t *testing.T) {
	ctx := context.Background()
	for _, state := range []string{"merged", "closed"} {
		f := fakeforge.New(t, "gitea")
		home := t.TempDir()
		provisionTestForge(t, home, f.Host, fakeforge.Token)
		reported := 0
		pr := ForgePR{Home: home, Repository: "https://" + f.Host + "/team/app.git", API: &forge.Client{HTTP: f.Client},
			Step: func(context.Context, string, StepCall) (StepAnswer, error) {
				reported++
				return StepAnswer{OK: true, Status: StepDone, Result: json.RawMessage(`{}`)}, nil
			}}
		if _, err := pr.Open(ctx, OpenPull{Title: "Fix", Head: "feat/x", Base: "main"}); err != nil {
			t.Fatal(err)
		}
		f.SetPull(1, func(p *fakeforge.Pull) { p.State = state })
		rep, err := pr.Show(ctx, 1)
		if err != nil || rep.Watch != "not watched: it is "+state || reported != 2 {
			t.Errorf("%s: %+v %v, %d reports", state, rep.Watch, err, reported)
		}
	}
}
