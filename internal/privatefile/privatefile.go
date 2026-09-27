// Package privatefile creates and checks files for secrets. Create makes a
// file exclusively (an existing path is never reused or truncated) and
// readable by its owner only from the moment it exists. Check refuses a file
// that another local account could read. On Unix a private file's mode is
// 0600. On Windows its DACL is protected (it inherits nothing) and grants
// access to the current user, SYSTEM and the Administrators group only.
package privatefile

import "os"

// Create creates path, which must not exist, for writing a secret.
func Create(path string) (*os.File, error) { return create(path) }

// Check refuses path unless it is a regular file that only its owner can
// read: mode bits on Unix, the effective access control list on Windows.
// Its errors name the path and a fix, never the file's content.
func Check(path string) error { return check(path) }
