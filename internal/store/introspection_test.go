package store

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

const testService = "aicrew-test"

// clock sets the store's clock to a movable time.
func clock(s *Store) *time.Time {
	now := time.Now().UTC().Truncate(time.Second)
	s.now = func() time.Time { return now }
	return &now
}

// secretsStored reports whether any audit or receipt row holds secret.
func secretStored(t *testing.T, s *Store, secret string) bool {
	t.Helper()
	var n int
	if err := s.db.QueryRow(`SELECT (SELECT COUNT(*) FROM audit WHERE input LIKE '%'||?||'%' OR result LIKE '%'||?||'%')
		+ (SELECT COUNT(*) FROM receipts WHERE result LIKE '%'||?||'%')`, secret, secret, secret).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n > 0
}

func TestIntrospectionCredentials(t *testing.T) {
	ctx := context.Background()
	s, _ := openTemp(t)
	now := clock(s)
	op := operator(t)
	agent := mustAgent(t, s, "a1", "someone")
	if _, _, err := s.IssueIntrospectionCredential(ctx, agentCaller(t, agent.ID), "k0", "hub-a"); !errors.Is(err, ErrForbidden) {
		t.Fatalf("issue by an agent: got %v, want ErrForbidden", err)
	}
	if _, _, err := s.IssueIntrospectionCredential(ctx, op, "k-bad", "hub a"); !errors.Is(err, ErrInvalid) {
		t.Fatalf("issue for a malformed hub: got %v, want ErrInvalid", err)
	}

	first, bearer, err := s.IssueIntrospectionCredential(ctx, op, "k1", "hub-a")
	if err != nil || !credentialShape.MatchString(bearer) || first.HubID != "hub-a" ||
		!first.ExpiresAt.Equal(now.Add(CredentialLifetime)) {
		t.Fatalf("issue = %+v, %q, %v", first, bearer, err)
	}
	if hub, err := s.AuthenticateIntrospection(ctx, bearer); err != nil || hub != "hub-a" {
		t.Fatalf("authenticate = %q, %v", hub, err)
	}
	if again, b, err := s.IssueIntrospectionCredential(ctx, op, "k1", "hub-a"); err != nil || again.ID != first.ID || b != "" {
		t.Fatalf("replay = %+v, bearer %q, %v; want the same credential and no bearer", again, b, err)
	}
	if secretStored(t, s, bearer) {
		t.Fatal("the bearer was recorded in the audit or a receipt")
	}
	second, bearer2, err := s.IssueIntrospectionCredential(ctx, op, "k2", "hub-a")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.IssueIntrospectionCredential(ctx, op, "k3", "hub-a"); !errors.Is(err, ErrCredentialLimit) {
		t.Fatalf("a third active credential: got %v, want ErrCredentialLimit", err)
	}
	if _, _, err := s.IssueIntrospectionCredential(ctx, op, "k-other-hub", "hub-b"); err != nil {
		t.Fatalf("the limit is per hub: %v", err)
	}

	revoked, err := s.RevokeIntrospectionCredential(ctx, op, "r1", first.ID)
	if err != nil || revoked.RevokedAt.IsZero() || revoked.Active(*now) {
		t.Fatalf("revoke = %+v, %v", revoked, err)
	}
	if _, err := s.AuthenticateIntrospection(ctx, bearer); !errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("a revoked credential: got %v, want ErrUnauthenticated", err)
	}
	*now = now.Add(time.Second)
	if again, err := s.RevokeIntrospectionCredential(ctx, op, "r1-again", first.ID); err != nil || !again.RevokedAt.Equal(revoked.RevokedAt) {
		t.Fatalf("revoking again = %+v, %v; want it unchanged", again, err)
	}
	if _, err := s.RevokeIntrospectionCredential(ctx, op, "r-missing", "no-such-id"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("revoking an unknown credential: got %v, want ErrNotFound", err)
	}
	if _, _, err := s.IssueIntrospectionCredential(ctx, op, "k4", "hub-a"); err != nil {
		t.Fatalf("issue after a revocation: %v", err)
	}

	// Expiry: the credential stops authenticating and no longer counts.
	*now = second.ExpiresAt
	if _, err := s.AuthenticateIntrospection(ctx, bearer2); !errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("an expired credential: got %v, want ErrUnauthenticated", err)
	}
	for _, b := range []string{"", "aicrew_introspect_", strings.Repeat("0", 64), credentialPrefix + strings.Repeat("0", 64)} {
		if _, err := s.AuthenticateIntrospection(ctx, b); !errors.Is(err, ErrUnauthenticated) {
			t.Fatalf("bearer %q: got %v, want ErrUnauthenticated", b, err)
		}
	}
	list, err := s.ListIntrospectionCredentials(ctx, op)
	if err != nil || len(list) != 4 {
		t.Fatalf("list = %d credentials, %v", len(list), err)
	}
	if _, err := s.ListIntrospectionCredentials(ctx, agentCaller(t, agent.ID)); !errors.Is(err, ErrForbidden) {
		t.Fatalf("list by an agent: got %v, want ErrForbidden", err)
	}
}

