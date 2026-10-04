// Package forge is aicrew-agent's read-only view of a forge (GitHub, Gitea
// or GitLab) with a member's own token (docs/proposals/PILOT-1-FOLLOWUPS.md,
// section 3): who the token authenticates as, a repository's default branch
// and a branch's head commit. It never writes to a forge, never logs or
// returns a token, and never follows a redirect to another host, so a token
// sent to one host never reaches another.
package forge

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
)

// Kind is a forge's API dialect.
type Kind string

// The dialects.
const (
	GitHub Kind = "github"
	Gitea  Kind = "gitea"
	GitLab Kind = "gitlab"
)

// Valid reports whether k is a known dialect.
func (k Kind) Valid() bool { return k == GitHub || k == Gitea || k == GitLab }

// Errors a caller tells apart.
var (
	// ErrUnreachable is a forge that could not be reached or answered with
	// a server error: nothing is known about the token.
	ErrUnreachable = errors.New("the forge could not be reached")
	// ErrRejected is a token the forge refused (401 or 403).
	ErrRejected = errors.New("the forge rejected the token")
	// ErrNotFound is a repository or branch the forge does not show to the
	// token.
	ErrNotFound = errors.New("the forge shows no such repository or branch to this token")
)

// maxBody bounds a forge answer.
const maxBody = 1 << 20

// hostShape is a host as a clone URL names it, lowercased: a DNS name or an
// IPv4 address, with an optional port.
var hostShape = regexp.MustCompile(`^[a-z0-9]([a-z0-9.-]{0,251}[a-z0-9])?(:[0-9]{1,5})?$`)

// NormalizeHost lowercases a host[:port], drops https's default port and
// checks its shape, so github.com and github.com:443 are one host.
func NormalizeHost(host string) (string, error) {
	h := strings.TrimSuffix(strings.ToLower(strings.TrimSpace(host)), ":443")
	if !hostShape.MatchString(h) {
		return "", fmt.Errorf("%q is not a forge host: a host name with an optional port", host)
	}
	return h, nil
}

// Repository splits a clone URL into its forge host and repository path
// (owner/name, without .git). It reads https://host[:port]/path,
// ssh://[user@]host[:port]/path and the scp form user@host:path.
func Repository(cloneURL string) (host, path string, err error) {
	raw := strings.TrimSpace(cloneURL)
	switch {
	case strings.HasPrefix(raw, "https://"), strings.HasPrefix(raw, "ssh://"):
		u, perr := url.Parse(raw)
		if perr != nil || u.Host == "" || u.RawQuery != "" || u.Fragment != "" {
			return "", "", fmt.Errorf("%q is not a clone URL", cloneURL)
		}
		host, path = u.Host, u.Path
		if u.Scheme == "ssh" {
			// An ssh port is not the forge's https port: the API is on the host.
			host = u.Hostname()
		}
	case strings.Contains(raw, "@") && strings.Contains(raw, ":") && !strings.Contains(raw, "://"):
		at := strings.Index(raw, "@")
		colon := strings.Index(raw[at:], ":") + at
		host, path = raw[at+1:colon], raw[colon+1:]
	default:
		return "", "", fmt.Errorf("%q is not a clone URL: https://, ssh:// or user@host:path", cloneURL)
	}
	if host, err = NormalizeHost(host); err != nil {
		return "", "", err
	}
	path = strings.TrimSuffix(strings.Trim(path, "/"), ".git")
	parts := strings.Split(path, "/")
	if len(parts) < 2 {
		return "", "", fmt.Errorf("%q names no owner/repository", cloneURL)
	}
	for _, p := range parts {
		if p == "" || p == "." || p == ".." {
			return "", "", fmt.Errorf("%q names no owner/repository", cloneURL)
		}
	}
	return host, path, nil
}

// Service is the host as WORKSPACE's credential reference spells it: each
// part of `<service>.<account>.<purpose>` is lowercase letters, digits and
// '-', so '.' and ':' become '-' (github.com is github-com, and
// gitea.example.org:3000 is gitea-example-org-3000).
func Service(host string) string { return EncodePart(host) }

