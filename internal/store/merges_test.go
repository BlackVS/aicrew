package store

import (
	"context"
	"errors"
	"testing"
)

// An attempt waits for the human merge from the coordinator's acceptance of
// its latest result until the delivery of that result is confirmed: not
// while the result is only submitted or sent back for rework, and not once
// the delivery is confirmed. The worker's last observation of the pull
// request comes with it. The operator and an architect credential read the
// list; a member does not.
func TestPendingMerges(t *testing.T) {
	ctx := context.Background()
	e, a := running(t)
	op := operator(t)
	list := func(team string) []PendingMerge {
		t.Helper()
		got, err := e.s.PendingMerges(ctx, op, team)
		if err != nil {
			t.Fatal(err)
		}
		return got
	}
	a = e.mustWork(t, "w1", a, IntentSubmit, "https://forge.example/team/app/pull/7")
	if got := list(""); len(got) != 0 {
		t.Fatalf("a submitted result is listed: %+v", got)
	}
	if _, err := e.review(t, "r1", a, e.lead, 1, ReviewRework); err != nil {
		t.Fatal(err)
	}
	if got := list(""); len(got) != 0 {
		t.Fatalf("a result in rework is listed: %+v", got)
	}
	a, _ = e.s.GetAttempt(ctx, a.ID)
	a = e.mustWork(t, "w-resume", a, IntentResume, "")
	a = e.mustWork(t, "w2", a, IntentSubmit, "https://forge.example/team/app/pull/7#2")
	if _, err := e.review(t, "r2", a, e.lead, 2, ReviewAccept); err != nil {
		t.Fatal(err)
	}
	if _, err := e.s.db.Exec(`INSERT INTO attempt_pulls (attempt_id, number, url, state, head_commit, labels, checks, comments, observed_at)
		VALUES (?, 7, 'https://forge.example/team/app/pull/7', 'open', '0123456789abcdef0123456789abcdef01234567',
		'["hands-reviewed"]', 'success', 3, '2026-10-12T10:00:00Z')`, a.ID); err != nil {
		t.Fatal(err)
	}
	got := list("")
	if len(got) != 1 {
		t.Fatalf("an accepted result: %+v", got)
	}
	m := got[0]
	if m.AttemptID != a.ID || m.TeamID != e.tm.ID || m.TeamName != e.tm.Name || m.ResultSeq != 2 ||
		m.ResultRef != "https://forge.example/team/app/pull/7#2" || m.WorkerID != e.builder.agent.ID || m.WorkerLabel != "builder" ||
		m.Task != a.Task || m.Branch != a.Branch || m.SubmittedAt == "" {
		t.Fatalf("the pending merge: %+v", m)
	}
	if p := m.Pull; p == nil || p.Number != 7 || p.Checks != "success" || len(p.Labels) != 1 || p.Labels[0] != "hands-reviewed" ||
		p.Comments != 3 {
		t.Fatalf("the pull request as last seen: %+v", m.Pull)
	}
	if got := list(e.tm.ID); len(got) != 1 {
		t.Fatalf("by team: %+v", got)
	}
	if _, err := e.s.PendingMerges(ctx, op, "no-such-team"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("an unknown team: %v", err)
	}
	cred, bearer, err := e.s.IssueArchitectCredential(ctx, op, "issue-1", "planning")
	if err != nil {
		t.Fatal(err)
	}
	arch, err := e.s.AuthenticateArchitect(ctx, bearer)
	if err != nil || cred.ID == "" {
		t.Fatal(err)
	}
	if got, err := e.s.PendingMerges(ctx, arch, ""); err != nil || len(got) != 1 {
		t.Fatalf("the architect reads: %+v %v", got, err)
	}
	if _, err := e.s.PendingMerges(ctx, e.lead.caller, ""); !errors.Is(err, ErrForbidden) {
		t.Fatalf("a member reads: %v", err)
	}
	if _, err := e.s.ConfirmDelivery(ctx, e.lead.caller, "del-1", a.ID, DeliveryConfirmation{SessionID: e.lead.sess.ID,
		Generation: e.lead.sess.Generation, ResultSeq: 2, Evidence: deliveryEvidence}); err != nil {
		t.Fatal(err)
	}
	if got := list(""); len(got) != 0 {
		t.Fatalf("a confirmed delivery is listed: %+v", got)
	}
}
