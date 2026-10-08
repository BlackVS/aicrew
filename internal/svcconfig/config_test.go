package svcconfig

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const validConfig = `{"store_path":"/var/lib/aicrew/aicrew.db","listen_addr":"127.0.0.1:8443",
	"tls_cert_file":"/etc/aicrew/cert.pem","tls_key_file":"/etc/aicrew/key.pem","service_id":"aicrew-example",
	"operator_token_file":"/etc/aicrew/operator.token"}`

func TestParseConfig(t *testing.T) {
	c, err := ParseConfig([]byte(validConfig))
	if err != nil {
		t.Fatal(err)
	}
	if c.ServiceID != "aicrew-example" || time.Duration(c.ShutdownTimeout) != DefaultShutdownTimeout {
		t.Fatalf("config = %+v", c)
	}
	c, err = ParseConfig([]byte(strings.Replace(validConfig, "}", `,"shutdown_timeout":"30s"}`, 1)))
	if err != nil || time.Duration(c.ShutdownTimeout) != 30*time.Second {
		t.Fatalf("shutdown_timeout = %v, %v", c.ShutdownTimeout, err)
	}
}

// A bad configuration is refused with the field named.
func TestParseConfigRefusals(t *testing.T) {
	for _, tc := range []struct{ name, raw, want string }{
		{"unknown field", strings.Replace(validConfig, "}", `,"tls_key":"x"}`, 1), "tls_key"},
		{"no store", strings.Replace(validConfig, `"/var/lib/aicrew/aicrew.db"`, `""`, 1), "store_path"},
		{"no listen", strings.Replace(validConfig, `"127.0.0.1:8443"`, `""`, 1), "listen_addr"},
		{"listen without port", strings.Replace(validConfig, `"127.0.0.1:8443"`, `"127.0.0.1"`, 1), "listen_addr"},
		{"listen bad port", strings.Replace(validConfig, `:8443"`, `:https"`, 1), "listen_addr"},
		{"no cert", strings.Replace(validConfig, `"/etc/aicrew/cert.pem"`, `""`, 1), "tls_cert_file"},
		{"no key", strings.Replace(validConfig, `"/etc/aicrew/key.pem"`, `""`, 1), "tls_key_file"},
		{"no operator token", strings.Replace(validConfig, `"/etc/aicrew/operator.token"`, `""`, 1), "operator_token_file"},
		{"no service", strings.Replace(validConfig, `"aicrew-example"`, `""`, 1), "service_id"},
		{"bad service", strings.Replace(validConfig, `"aicrew-example"`, `"aicrew example"`, 1), "service_id"},
		{"bad shutdown", strings.Replace(validConfig, "}", `,"shutdown_timeout":"-1s"}`, 1), "shutdown_timeout"},
		{"long shutdown", strings.Replace(validConfig, "}", `,"shutdown_timeout":"1h"}`, 1), "shutdown_timeout"},
		{"shutdown not a duration", strings.Replace(validConfig, "}", `,"shutdown_timeout":15}`, 1), "duration"},
		{"two objects", validConfig + validConfig, "one JSON object"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := ParseConfig([]byte(tc.raw)); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("got %v, want an error naming %q", err, tc.want)
			}
		})
	}
}

func TestLoadConfig(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "aicrewd.json")
	if err := os.WriteFile(path, []byte(validConfig), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadConfig(path); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadConfig(filepath.Join(dir, "missing.json")); err == nil {
		t.Fatal("a missing config file was accepted")
	}
	big := filepath.Join(dir, "big.json")
	if err := os.WriteFile(big, []byte(strings.Repeat(" ", MaxConfigBytes+1)), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadConfig(big); err == nil || !strings.Contains(err.Error(), "larger") {
		t.Fatalf("an oversized config = %v", err)
	}
}

func withAimem(section string) string {
	return strings.Replace(validConfig, "}", `,"aimem":`+section+`}`, 1)
}

const validAimem = `{"base_url":"https://hub.example:8443","tls_trust_mode":"ca_dns",
	"tls_trust_value":"hub.example","redemption_token_file":"/etc/aicrew/redemption.token"}`

// The aimem section is optional; when present it is checked offline as the
// verifier checks it.
func TestConfigAimem(t *testing.T) {
	c, err := ParseConfig([]byte(withAimem(validAimem)))
	if err != nil || c.Aimem == nil || c.Aimem.TLSTrustValue != "hub.example" {
		t.Fatalf("aimem section = %+v, %v", c.Aimem, err)
	}
	for name, section := range map[string]string{
		"plain http":    strings.Replace(validAimem, "https://", "http://", 1),
		"no trust":      strings.Replace(validAimem, `"ca_dns"`, `""`, 1),
		"other host":    strings.Replace(validAimem, `"tls_trust_value":"hub.example"`, `"tls_trust_value":"other.example"`, 1),
		"no token file": strings.Replace(validAimem, `"/etc/aicrew/redemption.token"`, `""`, 1),
		"unknown field": strings.Replace(validAimem, "}", `,"insecure":true}`, 1),
	} {
		if _, err := ParseConfig([]byte(withAimem(section))); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}
