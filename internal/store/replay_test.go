package store

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"
)

// resume resumes token's session with a fresh proof and returns the new
// generation's token; token's generation is fenced.
func (e claimStopEnv) resume(t *testing.T, token string) string {
	t.Helper()
	b, err := e.s.AuthenticateSessionToken(context.Background(), token)
	if err != nil {
		t.Fatal(err)
	}
	ch, receipt := e.p.proof(b.AgentID, "token-"+b.AgentID)
	_, sec, err := e.s.ResumeSessionWithProof(context.Background(), e.p.v, fmt.Sprintf("resume-%d", e.p.n), ch.ID, receipt,
		b.SessionID, testService)
	if err != nil {
		t.Fatalf("resume: %v", err)
	}
	return sec.Token.Reveal()
}

// setRole changes agentID's membership role behind its session's back, as
// a concurrent change the session's token does not reflect.
func (e claimStopEnv) setRole(t *testing.T, agentID string, role Role) {
	t.Helper()
	if _, err := e.s.db.Exec(`UPDATE memberships SET role = ? WHERE team_id = ? AND agent_id = ?`,
		string(role), e.teamID, agentID); err != nil {
		t.Fatal(err)
	}
}

func (e claimStopEnv) generation(t *testing.T, token string) int64 {
	t.Helper()
	b, err := e.s.AuthenticateSessionToken(context.Background(), token)
	if err != nil {
		t.Fatal(err)
	}
	return b.Generation
}

func (e claimStopEnv) fact(t *testing.T, proof string) Fact {
	t.Helper()
	f, err := e.s.CoordinationFact(context.Background(), proof, "hub-test")
	if err != nil {
		t.Fatal(err)
	}
	return f
}

// replayed checks that a replay answered the same pending step with a
// replacement proof for the resumed generation, and that the earlier proof
// answers no fact.
func (e claimStopEnv) replayed(t *testing.T, before, after Step, token string) {
	t.Helper()
	if after.RequestKey != before.RequestKey || after.CoordinationProof == "" ||
		after.CoordinationProof == before.CoordinationProof {
		t.Fatalf("the replay: %+v, before %+v", after, before)
	}
	if f := e.fact(t, before.CoordinationProof); f.Active {
		t.Fatalf("the earlier generation's proof is still active: %+v", f)
	}
	if f := e.fact(t, after.CoordinationProof); !f.Active || f.Member.Generation != e.generation(t, token) {
		t.Fatalf("the replacement's fact: %+v", f)
	}
}

// A claim begun before its session resumed is recovered with its keys:
// the old generation's token cannot replay it, the same key with another
// request is still a conflict, and the resumed session gets a replacement
// proof, settles the claim and then replays only its outcome. Another
// member's same key is its own.
func TestClaimReplaysAcrossResume(t *testing.T) {
	ctx := context.Background()
	e := newClaimStopEnv(t)
	in := e.claimInput("task-1")
	a, st, err := e.s.BeginClaimWithToken(ctx, "claim-r", e.indepTk, in)
	if err != nil {
		t.Fatal(err)
	}
	tok := e.resume(t, e.indepTk)
	if _, _, err := e.s.BeginClaimWithToken(ctx, "claim-r", e.indepTk, in); !errors.Is(err, ErrTokenInvalid) {
		t.Fatalf("a replay with the fenced generation's token: %v", err)
	}
	for name, other := range map[string]ClaimInput{
		"branch":   func() ClaimInput { x := in; x.Branch = "work/other"; return x }(),
		"task":     e.claimInput("task-2"),
		"revision": func() ClaimInput { x := in; x.ExpectedRevision = 4; return x }(),
	} {
		if _, _, err := e.s.BeginClaimWithToken(ctx, "claim-r", tok, other); !errors.Is(err, ErrIdempotencyConflict) {
			t.Fatalf("the same key with another %s after the resume: %v", name, err)
		}
	}
	a2, st2, err := e.s.BeginClaimWithToken(ctx, "claim-r", tok, in)
	if err != nil || a2.ID != a.ID {
		t.Fatalf("the replay after the resume: %+v %v", a2, err)
	}
	e.replayed(t, st, st2, tok)
	if openProofs(t, e.s, a.ID) != 1 {
		t.Fatal("the earlier proof did not end")
	}

	e.reader.commit(st2.CoordinationProof, receiptFor(a2, st2, "res-1", "1", 4))
	a3, set, err := e.settle(t, tok, a2, st2, HintCommitted)
	if err != nil || !set.Settled || a3.State != AttemptRunning {
		t.Fatalf("settle: %+v %+v %v", a3, set, err)
	}
	a4, st4, err := e.s.BeginClaimWithToken(ctx, "claim-r", tok, in)
	if err != nil || a4.ID != a.ID || st4.CoordinationProof != "" || a4.State != AttemptRunning {
		t.Fatalf("a replay of the settled claim: %+v %+v %v", a4, st4, err)
	}

	// Another member's key is its own: the same key begins its own claim.
	other, _ := member(t, e.s, e.teamID, "indep-2", RoleIndependent)
	_, osec := e.p.enter("e-indep-2", other, e.teamID)
	b, _, err := e.s.BeginClaimWithToken(ctx, "claim-r", osec.Token.Reveal(), e.claimInput("task-3"))
	if err != nil || b.ID == a.ID || b.WorkerAgentID != other.ID {
		t.Fatalf("another member's same key: %+v %v", b, err)
	}
}

