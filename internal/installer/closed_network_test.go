package installer

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/pem"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
)

// The closed-network install (task 01a1cf0c-3471): each installer reads an
// aiforge export, format 1, from AICREW_BASE. <base>/MANIFEST is
// `aiforge-export 1` and then `<product> <tag>` lines; <base>/aicrew/<tag>/
// holds the release's assets under their published names with its
// SHA256SUMS. The export is the contract with aiskills' export script
// (docs/DESIGN-INSTANCE-BOUNDARY.md, D4).

// exportOf writes an export at a new directory whose MANIFEST names tag
// for aicrew (and another product, which a reader ignores), and whose
// aicrew/<tag>/ holds every file of rel; it returns the export's root.
func exportOf(t *testing.T, rel, tag string) string {
	t.Helper()
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "MANIFEST"), "aiforge-export 1\naimem v0.10.23\naicrew "+tag+"\n")
	dir := filepath.Join(root, "aicrew", tag)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(rel)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		b, err := os.ReadFile(filepath.Join(rel, e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, e.Name()), b, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// hubRelease is the hub installer's release directory: aicrewd and aicrew
// of version under their linux-amd64 asset names, and SHA256SUMS.
func hubRelease(t *testing.T, version string) string {
	t.Helper()
	built := release(t, version)
	dir := t.TempDir()
	var sums strings.Builder
	for _, name := range []string{"aicrewd", "aicrew"} {
		b, err := os.ReadFile(filepath.Join(built, name))
		if err != nil {
			t.Fatal(err)
		}
		asset := name + "-linux-amd64"
		if err := os.WriteFile(filepath.Join(dir, asset), b, 0o755); err != nil {
			t.Fatal(err)
		}
		h := sha256.Sum256(b)
		sums.WriteString(hex.EncodeToString(h[:]) + "  " + asset + "\n")
	}
	writeFile(t, filepath.Join(dir, "SHA256SUMS"), sums.String())
	return dir
}

// longerName rewrites dir's SHA256SUMS so that it lists asset only under a
// longer name (asset.old) with asset's own hash: an installer must match
// the whole name and so find no entry for it.
func longerName(t *testing.T, dir, asset string) {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, "SHA256SUMS"))
	if err != nil {
		t.Fatal(err)
	}
	out := strings.Replace(string(b), "  "+asset+"\n", "  "+asset+".old\n", 1)
	if out == string(b) {
		t.Fatalf("SHA256SUMS lists no %s", asset)
	}
	writeFile(t, filepath.Join(dir, "SHA256SUMS"), out)
}

// noNetwork is a directory holding a curl that records being run and
// fails: put first on PATH, it shows that an install contacted no host.
func noNetwork(t *testing.T) (dir, marker string) {
	t.Helper()
	dir = t.TempDir()
	marker = filepath.Join(dir, "curl-ran")
	writeFile(t, filepath.Join(dir, "curl"), "#!/bin/sh\necho \"$@\" >> '"+filepath.ToSlash(marker)+"'\nexit 7\n")
	if err := os.Chmod(filepath.Join(dir, "curl"), 0o755); err != nil {
		t.Fatal(err)
	}
	return dir, marker
}

// forgeURL is any forge address an installer could print.
var forgeURL = regexp.MustCompile(`github\.com|githubusercontent`)

// bootShRun runs the whole boot.sh, as `bash boot.sh` in place, with env.
func bootShRun(t *testing.T, home string, env ...string) (string, int) {
	t.Helper()
	boot, err := filepath.Abs(filepath.Join(repoRoot, "boot.sh"))
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("bash", boot)
	cmd.Dir = home
	cmd.Env = append(os.Environ(), "HOME="+home, "TMPDIR="+t.TempDir(), "AICREW_NO_SERVICE=1", serviceEnv(home))
	cmd.Env = append(cmd.Env, env...)
	return combined(t, cmd)
}

func posixShell(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("boot.sh runs on Linux and macOS")
	}
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("no bash")
	}
}

