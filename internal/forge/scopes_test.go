package forge

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/BlackVS/aicrew/internal/forge/fakeforge"
)

// scopeForge answers as Gitea 1.27.3 did on the try: a route whose scope the
// token lacks is refused with the scope named, a route it may use stops at
// the record or the body. answers overrides a route's answer, "METHOD PATH"
// to status and message.
type scopeForge struct {
	scopes  []string
	answers map[string]answer
	mu      sync.Mutex
	seen    []string
}

type answer struct {
	status int
	msg    string
}

func (f *scopeForge) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	key := r.Method + " " + r.URL.Path
	f.mu.Lock()
	f.seen = append(f.seen, key)
	f.mu.Unlock()
	if r.Header.Get("Authorization") != "token "+testToken {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	if r.ContentLength > 0 {
		w.WriteHeader(http.StatusTeapot) // a probe never carries a body
		return
	}
	if a, ok := f.answers[key]; ok {
		w.WriteHeader(a.status)
		w.Write([]byte(`{"message":` + quote(a.msg) + `}`))
		return
	}
	need, ok := map[string]string{
		"PATCH /api/v1/repos/team/app/pulls/0":        "write:repository",
		"POST /api/v1/repos/team/app/issues/0/labels": "write:issue",
		"GET /api/v1/user":                            "read:user",
		"GET /api/v1/repos/team/app":                  "read:repository",
		"GET /api/v1/orgs/team/labels":                "read:organization",
	}[key]
	if !ok {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	held := slices.Contains(f.scopes, need) ||
		need == "read:repository" && slices.Contains(f.scopes, "write:repository")
	if !held {
		w.WriteHeader(http.StatusForbidden)
		w.Write([]byte(`{"message":"token does not have at least one of required scope(s), required=[` + need +
			`], token scope=` + strings.Join(f.scopes, ",") + `"}`))
		return
	}
	switch need {
	case "write:issue":
		w.WriteHeader(http.StatusUnprocessableEntity)
		w.Write([]byte(`{"message":"[]: Empty Content-Type"}`))
	case "read:user":
		w.Write([]byte(`{"id":2,"login":"alice"}`))
	case "read:repository":
		w.Write([]byte(`{"permissions":{"push":true,"pull":true}}`))
	default:
		w.WriteHeader(http.StatusNotFound)
		w.Write([]byte(`{"message":"not found"}`))
	}
}

func quote(s string) string { return `"` + strings.ReplaceAll(s, `"`, `\"`) + `"` }

func scopeServer(t *testing.T, f *scopeForge) (*Client, string) {
	t.Helper()
	srv := httptest.NewTLSServer(f)
	t.Cleanup(srv.Close)
	return &Client{HTTP: srv.Client()}, strings.TrimPrefix(srv.URL, "https://")
}

func rights(rs []RightCheck) map[string]string {
	out := map[string]string{}
	for _, r := range rs {
		out[r.Right] = r.State
	}
	return out
}

// A Gitea token's write scopes are read by two requests aimed at number 0
// with no body: the 1.27.3 answer past the scope check is the scope held, a
// refusal naming the scope is the scope missing. The account's push
// permission, which the forge reports for every token, does not enter.
func TestTokenRightsGitea(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name   string
		scopes []string
		want   map[string]string
	}{
		{"the listed scopes", []string{"write:repository", "write:issue", "read:user"},
			map[string]string{"write:repository": RightPresent, "write:issue": RightPresent}},
		{"no write:issue", []string{"write:repository", "read:user"},
			map[string]string{"write:repository": RightPresent, "write:issue": RightMissing}},
		{"read:repository only", []string{"read:repository", "write:issue", "read:user"},
			map[string]string{"write:repository": RightMissing, "write:issue": RightPresent}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := &scopeForge{scopes: tc.scopes}
			c, host := scopeServer(t, f)
			got := c.TokenRights(ctx, host, Gitea, testToken, "team/app")
			if g := rights(got); !maps(g, tc.want) {
				t.Fatalf("rights %v, want %v (%+v)", g, tc.want, got)
			}
			for _, r := range got {
				if r.State == RightMissing && !strings.Contains(r.Detail, r.Right) {
					t.Errorf("a missing right's detail does not name it: %+v", r)
				}
			}
			if acc, err := c.RepositoryAccess(ctx, host, Gitea, testToken, "team/app"); err != nil || acc != AccessWrite {
				t.Errorf("the account's access: %q, %v", acc, err)
			}
			for _, s := range f.seen {
				if strings.Contains(s, "/0") && !strings.HasSuffix(s, "/pulls/0") && !strings.HasSuffix(s, "/issues/0/labels") {
					t.Errorf("an unexpected request: %s", s)
				}
			}
		})
	}
}

