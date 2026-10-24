package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/BlackVS/aicrew/internal/opapi"
	"github.com/BlackVS/aicrew/internal/optoken"
)

// serveHandoff points the command at an aicrewd stand-in that answers the
// handoff route with h and records the attempt it was asked for. The
// route's own test is the server's.
func serveHandoff(t *testing.T, h opapi.AttemptHandoff) *string {
	t.Helper()
	asked := new(string)
	mux := http.NewServeMux()
	mux.HandleFunc("GET "+opapi.AttemptHandoffPath, func(w http.ResponseWriter, r *http.Request) {
		*asked = r.URL.Query().Get("id")
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(h)
	})
	srv := httptest.NewTLSServer(mux)
	t.Cleanup(srv.Close)
	sum := sha256.Sum256(srv.Certificate().RawSubjectPublicKeyInfo)
	tok, _ := optoken.Generate()
	tokenFile := filepath.Join(t.TempDir(), "operator.token")
	if err := optoken.Write(tokenFile, tok); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AICREW_URL", srv.URL)
	t.Setenv("AICREW_TLS_TRUST_MODE", "spki_sha256")
	t.Setenv("AICREW_TLS_TRUST_VALUE", "sha256-"+base64.StdEncoding.EncodeToString(sum[:]))
	t.Setenv("AICREW_OPERATOR_TOKEN_FILE", tokenFile)
	return asked
}

// aicrew attempts handoff prints an attempt's handoff with its push time,
// without the terminal escapes and control characters the worker's text
// may hold; --json prints aicrewd's answer as it is; an attempt without one
// says so; no --id is a usage error.
func TestAttemptsHandoff(t *testing.T) {
	ctx := context.Background()
	at := time.Date(2026, 10, 24, 5, 30, 0, 0, time.UTC)
	doc := "# Handoff\n\n### Next\n\n\x1b[31mOpen\x1b[0m the pull\x07 request.\r\n"
	asked := serveHandoff(t, opapi.AttemptHandoff{AttemptID: "att-1", TeamID: "team-1", Handoff: doc, UpdatedAt: at})

	var out, errb bytes.Buffer
	if code := run(ctx, []string{"attempts", "handoff", "--id", "att-1"}, &out, &errb); code != 0 {
		t.Fatalf("handoff: %d %s", code, errb.String())
	}
	want := "Handoff of attempt att-1 of team team-1, pushed 2026-10-24T05:30:00Z:\n\n# Handoff\n\n### Next\n\nOpen the pull request.\n"
	if *asked != "att-1" || out.String() != want {
		t.Fatalf("asked for %q, printed %q, want %q", *asked, out.String(), want)
	}

	out.Reset()
	if code := run(ctx, []string{"attempts", "handoff", "--id", "att-1", "--json"}, &out, &errb); code != 0 {
		t.Fatalf("handoff --json: %d %s", code, errb.String())
	}
	var got opapi.AttemptHandoff
	if err := json.Unmarshal(out.Bytes(), &got); err != nil || got.Handoff != doc || !got.UpdatedAt.Equal(at) {
		t.Fatalf("handoff --json: %s %v", out.String(), err)
	}

	serveHandoff(t, opapi.AttemptHandoff{AttemptID: "att-2", TeamID: "team-1"})
	out.Reset()
	if code := run(ctx, []string{"attempts", "handoff", "--id", "att-2"}, &out, &errb); code != 0 ||
		out.String() != "Attempt att-2 of team team-1 has no handoff.\n" {
		t.Fatalf("no handoff: %d %q %s", code, out.String(), errb.String())
	}

	for _, args := range [][]string{{"attempts"}, {"attempts", "show"}, {"attempts", "handoff"}} {
		errb.Reset()
		if code := run(ctx, args, &out, &errb); code != 2 || !strings.Contains(errb.String(), "aicrew attempts handoff --id") {
			t.Errorf("%v: %d %s", args, code, errb.String())
		}
	}
}
