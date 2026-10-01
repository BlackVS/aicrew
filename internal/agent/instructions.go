package agent

import (
	"fmt"
	"runtime"
	"strings"
)

// The exact steps a blocked check reports (5a installs nothing). Each pins
// the version it names and installs only from a verified distribution:
// aimem's boot script checks its release asset against SHA256SUMS, and the
// ai-skills steps check the release archive before installing from it (its
// one-line boot verifies nothing).

// goos is the platform the instructions are written for; tests replace it.
var goos = runtime.GOOS

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

func aimemInstruction(c supportedComponent, found string) string {
	need := fmt.Sprintf("aimem %s or later is required", c.minimum())
	if c.Note != "" {
		need += " (" + c.Note + ")"
	}
	rel := c.installRelease(found)
	if rel == "" {
		return need + ". No aimem release provides it yet: install aimem built from its main branch " +
			"(https://github.com/BlackVS/aimem, `./install.sh user` from a checkout; " +
			"`install.ps1` on Windows), or wait for the next aimem release; then rerun `aicrew-agent check`"
	}
	if goos == "windows" {
		return fmt.Sprintf("%s. Install aimem v%s with its verifying installer, from an empty directory: "+
			"`$env:AIMEM_VERSION='v%s'; Set-Location (New-Item -ItemType Directory (Join-Path $env:TEMP "+
			"([guid]::NewGuid()))); powershell -NoProfile -ExecutionPolicy Bypass -Command \"irm "+
			"https://raw.githubusercontent.com/BlackVS/aimem/master/boot.ps1 | iex\"`; then rerun `aicrew-agent check`",
			need, rel, rel)
	}
	return fmt.Sprintf("%s. Install aimem v%s with its verifying installer, from an empty directory: "+
		"`cd \"$(mktemp -d)\" && curl -fsSL https://raw.githubusercontent.com/BlackVS/aimem/master/boot.sh | "+
		"AIMEM_VERSION=v%s bash`; then rerun `aicrew-agent check`", need, rel, rel)
}

// skillsInstruction installs the required skills at user level for Claude
// Code, where OpenCode reads them too, from the verified release archive.
func skillsInstruction(c supportedComponent, found string, skills []string) string {
	rel := c.installRelease(found)
	list := strings.Join(skills, ",")
	base := "https://github.com/BlackVS/aiskills/releases/download/v" + rel
	if goos == "windows" {
		return fmt.Sprintf("install ai-skills %s (%s) from its release archive, checked against SHA256SUMS: "+
			"`Set-Location (New-Item -ItemType Directory (Join-Path $env:TEMP ([guid]::NewGuid()))); "+
			"irm %s/aiskills-%s.zip -OutFile a.zip; irm %s/SHA256SUMS -OutFile SHA256SUMS; "+
			"$h = (Get-FileHash a.zip -Algorithm SHA256).Hash.ToLower(); "+
			"if (-not (Select-String -Path SHA256SUMS -Pattern \"^$h\\s+aiskills-%s.zip$\" -Quiet)) { throw 'checksum mismatch' }; "+
			"Expand-Archive a.zip src; & \"$((Get-ChildItem src -Directory)[0].FullName)\\install.ps1\" -User -Tool claude -Skills %s`; "+
			"then rerun `aicrew-agent check`", rel, list, base, rel, base, rel, list)
	}
	sum := "sha256sum --ignore-missing -c SHA256SUMS"
	if goos == "darwin" {
		sum = "shasum -a 256 --ignore-missing -c SHA256SUMS"
	}
	return fmt.Sprintf("install ai-skills %s (%s) from its release archive, checked against SHA256SUMS: "+
		"`cd \"$(mktemp -d)\" && curl -fsSLO %s/aiskills-%s.tar.gz && curl -fsSLO %s/SHA256SUMS && %s && "+
		"mkdir src && tar -xzf aiskills-%s.tar.gz -C src --strip-components=1 && ./src/install.sh --user -t claude -s %s`; "+
		"then rerun `aicrew-agent check`", rel, list, base, rel, base, sum, rel, list)
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
