package main

import (
	"bytes"
	"context"
	"crypto/x509"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/BlackVS/aicrew/internal/agent"
)

func TestPreflightCommand(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"status":"ok","version":"v0.11.0"}`))
	}))
	defer srv.Close()
	roots := x509.NewCertPool()
	roots.AddCert(srv.Certificate())
	old := preflightDeps
	t.Cleanup(func() { preflightDeps = old })
	preflightDeps = agent.PreflightDeps{Roots: roots}
	base := []string{"--url", srv.URL, "--tls-trust-mode", "ca_dns", "--tls-trust-value", "127.0.0.1"}

	var out, errb bytes.Buffer
	if code := preflight(context.Background(), base, &out, &errb); code != exitOK ||
		!strings.HasPrefix(out.String(), "aicrewd: reachable: aicrewd v0.11.0 answers") {
		t.Fatalf("%d %q %q", code, out.String(), errb.String())
	}
	out.Reset()
	if code := preflight(context.Background(), append(base, "--client", "opencode", "--json"), &out, &errb); code != exitOK ||
		!strings.Contains(out.String(), `"state": "no_check"`) {
		t.Fatalf("%d %q", code, out.String())
	}
	preflightDeps.AuthStatus = func(context.Context) ([]byte, int, error) {
		return []byte(`{"loggedIn": false, "authMethod": "none"}`), 1, nil
	}
	out.Reset()
	if code := preflight(context.Background(), append(base, "--client", "claude"), &out, &errb); code != exitFailed ||
		!strings.Contains(out.String(), "login (claude): none:") {
		t.Fatalf("%d %q", code, out.String())
	}
	for _, args := range [][]string{nil, base[:4], append(base, "extra"), {"--url", "http://x", "--tls-trust-mode", "ca_dns", "--tls-trust-value", "x"}} {
		errb.Reset()
		if code := preflight(context.Background(), args, &out, &errb); code != exitUsage || errb.Len() == 0 {
			t.Fatalf("%v: %d %q", args, code, errb.String())
		}
	}
}
