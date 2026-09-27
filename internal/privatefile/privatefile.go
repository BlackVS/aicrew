// Package privatefile creates files for secrets: exclusively (an existing
// path is never reused or truncated) and readable by their owner only from
// the moment they exist. On Unix the file's mode is 0600. On Windows its DACL
// is protected (it inherits nothing) and grants access to the current user,
// SYSTEM and the Administrators group only.
package privatefile

import "os"

// Create creates path, which must not exist, for writing a secret.
func Create(path string) (*os.File, error) { return create(path) }
