package store

import (
	"context"
	"errors"
	"testing"
	"time"
)

// claimStopEnv is a team on hub-test with a coordinator, an independent
// member and a worker, all in session with tokens, a store clock and a fake
// read scope.
type claimStopEnv struct {
	s                *Store
	now              *time.Time
	reader           *fakeReader
	p                *prover
	teamID           string
	lead, indep      string
	indepAgent       Agent
	leadTok, indepTk string
	workerTok        string
	expires          time.Time
}

func newClaimStopEnv(t *testing.T) claimStopEnv {
	t.Helper()
	s, _ := openTemp(t)
	now := clock(s)
	tm := mustTeam(t, s, "t1", "crew", ProjectRef{HubID: "hub-test", ProjectID: "project-t"})
	p := newProver(t, s)
	lead, _ := member(t, s, tm.ID, "lead", RoleCoordinator)
	indep, _ := member(t, s, tm.ID, "indep", RoleIndependent)
	worker, _ := member(t, s, tm.ID, "worker", RoleWorker)
	le, ls := p.enter("e-lead", lead, tm.ID)
	ie, is := p.enter("e-indep", indep, tm.ID)
	we, ws := p.enter("e-worker", worker, tm.ID)
	expires := le.Token.ExpiresAt
	for _, x := range []time.Time{ie.Token.ExpiresAt, we.Token.ExpiresAt} {
		if x.After(expires) {
			expires = x
		}
	}
	return claimStopEnv{s: s, now: now, reader: newFakeReader(), p: p, teamID: tm.ID, lead: lead.ID, indep: indep.ID,
		indepAgent: indep, leadTok: ls.Token.Reveal(), indepTk: is.Token.Reveal(), workerTok: ws.Token.Reveal(),
		expires: expires}
}

func (e claimStopEnv) claimInput(taskID string) ClaimInput {
	return ClaimInput{Task: TaskRef{HubID: "hub-test", ProjectID: "project-t", TaskID: taskID}, ExpectedRevision: 3,
		BaseCommit: "base-1", Branch: "work/" + taskID, Process: testPin, InstructionDigest: testPin.InstructionDigest}
}

func (e claimStopEnv) settle(t *testing.T, token string, a Attempt, st Step, hint StepHint) (Attempt, Settlement, error) {
	t.Helper()
	return e.s.SettleWithToken(context.Background(), token, e.reader, a.ID, st.RequestKey, report(hint))
}

// runningClaim claims taskID and settles the claim from the read scope.
func (e claimStopEnv) runningClaim(t *testing.T, taskID string) (Attempt, Step) {
	t.Helper()
	a, st, err := e.s.BeginClaimWithToken(context.Background(), "claim-"+taskID, e.indepTk, e.claimInput(taskID))
	if err != nil {
		t.Fatal(err)
	}
	e.reader.commit(st.CoordinationProof, receiptFor(a, st, "res-"+taskID, "1", 4))
	a, set, err := e.settle(t, e.indepTk, a, st, HintCommitted)
	if err != nil || !set.Settled || a.State != AttemptRunning {
		t.Fatalf("settle the claim: %+v %+v %v", a, set, err)
	}
	return a, st
}

