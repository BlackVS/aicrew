package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/BlackVS/aicrew/internal/privatefile"
	"github.com/BlackVS/aicrew/internal/svcconfig"
)

const (
	hubTestConfig = `{"store_path":"/var/lib/aicrew/aicrew.db","listen_addr":"127.0.0.1:8443",
	"tls_cert_file":"/etc/aicrew/cert.pem","tls_key_file":"/etc/aicrew/key.pem","service_id":"aicrew-example",
	"operator_token_file":"/etc/aicrew/operator.token"}`
	hubTestID = "01a119aa-85ea-7000-80e5-1ee514e3db40"
)

// fakeHub answers team-reads for aicrew-example with the team.read
// credential the provisioned directory holds, and refuses with code
// otherwise.
func fakeHub(t *testing.T, readToken, code string) (*httptest.Server, string) {
	t.Helper()
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if code != "" || r.URL.Path != "/v1/identity/peers/aicrew-example/team-reads" ||
			r.Header.Get("Authorization") != "Bearer "+readToken {
			if code == "" {
				code = "peer_unauthenticated"
			}
			w.WriteHeader(http.StatusForbidden)
			json.NewEncoder(w).Encode(map[string]string{"code": code, "message": "m"})
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"teams": []any{}})
	}))
	t.Cleanup(srv.Close)
	sum := sha256.Sum256(srv.Certificate().RawSubjectPublicKeyInfo)
	return srv, "sha256-" + base64.StdEncoding.EncodeToString(sum[:])
}

// provisionDir writes the files aimem identity peer provision writes and
// returns the directory and the team.read credential.
func provisionDir(t *testing.T) (string, string) {
	t.Helper()
	dir := t.TempDir()
	files := map[string]string{svcconfig.HubIDFile: hubTestID}
	for i, name := range []string{svcconfig.RedemptionTokenFile, svcconfig.ReadTokenFile, svcconfig.TeamRegisterTokenFile,
		svcconfig.TeamReadTokenFile, svcconfig.BoardReadTokenFile} {
		files[name] = "aimem_peer_" + strings.Repeat(string("abcde"[i]), 64)
	}
	for name, v := range files {
		f, err := privatefile.Create(filepath.Join(dir, name))
		if err != nil {
			t.Fatal(err)
		}
		f.WriteString(v + "\n")
		f.Close()
	}
	return dir, files[svcconfig.TeamReadTokenFile]
}

func runHubCmd(t *testing.T, args ...string) (int, string, string) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	code := runHub(context.Background(), args, &stdout, &stderr, time.Date(2026, 10, 8, 4, 30, 0, 0, time.UTC))
	return code, stdout.String(), stderr.String()
}

// hub add binds a reachable hub that accepts the team.read credential, and
// tells the operator to restart aicrewd.
func TestHubAdd(t *testing.T) {
	dir, readToken := provisionDir(t)
	srv, pin := fakeHub(t, readToken, "")
	path := filepath.Join(t.TempDir(), "aicrewd.json")
	if err := os.WriteFile(path, []byte(hubTestConfig), 0o600); err != nil {
		t.Fatal(err)
	}
	code, out, errOut := runHubCmd(t, "add", "main", "--config", path, "--base-url", srv.URL,
		"--tls-trust-mode", "spki_sha256", "--tls-trust-value", pin, "--cred-dir", dir)
	if code != 0 || !strings.Contains(out, "hub main (hub ID "+hubTestID+") added as aimem_hubs[0]") ||
		!strings.Contains(out, "Restart aicrewd") {
		t.Fatalf("exit %d:\n%s%s", code, out, errOut)
	}
	c, err := svcconfig.LoadConfig(path)
	if err != nil || len(c.AimemHubs) != 1 || c.AimemHubs[0].HubID != hubTestID {
		t.Fatalf("config = %+v, %v", c, err)
	}
	if strings.Contains(out+errOut, readToken) {
		t.Fatal("a credential was printed")
	}
}