// boot.sh installs the release an export's MANIFEST names from a directory
// export, every file checked, with no host contacted and no forge URL
// printed. A missing or mismatching file, a missing MANIFEST, an unknown
// format or a MANIFEST without aicrew stop it with the path, and nothing
// is installed; so does a base that is neither absolute nor https, plain
// http among them.
func TestBootShInstallsFromAnExport(t *testing.T) {
	posixShell(t)
	rel := agentRelease(t, "v0.0.2", "")
	bad := agentRelease(t, "v0.0.2", "aicrew")
	for _, tc := range []struct {
		name  string
		setup func(root string)
		env   []string
		want  string // "" installs v0.0.2
	}{
		{"installs", nil, nil, ""},
		{"an explicit version wins", func(root string) {
			writeFile(t, filepath.Join(root, "MANIFEST"), "aiforge-export 1\naicrew v0.0.1\n")
		}, []string{"AICREW_VERSION=v0.0.2"}, ""},
		{"missing asset", func(root string) {
			os.Remove(filepath.Join(root, "aicrew", "v0.0.2", asset("aicrew")))
		}, nil, "/aicrew/v0.0.2/" + asset("aicrew") + " is missing: the export holds an incomplete copy of aicrew v0.0.2"},
		{"missing sums", func(root string) {
			os.Remove(filepath.Join(root, "aicrew", "v0.0.2", "SHA256SUMS"))
		}, nil, "/aicrew/v0.0.2/SHA256SUMS is missing: the export holds an incomplete copy"},
		{"mismatch", func(root string) {
			b, _ := os.ReadFile(filepath.Join(bad, "SHA256SUMS"))
			writeFile(t, filepath.Join(root, "aicrew", "v0.0.2", "SHA256SUMS"), string(b))
		}, nil, "checksum mismatch for "},
		{"listed only under a longer name", func(root string) {
			longerName(t, filepath.Join(root, "aicrew", "v0.0.2"), asset("aicrew"))
		}, nil, "/aicrew/v0.0.2/" + asset("aicrew") + " (want absent"},
		{"no manifest", func(root string) { os.Remove(filepath.Join(root, "MANIFEST")) }, nil,
			"/MANIFEST is missing (or set AICREW_VERSION to name the release)"},
		{"unknown format", func(root string) {
			writeFile(t, filepath.Join(root, "MANIFEST"), "aiforge-export 2\naicrew v0.0.2\n")
		}, nil, "is 'aiforge-export 2', not a format this installer reads (aiforge-export 1)"},
		{"no aicrew line", func(root string) {
			writeFile(t, filepath.Join(root, "MANIFEST"), "aiforge-export 1\naimem v0.10.23\n")
		}, nil, "/MANIFEST names no aicrew release"},
		{"plain http", nil, []string{"AICREW_BASE=http://files.example.invalid/aiforge"},
			"AICREW_BASE must be an absolute directory or an https:// URL, not 'http://files.example.invalid/aiforge'"},
		{"relative", nil, []string{"AICREW_BASE=aiforge"}, "AICREW_BASE must be an absolute directory"},
		{"with a prebuilt directory", nil, []string{"AICREW_PREBUILT_DIR=/nonexistent"},
			"AICREW_BASE and AICREW_PREBUILT_DIR exclude each other"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := exportOf(t, rel, "v0.0.2")
			if tc.setup != nil {
				tc.setup(root)
			}
			home, bin := t.TempDir(), t.TempDir()
			path, marker := noNetwork(t)
			env := append([]string{"AICREW_BASE=" + root, "AICREW_BIN_DIR=" + bin, "PATH=" + path + ":" + os.Getenv("PATH")}, tc.env...)
			out, code := bootShRun(t, home, env...)
			if _, err := os.Stat(marker); err == nil {
				t.Fatalf("the install ran curl, contacting a host:\n%s", out)
			}
			if forgeURL.MatchString(out) {
				t.Fatalf("the install printed a forge URL:\n%s", out)
			}
			if tc.want == "" {
				if code != 0 || agentVersion(t, filepath.Join(bin, "aicrew-agent")) != "v0.0.2" ||
					agentVersion(t, filepath.Join(bin, "aicrew")) != "v0.0.2" {
					t.Fatalf("exit %d:\n%s", code, out)
				}
				return
			}
			if code == 0 || !strings.Contains(out, tc.want) || len(files(t, bin)) != 0 || len(files(t, home)) != 0 {
				t.Fatalf("want a refusal %q with nothing installed (exit %d, bin %v):\n%s", tc.want, code, files(t, bin), out)
			}
		})
	}
}

