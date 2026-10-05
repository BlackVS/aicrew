package server

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

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
func (f *fakeHubTeams) ReadTeams(context.Context) ([]hubteams.Team, error) {
	return nil, errors.New("not used")
}

// grantingHub is a hub that grants every team the projects in grants, and
// answers team reads as team.read does; down makes it unreachable. It
// counts the reads.
type grantingHub struct {
	mu     sync.Mutex
	grants []string
	down   bool
	reads  int
	// listed are the teams a list of the hub's teams names; repoURL, when
	// set, is the URL the hub binds to every granted project.
	listed  []string
	repoURL string
}

func (h *grantingHub) set(down bool, grants ...string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.down, h.grants = down, grants
}

func (h *grantingHub) team(id string) hubteams.Team {
	t := hubteams.Team{TeamID: id, TeamName: "crew", Enabled: true, Projects: []hubteams.Project{}}
	for _, p := range h.grants {
		url := "https://git.example.test/crew/" + p + ".git"
		if h.repoURL != "" {
			url = h.repoURL
		}
		t.Projects = append(t.Projects, hubteams.Project{Project: p,
			Repository: &hubteams.Repository{Kind: "gitea", URL: url, Host: "git.example.test", Access: "write"}})
	}
	return t
}

func (h *grantingHub) CanRegister() bool { return false }
func (h *grantingHub) CanRead() bool     { return true }
func (h *grantingHub) Register(context.Context, string, string) (hubteams.Registration, error) {
	return hubteams.Registration{}, errors.New("not used")
}
func (h *grantingHub) ReadTeam(_ context.Context, id string) (hubteams.Team, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.reads++
	if h.down {
		return hubteams.Team{}, &hubteams.Error{Code: hubteams.CodeUnavailable}
	}
	return h.team(id), nil
}
func (h *grantingHub) ReadTeams(_ context.Context) ([]hubteams.Team, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.reads++
	if h.down {
		return nil, &hubteams.Error{Code: hubteams.CodeUnavailable}
	}
	var out []hubteams.Team
	for _, id := range h.listed {
		out = append(out, h.team(id))
	}
	return out, nil
}

func (h *grantingHub) readCount() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.reads
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

// One named hub still routes by hub ID: a challenge naming another hub is
// peer_unknown and its receipt never reaches the configured hub.
func TestOneNamedHubRoutesByHubID(t *testing.T) {
	var hits atomic.Int32
	hub := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(hub.Close)
	sum := sha256.Sum256(hub.Certificate().RawSubjectPublicKeyInfo)
	certFile, keyFile, _ := testCert(t)
	st, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "aicrew.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	redeem := filepath.Join(t.TempDir(), "redemption.token")
	writePrivate(t, redeem, "sample-redemption-credential")
	opFile, _ := operatorToken(t)
	cfg := Config{ListenAddr: "127.0.0.1:0", TLSCertFile: certFile, TLSKeyFile: keyFile, ServiceID: "aicrew-test",
		OperatorTokenFile: opFile, ShutdownTimeout: Duration(time.Second),
		AimemHubs: []AimemHub{{Name: "main", HubID: "hub-a", AimemConfig: AimemConfig{BaseURL: hub.URL,
			TLSTrustMode: "spki_sha256", TLSTrustValue: "sha256-" + base64.StdEncoding.EncodeToString(sum[:]),
			RedemptionTokenFile: redeem}}}}
	srv, err := New(cfg, st, slogDiscard())
	if err != nil {
		t.Fatal(err)
	}
	receipt := store.NewSecret("amr1_" + strings.Repeat("A", 43))
	_, err = srv.verifier.Redeem(context.Background(), store.RedeemRequest{ChallengeID: "ch-1", HubID: "hub-x",
		Receipt: receipt, RequestKey: "key-1"})
	if err == nil || !strings.Contains(err.Error(), "peer_unknown") || hits.Load() != 0 {
		t.Fatalf("another hub's challenge: %v, %d calls to the configured hub", err, hits.Load())
	}
	// The configured hub's own challenge reaches it.
	srv.verifier.Redeem(context.Background(), store.RedeemRequest{ChallengeID: "ch-2", HubID: "hub-a",
		Receipt: receipt, RequestKey: "key-2"})
	if hits.Load() != 1 {
		t.Fatalf("the hub's own challenge made %d calls", hits.Load())
	}
}

