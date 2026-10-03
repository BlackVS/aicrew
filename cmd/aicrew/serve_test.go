package main

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/pem"
	"io"
	"log/slog"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/BlackVS/aicrew/internal/optoken"
	"github.com/BlackVS/aicrew/internal/server"
	"github.com/BlackVS/aicrew/internal/store"
)

// svc is a running aicrewd the commands reach over TLS, the way an operator
// does: AICREW_URL, the SPKI pin and the operator token file are in the
// environment. Tests seed and check state through its store, which only the
// service holds.
type svc struct {
	store     *store.Store
	tokenFile string
	url       string
	pin       string
}

func serve(t *testing.T) *svc {
	t.Helper()
	dir := t.TempDir()
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
	os.WriteFile(certFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600)
	os.WriteFile(keyFile, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}), 0o600)
	leaf, _ := x509.ParseCertificate(der)
	sum := sha256.Sum256(leaf.RawSubjectPublicKeyInfo)

	tok, err := optoken.Generate()
	if err != nil {
		t.Fatal(err)
	}
	tokenFile := filepath.Join(dir, "operator.token")
	if err := optoken.Write(tokenFile, tok); err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(context.Background(), filepath.Join(dir, "aicrew.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	srv, err := server.New(server.Config{StorePath: filepath.Join(dir, "aicrew.db"), ListenAddr: "127.0.0.1:0",
		TLSCertFile: certFile, TLSKeyFile: keyFile, ServiceID: "aicrew-test", ShutdownTimeout: server.Duration(5 * time.Second),
		OperatorTokenFile: tokenFile}, st, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- srv.Serve(ctx, ln) }()
	t.Cleanup(func() { cancel(); <-done })
	s := &svc{store: st, tokenFile: tokenFile, url: "https://" + ln.Addr().String(),
		pin: "sha256-" + base64.StdEncoding.EncodeToString(sum[:])}
	t.Setenv("AICREW_URL", s.url)
	t.Setenv("AICREW_TLS_TRUST_MODE", "spki_sha256")
	t.Setenv("AICREW_TLS_TRUST_VALUE", s.pin)
	t.Setenv("AICREW_OPERATOR_TOKEN_FILE", s.tokenFile)
	return s
}
