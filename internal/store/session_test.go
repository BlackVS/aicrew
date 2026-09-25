package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// seedVerifiedLink stands in for verified proof integration, which does not
// exist yet (crew-core 2b). Only tests may create a link; no production path
// in this package sets one.
func seedVerifiedLink(t *testing.T, s *Store, agentID string) {
	t.Helper()
	if _, err := s.db.Exec(`UPDATE agents SET linked_hub_id = 'hub-test', linked_user_id = ? WHERE id = ?`,
		"user-"+agentID, agentID); err != nil {
		t.Fatalf("seed link: %v", err)
	}
}

func agentCaller(t *testing.T, agentID string) Caller {
	t.Helper()
	c, err := AgentCaller(agentID)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// member creates a linked agent that is a member of the team with the role.
func member(t *testing.T, s *Store, teamID, label string, role Role) (Agent, Caller) {
	t.Helper()
	a := mustAgent(t, s, "agent-"+label, label)
	seedVerifiedLink(t, s, a.ID)
	if _, err := s.AddMember(context.Background(), operator(t), "member-"+label, teamID, a.ID, role); err != nil {
		t.Fatalf("add member %s: %v", label, err)
	}
	return a, agentCaller(t, a.ID)
}

func TestStartSessionRequiresVerifiedLink(t *testing.T) {
	s, _ := openTemp(t)
	ctx := context.Background()
	tm := mustTeam(t, s, "t1", "crew")
	a := mustAgent(t, s, "a1", "builder")
	if _, err := s.AddMember(ctx, operator(t), "m1", tm.ID, a.ID, RoleWorker); err != nil {
		t.Fatal(err)
	}
	self := agentCaller(t, a.ID)
	if _, err := s.StartSession(ctx, self, "s1", tm.ID); !errors.Is(err, ErrIdentityLinkRequired) {
		t.Fatalf("unlinked start: got %v, want ErrIdentityLinkRequired", err)
	}
	if n := count(t, s, "sessions"); n != 0 {
		t.Fatalf("unlinked start created %d sessions", n)
	}
	seedVerifiedLink(t, s, a.ID)
	sess, err := s.StartSession(ctx, self, "s2", tm.ID)
	if err != nil || sess.Generation != 1 || sess.State != SessionActive || sess.Role != RoleWorker {
		t.Fatalf("linked start = %+v, %v", sess, err)
	}
}

func TestSessionCallerRules(t *testing.T) {
	s, _ := openTemp(t)
	ctx := context.Background()
	tm := mustTeam(t, s, "t1", "crew")
	_, worker := member(t, s, tm.ID, "worker", RoleWorker)
	other, otherCaller := member(t, s, tm.ID, "other", RoleWorker)
	outsider := mustAgent(t, s, "a-out", "outsider")
	seedVerifiedLink(t, s, outsider.ID)

	if _, err := s.StartSession(ctx, operator(t), "s0", tm.ID); !errors.Is(err, ErrForbidden) {
		t.Errorf("operator start: got %v, want ErrForbidden", err)
	}
	var zero Caller
	if _, err := s.StartSession(ctx, zero, "s0", tm.ID); !errors.Is(err, ErrForbidden) {
		t.Errorf("zero caller start: got %v, want ErrForbidden", err)
	}
	if _, err := s.StartSession(ctx, agentCaller(t, outsider.ID), "s0", tm.ID); !errors.Is(err, ErrForbidden) {
		t.Errorf("non-member start: got %v, want ErrForbidden", err)
	}
	sess, err := s.StartSession(ctx, worker, "s1", tm.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Heartbeat(ctx, otherCaller, "h1", sess.ID, sess.Generation); !errors.Is(err, ErrForbidden) {
		t.Errorf("heartbeat on another agent's session: got %v, want ErrForbidden", err)
	}
	if _, err := s.ResumeSession(ctx, otherCaller, "r1", sess.ID); !errors.Is(err, ErrForbidden) {
		t.Errorf("resume of another agent's session: got %v, want ErrForbidden", err)
	}
	if _, err := s.LeaveSession(ctx, otherCaller, "l1", sess.ID, sess.Generation); !errors.Is(err, ErrForbidden) {
		t.Errorf("leave of another agent's session: got %v, want ErrForbidden", err)
	}
	if _, err := s.StopSession(ctx, otherCaller, "x1", sess.ID); !errors.Is(err, ErrForbidden) {
		t.Errorf("agent stop: got %v, want ErrForbidden", err)
	}
	_ = other
}

func TestOneActiveSessionPerMembership(t *testing.T) {
	s, _ := openTemp(t)
	ctx := context.Background()
	tm := mustTeam(t, s, "t1", "crew")
	_, self := member(t, s, tm.ID, "worker", RoleWorker)

	first, err := s.StartSession(ctx, self, "s1", tm.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.StartSession(ctx, self, "s2", tm.ID); !errors.Is(err, ErrSessionActive) {
		t.Fatalf("second start: got %v, want ErrSessionActive", err)
	}
	left, err := s.LeaveSession(ctx, self, "l1", first.ID, first.Generation)
	if err != nil || left.State != SessionLeft || left.Generation != 2 {
		t.Fatalf("leave = %+v, %v", left, err)
	}
	second, err := s.StartSession(ctx, self, "s3", tm.ID)
	if err != nil || second.ID == first.ID || second.Generation != 1 {
		t.Fatalf("start after leave = %+v, %v", second, err)
	}
}

func TestStaleGenerationRefusedWithoutEffect(t *testing.T) {
	s, _ := openTemp(t)
	ctx := context.Background()
	tm := mustTeam(t, s, "t1", "crew")
	_, self := member(t, s, tm.ID, "worker", RoleWorker)
	sess, err := s.StartSession(ctx, self, "s1", tm.ID)
	if err != nil {
		t.Fatal(err)
	}
	resumed, err := s.ResumeSession(ctx, self, "r1", sess.ID)
	if err != nil || resumed.Generation != 2 {
		t.Fatalf("resume = %+v, %v", resumed, err)
	}
	auditBefore := count(t, s, "audit")
	s.now = func() time.Time { return time.Now().UTC().Add(time.Hour) }
	if _, err := s.Heartbeat(ctx, self, "h-stale", sess.ID, sess.Generation); !errors.Is(err, ErrContextStale) {
		t.Fatalf("stale heartbeat: got %v, want ErrContextStale", err)
	}
	if _, err := s.LeaveSession(ctx, self, "l-stale", sess.ID, sess.Generation); !errors.Is(err, ErrContextStale) {
		t.Fatalf("stale leave: got %v, want ErrContextStale", err)
	}
	after, err := s.GetSession(ctx, sess.ID)
	if err != nil || after.State != SessionActive || after.Generation != 2 || !after.LastSeenAt.Equal(resumed.LastSeenAt) {
		t.Fatalf("session changed by stale commands: %+v, %v", after, err)
	}
	if got := count(t, s, "audit"); got != auditBefore {
		t.Errorf("stale commands wrote audit rows: %d -> %d", auditBefore, got)
	}
}

// Replaying a session result is refused once the session has moved to a
// different generation, even though the receipt exists.
func TestReplayAfterGenerationChangeIsStale(t *testing.T) {
	s, _ := openTemp(t)
	ctx := context.Background()
	tm := mustTeam(t, s, "t1", "crew")
	_, self := member(t, s, tm.ID, "worker", RoleWorker)
	sess, err := s.StartSession(ctx, self, "s1", tm.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Heartbeat(ctx, self, "h1", sess.ID, 1); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Heartbeat(ctx, self, "h1", sess.ID, 1); err != nil {
		t.Fatalf("replay at same generation: %v", err)
	}
	if _, err := s.ResumeSession(ctx, self, "r1", sess.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Heartbeat(ctx, self, "h1", sess.ID, 1); !errors.Is(err, ErrContextStale) {
		t.Errorf("heartbeat replay after resume: got %v, want ErrContextStale", err)
	}
	if _, err := s.StartSession(ctx, self, "s1", tm.ID); !errors.Is(err, ErrContextStale) {
		t.Errorf("start replay after resume: got %v, want ErrContextStale", err)
	}
	// A final transition keeps its generation, so its own replay still
	// returns the recorded result (a lost reply to leave is recoverable).
	left, err := s.LeaveSession(ctx, self, "l1", sess.ID, 2)
	if err != nil {
		t.Fatal(err)
	}
	again, err := s.LeaveSession(ctx, self, "l1", sess.ID, 2)
	if err != nil || again.Generation != left.Generation || again.State != SessionLeft {
		t.Errorf("leave replay = %+v, %v", again, err)
	}
}

func TestConcurrentResumeGivesDistinctGenerations(t *testing.T) {
	s, _ := openTemp(t)
	ctx := context.Background()
	tm := mustTeam(t, s, "t1", "crew")
	_, self := member(t, s, tm.ID, "worker", RoleWorker)
	sess, err := s.StartSession(ctx, self, "s1", tm.ID)
	if err != nil {
		t.Fatal(err)
	}
	const n = 8
	var wg sync.WaitGroup
	gens := make([]int64, n)
	errs := make([]error, n)
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			var r Session
			r, errs[i] = s.ResumeSession(ctx, self, fmt.Sprintf("r%d", i), sess.ID)
			gens[i] = r.Generation
		}()
	}
	wg.Wait()
	seen := map[int64]bool{}
	for i, err := range errs {
		if err != nil {
			t.Fatalf("resume %d: %v", i, err)
		}
		if seen[gens[i]] || gens[i] < 2 || gens[i] > n+1 {
			t.Fatalf("generations %v are not distinct values in 2..%d", gens, n+1)
		}
		seen[gens[i]] = true
	}
	final, err := s.GetSession(ctx, sess.ID)
	if err != nil || final.Generation != n+1 {
		t.Fatalf("final generation = %d, %v; want %d", final.Generation, err, n+1)
	}
	for _, g := range gens {
		_, err := s.Heartbeat(ctx, self, fmt.Sprintf("h%d", g), sess.ID, g)
		if g == n+1 && err != nil {
			t.Errorf("heartbeat at current generation %d: %v", g, err)
		}
		if g != n+1 && !errors.Is(err, ErrContextStale) {
			t.Errorf("heartbeat at superseded generation %d: got %v, want ErrContextStale", g, err)
		}
	}
}

func TestConcurrentStartsHaveOneWinner(t *testing.T) {
	s, _ := openTemp(t)
	ctx := context.Background()
	tm := mustTeam(t, s, "t1", "crew")
	_, self := member(t, s, tm.ID, "worker", RoleWorker)
	callers := make([]Caller, 6)
	for i := range callers {
		_, callers[i] = member(t, s, tm.ID, fmt.Sprintf("coord-%d", i), RoleCoordinator)
	}

	const n = 6
	var wg sync.WaitGroup
	sameMember := make([]error, n)
	coordinators := make([]error, n)
	for i := range n {
		wg.Add(2)
		go func() {
			defer wg.Done()
			_, sameMember[i] = s.StartSession(ctx, self, fmt.Sprintf("w%d", i), tm.ID)
		}()
		go func() {
			defer wg.Done()
			_, coordinators[i] = s.StartSession(ctx, callers[i], "c", tm.ID)
		}()
	}
	wg.Wait()
	oneWinner := func(name string, errs []error, loser error) {
		t.Helper()
		wins := 0
		for _, err := range errs {
			switch {
			case err == nil:
				wins++
			case !errors.Is(err, loser):
				t.Errorf("%s: unexpected error %v", name, err)
			}
		}
		if wins != 1 {
			t.Errorf("%s: %d winners, want 1", name, wins)
		}
	}
	oneWinner("same membership", sameMember, ErrSessionActive)
	oneWinner("coordinators", coordinators, ErrCoordinatorActive)
}

func TestCoordinatorTakeover(t *testing.T) {
	s, _ := openTemp(t)
	ctx := context.Background()
	op := operator(t)
	tm := mustTeam(t, s, "t1", "crew")
	_, alice := member(t, s, tm.ID, "alice", RoleCoordinator)
	_, bob := member(t, s, tm.ID, "bob", RoleCoordinator)

	coordGen := func() int64 {
		t.Helper()
		team, err := s.GetTeam(ctx, tm.ID)
		if err != nil {
			t.Fatal(err)
		}
		return team.CoordinatorGeneration
	}

	a1, err := s.StartSession(ctx, alice, "a-start", tm.ID)
	if err != nil || a1.CoordinatorGeneration != 1 || coordGen() != 1 {
		t.Fatalf("alice start = %+v, %v, team gen %d", a1, err, coordGen())
	}
	if _, err := s.StartSession(ctx, bob, "b-start", tm.ID); !errors.Is(err, ErrCoordinatorActive) {
		t.Fatalf("bob start while alice active: got %v, want ErrCoordinatorActive", err)
	}
	// Takeover by the same agent: resume advances both generations.
	a2, err := s.ResumeSession(ctx, alice, "a-resume", a1.ID)
	if err != nil || a2.CoordinatorGeneration != 2 || a2.Generation != 2 || coordGen() != 2 {
		t.Fatalf("alice resume = %+v, %v, team gen %d", a2, err, coordGen())
	}
	// Takeover by another coordinator: operator stop, then start.
	stopped, err := s.StopSession(ctx, op, "stop-alice", a1.ID)
	if err != nil || stopped.State != SessionStopped || coordGen() != 3 {
		t.Fatalf("stop = %+v, %v, team gen %d", stopped, err, coordGen())
	}
	if _, err := s.Heartbeat(ctx, alice, "a-hb", a1.ID, a2.Generation); !errors.Is(err, ErrContextStale) {
		t.Errorf("stopped coordinator heartbeat: got %v, want ErrContextStale", err)
	}
	b1, err := s.StartSession(ctx, bob, "b-start-2", tm.ID)
	if err != nil || b1.CoordinatorGeneration != 4 || coordGen() != 4 {
		t.Fatalf("bob start after stop = %+v, %v, team gen %d", b1, err, coordGen())
	}
	if _, err := s.StopSession(ctx, op, "stop-alice-again", a1.ID); !errors.Is(err, ErrContextStale) {
		t.Errorf("stopping a stopped session: got %v, want ErrContextStale", err)
	}
}

func TestRoleChangeAndRemovalEndTheSession(t *testing.T) {
	s, _ := openTemp(t)
	ctx := context.Background()
	op := operator(t)
	tm := mustTeam(t, s, "t1", "crew")
	a, self := member(t, s, tm.ID, "worker", RoleWorker)

	sess, err := s.StartSession(ctx, self, "s1", tm.ID)
	if err != nil {
		t.Fatal(err)
	}
	m, err := s.SetMemberRole(ctx, op, "role-1", tm.ID, a.ID, 1, RoleCoordinator)
	if err != nil {
		t.Fatal(err)
	}
	ended, err := s.GetSession(ctx, sess.ID)
	if err != nil || ended.State != SessionEnded || ended.Generation != 2 {
		t.Fatalf("session after role change = %+v, %v", ended, err)
	}
	if _, err := s.Heartbeat(ctx, self, "h1", sess.ID, 1); !errors.Is(err, ErrContextStale) {
		t.Errorf("heartbeat after role change: got %v, want ErrContextStale", err)
	}
	// The member starts again under the new role.
	coord, err := s.StartSession(ctx, self, "s2", tm.ID)
	if err != nil || coord.Role != RoleCoordinator || coord.CoordinatorGeneration == 0 {
		t.Fatalf("start as coordinator = %+v, %v", coord, err)
	}
	if err := s.RemoveMember(ctx, op, "remove-1", tm.ID, a.ID, m.Revision); err != nil {
		t.Fatal(err)
	}
	removed, err := s.GetSession(ctx, coord.ID)
	if err != nil || removed.State != SessionEnded || removed.Generation != 2 {
		t.Fatalf("session after removal = %+v, %v", removed, err)
	}
	team, err := s.GetTeam(ctx, tm.ID)
	if err != nil || team.CoordinatorGeneration != coord.CoordinatorGeneration+1 {
		t.Errorf("coordinator generation after removal = %d, want %d", team.CoordinatorGeneration, coord.CoordinatorGeneration+1)
	}
	if _, err := s.StartSession(ctx, self, "s3", tm.ID); !errors.Is(err, ErrForbidden) {
		t.Errorf("start after removal: got %v, want ErrForbidden", err)
	}
}

// Liveness is informational: a long silence changes nothing, and no code
// path expires a session.
func TestLivenessLapseChangesNothing(t *testing.T) {
	s, _ := openTemp(t)
	ctx := context.Background()
	tm := mustTeam(t, s, "t1", "crew")
	_, self := member(t, s, tm.ID, "worker", RoleWorker)
	sess, err := s.StartSession(ctx, self, "s1", tm.ID)
	if err != nil {
		t.Fatal(err)
	}
	s.now = func() time.Time { return time.Now().UTC().Add(30 * 24 * time.Hour) }
	later, err := s.GetSession(ctx, sess.ID)
	if err != nil || later.State != SessionActive || later.Generation != 1 {
		t.Fatalf("session after a month of silence = %+v, %v", later, err)
	}
	hb, err := s.Heartbeat(ctx, self, "h1", sess.ID, 1)
	if err != nil || hb.Generation != 1 || !hb.LastSeenAt.After(sess.LastSeenAt) {
		t.Fatalf("heartbeat after silence = %+v, %v", hb, err)
	}
}

func TestInvalidUTF8Rejected(t *testing.T) {
	s, _ := openTemp(t)
	ctx := context.Background()
	op := operator(t)
	tm := mustTeam(t, s, "t1", "crew")
	_, self := member(t, s, tm.ID, "worker", RoleWorker)
	sess, err := s.StartSession(ctx, self, "s1", tm.ID)
	if err != nil {
		t.Fatal(err)
	}
	receipts := count(t, s, "receipts")

	cases := map[string]func() error{
		// These two collapse to the same JSON text if not rejected.
		`profile model \xff`: func() error {
			_, err := s.CreateAgent(ctx, op, "k1", NewAgent{Label: "x", Profile: Profile{Model: "\xff"}})
			return err
		},
		`profile model \xfe`: func() error {
			_, err := s.CreateAgent(ctx, op, "k1", NewAgent{Label: "x", Profile: Profile{Model: "\xfe"}})
			return err
		},
		"idempotency key": func() error {
			_, err := s.CreateAgent(ctx, op, "k\xff", NewAgent{Label: "x"})
			return err
		},
		"session id argument": func() error {
			_, err := s.Heartbeat(ctx, self, "h1", sess.ID+"\xff", 1)
			return err
		},
		"team id argument": func() error {
			_, err := s.SetTeamProjects(ctx, op, "p1", tm.ID+"\xfe", tm.Revision, nil)
			return err
		},
		"operator caller id": func() error {
			_, err := OperatorCaller("op\xff")
			return err
		},
		"agent caller id": func() error {
			_, err := AgentCaller("agent\xfe")
			return err
		},
	}
	for name, call := range cases {
		if err := call(); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: got %v, want ErrInvalid", name, err)
		}
	}
	if got := count(t, s, "receipts"); got != receipts {
		t.Errorf("invalid inputs stored receipts: %d -> %d", receipts, got)
	}
}

// Multi-statement reads must see one snapshot: a team's revision and its
// project list always belong together, even while a writer replaces them.
func TestTeamReadsAreConsistentSnapshots(t *testing.T) {
	s, _ := openTemp(t)
	ctx := context.Background()
	op := operator(t)
	tm := mustTeam(t, s, "t0", "crew", ProjectRef{HubID: "hub-a", ProjectID: "rev-1"})

	done := make(chan struct{})
	var writerErr error
	go func() {
		defer close(done)
		rev := tm.Revision
		for i := range 300 {
			next := rev + 1
			team, err := s.SetTeamProjects(ctx, op, fmt.Sprintf("w%d", i), tm.ID, rev,
				[]ProjectRef{{HubID: "hub-a", ProjectID: fmt.Sprintf("rev-%d", next)}})
			if err != nil {
				writerErr = err
				return
			}
			rev = team.Revision
		}
	}()

	var wg sync.WaitGroup
	var mu sync.Mutex
	mismatches := 0
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-done:
					return
				default:
				}
				team, err := s.GetTeam(ctx, tm.ID)
				if err != nil {
					t.Errorf("read: %v", err)
					return
				}
				want := fmt.Sprintf("rev-%d", team.Revision)
				if len(team.Projects) != 1 || team.Projects[0].ProjectID != want {
					mu.Lock()
					mismatches++
					mu.Unlock()
				}
			}
		}()
	}
	wg.Wait()
	if writerErr != nil {
		t.Fatalf("writer: %v", writerErr)
	}
	if mismatches > 0 {
		t.Fatalf("%d reads saw a revision with another revision's projects", mismatches)
	}
}

