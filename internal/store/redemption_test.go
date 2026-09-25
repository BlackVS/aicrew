package store

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestRedemptionLinksJoinsAndKeepsNoSecrets(t *testing.T) {
	s, _ := openTemp(t)
	ctx := context.Background()
	v := newFakeVerifier()
	tm := mustTeam(t, s, "t1", "crew")
	code := newCode(t)
	inv := issueJoin(t, s, "k1", tm.ID, code)
	ch, err := s.BeginRedemption(ctx, "b1", code)
	if err != nil {
		t.Fatal(err)
	}
	const receiptValue = "aimem-receipt-7c1e"
	res, err := s.CompleteRedemption(ctx, v, "c1", code, ch.ID, v.receipt(receiptValue, "hub-a", "user-1", "tok-1"))
	if err != nil || !res.Created || res.Membership.TeamID != tm.ID {
		t.Fatalf("redemption = %+v, %v", res, err)
	}
	a, _ := s.GetAgent(ctx, res.AgentID)
	if a.Linked == nil || a.Linked.UserID != "user-1" {
		t.Fatalf("agent after redemption = %+v", a)
	}
	if done, _ := s.GetInvitation(ctx, inv.ID); done.State != InvitationRedeemed {
		t.Fatalf("invitation state = %s", done.State)
	}
	// Neither the code nor the receipt appears in any stored command text.
	rows, err := s.db.Query(`SELECT scope || input || result FROM audit UNION ALL SELECT scope || result FROM receipts`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var text string
		if err := rows.Scan(&text); err != nil {
			t.Fatal(err)
		}
		for _, secret := range []string{receiptValue, code.Reveal(), strings.ReplaceAll(code.Reveal(), "-", "")} {
			if strings.Contains(text, secret) {
				t.Fatalf("secret found in stored command text: %s", text)
			}
		}
	}
}

func TestChallengeDeadlineCappedAtInvitationExpiry(t *testing.T) {
	s, _ := openTemp(t)
	ctx := context.Background()
	tm := mustTeam(t, s, "t1", "crew")
	code := newCode(t)
	inv, err := s.IssueInvitation(ctx, operator(t), "k1", InvitationRequest{
		Purpose: PurposeJoin, TeamID: tm.ID, Role: RoleWorker, HubID: "hub-a", Label: "x", TTL: time.Hour,
	}, code)
	if err != nil {
		t.Fatal(err)
	}
	// Two minutes before the invitation expires, a challenge gets two
	// minutes, not five.
	s.now = func() time.Time { return inv.ExpiresAt.Add(-2 * time.Minute) }
	ch, err := s.BeginRedemption(ctx, "b1", code)
	if err != nil {
		t.Fatal(err)
	}
	if !ch.ExpiresAt.Equal(inv.ExpiresAt) {
		t.Fatalf("challenge expires %s, invitation %s", ch.ExpiresAt, inv.ExpiresAt)
	}
	// Well before expiry the normal five-minute bound applies.
	code2 := newCode(t)
	issueJoin(t, s, "k2", tm.ID, code2)
	s.now = func() time.Time { return time.Now().UTC() }
	ch2, err := s.BeginRedemption(ctx, "b2", code2)
	if err != nil {
		t.Fatal(err)
	}
	if got := ch2.ExpiresAt.Sub(ch2.CreatedAt); got != challengeTTL {
		t.Fatalf("challenge lifetime = %s, want %s", got, challengeTTL)
	}
}

func TestBeginRetryRules(t *testing.T) {
	s, _ := openTemp(t)
	ctx := context.Background()
	op := operator(t)
	tm := mustTeam(t, s, "t1", "crew")
	code := newCode(t)
	inv := issueJoin(t, s, "k1", tm.ID, code)
	first, err := s.BeginRedemption(ctx, "b1", code)
	if err != nil {
		t.Fatal(err)
	}
	again, err := s.BeginRedemption(ctx, "b1", code)
	if err != nil || again.ID != first.ID {
		t.Fatalf("lost-reply retry = %+v, %v", again, err)
	}
	if got, _ := s.GetInvitation(ctx, inv.ID); got.Attempts != 1 {
		t.Fatalf("retry counted: attempts = %d", got.Attempts)
	}
	// The same key for another invitation is its own begin, not a replay.
	other := newCode(t)
	issueJoin(t, s, "k2", tm.ID, other)
	otherCh, err := s.BeginRedemption(ctx, "b1", other)
	if err != nil || otherCh.ID == first.ID || otherCh.InvitationID == first.InvitationID {
		t.Fatalf("key reused across invitations = %+v, %v", otherCh, err)
	}
	// After revocation, the retry is refused.
	cur, _ := s.GetInvitation(ctx, inv.ID)
	if _, err := s.RevokeInvitation(ctx, op, "r1", inv.ID, cur.Revision); err != nil {
		t.Fatal(err)
	}
	if _, err := s.BeginRedemption(ctx, "b1", code); err != ErrInvitationInvalid { //nolint:errorlint
		t.Fatalf("retry after revocation: %v", err)
	}
}

