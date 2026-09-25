package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeVerifier stands in for aimem in tests only. Receipts map to the
// identity aimem would vouch for; a retried request key returns the
// original answer, as the context contract requires.
type fakeVerifier struct {
	mu       sync.Mutex
	receipts map[string]VerifiedIdentity
	byKey    map[string]VerifiedIdentity
	fail     error
	calls    int
	// onRedeem, if set, runs inside Redeem before it answers, to model
	// something else happening while aimem is being asked.
	onRedeem func()
}

func newFakeVerifier() *fakeVerifier {
	return &fakeVerifier{receipts: map[string]VerifiedIdentity{}, byKey: map[string]VerifiedIdentity{}}
}

var errAimemUnavailable = errors.New("aimem unavailable")

func (f *fakeVerifier) Redeem(_ context.Context, req RedeemRequest) (VerifiedIdentity, error) {
	if f.onRedeem != nil {
		f.onRedeem()
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if f.fail != nil {
		return VerifiedIdentity{}, f.fail
	}
	if id, ok := f.byKey[req.RequestKey]; ok {
		return id, nil
	}
	id, ok := f.receipts[req.Receipt.Reveal()]
	if !ok {
		return VerifiedIdentity{}, errors.New("receipt refused")
	}
	f.byKey[req.RequestKey] = id
	return id, nil
}

// receipt registers a receipt for an identity and returns it as a Secret.
func (f *fakeVerifier) receipt(value, hub, user, token string) Secret {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.receipts[value] = VerifiedIdentity{HubID: hub, UserID: user, TokenID: token}
	return NewSecret(value)
}

// The two helpers below play the future redemption command: they run the
// package-internal invitation functions inside one store command, which is
// exactly how crew-onboarding will compose them with the invitation store.

func (s *Store) issueForTest(ctx context.Context, key string, sc InvitationScope) (Challenge, error) {
	var out Challenge
	err := s.run(ctx, Caller{}, command{
		op: "test.issue_invitation_challenge", scope: sc.invitationID, key: key,
		input: struct{ Key string }{key}, authorize: anyCaller,
		apply: func(ctx context.Context, tx *sql.Tx, now time.Time) (any, error) {
			return issueInvitationChallenge(ctx, tx, sc, now)
		},
	}, &out)
	return out, err
}

func (s *Store) redeemForTest(ctx context.Context, key string, sc InvitationScope, v Verifier, challengeID string, receipt Secret) (LinkResult, error) {
	ch, err := s.pendingChallenge(ctx, challengeID, challengeInvitation)
	if err != nil {
		return LinkResult{}, err
	}
	id, err := verifyChallenge(ctx, v, ch, receipt, "test-redeem:"+key)
	if err != nil {
		return LinkResult{}, err
	}
	var out LinkResult
	err = s.run(ctx, Caller{}, command{
		op: "test.redeem", scope: sc.invitationID, key: key,
		input: struct{ ChallengeID string }{challengeID}, authorize: anyCaller,
		apply: func(ctx context.Context, tx *sql.Tx, now time.Time) (any, error) {
			return s.bindInvitation(ctx, tx, sc, challengeID, id, now)
		},
	}, &out)
	return out, err
}

func scope(t *testing.T, inv string, purpose InvitationPurpose, teamID string, role Role, agentID, pinned, label string) InvitationScope {
	t.Helper()
	sc, err := newInvitationScope(inv, purpose, teamID, role, "hub-a", agentID, pinned, label)
	if err != nil {
		t.Fatalf("scope: %v", err)
	}
	return sc
}

// joinAs redeems a join invitation for the given identity.
func joinAs(t *testing.T, s *Store, v *fakeVerifier, inv, teamID string, role Role, label, user, token string) LinkResult {
	t.Helper()
	ctx := context.Background()
	sc := scope(t, inv, PurposeJoin, teamID, role, "", "", label)
	ch, err := s.issueForTest(ctx, inv+"-issue", sc)
	if err != nil {
		t.Fatal(err)
	}
	res, err := s.redeemForTest(ctx, inv+"-redeem", sc, v, ch.ID, v.receipt(inv+"-receipt", "hub-a", user, token))
	if err != nil {
		t.Fatalf("join %s: %v", inv, err)
	}
	return res
}

func TestJoinCreatesLinkedAgentWithMembership(t *testing.T) {
	s, _ := openTemp(t)
	ctx := context.Background()
	v := newFakeVerifier()
	tm := mustTeam(t, s, "t1", "crew")
	res := joinAs(t, s, v, "inv-1", tm.ID, RoleWorker, "builder", "user-1", "tok-1")
	if !res.Created || res.Membership.Role != RoleWorker {
		t.Fatalf("join result = %+v", res)
	}
	a, err := s.GetAgent(ctx, res.AgentID)
	if err != nil || a.Label != "builder" || a.Linked == nil ||
		*a.Linked != (LinkedActor{HubID: "hub-a", UserID: "user-1", TokenID: "tok-1"}) {
		t.Fatalf("agent = %+v, %v", a, err)
	}
	ch, err := getChallenge(ctx, s.db, res.ChallengeID)
	if err != nil || ch.State != "consumed" {
		t.Fatalf("challenge after join = %+v, %v", ch, err)
	}
	// The linked agent can now start a session bound to its credential.
	sess, err := s.StartSession(ctx, agentCaller(t, a.ID), "s1", tm.ID)
	if err != nil || sess.TokenID != "tok-1" {
		t.Fatalf("session = %+v, %v", sess, err)
	}
}

func TestFailedOrUnavailableVerificationChangesNothing(t *testing.T) {
	s, _ := openTemp(t)
	ctx := context.Background()
	v := newFakeVerifier()
	tm := mustTeam(t, s, "t1", "crew")
	sc := scope(t, "inv-1", PurposeJoin, tm.ID, RoleWorker, "", "", "builder")
	ch, err := s.issueForTest(ctx, "issue", sc)
	if err != nil {
		t.Fatal(err)
	}
	counts := func() [4]int {
		return [4]int{count(t, s, "agents"), count(t, s, "memberships"), count(t, s, "audit"), count(t, s, "receipts")}
	}
	before := counts()

	cases := map[string]func() error{
		"aimem unavailable": func() error {
			v.fail = errAimemUnavailable
			defer func() { v.fail = nil }()
			_, err := s.redeemForTest(ctx, "r1", sc, v, ch.ID, NewSecret("anything"))
			if !errors.Is(err, errAimemUnavailable) {
				return fmt.Errorf("got %v", err)
			}
			return nil
		},
		"receipt refused": func() error {
			_, err := s.redeemForTest(ctx, "r2", sc, v, ch.ID, NewSecret("unknown-receipt"))
			if err == nil {
				return errors.New("refused receipt was accepted")
			}
			return nil
		},
		"incomplete identity": func() error {
			_, err := s.redeemForTest(ctx, "r3", sc, v, ch.ID, v.receipt("partial", "hub-a", "user-1", ""))
			if !errors.Is(err, ErrIdentityMismatch) {
				return fmt.Errorf("got %v", err)
			}
			return nil
		},
		"wrong hub": func() error {
			_, err := s.redeemForTest(ctx, "r4", sc, v, ch.ID, v.receipt("other-hub", "hub-b", "user-1", "tok-1"))
			if !errors.Is(err, ErrIdentityMismatch) {
				return fmt.Errorf("got %v", err)
			}
			return nil
		},
		"empty receipt": func() error {
			_, err := s.redeemForTest(ctx, "r5", sc, v, ch.ID, NewSecret(""))
			if !errors.Is(err, ErrInvalid) {
				return fmt.Errorf("got %v", err)
			}
			return nil
		},
	}
	for name, run := range cases {
		if err := run(); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
	if after := counts(); after != before {
		t.Errorf("failed verification changed state: %v -> %v", before, after)
	}
	still, err := getChallenge(ctx, s.db, ch.ID)
	if err != nil || still.State != "pending" {
		t.Errorf("challenge after failures = %+v, %v", still, err)
	}

	// Agent re-proof: an unavailable verifier leaves the link untouched.
	res := joinAs(t, s, v, "inv-2", tm.ID, RoleIndependent, "linked", "user-2", "tok-2")
	pc, err := s.IssueAgentChallenge(ctx, Caller{}, "c1", res.AgentID)
	if err != nil {
		t.Fatal(err)
	}
	v.fail = errAimemUnavailable
	if _, err := s.CompleteAgentProof(ctx, Caller{}, v, "p1", pc.ID, NewSecret("x")); !errors.Is(err, errAimemUnavailable) {
		t.Fatalf("proof with aimem down: %v", err)
	}
	v.fail = nil
	a, _ := s.GetAgent(ctx, res.AgentID)
	if a.Linked.TokenID != "tok-2" {
		t.Errorf("link changed by failed proof: %+v", a.Linked)
	}
}

// One aimem user links to at most one agent.
func TestOneUserOneAgent(t *testing.T) {
	s, _ := openTemp(t)
	ctx := context.Background()
	v := newFakeVerifier()
	t1 := mustTeam(t, s, "t1", "one")
	t2 := mustTeam(t, s, "t2", "two")
	first := joinAs(t, s, v, "inv-1", t1.ID, RoleWorker, "builder", "user-1", "tok-1")

	// A second join by the same user adds a membership to the same agent.
	second := joinAs(t, s, v, "inv-2", t2.ID, RoleWorker, "ignored", "user-1", "tok-1")
	if second.AgentID != first.AgentID || second.Created {
		t.Fatalf("second join made a duplicate agent: %+v", second)
	}
	if n := count(t, s, "agents"); n != 1 {
		t.Fatalf("agents = %d, want 1", n)
	}

	// Linking another agent record to that user is refused.
	other := mustAgent(t, s, "a-other", "other")
	sc := scope(t, "inv-3", PurposeLink, t1.ID, RoleWorker, other.ID, "", "")
	ch, err := s.issueForTest(ctx, "issue-3", sc)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.redeemForTest(ctx, "r3", sc, v, ch.ID, v.receipt("rc-3", "hub-a", "user-1", "tok-9")); !errors.Is(err, ErrIdentityAlreadyLinked) {
		t.Fatalf("link to a linked user: got %v, want ErrIdentityAlreadyLinked", err)
	}
	// So is a rebind of another agent to that user.
	joined := joinAs(t, s, v, "inv-4", t1.ID, RoleIndependent, "third", "user-3", "tok-3")
	rb := scope(t, "inv-5", PurposeRebind, t1.ID, RoleIndependent, joined.AgentID, "user-1", "")
	ch5, err := s.issueForTest(ctx, "issue-5", rb)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.redeemForTest(ctx, "r5", rb, v, ch5.ID, v.receipt("rc-5", "hub-a", "user-1", "tok-1")); !errors.Is(err, ErrIdentityAlreadyLinked) {
		t.Fatalf("rebind to a linked user: got %v, want ErrIdentityAlreadyLinked", err)
	}
	// The schema backs the rule even against a direct write.
	if _, err := s.db.Exec(`UPDATE agents SET linked_hub_id = 'hub-a', linked_user_id = 'user-1', linked_token_id = 't'
		WHERE id = ?`, other.ID); err == nil {
		t.Fatal("schema accepted a second agent for one identity")
	}
}

func TestLinkAndPinRules(t *testing.T) {
	s, _ := openTemp(t)
	ctx := context.Background()
	v := newFakeVerifier()
	tm := mustTeam(t, s, "t1", "crew")
	a := mustAgent(t, s, "a1", "builder")

	// A pinned join refuses a different user.
	pinned := scope(t, "inv-p", PurposeJoin, tm.ID, RoleWorker, "", "user-9", "pinned")
	chp, err := s.issueForTest(ctx, "issue-p", pinned)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.redeemForTest(ctx, "rp", pinned, v, chp.ID, v.receipt("rc-p", "hub-a", "user-1", "tok-1")); !errors.Is(err, ErrIdentityMismatch) {
		t.Fatalf("pinned join by another user: got %v, want ErrIdentityMismatch", err)
	}

	// Link an unlinked record, then link it again for another team as the same user.
	sc := scope(t, "inv-1", PurposeLink, tm.ID, RoleWorker, a.ID, "", "")
	ch, err := s.issueForTest(ctx, "issue-1", sc)
	if err != nil {
		t.Fatal(err)
	}
	res, err := s.redeemForTest(ctx, "r1", sc, v, ch.ID, v.receipt("rc-1", "hub-a", "user-1", "tok-1"))
	if err != nil || res.AgentID != a.ID || res.Created {
		t.Fatalf("link = %+v, %v", res, err)
	}
	t2 := mustTeam(t, s, "t2", "two")
	again := scope(t, "inv-2", PurposeLink, t2.ID, RoleWorker, a.ID, "", "")
	ch2, err := s.issueForTest(ctx, "issue-2", again)
	if err != nil {
		t.Fatal(err)
	}
	if res, err := s.redeemForTest(ctx, "r2", again, v, ch2.ID, v.receipt("rc-2", "hub-a", "user-1", "tok-1")); err != nil || res.Membership.TeamID != t2.ID {
		t.Fatalf("same-user link = %+v, %v", res, err)
	}
	// Linking that record for a different user needs a rebind.
	third := scope(t, "inv-3", PurposeLink, tm.ID, RoleWorker, a.ID, "", "")
	ch3, err := s.issueForTest(ctx, "issue-3", third)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.redeemForTest(ctx, "r3", third, v, ch3.ID, v.receipt("rc-3", "hub-a", "user-2", "tok-2")); !errors.Is(err, ErrIdentityMismatch) {
		t.Fatalf("link to another user: got %v, want ErrIdentityMismatch", err)
	}
}

func TestRoleConflictChangesNothing(t *testing.T) {
	s, _ := openTemp(t)
	ctx := context.Background()
	v := newFakeVerifier()
	tm := mustTeam(t, s, "t1", "crew")
	res := joinAs(t, s, v, "inv-1", tm.ID, RoleWorker, "builder", "user-1", "tok-1")
	sc := scope(t, "inv-2", PurposeJoin, tm.ID, RoleCoordinator, "", "", "builder")
	ch, err := s.issueForTest(ctx, "issue-2", sc)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.redeemForTest(ctx, "r2", sc, v, ch.ID, v.receipt("rc-2", "hub-a", "user-1", "tok-1")); !errors.Is(err, ErrRoleConflict) {
		t.Fatalf("join with another role: got %v, want ErrRoleConflict", err)
	}
	m, err := getMembership(ctx, s.db, tm.ID, res.AgentID)
	if err != nil || m.Role != RoleWorker {
		t.Fatalf("membership after conflict = %+v, %v", m, err)
	}
	if still, _ := getChallenge(ctx, s.db, ch.ID); still.State != "pending" {
		t.Errorf("refused redemption consumed the challenge")
	}
}

func TestRebindEndsSessionsAndRespectsOutstandingWork(t *testing.T) {
	s, _ := openTemp(t)
	ctx := context.Background()
	v := newFakeVerifier()
	tm := mustTeam(t, s, "t1", "crew")
	res := joinAs(t, s, v, "inv-1", tm.ID, RoleCoordinator, "builder", "user-1", "tok-1")
	self := agentCaller(t, res.AgentID)
	sess, err := s.StartSession(ctx, self, "s1", tm.ID)
	if err != nil {
		t.Fatal(err)
	}
	rb := scope(t, "inv-2", PurposeRebind, tm.ID, RoleCoordinator, res.AgentID, "user-2", "")
	ch, err := s.issueForTest(ctx, "issue-2", rb)
	if err != nil {
		t.Fatal(err)
	}

	// Outstanding work blocks the rebind and changes nothing.
	s.outstandingWork = func(context.Context, *sql.Tx, string) (bool, error) { return true, nil }
	if _, err := s.redeemForTest(ctx, "r-busy", rb, v, ch.ID, v.receipt("rc-busy", "hub-a", "user-2", "tok-2")); !errors.Is(err, ErrWorkOutstanding) {
		t.Fatalf("rebind with work: got %v, want ErrWorkOutstanding", err)
	}
	if a, _ := s.GetAgent(ctx, res.AgentID); a.Linked.UserID != "user-1" {
		t.Fatalf("link changed despite outstanding work: %+v", a.Linked)
	}
	if live, _ := s.GetSession(ctx, sess.ID); live.State != SessionActive {
		t.Fatalf("session ended despite refusal: %+v", live)
	}

	// Without work, the pinned user rebinds and every session ends.
	s.outstandingWork = func(context.Context, *sql.Tx, string) (bool, error) { return false, nil }
	out, err := s.redeemForTest(ctx, "r2", rb, v, ch.ID, v.receipt("rc-2", "hub-a", "user-2", "tok-2"))
	if err != nil || !out.Rebound {
		t.Fatalf("rebind = %+v, %v", out, err)
	}
	a, _ := s.GetAgent(ctx, res.AgentID)
	if a.Linked.UserID != "user-2" || a.Linked.TokenID != "tok-2" {
		t.Fatalf("link after rebind = %+v", a.Linked)
	}
	ended, _ := s.GetSession(ctx, sess.ID)
	if ended.State != SessionEnded || ended.Generation != sess.Generation+1 {
		t.Fatalf("session after rebind = %+v", ended)
	}
	if team, _ := s.GetTeam(ctx, tm.ID); team.CoordinatorGeneration != sess.CoordinatorGeneration+1 {
		t.Errorf("coordinator generation after rebind = %d", team.CoordinatorGeneration)
	}
}

func TestRotationRebindsSessionsUnderNewGeneration(t *testing.T) {
	s, _ := openTemp(t)
	ctx := context.Background()
	v := newFakeVerifier()
	tm := mustTeam(t, s, "t1", "crew")
	res := joinAs(t, s, v, "inv-1", tm.ID, RoleWorker, "builder", "user-1", "tok-1")
	self := agentCaller(t, res.AgentID)
	sess, err := s.StartSession(ctx, self, "s1", tm.ID)
	if err != nil {
		t.Fatal(err)
	}
	ch, err := s.IssueAgentChallenge(ctx, Caller{}, "c1", res.AgentID)
	if err != nil {
		t.Fatal(err)
	}
	proof, err := s.CompleteAgentProof(ctx, Caller{}, v, "p1", ch.ID, v.receipt("rc-rot", "hub-a", "user-1", "tok-2"))
	if err != nil || !proof.Rotated {
		t.Fatalf("rotation proof = %+v, %v", proof, err)
	}
	rebound, _ := s.GetSession(ctx, sess.ID)
	if rebound.State != SessionActive || rebound.TokenID != "tok-2" || rebound.Generation != 2 {
		t.Fatalf("session after rotation = %+v", rebound)
	}
	if _, err := s.Heartbeat(ctx, self, "h-old", sess.ID, 1); !errors.Is(err, ErrContextStale) {
		t.Errorf("command under the old credential's generation: got %v, want ErrContextStale", err)
	}
	if team, _ := s.GetTeam(ctx, tm.ID); team.CoordinatorGeneration != 0 {
		t.Errorf("rotation changed the coordinator generation: %d", team.CoordinatorGeneration)
	}
	// A proof by a different user is refused and changes nothing.
	ch2, _ := s.IssueAgentChallenge(ctx, Caller{}, "c2", res.AgentID)
	if _, err := s.CompleteAgentProof(ctx, Caller{}, v, "p2", ch2.ID, v.receipt("rc-x", "hub-a", "user-9", "tok-9")); !errors.Is(err, ErrIdentityMismatch) {
		t.Fatalf("proof by another user: got %v, want ErrIdentityMismatch", err)
	}
	if a, _ := s.GetAgent(ctx, res.AgentID); a.Linked.UserID != "user-1" || a.Linked.TokenID != "tok-2" {
		t.Fatalf("link after refused proof = %+v", a.Linked)
	}
}

func TestChallengeValidity(t *testing.T) {
	s, _ := openTemp(t)
	ctx := context.Background()
	v := newFakeVerifier()
	tm := mustTeam(t, s, "t1", "crew")

	unlinked := mustAgent(t, s, "a1", "unlinked")
	if _, err := s.IssueAgentChallenge(ctx, Caller{}, "c0", unlinked.ID); !errors.Is(err, ErrIdentityLinkRequired) {
		t.Fatalf("challenge for an unlinked agent: got %v, want ErrIdentityLinkRequired", err)
	}

	sc := scope(t, "inv-1", PurposeJoin, tm.ID, RoleWorker, "", "", "builder")
	old, err := s.issueForTest(ctx, "issue-a", sc)
	if err != nil {
		t.Fatal(err)
	}
	newer, err := s.issueForTest(ctx, "issue-b", sc)
	if err != nil {
		t.Fatal(err)
	}
	calls := v.calls
	if _, err := s.redeemForTest(ctx, "r-old", sc, v, old.ID, v.receipt("rc-1", "hub-a", "user-1", "tok-1")); !errors.Is(err, ErrChallengeInvalid) {
		t.Errorf("superseded challenge: got %v, want ErrChallengeInvalid", err)
	}
	other := scope(t, "inv-2", PurposeJoin, tm.ID, RoleWorker, "", "", "other")
	if _, err := s.redeemForTest(ctx, "r-other", other, v, newer.ID, v.receipt("rc-2", "hub-a", "user-2", "tok-2")); !errors.Is(err, ErrChallengeInvalid) {
		t.Errorf("challenge of another invitation: got %v, want ErrChallengeInvalid", err)
	}
	if _, err := s.CompleteAgentProof(ctx, Caller{}, v, "p-kind", newer.ID, v.receipt("rc-3", "hub-a", "user-1", "tok-1")); !errors.Is(err, ErrChallengeInvalid) {
		t.Errorf("invitation challenge used as agent challenge: got %v, want ErrChallengeInvalid", err)
	}
	if v.calls != calls+1 {
		// Only the other-invitation case reaches the verifier: its
		// challenge is pending; the binding check refuses it afterwards.
		t.Errorf("verifier calls = %d, want %d", v.calls, calls+1)
	}

	// Expired challenges are refused before aimem is asked.
	s.now = func() time.Time { return time.Now().UTC().Add(challengeTTL + time.Minute) }
	calls = v.calls
	if _, err := s.redeemForTest(ctx, "r-late", sc, v, newer.ID, v.receipt("rc-4", "hub-a", "user-1", "tok-1")); !errors.Is(err, ErrChallengeInvalid) {
		t.Errorf("expired challenge: got %v, want ErrChallengeInvalid", err)
	}
	if v.calls != calls {
		t.Errorf("verifier called for an expired challenge")
	}
}

func TestProofRetryAnswersFromReceipt(t *testing.T) {
	s, _ := openTemp(t)
	ctx := context.Background()
	v := newFakeVerifier()
	tm := mustTeam(t, s, "t1", "crew")
	res := joinAs(t, s, v, "inv-1", tm.ID, RoleWorker, "builder", "user-1", "tok-1")
	ch, err := s.IssueAgentChallenge(ctx, Caller{}, "c1", res.AgentID)
	if err != nil {
		t.Fatal(err)
	}
	rc := v.receipt("rc-1", "hub-a", "user-1", "tok-1")
	first, err := s.CompleteAgentProof(ctx, Caller{}, v, "p1", ch.ID, rc)
	if err != nil {
		t.Fatal(err)
	}
	calls := v.calls
	again, err := s.CompleteAgentProof(ctx, Caller{}, v, "p1", ch.ID, rc)
	if err != nil || again != first {
		t.Fatalf("retry = %+v, %v; want %+v", again, err, first)
	}
	if v.calls != calls {
		t.Errorf("retry called the verifier again")
	}
	if _, err := s.CompleteAgentProof(ctx, Caller{}, v, "p2", ch.ID, rc); !errors.Is(err, ErrChallengeInvalid) {
		t.Errorf("reuse of a consumed challenge: got %v, want ErrChallengeInvalid", err)
	}
}

func TestConcurrentCompletionsConsumeOnce(t *testing.T) {
	s, _ := openTemp(t)
	ctx := context.Background()
	v := newFakeVerifier()
	tm := mustTeam(t, s, "t1", "crew")
	sc := scope(t, "inv-1", PurposeJoin, tm.ID, RoleWorker, "", "", "builder")
	ch, err := s.issueForTest(ctx, "issue", sc)
	if err != nil {
		t.Fatal(err)
	}
	rc := v.receipt("rc-1", "hub-a", "user-1", "tok-1")
	const n = 6
	errs := make([]error, n)
	var wg sync.WaitGroup
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, errs[i] = s.redeemForTest(ctx, fmt.Sprintf("r%d", i), sc, v, ch.ID, rc)
		}()
	}
	wg.Wait()
	wins := 0
	for _, err := range errs {
		switch {
		case err == nil:
			wins++
		case !errors.Is(err, ErrChallengeInvalid):
			t.Errorf("unexpected error %v", err)
		}
	}
	if wins != 1 || count(t, s, "agents") != 1 || count(t, s, "memberships") != 1 {
		t.Fatalf("wins = %d, agents = %d, memberships = %d", wins, count(t, s, "agents"), count(t, s, "memberships"))
	}
}

