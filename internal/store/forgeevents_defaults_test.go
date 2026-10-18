package store

import (
	"context"
	"strings"
	"testing"
)

// The gates wake is the worker's cue only where the launcher does not
// perform the transition itself (increment 2d-d): an observation whose
// automations name gates-green, with both gates green at the head, records
// the change and wakes no one; one whose automations lack it, because the
// process disabled the default, gets the gates need as before.
func TestGatesWakeOnlyWithoutGatesGreen(t *testing.T) {
	ctx := context.Background()
	e := newClaimStopEnv(t)
	a, _ := e.runningClaim(t, "task-1")
	o := observed(a.Branch)
	o.Automations = []string{"review-request", "ci-rerun", AutomationGatesGreen}
	if res, err := e.s.ObservePullWithToken(ctx, e.indepTk, o); err != nil || !res.Baseline {
		t.Fatalf("the baseline: %+v, %v", res, err)
	}
	// Both gates green at the head while the default runs: a change, no need.
	o.Labels, o.Comments, o.Checks = []string{"hands-reviewed"}, 1, "success"
	o.Content, o.Review = true, hands(o.HeadCommit, verdictReady)
	o.Passed = []PassedCheck{{Name: "go-test", URL: "https://ci.example/1"}}
	res, err := e.s.ObservePullWithToken(ctx, e.indepTk, o)
	if err != nil || res.EventID == "" || len(res.Needs) != 0 || len(res.Changes) == 0 {
		t.Fatalf("gates green with the default: %+v, %v", res, err)
	}
	if runs := settle(t, e.s, e.now); len(runs) != 0 {
		t.Fatalf("the default's gates woke someone: %+v", runs)
	}
	if got := forgeEvents(t, e.s, e.indepTk); len(got) != 0 {
		t.Fatalf("the default's gates woke the worker: %+v", got)
	}
	// A new head, and the process disabled the default: the gates need is
	// the worker's again.
	o.HeadCommit, o.Checks, o.Comments = head2, "pending", 2
	o.Automations = []string{"review-request", "ci-rerun"}
	o.Review, o.Passed = hands(head2, verdictReady), nil
	if res, err = e.s.ObservePullWithToken(ctx, e.indepTk, o); err != nil || len(res.Needs) != 0 {
		t.Fatalf("a READY review while CI runs: %+v, %v", res, err)
	}
	o.Checks, o.Passed = "success", []PassedCheck{{Name: "go-test", URL: "https://ci.example/2"}}
	if res, err = e.s.ObservePullWithToken(ctx, e.indepTk, o); err != nil || strings.Join(res.Needs, ",") != NeedGates {
		t.Fatalf("gates green without the default: %+v, %v", res, err)
	}
	settle(t, e.s, e.now)
	got := forgeEvents(t, e.s, e.indepTk)
	if len(got) != 1 || got[0].Kind != KindActionNeeded || !strings.Contains(got[0].Text, "Both gates are green at the head") {
		t.Fatalf("the gates' message without the default: %+v", got)
	}
}