// The hub installer reads the same export: resolve_source and
// fetch_release stage the release MANIFEST names, every file checked, with
// no host contacted, and the upgrade transaction installs it. A missing or
// mismatching file stops it with the path, staging nothing.
func TestInstallAicrewdFromAnExport(t *testing.T) {
	shellOnly(t)
	if runtime.GOARCH != "amd64" {
		t.Skip("the staged release is linux-amd64")
	}
	rel := hubRelease(t, "v0.0.2")
	stage := "set -euo pipefail\n" + extract(t, "release-source") + extract(t, "download") +
		"resolve_source || exit 1\nfetch_release \"$STAGED\"\n"
	path, marker := noNetwork(t)
	env := func(root, staged string) []string {
		return []string{"AICREW_BASE=" + root, "RELEASE=v9.9.9", "ARCH=amd64", "STAGED=" + staged,
			"PATH=" + path + ":" + os.Getenv("PATH")}
	}

	root := exportOf(t, rel, "v0.0.2")
	staged := t.TempDir()
	out, code := bash(t, stage, env(root, staged)...)
	if code != 0 || forgeURL.MatchString(out) || !strings.Contains(out, "checksum OK: aicrewd-linux-amd64") {
		t.Fatalf("staging from the export failed or printed a forge URL (exit %d):\n%s", code, out)
	}
	s := newSandbox(t, release(t, "v0.0.1"), false, "")
	if out, code := runUpgrade(t, s, staged); code != 0 || !strings.Contains(out, "health now: v0.0.2") {
		t.Fatalf("the upgrade to the export's release failed (exit %d):\n%s", code, out)
	}

	for name, damage := range map[string]func(dir string){
		"missing": func(dir string) { os.Remove(filepath.Join(dir, "aicrew-linux-amd64")) },
		"mismatch": func(dir string) {
			writeFile(t, filepath.Join(dir, "aicrew-linux-amd64"), "not the release's aicrew")
		},
		"longer name": func(dir string) { longerName(t, dir, "aicrew-linux-amd64") },
	} {
		t.Run(name, func(t *testing.T) {
			root := exportOf(t, rel, "v0.0.2")
			damage(filepath.Join(root, "aicrew", "v0.0.2"))
			staged := t.TempDir()
			out, code := bash(t, stage, env(root, staged)...)
			want := map[string]string{
				"missing":     "/aicrew/v0.0.2/aicrew-linux-amd64 is missing: the export holds an incomplete copy of aicrew v0.0.2",
				"mismatch":    "checksum mismatch for " + root + "/aicrew/v0.0.2/aicrew-linux-amd64",
				"longer name": "checksum mismatch for " + root + "/aicrew/v0.0.2/aicrew-linux-amd64 (want absent",
			}[name]
			if code == 0 || !strings.Contains(out, want) || len(files(t, staged)) > 1 {
				t.Fatalf("want %q with nothing staged but the sums (exit %d, staged %v):\n%s", want, code, files(t, staged), out)
			}
		})
	}
	for _, base := range []string{"http://files.example.invalid/aiforge", "aiforge"} {
		out, code := bash(t, stage, append(env(root, t.TempDir()), "AICREW_BASE="+base)...)
		if code == 0 || !strings.Contains(out, "AICREW_BASE must be an absolute directory or an https:// URL") {
			t.Errorf("base %q was not refused (exit %d):\n%s", base, code, out)
		}
	}
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("staging from a directory export ran curl")
	}
}

