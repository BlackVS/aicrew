package store

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

// lastAudit is the newest audit record of op: its caller kind and input.
func lastAudit(t *testing.T, s *Store, op string) (string, map[string]any) {
	t.Helper()
	var kind, input string
	if err := s.db.QueryRow(`SELECT caller_kind, input FROM audit WHERE operation = ? ORDER BY id DESC LIMIT 1`, op).
		Scan(&kind, &input); err != nil {
		t.Fatal(err)
	}
	var in map[string]any
	json.Unmarshal([]byte(input), &in)
	return kind, in
}

// The reconciler may only settle and close as recovered: every other
// command family refuses it, including those open to any caller.
func TestReconcilerCallerIsRefusedElsewhere(t *testing.T) {
	ctx := context.Background()
	e := newClaimStopEnv(t)
	a, seq := e.submitted(t, "task-1")
	r := ReconcilerCaller()
	for name, err := range map[string]error{
		"a challenge (open to any caller)": func() error { _, err := e.s.IssueAgentChallenge(ctx, r, "ch-1", e.indep); return err }(),
		"an agent record":                  func() error { _, err := e.s.CreateAgent(ctx, r, "ag-1", NewAgent{Label: "x"}); return err }(),
		"a membership": func() error {
			_, err := e.s.AddMember(ctx, r, "m-1", e.teamID, e.indep, RoleWorker)
			return err
		}(),
		"a session":     func() error { _, err := e.s.StartSession(ctx, r, "s-1", e.teamID); return err }(),
		"an acceptance": func() error { _, _, err := e.s.BeginAccept(ctx, r, "acc-1", a.ID, AcceptRequest{}); return err }(),
		"a stop":        func() error { _, err := e.s.RequestStop(ctx, r, "stop-1", a.ID, StopRequest{Reason: "x"}); return err }(),
		"a review": func() error {
			_, err := e.s.ReviewResult(ctx, r, "rev-1", a.ID, ResultReview{ResultSeq: seq, Decision: ReviewAccept})
			return err
		}(),
		"a delivery": func() error {
			_, err := e.s.ConfirmDelivery(ctx, r, "del-1", a.ID, DeliveryConfirmation{ResultSeq: seq, Evidence: deliveryEvidence})
			return err
		}(),
		"a work update": func() error {
			_, err := e.s.UpdateWork(ctx, r, &updatePort{attempt: a.ID}, "w-1", a.ID, WorkUpdate{Intent: IntentBlock, Detail: "x"})
			return err
		}(),
		"a revision refresh": func() error { _, err := e.s.refreshRevision(ctx, r, a, a.TaskRevision+1); return err }(),
	} {
		if !errors.Is(err, ErrForbidden) {
			t.Errorf("%s by the reconciler: %v", name, err)
		}
	}
	if after, _ := e.s.GetAttempt(ctx, a.ID); after.Revision != a.Revision {
		t.Fatalf("a refused command changed the attempt: %+v", after)
	}
}

// A crashed member's claim that aimem committed is settled by the reconciler
// on the committed receipt for that exact step, audited as the reconciler's
// with the receipt; a live proof is never cut off.
func TestReconcileStepSettlesOnTheReceipt(t *testing.T) {
	ctx := context.Background()
	e := newClaimStopEnv(t)
	a, st, err := e.s.BeginClaimWithToken(ctx, "claim-1", e.indepTk, e.claimInput("task-1"))
	if err != nil {
		t.Fatal(err)
	}
	// No receipt yet, and the proof lives: pending, and the proof is not
	// ended (the member may still be sending it).
	a2, set, err := e.s.ReconcileStep(ctx, e.reader, a.ID)
	if err != nil || set.Settled || a2.State != AttemptClaiming || openProofs(t, e.s, a.ID) != 1 {
		t.Fatalf("before the commit: %+v %+v %v", a2, set, err)
	}
	e.reader.commit(st.CoordinationProof, receiptFor(a, st, "res-1", "1", 4))
	a3, set, err := e.s.ReconcileStep(ctx, e.reader, a.ID)
	if err != nil || !set.Settled || set.Outcome != "committed" || a3.State != AttemptRunning || a3.LastReceiptID == "" {
		t.Fatalf("after the commit: %+v %+v %v", a3, set, err)
	}
	kind, in := lastAudit(t, e.s, opSettle)
	if kind != "reconciler" || in["receipt_id"] != a3.LastReceiptID || in["attempt_id"] != a.ID {
		t.Fatalf("the settlement's audit: %s %v", kind, in)
	}
}

