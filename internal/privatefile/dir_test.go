package privatefile

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/BlackVS/aicrew/internal/privatefile/privatefiletest"
)

// MakeDir makes a directory only its owner can reach, restricts an existing
// one the same way, and CheckDir refuses one another account can read or
// traverse.
func TestMakeDir(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "home", "state")
	if err := MakeDir(dir); err != nil {
		t.Fatal(err)
	}
	if err := CheckDir(dir); err != nil {
		t.Fatalf("a directory made by MakeDir: %v", err)
	}
	assertPrivateDir(t, dir)
	// What it will contain (the step socket) gets the same protection
	// without being set on its own.
	assertInherited(t, dir)

	if err := privatefiletest.Expose(dir); err != nil {
		t.Fatal(err)
	}
	if err := CheckDir(dir); err == nil {
		t.Fatal("accepted a directory every account can read")
	}
	if err := MakeDir(dir); err != nil {
		t.Fatal(err)
	}
	if err := CheckDir(dir); err != nil {
		t.Fatalf("MakeDir did not restrict an existing directory: %v", err)
	}

	if err := privatefiletest.ExposeTraverse(dir); err != nil {
		t.Fatal(err)
	}
	if err := CheckDir(dir); err == nil {
		t.Fatal("accepted a directory every account can traverse")
	}

	file := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := CheckDir(file); err == nil {
		t.Fatal("accepted a file as a directory")
	}
	if err := CheckDir(filepath.Join(dir, "missing")); !os.IsNotExist(err) {
		t.Fatalf("missing directory: got %v", err)
	}
}
