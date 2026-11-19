package agent

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
	"errors"
	"fmt"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/BlackVS/aicrew/internal/tlstrust"
)

// preflightCert is a self-signed certificate for 127.0.0.1, valid from
// notBefore for a day.
func preflightCert(t *testing.T, notBefore time.Time) (tls.Certificate, *x509.Certificate) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tpl := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "aicrewd"},
		NotBefore: notBefore, NotAfter: notBefore.Add(24 * time.Hour),
		IPAddresses: []net.IP{net.ParseIP("127.0.0.1")}, IsCA: true, BasicConstraintsValid: true,
		KeyUsage: x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tpl, tpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	c, _ := x509.ParseCertificate(der)
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}, c
}

// preflightServer serves h over TLS with a certificate valid from notBefore,
// and returns its URL and the roots that trust it.
func preflightServer(t *testing.T, notBefore time.Time, h http.HandlerFunc) (string, *x509.CertPool, *x509.Certificate) {
	t.Helper()
	cert, leaf := preflightCert(t, notBefore)
	srv := httptest.NewUnstartedServer(h)
	srv.TLS = &tls.Config{Certificates: []tls.Certificate{cert}}
	srv.StartTLS()
	t.Cleanup(srv.Close)
	roots := x509.NewCertPool()
	roots.AddCert(leaf)
	return srv.URL, roots, leaf
}

func healthOK(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/healthz" || r.Header.Get("Authorization") != "" {
		http.NotFound(w, r)
		return
	}
	w.Write([]byte(`{"status":"ok","version":"v0.11.0"}` + "\n"))
}

// cutAfterStatus sends a status line and a Content-Length the body never
// reaches, then closes the connection.
func cutAfterStatus(t *testing.T, status int) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		conn, _, err := w.(http.Hijacker).Hijack()
		if err != nil {
			t.Error(err)
			return
		}
		defer conn.Close()
		fmt.Fprintf(conn, "HTTP/1.1 %d %s\r\nContent-Type: application/json\r\nContent-Length: 100\r\n\r\n{\"status\"",
			status, http.StatusText(status))
	}
}

func caDNS() tlstrust.Binding { return tlstrust.Binding{Mode: tlstrust.CADNS, Value: "127.0.0.1"} }

func pinOf(c *x509.Certificate) string {
	sum := sha256.Sum256(c.RawSubjectPublicKeyInfo)
	return "sha256-" + base64.StdEncoding.EncodeToString(sum[:])
}

// One answer per case: each failure is named for what is wrong.
func TestPreflightAicrewd(t *testing.T) {
	now := time.Now()
	url, roots, leaf := preflightServer(t, now.Add(-time.Hour), healthOK)
	futureURL, futureRoots, _ := preflightServer(t, now.Add(48*time.Hour), healthOK)
	wrongURL, wrongRoots, _ := preflightServer(t, now.Add(-time.Hour), func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`<html>a web server</html>`))
	})
	cutURL, cutRoots, _ := preflightServer(t, now.Add(-time.Hour), cutAfterStatus(t, http.StatusOK))
	cut404URL, cut404Roots, _ := preflightServer(t, now.Add(-time.Hour), cutAfterStatus(t, http.StatusNotFound))
	_, otherRoots, other := preflightServer(t, now.Add(-time.Hour), healthOK)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	closed := "https://" + ln.Addr().String()
	ln.Close()
	// The name case resolves nothing: its dialer fails as Go's dialer does
	// for a name the resolver does not know, so no resolver of the host
	// running the test is asked.
	unknownName := func(_ context.Context, network, addr string) (net.Conn, error) {
		host, _, _ := net.SplitHostPort(addr)
		return nil, &net.OpError{Op: "dial", Net: network,
			Err: &net.DNSError{Err: "no such host", Name: host, IsNotFound: true}}
	}

	for _, c := range []struct {
		name  string
		url   string
		trust tlstrust.Binding
		roots *x509.CertPool
		state string
		text  string
		dial  func(context.Context, string, string) (net.Conn, error)
	}{
		{"reachable", url, caDNS(), roots, ReachReachable, "aicrewd v0.11.0 answers", nil},
		{"reachable by pin", url, tlstrust.Binding{Mode: tlstrust.SPKI, Value: pinOf(leaf)}, nil, ReachReachable, "v0.11.0", nil},
		{"name", "https://aicrewd.example:8443", tlstrust.Binding{Mode: tlstrust.CADNS, Value: "aicrewd.example"}, roots,
			ReachName, "aicrewd.example does not resolve", unknownName},
		{"refused", closed, caDNS(), roots, ReachRefused, "nothing answers", nil},
		{"certificate, another CA", url, caDNS(), otherRoots, ReachCertificate, "does not match the CA", nil},
		{"certificate, another pin", url, tlstrust.Binding{Mode: tlstrust.SPKI, Value: pinOf(other)}, nil,
			ReachCertificate, "does not match the pinned key", nil},
		{"clock", futureURL, caDNS(), futureRoots, ReachClock, "check this host's clock", nil},
		{"wrong address", wrongURL, caDNS(), wrongRoots, ReachWrong, "not aicrewd's health route (HTTP 200)", nil},
		{"interrupted", cutURL, caDNS(), cutRoots, ReachInterrupted, "began (HTTP 200), but reading it failed: unexpected EOF", nil},
		{"wrong address, cut", cut404URL, caDNS(), cut404Roots, ReachWrong, "not aicrewd's health route (HTTP 404)", nil},
	} {
		t.Run(c.name, func(t *testing.T) {
			rep, err := Preflight(context.Background(), PreflightOptions{URL: c.url, Trust: c.trust},
				PreflightDeps{Roots: c.roots, Dial: c.dial})
			if err != nil {
				t.Fatal(err)
			}
			if rep.Aicrewd.State != c.state || !strings.Contains(rep.Aicrewd.Detail, c.text) ||
				rep.OK != (c.state == ReachReachable) || rep.Login != nil {
				t.Fatalf("%+v", rep)
			}
		})
	}
}