// With no receipt, the reconciler's settle is final only by the scope's
// rules: once every proof of the step has expired and the grace period has
// passed, the step did not commit.
func TestReconcileStepNotCommittedOnlyAfterTheProofs(t *testing.T) {
	ctx := context.Background()
	e := newClaimStopEnv(t)
	a, _, err := e.s.BeginClaimWithToken(ctx, "claim-1", e.indepTk, e.claimInput("task-1"))
	if err != nil {
		t.Fatal(err)
	}
	*e.now = e.now.Add(ProofLifetime - time.Second)
	if _, set, err := e.s.ReconcileStep(ctx, e.reader, a.ID); err != nil || set.Settled {
		t.Fatalf("a live proof: %+v %v", set, err)
	}
	*e.now = e.now.Add(time.Second + NoneFinalAfter/2)
	if _, set, err := e.s.ReconcileStep(ctx, e.reader, a.ID); err != nil || set.Settled {
		t.Fatalf("within the grace: %+v %v", set, err)
	}
	*e.now = e.now.Add(NoneFinalAfter)
	a2, set, err := e.s.ReconcileStep(ctx, e.reader, a.ID)
	if err != nil || !set.Settled || set.Outcome != "not_committed" || a2.State != AttemptClosed {
		t.Fatalf("after the grace: %+v %+v %v", a2, set, err)
	}
}

// While aimem is unavailable or busy, however long, nothing settles (that
// nothing closes is the loop's, which closes only on a hold it could read).
func TestReconcileNothingWhileAimemIsUnavailable(t *testing.T) {
	ctx := context.Background()
	e := newClaimStopEnv(t)
	a, _, err := e.s.BeginClaimWithToken(ctx, "claim-1", e.indepTk, e.claimInput("task-1"))
	if err != nil {
		t.Fatal(err)
	}
	e.reader.err = errors.New("read scope: request_in_progress")
	for i := 0; i < 20; i++ {
		*e.now = e.now.Add(10 * time.Minute)
		if _, set, err := e.s.ReconcileStep(ctx, e.reader, a.ID); set.Settled || err == nil || !errors.Is(err, ErrOutcomeUnknown) {
			t.Fatalf("round %d: %+v %v", i, set, err)
		}
	}
	if cur, _ := e.s.GetAttempt(ctx, a.ID); cur.State != AttemptClaiming {
		t.Fatalf("after 200 minutes unavailable: %+v", cur)
	}
}

