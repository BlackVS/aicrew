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

// An alias is one more DNS name on the service's certificate, which a
// proxy in front of the service presents under the alias's name.
func TestLeafAliases(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "tls")
	var out, errb bytes.Buffer
	if code := run([]string{"-out", dir, "-leaf", "sim-aimem,sim-px-aimem"}, &out, &errb); code != 0 {
		t.Fatalf("exit %d: %s", code, errb.String())
	}
	raw, _ := os.ReadFile(filepath.Join(dir, "ca.pem"))
	b, _ := pem.Decode(raw)
	ca, err := x509.ParseCertificate(b.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	pool := x509.NewCertPool()
	pool.AddCert(ca)
	pair, err := tls.LoadX509KeyPair(filepath.Join(dir, "sim-aimem.pem"), filepath.Join(dir, "sim-aimem.key"))
	if err != nil {
		t.Fatal(err)
	}
	leaf, _ := x509.ParseCertificate(pair.Certificate[0])
	for _, name := range []string{"sim-aimem", "sim-px-aimem"} {
		if _, err := leaf.Verify(x509.VerifyOptions{Roots: pool, DNSName: name}); err != nil {
			t.Errorf("the certificate does not verify for %s: %v", name, err)
		}
	}
	if _, err := leaf.Verify(x509.VerifyOptions{Roots: pool, DNSName: "sim-aicrewd"}); err == nil {
		t.Error("the certificate verifies for a name it was not given")
	}
	if _, err := os.Stat(filepath.Join(dir, "sim-px-aimem.pem")); err == nil {
		t.Error("an alias got a certificate of its own")
	}
	pins, _ := os.ReadFile(filepath.Join(dir, "pins.env"))
	if strings.Count(string(pins), "\n") != 1 || !strings.HasPrefix(string(pins), "PIN_SIM_AIMEM=") {
		t.Errorf("pins.env: %q", pins)
	}
}

func TestUsageAndBadNames(t *testing.T) {
	for _, args := range [][]string{nil, {"-out", t.TempDir()}, {"-leaf", "x"}, {"-out", t.TempDir(), "-leaf", "a b"},
		{"-out", t.TempDir(), "-leaf", "a,"}, {"-out", t.TempDir(), "-leaf", ",a"}} {
		var out, errb bytes.Buffer
		if code := run(args, &out, &errb); code == 0 {
			t.Errorf("%q: exit 0", args)
		}
	}
}
