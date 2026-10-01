//go:build realaimem

package realaimem

import (
	"bytes"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"regexp"
	"strings"
	"sync"
	"time"
)

// faultProxy is the run's HTTP-aware fault proxy (crew-execution b4b-1,
// D-b4b-1(a)). It terminates its client's TLS with a key generated for the
// run, opens its own TLS to its target, and forwards each HTTP/1.1 request
// unchanged, so the target still terminates TLS itself. A fault is armed
// for the next request matching a method and a path, and fires once:
//   - dropReply: forward the request, read the target's whole answer, then
//     close the client's connection without a byte of it (the change is
//     made; its answer is lost);
//   - dropRequest: close the client's connection without forwarding (the
//     target never sees the request);
//   - delayReply: forward, then hold the answer for a time.
//
// It never presents a key or trusts a CA from outside the run: its server
// key is one of the run's leaves, and it trusts only the run's CA.
type faultProxy struct {
	h      *harness
	name   string
	ln     net.Listener
	url    string
	target string // https origin
	client *http.Client

	mu    sync.Mutex
	rules []*faultRule
}

type faultAction string

const (
	dropReply   faultAction = "drop_reply"
	dropRequest faultAction = "drop_request"
	delayReply  faultAction = "delay_reply"
)

// faultRule is one armed fault.
type faultRule struct {
	Case   string // the skip-the-fault case it belongs to
	Action faultAction
	Method string
	Path   *regexp.Regexp
	Delay  time.Duration
	fired  chan struct{}
	once   sync.Once
}

// newFaultProxy listens on 127.0.0.1 with the run leaf certFile/keyFile and
// forwards to target, trusting only the run's CA.
func (h *harness) newFaultProxy(name, certFile, keyFile, target string) *faultProxy {
	t := h.t
	cert, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{certFile, keyFile} {
		if !strings.HasPrefix(f, h.root+"/") {
			t.Fatalf("the fault proxy's key %s is not the run's", f)
		}
	}
	ln, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12,
		NextProtos: []string{"http/1.1"}})
	if err != nil {
		t.Fatal(err)
	}
	pool := x509.NewCertPool()
	pool.AddCert(h.ca)
	p := &faultProxy{h: h, name: name, ln: ln, url: "https://" + ln.Addr().String(), target: target,
		client: &http.Client{Timeout: 2 * time.Minute, CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		}, Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12},
			ForceAttemptHTTP2: false, TLSNextProto: map[string]func(string, *tls.Conn) http.RoundTripper{}}}}
	srv := &http.Server{Handler: p, ReadHeaderTimeout: 30 * time.Second, ErrorLog: nil}
	go srv.Serve(ln)
	h.t.Cleanup(func() { srv.Close() })
	return p
}

// arm installs a fault for the next matching request. Under
// AICREW_E2E_SKIP_FAULT=<rule.Case> it installs nothing: the scenario then
// runs without its fault, and must fail.
func (p *faultProxy) arm(rule *faultRule) *faultRule {
	rule.fired = make(chan struct{})
	if skipFault(rule.Case) {
		p.h.report.write(map[string]any{"type": "fault", "case": rule.Case, "proxy": p.name, "action": string(rule.Action),
			"method": rule.Method, "path": rule.Path.String(), "skipped": true})
		return rule
	}
	p.mu.Lock()
	p.rules = append(p.rules, rule)
	p.mu.Unlock()
	return rule
}

// skipFault reports whether the run leaves the fault of case c out.
func skipFault(c string) bool {
	return c != "" && os.Getenv("AICREW_E2E_SKIP_FAULT") == c
}

// fired reports whether the rule's fault was injected.
func (r *faultRule) wasFired() bool {
	select {
	case <-r.fired:
		return true
	default:
		return false
	}
}

func (p *faultProxy) take(method, path string) *faultRule {
	p.mu.Lock()
	defer p.mu.Unlock()
	for i, r := range p.rules {
		if r.Method == method && r.Path.MatchString(path) {
			p.rules = append(p.rules[:i], p.rules[i+1:]...)
			return r
		}
	}
	return nil
}

func (p *faultProxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	rule := p.take(r.Method, r.URL.Path)
	if rule != nil {
		p.h.report.write(map[string]any{"type": "fault", "case": rule.Case, "proxy": p.name, "action": string(rule.Action),
			"method": r.Method, "path": r.URL.Path})
		rule.once.Do(func() { close(rule.fired) })
	}
	if rule != nil && rule.Action == dropRequest {
		hangUp(w)
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 8<<20))
	if err != nil {
		hangUp(w)
		return
	}
	out, err := http.NewRequestWithContext(r.Context(), r.Method, p.target+r.URL.RequestURI(), bytes.NewReader(body))
	if err != nil {
		hangUp(w)
		return
	}
	out.Header = r.Header.Clone()
	out.Header.Del("Connection")
	out.ContentLength = int64(len(body))
	resp, err := p.client.Do(out)
	if err != nil {
		hangUp(w)
		return
	}
	answer, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	resp.Body.Close()
	if err != nil {
		hangUp(w)
		return
	}
	if rule != nil {
		switch rule.Action {
		case dropReply:
			hangUp(w)
			return
		case delayReply:
			time.Sleep(rule.Delay)
		}
	}
	for k, v := range resp.Header {
		if k == "Connection" || k == "Content-Length" || k == "Transfer-Encoding" {
			continue
		}
		w.Header()[k] = v
	}
	w.WriteHeader(resp.StatusCode)
	w.Write(answer)
}

// hangUp closes the client's connection without an answer.
func hangUp(w http.ResponseWriter) {
	hj, ok := w.(http.Hijacker)
	if !ok {
		panic(fmt.Sprintf("the fault proxy cannot hang up a %T", w))
	}
	conn, _, err := hj.Hijack()
	if err == nil {
		conn.Close()
	}
}

func route(re string) *regexp.Regexp { return regexp.MustCompile(re) }
