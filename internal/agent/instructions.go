package agent

import (
	"errors"
	"fmt"
	"net/url"
	"path/filepath"
	"runtime"
	"strings"
)

// The exact steps a blocked check reports (5a installs nothing). Each pins
// the version it names and installs only from a verified source: aimem's
// boot script, taken at the tag it installs, checks its release asset
// against SHA256SUMS, and the skills come from the aimem hub, which serves
// the set the project's process pins (docs/DESIGN-INSTANCE-BOUNDARY.md,
// D4). No skills text names a forge URL; with an export base, no aimem
// text does either.

// goos is the platform the instructions are written for; tests replace it.
var goos = runtime.GOOS

const (
	// exportAimem is the first aimem whose boot scripts read AIMEM_BASE.
	exportAimem = "0.10.23"
	// BaseEnv names an aiforge export, as the installers read it.
	BaseEnv = "AICREW_BASE"
)

// checkBase validates an export base as the installers do: an absolute
// directory (on Windows also a drive or \\host\share path) or an https://
// URL with a host; plain http and a relative path are refused. It returns
// the base without a trailing separator.
func checkBase(s string) (string, error) {
	if strings.Contains(s, "://") {
		u, err := url.Parse(s)
		if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
			return "", fmt.Errorf("%s=%q is not an https:// URL with a host (plain http is refused)", BaseEnv, s)
		}
		return strings.TrimRight(s, "/"), nil
	}
	if s == "" || !filepath.IsAbs(s) {
		return "", fmt.Errorf("%s=%q is not an absolute directory or an https:// URL", BaseEnv, s)
	}
	t := strings.TrimRight(s, `/\`)
	if t == "" || (len(t) == 2 && t[1] == ':') {
		return "", errors.New(BaseEnv + " names the root of a drive or of the file system, not an export")
	}
	return t, nil
}

// installRelease is the release a component's instructions install: the
// tested release of the line the found version belongs to, else of the
// first line. Empty when no release of the set's lines exists yet.
func (c supportedComponent) installRelease(found string) string {
	if v, _, ok := parseVersion(found); ok {
		for _, r := range c.Ranges {
			if m, _, _ := parseVersion(r.Min); m[0] == v[0] && r.Tested != "" {
				return r.Tested
			}
		}
	}
	return c.Ranges[0].Tested
}

// atLeast reports whether release rel is min or later.
func atLeast(rel, min string) bool {
	v, _, ok := parseVersion(rel)
	m, _, _ := parseVersion(min)
	return ok && !v.less(m)
}

// shQuote quotes s for a POSIX shell; psQuote for PowerShell.
func shQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }
func psQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", "''") + "'" }

// aimemInstruction installs aimem's tested release with its verifying boot
// script, taken at that release's tag. With base, an aiforge export, it
// installs from the export and names no other host.
func aimemInstruction(c supportedComponent, found, base string) string {
	need := fmt.Sprintf("aimem %s or later is required", c.minimum())
	rel := c.installRelease(found)
	if rel == "" {
		return need + ". No aimem release provides it yet: install aimem built from its main branch " +
			"(https://github.com/BlackVS/aimem, `./install.sh user` from a checkout; " +
			"`install.ps1` on Windows), or wait for the next aimem release; then rerun `aicrew-agent check`"
	}
	const rerun = "; then rerun `aicrew-agent check`"
	// The note describes the release installed, a tested one, never the
	// minimum.
	release := "v" + rel
	if c.Note != "" {
		release += " (" + c.Note + ")"
	}
	head := fmt.Sprintf("%s. Install aimem %s with its verifying installer", need, release)
	if base != "" && atLeast(rel, exportAimem) {
		return head + ", from the aiforge export " + base + ": " + aimemFromExport(base, rel) + rerun
	}
	closed := ". In a closed network, install it from your aiforge export instead, as the export's README.txt says, " +
		"and give `aicrew-agent join` the export in " + BaseEnv + " so that this check names it"
	if goos == "windows" {
		return fmt.Sprintf("%s, from an empty directory: `$env:AIMEM_VERSION='v%s'; Set-Location (New-Item -ItemType "+
			"Directory (Join-Path $env:TEMP ([guid]::NewGuid()))); powershell -NoProfile -ExecutionPolicy Bypass -Command "+
			"\"irm https://raw.githubusercontent.com/BlackVS/aimem/v%s/boot.ps1 | iex\"`%s%s",
			head, rel, rel, closed, rerun)
	}
	return fmt.Sprintf("%s, from an empty directory: `cd \"$(mktemp -d)\" && curl -fsSL "+
		"https://raw.githubusercontent.com/BlackVS/aimem/v%s/boot.sh | AIMEM_VERSION=v%s bash`%s%s",
		head, rel, rel, closed, rerun)
}

