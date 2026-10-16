// Package fakeforge is a forge for tests: one dialect's pull request API
// (GitHub, Gitea or GitLab) over TLS on 127.0.0.1, answering for one token,
// with the state the requests change. It is used only by tests.
package fakeforge

import (
	"crypto/tls"
	"crypto/x509"
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
	"time"
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
	// Checks, when set, are served instead.
	CI     string
	Checks []Check
	// Reviews are the submitted reviews, with their inline comments.
	Reviews []Review
	// SystemNotes adds a GitLab system note before each comment.
	SystemNotes bool

	// meta is each comment's author and time, by index.
	meta []meta
	seq  int
}

type meta struct {
	author string
	at     time.Time
}

// Check is one check of the head commit: a GitHub check run, a Gitea commit
// status or a GitLab job. Log is its log; NotActions marks a GitHub check
// run of another app than Actions.
type Check struct {
	Name       string
	Status     string // queued, in_progress or completed
	Conclusion string // success, failure, skipped, ...
	Log        string
	NotActions bool
}

// Review is a submitted review.
type Review struct {
	Author, State, Body string
	Inline              []Inline
	at                  time.Time
}

// Inline is a comment on a line of the diff.
type Inline struct {
	Path string
	Line int64
	Body string
}

// Commenter is the author of a comment a test adds with Comment, and Member
// the token's account, the author of one posted through the API.
const (
	Commenter = "reviewer"
	Member    = "member"
)

// t0 is the time of the first comment; each later one is a minute after.
var t0 = time.Date(2026, 10, 15, 9, 0, 0, 0, time.UTC)

func (p *Pull) tick() time.Time {
	p.seq++
	return t0.Add(time.Duration(p.seq) * time.Minute)
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
	// GraphQLError, when set, is the type of the error GitHub's GraphQL
	// endpoint answers every mutation with, such as FORBIDDEN.
	GraphQLError string
	// LogHost is the host GitHub's job logs redirect to; LogAuthorized
	// counts the requests that reached it with a credential.
	LogHost       string
	LogAuthorized int
	// LogsForbidden makes every job log answer 403.
	LogsForbidden bool
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
	logs := httptest.NewTLSServer(http.HandlerFunc(f.serveLog))
	t.Cleanup(logs.Close)
	f.Host, f.LogHost = strings.TrimPrefix(srv.URL, "https://"), strings.TrimPrefix(logs.URL, "https://")
	// The client trusts both: the forge and the host its logs redirect to.
	roots := x509.NewCertPool()
	roots.AddCert(srv.Certificate())
	roots.AddCert(logs.Certificate())
	f.Client = &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: roots}}}
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
	c.Checks = append([]Check(nil), p.Checks...)
	c.Reviews = append([]Review(nil), p.Reviews...)
	c.meta = append([]meta(nil), p.meta...)
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
func (p *Pull) Comment(body string)  { p.post(Commenter, body) }

// CommentAs posts body as author.
func (p *Pull) CommentAs(author, body string) { p.post(author, body) }

// Review submits r.
func (p *Pull) Review(r Review) {
	r.at = p.tick()
	p.Reviews = append(p.Reviews, r)
}

func (p *Pull) post(author, body string) {
	p.Comments = append(p.Comments, body)
	p.meta = append(p.meta, meta{author: author, at: p.tick()})
}

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
	if f.Kind == "github" && r.URL.EscapedPath() == "/api/graphql" && r.Method == http.MethodPost {
		f.graphql(w, r)
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
	case r.Method == http.MethodGet && (strings.HasPrefix(rest, "/actions/jobs/") || strings.HasPrefix(rest, "/jobs/") ||
		strings.HasPrefix(rest, "/pipelines/")):
		f.ciRoute(w, r, rest)
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
		if ci := p.ci(); ci != "" {
			out["head_pipeline"] = map[string]any{"id": PipelineID, "status": map[string]string{"pending": "running",
				"success": "success", "failure": "failed"}[ci]}
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
			out["node_id"] = fmt.Sprintf("PR_node%d", p.Number)
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
	case m[2] == "" && r.Method == http.MethodPatch && f.Kind != "gitlab":
		if t, ok := in["title"].(string); ok {
			p.Title = t
		}
		w.WriteHeader(map[string]int{"github": http.StatusOK, "gitea": http.StatusCreated}[f.Kind])
		f.writePull(w, p)
	case m[2] == "" && r.Method == http.MethodPut && f.Kind == "gitlab":
		if t, ok := in["title"].(string); ok {
			p.Title = t
		}
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
	case r.Method == http.MethodGet && m[2] != "":
		f.pullLists(w, r, p, m[2])
	case m[2] == "/notes" && r.Method == http.MethodPost && f.Kind == "gitlab":
		p.post(Member, str(in, "body"))
		w.WriteHeader(http.StatusCreated)
		fmt.Fprintf(w, `{"id":%d,"body":%q}`, 500+len(p.Comments), str(in, "body"))
	default:
		http.NotFound(w, r)
	}
}

