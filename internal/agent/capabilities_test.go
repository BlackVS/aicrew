package agent

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"github.com/BlackVS/aicrew/internal/forge"
	"github.com/BlackVS/aicrew/internal/hubteams"
	"github.com/BlackVS/aicrew/internal/store"
)

// fakeAccess answers each repository path with an access, or an error.
type fakeAccess struct {
	access map[string]string
	errs   map[string]error
	token  string
	// rights are the token's rights by path; a path without them holds
	// every right.
	rights map[string][]forge.RightCheck
}

func (f fakeAccess) TokenRights(_ context.Context, _ string, _ forge.Kind, _, path string) []forge.RightCheck {
	if r, ok := f.rights[path]; ok {
		return r
	}
	return []forge.RightCheck{{Right: "repo", State: forge.RightPresent}}
}

func (f fakeAccess) RepositoryAccess(_ context.Context, _ string, _ forge.Kind, token, path string) (string, error) {
	if token != f.token {
		return "", forge.ErrRejected
	}
	if err := f.errs[path]; err != nil {
		return "", err
	}
	if a, ok := f.access[path]; ok {
		return a, nil
	}
	return "", forge.ErrNotFound
}

func requirement(project, url, access string) Requirement {
	r := Requirement{HubID: "hub-a", ProjectID: project}
	r.Repository.Kind, r.Repository.URL, r.Repository.Access = "github", url, access
	return r
}

// A capability check verifies each requirement with the home's own
// credential for its host, as the forge reports the access; only a verified
// repository is reported, with the access the forge reports, and nothing
// it reads is a blocker.
func TestVerifyCapabilities(t *testing.T) {
	home := forgeHome(t, "gh") // a credential for github.com only
	api := fakeAccess{token: "gh",
		access: map[string]string{"team/app": "write", "team/docs": "read", "team/none": ""},
		errs:   map[string]error{"team/down": forge.ErrUnreachable}}
	rows, caps := VerifyCapabilities(context.Background(), home, api, []Requirement{
		requirement("app", "https://github.com/team/app.git", "write"),
		requirement("docs", "https://github.com/team/docs.git", "write"),
		requirement("read-docs", "https://github.com/team/docs.git", "read"),
		requirement("none", "https://github.com/team/none.git", "read"),
		requirement("hidden", "https://github.com/team/hidden.git", "read"),
		requirement("down", "https://github.com/team/down.git", "read"),
		requirement("lab", "https://gitlab.example.org/team/lab.git", "read"),
	})
	want := map[string]string{"app": CapabilityVerified, "docs": CapabilityInsufficient, "read-docs": CapabilityVerified,
		"none": CapabilityInsufficient, "hidden": CapabilityRefused, "down": CapabilityUnreachable, "lab": CapabilityMissing}
	for _, r := range rows {
		if r.State != want[r.Project] {
			t.Errorf("%s: %s (%s), want %s", r.Project, r.State, r.Detail, want[r.Project])
		}
	}
	if len(caps) != 1 || caps[0].Host != "github.com" || caps[0].Account == "" {
		t.Fatalf("capabilities = %+v", caps)
	}
	got := map[string]string{}
	for _, r := range caps[0].Repositories {
		got[r.URL] = r.Access
	}
	if len(got) != 2 || got["https://github.com/team/app.git"] != "write" || got["https://github.com/team/docs.git"] != "read" {
		t.Fatalf("reported repositories = %v", got)
	}
	raw, _ := json.Marshal(rows)
	if strings.Contains(string(raw), `"gh"`) {
		t.Fatal("a row carries the token")
	}
}

