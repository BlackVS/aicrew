package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/BlackVS/aicrew/internal/forge"
)

const cloneToken = "clone-token-never-anywhere"

// git runs git in dir for test setup.
func git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0", "GIT_AUTHOR_NAME=setup", "GIT_AUTHOR_EMAIL=setup@example",
		"GIT_COMMITTER_NAME=setup", "GIT_COMMITTER_EMAIL=setup@example")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// cloneHome is a home holding a github.com credential, and a bare repository
// that https://github.com/team/app maps to for this test's git.
func cloneHome(t *testing.T) (home, bare, base string, runner GitRunner, argv *[][]string) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	home, dir := t.TempDir(), t.TempDir()
	f := newFakeForge()
	f.kinds["github.com"] = forge.GitHub
	f.ids[cloneToken] = forge.Identity{Account: "example-bot", Name: "Example Bot", CommitEmail: "42+example-bot@users.noreply.github.com"}
	doc := agentDoc{top: map[string]json.RawMessage{}, aicrew: map[string]json.RawMessage{}}
	toks, _ := readForgeTokens([]ForgeCred{{"github.com", tokenFile(t, dir, "gh", cloneToken)}}, nil)
	if _, err := provisionForge(context.Background(), home, &doc, toks, f, new(strings.Builder)); err != nil {
		t.Fatal(err)
	}
	if err := doc.write(home); err != nil {
		t.Fatal(err)
	}
	// The forge: a bare repository with one commit on main.
	src, bare := filepath.Join(dir, "src"), filepath.Join(dir, "app.git")
	os.MkdirAll(src, 0o755)
	git(t, src, "init", "-q", "-b", "main")
	os.WriteFile(filepath.Join(src, "README"), []byte("app\n"), 0o644)
	git(t, src, "add", "README")
	git(t, src, "commit", "-q", "-m", "first")
	base = git(t, src, "rev-parse", "HEAD")
	git(t, dir, "clone", "-q", "--bare", src, bare)
	var calls [][]string
	runner = func(ctx context.Context, dir string, env []string, args ...string) (string, error) {
		calls = append(calls, append([]string{}, args...))
		env = append(env, "GIT_CONFIG_COUNT=1", "GIT_CONFIG_KEY_0=url."+filepath.ToSlash(bare)+".insteadOf",
			"GIT_CONFIG_VALUE_0=https://github.com/team/app")
		return execGit(ctx, dir, env, args...)
	}
	return home, bare, base, runner, &calls
}

// Clone makes the clone and the attempt's worktree; the clone's own
// configuration names the helper and the member's identity and holds no
// token; no git argument and no output carries it; a commit in the
// worktree is authored by the member's forge identity.
func TestClone(t *testing.T) {
	home, _, base, runner, argv := cloneHome(t)
	var out bytes.Buffer
	rep, err := Clone(context.Background(), CloneOptions{Home: home, Repository: "https://github.com/team/app",
		Attempt: "att-1", Base: base, Branch: "attempt/att-1", Self: "/opt/aicrew/aicrew-agent", Out: &out, Git: runner})
	if err != nil {
		t.Fatalf("clone: %v\n%s", err, out.String())
	}
	if !rep.Cloned || rep.Account != "example-bot" || rep.Worktree != filepath.Join(home, "worktrees", "att-1") ||
		rep.Clone != filepath.Join(home, "repos", "github-com", "team", "app") {
		t.Fatalf("report %+v", rep)
	}
	if head := git(t, rep.Worktree, "rev-parse", "HEAD"); head != base {
		t.Fatalf("the worktree is at %s, want %s", head, base)
	}
	if b := git(t, rep.Worktree, "rev-parse", "--abbrev-ref", "HEAD"); b != "attempt/att-1" {
		t.Fatalf("the worktree is on %s", b)
	}
	cfg, err := os.ReadFile(filepath.Join(rep.Clone, ".git", "config"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(cfg), "git-credential --home") || !strings.Contains(string(cfg), "Example Bot") ||
		!strings.Contains(string(cfg), "42+example-bot@users.noreply.github.com") {
		t.Fatalf("the clone's configuration:\n%s", cfg)
	}
	raw, err := exec.Command("git", "-C", rep.Clone, "config", "--local", "--get-all", "credential.helper").Output()
	helpers := strings.TrimSuffix(string(raw), "\n")
	if lines := strings.Split(helpers, "\n"); err != nil || len(lines) != 2 || lines[0] != "" {
		t.Fatalf("the helpers are not reset, then ours: %q", helpers)
	}
	// The token is nowhere: configuration, arguments, output.
	for _, where := range []string{string(cfg), out.String(), rep.Clone, rep.Worktree} {
		if strings.Contains(where, cloneToken) {
			t.Fatal("the token appears in the configuration, the output or a path")
		}
	}
	for _, a := range *argv {
		if strings.Contains(strings.Join(a, " "), cloneToken) {
			t.Fatalf("a git argument carries the token: %v", a)
		}
	}
	filepath.Walk(home, func(p string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() || strings.HasPrefix(p, filepath.Join(home, "creds")) {
			return nil
		}
		if b, _ := os.ReadFile(p); bytes.Contains(b, []byte(cloneToken)) {
			t.Errorf("%s holds the token", p)
		}
		return nil
	})
	// A commit in the worktree is the member's.
	os.WriteFile(filepath.Join(rep.Worktree, "CHANGE"), []byte("x\n"), 0o644)
	exec.Command("git", "-C", rep.Worktree, "add", "CHANGE").Run()
	cmd := exec.Command("git", "-c", "commit.gpgsign=false", "commit", "-q", "-m", "work")
	cmd.Dir = rep.Worktree
	cmd.Env = os.Environ()
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("commit: %v %s", err, out)
	}
	if author := git(t, rep.Worktree, "log", "-1", "--format=%an <%ae>"); author != "Example Bot <42+example-bot@users.noreply.github.com>" {
		t.Fatalf("the commit is authored by %q", author)
	}

	// A second attempt reuses the clone; the same attempt is refused.
	rep2, err := Clone(context.Background(), CloneOptions{Home: home, Repository: "https://github.com/team/app",
		Attempt: "att-2", Base: base, Branch: "attempt/att-2", Self: "/opt/aicrew/aicrew-agent", Git: runner})
	if err != nil || rep2.Cloned {
		t.Fatalf("second attempt: %+v %v", rep2, err)
	}
	if _, err := Clone(context.Background(), CloneOptions{Home: home, Repository: "https://github.com/team/app",
		Attempt: "att-1", Base: base, Branch: "attempt/again", Self: "x", Git: runner}); err == nil ||
		!strings.Contains(err.Error(), "already exists") {
		t.Fatalf("the same attempt again: %v", err)
	}
}

