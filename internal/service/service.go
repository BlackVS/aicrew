// Package service runs a member's turn loop (aicrew-agent serve) as a
// service of the member's own account, so that it outlives the terminal it
// was started from and starts again at the next login: a systemd user unit
// on Linux, a launchd agent on macOS and a logon task on Windows.
//
// Each agent home has its own service. A small per-user registry lists the
// homes that have one, so that the member one-liner can stop them all
// before it swaps the binaries and start them again after.
//
// The serve lock (state/serve.lock) tells a running serve apart from a
// stopped one whoever started it: serve holds it for its whole life and
// records its PID beside it. A serve that holds it while the home's service
// is not running was started by hand; it is reported, never duplicated.
package service

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/BlackVS/aicrew/internal/filelock"
)

// NoServiceEnv, set to 1, makes the member one-liner and join leave the
// services alone.
const NoServiceEnv = "AICREW_NO_SERVICE"

// Disabled reports whether NoServiceEnv asks to leave the services alone.
func Disabled(getenv func(string) string) bool { return getenv(NoServiceEnv) == "1" }

// Spec is one home's service: what it runs, and as what.
type Spec struct {
	Name  string   // the service's name, from Name
	Label string   // the home's label, for the description
	Home  string   // the absolute agent home
	Exe   string   // the absolute aicrew-agent it runs
	Args  []string // the arguments after Exe
	// Path is the PATH the service runs with where the manager would give
	// it its own (systemd, launchd): serve finds claude and aimem on it. ""
	// leaves the manager's.
	Path string
}

// NewSpec is the service that runs `exe serve --home HOME --log LOG` for
// the home.
func NewSpec(home, label, exe, path string) Spec {
	return Spec{Name: Name(home, label), Label: label, Home: home, Exe: exe,
		Args: []string{"serve", "--home", home, "--log", LogPath(home)}, Path: path}
}

// LogPath is the log a service's serve writes.
func LogPath(home string) string { return filepath.Join(home, "logs", "serve.log") }

// Name is the home's service name: aicrew-member-<label>-<8 hex digits of
// the home path's hash>, so two homes never share one and the label says
// whose it is.
func Name(home, label string) string {
	key := filepath.Clean(home)
	if runtime.GOOS == "windows" {
		key = strings.ToLower(key)
	}
	sum := sha256.Sum256([]byte(key))
	return "aicrew-member-" + slug(label) + "-" + hex.EncodeToString(sum[:4])
}

// slug keeps a label's letters and digits, lower-cased, and turns every
// other run into one hyphen: a name every manager accepts.
func slug(label string) string {
	var b strings.Builder
	dash := false
	for _, r := range strings.ToLower(label) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			if dash && b.Len() > 0 {
				b.WriteByte('-')
			}
			dash = false
			b.WriteRune(r)
			if b.Len() >= 32 {
				break
			}
			continue
		}
		dash = true
	}
	if b.Len() == 0 {
		return "home"
	}
	return b.String()
}

// The serve lock.

// lockPath and pidPath are the serve lock and the PID its holder records.
func lockPath(home string) string { return filepath.Join(home, "state", "serve.lock") }
func pidPath(home string) string  { return filepath.Join(home, "state", "serve.pid") }

// holdWait bounds HoldServe's wait: Serving holds the lock for a moment
// when it looks, and a serve starting then must not take it for another's.
const holdWait = 2 * time.Second

// ServingError is HoldServe's refusal: another serve of the home runs.
type ServingError struct {
	Home string
	PID  int // 0 when its PID is unknown
}

func (e *ServingError) Error() string {
	if e.PID == 0 {
		return "another aicrew-agent serve is running for " + e.Home
	}
	return fmt.Sprintf("another aicrew-agent serve is running for %s (pid %d)", e.Home, e.PID)
}

// HoldServe takes the home's serve lock for this process and records its
// PID, or answers a *ServingError when another serve holds it. release
// drops both.
func HoldServe(home string) (release func(), err error) {
	ctx, cancel := context.WithTimeout(context.Background(), holdWait)
	defer cancel()
	unlock, err := filelock.Lock(ctx, lockPath(home))
	if errors.Is(err, context.DeadlineExceeded) {
		return nil, &ServingError{Home: home, PID: readPID(home)}
	}
	if err != nil {
		return nil, err
	}
	if err := os.WriteFile(pidPath(home), []byte(strconv.Itoa(os.Getpid())+"\n"), 0o600); err != nil {
		unlock()
		return nil, err
	}
	return func() {
		_ = os.Remove(pidPath(home))
		unlock()
	}, nil
}

// Serving reports whether a serve holds the home's serve lock, and its PID
// (0 when unknown). A home without a state directory runs none.
func Serving(home string) (pid int, serving bool, err error) {
	if _, err := os.Stat(filepath.Join(home, "state")); errors.Is(err, os.ErrNotExist) {
		return 0, false, nil
	}
	unlock, err := filelock.TryLock(lockPath(home))
	if errors.Is(err, filelock.ErrBusy) {
		return readPID(home), true, nil
	}
	if err != nil {
		return 0, false, err
	}
	unlock()
	return 0, false, nil
}

func readPID(home string) int {
	b, err := os.ReadFile(pidPath(home))
	if err != nil {
		return 0
	}
	n, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil || n <= 0 {
		return 0
	}
	return n
}
