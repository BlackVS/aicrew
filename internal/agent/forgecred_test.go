package agent

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/BlackVS/aicrew/internal/forge"
	"github.com/BlackVS/aicrew/internal/privatefile"
)

// fakeForge answers for tokens it knows, per host.
type fakeForge struct {
	kinds       map[string]forge.Kind
	unreachable map[string]bool
	ids         map[string]forge.Identity // by token
	whoami      int
}

func newFakeForge() *fakeForge {
	return &fakeForge{kinds: map[string]forge.Kind{}, unreachable: map[string]bool{}, ids: map[string]forge.Identity{}}
}

func (f *fakeForge) Detect(_ context.Context, host string) (forge.Kind, error) {
	if f.unreachable[host] {
		return "", forge.ErrUnreachable
	}
	if k, ok := f.kinds[host]; ok {
		return k, nil
	}
	return forge.GitLab, nil
}

func (f *fakeForge) WhoAmI(_ context.Context, host string, _ forge.Kind, token string) (forge.Identity, error) {
	f.whoami++
	if f.unreachable[host] {
		return forge.Identity{}, forge.ErrUnreachable
	}
	id, ok := f.ids[token]
	if !ok {
		return forge.Identity{}, forge.ErrRejected
	}
	return id, nil
}

// tokenFile writes a token to a new owner-only file.
func tokenFile(t *testing.T, dir, name, token string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	f, err := privatefile.Create(p)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(token + "\n"); err != nil {
		t.Fatal(err)
	}
	f.Close()
	return p
}

