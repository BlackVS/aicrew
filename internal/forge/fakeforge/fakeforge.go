// Package fakeforge is a forge for tests: one dialect's pull request API
// (GitHub, Gitea or GitLab) over TLS on 127.0.0.1, answering for one token,
// with the state the requests change. It is used only by tests.
package fakeforge

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
)

// Token is the only token the fake accepts.
const Token = "forge-token-under-test"

// The repositories the fake serves: Repo may be written, ReadOnly may not.
const (
	Repo     = "team/app"
	ReadOnly = "team/docs"
)

// HeadCommit is the head commit of every pull request.
const HeadCommit = "89abcdef0123456789abcdef0123456789abcdef"

// Pull is one pull request the fake holds.
type Pull struct {
	Number      int64
	Title, Body string
	Head, Base  string
	Draft       bool
	Labels      []string
	Comments    []string
	// State is "" (open), "closed" or "merged".
	State string
	// CI is the head commit's CI as the test sets it: "" (none reported),
	// "pending", "success" or "failure", served in the dialect's own shape.
	CI string
}

// Forge is one fake forge.
type Forge struct {
	Kind string // github, gitea or gitlab
	Host string // 127.0.0.1:PORT
	// Client trusts the fake's certificate.
	Client *http.Client

	mu    sync.Mutex
	pulls map[int64]*Pull
	// repoLabels and orgLabels are the labels the repository and its owner
	// define, by name, with their IDs.
	repoLabels, orgLabels map[string]int64
	// Requests is every request, as "METHOD PATH" (the escaped path, without
	// the query).
	Requests []string
	// Created is every label a request would have created.
	Created []string
	// Unauthorized counts requests without the token.
	Unauthorized int
}

// New starts a fake forge of the dialect kind. The repository defines the
// label review-this; its owner defines hands-reviewed (on GitHub, which has
// no organization labels, the repository defines both).
func New(t *testing.T, kind string) *Forge {
	t.Helper()
	f := &Forge{Kind: kind, pulls: map[int64]*Pull{},
		repoLabels: map[string]int64{"review-this": 11}, orgLabels: map[string]int64{"hands-reviewed": 21}}
	if kind == "github" {
		f.repoLabels["hands-reviewed"] = 21
	}
	srv := httptest.NewTLSServer(http.HandlerFunc(f.serve))
	t.Cleanup(srv.Close)
	f.Host, f.Client = strings.TrimPrefix(srv.URL, "https://"), srv.Client()
	return f
}

// Pull is a copy of pull request n, or nil.
func (f *Forge) Pull(n int64) *Pull {
	f.mu.Lock()
	defer f.mu.Unlock()
	p, ok := f.pulls[n]
	if !ok {
		return nil
	}
	c := *p
	c.Labels = append([]string(nil), p.Labels...)
	c.Comments = append([]string(nil), p.Comments...)
	return &c
}

// SetPull changes pull request n as another party would: its state, its
// CI, labels added, and comments posted.
func (f *Forge) SetPull(n int64, change func(p *Pull)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	change(f.pulls[n])
}

// AddLabel and Comment are the changes a reviewer makes.
func (p *Pull) AddLabel(l string)    { p.addLabel(l) }
func (p *Pull) RemoveLabel(l string) { p.removeLabel(l) }
func (p *Pull) Comment(body string)  { p.Comments = append(p.Comments, body) }

func (f *Forge) base() string {
	return map[string]string{"github": "/api/v3", "gitea": "/api/v1", "gitlab": "/api/v4"}[f.Kind]
}

func (f *Forge) authorized(r *http.Request) bool {
	switch f.Kind {
	case "github":
		return r.Header.Get("Authorization") == "Bearer "+Token
	case "gitea":
		return r.Header.Get("Authorization") == "token "+Token
	}
	return r.Header.Get("PRIVATE-TOKEN") == Token
}

var (
	ghRoute = regexp.MustCompile(`^/repos/([^/]+/[^/]+)(/.*)?$`)
	glRoute = regexp.MustCompile(`^/projects/([^/]+)(/.*)?$`)
	numRe   = regexp.MustCompile(`^/(\d+)(/.*)?$`)
)

