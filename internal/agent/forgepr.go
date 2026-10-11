package agent

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"

	"github.com/BlackVS/aicrew/internal/forge"
)

// Pull requests with the home's own forge credential (pilot finding
// 2026-10-11). A member pushes its branch with the clone's credential
// helper; `aicrew-agent forge pr` opens, reads, labels and comments on the
// pull request with the same credential, read through agent.json as the
// helper reads it. The token is read in this process only: it never
// appears in an argument, the output or an error, and the forge client
// never sends it to another host. No other forge client is a member's.

// PullAPI is what `forge pr` asks of a forge; forge.Client is the real one.
type PullAPI interface {
	DefaultBranch(ctx context.Context, host string, k forge.Kind, token, path string) (string, error)
	OpenPull(ctx context.Context, host string, k forge.Kind, token, path string, p forge.NewPull) (forge.Pull, error)
	GetPull(ctx context.Context, host string, k forge.Kind, token, path string, n int64) (forge.Pull, error)
	AddLabels(ctx context.Context, host string, k forge.Kind, token, path string, n int64, labels []string) error
	RemoveLabels(ctx context.Context, host string, k forge.Kind, token, path string, n int64, labels []string) error
	CommentPull(ctx context.Context, host string, k forge.Kind, token, path string, n int64, body string) (forge.Comment, error)
}

// ForgePR is the home and repository one `forge pr` command acts in.
type ForgePR struct {
	Home string
	// Repository is the repository's clone URL; empty means the `origin`
	// of the git checkout in Dir.
	Repository string
	// Dir is the checkout the command runs in: its origin names the
	// repository and its branch the head of a new pull request. Empty means
	// the working directory.
	Dir string
	API PullAPI
	// Git runs git; nil means the git on PATH.
	Git GitRunner
}

// PullReport is what a `forge pr` command did. It never carries a secret.
type PullReport struct {
	Host       string         `json:"host"`
	Repository string         `json:"repository"`
	Account    string         `json:"account"`
	Pull       forge.Pull     `json:"pull"`
	Comment    *forge.Comment `json:"comment,omitempty"`
}

// MaxPullText bounds a pull request's body or a comment read from a file.
const MaxPullText = 256 << 10

// ReadPullText reads a body file, or standard input for "-": UTF-8 text of
// at most MaxPullText bytes. A file in home's creds/, by any path or link,
// is refused unread: what this command reads, it posts.
func ReadPullText(home, name string, stdin io.Reader) (string, error) {
	r := stdin
	if name != "-" {
		if inCreds(home, name) {
			return "", errors.New("the body file is in this home's creds/: a credential is never posted")
		}
		f, err := os.Open(name)
		if err != nil {
			return "", err
		}
		defer f.Close()
		r = f
	}
	raw, err := io.ReadAll(io.LimitReader(r, MaxPullText+1))
	if err != nil {
		return "", err
	}
	if len(raw) > MaxPullText {
		return "", fmt.Errorf("the text is larger than %d KiB", MaxPullText>>10)
	}
	s := string(raw)
	if !utf8.ValidString(s) {
		return "", errors.New("the text is not UTF-8")
	}
	return s, nil
}

