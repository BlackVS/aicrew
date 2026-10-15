package service

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"
)

// RealServiceEnv, set to 1, lets TestRealService register a service of this
// account with the real service manager: CI does, on the runners that
// allow it.
const RealServiceEnv = "AICREW_REAL_SERVICE"

// TestMain lets the test binary stand in for aicrew-agent serve, as the
// real service runs it: it holds the home's serve lock and writes its log
// until it is stopped.
func TestMain(m *testing.M) {
	if len(os.Args) > 1 && os.Args[1] == "serve" {
		os.Exit(fakeServe(os.Args[2:]))
	}
	os.Exit(m.Run())
}

func fakeServe(args []string) int {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	home := fs.String("home", "", "")
	logFile := fs.String("log", "", "")
	if fs.Parse(args) != nil || *home == "" {
		return 2
	}
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, os.Interrupt, syscall.SIGTERM)
	release, err := HoldServe(*home)
	if err != nil {
		return 2
	}
	defer release()
	if *logFile != "" {
		_ = os.MkdirAll(filepath.Dir(*logFile), 0o700)
		_ = os.WriteFile(*logFile, []byte(fmt.Sprintf("serving %d\n", os.Getpid())), 0o600)
	}
	<-sigs
	return 0
}

// The real service manager runs the home's service: install starts it,
// and its serve holds the lock; a second serve is refused; stop ends it;
// start runs it again; uninstall removes it and its definition.
func TestRealService(t *testing.T) {
	if os.Getenv(RealServiceEnv) != "1" {
		t.Skip("set " + RealServiceEnv + "=1 to register a real service of this account")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	reg := Registry{Path: filepath.Join(t.TempDir(), "services.json")}
	b, err := NewBackend(runtime.GOOS, ExecRunner, reg.Dir())
	if errors.Is(err, ErrUnsupported) {
		t.Skip(err)
	}
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS == "linux" {
		if out, err := exec.Command("systemctl", "--user", "show-environment").CombinedOutput(); err != nil {
			t.Skipf("no systemd user manager on this runner: %v %s", err, strings.TrimSpace(string(out)))
		}
	}
	home := newHome(t)
	m := &Manager{Backend: b, Registry: reg}
	spec := NewSpec(home, "realtest", exe, os.Getenv("PATH"))
	t.Cleanup(func() {
		if _, err := m.Uninstall(context.Background(), home, "realtest"); err != nil {
			t.Errorf("cleanup: %v", err)
		}
	})
	t.Logf("service %s (%s)", spec.Name, b.Kind())

	running := func(want bool) Status {
		t.Helper()
		deadline := time.Now().Add(60 * time.Second)
		for {
			st, err := m.Status(ctx, home, "realtest")
			if err != nil {
				t.Fatal(err)
			}
			if st.Active == want && (st.ServePID != 0) == want && !st.HandServe {
				return st
			}
			if time.Now().After(deadline) {
				log, _ := os.ReadFile(LogPath(home))
				t.Fatalf("waiting for running=%v: %+v; log %q", want, st, log)
			}
			time.Sleep(250 * time.Millisecond)
		}
	}

	st, err := m.Install(ctx, spec)
	if errors.Is(err, ErrNoLoginSession) || (err == nil && len(st.Notes) > 0 && strings.Contains(st.Notes[0], "no login session")) {
		t.Skip("no login session for launchd on this runner")
	}
	if err != nil {
		t.Fatal(err)
	}
	st = running(true)
	if b, err := os.ReadFile(LogPath(home)); err != nil || !strings.HasPrefix(string(b), "serving ") {
		t.Fatalf("the serve's log: %v %q", err, b)
	}
	var se *ServingError
	if _, err := HoldServe(home); !errors.As(err, &se) || se.PID != st.ServePID {
		t.Fatalf("a second serve beside the service's (pid %d): %v", st.ServePID, err)
	}
	if _, err := m.Stop(ctx, home, "realtest"); err != nil {
		t.Fatal(err)
	}
	running(false)
	if _, err := m.Start(ctx, spec); err != nil {
		t.Fatal(err)
	}
	running(true)
	st, err = m.Uninstall(ctx, home, "realtest")
	if err != nil || st.Installed || st.Active || st.ServePID != 0 {
		t.Fatalf("uninstalled: %+v %v", st, err)
	}
}
