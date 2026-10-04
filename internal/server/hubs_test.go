package server

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/BlackVS/aicrew/internal/hubteams"
	"github.com/BlackVS/aicrew/internal/opapi"
	"github.com/BlackVS/aicrew/internal/store"
)

func withHubs(hubs string) string {
	return strings.Replace(validConfig, "}", `,"aimem_hubs":`+hubs+`}`, 1)
}

func hubBlock(name, id, extra string) string {
	return `{"name":"` + name + `","hub_id":"` + id + `","base_url":"https://` + name + `.example","tls_trust_mode":"ca_dns",
		"tls_trust_value":"` + name + `.example","redemption_token_file":"/etc/aicrew/` + name + `.redemption"` + extra + `}`
}

// Named aimem blocks: two are accepted, each with its own hub ID and team
// credentials; the legacy block is read as "default"; the refusals name the
// field.
func TestConfigNamedHubs(t *testing.T) {
	two := "[" + hubBlock("main", "hub-a", `,"read_token_file":"/etc/aicrew/main.read","team_read_token_file":"/etc/aicrew/main.team-read",
		"team_register_token_file":"/etc/aicrew/main.team-register"`) + "," + hubBlock("lab", "hub-b", "") + "]"
	c, err := ParseConfig([]byte(withHubs(two)))
	if err != nil {
		t.Fatal(err)
	}
	hubs := c.Hubs()
	if len(hubs) != 2 || hubs[0].Name != "main" || hubs[0].HubID != "hub-a" || hubs[0].TeamReadTokenFile == "" || hubs[1].BaseURL != "https://lab.example" {
		t.Fatalf("hubs %+v", hubs)
	}
	legacy, err := ParseConfig([]byte(withAimem(validAimem)))
	if err != nil || len(legacy.Hubs()) != 1 || legacy.Hubs()[0].Name != LegacyHubName || legacy.Hubs()[0].HubID != "" {
		t.Fatalf("legacy %+v %v", legacy.Hubs(), err)
	}
	for name, cfg := range map[string]string{
		"both forms":     strings.Replace(withHubs("["+hubBlock("main", "hub-a", "")+"]"), "}", `,"aimem":`+validAimem+`}`, 1),
		"no hub ID":      withHubs("[" + hubBlock("main", "", "") + "]"),
		"same name":      withHubs("[" + hubBlock("main", "hub-a", "") + "," + hubBlock("main", "hub-b", "") + "]"),
		"same hub ID":    withHubs("[" + hubBlock("main", "hub-a", "") + "," + hubBlock("lab", "hub-a", "") + "]"),
		"bad name":       withHubs("[" + hubBlock("Main", "hub-a", "") + "]"),
		"two readers":    withHubs("[" + hubBlock("main", "hub-a", `,"read_token_file":"/r1"`) + "," + hubBlock("lab", "hub-b", `,"read_token_file":"/r2"`) + "]"),
		"same team file": withHubs("[" + hubBlock("main", "hub-a", `,"team_read_token_file":"/t","team_register_token_file":"/t"`) + "]"),
	} {
		if _, err := ParseConfig([]byte(cfg)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

// A redemption reaches its hub's verifier by the challenge's hub ID; an
// unknown hub is peer_unknown and reaches none.
func TestHubVerifierRoutes(t *testing.T) {
	a, b := &countVerifier{}, &countVerifier{}
	m := hubVerifier{"hub-a": a, "hub-b": b}
	m.Redeem(context.Background(), store.RedeemRequest{HubID: "hub-b", ChallengeID: "c1"})
	if a.calls != 0 || b.calls != 1 {
		t.Fatalf("routed to a %d, b %d", a.calls, b.calls)
	}
	if _, err := m.Redeem(context.Background(), store.RedeemRequest{HubID: "hub-x"}); err == nil || !strings.Contains(err.Error(), "peer_unknown") {
		t.Fatalf("unknown hub: %v", err)
	}
}

type countVerifier struct{ calls int }

func (c *countVerifier) Redeem(context.Context, store.RedeemRequest) (store.VerifiedIdentity, error) {
	c.calls++
	return store.VerifiedIdentity{}, nil
}

// fakeHubTeams registers by name: "taken" is team_name_taken, "down" is an
// unreachable hub.
type fakeHubTeams struct{ registered map[string]string }

func (f *fakeHubTeams) CanRegister() bool { return true }
func (f *fakeHubTeams) CanRead() bool     { return true }
func (f *fakeHubTeams) Register(_ context.Context, teamID, name string) (hubteams.Registration, error) {
	switch name {
	case "taken":
		return hubteams.Registration{}, &hubteams.Error{Code: "team_name_taken"}
	case "down":
		return hubteams.Registration{}, &hubteams.Error{Code: hubteams.CodeUnavailable}
	}
	f.registered[teamID] = name
	return hubteams.Registration{ProfileID: "p", TeamID: teamID, TeamName: name}, nil
}
func (f *fakeHubTeams) ReadTeam(context.Context, string) (hubteams.Team, error) {
	return hubteams.Team{}, errors.New("not used")
}

// team create --hub registers the team on that hub and records the
// outcome; rename re-registers; a refused or unreachable hub is recorded
// with what to do and the team still exists; register retries; an unknown
// or legacy hub is refused before anything is created.
func TestTeamRegistration(t *testing.T) {
	hub := &fakeHubTeams{registered: map[string]string{}}
	e := setupAPI(t, WithHub("main", "hub-a", hub), WithHub("old", "", nil))
	tok := e.opToken
	var team opapi.Team
	if got := e.admin(t, http.MethodPost, opapi.TeamsPath, tok, opapi.TeamRequest{Name: "alpha", Hub: "main"}, &team); got.status != http.StatusCreated ||
		team.Hub != "main" || team.Registration == nil || team.Registration.State != "registered" || hub.registered[team.ID] != "alpha" {
		t.Fatalf("create: %d %s", got.status, got.raw)
	}
	if got := e.admin(t, http.MethodPost, opapi.TeamRenamePath, tok,
		opapi.TeamRenameRequest{TeamID: team.ID, ExpectedRevision: team.Revision, Name: "beta"}, &team); got.status != http.StatusOK ||
		team.Registration.Name != "beta" || hub.registered[team.ID] != "beta" {
		t.Fatalf("rename: %d %s", got.status, got.raw)
	}
	var taken opapi.Team
	if got := e.admin(t, http.MethodPost, opapi.TeamsPath, tok, opapi.TeamRequest{Name: "taken", Hub: "main"}, &taken); got.status != http.StatusCreated ||
		taken.Registration.State != "team_name_taken" || !strings.Contains(taken.Registration.Detail, "aicrew team register") {
		t.Fatalf("taken: %d %s", got.status, got.raw)
	}
	var down opapi.Team
	e.admin(t, http.MethodPost, opapi.TeamsPath, tok, opapi.TeamRequest{Name: "down", Hub: "main"}, &down)
	if down.Registration == nil || down.Registration.State != hubteams.CodeUnavailable {
		t.Fatalf("down: %+v", down.Registration)
	}
	// Renamed to a free name, the retry registers it.
	e.admin(t, http.MethodPost, opapi.TeamRenamePath, tok, opapi.TeamRenameRequest{TeamID: taken.ID, ExpectedRevision: taken.Revision, Name: "gamma"}, &taken)
	var retried opapi.Team
	if got := e.admin(t, http.MethodPost, opapi.TeamRegisterPath, tok, opapi.IDRequest{ID: taken.ID}, &retried); got.status != http.StatusOK ||
		retried.Registration.State != "registered" || hub.registered[taken.ID] != "gamma" {
		t.Fatalf("retry: %d %s", got.status, got.raw)
	}
	before, _ := e.store.ListTeams(context.Background())
	for _, req := range []opapi.TeamRequest{{Name: "x1", Hub: "nowhere"}, {Name: "x2", Hub: "old"}} {
		adminRefused(t, e.admin(t, http.MethodPost, opapi.TeamsPath, tok, req, nil), http.StatusBadRequest, opapi.CodeInvalid)
	}
	after, _ := e.store.ListTeams(context.Background())
	if len(after) != len(before) {
		t.Fatal("a refused hub created a team")
	}
	// A team without a hub cannot be registered.
	adminRefused(t, e.admin(t, http.MethodPost, opapi.TeamRegisterPath, tok, opapi.IDRequest{ID: e.teamID}, nil), http.StatusConflict, opapi.CodeInvalid)
}
