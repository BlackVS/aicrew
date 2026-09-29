package store

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"testing"
)

// updatePort commits every update against the hold it names: the one-shot
// work update is the only way to submit a result until b1b-3 moves it to
// two phases.
type updatePort struct{ attempt string }

func (p *updatePort) Mutate(_ context.Context, op ReservationOp, req ReservationRequest) (ReservationResult, error) {

	return ReservationResult{
		Receipt: ReservationReceipt{ID: "rcpt-update-" + req.RequestKey, State: ReceiptCommitted, Operation: string(op),
			RequestKey: req.RequestKey, VerifiedMode: "team"},
		TaskRevision: req.ExpectedRevision + 1,
		Reservation: ReservationState{ID: req.ReservationID, Fence: req.Fence, Active: true, HolderMode: "external",
			OwnWorkRef: "aicrew-attempt-" + p.attempt},
	}, nil
}

func (p *updatePort) Receipt(context.Context, TaskRef, ReservationOp, string) (ReceiptLookup, error) {
	return ReceiptLookup{State: ReceiptNotCommitted}, nil
}

func (p *updatePort) Status(context.Context, TaskRef) (HoldStatus, error) {
	return HoldStatus{State: "none"}, nil
}

var deliveryEvidence = []Evidence{
	{Kind: "reviewed_head", Ref: "https://forge.example/pull/7#review-1"},
	{Kind: "human_merge", Ref: "https://forge.example/commit/merge-7"},
	{Kind: "post_merge_ci", Ref: "https://forge.example/actions/runs/7"},
}

func refsOf(evidence []Evidence) []string {
	var out []string
	for _, e := range evidence {
		out = append(out, e.Ref)
	}
	return out
}

// work runs one of the holder's work updates through the one-shot path.
func (e claimStopEnv) work(t *testing.T, a Attempt, key string, intent WorkIntent, detail string) Attempt {
	t.Helper()
	b, err := e.s.AuthenticateSessionToken(context.Background(), e.indepTk)
	if err != nil {
		t.Fatal(err)
	}
	port := &updatePort{attempt: a.ID}
	got, err := e.s.UpdateWork(context.Background(), Caller{kind: callerAgent, id: e.indep}, port, key, a.ID,
		WorkUpdate{SessionID: b.SessionID, Generation: b.Generation, Intent: intent, Detail: detail})
	if err != nil {
		t.Fatalf("%s: %v", intent, err)
	}
	return got
}

// A running, submitted claim with its latest result's sequence.
func (e claimStopEnv) submitted(t *testing.T, taskID string) (Attempt, int64) {
	t.Helper()
	a, _ := e.runningClaim(t, taskID)
	a = e.work(t, a, "submit-"+taskID, IntentSubmit, "https://forge.example/pull/7")
	res, err := e.s.AttemptResults(context.Background(), a.ID)
	if err != nil || len(res) == 0 {
		t.Fatalf("results: %v %v", res, err)
	}
	return a, res[len(res)-1].Seq
}

