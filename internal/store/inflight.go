package store

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"
)

// ErrInProgress refuses a command while an identical one (same caller,
// operation, scope and key) is still running and the wait bound has passed.
// Nothing has changed; the caller retries with the same key.
var ErrInProgress = errors.New("request_in_progress")

// defaultInflightWait bounds how long an identical command waits for the
// one in flight before ErrInProgress.
const defaultInflightWait = 30 * time.Second

// inflight serializes identical commands that call out before their
// transaction, such as identity proof. The one holding an entry runs its
// whole command: receipt lookup, pre-checks, verification and commit. An
// identical command waits and then starts from its receipt lookup, which
// sees the holder's receipt if it committed, because the lookup begins after
// the commit returned. Different keys and callers never wait on each other.
//
// Entries live in memory, so the guarantee covers commands through one
// Store; a database file is served by one Store.
type inflight struct {
	mu      sync.Mutex
	entries map[string]*flight
}

type flight struct {
	done    chan struct{}
	waiters int
}

// acquire takes the entry for c and cmd, waiting for an identical command in
// flight until it finishes, ctx ends or maxWait passes. The returned release
// must be called exactly once.
func (f *inflight) acquire(ctx context.Context, c Caller, cmd command, maxWait time.Duration) (func(), error) {
	key := fmt.Sprintf("%q %q %q %q %q", c.kind.String(), c.id, cmd.op, cmd.scope, cmd.key)
	var timeout <-chan time.Time
	for {
		f.mu.Lock()
		if f.entries == nil {
			f.entries = map[string]*flight{}
		}
		cur, busy := f.entries[key]
		if !busy {
			own := &flight{done: make(chan struct{})}
			f.entries[key] = own
			f.mu.Unlock()
			return func() {
				f.mu.Lock()
				delete(f.entries, key)
				f.mu.Unlock()
				close(own.done)
			}, nil
		}
		cur.waiters++
		f.mu.Unlock()
		if timeout == nil {
			t := time.NewTimer(maxWait)
			defer t.Stop()
			timeout = t.C
		}
		var err error
		select {
		case <-cur.done:
		case <-ctx.Done():
			err = ctx.Err()
		case <-timeout:
			err = fmt.Errorf("%s key %q: %w", cmd.op, cmd.key, ErrInProgress)
		}
		f.mu.Lock()
		cur.waiters--
		f.mu.Unlock()
		if err != nil {
			return nil, err
		}
	}
}
