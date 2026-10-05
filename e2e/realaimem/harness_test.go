//go:build realaimem

// Package realaimem runs aicrew end to end against a real aimem on an
// isolated local hub (crew-execution b4; docs/E2E-REAL-AIMEM.md). Every
// process is a real binary: `aimem serve` and the aimem CLI built from the
// pinned aimem commit, and this tree's aicrewd, aicrew and aicrew-agent. The
// harness only provisions (TLS, the hub's operator steps, the aicrew team)
// and observes (both stores, both logs); every step runs through
// `aicrew-agent step`, the member's own aimem, aimem's coordination.v1 calls
// to aicrewd and aicrewd's read scope.
//
// Run it with scripts/e2e-real-aimem.sh, not `go test ./...`: it is behind
// the realaimem build tag and needs a local aimem clone.
package realaimem

import (
	"bufio"
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// aimemPin is the aimem commit the harness builds (b4 decision D-b4-1):
// aimem master after #176, past the v0.7.4 release. Beside every earlier
// prerequisite (C5b, C6, C5-w3, `aimem hub credential`, the fixture's
// corrected fences of #164, `hub add --ca-file` of #165) it carries team
// registration and team read (#175): aicrewd names its own team profiles.
const aimemPin = "935873dbf3808cd937d5ecc97ed292bbad5f9c9c"

const (
	serviceID = "aicrew-e2e"
	projectID = "pilot"
	hubName   = "e2e"
	// The process the project selects, which every offer and claim pins:
	// its URL is recorded, never fetched.
	processRepo     = "https://git.example.test/e2e/process.git"
	processManifest = "process/manifest.json"
	// The repository the hub binds to the project, which every offer and
	// claim names: recorded, never fetched.
	repoKind = "gitea"
	repoURL  = "https://git.example.test/e2e/pilot.git"
)

// processCommit and instructionHash are the bootstrap's: the commit of the
// real process repository it makes, and the digest of the manifest's exact
// bytes there (CREW-CONTRACT, "Process pins").
var processCommit, instructionHash string

// harness is one isolated run: a fresh directory, a hub, aicrewd, and the
// members, torn down at the end.
type harness struct {
	t       *testing.T
	root    string // every path the run uses is under it
	bin     string
	report  *report
	aimemV  string // the aimem commit built
	aicrewV string // this tree's commit

	ca          *x509.Certificate
	caKey       *ecdsa.PrivateKey
	caFile      string
	hubPort     int
	hubURL      string
	hubPin      string // sha256-BASE64 of the hub's public key
	aicrewdPort int
	aicrewdURL  string
	aicrewdPin  string
	adminToken  string
	adminFile   string // the admin bearer, one line, owner only
	hubID       string
	teamID      string
	storePath   string
	aicrewdLog  string
	timingLog   string // the aimem wrapper's call timings
	gate        string // the aimem wrapper's pause gates
	procs       []*proc
	members     map[string]*member
	hubProc     *proc
	processDir  string // the process repository the bootstrap makes
	aicrewdProc *proc
	httpClient  *http.Client // trusts the run's CA
	// The fault proxies (b4b-1): the members' and aicrewd's way to the
	// hub, and the members' and the hub's way to aicrewd.
	hubProxy, aicrewdProxy *faultProxy
	// F7: every command's output, and every secret the run made.
	outMu   sync.Mutex
	outputs []string
	secrets []string
	proofs  []string // every captured proof, in order
}

// member is one agent: its own aimem state, its agent home and launcher.
type member struct {
	name     string
	role     string
	dir      string
	env      []string
	home     string // the agent home
	userID   string // aimem user
	agentID  string // aicrew agent
	launcher *proc
	token    string // the admin-issued aimem token, until the client holds it
	code     string // the invitation code, until join types it
	// checkNote is what join's dependency and client check reported, when
	// not ready: the run's client is a stand-in.
	checkNote string
}

// lockedBuffer is a buffer a process writes while the harness reads it.
type lockedBuffer struct {
	mu sync.Mutex
	b  *bytes.Buffer
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

func requireEnv(t *testing.T, name string) string {
	t.Helper()
	v := os.Getenv(name)
	if v == "" {
		t.Skipf("%s is not set: run scripts/e2e-real-aimem.sh", name)
	}
	return v
}

// newHarness builds the binaries and prepares an empty, isolated run
// directory. It refuses anything shared with the host (H).
func newHarness(t *testing.T) *harness {
	t.Helper()
	if runtime.GOOS != "linux" {
		t.Skip("the real-aimem harness runs on Linux (the gate, D-b4-4); Windows is best effort and not wired yet")
	}
	src := requireEnv(t, "AICREW_E2E_AIMEM_SRC")
	// A short root: Unix socket paths are limited to about 104 bytes.
	root, err := os.MkdirTemp("", "ae2e")
	if err != nil {
		t.Fatal(err)
	}
	h := &harness{t: t, root: root, bin: filepath.Join(root, "bin"), members: map[string]*member{}}
	if os.Getenv("AICREW_E2E_KEEP") == "" {
		t.Cleanup(func() { os.RemoveAll(root) })
	} else {
		t.Logf("keeping the run directory %s", root)
	}
	t.Cleanup(h.teardown)
	h.mkdir(h.bin)
	h.gate = h.mkdir(filepath.Join(root, "gate"))
	h.timingLog = filepath.Join(root, "aimem-timing.jsonl")
	h.report = newReport(t, filepath.Join(root, "report.jsonl"))
	h.build(src)
	h.report.run(h)
	return h
}

func (h *harness) mkdir(p string) string {
	h.t.Helper()
	if err := os.MkdirAll(p, 0o700); err != nil {
		h.t.Fatal(err)
	}
	return p
}

// build compiles the pinned aimem from an archive of the local clone (the
// clone is only read) and this tree's binaries.
func (h *harness) build(aimemSrc string) {
	t := h.t
	t.Helper()
	srcDir := h.mkdir(filepath.Join(h.root, "aimem-src"))
	archive := exec.Command("git", "-C", aimemSrc, "archive", "--format=tar", aimemPin)
	untar := exec.Command("tar", "-x", "-C", srcDir)
	pipe, err := archive.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	untar.Stdin = pipe
	var aerr bytes.Buffer
	archive.Stderr = &aerr
	if err := untar.Start(); err != nil {
		t.Fatal(err)
	}
	if err := archive.Run(); err != nil {
		t.Fatalf("git archive %s from %s: %v %s (fetch the pinned commit into that clone first)", aimemPin, aimemSrc, err, aerr.String())
	}
	if err := untar.Wait(); err != nil {
		t.Fatalf("untar aimem: %v", err)
	}
	h.aimemV = aimemPin
	// A source install stamps its version as aimem's release build does,
	// with the commit's `git describe`: v0.7.4 at the pin, which
	// aicrew-agent's dependency check reads as the supported release.
	desc, err := exec.Command("git", "-C", aimemSrc, "describe", "--tags", aimemPin).Output()
	if err != nil {
		t.Fatalf("git describe %s: %v", aimemPin, err)
	}
	h.goBuild(srcDir, filepath.Join(h.bin, "aimem"), "./cmd/aimem",
		"-ldflags", "-X main.version="+strings.TrimSpace(string(desc)))
	repo := repoRoot(t)
	for _, c := range []string{"aicrewd", "aicrew", "aicrew-agent"} {
		h.goBuild(repo, filepath.Join(h.bin, c), "./cmd/"+c)
	}
	out, err := exec.Command("git", "-C", repo, "rev-parse", "HEAD").Output()
	if err != nil {
		t.Fatal(err)
	}
	h.aicrewV = strings.TrimSpace(string(out))
	if dirty, _ := exec.Command("git", "-C", repo, "status", "--porcelain", "--untracked-files=no").Output(); len(dirty) > 0 {
		h.aicrewV += "+dirty"
	}
	h.writeAimemWrapper()
}

func (h *harness) goBuild(dir, out, pkg string, flags ...string) {
	h.t.Helper()
	cmd := exec.Command("go", append(append([]string{"build", "-trimpath"}, flags...), "-o", out, pkg)...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0")
	if b, err := cmd.CombinedOutput(); err != nil {
		h.t.Fatalf("go build %s in %s: %v\n%s", pkg, dir, err, b)
	}
}

func repoRoot(t *testing.T) string {
	t.Helper()
	out, err := exec.Command("go", "env", "GOMOD").Output()
	if err != nil || len(bytes.TrimSpace(out)) == 0 {
		t.Fatalf("go env GOMOD: %v", err)
	}
	return filepath.Dir(strings.TrimSpace(string(out)))
}

// writeAimemWrapper writes the aimem the members' clients run: the real
// aimem, timed for the report. Before a `reservation claim` it can pause on
// a gate the harness opens (S5's race). It logs the command and its two
// leading arguments, which hold no secret.
func (h *harness) writeAimemWrapper() {
	script := fmt.Sprintf(`#!/bin/sh
umask 077
gate=%q
name="$1-$2"
input=""
if [ -e "$gate/capture-$name" ]; then
  rm -f "$gate/capture-$name"
  cat > "$gate/captured-$name"
  input="$gate/captured-$name"
fi
if [ -e "$gate/hold-$name" ]; then
  rm -f "$gate/hold-$name"
  : > "$gate/paused-$name"
  while [ ! -e "$gate/go-$name" ]; do sleep 0.05; done
  rm -f "$gate/go-$name"
fi
start=$(date +%%s%%N)
if [ -n "$input" ]; then
  %q "$@" < "$input"
else
  %q "$@"
fi
rc=$?
if [ -e "$gate/hold-after-$name" ]; then
  rm -f "$gate/hold-after-$name"
  : > "$gate/paused-after-$name"
  while [ ! -e "$gate/go-after-$name" ]; do sleep 0.05; done
  rm -f "$gate/go-after-$name"
fi
end=$(date +%%s%%N)
printf '{"cmd":"%%s","op":"%%s","start_ns":%%s,"end_ns":%%s,"exit":%%s}\n' "$1" "$2" "$start" "$end" "$rc" >> %q
exit $rc
`, h.gate, filepath.Join(h.bin, "aimem"), filepath.Join(h.bin, "aimem"), h.timingLog)
	path := filepath.Join(h.bin, "aimem-timed")
	if err := os.WriteFile(path, []byte(script), 0o700); err != nil {
		h.t.Fatal(err)
	}
}

// isolatedEnv is a process environment built from nothing: PATH, and a home
// and aimem state under dir. No variable is taken from the host but PATH,
// so neither the host's aimem env file, its runtime directory nor its
// state is reachable (H).
func (h *harness) isolatedEnv(dir string, extra ...string) []string {
	h.t.Helper()
	home := h.mkdir(filepath.Join(dir, "home"))
	env := []string{
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + home,
		"USERPROFILE=" + home,
		"TMPDIR=" + h.mkdir(filepath.Join(dir, "tmp")),
		"AIMEM_STATE_DIR=" + h.mkdir(filepath.Join(dir, "aimem")),
		"AIMEM_SOCKET=" + filepath.Join(dir, "a.sock"),
	}
	env = append(env, extra...)
	h.checkIsolated(env)
	return env
}

// checkIsolated refuses an environment that reaches outside the run.
func (h *harness) checkIsolated(env []string) {
	h.t.Helper()
	for _, kv := range env {
		k, v, _ := strings.Cut(kv, "=")
		switch k {
		case "PATH", "AIMEM_HTTP_LISTEN", "AIMEM_HTTP_TOKEN":
			continue
		case "XDG_RUNTIME_DIR", "XDG_STATE_HOME", "XDG_CONFIG_HOME":
			h.t.Fatalf("isolation: %s must not be set", k)
		}
		if strings.HasPrefix(v, "/") && !strings.HasPrefix(v, h.root+"/") {
			h.t.Fatalf("isolation: %s=%s is outside the run directory %s", k, v, h.root)
		}
	}
}

// --- TLS ---------------------------------------------------------------

// newCA makes the run's throwaway CA.
func (h *harness) newCA() {
	t := h.t
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "aicrew e2e CA"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(24 * time.Hour),
		IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	h.ca, _ = x509.ParseCertificate(der)
	h.caKey = key
	h.caFile = filepath.Join(h.root, "ca.pem")
	h.writePEM(h.caFile, "CERTIFICATE", der, 0o644)
	pool := x509.NewCertPool()
	pool.AddCert(h.ca)
	h.httpClient = &http.Client{Timeout: 30 * time.Second,
		Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}}}
}

// leaf issues a server certificate for 127.0.0.1 and localhost, and
// returns its files and its public key's pin.
func (h *harness) leaf(name string) (certFile, keyFile, pin string) {
	t := h.t
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	serial, _ := rand.Int(rand.Reader, big.NewInt(1<<62))
	tmpl := &x509.Certificate{SerialNumber: serial, Subject: pkix.Name{CommonName: name},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(24 * time.Hour),
		DNSNames: []string{"localhost"}, IPAddresses: []net.IP{net.ParseIP("127.0.0.1")},
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, h.ca, &key.PublicKey, h.caKey)
	if err != nil {
		t.Fatal(err)
	}
	kder, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	dir := h.mkdir(filepath.Join(h.root, "tls"))
	certFile, keyFile = filepath.Join(dir, name+".pem"), filepath.Join(dir, name+".key")
	h.writePEM(certFile, "CERTIFICATE", der, 0o644)
	h.writePEM(keyFile, "EC PRIVATE KEY", kder, 0o600)
	spki, err := x509.MarshalPKIXPublicKey(&key.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(spki)
	return certFile, keyFile, "sha256-" + base64.StdEncoding.EncodeToString(sum[:])
}

func (h *harness) writePEM(path, typ string, der []byte, mode os.FileMode) {
	if err := os.WriteFile(path, pem.EncodeToMemory(&pem.Block{Type: typ, Bytes: der}), mode); err != nil {
		h.t.Fatal(err)
	}
}

func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

func secret(t *testing.T, prefix string) string {
	t.Helper()
	var b [24]byte
	if _, err := rand.Read(b[:]); err != nil {
		t.Fatal(err)
	}
	return prefix + hex.EncodeToString(b[:])
}

// writePrivate writes one owner-only file.
func (h *harness) writePrivate(path string, data []byte) {
	h.t.Helper()
	if err := os.WriteFile(path, data, 0o600); err != nil {
		h.t.Fatal(err)
	}
}

// --- processes -----------------------------------------------------------

// proc is a long-running process with its output in a log file.
type proc struct {
	name string
	cmd  *exec.Cmd
	log  string
	done chan error
}

func (h *harness) start(name string, env []string, dir string, args ...string) *proc {
	h.t.Helper()
	logPath := filepath.Join(h.mkdir(filepath.Join(h.root, "logs")), name+".log")
	f, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		h.t.Fatal(err)
	}
	cmd := exec.Command(args[0], args[1:]...)
	cmd.Env, cmd.Dir, cmd.Stdout, cmd.Stderr = env, dir, f, f
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		f.Close()
		h.t.Fatalf("start %s: %v", name, err)
	}
	p := &proc{name: name, cmd: cmd, log: logPath, done: make(chan error, 1)}
	go func() { p.done <- cmd.Wait(); f.Close() }()
	h.procs = append(h.procs, p)
	return p
}

