package main

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/BlackVS/aicrew/internal/agent"
	"github.com/BlackVS/aicrew/internal/service"
)

// fakeServiceManager is a service manager in memory whose service holds
// the home's serve lock while it runs.
type fakeServiceManager struct {
	installed map[string]service.Spec
	running   map[string]func()
}

func (f *fakeServiceManager) Kind() string { return "fake" }
func (f *fakeServiceManager) Install(_ context.Context, s service.Spec) error {
	f.installed[s.Name] = s
	return nil
}
func (f *fakeServiceManager) Start(_ context.Context, name string) error {
	if _, ok := f.running[name]; ok {
		return nil
	}
	release, err := service.HoldServe(f.installed[name].Home)
	if err != nil {
		return err
	}
	f.running[name] = release
	return nil
}
func (f *fakeServiceManager) Stop(_ context.Context, name string) error {
	if r := f.running[name]; r != nil {
		r()
	}
	delete(f.running, name)
	return nil
}
func (f *fakeServiceManager) Remove(ctx context.Context, name string) error {
	delete(f.installed, name)
	return nil
}
func (f *fakeServiceManager) State(_ context.Context, name string) (bool, bool, error) {
	_, i := f.installed[name]
	_, r := f.running[name]
	return i, r, nil
}
func (f *fakeServiceManager) Notes(context.Context) []string { return nil }
func (f *fakeServiceManager) OrphansServe() bool             { return false }

// useFakeServices puts the commands on a fake manager and registry.
func useFakeServices(t *testing.T) *fakeServiceManager {
	t.Helper()
	f := &fakeServiceManager{installed: map[string]service.Spec{}, running: map[string]func(){}}
	reg := service.Registry{Path: filepath.Join(t.TempDir(), "services.json")}
	oldM, oldX := newServiceManager, serviceExecutable
	newServiceManager = func(func(string) string) (*service.Manager, error) {
		return &service.Manager{Backend: f, Registry: reg}, nil
	}
	serviceExecutable = func() (string, error) { return "/opt/bin/aicrew-agent", nil }
	t.Cleanup(func() {
		for _, r := range f.running {
			r()
		}
		newServiceManager, serviceExecutable = oldM, oldX
	})
	return f
}