// introspectAs reads what aimem would learn about handle.
func introspectAs(t *testing.T, s *Store, handle, hub string) Introspection {
	t.Helper()
	got, err := s.Introspect(context.Background(), handle, hub, testService)
	if err != nil {
		t.Fatal(err)
	}
	return got
}

func TestSessionHandleIssueAndIntrospect(t *testing.T) {
	ctx := context.Background()
	s, _ := openTemp(t)
	tm := mustTeam(t, s, "t1", "crew")
	m := joinCrew(t, s, tm.ID, "builder", RoleWorker)
	other := joinCrew(t, s, tm.ID, "other", RoleWorker)
	now := clock(s)

	meta, handle, err := s.IssueSessionHandle(ctx, m.caller, "h1", m.sess.ID, m.sess.Generation, testService)
	if err != nil || !handleShape.MatchString(handle) || meta.HubID != "hub-test" || meta.ServiceID != testService ||
		!meta.ExpiresAt.Equal(now.Add(HandleLifetime)) {
		t.Fatalf("issue = %+v, %q, %v", meta, handle, err)
	}
	got := introspectAs(t, s, handle, "hub-test")
	want := Introspection{Active: true, UserID: "user-" + m.agent.ID, TokenID: "token-" + m.agent.ID, AgentID: m.agent.ID,
		TeamID: tm.ID, Role: RoleWorker, SessionID: m.sess.ID, Generation: m.sess.Generation, ExpiresAt: meta.ExpiresAt}
	if got != want {
		t.Fatalf("introspection = %+v, want %+v", got, want)
	}
	if b, h, err := s.IssueSessionHandle(ctx, m.caller, "h1", m.sess.ID, m.sess.Generation, testService); err != nil || h != "" || !b.IssuedAt.Equal(meta.IssuedAt) {
		t.Fatalf("replay = %+v, handle %q, %v; want the metadata and no handle", b, h, err)
	}
	if secretStored(t, s, handle) {
		t.Fatal("the handle was recorded in the audit or a receipt")
	}
	for name, h := range map[string]string{
		"another hub": "hub-other", "no hub": "",
	} {
		if got := introspectAs(t, s, handle, h); got.Active {
			t.Fatalf("%s: %+v, want inactive", name, got)
		}
	}
	if got, _ := s.Introspect(ctx, handle, "hub-test", "another-service"); got.Active {
		t.Fatalf("another service: %+v, want inactive", got)
	}
	for _, h := range []string{"", "acs1_", handle + "x", strings.Replace(handle, "acs1_", "acs2_", 1),
		"acs1_" + strings.Repeat("A", 43)} {
		if got := introspectAs(t, s, h, "hub-test"); got != (Introspection{}) {
			t.Fatalf("handle %q: %+v, want an empty inactive answer", h, got)
		}
	}

	// Only the session's own agent, at its current generation.
	if _, _, err := s.IssueSessionHandle(ctx, other.caller, "h-other", m.sess.ID, m.sess.Generation, testService); err == nil {
		t.Fatal("another agent issued a handle for the session")
	}
	if _, _, err := s.IssueSessionHandle(ctx, m.caller, "h-stale", m.sess.ID, m.sess.Generation+1, testService); !errors.Is(err, ErrContextStale) {
		t.Fatalf("issue at another generation: got %v, want ErrContextStale", err)
	}
	if _, _, err := s.IssueSessionHandle(ctx, m.caller, "h-svc", m.sess.ID, m.sess.Generation, "bad service"); !errors.Is(err, ErrInvalid) {
		t.Fatalf("issue for a malformed service: got %v, want ErrInvalid", err)
	}
	if _, _, err := s.IssueSessionHandle(ctx, operator(t), "h-op", m.sess.ID, m.sess.Generation, testService); !errors.Is(err, ErrForbidden) {
		t.Fatalf("issue by the operator: got %v, want ErrForbidden", err)
	}

	// Expiry, by aicrew's clock.
	*now = meta.ExpiresAt.Add(-time.Second)
	if !introspectAs(t, s, handle, "hub-test").Active {
		t.Fatal("inactive before expiry")
	}
	*now = meta.ExpiresAt
	if introspectAs(t, s, handle, "hub-test").Active {
		t.Fatal("active at expiry")
	}
}

