package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
)

// beginForTest and completeForTest call the redemption commands with the
// argument order the invitation tests were written against.

func (s *Store) beginForTest(ctx context.Context, key string, code Secret) (Challenge, error) {
	return s.BeginRedemption(ctx, key, code)
}

func (s *Store) completeForTest(ctx context.Context, key string, code Secret, v Verifier, challengeID string, receipt Secret) (LinkResult, error) {
	return s.CompleteRedemption(ctx, v, key, code, challengeID, receipt)
}

func newCode(t *testing.T) Secret {
	t.Helper()
	code, err := GenerateInvitationCode()
	if err != nil {
		t.Fatal(err)
	}
	return code
}

func issueJoin(t *testing.T, s *Store, key, teamID string, code Secret) Invitation {
	t.Helper()
	inv, err := s.IssueInvitation(context.Background(), operator(t), key, InvitationRequest{
		Purpose: PurposeJoin, TeamID: teamID, Role: RoleWorker, HubID: "hub-a", Label: "builder",
	}, code)
	if err != nil {
		t.Fatalf("issue: %v", err)
	}
	return inv
}

func TestInvitationCodeFormat(t *testing.T) {
	code := newCode(t)
	text := code.Reveal()
	groups := strings.Split(text, "-")
	if len(groups) != 7 || len(strings.ReplaceAll(text, "-", "")) != codeBodyLen+codeCheckLen {
		t.Fatalf("code %q is not 7 groups of 28 characters", text)
	}
	want, err := invitationCodeDigest(code)
	if err != nil {
		t.Fatal(err)
	}
	// Case, separators and common confusions do not change the digest.
	loose := strings.ToLower(strings.ReplaceAll(text, "-", " "))
	loose = strings.NewReplacer("0", "o", "1", "l").Replace(loose)
	if got, err := invitationCodeDigest(NewSecret(loose)); err != nil || got != want {
		t.Fatalf("loosely typed code: %v, digest match %v", err, got == want)
	}
	// A typing error fails the checksum.
	body := []byte(strings.ReplaceAll(text, "-", ""))
	for _, r := range codeAlphabet {
		if byte(r) == body[3] {
			continue
		}
		typo := append([]byte(nil), body...)
		typo[3] = byte(r)
		if codeChecksum(string(typo[:codeBodyLen])) == string(body[codeBodyLen:]) {
			continue // this substitution happens to share the checksum
		}
		if _, err := invitationCodeDigest(NewSecret(string(typo))); !errors.Is(err, ErrInvalid) {
			t.Fatalf("typo %q accepted: %v", typo, err)
		}
		break
	}
	for _, bad := range []string{"", "short", strings.Repeat("U", 28), text + "X"} {
		if _, err := invitationCodeDigest(NewSecret(bad)); !errors.Is(err, ErrInvalid) {
			t.Errorf("code %q accepted", bad)
		}
	}
	seen := map[string]bool{}
	for range 1000 {
		c := newCode(t).Reveal()
		if seen[c] {
			t.Fatal("duplicate code generated")
		}
		seen[c] = true
	}
	if fmt.Sprint(code) == text || strings.Contains(fmt.Sprintf("%+v %#v", code, code), text) {
		t.Fatal("code is printed in clear")
	}
}

func TestIssueInvitationIsOperatorOnly(t *testing.T) {
	s, _ := openTemp(t)
	ctx := context.Background()
	tm := mustTeam(t, s, "t1", "crew")
	a := mustAgent(t, s, "a1", "builder")
	req := InvitationRequest{Purpose: PurposeJoin, TeamID: tm.ID, Role: RoleWorker, HubID: "hub-a", Label: "x"}
	var zero Caller
	if _, err := s.IssueInvitation(ctx, zero, "k", req, newCode(t)); !errors.Is(err, ErrForbidden) {
		t.Errorf("zero caller issue: %v", err)
	}
	if _, err := s.IssueInvitation(ctx, agentCaller(t, a.ID), "k", req, newCode(t)); !errors.Is(err, ErrForbidden) {
		t.Errorf("agent caller issue: %v", err)
	}
	inv := issueJoin(t, s, "k1", tm.ID, newCode(t))
	if _, err := s.RevokeInvitation(ctx, zero, "r", inv.ID, inv.Revision); !errors.Is(err, ErrForbidden) {
		t.Errorf("zero caller revoke: %v", err)
	}
	if n := count(t, s, "invitations"); n != 1 {
		t.Errorf("invitations = %d, want 1", n)
	}
}

