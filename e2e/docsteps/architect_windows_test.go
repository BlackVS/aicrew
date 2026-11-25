package docsteps

import (
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// windowsScenario is the declaration (scenarios.json) of the document's
// steps the Windows test job runs: step 8's PowerShell lines.
const windowsScenario = "go-test-windows"

// TestWindowsArchitectInit runs step 8's PowerShell line for the
// architect's directory as the document writes it, in Windows PowerShell,
// against a temporary home, and checks what the document says to check:
// the ending line, and a protected access list on creds\ of the user,
// SYSTEM and Administrators.
func TestWindowsArchitectInit(t *testing.T) {
	const id = "architect-init-windows"
	d, err := Load(docPath)
	if err != nil {
		t.Fatal(err)
	}
	decl, err := LoadScenarios("scenarios.json")
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(decl[windowsScenario], id) {
		t.Fatalf("scenarios.json's %s does not declare %s", windowsScenario, id)
	}
	line, err := d.Text(id, map[string]string{"PROJECT": "demo", "AICREWD": "aicrewd.example", "PORT": "9443"})
	if err != nil {
		t.Fatal(err)
	}
	bins, home := buildCLIs(t), t.TempDir()
	out, err := powershell(t, home, bins, line)
	if err != nil {
		t.Fatalf("%s: %v\n%s", line, err, out)
	}
	if want := "the architect directory is ready"; !strings.Contains(out, want) {
		t.Fatalf("%s: the output does not end with %q:\n%s", line, want, out)
	}
	creds := filepath.Join(home, "aicrew", "architect", "creds")
	user := whoami(t)
	acl := icacls(t, creds)
	want := []string{user + ":(OI)(CI)(F)", `NT AUTHORITY\SYSTEM:(OI)(CI)(F)`, `BUILTIN\Administrators:(OI)(CI)(F)`}
	slices.Sort(acl)
	slices.Sort(want)
	if !slices.Equal(acl, want) {
		t.Errorf("icacls %s: %q, want %q (none inherited)", creds, acl, want)
	}
	if got, err := powershell(t, home, bins, "(Get-Acl -LiteralPath '"+creds+"').AreAccessRulesProtected"); err != nil || strings.TrimSpace(got) != "True" {
		t.Errorf("%s's access list is not protected: %q, %v", creds, got, err)
	}
}

// powershell runs a command line in Windows PowerShell with home as the
// user's home ($HOME) and bins first on PATH.
func powershell(t *testing.T, home, bins, line string) (string, error) {
	t.Helper()
	vol := filepath.VolumeName(home)
	env := slices.DeleteFunc(os.Environ(), func(e string) bool {
		k, _, _ := strings.Cut(e, "=")
		return slices.Contains([]string{"USERPROFILE", "HOMEDRIVE", "HOMEPATH", "PATH"}, strings.ToUpper(k))
	})
	env = append(env, "USERPROFILE="+home, "HOMEDRIVE="+vol, "HOMEPATH="+strings.TrimPrefix(home, vol),
		"PATH="+bins+string(os.PathListSeparator)+os.Getenv("PATH"))
	cmd := exec.Command("powershell.exe", "-NoProfile", "-NonInteractive", "-Command", line)
	cmd.Env = env
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// whoami is the account running the test as icacls names it, DOMAIN\user.
func whoami(t *testing.T) string {
	t.Helper()
	u, err := user.Current()
	if err != nil {
		t.Fatalf("the current user: %v", err)
	}
	return u.Username
}

// icacls is path's access entries as icacls prints them, one per entry.
func icacls(t *testing.T, path string) []string {
	t.Helper()
	out, err := exec.Command("icacls", path).Output()
	if err != nil {
		t.Fatalf("icacls %s: %v\n%s", path, err, out)
	}
	var entries []string
	for i, l := range strings.Split(strings.ReplaceAll(string(out), "\r", ""), "\n") {
		if i == 0 {
			l = strings.TrimPrefix(l, path)
		}
		if l = strings.TrimSpace(l); l == "" || strings.HasPrefix(l, "Successfully processed") {
			continue
		}
		entries = append(entries, l)
	}
	return entries
}

// TestWindowsArchitectCarry runs step 8's carrying line as the document
// writes it, with Windows' own OpenSSH client, against the local OpenSSH
// server scripts/ci-windows-sshd.ps1 sets up on a CI runner, which stands
// in for aicrewd's host (its user's home holds the handed-over file). It
// carries a throwaway file into the directory step 8's first line made and
// checks what the document says to check: the carried file's access list
// is the user, SYSTEM and Administrators, each inherited from creds\, and
// no copy is left on the server. Without that server
// (AICREW_CI_SSH_HOST unset) it skips.
func TestWindowsArchitectCarry(t *testing.T) {
	const id = "architect-carry-windows"
	host := os.Getenv("AICREW_CI_SSH_HOST")
	if host == "" {
		t.Skip("no local OpenSSH server: AICREW_CI_SSH_HOST is set by scripts/ci-windows-sshd.ps1 in CI")
	}
	d, err := Load(docPath)
	if err != nil {
		t.Fatal(err)
	}
	decl, err := LoadScenarios("scenarios.json")
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(decl[windowsScenario], id) {
		t.Fatalf("scenarios.json's %s does not declare %s", windowsScenario, id)
	}
	bins, home := buildCLIs(t), t.TempDir()
	initLine, err := d.Text("architect-init-windows", map[string]string{"PROJECT": "demo", "AICREWD": "aicrewd.example", "PORT": "9443"})
	if err != nil {
		t.Fatal(err)
	}
	if out, err := powershell(t, home, bins, initLine); err != nil {
		t.Fatalf("%s: %v\n%s", initLine, err, out)
	}

	// The server's side: the login user's home is the runner user's own.
	serverHome, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	handed := filepath.Join(serverHome, "architect-planning.token")
	if _, err := os.Lstat(handed); err == nil {
		t.Fatalf("%s exists already; it is not this test's to replace", handed)
	}
	const value = "throwaway-architect-token\n"
	if err := os.WriteFile(handed, []byte(value), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Remove(handed) })

	line, err := d.Text(id, map[string]string{"AICREWD_HOST": host})
	if err != nil {
		t.Fatal(err)
	}
	if out, err := powershell(t, home, bins, line); err != nil {
		t.Fatalf("%s: %v\n%s", line, err, out)
	}
	carried := filepath.Join(home, "aicrew", "architect", "creds", "aicrew.architect")
	if b, err := os.ReadFile(carried); err != nil || string(b) != value {
		t.Fatalf("the carried file %s: %q, %v", carried, b, err)
	}
	if _, err := os.Lstat(handed); !os.IsNotExist(err) {
		t.Errorf("the server's copy %s is left: %v", handed, err)
	}
	acl := icacls(t, carried)
	want := []string{whoami(t) + ":(I)(F)", `NT AUTHORITY\SYSTEM:(I)(F)`, `BUILTIN\Administrators:(I)(F)`}
	slices.Sort(acl)
	slices.Sort(want)
	if !slices.Equal(acl, want) {
		t.Errorf("icacls %s: %q, want %q", carried, acl, want)
	}
}