// A store created by the core-1 schema (v1) upgrades in place.
func TestMigratesVersion1Store(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "v1.db")
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	stmts := append([]string{`CREATE TABLE schema_version (version INTEGER NOT NULL)`}, schemaV1...)
	stmts = append(stmts,
		`INSERT INTO schema_version (version) VALUES (1)`,
		`INSERT INTO teams (id, name, revision, created_at, updated_at)
		 VALUES ('team-1', 'crew', 1, '2026-09-25T00:00:00.000000000Z', '2026-09-25T00:00:00.000000000Z')`)
	for _, stmt := range stmts {
		if _, err := raw.Exec(stmt); err != nil {
			t.Fatalf("build v1 store: %v", err)
		}
	}
	raw.Close()

	s, err := Open(ctx, path)
	if err != nil {
		t.Fatalf("open v1 store: %v", err)
	}
	defer s.Close()
	var version int
	if err := s.db.QueryRow(`SELECT version FROM schema_version`).Scan(&version); err != nil || version != schemaVersion {
		t.Fatalf("schema version = %d, %v", version, err)
	}
	team, err := s.GetTeam(ctx, "team-1")
	if err != nil || team.Name != "crew" || team.CoordinatorGeneration != 0 {
		t.Fatalf("team after migration = %+v, %v", team, err)
	}
	_, self := member(t, s, team.ID, "worker", RoleWorker)
	if _, err := s.StartSession(ctx, self, "s1", team.ID); err != nil {
		t.Fatalf("session on migrated store: %v", err)
	}
}
