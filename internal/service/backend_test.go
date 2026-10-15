package service

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// script is a fake service manager command line: each call is recorded,
// and answered by the first rule whose prefix it starts with.
type script struct {
	calls []string
	rules []rule
}

type rule struct {
	prefix string
	out    string
	fail   bool
}

func (s *script) run(_ context.Context, name string, args ...string) (string, error) {
	line := strings.Join(append([]string{name}, args...), " ")
	s.calls = append(s.calls, line)
	for _, r := range s.rules {
		if strings.HasPrefix(line, r.prefix) {
			if r.fail {
				return r.out, errors.New("exit status 1")
			}
			return r.out, nil
		}
	}
	return "", nil
}

func (s *script) joined() string { return strings.Join(s.calls, "\n") }

var spec = Spec{Name: "aicrew-member-sol-0a1b2c3d", Label: "sol", Home: "/h/sol", Exe: "/bin/aicrew-agent",
	Args: []string{"serve", "--home", "/h/sol"}}

// systemd: the unit is written, reloaded and enabled; start resets a unit
// that gave up; is-active's word is the state; removal disables and
// deletes it.
func TestSystemdBackend(t *testing.T) {
	ctx := context.Background()
	s := &script{rules: []rule{{prefix: "systemctl --user is-active", out: "active\n"}}}
	b := &Systemd{Run: s.run, Dir: t.TempDir()}
	if inst, _, _ := b.State(ctx, spec.Name); inst {
		t.Fatal("installed before Install")
	}
	if err := b.Install(ctx, spec); err != nil {
		t.Fatal(err)
	}
	unit, err := os.ReadFile(filepath.Join(b.Dir, spec.Name+".service"))
	if err != nil || string(unit) != SystemdUnit(spec) {
		t.Fatalf("the unit file: %v\n%s", err, unit)
	}
	if err := b.Start(ctx, spec.Name); err != nil {
		t.Fatal(err)
	}
	if inst, act, _ := b.State(ctx, spec.Name); !inst || !act {
		t.Fatalf("state %v %v", inst, act)
	}
	if err := b.Stop(ctx, spec.Name); err != nil {
		t.Fatal(err)
	}
	if err := b.Remove(ctx, spec.Name); err != nil {
		t.Fatal(err)
	}
	u := spec.Name + ".service"
	want := strings.Join([]string{
		"systemctl --user daemon-reload", "systemctl --user enable " + u,
		"systemctl --user reset-failed " + u, "systemctl --user start " + u,
		"systemctl --user is-active " + u,
		"systemctl --user stop " + u,
		"systemctl --user disable --now " + u, "systemctl --user daemon-reload",
	}, "\n")
	if s.joined() != want {
		t.Fatalf("calls:\n%s\nwant:\n%s", s.joined(), want)
	}
	if _, err := os.Stat(filepath.Join(b.Dir, u)); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("the unit outlived its removal")
	}
	for out, active := range map[string]bool{"inactive\n": false, "failed\n": false, "activating\n": true} {
		s := &script{rules: []rule{{prefix: "systemctl --user is-active", out: out, fail: !active}}}
		b := &Systemd{Run: s.run, Dir: t.TempDir()}
		_ = b.Install(ctx, spec)
		if _, act, _ := b.State(ctx, spec.Name); act != active {
			t.Errorf("is-active %q: active %v", out, act)
		}
	}
	// A failed command is reported with its output.
	s = &script{rules: []rule{{prefix: "systemctl --user start", out: "Failed to connect to bus", fail: true}}}
	b = &Systemd{Run: s.run, Dir: t.TempDir()}
	if err := b.Start(ctx, spec.Name); err == nil || !strings.Contains(err.Error(), "Failed to connect to bus") {
		t.Fatalf("a failed start: %v", err)
	}
}

// Lingering off is a note; on, or unreadable, is none.
func TestSystemdLingerNote(t *testing.T) {
	ctx := context.Background()
	for out, note := range map[string]bool{"no\n": true, "yes\n": false} {
		s := &script{rules: []rule{{prefix: "loginctl show-user", out: out}}}
		notes := (&Systemd{Run: s.run}).Notes(ctx)
		if (len(notes) == 1 && strings.Contains(notes[0], "loginctl enable-linger")) != note {
			t.Errorf("linger %q: notes %v", out, notes)
		}
	}
	s := &script{rules: []rule{{prefix: "loginctl", fail: true}}}
	if notes := (&Systemd{Run: s.run}).Notes(ctx); len(notes) != 0 {
		t.Errorf("no loginctl: notes %v", notes)
	}
}

