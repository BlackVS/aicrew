package main

import (
	"bytes"
	"context"
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
