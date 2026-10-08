package installer

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/BlackVS/aicrew/internal/optoken"
	"github.com/BlackVS/aicrew/internal/privatefile"
)

const (
	repoRoot = "../.."
	script   = "install-aicrewd.sh"
)

func read(t *testing.T, rel string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(repoRoot, rel))
	if err != nil {
		t.Fatal(err)
	}
	return strings.ReplaceAll(string(b), "\r\n", "\n")
}

// newestRelease is the newest released version in the CHANGELOG, as a tag.
func newestRelease(t *testing.T) string {
	t.Helper()
	m := regexp.MustCompile(`(?m)^## \[(\d+\.\d+\.\d+)\]`).FindStringSubmatch(read(t, "CHANGELOG.md"))
	if m == nil {
		t.Fatal("CHANGELOG.md has no released version heading")
	}
	return "v" + m[1]
}

// The installer installs the release named in its RELEASE line, and that is
// the newest release in the CHANGELOG: rolling the CHANGELOG into a version
// without bumping the pin fails here (and the release workflow refuses the
// tag).
func TestScriptPinsTheNewestRelease(t *testing.T) {
	m := regexp.MustCompile(`(?m)^RELEASE=(\S+)$`).FindAllStringSubmatch(read(t, script), -1)
	if len(m) != 1 {
		t.Fatalf("%s: want exactly one release pin, found %d", script, len(m))
	}
	if want := newestRelease(t); m[0][1] != want {
		t.Fatalf("%s pins %s; the newest CHANGELOG release is %s", script, m[0][1], want)
	}
}

// The documented one-liners fetch the installer from the pinned release
// tag, never from a branch, so the script and the binaries it installs come
// from the same release.
func TestOneLinersNameTheReleaseTag(t *testing.T) {
	want := newestRelease(t)
	files := []string{"README.md", script}
	docs, err := filepath.Glob(filepath.Join(repoRoot, "docs", "*.md"))
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range docs {
		rel, _ := filepath.Rel(repoRoot, d)
		files = append(files, rel)
	}
	url := regexp.MustCompile(`raw\.githubusercontent\.com/BlackVS/aicrew/([^/\s]+)/install-aicrewd\.sh`)
	seen := map[string]bool{}
	for _, f := range files {
		if _, err := os.Stat(filepath.Join(repoRoot, f)); err != nil {
			continue
		}
		for _, m := range url.FindAllStringSubmatch(read(t, f), -1) {
			seen[f] = true
			if m[1] != want {
				t.Errorf("%s fetches install-aicrewd.sh from %q; want the release tag %s", f, m[1], want)
			}
		}
	}
	if !seen[script] || !seen[filepath.Join("docs", "DEVELOPMENT.md")] {
		t.Fatalf("the one-liner is missing from %s or docs/DEVELOPMENT.md (found in %v)", script, seen)
	}
}

// The release workflow's check refuses a tag the installer does not pin
// (scripts/release_test.sh runs the refusal).
func TestReleaseCheckRefusesAnotherPin(t *testing.T) {
	if !strings.Contains(read(t, "scripts/release.sh"), `grep -qx "RELEASE=$TAG" install-aicrewd.sh`) {
		t.Fatal("scripts/release.sh does not check that install-aicrewd.sh pins the tag")
	}
}

// extract returns the installer's text between the BEGIN and END markers.
func extract(t *testing.T, name string) string {
	t.Helper()
	s := read(t, script)
	begin, end := "# BEGIN "+name+"\n", "# END "+name+"\n"
	i, j := strings.Index(s, begin), strings.Index(s, end)
	if i < 0 || j < i {
		t.Fatalf("%s has no %s between markers", script, name)
	}
	return s[i : j+len(end)]
}

func shellOnly(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("install-aicrewd.sh runs on Linux; its tests run where bash does")
	}
	for _, tool := range []string{"bash", "curl"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skip("no " + tool)
		}
	}
}

