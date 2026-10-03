package main

import (
	"os/exec"
	"strings"
	"testing"
)

// aicrew is a client of the operator API: it never links the store or its
// database driver, so it cannot open a store whatever its flags.
func TestNoStoreLinked(t *testing.T) {
	gobin, err := exec.LookPath("go")
	if err != nil {
		t.Skip("the go tool is not on PATH")
	}
	out, err := exec.Command(gobin, "list", "-deps", ".").Output()
	if err != nil {
		t.Fatalf("go list: %v", err)
	}
	for _, pkg := range strings.Fields(string(out)) {
		if pkg == "github.com/BlackVS/aicrew/internal/store" || strings.HasPrefix(pkg, "modernc.org/sqlite") {
			t.Fatalf("aicrew links %s", pkg)
		}
	}
	if !strings.Contains(string(out), "github.com/BlackVS/aicrew/internal/opclient") {
		t.Fatal("aicrew does not link the operator API's client")
	}
}