// launchd: the plist is written; start bootstraps an unloaded agent and
// kickstarts a loaded one; without a login session it says so; stop boots
// out only a loaded agent.
func TestLaunchdBackend(t *testing.T) {
	ctx := context.Background()
	label := "io.github.blackvs.aicrew." + spec.Name
	s := &script{rules: []rule{{prefix: "launchctl print gui/501/", fail: true}}}
	b := &Launchd{Run: s.run, Dir: t.TempDir(), UID: 501}
	if err := b.Install(ctx, spec); err != nil {
		t.Fatal(err)
	}
	plist := filepath.Join(b.Dir, label+".plist")
	if got, _ := os.ReadFile(plist); string(got) != LaunchdPlist(spec) {
		t.Fatalf("the plist:\n%s", got)
	}
	if err := b.Start(ctx, spec.Name); err != nil {
		t.Fatal(err)
	}
	if err := b.Stop(ctx, spec.Name); err != nil {
		t.Fatal(err)
	}
	want := "launchctl print gui/501\nlaunchctl print gui/501/" + label + "\nlaunchctl bootstrap gui/501 " + plist +
		"\nlaunchctl print gui/501/" + label
	if s.joined() != want {
		t.Fatalf("calls:\n%s\nwant:\n%s", s.joined(), want)
	}

	s = &script{rules: []rule{{prefix: "launchctl print gui/501/", out: "\tstate = running\n"}}}
	b.Run = s.run
	if inst, act, _ := b.State(ctx, spec.Name); !inst || !act {
		t.Fatalf("a running agent: %v %v", inst, act)
	}
	_ = b.Start(ctx, spec.Name)
	_ = b.Stop(ctx, spec.Name)
	if !strings.Contains(s.joined(), "launchctl kickstart gui/501/"+label) || !strings.Contains(s.joined(), "launchctl bootout gui/501/"+label) {
		t.Fatalf("calls:\n%s", s.joined())
	}
	if err := b.Remove(ctx, spec.Name); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(plist); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("the plist outlived its removal")
	}

	s = &script{rules: []rule{{prefix: "launchctl print gui/501", fail: true}}}
	b.Run = s.run
	if err := b.Start(ctx, spec.Name); !errors.Is(err, ErrNoLoginSession) {
		t.Fatalf("no login session: %v", err)
	}
}

// The task scheduler: the definition is registered from UTF-16 XML in the
// \aicrew\ folder; its state is the module's untranslated word; start and
// stop act only when needed; removal deletes task and file.
func TestTaskBackend(t *testing.T) {
	ctx := context.Background()
	s := &script{rules: []rule{{prefix: "powershell.exe", out: "Ready\r\n"}}}
	b := &Task{Run: s.run, Dir: t.TempDir(), User: `HOST\sol`}
	if err := b.Install(ctx, spec); err != nil {
		t.Fatal(err)
	}
	xmlFile := filepath.Join(b.Dir, spec.Name+".xml")
	if got, _ := os.ReadFile(xmlFile); string(got) != string(utf16File(TaskXML(spec, `HOST\sol`))) {
		t.Fatal("the task definition differs")
	}
	if err := b.Start(ctx, spec.Name); err != nil {
		t.Fatal(err)
	}
	if err := b.Stop(ctx, spec.Name); err != nil { // Ready: nothing to end
		t.Fatal(err)
	}
	tn := `\aicrew\` + spec.Name
	calls := s.joined()
	for _, w := range []string{"schtasks /Create /TN " + tn + " /XML " + xmlFile + " /F", "schtasks /Run /TN " + tn} {
		if !strings.Contains(calls, w) {
			t.Fatalf("calls lack %q:\n%s", w, calls)
		}
	}
	if strings.Contains(calls, "/End") {
		t.Fatalf("a task not running was ended:\n%s", calls)
	}
	if !strings.Contains(calls, `-TaskPath '\aicrew\' -TaskName '`+spec.Name+`'`) {
		t.Fatalf("the state query:\n%s", calls)
	}

	s = &script{rules: []rule{{prefix: "powershell.exe", out: "Running\r\n"}}}
	b.Run = s.run
	if inst, act, _ := b.State(ctx, spec.Name); !inst || !act {
		t.Fatal("a running task")
	}
	if err := b.Remove(ctx, spec.Name); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(s.joined(), "schtasks /End /TN "+tn) || !strings.Contains(s.joined(), "schtasks /Delete /TN "+tn+" /F") {
		t.Fatalf("calls:\n%s", s.joined())
	}
	if strings.Contains(s.joined(), "schtasks /Run") {
		t.Fatal("a running task was run again")
	}
	if _, err := os.Stat(xmlFile); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("the definition outlived its removal")
	}
	s = &script{rules: []rule{{prefix: "powershell.exe", out: "Missing\r\n"}}}
	b.Run = s.run
	if inst, _, _ := b.State(ctx, spec.Name); inst {
		t.Fatal("a missing task is installed")
	}
	if !b.OrphansServe() || (&Systemd{}).OrphansServe() || (&Launchd{}).OrphansServe() {
		t.Fatal("OrphansServe")
	}
}