// stop ends p with SIGTERM, then kills it after grace.
func (p *proc) stop(grace time.Duration) error {
	if p.cmd.ProcessState != nil {
		return nil
	}
	_ = p.cmd.Process.Signal(syscall.SIGTERM)
	select {
	case err := <-p.done:
		p.done <- err
		return err
	case <-time.After(grace):
		_ = syscall.Kill(-p.cmd.Process.Pid, syscall.SIGKILL)
		err := <-p.done
		p.done <- err
		return fmt.Errorf("%s did not stop within %s: killed", p.name, grace)
	}
}

func (h *harness) teardown() {
	for i := len(h.procs) - 1; i >= 0; i-- {
		p := h.procs[i]
		if strings.HasPrefix(p.name, "launcher-") && p.cmd.ProcessState == nil {
			// A launcher keeping a session for open work waits out its
			// leave; at the end of a run it is simply ended, with its client.
			_ = killGroup(p)
		}
		if err := p.stop(45 * time.Second); err != nil {
			h.t.Logf("teardown: %v", err)
		}
	}
	h.report.close()
	h.keepArtifacts()
}

// keepArtifacts copies the run's process logs and the aimem call timings to
// AICREW_E2E_ARTIFACTS, if set: the run directory itself lives in a
// temporary directory, which Unix sockets need.
func (h *harness) keepArtifacts() {
	dst := os.Getenv("AICREW_E2E_ARTIFACTS")
	if dst == "" {
		return
	}
	if err := os.MkdirAll(dst, 0o700); err != nil {
		h.t.Logf("artifacts: %v", err)
		return
	}
	files, _ := filepath.Glob(filepath.Join(h.root, "logs", "*.log"))
	files = append(files, h.timingLog)
	for _, f := range files {
		if b, err := os.ReadFile(f); err == nil {
			_ = os.WriteFile(filepath.Join(dst, filepath.Base(f)), b, 0o600)
		}
	}
}

