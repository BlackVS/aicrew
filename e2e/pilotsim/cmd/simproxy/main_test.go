package main

import (
	"bufio"
	"bytes"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/BlackVS/aicrew/e2e/testca"
)

// fixture is a TLS target and the proxy in front of it, both with the
// target's certificate, as in the simulation.
type fixture struct {
	p      *proxy
	url    string // the proxy's
	client *http.Client
	out    *syncBuffer
	proofs string

	mu   sync.Mutex
	seen map[string]int // the target's requests by path
}

type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	ca, err := testca.New("simproxy test CA", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	leaf, err := ca.Leaf("localhost", []string{"localhost"}, []net.IP{net.ParseIP("127.0.0.1")}, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	certFile, keyFile := filepath.Join(dir, "t.pem"), filepath.Join(dir, "t.key")
	if err := testca.WritePEM(certFile, "CERTIFICATE", leaf.CertDER, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := testca.WritePEM(keyFile, "EC PRIVATE KEY", leaf.KeyDER, 0o600); err != nil {
		t.Fatal(err)
	}
	cert, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		t.Fatal(err)
	}
	pool := x509.NewCertPool()
	pool.AddCert(ca.Cert)

	f := &fixture{seen: map[string]int{}, out: &syncBuffer{}, proofs: filepath.Join(dir, "proofs")}
	target := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.seen[r.URL.Path]++
		f.mu.Unlock()
		body, _ := io.ReadAll(r.Body)
		if r.URL.Path == "/begin" {
			w.Header().Set("Content-Type", "application/json")
			io.WriteString(w, `{"attempt_id":"a1","step":{"request_key":"k","coordination_proof":"proof-`+string(body)+`"}}`)
			return
		}
		w.WriteHeader(http.StatusCreated)
		io.WriteString(w, "answer to "+r.Method+" "+string(body))
	}))
	target.TLS = &tls.Config{Certificates: []tls.Certificate{cert}}
	target.StartTLS()
	t.Cleanup(target.Close)

	f.p = newProxy(target.URL, pool, f.out, f.proofs)
	ln, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{Certificates: []tls.Certificate{cert}, NextProtos: []string{"http/1.1"}})
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: f.p}
	go srv.Serve(ln)
	t.Cleanup(func() { srv.Close() })
	f.url = "https://" + ln.Addr().String()
	f.client = &http.Client{Timeout: 10 * time.Second, Transport: &http.Transport{DisableKeepAlives: true,
		TLSClientConfig: &tls.Config{RootCAs: pool, ServerName: "localhost"}}}
	return f
}

func (f *fixture) count(path string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.seen[path]
}

func (f *fixture) post(path, body string) (string, error) {
	resp, err := f.client.Post(f.url+path, "text/plain", strings.NewReader(body))
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	return string(b), err
}