// bash runs text in bash with env added, and returns its output and exit
// status.
func bash(t *testing.T, text string, env ...string) (string, int) {
	t.Helper()
	cmd := exec.Command("bash", "-c", text)
	cmd.Env = append(os.Environ(), env...)
	out, err := cmd.CombinedOutput()
	if ee, ok := err.(*exec.ExitError); ok {
		return string(out), ee.ExitCode()
	} else if err != nil {
		t.Fatal(err)
	}
	return string(out), 0
}

// The whole script parses.
func TestScriptParses(t *testing.T) {
	shellOnly(t)
	if out, code := bash(t, "bash -n "+filepath.Join(repoRoot, script)); code != 0 {
		t.Fatalf("bash -n: %s", out)
	}
}

// A binary is installed only when the release's SHA256SUMS lists its hash:
// a different hash, or none, is refused and nothing is left behind.
func TestDownloadRefusesAnUnlistedHash(t *testing.T) {
	shellOnly(t)
	if _, err := exec.LookPath("sha256sum"); err != nil {
		t.Skip("no sha256sum")
	}
	sum := func(b string) string { h := sha256.Sum256([]byte(b)); return hex.EncodeToString(h[:]) }
	for _, tc := range []struct {
		name, sums string
		ok         bool
	}{
		{"listed", sum("d") + "  aicrewd-linux-amd64\n" + sum("c") + "  aicrew-linux-amd64\n", true},
		{"different hash", sum("d") + "  aicrewd-linux-amd64\n" + sum("x") + "  aicrew-linux-amd64\n", false},
		{"absent", sum("d") + "  aicrewd-linux-amd64\n", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rel, dst := t.TempDir(), t.TempDir()
			for name, body := range map[string]string{"aicrewd-linux-amd64": "d", "aicrew-linux-amd64": "c", "SHA256SUMS": tc.sums} {
				if err := os.WriteFile(filepath.Join(rel, name), []byte(body), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			out, code := bash(t, "set -euo pipefail\n"+extract(t, "download")+`fetch_release "$DST"`,
				"DL_BASE=file://"+filepath.ToSlash(rel), "ARCH=amd64", "DST="+dst)
			_, errD := os.Stat(filepath.Join(dst, "aicrewd"))
			_, errC := os.Stat(filepath.Join(dst, "aicrew"))
			if tc.ok {
				if code != 0 || errD != nil || errC != nil {
					t.Fatalf("exit %d:\n%s", code, out)
				}
				return
			}
			if code == 0 || !strings.Contains(out, "checksum mismatch for aicrew-linux-amd64") || errD == nil || errC == nil {
				t.Fatalf("an unlisted hash was not refused (exit %d):\n%s", code, out)
			}
		})
	}
}

var (
	binDir  string
	binOnce sync.Map // version -> *sync.Once
	binErr  sync.Map // version -> error
)

func TestMain(m *testing.M) {
	d, err := os.MkdirTemp("", "aicrew-installer-bin")
	if err != nil {
		panic(err)
	}
	binDir = d
	code := m.Run()
	os.RemoveAll(d)
	os.Exit(code)
}

// release builds, once per test run, aicrewd and aicrew stamped with
// version into a directory of their own, as AICREW_PREBUILT_DIR takes them.
func release(t *testing.T, version string) string {
	t.Helper()
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("no go toolchain to build the binaries under test")
	}
	dir := filepath.Join(binDir, version)
	once, _ := binOnce.LoadOrStore(version, &sync.Once{})
	once.(*sync.Once).Do(func() {
		for _, name := range []string{"aicrewd", "aicrew"} {
			cmd := exec.Command("go", "build", "-o", filepath.Join(dir, name),
				"-ldflags", "-X github.com/BlackVS/aicrew/internal/version.Override="+version, "./cmd/"+name)
			cmd.Dir = repoRoot
			cmd.Env = append(os.Environ(), "CGO_ENABLED=0")
			if out, err := cmd.CombinedOutput(); err != nil {
				binErr.Store(version, fmt.Errorf("build %s: %v\n%s", name, err, out))
				return
			}
		}
	})
	if err, ok := binErr.Load(version); ok {
		t.Fatal(err)
	}
	return dir
}