// Redemption commits link, agent, membership and challenge state together.
func TestRedemptionIsAtomic(t *testing.T) {
	s, _ := openTemp(t)
	ctx := context.Background()
	v := newFakeVerifier()
	tm := mustTeam(t, s, "t1", "crew")
	sc := scope(t, "inv-1", PurposeJoin, tm.ID, RoleWorker, "", "", "builder")
	ch, err := s.issueForTest(ctx, "issue", sc)
	if err != nil {
		t.Fatal(err)
	}
	injected := errors.New("injected")
	s.beforeReceipt = func(op string) error {
		if op == "test.redeem" {
			return injected
		}
		return nil
	}
	if _, err := s.redeemForTest(ctx, "r1", sc, v, ch.ID, v.receipt("rc-1", "hub-a", "user-1", "tok-1")); !errors.Is(err, injected) {
		t.Fatalf("got %v, want injected failure", err)
	}
	if count(t, s, "agents") != 0 || count(t, s, "memberships") != 0 {
		t.Fatalf("partial redemption left agents=%d memberships=%d", count(t, s, "agents"), count(t, s, "memberships"))
	}
	if still, _ := getChallenge(ctx, s.db, ch.ID); still.State != "pending" {
		t.Fatalf("challenge after rollback = %s", still.State)
	}
}

