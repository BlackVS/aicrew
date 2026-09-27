//go:build !windows

package privatefiletest

import "os"

// Expose gives every local account read access to path.
func Expose(path string) error { return os.Chmod(path, 0o644) }
