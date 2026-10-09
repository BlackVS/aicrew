package architect

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/BlackVS/aicrew/internal/managedfiles"
	"github.com/BlackVS/aicrew/internal/privatefile"
)

func read(t *testing.T, dir, rel string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(rel)))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func actions(rep Report) map[string]string {
	m := map[string]string{}
	for _, c := range rep.Changes {
		m[c.Path] = c.Action
	}
	return m
}

// The first init writes every file as Files describes it, creates creds/
// owner-only and records each managed write's digest.
func TestInitWritesTheDirectory(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "architect")
	o := Options{Dir: dir, Projects: []string{"crew-app", "crew-ops"}, Hub: "main"}
	rep, err := Init(o)
	if err != nil || rep.Status != "ready" {
		t.Fatalf("init: %+v, %v", rep, err)
	}
	want := []string{"AGENTS.md", "CLAUDE.md", "docs/ARCHITECT.md", ".claude/settings.json", ".mcp.json",
		"docs/NOTES.md", ".aimem.json", ".claude/commands/arch-plan.md", ".claude/commands/arch-task.md",
		".claude/commands/arch-ready.md", ".claude/commands/arch-escalations.md"}
	var got []string
	for _, f := range Files(o) {
		got = append(got, f.Path)
		if a := actions(rep)[f.Path]; a != "create" {
			t.Errorf("%s: %s, want create", f.Path, a)
		}
		if c := read(t, dir, f.Path); c != f.Content {
			t.Errorf("%s differs from its generated content", f.Path)
		}
	}
	if !slices.Equal(got, want) {
		t.Fatalf("files %v, want %v", got, want)
	}
	if err := privatefile.CheckDir(filepath.Join(dir, "creds")); err != nil {
		t.Fatalf("creds/ is not owner-only: %v", err)
	}
	var rec struct {
		Layout   int               `json:"layout"`
		Projects []string          `json:"projects"`
		Hub      string            `json:"hub"`
		Managed  map[string]string `json:"managed"`
	}
	if err := json.Unmarshal([]byte(read(t, dir, recordFile)), &rec); err != nil {
		t.Fatal(err)
	}
	if rec.Layout != Layout || !slices.Equal(rec.Projects, o.Projects) || rec.Hub != "main" {
		t.Fatalf("record %+v", rec)
	}
	for _, f := range Files(o) {
		_, recorded := rec.Managed[f.Path]
		if recorded != f.Managed || (f.Managed && rec.Managed[f.Path] != managedfiles.Digest([]byte(f.Content))) {
			t.Errorf("%s: recorded %v (%s), managed %v", f.Path, recorded, rec.Managed[f.Path], f.Managed)
		}
	}
	var binding map[string]string
	if json.Unmarshal([]byte(read(t, dir, ".aimem.json")), &binding) != nil ||
		len(binding) != 2 || binding["project"] != "crew-app" || binding["hub"] != "main" {
		t.Fatalf(".aimem.json: %s", read(t, dir, ".aimem.json"))
	}
	var mcp struct {
		MCPServers map[string]struct {
			Command string            `json:"command"`
			Args    []string          `json:"args"`
			Env     map[string]string `json:"env"`
		} `json:"mcpServers"`
	}
	if json.Unmarshal([]byte(read(t, dir, ".mcp.json")), &mcp) != nil || len(mcp.MCPServers) != 1 ||
		mcp.MCPServers["aimem"].Command != "aimem" || !slices.Equal(mcp.MCPServers["aimem"].Args, []string{"mcp"}) ||
		mcp.MCPServers["aimem"].Env != nil {
		t.Fatalf(".mcp.json names another installation: %s", read(t, dir, ".mcp.json"))
	}
}

