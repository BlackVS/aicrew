package agent

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// State is the nonsecret recovery record of the agent's current team
// session, kept in the agent home's state/ directory so that a restarted
// client resumes the session instead of starting another. It never holds a
// token, receipt or handle.
type State struct {
	Version   int       `json:"version"`
	AgentID   string    `json:"agent_id"`
	TeamID    string    `json:"team_id"`
	ServiceID string    `json:"service_id"`
	HubID     string    `json:"hub_id"`
	SessionID string    `json:"session_id"`
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

// SaveState replaces the record atomically.
func SaveState(home string, s State) error {
	s.Version = 1
	dir := filepath.Dir(statePath(home))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
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
	err := os.Remove(statePath(home))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}
