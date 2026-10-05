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

	"github.com/BlackVS/aicrew/internal/hubteams"
	"github.com/BlackVS/aicrew/internal/optoken"
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
	store     *store.Store
	storePath string
	cfg       Config
	root      string // the fake aimem's state root
	logs      *bytes.Buffer
	agentID   string
	teamID    string
	operator  store.Caller
}

// setupCrew starts aicrewd over real TLS with the test verifier, seeds one
// linked member of a team and writes the agent's configuration.
func setupCrew(t *testing.T) *crewEnv {
	t.Helper()
	return setupCrewWith(t, crewOptions{role: store.RoleWorker})
}

// crewOptions shape setupCrewWith: the member's role, and for step tests a
// team project and aimem's read scope over the fake aimem's state.
type crewOptions struct {
	role  store.Role
	steps bool
	// shortHome puts the agent home in a short temporary directory, for a
	// test that serves the step socket: a Unix socket's path is limited to
	// about 104 bytes.
	shortHome bool
}

// grantingHub is the team's hub: it grants every team project-t.
type grantingHub struct{}

func (grantingHub) CanRegister() bool { return false }
func (grantingHub) CanRead() bool     { return true }
func (grantingHub) Register(context.Context, string, string) (hubteams.Registration, error) {
	return hubteams.Registration{}, errors.New("not used")
}
func (grantingHub) ReadTeam(_ context.Context, id string) (hubteams.Team, error) {
	return hubteams.Team{TeamID: id, TeamName: "crew", Enabled: true, Projects: []hubteams.Project{{Project: "project-t"}}}, nil
}
func (grantingHub) ReadTeams(context.Context) ([]hubteams.Team, error) { return nil, nil }

