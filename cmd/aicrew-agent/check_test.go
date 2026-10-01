package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/BlackVS/aicrew/internal/version"
)

func TestCheckCommandUsage(t *testing.T) {
	for _, args := range [][]string{nil, {"-home"}, {"-home", "x", "extra"}, {"-bogus"}} {
		var out, errb bytes.Buffer
		if code := check(context.Background(), args, &out, &errb); code != exitUsage {
			t.Errorf("%v: exit %d", args, code)
		}
	}
	var out, errb bytes.Buffer
	if code := check(context.Background(), []string{"-home", filepath.Join(t.TempDir(), "none")}, &out, &errb); code != exitFailed ||
		!strings.Contains(errb.String(), "not an agent home") {
		t.Fatalf("a missing home: exit %d, %s", code, errb.String())
	}
	// An unknown client on an existing home is a usage error (exit 2), not
	// a failed check.
	home := t.TempDir()
	os.WriteFile(filepath.Join(home, "agent.json"), []byte(`{"layout": 1, "label": "builder", "aicrew": {}}`), 0o600)
	errb.Reset()
	if code := check(context.Background(), []string{"-home", home, "-client", "cursor"}, &out, &errb); code != exitUsage ||
		!strings.Contains(errb.String(), `unknown client "cursor"`) {
		t.Fatalf("an unknown client: exit %d, %s", code, errb.String())
	}
}

func TestVersionCommand(t *testing.T) {
	var out, errb bytes.Buffer
	if code := versionCmd([]string{"-json"}, &out, &errb); code != exitOK {
		t.Fatalf("exit %d", code)
	}
	var i version.Info
	if err := json.Unmarshal(out.Bytes(), &i); err != nil || i.Version == "" || i.Go == "" {
		t.Fatalf("%s: %v", out.String(), err)
	}
	out.Reset()
	defer func(o string) { version.Override = o }(version.Override)
	version.Override = "v1.2.3"
	versionCmd(nil, &out, &errb)
	if !strings.HasPrefix(out.String(), "aicrew-agent v1.2.3") {
		t.Fatalf("%q", out.String())
	}
	if code := versionCmd([]string{"extra"}, &out, &errb); code != exitUsage {
		t.Fatalf("usage: exit %d", code)
	}
}
