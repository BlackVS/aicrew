package service

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/BlackVS/aicrew/internal/managedfiles"
)

// Runner runs a service manager's command and returns its combined output.
type Runner func(ctx context.Context, name string, args ...string) (string, error)

// ExecRunner runs the command for real.
func ExecRunner(ctx context.Context, name string, args ...string) (string, error) {
	out, err := exec.CommandContext(ctx, name, args...).CombinedOutput()
	return string(out), err
}

// Backend is one operating system's service manager.
type Backend interface {
	// Kind names it: systemd, launchd or task.
	Kind() string
	// Install writes the definition and registers it to start at login,
	// without starting it. It rewrites an existing one.
	Install(ctx context.Context, s Spec) error
	// Start starts the service; a running one is left as it is.
	Start(ctx context.Context, name string) error
	// Stop stops the service; a stopped one is left as it is.
	Stop(ctx context.Context, name string) error
	// Remove stops and unregisters it and deletes its definition.
	Remove(ctx context.Context, name string) error
	// State reports whether it is installed and whether it runs.
	State(ctx context.Context, name string) (installed, active bool, err error)
	// Notes are what the member should know about this manager, such as
	// lingering being off.
	Notes(ctx context.Context) []string
	// OrphansServe is true when stopping the service can leave its serve
	// running, so that the serve's own PID must be stopped as well.
	OrphansServe() bool
}

// ErrUnsupported is NewBackend's answer where no service manager is found.
var ErrUnsupported = errors.New("no supported service manager")

// NewBackend is the service manager of goos: systemd's user manager, launchd
// or the Windows task scheduler. dir is the registry's directory, where
// the task definitions are kept.
func NewBackend(goos string, run Runner, dir string) (Backend, error) {
	switch goos {
	case "linux":
		if _, err := exec.LookPath("systemctl"); err != nil {
			return nil, fmt.Errorf("%w: systemctl not found", ErrUnsupported)
		}
		d, err := systemdUnitDir()
		if err != nil {
			return nil, err
		}
		return &Systemd{Run: run, Dir: d}, nil
	case "darwin":
		h, err := os.UserHomeDir()
		if err != nil {
			return nil, err
		}
		return &Launchd{Run: run, Dir: filepath.Join(h, "Library", "LaunchAgents"), UID: os.Getuid()}, nil
	case "windows":
		u, err := user.Current()
		if err != nil {
			return nil, err
		}
		return &Task{Run: run, Dir: filepath.Join(dir, "tasks"), User: u.Username}, nil
	}
	return nil, fmt.Errorf("%w on %s", ErrUnsupported, goos)
}

func systemdUnitDir() (string, error) {
	if x := os.Getenv("XDG_CONFIG_HOME"); x != "" {
		return filepath.Join(x, "systemd", "user"), nil
	}
	h, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(h, ".config", "systemd", "user"), nil
}

func writeDefinition(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	return managedfiles.WriteAtomic(path, data)
}

func failed(what, out string, err error) error {
	out = strings.TrimSpace(out)
	if out == "" {
		return fmt.Errorf("%s: %w", what, err)
	}
	return fmt.Errorf("%s: %w: %s", what, err, out)
}

// Systemd is the systemd user manager (systemctl --user).
type Systemd struct {
	Run Runner
	Dir string // the user unit directory
}

func (b *Systemd) Kind() string { return "systemd" }

func (b *Systemd) unit(name string) string { return name + ".service" }

func (b *Systemd) ctl(ctx context.Context, args ...string) (string, error) {
	return b.Run(ctx, "systemctl", append([]string{"--user"}, args...)...)
}

func (b *Systemd) Install(ctx context.Context, s Spec) error {
	if err := writeDefinition(filepath.Join(b.Dir, b.unit(s.Name)), []byte(SystemdUnit(s))); err != nil {
		return err
	}
	if out, err := b.ctl(ctx, "daemon-reload"); err != nil {
		return failed("systemctl --user daemon-reload", out, err)
	}
	if out, err := b.ctl(ctx, "enable", b.unit(s.Name)); err != nil {
		return failed("systemctl --user enable "+b.unit(s.Name), out, err)
	}
	return nil
}