// httpsExport serves root over TLS, with two redirecting prefixes: /hop/X
// answers a relative redirect to /X, and /down/X a redirect to the same
// file on a plain http server, whose requests it counts. It returns the
// TLS server's URL, a CA bundle file for curl and the plain server's
// request count.
func httpsExport(t *testing.T, root string) (string, string, *atomic.Int32) {
	t.Helper()
	var plainHits atomic.Int32
	plain := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		plainHits.Add(1)
		http.FileServer(http.Dir(root)).ServeHTTP(w, r)
	}))
	t.Cleanup(plain.Close)
	files := http.FileServer(http.Dir(root))
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasPrefix(r.URL.Path, "/hop/"):
			http.Redirect(w, r, strings.TrimPrefix(r.URL.Path, "/hop"), http.StatusFound)
		case strings.HasPrefix(r.URL.Path, "/down/"):
			http.Redirect(w, r, plain.URL+strings.TrimPrefix(r.URL.Path, "/down"), http.StatusFound)
		default:
			files.ServeHTTP(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	ca := filepath.Join(t.TempDir(), "ca.pem")
	writeFile(t, ca, string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw})))
	return srv.URL, ca, &plainHits
}

// An https base is read like a directory, and a redirect is followed only
// to https: a relative https redirect installs, a redirect to plain http
// stops the install with nothing fetched over http and nothing installed.
func TestBootShHttpsExport(t *testing.T) {
	posixShell(t)
	if _, err := exec.LookPath("curl"); err != nil {
		t.Skip("no curl")
	}
	rel := agentRelease(t, "v0.0.2", "")
	url, ca, plainHits := httpsExport(t, exportOf(t, rel, "v0.0.2"))
	for _, tc := range []struct {
		name, base string
		ok         bool
	}{{"direct", url, true}, {"relative https redirect", url + "/hop", true}, {"redirect to http", url + "/down", false}} {
		t.Run(tc.name, func(t *testing.T) {
			home, bin := t.TempDir(), t.TempDir()
			before := plainHits.Load()
			out, code := bootShRun(t, home, "AICREW_BASE="+tc.base, "AICREW_BIN_DIR="+bin, "CURL_CA_BUNDLE="+ca)
			if forgeURL.MatchString(out) {
				t.Fatalf("the install printed a forge URL:\n%s", out)
			}
			if tc.ok {
				if code != 0 || agentVersion(t, filepath.Join(bin, "aicrew-agent")) != "v0.0.2" {
					t.Fatalf("exit %d:\n%s", code, out)
				}
				return
			}
			if code == 0 || plainHits.Load() != before || len(files(t, bin)) != 0 {
				t.Fatalf("a redirect to http was followed (exit %d, http requests %d, bin %v):\n%s",
					code, plainHits.Load()-before, files(t, bin), out)
			}
		})
	}
}

// The hub installer's downloads follow redirects only to https too.
func TestInstallAicrewdHttpsExport(t *testing.T) {
	shellOnly(t)
	rel := hubRelease(t, "v0.0.2")
	url, ca, plainHits := httpsExport(t, exportOf(t, rel, "v0.0.2"))
	stage := "set -euo pipefail\n" + extract(t, "release-source") + extract(t, "download") +
		"resolve_source || exit 1\nfetch_release \"$STAGED\"\n"
	for _, tc := range []struct {
		name, base string
		ok         bool
	}{{"direct", url, true}, {"relative https redirect", url + "/hop", true}, {"redirect to http", url + "/down", false}} {
		t.Run(tc.name, func(t *testing.T) {
			staged := t.TempDir()
			before := plainHits.Load()
			out, code := bash(t, stage, "AICREW_BASE="+tc.base, "RELEASE=v9.9.9", "ARCH=amd64", "STAGED="+staged,
				"CURL_CA_BUNDLE="+ca)
			_, errD := os.Stat(filepath.Join(staged, "aicrewd"))
			if tc.ok {
				if code != 0 || errD != nil {
					t.Fatalf("exit %d:\n%s", code, out)
				}
				return
			}
			if code == 0 || plainHits.Load() != before || errD == nil {
				t.Fatalf("a redirect to http was followed (exit %d, http requests %d):\n%s", code, plainHits.Load()-before, out)
			}
		})
	}
}

