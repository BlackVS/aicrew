package privatefile

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/BlackVS/aicrew/internal/privatefile/privatefiletest"
)

func TestCheck(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "secret")
	f, err := Create(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString("secret-value\n"); err != nil {
		t.Fatal(err)
	}
	f.Close()
	if err := Check(path); err != nil {
		t.Fatalf("a file made by Create: %v", err)
	}

	if err := privatefiletest.Expose(path); err != nil {
		t.Fatal(err)
	}
	err = Check(path)
	if err == nil {
		t.Fatal("accepted a file every account can read")
	}
	if strings.Contains(err.Error(), "secret-value") {
		t.Fatalf("the refusal quotes the content: %v", err)
	}

	if err := Check(dir); err == nil {
		t.Fatal("accepted a directory")
	}
	if err := Check(filepath.Join(dir, "missing")); !os.IsNotExist(err) {
		t.Fatalf("missing file: got %v", err)
	}
}
