package store

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
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

// The heads the tests push.
const (
	head1 = "0123456789abcdef0123456789abcdef01234567"
	head2 = "89abcdef0123456789abcdef0123456789abcdef"
	head3 = "fedcba9876543210fedcba9876543210fedcba98"
	head4 = "1111111111111111111111111111111111111111"
	head5 = "2222222222222222222222222222222222222222"
)

// hands is the external review at head with the verdict, as the worker's
// launcher reads it.
func hands(head, verdict string) *PullReview {
	return &PullReview{Author: "hands-bot", URL: "https://git.example.test/crew/project-a/pulls/7#issuecomment-" + head[:6],
		Head: head, AtHead: true, Verdict: verdict, Blocker: verdict != verdictReady,
		Body: "[hands-bot review] reviewed at head " + head + "\n\nBLOCKERS\n- [BLOCKER] The parser loops on an empty line.\n\nVERDICT\n" + verdict}
}

// The worker's first observation of its attempt's pull request is the
// baseline. A change that only reports progress (a label, a comment, CI
// concluding without a review, a READY review while CI runs) is recorded,
// journaled and wakes no one. A review with a BLOCKER at the head, CI
// failing on the change, both gates green at the head, and a merge each
// wake the worker with one action.needed message holding the content; the
// coordinators hear of one while the result is submitted, and of a merge.
func TestObservePull(t *testing.T) {
	ctx := context.Background()
	e := newClaimStopEnv(t)
	a, _ := e.runningClaim(t, "task-1")
	o := observed(a.Branch)
	res, err := e.s.ObservePullWithToken(ctx, e.indepTk, o)
	if err != nil || !res.Baseline || res.AttemptID != a.ID || res.EventID != "" {
		t.Fatalf("the baseline: %+v, %v", res, err)
	}
	quiet := func(what string) {
		t.Helper()
		if runs := settle(t, e.s, e.now); len(runs) != 0 {
			t.Fatalf("%s: runs %+v", what, runs)
		}
		if got := forgeEvents(t, e.s, e.indepTk); len(got) != 0 {
			t.Fatalf("%s woke the worker: %+v", what, got)
		}
	}
	// Progress: hands picks it up and comments; CI passes with no review.
	o.Labels, o.Comments, o.Checks = []string{"review-this", "hands-reviewing"}, 1, "success"
	if res, err = e.s.ObservePullWithToken(ctx, e.indepTk, o); err != nil || res.EventID == "" || len(res.Needs) != 0 ||
		len(res.Changes) != 3 {
		t.Fatalf("progress: %+v, %v", res, err)
	}
	quiet("progress")
	// A READY review on a new head while CI runs there: no decision yet.
	o.HeadCommit, o.Checks, o.Comments = head2, "pending", 2
	o.Content, o.Review = true, hands(head2, verdictReady)
	if res, err = e.s.ObservePullWithToken(ctx, e.indepTk, o); err != nil || len(res.Needs) != 0 {
		t.Fatalf("a READY review while CI runs: %+v, %v", res, err)
	}
	quiet("a READY review while CI runs")
	// CI passes at that head: both gates are green.
	o.Checks, o.Passed = "success", []PassedCheck{{Name: "go-test", URL: "https://ci.example/1"}, {Name: "go-lint"}}
	if res, err = e.s.ObservePullWithToken(ctx, e.indepTk, o); err != nil || strings.Join(res.Needs, ",") != NeedGates {
		t.Fatalf("gates green: %+v, %v", res, err)
	}
	settle(t, e.s, e.now)
	got := forgeEvents(t, e.s, e.indepTk)
	if len(got) != 1 || got[0].Kind != KindActionNeeded || got[0].Action == nil || len(got[0].Action.Subjects) != 1 {
		t.Fatalf("the gates' message: %+v", got)
	}
	sub := got[0].Action.Subjects[0]
	if sub.Kind != "pull" || strings.Join(sub.Needs, ",") != NeedGates || sub.Review == nil || sub.Review.Verdict != verdictReady ||
		len(sub.Passed) != 2 || !strings.Contains(got[0].Text, "Both gates are green at the head") ||
		!strings.Contains(got[0].Text, "aicrew-agent forge pr ready --pr 7") || !strings.Contains(got[0].Text, "go-test, go-lint") {
		t.Fatalf("the gates' subject: %+v\n%s", sub, got[0].Text)
	}
	ackTok(t, e.s, e.indepTk, got)
	// The same observation again, content and all: nothing new.
	if res, err = e.s.ObservePullWithToken(ctx, e.indepTk, o); err != nil || res.EventID != "" {
		t.Fatalf("the gates again: %+v, %v", res, err)
	}
	// A new head with a review that has a BLOCKER: the body reaches the
	// worker.
	o.HeadCommit, o.Checks, o.Comments, o.Passed = head3, "pending", 3, nil
	o.Review = hands(head3, "RETURN_TO_IMPLEMENTATION")
	if res, err = e.s.ObservePullWithToken(ctx, e.indepTk, o); err != nil || strings.Join(res.Needs, ",") != NeedReview {
		t.Fatalf("a BLOCKER: %+v, %v", res, err)
	}
	settle(t, e.s, e.now)
	got = forgeEvents(t, e.s, e.indepTk)
	if len(got) != 1 || !strings.Contains(got[0].Text, "The external review at the head asks for changes (verdict RETURN_TO_IMPLEMENTATION)") ||
		!strings.Contains(got[0].Text, "  | - [BLOCKER] The parser loops on an empty line.") ||
		got[0].Action.Subjects[0].Review == nil || !got[0].Action.Subjects[0].Review.Blocker {
		t.Fatalf("the BLOCKER's message: %+v", got)
	}
	ackTok(t, e.s, e.indepTk, got)
	// CI fails at that head: the failed job and its excerpt reach the
	// worker, and the review, already announced, is not repeated.
	o.Checks = "failure"
	o.Failed = []FailedCheck{{Name: "go-test", Conclusion: "failure", URL: "https://ci.example/2", Excerpt: "--- FAIL: TestParser\nFAIL"},
		{Name: "go-race", Conclusion: "failure", NoLog: "the forge refuses this job's log to the token"}}
	if res, err = e.s.ObservePullWithToken(ctx, e.indepTk, o); err != nil || strings.Join(res.Needs, ",") != NeedCI {
		t.Fatalf("CI fails: %+v, %v", res, err)
	}
	settle(t, e.s, e.now)
	got = forgeEvents(t, e.s, e.indepTk)
	if len(got) != 1 || !strings.Contains(got[0].Text, "CI failed on the change:\n  - go-test: failure https://ci.example/2\n  | --- FAIL: TestParser") ||
		!strings.Contains(got[0].Text, "no log excerpt: the forge refuses") || len(got[0].Action.Subjects[0].Failed) != 2 ||
		strings.Contains(got[0].Text, "asks for changes") {
		t.Fatalf("the CI failure's message: %+v", got)
	}
	ackTok(t, e.s, e.indepTk, got)
	if l := forgeEvents(t, e.s, e.leadTok); len(l) != 0 {
		t.Fatalf("the coordinator heard of work in progress: %+v", l)
	}
	// Submitted: a failure on a new head reaches the coordinator too, a
	// label does not.
	if _, err := e.s.db.Exec(`UPDATE attempts SET phase = ? WHERE id = ?`, string(PhaseSubmitted), a.ID); err != nil {
		t.Fatal(err)
	}
	o.Labels = append(o.Labels, "hands-reviewed")
	if _, err = e.s.ObservePullWithToken(ctx, e.indepTk, o); err != nil {
		t.Fatal(err)
	}
	quiet("a label while submitted")
	// A new head fails, its content not read this time.
	o.HeadCommit, o.Content, o.Review, o.Failed = head4, false, nil, nil
	if _, err = e.s.ObservePullWithToken(ctx, e.indepTk, o); err != nil {
		t.Fatal(err)
	}
	settle(t, e.s, e.now)
	if w, l := forgeEvents(t, e.s, e.indepTk), forgeEvents(t, e.s, e.leadTok); len(w) != 1 || len(l) != 1 || w[0].ID != l[0].ID {
		t.Fatalf("a failure while submitted: worker %+v, coordinator %+v", w, l)
	}
	// A merge reaches both, whatever the phase.
	if _, err := e.s.db.Exec(`UPDATE attempts SET phase = ? WHERE id = ?`, string(PhaseAccepted), a.ID); err != nil {
		t.Fatal(err)
	}
	o.State = "merged"
	if res, err = e.s.ObservePullWithToken(ctx, e.indepTk, o); err != nil || strings.Join(res.Needs, ",") != NeedMerged {
		t.Fatalf("the merge: %+v, %v", res, err)
	}
	settle(t, e.s, e.now)
	if w, l := forgeEvents(t, e.s, e.indepTk), forgeEvents(t, e.s, e.leadTok); len(w) != 2 || len(l) != 2 ||
		!strings.Contains(w[1].Text, "The pull request was merged.") {
		t.Fatalf("after the merge the worker has %+v and the coordinator %+v", w, l)
	}
}

