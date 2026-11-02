// Command simproxy is the pilot simulation's fault proxy
// (docs/DESIGN-PILOT-SIMULATION.md, section 6.1): a compose service in front
// of one TLS service, which the runner arms through a control API to lose
// or hold a request or its reply. It terminates its clients' TLS with the
// target's own certificate and key, which simca issued with the proxy's
// name as an alias, so a client that checks the name or pins the key sees
// the service it expects; it opens its own TLS to the target, trusting
// only the run's CA, and forwards each HTTP/1.1 request unchanged.
//
//	simproxy serve -listen ADDR -cert F -key F -ca F -target https://HOST:PORT
//	               [-control 127.0.0.1:7070] [-proofs FILE]
//	simproxy arm -case C -action drop_request|drop_reply|delay_reply
//	             -method M -path REGEXP [-delay D] [-persist]     prints the rule's id
//	simproxy disarm ID
//	simproxy fired ID                                             exit 0 once it fired
//	simproxy log                                                  the request log, JSON lines
//
// A rule fires on the next request whose method and path match, and is
// removed then unless -persist keeps it until disarm:
//   - drop_request: the connection is closed before the request is
//     forwarded, so the target never sees it;
//   - drop_reply: the request is forwarded and the whole answer read, then
//     the connection is closed without a byte of it;
//   - delay_reply: the answer is held for -delay, then sent.
//
// The control API listens on the container's loopback only: the runner
// reaches it with `docker compose exec`, and no member can. The request log
// names methods, paths and statuses, never a header, query or body. With
// -proofs, every coordination proof in a reply is appended to FILE (owner
// only), for the run's secret scan (case F7); nothing else keeps one.
package main

import (
	"bytes"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

const usage = `usage: simproxy serve -listen ADDR -cert F -key F -ca F -target URL [-control ADDR] [-proofs FILE]
       simproxy arm -case C -action drop_request|drop_reply|delay_reply -method M -path RE [-delay D] [-persist]
       simproxy disarm ID | fired ID | log   [-control ADDR]`

const defaultControl = "127.0.0.1:7070"

func run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, usage)
		return 2
	}
	switch args[0] {
	case "serve":
		return serveCmd(args[1:], stdout, stderr)
	case "arm", "disarm", "fired", "log":
		return clientCmd(args[0], args[1:], stdout, stderr)
	}
	fmt.Fprintln(stderr, usage)
	return 2
}

// ---- the proxy ------------------------------------------------------------------

type action string

const (
	dropRequest action = "drop_request"
	dropReply   action = "drop_reply"
	delayReply  action = "delay_reply"
)

// rule is one armed fault, as the control API takes and reports it.
type rule struct {
	ID      int64  `json:"id"`
	Case    string `json:"case"`
	Action  action `json:"action"`
	Method  string `json:"method"`
	Path    string `json:"path"`
	DelayMS int64  `json:"delay_ms,omitempty"`
	Persist bool   `json:"persist,omitempty"`
	Armed   bool   `json:"armed"`
	Fired   int    `json:"fired"`
	re      *regexp.Regexp
}

// entry is one request in the log.
type entry struct {
	Seq    int64     `json:"seq"`
	At     time.Time `json:"at"`
	Method string    `json:"method"`
	Path   string    `json:"path"`
	// Status is the target's answer; 0 when there was none.
	Status int `json:"status"`
	// Outcome: forwarded, dropped_request, dropped_reply, delayed_reply or
	// upstream_error.
	Outcome string `json:"outcome"`
	Rule    int64  `json:"rule,omitempty"`
	Case    string `json:"case,omitempty"`
}

// maxLog bounds the log a run keeps in memory; the oldest entries go first.
const maxLog = 20000

type proxy struct {
	target string
	client *http.Client
	out    io.Writer // the log's lines, as the container's output
	proofs string    // "" for none

	mu     sync.Mutex
	rules  []*rule // every rule, armed or not, in arming order
	nextID int64
	log    []entry
	seq    int64
}