// A refresh keeps the session and generation; the replaced handle stays
// active for at most the overlap, and never past its own expiry.
func TestSessionHandleRefreshOverlap(t *testing.T) {
	ctx := context.Background()
	s, _ := openTemp(t)
	tm := mustTeam(t, s, "t1", "crew")
	m := joinCrew(t, s, tm.ID, "builder", RoleWorker)
	now := clock(s)
	issue := func(key string) (SessionHandle, string) {
		t.Helper()
		meta, h, err := s.IssueSessionHandle(ctx, m.caller, key, m.sess.ID, m.sess.Generation, testService)
		if err != nil {
			t.Fatal(err)
		}
		return meta, h
	}
	active := func(h string) bool { return introspectAs(t, s, h, "hub-test").Active }

	t0 := *now
	_, h1 := issue("h1")
	*now = t0.Add(time.Minute)
	m2, h2 := issue("h2")
	if m2.Generation != m.sess.Generation || m2.SessionID != m.sess.ID {
		t.Fatalf("refresh changed the session: %+v", m2)
	}
	if got := introspectAs(t, s, h1, "hub-test"); !got.Active || !got.ExpiresAt.Equal(t0.Add(time.Minute+HandleOverlap)) {
		t.Fatalf("replaced handle = %+v; want active until the overlap ends", got)
	}
	*now = t0.Add(time.Minute + HandleOverlap - time.Second)
	if !active(h1) || !active(h2) {
		t.Fatal("both handles should be active inside the overlap")
	}
	*now = t0.Add(time.Minute + HandleOverlap)
	if active(h1) || !active(h2) {
		t.Fatal("the replaced handle outlived the overlap")
	}

	// The overlap never extends a handle past its own expiry.
	t1 := *now
	_, h3 := issue("h3")
	*now = t1.Add(HandleLifetime - 30*time.Second)
	_, h4 := issue("h4")
	if got := introspectAs(t, s, h3, "hub-test"); !got.Active || !got.ExpiresAt.Equal(t1.Add(HandleLifetime)) {
		t.Fatalf("replaced handle near expiry = %+v; want its own expiry", got)
	}
	*now = t1.Add(HandleLifetime)
	if active(h3) || !active(h4) {
		t.Fatal("the overlap extended a handle past its own expiry")
	}
}

