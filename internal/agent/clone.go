package agent

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/BlackVS/aicrew/internal/forge"
)

// Cloning an attempt's repository with the member's own credential
// (docs/proposals/PILOT-1-FOLLOWUPS.md, section 3.4). The clone lives under
// repos/<service>/<owner>/<name>, the attempt's worktree under
// worktrees/<attempt>. The clone's own git configuration names a credential
// helper, `aicrew-agent git-credential`, which reads the member's file
// through agent.json when git asks; inherited helpers are reset there, so
// no other helper sees the member's host. The member's commit identity is
// set in the clone. The token never appears in a URL, a git configuration,
// a process argument or the output: git receives it from the helper on a
// pipe.

// CloneOptions is one clone of an attempt's repository.
type CloneOptions struct {
	Home       string
	Repository string // an https clone URL
	Attempt    string // the attempt's ID, which names the worktree
	Base       string // the commit the worktree starts from
	Branch     string // the branch the worktree creates
	// Self is the aicrew-agent executable the credential helper runs;
	// empty means this process's own.
	Self string
	Out  io.Writer // progress, never a secret
	// Git runs git; nil means the git on PATH. Tests record it.
	Git GitRunner
}

// GitRunner runs one git command in dir with extra environment, and returns
// its standard output.
type GitRunner func(ctx context.Context, dir string, env []string, args ...string) (string, error)

// CloneReport is what a clone did. It never carries a secret.
type CloneReport struct {
	Clone    string `json:"clone"`
	Worktree string `json:"worktree"`
	Branch   string `json:"branch"`
	Base     string `json:"base"`
	Host     string `json:"host"`
	Account  string `json:"account"`
	Cloned   bool   `json:"cloned"` // this run made the clone
}

var (
	attemptShape = regexp.MustCompile(`^[A-Za-z0-9._:-]{1,128}$`)
	branchShape  = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._/-]{0,199}$`)
	segmentShape = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,99}$`)
	baseShape    = regexp.MustCompile(`^[0-9a-f]{40}$`)
)

// gitEnv keeps every git this command runs from prompting: a missing
// credential fails instead of waiting at a terminal.
var gitEnv = []string{"GIT_TERMINAL_PROMPT=0", "GCM_INTERACTIVE=never"}

// Clone clones the repository if the home has no clone of it yet, gives the
// clone the member's credential helper and commit identity, fetches, and
// makes the attempt's worktree on a new branch at the base commit.
func Clone(ctx context.Context, o CloneOptions) (CloneReport, error) {
	if o.Out == nil {
		o.Out = io.Discard
	}
	if o.Git == nil {
		o.Git = execGit
	}
	if !strings.HasPrefix(o.Repository, "https://") {
		return CloneReport{}, errors.New("--repository must be an https clone URL: the member's credential is an https token")
	}
	host, path, err := forge.Repository(o.Repository)
	if err != nil {
		return CloneReport{}, err
	}
	if !attemptShape.MatchString(o.Attempt) || o.Attempt == "." || o.Attempt == ".." {
		return CloneReport{}, errors.New("--attempt must be an attempt ID: letters, digits, '.', '_', ':' or '-'")
	}
	if !baseShape.MatchString(o.Base) {
		return CloneReport{}, errors.New("--base must be a full commit ID: 40 lowercase hex characters")
	}
	if !branchShape.MatchString(o.Branch) || strings.Contains(o.Branch, "..") || strings.HasSuffix(o.Branch, ".lock") ||
		strings.HasSuffix(o.Branch, "/") {
		return CloneReport{}, errors.New("--branch must be a branch name: letters, digits, '.', '_', '/' or '-'")
	}
	segs := strings.Split(path, "/")
	for _, s := range segs {
		if !segmentShape.MatchString(s) {
			return CloneReport{}, fmt.Errorf("the repository path %q has a segment the home cannot use as a directory", path)
		}
	}
	doc, _, err := readAgentDoc(o.Home)
	if err != nil {
		return CloneReport{}, err
	}
	entry, _, err := forgeTokenFor(o.Home, doc, host)
	if err != nil {
		return CloneReport{}, err
	}
	self := o.Self
	if self == "" {
		if self, err = os.Executable(); err != nil {
			return CloneReport{}, fmt.Errorf("find this aicrew-agent for the credential helper: %w", err)
		}
	}
	helper := credentialHelper(self, o.Home)
	clone := filepath.Join(append([]string{o.Home, "repos", forge.Service(host)}, segs...)...)
	worktree := filepath.Join(o.Home, "worktrees", o.Attempt)
	rep := CloneReport{Clone: clone, Worktree: worktree, Branch: o.Branch, Base: o.Base, Host: host, Account: entry.Account}
	if _, err := os.Stat(worktree); err == nil {
		return rep, fmt.Errorf("worktrees/%s already exists: one worktree per attempt", o.Attempt)
	}

	if _, err := os.Stat(filepath.Join(clone, ".git")); err != nil {
		if err := os.MkdirAll(filepath.Dir(clone), 0o755); err != nil {
			return rep, err
		}
		fmt.Fprintf(o.Out, "Cloning %s into repos/%s.\n", o.Repository, filepath.ToSlash(strings.TrimPrefix(clone, filepath.Join(o.Home, "repos")+string(filepath.Separator))))
		// The helper is given on the command line for the clone itself, and
		// written to the clone's own configuration right after.
		if _, err := o.Git(ctx, o.Home, gitEnv, "-c", "credential.helper=", "-c", "credential.helper="+helper,
			"clone", "--no-checkout", "--", o.Repository, clone); err != nil {
			return rep, fmt.Errorf("clone %s: %w", o.Repository, err)
		}
		rep.Cloned = true
	} else if url, err := o.Git(ctx, clone, gitEnv, "config", "--get", "remote.origin.url"); err != nil || strings.TrimSpace(url) != o.Repository {
		return rep, fmt.Errorf("repos/%s is a clone of another repository: move it, then clone again", strings.Join(segs, "/"))
	}
	if err := configureClone(ctx, o.Git, clone, helper, entry); err != nil {
		return rep, err
	}
	if _, err := o.Git(ctx, clone, gitEnv, "fetch", "--quiet", "origin"); err != nil {
		return rep, fmt.Errorf("fetch %s: %w", o.Repository, err)
	}
	if _, err := o.Git(ctx, clone, gitEnv, "cat-file", "-e", o.Base+"^{commit}"); err != nil {
		return rep, fmt.Errorf("the base commit %s is not in %s", o.Base, o.Repository)
	}
	if err := os.MkdirAll(filepath.Join(o.Home, "worktrees"), 0o755); err != nil {
		return rep, err
	}
	if _, err := o.Git(ctx, clone, gitEnv, "worktree", "add", "-b", o.Branch, "--", worktree, o.Base); err != nil {
		return rep, fmt.Errorf("make worktrees/%s on branch %s: %w", o.Attempt, o.Branch, err)
	}
	fmt.Fprintf(o.Out, "Worktree worktrees/%s on branch %s at %s, commits as %s <%s>.\n", o.Attempt, o.Branch, o.Base,
		entry.CommitName, entry.CommitEmail)
	return rep, nil
}