// Accepted recovery (docs/ONBOARDING-CONTRACT.md, retry table): a same-key
// begin retry after the challenge expired is refused and counts nothing; a
// new key begins a new attempt, which completes while the invitation is
// still usable.
func TestExpiredBeginRetryRecoversWithNewKey(t *testing.T) {
	s, _ := openTemp(t)
	ctx := context.Background()
	v := newFakeVerifier()
	tm := mustTeam(t, s, "t1", "crew")
	code := newCode(t)
	inv := issueJoin(t, s, "k1", tm.ID, code)
	first, err := s.BeginRedemption(ctx, "b1", code)
	if err != nil {
		t.Fatal(err)
	}

	// The reply was lost and the challenge expired before the retry.
	s.now = func() time.Time { return first.ExpiresAt.Add(time.Second) }
	if _, err := s.BeginRedemption(ctx, "b1", code); !errors.Is(err, ErrChallengeInvalid) {
		t.Fatalf("same-key retry after expiry: got %v, want ErrChallengeInvalid", err)
	}
	if got, _ := s.GetInvitation(ctx, inv.ID); got.Attempts != 1 || got.State != InvitationIssued {
		t.Fatalf("refused retry changed the invitation: %+v", got)
	}

	// Recovery: a new key is a new attempt with a fresh challenge.
	second, err := s.BeginRedemption(ctx, "b2", code)
	if err != nil {
		t.Fatalf("new-key begin: %v", err)
	}
	if second.ID == first.ID || second.State != "pending" {
		t.Fatalf("new-key challenge = %+v", second)
	}
	if got, _ := s.GetInvitation(ctx, inv.ID); got.Attempts != 2 {
		t.Fatalf("attempts after recovery = %d, want 2", got.Attempts)
	}
	res, err := s.CompleteRedemption(ctx, v, "c1", code, second.ID, v.receipt("rc", "hub-a", "user-1", "tok-1"))
	if err != nil || !res.Created {
		t.Fatalf("completion after recovery = %+v, %v", res, err)
	}
}

func TestCompleteRetryAndConflict(t *testing.T) {
	s, _ := openTemp(t)
	ctx := context.Background()
	v := newFakeVerifier()
	tm := mustTeam(t, s, "t1", "crew")
	code := newCode(t)
	issueJoin(t, s, "k1", tm.ID, code)
	ch, err := s.BeginRedemption(ctx, "b1", code)
	if err != nil {
		t.Fatal(err)
	}
	rc := v.receipt("rc-1", "hub-a", "user-1", "tok-1")
	first, err := s.CompleteRedemption(ctx, v, "c1", code, ch.ID, rc)
	if err != nil {
		t.Fatal(err)
	}
	calls := v.calls
	// Lost reply: the exact retry is answered from the receipt.
	again, err := s.CompleteRedemption(ctx, v, "c1", code, ch.ID, rc)
	if err != nil || again != first {
		t.Fatalf("retry = %+v, %v", again, err)
	}
	if v.calls != calls {
		t.Fatal("retry asked aimem again")
	}
	// The same key with a different challenge is a conflict, not a replay.
	if _, err := s.CompleteRedemption(ctx, v, "c1", code, "another-challenge", rc); !errors.Is(err, ErrIdempotencyConflict) {
		t.Fatalf("same key, different challenge: %v", err)
	}
	// A new key after redemption is refused without asking aimem.
	if _, err := s.CompleteRedemption(ctx, v, "c2", code, ch.ID, rc); err != ErrInvitationInvalid { //nolint:errorlint
		t.Fatalf("new key after redemption: %v", err)
	}
	if v.calls != calls {
		t.Fatal("refused completion asked aimem")
	}
}