// aimemFromExport is aimem's own export form (its ADMIN-MANUAL, "Installing
// from an aiforge export"), from an empty directory: a directory export runs
// the script in place, an https one pipes it, and on Windows a directory or
// share is copied to the new directory first and the copy run.
func aimemFromExport(base, rel string) string {
	https := strings.HasPrefix(base, "https://")
	if goos == "windows" {
		env := fmt.Sprintf("$env:AIMEM_BASE=%s; $env:AIMEM_VERSION='v%s'; Set-Location (New-Item -ItemType Directory "+
			"(Join-Path $env:TEMP ([guid]::NewGuid()))); ", psQuote(base), rel)
		clear := "Remove-Item Env:AIMEM_BASE, Env:AIMEM_VERSION"
		if https {
			return fmt.Sprintf("`%spowershell -NoProfile -ExecutionPolicy Bypass -Command \"irm %s | iex\"; %s`",
				env, psQuote(base+"/aimem/v"+rel+"/boot.ps1"), clear)
		}
		return fmt.Sprintf("`%stry { Copy-Item -LiteralPath %s -Destination boot.ps1 -ErrorAction Stop; "+
			"powershell -NoProfile -ExecutionPolicy RemoteSigned -File .\\boot.ps1 } finally { %s }`",
			env, psQuote(base+`\aimem\v`+rel+`\boot.ps1`), clear)
	}
	vars := fmt.Sprintf("AIMEM_BASE=%s AIMEM_VERSION=v%s", shQuote(base), rel)
	script := shQuote(base + "/aimem/v" + rel + "/boot.sh")
	if https {
		return fmt.Sprintf("`cd \"$(mktemp -d)\" && curl -fsSL %s | %s bash`", script, vars)
	}
	return fmt.Sprintf("`cd \"$(mktemp -d)\" && %s bash %s`", vars, script)
}

// skillsInstruction gets the required skills the documented way: a rerun of
// join, which installs them from the aimem hub inside the team's
// conversation with the home's own aimem (joinskills.go). The hub serves the
// version the project's process pins, so the text names no ai-skills
// version to download and no URL, and the command is complete as printed.
func skillsInstruction(c supportedComponent, found string, skills []string, home string) string {
	need := ""
	if found != "" {
		need = fmt.Sprintf("ai-skills %s or later is required (found %s): ", c.minimumFor(found), found)
	}
	return fmt.Sprintf("%sinstall the skills %s from the aimem hub with `%s`: join installs them inside the team's "+
		"conversation, under the team's grant, with the home's aimem %s or later; the hub serves the versions "+
		"the process of the team's project pins, from the set the hub's administrator imported",
		need, strings.Join(skills, ", "), joinRerun(home), skillsTeamAimem)
}

// shellArg quotes s, when it needs it, for the shell a printed command is
// pasted into: PowerShell on Windows, a POSIX shell elsewhere.
func shellArg(s string) string {
	special := " \t'\"$`&;|()<>{}#*?[]~!"
	if goos != "windows" {
		// A POSIX shell drops an unquoted backslash; PowerShell keeps it,
		// and every Windows path holds one.
		special += "\\"
	}
	if !strings.ContainsAny(s, special) {
		return s
	}
	if goos == "windows" {
		return psQuote(s)
	}
	return shQuote(s)
}

// commandWord is an executable as the first word of a command the reader
// pastes: bare when it needs no quoting, else quoted, and on Windows called
// with &, since PowerShell reads a quoted path alone as a string.
func commandWord(exe string) string {
	if !strings.ContainsAny(exe, " \t'\"$`&;|()<>{}#") {
		return exe
	}
	if goos == "windows" {
		return "& " + psQuote(exe)
	}
	return shQuote(exe)
}

// clientInstruction names the npm package of the tested release.
func clientInstruction(name string, c supportedComponent, found string) string {
	rel := c.installRelease(found)
	pkg := "@anthropic-ai/claude-code"
	label := "Claude Code"
	if name == "opencode" {
		label = "OpenCode"
		pkg = "opencode-ai"
		if v, _, _ := parseVersion(rel); v[0] >= 2 {
			pkg = "@opencode/cli"
		}
	}
	return fmt.Sprintf("%s %s or later is required (tested: %s): install or upgrade it, for example "+
		"`npm install -g %s@%s`; then rerun `aicrew-agent check`", label, c.minimumFor(found), rel, pkg, rel)
}

// minimumFor is the minimum of the line a found version belongs to.
func (c supportedComponent) minimumFor(found string) string {
	if v, _, ok := parseVersion(found); ok {
		for _, r := range c.Ranges {
			if m, _, _ := parseVersion(r.Min); m[0] == v[0] {
				return r.Min
			}
		}
	}
	return c.minimum()
}
