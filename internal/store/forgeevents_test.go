package store

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// observed is the claim's pull request as the fake forge would report it.
func observed(branch string) PullObservation {
	return PullObservation{Host: "git.example.test", Repository: "crew/project-a", Number: 7,
		URL: "https://git.example.test/crew/project-a/pulls/7", State: "open", Head: branch,
		HeadCommit: "0123456789abcdef0123456789abcdef01234567", Labels: []string{"review-this"}, Checks: "pending"}
}

// forgeEvents is the forge.event messages waiting in the token's inbox.
func forgeEvents(t *testing.T, s *Store, token string) []InboxItem {
	t.Helper()
	items, err := s.ReadInboxWithToken(context.Background(), token, 100)
	if err != nil {
		t.Fatal(err)
	}
	var out []InboxItem
	for _, it := range items {
		if it.Forge != nil {
			out = append(out, it)
		}
	}
	return out
}

// The worker's first observation of its attempt's pull request is the
// baseline and announces nothing; each later change is announced once, to
// the worker, and a repeated report announces nothing again. A label
// removed or a new head with CI starting is recorded without a message.
// The coordinators hear of a change while the result is submitted, and of a
// merge whatever the phase.
func TestObservePull(t *testing.T) {
	ctx := context.Background()
	e := newClaimStopEnv(t)
	a, _ := e.runningClaim(t, "task-1")
	o := observed(a.Branch)
	res, err := e.s.ObservePullWithToken(ctx, e.indepTk, o)
	if err != nil || !res.Baseline || res.AttemptID != a.ID || res.MessageID != "" {
		t.Fatalf("the baseline: %+v, %v", res, err)
	}
	// hands picks the pull request up and reviews it; CI concludes.
	o.Labels, o.Checks, o.Comments = []string{"hands-reviewed"}, "success", 1
	res, err = e.s.ObservePullWithToken(ctx, e.indepTk, o)
	want := []string{"label hands-reviewed added", "CI concluded success at 0123456789ab", "1 new comment(s)"}
	if err != nil || res.Baseline || res.MessageID == "" || strings.Join(res.Changes, "|") != strings.Join(want, "|") {
		t.Fatalf("the review: %+v, %v", res, err)
	}
	if again, err := e.s.ObservePullWithToken(ctx, e.indepTk, o); err != nil || again.MessageID != "" || len(again.Changes) != 0 {
		t.Fatalf("the same observation again: %+v, %v", again, err)
	}
	// The worker's own forge pr command reports a baseline: its own label
	// announces nothing.
	cmd := o
	cmd.Labels, cmd.Baseline = []string{"hands-reviewed", "review-this"}, true
	if res, err := e.s.ObservePullWithToken(ctx, e.indepTk, cmd); err != nil || !res.Baseline || res.MessageID != "" {
		t.Fatalf("a command's report: %+v, %v", res, err)
	}
	o.Labels = cmd.Labels
	evs := forgeEvents(t, e.s, e.indepTk)
	if len(evs) != 1 || evs[0].ID != res.MessageID || evs[0].AttemptID != a.ID || evs[0].Kind != KindLifecycle ||
		evs[0].Forge.Pull != 7 || evs[0].Forge.Checks != "success" || !strings.Contains(evs[0].Text, "forge.event: pull request #7") {
		t.Fatalf("the worker's forge events: %+v", evs)
	}
	if got := forgeEvents(t, e.s, e.leadTok); len(got) != 0 {
		t.Fatalf("the coordinator heard of a review before a submit: %+v", got)
	}
	// The worker pushes a fix: the labels go, CI starts on the new head.
	o.Labels, o.Checks, o.HeadCommit = []string{}, "pending", "89abcdef0123456789abcdef0123456789abcdef"
	if res, err = e.s.ObservePullWithToken(ctx, e.indepTk, o); err != nil || res.MessageID != "" || len(res.Changes) != 0 {
		t.Fatalf("a new head: %+v, %v", res, err)
	}
	// Submitted: CI failing reaches the coordinator too.
	if _, err := e.s.db.Exec(`UPDATE attempts SET phase = ? WHERE id = ?`, string(PhaseSubmitted), a.ID); err != nil {
		t.Fatal(err)
	}
	o.Checks = "failure"
	if res, err = e.s.ObservePullWithToken(ctx, e.indepTk, o); err != nil || res.MessageID == "" {
		t.Fatalf("CI fails: %+v, %v", res, err)
	}
	if got := forgeEvents(t, e.s, e.leadTok); len(got) != 1 || got[0].ID != res.MessageID {
		t.Fatalf("the coordinator's forge events after a submit: %+v", got)
	}
	// A merge reaches both, whatever the phase.
	if _, err := e.s.db.Exec(`UPDATE attempts SET phase = ? WHERE id = ?`, string(PhaseAccepted), a.ID); err != nil {
		t.Fatal(err)
	}
	o.State = "merged"
	if res, err = e.s.ObservePullWithToken(ctx, e.indepTk, o); err != nil || res.Changes[0] != "the pull request was merged" {
		t.Fatalf("the merge: %+v, %v", res, err)
	}
	if w, l := forgeEvents(t, e.s, e.indepTk), forgeEvents(t, e.s, e.leadTok); len(w) != 3 || len(l) != 2 {
		t.Fatalf("after the merge the worker has %d forge events and the coordinator %d", len(w), len(l))
	}
}