// A registration retry and a rename are serialized: the retry reads the
// team's name only once the rename, and its registration, are done, so it
// never sends the replaced name.
func TestRegisterRetryWaitsForRename(t *testing.T) {
	hub := &slowHubTeams{fakeHubTeams: fakeHubTeams{registered: map[string]string{}}, entered: make(chan string, 4),
		release: make(chan struct{})}
	e := setupAPI(t, WithHub("main", "hub-a", hub))
	tok := e.opToken
	var team opapi.Team
	close(hub.release)
	e.admin(t, http.MethodPost, opapi.TeamsPath, tok, opapi.TeamRequest{Name: "alpha", Hub: "main"}, &team)
	<-hub.entered
	hub.release = make(chan struct{})
	renamed := make(chan struct{})
	go func() {
		defer close(renamed)
		e.admin(t, http.MethodPost, opapi.TeamRenamePath, tok,
			opapi.TeamRenameRequest{TeamID: team.ID, ExpectedRevision: team.Revision, Name: "beta"}, nil)
	}()
	if got := <-hub.entered; got != "beta" {
		t.Fatalf("the rename registered %q", got)
	}
	retried := make(chan struct{})
	go func() {
		defer close(retried)
		e.admin(t, http.MethodPost, opapi.TeamRegisterPath, tok, opapi.IDRequest{ID: team.ID}, nil)
	}()
	select {
	case got := <-hub.entered:
		t.Fatalf("the retry registered %q while the rename was registering", got)
	case <-time.After(200 * time.Millisecond):
	}
	close(hub.release)
	<-renamed
	if got := <-hub.entered; got != "beta" {
		t.Fatalf("the retry registered %q after the rename", got)
	}
	<-retried
	if hub.registered[team.ID] != "beta" {
		t.Fatalf("the hub holds %q", hub.registered[team.ID])
	}
}

// slowHubTeams reports each registration's name, then holds it until
// release is closed.
type slowHubTeams struct {
	fakeHubTeams
	entered chan string
	release chan struct{}
}

func (f *slowHubTeams) Register(ctx context.Context, teamID, name string) (hubteams.Registration, error) {
	f.entered <- name
	<-f.release
	return f.fakeHubTeams.Register(ctx, teamID, name)
}