// result is a finished command.
type result struct {
	stdout, stderr string
	code           int
}

// run runs a command to its end, with stdin.
func (h *harness) run(env []string, stdin []byte, args ...string) result {
	h.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, args[0], args[1:]...)
	cmd.Env = env
	if stdin != nil {
		cmd.Stdin = bytes.NewReader(stdin)
	}
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	err := cmd.Run()
	code := 0
	if ee, ok := err.(*exec.ExitError); ok {
		code = ee.ExitCode()
	} else if err != nil {
		h.t.Fatalf("%s: %v", strings.Join(args[:min(3, len(args))], " "), err)
	}
	// Every command's output is kept for F7's scan, except that of the one
	// command whose job is to print a secret once (aimem's token issue).
	if !contains(args, "token-issue-user") {
		h.outMu.Lock()
		h.outputs = append(h.outputs, out.String(), errb.String())
		h.outMu.Unlock()
	}
	return result{stdout: out.String(), stderr: errb.String(), code: code}
}

// knowSecret records a secret F7 must find nowhere it scans.
func (h *harness) knowSecret(s string) {
	s = strings.TrimSpace(s)
	if s == "" {
		return
	}
	h.outMu.Lock()
	h.secrets = append(h.secrets, s)
	h.outMu.Unlock()
}