func (f *fixture) arm(t *testing.T, r rule) int64 {
	t.Helper()
	id, err := f.p.arm(r)
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func (f *fixture) outcomes() []string {
	var out []string
	for _, e := range f.p.entries() {
		out = append(out, e.Method+" "+e.Path+" "+e.Outcome)
	}
	return out
}

func TestForwardsUnchanged(t *testing.T) {
	f := newFixture(t)
	got, err := f.post("/x", "hello")
	if err != nil || got != "answer to POST hello" {
		t.Fatalf("%q %v", got, err)
	}
	if o := f.outcomes(); len(o) != 1 || o[0] != "POST /x forwarded" {
		t.Errorf("log %q", o)
	}
	if e := f.p.entries()[0]; e.Status != http.StatusCreated || e.Seq != 1 {
		t.Errorf("entry %+v", e)
	}
}

// A rule without persist fires once: the next request goes through.
func TestDropRequestOnce(t *testing.T) {
	f := newFixture(t)
	id := f.arm(t, rule{Case: "F1-claim-request", Action: dropRequest, Method: "POST", Path: `^/op$`})
	if _, err := f.post("/op", "1"); err == nil {
		t.Fatal("the dropped request was answered")
	}
	if n := f.count("/op"); n != 0 {
		t.Fatalf("the target saw %d dropped request(s)", n)
	}
	if _, err := f.post("/op", "2"); err != nil || f.count("/op") != 1 {
		t.Fatalf("the next request: %v, target saw %d", err, f.count("/op"))
	}
	r, _ := f.p.status(id)
	if r.Fired != 1 || r.Armed {
		t.Errorf("rule %+v", r)
	}
	if o := f.outcomes(); strings.Join(o, ",") != "POST /op dropped_request,POST /op forwarded" {
		t.Errorf("log %q", o)
	}
	if e := f.p.entries()[0]; e.Rule != id || e.Case != "F1-claim-request" || e.Status != 0 {
		t.Errorf("entry %+v", e)
	}
}

// A persisting rule drops every matching reply until it is disarmed; the
// target saw and answered each. Other methods and paths pass.
func TestDropReplyPersists(t *testing.T) {
	f := newFixture(t)
	id := f.arm(t, rule{Case: "c", Action: dropReply, Method: "POST", Path: `^/op$`, Persist: true})
	for i := 0; i < 2; i++ {
		if _, err := f.post("/op", "x"); err == nil {
			t.Fatal("a dropped reply arrived")
		}
	}
	if f.count("/op") != 2 {
		t.Fatalf("the target saw %d", f.count("/op"))
	}
	if _, err := f.post("/other", "x"); err != nil {
		t.Fatal(err)
	}
	resp, err := f.client.Get(f.url + "/op")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if !f.p.disarm(id) {
		t.Fatal("disarm")
	}
	if got, err := f.post("/op", "y"); err != nil || got != "answer to POST y" {
		t.Fatalf("after disarm: %q %v", got, err)
	}
	r, _ := f.p.status(id)
	if r.Fired != 2 || r.Armed {
		t.Errorf("rule %+v", r)
	}
	if e := f.p.entries()[0]; e.Outcome != "dropped_reply" || e.Status != http.StatusCreated {
		t.Errorf("entry %+v", e)
	}
	if f.p.disarm(99) {
		t.Error("disarmed an unknown rule")
	}
}

func TestDelayReply(t *testing.T) {
	f := newFixture(t)
	f.arm(t, rule{Case: "c", Action: delayReply, Method: "POST", Path: `^/op$`, DelayMS: 300})
	start := time.Now()
	logged := make(chan time.Duration, 1)
	go func() {
		for len(f.p.entries()) == 0 {
			time.Sleep(5 * time.Millisecond)
		}
		logged <- time.Since(start)
	}()
	got, err := f.post("/op", "z")
	if d := <-logged; d >= 300*time.Millisecond {
		t.Errorf("the held answer was logged after %v, not before it was held", d)
	}
	if err != nil || got != "answer to POST z" {
		t.Fatalf("%q %v", got, err)
	}
	if d := time.Since(start); d < 300*time.Millisecond {
		t.Errorf("answered after %v", d)
	}
	if o := f.outcomes(); o[0] != "POST /op delayed_reply" {
		t.Errorf("log %q", o)
	}
}

func TestBadRules(t *testing.T) {
	f := newFixture(t)
	for _, r := range []rule{
		{Case: "c", Action: "drop_all", Method: "POST", Path: "x"},
		{Case: "c", Action: dropReply, Method: "POST", Path: "("},
		{Action: dropReply, Method: "POST", Path: "x"},
		{Case: "c", Action: dropReply, Path: "x"},
		{Case: "c", Action: delayReply, Method: "POST", Path: "x"},
	} {
		if _, err := f.p.arm(r); err == nil {
			t.Errorf("armed %+v", r)
		}
	}
}

// The proofs in answers are kept in the proofs file, owner only, and the
// log names none of them.
func TestKeepsProofs(t *testing.T) {
	f := newFixture(t)
	for _, b := range []string{"one", "two"} {
		if _, err := f.post("/begin", b); err != nil {
			t.Fatal(err)
		}
	}
	raw, err := os.ReadFile(f.proofs)
	if err != nil || string(raw) != "proof-one\nproof-two\n" {
		t.Fatalf("%q %v", raw, err)
	}
	if fi, _ := os.Stat(f.proofs); runtime.GOOS != "windows" && fi.Mode().Perm() != 0o600 {
		t.Errorf("mode %v", fi.Mode().Perm())
	}
	if strings.Contains(f.out.String(), "proof-") {
		t.Errorf("the output names a proof: %s", f.out.String())
	}
}

// The runner's commands against the control API.
func TestControlCommands(t *testing.T) {
	f := newFixture(t)
	ctl := httptest.NewServer(f.p.control())
	defer ctl.Close()
	addr := strings.TrimPrefix(ctl.URL, "http://")
	cmd := func(args ...string) (int, string, string) {
		var out, errb bytes.Buffer
		code := run(append(args, "-control", addr), &out, &errb)
		return code, strings.TrimSpace(out.String()), errb.String()
	}
	// The flags go before the arguments, as flag parses them.
	ctlCmd := func(name, arg string) (int, string, string) {
		var out, errb bytes.Buffer
		code := run([]string{name, "-control", addr, arg}, &out, &errb)
		return code, strings.TrimSpace(out.String()), errb.String()
	}
	code, id, errs := cmd("arm", "-case", "F2-begin", "-action", "drop_reply", "-method", "POST", "-path", `^/v1/crew/attempts$`)
	if code != 0 || id != "1" {
		t.Fatalf("arm: %d %q %s", code, id, errs)
	}
	if code, n, _ := ctlCmd("fired", id); code != 1 || n != "0" {
		t.Fatalf("fired before: %d %q", code, n)
	}
	if _, err := f.post("/v1/crew/attempts", "x"); err == nil {
		t.Fatal("the reply arrived")
	}
	if code, n, _ := ctlCmd("fired", id); code != 0 || n != "1" {
		t.Fatalf("fired after: %d %q", code, n)
	}
	if code, _, _ := ctlCmd("disarm", id); code != 0 {
		t.Fatal("disarm")
	}
	if code, _, _ := ctlCmd("fired", "7"); code != 1 {
		t.Fatal("fired of an unknown rule")
	}
	code, log, _ := cmd("log")
	if code != 0 {
		t.Fatal("log")
	}
	var entries []entry
	sc := bufio.NewScanner(strings.NewReader(log))
	for sc.Scan() {
		var e entry
		if err := json.Unmarshal(sc.Bytes(), &e); err != nil {
			t.Fatalf("%q: %v", sc.Text(), err)
		}
		entries = append(entries, e)
	}
	if len(entries) != 1 || entries[0].Outcome != "dropped_reply" || entries[0].Path != "/v1/crew/attempts" {
		t.Errorf("log %+v", entries)
	}
	if code, _, errs := cmd("arm", "-case", "c", "-action", "nope", "-method", "POST", "-path", "x"); code != 1 ||
		!strings.Contains(errs, "nope") {
		t.Errorf("a bad rule: %d %s", code, errs)
	}
	if code, _, _ := cmd("arm", "extra"); code != 2 {
		t.Error("arm with an argument")
	}
}

func TestUsage(t *testing.T) {
	for _, args := range [][]string{nil, {"nope"}, {"serve"}, {"serve", "-listen", ":1", "-cert", "c", "-key", "k", "-ca", "a",
		"-target", "http://x"}, {"serve", "-listen", ":1", "-cert", "c", "-key", "k", "-ca", "a", "-target", "https://x",
		"-control", "0.0.0.0:7070"}, {"disarm"}, {"fired"}} {
		var out, errb bytes.Buffer
		if code := run(args, &out, &errb); code != 2 {
			t.Errorf("%q: exit %d", args, code)
		}
	}
}
