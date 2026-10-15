package service

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// newHome is an agent home with its state directory.
func newHome(t *testing.T) string {
	t.Helper()
	home := filepath.Join(t.TempDir(), "home")
	if err := os.MkdirAll(filepath.Join(home, "state"), 0o700); err != nil {
		t.Fatal(err)
	}
	return home
}

// The serve lock: held, it reports the holder's PID and refuses a second
// serve; released, the home runs none.
func TestServeLock(t *testing.T) {
	home := newHome(t)
	if _, serving, err := Serving(home); err != nil || serving {
		t.Fatalf("a fresh home: serving %v, %v", serving, err)
	}
	if _, serving, err := Serving(filepath.Join(t.TempDir(), "none")); err != nil || serving {
		t.Fatalf("a missing home: serving %v, %v", serving, err)
	}
	release, err := HoldServe(home)
	if err != nil {
		t.Fatal(err)
	}
	pid, serving, err := Serving(home)
	if err != nil || !serving || pid != os.Getpid() {
		t.Fatalf("held: pid %d serving %v %v", pid, serving, err)
	}
	var se *ServingError
	if _, err := HoldServe(home); !errors.As(err, &se) || se.PID != os.Getpid() {
		t.Fatalf("a second serve: %v", err)
	}
	if !strings.Contains(se.Error(), "another aicrew-agent serve is running") {
		t.Fatal(se.Error())
	}
	release()
	if _, serving, _ := Serving(home); serving {
		t.Fatal("still serving after the release")
	}
	if _, err := os.Stat(pidPath(home)); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("the PID file outlived the release")
	}
}

