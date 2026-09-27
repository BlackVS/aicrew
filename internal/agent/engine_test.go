package agent

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"database/sql"
	"encoding/base64"
	"encoding/pem"
	"errors"
	"log/slog"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/BlackVS/aicrew/internal/server"
	"github.com/BlackVS/aicrew/internal/store"
	"github.com/BlackVS/aicrew/internal/tlstrust"
	"github.com/BlackVS/aicrew/internal/verifier"
)

// testVerifier stands in for aimem: it vouches for the fake aimem's
// receipts, for the one seeded identity.
type testVerifier struct{}

func (testVerifier) Redeem(_ context.Context, req store.RedeemRequest) (store.VerifiedIdentity, error) {
	if req.Receipt.Reveal() != fakeReceipt(req.ChallengeID) {
		return store.VerifiedIdentity{}, &verifier.Error{Code: "proof_invalid", Reason: "refused"}
	}
	return store.VerifiedIdentity{HubID: "hub-test", UserID: "user-1", TokenID: "tok-1"}, nil
}

type crewEnv struct {
	store    *store.Store
	cfg      Config
	root     string // the fake aimem's state root
	logs     *bytes.Buffer
	agentID  string
	teamID   string
	operator store.Caller
}

// setupCrew starts aicrewd over real TLS with the test verifier, seeds one
// linked member of a team and writes the agent's configuration.
func setupCrew(t *testing.T) *crewEnv {
	t.Helper()
	ctx := context.Background()
	dir := t.TempDir()
	certFile, keyFile, pin := writeCert(t, dir)
	storePath := filepath.Join(dir, "aicrew.db")
	st, err := store.Open(ctx, storePath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	op, _ := store.OperatorCaller("op-test")
	a, err := st.CreateAgent(ctx, op, "agent", store.NewAgent{Label: "builder"})
	if err != nil {
		t.Fatal(err)
	}
	tm, err := st.CreateTeam(ctx, op, "team", store.NewTeam{Name: "crew"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.AddMember(ctx, op, "member", tm.ID, a.ID, store.RoleWorker); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", storePath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE agents SET linked_hub_id = 'hub-test', linked_user_id = 'user-1', linked_token_id = 'tok-1' WHERE id = ?`, a.ID); err != nil {
		t.Fatal(err)
	}
	db.Close()

	srv, err := server.New(server.Config{StorePath: storePath, ListenAddr: "127.0.0.1:0", TLSCertFile: certFile,
		TLSKeyFile: keyFile, ServiceID: "aicrew-test", ShutdownTimeout: server.Duration(5 * time.Second)},
		st, slog.New(slog.NewTextHandler(new(bytes.Buffer), nil)), server.WithVerifier(testVerifier{}))
	if err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	sctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- srv.Serve(sctx, ln) }()
	t.Cleanup(func() { cancel(); <-done })

	home := filepath.Join(dir, "home")
	root := filepath.Join(dir, "aimem")
	os.MkdirAll(home, 0o700)
	os.MkdirAll(root, 0o700)
	t.Setenv("AICREW_FAKE_AIMEM_ROOT", root)
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cfg := Config{Home: home, URL: "https://" + ln.Addr().String(), Trust: tlstrust.Binding{Mode: tlstrust.SPKI, Value: pin},
		AgentID: a.ID, TeamID: tm.ID, AimemCommand: self}
	return &crewEnv{store: st, cfg: cfg, root: root, logs: new(bytes.Buffer), agentID: a.ID, teamID: tm.ID, operator: op}
}

func writeCert(t *testing.T, dir string) (certFile, keyFile, pin string) {
	t.Helper()
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "aicrewd-test"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		IPAddresses: []net.IP{net.ParseIP("127.0.0.1")}, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		KeyUsage: x509.KeyUsageDigitalSignature}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	leaf, _ := x509.ParseCertificate(der)
	kder, _ := x509.MarshalPKCS8PrivateKey(key)
	certFile, keyFile = filepath.Join(dir, "cert.pem"), filepath.Join(dir, "key.pem")
	os.WriteFile(certFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600)
	os.WriteFile(keyFile, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: kder}), 0o600)
	sum := sha256.Sum256(leaf.RawSubjectPublicKeyInfo)
	return certFile, keyFile, "sha256-" + base64.StdEncoding.EncodeToString(sum[:])
}

