package main

import (
	"context"
	"encoding/json"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/BlackVS/aicrew/internal/opapi"
	"github.com/BlackVS/aicrew/internal/store"
)

func teamCLI(t *testing.T, want int, args ...string) result {
	t.Helper()
	r := cli(t, append([]string{"team"}, args...)...)
	if r.code != want {
		t.Fatalf("aicrew team %v: exit %d, want %d\nstdout: %s\nstderr: %s", args, r.code, want, r.stdout, r.stderr)
	}
	return r
}

func decodeTeam(t *testing.T, r result) opapi.Team {
	t.Helper()
	var tm opapi.Team
	if err := json.Unmarshal([]byte(r.stdout), &tm); err != nil || tm.ID == "" {
		t.Fatalf("output is not a team: %v\n%s", err, r.stdout)
	}
	return tm
}

// Create, list, show and rename: each prints the team as JSON and the store
// holds what was printed. show prints the grants aicrewd last read from the
// team's hub.
func TestTeamLifecycle(t *testing.T) {
	s := serve(t)

	created := decodeTeam(t, teamCLI(t, 0, "create", "-name", "crew"))
	if created.Name != "crew" || created.Revision != 1 || created.CreatedAt.IsZero() || created.Grants == nil || len(created.Grants) != 0 {
		t.Fatalf("created = %+v", created)
	}
	other := decodeTeam(t, teamCLI(t, 0, "create", "-name", "alpha"))
	read := store.TeamGrantsRead{HubID: "hub-a", State: store.GrantsEnabled, At: time.Now(), Grants: []store.TeamGrant{{HubID: "hub-a", ProjectID: "docs",
		Repository: &store.GrantRepository{Kind: "git", URL: "https://git.example.test/docs.git", Host: "git.example.test", Access: "write"},
		Process:    &store.GrantProcess{Repo: "https://git.example.test/process.git", Commit: strings.Repeat("c", 40), Manifest: "m.json"}}}}
	if _, err := s.store.RecordTeamGrants(context.Background(), store.ReconcilerCaller(), created.ID, read); err != nil {
		t.Fatal(err)
	}

	// A member, added the way an invitation adds one, shows in show and in
	// list's count.
	op := operator(t)
	agent, err := s.store.CreateAgent(context.Background(), op, "agent", store.NewAgent{Label: "builder"})
	if err == nil {
		_, err = s.store.AddMember(context.Background(), op, "member", created.ID, agent.ID, store.RoleWorker)
	}
	if err != nil {
		t.Fatal(err)
	}

	var listed []opapi.TeamSummary
	if err := json.Unmarshal([]byte(teamCLI(t, 0, "list").stdout), &listed); err != nil {
		t.Fatal(err)
	}
	if len(listed) != 2 || listed[0].ID != other.ID || listed[1].ID != created.ID || listed[1].Members != 1 || listed[0].Members != 0 {
		t.Fatalf("list = %+v, want alpha (0 members) then crew (1 member)", listed)
	}

	var shown opapi.TeamDetail
	if err := json.Unmarshal([]byte(teamCLI(t, 0, "show", "-team", created.ID).stdout), &shown); err != nil {
		t.Fatal(err)
	}
	if shown.ID != created.ID || len(shown.Members) != 1 || shown.Members[0].AgentID != agent.ID || shown.Members[0].Role != string(store.RoleWorker) {
		t.Fatalf("show = %+v", shown)
	}
	g := shown.Grants
	if len(g) != 1 || g[0].ProjectID != "docs" || g[0].Repository == nil || g[0].Repository.Access != "write" || g[0].Process == nil ||
		shown.GrantsState != store.GrantsEnabled || shown.GrantsReadAt == nil {
		t.Fatalf("show's grants = %+v", shown)
	}

	renamed := decodeTeam(t, teamCLI(t, 0, "rename", "-team", created.ID, "-expect-revision", "1", "-name", "crew-2"))
	if renamed.Name != "crew-2" || renamed.Revision != 2 {
		t.Fatalf("renamed = %+v", renamed)
	}
	// Renaming a team to its own name is not a clash with itself.
	decodeTeam(t, teamCLI(t, 0, "rename", "-team", created.ID, "-expect-revision", "2", "-name", "crew-2"))
}

// The refusals: a duplicate name, a stale revision, an unknown team, an
// invalid name or project, and usage errors. None of them changes the store.
func TestTeamRefusals(t *testing.T) {
	serve(t)
	crew := decodeTeam(t, teamCLI(t, 0, "create", "-name", "crew"))
	alpha := decodeTeam(t, teamCLI(t, 0, "create", "-name", "alpha"))
	rev := strconv.FormatInt(crew.Revision, 10)

	failures := []struct {
		want string
		args []string
	}{
		{"team_exists", []string{"create", "-name", "crew"}},
		{"team_exists", []string{"rename", "-team", alpha.ID, "-expect-revision", "1", "-name", "crew"}},
		{"revision_conflict", []string{"rename", "-team", crew.ID, "-expect-revision", "7", "-name", "other"}},
		{"not found", []string{"show", "-team", "no-such-team"}},
		{"invalid input", []string{"create", "-name", "Crew!"}},
		{"not an aimem block", []string{"create", "-name", "beta", "-hub", "main"}},
		{"names no hub", []string{"register", "-team", crew.ID}},
	}
	for _, f := range failures {
		r := teamCLI(t, 1, f.args...)
		if !strings.Contains(r.stderr, f.want) || r.stdout != "" {
			t.Errorf("%v: stderr %q, stdout %q; want %q on stderr only", f.args, r.stderr, r.stdout, f.want)
		}
	}

	usages := [][]string{
		{},
		{"delete"},
		{"create"},
		{"create", "-name", "beta", "-team", crew.ID},
		{"list", "-name", "crew"},
		{"show"},
		{"rename", "-team", crew.ID, "-name", "beta"},
		{"rename", "-team", crew.ID, "-expect-revision", "1"},
		{"show", "-team", crew.ID, "extra"},
		{"create", "-name", "beta", "-hub", ""},
		{"register"},
		{"register", "-team", crew.ID, "-name", "beta"},
		{"register", "-team", crew.ID, "-hub", ""},
	}
	// The project list of earlier releases is gone, and its use says so.
	for _, args := range [][]string{
		{"projects", "-team", crew.ID, "-expect-revision", rev, "-project", "hub-a/docs"},
		{"create", "-name", "beta", "-project", "hub-a/docs"},
	} {
		if r := teamCLI(t, 2, args...); !strings.Contains(r.stderr, "grants its hub holds") || !strings.Contains(r.stderr, "usage:") {
			t.Errorf("%v: stderr %q, want the removal explained", args, r.stderr)
		}
	}
	for _, args := range usages {
		if r := teamCLI(t, 2, args...); !strings.Contains(r.stderr, "usage:") {
			t.Errorf("%v: stderr %q, want the usage", args, r.stderr)
		}
	}

	var listed []opapi.TeamSummary
	if err := json.Unmarshal([]byte(teamCLI(t, 0, "list").stdout), &listed); err != nil {
		t.Fatal(err)
	}
	if len(listed) != 2 || listed[0].Revision != 1 || listed[1].Revision != 1 || listed[1].Name != "crew" || len(listed[1].Grants) != 0 {
		t.Fatalf("the refusals changed the store: %+v", listed)
	}
}