// An offer and a claim each read the team's grants from its hub, live: a
// revoked grant refuses the next step by name, a hub that does not answer
// refuses it as hub_unavailable even when the snapshot holds the grant, and
// nothing begins either way. Only the role that may take the step reads.
func TestOfferAndClaimReadTheHubLive(t *testing.T) {
	e := setupCoordination(t)
	ctx := context.Background()
	expires := time.Now().Add(time.Hour)
	attempts := func() int {
		t.Helper()
		db, err := sql.Open("sqlite", e.storePath)
		if err != nil {
			t.Fatal(err)
		}
		defer db.Close()
		var n int
		if err := db.QueryRow(`SELECT COUNT(*) FROM attempts`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}

	e.hub.set(false) // the hub revokes the team's only grant
	refused(t, e.call(t, e.lead.token, AttemptsPath, "offer-1", e.offerBody("task-1", expires)), http.StatusConflict, "project_not_granted")
	refused(t, e.call(t, e.indep.token, ClaimPath, "claim-1", e.claimBody("task-2")), http.StatusConflict, "project_not_granted")
	if tm, err := e.store.GetTeam(ctx, e.team.ID); err != nil || len(tm.Grants) != 0 || tm.GrantsState != store.GrantsEnabled {
		t.Fatalf("the snapshot after the revoking read = %+v, %v", tm, err)
	}

	seedGrants(t, e.store, e.team.ID)  // the snapshot holds the grant ...
	e.hub.set(true, "project-example") // ... and the hub does not answer
	got := e.call(t, e.lead.token, AttemptsPath, "offer-1", e.offerBody("task-1", expires))
	refused(t, got, http.StatusServiceUnavailable, "hub_unavailable")
	if got.header.Get("Retry-After") != "30" {
		t.Fatalf("Retry-After %q", got.header.Get("Retry-After"))
	}
	refused(t, e.call(t, e.indep.token, ClaimPath, "claim-1", e.claimBody("task-2")), http.StatusServiceUnavailable, "hub_unavailable")
	if n := attempts(); n != 0 {
		t.Fatalf("%d attempts began while the hub refused", n)
	}

	reads := e.hub.readCount()
	refused(t, e.call(t, e.worker.token, AttemptsPath, "offer-w", e.offerBody("task-1", expires)), http.StatusForbidden, "attempt_forbidden")
	if e.hub.readCount() != reads {
		t.Fatal("a worker's offer read the hub")
	}

	e.hub.set(false, "project-example")
	stepOf(t, e.call(t, e.lead.token, AttemptsPath, "offer-1", e.offerBody("task-1", expires)))
	stepOf(t, e.call(t, e.indep.token, ClaimPath, "claim-1", e.claimBody("task-2")))
	if e.hub.readCount() != reads+2 {
		t.Fatalf("%d reads for two steps", e.hub.readCount()-reads)
	}
}

// A team that names no hub, or a task on another hub than the team's, is
// granted nothing; a team whose hub has no team.read credential cannot be
// read.
func TestLiveGrantWithoutTheHub(t *testing.T) {
	e := setupAPI(t, WithHub("main", "hub-a", &grantingHub{grants: []string{"docs"}}), WithHub("noread", "hub-b", nil))
	ctx := context.Background()
	team := func(name, hub string) string {
		t.Helper()
		tm, err := e.store.CreateTeam(ctx, mustOperator(t), "team-"+name, store.NewTeam{Name: name, Hub: hub})
		if err != nil {
			t.Fatal(err)
		}
		return tm.ID
	}
	task := store.TaskRef{HubID: "hub-a", ProjectID: "docs", TaskID: "task-1"}
	if _, code := e.srv.liveGrant(ctx, team("bare", ""), task); code != "project_not_granted" {
		t.Fatalf("a team without a hub: %q", code)
	}
	main := team("main-crew", "main")
	if _, code := e.srv.liveGrant(ctx, main, task); code != "" {
		t.Fatalf("the granted project: %q", code)
	}
	other := task
	other.HubID = "hub-b"
	if _, code := e.srv.liveGrant(ctx, main, other); code != "project_not_granted" {
		t.Fatalf("a task on another hub: %q", code)
	}
	if _, code := e.srv.liveGrant(ctx, team("lab-crew", "noread"), other); code != "hub_unavailable" {
		t.Fatalf("a hub without team.read: %q", code)
	}
}

// The refresh reads each hub once and records every team of it: the
// granted projects of a listed team, nothing for a team the hub does not
// list. A hub that does not answer keeps the snapshot.
func TestRefreshGrants(t *testing.T) {
	hub := &grantingHub{grants: []string{"docs"}}
	e := setupAPI(t, WithHub("main", "hub-a", hub))
	ctx := context.Background()
	op := mustOperator(t)
	listed, err := e.store.CreateTeam(ctx, op, "listed", store.NewTeam{Name: "listed", Hub: "main"})
	if err != nil {
		t.Fatal(err)
	}
	unlisted, err := e.store.CreateTeam(ctx, op, "unlisted", store.NewTeam{Name: "unlisted", Hub: "main"})
	if err != nil {
		t.Fatal(err)
	}
	hub.mu.Lock()
	hub.listed = []string{listed.ID}
	hub.mu.Unlock()
	e.srv.refreshGrants(ctx)
	tm, err := e.store.GetTeam(ctx, listed.ID)
	if err != nil || len(tm.Grants) != 1 || tm.Grants[0].ProjectID != "docs" || tm.Grants[0].HubID != "hub-a" ||
		tm.Grants[0].Repository == nil || tm.GrantsState != store.GrantsEnabled {
		t.Fatalf("listed team = %+v, %v", tm, err)
	}
	if un, err := e.store.GetTeam(ctx, unlisted.ID); err != nil || un.GrantsState != store.GrantsNotRegistered || len(un.Grants) != 0 {
		t.Fatalf("unlisted team = %+v, %v", un, err)
	}
	if hub.readCount() != 1 {
		t.Fatalf("%d reads of one hub", hub.readCount())
	}
	hub.set(true)
	e.srv.refreshGrants(ctx)
	if tm, err := e.store.GetTeam(ctx, listed.ID); err != nil || len(tm.Grants) != 1 {
		t.Fatalf("a hub that did not answer changed the snapshot: %+v, %v", tm, err)
	}
}

func mustOperator(t *testing.T) store.Caller {
	t.Helper()
	op, err := store.OperatorCaller("op-test")
	if err != nil {
		t.Fatal(err)
	}
	return op
}

// aicrew team register --hub names the hub of a team created before teams
// named one, then registers it; a team keeps its hub.
func TestRegisterNamesTheHubOnce(t *testing.T) {
	hub := &fakeHubTeams{registered: map[string]string{}}
	e := setupAPI(t, WithHub("main", "hub-a", hub), WithHub("lab", "hub-b", hub))
	tok := e.opToken
	var team opapi.Team
	if got := e.admin(t, http.MethodPost, opapi.TeamRegisterPath, tok, opapi.TeamRegisterRequest{ID: e.teamID, Hub: "main"}, &team); got.status != http.StatusOK ||
		team.Hub != "main" || team.Registration == nil || team.Registration.State != "registered" || hub.registered[e.teamID] != "crew" {
		t.Fatalf("register with a hub: %d %s", got.status, got.raw)
	}
	adminRefused(t, e.admin(t, http.MethodPost, opapi.TeamRegisterPath, tok, opapi.TeamRegisterRequest{ID: e.teamID, Hub: "lab"}, nil),
		http.StatusBadRequest, opapi.CodeInvalid)
	adminRefused(t, e.admin(t, http.MethodPost, opapi.TeamRegisterPath, tok, opapi.TeamRegisterRequest{ID: e.teamID, Hub: "nowhere"}, nil),
		http.StatusBadRequest, opapi.CodeInvalid)
	if got := e.admin(t, http.MethodPost, opapi.TeamRegisterPath, tok, opapi.TeamRegisterRequest{ID: e.teamID, Hub: "main"}, &team); got.status != http.StatusOK ||
		team.Hub != "main" {
		t.Fatalf("the same hub again: %d %s", got.status, got.raw)
	}
}

// A hub's project that no task can name, or one named twice, is left out of
// the snapshot; the team keeps its other grants.
func TestGrantsReadSkipsUnusableProjects(t *testing.T) {
	team := hubteams.Team{TeamID: "t1", TeamName: "crew", Enabled: true, Projects: []hubteams.Project{
		{Project: "docs"}, {Project: "has space"}, {Project: "docs"},
		{Project: "api", Repository: &hubteams.Repository{Kind: "git", URL: strings.Repeat("u", 4096)}},
	}}
	read, ok := grantsRead("hub-a", team, nil, time.Now())
	if !ok || read.State != store.GrantsEnabled || len(read.Grants) != 1 || read.Grants[0].ProjectID != "docs" {
		t.Fatalf("read = %+v, %v", read, ok)
	}
	if off, ok := grantsRead("hub-a", hubteams.Team{TeamID: "t1", Projects: []hubteams.Project{}}, nil, time.Now()); !ok ||
		off.State != store.GrantsDisabled {
		t.Fatalf("a disabled profile = %+v, %v", off, ok)
	}
	for code, want := range map[string]string{"not_found": store.GrantsNotRegistered, "profile_disabled": store.GrantsDisabled} {
		if r, ok := grantsRead("hub-a", hubteams.Team{}, &hubteams.Error{Code: code}, time.Now()); !ok || r.State != want {
			t.Fatalf("%s = %+v, %v", code, r, ok)
		}
	}
	for _, code := range []string{hubteams.CodeUnavailable, "peer_forbidden", "rate_limited"} {
		if _, ok := grantsRead("hub-a", hubteams.Team{}, &hubteams.Error{Code: code}, time.Now()); ok {
			t.Fatalf("%s was taken for an answer", code)
		}
	}
}

// An offer or a claim names the repository the hub binds to the task's
// project: another kind, URL or access, or a project the hub binds no
// repository to, is refused repository_mismatch and nothing begins.
func TestStepsNameTheHubsRepository(t *testing.T) {
	e := setupCoordination(t)
	expires := time.Now().Add(time.Hour)
	for name, change := range map[string]func(map[string]string){
		"kind":   func(r map[string]string) { r["kind"] = "github" },
		"url":    func(r map[string]string) { r["url"] = "https://git.example.test/crew/other.git" },
		"access": func(r map[string]string) { r["access"] = "read" },
	} {
		body := e.offerBody("task-1", expires)
		repo := repositoryJSON("task-1")
		change(repo)
		body["repository"] = repo
		refused(t, e.call(t, e.lead.token, AttemptsPath, "offer-"+name, body), http.StatusConflict, "repository_mismatch")
		claim := e.claimBody("task-2")
		claim["repository"] = repo
		refused(t, e.call(t, e.indep.token, ClaimPath, "claim-"+name, claim), http.StatusConflict, "repository_mismatch")
	}
	// A body in the shape before the repository object is refused as such.
	old := e.offerBody("task-1", expires)
	delete(old, "repository")
	old["base_commit"], old["branch"] = "base-1", "work/task-1"
	refused(t, e.call(t, e.lead.token, AttemptsPath, "offer-old", old), http.StatusBadRequest, "invalid_request")

	// The hub changes the project's repository: the running attempt keeps
	// what it recorded, and the next offer must name the new repository.
	id, _ := e.offerAndSettle(t, "offer-1", "task-1", expires)
	moved := "https://git.example.test/crew/project-example-2.git"
	e.hub.mu.Lock()
	e.hub.repoURL = moved
	e.hub.mu.Unlock()
	refused(t, e.call(t, e.lead.token, AttemptsPath, "offer-3", e.offerBody("task-3", expires)), http.StatusConflict, "repository_mismatch")
	a, err := e.store.GetAttempt(context.Background(), id)
	if err != nil || a.Repository != coordRepo || a.BaseCommit != "base-1" || a.Branch != "work/task-1" {
		t.Fatalf("the recorded repository after the hub moved it = %+v, %v", a.Repository, err)
	}
	claim := e.claimBody("task-3")
	repo := repositoryJSON("task-3")
	repo["url"] = moved
	claim["repository"] = repo
	got := e.call(t, e.indep.token, ClaimPath, "claim-3", claim)
	if c, err := e.store.GetAttempt(context.Background(), attemptOf(t, got)); err != nil || c.Repository.URL != moved {
		t.Fatalf("the claim after the move = %+v, %v", c.Repository, err)
	}
}

// The refresh blocks the team's open attempt when the hub revokes its
// project, tells the worker, and a re-grant clears the block; the attempt's
// state and hold never change.
func TestRefreshBlocksAndUnblocks(t *testing.T) {
	e := setupCoordination(t)
	ctx := context.Background()
	id, _ := e.offerAndSettle(t, "offer-1", "task-1", time.Now().Add(time.Hour))
	before, err := e.store.GetAttempt(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	e.hub.mu.Lock()
	e.hub.listed, e.hub.grants = []string{e.team.ID}, nil
	e.hub.mu.Unlock()
	e.srv.refreshGrants(ctx)
	a, err := e.store.GetAttempt(ctx, id)
	if err != nil || a.Blocked == nil || a.Blocked.Reason != store.BlockedGrantRevoked || a.State != before.State ||
		a.ReservationID != before.ReservationID {
		t.Fatalf("after the revoke = %+v %+v, %v", a.Blocked, a.State, err)
	}
	told := false
	for _, m := range inboxOf(t, e.get(t, e.worker.token, InboxPath)) {
		if m.Kind == "lifecycle" && m.AttemptID == id && strings.Contains(m.Text, "is blocked") {
			told = true
		}
	}
	if !told {
		t.Fatal("the worker was not told of the block")
	}
	e.hub.set(false, "project-example")
	e.srv.refreshGrants(ctx)
	if a, err := e.store.GetAttempt(ctx, id); err != nil || a.Blocked != nil {
		t.Fatalf("after the re-grant = %+v, %v", a.Blocked, err)
	}
}
