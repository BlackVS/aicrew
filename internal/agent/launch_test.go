package agent

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/BlackVS/aicrew/internal/store"
)

func selfPath(t *testing.T) string {
	t.Helper()
	p, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func readReport(t *testing.T, dir string) probeReport {
	t.Helper()
	var r probeReport
	waitFor(t, "the probe client's report", func() bool {
		b, err := os.ReadFile(filepath.Join(dir, "report.json"))
		return err == nil && json.Unmarshal(b, &r) == nil
	})
	return r
}

// noSecretIn fails if a token, handle or receipt appears in the client's
// environment or arguments: all three have fixed prefixes.
func noSecretIn(t *testing.T, r probeReport) {
	t.Helper()
	all := strings.Join(r.Env, "\n") + "\n" + strings.Join(r.Args, " ")
	for _, prefix := range []string{"ast1_", "acs1_", "amr1_"} {
		if strings.Contains(all, prefix) {
			t.Fatalf("a %s secret reached the client's environment or arguments", prefix)
		}
	}
}

func probeEnv(t *testing.T, exit string) {
	t.Helper()
	t.Setenv("AICREW_PROBE", "1")
	t.Setenv("AICREW_PROBE_EXIT", exit)
	// An inherited value is replaced, never kept.
	t.Setenv(SessionEnv, "inherited-from-the-user-shell")
}

// The client, and the process it starts the way a client starts its MCP
// server, see exactly this session's file, and nothing secret; when the
// client exits, the launcher leaves and closes aimem's binding.
func TestRunClientScopesEnvAndLeaves(t *testing.T) {
	c := setupCrew(t)
	probeEnv(t, "0")
	dir := t.TempDir()
	e := c.engine(t)
	code, err := RunClient(context.Background(), e, Client{Path: selfPath(t), Args: []string{"probe-client", dir}},
		Stdio{Out: io.Discard, Err: io.Discard}, nil)
	if err != nil || code != 0 {
		t.Fatalf("run: code %d, %v", code, err)
	}
	r := readReport(t, dir)
	if !r.HasVar || r.Value != e.AimemFile() || r.Value != sessionFile(c.root, e.SessionID()) {
		t.Fatalf("the client saw %s=%q (present %v), want the session file", SessionEnv, r.Value, r.HasVar)
	}
	if r.Grandchild != "present:"+e.AimemFile() {
		t.Fatalf("the client's own child saw %q", r.Grandchild)
	}
	n := 0
	for _, kv := range r.Env {
		if strings.HasPrefix(strings.ToUpper(kv), SessionEnv+"=") {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("%d %s entries in the client's environment", n, SessionEnv)
	}
	noSecretIn(t, r)
	wd, werr := os.Stat(r.Dir)
	home, herr := os.Stat(c.cfg.Home)
	if werr != nil || herr != nil || !os.SameFile(wd, home) {
		t.Fatalf("the client ran in %q, not the agent home", r.Dir)
	}
	if os.Getenv(SessionEnv) != "inherited-from-the-user-shell" {
		t.Fatal("the launcher changed its own environment")
	}
	if sess, _ := c.store.GetSession(context.Background(), e.SessionID()); sess.State != store.SessionLeft {
		t.Fatalf("session after the client exited: %s", sess.State)
	}
	if _, err := os.Stat(sessionFile(c.root, e.SessionID())); !os.IsNotExist(err) {
		t.Fatal("aimem's binding outlived the session")
	}
}

// The client's exit code comes back; open work keeps the session, and the
// launcher reports it rather than forcing a leave.
func TestRunClientExitCodeAndWorkKept(t *testing.T) {
	c := setupCrew(t)
	probeEnv(t, "7")
	c.seedOpenWork(t)
	e := c.engine(t)
	code, err := RunClient(context.Background(), e, Client{Path: selfPath(t), Args: []string{"probe-client", t.TempDir()}},
		Stdio{Out: io.Discard, Err: io.Discard}, nil)
	var kept *WorkOutstanding
	if code != 7 || !errors.As(err, &kept) {
		t.Fatalf("run: code %d, %v", code, err)
	}
	if sess, _ := c.store.GetSession(context.Background(), e.SessionID()); sess.State != store.SessionActive {
		t.Fatalf("session with open work: %s", sess.State)
	}
	if !c.handleActive(t, c.fileHandle(t, e.SessionID())) {
		t.Fatal("the kept session's binding does not work")
	}
}

// A client ended by a signal reports 128 plus the signal, as a shell does.
func TestExitCodeSignaled(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("no signals on Windows")
	}
	err := exec.Command("sh", "-c", "kill -TERM $$").Run()
	if got := exitCode(err); got != 128+int(syscall.SIGTERM) {
		t.Fatalf("exit code %d, want %d (%v)", got, 128+int(syscall.SIGTERM), err)
	}
}

// SIGTERM sent to the launcher reaches the client; an interrupt does not
// end the launcher, which waits for the client.
func TestRunClientSignals(t *testing.T) {
	c := setupCrew(t)
	probeEnv(t, "")
	dir := t.TempDir()
	e := c.engine(t)
	sigs := make(chan os.Signal, 2)
	type result struct {
		code int
		err  error
	}
	done := make(chan result, 1)
	go func() {
		code, err := RunClient(context.Background(), e, Client{Path: selfPath(t), Args: []string{"probe-client", dir}},
			Stdio{Out: io.Discard, Err: io.Discard}, sigs)
		done <- result{code, err}
	}()
	readReport(t, dir)
	sigs <- os.Interrupt
	select {
	case r := <-done:
		t.Fatalf("an interrupt ended the launcher: %+v", r)
	case <-time.After(300 * time.Millisecond):
	}
	if runtime.GOOS == "windows" {
		// No SIGTERM on Windows; the client ends itself.
		os.WriteFile(filepath.Join(dir, "stop"), nil, 0o600)
	} else {
		sigs <- syscall.SIGTERM
	}
	select {
	case r := <-done:
		if r.code != 0 || r.err != nil {
			t.Fatalf("run: %+v", r)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("the client did not stop")
	}
	if sess, _ := c.store.GetSession(context.Background(), e.SessionID()); sess.State != store.SessionLeft {
		t.Fatalf("session after the client stopped: %s", sess.State)
	}
}

func heartbeat(dir string) string {
	b, _ := os.ReadFile(filepath.Join(dir, "heartbeat"))
	return string(b)
}

// A launcher killed outright: on Linux and Windows the client is stopped
// with it. On macOS nothing can stop it, but its binding is no longer
// refreshed, so it loses team access within the handle's 15 minutes, and
// the next run resumes the session under a new generation, fencing the
// old handle at once. On every platform the session is kept, not left.
func TestKilledLauncher(t *testing.T) {
	c := setupCrew(t)
	probeEnv(t, "")
	self := selfPath(t)
	cfg, _ := json.Marshal(map[string]any{"aicrew": map[string]string{
		"url": c.cfg.URL, "tls_trust_mode": c.cfg.Trust.Mode, "tls_trust_value": c.cfg.Trust.Value,
		"agent_id": c.agentID, "team_id": c.teamID, "aimem_command": self}})
	if err := os.WriteFile(filepath.Join(c.cfg.Home, "agent.json"), cfg, 0o600); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	launcher := exec.Command(self, "probe-launcher", c.cfg.Home, dir)
	if err := launcher.Start(); err != nil {
		t.Fatal(err)
	}
	readReport(t, dir)
	waitFor(t, "a heartbeat", func() bool { return heartbeat(dir) != "" })
	st, ok, _ := LoadState(c.cfg.Home)
	if !ok {
		t.Fatal("no session record")
	}
	oldHandle := c.fileHandle(t, st.SessionID)
	if err := launcher.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	launcher.Wait()

	if runtime.GOOS == "linux" || runtime.GOOS == "windows" {
		stopped := func() bool {
			a := heartbeat(dir)
			time.Sleep(300 * time.Millisecond)
			return heartbeat(dir) == a
		}
		waitFor(t, "the client to stop with its launcher", stopped)
	} else {
		a := heartbeat(dir)
		time.Sleep(300 * time.Millisecond)
		if heartbeat(dir) == a {
			t.Fatal("the client stopped; on this platform the handle's expiry is the documented guarantee")
		}
		t.Cleanup(func() { os.WriteFile(filepath.Join(dir, "stop"), nil, 0o600) })
	}
	ctx := context.Background()
	sess, err := c.store.GetSession(ctx, st.SessionID)
	if err != nil || sess.State != store.SessionActive {
		t.Fatalf("the session after the launcher died: %+v, %v", sess, err)
	}
	next := c.engine(t)
	if err := next.Start(ctx); err != nil {
		t.Fatal(err)
	}
	if next.SessionID() != st.SessionID || next.session.Generation != "2" {
		t.Fatalf("the next run gave %s at generation %s", next.SessionID(), next.session.Generation)
	}
	if c.handleActive(t, oldHandle) {
		t.Fatal("the dead launcher's handle still works after the resume")
	}
	if err := next.Leave(ctx); err != nil {
		t.Fatal(err)
	}
}

func TestScopedEnv(t *testing.T) {
	env := []string{"PATH=/bin", SessionEnv + "=old", "HOME=/h"}
	if runtime.GOOS == "windows" {
		env = append(env, "aimem_team_session=lower")
	}
	got := ScopedEnv(env, "/new")
	n := 0
	for _, kv := range got {
		if strings.HasPrefix(strings.ToUpper(kv), SessionEnv+"=") {
			n++
			if kv != SessionEnv+"=/new" {
				t.Fatalf("kept %q", kv)
			}
		}
	}
	if n != 1 || len(env) == 0 || env[1] != SessionEnv+"=old" {
		t.Fatalf("scoped env %v (input changed: %v)", got, env)
	}
}
