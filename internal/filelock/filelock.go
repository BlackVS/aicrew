// Package filelock takes an exclusive operating-system lock on a file,
// waiting for it: flock on Unix, LockFileEx on Windows. The lock belongs to
// one open of the file, so it excludes other processes and other opens in
// this process alike, and the operating system releases it when the process
// ends however it ends.
package filelock

import (
	"context"
	"errors"
	"os"
	"time"
)

// errBusy is a platform's "held elsewhere" answer to a lock attempt.
var errBusy = errors.New("locked")

// poll is how often a waiting Lock tries again.
const poll = 20 * time.Millisecond

// Lock opens (creating if needed) the file at path and waits until it holds
// an exclusive lock on it, or ctx ends. The returned function unlocks and
// closes it.
func Lock(ctx context.Context, path string) (func(), error) {
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, err
	}
	for {
		err := tryLock(f)
		if err == nil {
			return func() {
				_ = unlock(f)
				f.Close()
			}, nil
		}
		if !errors.Is(err, errBusy) {
			f.Close()
			return nil, err
		}
		t := time.NewTimer(poll)
		select {
		case <-ctx.Done():
			t.Stop()
			f.Close()
			return nil, ctx.Err()
		case <-t.C:
		}
	}
}
