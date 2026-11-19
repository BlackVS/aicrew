package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/BlackVS/aicrew/internal/filelock"
)

// State is the nonsecret recovery record of the agent's current team
// session, kept in the agent home's state/ directory so that a restarted
// client resumes the session instead of starting another. It never holds a
// token, receipt or handle.
type State struct {
	Version   int    `json:"version"`
	AgentID   string `json:"agent_id"`
	TeamID    string `json:"team_id"`
	ServiceID string `json:"service_id"`
	HubID     string `json:"hub_id"`
	SessionID string `json:"session_id"`
	// Role is the member's role in the team at the session's last entry or
	// resume.
	Role      string    `json:"role,omitempty"`
	AimemFile string    `json:"aimem_session_file,omitempty"`
	UpdatedAt time.Time `json:"updated_at"`
}

func statePath(home string) string { return filepath.Join(home, "state", "aicrew-session.json") }

// LoadState reads the record; ok is false when there is none.
func LoadState(home string) (State, bool, error) {
	raw, err := os.ReadFile(statePath(home))
	if errors.Is(err, os.ErrNotExist) {
		return State{}, false, nil
	}
	if err != nil {
		return State{}, false, err
	}
	var s State
	if err := json.Unmarshal(raw, &s); err != nil || s.Version != 1 || s.SessionID == "" {
		return State{}, false, fmt.Errorf("%s is not a session record", statePath(home))
	}
	return s, true, nil
}

// recordLockPath is the lock every writer of the record takes: SaveState,
// ClearState and ClearStateOf. The engine was the record's only writer;
// join, ending its own conversation, is a second (joinskills.go).
func recordLockPath(home string) string { return filepath.Join(home, "state", "aicrew-session.lock") }

// recordLockWait bounds the wait for the record's lock. The lock is held for
// one write of a record under 1 KiB and a rename, or one read and a remove:
// milliseconds. Five seconds is far beyond that on a slow disk, and short
// enough that a stuck holder is reported, not waited out.
const recordLockWait = 5 * time.Second

// lockRecord takes the record's lock. It is a leaf: held only for the file
// operation on the record, never across a network call, a child process or
// another lock, so it cannot be part of a lock cycle with the home's locks
// or the serve lock.
func lockRecord(home string) (func(), error) {
	path := recordLockPath(home)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), recordLockWait)
	defer cancel()
	unlock, err := filelock.Lock(ctx, path)
	if errors.Is(err, context.DeadlineExceeded) {
		return nil, fmt.Errorf("%s: another aicrew process has held the home's session record for over %s", path, recordLockWait)
	}
	return unlock, err
}

// SaveState replaces the record atomically.
func SaveState(home string, s State) error {
	s.Version = 1
	dir := filepath.Dir(statePath(home))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	unlock, err := lockRecord(home)
	if err != nil {
		return err
	}
	defer unlock()
	raw, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".aicrew-session-*.json")
	if err != nil {
		return err
	}
	_, werr := tmp.Write(append(raw, '\n'))
	cerr := tmp.Close()
	if werr != nil || cerr != nil {
		os.Remove(tmp.Name())
		return errors.Join(werr, cerr)
	}
	if err := os.Rename(tmp.Name(), statePath(home)); err != nil {
		os.Remove(tmp.Name())
		return err
	}
	return nil
}

// ClearState removes the record once the session has ended.
func ClearState(home string) error {
	if _, err := os.Stat(filepath.Dir(statePath(home))); errors.Is(err, os.ErrNotExist) {
		return nil
	}
	unlock, err := lockRecord(home)
	if err != nil {
		return err
	}
	defer unlock()
	err = os.Remove(statePath(home))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}

// ClearStateOf removes the record only while it names session: read,
// compared and removed under the record's lock, so a record another writer
// saved meanwhile stays in its place throughout.
func ClearStateOf(home, session string) error {
	if _, err := os.Stat(filepath.Dir(statePath(home))); errors.Is(err, os.ErrNotExist) {
		return nil
	}
	unlock, err := lockRecord(home)
	if err != nil {
		return err
	}
	defer unlock()
	st, ok, err := LoadState(home)
	if err != nil || !ok || st.SessionID != session {
		return err
	}
	err = os.Remove(statePath(home))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}