func TestIssueInvitationScopeRules(t *testing.T) {
	s, _ := openTemp(t)
	ctx := context.Background()
	op := operator(t)
	tm := mustTeam(t, s, "t1", "crew")
	a := mustAgent(t, s, "a1", "builder")
	cases := map[string]struct {
		req  InvitationRequest
		want error
	}{
		"join without label":  {InvitationRequest{Purpose: PurposeJoin, TeamID: tm.ID, Role: RoleWorker, HubID: "hub-a"}, ErrInvalid},
		"link without agent":  {InvitationRequest{Purpose: PurposeLink, TeamID: tm.ID, Role: RoleWorker, HubID: "hub-a"}, ErrInvalid},
		"rebind without pin":  {InvitationRequest{Purpose: PurposeRebind, TeamID: tm.ID, Role: RoleWorker, HubID: "hub-a", AgentID: a.ID}, ErrInvalid},
		"lifetime over 72h":   {InvitationRequest{Purpose: PurposeJoin, TeamID: tm.ID, Role: RoleWorker, HubID: "hub-a", Label: "x", TTL: 73 * time.Hour}, ErrInvalid},
		"negative lifetime":   {InvitationRequest{Purpose: PurposeJoin, TeamID: tm.ID, Role: RoleWorker, HubID: "hub-a", Label: "x", TTL: -time.Hour}, ErrInvalid},
		"unknown team":        {InvitationRequest{Purpose: PurposeJoin, TeamID: "nope", Role: RoleWorker, HubID: "hub-a", Label: "x"}, ErrNotFound},
		"unknown bound agent": {InvitationRequest{Purpose: PurposeLink, TeamID: tm.ID, Role: RoleWorker, HubID: "hub-a", AgentID: "nope"}, ErrNotFound},
	}
	for name, c := range cases {
		if _, err := s.IssueInvitation(ctx, op, "k-"+name, c.req, newCode(t)); !errors.Is(err, c.want) {
			t.Errorf("%s: got %v, want %v", name, err, c.want)
		}
	}
	if _, err := s.IssueInvitation(ctx, op, "k-malformed", InvitationRequest{Purpose: PurposeJoin, TeamID: tm.ID, Role: RoleWorker, HubID: "hub-a", Label: "x"}, NewSecret("not-a-code")); !errors.Is(err, ErrInvalid) {
		t.Errorf("malformed code: %v", err)
	}
	inv := issueJoin(t, s, "k-ok", tm.ID, newCode(t))
	if got := inv.ExpiresAt.Sub(inv.CreatedAt); got != defaultInvitationTTL {
		t.Errorf("default lifetime = %s", got)
	}
	rebind, err := s.IssueInvitation(ctx, op, "k-rebind", InvitationRequest{
		Purpose: PurposeRebind, TeamID: tm.ID, Role: RoleWorker, HubID: "hub-a", AgentID: a.ID, ExpectedUserID: "user-2", TTL: 72 * time.Hour,
	}, newCode(t))
	if err != nil || rebind.ExpectedUserID != "user-2" || rebind.ExpiresAt.Sub(rebind.CreatedAt) != 72*time.Hour {
		t.Errorf("pinned rebind = %+v, %v", rebind, err)
	}
}