// inCreds reports whether name, with its links resolved, is inside home's
// creds/.
func inCreds(home, name string) bool {
	resolve := func(p string) (string, bool) {
		abs, err := filepath.Abs(p)
		if err != nil {
			return "", false
		}
		r, err := filepath.EvalSymlinks(abs)
		if err != nil {
			return "", false
		}
		return r, true
	}
	// A path that does not resolve names no file: the open that follows
	// fails on its own.
	file, ok := resolve(name)
	creds, credsOK := resolve(filepath.Join(home, "creds"))
	if !ok || !credsOK {
		return false
	}
	rel, err := filepath.Rel(creds, file)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// errHoldsToken refuses a text that carries the host's token.
var errHoldsToken = errors.New("the text contains this home's forge credential: a credential is never posted")

// pullTarget is the repository a command acts on and the home's credential
// for its host, held only in memory.
type pullTarget struct {
	host, path string
	entry      forgeEntry
	token      string
}

func (f *ForgePR) git(ctx context.Context, args ...string) (string, error) {
	run := f.Git
	if run == nil {
		run = execGit
	}
	dir := f.Dir
	if dir == "" {
		dir = "."
	}
	out, err := run(ctx, dir, gitEnv, args...)
	return strings.TrimSpace(out), err
}

// target resolves the repository and reads the home's credential for its
// host.
func (f *ForgePR) target(ctx context.Context) (pullTarget, error) {
	repo := f.Repository
	if repo == "" {
		var err error
		if repo, err = f.git(ctx, "remote", "get-url", "origin"); err != nil || repo == "" {
			return pullTarget{}, errors.New("this directory is not a checkout with an origin: run the command in your " +
				"worktree, or pass --repository REPO_URL")
		}
	}
	host, path, err := forge.Repository(repo)
	if err != nil {
		return pullTarget{}, err
	}
	doc, _, err := readAgentDoc(f.Home)
	if err != nil {
		return pullTarget{}, err
	}
	e, tok, err := forgeTokenFor(f.Home, doc, host)
	if err != nil {
		return pullTarget{}, err
	}
	if !forge.Kind(e.Kind).Valid() {
		return pullTarget{}, fmt.Errorf("agent.json records no forge dialect for %s: provision the credential again "+
			"with aicrew-agent join --home %s --cred %s=FILE", host, quoteArg(f.Home), host)
	}
	return pullTarget{host: host, path: path, entry: e, token: tok}, nil
}

func (t pullTarget) report(p forge.Pull) PullReport {
	return PullReport{Host: t.host, Repository: t.path, Account: t.entry.Account, Pull: p}
}

func (t pullTarget) kind() forge.Kind { return forge.Kind(t.entry.Kind) }

// OpenPull is the options of `forge pr open`.
type OpenPull struct {
	Title, Body string
	// Head is the branch to merge; empty means the checkout's branch.
	Head string
	// Base is the branch to merge into; empty means the repository's
	// default branch.
	Base  string
	Draft bool
}

// Open opens a pull request from the head branch, which the member pushed,
// into the base branch.
func (f *ForgePR) Open(ctx context.Context, o OpenPull) (PullReport, error) {
	if strings.TrimSpace(o.Title) == "" {
		return PullReport{}, errors.New("--title must not be empty")
	}
	if strings.ContainsAny(o.Title, "\r\n") {
		return PullReport{}, errors.New("--title must be one line")
	}
	t, err := f.target(ctx)
	if err != nil {
		return PullReport{}, err
	}
	if strings.Contains(o.Title, t.token) || strings.Contains(o.Body, t.token) {
		return PullReport{}, errHoldsToken
	}
	if o.Head == "" {
		if o.Head, err = f.git(ctx, "symbolic-ref", "--quiet", "--short", "HEAD"); err != nil || o.Head == "" {
			return PullReport{}, errors.New("this checkout is on no branch: pass --head BRANCH")
		}
	}
	if o.Base == "" {
		if o.Base, err = f.API.DefaultBranch(ctx, t.host, t.kind(), t.token, t.path); err != nil {
			return PullReport{}, fmt.Errorf("read %s's default branch on %s: %w", t.path, t.host, err)
		}
	}
	for _, b := range []string{o.Head, o.Base} {
		if !branchShape.MatchString(b) || strings.Contains(b, "..") {
			return PullReport{}, fmt.Errorf("%q is not a branch name", b)
		}
	}
	if o.Head == o.Base {
		return PullReport{}, fmt.Errorf("the head and the base are both %s: open the pull request from your own branch", o.Head)
	}
	p, err := f.API.OpenPull(ctx, t.host, t.kind(), t.token, t.path,
		forge.NewPull{Title: o.Title, Body: o.Body, Head: o.Head, Base: o.Base, Draft: o.Draft})
	if err != nil {
		return PullReport{}, fmt.Errorf("open a pull request from %s into %s on %s: %w", o.Head, o.Base, t.host, err)
	}
	return t.report(p), nil
}

// Show reads pull request n.
func (f *ForgePR) Show(ctx context.Context, n int64) (PullReport, error) {
	t, err := f.target(ctx)
	if err != nil {
		return PullReport{}, err
	}
	return f.show(ctx, t, n)
}

func (f *ForgePR) show(ctx context.Context, t pullTarget, n int64) (PullReport, error) {
	if n <= 0 {
		return PullReport{}, errors.New("--pr must be a pull request number")
	}
	p, err := f.API.GetPull(ctx, t.host, t.kind(), t.token, t.path, n)
	if err != nil {
		return PullReport{}, fmt.Errorf("read pull request %d of %s on %s: %w", n, t.path, t.host, err)
	}
	return t.report(p), nil
}

// Label adds labels to pull request n, or removes them, and reads it back.
// Only labels the repository defines are added: none is created.
func (f *ForgePR) Label(ctx context.Context, n int64, add bool, labels []string) (PullReport, error) {
	if len(labels) == 0 {
		return PullReport{}, errors.New("name at least one label")
	}
	for _, l := range labels {
		if strings.TrimSpace(l) == "" || len(l) > 100 || strings.ContainsAny(l, ",\r\n\t") {
			return PullReport{}, fmt.Errorf("%q is not a label: one line, no comma, at most 100 bytes", l)
		}
	}
	t, err := f.target(ctx)
	if err != nil {
		return PullReport{}, err
	}
	if n <= 0 {
		return PullReport{}, errors.New("--pr must be a pull request number")
	}
	verb := "add"
	if add {
		err = f.API.AddLabels(ctx, t.host, t.kind(), t.token, t.path, n, labels)
	} else {
		verb = "remove"
		err = f.API.RemoveLabels(ctx, t.host, t.kind(), t.token, t.path, n, labels)
	}
	if err != nil {
		return PullReport{}, fmt.Errorf("%s labels on pull request %d of %s on %s: %w", verb, n, t.path, t.host, err)
	}
	return f.show(ctx, t, n)
}

// Comment posts body on pull request n.
func (f *ForgePR) Comment(ctx context.Context, n int64, body string) (PullReport, error) {
	if strings.TrimSpace(body) == "" {
		return PullReport{}, errors.New("the comment is empty")
	}
	t, err := f.target(ctx)
	if err != nil {
		return PullReport{}, err
	}
	if strings.Contains(body, t.token) {
		return PullReport{}, errHoldsToken
	}
	rep, err := f.show(ctx, t, n)
	if err != nil {
		return PullReport{}, err
	}
	c, err := f.API.CommentPull(ctx, t.host, t.kind(), t.token, t.path, n, body)
	if err != nil {
		return PullReport{}, fmt.Errorf("comment on pull request %d of %s on %s: %w", n, t.path, t.host, err)
	}
	rep.Comment = &c
	return rep, nil
}