// graphql serves GitHub's markPullRequestReadyForReview mutation, by the
// pull request's node ID.
func (f *Forge) graphql(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Query     string            `json:"query"`
		Variables map[string]string `json:"variables"`
	}
	json.NewDecoder(r.Body).Decode(&in)
	if f.GraphQLError != "" {
		fmt.Fprintf(w, `{"data":{"markPullRequestReadyForReview":null},"errors":[{"type":%q,"message":"Resource not accessible by personal access token"}]}`, f.GraphQLError)
		return
	}
	var n int64
	if !strings.Contains(in.Query, "markPullRequestReadyForReview") ||
		func() bool { _, err := fmt.Sscanf(in.Variables["id"], "PR_node%d", &n); return err != nil }() || f.pulls[n] == nil {
		w.Write([]byte(`{"errors":[{"type":"NOT_FOUND","message":"Could not resolve to a node"}]}`))
		return
	}
	f.pulls[n].Draft = false
	w.Write([]byte(`{"data":{"markPullRequestReadyForReview":{"pullRequest":{"isDraft":false}}}}`))
}

// commitRoute serves GitHub's and Gitea's /commits/SHA/status and GitHub's
// /commits/SHA/check-runs for HeadCommit: the checks of the pull request
// whose CI or checks are set, GitHub's as check runs, Gitea's as commit
// statuses.
func (f *Forge) commitRoute(w http.ResponseWriter, r *http.Request, rest string) {
	sha, what, _ := strings.Cut(rest, "/")
	if sha != HeadCommit {
		http.NotFound(w, r)
		return
	}
	checks := f.checks()
	switch {
	case what == "status":
		if f.Kind == "github" || len(checks) == 0 {
			w.Write([]byte(`{"state":"pending","total_count":0,"statuses":[]}`))
			return
		}
		statuses := []map[string]any{}
		for _, c := range checks {
			statuses = append(statuses, map[string]any{"context": c.Name, "status": giteaStatus(c),
				"target_url": "https://" + f.Host + "/team/app/actions/runs/1/jobs/" + c.Name})
		}
		json.NewEncoder(w).Encode(map[string]any{"state": combined(checks), "total_count": len(checks), "statuses": statuses})
	case what == "check-runs" && f.Kind == "github":
		runs := []any{}
		for i, c := range checks {
			app := "github-actions"
			if c.NotActions {
				app = "other-app"
			}
			var conclusion any
			if c.Status == "completed" {
				conclusion = c.Conclusion
			}
			runs = append(runs, map[string]any{"id": JobID(i), "name": c.Name, "status": c.Status, "conclusion": conclusion,
				"html_url": fmt.Sprintf("https://%s/team/app/runs/%d", f.Host, JobID(i)), "app": map[string]any{"slug": app}})
		}
		page, size := pageOf(r, 30)
		lo, hi := min(len(runs), (page-1)*size), min(len(runs), page*size)
		json.NewEncoder(w).Encode(map[string]any{"total_count": len(runs), "check_runs": runs[lo:hi]})
	default:
		http.NotFound(w, r)
	}
}

// PipelineID is GitLab's head pipeline.
const PipelineID = 77

// JobID is the ID of the i-th check's job.
func JobID(i int) int64 { return int64(5000 + i) }

// ci is the pull request's CI: as set, or the conclusion of its checks.
func (p *Pull) ci() string {
	if len(p.Checks) > 0 {
		return combined(p.Checks)
	}
	return p.CI
}