func setupCrewWith(t *testing.T, o crewOptions) *crewEnv {
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
	nt := store.NewTeam{Name: "crew"}
	if o.steps {
		nt.Hub = "main"
	}
	tm, err := st.CreateTeam(ctx, op, "team", nt)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.AddMember(ctx, op, "member", tm.ID, a.ID, o.role); err != nil {
		t.Fatal(err)
	}
	if o.steps {
		read := store.TeamGrantsRead{State: store.GrantsEnabled, At: time.Now(),
			Grants: []store.TeamGrant{{HubID: "hub-test", ProjectID: "project-t"}}}
		if _, err := st.RecordTeamGrants(ctx, store.ReconcilerCaller(), tm.ID, read); err != nil {
			t.Fatal(err)
		}
	}
	db, err := sql.Open("sqlite", storePath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE agents SET linked_hub_id = 'hub-test', linked_user_id = 'user-1', linked_token_id = 'tok-1' WHERE id = ?`, a.ID); err != nil {
		t.Fatal(err)
	}
	db.Close()

	root := filepath.Join(dir, "aimem")
	opts := []server.Option{server.WithVerifier(testVerifier{})}
	if o.steps {
		opts = append(opts, server.WithReader(fileReader{root: root}), server.WithHub("main", "hub-test", grantingHub{}))
	}
	srv, err := server.New(server.Config{StorePath: storePath, ListenAddr: "127.0.0.1:0", TLSCertFile: certFile,
		TLSKeyFile: keyFile, ServiceID: "aicrew-test", ShutdownTimeout: server.Duration(5 * time.Second),
		OperatorTokenFile: operatorTokenFile(t)},
		st, slog.New(slog.NewTextHandler(new(bytes.Buffer), nil)), opts...)
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
	if o.shortHome {
		if home, err = os.MkdirTemp("", "ah"); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { os.RemoveAll(home) })
	}
	os.MkdirAll(home, 0o700)
	os.MkdirAll(root, 0o700)
	t.Setenv("AICREW_FAKE_AIMEM_ROOT", root)
	foreignInstallation(t, home)
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cfg := Config{Home: home, URL: "https://" + ln.Addr().String(), Trust: tlstrust.Binding{Mode: tlstrust.SPKI, Value: pin},
		AgentID: a.ID, TeamID: tm.ID, AimemCommand: self}
	return &crewEnv{store: st, storePath: storePath, cfg: cfg, root: root, logs: new(bytes.Buffer), agentID: a.ID, teamID: tm.ID, operator: op}
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
	return NewEngine(c.cfg, crew, ExecAimem{Command: c.cfg.AimemCommand, Home: c.cfg.Home}, slog.New(slog.NewTextHandler(c.logs, nil)))
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

// A binding that fails leaves the session recorded, so a restarted client
// resumes it instead of trying to enter the team beside it.
func TestFailedBindStillResumes(t *testing.T) {
	c := setupCrew(t)
	ctx := context.Background()
	flag := filepath.Join(c.root, "fail-open")
	if err := os.WriteFile(flag, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	a := c.engine(t)
	if err := a.Start(ctx); err == nil || !strings.Contains(err.Error(), "cannot be reached") {
		t.Fatalf("start with aimem failing: %v", err)
	}
	st, ok, err := LoadState(c.cfg.Home)
	if err != nil || !ok || st.SessionID != a.SessionID() {
		t.Fatalf("no record of the entered session: %+v %v %v", st, ok, err)
	}
	os.Remove(flag)
	b := c.engine(t)
	if err := b.Start(ctx); err != nil {
		t.Fatalf("restart: %v", err)
	}
	if b.SessionID() != a.SessionID() || b.session.Generation != "2" {
		t.Fatalf("restart gave %s at generation %s, want %s resumed", b.SessionID(), b.session.Generation, a.SessionID())
	}
	if !c.handleActive(t, c.fileHandle(t, b.SessionID())) {
		t.Fatal("aimem holds no active handle after the restart")
	}
}

func (c *crewEnv) sessionsOf(t *testing.T) int {
	t.Helper()
	db, err := sql.Open("sqlite", c.storePath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM sessions WHERE agent_id = ?`, c.agentID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// Leaving a recorded session that has already ended enters nothing: it
// closes aimem's binding of that session and clears the record.
func TestLeaveRecordedEndedSession(t *testing.T) {
	c := setupCrew(t)
	ctx := context.Background()
	a := c.engine(t)
	if err := a.Start(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := c.store.StopSession(ctx, c.operator, "stop", a.SessionID()); err != nil {
		t.Fatal(err)
	}
	b := c.engine(t)
	if err := b.LeaveRecorded(ctx); err != nil {
		t.Fatalf("leave: %v", err)
	}
	if n := c.sessionsOf(t); n != 1 {
		t.Fatalf("%d sessions: the leave entered the team", n)
	}
	if _, err := os.Stat(sessionFile(c.root, a.SessionID())); !os.IsNotExist(err) {
		t.Fatal("the original binding was not closed")
	}
	if _, ok, _ := LoadState(c.cfg.Home); ok {
		t.Fatal("the record outlived the ended session")
	}
	for _, call := range fakeCalls(t, c.root) {
		if call.Event == "open" && flagValue(call.Args, "--session") != a.SessionID() {
			t.Fatal("aimem was bound to another session")
		}
	}
}

// Leaving a recorded session that is still active resumes it and leaves.
func TestLeaveRecordedActiveSession(t *testing.T) {
	c := setupCrew(t)
	ctx := context.Background()
	a := c.engine(t)
	if err := a.Start(ctx); err != nil {
		t.Fatal(err)
	}
	if err := c.engine(t).LeaveRecorded(ctx); err != nil {
		t.Fatalf("leave: %v", err)
	}
	if sess, _ := c.store.GetSession(ctx, a.SessionID()); sess.State != store.SessionLeft {
		t.Fatalf("session after leave: %s", sess.State)
	}
	if c.sessionsOf(t) != 1 {
		t.Fatal("the leave entered the team")
	}
	if _, ok, _ := LoadState(c.cfg.Home); ok {
		t.Fatal("the record outlived the session")
	}
}

// seedOpenWork gives the agent a running claimed attempt in its team, which
// makes aicrew refuse its leave with work_outstanding.
func (c *crewEnv) seedOpenWork(t *testing.T) {
	t.Helper()
	db, err := sql.Open("sqlite", c.storePath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	now := time.Now().UTC().Format(time.RFC3339Nano)
	if _, err := db.Exec(`INSERT INTO attempts (id, team_id, task_hub_id, task_project_id, task_id, worker_agent_id, origin,
		coordinator_session_id, coordinator_generation, state, base_commit, branch, process_repository, process_commit,
		process_manifest, instruction_digest, offer_expires_at, task_revision, revision, created_at, updated_at, phase)
		VALUES ('attempt-open', ?, 'hub-test', 'project-a', 'task-1', ?, 'claim', '', 0, 'running', 'base', 'work/task-1',
		'github.com/example/process', 'abc1234', 'process.yaml', 'sha256:x', '', 1, 1, ?, ?, 'working')`,
		c.teamID, c.agentID, now, now); err != nil {
		t.Fatal(err)
	}
}

// A leave refused for open work keeps the session with a working binding:
// aimem holds the current generation's active handle, not the one the
// resume fenced.
func TestRefusedLeaveKeepsWorkingBinding(t *testing.T) {
	c := setupCrew(t)
	ctx := context.Background()
	a := c.engine(t)
	if err := a.Start(ctx); err != nil {
		t.Fatal(err)
	}
	c.seedOpenWork(t)
	b := c.engine(t)
	var kept *WorkOutstanding
	if err := b.LeaveRecorded(ctx); !errors.As(err, &kept) {
		t.Fatalf("leave with open work: %v", err)
	}
	sess, err := c.store.GetSession(ctx, a.SessionID())
	if err != nil || sess.State != store.SessionActive || sess.Generation != 2 {
		t.Fatalf("session after the refused leave: %+v, %v", sess, err)
	}
	got, err := c.store.Introspect(ctx, c.fileHandle(t, a.SessionID()), "hub-test", "aicrew-test")
	if err != nil || !got.Active || got.Generation != sess.Generation {
		t.Fatalf("aimem's handle after the refused leave: %+v, %v", got, err)
	}
	if st, ok, _ := LoadState(c.cfg.Home); !ok || st.SessionID != a.SessionID() {
		t.Fatal("the record of the kept session was lost")
	}
}

// A leave whose token another client's resume ended resumes the session
// itself, and gives aimem the new handle before leaving: when the leave is
// then refused for open work, aimem's binding still works.
func TestInvalidTokenLeaveKeepsWorkingBinding(t *testing.T) {
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
	if b.SessionID() != a.SessionID() {
		t.Fatalf("the second client entered %s beside %s", b.SessionID(), a.SessionID())
	}
	c.seedOpenWork(t)
	var kept *WorkOutstanding
	if err := a.Leave(ctx); !errors.As(err, &kept) {
		t.Fatalf("leave with an ended token and open work: %v", err)
	}
	sess, err := c.store.GetSession(ctx, a.SessionID())
	if err != nil || sess.State != store.SessionActive || sess.Generation != 3 {
		t.Fatalf("session after the refused leave: %+v, %v", sess, err)
	}
	got, err := c.store.Introspect(ctx, c.fileHandle(t, a.SessionID()), "hub-test", "aicrew-test")
	if err != nil || !got.Active || got.Generation != sess.Generation {
		t.Fatalf("aimem's handle after the refused leave: %+v, %v", got, err)
	}
}

// A rebind that fails after the resume does not hold the leave back: with
// no open work the leave completes, and aimem's binding is closed.
func TestInvalidTokenLeaveDespiteFailedRebind(t *testing.T) {
	c := setupCrew(t)
	ctx := context.Background()
	a := c.engine(t)
	if err := a.Start(ctx); err != nil {
		t.Fatal(err)
	}
	if err := c.engine(t).Start(ctx); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(c.root, "fail-refresh"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := a.Leave(ctx); err != nil {
		t.Fatalf("leave after a failed rebind: %v", err)
	}
	if sess, err := c.store.GetSession(ctx, a.SessionID()); err != nil || sess.State != store.SessionLeft {
		t.Fatalf("session after the leave: %+v, %v", sess, err)
	}
	if _, err := os.Stat(sessionFile(c.root, a.SessionID())); !os.IsNotExist(err) {
		t.Fatal("aimem's binding outlived the session")
	}
	if !strings.Contains(c.logs.String(), "not refreshed after the resume") {
		t.Fatal("the failed rebind was not reported")
	}
}

// A close aimem refuses keeps the binding and the record; a retry then
// removes both, and neither attempt enters the team.
func TestLeaveRecordedCloseFailsThenRetries(t *testing.T) {
	c := setupCrew(t)
	ctx := context.Background()
	a := c.engine(t)
	if err := a.Start(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := c.store.StopSession(ctx, c.operator, "stop", a.SessionID()); err != nil {
		t.Fatal(err)
	}
	flag := filepath.Join(c.root, "fail-close")
	if err := os.WriteFile(flag, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := c.engine(t).LeaveRecorded(ctx); err == nil {
		t.Fatal("a refused close was reported as done")
	}
	if _, err := os.Stat(sessionFile(c.root, a.SessionID())); err != nil {
		t.Fatal("the binding went although aimem refused the close")
	}
	if _, ok, _ := LoadState(c.cfg.Home); !ok {
		t.Fatal("the record went although aimem refused the close")
	}
	os.Remove(flag)
	if err := c.engine(t).LeaveRecorded(ctx); err != nil {
		t.Fatalf("retry: %v", err)
	}
	if _, err := os.Stat(sessionFile(c.root, a.SessionID())); !os.IsNotExist(err) {
		t.Fatal("the retry did not close the binding")
	}
	if _, ok, _ := LoadState(c.cfg.Home); ok {
		t.Fatal("the retry did not clear the record")
	}
	if n := c.sessionsOf(t); n != 1 {
		t.Fatalf("%d sessions: a leave entered the team", n)
	}
}

// operatorTokenFile is a new operator credential file, which aicrewd
// requires to start.
func operatorTokenFile(t *testing.T) string {
	t.Helper()
	tok, err := optoken.Generate()
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "operator.token")
	if err := optoken.Write(path, tok); err != nil {
		t.Fatal(err)
	}
	return path
}