func TestConcurrentCompletions(t *testing.T) {
	s, _ := openTemp(t)
	ctx := context.Background()
	v := newFakeVerifier()
	tm := mustTeam(t, s, "t1", "crew")
	const n = 6

	run := func(code Secret, chID string, key func(int) string) ([]LinkResult, []error) {
		results := make([]LinkResult, n)
		errs := make([]error, n)
		var wg sync.WaitGroup
		for i := range n {
			wg.Add(1)
			go func() {
				defer wg.Done()
				results[i], errs[i] = s.CompleteRedemption(ctx, v, key(i), code, chID, v.receipt("rc-"+chID, "hub-a", "user-"+chID, "tok"))
			}()
		}
		wg.Wait()
		return results, errs
	}

	// Different keys: exactly one completion wins.
	code := newCode(t)
	issueJoin(t, s, "k1", tm.ID, code)
	ch, err := s.BeginRedemption(ctx, "b1", code)
	if err != nil {
		t.Fatal(err)
	}
	_, errs := run(code, ch.ID, func(i int) string { return fmt.Sprintf("c%d", i) })
	wins := 0
	for _, err := range errs {
		switch {
		case err == nil:
			wins++
		case err != ErrInvitationInvalid && !errors.Is(err, ErrChallengeInvalid): //nolint:errorlint
			t.Errorf("unexpected error %v", err)
		}
	}
	if wins != 1 || count(t, s, "agents") != 1 || count(t, s, "memberships") != 1 {
		t.Fatalf("wins = %d, agents = %d, memberships = %d", wins, count(t, s, "agents"), count(t, s, "memberships"))
	}

	// The same key: every call gets the one result.
	code2 := newCode(t)
	issueJoin(t, s, "k2", tm.ID, code2)
	ch2, err := s.BeginRedemption(ctx, "b2", code2)
	if err != nil {
		t.Fatal(err)
	}
	calls := v.calls
	results, errs := run(code2, ch2.ID, func(int) string { return "same-key" })
	for i, err := range errs {
		if err != nil || results[i].AgentID == "" || !reflect.DeepEqual(results[i], results[0]) {
			t.Errorf("same-key completion %d = %+v, %v; want %+v", i, results[i], err, results[0])
		}
	}
	if v.calls != calls+1 {
		t.Errorf("same-key completions asked aimem %d times, want once", v.calls-calls)
	}
	if count(t, s, "agents") != 2 {
		t.Fatalf("same-key completions created %d agents in total, want 2", count(t, s, "agents"))
	}
}

// Revocation and completion serialize: whichever commits first wins.
func TestRevocationAndCompletionOneOutcome(t *testing.T) {
	s, _ := openTemp(t)
	ctx := context.Background()
	op := operator(t)
	v := newFakeVerifier()
	tm := mustTeam(t, s, "t1", "crew")

	// Revocation lands while aimem is verifying: completion creates nothing.
	code := newCode(t)
	inv := issueJoin(t, s, "k1", tm.ID, code)
	ch, err := s.BeginRedemption(ctx, "b1", code)
	if err != nil {
		t.Fatal(err)
	}
	v.onRedeem = func() {
		v.onRedeem = nil
		cur, _ := s.GetInvitation(ctx, inv.ID)
		if _, err := s.RevokeInvitation(ctx, op, "r1", inv.ID, cur.Revision); err != nil {
			t.Errorf("revoke during verification: %v", err)
		}
	}
	if _, err := s.CompleteRedemption(ctx, v, "c1", code, ch.ID, v.receipt("rc-1", "hub-a", "user-1", "tok-1")); err != ErrInvitationInvalid { //nolint:errorlint
		t.Fatalf("completion after revocation: %v", err)
	}
	if count(t, s, "agents") != 0 || count(t, s, "memberships") != 0 {
		t.Fatal("revoked invitation created an agent or membership")
	}
	if got, _ := s.GetInvitation(ctx, inv.ID); got.State != InvitationRevoked {
		t.Fatalf("state = %s, want revoked", got.State)
	}

	// Completion first: the later revocation is refused.
	code2 := newCode(t)
	inv2 := issueJoin(t, s, "k2", tm.ID, code2)
	ch2, err := s.BeginRedemption(ctx, "b2", code2)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.CompleteRedemption(ctx, v, "c2", code2, ch2.ID, v.receipt("rc-2", "hub-a", "user-2", "tok-2")); err != nil {
		t.Fatal(err)
	}
	cur, _ := s.GetInvitation(ctx, inv2.ID)
	if _, err := s.RevokeInvitation(ctx, op, "r2", inv2.ID, cur.Revision); !errors.Is(err, ErrInvitationFinal) {
		t.Fatalf("revocation after completion: %v", err)
	}
}