// broken is a release whose aicrewd answers -version and config like real
// one's, but exits at once instead of serving: a release that does not
// come up.
func broken(t *testing.T, real string) string {
	t.Helper()
	dir := t.TempDir()
	stub := "#!/bin/sh\ncase \"$1\" in -version|config) exec " + filepath.Join(real, "aicrewd") + " \"$@\" ;; esac\nexit 1\n"
	if err := os.WriteFile(filepath.Join(dir, "aicrewd"), []byte(stub), 0o755); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(real, "aicrew"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "aicrew"), b, 0o755); err != nil {
		t.Fatal(err)
	}
	return dir
}

// sandbox is an installation under ~/aicrew as the installer lays it out,
// in a temporary home.
type sandbox struct {
	dir, root, config, store, listen string
}

const testHubID = "01a119a5-8bb1-7000-a006-02f786cc1ee5"

// newSandbox installs the release in rel with a configuration on a free
// port; with legacy set, the configuration has the single aimem block of
// 0.2.0, its token files in credDir.
func newSandbox(t *testing.T, rel string, legacy bool, credDir string) *sandbox {
	t.Helper()
	dir := t.TempDir()
	s := &sandbox{dir: dir, root: filepath.Join(dir, "aicrew")}
	for _, d := range []string{"bin", "etc", "lib"} {
		if err := os.MkdirAll(filepath.Join(s.root, d), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	for _, name := range []string{"aicrewd", "aicrew"} {
		b, err := os.ReadFile(filepath.Join(rel, name))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(s.root, "bin", name), b, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	certFile, keyFile := writeCert(t, filepath.Join(s.root, "etc"))
	opFile := filepath.Join(s.root, "etc", "operator.token")
	tok, err := optoken.Generate()
	if err != nil {
		t.Fatal(err)
	}
	if err := optoken.Write(opFile, tok); err != nil {
		t.Fatal(err)
	}
	s.store = filepath.Join(s.root, "lib", "aicrew.db")
	s.listen = "127.0.0.1:" + strconv.Itoa(freePort(t))
	cfg := map[string]any{"store_path": s.store, "listen_addr": s.listen, "tls_cert_file": certFile,
		"tls_key_file": keyFile, "service_id": "aicrew-test", "shutdown_timeout": "5s", "operator_token_file": opFile}
	if legacy {
		cfg["aimem"] = map[string]string{"base_url": "https://localhost:1", "tls_trust_mode": "ca_dns",
			"tls_trust_value": "localhost", "redemption_token_file": filepath.Join(credDir, "aimem-redeem.token"),
			"read_token_file": filepath.Join(credDir, "aimem-read.token")}
	}
	raw, _ := json.MarshalIndent(cfg, "", "  ")
	s.config = filepath.Join(s.root, "etc", "aicrewd.json")
	if err := os.WriteFile(s.config, append(raw, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
	return s
}

func writeCert(t *testing.T, dir string) (string, string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "aicrewd test"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		IPAddresses: []net.IP{net.ParseIP("127.0.0.1")}, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		KeyUsage: x509.KeyUsageDigitalSignature}
	der, err := x509.CreateCertificate(rand.Reader, tpl, tpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, _ := x509.MarshalECPrivateKey(key)
	certFile, keyFile := filepath.Join(dir, "cert.pem"), filepath.Join(dir, "key.pem")
	if err := os.WriteFile(certFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyFile, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}), 0o600); err != nil {
		t.Fatal(err)
	}
	return certFile, keyFile
}

func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

// credDir writes what aimem identity peer provision writes: the hub ID and
// four private credential files, each different.
func credDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	writePrivate(t, filepath.Join(dir, "aimem-hub-id"), testHubID+"\n")
	for i, name := range []string{"aimem-redeem.token", "aimem-read.token", "aimem-team-register.token", "aimem-team-read.token"} {
		writePrivate(t, filepath.Join(dir, name), "aimem_peer_"+strings.Repeat(string("0123456789abcdef"[i+1]), 64)+"\n")
	}
	return dir
}

func writePrivate(t *testing.T, path, content string) {
	t.Helper()
	os.Remove(path)
	f, err := privatefile.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(content); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
}

// runUpgrade starts the installed release (by its PID, in place of the
// systemd unit), waits for its health, then runs the installer's preflight
// and upgrade transaction to the release in staged, and prints the version
// health answers at the end.
func runUpgrade(t *testing.T, s *sandbox, staged string, env ...string) (string, int) {
	t.Helper()
	text := "set -euo pipefail\n" + extract(t, "upgrade-transaction") + `
SVC_PID=
svc_stop() {
  if [ -n "$SVC_PID" ]; then kill "$SVC_PID" 2>/dev/null || true; wait "$SVC_PID" 2>/dev/null || true; SVC_PID=; fi
}
svc_start() { "$AICREWD_BIN" -config "$CONFIG" >>"$SANDBOX/serve.log" 2>&1 & SVC_PID=$!; }
crew_as() { "$@"; }
trap svc_stop EXIT
TAG=test
AICREWD_BIN=$ROOT/bin/aicrewd
AICREW_BIN=$ROOT/bin/aicrew
CONFIG=$ROOT/etc/aicrewd.json
svc_start
AICREW_UPGRADE_WAIT=20 txn_wait "$LISTEN" any || { echo "the installed release did not come up"; cat "$SANDBOX/serve.log"; exit 99; }
preflight_upgrade "$STAGED/aicrewd" || exit 1
rc=0
upgrade_txn "$STAGED" || rc=$?
echo "health now: $(health_version "$LISTEN")"
exit "$rc"
`
	out, code := bash(t, text, append([]string{"SANDBOX=" + s.dir, "ROOT=" + s.root, "LISTEN=" + s.listen,
		"STAGED=" + staged, "AICREW_UPGRADE_WAIT=15"}, env...)...)
	if code == 99 {
		t.Fatalf("setup failed:\n%s", out)
	}
	return out, code
}

// backups lists the files in dir whose names start with base and contain
// tag.
func backups(t *testing.T, dir, base, tag string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), base) && strings.Contains(e.Name(), tag) {
			names = append(names, e.Name())
		}
	}
	return names
}