// The code is never stored: not in the invitation, audit or retry receipt.
func TestInvitationCodeIsNeverStored(t *testing.T) {
	s, _ := openTemp(t)
	tm := mustTeam(t, s, "t1", "crew")
	code := newCode(t)
	inv := issueJoin(t, s, "k1", tm.ID, code)
	issueJoin(t, s, "k1", tm.ID, code) // replay
	plain := code.Reveal()
	norm := strings.ReplaceAll(plain, "-", "")
	rows, err := s.db.Query(`SELECT id || code_digest || purpose || team_id || hub_id || label || issued_by FROM invitations
		UNION ALL SELECT input || result FROM audit UNION ALL SELECT result FROM receipts`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var text string
		if err := rows.Scan(&text); err != nil {
			t.Fatal(err)
		}
		if strings.Contains(text, plain) || strings.Contains(text, norm) {
			t.Fatalf("invitation code stored in: %s", text)
		}
	}
	shown, _ := json.Marshal(inv)
	if strings.Contains(string(shown), "digest") || strings.Contains(string(shown), norm) {
		t.Fatalf("invitation view exposes code material: %s", shown)
	}
}

func TestIssueReplayConflictAndDuplicateCode(t *testing.T) {
	s, _ := openTemp(t)
	ctx := context.Background()
	tm := mustTeam(t, s, "t1", "crew")
	code := newCode(t)
	first := issueJoin(t, s, "k1", tm.ID, code)

	// Lost reply: the caller still holds the code and retries identically.
	again := issueJoin(t, s, "k1", tm.ID, code)
	if again.ID != first.ID || count(t, s, "invitations") != 1 {
		t.Fatalf("replay issued a second invitation: %+v", again)
	}
	req := InvitationRequest{Purpose: PurposeJoin, TeamID: tm.ID, Role: RoleWorker, HubID: "hub-a", Label: "builder"}
	if _, err := s.IssueInvitation(ctx, operator(t), "k1", req, newCode(t)); !errors.Is(err, ErrIdempotencyConflict) {
		t.Errorf("same key, different code: %v", err)
	}
	if _, err := s.IssueInvitation(ctx, operator(t), "k2", req, code); !errors.Is(err, ErrExists) {
		t.Errorf("same code, new key: %v", err)
	}
}

// A refusal for a presented code never says why.
func TestRedemptionRefusalsDoNotDisclose(t *testing.T) {
	s, _ := openTemp(t)
	ctx := context.Background()
	op := operator(t)
	v := newFakeVerifier()
	tm := mustTeam(t, s, "t1", "crew")

	expiredCode, revokedCode, redeemedCode, lockedCode := newCode(t), newCode(t), newCode(t), newCode(t)
	if _, err := s.IssueInvitation(ctx, op, "exp", InvitationRequest{
		Purpose: PurposeJoin, TeamID: tm.ID, Role: RoleWorker, HubID: "hub-a", Label: "x", TTL: time.Hour,
	}, expiredCode); err != nil {
		t.Fatal(err)
	}
	rev := issueJoin(t, s, "rev", tm.ID, revokedCode)
	if _, err := s.RevokeInvitation(ctx, op, "revoke", rev.ID, rev.Revision); err != nil {
		t.Fatal(err)
	}
	issueJoin(t, s, "red", tm.ID, redeemedCode)
	ch, err := s.beginForTest(ctx, "b-red", redeemedCode)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.completeForTest(ctx, "c-red", redeemedCode, v, ch.ID, v.receipt("rc", "hub-a", "user-1", "tok-1")); err != nil {
		t.Fatal(err)
	}
	issueJoin(t, s, "lock", tm.ID, lockedCode)
	for i := range maxInvitationAttempts {
		if _, err := s.beginForTest(ctx, fmt.Sprintf("b-lock-%d", i), lockedCode); err != nil {
			t.Fatal(err)
		}
	}
	s.now = func() time.Time { return time.Now().UTC().Add(2 * time.Hour) }

	cases := map[string]Secret{
		"unknown":   newCode(t),
		"malformed": NewSecret("garbage"),
		"expired":   expiredCode,
		"revoked":   revokedCode,
		"redeemed":  redeemedCode,
		"locked":    lockedCode,
	}
	for name, code := range cases {
		_, err := s.beginForTest(ctx, "probe-"+name, code)
		if err != ErrInvitationInvalid { //nolint:errorlint // identical error, on purpose
			t.Errorf("%s: got %v, want exactly ErrInvitationInvalid", name, err)
		}
	}
}