func (c *crewEnv) engine(t *testing.T) *Engine {
	t.Helper()
	crew, err := NewCrew(c.cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	return NewEngine(c.cfg, crew, ExecAimem{Command: c.cfg.AimemCommand}, slog.New(slog.NewTextHandler(c.logs, nil)))
}

func (c *crewEnv) handleActive(t *testing.T, handle string) bool {
	t.Helper()
	got, err := c.store.Introspect(context.Background(), handle, "hub-test", "aicrew-test")
	if err != nil {
		t.Fatal(err)
	}
	return got.Active
}

func (c *crewEnv) fileHandle(t *testing.T, sessionID string) string {
	t.Helper()
	raw, err := os.ReadFile(sessionFile(c.root, sessionID))
	if err != nil {
		t.Fatalf("aimem's session file: %v", err)
	}
	var f map[string]string
	if err := jsonUnmarshal(raw, &f); err != nil {
		t.Fatal(err)
	}
	return f["handle"]
}

// noSecrets checks that none of the secrets appears in the state directory,
// the engine's log or any fake aimem argument.
func (c *crewEnv) noSecrets(t *testing.T, secrets ...string) {
	t.Helper()
	var hay strings.Builder
	filepath.Walk(c.cfg.Home, func(p string, info os.FileInfo, err error) error {
		if err == nil && !info.IsDir() {
			b, _ := os.ReadFile(p)
			hay.Write(b)
		}
		return nil
	})
	hay.WriteString(c.logs.String())
	for _, call := range fakeCalls(t, c.root) {
		hay.WriteString(strings.Join(call.Args, " "))
		if call.Event == "proof" {
			secrets = append(secrets, fakeReceipt(flagValue(call.Args, "--challenge")))
		}
	}
	for _, s := range secrets {
		if s != "" && strings.Contains(hay.String(), s) {
			t.Fatalf("a secret reached the state, the log or an argument")
		}
	}
}

func TestStartBindsAndLeaves(t *testing.T) {
	c := setupCrew(t)
	ctx := context.Background()
	e := c.engine(t)
	if err := e.Start(ctx); err != nil {
		t.Fatal(err)
	}
	if e.SessionID() == "" || e.AimemFile() != sessionFile(c.root, e.SessionID()) {
		t.Fatalf("session %q, file %q", e.SessionID(), e.AimemFile())
	}
	handle := c.fileHandle(t, e.SessionID())
	if !c.handleActive(t, handle) {
		t.Fatal("aimem was given an inactive handle")
	}
	if b, err := c.store.AuthenticateSessionToken(ctx, e.token); err != nil || b.SessionID != e.SessionID() {
		t.Fatalf("the engine's token: %+v, %v", b, err)
	}
	st, ok, err := LoadState(c.cfg.Home)
	if err != nil || !ok || st.SessionID != e.SessionID() || st.AimemFile != e.AimemFile() {
		t.Fatalf("state = %+v, %v, %v", st, ok, err)
	}
	c.noSecrets(t, e.token, handle)
	for _, call := range fakeCalls(t, c.root) {
		if call.Event == "open" && !call.StdinPipe {
			t.Fatal("open did not receive the handle on stdin")
		}
	}
	token := e.token
	if err := e.Leave(ctx); err != nil {
		t.Fatal(err)
	}
	if sess, _ := c.store.GetSession(ctx, st.SessionID); sess.State != store.SessionLeft {
		t.Fatalf("session after leave: %s", sess.State)
	}
	if _, err := os.Stat(sessionFile(c.root, st.SessionID)); !os.IsNotExist(err) {
		t.Fatal("aimem's binding was not closed")
	}
	if _, ok, _ := LoadState(c.cfg.Home); ok {
		t.Fatal("the state outlived the session")
	}
	c.noSecrets(t, token, handle)
}

// A restarted client resumes the recorded session: same session, a new
// generation, aimem's existing file refreshed rather than opened again, and
// the old token fenced.
func TestRestartResumes(t *testing.T) {
	c := setupCrew(t)
	ctx := context.Background()
	a := c.engine(t)
	if err := a.Start(ctx); err != nil {
		t.Fatal(err)
	}
	b := c.engine(t)
	if err := b.Start(ctx); err != nil {
		t.Fatal(err)
	}
	if b.SessionID() != a.SessionID() || b.session.Generation != "2" {
		t.Fatalf("resumed %s at generation %s, want %s at 2", b.SessionID(), b.session.Generation, a.SessionID())
	}
	opens := 0
	for _, call := range fakeCalls(t, c.root) {
		if call.Event == "open" {
			opens++
		}
	}
	if opens != 1 {
		t.Fatalf("aimem was opened %d times", opens)
	}
	if _, err := c.store.AuthenticateSessionToken(ctx, a.token); !errors.Is(err, store.ErrTokenInvalid) {
		t.Fatalf("the first client's token after the resume: %v", err)
	}
	if !c.handleActive(t, c.fileHandle(t, b.SessionID())) {
		t.Fatal("aimem holds an inactive handle after the resume")
	}
}

// A recorded session that has ended is forgotten and the team entered anew.
func TestEndedSessionEntersAnew(t *testing.T) {
	c := setupCrew(t)
	ctx := context.Background()
	a := c.engine(t)
	if err := a.Start(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := c.store.StopSession(ctx, c.operator, "stop", a.SessionID()); err != nil {
		t.Fatal(err)
	}
	os.Remove(sessionFile(c.root, a.SessionID())) // aimem would drop it once the hub reports the end
	b := c.engine(t)
	if err := b.Start(ctx); err != nil {
		t.Fatal(err)
	}
	if b.SessionID() == a.SessionID() {
		t.Fatal("resumed an ended session")
	}
}

// Run refreshes the handle before it expires, hands each new handle to
// aimem, and leaves when stopped.
func TestRunRefreshesThenLeaves(t *testing.T) {
	c := setupCrew(t)
	e := c.engine(t)
	clock := newFakeClock()
	e.Now, e.Sleep = clock.now, nil
	e.Crew.(*Crew).now = clock.now
	ctx, cancel := context.WithCancel(context.Background())
	var slept []time.Duration
	e.Sleep = func(ctx context.Context, d time.Duration) error {
		slept = append(slept, d)
		if len(slept) == 3 {
			cancel()
			return ctx.Err()
		}
		clock.advance(d)
		return nil
	}
	if err := e.Start(ctx); err != nil {
		t.Fatal(err)
	}
	if err := e.Run(ctx); err != nil {
		t.Fatalf("run: %v", err)
	}
	// The first wait ends when a third of a 15-minute handle remains
	// (expires_in is whole seconds, so it can be a second or two early).
	if slept[0] > 10*time.Minute || slept[0] < 10*time.Minute-2*time.Second {
		t.Fatalf("first refresh after %v, want about 10m", slept[0])
	}
	refreshes := 0
	for _, call := range fakeCalls(t, c.root) {
		if call.Event == "refresh-end" {
			refreshes++
		}
	}
	if refreshes != 2 {
		t.Fatalf("%d refreshes reached aimem, want 2", refreshes)
	}
	if sess, _ := c.store.GetSession(context.Background(), e.SessionID()); sess.State != store.SessionLeft {
		t.Fatalf("session after stop: %s", sess.State)
	}
}

// Before the token's ceiling, the engine resumes with a new proof instead of
// refreshing.
func TestResumeBeforeCeiling(t *testing.T) {
	c := setupCrew(t)
	ctx := context.Background()
	e := c.engine(t)
	if err := e.Start(ctx); err != nil {
		t.Fatal(err)
	}
	e.tokenEnds = e.Now().Add(resumeLead + time.Minute)
	at, resume := e.next()
	if !resume || at.After(e.Now().Add(time.Minute)) {
		t.Fatalf("next = %v, resume %v", at, resume)
	}
	old := e.token
	if err := e.resume(ctx); err != nil {
		t.Fatal(err)
	}
	if e.session.Generation != "2" || e.token == old {
		t.Fatalf("after the resume: generation %s", e.session.Generation)
	}
	if _, err := c.store.AuthenticateSessionToken(ctx, old); !errors.Is(err, store.ErrTokenInvalid) {
		t.Fatalf("the replaced token: %v", err)
	}
}

func TestNextSchedule(t *testing.T) {
	now := time.Unix(1_000_000, 0)
	e := &Engine{Now: func() time.Time { return now }}
	for _, c := range []struct {
		name      string
		life      time.Duration
		tokenLeft time.Duration
		wantIn    time.Duration
		resume    bool
	}{
		{"a third of a 15-minute handle", 15 * time.Minute, 8 * time.Hour, 10 * time.Minute, false},
		{"the 90 s floor on a short handle", 3 * time.Minute, 8 * time.Hour, 90 * time.Second, false},
		{"the ceiling first", 15 * time.Minute, 15 * time.Minute, 5 * time.Minute, true},
	} {
		e.handleAt, e.handleEnd, e.tokenEnds = now, now.Add(c.life), now.Add(c.tokenLeft)
		at, resume := e.next()
		if at.Sub(now) != c.wantIn || resume != c.resume {
			t.Errorf("%s: in %v resume %v, want %v %v", c.name, at.Sub(now), resume, c.wantIn, c.resume)
		}
	}
}

type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func newFakeClock() *fakeClock { return &fakeClock{t: time.Now()} }
func (f *fakeClock) now() time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.t
}
func (f *fakeClock) advance(d time.Duration) {
	f.mu.Lock()
	f.t = f.t.Add(d)
	f.mu.Unlock()
}
