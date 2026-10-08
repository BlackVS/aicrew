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
	agentOnce sync.Map // version -> *sync.Once
	agentErr  sync.Map // version -> error
)

// agentAsset is the release asset name of aicrew-agent for this platform.
func agentAsset() string {
	name := "aicrew-agent-" + runtime.GOOS + "-" + runtime.GOARCH
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	return name
}

// agentRelease is a release directory, as file:// serves it to the boot
// scripts: aicrew-agent stamped with version under its asset name, and a
// SHA256SUMS listing it (or, with badSum, listing another hash).
func agentRelease(t *testing.T, version string, badSum bool) string {
	t.Helper()
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("no go toolchain to build the binaries under test")
	}
	built := filepath.Join(binDir, "agent-"+version, agentAsset())
	once, _ := agentOnce.LoadOrStore(version, &sync.Once{})
	once.(*sync.Once).Do(func() {
		cmd := exec.Command("go", "build", "-o", built,
			"-ldflags", "-X github.com/BlackVS/aicrew/internal/version.Override="+version, "./cmd/aicrew-agent")
		cmd.Dir = repoRoot
		cmd.Env = append(os.Environ(), "CGO_ENABLED=0")
		if out, err := cmd.CombinedOutput(); err != nil {
			agentErr.Store(version, fmt.Errorf("build aicrew-agent: %v\n%s", err, out))
		}
	})
	if err, ok := agentErr.Load(version); ok {
		t.Fatal(err)
	}
	b, err := os.ReadFile(built)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, agentAsset()), b, 0o755); err != nil {
		t.Fatal(err)
	}
	h := sha256.Sum256(b)
	if badSum {
		h = sha256.Sum256(append(b, 0))
	}
	sums := hex.EncodeToString(h[:]) + "  " + agentAsset() + "\n" + strings.Repeat("0", 64) + "  LICENSE\n"
	if err := os.WriteFile(filepath.Join(dir, "SHA256SUMS"), []byte(sums), 0o644); err != nil {
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

func bootSh(home, binDir, tmp string) bootRun {
	return func(t *testing.T, rel, tag string) (string, int) {
		t.Helper()
		text := "set -euo pipefail\n" + extractFrom(t, "boot.sh", "install-agent") + "install_agent\n"
		cmd := exec.Command("bash", "-c", text)
		cmd.Dir = home
		cmd.Env = append(os.Environ(), "HOME="+home, "TMPDIR="+tmp, "TAG="+tag, "DL_BASE="+fileURL(rel),
			"BIN_DIR="+binDir, "OS="+runtime.GOOS, "ARCH="+runtime.GOARCH)
		return combined(t, cmd)
	}
}

func bootPs1(home, binDir, tmp string) bootRun {
	return func(t *testing.T, rel, tag string) (string, int) {
		t.Helper()
		script := filepath.Join(t.TempDir(), "run.ps1")
		text := extractFrom(t, "boot.ps1", "install-agent") +
			fmt.Sprintf("Install-Agent '%s' '%s' '%s'\n", tag, fileURL(rel), binDir)
		if err := os.WriteFile(script, []byte(text), 0o600); err != nil {
			t.Fatal(err)
		}
		cmd := exec.Command("powershell.exe", "-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass", "-File", script)
		cmd.Dir = home
		// HOME and USERPROFILE stay the real ones: PowerShell itself keeps
		// startup data there. The script writes only into binDir, and runs
		// from home as its working directory.
		cmd.Env = append(os.Environ(), "TEMP="+tmp, "TMP="+tmp)
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

// The boot script installs the release into the bin directory, says a
// current one is current and leaves it alone, refuses a binary whose hash
// SHA256SUMS does not list (keeping the installed one), and upgrades an
// older one, naming both versions. Run from the home directory, it leaves
// no file anywhere but the binary, and its temporary directory is removed.
func TestBootInstallsAndUpgrades(t *testing.T) {
	var (
		run   bootRun
		exe   string
		home  = t.TempDir()
		tmp   = t.TempDir()
		bin   = filepath.Join(home, ".local", "bin")
		v1    = agentRelease(t, "v0.0.1", false)
		v2bad = agentRelease(t, "v0.0.2", true)
		v2    = agentRelease(t, "v0.0.2", false)
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
		run, exe = bootPs1(home, bin, tmp), "aicrew-agent.exe"
	default:
		shellOnly(t)
		run, exe = bootSh(home, bin, tmp), "aicrew-agent"
	}
	installed := filepath.Join(bin, exe)

	out, code := run(t, v1, "v0.0.1")
	if code != 0 || !strings.Contains(out, "installed aicrew-agent v0.0.1") || agentVersion(t, installed) != "v0.0.1" {
		t.Fatalf("install: exit %d:\n%s", code, out)
	}
	info, err := os.Stat(installed)
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(10 * time.Millisecond)

	out, code = run(t, v1, "v0.0.1")
	if code != 0 || !strings.Contains(out, "aicrew-agent v0.0.1 is current") {
		t.Fatalf("same release: exit %d:\n%s", code, out)
	}
	if again, err := os.Stat(installed); err != nil || !again.ModTime().Equal(info.ModTime()) {
		t.Fatal("re-running the installed release changed the binary")
	}

	out, code = run(t, v2bad, "v0.0.2")
	if code == 0 || !strings.Contains(out, "checksum mismatch for "+agentAsset()) || agentVersion(t, installed) != "v0.0.1" {
		t.Fatalf("an unlisted hash was not refused, or the installed binary changed (exit %d):\n%s", code, out)
	}

	// The installed binary is running through the upgrade, as a member's
	// launcher or Stop hook would be: git-credential waits on its input.
	running := exec.Command(installed, "git-credential", "--home", t.TempDir(), "erase")
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
	if code != 0 || !strings.Contains(out, "upgraded aicrew-agent v0.0.1 -> v0.0.2") || agentVersion(t, installed) != "v0.0.2" {
		t.Fatalf("upgrade: exit %d:\n%s", code, out)
	}

	got := files(t, home)
	want := []string{filepath.ToSlash(must(filepath.Rel(home, installed)))}
	if runtime.GOOS == "windows" {
		// The upgrade renamed the previous binary aside, in the bin
		// directory; the next run removes it.
		for _, f := range got {
			if strings.Contains(f, "aicrew-agent.exe.old-") {
				want = append(want, f)
			}
		}
		sort.Strings(want)
	}
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
