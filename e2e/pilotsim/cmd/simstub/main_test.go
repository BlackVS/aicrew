package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

const repo = "operator/fixture"

// fakeGitea answers the stubs' calls as Gitea 1.27 does, in its JSON
// shapes and status codes, and records the label changes in order.
type fakeGitea struct {
	mu       sync.Mutex
	t        *testing.T
	token    string
	labels   []label
	pulls    map[int64]*pull
	branches map[string]string // name -> head SHA
	comments map[int64][]string
	statuses map[string][]map[string]string // SHA -> statuses
	events   []string                       // "add hands-reviewing #1", "del review-this #1", "comment #1"
	// onGetPull runs before a single pull request is answered: a test moves
	// the head there.
	onGetPull func(p *pull)
	// onBranches runs, unlocked, before the branches are listed: a test
	// holds a poll there.
	onBranches func()
}

func newFake(t *testing.T) *fakeGitea {
	f := &fakeGitea{t: t, token: "stub-token", pulls: map[int64]*pull{}, branches: map[string]string{},
		comments: map[int64][]string{}, statuses: map[string][]map[string]string{}}
	for i, n := range []string{"bug", labelRequest, labelReviewing, labelReviewed} {
		f.labels = append(f.labels, label{ID: int64(10 + i), Name: n})
	}
	return f
}

func (f *fakeGitea) label(name string) label {
	for _, l := range f.labels {
		if l.Name == name {
			return l
		}
	}
	f.t.Fatalf("no label %s", name)
	return label{}
}

func (f *fakeGitea) addPull(n int64, head string, labels ...string) {
	p := &pull{Number: n, State: "open"}
	p.Head.SHA = head
	for _, l := range labels {
		p.Labels = append(p.Labels, f.label(l))
	}
	f.pulls[n] = p
}

var (
	rePulls    = regexp.MustCompile(`^/api/v1/repos/` + repo + `/pulls$`)
	rePull     = regexp.MustCompile(`^/api/v1/repos/` + repo + `/pulls/(\d+)$`)
	reLabels   = regexp.MustCompile(`^/api/v1/repos/` + repo + `/labels$`)
	reIssueLbl = regexp.MustCompile(`^/api/v1/repos/` + repo + `/issues/(\d+)/labels(?:/(\d+))?$`)
	reComments = regexp.MustCompile(`^/api/v1/repos/` + repo + `/issues/(\d+)/comments$`)
	reBranches = regexp.MustCompile(`^/api/v1/repos/` + repo + `/branches$`)
	reCombined = regexp.MustCompile(`^/api/v1/repos/` + repo + `/commits/([0-9a-f]{40})/status$`)
	reStatuses = regexp.MustCompile(`^/api/v1/repos/` + repo + `/statuses/([0-9a-f]{40})$`)
)