// EncodePart lowercases s and replaces every character outside
// [a-z0-9-] by '-'. The encoding is one-way: a reference is found through
// the home's records, never parsed back.
func EncodePart(s string) string {
	b := []byte(strings.ToLower(s))
	for i, c := range b {
		if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-') {
			b[i] = '-'
		}
	}
	return string(b)
}

// Client reads forges over https with one http.Client.
type Client struct {
	HTTP *http.Client
}

// NewClient is a client with the system roots, a bounded timeout and no
// redirect to another host.
func NewClient() *Client {
	return &Client{HTTP: &http.Client{Timeout: 20 * time.Second}}
}

func (c *Client) http() *http.Client {
	base := c.HTTP
	if base == nil {
		base = &http.Client{Timeout: 20 * time.Second}
	}
	cl := *base
	cl.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if len(via) >= 5 {
			return errors.New("too many redirects")
		}
		if !strings.EqualFold(req.URL.Host, via[0].URL.Host) || req.URL.Scheme != "https" {
			return fmt.Errorf("refused a redirect to %s: a token never leaves its host", req.URL.Host)
		}
		return nil
	}
	return &cl
}

// apiBase is the dialect's API root on host.
func apiBase(host string, k Kind) string {
	switch k {
	case GitHub:
		if host == "github.com" {
			return "https://api.github.com"
		}
		return "https://" + host + "/api/v3"
	case Gitea:
		return "https://" + host + "/api/v1"
	default:
		return "https://" + host + "/api/v4"
	}
}

// Detect names host's dialect. github.com and gitlab.com are known; any
// other host is told apart by public endpoints that need no token: Gitea's
// /api/v1/version, then GitHub Enterprise's /api/v3/meta; otherwise GitLab,
// which WhoAmI then verifies.
func (c *Client) Detect(ctx context.Context, host string) (Kind, error) {
	switch host {
	case "github.com":
		return GitHub, nil
	case "gitlab.com":
		return GitLab, nil
	}
	var v struct {
		Version string `json:"version"`
	}
	ok, err := c.probe(ctx, "https://"+host+"/api/v1/version", &v)
	if err != nil {
		return "", err
	}
	if ok && v.Version != "" {
		return Gitea, nil
	}
	var meta map[string]any
	if ok, err = c.probe(ctx, "https://"+host+"/api/v3/meta", &meta); err != nil {
		return "", err
	}
	if ok && meta != nil {
		return GitHub, nil
	}
	return GitLab, nil
}

// probe GETs a public URL without a token: true with out decoded on a JSON
// 200, false on any other answer, an error only when the host is not
// reachable.
func (c *Client) probe(ctx context.Context, u string, out any) (bool, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return false, err
	}
	req.Header.Set("Accept", "application/json")
	resp, err := c.http().Do(req)
	if err != nil {
		return false, fmt.Errorf("%w: %s", ErrUnreachable, reason(err))
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return false, nil
	}
	return json.NewDecoder(io.LimitReader(resp.Body, maxBody)).Decode(out) == nil, nil
}

// Identity is who a token authenticates as.
type Identity struct {
	Account     string // the login the forge reports
	Name        string // the display name, for commits
	CommitEmail string // the address commits are authored with
}