// The deny rules cover the directory's creds/ through the tools and the
// shells, exactly.
func TestSettingsDenyTheCredentials(t *testing.T) {
	var s struct {
		Permissions struct {
			Deny  []string `json:"deny"`
			Allow []string `json:"allow"`
		} `json:"permissions"`
		Hooks any `json:"hooks"`
		Env   any `json:"env"`
	}
	if err := json.Unmarshal([]byte(settingsJSON(Options{})), &s); err != nil {
		t.Fatal(err)
	}
	want := []string{"Read(/creds/**)", "Edit(/creds/**)", "Bash(*creds/*)", `Bash(*creds\*)`,
		"PowerShell(*creds/*)", `PowerShell(*creds\*)`}
	if !slices.Equal(s.Permissions.Deny, want) || s.Permissions.Allow != nil || s.Hooks != nil || s.Env != nil {
		t.Fatalf("settings: %+v", s)
	}
}

// A rerun follows the rule: unchanged files stay, a managed file unchanged
// since its last write is updated, an edited one gets .aicrew-new, files
// written once are never changed, and nothing unknown is touched.
func TestInitRerunRules(t *testing.T) {
	dir := t.TempDir()
	o := Options{Dir: dir, Projects: []string{"crew-app"}}
	if _, err := Init(o); err != nil {
		t.Fatal(err)
	}
	rep, err := Init(o)
	if err != nil || rep.Status != "ready" {
		t.Fatalf("rerun: %+v, %v", rep, err)
	}
	for _, f := range Files(o) {
		want := "unchanged"
		if !f.Managed {
			want = "kept"
		}
		if a := actions(rep)[f.Path]; a != want {
			t.Errorf("rerun %s: %s, want %s", f.Path, a, want)
		}
	}

	os.WriteFile(filepath.Join(dir, "AGENTS.md"), []byte("edited by the operator\n"), 0o644)
	os.WriteFile(filepath.Join(dir, "docs", "NOTES.md"), []byte("my plan\n"), 0o644)
	os.WriteFile(filepath.Join(dir, "plan.txt"), []byte("unknown\n"), 0o644)
	os.WriteFile(filepath.Join(dir, "creds", "aicrew.svc.architect"), []byte("material\n"), 0o600)
	o2 := Options{Dir: dir, Projects: []string{"crew-ops", "crew-app"}, Hub: "main"}
	rep, err = Init(o2)
	if err != nil || rep.Status != "conflict" || !strings.Contains(rep.Note, ".aicrew-new") {
		t.Fatalf("rerun after edits: %+v, %v", rep, err)
	}
	a := actions(rep)
	if a["AGENTS.md"] != "conflict" || a["docs/ARCHITECT.md"] != "update" || a["docs/NOTES.md"] != "kept" ||
		a[".aimem.json"] != "kept" || a["CLAUDE.md"] != "unchanged" {
		t.Fatalf("actions %v", a)
	}
	for rel, want := range map[string]string{
		"AGENTS.md":                  "edited by the operator\n",
		"AGENTS.md.aicrew-new":       agentsMD,
		"docs/NOTES.md":              "my plan\n",
		"plan.txt":                   "unknown\n",
		"creds/aicrew.svc.architect": "material\n",
		".aimem.json":                bindingJSON(o),
		"docs/ARCHITECT.md":          guidance(o2.Projects),
	} {
		if got := read(t, dir, rel); got != want {
			t.Errorf("%s = %q, want %q", rel, got, want)
		}
	}
	// The edited file keeps its last managed digest, so a later rerun
	// still sees the edit as a conflict.
	rep, _ = Init(o2)
	if actions(rep)["AGENTS.md"] != "conflict" {
		t.Fatalf("second rerun: %v", actions(rep))
	}
}

// A record of another layout is reported, never migrated, and nothing is
// written.
func TestInitRefusesAnotherLayout(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, recordFile), []byte(`{"layout": 2}`), 0o644)
	if _, err := Init(Options{Dir: dir, Projects: []string{"p"}}); err == nil || !strings.Contains(err.Error(), "layout") {
		t.Fatalf("err %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "AGENTS.md")); !os.IsNotExist(err) {
		t.Fatal("init wrote into a directory of another layout")
	}
}