// credentialHelper is the helper string git runs: a shell snippet naming
// this executable and the home, in forward slashes for git's own shell.
func credentialHelper(self, home string) string {
	return "!" + shellQuote(filepath.ToSlash(self)) + " git-credential --home " + shellQuote(filepath.ToSlash(home))
}

// shellQuote quotes s for a POSIX shell.
func shellQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

// configureClone writes the clone's own credential helper (resetting any
// inherited one) and the member's commit identity.
func configureClone(ctx context.Context, git GitRunner, clone, helper string, e forgeEntry) error {
	steps := [][]string{
		{"config", "--local", "--unset-all", "credential.helper"},
		{"config", "--local", "--add", "credential.helper", ""},
		{"config", "--local", "--add", "credential.helper", helper},
		{"config", "--local", "user.name", e.CommitName},
		{"config", "--local", "user.email", e.CommitEmail},
	}
	for i, s := range steps {
		if _, err := git(ctx, clone, gitEnv, s...); err != nil && i != 0 { // nothing to unset is fine
			return fmt.Errorf("configure the clone: git %s: %w", strings.Join(s[:3], " "), err)
		}
	}
	return nil
}

// execGit runs the git on PATH.
func execGit(ctx context.Context, dir string, env []string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), env...)
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(errb.String())
		if len(msg) > 400 {
			msg = msg[:400]
		}
		return out.String(), fmt.Errorf("%w: %s", err, msg)
	}
	return out.String(), nil
}

// GitCredential answers git's credential protocol for `get` (git-credential
// in a clone's configuration): for an https request to a host the home holds
// a credential for, the account and the token, written to w, which is git's
// pipe. Any other request gets an empty answer, so git asks no further and
// fails. store and erase are accepted and ignored: the home's file is the
// only copy.
func GitCredential(home, op string, r io.Reader, w io.Writer) error {
	if op != "get" {
		_, err := io.Copy(io.Discard, io.LimitReader(r, 64<<10))
		return err
	}
	req := map[string]string{}
	sc := bufio.NewScanner(io.LimitReader(r, 64<<10))
	for sc.Scan() {
		line := sc.Text()
		if line == "" {
			break
		}
		if k, v, ok := strings.Cut(line, "="); ok {
			req[k] = v
		}
	}
	if err := sc.Err(); err != nil {
		return err
	}
	if req["protocol"] != "https" {
		return nil
	}
	host, err := forge.NormalizeHost(req["host"])
	if err != nil {
		return nil
	}
	doc, _, err := readAgentDoc(home)
	if err != nil {
		return err
	}
	e, tok, err := forgeTokenFor(home, doc, host)
	if err != nil {
		return nil
	}
	_, err = fmt.Fprintf(w, "username=%s\npassword=%s\n", e.Account, tok)
	return err
}
