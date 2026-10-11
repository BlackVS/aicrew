package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/BlackVS/aicrew/internal/agent"
	"github.com/BlackVS/aicrew/internal/forge"
	"github.com/BlackVS/aicrew/internal/forge/fakeforge"
	"github.com/BlackVS/aicrew/internal/privatefile"
)

// forgeHome is a home holding a forge credential for f's host, with the
// forge client pointed at f.
func forgeHome(t *testing.T, f *fakeforge.Forge) string {
	t.Helper()
	home := t.TempDir()
	if err := privatefile.MakeDir(filepath.Join(home, "creds")); err != nil {
		t.Fatal(err)
	}
	file := forge.Service(f.Host) + ".example-bot.repo-write"
	w, err := privatefile.Create(filepath.Join(home, "creds", file))
	if err != nil {
		t.Fatal(err)
	}
	w.WriteString(fakeforge.Token + "\n")
	w.Close()
	doc := map[string]any{"forge": map[string]any{f.Host: map[string]string{"kind": f.Kind, "account": "example-bot",
		"purpose": "repo-write", "file": file, "commit_name": "Example Bot", "commit_email": "b@example.org"}}}
	raw, _ := json.Marshal(doc)
	if err := os.WriteFile(filepath.Join(home, "agent.json"), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	prev := newPullAPI
	newPullAPI = func() agent.PullAPI { return &forge.Client{HTTP: f.Client} }
	t.Cleanup(func() { newPullAPI = prev })
	return home
}

func runForge(t *testing.T, home, stdin string, args ...string) (int, string, string) {
	t.Helper()
	var out, errOut bytes.Buffer
	env := func(name string) string {
		if name == agent.HomeEnv {
			return home
		}
		return ""
	}
	code := forgeCmd(context.Background(), args, strings.NewReader(stdin), &out, &errOut, env)
	if strings.Contains(out.String()+errOut.String(), fakeforge.Token) {
		t.Fatalf("forge %v printed the token", args)
	}
	return code, out.String(), errOut.String()
}

// `forge pr` opens a pull request with the home's own credential and prints
// its number and URL; it labels it, comments on it and shows it, in each
// dialect, and never prints the token.
func TestForgePRCommand(t *testing.T) {
	for _, kind := range []string{"github", "gitea", "gitlab"} {
		t.Run(kind, func(t *testing.T) {
			f := fakeforge.New(t, kind)
			home := forgeHome(t, f)
			repo := "--repository=https://" + f.Host + "/team/app.git"
			body := filepath.Join(t.TempDir(), "body.md")
			os.WriteFile(body, []byte("Fixes the parser.\n"), 0o600)

			code, out, errOut := runForge(t, home, "", "pr", "open", repo, "--title", "Fix the parser", "--body-file", body,
				"--head", "feat/parser", "--draft")
			first, _, _ := strings.Cut(out, "\n")
			if code != exitOK || !strings.HasPrefix(first, "Pull request 1: https://"+f.Host+"/") || !strings.Contains(out, "open, draft") {
				t.Fatalf("open: %d\n%s%s", code, out, errOut)
			}
			if p := f.Pull(1); p.Base != "main" || p.Head != "feat/parser" || p.Body != "Fixes the parser.\n" {
				t.Fatalf("the forge holds %+v", p)
			}
			code, out, _ = runForge(t, home, "", "pr", "label", "add", repo, "--pr", "1", "review-this")
			if code != exitOK || !strings.HasPrefix(out, "Pull request 1: ") || !strings.Contains(out, "labels: review-this\n") {
				t.Fatalf("label add: %d\n%s", code, out)
			}
			code, _, errOut = runForge(t, home, "", "pr", "label", "add", repo, "--pr", "1", "review-this:other")
			if code != exitFailed || !strings.Contains(errOut, "defines no such label") {
				t.Errorf("an unknown label: %d %s", code, errOut)
			}
			code, out, _ = runForge(t, home, "", "pr", "label", "remove", repo, "--pr", "1", "review-this")
			if code != exitOK || !strings.Contains(out, "labels: \n") {
				t.Errorf("label remove: %d\n%s", code, out)
			}
			code, out, _ = runForge(t, home, "Reviewed at the head.\n", "pr", "comment", repo, "--pr", "1", "--body-file", "-")
			if code != exitOK || !strings.HasPrefix(out, "Pull request 1: ") || !strings.Contains(out, "comment: https://"+f.Host+"/") {
				t.Fatalf("comment: %d\n%s", code, out)
			}
			if got := f.Pull(1).Comments; !slices.Equal(got, []string{"Reviewed at the head.\n"}) {
				t.Errorf("the forge holds the comments %q", got)
			}
			code, out, _ = runForge(t, home, "", "pr", "show", repo, "--pr", "1", "--json")
			var rep agent.PullReport
			if code != exitOK || json.Unmarshal([]byte(out), &rep) != nil || rep.Pull.Number != 1 || rep.Account != "example-bot" ||
				rep.Repository != "team/app" || rep.Pull.HeadCommit != fakeforge.HeadCommit {
				t.Fatalf("show --json: %d\n%s", code, out)
			}
			if code, _, errOut = runForge(t, home, "", "pr", "show", repo, "--pr", "9"); code != exitFailed ||
				!strings.Contains(errOut, "no such") {
				t.Errorf("an unknown pull request: %d %s", code, errOut)
			}
		})
	}
}

// `forge pr` refuses an incomplete command, names the join that provisions
// a missing credential, and takes the repository and head from the
// current checkout.
func TestForgePRCommandRefusalsAndCheckout(t *testing.T) {
	f := fakeforge.New(t, "gitea")
	home := forgeHome(t, f)
	repo := "--repository=https://" + f.Host + "/team/app.git"
	for _, args := range [][]string{
		{}, {"pr"}, {"issue", "open"}, {"pr", "merge", "--pr", "1"},
		{"pr", "open", repo, "--body-file", "-"},
		{"pr", "show", repo},
		{"pr", "label", repo, "--pr", "1", "x"},
		{"pr", "label", "add", repo, "--pr", "1"},
		{"pr", "comment", repo, "--pr", "1"},
	} {
		if code, _, _ := runForge(t, home, "", args...); code != exitUsage {
			t.Errorf("forge %v: %d, not a usage error", args, code)
		}
	}
	code, _, errOut := runForge(t, home, "", "pr", "show", "--repository=https://other.example/team/app.git", "--pr", "1")
	if code != exitFailed || !strings.Contains(errOut, "holds no forge credential for other.example") ||
		!strings.Contains(errOut, "--cred other.example=FILE") {
		t.Errorf("a host without a credential: %d %s", code, errOut)
	}
	if code, _, errOut = runForge(t, home, "", "pr", "open", repo, "--title", "T", "--body-file", "-", "--head", "main"); code != exitFailed ||
		!strings.Contains(errOut, "both main") {
		t.Errorf("a head that is the base: %d %s", code, errOut)
	}
	if code, _, errOut = runForge(t, home, "", "pr", "open", "--repository=https://"+f.Host+"/team/docs.git", "--title", "T",
		"--body-file", "-", "--head", "feat/x"); code != exitFailed || !strings.Contains(errOut, "forbids") {
		t.Errorf("a repository the account may not write: %d %s", code, errOut)
	}

	// A credential is never posted: not a body file in the home's creds/,
	// by its own path or through a link, and not a text holding the token.
	before := len(f.Requests)
	credFile := filepath.Join(home, "creds", forge.Service(f.Host)+".example-bot.repo-write")
	if code, _, errOut = runForge(t, home, "", "pr", "open", repo, "--title", "T", "--body-file", credFile, "--head", "feat/x"); code != exitFailed ||
		!strings.Contains(errOut, "in this home's creds/") {
		t.Errorf("a body file in creds/: %d %s", code, errOut)
	}
	link := filepath.Join(t.TempDir(), "notes.md")
	if os.Symlink(credFile, link) == nil {
		if code, _, errOut = runForge(t, home, "", "pr", "comment", repo, "--pr", "1", "--body-file", link); code != exitFailed ||
			!strings.Contains(errOut, "in this home's creds/") {
			t.Errorf("a link to a file in creds/: %d %s", code, errOut)
		}
	}
	if code, _, errOut = runForge(t, home, "see "+fakeforge.Token, "pr", "comment", repo, "--pr", "1", "--body-file", "-"); code != exitFailed ||
		!strings.Contains(errOut, "contains this home's forge credential") {
		t.Errorf("a comment holding the token: %d", code)
	}
	if code, _, errOut = runForge(t, home, "", "pr", "open", repo, "--title", "x"+fakeforge.Token, "--body-file", "-", "--head", "feat/x"); code != exitFailed ||
		!strings.Contains(errOut, "contains this home's forge credential") {
		t.Errorf("a title holding the token: %d", code)
	}
	// Every other input holding the token is refused as soon as the token is
	// read, before an error could quote it or a request send it: the head
	// and base, a label (also an invalid one), the repository's path. The
	// token is a valid branch name, label and path segment.
	tok := fakeforge.Token
	for _, args := range [][]string{
		{"pr", "open", repo, "--title", "T", "--body-file", "-", "--head", tok, "--base", tok},
		{"pr", "open", repo, "--title", "T", "--body-file", "-", "--head", tok},
		{"pr", "open", repo, "--title", "T", "--body-file", "-", "--head", "feat/x", "--base", tok},
		{"pr", "label", "add", repo, "--pr", "1", tok + ","},
		{"pr", "label", "remove", repo, "--pr", "1", tok},
		{"pr", "show", "--repository=https://" + f.Host + "/team/" + tok, "--pr", "1"},
	} {
		if code, _, errOut := runForge(t, home, "", args...); code != exitFailed ||
			!strings.Contains(errOut, "contains this home's forge credential") {
			t.Errorf("forge %v: %d %s", args[1:3], code, errOut)
		}
	}
	if got := f.Requests[before:]; len(got) != 0 {
		t.Errorf("a refused text reached the forge: %v", got)
	}
	// An input quoted before any check could refuse it, a body file's name
	// or a host, is redacted wherever it is printed (runForge fails on the
	// token in any output).
	for _, args := range [][]string{
		{"pr", "comment", repo, "--pr", "1", "--body-file", filepath.Join(t.TempDir(), tok+".md")},
		{"pr", "show", "--repository=https://" + tok + ".example/team/app", "--pr", "1"},
	} {
		if code, _, errOut := runForge(t, home, "", args...); code != exitFailed || !strings.Contains(errOut, "[a forge credential]") {
			t.Errorf("forge %v: %d %s", args[1:3], code, errOut)
		}
	}

	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	dir := t.TempDir()
	for _, args := range [][]string{{"init", "-q", "-b", "feat/checkout"}, {"remote", "add", "origin", "https://" + f.Host + "/team/app.git"}} {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	t.Chdir(dir)
	code, out, errOut := runForge(t, home, "", "pr", "open", "--title", "From the checkout", "--body-file", "-")
	if code != exitOK || !strings.HasPrefix(out, "Pull request 1: ") {
		t.Fatalf("open in a checkout: %d\n%s%s", code, out, errOut)
	}
	if p := f.Pull(1); p.Head != "feat/checkout" || p.Base != "main" {
		t.Errorf("open in a checkout opened %+v", p)
	}
}
