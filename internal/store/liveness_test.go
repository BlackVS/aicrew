package store

import (
	"context"
	"testing"
	"time"
)

// proofEnded is when proof ended, or "" while it is live.
func proofEnded(t *testing.T, s *Store, proof string) string {
	t.Helper()
	var ended string
	if err := s.db.QueryRow(`SELECT ended_at FROM coordination_proofs WHERE digest = ?`, secretDigest(proof)).Scan(&ended); err != nil {
		t.Fatal(err)
	}
	return ended
}

// Every move of a session's generation (resume, end, credential rotation)
// ends the live proofs the session issued before it, at the move's own time,
// and leaves another session's live proof alone (01a0f758-c827).
func TestGenerationMoveEndsTheSessionsProofs(t *testing.T) {
	ctx := context.Background()
	moves := map[string]func(t *testing.T, e stepEnv){
		"resume": func(t *testing.T, e stepEnv) {
			if _, err := e.s.ResumeSession(ctx, e.lead.caller, "resume", e.lead.sess.ID); err != nil {
				t.Fatal(err)
			}
		},
		"end": func(t *testing.T, e stepEnv) {
			if _, err := e.s.StopSession(ctx, operator(t), "stop", e.lead.sess.ID); err != nil {
				t.Fatal(err)
			}
		},
		"credential rotation": func(t *testing.T, e stepEnv) {
			tx, err := e.s.db.BeginTx(ctx, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback()
			agent, err := getAgent(ctx, tx, e.lead.agent.ID)
			if err != nil {
				t.Fatal(err)
			}
			id := VerifiedIdentity{HubID: agent.Linked.HubID, UserID: agent.Linked.UserID, TokenID: "token-rotated"}
			if rotated, err := rotateIfNeeded(ctx, tx, agent, id, e.s.now()); err != nil || !rotated {
				t.Fatalf("rotate: %v %v", rotated, err)
			}
			if err := tx.Commit(); err != nil {
				t.Fatal(err)
			}
		},
	}
	for name, move := range moves {
		t.Run(name, func(t *testing.T) {
			e := newStepEnv(t)
			other := newExecTeamNamed(t, e.s, "other")
			if _, err := e.s.db.Exec(`UPDATE agents SET linked_hub_id = 'hub-a'`); err != nil {
				t.Fatal(err)
			}
			a, st := e.begin(t, "offer", "task-1")
			oreq := other.offerReq("task-2")
			oreq.ExpiresAt = e.s.now().Add(time.Hour)
			oa, ost, err := e.s.BeginOffer(ctx, other.lead.caller, "other-offer", oreq)
			if err != nil {
				t.Fatal(err)
			}
			if !active(t, e.s, st.CoordinationProof) || !active(t, e.s, ost.CoordinationProof) {
				t.Fatal("the proofs do not answer before the move")
			}

			e.advance(time.Second)
			at := formatTime(e.s.now())
			move(t, e)

			if got := proofEnded(t, e.s, st.CoordinationProof); got != at {
				t.Fatalf("the moved session's proof ended at %q, want the move's time %q", got, at)
			}
			if openProofs(t, e.s, a.ID) != 0 || active(t, e.s, st.CoordinationProof) {
				t.Fatal("the moved session's proof still lives")
			}
			if proofEnded(t, e.s, ost.CoordinationProof) != "" || openProofs(t, e.s, oa.ID) != 1 || !active(t, e.s, ost.CoordinationProof) {
				t.Fatal("another session's live proof was ended")
			}
		})
	}
}

// After a resume, the never-sent step's proof has ended at the resume, so
// the reconciler settles it not committed NoneFinalAfter later, not when
// the proof would have expired, and the worker's capacity is free again.
func TestNeverSentStepSettlesAfterTheResume(t *testing.T) {
	ctx := context.Background()
	e := newStepEnv(t)
	a, _ := e.begin(t, "offer", "task-1")
	e.advance(time.Minute)
	if _, err := e.s.ResumeSession(ctx, e.lead.caller, "resume", e.lead.sess.ID); err != nil {
		t.Fatal(err)
	}

	e.advance(NoneFinalAfter - time.Second)
	got, set, err := e.s.ReconcileStep(ctx, e.reader, a.ID)
	if err != nil || set.Settled || got.State != AttemptOffering || set.RetryAfter != time.Second {
		t.Fatalf("before none is final: %+v %+v %v", got, set, err)
	}
	e.advance(time.Second)
	got, set, err = e.s.ReconcileStep(ctx, e.reader, a.ID)
	if err != nil || !set.Settled || got.State != AttemptClosed || got.CloseReason != "claim not_committed" ||
		busy(t, e.s, e.builder.agent.ID) {
		t.Fatalf("once none is final after the resume: %+v %+v %v", got, set, err)
	}
	if ProofLifetime <= time.Minute+NoneFinalAfter {
		t.Fatal("the test no longer separates the resume's end from the proof's expiry")
	}
}

// A step's begin replayed after the resume still gets a fresh, live proof at
// the session's new generation; the proof the resume ended stays ended.
func TestReplayAfterResumeGetsALiveProof(t *testing.T) {
	ctx := context.Background()
	e := newClaimStopEnv(t)
	a, st, err := e.s.BeginClaimWithToken(ctx, "claim-1", e.indepTk, e.claimInput("task-1"))
	if err != nil {
		t.Fatal(err)
	}
	*e.now = e.now.Add(time.Second)
	at := formatTime(*e.now)
	tok := e.resume(t, e.indepTk)
	if got := proofEnded(t, e.s, st.CoordinationProof); got != at {
		t.Fatalf("the claim's proof ended at %q, want the resume's time %q", got, at)
	}

	*e.now = e.now.Add(time.Second)
	again, st2, err := e.s.BeginClaimWithToken(ctx, "claim-1", tok, e.claimInput("task-1"))
	if err != nil || again.ID != a.ID || st2.CoordinationProof == "" || st2.CoordinationProof == st.CoordinationProof {
		t.Fatalf("replay after resume: %+v %+v %v", again, st2, err)
	}
	f := e.fact(t, st2.CoordinationProof)
	if !f.Active || f.Member.Generation != e.generation(t, tok) {
		t.Fatalf("the replacement's fact = %+v, want active at the new generation %d", f, e.generation(t, tok))
	}
	if proofEnded(t, e.s, st.CoordinationProof) != at || e.fact(t, st.CoordinationProof).Active {
		t.Fatal("the proof the resume ended answers again")
	}
}