func (f *Forge) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Requests = append(f.Requests, r.Method+" "+r.URL.EscapedPath())
	if !f.authorized(r) {
		f.Unauthorized++
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	p, ok := strings.CutPrefix(r.URL.EscapedPath(), f.base())
	if !ok {
		http.NotFound(w, r)
		return
	}
	var in map[string]any
	if r.Body != nil {
		json.NewDecoder(r.Body).Decode(&in)
	}
	if f.Kind == "gitea" && strings.HasPrefix(p, "/orgs/team/labels") {
		f.writeLabelList(w, r, f.orgLabels)
		return
	}
	var repo, rest string
	if f.Kind == "gitlab" {
		m := glRoute.FindStringSubmatch(p)
		if m == nil {
			http.NotFound(w, r)
			return
		}
		repo, _ = url.PathUnescape(m[1])
		rest = m[2]
	} else {
		m := ghRoute.FindStringSubmatch(p)
		if m == nil {
			http.NotFound(w, r)
			return
		}
		repo, rest = m[1], m[2]
	}
	if repo != Repo && repo != ReadOnly {
		http.NotFound(w, r)
		return
	}
	if repo == ReadOnly && r.Method != http.MethodGet {
		w.WriteHeader(http.StatusForbidden)
		w.Write([]byte(`{"message":"Must have push access to this repository"}`))
		return
	}
	switch {
	case rest == "" && r.Method == http.MethodGet:
		w.Write([]byte(`{"default_branch":"main"}`))
	case strings.HasPrefix(rest, "/labels/") && r.Method == http.MethodGet:
		name, _ := url.PathUnescape(strings.TrimPrefix(rest, "/labels/"))
		_, inRepo := f.repoLabels[name]
		_, inOrg := f.orgLabels[name]
		if !inRepo && !(inOrg && f.Kind == "gitlab") {
			http.NotFound(w, r)
			return
		}
		fmt.Fprintf(w, `{"name":%q}`, name)
	case rest == "/labels" && r.Method == http.MethodGet:
		f.writeLabelList(w, r, f.repoLabels)
	case (rest == "/pulls" || rest == "/merge_requests") && r.Method == http.MethodPost:
		f.open(w, in)
	case strings.HasPrefix(rest, "/pulls/") || strings.HasPrefix(rest, "/merge_requests/"):
		f.pullRoute(w, r, numRe.FindStringSubmatch(strings.TrimPrefix(strings.TrimPrefix(rest, "/pulls"), "/merge_requests")), in)
	case strings.HasPrefix(rest, "/commits/") && r.Method == http.MethodGet && f.Kind != "gitlab":
		f.commitRoute(w, r, strings.TrimPrefix(rest, "/commits/"))
	case strings.HasPrefix(rest, "/issues/"):
		f.issueRoute(w, r, numRe.FindStringSubmatch(strings.TrimPrefix(rest, "/issues")), in)
	default:
		http.NotFound(w, r)
	}
}

func (f *Forge) writeLabelList(w http.ResponseWriter, r *http.Request, labels map[string]int64) {
	page, _ := strconv.Atoi(r.URL.Query().Get("page"))
	names := make([]string, 0, len(labels))
	for n := range labels {
		names = append(names, n)
	}
	sort.Strings(names)
	out := []map[string]any{}
	if page <= 1 {
		for _, n := range names {
			out = append(out, map[string]any{"id": labels[n], "name": n})
		}
	}
	json.NewEncoder(w).Encode(out)
}

func str(in map[string]any, k string) string { s, _ := in[k].(string); return s }

func (f *Forge) open(w http.ResponseWriter, in map[string]any) {
	p := &Pull{Title: str(in, "title"), Body: str(in, "body"), Head: str(in, "head"), Base: str(in, "base")}
	if f.Kind == "gitlab" {
		p.Body, p.Head, p.Base = str(in, "description"), str(in, "source_branch"), str(in, "target_branch")
	}
	p.Draft, _ = in["draft"].(bool)
	for _, q := range f.pulls {
		if q.Head == p.Head {
			w.WriteHeader(map[string]int{"github": 422, "gitea": 409, "gitlab": 409}[f.Kind])
			switch f.Kind {
			case "github":
				fmt.Fprintf(w, `{"message":"Validation Failed","errors":[{"message":"A pull request already exists for team:%s."}]}`, p.Head)
			case "gitea":
				w.Write([]byte(`{"message":"pull request already exists for these targets"}`))
			default:
				fmt.Fprintf(w, `{"message":["Another open merge request already exists for this source branch: !%d"]}`, q.Number)
			}
			return
		}
	}
	p.Number = int64(len(f.pulls) + 1)
	f.pulls[p.Number] = p
	w.WriteHeader(http.StatusCreated)
	f.writePull(w, p)
}

