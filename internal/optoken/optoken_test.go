package optoken

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/BlackVS/aicrew/internal/privatefile"
)

func TestGenerateWriteRead(t *testing.T) {
	a, err := Generate()
	if err != nil || !Valid(a) {
		t.Fatalf("generated %q, %v", a, err)
	}
	if b, _ := Generate(); b == a {
		t.Fatal("two tokens are equal")
	}
	path := filepath.Join(t.TempDir(), "operator.token")
	if err := Write(path, a); err != nil {
		t.Fatal(err)
	}
	if got, err := Read(path); err != nil || got != a {
		t.Fatalf("read %q, %v", got, err)
	}
	// An existing file is never replaced.
	if err := Write(path, a); err == nil {
		t.Fatal("Write replaced an existing file")
	}
}

// A file that is not one generated token is refused, and the error never
// quotes the content.
func TestReadRefusals(t *testing.T) {
	dir := t.TempDir()
	tok, _ := Generate()
	for name, content := range map[string]string{
		"empty":     "",
		"weak":      "aop_secret\n",
		"two lines": tok + "\n" + tok + "\n",
		"spaces":    " " + tok + "\n",
		"upper hex": "aop_" + strings.ToUpper(tok[4:]) + "\n",
	} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(dir, strings.ReplaceAll(name, " ", "-"))
			f, err := privatefile.Create(path)
			if err != nil {
				t.Fatal(err)
			}
			f.WriteString(content)
			f.Close()
			_, err = Read(path)
			if err == nil || (content != "" && strings.Contains(err.Error(), strings.TrimSpace(content))) {
				t.Fatalf("%q: %v", content, err)
			}
		})
	}
	if _, err := Read(filepath.Join(dir, "missing")); err == nil {
		t.Fatal("a missing file was read")
	}
	if err := Write(filepath.Join(dir, "bad"), "aop_weak"); err == nil {
		t.Fatal("a malformed token was written")
	}
}

// A file another account can read is refused.
func TestReadRefusesSharedFile(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the Windows ACL check is covered by privatefile's tests")
	}
	path := filepath.Join(t.TempDir(), "operator.token")
	tok, _ := Generate()
	if err := os.WriteFile(path, []byte(tok+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Read(path); err == nil {
		t.Fatal("a world-readable token file was read")
	}
}