func (f *fakeGitea) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method == "GET" && reBranches.MatchString(r.URL.Path) && f.onBranches != nil {
		f.onBranches()
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if r.Header.Get("Authorization") != "token "+f.token {
		http.Error(w, `{"message":"token is required"}`, http.StatusUnauthorized)
		return
	}
	reply := func(code int, v any) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(code)
		_ = json.NewEncoder(w).Encode(v)
	}
	num := func(s string) int64 { n, _ := strconv.ParseInt(s, 10, 64); return n }
	p := r.URL.Path
	switch {
	case r.Method == "GET" && rePulls.MatchString(p):
		if r.URL.Query().Get("state") != "open" {
			f.t.Errorf("pulls listed with state %q", r.URL.Query().Get("state"))
		}
		out := []pull{}
		for n := int64(1); n <= int64(len(f.pulls)); n++ {
			if q, ok := f.pulls[n]; ok && q.State == "open" {
				out = append(out, *q)
			}
		}
		reply(200, out)
	case r.Method == "GET" && rePull.MatchString(p):
		q := f.pulls[num(rePull.FindStringSubmatch(p)[1])]
		if q == nil {
			reply(404, map[string]string{"message": "not found"})
			return
		}
		if f.onGetPull != nil {
			f.onGetPull(q)
		}
		reply(200, q)
	case r.Method == "GET" && reLabels.MatchString(p):
		reply(200, f.labels)
	case reIssueLbl.MatchString(p):
		m := reIssueLbl.FindStringSubmatch(p)
		q := f.pulls[num(m[1])]
		switch r.Method {
		case "POST":
			var in struct {
				Labels []int64 `json:"labels"`
			}
			if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
				reply(422, map[string]string{"message": err.Error()})
				return
			}
			for _, id := range in.Labels {
				for _, l := range f.labels {
					if l.ID == id && !q.has(l.Name) {
						q.Labels = append(q.Labels, l)
						f.events = append(f.events, fmt.Sprintf("add %s #%d", l.Name, q.Number))
					}
				}
			}
			reply(200, q.Labels)
		case "DELETE":
			id := num(m[2])
			i := slices.IndexFunc(q.Labels, func(l label) bool { return l.ID == id })
			if i < 0 {
				reply(404, map[string]string{"message": "label not found"})
				return
			}
			f.events = append(f.events, fmt.Sprintf("del %s #%d", q.Labels[i].Name, q.Number))
			q.Labels = slices.Delete(q.Labels, i, i+1)
			w.WriteHeader(http.StatusNoContent)
		}
	case r.Method == "POST" && reComments.MatchString(p):
		n := num(reComments.FindStringSubmatch(p)[1])
		var in struct {
			Body string `json:"body"`
		}
		_ = json.NewDecoder(r.Body).Decode(&in)
		f.comments[n] = append(f.comments[n], in.Body)
		f.events = append(f.events, fmt.Sprintf("comment #%d", n))
		reply(201, map[string]any{"id": len(f.comments[n]), "body": in.Body})
	case r.Method == "GET" && reBranches.MatchString(p):
		out := []branch{}
		for name, sha := range f.branches {
			b := branch{Name: name}
			b.Commit.ID = sha
			out = append(out, b)
		}
		reply(200, out)
	case r.Method == "GET" && reCombined.MatchString(p):
		sha := reCombined.FindStringSubmatch(p)[1]
		reply(200, map[string]any{"sha": sha, "statuses": f.statuses[sha]})
	case r.Method == "POST" && reStatuses.MatchString(p):
		sha := reStatuses.FindStringSubmatch(p)[1]
		var in map[string]string
		_ = json.NewDecoder(r.Body).Decode(&in)
		f.statuses[sha] = append(f.statuses[sha], in)
		reply(201, in)
	default:
		f.t.Errorf("unexpected call %s %s", r.Method, r.URL)
		reply(404, map[string]string{"message": "not found"})
	}
}

func (f *fakeGitea) labelNames(n int64) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for _, l := range f.pulls[n].Labels {
		out = append(out, l.Name)
	}
	return out
}

