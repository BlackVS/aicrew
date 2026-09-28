package server

import (
	"bufio"
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/BlackVS/aicrew/internal/store"
)

// testCert writes a self-signed certificate for 127.0.0.1 and its key, and
// returns their paths and a pool that trusts the certificate.
func testCert(t *testing.T) (certFile, keyFile string, pool *x509.CertPool) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tpl := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "aicrewd test"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		IPAddresses: []net.IP{net.ParseIP("127.0.0.1")},
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}, KeyUsage: x509.KeyUsageDigitalSignature,
		IsCA: true, BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tpl, tpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	certFile, keyFile = filepath.Join(dir, "cert.pem"), filepath.Join(dir, "key.pem")
	if err := os.WriteFile(certFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyFile, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}), 0o600); err != nil {
		t.Fatal(err)
	}
	cert, _ := x509.ParseCertificate(der)
	pool = x509.NewCertPool()
	pool.AddCert(cert)
	return certFile, keyFile, pool
}

// syncBuffer is a log sink safe for the server's goroutines.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// running is a started service.
type running struct {
	srv    *Server
	addr   string
	client *http.Client
	logs   *syncBuffer
	pool   *x509.CertPool
	cancel context.CancelFunc
	done   chan error
	// The service's store and its file, for tests that seed state.
	store     *store.Store
	storePath string
}

func start(t *testing.T, register func(*Server)) *running {
	t.Helper()
	return startWith(t, "aicrew-test", register)
}