func newProxy(target string, roots *x509.CertPool, out io.Writer, proofs string) *proxy {
	return &proxy{target: strings.TrimSuffix(target, "/"), out: out, proofs: proofs,
		client: &http.Client{Timeout: 5 * time.Minute,
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
			Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12},
				ForceAttemptHTTP2: false, TLSNextProto: map[string]func(string, *tls.Conn) http.RoundTripper{}}}}
}

func (p *proxy) arm(r rule) (int64, error) {
	re, err := regexp.Compile(r.Path)
	switch {
	case err != nil:
		return 0, fmt.Errorf("path: %w", err)
	case r.Action != dropRequest && r.Action != dropReply && r.Action != delayReply:
		return 0, fmt.Errorf("action %q is not drop_request, drop_reply or delay_reply", r.Action)
	case r.Method == "" || r.Case == "":
		return 0, errors.New("a rule names its case and method")
	case r.Action == delayReply && r.DelayMS <= 0:
		return 0, errors.New("delay_reply needs a delay")
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.nextID++
	r.ID, r.re, r.Armed, r.Fired = p.nextID, re, true, 0
	p.rules = append(p.rules, &r)
	fmt.Fprintf(p.out, "simproxy: armed rule %d (case %s): %s %s %s persist=%t\n", r.ID, r.Case, r.Action, r.Method, r.Path, r.Persist)
	return r.ID, nil
}

func (p *proxy) find(id int64) *rule {
	for _, r := range p.rules {
		if r.ID == id {
			return r
		}
	}
	return nil
}

func (p *proxy) disarm(id int64) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	r := p.find(id)
	if r == nil {
		return false
	}
	if r.Armed {
		r.Armed = false
		fmt.Fprintf(p.out, "simproxy: disarmed rule %d (case %s), fired %d time(s)\n", r.ID, r.Case, r.Fired)
	}
	return true
}

func (p *proxy) status(id int64) (rule, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	r := p.find(id)
	if r == nil {
		return rule{}, false
	}
	return *r, true
}

// take is the first armed rule the request matches, counted as fired; a
// rule without persist is disarmed by it.
func (p *proxy) take(method, path string) *rule {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, r := range p.rules {
		if r.Armed && r.Method == method && r.re.MatchString(path) {
			r.Fired++
			if !r.Persist {
				r.Armed = false
			}
			c := *r
			return &c
		}
	}
	return nil
}

func (p *proxy) record(e entry) {
	p.mu.Lock()
	p.seq++
	e.Seq, e.At = p.seq, time.Now().UTC()
	p.log = append(p.log, e)
	if len(p.log) > maxLog {
		p.log = p.log[len(p.log)-maxLog:]
	}
	p.mu.Unlock()
	line, _ := json.Marshal(e)
	fmt.Fprintf(p.out, "%s\n", line)
}

func (p *proxy) entries() []entry {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]entry(nil), p.log...)
}