// Every https download of the shell installers, the connected one
// included, holds curl to https on each hop.
func TestShellDownloadsStayOnHttps(t *testing.T) {
	for _, f := range []string{"boot.sh", script} {
		text := read(t, f)
		curls := regexp.MustCompile(`(?m)^[^#\n]*\bcurl -fsSL\b[^\n]*$`).FindAllString(text, -1)
		if len(curls) != 1 || !strings.Contains(curls[0], "--proto =https --proto-redir =https") {
			t.Errorf("%s: want its one download through get_file with --proto =https --proto-redir =https, found %q", f, curls)
		}
	}
}

// The two shell installers carry the same release-source block.
func TestReleaseSourceBlocksAgree(t *testing.T) {
	if a, b := extractFrom(t, "boot.sh", "release-source"), extract(t, "release-source"); a != b {
		t.Fatal("the release-source blocks of boot.sh and install-aicrewd.sh differ")
	}
}

// A script piped to a shell runs nothing when its download is cut short:
// every prefix that ends at a line boundary, run under bash -x with an
// empty PATH, traces only `set` and literal assignments and prints nothing.
func TestPipedScriptsRunNothingWhenCutShort(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the shell installers run on Linux and macOS; a bash per prefix costs a minute on Windows")
	}
	sh, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("no bash")
	}
	allowed := regexp.MustCompile(`^\++ (set -euo pipefail|[A-Za-z_][A-Za-z0-9_]*=[A-Za-z0-9._-]*)$`)
	for _, f := range []string{"boot.sh", script} {
		lines := strings.SplitAfter(read(t, f), "\n")
		for k := 0; k < len(lines)-1; k++ {
			prefix := strings.Join(lines[:k], "")
			cmd := exec.Command(sh, "--noprofile", "--norc", "-x", "-c", prefix)
			cmd.Env = []string{"PATH="}
			var stdout, stderr strings.Builder
			cmd.Stdout, cmd.Stderr = &stdout, &stderr
			_ = cmd.Run()
			if stdout.Len() != 0 {
				t.Fatalf("%s cut after line %d printed:\n%s", f, k, stdout.String())
			}
			for _, l := range strings.Split(stderr.String(), "\n") {
				if strings.HasPrefix(l, "+") && !allowed.MatchString(strings.TrimRight(l, "\r")) {
					t.Fatalf("%s cut after line %d ran %q", f, k, l)
				}
			}
		}
	}
}

func powershell(t *testing.T) {
	t.Helper()
	if runtime.GOOS != "windows" {
		t.Skip("boot.ps1 runs on Windows")
	}
	if _, err := exec.LookPath("powershell.exe"); err != nil {
		t.Skip("no powershell.exe")
	}
}

// boot.ps1 piped to iex is parsed whole before it runs; the script is one
// script block closed on its last line, so a prefix cut at any line either
// does not parse or holds no statement, and the whole script is one.
func TestBootPs1RunsNothingWhenCutShort(t *testing.T) {
	powershell(t)
	path, err := filepath.Abs(filepath.Join(repoRoot, "boot.ps1"))
	if err != nil {
		t.Fatal(err)
	}
	check := `$lines = [IO.File]::ReadAllText($env:SCRIPT) -split "(?<=\n)"
$bad = @()
for ($k = 0; $k -lt $lines.Count; $k++) {
  $text = -join $lines[0..($k - 1)]
  if ($k -eq 0) { $text = '' }
  $e = $null
  $ast = [System.Management.Automation.Language.Parser]::ParseInput($text, [ref]$null, [ref]$e)
  $n = $ast.EndBlock.Statements.Count
  $complete = ($text.Trim() -eq ([IO.File]::ReadAllText($env:SCRIPT)).Trim())
  if ($complete) { if ($e -or $n -ne 1) { $bad += "whole: errors $($e.Count), statements $n" } }
  elseif (-not $e -and $n -gt 0) { $bad += "cut after line $k parses with $n statements" }
}
if ($bad) { $bad; exit 1 }
'whole script: one statement'`
	cmd := exec.Command("powershell.exe", "-NoProfile", "-NonInteractive", "-Command", check)
	cmd.Env = append(os.Environ(), "SCRIPT="+path)
	out, err := cmd.CombinedOutput()
	if err != nil || !strings.Contains(string(out), "whole script: one statement") {
		t.Fatalf("%v\n%s", err, out)
	}
}

