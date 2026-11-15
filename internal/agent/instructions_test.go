package agent

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"

	"github.com/BlackVS/aicrew/internal/store"
)

// forgeText is what no skills text may hold, nor an aimem text with an
// export base: a URL or a forge host.
var forgeText = regexp.MustCompile(`(?i)://|github|raw\.githubusercontent|releases/download`)

func testedAimem(t *testing.T) (supportedComponent, string) {
	t.Helper()
	set, err := LoadSupported()
	if err != nil {
		t.Fatal(err)
	}
	c := set.Components["aimem"]
	return c, c.Ranges[0].Tested
}

func withGOOS(t *testing.T, g string) {
	t.Helper()
	prev := goos
	goos = g
	t.Cleanup(func() { goos = prev })
}

// The skills instruction installs from the hub with the home's own aimem,
// names the aimem release that has the command, and holds no URL, on every
// platform, for a missing set and for one below the range.
func TestSkillsInstructionNamesNoForgeURL(t *testing.T) {
	set, _ := LoadSupported()
	home := filepath.Join(t.TempDir(), "agents", "builder")
	for _, g := range []string{"linux", "darwin", "windows"} {
		withGOOS(t, g)
		for _, found := range []string{"", "1.20.0"} {
			s := skillsInstruction(set.Components["ai-skills"], found, []string{"oh-code-review"}, home, "aimem")
			if forgeText.MatchString(s) {
				t.Fatalf("%s %q names a forge: %s", g, found, s)
			}
			for _, want := range []string{"`aimem skills install -p <project> --tool claude`", "aimem " + skillsInstallAimem + " or later",
				"AIMEM_STATE_DIR=" + AimemDir(home), "AIMEM_SOCKET=" + AimemSocket(home), "oh-code-review", "rerun `aicrew-agent check`"} {
				if !strings.Contains(s, want) {
					t.Fatalf("%s %q lacks %q: %s", g, found, want, s)
				}
			}
			if (found != "") != strings.Contains(s, "ai-skills 1.26.1 or later is required (found 1.20.0)") {
				t.Fatalf("%s %q: %s", g, found, s)
			}
		}
	}
}

// Every instruction the check gives for skills, through the real check:
// a client that sees no required skill, a version below the range and an
// unusable record, each with and without an export base.
func TestCheckSkillsInstructionsNameNoForgeURL(t *testing.T) {
	base := t.TempDir()
	for _, withBase := range []bool{false, true} {
		for _, c := range []struct{ name, record, reason string }{
			{"missing", "-", "claude_skills"}, {"below", "1.20.0", "ai-skills_below"}, {"record", "{", "ai-skills_record"},
		} {
			t.Run(c.name, func(t *testing.T) {
				e := setupCheck(t, readyTools, "aimem", "claude")
				if withBase {
					t.Setenv(BaseEnv, base)
				}
				switch c.record {
				case "-":
				case "{":
					e.installSkills(t, "", "oh-code-review")
					os.WriteFile(filepath.Join(e.user, ".claude", "skills", ".ai-skills.json"), []byte(c.record), 0o644)
				default:
					e.installSkills(t, c.record, "oh-code-review")
				}
				rep := e.check(t, "claude")
				if rep.Reason != c.reason {
					t.Fatalf("reason %q: %+v", rep.Reason, rep)
				}
				n := 0
				for _, s := range rep.Instructions {
					if !strings.Contains(s, "skill") {
						continue
					}
					n++
					if forgeText.MatchString(s) || !strings.Contains(s, "aimem skills install -p <project> --tool claude") {
						t.Fatalf("skills instruction: %s", s)
					}
				}
				if n == 0 {
					t.Fatalf("no skills instruction: %+v", rep.Instructions)
				}
			})
		}
	}
}

// Without a base, the missing-aimem line fetches aimem's boot script at the
// tested tag, never from the default branch, for the shell and for
// PowerShell, and says in one sentence how a closed network installs.
func TestAimemInstructionAtTestedTag(t *testing.T) {
	c, tested := testedAimem(t)
	for g, script := range map[string]string{"linux": "boot.sh", "darwin": "boot.sh", "windows": "boot.ps1"} {
		withGOOS(t, g)
		for _, found := range []string{"", "0.9.2"} {
			s := aimemInstruction(c, found, "")
			for _, want := range []string{"https://raw.githubusercontent.com/BlackVS/aimem/v" + tested + "/" + script,
				"AIMEM_VERSION", "v" + tested, "In a closed network, install it from your aiforge export instead, as the export's README.txt says"} {
				if !strings.Contains(s, want) {
					t.Fatalf("%s %q lacks %q: %s", g, found, want, s)
				}
			}
			if strings.Contains(s, "/master/") || strings.Count(s, "://") != 1 {
				t.Fatalf("%s %q: %s", g, found, s)
			}
		}
	}
}

