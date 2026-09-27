package store

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

// prover makes agent challenges and matching aimem receipts for an agent
// whose link seedVerifiedLink set (hub-test, user-<id>, token-<id>).
type prover struct {
	t *testing.T
	s *Store
	v *fakeVerifier
	n int
}

func newProver(t *testing.T, s *Store) *prover {
	return &prover{t: t, s: s, v: newFakeVerifier()}
}

// proof issues a challenge for the agent and a receipt for token.
func (p *prover) proof(agentID, token string) (Challenge, Secret) {
	p.t.Helper()
	p.n++
	ch, err := p.s.IssueAgentChallenge(context.Background(), Caller{}, "challenge-"+strings.Repeat("x", p.n), agentID)
	if err != nil {
		p.t.Fatalf("challenge: %v", err)
	}
	receipt := p.v.receipt("receipt-"+ch.ID, "hub-test", "user-"+agentID, token)
	return ch, receipt
}

func (p *prover) enter(key string, a Agent, teamID string) (SessionEntry, EntrySecrets) {
	p.t.Helper()
	ch, receipt := p.proof(a.ID, "token-"+a.ID)
	entry, sec, err := p.s.EnterSession(context.Background(), p.v, key, ch.ID, receipt, teamID, testService)
	if err != nil {
		p.t.Fatalf("enter %s: %v", key, err)
	}
	return entry, sec
}

func mustBind(t *testing.T, s *Store, token Secret) TokenBinding {
	t.Helper()
	b, err := s.AuthenticateSessionToken(context.Background(), token.Reveal())
	if err != nil {
		t.Fatalf("authenticate token: %v", err)
	}
	return b
}

func tokenDead(t *testing.T, s *Store, token Secret, why string) {
	t.Helper()
	if _, err := s.AuthenticateSessionToken(context.Background(), token.Reveal()); !errors.Is(err, ErrTokenInvalid) {
		t.Fatalf("%s: token authenticates (%v)", why, err)
	}
}

func handleActive(t *testing.T, s *Store, handle Secret) bool {
	t.Helper()
	return introspectAs(t, s, handle.Reveal(), "hub-test").Active
}