// Every change of generation and every end of the session makes the
// session's handles inactive; a new handle is issued for the new generation.
func TestSessionHandleRevocation(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name   string
		change func(t *testing.T, s *Store, tm Team, m crewMember) crewMember
	}{
		{"resume", func(t *testing.T, s *Store, _ Team, m crewMember) crewMember {
			sess, err := s.ResumeSession(ctx, m.caller, "resume", m.sess.ID)
			if err != nil {
				t.Fatal(err)
			}
			m.sess = sess
			return m
		}},
		{"leave", func(t *testing.T, s *Store, _ Team, m crewMember) crewMember {
			if _, err := s.LeaveSession(ctx, m.caller, "leave", m.sess.ID, m.sess.Generation); err != nil {
				t.Fatal(err)
			}
			return m
		}},
		{"operator stop", func(t *testing.T, s *Store, _ Team, m crewMember) crewMember {
			if _, err := s.StopSession(ctx, operator(t), "stop", m.sess.ID); err != nil {
				t.Fatal(err)
			}
			return m
		}},
		{"role change", func(t *testing.T, s *Store, tm Team, m crewMember) crewMember {
			ms, err := getMembership(ctx, s.rdb, tm.ID, m.agent.ID)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := s.SetMemberRole(ctx, operator(t), "role", tm.ID, m.agent.ID, ms.Revision, RoleIndependent); err != nil {
				t.Fatal(err)
			}
			return m
		}},
		{"removal", func(t *testing.T, s *Store, tm Team, m crewMember) crewMember {
			ms, err := getMembership(ctx, s.rdb, tm.ID, m.agent.ID)
			if err != nil {
				t.Fatal(err)
			}
			if err := s.RemoveMember(ctx, operator(t), "remove", tm.ID, m.agent.ID, ms.Revision); err != nil {
				t.Fatal(err)
			}
			return m
		}},
		{"relinked to another hub", func(t *testing.T, s *Store, _ Team, m crewMember) crewMember {
			if _, err := s.db.Exec(`UPDATE agents SET linked_hub_id = 'hub-other' WHERE id = ?`, m.agent.ID); err != nil {
				t.Fatal(err)
			}
			return m
		}},
		{"credential rotated", func(t *testing.T, s *Store, _ Team, m crewMember) crewMember {
			if _, err := s.db.Exec(`UPDATE agents SET linked_token_id = 'token-rotated' WHERE id = ?`, m.agent.ID); err != nil {
				t.Fatal(err)
			}
			return m
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, _ := openTemp(t)
			tm := mustTeam(t, s, "t1", "crew")
			m := joinCrew(t, s, tm.ID, "builder", RoleWorker)
			_, h, err := s.IssueSessionHandle(ctx, m.caller, "h1", m.sess.ID, m.sess.Generation, testService)
			if err != nil {
				t.Fatal(err)
			}
			if !introspectAs(t, s, h, "hub-test").Active {
				t.Fatal("inactive before the change")
			}
			after := tc.change(t, s, tm, m)
			if got := introspectAs(t, s, h, "hub-test"); got.Active {
				t.Fatalf("after %s: %+v, want inactive", tc.name, got)
			}
			// Asked about the new hub, the handle bound to the old one is
			// still inactive.
			if got := introspectAs(t, s, h, "hub-other"); got.Active {
				t.Fatalf("after %s, asked for hub-other: %+v, want inactive", tc.name, got)
			}
			if tc.name == "resume" {
				_, h2, err := s.IssueSessionHandle(ctx, after.caller, "h2", after.sess.ID, after.sess.Generation, testService)
				if err != nil {
					t.Fatal(err)
				}
				if got := introspectAs(t, s, h2, "hub-test"); !got.Active || got.Generation != after.sess.Generation {
					t.Fatalf("new handle after resume = %+v", got)
				}
			}
			if tc.name == "credential rotated" {
				if _, _, err := s.IssueSessionHandle(ctx, m.caller, "h3", m.sess.ID, m.sess.Generation, testService); !errors.Is(err, ErrContextStale) {
					t.Fatalf("issue after an unapplied rotation: got %v, want ErrContextStale", err)
				}
			}
		})
	}
}

// The store keeps a handle's digest only, and the schema refuses a handle at
// no generation.
func TestSessionHandleStorage(t *testing.T) {
	ctx := context.Background()
	s, _ := openTemp(t)
	tm := mustTeam(t, s, "t1", "crew")
	m := joinCrew(t, s, tm.ID, "builder", RoleWorker)
	_, h, err := s.IssueSessionHandle(ctx, m.caller, "h1", m.sess.ID, m.sess.Generation, testService)
	if err != nil {
		t.Fatal(err)
	}
	var stored int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM session_handles WHERE digest = ?`, secretDigest(h)).Scan(&stored); err != nil || stored != 1 {
		t.Fatalf("stored digests = %d, %v", stored, err)
	}
	var plain int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM session_handles WHERE digest = ?`, h).Scan(&plain); err != nil || plain != 0 {
		t.Fatal("the handle itself was stored")
	}
	if _, err := s.db.Exec(`UPDATE session_handles SET generation = 0`); err == nil {
		t.Fatal("the schema accepted a handle at generation 0")
	}
}
