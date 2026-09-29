//go:build !windows

package privatefiletest

import "os"

// Expose gives every local account read access to path.
func Expose(path string) error { return os.Chmod(path, 0o644) }

// ExposeTraverse lets every local account traverse the directory path,
// reaching what it contains by name, but not list it.
func ExposeTraverse(path string) error { return os.Chmod(path, 0o711) }