func TestReceiptIsNeverStoredOrPrinted(t *testing.T) {
	s, _ := openTemp(t)
	ctx := context.Background()
	v := newFakeVerifier()
	tm := mustTeam(t, s, "t1", "crew")
	const value = "receipt-SECRET-4f7a"
	sc := scope(t, "inv-1", PurposeJoin, tm.ID, RoleWorker, "", "", "builder")
	ch, err := s.issueForTest(ctx, "issue", sc)
	if err != nil {
		t.Fatal(err)
	}
	rc := v.receipt(value, "hub-a", "user-1", "tok-1")
	res, err := s.redeemForTest(ctx, "r1", sc, v, ch.ID, rc)
	if err != nil {
		t.Fatal(err)
	}
	pc, _ := s.IssueAgentChallenge(ctx, Caller{}, "c1", res.AgentID)
	if _, err := s.CompleteAgentProof(ctx, Caller{}, v, "p1", pc.ID, rc); err != nil {
		t.Fatal(err)
	}
	// Audit and receipts are the only tables that store command text.
	rows, err := s.db.Query(`SELECT input || result FROM audit UNION ALL SELECT result FROM receipts`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var text string
		if err := rows.Scan(&text); err != nil {
			t.Fatal(err)
		}
		if strings.Contains(text, value) {
			t.Fatalf("receipt found in stored text: %s", text)
		}
	}
	printed := fmt.Sprintf("%v %+v %#v %s", rc, rc, rc, rc)
	encoded, _ := json.Marshal(RedeemRequest{Receipt: rc})
	if strings.Contains(printed, value) || strings.Contains(string(encoded), value) {
		t.Fatalf("receipt leaked: %s / %s", printed, encoded)
	}
}

