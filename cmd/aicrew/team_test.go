package main

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

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

func decodeTeam(t *testing.T, r result) store.Team {
	t.Helper()
	var tm store.Team
	if err := json.Unmarshal([]byte(r.stdout), &tm); err != nil || tm.ID == "" {
		t.Fatalf("output is not a team: %v\n%s", err, r.stdout)
	}
	return tm
}

// Create, list, show, replace the projects and rename: each prints the team
// as JSON and the store holds what was printed.
func TestTeamLifecycle(t *testing.T) {
	path := filepath.Join(t.TempDir(), "aicrew.db")

	created := decodeTeam(t, teamCLI(t, 0, "create", "-store", path, "-name", "crew",
		"-project", "hub-a/docs", "-project", "hub-a/api", "-project", "hub-a/docs"))
	if created.Name != "crew" || created.Revision != 1 || created.CreatedAt.IsZero() {
		t.Fatalf("created = %+v", created)
	}
	want := []store.ProjectRef{{HubID: "hub-a", ProjectID: "api"}, {HubID: "hub-a", ProjectID: "docs"}}
	if len(created.Projects) != 2 || created.Projects[0] != want[0] || created.Projects[1] != want[1] {
		t.Fatalf("projects = %v, want %v (deduplicated and sorted)", created.Projects, want)
	}
	other := decodeTeam(t, teamCLI(t, 0, "create", "-store", path, "-name", "alpha"))
	if len(other.Projects) != 0 {
		t.Fatalf("a team created without -project has projects %v", other.Projects)
	}

	// A member, added the way an invitation adds one, shows in show and in
	// list's count.
	st, err := store.Open(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	op, _ := store.OperatorCaller("op-test")
	agent, err := st.CreateAgent(context.Background(), op, "agent", store.NewAgent{Label: "builder"})
	if err == nil {
		_, err = st.AddMember(context.Background(), op, "member", created.ID, agent.ID, store.RoleWorker)
	}
	st.Close()
	if err != nil {
		t.Fatal(err)
	}

	var listed []store.TeamSummary
	if err := json.Unmarshal([]byte(teamCLI(t, 0, "list", "-store", path).stdout), &listed); err != nil {
		t.Fatal(err)
	}
	if len(listed) != 2 || listed[0].ID != other.ID || listed[1].ID != created.ID || listed[1].Members != 1 || listed[0].Members != 0 {
		t.Fatalf("list = %+v, want alpha (0 members) then crew (1 member)", listed)
	}

	var shown teamDetail
	if err := json.Unmarshal([]byte(teamCLI(t, 0, "show", "-store", path, "-team", created.ID).stdout), &shown); err != nil {
		t.Fatal(err)
	}
	if shown.ID != created.ID || len(shown.Members) != 1 || shown.Members[0].AgentID != agent.ID || shown.Members[0].Role != store.RoleWorker {
		t.Fatalf("show = %+v", shown)
	}

	replaced := decodeTeam(t, teamCLI(t, 0, "projects", "-store", path, "-team", created.ID, "-expect-revision", "1",
		"-project", "hub-b/ops"))
	if replaced.Revision != 2 || len(replaced.Projects) != 1 || replaced.Projects[0] != (store.ProjectRef{HubID: "hub-b", ProjectID: "ops"}) {
		t.Fatalf("projects replaced = %+v", replaced)
	}
	cleared := decodeTeam(t, teamCLI(t, 0, "projects", "-store", path, "-team", created.ID, "-expect-revision", "2"))
	if cleared.Revision != 3 || len(cleared.Projects) != 0 {
		t.Fatalf("an empty -project list must clear the set: %+v", cleared)
	}

	renamed := decodeTeam(t, teamCLI(t, 0, "rename", "-store", path, "-team", created.ID, "-expect-revision", "3", "-name", "crew-2"))
	if renamed.Name != "crew-2" || renamed.Revision != 4 {
		t.Fatalf("renamed = %+v", renamed)
	}
	// Renaming a team to its own name is not a clash with itself.
	decodeTeam(t, teamCLI(t, 0, "rename", "-store", path, "-team", created.ID, "-expect-revision", "4", "-name", "crew-2"))
}

// The refusals: a duplicate name, a stale revision, an unknown team, an
// invalid name or project, and usage errors. None of them changes the store.
func TestTeamRefusals(t *testing.T) {
	path := filepath.Join(t.TempDir(), "aicrew.db")
	crew := decodeTeam(t, teamCLI(t, 0, "create", "-store", path, "-name", "crew"))
	alpha := decodeTeam(t, teamCLI(t, 0, "create", "-store", path, "-name", "alpha"))
	rev := strconv.FormatInt(crew.Revision, 10)

	failures := []struct {
		want string
		args []string
	}{
		{"team_exists", []string{"create", "-store", path, "-name", "crew"}},
		{"team_exists", []string{"rename", "-store", path, "-team", alpha.ID, "-expect-revision", "1", "-name", "crew"}},
		{"revision_conflict", []string{"projects", "-store", path, "-team", crew.ID, "-expect-revision", "7", "-project", "hub-a/docs"}},
		{"revision_conflict", []string{"rename", "-store", path, "-team", crew.ID, "-expect-revision", "7", "-name", "other"}},
		{"not found", []string{"show", "-store", path, "-team", "no-such-team"}},
		{"not found", []string{"projects", "-store", path, "-team", "no-such-team", "-expect-revision", "1"}},
		{"invalid input", []string{"create", "-store", path, "-name", "Crew!"}},
		{"invalid input", []string{"create", "-store", path, "-name", "beta", "-project", "hub-a"}},
		{"invalid input", []string{"create", "-store", path, "-name", "beta", "-project", "/docs"}},
		{"invalid input", []string{"projects", "-store", path, "-team", crew.ID, "-expect-revision", rev, "-project", "hub a/docs"}},
	}
	for _, f := range failures {
		r := teamCLI(t, 1, f.args...)
		if !strings.Contains(r.stderr, f.want) || r.stdout != "" {
			t.Errorf("%v: stderr %q, stdout %q; want %q on stderr only", f.args, r.stderr, r.stdout, f.want)
		}
	}

	usages := [][]string{
		{},
		{"delete", "-store", path},
		{"create", "-store", path},
		{"create", "-name", "beta"},
		{"create", "-store", path, "-name", "beta", "-team", crew.ID},
		{"list", "-store", path, "-name", "crew"},
		{"show", "-store", path},
		{"projects", "-store", path, "-team", crew.ID},
		{"projects", "-store", path, "-expect-revision", "1"},
		{"rename", "-store", path, "-team", crew.ID, "-name", "beta"},
		{"rename", "-store", path, "-team", crew.ID, "-expect-revision", "1"},
		{"show", "-store", path, "-team", crew.ID, "extra"},
	}
	for _, args := range usages {
		if r := teamCLI(t, 2, args...); !strings.Contains(r.stderr, "usage:") {
			t.Errorf("%v: stderr %q, want the usage", args, r.stderr)
		}
	}

	var listed []store.TeamSummary
	if err := json.Unmarshal([]byte(teamCLI(t, 0, "list", "-store", path).stdout), &listed); err != nil {
		t.Fatal(err)
	}
	if len(listed) != 2 || listed[0].Revision != 1 || listed[1].Revision != 1 || listed[1].Name != "crew" || len(listed[1].Projects) != 0 {
		t.Fatalf("the refusals changed the store: %+v", listed)
	}
}
