package tlstrust

import (
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func pinOf(der []byte) string {
	c, _ := x509.ParseCertificate(der)
	sum := sha256.Sum256(c.RawSubjectPublicKeyInfo)
	return "sha256-" + base64.StdEncoding.EncodeToString(sum[:])
}

func TestCheck(t *testing.T) {
	pin := "sha256-" + base64.StdEncoding.EncodeToString(make([]byte, 32))
	for _, c := range []struct {
		b    Binding
		host string
		ok   bool
	}{
		{Binding{CADNS, "hub.example"}, "hub.example", true},
		{Binding{CADNS, "hub.example"}, "other.example", false},
		{Binding{CADNS, ""}, "", false},
		{Binding{SPKI, pin}, "anything", true},
		{Binding{SPKI, strings.TrimPrefix(pin, "sha256-")}, "x", false},
		{Binding{SPKI, "sha256-" + base64.StdEncoding.EncodeToString(make([]byte, 31))}, "x", false},
		{Binding{"", ""}, "x", false},
		{Binding{"tofu", "x"}, "x", false},
	} {
		if err := c.b.Check(c.host); (err == nil) != c.ok {
			t.Errorf("%+v for %q: %v", c.b, c.host, err)
		}
	}
}

// A pinned client reaches the pinned server and no other; a CA-trusted
// client needs the CA.
func TestClientConfig(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer srv.Close()
	get := func(b Binding, pool *x509.CertPool, url string) error {
		cfg, err := b.ClientConfig(pool)
		if err != nil {
			return err
		}
		resp, err := (&http.Client{Transport: &http.Transport{TLSClientConfig: cfg}}).Get(url)
		if err == nil {
			resp.Body.Close()
		}
		return err
	}
	pin := pinOf(srv.Certificate().Raw)
	if err := get(Binding{SPKI, pin}, nil, srv.URL); err != nil {
		t.Fatalf("pinned: %v", err)
	}
	zeros := "sha256-" + base64.StdEncoding.EncodeToString(make([]byte, 32))
	if err := get(Binding{SPKI, zeros}, nil, srv.URL); !Untrusted(err) || !errors.Is(err, ErrPinMismatch) {
		t.Fatalf("another pin: %v", err)
	}
	pool := x509.NewCertPool()
	pool.AddCert(srv.Certificate())
	if err := get(Binding{CADNS, "127.0.0.1"}, pool, srv.URL); err != nil {
		t.Fatalf("ca_dns: %v", err)
	}
	if err := get(Binding{CADNS, "127.0.0.1"}, nil, srv.URL); !Untrusted(err) {
		t.Fatalf("ca_dns with system roots: %v", err)
	}
	if _, err := (Binding{"tofu", ""}).ClientConfig(nil); err == nil {
		t.Fatal("an unknown mode built a config")
	}
}