func sessionCount(t *testing.T, s *Store, agentID string) int {
	t.Helper()
	var n int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM sessions WHERE agent_id = ?`, agentID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestEnterSessionIssuesTokenAndHandle(t *testing.T) {
	ctx := context.Background()
	s, _ := openTemp(t)
	now := clock(s)
	tm := mustTeam(t, s, "t1", "crew")
	a, _ := member(t, s, tm.ID, "builder", RoleWorker)
	p := newProver(t, s)
	ch, receipt := p.proof(a.ID, "token-"+a.ID)

	entry, sec, err := s.EnterSession(ctx, p.v, "enter-1", ch.ID, receipt, tm.ID, testService)
	if err != nil {
		t.Fatal(err)
	}
	if entry.Session.State != SessionActive || entry.Session.AgentID != a.ID || entry.Session.Generation != 1 || entry.Rotated {
		t.Fatalf("entry = %+v", entry)
	}
	if !tokenShape.MatchString(sec.Token.Reveal()) || !handleShape.MatchString(sec.Handle.Reveal()) {
		t.Fatal("malformed token or handle")
	}
	if TokenLifetime != 8*time.Hour || !entry.Token.ExpiresAt.Equal(now.Add(8*time.Hour)) || !entry.Handle.ExpiresAt.Equal(now.Add(HandleLifetime)) {
		t.Fatalf("expiry: token %v, handle %v", entry.Token.ExpiresAt, entry.Handle.ExpiresAt)
	}
	want := TokenBinding{AgentID: a.ID, TeamID: tm.ID, SessionID: entry.Session.ID, Generation: 1, Role: RoleWorker,
		HubID: "hub-test", UserID: "user-" + a.ID, TokenID: "token-" + a.ID, ExpiresAt: now.Add(TokenLifetime)}
	if got := mustBind(t, s, sec.Token); got != want {
		t.Fatalf("binding = %+v, want %+v", got, want)
	}
	if !handleActive(t, s, sec.Handle) {
		t.Fatal("the entry's handle is not active")
	}
	// The challenge is consumed: a second entry with it is refused.
	if _, _, err := s.EnterSession(ctx, p.v, "enter-2", ch.ID, receipt, tm.ID, testService); !errors.Is(err, ErrChallengeInvalid) {
		t.Fatalf("second entry with a consumed challenge: %v", err)
	}
	for name, secret := range map[string]string{"token": sec.Token.Reveal(), "handle": sec.Handle.Reveal(), "receipt": receipt.Reveal()} {
		if secretStored(t, s, secret) {
			t.Fatalf("the %s was recorded in the audit or a receipt", name)
		}
	}
	if strings.Contains(sec.Token.String()+sec.Handle.String(), "ast1_") {
		t.Fatal("formatting the secrets shows them")
	}
}

// Nothing changes when an entry is refused, and aimem is not asked about an
// entry that cannot succeed.
func TestEntryRefusals(t *testing.T) {
	ctx := context.Background()
	s, _ := openTemp(t)
	tm := mustTeam(t, s, "t1", "crew")
	other := mustTeam(t, s, "t2", "other")
	a, _ := member(t, s, tm.ID, "builder", RoleWorker)
	busy := joinCrew(t, s, tm.ID, "busy", RoleWorker)
	p := newProver(t, s)

	ch, receipt := p.proof(a.ID, "token-"+a.ID)
	if _, _, err := s.EnterSession(ctx, p.v, "k", ch.ID, receipt, other.ID, testService); !errors.Is(err, ErrForbidden) {
		t.Fatalf("entry into a team the agent is not in: %v", err)
	}
	ch2, receipt2 := p.proof(busy.agent.ID, "token-"+busy.agent.ID)
	if _, _, err := s.EnterSession(ctx, p.v, "k", ch2.ID, receipt2, tm.ID, testService); !errors.Is(err, ErrSessionActive) {
		t.Fatalf("entry beside an active session: %v", err)
	}
	if _, _, err := s.EnterSession(ctx, p.v, "k", ch.ID, receipt, tm.ID, "bad service"); !errors.Is(err, ErrInvalid) {
		t.Fatalf("malformed service ID: %v", err)
	}
	if p.v.calls != 0 {
		t.Fatalf("aimem was asked %d times about entries that could not succeed", p.v.calls)
	}

	// Another user's proof, and an unavailable aimem, change nothing.
	wrong := p.v.receipt("receipt-wrong", "hub-test", "user-other", "token-other")
	if _, _, err := s.EnterSession(ctx, p.v, "k-wrong", ch.ID, wrong, tm.ID, testService); !errors.Is(err, ErrIdentityMismatch) {
		t.Fatalf("another user's proof: %v", err)
	}
	p.v.fail = errAimemUnavailable
	if _, _, err := s.EnterSession(ctx, p.v, "k-down", ch.ID, receipt, tm.ID, testService); !errors.Is(err, errAimemUnavailable) {
		t.Fatalf("aimem unavailable: %v", err)
	}
	p.v.fail = nil
	if n := sessionCount(t, s, a.ID); n != 0 {
		t.Fatalf("refused entries created %d sessions", n)
	}
	// The challenge is still usable.
	if _, _, err := s.EnterSession(ctx, p.v, "k-ok", ch.ID, receipt, tm.ID, testService); err != nil {
		t.Fatalf("entry after refusals: %v", err)
	}
}

// A proof with a new credential for the same user is a rotation.
func TestEnterSessionRotates(t *testing.T) {
	ctx := context.Background()
	s, _ := openTemp(t)
	tm := mustTeam(t, s, "t1", "crew")
	a, _ := member(t, s, tm.ID, "builder", RoleWorker)
	p := newProver(t, s)
	ch, receipt := p.proof(a.ID, "token-rotated")
	entry, sec, err := s.EnterSession(ctx, p.v, "enter", ch.ID, receipt, tm.ID, testService)
	if err != nil || !entry.Rotated || entry.Session.TokenID != "token-rotated" {
		t.Fatalf("entry = %+v, %v", entry, err)
	}
	if b := mustBind(t, s, sec.Token); b.TokenID != "token-rotated" {
		t.Fatalf("binding = %+v", b)
	}
}

func TestResumeSessionWithProof(t *testing.T) {
	ctx := context.Background()
	s, _ := openTemp(t)
	tm := mustTeam(t, s, "t1", "crew")
	a, _ := member(t, s, tm.ID, "builder", RoleWorker)
	b, _ := member(t, s, tm.ID, "tester", RoleWorker)
	p := newProver(t, s)
	first, sec1 := p.enter("enter", a, tm.ID)

	ch, receipt := p.proof(a.ID, "token-"+a.ID)
	resumed, sec2, err := s.ResumeSessionWithProof(ctx, p.v, "resume", ch.ID, receipt, first.Session.ID, testService)
	if err != nil {
		t.Fatal(err)
	}
	if resumed.Session.ID != first.Session.ID || resumed.Session.Generation != first.Session.Generation+1 {
		t.Fatalf("resumed = %+v", resumed.Session)
	}
	tokenDead(t, s, sec1.Token, "the token of the fenced generation")
	if _, _, err := s.RefreshHandle(ctx, "refresh-fenced", sec1.Token.Reveal(), testService); !errors.Is(err, ErrTokenInvalid) {
		t.Fatalf("refresh with the fenced generation's token: %v", err)
	}
	if _, err := s.LeaveWithToken(ctx, "leave-fenced", sec1.Token.Reveal()); !errors.Is(err, ErrTokenInvalid) {
		t.Fatalf("leave with the fenced generation's token: %v", err)
	}
	if handleActive(t, s, sec1.Handle) {
		t.Fatal("the handle of the fenced generation is active")
	}
	if got := mustBind(t, s, sec2.Token); got.Generation != resumed.Session.Generation {
		t.Fatalf("binding = %+v", got)
	}

	// Another agent's session, or an ended one, cannot be resumed.
	chB, receiptB := p.proof(b.ID, "token-"+b.ID)
	if _, _, err := s.ResumeSessionWithProof(ctx, p.v, "steal", chB.ID, receiptB, first.Session.ID, testService); !errors.Is(err, ErrForbidden) {
		t.Fatalf("resume of another agent's session: %v", err)
	}
	if _, err := s.LeaveWithToken(ctx, "leave", sec2.Token.Reveal()); err != nil {
		t.Fatal(err)
	}
	ch3, receipt3 := p.proof(a.ID, "token-"+a.ID)
	if _, _, err := s.ResumeSessionWithProof(ctx, p.v, "late", ch3.ID, receipt3, first.Session.ID, testService); !errors.Is(err, ErrContextStale) {
		t.Fatalf("resume of a left session: %v", err)
	}
}

// D-1b: a lost entry reply is recovered by the same key and input within the
// challenge's window, with fresh secrets and without a second session.
func TestLostEntryReply(t *testing.T) {
	ctx := context.Background()
	s, _ := openTemp(t)
	now := clock(s)
	tm := mustTeam(t, s, "t1", "crew")
	a, _ := member(t, s, tm.ID, "builder", RoleWorker)
	p := newProver(t, s)
	ch, receipt := p.proof(a.ID, "token-"+a.ID)
	enter := func(key string, r Secret, service string) (SessionEntry, EntrySecrets, error) {
		return s.EnterSession(ctx, p.v, key, ch.ID, r, tm.ID, service)
	}
	first, lost, err := enter("enter", receipt, testService)
	if err != nil {
		t.Fatal(err)
	}
	*now = now.Add(time.Minute)

	again, fresh, err := enter("enter", receipt, testService)
	if err != nil {
		t.Fatalf("retry: %v", err)
	}
	if again.Session.ID != first.Session.ID || again.Session.Generation != first.Session.Generation ||
		!again.Token.ExpiresAt.Equal(first.Token.ExpiresAt) {
		t.Fatalf("retry = %+v, want the recorded session and ceiling", again)
	}
	if fresh.Token.Reveal() == lost.Token.Reveal() || fresh.Handle.Reveal() == lost.Handle.Reveal() {
		t.Fatal("the retry returned the lost secrets")
	}
	tokenDead(t, s, lost.Token, "the lost reply's token")
	if handleActive(t, s, lost.Handle) {
		t.Fatal("the lost reply's handle is active")
	}
	mustBind(t, s, fresh.Token)
	if !handleActive(t, s, fresh.Handle) {
		t.Fatal("the reissued handle is not active")
	}
	if n := sessionCount(t, s, a.ID); n != 1 || p.v.calls != 1 {
		t.Fatalf("%d sessions, %d aimem calls; want 1 and 1", n, p.v.calls)
	}
	var reissues int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM audit WHERE operation = ?`, opReissueEntry).Scan(&reissues); err != nil || reissues != 1 {
		t.Fatalf("reissue audit rows = %d, %v", reissues, err)
	}
	if secretStored(t, s, fresh.Token.Reveal()) || secretStored(t, s, fresh.Handle.Reveal()) {
		t.Fatal("a reissued secret was recorded")
	}

	// Any change of input under the same key is a conflict and changes nothing.
	other := p.v.receipt("receipt-other", "hub-test", "user-"+a.ID, "token-"+a.ID)
	if _, _, err := enter("enter", other, testService); !errors.Is(err, ErrIdempotencyConflict) {
		t.Fatalf("retry with another receipt: %v", err)
	}
	if _, _, err := enter("enter", receipt, "aicrew-other"); !errors.Is(err, ErrIdempotencyConflict) {
		t.Fatalf("retry with another service: %v", err)
	}
	if _, _, err := s.ResumeSessionWithProof(ctx, p.v, "enter", ch.ID, receipt, first.Session.ID, testService); !errors.Is(err, ErrChallengeInvalid) {
		t.Fatalf("the same key for another operation: %v", err)
	}
	mustBind(t, s, fresh.Token)

	// After the challenge's window the retry is refused, and the current
	// token stays valid.
	*now = ch.ExpiresAt
	if _, _, err := enter("enter", receipt, testService); !errors.Is(err, ErrChallengeInvalid) {
		t.Fatalf("retry after the window: %v", err)
	}
	mustBind(t, s, fresh.Token)
	if n := sessionCount(t, s, a.ID); n != 1 {
		t.Fatalf("%d sessions", n)
	}
}