// ackTok acknowledges the items for the token's member.
func ackTok(t *testing.T, s *Store, token string, items []InboxItem) {
	t.Helper()
	ids := make([]string, 0, len(items))
	for _, it := range items {
		ids = append(ids, it.ID)
	}
	if _, err := s.AckWithToken(context.Background(), "ack-"+ids[0], token, ids); err != nil {
		t.Fatal(err)
	}
}

// A review body and excerpts as long as the launcher may send, of
// characters JSON escapes, are cut to what an event carries.
func TestObservedContentIsBounded(t *testing.T) {
	ctx := context.Background()
	e := newClaimStopEnv(t)
	a, _ := e.runningClaim(t, "task-1")
	o := observed(a.Branch)
	if _, err := e.s.ObservePullWithToken(ctx, e.indepTk, o); err != nil {
		t.Fatal(err)
	}
	o.Checks, o.Content = "failure", true
	o.Review = hands(head1, "RETURN_TO_IMPLEMENTATION")
	o.Review.Body = strings.Repeat("<", maxObservedLen)
	for i := range 6 {
		o.Failed = append(o.Failed, FailedCheck{Name: fmt.Sprintf("job-%d", i), Excerpt: strings.Repeat("<", maxObservedLen)})
	}
	res, err := e.s.ObservePullWithToken(ctx, e.indepTk, o)
	if err != nil || strings.Join(res.Needs, ",") != NeedCI+","+NeedReview {
		t.Fatalf("%+v, %v", res, err)
	}
	var payload string
	if err := e.s.db.QueryRow(`SELECT payload FROM events WHERE id = ?`, res.EventID).Scan(&payload); err != nil || len(payload) > maxEventPayload {
		t.Fatalf("the event's payload: %d bytes, %v", len(payload), err)
	}
	settle(t, e.s, e.now)
	if got := forgeEvents(t, e.s, e.indepTk); len(got) != 1 || len(got[0].Text) > maxMessageText ||
		!got[0].Action.Subjects[0].Review.Cut {
		t.Fatalf("the message: %d", len(got))
	}
	o.Review.Body = strings.Repeat("x", maxObservedLen+1)
	if _, err := e.s.ObservePullWithToken(ctx, e.indepTk, o); !errors.Is(err, ErrInvalid) {
		t.Fatalf("a body over the bound: %v", err)
	}
	o.Review, o.Content = nil, false
	o.Failed = []FailedCheck{{Name: "x"}}
	if _, err := e.s.ObservePullWithToken(ctx, e.indepTk, o); !errors.Is(err, ErrInvalid) {
		t.Fatalf("content without the content mark: %v", err)
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

// Without a new read of the content, the failed checks read for another
// head are not shown as this head's: the message says where to read them.
func TestStaleChecksAreNotShown(t *testing.T) {
	ctx := context.Background()
	e := newClaimStopEnv(t)
	a, _ := e.runningClaim(t, "task-1")
	o := observed(a.Branch)
	if _, err := e.s.ObservePullWithToken(ctx, e.indepTk, o); err != nil {
		t.Fatal(err)
	}
	o.Checks, o.Content = "failure", true
	o.Failed = []FailedCheck{{Name: "old-job", Excerpt: "old failure"}}
	if _, err := e.s.ObservePullWithToken(ctx, e.indepTk, o); err != nil {
		t.Fatal(err)
	}
	settle(t, e.s, e.now)
	ackTok(t, e.s, e.indepTk, forgeEvents(t, e.s, e.indepTk))
	// A new head fails too, and the launcher could not read the content.
	o.HeadCommit, o.Content, o.Failed = head2, false, nil
	if res, err := e.s.ObservePullWithToken(ctx, e.indepTk, o); err != nil || strings.Join(res.Needs, ",") != NeedCI {
		t.Fatalf("%+v, %v", res, err)
	}
	settle(t, e.s, e.now)
	got := forgeEvents(t, e.s, e.indepTk)
	if len(got) != 1 || strings.Contains(got[0].Text, "old-job") || !strings.Contains(got[0].Text, "names the failed jobs") ||
		len(got[0].Action.Subjects[0].Failed) != 0 {
		t.Fatalf("the message: %+v", got)
	}
}

// A run whose events no longer agree keeps what holds at its last
// observation: a gates-green event followed by a blocking review on the
// same head wakes for the review alone, never "mark it ready" (hands review
// of #165).
func TestMergedRunKeepsWhatHolds(t *testing.T) {
	ctx := context.Background()
	e := newClaimStopEnv(t)
	a, _ := e.runningClaim(t, "task-1")
	o := observed(a.Branch)
	if _, err := e.s.ObservePullWithToken(ctx, e.indepTk, o); err != nil {
		t.Fatal(err)
	}
	o.Checks, o.Comments, o.Content = "success", 1, true
	o.Review, o.Passed = hands(head1, verdictReady), []PassedCheck{{Name: "go-test"}}
	if res, err := e.s.ObservePullWithToken(ctx, e.indepTk, o); err != nil || strings.Join(res.Needs, ",") != NeedGates {
		t.Fatalf("gates: %+v, %v", res, err)
	}
	*e.now = e.now.Add(10 * time.Second)
	o.Comments = 2
	o.Review = hands(head1, "RETURN_TO_IMPLEMENTATION")
	o.Review.URL += "-second"
	if res, err := e.s.ObservePullWithToken(ctx, e.indepTk, o); err != nil || strings.Join(res.Needs, ",") != NeedReview {
		t.Fatalf("the later BLOCKER: %+v, %v", res, err)
	}
	settle(t, e.s, e.now)
	got := forgeEvents(t, e.s, e.indepTk)
	if len(got) != 1 || strings.Join(got[0].Action.Subjects[0].Needs, ",") != NeedReview ||
		strings.Contains(got[0].Text, "Both gates are green") || strings.Contains(got[0].Text, "forge pr ready") ||
		!got[0].Action.Subjects[0].Review.Blocker || len(got[0].Action.Subjects[0].Passed) != 0 {
		t.Fatalf("the merged run: %+v", got)
	}
	ackTok(t, e.s, e.indepTk, got)
	// CI fails on one head with its jobs read, then on a newer head whose
	// content was not read: the run shows no job of the older head.
	o.HeadCommit, o.Checks, o.Review, o.Passed = head2, "failure", nil, nil
	o.Failed = []FailedCheck{{Name: "old-job", Excerpt: "old failure"}}
	if _, err := e.s.ObservePullWithToken(ctx, e.indepTk, o); err != nil {
		t.Fatal(err)
	}
	*e.now = e.now.Add(10 * time.Second)
	o.HeadCommit, o.Content, o.Failed = head3, false, nil
	if _, err := e.s.ObservePullWithToken(ctx, e.indepTk, o); err != nil {
		t.Fatal(err)
	}
	settle(t, e.s, e.now)
	got = forgeEvents(t, e.s, e.indepTk)
	if len(got) != 1 || strings.Contains(got[0].Text, "old-job") || got[0].Forge.HeadCommit != head3 ||
		len(got[0].Action.Subjects[0].Failed) != 0 {
		t.Fatalf("a failure on a newer head: %+v", got)
	}
}

// Metadata within every field bound but too large together (thirty passed
// checks with long names) still journals the event: the content is cut,
// the message says what was left out, and the worker is woken (hands review
// of #165). A review read at another head is refused.
func TestLargeMetadataStillWakes(t *testing.T) {
	ctx := context.Background()
	e := newClaimStopEnv(t)
	a, _ := e.runningClaim(t, "task-1")
	o := observed(a.Branch)
	if _, err := e.s.ObservePullWithToken(ctx, e.indepTk, o); err != nil {
		t.Fatal(err)
	}
	o.Checks, o.Comments, o.Content, o.Review = "success", 1, true, hands(head1, verdictReady)
	for i := range maxPassed {
		o.Passed = append(o.Passed, PassedCheck{Name: fmt.Sprintf("%03d-%s", i, strings.Repeat("n", 600)),
			URL: "https://ci.example/" + strings.Repeat("u", 400)})
	}
	res, err := e.s.ObservePullWithToken(ctx, e.indepTk, o)
	if err != nil || strings.Join(res.Needs, ",") != NeedGates {
		t.Fatalf("%+v, %v", res, err)
	}
	var payload string
	if err := e.s.db.QueryRow(`SELECT payload FROM events WHERE id = ?`, res.EventID).Scan(&payload); err != nil || len(payload) > maxEventPayload {
		t.Fatalf("the payload: %d bytes, %v", len(payload), err)
	}
	settle(t, e.s, e.now)
	got := forgeEvents(t, e.s, e.indepTk)
	if len(got) != 1 || !strings.Contains(got[0].Text, "Both gates are green") || !strings.Contains(got[0].Text, "Left out for room:") ||
		!strings.Contains(got[0].Text, "passed check(s)") {
		t.Fatalf("the message: %+v", got)
	}
	o.Review, o.Passed = hands(head2, verdictReady), nil
	if _, err := e.s.ObservePullWithToken(ctx, e.indepTk, o); !errors.Is(err, ErrInvalid) {
		t.Fatalf("a review read at another head: %v", err)
	}
}

// A run is checked against the pull request as aicrewd last recorded it,
// not only its own events: gates green, then CI running again on the same
// head before the queue runs (a change that needs no decision and queues
// nothing), is no longer "mark it ready", and wakes no one (hands review of
// #165). Gates green again then wakes.
func TestQueuedGatesAfterProgress(t *testing.T) {
	ctx := context.Background()
	e := newClaimStopEnv(t)
	a, _ := e.runningClaim(t, "task-1")
	o := observed(a.Branch)
	if _, err := e.s.ObservePullWithToken(ctx, e.indepTk, o); err != nil {
		t.Fatal(err)
	}
	o.Checks, o.Comments, o.Content = "success", 1, true
	o.Review, o.Passed = hands(head1, verdictReady), []PassedCheck{{Name: "go-test"}}
	if res, err := e.s.ObservePullWithToken(ctx, e.indepTk, o); err != nil || strings.Join(res.Needs, ",") != NeedGates {
		t.Fatalf("gates: %+v, %v", res, err)
	}
	*e.now = e.now.Add(10 * time.Second)
	// CI runs again on that head: recorded, queues nothing.
	o.Checks, o.Content, o.Review, o.Passed = "pending", false, nil, nil
	if res, err := e.s.ObservePullWithToken(ctx, e.indepTk, o); err != nil || len(res.Needs) != 0 {
		t.Fatalf("CI again: %+v, %v", res, err)
	}
	runs := settle(t, e.s, e.now)
	if got := forgeEvents(t, e.s, e.indepTk); len(got) != 0 || len(runs) != 1 || runs[0].MessageID != "" {
		t.Fatalf("a stale gates wake: %+v (runs %+v)", got, runs)
	}
	// CI passes again: gates green at the head, and a wake.
	o.Checks, o.Content, o.Review, o.Passed = "success", true, hands(head1, verdictReady), []PassedCheck{{Name: "go-test"}}
	if res, err := e.s.ObservePullWithToken(ctx, e.indepTk, o); err != nil || strings.Join(res.Needs, ",") != NeedGates {
		t.Fatalf("gates again: %+v, %v", res, err)
	}
	settle(t, e.s, e.now)
	if got := forgeEvents(t, e.s, e.indepTk); len(got) != 1 || !strings.Contains(got[0].Text, "Both gates are green") {
		t.Fatalf("the gates wake: %+v", got)
	}
}

// A declared check that failed after the push (2d-b) wakes the worker with
// the command, its exit and its output in the message's pre_push field; the
// coordinators hear nothing. The failure is the observation's: one at the
// same head with it again is nothing new, one without it (a run there
// passed) or at a new head clears a queued need, and a new head that fails
// again is a new need. The output is bounded.
func TestFailedCheckAfterPush(t *testing.T) {
	ctx := context.Background()
	e := newClaimStopEnv(t)
	a, _ := e.runningClaim(t, "task-1")
	o := observed(a.Branch)
	if _, err := e.s.ObservePullWithToken(ctx, e.indepTk, o); err != nil {
		t.Fatal(err)
	}
	fail := &PrePushFailure{Command: "bash scripts/check-repo.sh", Exit: 1, Output: "lint: main.go:12: unused variable x\nFAIL\n", Others: []string{"second"}}
	o.PrePush = fail
	res, err := e.s.ObservePullWithToken(ctx, e.indepTk, o)
	if err != nil || strings.Join(res.Needs, ",") != NeedPrePush || res.EventID == "" {
		t.Fatalf("the failed check: %+v, %v", res, err)
	}
	settle(t, e.s, e.now)
	got := forgeEvents(t, e.s, e.indepTk)
	if len(got) != 1 || len(got[0].Action.Subjects) != 1 {
		t.Fatalf("the worker's message: %+v", got)
	}
	sub := got[0].Action.Subjects[0]
	if strings.Join(sub.Needs, ",") != NeedPrePush || sub.PrePush == nil || sub.PrePush.Command != fail.Command || sub.PrePush.Exit != 1 ||
		sub.PrePush.Output != fail.Output || strings.Join(sub.PrePush.Others, ",") != "second" || sub.Review != nil || sub.Failed != nil {
		t.Fatalf("the pre_push subject: %+v", sub)
	}
	if text := got[0].Text; !strings.Contains(text, "A declared check failed after the push: `bash scripts/check-repo.sh` (exit 1).") ||
		!strings.Contains(text, "\n  | lint: main.go:12: unused variable x\n  | FAIL") ||
		!strings.Contains(text, "Other checks failed at this point too: second") {
		t.Fatalf("the message's text:\n%s", text)
	}
	if l := forgeEvents(t, e.s, e.leadTok); len(l) != 0 {
		t.Fatalf("the coordinator heard of a failed check: %+v", l)
	}
	ackTok(t, e.s, e.indepTk, got)
	// The same observation again, a poll: nothing new.
	if res, err = e.s.ObservePullWithToken(ctx, e.indepTk, o); err != nil || res.EventID != "" {
		t.Fatalf("the failure again: %+v, %v", res, err)
	}
	// A new head fails too, and a head after it passes before the queue
	// runs: the need is gone with the head.
	o.HeadCommit = head2
	if res, err = e.s.ObservePullWithToken(ctx, e.indepTk, o); err != nil || strings.Join(res.Needs, ",") != NeedPrePush {
		t.Fatalf("a new head's failure: %+v, %v", res, err)
	}
	o.HeadCommit, o.PrePush = head3, nil
	if res, err = e.s.ObservePullWithToken(ctx, e.indepTk, o); err != nil || len(res.Needs) != 0 {
		t.Fatalf("a passing head: %+v, %v", res, err)
	}
	if runs := settle(t, e.s, e.now); len(runs) != 1 || runs[0].MessageID != "" {
		t.Fatalf("a cleared need was delivered: %+v", runs)
	}
	if got := forgeEvents(t, e.s, e.indepTk); len(got) != 0 {
		t.Fatalf("a passing head woke the worker: %+v", got)
	}
	// A failure at that head, then a run there that passes before the
	// queue runs: cleared too. Then the failure again: delivered.
	o.PrePush = fail
	if res, err = e.s.ObservePullWithToken(ctx, e.indepTk, o); err != nil || strings.Join(res.Needs, ",") != NeedPrePush {
		t.Fatalf("the failure at the third head: %+v, %v", res, err)
	}
	o.PrePush = nil
	if _, err = e.s.ObservePullWithToken(ctx, e.indepTk, o); err != nil {
		t.Fatal(err)
	}
	settle(t, e.s, e.now)
	if got := forgeEvents(t, e.s, e.indepTk); len(got) != 0 {
		t.Fatalf("a passing run woke the worker: %+v", got)
	}
	long := &PrePushFailure{Command: "bash scripts/check-repo.sh", Exit: 2, Output: strings.Repeat("x", 20<<10)}
	o.PrePush = long
	if res, err = e.s.ObservePullWithToken(ctx, e.indepTk, o); err != nil || strings.Join(res.Needs, ",") != NeedPrePush {
		t.Fatalf("the failure once more: %+v, %v", res, err)
	}
	settle(t, e.s, e.now)
	got = forgeEvents(t, e.s, e.indepTk)
	if len(got) != 1 || got[0].Action.Subjects[0].PrePush == nil || !got[0].Action.Subjects[0].PrePush.Cut ||
		len(got[0].Action.Subjects[0].PrePush.Output) > maxPrePushOut || !strings.Contains(got[0].Text, "[cut: the home's logs/automations.jsonl holds it whole]") {
		t.Fatalf("the long output's message: %+v", got)
	}
	// A failure that names no command is refused.
	o.PrePush = &PrePushFailure{Exit: 1, Output: "x"}
	if _, err := e.s.ObservePullWithToken(ctx, e.indepTk, o); !errors.Is(err, ErrInvalid) {
		t.Fatalf("a failure without a command: %v", err)
	}
}
