package main

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"math/big"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/BlackVS/aicrew/internal/store"
	"github.com/BlackVS/aicrew/internal/version"
)

type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// setup writes a certificate, its key and a config listening on a free
// port, and returns the config path, the store path and a trusting pool.
func setup(t *testing.T) (configPath, storePath string, pool *x509.CertPool) {
	t.Helper()
	dir := t.TempDir()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tpl := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "aicrewd test"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		IPAddresses: []net.IP{net.ParseIP("127.0.0.1")}, IsCA: true, BasicConstraintsValid: true,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}, KeyUsage: x509.KeyUsageDigitalSignature,
	}
	der, err := x509.CreateCertificate(rand.Reader, tpl, tpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, _ := x509.MarshalECPrivateKey(key)
	certFile, keyFile := filepath.Join(dir, "cert.pem"), filepath.Join(dir, "key.pem")
	_ = os.WriteFile(certFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600)
	_ = os.WriteFile(keyFile, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}), 0o600)
	cert, _ := x509.ParseCertificate(der)
	pool = x509.NewCertPool()
	pool.AddCert(cert)

	storePath = filepath.Join(dir, "aicrew.db")
	cfg, _ := json.Marshal(map[string]string{"store_path": storePath, "listen_addr": "127.0.0.1:0",
		"tls_cert_file": certFile, "tls_key_file": keyFile, "service_id": "aicrew-test", "shutdown_timeout": "5s"})
	configPath = filepath.Join(dir, "aicrewd.json")
	if err := os.WriteFile(configPath, cfg, 0o600); err != nil {
		t.Fatal(err)
	}
	return configPath, storePath, pool
}

var listening = regexp.MustCompile(`"msg":"listening","addr":"([^"]+)"`)

// The service starts from its config, serves the health check over TLS,
// and on cancellation shuts down, closes the store and exits 0.
func TestRunServesAndShutsDown(t *testing.T) {
	configPath, storePath, pool := setup(t)
	logs := &syncBuffer{}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	exit := make(chan int, 1)
	go func() { exit <- run(ctx, []string{"-config", configPath}, logs) }()

	var addr string
	for deadline := time.Now().Add(10 * time.Second); addr == ""; {
		if m := listening.FindStringSubmatch(logs.String()); m != nil {
			addr = m[1]
		} else if time.Now().After(deadline) {
			t.Fatalf("the service did not start: %s", logs.String())
		}
		time.Sleep(20 * time.Millisecond)
	}
	client := &http.Client{Timeout: 5 * time.Second, Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool}}}
	resp, err := client.Get("https://" + addr + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("health = %d", resp.StatusCode)
	}
	// While it runs, the store is held by the service.
	if _, err := store.Open(context.Background(), storePath); err == nil {
		t.Fatal("the store could be opened a second time while the service runs")
	}

	cancel()
	select {
	case code := <-exit:
		if code != 0 {
			t.Fatalf("exit code %d: %s", code, logs.String())
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the service did not stop")
	}
	st, err := store.Open(context.Background(), storePath)
	if err != nil {
		t.Fatalf("the store is still held after shutdown: %v", err)
	}
	st.Close()
	if !strings.Contains(logs.String(), `"msg":"stopped"`) {
		t.Fatalf("logs lack the stop record: %s", logs.String())
	}
}

// Usage errors exit 2; a refused configuration or a store in use exits 1.
func TestRunRefusals(t *testing.T) {
	ctx := context.Background()
	if code := run(ctx, nil, &syncBuffer{}); code != 2 {
		t.Fatalf("no -config: exit %d, want 2", code)
	}
	if code := run(ctx, []string{"-config", "x", "extra"}, &syncBuffer{}); code != 2 {
		t.Fatalf("extra argument: exit %d, want 2", code)
	}
	bad := filepath.Join(t.TempDir(), "bad.json")
	_ = os.WriteFile(bad, []byte(`{"listen_addr":"127.0.0.1:0"}`), 0o600)
	logs := &syncBuffer{}
	if code := run(ctx, []string{"-config", bad}, logs); code != 1 || !strings.Contains(logs.String(), "store_path") {
		t.Fatalf("bad config: exit %d, logs %s", code, logs.String())
	}
	configPath, storePath, _ := setup(t)
	held, err := store.Open(ctx, storePath)
	if err != nil {
		t.Fatal(err)
	}
	defer held.Close()
	if code := run(ctx, []string{"-config", configPath}, &syncBuffer{}); code != 1 {
		t.Fatalf("store in use: exit %d, want 1", code)
	}
}

// `aicrewd -version [-json]` prints the build and exits; any other command
// line is the service's.
func TestVersionFlag(t *testing.T) {
	defer func(o string) { version.Override = o }(version.Override)
	version.Override = "v1.2.3"
	var out, errb bytes.Buffer
	if code, ok := versionFlag([]string{"-version"}, &out, &errb); !ok || code != 0 || !strings.HasPrefix(out.String(), "aicrewd v1.2.3") {
		t.Fatalf("%d %v %q", code, ok, out.String())
	}
	out.Reset()
	if code, ok := versionFlag([]string{"--version", "-json"}, &out, &errb); !ok || code != 0 || !strings.Contains(out.String(), `"version":"v1.2.3"`) {
		t.Fatalf("%d %v %q", code, ok, out.String())
	}
	if code, ok := versionFlag([]string{"-version", "extra"}, &out, &errb); !ok || code != 2 {
		t.Fatalf("extra argument: %d %v", code, ok)
	}
	if _, ok := versionFlag([]string{"-config", "x"}, &out, &errb); ok {
		t.Fatal("the service's command line was taken for -version")
	}
}
