package store

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
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

// mustTeam creates a team; its hub grants it projects, as a team.read of
// the hub would record them.
func mustTeam(t *testing.T, s *Store, key, name string, projects ...ProjectRef) Team {
	t.Helper()
	tm, err := s.CreateTeam(context.Background(), operator(t), key, NewTeam{Name: name})
	if err != nil {
		t.Fatalf("create team %s: %v", name, err)
	}
	if len(projects) == 0 {
		return tm
	}
	return grant(t, s, tm.ID, projects...)
}

// grantClock orders the tests' team reads: each is sent after the last.
var grantClock atomic.Int64

// nextReadAt is when the tests' next team read is sent.
func nextReadAt() time.Time {
	return time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC).Add(time.Duration(grantClock.Add(1)) * time.Second)
}

// grant records a team.read of the team's hub that grants exactly projects.
func grant(t *testing.T, s *Store, teamID string, projects ...ProjectRef) Team {
	t.Helper()
	read := TeamGrantsRead{HubID: "hub-a", State: GrantsEnabled, Grants: []TeamGrant{}, At: nextReadAt()}
	if len(projects) > 0 {
		read.HubID = projects[0].HubID
	}
	for _, p := range projects {
		read.Grants = append(read.Grants, TeamGrant{HubID: p.HubID, ProjectID: p.ProjectID})
	}
	if ok, err := s.RecordTeamGrants(context.Background(), ReconcilerCaller(), teamID, read); err != nil || !ok {
		t.Fatalf("grant %v to team %s: %v, %v", projects, teamID, ok, err)
	}
	tm, err := s.GetTeam(context.Background(), teamID)
	if err != nil {
		t.Fatal(err)
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
		"create agent": func() error { _, err := s.CreateAgent(ctx, zero, "k", NewAgent{Label: "x"}); return err }(),
		"rename agent": func() error { _, err := s.RenameAgent(ctx, zero, "k", a.ID, a.Revision, "y"); return err }(),
		"set profile":  func() error { _, err := s.SetAgentProfile(ctx, zero, "k", a.ID, a.Revision, Profile{}); return err }(),
		"create team":  func() error { _, err := s.CreateTeam(ctx, zero, "k", NewTeam{Name: "z"}); return err }(),
		"rename team":  func() error { _, err := s.RenameTeam(ctx, zero, "k", tm.ID, tm.Revision, "z"); return err }(),
		"set hub":      func() error { _, err := s.SetTeamHub(ctx, zero, "k", tm.ID, "main"); return err }(),
		"record grants": func() error {
			_, err := s.RecordTeamGrants(ctx, zero, tm.ID, TeamGrantsRead{HubID: "hub-a", State: GrantsDisabled, At: time.Now()})
			return err
		}(),
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

// A team's grants are the last team.read sent, ordered by project: a read
// sent earlier but answered later never replaces a newer one, and a read
// that is not a valid answer records nothing.
func TestTeamGrantsFollowTheNewestRead(t *testing.T) {
	s, _ := openTemp(t)
	ctx := context.Background()
	tm := mustTeam(t, s, "t1", "crew")
	p1 := ProjectRef{HubID: "hub-a", ProjectID: "p1"}
	p2 := ProjectRef{HubID: "hub-a", ProjectID: "p2"}
	if len(tm.Grants) != 0 || tm.GrantsState != "" || tm.GrantsReadAt != nil {
		t.Fatalf("a new team has grants: %+v", tm)
	}
	got := grant(t, s, tm.ID, p2, p1)
	if len(got.Grants) != 2 || got.Grants[0].ProjectID != "p1" || got.Grants[1].ProjectID != "p2" ||
		got.GrantsState != GrantsEnabled || got.GrantsReadAt == nil {
		t.Fatalf("granted = %+v", got)
	}
	stale := TeamGrantsRead{HubID: "hub-a", State: GrantsDisabled, At: got.GrantsReadAt.Add(-time.Second)}
	if ok, err := s.RecordTeamGrants(ctx, ReconcilerCaller(), tm.ID, stale); err != nil || ok {
		t.Fatalf("a stale read was recorded: %v, %v", ok, err)
	}
	full := &GrantRepository{Kind: "git", URL: "https://git.example.test/crew/p1.git", Host: "git.example.test", Access: "write"}
	pin := &GrantProcess{Repo: "https://git.example.test/process.git", Commit: strings.Repeat("a", 40), Manifest: "m.json"}
	newer := TeamGrantsRead{HubID: "hub-a", State: GrantsEnabled, At: got.GrantsReadAt.Add(time.Second),
		Grants: []TeamGrant{{HubID: "hub-a", ProjectID: "p1", Repository: full, Process: pin}}}
	if ok, err := s.RecordTeamGrants(ctx, ReconcilerCaller(), tm.ID, newer); err != nil || !ok {
		t.Fatalf("a newer read: %v, %v", ok, err)
	}
	got, err := s.GetTeam(ctx, tm.ID)
	if err != nil || len(got.Grants) != 1 || *got.Grants[0].Repository != *full || *got.Grants[0].Process != *pin {
		t.Fatalf("after the newer read = %+v, %v", got, err)
	}
	for name, bad := range map[string]TeamGrantsRead{
		"unknown state":       {HubID: "hub-a", State: "maybe", At: time.Now()},
		"no time":             {HubID: "hub-a", State: GrantsDisabled},
		"no hub":              {State: GrantsDisabled, At: time.Now()},
		"another hub's grant": {HubID: "hub-b", State: GrantsEnabled, At: time.Now(), Grants: []TeamGrant{{HubID: "hub-a", ProjectID: "p1"}}},
		"disabled with grant": {HubID: "hub-a", State: GrantsDisabled, At: time.Now(), Grants: []TeamGrant{{HubID: "hub-a", ProjectID: "p1"}}},
		"twice":               {HubID: "hub-a", State: GrantsEnabled, At: time.Now(), Grants: []TeamGrant{{HubID: "hub-a", ProjectID: "p1"}, {HubID: "hub-a", ProjectID: "p1"}}},
		"bad project":         {HubID: "hub-a", State: GrantsEnabled, At: time.Now(), Grants: []TeamGrant{{HubID: "hub-a", ProjectID: "p 1"}}},
		"long url": {HubID: "hub-a", State: GrantsEnabled, At: time.Now(), Grants: []TeamGrant{{HubID: "hub-a", ProjectID: "p1",
			Repository: &GrantRepository{Kind: "git", URL: strings.Repeat("u", maxGrantField+1)}}}},
	} {
		if _, err := s.RecordTeamGrants(ctx, ReconcilerCaller(), tm.ID, bad); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: %v", name, err)
		}
	}
	if _, err := s.RecordTeamGrants(ctx, ReconcilerCaller(), "no-such-team", TeamGrantsRead{HubID: "hub-a", State: GrantsDisabled, At: time.Now()}); !errors.Is(err, ErrNotFound) {
		t.Errorf("unknown team: %v", err)
	}
	agent, err := AgentCaller("agent-1")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.RecordTeamGrants(ctx, agent, tm.ID, TeamGrantsRead{HubID: "hub-a", State: GrantsDisabled, At: time.Now()}); !errors.Is(err, ErrForbidden) {
		t.Errorf("an agent recorded grants: %v", err)
	}
}

// A team created before teams named a hub can be given one, once.
func TestSetTeamHub(t *testing.T) {
	s, _ := openTemp(t)
	ctx := context.Background()
	op := operator(t)
	tm := mustTeam(t, s, "t1", "crew")
	got, err := s.SetTeamHub(ctx, op, "h1", tm.ID, "main")
	if err != nil || got.Hub != "main" || got.Revision != tm.Revision+1 {
		t.Fatalf("set hub = %+v, %v", got, err)
	}
	if again, err := s.SetTeamHub(ctx, op, "h2", tm.ID, "main"); err != nil || again.Revision != got.Revision {
		t.Fatalf("the same hub again = %+v, %v", again, err)
	}
	if _, err := s.SetTeamHub(ctx, op, "h3", tm.ID, "lab"); !errors.Is(err, ErrInvalid) {
		t.Fatalf("another hub: %v", err)
	}
	if _, err := s.SetTeamHub(ctx, op, "h4", tm.ID, "Lab!"); !errors.Is(err, ErrInvalid) {
		t.Fatalf("a bad alias: %v", err)
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
	if err != nil || gotTeam.Name != "crew" || len(gotTeam.Grants) != 1 {
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
	// The second attempt proves the failed first one released its lock.
	for range 2 {
		if _, err := Open(ctx, path); !errors.Is(err, ErrSchemaTooNew) {
			t.Fatalf("got %v, want ErrSchemaTooNew", err)
		}
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
	if _, err := s.CreateTeam(ctx, op, "t1", NewTeam{Name: "crew", Hub: "Main Hub"}); !errors.Is(err, ErrInvalid) {
		t.Errorf("bad hub alias: %v", err)
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
			_, projectErrs[i] = s.RenameTeam(ctx, op, fmt.Sprintf("p%d", i), tm.ID, tm.Revision, fmt.Sprintf("crew-%d", i))
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
	tally("rename", projectErrs, ErrRevisionConflict)
	tally("add member", memberErrs, ErrExists)

	got, err := s.GetTeam(ctx, tm.ID)
	if err != nil || got.Revision != 2 || got.Name == "crew" {
		t.Errorf("team after race = %+v, %v", got, err)
	}
}

// A command's input is checked for invalid UTF-8 in the strings it encodes,
// and only those: validateText does not enter a time.Time, whose zone name
// JSON never encodes and whose *Location the time package may be filling in
// meanwhile (a data race under -race).
func TestInputDigestStopsAtTime(t *testing.T) {
	odd := time.Date(2026, 10, 1, 12, 0, 0, 0, time.FixedZone("zone-\xff", 2*60*60))
	type input struct {
		Text string
		At   time.Time
		When *time.Time
	}
	encoded, _, err := inputDigest(input{Text: "ok", At: odd, When: &odd})
	if err != nil {
		t.Fatalf("a time in a zone named with invalid UTF-8: %v, want it accepted", err)
	}
	if strings.Contains(encoded, "zone-") || !strings.Contains(encoded, `"2026-10-01T12:00:00+02:00"`) {
		t.Fatalf("encoded input = %s, want the time as RFC 3339 with its offset and no zone name", encoded)
	}
	if _, _, err := inputDigest(input{Text: "bad \xff", At: odd}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("invalid text beside a time: %v, want ErrInvalid", err)
	}
	if _, _, err := inputDigest(map[string]any{"at": odd, "note": []string{"fine", "\xff"}}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("invalid text in a nested slice beside a time: %v, want ErrInvalid", err)
	}
}