// Only the member whose running attempt the pull request is may report it:
// another member, another branch or another repository is refused, and so
// is an observation that is not well formed.
func TestObservePullRefusals(t *testing.T) {
	ctx := context.Background()
	e := newClaimStopEnv(t)
	a, _ := e.runningClaim(t, "task-1")
	for name, c := range map[string]struct {
		token string
		o     func(PullObservation) PullObservation
		want  error
	}{
		"another member":         {e.workerTok, func(o PullObservation) PullObservation { return o }, ErrNoAttempt},
		"another branch":         {e.indepTk, func(o PullObservation) PullObservation { o.Head = "work/other"; return o }, ErrNoAttempt},
		"another repository":     {e.indepTk, func(o PullObservation) PullObservation { o.Repository = "crew/other"; return o }, ErrNoAttempt},
		"another host":           {e.indepTk, func(o PullObservation) PullObservation { o.Host = "github.com"; return o }, ErrNoAttempt},
		"no number":              {e.indepTk, func(o PullObservation) PullObservation { o.Number = 0; return o }, ErrInvalid},
		"an unknown state":       {e.indepTk, func(o PullObservation) PullObservation { o.State = "draft"; return o }, ErrInvalid},
		"unknown checks":         {e.indepTk, func(o PullObservation) PullObservation { o.Checks = "green"; return o }, ErrInvalid},
		"a plain-http url":       {e.indepTk, func(o PullObservation) PullObservation { o.URL = "http://x/1"; return o }, ErrInvalid},
		"an uppercase host":      {e.indepTk, func(o PullObservation) PullObservation { o.Host = "Git.example.test"; return o }, ErrInvalid},
		"a bad commit":           {e.indepTk, func(o PullObservation) PullObservation { o.HeadCommit = "HEAD"; return o }, ErrInvalid},
		"a label with a newline": {e.indepTk, func(o PullObservation) PullObservation { o.Labels = []string{"a\nb"}; return o }, ErrInvalid},
		"no token":               {"not-a-token", func(o PullObservation) PullObservation { return o }, ErrTokenInvalid},
	} {
		if _, err := e.s.ObservePullWithToken(ctx, c.token, c.o(observed(a.Branch))); !errors.Is(err, c.want) {
			t.Errorf("%s: %v, want %v", name, err, c.want)
		}
	}
	// The repository's path is matched without regard to case, as forges do.
	o := observed(a.Branch)
	o.Repository = "Crew/Project-A"
	if res, err := e.s.ObservePullWithToken(ctx, e.indepTk, o); err != nil || res.AttemptID != a.ID {
		t.Errorf("the path in another case: %+v, %v", res, err)
	}
}