// turnsHome is a linked agent home in the given wake mode.
func turnsHome(t *testing.T, label, mode string) string {
	t.Helper()
	home := filepath.Join(t.TempDir(), label)
	if err := os.MkdirAll(filepath.Join(home, "state"), 0o700); err != nil {
		t.Fatal(err)
	}
	cfg := `{"label": "` + label + `", "wake": {"mode": "` + mode + `"}, "aicrew": {"url": "https://aicrew.example",
		"tls_trust_mode": "ca_dns", "tls_trust_value": "aicrew.example", "agent_id": "agent-1", "team_id": "team-1"}}`
	if err := os.WriteFile(filepath.Join(home, "agent.json"), []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	return home
}

func pathEnv(k string) string {
	if k == "PATH" {
		return "/usr/bin:/opt/bin"
	}
	return ""
}

// The service command installs, reports, stops and starts a turns home's
// service, refuses a stop-hook home, and acts on every registered home with
// --all.
func TestServiceCommand(t *testing.T) {
	ctx := context.Background()
	f := useFakeServices(t)
	home := turnsHome(t, "sol", agent.WakeTurns)
	name := service.Name(home, "sol")
	run := func(args ...string) (int, string, string) {
		var out, errb bytes.Buffer
		code := serviceCmd(ctx, args, &out, &errb, pathEnv)
		return code, out.String(), errb.String()
	}
	for _, args := range [][]string{{}, {"bogus", "--home", home}, {"status"}, {"status", "--home", home, "--all"},
		{"install", "--all"}, {"status", "--home", home, "extra"}} {
		if code, _, errb := run(args...); code != exitUsage || !strings.Contains(errb, "usage: aicrew-agent service") {
			t.Errorf("%q: exit %d %s", args, code, errb)
		}
	}
	if code, out, _ := run("status", "--home", home); code != exitOK || out != name+": not installed (fake, "+home+")\n" {
		t.Fatalf("status before install: %d %q", code, out)
	}
	code, out, errb := run("install", "--home", home)
	if code != exitOK || !strings.HasPrefix(out, name+": running, pid ") {
		t.Fatalf("install: %d %q %s", code, out, errb)
	}
	s := f.installed[name]
	if s.Exe != "/opt/bin/aicrew-agent" || s.Path != "/usr/bin:/opt/bin" || s.Label != "sol" ||
		strings.Join(s.Args, " ") != "serve --home "+home+" --log "+service.LogPath(home) {
		t.Fatalf("spec %+v", s)
	}
	if code, out, _ := run("stop", "--home", home); code != exitOK || out != name+": stopped (fake, "+home+")\n" {
		t.Fatalf("stop: %d %q", code, out)
	}
	other := turnsHome(t, "opus", agent.WakeTurns)
	if code, _, errb := run("install", "--home", other); code != exitOK {
		t.Fatal(errb)
	}
	code, out, _ = run("restart", "--all", "--json")
	var sts []service.Status
	if code != exitOK || json.Unmarshal([]byte(out), &sts) != nil || len(sts) != 2 || !sts[0].Active || !sts[1].Active {
		t.Fatalf("restart --all: %d %s", code, out)
	}
	if code, out, _ := run("stop", "--all"); code != exitOK || strings.Count(out, ": stopped (") != 2 {
		t.Fatalf("stop --all: %d %q", code, out)
	}
	if code, out, _ := run("uninstall", "--home", other); code != exitOK || !strings.Contains(out, ": not installed (") {
		t.Fatalf("uninstall: %d %q", code, out)
	}
	if code, out, _ := run("status", "--all"); code != exitOK || strings.Count(out, "\n") != 1 || !strings.HasPrefix(out, name) {
		t.Fatalf("status --all after an uninstall: %d %q", code, out)
	}

	hook := turnsHome(t, "hook", agent.WakeStopHook)
	if code, _, errb := run("install", "--home", hook); code != exitFailed || !strings.Contains(errb, "stop-hook mode") ||
		!strings.Contains(errb, "--wake turns") {
		t.Fatalf("a stop-hook home: %d %s", code, errb)
	}
	if code, _, errb := run("install", "--home", filepath.Join(t.TempDir(), "none")); code != exitFailed ||
		!strings.Contains(errb, "not a usable agent home") {
		t.Fatalf("not a home: %d %s", code, errb)
	}
}

// A serve started by hand is reported by install, which leaves it alone.
func TestServiceCommandReportsAServeStartedByHand(t *testing.T) {
	f := useFakeServices(t)
	home := turnsHome(t, "sol", agent.WakeTurns)
	release, err := service.HoldServe(home)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	var out, errb bytes.Buffer
	code := serviceCmd(context.Background(), []string{"install", "--home", home}, &out, &errb, pathEnv)
	if code != exitFailed || !strings.Contains(out.String(), ": started by hand, pid ") ||
		!strings.Contains(errb.String(), "aicrew-agent service start --home "+home) || len(f.installed) != 0 {
		t.Fatalf("exit %d\n%s\n%s", code, out.String(), errb.String())
	}
}

// serve holds the home's serve lock: a second serve of a turns home is
// refused with exit 2, which a service does not restart into, and --log
// writes the log to the file as well.
func TestServeRefusesASecondServe(t *testing.T) {
	home := turnsHome(t, "sol", agent.WakeTurns)
	release, err := service.HoldServe(home)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	build := func(cfg agent.Config, log *slog.Logger) (*agent.Engine, error) {
		return &agent.Engine{Cfg: cfg, Log: log}, nil
	}
	logFile := filepath.Join(home, "logs", "serve.log")
	var errb bytes.Buffer
	cfg, _ := os.ReadFile(filepath.Join(home, "agent.json"))
	os.WriteFile(filepath.Join(home, "agent.json"), bytes.Replace(cfg, []byte(`"team_id": "team-1"`),
		[]byte(`"team_id": "team-1", "client_command": "/bin/true"`), 1), 0o600)
	code := serveHome(context.Background(), []string{"-home", home, "-log", logFile}, &errb, nil, build, noEnv)
	if code != exitUsage || !strings.Contains(errb.String(), "another aicrew-agent serve is running for ") ||
		!strings.Contains(errb.String(), "(pid "+strconv.Itoa(os.Getpid())+")") {
		t.Fatalf("exit %d, %s", code, errb.String())
	}
	b, err := os.ReadFile(logFile)
	if err != nil || !strings.Contains(string(b), "another aicrew-agent serve is running") {
		t.Fatalf("the log file: %v %q", err, b)
	}
}

// join's report names the service it installed, or the command that would.
func TestJoinReportNamesTheService(t *testing.T) {
	rep := agent.JoinReport{Status: agent.JoinReady, Home: "/h/sol", Next: agent.ServiceCommand("/h/sol")}
	var b bytes.Buffer
	printJoinReport(&b, joinOutput{JoinReport: rep, Service: &service.Status{Name: "aicrew-member-sol-1", Home: "/h/sol",
		Kind: "systemd", Installed: true, Active: true, ServePID: 42, Notes: []string{"lingering is off"}}})
	for _, w := range []string{"service aicrew-member-sol-1: running, pid 42 (systemd)\n", "  note: lingering is off\n",
		"the member runs as the service aicrew-member-sol-1; see it with: aicrew-agent service status --home /h/sol\n"} {
		if !strings.Contains(b.String(), w) {
			t.Errorf("lacks %q:\n%s", w, b.String())
		}
	}
	if strings.Contains(b.String(), "start the member with") {
		t.Errorf("a running service still asks to start the member:\n%s", b.String())
	}
	b.Reset()
	printJoinReport(&b, joinOutput{JoinReport: rep, ServiceError: "no supported service manager"})
	if !strings.Contains(b.String(), "service not started: no supported service manager\n") ||
		!strings.Contains(b.String(), "start the member with: aicrew-agent service install --home /h/sol\n") {
		t.Errorf("a failed install:\n%s", b.String())
	}
}

// check reports the service of a turns home, with what to do about it, and
// says a stop-hook home uses none.
func TestCheckReportsTheService(t *testing.T) {
	ctx := context.Background()
	useFakeServices(t)
	home := turnsHome(t, "sol", agent.WakeTurns)
	name := service.Name(home, "sol")
	line := func() string {
		var b bytes.Buffer
		printServiceCheck(&b, checkService(ctx, home))
		return b.String()
	}
	if got, want := line(), "service: "+name+": not installed (fake)\nto do: "+agent.ServiceCommand(home)+"\n"; got != want {
		t.Fatalf("not installed:\n%q\nwant\n%q", got, want)
	}
	var out, errb bytes.Buffer
	if code := serviceCmd(ctx, []string{"install", "--home", home}, &out, &errb, pathEnv); code != exitOK {
		t.Fatal(errb.String())
	}
	if got := line(); !strings.HasPrefix(got, "service: "+name+": running, pid ") || strings.Contains(got, "to do") {
		t.Fatalf("running: %q", got)
	}
	serviceCmd(ctx, []string{"stop", "--home", home}, &out, &errb, pathEnv)
	if got := line(); got != "service: "+name+": stopped (fake)\nto do: aicrew-agent service start --home "+home+"\n" {
		t.Fatalf("stopped: %q", got)
	}
	release, err := service.HoldServe(home)
	if err != nil {
		t.Fatal(err)
	}
	got := line()
	release()
	if !strings.Contains(got, ": started by hand, pid ") || !strings.Contains(got, "to do: stop the serve started by hand") {
		t.Fatalf("started by hand: %q", got)
	}
	hook := turnsHome(t, "hook", agent.WakeStopHook)
	c := checkService(ctx, hook)
	var b bytes.Buffer
	printServiceCheck(&b, c)
	if c.State != "not_used" || !strings.HasPrefix(b.String(), "service: not used (a stop-hook home: the member runs aicrew-agent run") {
		t.Fatalf("a stop-hook home: %q", b.String())
	}
}

// AICREW_NO_SERVICE=1 gives join no service to install.
func TestNoServiceEnv(t *testing.T) {
	if realInstallService(func(k string) string {
		if k == service.NoServiceEnv {
			return "1"
		}
		return ""
	}) != nil {
		t.Fatal("AICREW_NO_SERVICE=1 still installs a service")
	}
	if realInstallService(noEnv) == nil {
		t.Fatal("no service without AICREW_NO_SERVICE")
	}
}
