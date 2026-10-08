package server

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// legacyConfig is a 0.2.0 configuration: the single aimem block, between
// other fields, with a shutdown_timeout aicrewd would otherwise default.
const legacyConfig = `{
  "store_path": "/var/lib/aicrew/aicrew.db",
  "listen_addr": "127.0.0.1:8443",
  "tls_cert_file": "/etc/aicrew/cert.pem",
  "tls_key_file": "/etc/aicrew/key.pem",
  "service_id": "aicrew-example",
  "aimem": {"base_url": "https://aimem.example:8443", "tls_trust_mode": "ca_dns", "tls_trust_value": "aimem.example",
    "redemption_token_file": "/etc/aicrew/aimem-redemption.token", "read_token_file": "/etc/aicrew/aimem-read.token"},
  "operator_token_file": "/etc/aicrew/operator.token",
  "shutdown_timeout": "20s"
}
`

var migrateNow = time.Date(2026, 10, 8, 4, 30, 0, 0, time.UTC)

func writeConfig(t *testing.T, raw string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "aicrewd.json")
	if err := os.WriteFile(path, []byte(raw), 0o640); err != nil {
		t.Fatal(err)
	}
	return path
}

func dirNames(t *testing.T, path string) []string {
	t.Helper()
	entries, err := os.ReadDir(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	return names
}

func topKeys(t *testing.T, raw []byte) []string {
	t.Helper()
	o, err := decodeJSONObject(raw)
	if err != nil {
		t.Fatal(err)
	}
	var keys []string
	for _, m := range o {
		keys = append(keys, m.key)
	}
	return keys
}

// A 0.2.0 file becomes one aimem_hubs entry carrying the block's five
// fields; every other field keeps its value and place; the previous file is
// kept; the fields still missing are named; and a second run changes
// nothing.
func TestMigrateLegacy(t *testing.T) {
	path := writeConfig(t, legacyConfig)
	r, err := MigrateConfig(path, MigrateOptions{Now: migrateNow})
	if err != nil {
		t.Fatal(err)
	}
	backup := path + ".20261008T043000Z.bak"
	if !r.Migrated || r.Backup != backup || r.ServiceID != "aicrew-example" {
		t.Fatalf("report = %+v", r)
	}
	if got, _ := os.ReadFile(backup); string(got) != legacyConfig {
		t.Fatalf("backup = %q", got)
	}
	out, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"store_path", "listen_addr", "tls_cert_file", "tls_key_file", "service_id", "aimem_hubs", "operator_token_file", "shutdown_timeout"}
	if got := topKeys(t, out); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("keys = %v, want %v", got, want)
	}
	c, err := decodeConfig(out)
	if err != nil {
		t.Fatal(err)
	}
	wantHub := AimemHub{Name: LegacyHubName, AimemConfig: AimemConfig{BaseURL: "https://aimem.example:8443",
		TLSTrustMode: "ca_dns", TLSTrustValue: "aimem.example",
		RedemptionTokenFile: "/etc/aicrew/aimem-redemption.token", ReadTokenFile: "/etc/aicrew/aimem-read.token"}}
	if c.Aimem != nil || len(c.AimemHubs) != 1 || c.AimemHubs[0] != wantHub || time.Duration(c.ShutdownTimeout) != 20*time.Second {
		t.Fatalf("migrated = %+v", c)
	}
	// Without its hub_id, aicrewd refuses the file, and migrate says so.
	if _, err := ParseConfig(out); err == nil || !strings.Contains(err.Error(), "aimem_hubs[0].hub_id") {
		t.Fatalf("ParseConfig = %v", err)
	}
	var missing []string
	for _, m := range r.Missing {
		missing = append(missing, m.Field)
	}
	if strings.Join(missing, ",") != "aimem_hubs[0].hub_id,aimem_hubs[0].team_register_token_file,aimem_hubs[0].team_read_token_file" || !r.Incomplete() {
		t.Fatalf("missing = %v, incomplete = %v", missing, r.Incomplete())
	}
	if runtime.GOOS != "windows" {
		if info, _ := os.Stat(path); info.Mode().Perm() != 0o640 {
			t.Fatalf("mode = %v", info.Mode())
		}
		if info, _ := os.Stat(backup); info.Mode().Perm() != 0o640 {
			t.Fatalf("backup mode = %v", info.Mode())
		}
	}

	before := dirNames(t, path)
	again, err := MigrateConfig(path, MigrateOptions{Now: migrateNow.Add(time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	if again.Migrated || again.Backup != "" || len(again.Missing) != 3 || !again.Incomplete() {
		t.Fatalf("second run = %+v", again)
	}
	if got, _ := os.ReadFile(path); !bytes.Equal(got, out) {
		t.Fatal("the second run changed the file")
	}
	if after := dirNames(t, path); strings.Join(after, ",") != strings.Join(before, ",") {
		t.Fatalf("files %v, then %v", before, after)
	}
}

// With every value given, the migrated file is one aicrewd accepts, and
// nothing is left to supply.
func TestMigrateComplete(t *testing.T) {
	path := writeConfig(t, legacyConfig)
	dir := filepath.Dir(path)
	t.Chdir(dir)
	r, err := MigrateConfig(path, MigrateOptions{Now: migrateNow, Name: "main", HubID: "hub-1",
		TeamRegisterTokenFile: "team-register.token", TeamReadTokenFile: "team-read.token", ServiceID: "aicrew-main"})
	if err != nil {
		t.Fatal(err)
	}
	if !r.Migrated || len(r.Missing) != 0 || r.Incomplete() || r.ServiceID != "aicrew-main" {
		t.Fatalf("report = %+v", r)
	}
	c, err := LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	h := c.AimemHubs[0]
	if c.ServiceID != "aicrew-main" || h.Name != "main" || h.HubID != "hub-1" ||
		h.TeamRegisterTokenFile != filepath.Join(dir, "team-register.token") || h.TeamReadTokenFile != filepath.Join(dir, "team-read.token") {
		t.Fatalf("migrated = %+v", c)
	}
	if again, err := MigrateConfig(path, MigrateOptions{Now: migrateNow}); err != nil || again.Migrated || len(again.Missing) != 0 {
		t.Fatalf("second run = %+v, %v", again, err)
	}
}

// A configuration with no hub has nothing to migrate and nothing missing.
func TestMigrateNoHub(t *testing.T) {
	path := writeConfig(t, validConfig)
	r, err := MigrateConfig(path, MigrateOptions{Now: migrateNow})
	if err != nil || r.Migrated || len(r.Missing) != 0 || r.Incomplete() {
		t.Fatalf("report = %+v, %v", r, err)
	}
	if got, _ := os.ReadFile(path); string(got) != validConfig {
		t.Fatal("the file changed")
	}
}

// A refused migration writes nothing: no copy, no change.
func TestMigrateRefusals(t *testing.T) {
	if raw := oversizedLegacy(); len(raw) != maxConfigBytes {
		t.Fatalf("oversizedLegacy is %d bytes", len(raw))
	} else if _, err := ParseConfig([]byte(raw)); err != nil {
		t.Fatalf("oversizedLegacy is refused as input: %v", err)
	}
	hubs := `"aimem_hubs":[{"name":"main","hub_id":"hub-1","base_url":"https://aimem.example:8443",
		"tls_trust_mode":"ca_dns","tls_trust_value":"aimem.example","redemption_token_file":"/etc/aicrew/r.token"}],`
	for _, tc := range []struct {
		name, raw string
		opt       MigrateOptions
		want      string
	}{
		{"both forms", strings.Replace(legacyConfig, `"aimem":`, hubs+`"aimem":`, 1), MigrateOptions{}, "given together"},
		{"not a config", `{"store_path": 1}`, MigrateOptions{}, "store_path"},
		{"unknown field", strings.Replace(legacyConfig, `"aimem": {`, `"aimem": {"hub_id": "x", `, 1), MigrateOptions{}, "hub_id"},
		{"bad name", legacyConfig, MigrateOptions{Name: "Main Hub"}, "aimem_hubs[0].name"},
		{"bad hub id", legacyConfig, MigrateOptions{HubID: "a hub"}, "aimem_hubs[0].hub_id"},
		{"bad service id", legacyConfig, MigrateOptions{ServiceID: "a service"}, "service_id"},
		{"same team files", legacyConfig, MigrateOptions{TeamRegisterTokenFile: "/t", TeamReadTokenFile: "/t"}, "aimem_hubs[0]"},
		{"empty aimem_hubs after", strings.Replace(legacyConfig, `"operator_token_file"`, `"aimem_hubs": [], "operator_token_file"`, 1),
			MigrateOptions{}, "given together"},
		{"empty aimem_hubs before", strings.Replace(legacyConfig, `"aimem":`, `"aimem_hubs": [], "aimem":`, 1),
			MigrateOptions{}, "given together"},
		{"null aimem_hubs after", strings.Replace(legacyConfig, `"operator_token_file"`, `"aimem_hubs": null, "operator_token_file"`, 1),
			MigrateOptions{}, "given together"},
		{"null aimem_hubs before", strings.Replace(legacyConfig, `"aimem":`, `"aimem_hubs": null, "aimem":`, 1),
			MigrateOptions{}, "given together"},
		{"result over the size limit", oversizedLegacy(), MigrateOptions{HubID: "hub-1"}, "migrated config would be refused: it is larger than"},
		{"aimem twice", strings.Replace(legacyConfig, `"operator_token_file"`, `"aimem": {}, "operator_token_file"`, 1),
			MigrateOptions{}, `"aimem" is given twice`},
		{"flags on a migrated file", strings.Replace(validConfig, `"operator_token_file"`, hubs+`"operator_token_file"`, 1),
			MigrateOptions{HubID: "hub-2"}, "no aimem block"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := writeConfig(t, tc.raw)
			tc.opt.Now = migrateNow
			if _, err := MigrateConfig(path, tc.opt); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("got %v, want an error naming %q", err, tc.want)
			}
			if got, _ := os.ReadFile(path); string(got) != tc.raw {
				t.Fatal("the file changed")
			}
			if names := dirNames(t, path); len(names) != 1 {
				t.Fatalf("files = %v", names)
			}
		})
	}
}