// checks is the checks of the pull request whose CI is set: its own, or
// those its CI implies (on GitHub, with a skipped run beside it).
func (f *Forge) checks() []Check {
	ci := ""
	for _, p := range f.pulls {
		if len(p.Checks) > 0 {
			return p.Checks
		}
		if p.CI != "" {
			ci = p.CI
		}
	}
	var run Check
	switch ci {
	case "":
		return nil
	case "pending":
		run = Check{Name: "build", Status: "in_progress"}
	default:
		run = Check{Name: "build", Status: "completed", Conclusion: ci}
	}
	if f.Kind == "github" {
		return []Check{{Name: "lint", Status: "completed", Conclusion: "skipped"}, run}
	}
	return []Check{run}
}

func combined(cs []Check) string {
	out := "success"
	for _, c := range cs {
		switch {
		case c.Status != "completed":
			out = "pending"
		case c.Conclusion != "success" && c.Conclusion != "skipped" && c.Conclusion != "neutral":
			return "failure"
		}
	}
	return out
}

func giteaStatus(c Check) string {
	switch {
	case c.Status != "completed":
		return "pending"
	case c.Conclusion == "skipped":
		return "success"
	}
	return c.Conclusion
}

func gitlabStatus(c Check) string {
	switch {
	case c.Status != "completed":
		return "running"
	case c.Conclusion == "failure":
		return "failed"
	case c.Conclusion == "cancelled":
		return "canceled"
	}
	return c.Conclusion
}

// ciRoute serves job logs (GitHub's redirect, GitLab's trace) and GitLab's
// pipeline jobs.
func (f *Forge) ciRoute(w http.ResponseWriter, r *http.Request, rest string) {
	checks := f.checks()
	if f.Kind == "gitlab" && rest == fmt.Sprintf("/pipelines/%d/jobs", PipelineID) {
		jobs := []any{}
		for i, c := range checks {
			jobs = append(jobs, map[string]any{"id": JobID(i), "name": c.Name, "status": gitlabStatus(c),
				"web_url": fmt.Sprintf("https://%s/team/app/-/jobs/%d", f.Host, JobID(i)), "allow_failure": false})
		}
		f.paged(w, r, jobs)
		return
	}
	var id int64
	var err error
	switch f.Kind {
	case "github":
		_, err = fmt.Sscanf(rest, "/actions/jobs/%d/logs", &id)
	case "gitlab":
		_, err = fmt.Sscanf(rest, "/jobs/%d/trace", &id)
	default:
		err = fmt.Errorf("no job logs")
	}
	i := int(id - 5000)
	if err != nil || i < 0 || i >= len(checks) {
		http.NotFound(w, r)
		return
	}
	if f.LogsForbidden {
		w.WriteHeader(http.StatusForbidden)
		return
	}
	if f.Kind == "gitlab" {
		w.Write([]byte(checks[i].Log))
		return
	}
	http.Redirect(w, r, fmt.Sprintf("https://%s/logs/%d?sig=signed", f.LogHost, id), http.StatusFound)
}

