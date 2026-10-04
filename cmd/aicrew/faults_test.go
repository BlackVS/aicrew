package main

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/BlackVS/aicrew/internal/opapi"
	"github.com/BlackVS/aicrew/internal/optoken"
)

// stub is an aicrewd stand-in for answers the real one never gives: an
// issue that commits but answers without its secret, or not at all. It
// records the revokes it receives.
type stub struct {
	mu      sync.Mutex
	revoked []string
}

func (s *stub) serve(t *testing.T, issue http.HandlerFunc) {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("POST "+opapi.CredentialsPath, issue)
	mux.HandleFunc("POST "+opapi.CredentialRotatePath, issue)
	mux.HandleFunc("POST "+opapi.InvitationsPath, issue)
	revoke := func(w http.ResponseWriter, r *http.Request) {
		var req opapi.IDRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		s.mu.Lock()
		s.revoked = append(s.revoked, req.ID)
		s.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{"id": req.ID})
	}
	mux.HandleFunc("POST "+opapi.CredentialRevokePath, revoke)
	mux.HandleFunc("POST "+opapi.InvitationRevokePath, revoke)
	srv := httptest.NewTLSServer(mux)
	t.Cleanup(srv.Close)
	sum := sha256.Sum256(srv.Certificate().RawSubjectPublicKeyInfo)
	tok, _ := optoken.Generate()
	tokenFile := filepath.Join(t.TempDir(), "operator.token")
	if err := optoken.Write(tokenFile, tok); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AICREW_URL", srv.URL)
	t.Setenv("AICREW_TLS_TRUST_MODE", "spki_sha256")
	t.Setenv("AICREW_TLS_TRUST_VALUE", "sha256-"+base64.StdEncoding.EncodeToString(sum[:]))
	t.Setenv("AICREW_OPERATOR_TOKEN_FILE", tokenFile)
}

func answer(body string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(body))
	}
}

// hangUp drops the connection after the request arrived: the outcome is
// unknown.
func hangUp(w http.ResponseWriter, _ *http.Request) {
	conn, _, err := w.(http.Hijacker).Hijack()
	if err == nil {
		conn.Close()
	}
}

// An issue whose answer lacks its secret revokes what it names; one whose
// answer never arrives tells the operator how to find what it may have
// issued. Neither leaves a secret or code file behind.
func TestIssueAnswerFaults(t *testing.T) {
	onTerminal(t, false)
	for _, tc := range []struct {
		name    string
		handler http.HandlerFunc
		args    []string
		revoked string
		says    string
	}{
		{"credential without bearer", answer(`{"id":"cred-1","hub_id":"hub-a","active":true}`),
			[]string{"hub-credential", "issue", "-hub", "hub-a", "--output"}, "cred-1", "was revoked"},
		{"rotation without replaces", answer(`{"id":"cred-2","hub_id":"hub-a","bearer":"aicrew_introspect_x"}`),
			[]string{"hub-credential", "rotate", "-hub", "hub-a", "--output"}, "cred-2", "was revoked"},
		{"invitation without code", answer(`{"id":"inv-1","team_id":"t","state":"issued"}`),
			[]string{"invitation", "issue", "-team", "t", "-role", "worker", "-hub", "hub-a", "-label", "b", "--output"}, "inv-1", "was revoked"},
		{"credential answer lost", hangUp,
			[]string{"hub-credential", "issue", "-hub", "hub-a", "--output"}, "", "hub-credential list --hub hub-a"},
		{"invitation answer lost", hangUp,
			[]string{"invitation", "issue", "-team", "t", "-role", "worker", "-hub", "hub-a", "-label", "b", "--output"}, "", "invitation list --team t"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := &stub{}
			s.serve(t, tc.handler)
			file := filepath.Join(t.TempDir(), "secret")
			r := cli(t, append(tc.args, file)...)
			if r.code != 1 || !strings.Contains(r.stderr, tc.says) {
				t.Fatalf("exit %d: %s", r.code, r.stderr)
			}
			if _, err := os.Stat(file); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("the secret or code file was left behind")
			}
			if (tc.revoked == "") != (len(s.revoked) == 0) || (tc.revoked != "" && s.revoked[0] != tc.revoked) {
				t.Fatalf("revoked %v, want %q", s.revoked, tc.revoked)
			}
			if strings.Contains(r.stdout+r.stderr, "aicrew_introspect_x") {
				t.Fatal("a bearer was printed")
			}
		})
	}
}

// A URL that is more than an origin is refused before anything is sent.
func TestConnectionRefusesNonOrigins(t *testing.T) {
	serve(t)
	base := os.Getenv("AICREW_URL")
	for _, u := range []string{base + "?", base + "#", base + "/v1", "http://" + strings.TrimPrefix(base, "https://"),
		strings.Replace(base, "https://", "https://user@", 1)} {
		if r := cli(t, "team", "list", "-url", u); r.code != 1 || !strings.Contains(r.stderr, "origin") {
			t.Fatalf("%s: exit %d: %s", u, r.code, r.stderr)
		}
	}
}

// -expires sets the invitation's lifetime; a refused issue leaves no code
// file and issues nothing.
func TestInvitationExpiresAndRefusal(t *testing.T) {
	s, team := invitationStore(t)
	onTerminal(t, false)
	file := filepath.Join(t.TempDir(), "code")
	r := cli(t, "invitation", "issue", "-team", team, "-role", "worker", "-hub", "hub-a", "-label", "b",
		"-expires", "1h", "--output", file)
	var v invitationView
	if r.code != 0 || json.Unmarshal([]byte(r.stdout), &v) != nil {
		t.Fatalf("issue: %d %s %s", r.code, r.stdout, r.stderr)
	}
	if d := v.ExpiresAt.Sub(v.CreatedAt); d < 59*time.Minute || d > 61*time.Minute {
		t.Fatalf("lifetime %s, want 1h", d)
	}
	for _, args := range [][]string{
		{"-team", team, "-role", "worker", "-hub", "hub-a", "-label", "c", "-expires", "100h"},
		{"-team", "no-such-team", "-role", "worker", "-hub", "hub-a", "-label", "c"},
		{"-team", team, "-role", "admin", "-hub", "hub-a", "-label", "c"},
	} {
		f := filepath.Join(t.TempDir(), "code")
		if r := cli(t, append(append([]string{"invitation", "issue"}, args...), "--output", f)...); r.code != 1 {
			t.Fatalf("%v: exit %d %s", args, r.code, r.stderr)
		}
		if _, err := os.Stat(f); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("%v: the code file was left behind", args)
		}
	}
	invs, _ := s.store.ListInvitations(t.Context(), operator(t))
	if len(invs) != 1 {
		t.Fatalf("%d invitations, want the one", len(invs))
	}
}