// setup starts the fake and writes the token and scenario files.
func setup(t *testing.T, scenarioJSON string) (*fakeGitea, *httptest.Server, string, string) {
	t.Helper()
	f := newFake(t)
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	dir := t.TempDir()
	tok := filepath.Join(dir, "token")
	sc := filepath.Join(dir, "scenario.json")
	if err := os.WriteFile(tok, []byte(f.token+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(sc, []byte(scenarioJSON), 0o600); err != nil {
		t.Fatal(err)
	}
	return f, srv, tok, sc
}

func once(t *testing.T, mode, api, tok, sc string) string {
	t.Helper()
	var out, errb bytes.Buffer
	if code := run(context.Background(), []string{mode, "-scenario", sc, "-token-file", tok, "-api", api, "-once"}, &out, &errb); code != 0 {
		t.Fatalf("simstub %s exited %d: %s", mode, code, errb.String())
	}
	return out.String()
}

const (
	head1 = "1111111111111111111111111111111111111111"
	head2 = "2222222222222222222222222222222222222222"
)

func TestHandsReviewsARequest(t *testing.T) {
	f, srv, tok, sc := setup(t, `{"repos":["`+repo+`"],"hands":{"review_seconds":0}}`)
	f.addPull(1, head1, "bug", labelRequest)
	f.addPull(2, head2, "bug") // not requested
	once(t, "hands", srv.URL+"/api/v1", tok, sc)

	want := []string{
		"add hands-reviewing #1", "del review-this #1", // new label on before the old comes off
		"comment #1",
		"add hands-reviewed #1", "del hands-reviewing #1",
	}
	if !slices.Equal(f.events, want) {
		t.Fatalf("events:\n got %q\nwant %q", f.events, want)
	}
	if got := f.labelNames(1); !slices.Equal(got, []string{"bug", labelReviewed}) {
		t.Errorf("labels after the review: %q", got)
	}
	if len(f.comments[2]) != 0 || len(f.labelNames(2)) != 1 {
		t.Errorf("an unrequested pull request was touched: %q %q", f.comments[2], f.labelNames(2))
	}
	body := f.comments[1][0]
	if first, _, _ := strings.Cut(body, "\n"); first != "[hands-bot review] reviewed at head "+head1 {
		t.Errorf("header line %q", first)
	}
	// verify-delivery's default external pattern and verdict pattern.
	if !regexp.MustCompile(`(?m)^\[hands-bot review\] reviewed at head (?P<sha>[0-9a-f]{40})\b`).MatchString(body) {
		t.Errorf("header does not match the review pattern:\n%s", body)
	}
	if !regexp.MustCompile(`(?m)^VERDICT\s*\n\s*READY_FOR_HUMAN_MERGE\b`).MatchString(body) {
		t.Errorf("no VERDICT line followed by READY_FOR_HUMAN_MERGE:\n%s", body)
	}

	// A second poll finds nothing to do: the request is served once.
	f.events = nil
	once(t, "hands", srv.URL+"/api/v1", tok, sc)
	if len(f.events) != 0 {
		t.Errorf("second poll changed %q", f.events)
	}
}

func TestHandsReturnVerdict(t *testing.T) {
	f, srv, tok, sc := setup(t, `{"repos":["`+repo+`"],"hands":{"verdict":"RETURN_TO_IMPLEMENTATION"}}`)
	f.addPull(1, head1, labelRequest)
	once(t, "hands", srv.URL+"/api/v1", tok, sc)
	body := f.comments[1][0]
	if !regexp.MustCompile(`(?m)^VERDICT\n`+verdictReturn+`$`).MatchString(body) || !strings.Contains(body, "[BLOCKER]") {
		t.Errorf("return verdict body:\n%s", body)
	}
}

func TestHandsMovedHeadGetsNoReview(t *testing.T) {
	f, srv, tok, sc := setup(t, `{"repos":["`+repo+`"]}`)
	f.addPull(1, head1, labelRequest)
	f.onGetPull = func(p *pull) { p.Head.SHA = head2 } // a push while hands-reviewing is on
	out := once(t, "hands", srv.URL+"/api/v1", tok, sc)

	want := []string{"add hands-reviewing #1", "del review-this #1", "del hands-reviewing #1"}
	if !slices.Equal(f.events, want) {
		t.Fatalf("events:\n got %q\nwant %q", f.events, want)
	}
	if len(f.comments[1]) != 0 {
		t.Errorf("a moved head got a comment: %q", f.comments[1])
	}
	if got := f.labelNames(1); len(got) != 0 {
		t.Errorf("labels left on: %q", got)
	}
	if !strings.Contains(out, "the head moved") {
		t.Errorf("log: %q", out)
	}

	// The request is released: a new review-this is served at the new head.
	f.onGetPull = nil
	f.pulls[1].Labels = []label{f.label(labelRequest)}
	f.events = nil
	once(t, "hands", srv.URL+"/api/v1", tok, sc)
	if len(f.comments[1]) != 1 || !strings.HasPrefix(f.comments[1][0], "[hands-bot review] reviewed at head "+head2+"\n") {
		t.Errorf("the renewed request: %q", f.comments[1])
	}
}

// A requester that takes review-this off between the stub's two label calls
// does not leave the stub stuck with hands-reviewing on.
func TestHandsToleratesALabelAlreadyOff(t *testing.T) {
	f, srv, tok, sc := setup(t, `{"repos":["`+repo+`"]}`)
	f.addPull(1, head1, labelRequest)
	requestID := f.label(labelRequest).ID
	// Take review-this off just before the stub's own call does.
	srvHook := srv.Config.Handler
	srv.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "DELETE" && strings.HasSuffix(r.URL.Path, fmt.Sprintf("/labels/%d", requestID)) {
			f.mu.Lock()
			f.pulls[1].Labels = slices.DeleteFunc(f.pulls[1].Labels, func(l label) bool { return l.Name == labelRequest })
			f.mu.Unlock()
		}
		srvHook.ServeHTTP(w, r)
	})
	once(t, "hands", srv.URL+"/api/v1", tok, sc)
	if len(f.comments[1]) != 1 || !slices.Equal(f.labelNames(1), []string{labelReviewed}) {
		t.Errorf("comments %q, labels %q", f.comments[1], f.labelNames(1))
	}
}

