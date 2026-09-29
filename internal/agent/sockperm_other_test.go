//go:build !windows

package agent

import (
	"os"
	"testing"
)

func assertSocketPrivate(t *testing.T, path string) {
	t.Helper()
	info, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm()&0o077 != 0 {
		t.Fatalf("socket mode %v", info.Mode())
	}
}
