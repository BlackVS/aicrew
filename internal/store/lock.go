package store

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sync"
)

// ErrStoreInUse refuses opening a database file that another Store, in this
// or another process, has open.
var ErrStoreInUse = errors.New("store_in_use")

// A database file is served by one Store: identical in-flight commands are
// coordinated in memory (inflight.go). Open enforces this with an exclusive
// operating-system lock on a sidecar file, <database path>.lock, held until
// Close. The lock belongs to the open file, so a second Open conflicts even
// within one process, and the operating system drops it when the process
// ends, however it ends: nothing is left to clean up by hand.
//
// The sidecar file itself is never removed and its presence means nothing.
// Removing it on Close would let a concurrent Open lock a new file with the
// same name while another opener still holds the old one.
//
// The sidecar path comes from the database path with symbolic links
// resolved, so every spelling of one file shares one lock; the file system
// resolves relative paths, directory links and, on case-insensitive volumes,
// case. Hard links to the database and
// network file systems are not supported.
type storeLock struct {
	f    *os.File
	once sync.Once
	err  error
}

func lockStore(path string) (*storeLock, error) {
	lp, err := lockPath(path)
	if err != nil {
		return nil, err
	}
	f, err := os.OpenFile(lp, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open store lock: %w", err)
	}
	if err := lockFile(f); err != nil {
		f.Close()
		if errors.Is(err, ErrStoreInUse) {
			return nil, fmt.Errorf("%w: %s", ErrStoreInUse, path)
		}
		return nil, fmt.Errorf("lock store: %w", err)
	}
	return &storeLock{f: f}, nil
}

// release unlocks and closes the sidecar file. Later calls do nothing.
func (l *storeLock) release() error {
	l.once.Do(func() {
		l.err = errors.Join(unlockFile(l.f), l.f.Close())
	})
	return l.err
}

// lockPath returns the sidecar path for the database at path: the database
// file with links resolved if it exists, otherwise its resolved directory.
// A relative result is fine: the lock file is opened once, right away.
func lockPath(path string) (string, error) {
	real, err := filepath.EvalSymlinks(path)
	if errors.Is(err, fs.ErrNotExist) {
		var dir string
		if dir, err = filepath.EvalSymlinks(filepath.Dir(path)); err == nil {
			real = filepath.Join(dir, filepath.Base(path))
		}
	}
	if err != nil {
		return "", fmt.Errorf("resolve store path: %w", err)
	}
	return real + ".lock", nil
}
