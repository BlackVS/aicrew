package agent

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/BlackVS/aicrew/internal/forge"
	"github.com/BlackVS/aicrew/internal/store"
)

// fakeAccess answers each repository path with an access, or an error.
type fakeAccess struct {
	access map[string]string
	errs   map[string]error
	token  string
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

// The launcher's capabilities operation reads the team's requirements,
// verifies them and reports them to aicrewd as the session, which then
// holds them; the answer is the check's rows.
func TestLauncherReportsCapabilities(t *testing.T) {
	s := setupSteps(t, store.RoleWorker)
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