func (b *Systemd) Start(ctx context.Context, name string) error {
	// A unit that gave up after failures does not start until reset.
	_, _ = b.ctl(ctx, "reset-failed", b.unit(name))
	if out, err := b.ctl(ctx, "start", b.unit(name)); err != nil {
		return failed("systemctl --user start "+b.unit(name), out, err)
	}
	return nil
}

func (b *Systemd) Stop(ctx context.Context, name string) error {
	if out, err := b.ctl(ctx, "stop", b.unit(name)); err != nil {
		return failed("systemctl --user stop "+b.unit(name), out, err)
	}
	return nil
}

func (b *Systemd) Remove(ctx context.Context, name string) error {
	path := filepath.Join(b.Dir, b.unit(name))
	if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if out, err := b.ctl(ctx, "disable", "--now", b.unit(name)); err != nil {
		return failed("systemctl --user disable --now "+b.unit(name), out, err)
	}
	if err := os.Remove(path); err != nil {
		return err
	}
	_, _ = b.ctl(ctx, "daemon-reload")
	return nil
}

func (b *Systemd) State(ctx context.Context, name string) (bool, bool, error) {
	if _, err := os.Stat(filepath.Join(b.Dir, b.unit(name))); errors.Is(err, os.ErrNotExist) {
		return false, false, nil
	}
	// is-active exits non-zero for any state but active; its output names
	// the state either way.
	out, _ := b.ctl(ctx, "is-active", b.unit(name))
	switch strings.TrimSpace(out) {
	case "active", "activating", "reloading", "deactivating":
		return true, true, nil
	}
	return true, false, nil
}

func (b *Systemd) Notes(ctx context.Context) []string {
	u, err := user.Current()
	if err != nil {
		return nil
	}
	out, err := b.Run(ctx, "loginctl", "show-user", u.Username, "--property=Linger", "--value")
	if err == nil && strings.TrimSpace(out) == "no" {
		return []string{"lingering is off for " + u.Username + ": the service stops at your last logout and starts " +
			"at your next login. To keep it running without a login: loginctl enable-linger " + u.Username}
	}
	return nil
}

func (b *Systemd) OrphansServe() bool { return false }

// Launchd is launchd's per-user agents (launchctl, the gui/UID domain).
type Launchd struct {
	Run Runner
	Dir string // ~/Library/LaunchAgents
	UID int
}

// ErrNoLoginSession is Launchd.Start's answer without a login session (an
// ssh login with nobody at the console): the agent loads at the next login.
var ErrNoLoginSession = errors.New("no login session for launchd: the agent loads at your next login")

func (b *Launchd) Kind() string { return "launchd" }

func (b *Launchd) domain() string            { return "gui/" + strconv.Itoa(b.UID) }
func (b *Launchd) target(name string) string { return b.domain() + "/" + launchdLabel(name) }
func (b *Launchd) plistPath(name string) string {
	return filepath.Join(b.Dir, launchdLabel(name)+".plist")
}

func (b *Launchd) Install(ctx context.Context, s Spec) error {
	return writeDefinition(b.plistPath(s.Name), []byte(LaunchdPlist(s)))
}

func (b *Launchd) loaded(ctx context.Context, name string) (string, bool) {
	out, err := b.Run(ctx, "launchctl", "print", b.target(name))
	return out, err == nil
}

func (b *Launchd) Start(ctx context.Context, name string) error {
	if _, err := b.Run(ctx, "launchctl", "print", b.domain()); err != nil {
		return ErrNoLoginSession
	}
	if _, ok := b.loaded(ctx, name); ok {
		if out, err := b.Run(ctx, "launchctl", "kickstart", b.target(name)); err != nil {
			return failed("launchctl kickstart "+b.target(name), out, err)
		}
		return nil
	}
	if out, err := b.Run(ctx, "launchctl", "bootstrap", b.domain(), b.plistPath(name)); err != nil {
		return failed("launchctl bootstrap "+b.domain(), out, err)
	}
	return nil
}

