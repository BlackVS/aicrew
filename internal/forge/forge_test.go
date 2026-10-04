package forge

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

const testToken = "forge-token-under-test"

// fake is one forge dialect at 127.0.0.1, answering for one token.
func fake(t *testing.T, k Kind) (*Client, string, *[]string) {
	t.Helper()
	var seen []string
	mux := http.NewServeMux()
	auth := func(r *http.Request) bool {
		switch k {
		case GitHub:
			return r.Header.Get("Authorization") == "Bearer "+testToken
		case Gitea:
			return r.Header.Get("Authorization") == "token "+testToken
		}
		return r.Header.Get("PRIVATE-TOKEN") == testToken
	}
	base := map[Kind]string{GitHub: "/api/v3", Gitea: "/api/v1", GitLab: "/api/v4"}[k]
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		seen = append(seen, r.URL.EscapedPath()+" token="+boolStr(r.Header.Get("Authorization") != "" || r.Header.Get("PRIVATE-TOKEN") != ""))
		switch {
		case k == Gitea && r.URL.Path == "/api/v1/version":
			w.Write([]byte(`{"version":"1.22.0"}`))
			return
		case k == GitHub && r.URL.Path == "/api/v3/meta":
			w.Write([]byte(`{"verifiable_password_authentication":false}`))
			return
		case !strings.HasPrefix(r.URL.Path, base):
			http.NotFound(w, r)
			return
		case !auth(r):
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		p := strings.TrimPrefix(r.URL.EscapedPath(), base)
		switch p {
		case "/user":
			switch k {
			case GitHub:
				w.Write([]byte(`{"id":42,"login":"Example-Bot","name":"Example Bot"}`))
			case Gitea:
				w.Write([]byte(`{"id":7,"login":"example.bot","full_name":"","email":""}`))
			default:
				w.Write([]byte(`{"id":9,"username":"example_bot","name":"Example Bot","commit_email":"bot@example.org"}`))
			}
		case "/repos/team/app", "/projects/team%2Fapp":
			w.Write([]byte(`{"default_branch":"main"}`))
		case "/repos/team/app/branches/main", "/projects/team%2Fapp/repository/branches/main":
			if k == GitLab {
				w.Write([]byte(`{"commit":{"id":"0123456789abcdef0123456789abcdef01234567"}}`))
			} else {
				w.Write([]byte(`{"commit":{"sha":"0123456789abcdef0123456789abcdef01234567"}}`))
			}
		default:
			http.NotFound(w, r)
		}
	})
	srv := httptest.NewTLSServer(mux)
	t.Cleanup(srv.Close)
	return &Client{HTTP: srv.Client()}, strings.TrimPrefix(srv.URL, "https://"), &seen
}

func boolStr(b bool) string {
	if b {
		return "yes"
	}
	return "no"
}

// Detect tells the dialects apart without sending a token; WhoAmI, the
// default branch and the branch head read each dialect's own API with its
// own header.
func TestDialects(t *testing.T) {
	for _, k := range []Kind{GitHub, Gitea, GitLab} {
		t.Run(string(k), func(t *testing.T) {
			c, host, seen := fake(t, k)
			got, err := c.Detect(context.Background(), host)
			if err != nil || got != k {
				t.Fatalf("detect: %q %v", got, err)
			}
			for _, s := range *seen {
				if strings.HasSuffix(s, "token=yes") {
					t.Fatalf("detection sent a token: %v", *seen)
				}
			}
			id, err := c.WhoAmI(context.Background(), host, k, testToken)
			if err != nil || id.Account == "" || id.CommitEmail == "" || id.Name == "" {
				t.Fatalf("who am I: %+v %v", id, err)
			}
			branch, err := c.DefaultBranch(context.Background(), host, k, testToken, "team/app")
			if err != nil || branch != "main" {
				t.Fatalf("default branch: %q %v", branch, err)
			}
			head, err := c.BranchHead(context.Background(), host, k, testToken, "team/app", branch)
			if err != nil || head != "0123456789abcdef0123456789abcdef01234567" {
				t.Fatalf("branch head: %q %v", head, err)
			}
			if _, err := c.WhoAmI(context.Background(), host, k, "wrong"); !errors.Is(err, ErrRejected) {
				t.Fatalf("a wrong token: %v", err)
			}
			if _, err := c.DefaultBranch(context.Background(), host, k, testToken, "team/none"); !errors.Is(err, ErrNotFound) {
				t.Fatalf("a missing repository: %v", err)
			}
		})
	}
}