func TestParseForgeCred(t *testing.T) {
	dir := t.TempDir()
	file := tokenFile(t, dir, "gh.token", "tok")
	c, err := ParseForgeCred("GitHub.com=" + file)
	if err != nil || c.Host != "github.com" || c.Source != file {
		t.Fatalf("parse: %+v %v", c, err)
	}
	if c, err := ParseForgeCred("gitea.example.org:3000=-"); err != nil || c.Source != "-" {
		t.Fatalf("stdin: %+v %v", c, err)
	}
	for _, bad := range []string{"github.com", "github.com=", "=x", "bad host=" + file} {
		if _, err := ParseForgeCred(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
	// A value that is not a file may be a token passed by mistake: the
	// refusal never quotes it.
	if _, err := ParseForgeCred("github.com=ghp_secretvalue"); err == nil || strings.Contains(err.Error(), "ghp_secretvalue") {
		t.Fatalf("a token as a value: %v", err)
	}
	if checkForgeCreds([]ForgeCred{{"a.example", file}, {"a.example", file}}) == nil {
		t.Error("one host twice accepted")
	}
	if checkForgeCreds([]ForgeCred{{"a.example", "-"}, {"b.example", "-"}}) == nil {
		t.Error("two standard inputs accepted")
	}
}

// Two hosts give two WORKSPACE-encoded files and two entries; agent.json
// holds no value; a rerun with the same tokens changes nothing; a rotated
// token rewrites the file; another account moves to a new file and removes
// the old one.
func TestProvisionForge(t *testing.T) {
	home, dir := t.TempDir(), t.TempDir()
	f := newFakeForge()
	f.kinds["github.com"], f.kinds["gitea.example.org:3000"] = forge.GitHub, forge.Gitea
	f.ids["gh-token-1"] = forge.Identity{Account: "Example-Bot", Name: "Example Bot", CommitEmail: "1+Example-Bot@users.noreply.github.com"}
	f.ids["gt-token-1"] = forge.Identity{Account: "example.bot", Name: "example.bot", CommitEmail: "example.bot@noreply.gitea.example.org"}
	creds := []ForgeCred{{"github.com", tokenFile(t, dir, "gh", "gh-token-1")}, {"gitea.example.org:3000", tokenFile(t, dir, "gt", "gt-token-1")}}
	doc := agentDoc{top: map[string]json.RawMessage{}, aicrew: map[string]json.RawMessage{}}
	provision := func(creds []ForgeCred) []ForgeCredReport {
		t.Helper()
		toks, err := readForgeTokens(creds, nil)
		if err != nil {
			t.Fatal(err)
		}
		reps, err := provisionForge(context.Background(), home, &doc, toks, f, new(strings.Builder))
		if err != nil {
			t.Fatal(err)
		}
		if err := doc.write(home); err != nil {
			t.Fatal(err)
		}
		return reps
	}
	reps := provision(creds)
	want := map[string]string{"github.com": "github-com.example-bot.repo-write",
		"gitea.example.org:3000": "gitea-example-org-3000.example-bot.repo-write"}
	for _, r := range reps {
		if r.State != ForgeProvisioned || r.File != want[r.Host] {
			t.Fatalf("report %+v", r)
		}
		p := filepath.Join(home, "creds", r.File)
		if err := privatefile.Check(p); err != nil {
			t.Fatalf("%s is not owner-only: %v", r.File, err)
		}
	}
	entries := doc.forgeEntries()
	if len(entries) != 2 || entries["github.com"].Kind != "github" || entries["github.com"].Account != "Example-Bot" ||
		entries["gitea.example.org:3000"].CommitEmail == "" {
		t.Fatalf("entries %+v", entries)
	}
	raw, _ := os.ReadFile(agentJSONPath(home))
	if strings.Contains(string(raw), "gh-token-1") || strings.Contains(string(raw), "gt-token-1") {
		t.Fatal("agent.json holds a token")
	}

	// The same tokens again: unchanged, and the dialect is not detected again.
	for _, r := range provision(creds) {
		if r.State != ForgeUnchanged {
			t.Fatalf("rerun %+v", r)
		}
	}
	// A rotated token for the same account rewrites the same file.
	f.ids["gh-token-2"] = f.ids["gh-token-1"]
	r := provision([]ForgeCred{{"github.com", tokenFile(t, dir, "gh2", "gh-token-2")}})[0]
	if b, _ := os.ReadFile(filepath.Join(home, "creds", r.File)); r.State != ForgeProvisioned || strings.TrimSpace(string(b)) != "gh-token-2" {
		t.Fatalf("rotation %+v %q", r, b)
	}
	// Another account: a new file, and the old one goes.
	f.ids["gh-token-3"] = forge.Identity{Account: "other-bot", Name: "Other", CommitEmail: "x"}
	r = provision([]ForgeCred{{"github.com", tokenFile(t, dir, "gh3", "gh-token-3")}})[0]
	if r.File != "github-com.other-bot.repo-write" {
		t.Fatalf("new account %+v", r)
	}
	if _, err := os.Stat(filepath.Join(home, "creds", want["github.com"])); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("the old account's file was kept")
	}
}

// An unreachable host and a refused token are reported and written nowhere;
// the other host proceeds. Two hosts whose encoded names collide: the second
// is refused by name.
func TestProvisionForgeRefusals(t *testing.T) {
	home, dir := t.TempDir(), t.TempDir()
	f := newFakeForge()
	f.unreachable["down.example"] = true
	f.ids["good"] = forge.Identity{Account: "bot", Name: "bot", CommitEmail: "bot@x"}
	doc := agentDoc{top: map[string]json.RawMessage{}, aicrew: map[string]json.RawMessage{}}
	toks, err := readForgeTokens([]ForgeCred{
		{"down.example", tokenFile(t, dir, "a", "good")},
		{"up.example", tokenFile(t, dir, "b", "rejected")},
		{"a.b", tokenFile(t, dir, "c", "good")},
		{"a-b", tokenFile(t, dir, "d", "good")},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	reps, err := provisionForge(context.Background(), home, &doc, toks, f, new(strings.Builder))
	if err != nil {
		t.Fatal(err)
	}
	states := map[string]ForgeCredReport{}
	for _, r := range reps {
		states[r.Host] = r
	}
	if states["down.example"].State != ForgeUnreachable || states["up.example"].State != ForgeRefused ||
		states["a.b"].State != ForgeProvisioned || states["a-b"].State != ForgeRefused ||
		!strings.Contains(states["a-b"].Detail, "a.b") {
		t.Fatalf("states %+v", states)
	}
	entries := doc.forgeEntries()
	if len(entries) != 1 || entries["a.b"].File != "a-b.bot.repo-write" {
		t.Fatalf("entries %+v", entries)
	}
	files, _ := os.ReadDir(filepath.Join(home, "creds"))
	if len(files) != 1 {
		t.Fatalf("creds/ holds %d files", len(files))
	}
}

// A token file must be owner-only and one line; standard input is read
// through the given reader; errors never quote the token.
func TestReadForgeTokens(t *testing.T) {
	dir := t.TempDir()
	open := filepath.Join(dir, "open")
	os.WriteFile(open, []byte("leaky-token\n"), 0o644)
	if _, err := readForgeTokens([]ForgeCred{{"h.example", open}}, nil); err == nil && privatefile.Check(open) != nil {
		t.Fatal("a file others can read was accepted")
	} else if err != nil && strings.Contains(err.Error(), "leaky-token") {
		t.Fatal("the error quotes the token")
	}
	two := tokenFile(t, dir, "two", "a b")
	if _, err := readForgeTokens([]ForgeCred{{"h.example", two}}, nil); err == nil {
		t.Fatal("a token with a space was accepted")
	}
	toks, err := readForgeTokens([]ForgeCred{{"h.example", "-"}}, func(host string) (string, error) {
		return "from-stdin\n", nil
	})
	if err != nil || len(toks) != 1 || toks[0].token != "from-stdin" {
		t.Fatalf("stdin: %+v %v", toks, err)
	}
	if _, err := readForgeTokens([]ForgeCred{{"h.example", "-"}}, nil); err == nil {
		t.Fatal("standard input without a reader was accepted")
	}
}

// The check verifies every recorded credential and reports, never blocks:
// verified, missing file, refused token, another account, unreachable.
func TestCheckForge(t *testing.T) {
	home, dir := t.TempDir(), t.TempDir()
	f := newFakeForge()
	f.ids["ok"] = forge.Identity{Account: "bot", Name: "bot", CommitEmail: "bot@x"}
	f.ids["moved"] = forge.Identity{Account: "bot", Name: "bot", CommitEmail: "bot@x"}
	doc := agentDoc{top: map[string]json.RawMessage{}, aicrew: map[string]json.RawMessage{}}
	toks, _ := readForgeTokens([]ForgeCred{{"ok.example", tokenFile(t, dir, "1", "ok")},
		{"gone.example", tokenFile(t, dir, "2", "ok")}, {"revoked.example", tokenFile(t, dir, "3", "ok")},
		{"other.example", tokenFile(t, dir, "4", "moved")}, {"down.example", tokenFile(t, dir, "5", "ok")}}, nil)
	if _, err := provisionForge(context.Background(), home, &doc, toks, f, new(strings.Builder)); err != nil {
		t.Fatal(err)
	}
	e := doc.forgeEntries()
	os.Remove(filepath.Join(home, "creds", e["gone.example"].File))
	tokenFile(t, t.TempDir(), "x", "x")
	os.WriteFile(filepath.Join(home, "creds", e["revoked.example"].File), []byte("revoked\n"), 0o600)
	f.ids["moved"] = forge.Identity{Account: "someone-else"}
	f.unreachable["down.example"] = true
	got := map[string]string{}
	for _, c := range checkForge(context.Background(), home, &doc, f) {
		got[c.Host] = c.State
	}
	want := map[string]string{"ok.example": ForgeVerified, "gone.example": ForgeMissing, "revoked.example": ForgeRefused,
		"other.example": ForgeRefused, "down.example": ForgeUnreachable}
	for h, s := range want {
		if got[h] != s {
			t.Errorf("%s: %s, want %s", h, got[h], s)
		}
	}
}

// fakeBase answers one repository's default branch and head.
type fakeBase struct{ token string }

func (b fakeBase) DefaultBranch(_ context.Context, host string, _ forge.Kind, token, path string) (string, error) {
	if token != b.token || path != "team/app" {
		return "", forge.ErrNotFound
	}
	return "main", nil
}

func (b fakeBase) BranchHead(_ context.Context, _ string, _ forge.Kind, token, path, branch string) (string, error) {
	if token != b.token || branch != "main" {
		return "", forge.ErrNotFound
	}
	return "0123456789abcdef0123456789abcdef01234567", nil
}

// ResolveBase fills the body's repository from the forge with the home's
// own credential: the URL and kind when absent, the default branch, and its
// head as the base commit; it keeps what the body names, and names a host
// the home holds no credential for.
func TestResolveBase(t *testing.T) {
	home, dir := t.TempDir(), t.TempDir()
	f := newFakeForge()
	f.kinds["github.com"] = forge.GitHub
	f.ids["gh"] = forge.Identity{Account: "bot", Name: "bot", CommitEmail: "x"}
	doc := agentDoc{top: map[string]json.RawMessage{}, aicrew: map[string]json.RawMessage{}}
	toks, _ := readForgeTokens([]ForgeCred{{"github.com", tokenFile(t, dir, "gh", "gh")}}, nil)
	if _, err := provisionForge(context.Background(), home, &doc, toks, f, new(strings.Builder)); err != nil {
		t.Fatal(err)
	}
	if err := doc.write(home); err != nil {
		t.Fatal(err)
	}
	body, res, err := ResolveBase(context.Background(), home, fakeBase{"gh"}, "https://github.com/team/app.git",
		[]byte(`{"repository":{"branch":"feature-x","access":"write"},"expected_revision":3}`))
	var fields struct {
		Repository map[string]any `json:"repository"`
		Revision   int            `json:"expected_revision"`
	}
	if err != nil || json.Unmarshal(body, &fields) != nil || res.DefaultBranch != "main" || res.Kept || fields.Revision != 3 {
		t.Fatalf("resolve: %s %+v %v", body, res, err)
	}
	want := map[string]any{"url": "https://github.com/team/app.git", "kind": "github", "access": "write", "default_branch": "main",
		"base_commit": "0123456789abcdef0123456789abcdef01234567", "branch": "feature-x"}
	if !reflect.DeepEqual(fields.Repository, want) {
		t.Fatalf("repository = %v, want %v", fields.Repository, want)
	}
	if strings.Contains(string(body), `"gh"`) {
		t.Fatal("the body carries the token")
	}
	given := `{"repository":{"kind":"github","url":"https://github.com/team/app","default_branch":"trunk",` +
		`"base_commit":"ffffffffffffffffffffffffffffffffffffffff"}}`
	kept, res, err := ResolveBase(context.Background(), home, fakeBase{"gh"}, "https://github.com/team/app", []byte(given))
	if err != nil || !res.Kept || string(kept) != given {
		t.Fatalf("keep: %s %+v %v", kept, res, err)
	}
	if _, _, err := ResolveBase(context.Background(), home, fakeBase{"gh"}, "https://github.com/team/app",
		[]byte(`{"repository":"https://github.com/team/app"}`)); err == nil {
		t.Fatal("a repository that is not an object was taken")
	}
	if _, _, err := ResolveBase(context.Background(), home, fakeBase{"gh"}, "https://gitlab.example.org/team/app", nil); err == nil ||
		!strings.Contains(err.Error(), "--cred gitlab.example.org=FILE") {
		t.Fatalf("no credential for the host: %v", err)
	}
}

// join takes --cred on a linked home's rerun: verified, written, recorded,
// and reported; a bad --cred file stops the run before anything changes.
func TestJoinTakesForgeCreds(t *testing.T) {
	e := setupJoin(t)
	code := e.invite(t, "inv-1", "worker")
	e.join(t, e.opts(), &recCrew{}, activeAimem(), code)
	dir := t.TempDir()
	f := newFakeForge()
	f.kinds["github.com"] = forge.GitHub
	f.ids["gh"] = forge.Identity{Account: "bot", Name: "bot", CommitEmail: "x"}
	reads := 0
	deps := e.deps(&recCrew{}, activeAimem(), &reads)
	deps.Forge = f
	rep, err := Join(context.Background(), JoinOptions{Home: e.home, Creds: []ForgeCred{{"github.com", tokenFile(t, dir, "gh", "gh")}}}, deps)
	if err != nil || len(rep.Forge) != 1 || rep.Forge[0].State != ForgeProvisioned || rep.Forge[0].File != "github-com.bot.repo-write" {
		t.Fatalf("rerun with --cred: %+v %v", rep, err)
	}
	doc, _, _ := readAgentDoc(e.home)
	if doc.forgeEntries()["github.com"].Account != "bot" {
		t.Fatalf("entries %+v", doc.forgeEntries())
	}
	open := filepath.Join(dir, "open")
	os.WriteFile(open, []byte("x\n"), 0o644)
	if privatefile.Check(open) == nil {
		return // a platform without owner-only modes for this test's file: covered above
	}
	before, _ := os.ReadFile(agentJSONPath(e.home))
	rep, err = Join(context.Background(), JoinOptions{Home: e.home, Creds: []ForgeCred{{"gitea.example", open}}}, deps)
	after, _ := os.ReadFile(agentJSONPath(e.home))
	if err != nil || rep.Status != JoinBlocked || rep.Reason != "forge_credential" || string(before) != string(after) {
		t.Fatalf("a bad --cred file: %+v %v", rep, err)
	}
}

// A recorded file that is not a credential reference is never opened or
// removed: an edited agent.json cannot point outside creds/.
func TestForgeEntryFileShape(t *testing.T) {
	home := t.TempDir()
	outside := filepath.Join(home, "outside.secret")
	tokenFile(t, home, "outside.secret", "not-for-the-forge")
	doc := agentDoc{top: map[string]json.RawMessage{}, aicrew: map[string]json.RawMessage{}}
	doc.set(doc.top, "forge", map[string]forgeEntry{"evil.example": {Kind: "gitlab", Account: "x", Purpose: "repo-write",
		File: "../outside.secret"}})
	f := newFakeForge()
	f.ids["not-for-the-forge"] = forge.Identity{Account: "x"}
	checks := checkForge(context.Background(), home, &doc, f)
	if len(checks) != 1 || checks[0].State != ForgeMissing || f.whoami != 0 {
		t.Fatalf("a planted path was followed: %+v, %d calls", checks, f.whoami)
	}
	if _, _, err := forgeTokenFor(home, doc, "evil.example"); err == nil {
		t.Fatal("forgeTokenFor followed a planted path")
	}
	// A rotation never removes a planted path either.
	f.kinds["evil.example"] = forge.GitLab
	f.ids["new"] = forge.Identity{Account: "y", Name: "y", CommitEmail: "y@x"}
	toks, _ := readForgeTokens([]ForgeCred{{"evil.example", tokenFile(t, t.TempDir(), "n", "new")}}, nil)
	if _, err := provisionForge(context.Background(), home, &doc, toks, f, new(strings.Builder)); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(outside); err != nil {
		t.Fatalf("the planted path was removed: %v", err)
	}
}

// A destination with the same bytes but a mode others can read is not
// "unchanged": the rerun replaces it owner-only, so check and base
// resolution can use it.
func TestProvisionRepairsAnOpenFile(t *testing.T) {
	home, dir := t.TempDir(), t.TempDir()
	f := newFakeForge()
	f.kinds["github.com"] = forge.GitHub
	f.ids["gh"] = forge.Identity{Account: "bot", Name: "bot", CommitEmail: "x"}
	doc := agentDoc{top: map[string]json.RawMessage{}, aicrew: map[string]json.RawMessage{}}
	creds := []ForgeCred{{"github.com", tokenFile(t, dir, "gh", "gh")}}
	toks, _ := readForgeTokens(creds, nil)
	if _, err := provisionForge(context.Background(), home, &doc, toks, f, new(strings.Builder)); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(home, "creds", "github-com.bot.repo-write")
	// Replace the file with the same bytes, readable by others.
	os.Remove(path)
	if err := os.WriteFile(path, []byte("gh\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if privatefile.Check(path) == nil {
		t.Skip("this platform's default file mode is owner-only; the open file cannot be made here")
	}
	reps, err := provisionForge(context.Background(), home, &doc, toks, f, new(strings.Builder))
	if err != nil || reps[0].State != ForgeProvisioned {
		t.Fatalf("rerun over an open file: %+v %v", reps, err)
	}
	if err := privatefile.Check(path); err != nil {
		t.Fatalf("the rerun left the file open: %v", err)
	}
	if c := checkForge(context.Background(), home, &doc, f); c[0].State != ForgeVerified {
		t.Fatalf("check after the repair: %+v", c)
	}
}