func startWith(t *testing.T, serviceID string, register func(*Server)) *running {
	t.Helper()
	certFile, keyFile, pool := testCert(t)
	storePath := filepath.Join(t.TempDir(), "aicrew.db")
	st, err := store.Open(context.Background(), storePath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	cfg := Config{StorePath: storePath, ListenAddr: "127.0.0.1:0", TLSCertFile: certFile, TLSKeyFile: keyFile,
		ServiceID: serviceID, ShutdownTimeout: Duration(5 * time.Second)}
	logs := &syncBuffer{}
	srv, err := New(cfg, st, slog.New(slog.NewJSONHandler(logs, nil)))
	if err != nil {
		t.Fatal(err)
	}
	if register != nil {
		register(srv)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	r := &running{srv: srv, addr: ln.Addr().String(), logs: logs, pool: pool, cancel: cancel, done: make(chan error, 1),
		store: st, storePath: storePath}
	go func() { r.done <- srv.Serve(ctx, ln) }()
	r.client = &http.Client{Timeout: 10 * time.Second, Transport: &http.Transport{
		TLSClientConfig: &tls.Config{RootCAs: pool}, DisableKeepAlives: true}}
	t.Cleanup(func() {
		cancel()
		<-r.done
	})
	return r
}

func (r *running) url(path string) string { return "https://" + r.addr + path }

// declare sends only a request's headers, declaring a body of n bytes, and
// returns the response's status and refusal code.
func (r *running) declare(t *testing.T, method, path string, n int) (int, string) {
	t.Helper()
	code, b := r.head(t, fmt.Sprintf("%s %s HTTP/1.1\r\nHost: x\r\nContent-Length: %d\r\n\r\n", method, path, n))
	var reply struct {
		Code string `json:"code"`
	}
	_ = json.Unmarshal(b, &reply)
	return code, reply.Code
}

// An oversized body is refused without being read, and every refusal closes
// the connection; net/http then skips its post-handler drain, so bytes a
// client is still writing can meet a reset that discards the response. The
// tests therefore never send a socket more than the server reads: a
// refusal decided by the declared length is observed with the headers
// alone (head), and one that needs body bytes runs through the server's
// whole handler chain without a connection (direct).

// head sends a request head declaring a body, and nothing more, and returns
// the response's status and body.
func (r *running) head(t *testing.T, head string) (int, []byte) {
	t.Helper()
	conn, err := tls.Dial("tcp", r.addr, &tls.Config{RootCAs: r.pool})
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
	if _, err := io.WriteString(conn, head); err != nil {
		t.Fatal(err)
	}
	resp, err := http.ReadResponse(bufio.NewReader(conn), &http.Request{Method: http.MethodPost})
	if err != nil {
		t.Fatalf("request head %q: %v", strings.SplitN(head, "\r\n", 2)[0], err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("response to %q: %v", strings.SplitN(head, "\r\n", 2)[0], err)
	}
	return resp.StatusCode, b
}

// unsized hides a body's length: httptest.NewRequest then leaves the
// request's length undeclared, as for a chunked body.
type unsized struct{ io.Reader }

// direct serves req through the server's whole handler chain, middleware
// included, without a connection.
func (r *running) direct(t *testing.T, req *http.Request) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	r.srv.http.Handler.ServeHTTP(rec, req)
	return rec
}

func (r *running) do(t *testing.T, method, path string, body io.Reader) (int, string) {
	t.Helper()
	req, err := http.NewRequest(method, r.url(path), body)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := r.client.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

// raw sends one request line exactly as given, with no body, and returns
// the response's status and Allow header. Nothing cleans the path first.
func (r *running) raw(t *testing.T, method, target string) (int, string) {
	t.Helper()
	conn, err := tls.Dial("tcp", r.addr, &tls.Config{RootCAs: r.pool})
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
	if _, err := fmt.Fprintf(conn, "%s %s HTTP/1.1\r\nHost: x\r\nConnection: close\r\n\r\n", method, target); err != nil {
		t.Fatal(err)
	}
	resp, err := http.ReadResponse(bufio.NewReader(conn), &http.Request{Method: method})
	if err != nil {
		t.Fatalf("%s %s: %v", method, target, err)
	}
	resp.Body.Close()
	return resp.StatusCode, resp.Header.Get("Allow")
}

// The only route is GET /healthz, matched exactly: any other method on it,
// HEAD included, is 405, and any other path, however spelled, is 404, never
// a redirect.
func TestRoutes(t *testing.T) {
	r := start(t, nil)
	if code, body := r.do(t, http.MethodGet, "/healthz", nil); code != http.StatusOK || strings.TrimSpace(body) != `{"status":"ok"}` {
		t.Fatalf("GET /healthz = %d %q", code, body)
	}
	for _, method := range []string{http.MethodHead, http.MethodPost, http.MethodPut, http.MethodDelete, http.MethodOptions} {
		if code, allow := r.raw(t, method, "/healthz"); code != http.StatusMethodNotAllowed || allow != "GET" {
			t.Fatalf("%s /healthz = %d, Allow %q; want 405, Allow GET", method, code, allow)
		}
	}
	// A query does not change the path.
	if code, _ := r.raw(t, http.MethodGet, "/healthz?probe=1"); code != http.StatusOK {
		t.Fatalf("GET /healthz?probe=1 = %d, want 200", code)
	}
	for _, target := range []string{"/", "/v1/crew/introspect/", "/v1/crew/Introspect", "/healthz/", "/healthz/x", "//healthz",
		"/./healthz", "/missing//child", "/a/../healthz", "/HEALTHZ",
		"/%68ealthz", "/healt%68z", "/%2Fhealthz", "/healthz%2F", "/healthz%3F", "https://x/healthz"} {
		if code, _ := r.raw(t, http.MethodGet, target); code != http.StatusNotFound {
			t.Fatalf("GET %s = %d, want 404", target, code)
		}
		if code, _ := r.raw(t, http.MethodHead, target); code != http.StatusNotFound {
			t.Fatalf("HEAD %s = %d, want 404", target, code)
		}
	}
}

// Every connection is TLS 1.2 or later: a plain-HTTP request and a TLS 1.1
// client are refused and never reach a handler.
func TestTLSOnly(t *testing.T) {
	r := start(t, nil)
	conn, err := net.DialTimeout("tcp", r.addr, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
	if _, err := io.WriteString(conn, "GET /healthz HTTP/1.1\r\nHost: x\r\n\r\n"); err != nil {
		t.Fatal(err)
	}
	status, _ := bufio.NewReader(conn).ReadString('\n')
	conn.Close()
	if !strings.Contains(status, "400") {
		t.Fatalf("plain HTTP answered %q, want the server's 400 refusal", status)
	}

	old := &http.Client{Timeout: 5 * time.Second, Transport: &http.Transport{TLSClientConfig: &tls.Config{
		RootCAs: r.pool, MinVersion: tls.VersionTLS10, MaxVersion: tls.VersionTLS11}}}
	if resp, err := old.Get(r.url("/healthz")); err == nil {
		resp.Body.Close()
		t.Fatal("a TLS 1.1 client was served")
	}
	if strings.Contains(r.logs.String(), `"msg":"request"`) {
		t.Fatalf("a refused connection reached a handler: %s", r.logs.String())
	}
	if code, _ := r.do(t, http.MethodGet, "/healthz", nil); code != http.StatusOK {
		t.Fatalf("TLS 1.2+ client = %d", code)
	}
}

// Bodies and headers are bounded, and the server's timeouts are set.
func TestBounds(t *testing.T) {
	type readResult struct {
		n   int64
		err error
	}
	reads := make(chan readResult, 4)
	r := start(t, func(s *Server) {
		s.handle(http.MethodPost, "/echo", func(w http.ResponseWriter, req *http.Request) {
			n, err := io.Copy(io.Discard, req.Body)
			reads <- readResult{n, err}
			w.WriteHeader(http.StatusNoContent)
		})
	})
	big := bytes.Repeat([]byte("x"), MaxBodyBytes+1)
	// Only the headers are sent: the refusal must come from the declared
	// length alone, before any body is read.
	for _, path := range []string{"/echo", "/healthz"} {
		if status, code := r.declare(t, http.MethodPost, path, MaxBodyBytes+1); status != http.StatusRequestEntityTooLarge || code != "request_too_large" {
			t.Fatalf("declared oversized body on %s = %d %q, want 413 request_too_large before routing", path, status, code)
		}
	}
	if code, _ := r.do(t, http.MethodPost, "/echo", bytes.NewReader(big[:MaxBodyBytes])); code != http.StatusNoContent {
		t.Fatalf("a body at the cap = %d, want it served", code)
	}
	if got := <-reads; got.n != MaxBodyBytes || got.err != nil {
		t.Fatalf("a body at the cap read %d bytes, err %v", got.n, got.err)
	}
	// An undeclared (chunked) body is cut off at the cap. It runs through
	// the handler chain directly: over a socket, the server's close could
	// reset the connection while the client is still writing.
	req := httptest.NewRequest(http.MethodPost, "/echo", unsized{bytes.NewReader(big)})
	if rec := r.direct(t, req); rec.Code != http.StatusNoContent {
		t.Fatalf("chunked body over the cap = %d", rec.Code)
	}
	got := <-reads
	var tooLarge *http.MaxBytesError
	if got.n > MaxBodyBytes || !errors.As(got.err, &tooLarge) {
		t.Fatalf("chunked body read %d bytes, err %v; want the cap enforced", got.n, got.err)
	}
	h := r.srv.http
	if h.ReadHeaderTimeout <= 0 || h.ReadTimeout <= 0 || h.WriteTimeout <= 0 || h.IdleTimeout <= 0 || h.MaxHeaderBytes != MaxHeaderBytes {
		t.Fatalf("server bounds = %+v", h)
	}
	if r.srv.tls.MinVersion != tls.VersionTLS12 {
		t.Fatalf("minimum TLS version = %x", r.srv.tls.MinVersion)
	}
}

// Logs name the method, route, status and duration, and never a header, a
// body, the query or the raw path.
func TestLogsCarryNoSecrets(t *testing.T) {
	r := start(t, nil)
	req, _ := http.NewRequest(http.MethodGet, r.url("/healthz?token=query-secret-1"), nil)
	req.Header.Set("Authorization", "Bearer header-secret-2")
	resp, err := r.client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	r.do(t, http.MethodGet, "/path-secret-3", nil)
	r.do(t, http.MethodPost, "/healthz", strings.NewReader("body-secret-4"))
	logs := r.logs.String()
	for _, secret := range []string{"query-secret-1", "header-secret-2", "path-secret-3", "body-secret-4", "Bearer", "token="} {
		if strings.Contains(logs, secret) {
			t.Fatalf("logs contain %q:\n%s", secret, logs)
		}
	}
	for _, want := range []string{`"route":"GET /healthz"`, `"status":200`, `"route":"unmatched"`, `"status":404`, `"duration_ms"`} {
		if !strings.Contains(logs, want) {
			t.Fatalf("logs lack %s:\n%s", want, logs)
		}
	}
}

// Shutdown stops accepting new connections and lets a request in flight
// finish before Serve returns.
func TestGracefulShutdown(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	r := start(t, func(s *Server) {
		s.handle(http.MethodGet, "/slow", func(w http.ResponseWriter, _ *http.Request) {
			close(entered)
			<-release
			w.WriteHeader(http.StatusOK)
		})
	})
	// Runs before start's cleanup, so a failing test never leaves the
	// handler blocked.
	t.Cleanup(unblock)
	result := make(chan int, 1)
	go func() {
		resp, err := r.client.Get(r.url("/slow"))
		if err != nil {
			result <- -1
			return
		}
		resp.Body.Close()
		result <- resp.StatusCode
	}()
	<-entered
	r.cancel()
	// New connections are refused once shutdown has begun.
	deadline := time.Now().Add(5 * time.Second)
	for {
		c, err := net.DialTimeout("tcp", r.addr, time.Second)
		if err != nil {
			break
		}
		c.Close()
		if time.Now().After(deadline) {
			t.Fatal("the listener still accepts after shutdown began")
		}
		time.Sleep(20 * time.Millisecond)
	}
	select {
	case err := <-r.done:
		r.done <- err // for the cleanup
		t.Fatalf("Serve returned %v with a request in flight", err)
	case <-time.After(100 * time.Millisecond):
	}
	unblock()
	if code := <-result; code != http.StatusOK {
		t.Fatalf("in-flight request = %d, want 200", code)
	}
	if err := <-r.done; err != nil {
		t.Fatalf("Serve after shutdown: %v", err)
	}
	r.done <- nil // for the cleanup
}

// A certificate and key that do not load stop the service at start.
func TestNewRefusesBadKeyPair(t *testing.T) {
	certFile, _, _ := testCert(t)
	st, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "aicrew.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	cfg := Config{ListenAddr: "127.0.0.1:0", TLSCertFile: certFile, TLSKeyFile: certFile, ServiceID: "s"}
	if _, err := New(cfg, st, slog.New(slog.NewJSONHandler(io.Discard, nil))); err == nil || !strings.Contains(err.Error(), "tls_key_file") {
		t.Fatalf("New with a certificate as its key = %v", err)
	}
}