// A work update begun before a resume replays as the same step, with no
// proof, and its key with another intent is a conflict.
func TestUpdateReplaysAcrossResume(t *testing.T) {
	ctx := context.Background()
	e := newClaimStopEnv(t)
	a, _ := e.runningClaim(t, "task-1")
	_, st, err := e.s.BeginWorkWithToken(ctx, "block-1", e.indepTk, a.ID, IntentBlock, "waiting")
	if err != nil {
		t.Fatal(err)
	}
	tok := e.resume(t, e.indepTk)
	if _, _, err := e.s.BeginWorkWithToken(ctx, "block-1", tok, a.ID, IntentBlock, "waiting on design"); !errors.Is(err, ErrIdempotencyConflict) {
		t.Fatalf("the same key with another detail: %v", err)
	}
	_, st2, err := e.s.BeginWorkWithToken(ctx, "block-1", tok, a.ID, IntentBlock, "waiting")
	if err != nil || st2.RequestKey != st.RequestKey || st2.CoordinationProof != "" {
		t.Fatalf("the replay after the resume: %+v %v", st2, err)
	}
}

// Every proof-backed step replays across its session's resume only while
// the resumed session still acts it, checked in the replacement's
// transaction: a member whose role changed behind its session, and a
// coordinator whose own resume moved the team's coordinator generation
// (D-ebb9-3), are refused, and nothing is issued.
func TestReplayAfterResumeRechecksAuthority(t *testing.T) {
	ctx := context.Background()

	// refused replays replay under tok and checks that it is refused and
	// issued nothing.
	refused := func(t *testing.T, e claimStopEnv, attemptID string, replay func() (Attempt, Step, error)) {
		t.Helper()
		before := openProofs(t, e.s, attemptID)
		if _, st, err := replay(); !errors.Is(err, ErrForbidden) || st.CoordinationProof != "" {
			t.Fatalf("a replay by a session that no longer acts the step: %+v %v", st, err)
		}
		if openProofs(t, e.s, attemptID) != before {
			t.Fatal("a refused replay changed the attempt's proofs")
		}
	}

	t.Run("claim", func(t *testing.T) {
		e := newClaimStopEnv(t)
		a, st, err := e.s.BeginClaimWithToken(ctx, "claim-1", e.indepTk, e.claimInput("task-1"))
		if err != nil {
			t.Fatal(err)
		}
		tok := e.resume(t, e.indepTk)
		e.setRole(t, e.indep, RoleWorker)
		refused(t, e, a.ID, func() (Attempt, Step, error) {
			return e.s.BeginClaimWithToken(ctx, "claim-1", tok, e.claimInput("task-1"))
		})
		e.setRole(t, e.indep, RoleIndependent)
		_, st2, err := e.s.BeginClaimWithToken(ctx, "claim-1", tok, e.claimInput("task-1"))
		if err != nil {
			t.Fatal(err)
		}
		e.replayed(t, st, st2, tok)
	})

	t.Run("stop release", func(t *testing.T) {
		e := newClaimStopEnv(t)
		a, _ := e.runningClaim(t, "task-1")
		if _, err := e.s.RequestStopWithToken(ctx, "stop-1", e.leadTok, a.ID, "priorities changed"); err != nil {
			t.Fatal(err)
		}
		if _, err := e.s.ConfirmStopWithToken(ctx, "confirm-1", e.indepTk, a.ID); err != nil {
			t.Fatal(err)
		}
		_, st, err := e.s.BeginStopReleaseWithToken(ctx, "release-1", e.indepTk, a.ID, ReleaseBlocked, "waiting")
		if err != nil {
			t.Fatal(err)
		}
		tok := e.resume(t, e.indepTk)
		e.setRole(t, e.indep, RoleCoordinator)
		refused(t, e, a.ID, func() (Attempt, Step, error) {
			return e.s.BeginStopReleaseWithToken(ctx, "release-1", tok, a.ID, ReleaseBlocked, "waiting")
		})
		e.setRole(t, e.indep, RoleIndependent)
		_, st2, err := e.s.BeginStopReleaseWithToken(ctx, "release-1", tok, a.ID, ReleaseBlocked, "waiting")
		if err != nil {
			t.Fatal(err)
		}
		e.replayed(t, st, st2, tok)
	})

	t.Run("finalize", func(t *testing.T) {
		e := newClaimStopEnv(t)
		a, seq := e.submitted(t, "task-1")
		if _, err := e.s.ReviewWithToken(ctx, "review-1", e.leadTok, a.ID, seq, ReviewAccept); err != nil {
			t.Fatal(err)
		}
		if _, err := e.s.ConfirmDeliveryWithToken(ctx, "confirm-1", e.leadTok, a.ID, seq, deliveryEvidence); err != nil {
			t.Fatal(err)
		}
		_, st, err := e.s.BeginFinalizeWithToken(ctx, "fin-1", e.indepTk, a.ID, seq)
		if err != nil {
			t.Fatal(err)
		}
		tok := e.resume(t, e.indepTk)
		e.setRole(t, e.indep, RoleWorker)
		refused(t, e, a.ID, func() (Attempt, Step, error) { return e.s.BeginFinalizeWithToken(ctx, "fin-1", tok, a.ID, seq) })
		e.setRole(t, e.indep, RoleIndependent)
		_, st2, err := e.s.BeginFinalizeWithToken(ctx, "fin-1", tok, a.ID, seq)
		if err != nil {
			t.Fatal(err)
		}
		e.replayed(t, st, st2, tok)
	})

	t.Run("finalize by the accepting coordinator", func(t *testing.T) {
		e := newClaimStopEnv(t)
		a, seq := e.submitted(t, "task-1")
		if _, err := e.s.ReviewWithToken(ctx, "review-1", e.leadTok, a.ID, seq, ReviewAccept); err != nil {
			t.Fatal(err)
		}
		if _, err := e.s.ConfirmDeliveryWithToken(ctx, "confirm-1", e.leadTok, a.ID, seq, deliveryEvidence); err != nil {
			t.Fatal(err)
		}
		if _, _, err := e.s.BeginFinalizeWithToken(ctx, "fin-1", e.leadTok, a.ID, seq); err != nil {
			t.Fatal(err)
		}
		tok := e.resume(t, e.leadTok)
		refused(t, e, a.ID, func() (Attempt, Step, error) { return e.s.BeginFinalizeWithToken(ctx, "fin-1", tok, a.ID, seq) })
	})

	t.Run("offer, accept and withdraw", func(t *testing.T) {
		e := newClaimStopEnv(t)
		wb, _ := e.s.AuthenticateSessionToken(ctx, e.workerTok)
		in := OfferInput{WorkerAgentID: wb.AgentID, Task: TaskRef{HubID: "hub-test", ProjectID: "project-t", TaskID: "task-1"},
			ExpectedRevision: 3, BaseCommit: "base-1", Branch: "work/task-1", Process: testPin, ExpiresAt: e.now.Add(time.Hour)}
		a, st, err := e.s.BeginOfferWithToken(ctx, "offer-1", e.leadTok, in)
		if err != nil {
			t.Fatal(err)
		}
		// The coordinator's own resume moves the team's coordinator
		// generation: its pending offer is not carried (D-ebb9-3).
		lead := e.resume(t, e.leadTok)
		refused(t, e, a.ID, func() (Attempt, Step, error) { return e.s.BeginOfferWithToken(ctx, "offer-1", lead, in) })

		// A committed offer, then the worker's acceptance begun: the
		// coordinator's resume refuses the acceptance's replay.
		e2 := newClaimStopEnv(t)
		wb, _ = e2.s.AuthenticateSessionToken(ctx, e2.workerTok)
		in.WorkerAgentID, in.ExpiresAt = wb.AgentID, e2.now.Add(time.Hour)
		a, st, err = e2.s.BeginOfferWithToken(ctx, "offer-1", e2.leadTok, in)
		if err != nil {
			t.Fatal(err)
		}
		e2.reader.commit(st.CoordinationProof, receiptFor(a, st, "res-1", "1", 4))
		if a, _, err = e2.settle(t, e2.leadTok, a, st, HintCommitted); err != nil {
			t.Fatal(err)
		}
		_, ast, err := e2.s.BeginAcceptWithToken(ctx, "accept-1", e2.workerTok, a.ID, testPin.InstructionDigest)
		if err != nil {
			t.Fatal(err)
		}
		// The worker's own resume carries its pending acceptance.
		worker := e2.resume(t, e2.workerTok)
		_, ast2, err := e2.s.BeginAcceptWithToken(ctx, "accept-1", worker, a.ID, testPin.InstructionDigest)
		if err != nil {
			t.Fatalf("the acceptance's replay after the worker's resume: %v", err)
		}
		e2.replayed(t, ast, ast2, worker)
		e2.resume(t, e2.leadTok)
		e2.workerTok = worker
		refused(t, e2, a.ID, func() (Attempt, Step, error) {
			return e2.s.BeginAcceptWithToken(ctx, "accept-1", e2.workerTok, a.ID, testPin.InstructionDigest)
		})

		// A withdrawal begun by the coordinator replays across its resume
		// while it is still the team's coordinator.
		e3 := newClaimStopEnv(t)
		wb, _ = e3.s.AuthenticateSessionToken(ctx, e3.workerTok)
		in.WorkerAgentID, in.ExpiresAt = wb.AgentID, e3.now.Add(time.Hour)
		a, st, err = e3.s.BeginOfferWithToken(ctx, "offer-1", e3.leadTok, in)
		if err != nil {
			t.Fatal(err)
		}
		e3.reader.commit(st.CoordinationProof, receiptFor(a, st, "res-1", "1", 4))
		if a, _, err = e3.settle(t, e3.leadTok, a, st, HintCommitted); err != nil {
			t.Fatal(err)
		}
		_, wst, err := e3.s.BeginWithdrawWithToken(ctx, "withdraw-1", e3.leadTok, a.ID)
		if err != nil {
			t.Fatal(err)
		}
		lead = e3.resume(t, e3.leadTok)
		e3.setRole(t, e3.lead, RoleWorker)
		refused(t, e3, a.ID, func() (Attempt, Step, error) { return e3.s.BeginWithdrawWithToken(ctx, "withdraw-1", lead, a.ID) })
		e3.setRole(t, e3.lead, RoleCoordinator)
		_, wst2, err := e3.s.BeginWithdrawWithToken(ctx, "withdraw-1", lead, a.ID)
		if err != nil {
			t.Fatal(err)
		}
		e3.replayed(t, wst, wst2, lead)
	})
}

// A key is the agent's per operation and scope: the claim's own key reused
// for a work update on the same attempt, after a resume, is its own receipt
// and begins its own step (the receipt is keyed on the operation and scope,
// which the intent's digest also covers).
func TestReplayKeyIsPerOperation(t *testing.T) {
	ctx := context.Background()
	e := newClaimStopEnv(t)
	a, claim := e.runningClaim(t, "task-1")
	tok := e.resume(t, e.indepTk)
	ua, upd, err := e.s.BeginWorkWithToken(ctx, "claim-task-1", tok, a.ID, IntentBlock, "waiting")
	if err != nil || ua.ID != a.ID || upd.Operation != ReservationUpdate || upd.RequestKey == claim.RequestKey {
		t.Fatalf("the claim's key for an update: %+v %v", upd, err)
	}
}