// The independent claim and the stop family, as member-driven steps: the
// claim's pin turns verified only with the committed claim receipt, and the
// holder's release of its stopped attempt carries the stopped fact and the
// values it sends aimem.
func TestClaimAndStopSteps(t *testing.T) {
	ctx := context.Background()
	e := newClaimStopEnv(t)
	a, st, err := e.s.BeginClaimWithToken(ctx, "claim-1", e.indepTk, e.claimInput("task-1"))
	if err != nil || st.Operation != ReservationClaim || st.Holder == nil || st.Holder.WorkRef != a.attemptRef() ||
		st.CoordinationProof == "" || st.TargetState != "" || a.State != AttemptClaiming || a.ProcessVerifiedReceipt != "" {
		t.Fatalf("begin claim: %+v %+v %v", a, st, err)
	}
	f, err := e.s.CoordinationFact(ctx, st.CoordinationProof, "hub-test")
	if err != nil || !f.Active || f.Kind != FactIndependentClaim || f.Process == nil || f.Process.Identity != testPin.Identity {
		t.Fatalf("the claim's fact: %+v %v", f, err)
	}
	// A retried claim gets a replacement proof for the same fact.
	a2, st2, err := e.s.BeginClaimWithToken(ctx, "claim-1", e.indepTk, e.claimInput("task-1"))
	if err != nil || a2.ID != a.ID || st2.CoordinationProof == st.CoordinationProof {
		t.Fatalf("a retried claim: %+v %+v %v", a2, st2, err)
	}
	if f, err := e.s.CoordinationFact(ctx, st2.CoordinationProof, "hub-test"); err != nil || f.Kind != FactIndependentClaim || f.Process == nil {
		t.Fatalf("the replacement's fact: %+v %v", f, err)
	}
	st = st2
	if _, _, err := e.s.BeginClaimWithToken(ctx, "claim-2", e.indepTk, e.claimInput("task-1")); !errors.Is(err, ErrTaskBusy) {
		t.Fatalf("a second claim of the task: %v", err)
	}
	if _, _, err := e.s.BeginClaimWithToken(ctx, "claim-l", e.leadTok, e.claimInput("task-9")); !errors.Is(err, ErrForbidden) {
		t.Fatalf("a claim by the coordinator: %v", err)
	}
	rc := receiptFor(a, st, "res-1", "1", 4)
	e.reader.commit(st.CoordinationProof, rc)
	if a, _, err = e.settle(t, e.indepTk, a, st, HintCommitted); err != nil || a.State != AttemptRunning ||
		a.ProcessVerifiedReceipt != rc.ID {
		t.Fatalf("settle the claim: %+v %v", a, err)
	}

	if _, err := e.s.RequestStopWithToken(ctx, "stop-self", e.indepTk, a.ID, "done"); !errors.Is(err, ErrForbidden) {
		t.Fatalf("a stop requested by the holder: %v", err)
	}
	if _, _, err := e.s.BeginStopReleaseWithToken(ctx, "release-0", e.indepTk, a.ID, ReleaseReady, ""); !errors.Is(err, ErrAttemptState) {
		t.Fatalf("a release before the stop: %v", err)
	}
	if a, err = e.s.RequestStopWithToken(ctx, "stop-1", e.leadTok, a.ID, "priorities changed"); err != nil || a.Stop != StopRequested {
		t.Fatalf("request the stop: %+v %v", a, err)
	}
	if _, err := e.s.ConfirmStopWithToken(ctx, "confirm-l", e.leadTok, a.ID); !errors.Is(err, ErrForbidden) && !errors.Is(err, ErrNotFound) {
		t.Fatalf("a stop confirmed by the coordinator: %v", err)
	}
	if a, err = e.s.ConfirmStopWithToken(ctx, "confirm-1", e.indepTk, a.ID); err != nil || a.Stop != StopConfirmed {
		t.Fatalf("confirm the stop: %+v %v", a, err)
	}
	a, rel, err := e.s.BeginStopReleaseWithToken(ctx, "release-1", e.indepTk, a.ID, ReleaseBlocked, "waiting on design")
	if err != nil || rel.Operation != ReservationRelease || rel.ReservationID != "res-1" || rel.Fence != "1" || rel.Holder != nil ||
		rel.TargetState != "BLOCKED" || rel.Reason != "stopped" || rel.Blocker != "waiting on design" {
		t.Fatalf("begin the stop release: %+v %v", rel, err)
	}
	_, rel2, err := e.s.BeginStopReleaseWithToken(ctx, "release-1", e.indepTk, a.ID, ReleaseBlocked, "waiting on design")
	if err != nil || rel2.CoordinationProof == rel.CoordinationProof || rel2.TargetState != "BLOCKED" {
		t.Fatalf("a retried release: %+v %v", rel2, err)
	}
	rel = rel2
	if f, err := e.s.CoordinationFact(ctx, rel.CoordinationProof, "hub-test"); err != nil || f.Kind != FactStopped || f.Process != nil {
		t.Fatalf("the release's fact: %+v %v", f, err)
	}
	e.reader.commit(rel.CoordinationProof, receiptFor(a, rel, "res-1", "2", 5))
	if a, _, err = e.settle(t, e.leadTok, a, rel, HintUnknown); err != nil || a.State != AttemptClosed || a.CloseReason != "stopped" {
		t.Fatalf("settle the stop release: %+v %v", a, err)
	}
}

// A claim whose pin aimem refused does not verify it: the refusal voids the
// proof, and the claim closes as not committed after the grace period.
func TestRefusedClaimStaysUnverified(t *testing.T) {
	e := newClaimStopEnv(t)
	a, st, err := e.s.BeginClaimWithToken(context.Background(), "claim-1", e.indepTk, e.claimInput("task-1"))
	if err != nil {
		t.Fatal(err)
	}
	if _, set, err := e.s.SettleWithToken(context.Background(), e.indepTk, e.reader, a.ID, st.RequestKey,
		StepReport{Outcome: HintRefused, Code: "process_mismatch"}); err != nil || set.Settled {
		t.Fatalf("a refused claim: %+v %v", set, err)
	}
	*e.now = e.now.Add(NoneFinalAfter)
	a, set, err := e.settle(t, e.indepTk, a, st, HintRefused)
	if err != nil || !set.Settled || a.State != AttemptClosed || a.ProcessVerifiedReceipt != "" {
		t.Fatalf("the refused claim settled: %+v %+v %v", a, set, err)
	}
}

