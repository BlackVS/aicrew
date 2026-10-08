package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const legacyConfig = `{"store_path":"/var/lib/aicrew/aicrew.db","listen_addr":"127.0.0.1:8443",
	"tls_cert_file":"/etc/aicrew/cert.pem","tls_key_file":"/etc/aicrew/key.pem","service_id":"aicrew-example",
	"aimem":{"base_url":"https://aimem.example:8443","tls_trust_mode":"ca_dns","tls_trust_value":"aimem.example",
	"redemption_token_file":"/etc/aicrew/aimem-redemption.token"},
	"operator_token_file":"/etc/aicrew/operator.token"}`

func runConfig(t *testing.T, args ...string) (int, string, string) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	code, ok := configCommand(args, &stdout, &stderr, time.Date(2026, 10, 8, 4, 30, 0, 0, time.UTC))
	if !ok {
		t.Fatalf("%v is not a config command", args)
	}
	return code, stdout.String(), stderr.String()
}

// migrate exits 3 while aicrewd would refuse the file for its missing
// hub_id, 0 once nothing it needs is missing, 1 on a refusal and 2 on a
// usage error.
func TestConfigMigrate(t *testing.T) {
	path := filepath.Join(t.TempDir(), "aicrewd.json")
	if err := os.WriteFile(path, []byte(legacyConfig), 0o600); err != nil {
		t.Fatal(err)
	}
	code, out, _ := runConfig(t, "config", "migrate", "-config", path)
	if code != exitIncomplete || !strings.Contains(out, path+".20261008T043000Z.bak") ||
		!strings.Contains(out, "aimem_hubs[0].hub_id:") || !strings.Contains(out, `service_id "aicrew-example"`) ||
		!strings.Contains(out, "do not restart it yet") {
		t.Fatalf("exit %d:\n%s", code, out)
	}
	code, out, _ = runConfig(t, "config", "migrate", "-config", path)
	if code != exitIncomplete || !strings.Contains(out, "nothing changed") {
		t.Fatalf("second run: exit %d:\n%s", code, out)
	}
	code, _, errOut := runConfig(t, "config", "migrate", "-config", path, "-hub-id", "hub-1")
	if code != 1 || !strings.Contains(errOut, "nothing was written") {
		t.Fatalf("flags on a migrated file: exit %d: %s", code, errOut)
	}

	complete := filepath.Join(t.TempDir(), "aicrewd.json")
	if err := os.WriteFile(complete, []byte(legacyConfig), 0o600); err != nil {
		t.Fatal(err)
	}
	code, out, _ = runConfig(t, "config", "migrate", "-config", complete, "-name", "main", "-hub-id", "hub-1",
		"-team-register-token-file", "/etc/aicrew/team-register.token", "-team-read-token-file", "/etc/aicrew/team-read.token")
	if code != 0 || strings.Contains(out, "Still to supply") || !strings.Contains(out, "Restart aicrewd") {
		t.Fatalf("complete: exit %d:\n%s", code, out)
	}

	for _, args := range [][]string{{"config"}, {"config", "show"}, {"config", "migrate"}, {"config", "migrate", "-config", path, "extra"}} {
		if code, _, _ := runConfig(t, args...); code != 2 {
			t.Errorf("%v: exit %d, want 2", args, code)
		}
	}
	if _, ok := configCommand([]string{"-config", path}, &bytes.Buffer{}, &bytes.Buffer{}, time.Now()); ok {
		t.Fatal("-config is taken for a config command")
	}
}

// config show prints the store, the listen address and whether the aimem
// block of 0.2.0 is still there; a hub without its hub_id is shown, any
// other refusal exits 1.
func TestConfigShow(t *testing.T) {
	path := filepath.Join(t.TempDir(), "aicrewd.json")
	if err := os.WriteFile(path, []byte(legacyConfig), 0o600); err != nil {
		t.Fatal(err)
	}
	want := "store_path=/var/lib/aicrew/aicrew.db\nlisten_addr=127.0.0.1:8443\nservice_id=aicrew-example\nlegacy_aimem_block=yes\n"
	if code, out, errOut := runConfig(t, "config", "show", "-config", path); code != 0 || out != want {
		t.Fatalf("legacy: exit %d:\n%s%s", code, out, errOut)
	}
	if code, _, _ := runConfig(t, "config", "migrate", "-config", path); code != exitIncomplete {
		t.Fatalf("migrate: exit %d", code)
	}
	want = strings.Replace(want, "=yes", "=no", 1)
	if code, out, errOut := runConfig(t, "config", "show", "-config", path); code != 0 || out != want {
		t.Fatalf("pending hub: exit %d:\n%s%s", code, out, errOut)
	}
	if err := os.WriteFile(path, []byte(`{"store_path": 1}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if code, _, _ := runConfig(t, "config", "show", "-config", path); code != 1 {
		t.Fatalf("refused file: exit %d", code)
	}
	for _, args := range [][]string{{"config", "show"}, {"config", "show", "-config", path, "extra"}, {"config", "other"}} {
		if code, _, _ := runConfig(t, args...); code != 2 {
			t.Errorf("%v: exit %d, want 2", args, code)
		}
	}
}