// A write requirement also reads what the token itself may do, apart from
// the account's access: a right it lacks makes the row insufficient and
// reports the repository as read, a right the forge does not show is said
// to be not checked, and a read refused for a scope names it.
func TestVerifyCapabilitiesTokenRights(t *testing.T) {
	home := forgeHome(t, "gh")
	api := fakeAccess{token: "gh",
		access: map[string]string{"team/full": "write", "team/noissue": "write", "team/fine": "write", "team/docs": "write"},
		errs:   map[string]error{"team/noread": &forge.ScopeError{Scopes: []string{"read:repository"}}},
		rights: map[string][]forge.RightCheck{
			"team/noissue": {{Right: "write:repository", State: forge.RightPresent},
				{Right: "write:issue", State: forge.RightMissing, Detail: "refused"}},
			"team/fine": {{Right: "repo", State: forge.RightNotChecked, Detail: "GitHub names no scopes"}},
			"team/docs": {{Right: "repo", State: forge.RightMissing}},
		}}
	rows, caps := VerifyCapabilities(context.Background(), home, api, []Requirement{
		requirement("full", "https://github.com/team/full.git", "write"),
		requirement("noissue", "https://github.com/team/noissue.git", "write"),
		requirement("fine", "https://github.com/team/fine.git", "write"),
		requirement("read-docs", "https://github.com/team/docs.git", "read"),
		requirement("noread", "https://github.com/team/noread.git", "read"),
	})
	type want struct{ state, access, token, detail string }
	wants := map[string]want{
		"full":      {CapabilityVerified, "write", TokenVerified, ""},
		"noissue":   {CapabilityInsufficient, "write", TokenInsufficient, "write:issue"},
		"fine":      {CapabilityVerified, "write", TokenNotChecked, "GitHub names no scopes"},
		"read-docs": {CapabilityVerified, "write", "", ""},
		"noread":    {CapabilityInsufficient, "", "", "read:repository"},
	}
	for _, r := range rows {
		w := wants[r.Project]
		if r.State != w.state || r.Access != w.access || r.Token != w.token ||
			!strings.Contains(r.Detail+r.TokenDetail, w.detail) {
			t.Errorf("%s: %+v, want %+v", r.Project, r, w)
		}
	}
	got := map[string]string{}
	for _, r := range caps[0].Repositories {
		got[r.URL] = r.Access
	}
	if got["https://github.com/team/noissue.git"] != "read" || got["https://github.com/team/full.git"] != "write" ||
		got["https://github.com/team/fine.git"] != "write" {
		t.Fatalf("reported repositories = %v", got)
	}
}

// reportSession answers the requirements and keeps what the report sends.
type reportSession struct {
	reqs []Requirement
	sent []byte
}

func (s *reportSession) Read(_ context.Context, _, path string) (json.RawMessage, error) {
	return json.Marshal(map[string]any{"projects": s.reqs})
}

func (s *reportSession) LocalStep(_ context.Context, _, _, path string, body []byte) (json.RawMessage, error) {
	if path == capabilitiesPath {
		s.sent = append([]byte(nil), body...)
	}
	return json.RawMessage(`{}`), nil
}

// What a report sends aicrewd: a repository whose token the forge names as
// lacking a write right is read; one whose token's rights are not checked
// keeps the account's write.
func TestReportCapabilitiesLowersOnlyNamedMissing(t *testing.T) {
	home := forgeHome(t, "gh")
	api := fakeAccess{token: "gh",
		access: map[string]string{"team/missing": "write", "team/unknown": "write", "team/full": "write"},
		rights: map[string][]forge.RightCheck{
			"team/missing": {{Right: "write:issue", State: forge.RightMissing}},
			"team/unknown": {{Right: "write:issue", State: forge.RightNotChecked, Detail: "status 500"}},
		}}
	s := &reportSession{reqs: []Requirement{
		requirement("missing", "https://github.com/team/missing.git", "write"),
		requirement("unknown", "https://github.com/team/unknown.git", "write"),
		requirement("full", "https://github.com/team/full.git", "write"),
	}}
	if _, err := ReportCapabilities(context.Background(), home, s, api, "session"); err != nil {
		t.Fatal(err)
	}
	var sent struct {
		Capabilities []Capability `json:"capabilities"`
	}
	if err := json.Unmarshal(s.sent, &sent); err != nil || len(sent.Capabilities) != 1 {
		t.Fatalf("sent %s: %v", s.sent, err)
	}
	got := map[string]string{}
	for _, r := range sent.Capabilities[0].Repositories {
		got[r.URL] = r.Access
	}
	want := map[string]string{"https://github.com/team/missing.git": "read",
		"https://github.com/team/unknown.git": "write", "https://github.com/team/full.git": "write"}
	if len(got) != len(want) {
		t.Fatalf("sent %v", got)
	}
	for u, a := range want {
		if got[u] != a {
			t.Errorf("%s sent as %q, want %q", u, got[u], a)
		}
	}
}

// The launcher's capabilities operation reads the team's requirements,
// verifies them and reports them to aicrewd as the session, which then
// holds them; the answer is the check's rows.
func TestLauncherReportsCapabilities(t *testing.T) {
	// The step socket needs a short home: macOS caps a socket path at 104
	// bytes, and its t.TempDir() is longer.
	s := setupStepsWith(t, crewOptions{role: store.RoleWorker, steps: true, shortHome: true})
	provisionTestForge(t, s.cfg.Home, "git.example.test", "gt")
	defer swapAccess(fakeAccess{token: "gt", access: map[string]string{"crew/project-t": "write"}})()
	s.serve(t)
	var answers []byte
	ans := s.call(t, &answers, StepCall{Op: "capabilities"})
	var rows []CapabilityRow
	if !ans.OK || json.Unmarshal(ans.Result, &rows) != nil || len(rows) != 1 || rows[0].State != CapabilityVerified ||
		rows[0].Project != "project-t" {
		t.Fatalf("capabilities: %+v %s", ans, ans.Result)
	}
	caps, at, err := s.store.AgentCapabilities(context.Background(), s.agentID)
	if err != nil || at == nil || len(caps) != 1 || caps[0].Repositories[0].URL != testRepository["url"] ||
		caps[0].Repositories[0].Access != "write" {
		t.Fatalf("aicrewd holds %+v %v %v", caps, at, err)
	}
}

