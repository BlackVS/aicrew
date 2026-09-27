//go:build !(darwin || dragonfly || freebsd || linux || netbsd || openbsd || windows)

package filelock

import (
	"errors"
	"os"
	"runtime"
)

// Without an exclusive file lock the exclusion cannot be kept, so Lock
// refuses rather than run unprotected.
func tryLock(*os.File) error {
	return errors.New("file locking is not supported on " + runtime.GOOS)
}

func unlock(*os.File) error { return nil }
