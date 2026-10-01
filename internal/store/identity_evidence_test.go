package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"testing"
)

// A finalize carries the attempt's identity after the confirmed delivery
// references (1aad G2), so a member or trusted caller may give at most one
// fewer than aimem takes: 15 of 16.
func TestFinalizeEvidenceIsCappedForTheIdentity(t *testing.T) {
	ctx := context.Background()
	e := newClaimStopEnv(t)
	a, seq := e.submitted(t, "task-1")
	if _, err := e.s.ReviewWithToken(ctx, "review-1", e.leadTok, a.ID, seq, ReviewAccept); err != nil {
		t.Fatal(err)
	}
	evidence := func(n int) []Evidence {
		out := append([]Evidence(nil), deliveryEvidence...)
		for i := len(out); i < n; i++ {
			out = append(out, Evidence{Kind: "check", Ref: fmt.Sprintf("https://forge.example/checks/%d", i)})
		}
		return out
	}
	if _, err := e.s.ConfirmDeliveryWithToken(ctx, "confirm-16", e.leadTok, a.ID, seq, evidence(16)); !errors.Is(err, ErrInvalid) {
		t.Fatalf("a confirmation of 16 references: %v", err)
	}
	if _, err := e.s.ConfirmDeliveryWithToken(ctx, "confirm-15", e.leadTok, a.ID, seq, evidence(15)); err != nil {
		t.Fatalf("a confirmation of 15 references: %v", err)
	}
	a, st, err := e.s.BeginFinalizeWithToken(ctx, "fin-1", e.indepTk, a.ID, seq)
	if err != nil {
		t.Fatal(err)
	}
	want := append(refsOf(evidence(15)), "aicrew attempt "+a.ID+" by member "+a.WorkerAgentID)
	if len(st.TerminalEvidence) != maxDeliveryEvidence || !reflect.DeepEqual(st.TerminalEvidence, want) {
		t.Fatalf("the finalize's terminal evidence: %q", st.TerminalEvidence)
	}
	if f := e.fact(t, st.CoordinationProof); f.EvidenceDigest != evidenceDigest(want) {
		t.Fatalf("the finalize's evidence digest: %+v", f)
	}

	// The trusted path has the same cap.
	if err := (TrustedDelivery{Required: DevelopmentDelivery, Evidence: evidence(16)}).check(); !errors.Is(err, ErrInvalid) {
		t.Fatalf("trusted evidence of 16 references: %v", err)
	}
}

// A delivery confirmed with 16 references before a finalize carried the
// identity leaves it no room: the finalize refuses it as unconfirmed, and it
// is confirmed again.
func TestFinalizeRefusesAnOlderFullConfirmation(t *testing.T) {
	ctx := context.Background()
	e := newClaimStopEnv(t)
	a, seq := e.submitted(t, "task-1")
	if _, err := e.s.ReviewWithToken(ctx, "review-1", e.leadTok, a.ID, seq, ReviewAccept); err != nil {
		t.Fatal(err)
	}
	if _, err := e.s.ConfirmDeliveryWithToken(ctx, "confirm-1", e.leadTok, a.ID, seq, deliveryEvidence); err != nil {
		t.Fatal(err)
	}
	full := append([]Evidence(nil), deliveryEvidence...)
	for i := len(full); i < maxDeliveryEvidence; i++ {
		full = append(full, Evidence{Kind: "check", Ref: fmt.Sprintf("https://forge.example/checks/%d", i)})
	}
	raw, _ := json.Marshal(full)
	if _, err := e.s.db.ExecContext(ctx, `UPDATE attempts SET delivery_evidence = ? WHERE id = ?`, string(raw), a.ID); err != nil {
		t.Fatal(err)
	}
	if _, _, err := e.s.BeginFinalizeWithToken(ctx, "fin-1", e.indepTk, a.ID, seq); !errors.Is(err, ErrDeliveryUnconfirmed) {
		t.Fatalf("a finalize over an older full confirmation: %v", err)
	}
	if _, err := e.s.ConfirmDeliveryWithToken(ctx, "confirm-2", e.leadTok, a.ID, seq, deliveryEvidence); err != nil {
		t.Fatal(err)
	}
	if _, _, err := e.s.BeginFinalizeWithToken(ctx, "fin-2", e.indepTk, a.ID, seq); err != nil {
		t.Fatalf("a finalize after confirming again: %v", err)
	}
}