// bootPs1Source runs boot.ps1's release-source and install-agent blocks as
// the script does: Resolve-Source, then Install-Agent into binDir. Its
// Invoke-WebRequest is a stub that serves https://export.test/root/X from
// root, answers https://export.test/hop/X with a relative redirect to
// /root/X and https://export.test/down/X with a redirect to the same file
// over http, and fails, recording the URL, for any other request.
func bootPs1Source(t *testing.T, root, binDir string, env ...string) (string, int) {
	t.Helper()
	stub := `function Invoke-WebRequest {
  [CmdletBinding()]
  param($Uri, $OutFile, [switch]$PassThru, [switch]$UseBasicParsing, $MaximumRedirection, [switch]$SkipHttpErrorCheck)
  Add-Content -LiteralPath $env:REQUESTS -Value $Uri
  if ($Uri -like 'https://export.test/root/*') {
    $p = Join-Path $env:EXPORT ($Uri.Substring('https://export.test/root/'.Length))
    if (-not (Test-Path -LiteralPath $p -PathType Leaf)) { throw "404 $Uri" }
    Copy-Item -LiteralPath $p -Destination $OutFile
    return [pscustomobject]@{ StatusCode = 200; Headers = @{} }
  }
  if ($Uri -like 'https://export.test/hop/*') {
    return [pscustomobject]@{ StatusCode = 302; Headers = @{ Location = '/root/' + $Uri.Substring('https://export.test/hop/'.Length) } }
  }
  if ($Uri -like 'https://export.test/down/*') {
    return [pscustomobject]@{ StatusCode = 302; Headers = @{ Location = 'http://export.test/root/' + $Uri.Substring('https://export.test/down/'.Length) } }
  }
  throw "network used: $Uri"
}
`
	script := filepath.Join(t.TempDir(), "run.ps1")
	text := "$ErrorActionPreference = 'Stop'\n" + stub + extractFrom(t, "boot.ps1", "release-source") +
		extractFrom(t, "boot.ps1", "install-agent") + fmt.Sprintf(`$work = New-Item -ItemType Directory (Join-Path $env:TEMP ('w' + [Guid]::NewGuid().ToString('N')))
$source = Resolve-Source $work.FullName 'v9.9.9'
if ($env:AICREW_BASE) { Write-Host "installing aicrew $($source.Tag) from $($source.Src)" }
Install-Agent $source.Tag $source.Src '%s'
`, binDir)
	if err := os.WriteFile(script, []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("powershell.exe", "-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass", "-File", script)
	cmd.Dir = t.TempDir()
	tmp := t.TempDir()
	cmd.Env = append(os.Environ(), "TEMP="+tmp, "TMP="+tmp, "EXPORT="+root, "AICREW_NO_SERVICE=1", serviceEnv(t.TempDir()))
	cmd.Env = append(cmd.Env, env...)
	return combined(t, cmd)
}

// boot.ps1 installs from a directory export, every file checked, with no
// download and no forge URL printed, and refuses an incomplete or wrong
// export, a base that is neither absolute nor https, and plain http. An
// https base follows a relative https redirect and refuses a redirect to
// http with nothing requested over http and nothing installed.
func TestBootPs1InstallsFromAnExport(t *testing.T) {
	powershell(t)
	rel := agentRelease(t, "v0.0.2", "")
	bad := agentRelease(t, "v0.0.2", "aicrew")
	for _, tc := range []struct {
		name  string
		setup func(root string)
		base  func(root string) string
		env   []string
		want  string
	}{
		{"directory", nil, nil, nil, ""},
		{"an explicit version wins", func(root string) {
			writeFile(t, filepath.Join(root, "MANIFEST"), "aiforge-export 1\naicrew v0.0.1\n")
		}, nil, []string{"AICREW_VERSION=v0.0.2"}, ""},
		{"https", nil, func(string) string { return "https://export.test/root" }, nil, ""},
		{"relative https redirect", nil, func(string) string { return "https://export.test/hop" }, nil, ""},
		{"redirect to http", nil, func(string) string { return "https://export.test/down" }, nil,
			"refusing a redirect to http://export.test/root/MANIFEST, which is not https"},
		{"missing asset", func(root string) {
			os.Remove(filepath.Join(root, "aicrew", "v0.0.2", "aicrew-windows-amd64.exe"))
		}, nil, nil, "aicrew-windows-amd64.exe is missing: the export holds an incomplete copy of aicrew v0.0.2"},
		{"mismatch", func(root string) {
			b, _ := os.ReadFile(filepath.Join(bad, "SHA256SUMS"))
			writeFile(t, filepath.Join(root, "aicrew", "v0.0.2", "SHA256SUMS"), string(b))
		}, nil, nil, "checksum mismatch for "},
		{"listed only under a longer name", func(root string) {
			longerName(t, filepath.Join(root, "aicrew", "v0.0.2"), "aicrew-windows-amd64.exe")
		}, nil, nil, "aicrew-windows-amd64.exe (want absent"},
		{"no manifest", func(root string) { os.Remove(filepath.Join(root, "MANIFEST")) }, nil, nil,
			"MANIFEST is missing (or set AICREW_VERSION to name the release)"},
		{"unknown format", func(root string) {
			writeFile(t, filepath.Join(root, "MANIFEST"), "aiforge-export 2\naicrew v0.0.2\n")
		}, nil, nil, "not a format this installer reads (aiforge-export 1)"},
		{"plain http", nil, func(string) string { return "http://export.test/root" }, nil,
			"AICREW_BASE must be an absolute directory, a \\\\host\\share path or an https:// URL"},
		{"relative", nil, func(string) string { return "aiforge" }, nil,
			"AICREW_BASE must be an absolute directory, a \\\\host\\share path or an https:// URL"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := exportOf(t, rel, "v0.0.2")
			if tc.setup != nil {
				tc.setup(root)
			}
			base := root
			if tc.base != nil {
				base = tc.base(root)
			}
			bin := t.TempDir()
			requests := filepath.Join(t.TempDir(), "requests.txt")
			out, code := bootPs1Source(t, root, bin, append([]string{"AICREW_BASE=" + base, "REQUESTS=" + requests}, tc.env...)...)
			flat := unwrapped(out)
			reqs, _ := os.ReadFile(requests)
			if strings.Contains(string(reqs), "http://") || forgeURL.MatchString(flat) || strings.Contains(flat, "network used") {
				t.Fatalf("a request left the export, went over http, or a forge URL was printed (requests %q):\n%s", reqs, out)
			}
			if tc.base == nil && len(reqs) != 0 {
				t.Fatalf("a directory export made requests %q", reqs)
			}
			if tc.want == "" {
				if code != 0 || agentVersion(t, filepath.Join(bin, "aicrew-agent.exe")) != "v0.0.2" ||
					agentVersion(t, filepath.Join(bin, "aicrew.exe")) != "v0.0.2" {
					t.Fatalf("exit %d:\n%s", code, out)
				}
				return
			}
			if code == 0 || !strings.Contains(flat, tc.want) || len(files(t, bin)) != 0 {
				t.Fatalf("want a refusal %q with nothing installed (exit %d, bin %v):\n%s", tc.want, code, files(t, bin), out)
			}
		})
	}
}

