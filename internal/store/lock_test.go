package store

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

func mustOpen(t *testing.T, path string) *Store {
	t.Helper()
	s, err := Open(context.Background(), path)
	if err != nil {
		t.Fatalf("open %s: %v", path, err)
	}
	return s
}

func wantInUse(t *testing.T, path string) {
	t.Helper()
	s, err := Open(context.Background(), path)
	if err == nil {
		s.Close()
	}
	if !errors.Is(err, ErrStoreInUse) {
		t.Fatalf("open %s: got %v, want ErrStoreInUse", path, err)
	}
}

func TestSecondOpenRefusedUntilClose(t *testing.T) {
	path := filepath.Join(t.TempDir(), "aicrew.db")
	s := mustOpen(t, path)
	mustAgent(t, s, "a1", "builder")
	wantInUse(t, path)
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s2 := mustOpen(t, path)
	defer s2.Close()
	if n := count(t, s2, "agents"); n != 1 {
		t.Fatalf("agents after reopen = %d, want 1", n)
	}
}

// A refused Open touches no database file.
func TestRefusedOpenTouchesNothing(t *testing.T) {
	path := filepath.Join(t.TempDir(), "aicrew.db")
	lock, err := lockStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.release()
	wantInUse(t, path)
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("refused open created the database: %v", err)
	}
}

func TestSimultaneousOpensOneWins(t *testing.T) {
	path := filepath.Join(t.TempDir(), "aicrew.db")
	const n = 8
	stores := make([]*Store, n)
	errs := make([]error, n)
	var wg sync.WaitGroup
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			stores[i], errs[i] = Open(context.Background(), path)
		}()
	}
	wg.Wait()
	wins := 0
	for i, err := range errs {
		switch {
		case err == nil:
			wins++
			defer stores[i].Close()
		case !errors.Is(err, ErrStoreInUse):
			t.Errorf("open %d: %v", i, err)
		}
	}
	if wins != 1 {
		t.Fatalf("%d opens succeeded, want 1", wins)
	}
}

// The sidecar file stays after Close; only a held lock refuses.
func TestLeftoverLockFileDoesNotBlock(t *testing.T) {
	path := filepath.Join(t.TempDir(), "aicrew.db")
	if err := os.WriteFile(path+".lock", []byte("left by a crashed process"), 0o600); err != nil {
		t.Fatal(err)
	}
	s := mustOpen(t, path)
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path + ".lock"); err != nil {
		t.Fatalf("lock file after close: %v", err)
	}
	mustOpen(t, path).Close()
}

func TestEquivalentPathsShareLock(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "aicrew.db")
	s := mustOpen(t, path)
	defer s.Close()
	sep := string(filepath.Separator)

	wantInUse(t, dir+sep+"."+sep+"aicrew.db")
	wantInUse(t, dir+sep+"sub"+sep+".."+sep+"aicrew.db")

	t.Run("relative", func(t *testing.T) {
		t.Chdir(dir)
		wantInUse(t, "aicrew.db")
	})
	t.Run("case variant", func(t *testing.T) {
		upper := filepath.Join(dir, "AICREW.DB")
		if _, err := os.Stat(upper); err != nil {
			t.Skip("case-sensitive file system")
		}
		wantInUse(t, upper)
	})
	links := t.TempDir()
	t.Run("symlinked file", func(t *testing.T) {
		link := filepath.Join(links, "link.db")
		if err := os.Symlink(path, link); err != nil {
			skipUnlessCI(t, err)
		}
		wantInUse(t, link)
	})
	t.Run("symlinked directory", func(t *testing.T) {
		link := filepath.Join(links, "linkdir")
		if err := os.Symlink(dir, link); err != nil {
			skipUnlessCI(t, err)
		}
		wantInUse(t, filepath.Join(link, "aicrew.db"))
	})
	t.Run("junction", func(t *testing.T) {
		if runtime.GOOS != "windows" {
			t.Skip("Windows only")
		}
		link := filepath.Join(links, "junction")
		if out, err := exec.Command("cmd", "/c", "mklink", "/J", link, dir).CombinedOutput(); err != nil {
			t.Fatalf("mklink /J: %v: %s", err, out)
		}
		wantInUse(t, filepath.Join(link, "aicrew.db"))
	})
}

// skipUnlessCI skips a test the local account cannot set up, such as a
// symlink on Windows without the privilege; CI must run it.
func skipUnlessCI(t *testing.T, err error) {
	t.Helper()
	if os.Getenv("CI") != "" {
		t.Fatalf("cannot set up on CI: %v", err)
	}
	t.Skipf("cannot set up locally: %v", err)
}

// A symlink to a database that does not exist yet: the first Open creates
// the target through the link, and every spelling must still share its lock.
func TestDanglingSymlinkSharesLock(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "real.db")
	link := filepath.Join(dir, "link.db")
	if err := os.Symlink(target, link); err != nil {
		skipUnlessCI(t, err)
	}
	s := mustOpen(t, link)
	defer s.Close()
	wantInUse(t, link)
	wantInUse(t, target)
}

// A process that holds the store and then ends, by exiting without Close or
// by being killed, leaves nothing that blocks the next Open.
func TestLockEndsWithHoldingProcess(t *testing.T) {
	for _, mode := range []string{"exit", "kill"} {
		t.Run(mode, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "aicrew.db")
			cmd := exec.Command(os.Args[0], "-test.run=^TestHelperHoldStore$")
			cmd.Env = append(os.Environ(), "AICREW_HOLD_STORE="+path, "AICREW_HOLD_MODE="+mode)
			stdin, err := cmd.StdinPipe()
			if err != nil {
				t.Fatal(err)
			}
			stdout, err := cmd.StdoutPipe()
			if err != nil {
				t.Fatal(err)
			}
			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}
			defer cmd.Process.Kill() //nolint:errcheck // cleanup after a failure
			line, err := bufio.NewReader(stdout).ReadString('\n')
			if err != nil || strings.TrimSpace(line) != "held" {
				t.Fatalf("child: %q, %v", line, err)
			}
			wantInUse(t, path)
			if mode == "kill" {
				if err := cmd.Process.Kill(); err != nil {
					t.Fatal(err)
				}
			} else {
				stdin.Close()
			}
			cmd.Wait() //nolint:errcheck // a killed child reports its signal
			// The operating system releases the lock as the process ends;
			// allow it a moment rather than assume the release is instant.
			deadline := time.Now().Add(5 * time.Second)
			for {
				s, err := Open(context.Background(), path)
				if err == nil {
					s.Close()
					return
				}
				if !errors.Is(err, ErrStoreInUse) || time.Now().After(deadline) {
					t.Fatalf("open after the holder ended: %v", err)
				}
				time.Sleep(10 * time.Millisecond)
			}
		})
	}
}

// TestHelperHoldStore is the child process of TestLockEndsWithHoldingProcess:
// it opens the store, reports it, and never closes it.
func TestHelperHoldStore(t *testing.T) {
	path := os.Getenv("AICREW_HOLD_STORE")
	if path == "" {
		t.Skip("helper process only")
	}
	if _, err := Open(context.Background(), path); err != nil {
		fmt.Println("error:", err)
		os.Exit(1)
	}
	fmt.Println("held")
	if os.Getenv("AICREW_HOLD_MODE") == "exit" {
		// Wait for the parent's signal, then exit without Close.
		bufio.NewReader(os.Stdin).ReadString('\n') //nolint:errcheck // EOF is the signal
		os.Exit(0)
	}
	time.Sleep(time.Hour) // killed by the parent
}
