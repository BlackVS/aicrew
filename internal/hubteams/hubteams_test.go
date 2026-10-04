package hubteams

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/json"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/BlackVS/aicrew/internal/privatefile"
)

const (
	service     = "aicrew-service"
	teamID      = "01a10828-0000-7000-8000-000000000001"
	registerTok = "aimem_peer_" + "1111111111111111111111111111111111111111111111111111111111111111"
	readTok     = "aimem_peer_" + "2222222222222222222222222222222222222222222222222222222222222222"
)

func newCert(t *testing.T) tls.Certificate {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "aimem-test"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		IPAddresses: []net.IP{net.ParseIP("127.0.0.1")},
		KeyUsage:    x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}, BasicConstraintsValid: true, IsCA: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	leaf, _ := x509.ParseCertificate(der)
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key, Leaf: leaf}
}

type hub struct {
	srv  *httptest.Server
	cert tls.Certificate
	seen []*http.Request
}

func newHub(t *testing.T, h func(w http.ResponseWriter, r *http.Request, body []byte)) *hub {
	t.Helper()
	s := &hub{cert: newCert(t)}
	s.srv = httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		s.seen = append(s.seen, r)
		h(w, r, b)
	}))
	s.srv.TLS = &tls.Config{Certificates: []tls.Certificate{s.cert}}
	s.srv.StartTLS()
	t.Cleanup(s.srv.Close)
	return s
}

func (s *hub) client(t *testing.T) *Client {
	t.Helper()
	sum := sha256.Sum256(s.cert.Leaf.RawSubjectPublicKeyInfo)
	dir := t.TempDir()
	reg, read := filepath.Join(dir, "register.token"), filepath.Join(dir, "read.token")
	for p, v := range map[string]string{reg: registerTok, read: readTok} {
		f, err := privatefile.Create(p)
		if err != nil {
			t.Fatal(err)
		}
		f.WriteString(v + "\n")
		f.Close()
	}
	c, err := New(Config{BaseURL: s.srv.URL, ServiceID: service, TLSMode: "spki_sha256",
		TLSValue: "sha256-" + base64.StdEncoding.EncodeToString(sum[:]), RegisterTokenFile: reg, ReadTokenFile: read})
	if err != nil {
		t.Fatal(err)
	}
	if err := c.CheckCredentials(); err != nil {
		t.Fatal(err)
	}
	return c
}

func refuse(w http.ResponseWriter, status int, code string) {
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]string{"code": code, "message": "m"})
}

// Register sends team.register's credential, the version header and the
// name, under this peer's path, and returns the hub's answer.
func TestRegister(t *testing.T) {
	h := newHub(t, func(w http.ResponseWriter, r *http.Request, body []byte) {
		var req map[string]string
		json.Unmarshal(body, &req)
		if r.Method != http.MethodPut || r.URL.Path != "/v1/identity/peers/"+service+"/team-registrations/"+teamID ||
			r.Header.Get("Authorization") != "Bearer "+registerTok || r.Header.Get(VersionHeader) != "1" {
			refuse(w, http.StatusForbidden, "peer_forbidden")
			return
		}
		switch req["team_name"] {
		case "taken":
			refuse(w, http.StatusConflict, "team_name_taken")
		case "off":
			refuse(w, http.StatusForbidden, "profile_disabled")
		default:
			json.NewEncoder(w).Encode(Registration{ProfileID: "p1", TeamID: teamID, TeamName: req["team_name"], Created: true})
		}
	})
	c := h.client(t)
	r, err := c.Register(context.Background(), teamID, "crew")
	if err != nil || !r.Created || r.TeamName != "crew" || r.ProfileID != "p1" {
		t.Fatalf("register: %+v %v", r, err)
	}
	for name, code := range map[string]string{"taken": "team_name_taken", "off": "profile_disabled"} {
		if _, err := c.Register(context.Background(), teamID, name); Code(err) != code {
			t.Errorf("%s: %v", name, err)
		}
	}
	if _, err := c.Register(context.Background(), "../x", "crew"); Code(err) != "invalid_request" {
		t.Errorf("a bad team ID: %v", err)
	}
}