// The clock answer under a pin: the pin replaces the chain check, so a
// certificate outside its dates still passes, as join would accept it.
func TestPreflightPinIgnoresDates(t *testing.T) {
	url, _, leaf := preflightServer(t, time.Now().Add(48*time.Hour), healthOK)
	rep, err := Preflight(context.Background(), PreflightOptions{URL: url,
		Trust: tlstrust.Binding{Mode: tlstrust.SPKI, Value: pinOf(leaf)}}, PreflightDeps{})
	if err != nil || rep.Aicrewd.State != ReachReachable {
		t.Fatalf("%+v, %v", rep, err)
	}
}

// join's own origin rule: a URL that join refuses is refused before any
// request, and the error never repeats it. A URL with userinfo would
// otherwise send it as Basic authorization and print it.
func TestPreflightRefusesBadInput(t *testing.T) {
	var hits atomic.Int32
	url, roots, _ := preflightServer(t, time.Now().Add(-time.Hour), func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		healthOK(w, r)
	})
	hostPort := strings.TrimPrefix(url, "https://")
	for _, o := range []PreflightOptions{
		{URL: "http://" + hostPort, Trust: caDNS()},
		{URL: "https://member:s3cret@" + hostPort, Trust: caDNS()},
		{URL: "https://s3cret@" + hostPort, Trust: caDNS()},
		{URL: url + "/healthz", Trust: caDNS()},
		{URL: url + "/?token=s3cret", Trust: caDNS()},
		{URL: url + "/?", Trust: caDNS()},
		{URL: url + "/#s3cret", Trust: caDNS()},
		{URL: url, Trust: tlstrust.Binding{Mode: tlstrust.CADNS, Value: "elsewhere"}},
		{URL: url, Trust: caDNS(), Client: "vim"},
	} {
		rep, err := Preflight(context.Background(), o, PreflightDeps{Roots: roots})
		if err == nil || rep.URL != "" || strings.Contains(err.Error(), "s3cret") {
			t.Fatalf("%+v: %+v, %v", o, rep, err)
		}
	}
	if n := hits.Load(); n != 0 {
		t.Fatalf("a refused URL reached the server %d times", n)
	}
	// The same server, with the URL join accepts, answers: the refusals
	// above are the URL's, not the server's.
	if rep, err := Preflight(context.Background(), PreflightOptions{URL: url + "/", Trust: caDNS()},
		PreflightDeps{Roots: roots}); err != nil || rep.Aicrewd.State != ReachReachable || hits.Load() != 1 {
		t.Fatalf("%+v, %v", rep, err)
	}
}

