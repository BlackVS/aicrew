package filelock

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"
)

// A second lock of the same file, even from this process, waits for the
// first to be released; ctx ends a wait.
func TestLockExcludes(t *testing.T) {
	path := filepath.Join(t.TempDir(), "session.lock")
	unlock, err := Lock(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if _, err := Lock(ctx, path); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("a held lock was taken again: %v", err)
	}
	got := make(chan func(), 1)
	go func() {
		u, err := Lock(context.Background(), path)
		if err != nil {
			t.Error(err)
		}
		got <- u
	}()
	select {
	case <-got:
		t.Fatal("the lock was taken while held")
	case <-time.After(100 * time.Millisecond):
	}
	unlock()
	select {
	case u := <-got:
		u()
	case <-time.After(5 * time.Second):
		t.Fatal("the waiting lock was never granted")
	}
	// Another file is independent.
	u, err := Lock(context.Background(), filepath.Join(t.TempDir(), "other.lock"))
	if err != nil {
		t.Fatal(err)
	}
	u()
}
