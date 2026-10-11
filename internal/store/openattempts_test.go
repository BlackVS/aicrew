package store

import (
	"context"
	"errors"
	"testing"
	"time"
)

// The team's coordinator lists its open attempts with its session token: an
// offer, flagged stale once the coordinator's session resumed, and an
// independent member's running claim, each with its next action; a closed
// attempt is not listed. Any other member is refused.
func TestOpenAttemptsWithToken(t *testing.T) {
	ctx := context.Background()
	e := newClaimStopEnv(t)
	wb, err := e.s.AuthenticateSessionToken(ctx, e.workerTok)
	if err != nil {
		t.Fatal(err)
	}
	in := OfferInput{WorkerAgentID: wb.AgentID, Task: TaskRef{HubID: "hub-test", ProjectID: "project-t", TaskID: "task-1"},
		ExpectedRevision: 3, BaseCommit: "base-1", Branch: "work/task-1", Repository: testRepository, Process: testPin,
		ExpiresAt: e.now.Add(time.Hour)}
	offer, st, err := e.s.BeginOfferWithToken(ctx, "offer-1", e.leadTok, in)
	if err != nil {
		t.Fatal(err)
	}
	e.reader.commit(st.CoordinationProof, receiptFor(offer, st, "res-1", "1", 4))
	if offer, _, err = e.settle(t, e.leadTok, offer, st, HintCommitted); err != nil {
		t.Fatal(err)
	}
	claim, _ := e.runningClaim(t, "task-2")

	got, err := e.s.OpenAttemptsWithToken(ctx, e.leadTok)
	if err != nil {
		t.Fatal(err)
	}
	o, c := byID(got, offer.ID), byID(got, claim.ID)
	if len(got.Attempts) != 2 || o.ID == "" || c.ID == "" {
		t.Fatalf("open attempts = %+v, want the offer and the claim", got.Attempts)
	}
	if o.State != AttemptOffered || o.Stale || o.Expired || o.Declined || o.NextAction != nextWaitAccept ||
		o.WorkerLabel != "worker" || o.Task.TaskID != "task-1" || o.CoordinatorGeneration != got.CoordinatorGeneration {
		t.Errorf("a fresh offer = %+v", o)
	}
	if c.State != AttemptRunning || c.NextAction != nextFollowRunning || c.WorkerLabel != "indep" || c.Stale {
		t.Errorf("a running claim = %+v", c)
	}

	// The coordinator restarts: its offer is stale now, and the list says
	// to withdraw it.
	lead := e.resume(t, e.leadTok)
	got, err = e.s.OpenAttemptsWithToken(ctx, lead)
	if err != nil {
		t.Fatal(err)
	}
	if o := byID(got, offer.ID); !o.Stale || o.NextAction != nextWithdrawReoffer {
		t.Errorf("an offer after the coordinator resumed = %+v, want stale with the withdraw action", o)
	}

	// The offer expires: still listed, expired.
	*e.now = e.now.Add(2 * time.Hour)
	if got, err = e.s.OpenAttemptsWithToken(ctx, lead); err != nil || !byID(got, offer.ID).Expired {
		t.Fatalf("an expired offer = %+v, %v", got.Attempts, err)
	}

	// Withdrawn, the offer is closed and no longer listed.
	a, wst, err := e.s.BeginWithdrawWithToken(ctx, "withdraw-1", lead, offer.ID)
	if err != nil {
		t.Fatal(err)
	}
	e.reader.commit(wst.CoordinationProof, receiptFor(a, wst, "res-1", "2", 5))
	if a, _, err = e.settle(t, lead, a, wst, HintCommitted); err != nil || a.State != AttemptClosed {
		t.Fatalf("withdraw the offer: %+v %v", a, err)
	}
	if got, err = e.s.OpenAttemptsWithToken(ctx, lead); err != nil || len(got.Attempts) != 1 || got.Attempts[0].ID != claim.ID {
		t.Fatalf("after the withdrawal = %+v, %v; want the claim only", got.Attempts, err)
	}

	for name, tok := range map[string]string{"worker": e.workerTok, "independent member": e.indepTk} {
		if _, err := e.s.OpenAttemptsWithToken(ctx, tok); !errors.Is(err, ErrForbidden) {
			t.Errorf("the %s's list: got %v, want ErrForbidden", name, err)
		}
	}
}

// byID is the listed attempt with id, or the zero value.
func byID(o OpenAttempts, id string) OpenAttempt {
	for _, a := range o.Attempts {
		if a.ID == id {
			return a
		}
	}
	return OpenAttempt{}
}