// ReadTeam and ReadTeams send team.read's credential and accept the hub's
// shapes; a disabled profile has no projects; not_found keeps its code.
func TestRead(t *testing.T) {
	team := Team{TeamID: teamID, TeamName: "crew", Enabled: true, Projects: []Project{
		{Project: "app", Repository: &Repository{Kind: "github", URL: "https://github.com/team/app", Host: "github.com", Access: "write"},
			Process: &ProcessPin{Repo: "https://github.com/team/process", Commit: strings.Repeat("a", 40), Manifest: "m.json"}},
		{Project: "docs"},
	}}
	h := newHub(t, func(w http.ResponseWriter, r *http.Request, _ []byte) {
		if r.Header.Get("Authorization") != "Bearer "+readTok || r.Header.Get(VersionHeader) != "1" {
			refuse(w, http.StatusForbidden, "peer_forbidden")
			return
		}
		switch r.URL.Path {
		case "/v1/identity/peers/" + service + "/team-reads/" + teamID:
			json.NewEncoder(w).Encode(team)
		case "/v1/identity/peers/" + service + "/team-reads":
			json.NewEncoder(w).Encode(map[string]any{"teams": []any{team, Team{TeamID: "other", TeamName: "x", Projects: []Project{}}}})
		default:
			refuse(w, http.StatusNotFound, "not_found")
		}
	})
	c := h.client(t)
	got, err := c.ReadTeam(context.Background(), teamID)
	if err != nil || len(got.Projects) != 2 || got.Projects[0].Repository.Host != "github.com" || got.Projects[1].Repository != nil {
		t.Fatalf("read team: %+v %v", got, err)
	}
	all, err := c.ReadTeams(context.Background())
	if err != nil || len(all) != 2 || all[1].Enabled {
		t.Fatalf("read teams: %+v %v", all, err)
	}
	if _, err := c.ReadTeam(context.Background(), "01a10828-0000-7000-8000-00000000000f"); Code(err) != "not_found" {
		t.Fatalf("unknown team: %v", err)
	}
}

// Answers that break the contract, transport failures, redirects and
// unknown codes are hub_unavailable; no error carries a bearer.
func TestUnavailable(t *testing.T) {
	cases := map[string]func(w http.ResponseWriter, r *http.Request){
		"another team": func(w http.ResponseWriter, r *http.Request) {
			json.NewEncoder(w).Encode(Team{TeamID: "someone-else", TeamName: "x", Enabled: true, Projects: []Project{}})
		},
		"disabled with projects": func(w http.ResponseWriter, r *http.Request) {
			json.NewEncoder(w).Encode(Team{TeamID: teamID, TeamName: "x", Projects: []Project{{Project: "app"}}})
		},
		"no projects field": func(w http.ResponseWriter, r *http.Request) {
			w.Write([]byte(`{"team_id":"` + teamID + `","team_name":"x","enabled":true}`))
		},
		"redirect": func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, "https://elsewhere.example/", http.StatusFound)
		},
		"unknown code": func(w http.ResponseWriter, r *http.Request) { refuse(w, http.StatusBadRequest, "something_new") },
		"not json":     func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("<html>")) },
		"trailing": func(w http.ResponseWriter, r *http.Request) {
			w.Write([]byte(`{"team_id":"` + teamID + `","team_name":"x","enabled":true,"projects":[]} {}`))
		},
	}
	for name, h := range cases {
		hb := newHub(t, func(w http.ResponseWriter, r *http.Request, _ []byte) { h(w, r) })
		_, err := hb.client(t).ReadTeam(context.Background(), teamID)
		if Code(err) != CodeUnavailable || strings.Contains(err.Error(), readTok) {
			t.Errorf("%s: %v", name, err)
		}
	}
	hb := newHub(t, func(w http.ResponseWriter, r *http.Request, _ []byte) {})
	c := hb.client(t)
	hb.srv.Close()
	if _, err := c.ReadTeam(context.Background(), teamID); Code(err) != CodeUnavailable {
		t.Fatalf("a closed hub: %v", err)
	}
	// A rate limit is retryable and keeps its code.
	rl := newHub(t, func(w http.ResponseWriter, r *http.Request, _ []byte) {
		w.Header().Set("Retry-After", "7")
		refuse(w, http.StatusTooManyRequests, "rate_limited")
	})
	_, err := rl.client(t).ReadTeam(context.Background(), teamID)
	var e *Error
	if Code(err) != "rate_limited" || !errorAs(err, &e) || !e.Retryable || e.RetryAfter != 7*time.Second {
		t.Fatalf("rate limited: %v", err)
	}
}

func errorAs(err error, e **Error) bool {
	x, ok := err.(*Error)
	*e = x
	return ok
}

// The configuration is checked without a network call: an https origin, a
// service ID, at least one credential, and two different files.
func TestConfig(t *testing.T) {
	ok := Config{BaseURL: "https://hub.example", ServiceID: service, TLSMode: "ca_dns", TLSValue: "hub.example",
		ReadTokenFile: "read.token"}
	if _, err := New(ok); err != nil {
		t.Fatal(err)
	}
	for name, mod := range map[string]func(*Config){
		"http":        func(c *Config) { c.BaseURL = "http://hub.example" },
		"path":        func(c *Config) { c.BaseURL = "https://hub.example/x" },
		"service":     func(c *Config) { c.ServiceID = "../x" },
		"no files":    func(c *Config) { c.ReadTokenFile = "" },
		"same file":   func(c *Config) { c.RegisterTokenFile = c.ReadTokenFile },
		"wrong trust": func(c *Config) { c.TLSValue = "other.example" },
	} {
		c := ok
		mod(&c)
		if _, err := New(c); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
	c, _ := New(ok)
	if c.CanRegister() || !c.CanRead() {
		t.Fatal("capabilities")
	}
	if _, err := c.Register(context.Background(), teamID, "x"); Code(err) != "no_credential" {
		t.Fatalf("register without a credential: %v", err)
	}
	_ = os.Remove
}