// The identities the dialects report, with their commit addresses.
func TestIdentities(t *testing.T) {
	want := map[Kind]Identity{
		GitHub: {Account: "Example-Bot", Name: "Example Bot"},
		Gitea:  {Account: "example.bot", Name: "example.bot"},
		GitLab: {Account: "example_bot", Name: "Example Bot", CommitEmail: "bot@example.org"},
	}
	for k, w := range want {
		c, host, _ := fake(t, k)
		id, err := c.WhoAmI(context.Background(), host, k, testToken)
		if err != nil || id.Account != w.Account || id.Name != w.Name {
			t.Fatalf("%s: %+v %v", k, id, err)
		}
		switch k {
		case GitHub:
			if id.CommitEmail != "42+Example-Bot@users.noreply.127.0.0.1" {
				t.Errorf("github commit email %q", id.CommitEmail)
			}
		case Gitea:
			if id.CommitEmail != "example.bot@noreply.127.0.0.1" {
				t.Errorf("gitea commit email %q", id.CommitEmail)
			}
		case GitLab:
			if id.CommitEmail != w.CommitEmail {
				t.Errorf("gitlab commit email %q", id.CommitEmail)
			}
		}
	}
}

// An unreachable host is ErrUnreachable, for detection and for a read.
func TestUnreachable(t *testing.T) {
	srv := httptest.NewTLSServer(http.NotFoundHandler())
	c := &Client{HTTP: srv.Client()}
	host := strings.TrimPrefix(srv.URL, "https://")
	srv.Close()
	if _, err := c.Detect(context.Background(), host); !errors.Is(err, ErrUnreachable) {
		t.Fatalf("detect: %v", err)
	}
	if _, err := c.WhoAmI(context.Background(), host, Gitea, testToken); !errors.Is(err, ErrUnreachable) {
		t.Fatalf("who am I: %v", err)
	}
}

// A redirect to another host is refused, so the token never follows it.
func TestNoRedirectToAnotherHost(t *testing.T) {
	var reached bool
	other := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reached = true
		w.Write([]byte(`{"login":"x"}`))
	}))
	defer other.Close()
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, other.URL+r.URL.Path, http.StatusFound)
	}))
	defer srv.Close()
	c := &Client{HTTP: srv.Client()}
	if _, err := c.WhoAmI(context.Background(), strings.TrimPrefix(srv.URL, "https://"), GitLab, testToken); err == nil || reached {
		t.Fatalf("followed a redirect to another host: %v, reached %v", err, reached)
	}
}

func TestRepositoryAndEncoding(t *testing.T) {
	for in, want := range map[string][2]string{
		"https://github.com/team/app.git":              {"github.com", "team/app"},
		"https://Gitea.Example.org:3000/team/app":      {"gitea.example.org:3000", "team/app"},
		"ssh://git@gitlab.example.org:2222/g/sub/app":  {"gitlab.example.org", "g/sub/app"},
		"git@github.com:team/app.git":                  {"github.com", "team/app"},
		"https://github.com:443/team/app":              {"github.com", "team/app"},
		"https://gitlab.example.org/group/sub/app.git": {"gitlab.example.org", "group/sub/app"},
	} {
		host, path, err := Repository(in)
		if err != nil || host != want[0] || path != want[1] {
			t.Errorf("%s: %q %q %v", in, host, path, err)
		}
	}
	for _, bad := range []string{"", "github.com/team/app", "https://github.com/app", "https://github.com/../x",
		"file:///tmp/repo", "https://user@ho st/a/b", "https://x-token@github.com/team/app", "https://u:p@github.com/team/app"} {
		if _, _, err := Repository(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
	for in, want := range map[string]string{"github.com": "github-com", "gitea.example.org:3000": "gitea-example-org-3000",
		"Example.Bot": "example-bot", "a_b+c": "a-b-c"} {
		if got := EncodePart(in); got != want {
			t.Errorf("EncodePart(%q) = %q, want %q", in, got, want)
		}
	}
	if Service("github.com") != "github-com" {
		t.Fatal("service")
	}
}

// A rate-limited 403 is not a rejected token: it is retried later.
func TestRateLimitIsNotRejection(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-RateLimit-Remaining", "0")
		w.WriteHeader(http.StatusForbidden)
	}))
	defer srv.Close()
	c := &Client{HTTP: srv.Client()}
	_, err := c.WhoAmI(context.Background(), strings.TrimPrefix(srv.URL, "https://"), GitHub, testToken)
	if !errors.Is(err, ErrUnreachable) || errors.Is(err, ErrRejected) {
		t.Fatalf("a rate limit: %v", err)
	}
}