// Finalizing as DONE needs a team member's confirmed delivery of the accepted
// result; its terminal evidence is the confirmed record's, and rework voids
// the confirmation with the acceptance.
func TestConfirmedDeliveryGatesFinalize(t *testing.T) {
	ctx := context.Background()
	e := newClaimStopEnv(t)
	a, seq := e.submitted(t, "task-1")
	if _, _, err := e.s.BeginFinalizeWithToken(ctx, "fin-0", e.indepTk, a.ID, seq); !errors.Is(err, ErrAttemptState) {
		t.Fatalf("a finalize before review: %v", err)
	}
	if _, err := e.s.ReviewWithToken(ctx, "review-self", e.indepTk, a.ID, seq, ReviewAccept); !errors.Is(err, ErrForbidden) {
		t.Fatalf("a review by the holder: %v", err)
	}
	a, err := e.s.ReviewWithToken(ctx, "review-1", e.leadTok, a.ID, seq, ReviewAccept)
	if err != nil || a.Phase != PhaseAccepted || a.AcceptedResult != seq {
		t.Fatalf("accept: %+v %v", a, err)
	}
	if _, _, err := e.s.BeginFinalizeWithToken(ctx, "fin-1", e.indepTk, a.ID, seq); !errors.Is(err, ErrDeliveryUnconfirmed) {
		t.Fatalf("a finalize without a confirmed delivery: %v", err)
	}
	if _, err := e.s.ConfirmDeliveryWithToken(ctx, "confirm-self", e.indepTk, a.ID, seq, deliveryEvidence); !errors.Is(err, ErrForbidden) {
		t.Fatalf("a confirmation by the worker: %v", err)
	}
	if _, err := e.s.ConfirmDeliveryWithToken(ctx, "confirm-thin", e.leadTok, a.ID, seq, deliveryEvidence[:1]); !errors.Is(err, ErrInvalid) {
		t.Fatalf("a confirmation without every kind: %v", err)
	}
	if _, err := e.s.ConfirmDeliveryWithToken(ctx, "confirm-other", e.leadTok, a.ID, seq+1, deliveryEvidence); !errors.Is(err, ErrAttemptState) {
		t.Fatalf("a confirmation of another result: %v", err)
	}
	b, _ := e.s.AuthenticateSessionToken(ctx, e.leadTok)
	a, err = e.s.ConfirmDeliveryWithToken(ctx, "confirm-1", e.leadTok, a.ID, seq, deliveryEvidence)
	if err != nil || a.DeliveryResult != seq || a.DeliveryByAgent != e.lead || a.DeliveryBySession != b.SessionID ||
		a.DeliveryByGeneration != b.Generation || a.DeliveryAt.IsZero() {
		t.Fatalf("confirm: %+v %v", a, err)
	}

	// Rework voids the confirmation with the acceptance: the next result
	// needs its own review and its own confirmed delivery.
	if a, err = e.s.ReviewWithToken(ctx, "rework-1", e.leadTok, a.ID, seq, ReviewRework); err != nil ||
		a.DeliveryResult != 0 || a.DeliveryEvidence != "" || a.AcceptedResult != 0 {
		t.Fatalf("rework: %+v %v", a, err)
	}
	a = e.work(t, a, "resume-1", IntentResume, "")
	a = e.work(t, a, "submit-2", IntentSubmit, "https://forge.example/pull/8")
	seq++
	if a, err = e.s.ReviewWithToken(ctx, "review-2", e.leadTok, a.ID, seq, ReviewAccept); err != nil {
		t.Fatal(err)
	}
	if _, _, err := e.s.BeginFinalizeWithToken(ctx, "fin-2", e.indepTk, a.ID, seq); !errors.Is(err, ErrDeliveryUnconfirmed) {
		t.Fatalf("a finalize after rework, with the old confirmation: %v", err)
	}
	if _, err = e.s.ConfirmDeliveryWithToken(ctx, "confirm-2", e.leadTok, a.ID, seq, deliveryEvidence); err != nil {
		t.Fatal(err)
	}
	a, st, err := e.s.BeginFinalizeWithToken(ctx, "fin-3", e.indepTk, a.ID, seq)
	if err != nil || st.Operation != ReservationFinalize || st.TargetState != "DONE" || st.Reason == "" ||
		!reflect.DeepEqual(st.TerminalEvidence, refsOf(deliveryEvidence)) {
		t.Fatalf("begin finalize: %+v %v", st, err)
	}
	var recorded []Evidence
	if err := json.Unmarshal([]byte(a.PendingEvidence), &recorded); err != nil || !reflect.DeepEqual(recorded, deliveryEvidence) {
		t.Fatalf("the finalize's evidence %q: %v", a.PendingEvidence, err)
	}
	if f, err := e.s.CoordinationFact(ctx, st.CoordinationProof, "hub-test"); err != nil || f.Kind != FactAcceptedForFinalization {
		t.Fatalf("the finalize's fact: %+v %v", f, err)
	}
	e.reader.commit(st.CoordinationProof, receiptFor(a, st, "", "2", 9))
	if a, _, err = e.settle(t, e.leadTok, a, st, HintCommitted); err != nil || a.State != AttemptClosed || a.CloseReason != "finalized" {
		t.Fatalf("settle the finalize: %+v %v", a, err)
	}
}

