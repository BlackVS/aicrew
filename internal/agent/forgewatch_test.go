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
	mu       sync.Mutex
	st       forge.PullStatus
	content  forge.PullContent
	reads    int
	contents int
	token    string
}

func (f *fakeStatus) GetPullContent(_ context.Context, host string, k forge.Kind, token, path string, n int64) (forge.Pull, forge.PullContent, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.contents++
	if token != f.token || n != f.st.Number {
		return forge.Pull{}, forge.PullContent{}, forge.ErrNotFound
	}
	return f.st.Pull, f.content, nil
}

func (f *fakeStatus) contentReads() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.contents
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
	srv := s.serve(t)

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

	// hands posts a READY review at the head and CI passes: both gates are
	// green, a decision for the worker. The launcher reads the content for
	// it, since the status changed.
	go func() {
		time.Sleep(300 * time.Millisecond)
		fake.mu.Lock()
		fake.content = forge.PullContent{
			Comments: []forge.PullComment{{Kind: forge.CommentIssue, Author: "hands-bot", URL: "https://git.example.test/c/1",
				Body: "[hands-bot review] reviewed at head 0123456789ab\n\nVERDICT\nREADY_FOR_HUMAN_MERGE",
				Review: &forge.ReviewHeader{Reviewer: forge.ReviewerExternal, Head: "0123456789ab", AtHead: true,
					Verdict: "READY_FOR_HUMAN_MERGE"}}},
			Checks: []forge.Check{{Name: "build", State: forge.ChecksSuccess, URL: "https://ci.example/1"}}}
		fake.mu.Unlock()
		fake.set(func(st *forge.PullStatus) {
			st.Labels, st.Checks, st.Comments = []string{"hands-reviewed"}, forge.ChecksSuccess, 1
		})
	}()
	ten, _ := json.Marshal(map[string]int{"seconds": 10})
	start := time.Now()
	ans = s.call(t, &answers, StepCall{Op: "wait-inbox", Body: ten})
	if json.Unmarshal(ans.Result, &wake) != nil || len(wake.Pending) != 1 || wake.Pending[0].Kind != "action.needed" ||
		wake.Pending[0].AttemptID != id || time.Since(start) > 5*time.Second {
		t.Fatalf("a review during the wait: %s after %s", ans.Result, time.Since(start))
	}
	page := s.call(t, &answers, StepCall{Op: "inbox"})
	var inbox struct {
		Messages []store.InboxItem `json:"messages"`
	}
	if json.Unmarshal(page.Result, &inbox) != nil || len(inbox.Messages) != 1 || inbox.Messages[0].Forge == nil ||
		strings.Join(inbox.Messages[0].Forge.Changes, "|") != "label hands-reviewed added|CI concluded success at 0123456789ab|1 new comment(s)" ||
		inbox.Messages[0].Action == nil || strings.Join(inbox.Messages[0].Action.Subjects[0].Needs, ",") != store.NeedGates ||
		!strings.Contains(inbox.Messages[0].Text, "Both gates are green at the head") {
		t.Fatalf("the forge event: %s", page.Result)
	}
	if n := fake.contentReads(); n != 1 {
		t.Fatalf("the content was read %d times, want once for the one change", n)
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
	// The points the observations dispatched may still run and log: the
	// log is read once they ended.
	srv.waitPoints()
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

// The content an observation carries is the newest external review at the
// head, with its verdict deciding whether it blocks, and the failed and
// passed checks; a local review and a review of another head are left out.
func TestWithContent(t *testing.T) {
	head := "0123456789abcdef0123456789abcdef01234567"
	review := func(reviewer, at string, atHead bool, verdict, body string) forge.PullComment {
		return forge.PullComment{Kind: forge.CommentIssue, Author: reviewer, URL: "https://x/" + at + verdict, Body: body,
			Review: &forge.ReviewHeader{Reviewer: reviewer, Head: at, AtHead: atHead, Verdict: verdict}}
	}
	c := forge.PullContent{
		Comments: []forge.PullComment{
			review(forge.ReviewerExternal, "fedcba9", false, "RETURN_TO_IMPLEMENTATION", "old head"),
			review(forge.ReviewerExternal, head[:7], true, "RETURN_TO_IMPLEMENTATION", "first at the head"),
			review(forge.ReviewerExternal, head[:7], true, "READY_FOR_HUMAN_MERGE", "second at the head"),
			review(forge.ReviewerLocal, head[:7], true, "READY_FOR_HUMAN_MERGE", "the author's own"),
			{Kind: forge.CommentIssue, Author: "someone", Body: "a plain comment"},
		},
		Checks: []forge.Check{
			{Name: "lint", State: forge.ChecksSuccess, URL: "https://ci/1"},
			{Name: "test", State: forge.ChecksFailure, Conclusion: "failure", Log: &forge.JobLog{Excerpt: "--- FAIL", Cut: true}},
			{Name: "race", State: forge.ChecksFailure, Log: &forge.JobLog{None: "no log"}},
			{Name: "build", State: forge.ChecksPending},
		},
	}
	o := withContent(PullObservation{HeadCommit: head}, c)
	if !o.Content || o.Review == nil || o.Review.Body != "second at the head" || o.Review.Blocker || o.Review.Verdict != verdictReady {
		t.Fatalf("the review: %+v", o.Review)
	}
	if len(o.Failed) != 2 || o.Failed[0].Excerpt != "--- FAIL" || !o.Failed[0].Cut || o.Failed[1].NoLog != "no log" ||
		len(o.Passed) != 1 || o.Passed[0].Name != "lint" {
		t.Fatalf("the checks: %+v %+v", o.Failed, o.Passed)
	}
	// No verdict: a body with a BLOCKER blocks, one without does not.
	for body, blocks := range map[string]bool{"- [BLOCKER] the parser loops": true, "looks fine": false} {
		c.Comments = []forge.PullComment{review(forge.ReviewerExternal, head[:7], true, "", body)}
		if o := withContent(PullObservation{HeadCommit: head}, c); o.Review == nil || o.Review.Blocker != blocks {
			t.Errorf("%q: %+v", body, o.Review)
		}
	}
}

// Content read for another head than the status's is not sent: the branch
// moved between the two reads (hands review of #165).
func TestAddContentNeedsOneHead(t *testing.T) {
	st := forge.PullStatus{Pull: forge.Pull{Number: 4, HeadCommit: "0123456789abcdef0123456789abcdef01234567"}, Checks: forge.ChecksFailure}
	c := forge.PullContent{Checks: []forge.Check{{Name: "test", State: forge.ChecksFailure}}}
	o := observation("git.example.test", "crew/project-t", st)
	moved := st.Pull
	moved.HeadCommit = "89abcdef0123456789abcdef0123456789abcdef"
	if got, added := addContent(o, st, moved, c); added || got.Content || len(got.Failed) != 0 {
		t.Fatalf("another head's content: %+v %v", got, added)
	}
	if got, added := addContent(o, st, st.Pull, c); !added || !got.Content || len(got.Failed) != 1 {
		t.Fatalf("the same head's content: %+v %v", got, added)
	}
}