func TestInvitationScopeRules(t *testing.T) {
	bad := []struct {
		name string
		make func() error
	}{
		{"join naming an agent", func() error {
			_, err := newInvitationScope("i", PurposeJoin, "t", RoleWorker, "hub-a", "agent", "", "label")
			return err
		}},
		{"join without a label", func() error {
			_, err := newInvitationScope("i", PurposeJoin, "t", RoleWorker, "hub-a", "", "", "")
			return err
		}},
		{"link without an agent", func() error {
			_, err := newInvitationScope("i", PurposeLink, "t", RoleWorker, "hub-a", "", "", "")
			return err
		}},
		{"rebind without a pinned user", func() error {
			_, err := newInvitationScope("i", PurposeRebind, "t", RoleWorker, "hub-a", "agent", "", "")
			return err
		}},
		{"unknown role", func() error {
			_, err := newInvitationScope("i", PurposeJoin, "t", Role("admin"), "hub-a", "", "", "label")
			return err
		}},
		{"invalid hub", func() error {
			_, err := newInvitationScope("i", PurposeJoin, "t", RoleWorker, "hub a", "", "", "label")
			return err
		}},
		{"invalid UTF-8", func() error {
			_, err := newInvitationScope("i\xff", PurposeJoin, "t", RoleWorker, "hub-a", "", "", "label")
			return err
		}},
		{"unknown purpose", func() error {
			_, err := newInvitationScope("i", InvitationPurpose("admin"), "t", RoleWorker, "hub-a", "", "", "label")
			return err
		}},
	}
	for _, c := range bad {
		if err := c.make(); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: got %v, want ErrInvalid", c.name, err)
		}
	}
}