// Clone refuses before touching anything: a non-https URL, a host without a
// credential, a bad attempt, base or branch, a base the repository lacks.
func TestCloneRefusals(t *testing.T) {
	home, _, base, runner, _ := cloneHome(t)
	ok := CloneOptions{Home: home, Repository: "https://github.com/team/app", Attempt: "att-1", Base: base,
		Branch: "attempt/att-1", Self: "x", Git: runner}
	for name, mod := range map[string]func(*CloneOptions){
		"ssh URL":         func(o *CloneOptions) { o.Repository = "git@github.com:team/app.git" },
		"no credential":   func(o *CloneOptions) { o.Repository = "https://gitlab.example.org/team/app" },
		"bad attempt":     func(o *CloneOptions) { o.Attempt = "../x" },
		"short base":      func(o *CloneOptions) { o.Base = "abc123" },
		"bad branch":      func(o *CloneOptions) { o.Branch = "a..b" },
		"dot-dot segment": func(o *CloneOptions) { o.Repository = "https://github.com/team/.hidden" },
	} {
		o := ok
		mod(&o)
		if _, err := Clone(context.Background(), o); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
	if _, err := os.Stat(filepath.Join(home, "repos", "github-com")); err == nil {
		t.Fatal("a refused clone touched repos/")
	}
	o := ok
	o.Base = strings.Repeat("a", 40)
	if _, err := Clone(context.Background(), o); err == nil || !strings.Contains(err.Error(), "is not in") {
		t.Fatalf("a base the repository lacks: %v", err)
	}
}

// The helper answers git's get for an https host the home holds a
// credential for, and nothing otherwise.
func TestGitCredential(t *testing.T) {
	home, _, _, _, _ := cloneHome(t)
	var out bytes.Buffer
	err := GitCredential(home, "get", strings.NewReader("protocol=https\nhost=github.com\npath=team/app.git\n\n"), &out)
	if kv := credAnswer(out.String()); err != nil || len(kv) != 2 || kv[credUser] != "example-bot" || kv[credSecret] != cloneToken {
		t.Fatalf("get: %v %v", kv, err)
	}
	for _, in := range []string{"protocol=http\nhost=github.com\n\n", "protocol=https\nhost=gitlab.example.org\n\n",
		"protocol=https\nhost=bad host\n\n"} {
		out.Reset()
		if err := GitCredential(home, "get", strings.NewReader(in), &out); err != nil || out.Len() != 0 {
			t.Errorf("%q answered %q %v", in, out.String(), err)
		}
	}
	out.Reset()
	store := "protocol=https\nhost=github.com\n" + credUser + "=x\n" + credSecret + "=y\n\n"
	if err := GitCredential(home, "store", strings.NewReader(store), &out); err != nil ||
		out.Len() != 0 {
		t.Fatalf("store: %q %v", out.String(), err)
	}
}

// credAnswer reads a credential helper's answer: one key=value per line.
func credAnswer(s string) map[string]string {
	kv := map[string]string{}
	for _, line := range strings.Split(strings.TrimSuffix(s, "\n"), "\n") {
		if k, v, ok := strings.Cut(line, "="); ok {
			kv[k] = v
		}
	}
	return kv
}

// A relative home works from its parent: the clone, its configuration and
// the worktree land under the home, and the helper names the absolute home.
func TestCloneRelativeHome(t *testing.T) {
	home, _, base, runner, _ := cloneHome(t)
	t.Chdir(filepath.Dir(home))
	rep, err := Clone(context.Background(), CloneOptions{Home: filepath.Base(home), Repository: "https://github.com/team/app",
		Attempt: "att-r", Base: base, Branch: "attempt/att-r", Self: "/opt/aicrew/aicrew-agent", Git: runner})
	if err != nil {
		t.Fatalf("clone with a relative home: %v", err)
	}
	if rep.Clone != filepath.Join(home, "repos", "github-com", "team", "app") || rep.Worktree != filepath.Join(home, "worktrees", "att-r") {
		t.Fatalf("report %+v", rep)
	}
	if head := git(t, rep.Worktree, "rev-parse", "HEAD"); head != base {
		t.Fatalf("the worktree is at %s", head)
	}
	helpers, _ := exec.Command("git", "-C", rep.Clone, "config", "--local", "--get-all", "credential.helper").Output()
	if !strings.Contains(string(helpers), filepath.ToSlash(home)) {
		t.Fatalf("the helper does not name the absolute home: %q", helpers)
	}
}
