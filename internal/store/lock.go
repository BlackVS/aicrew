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
// Every spelling of one database file shares one lock: the sidecar is named
// after the file the path leads to, following a final symbolic link even to
// a database not created yet, and the operating system resolves the rest of
// the path (relative parts, directory links, "..", and case on
// case-insensitive volumes) exactly as it does for the database. Hard links
// to the database and network file systems are not supported.
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

// lockPath returns the sidecar path for the database at path. It is the
// path as given, never cleaned here, so the operating system resolves its
// directories, directory links and ".." the same way when it opens the
// sidecar as when it opens the database. Only a link in the final component
// is followed here, since the sidecar must be named after the file the link
// leads to, even one the first Open has not created yet.
func lockPath(path string) (string, error) {
	p := path
	for range maxLinkHops {
		fi, err := os.Lstat(p)
		if errors.Is(err, fs.ErrNotExist) || (err == nil && fi.Mode()&fs.ModeSymlink == 0) {
			return p + ".lock", nil
		}
		if err != nil {
			return "", fmt.Errorf("resolve store path: %w", err)
		}
		target, err := os.Readlink(p)
		if err != nil {
			return "", fmt.Errorf("resolve store path: %w", err)
		}
		if !filepath.IsAbs(target) {
			dir, _ := filepath.Split(p) // the raw directory, as the link sees it
			target = dir + target
		}
		p = target
	}
	return "", fmt.Errorf("%w: store path has too many symbolic links", ErrInvalid)
}

// maxLinkHops bounds link following, as operating systems do.
const maxLinkHops = 40