// The login: what claude auth status says, and never more. Its outputs are
// those of Claude Code 2.1.286.
func TestPreflightLogin(t *testing.T) {
	url, roots, _ := preflightServer(t, time.Now().Add(-time.Hour), healthOK)
	stored := `{"loggedIn": true, "authMethod": "claude.ai", "apiProvider": "firstParty", "subscriptionType": "pro"}`
	none := `{"loggedIn": false, "authMethod": "none", "apiProvider": "firstParty"}`
	for _, c := range []struct {
		name   string
		client string
		out    string
		code   int
		err    error
		state  string
		ok     bool
		text   string
	}{
		{"stored", "claude", stored, 0, nil, LoginStored, true, "whether it is valid is not checked"},
		{"none", "claude", none, 1, nil, LoginNone, false, "run `claude` and log in"},
		{"not installed", "claude", "", 0, exec.ErrNotFound, LoginNoClient, false, "not installed"},
		{"unreadable", "claude", "Usage: claude", 1, nil, LoginUnreadable, false, "no answer"},
		{"failed to run", "claude", "", 0, errors.New("signal: killed"), LoginUnreadable, false, "killed"},
		{"opencode", "opencode", "", 0, nil, LoginNoCheck, true, "no check of its login without a model call"},
	} {
		t.Run(c.name, func(t *testing.T) {
			calls := 0
			rep, err := Preflight(context.Background(), PreflightOptions{URL: url, Trust: caDNS(), Client: c.client},
				PreflightDeps{Roots: roots, AuthStatus: func(context.Context) ([]byte, int, error) {
					calls++
					return []byte(c.out), c.code, c.err
				}})
			if err != nil {
				t.Fatal(err)
			}
			l := rep.Login
			if l == nil || l.State != c.state || !strings.Contains(l.Detail, c.text) || rep.OK != c.ok {
				t.Fatalf("%+v %+v", rep, l)
			}
			if c.state == LoginStored && (strings.Contains(l.Detail, "login is valid") || !strings.Contains(l.Detail, "$0.002")) {
				t.Fatalf("a stored login is reported as more than stored, or the model call is not named: %s", l.Detail)
			}
			if want := map[bool]int{true: 0, false: 1}[c.client == "opencode"]; calls != want {
				t.Fatalf("auth status ran %d times", calls)
			}
		})
	}
}

// fakeClaudeAuthEnv makes the test binary answer `claude auth status
// --json` as Claude Code 2.1.286 does with no login stored: a provider
// variable alone makes it report a login.
const fakeClaudeAuthEnv = "AICREW_FAKE_CLAUDE_AUTH"

func init() {
	if os.Getenv(fakeClaudeAuthEnv) != "1" || len(os.Args) != 4 || os.Args[1] != "auth" {
		return
	}
	for _, k := range []string{"ANTHROPIC_API_KEY", "ANTHROPIC_AUTH_TOKEN", "CLAUDE_CODE_OAUTH_TOKEN",
		"CLAUDE_CODE_USE_BEDROCK", "CLAUDE_CODE_USE_VERTEX"} {
		if os.Getenv(k) != "" {
			fmt.Printf(`{"loggedIn": true, "authMethod": "from %s", "configDirectory": %q}`+"\n", k, os.Getenv("CLAUDE_CONFIG_DIR"))
			os.Exit(0)
		}
	}
	fmt.Printf(`{"loggedIn": false, "authMethod": "none", "configDirectory": %q}`+"\n", os.Getenv("CLAUDE_CONFIG_DIR"))
	os.Exit(1)
}

// A provider variable in the caller's shell does not reach auth status:
// the member's service has none, so it would report a login the service
// does not have. CLAUDE_CONFIG_DIR still does.
func TestPreflightLoginIgnoresProviderVariables(t *testing.T) {
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	name := "claude"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	b, err := os.ReadFile(self)
	if err != nil {
		t.Fatal(err)
	}
	fake := filepath.Join(dir, name)
	if err := os.WriteFile(fake, b, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
	t.Setenv(fakeClaudeAuthEnv, "1")
	t.Setenv("CLAUDE_CONFIG_DIR", "/members/worker/.claude")
	for _, k := range []string{"ANTHROPIC_API_KEY", "ANTHROPIC_AUTH_TOKEN", "CLAUDE_CODE_OAUTH_TOKEN",
		"CLAUDE_CODE_USE_BEDROCK", "CLAUDE_CODE_USE_VERTEX"} {
		t.Run(k, func(t *testing.T) {
			t.Setenv(k, "made-up")
			// The fake does answer to the variable: with the caller's whole
			// environment it reports a login.
			if out, err := exec.Command(fake, "auth", "status", "--json").Output(); err != nil ||
				!strings.Contains(string(out), `"loggedIn": true`) {
				t.Fatalf("the fake ignores %s: %q %v", k, out, err)
			}
			out, code, err := claudeAuthStatus(context.Background())
			if err != nil || code != 1 || !strings.Contains(string(out), `"loggedIn": false`) ||
				!strings.Contains(string(out), "/members/worker/.claude") {
				t.Fatalf("%s reached auth status, or CLAUDE_CONFIG_DIR did not: %q %d %v", k, out, code, err)
			}
			if l := clientLogin(context.Background(), "claude", PreflightDeps{}); l.State != LoginNone {
				t.Fatalf("%+v", l)
			}
		})
	}
}