// windowsExportForms are the documented Windows forms that install from a
// share: each fenced or commented block that copies boot.ps1 out of
// $env:AICREW_BASE, from README.md, docs/DEVELOPMENT.md and boot.ps1's
// header.
func windowsExportForms(t *testing.T) map[string]string {
	t.Helper()
	forms := map[string]string{}
	block := regexp.MustCompile("(?s)```powershell\n(.*?)```")
	for _, f := range []string{"README.md", filepath.Join("docs", "DEVELOPMENT.md")} {
		for _, m := range block.FindAllStringSubmatch(read(t, f), -1) {
			if strings.Contains(m[1], "Copy-Item -LiteralPath") {
				forms[f] = m[1]
			}
		}
	}
	var header []string
	for _, l := range strings.Split(strings.ReplaceAll(read(t, "boot.ps1"), "\r\n", "\n"), "\n") {
		if strings.HasPrefix(l, "#   $env:AICREW_BASE = '\\\\") {
			header = []string{}
		}
		if header != nil {
			if !strings.HasPrefix(l, "#   ") {
				break
			}
			header = append(header, strings.TrimPrefix(l, "#   "))
		}
	}
	forms["boot.ps1"] = strings.Join(header, "\n")
	if len(forms) != 3 {
		t.Fatalf("want the Windows export form in README.md, docs/DEVELOPMENT.md and boot.ps1, found %v", forms)
	}
	return forms
}