// knowSecretFile records the contents of a secret file.
func (h *harness) knowSecretFile(path string) {
	b, err := os.ReadFile(path)
	if err != nil {
		h.t.Fatalf("a secret file: %v", err)
	}
	h.knowSecret(string(b))
}

// must runs a command that must succeed.
func (h *harness) must(env []string, stdin []byte, args ...string) string {
	h.t.Helper()
	r := h.run(env, stdin, args...)
	if r.code != 0 {
		h.t.Fatalf("%s exited %d:\n%s\n%s", strings.Join(args[:min(4, len(args))], " "), r.code, r.stdout, r.stderr)
	}
	return r.stdout
}

// waitFor polls cond until it holds or the deadline passes.
func (h *harness) waitFor(what string, limit time.Duration, cond func() bool) {
	h.t.Helper()
	deadline := time.Now().Add(limit)
	for !cond() {
		if time.Now().After(deadline) {
			h.t.Fatalf("timed out after %s waiting for %s", limit, what)
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// --- JSON over HTTPS ----------------------------------------------------

// hubJSON calls the hub's API as its admin.
func (h *harness) hubJSON(method, path string, headers map[string]string, body, out any) int {
	h.t.Helper()
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			h.t.Fatal(err)
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, h.hubURL+path, rd)
	if err != nil {
		h.t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+h.adminToken)
	req.Header.Set("Content-Type", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := h.httpClient.Do(req)
	if err != nil {
		h.t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if out != nil && len(raw) > 0 {
		if err := json.Unmarshal(raw, out); err != nil {
			h.t.Fatalf("%s %s answered %d, not JSON: %s", method, path, resp.StatusCode, raw)
		}
	}
	return resp.StatusCode
}

// lines reads a file's lines, or none.
func lines(path string) []string {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()
	var out []string
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for sc.Scan() {
		out = append(out, sc.Text())
	}
	return out
}

// killGroup kills p and every process it started (it leads its group), as
// a crash would.
func killGroup(p *proc) error {
	return syscall.Kill(-p.cmd.Process.Pid, syscall.SIGKILL)
}
