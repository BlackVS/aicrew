package store

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"testing"
)

func openTemp(t *testing.T) (*Store, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "aicrew.db")
	s, err := Open(context.Background(), path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s, path
}

func operator(t *testing.T) Caller {
	t.Helper()
	c, err := OperatorCaller("op-1")
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func count(t *testing.T, s *Store, table string) int {
	t.Helper()
	var n int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM ` + table).Scan(&n); err != nil {
		t.Fatalf("count %s: %v", table, err)
	}
	return n
}

func mustAgent(t *testing.T, s *Store, key, label string) Agent {
	t.Helper()
	a, err := s.CreateAgent(context.Background(), operator(t), key, NewAgent{Label: label})
	if err != nil {
		t.Fatalf("create agent %s: %v", label, err)
	}
	return a
}

func mustTeam(t *testing.T, s *Store, key, name string, projects ...ProjectRef) Team {
	t.Helper()
	tm, err := s.CreateTeam(context.Background(), operator(t), key, NewTeam{Name: name, Projects: projects})
	if err != nil {
		t.Fatalf("create team %s: %v", name, err)
	}
	return tm
}

func TestZeroCallerHasNoAuthority(t *testing.T) {
	s, _ := openTemp(t)
	ctx := context.Background()
	a := mustAgent(t, s, "a1", "builder")
	tm := mustTeam(t, s, "t1", "crew")
	before := count(t, s, "audit")

	var zero Caller
	checks := map[string]error{
		"create agent":  func() error { _, err := s.CreateAgent(ctx, zero, "k", NewAgent{Label: "x"}); return err }(),
		"rename agent":  func() error { _, err := s.RenameAgent(ctx, zero, "k", a.ID, a.Revision, "y"); return err }(),
		"set profile":   func() error { _, err := s.SetAgentProfile(ctx, zero, "k", a.ID, a.Revision, Profile{}); return err }(),
		"create team":   func() error { _, err := s.CreateTeam(ctx, zero, "k", NewTeam{Name: "z"}); return err }(),
		"rename team":   func() error { _, err := s.RenameTeam(ctx, zero, "k", tm.ID, tm.Revision, "z"); return err }(),
		"set projects":  func() error { _, err := s.SetTeamProjects(ctx, zero, "k", tm.ID, tm.Revision, nil); return err }(),
		"add member":    func() error { _, err := s.AddMember(ctx, zero, "k", tm.ID, a.ID, RoleWorker); return err }(),
		"set role":      func() error { _, err := s.SetMemberRole(ctx, zero, "k", tm.ID, a.ID, 1, RoleWorker); return err }(),
		"remove member": s.RemoveMember(ctx, zero, "k", tm.ID, a.ID, 1),
	}
	for name, err := range checks {
		if !errors.Is(err, ErrForbidden) {
			t.Errorf("%s with zero caller: got %v, want ErrForbidden", name, err)
		}
	}
	if got := count(t, s, "audit"); got != before {
		t.Errorf("forbidden calls wrote audit rows: %d -> %d", before, got)
	}
}

func TestOperatorCallerRequiresID(t *testing.T) {
	for _, id := range []string{"", "has space", "tab\t"} {
		if _, err := OperatorCaller(id); !errors.Is(err, ErrInvalid) {
			t.Errorf("OperatorCaller(%q): got %v, want ErrInvalid", id, err)
		}
	}
}

// Labels, model and client data never grant authority: an agent whose label
// and declared profile say "operator" is still only an agent caller.
func TestLabelsAndProfileGrantNoAuthority(t *testing.T) {
	s, _ := openTemp(t)
	ctx := context.Background()
	a, err := s.CreateAgent(ctx, operator(t), "a1", NewAgent{
		Label:   "operator",
		Profile: Profile{Model: "operator", Client: "operator", ClientVersion: "admin"},
	})
	if err != nil {
		t.Fatal(err)
	}
	self, err := AgentCaller(a.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateTeam(ctx, self, "t1", NewTeam{Name: "crew"}); !errors.Is(err, ErrForbidden) {
		t.Errorf("agent caller created a team: %v", err)
	}
	if _, err := s.SetAgentProfile(ctx, self, "p1", a.ID, a.Revision, Profile{Model: "other"}); !errors.Is(err, ErrForbidden) {
		t.Errorf("agent caller changed its own profile: %v", err)
	}
	// An agent caller named like an operator is still an agent.
	fake, err := AgentCaller("op-1")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateAgent(ctx, fake, "a2", NewAgent{Label: "other"}); !errors.Is(err, ErrForbidden) {
		t.Errorf("agent caller with an operator id created an agent: %v", err)
	}
}

func TestReplayReturnsOriginalResultWithoutSecondEffect(t *testing.T) {
	s, _ := openTemp(t)
	ctx := context.Background()
	first := mustAgent(t, s, "same-key", "builder")
	second, err := s.CreateAgent(ctx, operator(t), "same-key", NewAgent{Label: "builder"})
	if err != nil {
		t.Fatalf("replay: %v", err)
	}
	if second.ID != first.ID || second.Revision != first.Revision || !second.CreatedAt.Equal(first.CreatedAt) {
		t.Errorf("replay returned %+v, want %+v", second, first)
	}
	if n := count(t, s, "agents"); n != 1 {
		t.Errorf("agents after replay = %d, want 1", n)
	}
	if n := count(t, s, "audit"); n != 1 {
		t.Errorf("audit rows after replay = %d, want 1", n)
	}
	if n := count(t, s, "receipts"); n != 1 {
		t.Errorf("receipts after replay = %d, want 1", n)
	}
}

func TestChangedInputUnderSameKeyConflicts(t *testing.T) {
	s, _ := openTemp(t)
	mustAgent(t, s, "k1", "builder")
	_, err := s.CreateAgent(context.Background(), operator(t), "k1", NewAgent{Label: "reviewer"})
	if !errors.Is(err, ErrIdempotencyConflict) {
		t.Fatalf("got %v, want ErrIdempotencyConflict", err)
	}
	if n := count(t, s, "agents"); n != 1 {
		t.Errorf("agents = %d, want 1", n)
	}
}

// Equal project sets in a different order are the same input.
func TestProjectOrderDoesNotChangeDigest(t *testing.T) {
	s, _ := openTemp(t)
	p1 := ProjectRef{HubID: "hub-a", ProjectID: "p1"}
	p2 := ProjectRef{HubID: "hub-a", ProjectID: "p2"}
	first := mustTeam(t, s, "k1", "crew", p2, p1, p1)
	again := mustTeam(t, s, "k1", "crew", p1, p2)
	if again.ID != first.ID {
		t.Fatalf("replay created a second team")
	}
	if len(first.Projects) != 2 || first.Projects[0] != p1 || first.Projects[1] != p2 {
		t.Errorf("projects = %+v, want sorted and de-duplicated", first.Projects)
	}
}

// A forbidden caller never receives another caller's receipt, even with the
// same key: authorization runs before any receipt lookup.
func TestReplayRechecksAuthorization(t *testing.T) {
	s, _ := openTemp(t)
	mustAgent(t, s, "k1", "builder")
	var zero Caller
	if _, err := s.CreateAgent(context.Background(), zero, "k1", NewAgent{Label: "builder"}); !errors.Is(err, ErrForbidden) {
		t.Fatalf("zero caller replay: got %v, want ErrForbidden", err)
	}
}

func TestFailedCommandRollsBackStateAuditAndReceipt(t *testing.T) {
	s, _ := openTemp(t)
	ctx := context.Background()
	injected := errors.New("injected failure")
	s.beforeReceipt = func(string) error { return injected }
	if _, err := s.CreateAgent(ctx, operator(t), "k1", NewAgent{Label: "builder"}); !errors.Is(err, injected) {
		t.Fatalf("got %v, want injected failure", err)
	}
	for _, table := range []string{"agents", "audit", "receipts"} {
		if n := count(t, s, table); n != 0 {
			t.Errorf("%s has %d rows after rollback, want 0", table, n)
		}
	}
	// With no receipt stored, the same key applies normally on retry.
	s.beforeReceipt = nil
	if _, err := s.CreateAgent(ctx, operator(t), "k1", NewAgent{Label: "builder"}); err != nil {
		t.Fatalf("retry after rollback: %v", err)
	}
	for _, table := range []string{"agents", "audit", "receipts"} {
		if n := count(t, s, table); n != 1 {
			t.Errorf("%s has %d rows after retry, want 1", table, n)
		}
	}
}

func TestPersistsAcrossReopen(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "aicrew.db")
	s, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	a := mustAgent(t, s, "a1", "builder")
	tm := mustTeam(t, s, "t1", "crew", ProjectRef{HubID: "hub-a", ProjectID: "p1"})
	m, err := s.AddMember(ctx, operator(t), "m1", tm.ID, a.ID, RoleWorker)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	s2, err := Open(ctx, path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer s2.Close()
	gotTeam, err := s2.GetTeam(ctx, tm.ID)
	if err != nil || gotTeam.Name != "crew" || len(gotTeam.Projects) != 1 {
		t.Fatalf("team after reopen = %+v, %v", gotTeam, err)
	}
	members, err := s2.ListMembers(ctx, tm.ID)
	if err != nil || len(members) != 1 || members[0].Role != RoleWorker || !members[0].CreatedAt.Equal(m.CreatedAt) {
		t.Fatalf("members after reopen = %+v, %v", members, err)
	}
	// Receipts survive too: replaying after reopen returns the original.
	replayed, err := s2.CreateAgent(ctx, operator(t), "a1", NewAgent{Label: "builder"})
	if err != nil || replayed.ID != a.ID {
		t.Fatalf("replay after reopen = %+v, %v", replayed, err)
	}
	if n := count(t, s2, "audit"); n != 3 {
		t.Errorf("audit rows = %d, want 3", n)
	}
}

func TestOpenRejectsAmbiguousPath(t *testing.T) {
	for _, p := range []string{"", filepath.Join(t.TempDir(), "a?b.db")} {
		if _, err := Open(context.Background(), p); !errors.Is(err, ErrInvalid) {
			t.Errorf("Open(%q): got %v, want ErrInvalid", p, err)
		}
	}
}

func TestSchemaFromNewerBuildIsRefused(t *testing.T) {
	ctx := context.Background()
	s, path := openTemp(t)
	if _, err := s.db.Exec(`UPDATE schema_version SET version = 99`); err != nil {
		t.Fatal(err)
	}
	s.Close()
	if _, err := Open(ctx, path); !errors.Is(err, ErrSchemaTooNew) {
		t.Fatalf("got %v, want ErrSchemaTooNew", err)
	}
}

func TestLabelResolution(t *testing.T) {
	s, _ := openTemp(t)
	ctx := context.Background()
	one := mustAgent(t, s, "a1", "builder")
	mustAgent(t, s, "a2", "twin")
	mustAgent(t, s, "a3", "twin")

	got, err := s.ResolveAgent(ctx, "builder")
	if err != nil || got.ID != one.ID {
		t.Errorf("resolve unique label = %+v, %v", got, err)
	}
	if _, err := s.ResolveAgent(ctx, "twin"); !errors.Is(err, ErrAmbiguous) {
		t.Errorf("resolve shared label: got %v, want ErrAmbiguous", err)
	}
	if _, err := s.ResolveAgent(ctx, "nobody"); !errors.Is(err, ErrNotFound) {
		t.Errorf("resolve missing label: got %v, want ErrNotFound", err)
	}
	if _, err := s.ResolveAgent(ctx, "Bad Label"); !errors.Is(err, ErrInvalid) {
		t.Errorf("resolve invalid label: got %v, want ErrInvalid", err)
	}

	mustTeam(t, s, "t1", "crew")
	mustTeam(t, s, "t2", "crew")
	if _, err := s.ResolveTeam(ctx, "crew"); !errors.Is(err, ErrAmbiguous) {
		t.Errorf("resolve shared team name: got %v, want ErrAmbiguous", err)
	}

	// Renaming changes the label only; the ID stays.
	renamed, err := s.RenameAgent(ctx, operator(t), "r1", one.ID, one.Revision, "builder-2")
	if err != nil || renamed.ID != one.ID || renamed.Revision != 2 {
		t.Fatalf("rename = %+v, %v", renamed, err)
	}
}

func TestInvalidInputRejected(t *testing.T) {
	s, _ := openTemp(t)
	ctx := context.Background()
	op := operator(t)
	if _, err := s.CreateAgent(ctx, op, "a1", NewAgent{Label: "UPPER"}); !errors.Is(err, ErrInvalid) {
		t.Errorf("uppercase label: %v", err)
	}
	if _, err := s.CreateAgent(ctx, op, "a2", NewAgent{Label: "ok", Profile: Profile{Model: "bad\x01"}}); !errors.Is(err, ErrInvalid) {
		t.Errorf("control character in profile: %v", err)
	}
	if _, err := s.CreateAgent(ctx, op, "", NewAgent{Label: "ok"}); !errors.Is(err, ErrInvalid) {
		t.Errorf("empty idempotency key: %v", err)
	}
	if _, err := s.CreateTeam(ctx, op, "t1", NewTeam{Name: "crew", Projects: []ProjectRef{{HubID: "", ProjectID: "p"}}}); !errors.Is(err, ErrInvalid) {
		t.Errorf("empty hub id: %v", err)
	}
	a := mustAgent(t, s, "a3", "builder")
	tm := mustTeam(t, s, "t2", "crew")
	if _, err := s.AddMember(ctx, op, "m1", tm.ID, a.ID, Role("admin")); !errors.Is(err, ErrInvalid) {
		t.Errorf("unknown role: %v", err)
	}
}

func TestLinkedIdentityStaysUnset(t *testing.T) {
	s, _ := openTemp(t)
	a := mustAgent(t, s, "a1", "builder")
	if a.Linked != nil {
		t.Fatalf("new agent has linked identity %+v", a.Linked)
	}
	got, err := s.GetAgent(context.Background(), a.ID)
	if err != nil || got.Linked != nil {
		t.Fatalf("stored agent linked = %+v, %v", got.Linked, err)
	}
	// The schema refuses a half-set link.
	if _, err := s.db.Exec(`UPDATE agents SET linked_hub_id = 'hub-a' WHERE id = ?`, a.ID); err == nil {
		t.Fatal("schema accepted a hub ID without a user ID")
	}
}

func TestMembershipLifecycle(t *testing.T) {
	s, _ := openTemp(t)
	ctx := context.Background()
	op := operator(t)
	a := mustAgent(t, s, "a1", "builder")
	tm := mustTeam(t, s, "t1", "crew")

	m, err := s.AddMember(ctx, op, "m1", tm.ID, a.ID, RoleWorker)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.AddMember(ctx, op, "m2", tm.ID, a.ID, RoleCoordinator); !errors.Is(err, ErrExists) {
		t.Errorf("duplicate membership: %v", err)
	}
	if _, err := s.AddMember(ctx, op, "m3", tm.ID, "missing", RoleWorker); !errors.Is(err, ErrNotFound) {
		t.Errorf("missing agent: %v", err)
	}
	m2, err := s.SetMemberRole(ctx, op, "r1", tm.ID, a.ID, m.Revision, RoleIndependent)
	if err != nil || m2.Role != RoleIndependent || m2.Revision != 2 {
		t.Fatalf("set role = %+v, %v", m2, err)
	}
	if _, err := s.SetMemberRole(ctx, op, "r2", tm.ID, a.ID, m.Revision, RoleWorker); !errors.Is(err, ErrRevisionConflict) {
		t.Errorf("stale role change: %v", err)
	}
	if err := s.RemoveMember(ctx, op, "x1", tm.ID, a.ID, m2.Revision); err != nil {
		t.Fatal(err)
	}
	if members, err := s.ListMembers(ctx, tm.ID); err != nil || len(members) != 0 {
		t.Fatalf("members after removal = %+v, %v", members, err)
	}
	if err := s.RemoveMember(ctx, op, "x2", tm.ID, a.ID, m2.Revision); !errors.Is(err, ErrNotFound) {
		t.Errorf("remove missing membership: %v", err)
	}
}

// A command prepared against a removed membership must not act on a later
// re-added membership of the same pair: revisions are never reused.
func TestStaleMembershipCommandsDoNotTouchReplacement(t *testing.T) {
	s, _ := openTemp(t)
	ctx := context.Background()
	op := operator(t)
	a := mustAgent(t, s, "a1", "builder")
	tm := mustTeam(t, s, "t1", "crew")

	old, err := s.AddMember(ctx, op, "add-1", tm.ID, a.ID, RoleWorker)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.RemoveMember(ctx, op, "remove-1", tm.ID, a.ID, old.Revision); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SetMemberRole(ctx, op, "role-removed", tm.ID, a.ID, old.Revision, RoleIndependent); !errors.Is(err, ErrNotFound) {
		t.Errorf("role change on removed membership: got %v, want ErrNotFound", err)
	}
	replacement, err := s.AddMember(ctx, op, "add-2", tm.ID, a.ID, RoleCoordinator)
	if err != nil {
		t.Fatal(err)
	}
	if replacement.Revision <= old.Revision {
		t.Fatalf("replacement revision %d reuses or precedes old revision %d", replacement.Revision, old.Revision)
	}

	// Delayed commands that expected the old membership's revision.
	if _, err := s.SetMemberRole(ctx, op, "role-stale", tm.ID, a.ID, old.Revision, RoleWorker); !errors.Is(err, ErrRevisionConflict) {
		t.Errorf("stale role change: got %v, want ErrRevisionConflict", err)
	}
	if err := s.RemoveMember(ctx, op, "remove-stale", tm.ID, a.ID, old.Revision); !errors.Is(err, ErrRevisionConflict) {
		t.Errorf("stale removal: got %v, want ErrRevisionConflict", err)
	}
	members, err := s.ListMembers(ctx, tm.ID)
	if err != nil || len(members) != 1 || members[0].Role != RoleCoordinator || members[0].Revision != replacement.Revision {
		t.Fatalf("replacement after stale commands = %+v, %v", members, err)
	}
}

// Concurrent conflicting writes: exactly one wins, the rest see a conflict.
func TestConcurrentConflictingWritesHaveOneWinner(t *testing.T) {
	s, _ := openTemp(t)
	ctx := context.Background()
	op := operator(t)
	tm := mustTeam(t, s, "t1", "crew")
	a := mustAgent(t, s, "a1", "builder")

	const n = 8
	var wg sync.WaitGroup
	projectErrs := make([]error, n)
	memberErrs := make([]error, n)
	for i := range n {
		wg.Add(2)
		go func() {
			defer wg.Done()
			_, projectErrs[i] = s.SetTeamProjects(ctx, op, fmt.Sprintf("p%d", i), tm.ID, tm.Revision,
				[]ProjectRef{{HubID: "hub-a", ProjectID: fmt.Sprintf("p%d", i)}})
		}()
		go func() {
			defer wg.Done()
			_, memberErrs[i] = s.AddMember(ctx, op, fmt.Sprintf("m%d", i), tm.ID, a.ID, RoleWorker)
		}()
	}
	wg.Wait()

	tally := func(name string, errs []error, loser error) {
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
	tally("set projects", projectErrs, ErrRevisionConflict)
	tally("add member", memberErrs, ErrExists)

	got, err := s.GetTeam(ctx, tm.ID)
	if err != nil || got.Revision != 2 || len(got.Projects) != 1 {
		t.Errorf("team after race = %+v, %v", got, err)
	}
}