// Each documented Windows export form runs boot.ps1 from a local copy,
// with AICREW_BASE set to the share, and leaves neither the variable nor
// the copy behind. A failed copy never runs a copy an earlier run left in
// TEMP. The export's boot.ps1 here is a stand-in that records what it saw.
func TestDocumentedWindowsExportFormRuns(t *testing.T) {
	powershell(t)
	for doc, form := range windowsExportForms(t) {
		t.Run(doc, func(t *testing.T) {
			for _, ok := range []bool{true, false} {
				root, tmp := t.TempDir(), t.TempDir()
				seen := filepath.Join(t.TempDir(), "seen.txt")
				dir := filepath.Join(root, "aicrew", "v0.0.2")
				if err := os.MkdirAll(dir, 0o755); err != nil {
					t.Fatal(err)
				}
				if ok {
					writeFile(t, filepath.Join(dir, "boot.ps1"),
						"Set-Content -LiteralPath $env:SEEN -Value (\"base=\" + $env:AICREW_BASE + \" from=\" + $PSCommandPath)\n")
				}
				// What an interrupted earlier run left behind.
				writeFile(t, filepath.Join(tmp, "aicrew-boot.ps1"), "Set-Content -LiteralPath $env:SEEN -Value 'stale copy ran'\n")
				run := strings.NewReplacer(`'\\files\aiforge-export'`, "'"+root+"'", "<tag>", "v0.0.2").Replace(form)
				script := filepath.Join(t.TempDir(), "form.ps1")
				writeFile(t, script, "try {\n"+run+"\n} catch { Write-Output \"failed: $_\" }\n"+
					"Write-Output (\"variable left: \" + (Test-Path Env:AICREW_BASE))\n"+
					"Write-Output (\"copy left: \" + (Test-Path (Join-Path $env:TEMP 'aicrew-boot.ps1')))\n")
				cmd := exec.Command("powershell.exe", "-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass", "-File", script)
				cmd.Env = append(os.Environ(), "TEMP="+tmp, "TMP="+tmp, "SEEN="+seen)
				out, _ := combined(t, cmd)
				got, _ := os.ReadFile(seen)
				if !strings.Contains(out, "variable left: False") || !strings.Contains(out, "copy left: False") {
					t.Fatalf("ok=%v: the form left the variable or the copy behind:\n%s", ok, out)
				}
				if ok && !strings.Contains(string(got), "base="+root+" from="+filepath.Join(tmp, "aicrew-boot.ps1")) {
					t.Fatalf("boot.ps1 did not run from the local copy with AICREW_BASE (saw %q):\n%s", got, out)
				}
				if !ok && (len(got) != 0 || !strings.Contains(out, "failed:")) {
					t.Fatalf("a failed copy ran %q or did not fail:\n%s", got, out)
				}
			}
		})
	}
}