// Any answer other than the two that were observed is not checked, with the
// answer: never missing, never present.
func TestTokenRightsGiteaOtherAnswers(t *testing.T) {
	ctx := context.Background()
	full := []string{"write:repository", "write:issue", "read:user"}
	for name, a := range map[string]answer{
		"a 403 for another reason":                      {http.StatusForbidden, "user should be an owner or a collaborator with write access"},
		"a 403 naming another scope":                    {http.StatusForbidden, "token does not have at least one of required scope(s), required=[write:admin], token scope=x"},
		"a 404 without the message":                     {http.StatusNotFound, "the issue does not exist"},
		"a 404 whose message contains the expected one": {http.StatusNotFound, "repository not found"},
		"a 422 whose message contains the expected one": {http.StatusUnprocessableEntity, "[]: Empty Content-Type; and more"},
		"a 422 for another reason":                      {http.StatusUnprocessableEntity, "invalid input"},
		"a 200":                                         {http.StatusOK, ""},
		"a 500":                                         {http.StatusInternalServerError, "boom"},
		"a 401":                                         {http.StatusUnauthorized, "token is required"},
	} {
		t.Run(name, func(t *testing.T) {
			f := &scopeForge{scopes: full, answers: map[string]answer{
				"PATCH /api/v1/repos/team/app/pulls/0":        a,
				"POST /api/v1/repos/team/app/issues/0/labels": a,
			}}
			c, host := scopeServer(t, f)
			for _, r := range c.TokenRights(ctx, host, Gitea, testToken, "team/app") {
				if r.State != RightNotChecked || !strings.Contains(r.Detail, "status") || !strings.Contains(r.Detail, "Gitea 1.27.3") {
					t.Errorf("%s: %+v", name, r)
				}
			}
		})
	}
	// The labels probe's answer is the pull probe's, and the other way round:
	// each probe has its own answer.
	f := &scopeForge{scopes: full, answers: map[string]answer{
		"PATCH /api/v1/repos/team/app/pulls/0":        {http.StatusUnprocessableEntity, "[]: Empty Content-Type"},
		"POST /api/v1/repos/team/app/issues/0/labels": {http.StatusNotFound, "not found"},
	}}
	c, host := scopeServer(t, f)
	if g := rights(c.TokenRights(ctx, host, Gitea, testToken, "team/app")); g["write:repository"] != RightNotChecked || g["write:issue"] != RightNotChecked {
		t.Errorf("crossed answers: %v", g)
	}
	// A forge that does not answer.
	srv := httptest.NewTLSServer(f)
	host = strings.TrimPrefix(srv.URL, "https://")
	srv.Close()
	for _, r := range c.TokenRights(ctx, host, Gitea, testToken, "team/app") {
		if r.State != RightNotChecked {
			t.Errorf("an unreachable forge: %+v", r)
		}
	}
}