// An existing copy under the same time is never overwritten.
func TestMigrateKeepsAnExistingCopy(t *testing.T) {
	path := writeConfig(t, legacyConfig)
	backup := path + ".20261008T043000Z.bak"
	if err := os.WriteFile(backup, []byte("older"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := MigrateConfig(path, MigrateOptions{Now: migrateNow}); err == nil {
		t.Fatal("migrated over an existing copy")
	}
	if got, _ := os.ReadFile(backup); string(got) != "older" {
		t.Fatal("the existing copy changed")
	}
	if got, _ := os.ReadFile(path); string(got) != legacyConfig {
		t.Fatal("the file changed")
	}
}

// The migrated file is the same JSON whatever the input's layout.
func TestMigratedIsIndented(t *testing.T) {
	out, err := migrated([]byte(strings.Join(strings.Fields(legacyConfig), "")), MigrateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	var v any
	if err := json.Unmarshal(out, &v); err != nil || !strings.Contains(string(out), "\n  \"aimem_hubs\": [\n    {\n      \"name\": \"default\",") {
		t.Fatalf("out = %s, %v", out, err)
	}
}

// A symbolic link stays a link: the file it names is migrated, and its copy
// is kept beside that file.
func TestMigrateFollowsALink(t *testing.T) {
	target := writeConfig(t, legacyConfig)
	link := filepath.Join(t.TempDir(), "aicrewd.json")
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("no symbolic link here: %v", err)
	}
	r, err := MigrateConfig(link, MigrateOptions{Now: migrateNow})
	if err != nil {
		t.Fatal(err)
	}
	if info, err := os.Lstat(link); err != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("the link was replaced: %v", err)
	}
	if want, _ := filepath.EvalSymlinks(target); r.Backup != want+".20261008T043000Z.bak" {
		t.Fatalf("backup = %s", r.Backup)
	}
	if c, err := decodeConfig(mustRead(t, target)); err != nil || len(c.AimemHubs) != 1 {
		t.Fatalf("target = %+v, %v", c, err)
	}
}

func mustRead(t *testing.T, path string) []byte {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// A path through a linked directory is migrated as given: the copy is named
// beside that path, not beside the directory's target.
func TestMigrateThroughALinkedDirectory(t *testing.T) {
	target := writeConfig(t, legacyConfig)
	linkDir := filepath.Join(t.TempDir(), "conf")
	if err := os.Symlink(filepath.Dir(target), linkDir); err != nil {
		t.Skipf("no symbolic link here: %v", err)
	}
	path := filepath.Join(linkDir, "aicrewd.json")
	r, err := MigrateConfig(path, MigrateOptions{Now: migrateNow})
	if err != nil {
		t.Fatal(err)
	}
	if r.Backup != path+".20261008T043000Z.bak" {
		t.Fatalf("backup = %s", r.Backup)
	}
}

// oversizedLegacy is a legacy configuration exactly at aicrewd's size
// limit, written compactly, whose migrated, indented form is over it.
func oversizedLegacy() string {
	compact := strings.Join(strings.Fields(legacyConfig), "")
	grow := maxConfigBytes - len(compact)
	return strings.Replace(compact, "aicrew.db", strings.Repeat("a", len("aicrew.db")+grow), 1)
}
