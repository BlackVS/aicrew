package main

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/BlackVS/aicrew/internal/hubteams"
	"github.com/BlackVS/aicrew/internal/server"
)

// setupHub is a hub's team operations for team setup: it registers a team
// unless the team's name is in refuse, which maps it to the hub's code, and
// grants each team the projects in grants; down makes reads fail.
type setupHub struct {
	mu         sync.Mutex
	refuse     map[string]string
	registered map[string]string
	registers  int
	grants     map[string][]string
	down       bool
	disabled   bool
}

func newSetupHub() *setupHub {
	return &setupHub{refuse: map[string]string{}, registered: map[string]string{}, grants: map[string][]string{}}
}

func (h *setupHub) CanRegister() bool { return true }
func (h *setupHub) CanRead() bool     { return true }
func (h *setupHub) Register(_ context.Context, teamID, name string) (hubteams.Registration, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.registers++
	if code, ok := h.refuse[name]; ok {
		return hubteams.Registration{}, &hubteams.Error{Code: code}
	}
	h.registered[teamID] = name
	return hubteams.Registration{ProfileID: "p", TeamID: teamID, TeamName: name}, nil
}
func (h *setupHub) ReadTeam(_ context.Context, id string) (hubteams.Team, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.down {
		return hubteams.Team{}, &hubteams.Error{Code: hubteams.CodeUnavailable}
	}
	name, ok := h.registered[id]
	if !ok {
		return hubteams.Team{}, &hubteams.Error{Code: "not_found"}
	}
	t := hubteams.Team{TeamID: id, TeamName: name, Enabled: !h.disabled, Projects: []hubteams.Project{}}
	if h.disabled {
		return t, nil
	}
	for _, p := range h.grants[name] {
		pr := hubteams.Project{Project: p}
		if p != "bare" {
			pr.Repository = &hubteams.Repository{Kind: "gitea", URL: "https://git.example.test/crew/" + p + ".git", Host: "git.example.test", Access: "write"}
			pr.Process = &hubteams.ProcessPin{Repo: "https://git.example.test/process.git", Commit: strings.Repeat("c", 40), Manifest: "m.json"}
		}
		t.Projects = append(t.Projects, pr)
	}
	return t, nil
}
func (h *setupHub) ReadTeams(context.Context) ([]hubteams.Team, error) { return nil, nil }

func (h *setupHub) grant(team string, projects ...string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.grants[team] = projects
}

func (h *setupHub) registerCount() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.registers
}

// On a hub with no grants, setup creates and registers the team and prints
// one grant command per project; run again after the grants exist, it
// reports every project granted and registers nothing again.
func TestTeamSetup(t *testing.T) {
	hub := newSetupHub()
	serve(t, server.WithHub("main", "hub-a", hub))

	r := cli(t, "team", "setup", "crew", "--hub", "main", "--project", "app", "--project", "docs", "--project", "app")
	if r.code != exitIncomplete || !strings.Contains(r.stdout, "team crew created on hub main") ||
		!strings.Contains(r.stdout, "team crew is registered on hub main") ||
		!strings.Contains(r.stdout, "aimem identity team grant --peer aicrew-test --team-name crew --project app\n") ||
		!strings.Contains(r.stdout, "aimem identity team grant --peer aicrew-test --team-name crew --project docs\n") ||
		strings.Count(r.stdout, "aimem identity team grant") != 2 {
		t.Fatalf("first run: exit %d\n%s%s", r.code, r.stdout, r.stderr)
	}

	hub.grant("crew", "zeta", "app", "docs", "bare")
	r = cli(t, "team", "setup", "crew", "--hub", "main", "--project", "app", "--project", "docs")
	if r.code != 0 || !strings.Contains(r.stdout, "team crew is already registered on hub main") ||
		!strings.Contains(r.stdout, "granted: app") || !strings.Contains(r.stdout, "granted: docs") ||
		!strings.Contains(r.stdout, "also granted: bare\nalso granted: zeta\n") || !strings.Contains(r.stdout, "every project is granted") ||
		strings.Contains(r.stdout, "aimem identity team grant") {
		t.Fatalf("second run: exit %d\n%s%s", r.code, r.stdout, r.stderr)
	}
	if hub.registerCount() != 1 {
		t.Fatalf("registered %d times", hub.registerCount())
	}

	// A granted project the hub binds no repository or process to is named.
	r = cli(t, "team", "setup", "crew", "--hub", "main", "--project", "bare")
	if r.code != 0 || !strings.Contains(r.stdout, "bare has no repository") || !strings.Contains(r.stdout, "bare has no selected process") {
		t.Fatalf("bare: exit %d\n%s%s", r.code, r.stdout, r.stderr)
	}
}