func TestRevocation(t *testing.T) {
	s, _ := openTemp(t)
	ctx := context.Background()
	op := operator(t)
	v := newFakeVerifier()
	tm := mustTeam(t, s, "t1", "crew")
	code := newCode(t)
	inv := issueJoin(t, s, "k1", tm.ID, code)
	ch, err := s.beginForTest(ctx, "b1", code)
	if err != nil {
		t.Fatal(err)
	}
	after, _ := s.GetInvitation(ctx, inv.ID)
	if _, err := s.RevokeInvitation(ctx, op, "r-stale", inv.ID, inv.Revision); !errors.Is(err, ErrRevisionConflict) {
		t.Errorf("revoke with a stale revision: %v", err)
	}
	revoked, err := s.RevokeInvitation(ctx, op, "r1", inv.ID, after.Revision)
	if err != nil || revoked.State != InvitationRevoked {
		t.Fatalf("revoke = %+v, %v", revoked, err)
	}
	// A challenge begun before revocation can no longer complete.
	if _, err := s.completeForTest(ctx, "c1", code, v, ch.ID, v.receipt("rc", "hub-a", "user-1", "tok-1")); err != ErrInvitationInvalid { //nolint:errorlint
		t.Errorf("complete after revocation: %v", err)
	}
	if count(t, s, "agents") != 0 || count(t, s, "memberships") != 0 {
		t.Fatal("revoked invitation created an agent or membership")
	}
	if _, err := s.RevokeInvitation(ctx, op, "r2", inv.ID, revoked.Revision); !errors.Is(err, ErrInvitationFinal) {
		t.Errorf("revoke twice: %v", err)
	}
}

func TestExpiry(t *testing.T) {
	s, _ := openTemp(t)
	ctx := context.Background()
	tm := mustTeam(t, s, "t1", "crew")
	code := newCode(t)
	if _, err := s.IssueInvitation(ctx, operator(t), "k1", InvitationRequest{
		Purpose: PurposeJoin, TeamID: tm.ID, Role: RoleWorker, HubID: "hub-a", Label: "x", TTL: time.Hour,
	}, code); err != nil {
		t.Fatal(err)
	}
	if _, err := s.beginForTest(ctx, "b1", code); err != nil {
		t.Fatalf("begin before expiry: %v", err)
	}
	s.now = func() time.Time { return time.Now().UTC().Add(61 * time.Minute) }
	if _, err := s.beginForTest(ctx, "b2", code); err != ErrInvitationInvalid { //nolint:errorlint
		t.Errorf("begin after expiry: %v", err)
	}
}

// Five begins are allowed; the fifth locks the invitation, whose newest
// challenge may still complete.
func TestAttemptLimit(t *testing.T) {
	s, _ := openTemp(t)
	ctx := context.Background()
	v := newFakeVerifier()
	tm := mustTeam(t, s, "t1", "crew")
	code := newCode(t)
	inv := issueJoin(t, s, "k1", tm.ID, code)
	var challenges []Challenge
	for i := range maxInvitationAttempts {
		ch, err := s.beginForTest(ctx, fmt.Sprintf("b%d", i), code)
		if err != nil {
			t.Fatalf("begin %d: %v", i+1, err)
		}
		challenges = append(challenges, ch)
	}
	locked, _ := s.GetInvitation(ctx, inv.ID)
	if locked.State != InvitationLocked || locked.Attempts != maxInvitationAttempts {
		t.Fatalf("after %d begins: %+v", maxInvitationAttempts, locked)
	}
	if _, err := s.beginForTest(ctx, "b-over", code); err != ErrInvitationInvalid { //nolint:errorlint
		t.Fatalf("begin past the limit: %v", err)
	}
	// Retrying the first begin is not counted again. Its challenge was
	// superseded, so the retry is refused rather than replayed.
	if _, err := s.beginForTest(ctx, "b0", code); !errors.Is(err, ErrChallengeInvalid) {
		t.Fatalf("retry of a superseded begin: %v", err)
	}
	if again, _ := s.GetInvitation(ctx, inv.ID); again.Attempts != maxInvitationAttempts {
		t.Fatalf("retry counted as an attempt: %d", again.Attempts)
	}
	rc := v.receipt("rc", "hub-a", "user-1", "tok-1")
	if _, err := s.completeForTest(ctx, "c-old", code, v, challenges[0].ID, rc); !errors.Is(err, ErrChallengeInvalid) {
		t.Errorf("superseded challenge: %v", err)
	}
	res, err := s.completeForTest(ctx, "c-new", code, v, challenges[len(challenges)-1].ID, rc)
	if err != nil || !res.Created {
		t.Fatalf("newest challenge on a locked invitation = %+v, %v", res, err)
	}
	if done, _ := s.GetInvitation(ctx, inv.ID); done.State != InvitationRedeemed {
		t.Errorf("state after completion = %s", done.State)
	}
}