func (f *Forge) pullURL(p *Pull) string {
	if f.Kind == "gitlab" {
		return fmt.Sprintf("https://%s/%s/-/merge_requests/%d", f.Host, Repo, p.Number)
	}
	return fmt.Sprintf("https://%s/%s/pull/%d", f.Host, Repo, p.Number)
}

func (f *Forge) writePull(w http.ResponseWriter, p *Pull) {
	var out map[string]any
	switch f.Kind {
	case "gitlab":
		state := map[string]string{"": "opened", "closed": "closed", "merged": "merged"}[p.State]
		out = map[string]any{"iid": p.Number, "id": 9000 + p.Number, "web_url": f.pullURL(p), "title": p.Title,
			"state": state, "draft": strings.HasPrefix(p.Title, "Draft: "), "source_branch": p.Head,
			"target_branch": p.Base, "sha": HeadCommit, "labels": append([]string{}, p.Labels...),
			"user_notes_count": len(p.Comments)}
		if p.State == "merged" {
			out["merge_commit_sha"] = "fedcba9876543210fedcba9876543210fedcba98"
		}
		if p.CI != "" {
			out["head_pipeline"] = map[string]any{"status": map[string]string{"pending": "running", "success": "success",
				"failure": "failed"}[p.CI]}
		}
	default:
		labels := []map[string]any{}
		for _, l := range p.Labels {
			labels = append(labels, map[string]any{"id": f.labelID(l), "name": l})
		}
		state := "open"
		if p.State != "" {
			state = "closed"
		}
		out = map[string]any{"number": p.Number, "html_url": f.pullURL(p), "title": p.Title, "state": state,
			"merged": p.State == "merged", "head": map[string]any{"ref": p.Head, "sha": HeadCommit},
			"base": map[string]any{"ref": p.Base}, "labels": labels, "comments": len(p.Comments)}
		if p.State == "merged" {
			out["merge_commit_sha"] = "fedcba9876543210fedcba9876543210fedcba98"
		}
		if f.Kind == "github" {
			out["draft"] = p.Draft
		}
	}
	json.NewEncoder(w).Encode(out)
}

func (f *Forge) labelID(name string) int64 {
	if id, ok := f.repoLabels[name]; ok {
		return id
	}
	return f.orgLabels[name]
}

func (f *Forge) labelByID(id int64) string {
	for _, m := range []map[string]int64{f.repoLabels, f.orgLabels} {
		for n, v := range m {
			if v == id {
				return n
			}
		}
	}
	return ""
}

func (f *Forge) known(name string) bool {
	_, a := f.repoLabels[name]
	_, b := f.orgLabels[name]
	return a || b
}

func (p *Pull) addLabel(l string) {
	if !slices.Contains(p.Labels, l) {
		p.Labels = append(p.Labels, l)
	}
}

func (p *Pull) removeLabel(l string) bool {
	for i, x := range p.Labels {
		if x == l {
			p.Labels = append(p.Labels[:i], p.Labels[i+1:]...)
			return true
		}
	}
	return false
}