// An invitation that expires while aimem is verifying is refused when the
// commit transaction re-resolves it, even though the pre-check passed.
func TestInvitationExpiringDuringVerification(t *testing.T) {
	s, _ := openTemp(t)
	ctx := context.Background()
	v := newFakeVerifier()
	tm := mustTeam(t, s, "t1", "crew")
	code := newCode(t)
	inv, err := s.IssueInvitation(ctx, operator(t), "k1", InvitationRequest{
		Purpose: PurposeJoin, TeamID: tm.ID, Role: RoleWorker, HubID: "hub-a", Label: "x", TTL: time.Hour,
	}, code)
	if err != nil {
		t.Fatal(err)
	}
	ch, err := s.BeginRedemption(ctx, "b1", code)
	if err != nil {
		t.Fatal(err)
	}
	v.onRedeem = func() {
		s.now = func() time.Time { return inv.ExpiresAt.Add(time.Second) }
	}
	if _, err := s.CompleteRedemption(ctx, v, "c1", code, ch.ID, v.receipt("rc", "hub-a", "user-1", "tok-1")); err != ErrInvitationInvalid { //nolint:errorlint
		t.Fatalf("completion after expiry during verification: %v", err)
	}
	if count(t, s, "agents") != 0 || count(t, s, "memberships") != 0 {
		t.Fatal("expired invitation created an agent or membership")
	}
}

func TestVerifierOutageCreatesNothing(t *testing.T) {
	s, _ := openTemp(t)
	ctx := context.Background()
	v := newFakeVerifier()
	tm := mustTeam(t, s, "t1", "crew")
	code := newCode(t)
	inv := issueJoin(t, s, "k1", tm.ID, code)
	ch, err := s.BeginRedemption(ctx, "b1", code)
	if err != nil {
		t.Fatal(err)
	}
	v.fail = errAimemUnavailable
	if _, err := s.CompleteRedemption(ctx, v, "c1", code, ch.ID, NewSecret("rc")); !errors.Is(err, errAimemUnavailable) {
		t.Fatalf("completion with aimem down: %v", err)
	}
	if count(t, s, "agents") != 0 || count(t, s, "memberships") != 0 {
		t.Fatal("outage created an agent or membership")
	}
	if got, _ := s.GetInvitation(ctx, inv.ID); got.State != InvitationIssued {
		t.Fatalf("state after outage = %s", got.State)
	}
	// Once aimem is back, the same challenge completes.
	v.fail = nil
	if _, err := s.CompleteRedemption(ctx, v, "c2", code, ch.ID, v.receipt("rc-ok", "hub-a", "user-1", "tok-1")); err != nil {
		t.Fatalf("completion after recovery: %v", err)
	}
}

// Invitations and challenges that cannot succeed are refused before aimem
// is asked.
func TestPrechecksAvoidAskingAimem(t *testing.T) {
	s, _ := openTemp(t)
	ctx := context.Background()
	v := newFakeVerifier()
	tm := mustTeam(t, s, "t1", "crew")
	codeA, codeB := newCode(t), newCode(t)
	issueJoin(t, s, "ka", tm.ID, codeA)
	issueJoin(t, s, "kb", tm.ID, codeB)
	chA, err := s.BeginRedemption(ctx, "ba", codeA)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.BeginRedemption(ctx, "bb", codeB); err != nil {
		t.Fatal(err)
	}
	rc := v.receipt("rc", "hub-a", "user-1", "tok-1")
	if _, err := s.CompleteRedemption(ctx, v, "c1", codeB, chA.ID, rc); !errors.Is(err, ErrChallengeInvalid) {
		t.Errorf("challenge of another invitation: %v", err)
	}
	if _, err := s.CompleteRedemption(ctx, v, "c2", newCode(t), chA.ID, rc); err != ErrInvitationInvalid { //nolint:errorlint
		t.Errorf("unknown code: %v", err)
	}
	s.now = func() time.Time { return time.Now().UTC().Add(25 * time.Hour) }
	if _, err := s.CompleteRedemption(ctx, v, "c3", codeA, chA.ID, rc); err != ErrInvitationInvalid { //nolint:errorlint
		t.Errorf("expired invitation: %v", err)
	}
	if v.calls != 0 {
		t.Fatalf("aimem asked %d times for completions that could not succeed", v.calls)
	}
}
