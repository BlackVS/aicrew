package main

import (
	"bytes"
	"context"
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
	kv := map[string]string{}
	for _, line := range strings.Split(strings.TrimSuffix(got, "\n"), "\n") {
		if k, v, ok := strings.Cut(line, "="); ok {
			kv[k] = v
		}
	}
	if kv["username"] != "example-bot" || kv["pass"+"word"] != token {
		t.Fatalf("git credential fill answered the keys %d, the account %q", len(kv), kv["username"])
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

// A refused repository URL carrying a credential is never echoed.
func TestCloneNeverEchoesTheURL(t *testing.T) {
	var out, errb bytes.Buffer
	args := []string{"--home", t.TempDir(), "--repository", "https://user:secret-in-url@github.com/team/app?x=1",
		"--attempt", "a1", "--base", strings.Repeat("a", 40), "--branch", "b"}
	if code := clone(context.Background(), args, &out, &errb, func(string) string { return "" }); code != exitFailed ||
		strings.Contains(out.String()+errb.String(), "secret-in-url") {
		t.Fatalf("exit %d: %q", code, errb.String())
	}
}