// WhoAmI asks host who token authenticates as.
func (c *Client) WhoAmI(ctx context.Context, host string, k Kind, token string) (Identity, error) {
	var u struct {
		ID          int64  `json:"id"`
		Login       string `json:"login"`
		Username    string `json:"username"`
		Name        string `json:"name"`
		FullName    string `json:"full_name"`
		Email       string `json:"email"`
		CommitEmail string `json:"commit_email"`
		PublicEmail string `json:"public_email"`
	}
	if err := c.get(ctx, host, k, token, "/user", &u); err != nil {
		return Identity{}, err
	}
	id := Identity{Account: u.Login, Name: u.Name}
	switch k {
	case GitHub:
		// GitHub's no-reply address keeps a member's own address private.
		mail := "github.com"
		if host != "github.com" {
			mail = host
		}
		id.CommitEmail = fmt.Sprintf("%d+%s@users.noreply.%s", u.ID, u.Login, hostname(mail))
	case Gitea:
		id.Name = first(u.FullName, u.Login)
		id.CommitEmail = first(u.Email, u.Login+"@noreply."+hostname(host))
	case GitLab:
		id.Account = u.Username
		id.CommitEmail = first(u.CommitEmail, u.PublicEmail, u.Username+"@noreply."+hostname(host))
	}
	if id.Name == "" {
		id.Name = id.Account
	}
	if id.Account == "" {
		return Identity{}, errors.New("the forge's answer names no account")
	}
	return id, nil
}

// DefaultBranch is the repository's default branch.
func (c *Client) DefaultBranch(ctx context.Context, host string, k Kind, token, path string) (string, error) {
	var r struct {
		DefaultBranch string `json:"default_branch"`
	}
	if err := c.get(ctx, host, k, token, repoPath(k, path), &r); err != nil {
		return "", err
	}
	if r.DefaultBranch == "" {
		return "", errors.New("the forge's answer names no default branch")
	}
	return r.DefaultBranch, nil
}

var commitShape = regexp.MustCompile(`^[0-9a-f]{40}$`)

// BranchHead is the commit a branch points at.
func (c *Client) BranchHead(ctx context.Context, host string, k Kind, token, path, branch string) (string, error) {
	var b struct {
		Commit struct {
			SHA string `json:"sha"`
			ID  string `json:"id"`
		} `json:"commit"`
	}
	p := repoPath(k, path) + "/branches/" + url.PathEscape(branch)
	if k == GitLab {
		p = repoPath(k, path) + "/repository/branches/" + url.PathEscape(branch)
	}
	if err := c.get(ctx, host, k, token, p, &b); err != nil {
		return "", err
	}
	sha := first(b.Commit.SHA, b.Commit.ID)
	if !commitShape.MatchString(sha) {
		return "", errors.New("the forge's answer names no commit")
	}
	return sha, nil
}

func repoPath(k Kind, path string) string {
	if k == GitLab {
		return "/projects/" + url.PathEscape(path)
	}
	return "/repos/" + path
}

// get is one authenticated read.
func (c *Client) get(ctx context.Context, host string, k Kind, token, path string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, apiBase(host, k)+path, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	switch k {
	case GitHub:
		req.Header.Set("Authorization", "Bearer "+token)
	case Gitea:
		req.Header.Set("Authorization", "token "+token)
	case GitLab:
		req.Header.Set("PRIVATE-TOKEN", token)
	default:
		return fmt.Errorf("unknown forge dialect %q", k)
	}
	resp, err := c.http().Do(req)
	if err != nil {
		return fmt.Errorf("%w: %s", ErrUnreachable, reason(err))
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		return ErrRejected
	case resp.StatusCode == http.StatusNotFound:
		return ErrNotFound
	case resp.StatusCode >= 500:
		return fmt.Errorf("%w: status %d", ErrUnreachable, resp.StatusCode)
	case resp.StatusCode != http.StatusOK:
		return fmt.Errorf("the forge answered status %d", resp.StatusCode)
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxBody)).Decode(out); err != nil {
		return fmt.Errorf("the forge's answer is not JSON: %w", err)
	}
	return nil
}

// reason is a transport error without its URL, which is harmless but long.
func reason(err error) string {
	var ue *url.Error
	if errors.As(err, &ue) {
		return ue.Err.Error()
	}
	return err.Error()
}

func hostname(host string) string {
	if h, _, err := net.SplitHostPort(host); err == nil {
		return h
	}
	return host
}

func first(s ...string) string {
	for _, v := range s {
		if v != "" {
			return v
		}
	}
	return ""
}
