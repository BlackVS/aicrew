package main

import (
	"bytes"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/BlackVS/aicrew/e2e/testca"
)

func TestIssueWritesAChainAndPins(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "tls")
	var out, errb bytes.Buffer
	if code := run([]string{"-out", dir, "-leaf", "sim-aimem", "-leaf", "sim-aicrewd"}, &out, &errb); code != 0 {
		t.Fatalf("exit %d: %s", code, errb.String())
	}
	raw, err := os.ReadFile(filepath.Join(dir, "ca.pem"))
	if err != nil {
		t.Fatal(err)
	}
	b, _ := pem.Decode(raw)
	ca, err := x509.ParseCertificate(b.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	pool := x509.NewCertPool()
	pool.AddCert(ca)
	pins, _ := os.ReadFile(filepath.Join(dir, "pins.env"))
	for _, name := range []string{"sim-aimem", "sim-aicrewd"} {
		pair, err := tls.LoadX509KeyPair(filepath.Join(dir, name+".pem"), filepath.Join(dir, name+".key"))
		if err != nil {
			t.Fatal(err)
		}
		leaf, _ := x509.ParseCertificate(pair.Certificate[0])
		if _, err := leaf.Verify(x509.VerifyOptions{Roots: pool, DNSName: name}); err != nil {
			t.Errorf("%s does not chain to the CA for its name: %v", name, err)
		}
		pin, _ := testca.Pin(leaf.PublicKey)
		line := "PIN_" + strings.ToUpper(strings.ReplaceAll(name, "-", "_")) + "=" + pin + "\n"
		if !strings.Contains(string(pins), line) {
			t.Errorf("pins.env lacks %q:\n%s", line, pins)
		}
		if fi, _ := os.Stat(filepath.Join(dir, name+".key")); runtime.GOOS != "windows" && fi.Mode().Perm() != 0o600 {
			t.Errorf("%s.key mode %v, want 0600", name, fi.Mode().Perm())
		}
	}

	// A second run keeps the first run's files.
	out.Reset()
	if code := run([]string{"-out", dir, "-leaf", "sim-aimem"}, &out, &errb); code != 0 ||
		!strings.Contains(out.String(), "already holds") {
		t.Fatalf("second run: exit %d %q", code, out.String())
	}
	again, _ := os.ReadFile(filepath.Join(dir, "pins.env"))
	if !bytes.Equal(again, pins) {
		t.Error("a second run changed the pins")
	}
}

func TestUsageAndBadNames(t *testing.T) {
	for _, args := range [][]string{nil, {"-out", t.TempDir()}, {"-leaf", "x"}, {"-out", t.TempDir(), "-leaf", "a b"}} {
		var out, errb bytes.Buffer
		if code := run(args, &out, &errb); code == 0 {
			t.Errorf("%q: exit 0", args)
		}
	}
}