// A hub's refusal of the registration is reported with its code and the
// remedy, and no grant is read.
func TestTeamSetupRefused(t *testing.T) {
	hub := newSetupHub()
	hub.refuse["blocked"] = "peer_forbidden"
	hub.refuse["taken"] = "team_name_taken"
	serve(t, server.WithHub("main", "hub-a", hub), server.WithHub("lab", "hub-b", newSetupHub()))

	r := cli(t, "team", "setup", "blocked", "--hub", "main", "--project", "app")
	if r.code != 1 || !strings.Contains(r.stderr, "refused the registration of team blocked with peer_forbidden") ||
		!strings.Contains(r.stderr, "check service_id in aicrewd.json against aimem identity peer list") {
		t.Fatalf("peer_forbidden: exit %d\n%s%s", r.code, r.stdout, r.stderr)
	}
	r = cli(t, "team", "setup", "taken", "--hub", "main", "--project", "app")
	if r.code != 1 || !strings.Contains(r.stderr, "team_name_taken") || !strings.Contains(r.stderr, "rename this team") {
		t.Fatalf("team_name_taken: exit %d\n%s%s", r.code, r.stdout, r.stderr)
	}
	// A team keeps its hub.
	cli(t, "team", "setup", "crew", "--hub", "main", "--project", "app")
	r = cli(t, "team", "setup", "crew", "--hub", "lab", "--project", "app")
	if r.code != 1 || !strings.Contains(r.stderr, "team crew is on hub main, not lab") {
		t.Fatalf("other hub: exit %d\n%s%s", r.code, r.stdout, r.stderr)
	}
	// A hub that does not answer the read is reported, and nothing is
	// claimed about the grants.
	hub.mu.Lock()
	hub.down = true
	hub.mu.Unlock()
	r = cli(t, "team", "setup", "crew", "--hub", "main", "--project", "app")
	if r.code != 1 || !strings.Contains(r.stderr, "hub_unavailable") || strings.Contains(r.stdout, "granted") {
		t.Fatalf("hub down: exit %d\n%s%s", r.code, r.stdout, r.stderr)
	}
}

func TestTeamSetupUsage(t *testing.T) {
	serve(t)
	for _, args := range [][]string{{"team", "setup"}, {"team", "setup", "--hub", "main"}, {"team", "setup", "crew", "--hub", "main"},
		{"team", "setup", "crew", "--project", "app"}, {"team", "setup", "crew", "--hub", "main", "--project", ""},
		{"team", "setup", "crew", "--hub", "main", "--project", "app", "extra"},
		{"team", "setup", "crew", "--hub", "main", "--project", "has space"},
		{"team", "setup", "crew", "--hub", "main", "--project", "app;rm"}} {
		if r := cli(t, args...); r.code != 2 {
			t.Errorf("%v: exit %d, want 2", args, r.code)
		}
	}
}

// A team aicrewd holds as registered that the hub no longer knows is
// registered again before its grants are reported; a disabled profile is
// reported as the problem, with no grant command.
func TestTeamSetupHubForgotOrDisabled(t *testing.T) {
	hub := newSetupHub()
	serve(t, server.WithHub("main", "hub-a", hub))
	cli(t, "team", "setup", "crew", "--hub", "main", "--project", "app")
	hub.mu.Lock()
	hub.registered = map[string]string{}
	hub.grants["crew"] = []string{"app"}
	hub.mu.Unlock()
	r := cli(t, "team", "setup", "crew", "--hub", "main", "--project", "app")
	if r.code != 0 || !strings.Contains(r.stdout, "the hub does not know team crew: registering it again") ||
		!strings.Contains(r.stdout, "granted: app") || hub.registerCount() != 2 {
		t.Fatalf("forgotten: exit %d, %d registrations\n%s%s", r.code, hub.registerCount(), r.stdout, r.stderr)
	}
	hub.mu.Lock()
	hub.disabled = true
	hub.mu.Unlock()
	r = cli(t, "team", "setup", "crew", "--hub", "main", "--project", "app")
	if r.code != 1 || !strings.Contains(r.stderr, "disabled team crew's profile") || strings.Contains(r.stdout, "aimem identity team grant") {
		t.Fatalf("disabled: exit %d\n%s%s", r.code, r.stdout, r.stderr)
	}
}
