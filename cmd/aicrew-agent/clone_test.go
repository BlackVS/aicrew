package main

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/BlackVS/aicrew/internal/privatefile"
)

// git, given a clone whose own configuration names the built aicrew-agent
// as its credential helper, receives the member's account and token from it
// on its pipe; the configuration holds no token.
func TestGitCredentialThroughGit(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	dir := t.TempDir()
	bin := filepath.Join(dir, "aicrew-agent")
	if runtime.GOOS == "windows" {
		bin += ".exe"
	}
	if out, err := exec.Command("go", "build", "-o", bin, ".").CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}
	home := filepath.Join(dir, "home")
	if err := privatefile.MakeDir(filepath.Join(home, "creds")); err != nil {
		t.Fatal(err)
	}
	const token = "token-only-on-the-pipe"
	f, err := privatefile.Create(filepath.Join(home, "creds", "github-com.example-bot.repo-write"))
	if err != nil {
		t.Fatal(err)
	}
	f.WriteString(token + "\n")
	f.Close()
	doc := map[string]any{"forge": map[string]any{"github.com": map[string]string{"kind": "github", "account": "example-bot",
		"purpose": "repo-write", "file": "github-com.example-bot.repo-write", "commit_name": "Example Bot", "commit_email": "b@x"}}}
	raw, _ := json.Marshal(doc)
	os.WriteFile(filepath.Join(home, "agent.json"), raw, 0o600)

	repo := filepath.Join(dir, "repo")
	run := func(stdin string, args ...string) string {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = repo
		cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0", "GCM_INTERACTIVE=never")
		cmd.Stdin = strings.NewReader(stdin)
		var out, errb bytes.Buffer
		cmd.Stdout, cmd.Stderr = &out, &errb
		if err := cmd.Run(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, errb.String())
		}
		return out.String()
	}
	os.MkdirAll(repo, 0o755)
	run("", "init", "-q")
	helper := "!'" + filepath.ToSlash(bin) + "' git-credential --home '" + filepath.ToSlash(home) + "'"
	run("", "config", "--local", "--add", "credential.helper", "")
	run("", "config", "--local", "--add", "credential.helper", helper)
	got := run("protocol=https\nhost=github.com\npath=team/app.git\n\n", "credential", "fill")
	if !strings.Contains(got, "username=example-bot\n") || !strings.Contains(got, "password="+token+"\n") {
		t.Fatalf("git credential fill answered %q", got)
	}
	cfg, _ := os.ReadFile(filepath.Join(repo, ".git", "config"))
	if strings.Contains(string(cfg), token) {
		t.Fatal("the clone's configuration holds the token")
	}
}

// The helper refuses to answer on a terminal, and needs --home and one
// operation.
func TestGitCredentialRefusals(t *testing.T) {
	var out, errb bytes.Buffer
	if code := gitCredential([]string{"--home", t.TempDir(), "get"}, strings.NewReader(""), &out, &errb, true); code != exitUsage ||
		out.Len() != 0 {
		t.Fatalf("on a terminal: %d %q", code, out.String())
	}
	for _, args := range [][]string{{"get"}, {"--home", "x"}, {"--home", "x", "get", "extra"}} {
		if code := gitCredential(args, strings.NewReader(""), &out, &errb, false); code != exitUsage {
			t.Errorf("%v: exit %d", args, code)
		}
	}
}