// pullRoute serves /pulls/N and /merge_requests/N[/notes].
func (f *Forge) pullRoute(w http.ResponseWriter, r *http.Request, m []string, in map[string]any) {
	if m == nil {
		http.NotFound(w, r)
		return
	}
	n, _ := strconv.ParseInt(m[1], 10, 64)
	p, ok := f.pulls[n]
	if !ok {
		http.NotFound(w, r)
		return
	}
	switch {
	case m[2] == "" && r.Method == http.MethodGet:
		f.writePull(w, p)
	case m[2] == "" && r.Method == http.MethodPut && f.Kind == "gitlab":
		for l := range strings.SplitSeq(str(in, "add_labels"), ",") {
			if l != "" {
				if !f.known(l) {
					f.Created = append(f.Created, l)
				}
				p.addLabel(l)
			}
		}
		for l := range strings.SplitSeq(str(in, "remove_labels"), ",") {
			if l != "" {
				p.removeLabel(l)
			}
		}
		f.writePull(w, p)
	case m[2] == "/notes" && r.Method == http.MethodPost && f.Kind == "gitlab":
		p.Comments = append(p.Comments, str(in, "body"))
		w.WriteHeader(http.StatusCreated)
		fmt.Fprintf(w, `{"id":%d,"body":%q}`, 500+len(p.Comments), str(in, "body"))
	default:
		http.NotFound(w, r)
	}
}

// commitRoute serves GitHub's and Gitea's /commits/SHA/status and GitHub's
// /commits/SHA/check-runs for HeadCommit: the CI of the pull request whose
// CI is set, GitHub's as a check run, Gitea's as a commit status.
func (f *Forge) commitRoute(w http.ResponseWriter, r *http.Request, rest string) {
	sha, what, _ := strings.Cut(rest, "/")
	if sha != HeadCommit {
		http.NotFound(w, r)
		return
	}
	ci := ""
	for _, p := range f.pulls {
		if p.CI != "" {
			ci = p.CI
		}
	}
	switch {
	case what == "status":
		if f.Kind == "github" || ci == "" {
			w.Write([]byte(`{"state":"pending","total_count":0,"statuses":[]}`))
			return
		}
		fmt.Fprintf(w, `{"state":%q,"total_count":1}`, ci)
	case what == "check-runs" && f.Kind == "github":
		if ci == "" {
			w.Write([]byte(`{"total_count":0,"check_runs":[]}`))
			return
		}
		run := map[string]string{"pending": `{"status":"in_progress","conclusion":null}`,
			"success": `{"status":"completed","conclusion":"success"}`, "failure": `{"status":"completed","conclusion":"failure"}`}[ci]
		fmt.Fprintf(w, `{"total_count":2,"check_runs":[{"status":"completed","conclusion":"skipped"},%s]}`, run)
	default:
		http.NotFound(w, r)
	}
}

// issueRoute serves GitHub's and Gitea's /issues/N/labels[/L] and
// /issues/N/comments.
func (f *Forge) issueRoute(w http.ResponseWriter, r *http.Request, m []string, in map[string]any) {
	if m == nil || f.Kind == "gitlab" {
		http.NotFound(w, r)
		return
	}
	n, _ := strconv.ParseInt(m[1], 10, 64)
	p, ok := f.pulls[n]
	if !ok {
		http.NotFound(w, r)
		return
	}
	switch {
	case m[2] == "/labels" && r.Method == http.MethodPost:
		raw, _ := in["labels"].([]any)
		for _, v := range raw {
			switch l := v.(type) {
			case string: // GitHub: names, created when unknown
				if !f.known(l) {
					f.Created = append(f.Created, l)
				}
				p.addLabel(l)
			case float64: // Gitea: IDs
				name := f.labelByID(int64(l))
				if name == "" {
					w.WriteHeader(http.StatusUnprocessableEntity)
					return
				}
				p.addLabel(name)
			}
		}
		json.NewEncoder(w).Encode([]any{})
	case strings.HasPrefix(m[2], "/labels/") && r.Method == http.MethodDelete:
		key, _ := url.PathUnescape(strings.TrimPrefix(m[2], "/labels/"))
		if f.Kind == "gitea" {
			id, _ := strconv.ParseInt(key, 10, 64)
			key = f.labelByID(id)
		}
		if !p.removeLabel(key) {
			http.NotFound(w, r)
			return
		}
		if f.Kind == "gitea" {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		json.NewEncoder(w).Encode([]any{})
	case m[2] == "/comments" && r.Method == http.MethodPost:
		p.Comments = append(p.Comments, str(in, "body"))
		id := 700 + len(p.Comments)
		w.WriteHeader(http.StatusCreated)
		fmt.Fprintf(w, `{"id":%d,"html_url":"%s#issuecomment-%d"}`, id, f.pullURL(p), id)
	default:
		http.NotFound(w, r)
	}
}