// Every write of the claim and stop steps checks the token inside its own
// transaction: a token that dies after the operation found it, and before
// the write, begins, replaces, records, voids and settles nothing.
func TestClaimAndStopWritesRecheckTheToken(t *testing.T) {
	ctx := context.Background()
	e := newClaimStopEnv(t)
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
	var n int
	count := func() int {
		t.Helper()
		if err := e.s.db.QueryRow(`SELECT COUNT(*) FROM attempts`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}

	e.s.afterTokenLookup = die
	_, _, err := e.s.BeginClaimWithToken(ctx, "claim-1", e.indepTk, e.claimInput("task-1"))
	dead("a claim", err)
	if count() != 0 {
		t.Fatal("a dead token claimed a task")
	}
	a, st, err := e.s.BeginClaimWithToken(ctx, "claim-1", e.indepTk, e.claimInput("task-1"))
	if err != nil {
		t.Fatal(err)
	}
	e.s.beforeProofReplace = die
	_, _, err = e.s.BeginClaimWithToken(ctx, "claim-1", e.indepTk, e.claimInput("task-1"))
	dead("a claim's proof replacement", err)
	if openProofs(t, e.s, a.ID) != 1 || !activeOn(t, e.s, st.CoordinationProof, "hub-test") {
		t.Fatal("a dead token replaced the claim's proof")
	}
	e.reader.answered = die
	_, _, err = e.settle(t, e.indepTk, a, st, HintUnknown)
	dead("a claim's void", err)
	if openProofs(t, e.s, a.ID) != 1 {
		t.Fatal("a dead token voided the claim")
	}
	e.reader.commit(st.CoordinationProof, receiptFor(a, st, "res-1", "1", 4))
	e.reader.answered = die
	_, _, err = e.settle(t, e.indepTk, a, st, HintCommitted)
	dead("a claim's settle", err)
	if got, _ := e.s.GetAttempt(ctx, a.ID); got.State != AttemptClaiming || got.ProcessVerifiedReceipt != "" {
		t.Fatalf("a dead token settled the claim: %+v", got)
	}
	if a, _, err = e.settle(t, e.indepTk, a, st, HintCommitted); err != nil || a.State != AttemptRunning {
		t.Fatalf("settle the claim: %+v %v", a, err)
	}

	e.s.afterTokenLookup = die
	_, err = e.s.RequestStopWithToken(ctx, "stop-1", e.leadTok, a.ID, "priorities changed")
	dead("a stop request", err)
	if got, _ := e.s.GetAttempt(ctx, a.ID); got.Stop != StopNone {
		t.Fatal("a dead token requested a stop")
	}
	if _, err := e.s.RequestStopWithToken(ctx, "stop-1", e.leadTok, a.ID, "priorities changed"); err != nil {
		t.Fatal(err)
	}
	e.s.afterTokenLookup = die
	_, err = e.s.ConfirmStopWithToken(ctx, "confirm-1", e.indepTk, a.ID)
	dead("a stop confirmation", err)
	if got, _ := e.s.GetAttempt(ctx, a.ID); got.Stop != StopRequested {
		t.Fatal("a dead token confirmed the stop")
	}
	if _, err := e.s.ConfirmStopWithToken(ctx, "confirm-1", e.indepTk, a.ID); err != nil {
		t.Fatal(err)
	}
	e.s.afterTokenLookup = die
	_, _, err = e.s.BeginStopReleaseWithToken(ctx, "release-1", e.indepTk, a.ID, ReleaseReady, "")
	dead("a stop release", err)
	if got, _ := e.s.GetAttempt(ctx, a.ID); got.PendingKey != "" {
		t.Fatal("a dead token began the release")
	}
	a, rel, err := e.s.BeginStopReleaseWithToken(ctx, "release-1", e.indepTk, a.ID, ReleaseReady, "")
	if err != nil {
		t.Fatal(err)
	}
	e.s.beforeProofReplace = die
	_, _, err = e.s.BeginStopReleaseWithToken(ctx, "release-1", e.indepTk, a.ID, ReleaseReady, "")
	dead("a release's proof replacement", err)
	if !activeOn(t, e.s, rel.CoordinationProof, "hub-test") {
		t.Fatal("a dead token replaced the release's proof")
	}
	e.reader.answered = die
	_, _, err = e.settle(t, e.indepTk, a, rel, HintRefused)
	dead("a release's void", err)
	if !activeOn(t, e.s, rel.CoordinationProof, "hub-test") {
		t.Fatal("a dead token voided the release")
	}
}