func TestConcurrentBeginsRespectTheLimit(t *testing.T) {
	s, _ := openTemp(t)
	ctx := context.Background()
	tm := mustTeam(t, s, "t1", "crew")
	code := newCode(t)
	inv := issueJoin(t, s, "k1", tm.ID, code)
	const n = 12
	errs := make([]error, n)
	var wg sync.WaitGroup
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, errs[i] = s.beginForTest(ctx, fmt.Sprintf("b%d", i), code)
		}()
	}
	wg.Wait()
	ok := 0
	for _, err := range errs {
		switch {
		case err == nil:
			ok++
		case err != ErrInvitationInvalid: //nolint:errorlint
			t.Errorf("unexpected error %v", err)
		}
	}
	after, _ := s.GetInvitation(ctx, inv.ID)
	if ok != maxInvitationAttempts || after.Attempts != maxInvitationAttempts || after.State != InvitationLocked {
		t.Fatalf("successful begins = %d, invitation = %+v", ok, after)
	}
}

// Redemption commits the invitation state with the link and membership.
func TestRedemptionComposesAtomically(t *testing.T) {
	s, _ := openTemp(t)
	ctx := context.Background()
	v := newFakeVerifier()
	tm := mustTeam(t, s, "t1", "crew")
	code := newCode(t)
	inv := issueJoin(t, s, "k1", tm.ID, code)
	ch, err := s.beginForTest(ctx, "b1", code)
	if err != nil {
		t.Fatal(err)
	}
	rc := v.receipt("rc", "hub-a", "user-1", "tok-1")

	injected := errors.New("injected")
	s.beforeReceipt = func(op string) error {
		if op == opCompleteRedemption {
			return injected
		}
		return nil
	}
	if _, err := s.completeForTest(ctx, "c1", code, v, ch.ID, rc); !errors.Is(err, injected) {
		t.Fatalf("got %v, want injected failure", err)
	}
	mid, _ := s.GetInvitation(ctx, inv.ID)
	if mid.State != InvitationIssued || count(t, s, "agents") != 0 || count(t, s, "memberships") != 0 {
		t.Fatalf("partial redemption: invitation %s, agents %d, memberships %d",
			mid.State, count(t, s, "agents"), count(t, s, "memberships"))
	}
	s.beforeReceipt = nil
	res, err := s.completeForTest(ctx, "c2", code, v, ch.ID, rc)
	if err != nil || !res.Created {
		t.Fatalf("redemption = %+v, %v", res, err)
	}
	done, _ := s.GetInvitation(ctx, inv.ID)
	if done.State != InvitationRedeemed {
		t.Fatalf("invitation after redemption = %s", done.State)
	}
	if _, err := s.completeForTest(ctx, "c3", code, v, ch.ID, rc); err == nil {
		t.Fatal("a redeemed invitation completed twice")
	}
}
