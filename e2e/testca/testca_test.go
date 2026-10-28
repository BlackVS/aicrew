package testca

import (
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestLeafChainsAndPins(t *testing.T) {
	ca, err := New("test CA", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	leaf, err := ca.Leaf("svc", []string{"svc.example.test"}, []net.IP{net.ParseIP("127.0.0.1")}, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := x509.ParseCertificate(leaf.CertDER)
	if err != nil {
		t.Fatal(err)
	}
	pool := x509.NewCertPool()
	pool.AddCert(ca.Cert)
	for _, name := range []string{"svc.example.test", "127.0.0.1"} {
		if _, err := cert.Verify(x509.VerifyOptions{Roots: pool, DNSName: name,
			KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}); err != nil {
			t.Errorf("verify for %s: %v", name, err)
		}
	}
	if _, err := cert.Verify(x509.VerifyOptions{Roots: pool, DNSName: "other.example.test"}); err == nil {
		t.Error("the leaf verifies for a name it was not issued for")
	}
	pin, err := Pin(cert.PublicKey)
	if err != nil || pin != leaf.Pin {
		t.Errorf("pin %q (%v), want the leaf's %q", pin, err, leaf.Pin)
	}

	dir := t.TempDir()
	certFile, keyFile := filepath.Join(dir, "svc.pem"), filepath.Join(dir, "svc.key")
	if err := WritePEM(certFile, "CERTIFICATE", leaf.CertDER, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := WritePEM(keyFile, "EC PRIVATE KEY", leaf.KeyDER, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := tls.LoadX509KeyPair(certFile, keyFile); err != nil {
		t.Errorf("the written pair does not load: %v", err)
	}
	raw, _ := os.ReadFile(certFile)
	if b, _ := pem.Decode(raw); b == nil || b.Type != "CERTIFICATE" {
		t.Errorf("the written certificate is not one PEM block")
	}
}
