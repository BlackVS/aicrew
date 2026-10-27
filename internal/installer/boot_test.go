package installer

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"
)

var (
	builtOnce sync.Map // binary@version -> *sync.Once
	builtErr  sync.Map // binary@version -> error
)

// memberBinaries are what the boot scripts install: the member's agent and
// the operator CLI beside it.
var memberBinaries = []string{"aicrew-agent", "aicrew"}

// asset is the release asset name of a binary for this platform.
func asset(binary string) string {
	name := binary + "-" + runtime.GOOS + "-" + runtime.GOARCH
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	return name
}

// agentAsset is the release asset name of aicrew-agent for this platform.
func agentAsset() string { return asset("aicrew-agent") }

// agentRelease is a release directory, as file:// serves it to the boot
// scripts: aicrew-agent and aicrew stamped with version under their asset
// names, and a SHA256SUMS listing them; badSum names the binary whose
// listed hash is another one ("" for none).
func agentRelease(t *testing.T, version, badSum string) string {
	t.Helper()
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("no go toolchain to build the binaries under test")
	}
	dir := t.TempDir()
	var sums strings.Builder
	for _, binary := range memberBinaries {
		key := binary + "@" + version
		built := filepath.Join(binDir, "release-"+version, asset(binary))
		once, _ := builtOnce.LoadOrStore(key, &sync.Once{})
		once.(*sync.Once).Do(func() {
			cmd := exec.Command("go", "build", "-o", built,
				"-ldflags", "-X github.com/BlackVS/aicrew/internal/version.Override="+version, "./cmd/"+binary)
			cmd.Dir = repoRoot
			cmd.Env = append(os.Environ(), "CGO_ENABLED=0")
			if out, err := cmd.CombinedOutput(); err != nil {
				builtErr.Store(key, fmt.Errorf("build %s: %v\n%s", binary, err, out))
			}
		})
		if err, ok := builtErr.Load(key); ok {
			t.Fatal(err)
		}
		b, err := os.ReadFile(built)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, asset(binary)), b, 0o755); err != nil {
			t.Fatal(err)
		}
		h := sha256.Sum256(b)
		if badSum == binary {
			h = sha256.Sum256(append(b, 0))
		}
		sums.WriteString(hex.EncodeToString(h[:]) + "  " + asset(binary) + "\n")
	}
	sums.WriteString(strings.Repeat("0", 64) + "  LICENSE\n")
	if err := os.WriteFile(filepath.Join(dir, "SHA256SUMS"), []byte(sums.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

func fileURL(dir string) string {
	p := filepath.ToSlash(dir)
	if !strings.HasPrefix(p, "/") {
		p = "/" + p
	}
	return "file://" + p
}

// files lists every regular file under root, relative to it.
func files(t *testing.T, root string) []string {
	t.Helper()
	var out []string
	err := filepath.Walk(root, func(p string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if !info.IsDir() {
			rel, _ := filepath.Rel(root, p)
			out = append(out, filepath.ToSlash(rel))
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(out)
	return out
}

// bootRun runs one install by the boot script for this platform, from home
// as the working directory, with home as HOME and tmp as the temporary
// directory.
type bootRun func(t *testing.T, rel, tag string) (string, int)

// serviceEnv keeps the installed aicrew-agent off the user's own service
// registry: a test's registry is a file under its home, which a registry
// never read is not created as.
func serviceEnv(home string) string {
	return "AICREW_SERVICE_REGISTRY=" + filepath.Join(home, "services.json")
}

// bootSh and bootPs1 run the boot script's install step; env is added to
// its environment.
func bootSh(home, binDir, tmp string, env ...string) bootRun {
	return func(t *testing.T, rel, tag string) (string, int) {
		t.Helper()
		text := "set -euo pipefail\n" + extractFrom(t, "boot.sh", "install-agent") + "install_agent\n"
		cmd := exec.Command("bash", "-c", text)
		cmd.Dir = home
		cmd.Env = append(os.Environ(), "HOME="+home, "TMPDIR="+tmp, "TAG="+tag, "DL_BASE="+fileURL(rel),
			"BIN_DIR="+binDir, "OS="+runtime.GOOS, "ARCH="+runtime.GOARCH, serviceEnv(home))
		cmd.Env = append(cmd.Env, env...)
		return combined(t, cmd)
	}
}

func bootPs1(home, binDir, tmp string, env ...string) bootRun {
	return func(t *testing.T, rel, tag string) (string, int) {
		t.Helper()
		script := filepath.Join(t.TempDir(), "run.ps1")
		// As the script does before its install step.
		text := "$ErrorActionPreference = 'Stop'\n" + extractFrom(t, "boot.ps1", "install-agent") +
			fmt.Sprintf("Install-Agent '%s' '%s' '%s'\n", tag, fileURL(rel), binDir)
		if err := os.WriteFile(script, []byte(text), 0o600); err != nil {
			t.Fatal(err)
		}
		cmd := exec.Command("powershell.exe", "-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass", "-File", script)
		cmd.Dir = home
		// HOME and USERPROFILE stay the real ones: PowerShell itself keeps
		// startup data there. The script writes only into binDir, and runs
		// from home as its working directory.
		cmd.Env = append(os.Environ(), "TEMP="+tmp, "TMP="+tmp, serviceEnv(home))
		cmd.Env = append(cmd.Env, env...)
		return combined(t, cmd)
	}
}

func combined(t *testing.T, cmd *exec.Cmd) (string, int) {
	t.Helper()
	out, err := cmd.CombinedOutput()
	if ee, ok := err.(*exec.ExitError); ok {
		return string(out), ee.ExitCode()
	} else if err != nil {
		t.Fatal(err)
	}
	return string(out), 0
}

// The boot script installs the release into the bin directory: aicrew-agent
// and the operator CLI aicrew beside it (01a124c9-1648). It says a current
// pair is current and leaves both alone, refuses a release where either
// binary's hash is not the one SHA256SUMS lists (installing neither and
// keeping the installed pair), and upgrades an older pair, naming both
// versions of each. Run from the home directory, it leaves no file anywhere
// but the binaries, and its temporary directory is removed.
func TestBootInstallsAndUpgrades(t *testing.T) {
	var (
		run      bootRun
		ext      string
		home     = t.TempDir()
		tmp      = t.TempDir()
		bin      = filepath.Join(home, ".local", "bin")
		v1       = agentRelease(t, "v0.0.1", "")
		v2badCLI = agentRelease(t, "v0.0.2", "aicrew")
		v2badAgt = agentRelease(t, "v0.0.2", "aicrew-agent")
		v2       = agentRelease(t, "v0.0.2", "")
	)
	switch runtime.GOOS {
	case "windows":
		if runtime.GOARCH != "amd64" {
			t.Skip("boot.ps1 installs the windows/amd64 release only")
		}
		if _, err := exec.LookPath("powershell.exe"); err != nil {
			t.Skip("no powershell.exe")
		}
		bin = filepath.Join(home, "AppData", "Local", "aicrew", "bin")
		run, ext = bootPs1(home, bin, tmp), ".exe"
	default:
		shellOnly(t)
		run = bootSh(home, bin, tmp)
	}
	installed := map[string]string{}
	for _, b := range memberBinaries {
		installed[b] = filepath.Join(bin, b+ext)
	}
	versions := func() string {
		return agentVersion(t, installed["aicrew-agent"]) + "/" + agentVersion(t, installed["aicrew"])
	}

	out, code := run(t, v1, "v0.0.1")
	if code != 0 || !strings.Contains(out, "installed aicrew-agent v0.0.1") || !strings.Contains(out, "installed aicrew v0.0.1") ||
		versions() != "v0.0.1/v0.0.1" {
		t.Fatalf("install: exit %d:\n%s", code, out)
	}
	mod := map[string]time.Time{}
	for b, p := range installed {
		info, err := os.Stat(p)
		if err != nil {
			t.Fatal(err)
		}
		mod[b] = info.ModTime()
	}
	time.Sleep(10 * time.Millisecond)

	out, code = run(t, v1, "v0.0.1")
	if code != 0 || !strings.Contains(out, "aicrew-agent v0.0.1 is current") || !strings.Contains(out, "aicrew v0.0.1 is current") {
		t.Fatalf("same release: exit %d:\n%s", code, out)
	}
	for b, p := range installed {
		if again, err := os.Stat(p); err != nil || !again.ModTime().Equal(mod[b]) {
			t.Fatalf("re-running the installed release changed %s", b)
		}
	}

	for _, bad := range []struct {
		dir, binary string
	}{{v2badCLI, "aicrew"}, {v2badAgt, "aicrew-agent"}} {
		out, code = run(t, bad.dir, "v0.0.2")
		if code == 0 || !strings.Contains(out, "checksum mismatch for "+asset(bad.binary)) || versions() != "v0.0.1/v0.0.1" {
			t.Fatalf("a bad hash for %s was not refused, or an installed binary changed (exit %d):\n%s", bad.binary, code, out)
		}
	}

	// The installed agent is running through the upgrade, as a member's
	// launcher or Stop hook would be: git-credential waits on its input.
	running := exec.Command(installed["aicrew-agent"], "git-credential", "--home", t.TempDir(), "erase")
	stdin, err := running.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := running.Start(); err != nil {
		t.Fatal(err)
	}
	exited := make(chan struct{})
	go func() { _ = running.Wait(); close(exited) }()
	time.Sleep(300 * time.Millisecond)
	out, code = run(t, v2, "v0.0.2")
	select {
	case <-exited:
		t.Fatal("the running aicrew-agent exited before the upgrade finished; the test proves nothing")
	default:
	}
	stdin.Close()
	<-exited
	if code != 0 || !strings.Contains(out, "upgraded aicrew-agent v0.0.1 -> v0.0.2") ||
		!strings.Contains(out, "upgraded aicrew v0.0.1 -> v0.0.2") || versions() != "v0.0.2/v0.0.2" {
		t.Fatalf("upgrade: exit %d:\n%s", code, out)
	}

	got := files(t, home)
	var want []string
	for _, p := range installed {
		want = append(want, filepath.ToSlash(must(filepath.Rel(home, p))))
	}
	if runtime.GOOS == "windows" {
		// The upgrade renamed the previous binaries aside, in the bin
		// directory; the next run removes them.
		for _, f := range got {
			if strings.Contains(f, ".exe.old-") {
				want = append(want, f)
			}
		}
	}
	sort.Strings(want)
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("files under the home: %v, want %v", got, want)
	}
	if left := files(t, tmp); len(left) != 0 {
		t.Fatalf("the temporary directory keeps %v", left)
	}
}

func must(s string, err error) string {
	if err != nil {
		panic(err)
	}
	return s
}

func agentVersion(t *testing.T, bin string) string {
	t.Helper()
	out, err := exec.Command(bin, "version").Output()
	if err != nil {
		t.Fatalf("%s version: %v", bin, err)
	}
	return strings.Fields(string(out))[1]
}

// boot.ps1 reads an installed binary's version when the binary lingers
// after printing it, and reads none when it fails. A pipeline cut short by
// Select-Object -First abandoned a process that had printed its line but
// not yet exited, and read its exit code as -1. On loaded Windows runners
// the installer then reinstalled the current release, or reported an
// upgrade as an install (task 01a12bf6-fdcb).
func TestBootPs1VersionRead(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("boot.ps1 runs on Windows")
	}
	if _, err := exec.LookPath("powershell.exe"); err != nil {
		t.Skip("no powershell.exe")
	}
	stub := filepath.Join(t.TempDir(), "lingerversion.exe")
	build := exec.Command("go", "build", "-o", stub, "./internal/installer/testdata/lingerversion")
	build.Dir = repoRoot
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build the stub: %v\n%s", err, out)
	}
	script := filepath.Join(t.TempDir(), "read.ps1")
	text := extractFrom(t, "boot.ps1", "install-agent") + fmt.Sprintf("Write-Output ('[' + (Get-AgentVersion '%s') + ']')\n", stub)
	if err := os.WriteFile(script, []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct{ linger, exit, want string }{
		{"0", "0", "[v0.0.1]"},
		{"300", "0", "[v0.0.1]"},
		{"300", "3", "[]"},
	} {
		cmd := exec.Command("powershell.exe", "-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass", "-File", script)
		cmd.Env = append(os.Environ(), "LINGER_MS="+c.linger, "EXIT_CODE="+c.exit)
		out, code := combined(t, cmd)
		if code != 0 || strings.TrimSpace(out) != c.want {
			t.Errorf("linger %sms, exit %s: read %q (exit %d), want %s", c.linger, c.exit, strings.TrimSpace(out), code, c.want)
		}
	}
}

// sumsForm is how prebuiltDir's SHA256SUMS lists a binary: not at all (no
// file), by its own name, or by its release asset name, as a SHA256SUMS
// copied from a release does.
type sumsForm int

const (
	noSums sumsForm = iota
	byName
	byAsset
)

// prebuiltDir is a directory as AICREW_PREBUILT_DIR takes it: the binaries
// of the release directory rel under their own names, and a SHA256SUMS
// listing them in the given form; badSum names the binary whose listed hash
// is another one ("" for none).
func prebuiltDir(t *testing.T, rel string, sums sumsForm, badSum string) string {
	t.Helper()
	dir := t.TempDir()
	var list strings.Builder
	for _, binary := range memberBinaries {
		b, err := os.ReadFile(filepath.Join(rel, asset(binary)))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, binary), b, 0o755); err != nil {
			t.Fatal(err)
		}
		h := sha256.Sum256(b)
		if badSum == binary {
			h = sha256.Sum256(append(b, 0))
		}
		listed := binary
		if sums == byAsset {
			listed = asset(binary)
		}
		list.WriteString(hex.EncodeToString(h[:]) + "  " + listed + "\n")
	}
	if sums != noSums {
		if err := os.WriteFile(filepath.Join(dir, "SHA256SUMS"), []byte(list.String()), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// boot.sh installs from AICREW_PREBUILT_DIR with nothing to download
// (01a17d8e-4267): a missing binary or one whose hash is not the one the
// directory's SHA256SUMS lists installs neither, and the refused one is
// never run; a SHA256SUMS may list the binaries by their own names or by
// their release asset names. An installed pair that is the same files is
// current, and another pair replaces it, with or without a SHA256SUMS.
func TestBootShPrebuilt(t *testing.T) {
	shellOnly(t)
	home, tmp := t.TempDir(), t.TempDir()
	bin := filepath.Join(home, ".local", "bin")
	nothing := t.TempDir() // DL_BASE: every download from it fails
	v1 := agentRelease(t, "v0.0.1", "")
	v2 := agentRelease(t, "v0.0.2", "")
	run := func(dir string) (string, int) {
		return bootSh(home, bin, tmp, "AICREW_PREBUILT_DIR="+dir)(t, nothing, "v9.9.9")
	}
	versions := func() string {
		return agentVersion(t, filepath.Join(bin, "aicrew-agent")) + "/" + agentVersion(t, filepath.Join(bin, "aicrew"))
	}

	partial := prebuiltDir(t, v1, byName, "")
	if err := os.Remove(filepath.Join(partial, "aicrew")); err != nil {
		t.Fatal(err)
	}
	out, code := run(partial)
	if code == 0 || !strings.Contains(out, "/aicrew is missing") || len(files(t, home)) != 0 {
		t.Fatalf("a missing binary was not refused, or something was installed (exit %d):\n%s", code, out)
	}
	out, code = run(prebuiltDir(t, v1, byName, "aicrew"))
	if code == 0 || !strings.Contains(out, "checksum mismatch for") || len(files(t, home)) != 0 {
		t.Fatalf("a bad hash was not refused, or something was installed (exit %d):\n%s", code, out)
	}
	out, code = run(prebuiltDir(t, v1, byAsset, "aicrew-agent"))
	if code == 0 || !strings.Contains(out, "checksum mismatch for") || len(files(t, home)) != 0 {
		t.Fatalf("a bad hash listed by asset name was not refused, or something was installed (exit %d):\n%s", code, out)
	}
	// A binary whose hash does not match is refused without being run.
	ran := filepath.Join(t.TempDir(), "ran")
	tampered := prebuiltDir(t, v1, byName, "")
	script := "#!/bin/sh\ntouch '" + ran + "'\necho 'aicrew-agent v0.0.9'\n"
	if err := os.WriteFile(filepath.Join(tampered, "aicrew-agent"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	out, code = run(tampered)
	if _, err := os.Stat(ran); err == nil || code == 0 || !strings.Contains(out, "checksum mismatch for") {
		t.Fatalf("a tampered binary was run or not refused (exit %d):\n%s", code, out)
	}

	out, code = run(prebuiltDir(t, v1, byName, ""))
	if code != 0 || !strings.Contains(out, "installing the prebuilt aicrew-agent from") ||
		!strings.Contains(out, "installed aicrew-agent v0.0.1") || !strings.Contains(out, "installed aicrew v0.0.1") ||
		versions() != "v0.0.1/v0.0.1" {
		t.Fatalf("install: exit %d:\n%s", code, out)
	}
	// An agent that is the installed one but whose SHA256SUMS entry is wrong,
	// or absent, beside a changed aicrew with a right one: neither installs.
	for _, absent := range []bool{false, true} {
		mixed := prebuiltDir(t, v2, byName, "aicrew-agent")
		agent, err := os.ReadFile(filepath.Join(bin, "aicrew-agent"))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(mixed, "aicrew-agent"), agent, 0o755); err != nil {
			t.Fatal(err)
		}
		if absent {
			b, err := os.ReadFile(filepath.Join(mixed, "SHA256SUMS"))
			if err != nil {
				t.Fatal(err)
			}
			_, cli, _ := strings.Cut(string(b), "\n")
			if err := os.WriteFile(filepath.Join(mixed, "SHA256SUMS"), []byte(cli), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		out, code = run(mixed)
		if code == 0 || !strings.Contains(out, "checksum mismatch for "+mixed+"/aicrew-agent") || versions() != "v0.0.1/v0.0.1" {
			t.Fatalf("a current agent with a bad entry (absent %v) let aicrew through (exit %d, %s):\n%s", absent, code, versions(), out)
		}
	}
	out, code = run(prebuiltDir(t, v1, noSums, ""))
	if code != 0 || !strings.Contains(out, "aicrew-agent v0.0.1 is current") || !strings.Contains(out, "aicrew v0.0.1 is current") {
		t.Fatalf("same binaries: exit %d:\n%s", code, out)
	}
	out, code = run(prebuiltDir(t, v2, byAsset, ""))
	if code != 0 || !strings.Contains(out, "upgraded aicrew-agent v0.0.1 -> v0.0.2") ||
		!strings.Contains(out, "upgraded aicrew v0.0.1 -> v0.0.2") || versions() != "v0.0.2/v0.0.2" {
		t.Fatalf("upgrade, SHA256SUMS by asset name: exit %d:\n%s", code, out)
	}
	out, code = run(prebuiltDir(t, v1, noSums, ""))
	if code != 0 || !strings.Contains(out, "upgraded aicrew-agent v0.0.2 -> v0.0.1") ||
		!strings.Contains(out, "upgraded aicrew v0.0.2 -> v0.0.1") || versions() != "v0.0.1/v0.0.1" {
		t.Fatalf("replace, no SHA256SUMS: exit %d:\n%s", code, out)
	}
	if left := files(t, tmp); len(left) != 0 {
		t.Fatalf("the temporary directory keeps %v", left)
	}
}

// boot.sh stops and starts the members' services around a prebuilt
// upgrade as it does around a downloaded one.
func TestBootShPrebuiltRestartsTheServices(t *testing.T) {
	shellOnly(t)
	home, tmp := t.TempDir(), t.TempDir()
	bin := filepath.Join(home, "bin")
	stubLog := filepath.Join(t.TempDir(), "service.log")
	nothing := t.TempDir()
	run := func(dir string) {
		t.Helper()
		out, code := bootSh(home, bin, tmp, "STUB_LOG="+stubLog, "STUB_SERVICES=1", "AICREW_PREBUILT_DIR="+dir)(t, nothing, "v9.9.9")
		if code != 0 {
			t.Fatalf("exit %d:\n%s", code, out)
		}
	}
	run(prebuiltDir(t, stubRelease(t, "v0.0.1"), byName, ""))
	os.Remove(stubLog)
	run(prebuiltDir(t, stubRelease(t, "v0.0.2"), byName, ""))
	b, _ := os.ReadFile(stubLog)
	got := strings.Split(strings.TrimSpace(string(b)), "\n")
	if len(got) != 2 || strings.HasPrefix(got[0], "v0.0.2 bin ") || !strings.HasSuffix(got[0], " service stop --all") ||
		got[1] != "v0.0.2 bin service start --all" {
		t.Fatalf("a prebuilt upgrade with a service: %q", got)
	}
}

// stubRelease is a release directory whose aicrew-agent and aicrew are
// stubagent stamped with version.
func stubRelease(t *testing.T, version string) string {
	t.Helper()
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("no go toolchain to build the stub")
	}
	built := filepath.Join(t.TempDir(), "stubagent")
	cmd := exec.Command("go", "build", "-o", built, "-ldflags", "-X main.version="+version,
		"./internal/installer/testdata/stubagent")
	cmd.Dir = repoRoot
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build the stub: %v\n%s", err, out)
	}
	b, err := os.ReadFile(built)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	h := sha256.Sum256(b)
	var sums strings.Builder
	for _, binary := range memberBinaries {
		if err := os.WriteFile(filepath.Join(dir, asset(binary)), b, 0o755); err != nil {
			t.Fatal(err)
		}
		sums.WriteString(hex.EncodeToString(h[:]) + "  " + asset(binary) + "\n")
	}
	if err := os.WriteFile(filepath.Join(dir, "SHA256SUMS"), []byte(sums.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

// The boot script stops the members' services with the new aicrew-agent,
// still in its temporary directory, before the swap, and starts them again
// with the installed one after it; with no service stopped it starts none,
// and AICREW_NO_SERVICE=1 or a current release leaves them alone.
func TestBootRestartsTheServices(t *testing.T) {
	home, tmp := t.TempDir(), t.TempDir()
	bin := filepath.Join(home, "bin")
	stubLog := filepath.Join(t.TempDir(), "service.log")
	var boot func(home, binDir, tmp string, env ...string) bootRun
	switch runtime.GOOS {
	case "windows":
		if runtime.GOARCH != "amd64" {
			t.Skip("boot.ps1 installs the windows/amd64 release only")
		}
		if _, err := exec.LookPath("powershell.exe"); err != nil {
			t.Skip("no powershell.exe")
		}
		boot = bootPs1
	default:
		shellOnly(t)
		boot = bootSh
	}
	read := func() []string {
		b, _ := os.ReadFile(stubLog)
		os.Remove(stubLog)
		return strings.Split(strings.TrimSpace(string(b)), "\n")
	}
	run := func(rel, tag string, env ...string) string {
		t.Helper()
		out, code := boot(home, bin, tmp, append([]string{"STUB_LOG=" + stubLog}, env...)...)(t, rel, tag)
		if code != 0 {
			t.Fatalf("%s: exit %d:\n%s", tag, code, out)
		}
		return out
	}

	run(stubRelease(t, "v0.0.1"), "v0.0.1")
	if got := read(); len(got) != 1 || !strings.HasPrefix(got[0], "v0.0.1 ") || strings.HasPrefix(got[0], "v0.0.1 bin ") ||
		!strings.HasSuffix(got[0], " service stop --all") {
		t.Fatalf("a first install with no service: %q", got)
	}

	out := run(stubRelease(t, "v0.0.2"), "v0.0.2", "STUB_SERVICES=1")
	got := read()
	if len(got) != 2 || strings.HasPrefix(got[0], "v0.0.2 bin ") || !strings.HasSuffix(got[0], " service stop --all") ||
		got[1] != "v0.0.2 bin service start --all" {
		t.Fatalf("an upgrade with a service: %q", got)
	}
	if !strings.Contains(out, "stopped for the upgrade:") || !strings.Contains(out, "aicrew-member-sol-0a1b2c3d: stopped") {
		t.Fatalf("the upgrade did not say what it stopped:\n%s", out)
	}

	run(stubRelease(t, "v0.0.2"), "v0.0.2", "STUB_SERVICES=1")
	if got := read(); len(got) != 1 || got[0] != "" {
		t.Fatalf("a current release touched the services: %q", got)
	}
	run(stubRelease(t, "v0.0.3"), "v0.0.3", "STUB_SERVICES=1", "AICREW_NO_SERVICE=1")
	if got := read(); len(got) != 1 || got[0] != "" {
		t.Fatalf("AICREW_NO_SERVICE=1 touched the services: %q", got)
	}
}