// A hub's refusal of the team read is reported with its code and remedy,
// and nothing is written.
func TestHubAddRefused(t *testing.T) {
	for code, remedy := range map[string]string{
		"peer_forbidden":       "check service_id",
		"peer_unauthenticated": "run aimem identity peer provision",
	} {
		t.Run(code, func(t *testing.T) {
			dir, readToken := provisionDir(t)
			srv, pin := fakeHub(t, readToken, code)
			path := filepath.Join(t.TempDir(), "aicrewd.json")
			if err := os.WriteFile(path, []byte(hubTestConfig), 0o600); err != nil {
				t.Fatal(err)
			}
			exit, _, errOut := runHubCmd(t, "add", "main", "--config", path, "--base-url", srv.URL,
				"--tls-trust-mode", "spki_sha256", "--tls-trust-value", pin, "--cred-dir", dir)
			if exit != 1 || !strings.Contains(errOut, code) || !strings.Contains(errOut, remedy) || !strings.Contains(errOut, "nothing was written") {
				t.Fatalf("exit %d: %s", exit, errOut)
			}
			if got, _ := os.ReadFile(path); string(got) != hubTestConfig {
				t.Fatal("the config changed")
			}
		})
	}
}

// A hub whose TLS identity is not the pinned one is unreachable for hub
// add: nothing is written.
func TestHubAddWrongPin(t *testing.T) {
	dir, readToken := provisionDir(t)
	srv, _ := fakeHub(t, readToken, "")
	path := filepath.Join(t.TempDir(), "aicrewd.json")
	if err := os.WriteFile(path, []byte(hubTestConfig), 0o600); err != nil {
		t.Fatal(err)
	}
	other := "sha256-" + base64.StdEncoding.EncodeToString(make([]byte, 32))
	exit, _, errOut := runHubCmd(t, "add", "main", "--config", path, "--base-url", srv.URL,
		"--tls-trust-mode", "spki_sha256", "--tls-trust-value", other, "--cred-dir", dir)
	if exit != 1 || !strings.Contains(errOut, "hub_unavailable") {
		t.Fatalf("exit %d: %s", exit, errOut)
	}
}

func TestHubUsage(t *testing.T) {
	for _, args := range [][]string{{}, {"add"}, {"list"}, {"add", "--config", "x"}, {"add", "main", "--config", "x"},
		{"add", "main", "--config", "x", "--base-url", "https://h", "--tls-trust-mode", "ca_dns", "--tls-trust-value", "h", "--cred-dir", "d", "extra"}} {
		if code, _, _ := runHubCmd(t, args...); code != 2 {
			t.Errorf("%v: exit %d, want 2", args, code)
		}
	}
}

// Adding a hub to a file whose migrated hub still has no hub_id writes the
// new entry, names the other hub and exits 3: aicrewd would refuse it.
func TestHubAddWithAPendingHub(t *testing.T) {
	dir, readToken := provisionDir(t)
	srv, pin := fakeHub(t, readToken, "")
	pending := strings.Replace(hubTestConfig, `"operator_token_file"`, `"aimem_hubs":[{"name":"default","base_url":"https://old.example",`+
		`"tls_trust_mode":"ca_dns","tls_trust_value":"old.example","redemption_token_file":"/etc/aicrew/r.token"}],"operator_token_file"`, 1)
	path := filepath.Join(t.TempDir(), "aicrewd.json")
	if err := os.WriteFile(path, []byte(pending), 0o600); err != nil {
		t.Fatal(err)
	}
	code, out, errOut := runHubCmd(t, "add", "main", "--config", path, "--base-url", srv.URL,
		"--tls-trust-mode", "spki_sha256", "--tls-trust-value", pin, "--cred-dir", dir)
	if code != exitIncomplete || !strings.Contains(out, "added as aimem_hubs[1]") || !strings.Contains(out, "hub_id: default") {
		t.Fatalf("exit %d:\n%s%s", code, out, errOut)
	}
}