func versionOf(t *testing.T, bin string) string {
	t.Helper()
	out, err := exec.Command(bin, "-version").Output()
	if err != nil {
		t.Fatalf("%s -version: %v", bin, err)
	}
	return strings.Fields(string(out))[1]
}

// An upgrade copies the store and the configuration beside themselves,
// keeps the previous binaries beside the new ones, and the service answers
// health at the new version.
func TestUpgrade(t *testing.T) {
	shellOnly(t)
	s := newSandbox(t, release(t, "v0.0.1"), false, "")
	out, code := runUpgrade(t, s, release(t, "v0.0.2"))
	if code != 0 || !strings.Contains(out, "upgraded aicrewd v0.0.1 -> v0.0.2") || !strings.Contains(out, "health now: v0.0.2") {
		t.Fatalf("exit %d:\n%s", code, out)
	}
	bin := filepath.Join(s.root, "bin")
	if versionOf(t, filepath.Join(bin, "aicrewd")) != "v0.0.2" || versionOf(t, filepath.Join(bin, "aicrewd.prev")) != "v0.0.1" {
		t.Fatal("the new aicrewd is not in place, or the previous one not kept")
	}
	if _, err := os.Stat(filepath.Join(bin, "aicrew.prev")); err != nil {
		t.Fatal("the previous aicrew is not kept")
	}
	if len(backups(t, filepath.Join(s.root, "etc"), "aicrewd.json.", "backup-")) != 1 ||
		len(backups(t, filepath.Join(s.root, "lib"), "aicrew.db.backup-", "")) == 0 {
		t.Fatalf("copies missing:\n%s", out)
	}
}