// With a base, the missing-aimem line installs from the export, in aimem's
// own export form, and names no forge host; an aimem release before the one
// that reads AIMEM_BASE cannot come from an export, so its line is the
// connected one with the closed-network sentence.
func TestAimemInstructionFromExport(t *testing.T) {
	c, tested := testedAimem(t)
	for _, tc := range []struct{ goos, base, want string }{
		{"linux", "/media/aiforge-export", "AIMEM_BASE='/media/aiforge-export' AIMEM_VERSION=v" + tested +
			" bash '/media/aiforge-export/aimem/v" + tested + "/boot.sh'"},
		{"darwin", "/Volumes/ex port", "bash '/Volumes/ex port/aimem/v" + tested + "/boot.sh'"},
		{"linux", "https://files.example.com/aiforge", "curl -fsSL 'https://files.example.com/aiforge/aimem/v" + tested +
			"/boot.sh' | AIMEM_BASE='https://files.example.com/aiforge' AIMEM_VERSION=v" + tested + " bash"},
		{"windows", `\\files\aiforge-export`, `Copy-Item -LiteralPath '\\files\aiforge-export\aimem\v` + tested +
			`\boot.ps1' -Destination boot.ps1 -ErrorAction Stop`},
		{"windows", `D:\export`, `$env:AIMEM_BASE='D:\export'`},
		{"windows", "https://files.example.com/aiforge", "irm 'https://files.example.com/aiforge/aimem/v" + tested + "/boot.ps1' | iex"},
	} {
		withGOOS(t, tc.goos)
		s := aimemInstruction(c, "", tc.base)
		if !strings.Contains(s, tc.want) || !strings.Contains(s, "from the aiforge export "+tc.base) {
			t.Fatalf("%s %s lacks %q: %s", tc.goos, tc.base, tc.want, s)
		}
		rest := strings.ReplaceAll(s, tc.base, "")
		if forgeText.MatchString(rest) || strings.Contains(s, "In a closed network") {
			t.Fatalf("%s %s names another host: %s", tc.goos, tc.base, s)
		}
	}
	withGOOS(t, "linux")
	old := supportedComponent{Ranges: []versionRange{{Min: "0.10.0", Tested: "0.10.22"}}}
	if s := aimemInstruction(old, "", "/media/aiforge-export"); strings.Contains(s, "AIMEM_BASE") ||
		!strings.Contains(s, "/aimem/v0.10.22/boot.sh") || !strings.Contains(s, "In a closed network") {
		t.Fatalf("an aimem before %s from an export: %s", exportAimem, s)
	}
}