// serveLog is the host GitHub's job logs redirect to: it serves a log to a
// request without a credential, as a signed address does.
func (f *Forge) serveLog(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if r.Header.Get("Authorization") != "" || r.Header.Get("PRIVATE-TOKEN") != "" {
		f.LogAuthorized++
	}
	var id int64
	checks := f.checks()
	_, err := fmt.Sscanf(r.URL.Path, "/logs/%d", &id)
	i := int(id - 5000)
	if err != nil || r.URL.Query().Get("sig") != "signed" || i < 0 || i >= len(checks) {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/plain")
	w.Write([]byte(checks[i].Log))
}

// pullLists serves a pull request's reviews and inline comments (GitHub,
// Gitea) and its notes (GitLab).
func (f *Forge) pullLists(w http.ResponseWriter, r *http.Request, p *Pull, what string) {
	user := func(name string) map[string]any { return map[string]any{"login": name} }
	switch {
	case f.Kind == "gitlab" && what == "/notes":
		notes := []any{}
		id := 600
		note := func(author, body string, at time.Time, extra map[string]any) {
			id++
			n := map[string]any{"id": id, "body": body, "author": map[string]any{"username": author},
				"created_at": at, "system": false, "type": nil}
			for k, v := range extra {
				n[k] = v
			}
			notes = append(notes, n)
		}
		for i, body := range p.Comments {
			m := p.metaOf(i)
			if p.SystemNotes {
				note("bot", "added 1 commit", m.at.Add(-time.Second), map[string]any{"system": true})
			}
			note(m.author, body, m.at, nil)
		}
		for _, rv := range p.Reviews {
			if rv.Body != "" {
				note(rv.Author, rv.Body, rv.at, nil)
			}
			for _, in := range rv.Inline {
				note(rv.Author, in.Body, rv.at, map[string]any{"type": "DiffNote",
					"position": map[string]any{"new_path": in.Path, "new_line": in.Line}})
			}
		}
		f.paged(w, r, notes)
	case f.Kind != "gitlab" && what == "/reviews":
		out := []any{}
		for i, rv := range p.Reviews {
			out = append(out, map[string]any{"id": 300 + i, "user": user(rv.Author), "body": rv.Body, "state": rv.State,
				"submitted_at": rv.at, "html_url": fmt.Sprintf("%s#pullrequestreview-%d", f.pullURL(p), 300+i),
				"comments_count": len(rv.Inline)})
		}
		f.paged(w, r, out)
	case f.Kind == "github" && what == "/comments":
		out := []any{}
		for i, rv := range p.Reviews {
			for j, in := range rv.Inline {
				out = append(out, map[string]any{"id": 400 + 10*i + j, "user": user(rv.Author), "body": in.Body,
					"path": in.Path, "line": in.Line, "created_at": rv.at})
			}
		}
		f.paged(w, r, out)
	case f.Kind == "gitea" && strings.HasPrefix(what, "/reviews/") && strings.HasSuffix(what, "/comments"):
		var i int
		if _, err := fmt.Sscanf(what, "/reviews/%d/comments", &i); err != nil || i < 300 || i-300 >= len(p.Reviews) {
			http.NotFound(w, r)
			return
		}
		rv := p.Reviews[i-300]
		out := []any{}
		for j, in := range rv.Inline {
			out = append(out, map[string]any{"id": 400 + 10*(i-300) + j, "user": user(rv.Author), "body": in.Body,
				"path": in.Path, "position": in.Line, "created_at": rv.at})
		}
		json.NewEncoder(w).Encode(out)
	default:
		http.NotFound(w, r)
	}
}

// metaOf is comment i's author and time.
func (p *Pull) metaOf(i int) meta {
	if i < len(p.meta) {
		return p.meta[i]
	}
	return meta{author: Commenter, at: t0}
}

// issueComments is a pull request's conversation comments in GitHub's and
// Gitea's shape.
func (f *Forge) issueComments(p *Pull) []any {
	out := []any{}
	for i, body := range p.Comments {
		m := p.metaOf(i)
		out = append(out, map[string]any{"id": 701 + i, "user": map[string]any{"login": m.author}, "body": body,
			"created_at": m.at, "html_url": fmt.Sprintf("%s#issuecomment-%d", f.pullURL(p), 701+i)})
	}
	return out
}

// pageOf is the page and page size a list request asks for.
func pageOf(r *http.Request, size int) (int, int) {
	q := r.URL.Query()
	page, _ := strconv.Atoi(q.Get("page"))
	n, _ := strconv.Atoi(q.Get("per_page"))
	if n <= 0 {
		n, _ = strconv.Atoi(q.Get("limit"))
	}
	if n > 0 {
		size = n
	}
	return max(page, 1), size
}

// paged writes the requested page of items.
func (f *Forge) paged(w http.ResponseWriter, r *http.Request, items []any) {
	page, size := pageOf(r, 30)
	lo, hi := min(len(items), (page-1)*size), min(len(items), page*size)
	json.NewEncoder(w).Encode(items[lo:hi])
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
	case m[2] == "/comments" && r.Method == http.MethodGet:
		f.paged(w, r, f.issueComments(p))
	case m[2] == "/comments" && r.Method == http.MethodPost:
		p.post(Member, str(in, "body"))
		id := 700 + len(p.Comments)
		w.WriteHeader(http.StatusCreated)
		fmt.Fprintf(w, `{"id":%d,"html_url":"%s#issuecomment-%d"}`, id, f.pullURL(p), id)
	default:
		http.NotFound(w, r)
	}
}