// forgeHome is a new home holding a github.com credential with token.
func forgeHome(t *testing.T, token string) string {
	t.Helper()
	home := t.TempDir()
	provisionTestForge(t, home, "github.com", token)
	return home
}

// provisionTestForge records a credential with token for host in home's
// agent.json, as join --cred does: GitHub for github.com, Gitea otherwise.
func provisionTestForge(t *testing.T, home, host, token string) {
	t.Helper()
	f := newFakeForge()
	f.kinds[host] = forge.Gitea
	if host == "github.com" {
		f.kinds[host] = forge.GitHub
	}
	f.ids[token] = forge.Identity{Account: "member", Name: "member", CommitEmail: "member@example.test"}
	doc, _, err := readAgentDoc(home)
	if err != nil {
		t.Fatal(err)
	}
	if doc.top == nil {
		doc = agentDoc{top: map[string]json.RawMessage{}, aicrew: map[string]json.RawMessage{}}
	}
	toks, err := readForgeTokens([]ForgeCred{{host, tokenFile(t, t.TempDir(), "tok", token)}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := provisionForge(context.Background(), home, &doc, toks, f, new(strings.Builder)); err != nil {
		t.Fatal(err)
	}
	if err := doc.write(home); err != nil {
		t.Fatal(err)
	}
}

// swapAccess makes api the forge the launcher's capability checks read,
// until the returned function restores the real one.
func swapAccess(api AccessAPI) func() {
	old := newAccessAPI
	newAccessAPI = func() AccessAPI { return api }
	return func() { newAccessAPI = old }
}

// The launcher records every project granted to the team in the home
// (3a4b), with the home's hub alias: one the hub grants without a
// repository too, which the requirements leave out. A grant the hub adds is
// recorded at the next read.
func TestLauncherRecordsTheTeamsProjects(t *testing.T) {
	ctx := context.Background()
	s := setupStepsWith(t, crewOptions{role: store.RoleCoordinator, steps: true, shortHome: true})
	s.d.Session.(*Engine).Cfg.AimemHub = "main"
	defer swapAccess(fakeAccess{})()
	srv := s.serve(t)
	if err := srv.RecordTeam(ctx); err != nil {
		t.Fatal(err)
	}
	rec, ok, err := LoadTeam(s.cfg.Home)
	if err != nil || !ok || rec.HubAlias != "main" || len(rec.Projects) != 1 ||
		rec.Projects[0].HubID != "hub-test" || rec.Projects[0].ProjectID != "project-t" ||
		rec.Projects[0].Repository == nil || rec.Projects[0].Repository.URL != testRepository["url"] || rec.ReadAt.IsZero() {
		t.Fatalf("the team record: %+v %v %v", rec, ok, err)
	}
	old := hubProjects
	hubProjects = func() []hubteams.Project { return append(old(), hubteams.Project{Project: "project-docs"}) }
	defer func() { hubProjects = old }()
	if err := srv.RecordTeam(ctx); err != nil {
		t.Fatal(err)
	}
	rec, _, err = LoadTeam(s.cfg.Home)
	byID := map[string]TeamProject{}
	for _, p := range rec.Projects {
		byID[p.ProjectID] = p
	}
	if docs, ok := byID["project-docs"]; err != nil || len(rec.Projects) != 2 || !ok || docs.Repository != nil ||
		byID["project-t"].Repository == nil {
		t.Fatalf("the team record after a grant without a repository: %+v %v", rec, err)
	}
	// The requirements still name only the projects a home must verify.
	rows, err := srv.ReportCapabilities(ctx)
	if err != nil || len(rows) != 1 || rows[0].Project != "project-t" {
		t.Fatalf("the requirements' rows: %+v %v", rows, err)
	}
	if st, ok, err := LoadState(s.cfg.Home); err != nil || !ok || st.Role != string(store.RoleCoordinator) {
		t.Fatalf("the session record's role: %+v %v %v", st, ok, err)
	}
	// The toolchain the operator selects for a project is recorded with it
	// at the next read (pilot finding #18).
	if _, err := s.store.SetTeamToolchain(ctx, s.operator, "toolchain-1", s.teamID, "project-t", "dev-go"); err != nil {
		t.Fatal(err)
	}
	if err := srv.RecordTeam(ctx); err != nil {
		t.Fatal(err)
	}
	if names, unknown, err := teamToolchains(s.cfg.Home); err != nil || !slices.Equal(names, []string{"dev-go"}) || unknown != nil {
		t.Fatalf("the recorded toolchains: %q %q %v", names, unknown, err)
	}
}