// A crashed member's finalize is settled only on the committed receipt for
// that exact step. A task made DONE by another route (an admin recovery),
// with no receipt for this step, is never settled as finalized.
func TestReconcileFinalizeOnlyOnItsReceipt(t *testing.T) {
	ctx := context.Background()
	finalizing := func(t *testing.T) (claimStopEnv, Attempt, Step) {
		e := newClaimStopEnv(t)
		a, seq := e.submitted(t, "task-1")
		if _, err := e.s.ReviewWithToken(ctx, "review-1", e.leadTok, a.ID, seq, ReviewAccept); err != nil {
			t.Fatal(err)
		}
		if _, err := e.s.ConfirmDeliveryWithToken(ctx, "confirm-1", e.leadTok, a.ID, seq, deliveryEvidence); err != nil {
			t.Fatal(err)
		}
		a, st, err := e.s.BeginFinalizeWithToken(ctx, "fin-1", e.indepTk, a.ID, seq)
		if err != nil {
			t.Fatal(err)
		}
		return e, a, st
	}

	t.Run("its receipt", func(t *testing.T) {
		e, a, st := finalizing(t)
		e.reader.commit(st.CoordinationProof, receiptFor(a, st, "res-task-1", "2", 9))
		a2, set, err := e.s.ReconcileStep(ctx, e.reader, a.ID)
		if err != nil || !set.Settled || set.Outcome != "committed" || a2.CloseReason != "finalized" {
			t.Fatalf("a committed finalize: %+v %+v %v", a2, set, err)
		}
		if kind, in := lastAudit(t, e.s, opSettle); kind != "reconciler" || in["receipt_id"] == "" || in["receipt_id"] == nil {
			t.Fatalf("the finalize's audit: %s %v", kind, in)
		}
	})

	t.Run("DONE by another route", func(t *testing.T) {
		e, a, _ := finalizing(t)
		// aimem closed the reservation by an admin recovery and the task is
		// DONE there, but nothing committed under this step's proof.
		e.reader.setHold(ScopeHold{State: ScopeClosed, ReservationID: a.ReservationID, ClosingFence: "9",
			ClosedBy: "recovery_release", ClosedAt: "2026-09-28T04:12:00Z", TaskRevision: 12})
		for _, wait := range []time.Duration{0, ProofLifetime, NoneFinalAfter + time.Second} {
			*e.now = e.now.Add(wait)
			cur, _, err := e.s.ReconcileStep(ctx, e.reader, a.ID)
			if err != nil {
				t.Fatal(err)
			}
			if cur.CloseReason == "finalized" || cur.Phase == PhaseFinalized || cur.FinalizedResult != 0 {
				t.Fatalf("finalized without its receipt: %+v", cur)
			}
		}
	})
}

// The recovered closure follows the scope's closure_use rule exactly: only
// "closed" for this exact reservation with a closing fence past the
// attempt's closes it. It is audited as the reconciler's, with the evidence.
func TestCloseRecoveredRule(t *testing.T) {
	ctx := context.Background()
	closed := func(res, closing string) ScopeHold {
		return ScopeHold{State: ScopeClosed, ReservationID: res, ClosingFence: closing, ClosedBy: "recovery_release",
			ClosedAt: "2026-09-28T04:12:00Z", TaskRevision: 6}
	}
	for name, c := range map[string]struct {
		hold  func(a Attempt) ScopeHold
		close bool
	}{
		"own reservation closed": {func(a Attempt) ScopeHold { return closed(a.ReservationID, "2") }, true},
		"another reservation":    {func(a Attempt) ScopeHold { return closed("res-other", "9") }, false},
		"fence not advanced":     {func(a Attempt) ScopeHold { return closed(a.ReservationID, a.Fence) }, false},
		"none is not closure":    {func(Attempt) ScopeHold { return ScopeHold{State: ScopeNone} }, false},
		"held is not closure": {func(a Attempt) ScopeHold {
			return ScopeHold{State: ScopeHeld, ReservationID: a.ReservationID, Fence: "2"}
		}, false},
		"held carrying closure fields is not closure": {func(a Attempt) ScopeHold {
			h := closed(a.ReservationID, "2")
			h.State = ScopeHeld
			return h
		}, false},
		"an unknown closure kind":   {func(a Attempt) ScopeHold { h := closed(a.ReservationID, "2"); h.ClosedBy = "magic"; return h }, false},
		"a malformed closing fence": {func(a Attempt) ScopeHold { return closed(a.ReservationID, "x") }, false},
	} {
		t.Run(name, func(t *testing.T) {
			e := newClaimStopEnv(t)
			a, _ := e.runningClaim(t, "task-1")
			h := c.hold(a)
			got, ok, err := e.s.CloseRecovered(ctx, a.ID, h)
			if err != nil || ok != c.close {
				t.Fatalf("closed %v, %v", ok, err)
			}
			if !c.close {
				if got.State != AttemptRunning || got.ReservationID != a.ReservationID {
					t.Fatalf("an attempt not closed changed: %+v", got)
				}
				return
			}
			if got.State != AttemptClosed || got.CloseReason != "recovered" || got.RecoveredBy != h.ClosedBy ||
				got.RecoveredFence != h.ClosingFence || got.RecoveredAt != h.ClosedAt || got.ReservationID != "" {
				t.Fatalf("the recovered attempt: %+v", got)
			}
			kind, in := lastAudit(t, e.s, opCloseRecovered)
			if kind != "reconciler" || in["reservation_id"] != a.ReservationID || in["closing_fence"] != h.ClosingFence ||
				in["closed_by"] != h.ClosedBy {
				t.Fatalf("the closure's audit: %s %v", kind, in)
			}
			// The member's capacity is free again: it may claim.
			if _, _, err := e.s.BeginClaimWithToken(ctx, "claim-2", e.indepTk, e.claimInput("task-2")); err != nil {
				t.Fatalf("a claim after the recovered closure: %v", err)
			}
		})
	}
}