// A Gitea read refused for a scope is a ScopeError naming it, which a
// caller that knows only ErrRejected still reads as a refusal.
func TestScopeErrorOnRead(t *testing.T) {
	c, host := scopeServer(t, &scopeForge{scopes: []string{"write:repository", "write:issue"}})
	_, err := c.WhoAmI(context.Background(), host, Gitea, testToken)
	var se *ScopeError
	if !errors.As(err, &se) || !slices.Equal(se.Scopes, []string{"read:user"}) || !errors.Is(err, ErrRejected) ||
		!strings.Contains(err.Error(), "read:user") {
		t.Fatalf("who-am-I without read:user: %v", err)
	}
	// On GitHub the same words are not read as a scope.
	if missingScopes(GitHub, "token does not have at least one of required scope(s), required=[repo]") != nil {
		t.Error("a GitHub message read as a Gitea scope refusal")
	}
}

// A classic GitHub token's scopes are read from X-OAuth-Scopes on the
// repository's answer; a token whose answer names none, as GitHub documents
// a fine-grained token, is not checked.
func TestTokenRightsGitHub(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name    string
		header  []string // nil: no header
		private bool
		want    string
	}{
		{"repo", []string{"repo, workflow"}, true, RightPresent},
		{"repo among others", []string{"read:org, repo"}, true, RightPresent},
		{"public_repo on a public repository", []string{"public_repo"}, false, RightPresent},
		{"public_repo on a private repository", []string{"public_repo"}, true, RightMissing},
		{"other scopes only", []string{"read:user, gist"}, false, RightMissing},
		{"no header", nil, true, RightNotChecked},
		{"an empty header", []string{""}, true, RightNotChecked},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodGet || r.URL.Path != "/api/v3/repos/team/app" || r.Header.Get("Authorization") != "Bearer "+testToken {
					w.WriteHeader(http.StatusNotFound)
					return
				}
				for _, v := range tc.header {
					w.Header().Add("X-OAuth-Scopes", v)
				}
				if tc.private {
					w.Write([]byte(`{"private":true,"permissions":{"push":true}}`))
				} else {
					w.Write([]byte(`{"private":false,"permissions":{"push":true}}`))
				}
			}))
			defer srv.Close()
			c := &Client{HTTP: srv.Client()}
			got := c.TokenRights(ctx, strings.TrimPrefix(srv.URL, "https://"), GitHub, testToken, "team/app")
			if len(got) != 1 || got[0].Right != "repo" || got[0].State != tc.want {
				t.Fatalf("%+v, want %s", got, tc.want)
			}
			if tc.want != RightPresent && got[0].Detail == "" {
				t.Errorf("no detail: %+v", got[0])
			}
		})
	}
}

// GitLab's token rights are not read: one not-checked line.
func TestTokenRightsGitLab(t *testing.T) {
	got := (&Client{}).TokenRights(context.Background(), "gitlab.example.org", GitLab, testToken, "team/app")
	if len(got) != 1 || got[0].State != RightNotChecked {
		t.Fatalf("%+v", got)
	}
}

// A label the repository defines is added without reading the organization's
// labels, which need read:organization; one defined only there still is.
func TestGiteaLabelsSkipTheOrganization(t *testing.T) {
	ctx := context.Background()
	f := fakeforge.New(t, "gitea")
	c := &Client{HTTP: f.Client}
	if _, err := c.OpenPull(ctx, f.Host, Gitea, fakeforge.Token, fakeforge.Repo, NewPull{Title: "A", Head: "feat/a", Base: "main"}); err != nil {
		t.Fatal(err)
	}
	orgReads := func() int {
		n := 0
		for _, r := range f.Requests {
			if strings.Contains(r, "/orgs/") {
				n++
			}
		}
		return n
	}
	if err := c.AddLabels(ctx, f.Host, Gitea, fakeforge.Token, fakeforge.Repo, 1, []string{"review-this"}); err != nil {
		t.Fatal(err)
	}
	if n := orgReads(); n != 0 {
		t.Fatalf("a repository label read the organization's labels %d times: %v", n, f.Requests)
	}
	if err := c.AddLabels(ctx, f.Host, Gitea, fakeforge.Token, fakeforge.Repo, 1, []string{"review-this", "hands-reviewed"}); err != nil {
		t.Fatal(err)
	}
	if orgReads() == 0 {
		t.Fatal("an organization's label was found without reading the organization's labels")
	}
}

func maps(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}