// The base is validated as the installers validate AICREW_BASE.
func TestCheckBase(t *testing.T) {
	good := map[string]string{"https://files.example.com/aiforge/": "https://files.example.com/aiforge"}
	bad := []string{"", "http://files.example.com/aiforge", "https://", "ftp://x/y", "relative/export", "./export",
		"https://user@files.example.com/x"}
	if runtime.GOOS == "windows" {
		good[`D:\export\`] = `D:\export`
		good[`\\files\aiforge-export`] = `\\files\aiforge-export`
		bad = append(bad, `D:\`, `D:`, "/media/export")
	} else {
		good["/media/aiforge-export/"] = "/media/aiforge-export"
		bad = append(bad, "/", `D:\export`)
	}
	for in, want := range good {
		if got, err := checkBase(in); err != nil || got != want {
			t.Fatalf("%q: %q, %v", in, got, err)
		}
	}
	for _, in := range bad {
		if got, err := checkBase(in); err == nil {
			t.Fatalf("%q accepted as %q", in, got)
		}
	}
}

// The check names the export join recorded; an AICREW_BASE in its own
// environment overrides it; an invalid one is a notice, and the connected
// form is given.
func TestCheckNamesTheExport(t *testing.T) {
	recorded, override := t.TempDir(), t.TempDir()
	for _, c := range []struct {
		name, record, env, want, notice string
	}{
		{"none", "", "", "raw.githubusercontent.com/BlackVS/aimem/v", ""},
		{"recorded", recorded, "", "from the aiforge export " + recorded, ""},
		{"environment wins", recorded, override, "from the aiforge export " + override, ""},
		{"invalid environment", recorded, "http://files.example.com/x", "raw.githubusercontent.com/BlackVS/aimem/v",
			"the environment's AICREW_BASE is not used"},
	} {
		t.Run(c.name, func(t *testing.T) {
			e := setupCheck(t, readyTools, "claude")
			if c.record != "" {
				path := filepath.Join(e.home, "agent.json")
				doc := readJSON(t, path)
				doc["export_base"] = c.record
				raw, _ := json.Marshal(doc)
				os.WriteFile(path, raw, 0o600)
			}
			if c.env != "" {
				t.Setenv(BaseEnv, c.env)
			}
			rep := e.check(t, "claude")
			if rep.Reason != "aimem_missing" || !hasInstruction(rep, c.want) || (c.notice != "" && !hasNotice(rep, c.notice)) {
				t.Fatalf("%+v", rep)
			}
			if _, err := LoadConfig(e.home); err != nil {
				t.Fatal(err)
			}
		})
	}
}

// Join records AICREW_BASE as export_base; a rerun without it keeps the
// recorded one, a rerun with another replaces it, a join without it records
// none, and an invalid one is refused before anything changes.
func TestJoinRecordsTheExportBase(t *testing.T) {
	base, other := t.TempDir(), t.TempDir()
	e := setupJoin(t)
	o := e.opts()
	o.ExportBase = "http://files.example.com/x"
	if rep, err := Join(context.Background(), o, e.deps(&recCrew{}, activeAimem(), new(int))); err != ErrJoinUsage ||
		!strings.Contains(rep.Instruction, "AICREW_BASE") {
		t.Fatalf("an http base: %v %+v", err, rep)
	}
	if _, err := os.Stat(filepath.Join(e.home, "agent.json")); err == nil {
		t.Fatal("a refused join wrote agent.json")
	}
	o.ExportBase = base + string(filepath.Separator)
	e.join(t, o, &recCrew{}, activeAimem(), e.invite(t, "inv-1", store.RoleWorker))
	recordedBase := func() any { return readJSON(t, filepath.Join(e.home, "agent.json"))["export_base"] }
	if got := recordedBase(); got != base || !strings.Contains(e.out.String(), "Recorded the aiforge export "+base) {
		t.Fatalf("first join recorded %v:\n%s", got, e.out.String())
	}
	if _, err := LoadConfig(e.home); err != nil {
		t.Fatal(err)
	}
	e.join(t, JoinOptions{Home: e.home}, &recCrew{}, activeAimem())
	if got := recordedBase(); got != base {
		t.Fatalf("a rerun without the base left %v", got)
	}
	e.join(t, JoinOptions{Home: e.home, ExportBase: other}, &recCrew{}, activeAimem())
	if got := recordedBase(); got != other {
		t.Fatalf("a rerun with another base left %v", got)
	}

	plain := setupJoin(t)
	plain.join(t, plain.opts(), &recCrew{}, activeAimem(), plain.invite(t, "inv-2", store.RoleWorker))
	if got := readJSON(t, filepath.Join(plain.home, "agent.json"))["export_base"]; got != nil {
		t.Fatalf("a join without a base recorded %v", got)
	}
}

// The export form the check prints runs: aimem's boot script from the
// export, as a local file on Windows, with AIMEM_BASE and AIMEM_VERSION set
// and gone afterwards, from a new empty directory; a missing script runs
// nothing.
func TestAimemExportFormRuns(t *testing.T) {
	c, tested := testedAimem(t)
	withGOOS(t, runtime.GOOS)
	form := regexp.MustCompile("`([^`]*AIMEM_BASE[^`]*)`")
	for _, present := range []bool{true, false} {
		base := t.TempDir()
		seen := filepath.Join(t.TempDir(), "seen.txt")
		dir := filepath.Join(base, "aimem", "v"+tested)
		os.MkdirAll(dir, 0o755)
		m := form.FindStringSubmatch(aimemInstruction(c, "", base))
		if m == nil {
			t.Fatal("no export form")
		}
		var cmd *exec.Cmd
		if runtime.GOOS == "windows" {
			ps, err := exec.LookPath("powershell.exe")
			if err != nil {
				t.Skip("no Windows PowerShell")
			}
			if present {
				os.WriteFile(filepath.Join(dir, "boot.ps1"), []byte("Set-Content -LiteralPath $env:SEEN -Value "+
					"(\"$env:AIMEM_BASE|$env:AIMEM_VERSION|$PSCommandPath|\" + (Get-ChildItem -Name | Measure-Object).Count)\n"), 0o644)
			}
			script := filepath.Join(t.TempDir(), "form.ps1")
			os.WriteFile(script, []byte("try {\n"+m[1]+"\n} catch { Write-Output \"failed: $_\" }\n"+
				"Write-Output (\"left: \" + (Test-Path Env:AIMEM_BASE) + (Test-Path Env:AIMEM_VERSION))\n"), 0o644)
			cmd = exec.Command(ps, "-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass", "-File", script)
		} else {
			if present {
				os.WriteFile(filepath.Join(dir, "boot.sh"), []byte("printf '%s|%s|%s|%s' \"$AIMEM_BASE\" \"$AIMEM_VERSION\" \"$0\" "+
					"\"$(ls -A | wc -l | tr -d ' ')\" > \"$SEEN\"\n"), 0o644)
			}
			cmd = exec.Command("bash", "-c", m[1])
		}
		cmd.Env = append(os.Environ(), "SEEN="+seen)
		out, err := cmd.CombinedOutput()
		got, _ := os.ReadFile(seen)
		if runtime.GOOS == "windows" && !strings.Contains(string(out), "left: FalseFalse") {
			t.Fatalf("present=%v: the form left a variable:\n%s", present, out)
		}
		if !present {
			if len(got) != 0 || (runtime.GOOS != "windows" && err == nil) ||
				(runtime.GOOS == "windows" && !strings.Contains(string(out), "failed:")) {
				t.Fatalf("a missing script ran %q or did not fail: %v\n%s", got, err, out)
			}
			continue
		}
		f := strings.Split(strings.TrimSpace(string(got)), "|")
		script := filepath.Join(dir, "boot.sh")
		empty := "0"
		if runtime.GOOS == "windows" {
			script, empty = "boot.ps1", "1" // the copy, alone in its new directory
		}
		if len(f) != 4 || f[0] != base || f[1] != "v"+tested || !strings.HasSuffix(f[2], script) || f[3] != empty ||
			(runtime.GOOS == "windows" && strings.HasPrefix(f[2], base)) {
			t.Fatalf("the form ran %q: %v\n%s", got, err, out)
		}
	}
}

// The skills command runs as printed when the home's aimem is a path that
// needs quoting: a space and an apostrophe, under PowerShell on Windows and
// bash elsewhere.
func TestSkillsCommandRunsWithAQuotedAimemPath(t *testing.T) {
	withGOOS(t, runtime.GOOS)
	dir := filepath.Join(t.TempDir(), "my tool's")
	os.MkdirAll(dir, 0o755)
	seen := filepath.Join(t.TempDir(), "seen.txt")
	exe := filepath.Join(dir, "aimem")
	if runtime.GOOS == "windows" {
		exe += ".cmd"
		os.WriteFile(exe, []byte("@echo %* > \"%SEEN%\"\r\n"), 0o755)
	} else {
		os.WriteFile(exe, []byte("#!/bin/sh\nprintf '%s ' \"$@\" > \"$SEEN\"\n"), 0o755)
	}
	set, _ := LoadSupported()
	s := skillsInstruction(set.Components["ai-skills"], "", []string{"oh-code-review"}, t.TempDir(), exe)
	m := regexp.MustCompile("`([^`]*skills install[^`]*)`").FindStringSubmatch(s)
	if m == nil {
		t.Fatalf("no command: %s", s)
	}
	line := strings.ReplaceAll(m[1], "<project>", "proj")
	var cmd *exec.Cmd
	if runtime.GOOS == "windows" {
		ps, err := exec.LookPath("powershell.exe")
		if err != nil {
			t.Skip("no Windows PowerShell")
		}
		cmd = exec.Command(ps, "-NoProfile", "-NonInteractive", "-Command", line)
	} else {
		cmd = exec.Command("bash", "-c", line)
	}
	cmd.Env = append(os.Environ(), "SEEN="+seen)
	out, err := cmd.CombinedOutput()
	got, _ := os.ReadFile(seen)
	if err != nil || strings.TrimSpace(string(got)) != "skills install -p proj --tool claude" {
		t.Fatalf("%s ran %q: %v\n%s", line, got, err, out)
	}
}