func TestHandsSkipsAPullRequestItHolds(t *testing.T) {
	f, srv, tok, sc := setup(t, `{"repos":["`+repo+`"]}`)
	f.addPull(1, head1, labelRequest, labelReviewing)
	once(t, "hands", srv.URL+"/api/v1", tok, sc)
	if len(f.events) != 0 {
		t.Errorf("touched a pull request already being reviewed: %q", f.events)
	}
}

func TestHandsNeedsTheLabels(t *testing.T) {
	f, srv, tok, sc := setup(t, `{"repos":["`+repo+`"]}`)
	f.labels = f.labels[:2] // no hands-reviewing, no hands-reviewed
	f.addPull(1, head1, labelRequest)
	var out, errb bytes.Buffer
	code := run(context.Background(), []string{"hands", "-scenario", sc, "-token-file", tok, "-api", srv.URL + "/api/v1", "-once"}, &out, &errb)
	if code != 1 || !strings.Contains(errb.String(), "has no label hands-reviewing") || len(f.events) != 0 {
		t.Errorf("code %d, stderr %q, events %q", code, errb.String(), f.events)
	}
}

func TestCIPostsOneStatusPerHead(t *testing.T) {
	f, srv, tok, sc := setup(t, `{"repos":["`+repo+`"]}`)
	f.branches["main"] = head1
	f.branches["feature"] = head2
	f.statuses[head2] = []map[string]string{{"context": "other", "state": "success"}}
	once(t, "ci", srv.URL+"/api/v1", tok, sc)
	for _, sha := range []string{head1, head2} {
		var mine []map[string]string
		for _, s := range f.statuses[sha] {
			if s["context"] == "sim-ci" {
				mine = append(mine, s)
			}
		}
		if len(mine) != 1 || mine[0]["state"] != "success" || mine[0]["target_url"] != srv.URL+"/"+repo+"/commit/"+sha {
			t.Errorf("%s: statuses %v", sha, f.statuses[sha])
		}
	}

	// A new head (the merge commit) gets one; the old ones get no second.
	f.branches["main"] = "3333333333333333333333333333333333333333"
	if err := os.WriteFile(sc, []byte(`{"repos":["`+repo+`"],"ci":{"state":"failure"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	once(t, "ci", srv.URL+"/api/v1", tok, sc)
	if n := len(f.statuses[head1]); n != 1 {
		t.Errorf("head1 has %d statuses", n)
	}
	if s := f.statuses["3333333333333333333333333333333333333333"]; len(s) != 1 || s[0]["state"] != "failure" {
		t.Errorf("merge commit statuses %v", s)
	}
}

func TestCILeavesAHeadWithItsStatus(t *testing.T) {
	f, srv, tok, sc := setup(t, `{"repos":["`+repo+`"]}`)
	f.branches["main"] = head1
	f.statuses[head1] = []map[string]string{{"context": "sim-ci", "state": "failure"}}
	once(t, "ci", srv.URL+"/api/v1", tok, sc)
	if n := len(f.statuses[head1]); n != 1 {
		t.Errorf("a head with a sim-ci status got another: %v", f.statuses[head1])
	}
}

func TestScenarioValidation(t *testing.T) {
	dir := t.TempDir()
	for _, bad := range []string{
		`{"repos":["noslash"]}`,
		`{"hands":{"verdict":"LGTM"}}`,
		`{"ci":{"state":"green"}}`,
		`not json`,
	} {
		p := filepath.Join(dir, "s.json")
		if err := os.WriteFile(p, []byte(bad), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := loadScenario(p); err == nil {
			t.Errorf("%s: accepted", bad)
		}
	}
}

func TestUsage(t *testing.T) {
	var out, errb bytes.Buffer
	for _, args := range [][]string{nil, {"review"}, {"hands"}, {"ci", "-api", "x"}} {
		if code := run(context.Background(), args, &out, &errb); code != 2 {
			t.Errorf("%q: exit %d", args, code)
		}
	}
}

func TestUnauthorizedIsAnError(t *testing.T) {
	f, srv, tok, sc := setup(t, `{"repos":["`+repo+`"]}`)
	f.token = "another"
	var out, errb bytes.Buffer
	if code := run(context.Background(), []string{"ci", "-scenario", sc, "-token-file", tok, "-api", srv.URL + "/api/v1", "-once"}, &out, &errb); code != 1 ||
		!strings.Contains(errb.String(), "HTTP 401") {
		t.Errorf("code %d, stderr %q", code, errb.String())
	}
}

// The review time is waited for, and a cancelled context stops the wait.
func TestHandsWaitsTheReviewTime(t *testing.T) {
	f, srv, _, _ := setup(t, `{}`)
	f.addPull(1, head1, labelRequest)
	g := &gitea{base: srv.URL + "/api/v1", token: f.token, client: srv.Client()}
	var waited time.Duration
	st := &stub{g: g, out: &bytes.Buffer{}, seen: map[string]bool{},
		sleep: func(ctx context.Context, d time.Duration) error { waited = d; return context.Canceled }}
	s := scenario{Repos: []string{repo}}
	s.Hands.ReviewSeconds = 1.5
	s.Hands.Verdict = verdictReady
	if err := st.handsPoll(context.Background(), s); err != context.Canceled {
		t.Fatalf("err %v", err)
	}
	if waited != 1500*time.Millisecond || len(f.comments[1]) != 0 {
		t.Errorf("waited %v, comments %q", waited, f.comments[1])
	}
}

// A poll that read the old scenario may still be running when the runner
// replaces it. The ack names the new generation only once that poll has
// finished, so a head the runner creates after the ack gets the new state:
// the interleaving the fixture with a failed CI depends on.
func TestScenarioAckFencesAnOldPoll(t *testing.T) {
	f, srv, tok, sc := setup(t, `{"generation":1,"repos":["`+repo+`"],"poll_seconds":0.01}`)
	ack := filepath.Join(filepath.Dir(sc), "ci.ack")
	f.branches["main"] = head1
	entered, release := make(chan struct{}), make(chan struct{})
	var held sync.Once
	f.onBranches = func() {
		held.Do(func() { close(entered); <-release })
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan int)
	go func() {
		done <- run(ctx, []string{"ci", "-scenario", sc, "-token-file", tok, "-api", srv.URL + "/api/v1", "-ack", ack}, &bytes.Buffer{}, &bytes.Buffer{})
	}()
	defer func() { cancel(); <-done }()
	readAck := func() string { b, _ := os.ReadFile(ack); return strings.TrimSpace(string(b)) }
	waitAck := func(want string) {
		t.Helper()
		for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(5 * time.Millisecond) {
			if readAck() == want {
				return
			}
		}
		t.Fatalf("ack %q, want %q", readAck(), want)
	}

	<-entered // the generation-1 poll is listing the branches
	if got := readAck(); got != "1" {
		t.Fatalf("ack during the first poll: %q", got)
	}
	if err := os.WriteFile(sc, []byte(`{"generation":2,"repos":["`+repo+`"],"poll_seconds":0.01,"ci":{"state":"failure"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	time.Sleep(50 * time.Millisecond)
	if got := readAck(); got != "1" {
		t.Fatalf("generation 2 acked while the generation-1 poll was still running: %q", got)
	}
	close(release)
	waitAck("2")

	// The runner creates the next head only now; it gets generation 2's state.
	f.mu.Lock()
	f.branches["fixture"] = head2
	f.mu.Unlock()
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(5 * time.Millisecond) {
		f.mu.Lock()
		st := f.statuses[head2]
		f.mu.Unlock()
		if len(st) > 0 {
			if len(st) != 1 || st[0]["state"] != "failure" {
				t.Fatalf("the head made after the ack got %v", st)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("no status on the new head")
		}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if st := f.statuses[head1]; len(st) != 1 || st[0]["state"] != "success" {
		t.Errorf("the generation-1 poll's head: %v", st)
	}
}