// The registry keeps one entry per home, replaced by a later Put.
func TestRegistry(t *testing.T) {
	r := Registry{Path: filepath.Join(t.TempDir(), "aicrew", "services.json")}
	if all, err := r.Entries(); err != nil || len(all) != 0 {
		t.Fatalf("an empty registry: %v %v", all, err)
	}
	for _, e := range []Entry{{Home: "/b", Label: "b", Name: "n-b"}, {Home: "/a", Label: "a", Name: "n-a"},
		{Home: "/b/", Label: "b2", Name: "n-b2"}} {
		if err := r.Put(e); err != nil {
			t.Fatal(err)
		}
	}
	all, _ := r.Entries()
	if len(all) != 2 || all[0].Home != "/a" || all[1].Name != "n-b2" {
		t.Fatalf("entries: %+v", all)
	}
	if e, ok, _ := r.Lookup("/b"); !ok || e.Label != "b2" {
		t.Fatalf("lookup: %+v %v", e, ok)
	}
	if err := r.Remove("/a"); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := r.Lookup("/a"); ok {
		t.Fatal("a removed home is still listed")
	}
	if err := os.WriteFile(r.Path, []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Entries(); err == nil {
		t.Fatal("a broken registry was read")
	}
}

// fakeBackend is a service manager in memory. Its "service" holds the
// serve lock while it runs, as the real serve does.
type fakeBackend struct {
	mu        sync.Mutex
	home      string
	orphans   bool
	installed map[string]Spec
	running   map[string]func() // the service's serve hold
	calls     []string
	stopKeeps bool // Stop leaves the serve running (as a task's End does)
}

func newFake(home string) *fakeBackend {
	return &fakeBackend{home: home, installed: map[string]Spec{}, running: map[string]func(){}}
}

func (f *fakeBackend) Kind() string { return "fake" }
func (f *fakeBackend) log(s string) { f.mu.Lock(); f.calls = append(f.calls, s); f.mu.Unlock() }
func (f *fakeBackend) Install(_ context.Context, s Spec) error {
	f.log("install " + s.Name)
	f.installed[s.Name] = s
	return nil
}
func (f *fakeBackend) Start(_ context.Context, name string) error {
	f.log("start " + name)
	if _, ok := f.running[name]; ok {
		return nil
	}
	release, err := HoldServe(f.home)
	if err != nil {
		return err
	}
	f.running[name] = release
	return nil
}
func (f *fakeBackend) Stop(_ context.Context, name string) error {
	f.log("stop " + name)
	if !f.stopKeeps {
		if r := f.running[name]; r != nil {
			r()
		}
	}
	delete(f.running, name)
	return nil
}
func (f *fakeBackend) Remove(ctx context.Context, name string) error {
	f.log("remove " + name)
	delete(f.installed, name)
	return nil
}
func (f *fakeBackend) State(_ context.Context, name string) (bool, bool, error) {
	_, inst := f.installed[name]
	_, run := f.running[name]
	return inst, run, nil
}
func (f *fakeBackend) Notes(context.Context) []string { return []string{"a note"} }
func (f *fakeBackend) OrphansServe() bool             { return f.orphans }

func newManager(t *testing.T, home string) (*Manager, *fakeBackend) {
	f := newFake(home)
	return &Manager{Backend: f, Registry: Registry{Path: filepath.Join(t.TempDir(), "services.json")},
		StopWait: 2 * time.Second, poll: 10 * time.Millisecond}, f
}

// Install registers and starts the service; Stop ends its serve; Start
// starts it again from a refreshed definition; Uninstall removes it.
func TestManagerLifecycle(t *testing.T) {
	ctx := context.Background()
	home := newHome(t)
	m, f := newManager(t, home)
	spec := NewSpec(home, "sol", "/bin/aicrew-agent", "/usr/bin")
	if _, err := m.Start(ctx, spec); !errors.Is(err, ErrNotInstalled) {
		t.Fatalf("starting an uninstalled service: %v", err)
	}
	st, err := m.Install(ctx, spec)
	if err != nil {
		t.Fatal(err)
	}
	if !st.Installed || !st.Active || st.ServePID != os.Getpid() || st.HandServe || st.State() != "running, pid "+strconv.Itoa(os.Getpid()) {
		t.Fatalf("installed: %+v %q", st, st.State())
	}
	if len(st.Notes) != 1 {
		t.Fatalf("notes: %v", st.Notes)
	}
	if e, ok, _ := m.Registry.Lookup(home); !ok || e.Name != spec.Name || e.Label != "sol" {
		t.Fatalf("registry: %+v %v", e, ok)
	}
	if st, err = m.Stop(ctx, home, "sol"); err != nil || st.Active || st.State() != "stopped" {
		t.Fatalf("stopped: %+v %v", st, err)
	}
	if _, serving, _ := Serving(home); serving {
		t.Fatal("the serve outlived the stop")
	}
	spec.Exe = "/new/aicrew-agent"
	if st, err = m.Start(ctx, spec); err != nil || !st.Active {
		t.Fatalf("started: %+v %v", st, err)
	}
	if f.installed[spec.Name].Exe != "/new/aicrew-agent" {
		t.Fatal("Start did not refresh the definition")
	}
	if st, err = m.Uninstall(ctx, home, "sol"); err != nil || st.Installed || st.Active {
		t.Fatalf("uninstalled: %+v %v", st, err)
	}
	if _, ok, _ := m.Registry.Lookup(home); ok {
		t.Fatal("the registry still lists an uninstalled home")
	}
	want := "install X|start X|stop X|install X|start X|stop X|remove X"
	if got := strings.ReplaceAll(strings.Join(f.calls, "|"), spec.Name, "X"); got != want {
		t.Fatalf("calls %s\nwant  %s", got, want)
	}
}

// A serve started by hand is reported and nothing is installed or started
// beside it; stopping leaves it running.
func TestManagerRefusesBesideAServeStartedByHand(t *testing.T) {
	ctx := context.Background()
	home := newHome(t)
	m, f := newManager(t, home)
	release, err := HoldServe(home)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	st, err := m.Install(ctx, NewSpec(home, "sol", "/bin/aicrew-agent", ""))
	var hs *HandStartedError
	if !errors.As(err, &hs) || hs.PID != os.Getpid() {
		t.Fatalf("install beside a hand-started serve: %v", err)
	}
	if !strings.Contains(err.Error(), "started by hand") || !strings.Contains(err.Error(), "aicrew-agent service start --home "+home) {
		t.Fatal(err)
	}
	if !st.HandServe || st.State() != "started by hand, pid "+strconv.Itoa(os.Getpid()) {
		t.Fatalf("status %+v", st)
	}
	if len(f.calls) != 0 {
		t.Fatalf("the manager acted: %v", f.calls)
	}
	if _, ok, _ := m.Registry.Lookup(home); ok {
		t.Fatal("registered beside a hand-started serve")
	}
	if st, err := m.Stop(ctx, home, "sol"); err != nil || !st.HandServe {
		t.Fatalf("stop: %+v %v", st, err)
	}
	if _, serving, _ := Serving(home); !serving {
		t.Fatal("stop ended a serve started by hand")
	}
}

// Where stopping the service leaves its serve running (a Windows task's
// conhost ends, its child does not), the serve's own PID is stopped, and a
// serve that never ends is reported.
func TestManagerStopsAnOrphanedServe(t *testing.T) {
	ctx := context.Background()
	home := newHome(t)
	m, f := newManager(t, home)
	f.orphans, f.stopKeeps = true, true
	spec := NewSpec(home, "sol", "/bin/aicrew-agent", "")
	if _, err := m.Install(ctx, spec); err != nil {
		t.Fatal(err)
	}
	hold := f.running[spec.Name]
	var killed int
	m.Kill = func(pid int) error { killed = pid; hold(); return nil }
	if _, err := m.Stop(ctx, home, "sol"); err != nil {
		t.Fatal(err)
	}
	if killed != os.Getpid() {
		t.Fatalf("killed pid %d, want the serve's %d", killed, os.Getpid())
	}
	if _, err := m.Start(ctx, spec); err != nil {
		t.Fatal(err)
	}
	hold = f.running[spec.Name]
	defer hold()
	m.Kill = func(int) error { return nil }
	m.StopWait = 100 * time.Millisecond
	if _, err := m.Stop(ctx, home, "sol"); err == nil || !strings.Contains(err.Error(), "still runs") {
		t.Fatalf("a serve that never ends: %v", err)
	}
}

// A changed label renames the service: the old one goes first.
func TestManagerRenamesOnALabelChange(t *testing.T) {
	ctx := context.Background()
	home := newHome(t)
	m, f := newManager(t, home)
	old := NewSpec(home, "sol", "/bin/aicrew-agent", "")
	if _, err := m.Install(ctx, old); err != nil {
		t.Fatal(err)
	}
	renamed := NewSpec(home, "opus", "/bin/aicrew-agent", "")
	st, err := m.Install(ctx, renamed)
	if err != nil || !st.Active || st.Name != renamed.Name {
		t.Fatalf("renamed: %+v %v", st, err)
	}
	if _, ok := f.installed[old.Name]; ok {
		t.Fatal("the old service is still installed")
	}
	if e, _, _ := m.Registry.Lookup(home); e.Name != renamed.Name {
		t.Fatalf("registry: %+v", e)
	}
	_, _ = m.Stop(ctx, home, "opus")
}