func (p *proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	e := entry{Method: r.Method, Path: r.URL.Path, Outcome: "forwarded"}
	ru := p.take(r.Method, r.URL.Path)
	if ru != nil {
		e.Rule, e.Case = ru.ID, ru.Case
	}
	if ru != nil && ru.Action == dropRequest {
		e.Outcome = "dropped_request"
		p.record(e)
		hangUp(w)
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 8<<20))
	if err != nil {
		e.Outcome = "upstream_error"
		p.record(e)
		hangUp(w)
		return
	}
	out, err := http.NewRequestWithContext(r.Context(), r.Method, p.target+r.URL.RequestURI(), bytes.NewReader(body))
	if err != nil {
		e.Outcome = "upstream_error"
		p.record(e)
		hangUp(w)
		return
	}
	out.Header = r.Header.Clone()
	out.Header.Del("Connection")
	out.ContentLength = int64(len(body))
	resp, err := p.client.Do(out)
	if err != nil {
		e.Outcome = "upstream_error"
		p.record(e)
		hangUp(w)
		return
	}
	answer, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	resp.Body.Close()
	e.Status = resp.StatusCode
	if err != nil {
		e.Outcome = "upstream_error"
		p.record(e)
		hangUp(w)
		return
	}
	p.keepProofs(answer)
	if ru != nil {
		switch ru.Action {
		case dropReply:
			e.Outcome = "dropped_reply"
			p.record(e)
			hangUp(w)
			return
		case delayReply:
			// Logged before it is held: the target has answered, which the
			// runner waits for (case F3-aicrewd).
			e.Outcome = "delayed_reply"
			p.record(e)
			time.Sleep(time.Duration(ru.DelayMS) * time.Millisecond)
		}
	}
	if e.Outcome == "forwarded" {
		p.record(e)
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

// keepProofs appends the coordination proofs a JSON answer carries, at any
// depth, to the proofs file.
func (p *proxy) keepProofs(answer []byte) {
	if p.proofs == "" || !bytes.Contains(answer, []byte(`"coordination_proof"`)) {
		return
	}
	var v any
	if json.Unmarshal(answer, &v) != nil {
		return
	}
	var found []string
	var walk func(any)
	walk = func(v any) {
		switch t := v.(type) {
		case map[string]any:
			for k, x := range t {
				if s, ok := x.(string); ok && k == "coordination_proof" && s != "" {
					found = append(found, s)
				}
				walk(x)
			}
		case []any:
			for _, x := range t {
				walk(x)
			}
		}
	}
	walk(v)
	if len(found) == 0 {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	f, err := os.OpenFile(p.proofs, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		fmt.Fprintf(p.out, "simproxy: cannot keep a proof: %v\n", err)
		return
	}
	defer f.Close()
	for _, s := range found {
		fmt.Fprintln(f, s)
	}
}

// hangUp closes the client's connection without an answer.
func hangUp(w http.ResponseWriter) {
	hj, ok := w.(http.Hijacker)
	if !ok {
		panic(fmt.Sprintf("simproxy cannot hang up a %T", w))
	}
	if conn, _, err := hj.Hijack(); err == nil {
		conn.Close()
	}
}

// control is the runner's API.
func (p *proxy) control() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /rules", func(w http.ResponseWriter, r *http.Request) {
		var ru rule
		if err := json.NewDecoder(io.LimitReader(r.Body, 64<<10)).Decode(&ru); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		id, err := p.arm(ru)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		writeJSON(w, map[string]int64{"id": id})
	})
	mux.HandleFunc("GET /rules/{id}", func(w http.ResponseWriter, r *http.Request) {
		id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
		ru, ok := p.status(id)
		if !ok {
			http.NotFound(w, r)
			return
		}
		writeJSON(w, ru)
	})
	mux.HandleFunc("DELETE /rules/{id}", func(w http.ResponseWriter, r *http.Request) {
		id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
		if !p.disarm(id) {
			http.NotFound(w, r)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("GET /log", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/x-ndjson")
		enc := json.NewEncoder(w)
		for _, e := range p.entries() {
			enc.Encode(e)
		}
	})
	return mux
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}

func serveCmd(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("simproxy serve", flag.ContinueOnError)
	fs.SetOutput(stderr)
	listen := fs.String("listen", "", "the address clients connect to")
	certFile := fs.String("cert", "", "the target's certificate")
	keyFile := fs.String("key", "", "the target's key")
	caFile := fs.String("ca", "", "the run's CA, the only one trusted toward the target")
	target := fs.String("target", "", "the target's https origin")
	ctl := fs.String("control", defaultControl, "the control API's loopback address")
	proofs := fs.String("proofs", "", "a file to keep the coordination proofs of the answers in")
	if err := fs.Parse(args); err != nil || fs.NArg() > 0 || *listen == "" || *certFile == "" || *keyFile == "" ||
		*caFile == "" || !strings.HasPrefix(*target, "https://") {
		fmt.Fprintln(stderr, usage)
		return 2
	}
	if host, _, err := net.SplitHostPort(*ctl); err != nil || !net.ParseIP(host).IsLoopback() {
		fmt.Fprintln(stderr, "simproxy: the control API listens on a loopback address only")
		return 2
	}
	cert, err := tls.LoadX509KeyPair(*certFile, *keyFile)
	if err != nil {
		fmt.Fprintln(stderr, "simproxy:", err)
		return 1
	}
	raw, err := os.ReadFile(*caFile)
	roots := x509.NewCertPool()
	if err != nil || !roots.AppendCertsFromPEM(raw) {
		fmt.Fprintln(stderr, "simproxy: no CA certificate in", *caFile)
		return 1
	}
	p := newProxy(*target, roots, stdout, *proofs)
	ln, err := tls.Listen("tcp", *listen, &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12,
		NextProtos: []string{"http/1.1"}})
	if err != nil {
		fmt.Fprintln(stderr, "simproxy:", err)
		return 1
	}
	cln, err := net.Listen("tcp", *ctl)
	if err != nil {
		fmt.Fprintln(stderr, "simproxy:", err)
		return 1
	}
	fmt.Fprintf(stdout, "simproxy: %s -> %s, control on %s\n", *listen, p.target, *ctl)
	errc := make(chan error, 2)
	go func() { errc <- (&http.Server{Handler: p.control(), ReadHeaderTimeout: 10 * time.Second}).Serve(cln) }()
	go func() { errc <- (&http.Server{Handler: p, ReadHeaderTimeout: 30 * time.Second}).Serve(ln) }()
	fmt.Fprintln(stderr, "simproxy:", <-errc)
	return 1
}