// A pending step, and an attempt that changed after the first check, are
// never closed as recovered: the rule is checked again in the closing
// transaction.
func TestCloseRecoveredRechecksInItsTransaction(t *testing.T) {
	ctx := context.Background()
	e := newClaimStopEnv(t)
	a, _ := e.runningClaim(t, "task-1")
	hold := ScopeHold{State: ScopeClosed, ReservationID: a.ReservationID, ClosingFence: "2", ClosedBy: "holder_release",
		ClosedAt: "2026-09-28T04:10:00Z", TaskRevision: 5}
	if _, _, err := e.s.BeginWorkWithToken(ctx, "block-1", e.indepTk, a.ID, IntentBlock, "waiting"); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := e.s.CloseRecovered(ctx, a.ID, hold); err != nil || ok {
		t.Fatalf("with a step pending: %v %v", ok, err)
	}

	e2 := newClaimStopEnv(t)
	b, _ := e2.runningClaim(t, "task-1")
	e2.s.beforeRecoveredClose = func() {
		// The attempt moves on between the check and the closing
		// transaction: its fence reaches the closing fence.
		if _, err := e2.s.db.Exec(`UPDATE attempts SET fence = '2' WHERE id = ?`, b.ID); err != nil {
			t.Fatal(err)
		}
	}
	if cur, ok, err := e2.s.CloseRecovered(ctx, b.ID, hold); err != nil || ok || cur.State == AttemptClosed {
		t.Fatalf("an attempt changed in between: %+v %v %v", cur, ok, err)
	}
}

// Candidates list the pending steps first, then the open attempts holding a
// reservation, each oldest first; closed attempts are never listed.
func TestReconcileCandidates(t *testing.T) {
	ctx := context.Background()
	e := newClaimStopEnv(t)
	running, _ := e.runningClaim(t, "task-1")
	other, _ := member(t, e.s, e.teamID, "indep-2", RoleIndependent)
	_, osec := e.p.enter("e-indep-2", other, e.teamID)
	*e.now = e.now.Add(time.Second)
	pending, _, err := e.s.BeginClaimWithToken(ctx, "claim-2", osec.Token.Reveal(), e.claimInput("task-2"))
	if err != nil {
		t.Fatal(err)
	}
	got, err := e.s.ReconcileCandidates(ctx)
	if err != nil || len(got) != 2 || got[0].AttemptID != pending.ID || !got[0].Pending ||
		got[1].AttemptID != running.ID || got[1].Pending || got[1].Task != running.Task {
		t.Fatalf("candidates: %+v %v", got, err)
	}
	if _, ok, err := e.s.CloseRecovered(ctx, running.ID, ScopeHold{State: ScopeClosed, ReservationID: running.ReservationID,
		ClosingFence: "2", ClosedBy: "holder_release", ClosedAt: "2026-09-28T04:10:00Z", TaskRevision: 5}); !ok || err != nil {
		t.Fatal(ok, err)
	}
	if got, _ := e.s.ReconcileCandidates(ctx); len(got) != 1 || strings.Compare(got[0].AttemptID, pending.ID) != 0 {
		t.Fatalf("after the closure: %+v", got)
	}
}
