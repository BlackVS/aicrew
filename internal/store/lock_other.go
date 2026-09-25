//go:build !(darwin || dragonfly || freebsd || linux || netbsd || openbsd || windows)

package store

import (
	"errors"
	"os"
	"runtime"
)

// Without an exclusive file lock the one-Store rule cannot be enforced, so
// Open refuses rather than run unprotected.
func lockFile(*os.File) error {
	return errors.New("store locking is not supported on " + runtime.GOOS)
}

func unlockFile(*os.File) error { return nil }