// A retry after the session's generation moved on is stale.
func TestLostEntryReplyAfterGenerationChange(t *testing.T) {
	ctx := context.Background()
	s, _ := openTemp(t)
	tm := mustTeam(t, s, "t1", "crew")
	a, self := member(t, s, tm.ID, "builder", RoleWorker)
	p := newProver(t, s)
	ch, receipt := p.proof(a.ID, "token-"+a.ID)
	first, _, err := s.EnterSession(ctx, p.v, "enter", ch.ID, receipt, tm.ID, testService)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.ResumeSession(ctx, self, "resume", first.Session.ID); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.EnterSession(ctx, p.v, "enter", ch.ID, receipt, tm.ID, testService); !errors.Is(err, ErrContextStale) {
		t.Fatalf("retry after a resume: %v", err)
	}
}

// Identical entries in flight together start one session.
func TestConcurrentEntries(t *testing.T) {
	ctx := context.Background()
	s, _ := openTemp(t)
	tm := mustTeam(t, s, "t1", "crew")
	a, _ := member(t, s, tm.ID, "builder", RoleWorker)
	p := newProver(t, s)
	ch, receipt := p.proof(a.ID, "token-"+a.ID)
	var wg sync.WaitGroup
	errs := make([]error, 6)
	for i := range errs {
		key := "same"
		if i%2 == 1 {
			key = "other-" + strings.Repeat("k", i)
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _, errs[i] = s.EnterSession(ctx, p.v, key, ch.ID, receipt, tm.ID, testService)
		}()
	}
	wg.Wait()
	ok := 0
	for _, err := range errs {
		switch {
		case err == nil:
			ok++
		case errors.Is(err, ErrChallengeInvalid), errors.Is(err, ErrSessionActive):
		default:
			t.Fatalf("unexpected error: %v", err)
		}
	}
	if ok == 0 || sessionCount(t, s, a.ID) != 1 {
		t.Fatalf("%d entries succeeded, %d sessions", ok, sessionCount(t, s, a.ID))
	}
}

