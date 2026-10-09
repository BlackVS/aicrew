package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// `aicrew architect init` writes the directory with no connection, exits 1
// on a conflict to merge, and 2 on a usage error.
func TestArchitectInit(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "arch")
	call := func(args ...string) (int, string, string) {
		var out, errb bytes.Buffer
		code := run(context.Background(), args, &out, &errb)
		return code, out.String(), errb.String()
	}
	for _, args := range [][]string{{"architect"}, {"architect", "init"}, {"architect", "init", "--dir", dir},
		{"architect", "init", "--project", "p"}, {"architect", "other", "--dir", dir, "--project", "p"}} {
		if code, _, _ := call(args...); code != 2 {
			t.Fatalf("%v: exit %d, want 2", args, code)
		}
	}
	code, out, errs := call("architect", "init", "--dir", dir, "--project", "crew-app", "--project", "crew-ops")
	if code != 0 || !strings.Contains(out, `"status": "ready"`) || !strings.Contains(errs, "ready") {
		t.Fatalf("init: %d %s %s", code, out, errs)
	}
	if _, err := os.Stat(filepath.Join(dir, "docs", "ARCHITECT.md")); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(dir, "CLAUDE.md"), []byte("mine\n"), 0o644)
	code, out, errs = call("architect", "init", "--dir", dir, "--project", "crew-app")
	if code != 1 || !strings.Contains(out, `"status": "conflict"`) || !strings.Contains(errs, ".aicrew-new") {
		t.Fatalf("conflict: %d %s %s", code, out, errs)
	}
}

// The operator issues an architect credential to a file; with that file as
// its token file, aicrew reads and answers escalations and is refused
// everything else; once revoked, it is refused everywhere.
func TestArchitectCredentialCommands(t *testing.T) {
	s := serve(t)
	call := func(args ...string) (int, string, string) {
		var out, errb bytes.Buffer
		code := run(context.Background(), args, &out, &errb)
		return code, out.String(), errb.String()
	}
	if code, _, _ := call("architect", "credential", "issue", "--label", "planning"); code != 2 {
		t.Fatalf("issue without --output: exit %d, want 2", code)
	}
	credFile := filepath.Join(t.TempDir(), "aicrew.architect")
	code, out, errs := call("architect", "credential", "issue", "--label", "planning", "--output", credFile)
	if code != 0 || !strings.Contains(out, `"label": "planning"`) || strings.Contains(out+errs, "aar_") {
		t.Fatalf("issue: %d %q %q", code, out, errs)
	}
	b, err := os.ReadFile(credFile)
	if err != nil || !strings.HasPrefix(string(b), "aar_") {
		t.Fatalf("the credential file: %q %v", b, err)
	}
	code, out, _ = call("architect", "credential", "list")
	if code != 0 || !strings.Contains(out, `"active": true`) || strings.Contains(out, "aar_") {
		t.Fatalf("list: %d %s", code, out)
	}
	var list []struct {
		ID string `json:"id"`
	}
	_ = json.Unmarshal([]byte(out), &list)

	t.Setenv("AICREW_OPERATOR_TOKEN_FILE", credFile)
	if code, out, errs := call("escalations", "list", "--open"); code != 0 || strings.TrimSpace(out) != "[]" {
		t.Fatalf("the architect lists: %d %s %s", code, out, errs)
	}
	if code, _, errs := call("escalations", "answer", "--id", "no-such", "--decision", "d", "--rationale", "r"); code != 1 ||
		!strings.Contains(errs, "not_found") {
		t.Fatalf("an unknown escalation: %d %s", code, errs)
	}
	if code, _, errs := call("team", "list"); code != 1 || !strings.Contains(errs, "unauthorized") {
		t.Fatalf("the architect reaches the team routes: %d %s", code, errs)
	}
	if code, _, errs := call("architect", "credential", "list"); code != 1 || !strings.Contains(errs, "unauthorized") {
		t.Fatalf("the architect lists credentials: %d %s", code, errs)
	}
	for _, args := range [][]string{{"escalations"}, {"escalations", "show"}, {"escalations", "answer", "--id", "x"},
		{"escalations", "list", "--team-name", "crew"}} {
		if code, _, _ := call(args...); code != 2 {
			t.Fatalf("%v: exit %d, want 2", args, code)
		}
	}

	t.Setenv("AICREW_OPERATOR_TOKEN_FILE", s.tokenFile)
	if code, _, errs := call("architect", "credential", "revoke", "--id", list[0].ID); code != 0 {
		t.Fatalf("revoke: %d %s", code, errs)
	}
	t.Setenv("AICREW_OPERATOR_TOKEN_FILE", credFile)
	if code, _, errs := call("escalations", "list"); code != 1 || !strings.Contains(errs, "unauthorized") {
		t.Fatalf("a revoked credential: %d %s", code, errs)
	}
}