func TestOptionsAreValidated(t *testing.T) {
	for _, o := range []Options{
		{Projects: []string{"p"}},
		{Dir: "d"},
		{Dir: "d", Projects: []string{"bad name"}},
		{Dir: "d", Projects: []string{"p", "p"}},
		{Dir: "d", Projects: []string{"p"}, Hub: "bad hub"},
	} {
		if _, err := Init(o); err == nil {
			t.Errorf("%+v was accepted", o)
		}
	}
}

// The guidance and commands name nothing outside the directory and carry
// no secret: no absolute or parent path, no home shorthand, no URL, no
// credential file or token shape.
func TestGuidanceNamesNothingOutside(t *testing.T) {
	bad := regexp.MustCompile(`(?i)(\.\./|~/|[a-z]:\\|^/|\s/[a-z]|https?://|\.creds|hub\.json|aimem_|aop_|acs1_|ghp_|sk-ant)`)
	for _, f := range Files(Options{Dir: "d", Projects: []string{"crew-app"}}) {
		if !strings.HasSuffix(f.Path, ".md") {
			continue
		}
		if m := bad.FindString(f.Content); m != "" {
			t.Errorf("%s names %q", f.Path, m)
		}
	}
	g := guidance([]string{"crew-app", "crew-ops"})
	for _, want := range []string{"- `crew-app`\n- `crew-ops`\n", "READY only",
		"[escalation.request ID]", "[escalation.answer ID]", "capability", "never read it"} {
		if !strings.Contains(g, want) {
			t.Errorf("the guidance lacks %q", want)
		}
	}
}

// With aicrewd's connection, the settings name it and the credential's file
// in creds/ by path, for the session's escalation commands; the connection
// is validated, and init reports where the credential goes.
func TestInitWithTheConnection(t *testing.T) {
	dir := t.TempDir()
	o := Options{Dir: dir, Projects: []string{"crew-app"},
		Crew: Connection{URL: "https://aicrew.example:8443", TrustMode: "ca_dns", TrustValue: "aicrew.example"}}
	rep, err := Init(o)
	want := filepath.Join(dir, "creds", "aicrew.architect")
	if err != nil || rep.CredentialFile != want {
		t.Fatalf("init: %+v %v", rep, err)
	}
	var s struct {
		Env         map[string]string `json:"env"`
		Permissions struct {
			Deny []string `json:"deny"`
		} `json:"permissions"`
	}
	if err := json.Unmarshal([]byte(read(t, dir, ".claude/settings.json")), &s); err != nil {
		t.Fatal(err)
	}
	if s.Env["AICREW_URL"] != o.Crew.URL || s.Env["AICREW_TLS_TRUST_MODE"] != "ca_dns" ||
		s.Env["AICREW_TLS_TRUST_VALUE"] != "aicrew.example" || s.Env["AICREW_OPERATOR_TOKEN_FILE"] != want ||
		len(s.Env) != 4 || len(s.Permissions.Deny) != 6 {
		t.Fatalf("settings: %+v", s)
	}
	// A rerun without the connection updates the managed settings back.
	rep, err = Init(Options{Dir: dir, Projects: []string{"crew-app"}})
	if err != nil || actions(rep)[".claude/settings.json"] != "update" || rep.CredentialFile != "" {
		t.Fatalf("rerun without the connection: %+v %v", rep, err)
	}
	for _, c := range []Connection{
		{URL: "http://aicrew.example", TrustMode: "ca_dns", TrustValue: "aicrew.example"},
		{URL: "https://aicrew.example", TrustMode: "none", TrustValue: "x"},
		{URL: "https://aicrew.example", TrustMode: "ca_dns"},
		{URL: "https://u:p@aicrew.example", TrustMode: "ca_dns", TrustValue: "aicrew.example"},
		{TrustMode: "ca_dns", TrustValue: "aicrew.example"},
	} {
		if _, err := Init(Options{Dir: t.TempDir(), Projects: []string{"p"}, Crew: c}); err == nil {
			t.Errorf("%+v was accepted", c)
		}
	}
}