// A token dies with its session, a generation change and its ceiling; a
// handle issued under it never outlives it.
func TestTokenDeath(t *testing.T) {
	ctx := context.Background()
	s, _ := openTemp(t)
	now := clock(s)
	tm := mustTeam(t, s, "t1", "crew")
	p := newProver(t, s)

	a, _ := member(t, s, tm.ID, "expiring", RoleWorker)
	entry, sec := p.enter("e-expiring", a, tm.ID)
	*now = entry.Token.ExpiresAt.Add(-5 * time.Minute)
	h, _, err := s.RefreshHandle(ctx, "r", sec.Token.Reveal(), testService)
	if err != nil || !h.ExpiresAt.Equal(entry.Token.ExpiresAt) {
		t.Fatalf("refresh near the ceiling = %+v, %v; want the handle capped at the token's expiry", h, err)
	}
	*now = entry.Token.ExpiresAt
	tokenDead(t, s, sec.Token, "at the ceiling")
	*now = entry.Token.ExpiresAt.Add(-8 * time.Hour)

	b, _ := member(t, s, tm.ID, "stopped", RoleWorker)
	entry, sec = p.enter("e-stopped", b, tm.ID)
	if _, err := s.StopSession(ctx, operator(t), "stop", entry.Session.ID); err != nil {
		t.Fatal(err)
	}
	tokenDead(t, s, sec.Token, "after an operator stop")

	c, _ := member(t, s, tm.ID, "removed", RoleWorker)
	_, sec = p.enter("e-removed", c, tm.ID)
	m, err := getMembership(ctx, s.rdb, tm.ID, c.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.RemoveMember(ctx, operator(t), "remove", tm.ID, c.ID, m.Revision); err != nil {
		t.Fatal(err)
	}
	tokenDead(t, s, sec.Token, "after removal")

	d, _ := member(t, s, tm.ID, "rotated", RoleWorker)
	_, sec = p.enter("e-rotated", d, tm.ID)
	ch, receipt := p.proof(d.ID, "token-new")
	if _, err := s.CompleteAgentProof(ctx, Caller{}, p.v, "reproof", ch.ID, receipt); err != nil {
		t.Fatal(err)
	}
	tokenDead(t, s, sec.Token, "after a rotation")
}

func TestRefreshHandle(t *testing.T) {
	ctx := context.Background()
	s, _ := openTemp(t)
	now := clock(s)
	tm := mustTeam(t, s, "t1", "crew")
	a, _ := member(t, s, tm.ID, "builder", RoleWorker)
	p := newProver(t, s)
	entry, sec := p.enter("enter", a, tm.ID)

	*now = now.Add(10 * time.Minute)
	h, handle, err := s.RefreshHandle(ctx, "r1", sec.Token.Reveal(), testService)
	if err != nil || !handleShape.MatchString(handle) || h.SessionID != entry.Session.ID {
		t.Fatalf("refresh = %+v, %v", h, err)
	}
	if !introspectAs(t, s, handle, "hub-test").Active || !handleActive(t, s, sec.Handle) {
		t.Fatal("new handle inactive, or the old one lost its overlap")
	}
	*now = now.Add(HandleOverlap)
	if handleActive(t, s, sec.Handle) {
		t.Fatal("the replaced handle outlived its overlap")
	}
	if replay, again, err := s.RefreshHandle(ctx, "r1", sec.Token.Reveal(), testService); err != nil || again != "" || !replay.IssuedAt.Equal(h.IssuedAt) {
		t.Fatalf("replay = %+v, %q, %v; want the metadata and no handle", replay, again, err)
	}
	if secretStored(t, s, handle) {
		t.Fatal("the refreshed handle was recorded")
	}
	*now = entry.Token.ExpiresAt
	if _, _, err := s.RefreshHandle(ctx, "r1", sec.Token.Reveal(), testService); !errors.Is(err, ErrTokenInvalid) {
		t.Fatalf("replay with the expired token: %v", err)
	}
	*now = entry.Token.ExpiresAt.Add(-time.Hour)
	if _, _, err := s.RefreshHandle(ctx, "r2", sec.Token.Reveal(), "bad service"); !errors.Is(err, ErrInvalid) {
		t.Fatalf("malformed service: %v", err)
	}
}

// Tokens, handles and introspection credentials are never interchangeable.
func TestTokenSeparation(t *testing.T) {
	ctx := context.Background()
	s, _ := openTemp(t)
	tm := mustTeam(t, s, "t1", "crew")
	a, _ := member(t, s, tm.ID, "builder", RoleWorker)
	p := newProver(t, s)
	_, sec := p.enter("enter", a, tm.ID)
	_, bearer, err := s.IssueIntrospectionCredential(ctx, operator(t), "cred", "hub-test")
	if err != nil {
		t.Fatal(err)
	}
	token, handle := sec.Token.Reveal(), sec.Handle.Reveal()
	// Same random part, other prefix: the prefix alone must not decide.
	swapped := handlePrefix + strings.TrimPrefix(token, tokenPrefix)
	for name, v := range map[string]string{"handle": handle, "introspection credential": bearer, "re-prefixed token": swapped} {
		if _, err := s.AuthenticateSessionToken(ctx, v); !errors.Is(err, ErrTokenInvalid) {
			t.Errorf("%s authenticates as a token: %v", name, err)
		}
		if _, _, err := s.RefreshHandle(ctx, "r-"+name, v, testService); !errors.Is(err, ErrTokenInvalid) {
			t.Errorf("%s refreshes a handle: %v", name, err)
		}
		if _, err := s.LeaveWithToken(ctx, "l-"+name, v); !errors.Is(err, ErrTokenInvalid) {
			t.Errorf("%s leaves a session: %v", name, err)
		}
	}
	if introspectAs(t, s, token, "hub-test").Active {
		t.Error("a token introspects as a handle")
	}
	if _, err := s.AuthenticateIntrospection(ctx, token); !errors.Is(err, ErrUnauthenticated) {
		t.Errorf("a token authenticates as the introspection credential: %v", err)
	}
	mustBind(t, s, sec.Token)
}

func TestLeaveWithToken(t *testing.T) {
	ctx := context.Background()
	s, _ := openTemp(t)
	e := newExecTeam(t, s)
	e.offer(t, "o1", "task-1") // the coordinator's open offer
	p := newProver(t, s)
	ch, receipt := p.proof(e.lead.agent.ID, "token-"+e.lead.agent.ID)
	lead, sec, err := s.ResumeSessionWithProof(ctx, p.v, "resume", ch.ID, receipt, e.lead.sess.ID, testService)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.LeaveWithToken(ctx, "leave", sec.Token.Reveal()); !errors.Is(err, ErrWorkOutstanding) {
		t.Fatalf("leave with an open offer: %v", err)
	}
	mustBind(t, s, sec.Token)

	tm2 := mustTeam(t, s, "t2", "quiet")
	a, _ := member(t, s, tm2.ID, "quiet", RoleWorker)
	entry, sec2 := p.enter("enter", a, tm2.ID)
	left, err := s.LeaveWithToken(ctx, "leave", sec2.Token.Reveal())
	if err != nil || left.State != SessionLeft || left.ID != entry.Session.ID {
		t.Fatalf("leave = %+v, %v", left, err)
	}
	tokenDead(t, s, sec2.Token, "after leave")
	if handleActive(t, s, sec2.Handle) {
		t.Fatal("a handle of the left session is active")
	}
	// A lost leave reply is replayed with the same key; a new key is refused.
	if again, err := s.LeaveWithToken(ctx, "leave", sec2.Token.Reveal()); err != nil || again != left {
		t.Fatalf("replay = %+v, %v", again, err)
	}
	if _, err := s.LeaveWithToken(ctx, "leave-again", sec2.Token.Reveal()); !errors.Is(err, ErrTokenInvalid) {
		t.Fatalf("a new leave with the dead token: %v", err)
	}
	if _, _, err := s.RefreshHandle(ctx, "refresh-left", sec2.Token.Reveal(), testService); !errors.Is(err, ErrTokenInvalid) {
		t.Fatalf("refresh with the left session's token: %v", err)
	}
	_ = lead
}
