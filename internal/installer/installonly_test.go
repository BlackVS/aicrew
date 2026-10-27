package installer

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// storeOf has the sandbox's installed release serve once, until it answers
// health, and stop: its store then exists, as on a host where it has run.
func storeOf(t *testing.T, s *sandbox) {
	t.Helper()
	text := extract(t, "upgrade-transaction") + `"$ROOT/bin/aicrewd" -config "$ROOT/etc/aicrewd.json" >/dev/null 2>&1 &
pid=$!
rc=0
AICREW_UPGRADE_WAIT=20 txn_wait "$LISTEN" any || rc=$?
kill "$pid"; wait "$pid" || true
exit "$rc"`
	if out, code := bash(t, text, "ROOT="+s.root, "LISTEN="+s.listen); code != 0 {
		t.Fatalf("the installed release did not serve (exit %d):\n%s", code, out)
	}
	if _, err := os.Stat(s.store); err != nil {
		t.Fatal(err)
	}
}

// An upgrade in install-only mode (01a17d8e-427b) copies the store and the
// configuration, keeps the previous binaries and swaps them, and starts
// nothing: it prints the command that starts the new release.
func TestUpgradeInstallOnly(t *testing.T) {
	shellOnly(t)
	s := newSandbox(t, release(t, "v0.0.1"), false, "")
	storeOf(t, s)
	out, code := runUpgrade(t, s, release(t, "v0.0.2"), "NO_SERVICE=1")
	bin := filepath.Join(s.root, "bin")
	if code != 0 || !strings.Contains(out, "upgraded aicrewd v0.0.1 -> v0.0.2; nothing was started") ||
		!strings.Contains(out, "Start it as the service user: "+filepath.Join(bin, "aicrewd")+" -config "+s.config) ||
		!strings.Contains(out, "health now: \n") {
		t.Fatalf("exit %d:\n%s", code, out)
	}
	if versionOf(t, filepath.Join(bin, "aicrewd")) != "v0.0.2" || versionOf(t, filepath.Join(bin, "aicrewd.prev")) != "v0.0.1" {
		t.Fatal("the new aicrewd is not in place, or the previous one not kept")
	}
	if len(backups(t, filepath.Join(s.root, "etc"), "aicrewd.json.", "backup-")) != 1 ||
		len(backups(t, filepath.Join(s.root, "lib"), "aicrew.db.backup-", "")) == 0 {
		t.Fatalf("copies missing:\n%s", out)
	}
}

// In install-only mode the installer cannot stop the aicrewd another
// supervisor runs, so it refuses to copy a store in use: nothing changes.
func TestUpgradeInstallOnlyRefusesWhileRunning(t *testing.T) {
	shellOnly(t)
	s := newSandbox(t, release(t, "v0.0.1"), false, "")
	before := snapshot(t, s.root)
	out, code := runUpgrade(t, s, release(t, "v0.0.2"), "NO_SERVICE=1", "RUNNING=1")
	if code == 0 || !strings.Contains(out, "does not stop it") || !strings.Contains(out, "Nothing was changed") {
		t.Fatalf("exit %d:\n%s", code, out)
	}
	if after := snapshot(t, s.root); !equalSnapshots(before, after) {
		t.Fatalf("files changed:\nbefore %v\nafter  %v", before, after)
	}
}

// A migration that fails in install-only mode still puts the previous
// binaries, the configuration copy and the store copy back, starts nothing,
// and says to start the previous release again.
func TestUpgradeInstallOnlyRollsBack(t *testing.T) {
	shellOnly(t)
	old, next := release(t, "v0.0.1"), release(t, "v0.0.2")
	good := credDir(t)
	s := newSandbox(t, old, true, good)
	storeOf(t, s)
	before, err := os.ReadFile(s.config)
	if err != nil {
		t.Fatal(err)
	}
	// The new aicrewd migrates the preflight's scratch copy, then fails the
	// real migration.
	staged := t.TempDir()
	mark := filepath.Join(t.TempDir(), "migrated")
	stub := "#!/bin/sh\nif [ \"$1 $2\" = 'config migrate' ]; then\n  [ ! -e '" + mark + "' ] || exit 1\n  touch '" + mark + "'\nfi\n" +
		"exec " + filepath.Join(next, "aicrewd") + " \"$@\"\n"
	if err := os.WriteFile(filepath.Join(staged, "aicrewd"), []byte(stub), 0o755); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(next, "aicrew"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(staged, "aicrew"), b, 0o755); err != nil {
		t.Fatal(err)
	}
	out, code := runUpgrade(t, s, staged, "NO_SERVICE=1", "AICREW_CRED_DIR="+good)
	if code == 0 || !strings.Contains(out, "config migrate did not complete; rolling back") ||
		!strings.Contains(out, "ROLLED BACK: the previous binaries, the configuration and the store copied before the upgrade are restored") ||
		!strings.Contains(out, "Start aicrewd v0.0.1 again as the service user") || !strings.Contains(out, "health now: \n") {
		t.Fatalf("exit %d:\n%s", code, out)
	}
	if versionOf(t, filepath.Join(s.root, "bin", "aicrewd")) != "v0.0.1" {
		t.Fatal("the previous aicrewd is not back")
	}
	if after, _ := os.ReadFile(s.config); string(after) != string(before) {
		t.Fatal("the configuration is not the copy")
	}
	if len(backups(t, filepath.Join(s.root, "lib"), "aicrew.db", "failed-")) == 0 {
		t.Fatalf("the store the failed upgrade left was not set aside:\n%s", out)
	}
}