// A stop voids a confirmed delivery with the acceptance, unless a finalize of
// it is in flight.
func TestStopVoidsConfirmedDelivery(t *testing.T) {
	ctx := context.Background()
	e := newClaimStopEnv(t)
	a, seq := e.submitted(t, "task-1")
	if _, err := e.s.ReviewWithToken(ctx, "review-1", e.leadTok, a.ID, seq, ReviewAccept); err != nil {
		t.Fatal(err)
	}
	if _, err := e.s.ConfirmDeliveryWithToken(ctx, "confirm-1", e.leadTok, a.ID, seq, deliveryEvidence); err != nil {
		t.Fatal(err)
	}
	a, err := e.s.RequestStopWithToken(ctx, "stop-1", e.leadTok, a.ID, "priorities changed")
	if err != nil || a.DeliveryResult != 0 || a.AcceptedResult != 0 || a.Phase != PhaseSubmitted {
		t.Fatalf("stop: %+v %v", a, err)
	}
}

// Every write of review, confirm-delivery and finalize checks the token inside
// its own transaction: a token that dies after the operation found it, and
// before the write, records, begins, replaces, voids and settles nothing.
func TestDeliveryWritesRecheckTheToken(t *testing.T) {
	ctx := context.Background()
	e := newClaimStopEnv(t)
	a, seq := e.submitted(t, "task-1")
	alive := *e.now
	die := func() { *e.now = e.expires }
	dead := func(what string, err error) {
		t.Helper()
		*e.now = alive
		e.s.afterTokenLookup, e.s.beforeProofReplace, e.reader.answered = nil, nil, nil
		if !errors.Is(err, ErrTokenInvalid) {
			t.Fatalf("%s with a dead token: %v", what, err)
		}
	}
	current := func() Attempt {
		t.Helper()
		got, err := e.s.GetAttempt(ctx, a.ID)
		if err != nil {
			t.Fatal(err)
		}
		return got
	}

	e.s.afterTokenLookup = die
	_, err := e.s.ReviewWithToken(ctx, "review-1", e.leadTok, a.ID, seq, ReviewAccept)
	dead("a review", err)
	if current().Phase != PhaseSubmitted {
		t.Fatal("a dead token reviewed the result")
	}
	if _, err := e.s.ReviewWithToken(ctx, "review-1", e.leadTok, a.ID, seq, ReviewAccept); err != nil {
		t.Fatal(err)
	}
	e.s.afterTokenLookup = die
	_, err = e.s.ConfirmDeliveryWithToken(ctx, "confirm-1", e.leadTok, a.ID, seq, deliveryEvidence)
	dead("a delivery confirmation", err)
	if current().DeliveryResult != 0 {
		t.Fatal("a dead token confirmed the delivery")
	}
	if _, err := e.s.ConfirmDeliveryWithToken(ctx, "confirm-1", e.leadTok, a.ID, seq, deliveryEvidence); err != nil {
		t.Fatal(err)
	}
	e.s.afterTokenLookup = die
	_, _, err = e.s.BeginFinalizeWithToken(ctx, "fin-1", e.indepTk, a.ID, seq)
	dead("a finalize", err)
	if current().PendingKey != "" {
		t.Fatal("a dead token began the finalize")
	}
	a, st, err := e.s.BeginFinalizeWithToken(ctx, "fin-1", e.indepTk, a.ID, seq)
	if err != nil {
		t.Fatal(err)
	}
	e.s.beforeProofReplace = die
	_, _, err = e.s.BeginFinalizeWithToken(ctx, "fin-1", e.indepTk, a.ID, seq)
	dead("a finalize's proof replacement", err)
	if !activeOn(t, e.s, st.CoordinationProof, "hub-test") {
		t.Fatal("a dead token replaced the finalize's proof")
	}
	e.reader.answered = die
	_, _, err = e.settle(t, e.indepTk, a, st, HintUnknown)
	dead("a finalize's void", err)
	if !activeOn(t, e.s, st.CoordinationProof, "hub-test") {
		t.Fatal("a dead token voided the finalize")
	}
	e.reader.commit(st.CoordinationProof, receiptFor(a, st, "", "2", 9))
	e.reader.answered = die
	_, _, err = e.settle(t, e.indepTk, a, st, HintCommitted)
	dead("a finalize's settle", err)
	if current().State != AttemptRunning {
		t.Fatal("a dead token settled the finalize")
	}
}