// ---- the runner's side ---------------------------------------------------------------

func clientCmd(cmd string, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("simproxy "+cmd, flag.ContinueOnError)
	fs.SetOutput(stderr)
	ctl := fs.String("control", defaultControl, "the control API's address")
	var r rule
	var delay time.Duration
	if cmd == "arm" {
		fs.StringVar(&r.Case, "case", "", "the skip-the-fault case the rule belongs to")
		fs.StringVar((*string)(&r.Action), "action", "", "drop_request, drop_reply or delay_reply")
		fs.StringVar(&r.Method, "method", "", "the request's method")
		fs.StringVar(&r.Path, "path", "", "a regular expression over the request's path")
		fs.DurationVar(&delay, "delay", 0, "delay_reply: how long the answer is held")
		fs.BoolVar(&r.Persist, "persist", false, "keep the rule for every matching request until disarm")
	}
	if err := fs.Parse(args); err != nil {
		return 2
	}
	want := map[string]int{"arm": 0, "disarm": 1, "fired": 1, "log": 0}[cmd]
	if fs.NArg() != want {
		fmt.Fprintln(stderr, usage)
		return 2
	}
	base := "http://" + *ctl
	c := &http.Client{Timeout: 30 * time.Second}
	var (
		resp *http.Response
		err  error
	)
	switch cmd {
	case "arm":
		r.DelayMS = delay.Milliseconds()
		raw, _ := json.Marshal(r)
		resp, err = c.Post(base+"/rules", "application/json", bytes.NewReader(raw))
	case "disarm":
		req, _ := http.NewRequest(http.MethodDelete, base+"/rules/"+fs.Arg(0), nil)
		resp, err = c.Do(req)
	case "fired":
		resp, err = c.Get(base + "/rules/" + fs.Arg(0))
	case "log":
		resp, err = c.Get(base + "/log")
	}
	if err != nil {
		fmt.Fprintln(stderr, "simproxy:", err)
		return 1
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if resp.StatusCode >= 300 {
		fmt.Fprintf(stderr, "simproxy %s: %s: %s", cmd, resp.Status, body)
		return 1
	}
	switch cmd {
	case "arm":
		var out struct {
			ID int64 `json:"id"`
		}
		if json.Unmarshal(body, &out) != nil {
			return 1
		}
		fmt.Fprintln(stdout, out.ID)
	case "fired":
		var ru rule
		if json.Unmarshal(body, &ru) != nil {
			return 1
		}
		fmt.Fprintln(stdout, ru.Fired)
		if ru.Fired == 0 {
			return 1
		}
	case "log":
		stdout.Write(body)
	}
	return 0
}