// The whole script in install-only mode, as root in a container without
// systemd would run it: a fresh install creates the service account and
// writes the configuration, never calls systemctl or loginctl and starts
// nothing; the release runs from the command it prints. Then an upgrade
// refuses while that release runs and, once it is stopped, swaps the
// binaries and starts nothing. The account and root commands are stubs.
func TestScriptInstallOnly(t *testing.T) {
	shellOnly(t)
	v1, v2 := release(t, "v0.0.1"), release(t, "v0.0.2")
	state, stubs, home := t.TempDir(), t.TempDir(), filepath.Join(t.TempDir(), "svc")
	idPath, err := exec.LookPath("id")
	if err != nil {
		t.Skip("no id")
	}
	for name, body := range map[string]string{
		"id": `case "$*" in
  -u) echo 0 ;;
  "$STUB_USER") [ -e "$STUB_STATE/user" ] ;;
  "-u $STUB_USER") [ -e "$STUB_STATE/user" ] && exec ` + idPath + ` -u ;;
  *) exec ` + idPath + ` "$@" ;;
esac`,
		"getent":    `[ "$1 $2" = "passwd $STUB_USER" ] && [ -e "$STUB_STATE/user" ] && echo "$STUB_USER:x:1000:1000::$STUB_HOME:/bin/bash"`,
		"useradd":   `mkdir -p "$STUB_HOME" && touch "$STUB_STATE/user"`,
		"runuser":   `[ "$1" = -u ] && [ "$3" = -- ] || exit 2; shift 3; exec "$@"`,
		"systemctl": `echo "systemctl $*" >>"$STUB_STATE/systemd-calls"; exit 1`,
		"loginctl":  `echo "loginctl $*" >>"$STUB_STATE/systemd-calls"; exit 1`,
	} {
		if err := os.WriteFile(filepath.Join(stubs, name), []byte("#!/bin/sh\n"+body+"\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	certFile, keyFile := writeCert(t, t.TempDir())
	listen := "127.0.0.1:" + strconv.Itoa(freePort(t))
	root := filepath.Join(home, "aicrew")
	aicrewd, config := filepath.Join(root, "bin", "aicrewd"), filepath.Join(root, "etc", "aicrewd.json")
	run := func(rel string) (string, int) {
		t.Helper()
		cmd := exec.Command("bash", filepath.Join(repoRoot, script))
		cmd.Env = append(os.Environ(), "PATH="+stubs+string(os.PathListSeparator)+os.Getenv("PATH"), "TMPDIR="+t.TempDir(),
			"STUB_USER=simsvc", "STUB_STATE="+state, "STUB_HOME="+home, "AICREW_HUB_USER=simsvc", "AICREW_NO_SERVICE=1",
			"AICREW_PREBUILT_DIR="+rel, "AICREW_LISTEN="+listen, "AICREW_TLS_CERT="+certFile, "AICREW_TLS_KEY="+keyFile)
		out, err := cmd.CombinedOutput()
		if ee, ok := err.(*exec.ExitError); ok {
			return string(out), ee.ExitCode()
		} else if err != nil {
			t.Fatal(err)
		}
		return string(out), 0
	}
	noSystemd := func(out string) {
		t.Helper()
		if calls, err := os.ReadFile(filepath.Join(state, "systemd-calls")); err == nil {
			t.Fatalf("systemd was called:\n%s\n%s", calls, out)
		}
		if _, err := os.Stat(filepath.Join(home, ".config", "systemd")); err == nil {
			t.Fatalf("a systemd unit directory was made:\n%s", out)
		}
	}
	healthAt := func(version string) bool {
		t.Helper()
		_, code := bash(t, extract(t, "upgrade-transaction")+`AICREW_UPGRADE_WAIT=15 txn_wait "$LISTEN" "$WANT"`,
			"LISTEN="+listen, "WANT="+version)
		return code == 0
	}

	out, code := run(v1)
	if code != 0 || !strings.Contains(out, "created user simsvc") ||
		!strings.Contains(out, "aicrewd is installed and not started (AICREW_NO_SERVICE=1). Have a supervisor run, as simsvc:\n  "+aicrewd+" -config "+config+"\n") ||
		!strings.Contains(out, "Restart aicrewd: through your supervisor, with the command above") || strings.Contains(out, "aicrewd is up") {
		t.Fatalf("fresh install: exit %d:\n%s", code, out)
	}
	noSystemd(out)
	if _, err := os.Stat(filepath.Join(root, "etc", "operator.token")); err != nil {
		t.Fatalf("no operator token:\n%s", out)
	}

	serve := exec.Command(aicrewd, "-config", config)
	if err := serve.Start(); err != nil {
		t.Fatal(err)
	}
	stop := func() {
		if serve != nil {
			serve.Process.Kill()
			serve.Wait()
			serve = nil
		}
	}
	defer stop()
	if !healthAt("v0.0.1") {
		t.Fatal("the printed command did not serve v0.0.1")
	}

	out, code = run(v2)
	if code == 0 || !strings.Contains(out, "does not stop it") || versionOf(t, aicrewd) != "v0.0.1" {
		t.Fatalf("an upgrade while aicrewd runs: exit %d:\n%s", code, out)
	}
	stop()
	out, code = run(v2)
	if code != 0 || !strings.Contains(out, "upgraded aicrewd v0.0.1 -> v0.0.2; nothing was started") || versionOf(t, aicrewd) != "v0.0.2" {
		t.Fatalf("upgrade: exit %d:\n%s", code, out)
	}
	noSystemd(out)
}