// A release that does not come up is rolled back: the previous binaries,
// the configuration copy and the store copy go back, and the previous
// release answers health again.
func TestUpgradeRollsBack(t *testing.T) {
	shellOnly(t)
	old := release(t, "v0.0.1")
	s := newSandbox(t, old, false, "")
	before, err := os.ReadFile(s.config)
	if err != nil {
		t.Fatal(err)
	}
	out, code := runUpgrade(t, s, broken(t, release(t, "v0.0.2")), "AICREW_UPGRADE_WAIT=3")
	if code == 0 || !strings.Contains(out, "rolling back") || !strings.Contains(out, "ROLLED BACK: aicrewd v0.0.1 is running again") ||
		!strings.Contains(out, "health now: unknown") && !strings.Contains(out, "health now: v0.0.1") {
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

// A 0.2.0 configuration is refused before anything changes unless
// AICREW_CRED_DIR names a directory it migrates from completely; with one,
// the upgrade migrates it and the new release answers health.
func TestUpgradeLegacyConfig(t *testing.T) {
	shellOnly(t)
	old, next := release(t, "v0.0.1"), release(t, "v0.0.2")
	good := credDir(t)
	bad := credDir(t)
	writePrivate(t, filepath.Join(bad, "aimem-hub-id"), "not-a-hub-id\n")
	for _, tc := range []struct {
		name, dir, want string
	}{
		{"no directory", "", "AICREW_CRED_DIR is not set"},
		{"bad directory", bad, "would not complete"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newSandbox(t, old, true, good)
			before := snapshot(t, s.root)
			out, code := runUpgrade(t, s, next, "AICREW_CRED_DIR="+tc.dir)
			if code == 0 || !strings.Contains(out, tc.want) || !strings.Contains(out, "Nothing was changed") ||
				!strings.Contains(out, "aimem identity peer provision aicrew-test") || !strings.Contains(out, "AICREW_CRED_DIR=DIR") {
				t.Fatalf("exit %d:\n%s", code, out)
			}
			if after := snapshot(t, s.root); !equalSnapshots(before, after) {
				t.Fatalf("files changed:\nbefore %v\nafter  %v", before, after)
			}
		})
	}
	t.Run("provisioned directory", func(t *testing.T) {
		s := newSandbox(t, old, true, good)
		out, code := runUpgrade(t, s, next, "AICREW_CRED_DIR="+good)
		if code != 0 || !strings.Contains(out, "the aimem block is now an aimem_hubs entry") ||
			strings.Contains(out, "Still to supply") || !strings.Contains(out, "health now: v0.0.2") {
			t.Fatalf("exit %d:\n%s", code, out)
		}
		raw, _ := os.ReadFile(s.config)
		var c struct {
			Aimem     json.RawMessage `json:"aimem"`
			AimemHubs []struct {
				HubID                 string `json:"hub_id"`
				TeamRegisterTokenFile string `json:"team_register_token_file"`
				TeamReadTokenFile     string `json:"team_read_token_file"`
			} `json:"aimem_hubs"`
		}
		if err := json.Unmarshal(raw, &c); err != nil || c.Aimem != nil || len(c.AimemHubs) != 1 ||
			c.AimemHubs[0].HubID != testHubID || c.AimemHubs[0].TeamReadTokenFile != filepath.Join(good, "aimem-team-read.token") ||
			c.AimemHubs[0].TeamRegisterTokenFile != filepath.Join(good, "aimem-team-register.token") {
			t.Fatalf("migrated config (%v):\n%s", err, raw)
		}
	})
}

// snapshot is every file under root but the store's own SQLite files, which
// the running service writes (and so the directories), with its size and
// modification time.
func snapshot(t *testing.T, root string) map[string]string {
	t.Helper()
	m := map[string]string{}
	err := filepath.Walk(root, func(p string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() || strings.HasPrefix(info.Name(), "aicrew.db") && !strings.Contains(info.Name(), "backup") {
			return nil
		}
		m[p] = fmt.Sprintf("%d %d", info.Size(), info.ModTime().UnixNano())
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func equalSnapshots(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}