func (b *Launchd) Stop(ctx context.Context, name string) error {
	if _, ok := b.loaded(ctx, name); !ok {
		return nil
	}
	if out, err := b.Run(ctx, "launchctl", "bootout", b.target(name)); err != nil {
		return failed("launchctl bootout "+b.target(name), out, err)
	}
	return nil
}

func (b *Launchd) Remove(ctx context.Context, name string) error {
	if err := b.Stop(ctx, name); err != nil {
		return err
	}
	if err := os.Remove(b.plistPath(name)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

func (b *Launchd) State(ctx context.Context, name string) (bool, bool, error) {
	if _, err := os.Stat(b.plistPath(name)); errors.Is(err, os.ErrNotExist) {
		return false, false, nil
	}
	out, ok := b.loaded(ctx, name)
	return true, ok && strings.Contains(out, "state = running"), nil
}

func (b *Launchd) Notes(context.Context) []string { return nil }

func (b *Launchd) OrphansServe() bool { return false }

// Task is the Windows task scheduler: a logon task in the \aicrew\ folder,
// registered from its XML definition with schtasks.
type Task struct {
	Run  Runner
	Dir  string // where the definitions are kept
	User string // DOMAIN\user, whose logon starts it
}

func (b *Task) Kind() string { return "task" }

func (b *Task) tn(name string) string { return `\aicrew\` + name }

func (b *Task) xmlPath(name string) string { return filepath.Join(b.Dir, name+".xml") }

func (b *Task) Install(ctx context.Context, s Spec) error {
	if err := writeDefinition(b.xmlPath(s.Name), utf16File(TaskXML(s, b.User))); err != nil {
		return err
	}
	if out, err := b.Run(ctx, "schtasks", "/Create", "/TN", b.tn(s.Name), "/XML", b.xmlPath(s.Name), "/F"); err != nil {
		return failed("schtasks /Create "+b.tn(s.Name), out, err)
	}
	return nil
}

func (b *Task) Start(ctx context.Context, name string) error {
	if _, active, _ := b.State(ctx, name); active {
		return nil
	}
	if out, err := b.Run(ctx, "schtasks", "/Run", "/TN", b.tn(name)); err != nil {
		return failed("schtasks /Run "+b.tn(name), out, err)
	}
	return nil
}

func (b *Task) Stop(ctx context.Context, name string) error {
	if _, active, _ := b.State(ctx, name); !active {
		return nil
	}
	if out, err := b.Run(ctx, "schtasks", "/End", "/TN", b.tn(name)); err != nil {
		return failed("schtasks /End "+b.tn(name), out, err)
	}
	return nil
}

func (b *Task) Remove(ctx context.Context, name string) error {
	installed, _, err := b.State(ctx, name)
	if err != nil {
		return err
	}
	if installed {
		if err := b.Stop(ctx, name); err != nil {
			return err
		}
		if out, err := b.Run(ctx, "schtasks", "/Delete", "/TN", b.tn(name), "/F"); err != nil {
			return failed("schtasks /Delete "+b.tn(name), out, err)
		}
	}
	if err := os.Remove(b.xmlPath(name)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

// State reads the task's state as the ScheduledTasks module names it,
// which is not translated, unlike schtasks /Query's columns.
func (b *Task) State(ctx context.Context, name string) (bool, bool, error) {
	script := `$t = Get-ScheduledTask -TaskPath '\aicrew\' -TaskName '` + strings.ReplaceAll(name, "'", "''") +
		`' -ErrorAction SilentlyContinue; if ($t) { [string]$t.State } else { 'Missing' }`
	out, err := b.Run(ctx, "powershell.exe", "-NoProfile", "-NonInteractive", "-Command", script)
	if err != nil {
		return false, false, failed("Get-ScheduledTask "+b.tn(name), out, err)
	}
	switch strings.TrimSpace(out) {
	case "Missing":
		return false, false, nil
	case "Running":
		return true, true, nil
	}
	return true, false, nil
}

func (b *Task) Notes(context.Context) []string { return nil }

// OrphansServe: ending a task ends its conhost and leaves the serve it
// started running.
func (b *Task) OrphansServe() bool { return true }