// Only the team's current coordinator who is not the attempt's worker
// confirms a delivery: not another member, and not the worker even once it
// has become the coordinator.
func TestOnlyAnotherCoordinatorConfirmsDelivery(t *testing.T) {
	ctx := context.Background()
	e := newClaimStopEnv(t)
	a, seq := e.submitted(t, "task-1")
	if _, err := e.s.ReviewWithToken(ctx, "review-1", e.leadTok, a.ID, seq, ReviewAccept); err != nil {
		t.Fatal(err)
	}
	if _, err := e.s.ConfirmDeliveryWithToken(ctx, "confirm-w", e.workerTok, a.ID, seq, deliveryEvidence); !errors.Is(err, ErrForbidden) {
		t.Fatalf("a confirmation by another member: %v", err)
	}
	// The coordinator leaves; the operator makes the attempt's worker the
	// coordinator, and it enters as such.
	lb, err := e.s.AuthenticateSessionToken(ctx, e.leadTok)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.s.StopSession(ctx, operator(t), "end-lead", lb.SessionID); err != nil {
		t.Fatal(err)
	}
	ib, err := e.s.AuthenticateSessionToken(ctx, e.indepTk)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.s.StopSession(ctx, operator(t), "end-indep", ib.SessionID); err != nil {
		t.Fatal(err)
	}
	ms, err := getMembership(ctx, e.s.rdb, e.teamID, e.indep)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.s.SetMemberRole(ctx, operator(t), "promote", e.teamID, e.indep, ms.Revision, RoleCoordinator); err != nil {
		t.Fatal(err)
	}
	_, sec := e.p.enter("e-promoted", e.indepAgent, e.teamID)
	if _, err := e.s.ConfirmDeliveryWithToken(ctx, "confirm-self", sec.Token.Reveal(), a.ID, seq, deliveryEvidence); !errors.Is(err, ErrForbidden) {
		t.Fatalf("a confirmation by the worker as coordinator: %v", err)
	}
	if got, _ := e.s.GetAttempt(ctx, a.ID); got.DeliveryResult != 0 {
		t.Fatalf("a refused confirmation was recorded: %+v", got)
	}
}

// A confirmation binds to its result: one that does not name the accepted
// result confirms nothing, even if it were left behind.
func TestFinalizeNeedsTheAcceptedResultsConfirmation(t *testing.T) {
	ctx := context.Background()
	e := newClaimStopEnv(t)
	a, seq := e.submitted(t, "task-1")
	if _, err := e.s.ReviewWithToken(ctx, "review-1", e.leadTok, a.ID, seq, ReviewAccept); err != nil {
		t.Fatal(err)
	}
	if _, err := e.s.ConfirmDeliveryWithToken(ctx, "confirm-1", e.leadTok, a.ID, seq, deliveryEvidence); err != nil {
		t.Fatal(err)
	}
	if _, err := e.s.db.Exec(`UPDATE attempts SET delivery_result = delivery_result + 1 WHERE id = ?`, a.ID); err != nil {
		t.Fatal(err)
	}
	if _, _, err := e.s.BeginFinalizeWithToken(ctx, "fin-1", e.indepTk, a.ID, seq); !errors.Is(err, ErrDeliveryUnconfirmed) {
		t.Fatalf("a finalize with another result's confirmation: %v", err)
	}
}
