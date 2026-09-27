package privatefile

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
)

// Create makes a new file and never reuses an existing path.
func TestCreateIsExclusive(t *testing.T) {
	path := filepath.Join(t.TempDir(), "secret")
	f, err := Create(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString("secret-value\n"); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	if b, err := os.ReadFile(path); err != nil || string(b) != "secret-value\n" {
		t.Fatalf("content = %q, %v", b, err)
	}
	if _, err := Create(path); !errors.Is(err, fs.ErrExist) {
		t.Fatalf("creating an existing path: got %v, want it refused", err)
	}
	if b, _ := os.ReadFile(path); string(b) != "secret-value\n" {
		t.Fatalf("the existing file was changed: %q", b)
	}
	if _, err := Create(filepath.Join(t.TempDir(), "missing-dir", "secret")); err == nil {
		t.Fatal("created a file in a missing directory")
	}
	assertPrivate(t, path)
}
