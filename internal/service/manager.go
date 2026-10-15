package service

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"
)

// Manager keeps the homes' services: the backend's registrations, the
// registry, and the serve lock that tells whose serve runs.
type Manager struct {
	Backend  Backend
	Registry Registry
	// Kill stops a serve's process by its PID, where stopping the service
	// leaves it running (Backend.OrphansServe). Default: os.Process.Kill.
	Kill func(pid int) error
	// StopWait bounds the wait for a stopped service's serve to end.
	StopWait time.Duration
	poll     time.Duration
}

// Status is a home's service as it stands.
type Status struct {
	Name      string   `json:"name"`
	Home      string   `json:"home"`
	Kind      string   `json:"manager"`
	Installed bool     `json:"installed"`
	Active    bool     `json:"running"`
	ServePID  int      `json:"serve_pid,omitempty"`  // the service's serve
	HandPID   int      `json:"hand_pid,omitempty"`   // a serve started by hand
	HandServe bool     `json:"hand_serve,omitempty"` // a serve started by hand runs (its PID may be unknown)
	Notes     []string `json:"notes,omitempty"`
}

// State is the status in one word or phrase, as check and status print it.
func (s Status) State() string {
	switch {
	case s.HandServe && s.HandPID != 0:
		return fmt.Sprintf("started by hand, pid %d", s.HandPID)
	case s.HandServe:
		return "started by hand"
	case !s.Installed:
		return "not installed"
	case s.Active && s.ServePID != 0:
		return fmt.Sprintf("running, pid %d", s.ServePID)
	case s.Active:
		return "running"
	}
	return "stopped"
}

// HandStartedError refuses to start a service beside a serve started by
// hand: two would fence each other's session.
type HandStartedError struct {
	Home string
	PID  int
}

func (e *HandStartedError) Error() string {
	who := "an aicrew-agent serve started by hand"
	if e.PID != 0 {
		who += fmt.Sprintf(" (pid %d)", e.PID)
	}
	return who + " is running for " + e.Home + "; stop it (Ctrl-C in its terminal), then run " +
		"`aicrew-agent service start --home " + e.Home + "`: from then on the service runs it"
}

// ErrNotInstalled is the answer for a home without a service.
var ErrNotInstalled = errors.New("this home has no service")

func (m *Manager) stopWait() time.Duration {
	if m.StopWait > 0 {
		return m.StopWait
	}
	return 75 * time.Second // beyond the unit's TimeoutStopSec
}

func (m *Manager) pollEvery() time.Duration {
	if m.poll > 0 {
		return m.poll
	}
	return 200 * time.Millisecond
}

// name is the home's registered service name, or the one its label gives.
func (m *Manager) name(home, label string) (string, error) {
	e, ok, err := m.Registry.Lookup(home)
	if err != nil {
		return "", err
	}
	if ok {
		return e.Name, nil
	}
	return Name(home, label), nil
}

// Status reports the home's service.
func (m *Manager) Status(ctx context.Context, home, label string) (Status, error) {
	name, err := m.name(home, label)
	if err != nil {
		return Status{}, err
	}
	return m.status(ctx, home, name)
}

func (m *Manager) status(ctx context.Context, home, name string) (Status, error) {
	st := Status{Name: name, Home: home, Kind: m.Backend.Kind()}
	installed, active, err := m.Backend.State(ctx, name)
	if err != nil {
		return st, err
	}
	st.Installed, st.Active = installed, active
	pid, serving, err := Serving(home)
	if err != nil {
		return st, err
	}
	switch {
	case serving && active:
		st.ServePID = pid
	case serving:
		st.HandServe, st.HandPID = true, pid
	}
	return st, nil
}

// Install writes and registers the home's service and starts it, or
// refreshes and starts an installed one: the definition is rendered again
// from spec, so a moved binary or a new PATH takes effect. A serve started
// by hand is reported, and nothing is changed.
func (m *Manager) Install(ctx context.Context, spec Spec) (Status, error) {
	old, had, err := m.Registry.Lookup(spec.Home)
	if err != nil {
		return Status{}, err
	}
	// Whose serve runs is judged by the service as registered: a changed
	// label renames it below.
	current := spec.Name
	if had {
		current = old.Name
	}
	st, err := m.status(ctx, spec.Home, current)
	if err != nil {
		return st, err
	}
	if st.HandServe {
		return st, &HandStartedError{Home: spec.Home, PID: st.HandPID}
	}
	if had && old.Name != spec.Name {
		// The label changed: the old name's service goes first.
		if err := m.stop(ctx, spec.Home, old.Name); err != nil {
			return st, err
		}
		if err := m.Backend.Remove(ctx, old.Name); err != nil {
			return st, err
		}
	}
	if err := m.Backend.Install(ctx, spec); err != nil {
		return st, err
	}
	if err := m.Registry.Put(Entry{Home: spec.Home, Label: spec.Label, Name: spec.Name}); err != nil {
		return st, err
	}
	var notes []string
	if err := m.Backend.Start(ctx, spec.Name); errors.Is(err, ErrNoLoginSession) {
		notes = append(notes, err.Error())
	} else if err != nil {
		return st, err
	}
	st, err = m.status(ctx, spec.Home, spec.Name)
	st.Notes = append(notes, m.Backend.Notes(ctx)...)
	return st, err
}

// Start starts an installed service, refreshing its definition from spec
// first, as Install does.
func (m *Manager) Start(ctx context.Context, spec Spec) (Status, error) {
	if _, ok, err := m.Registry.Lookup(spec.Home); err != nil {
		return Status{}, err
	} else if !ok {
		st, _ := m.status(ctx, spec.Home, spec.Name)
		return st, ErrNotInstalled
	}
	return m.Install(ctx, spec)
}

// Stop stops the home's service and waits for its serve to end. A serve
// started by hand is left running and reported in the status.
func (m *Manager) Stop(ctx context.Context, home, label string) (Status, error) {
	name, err := m.name(home, label)
	if err != nil {
		return Status{}, err
	}
	if err := m.stop(ctx, home, name); err != nil {
		st, _ := m.status(ctx, home, name)
		return st, err
	}
	return m.status(ctx, home, name)
}

func (m *Manager) stop(ctx context.Context, home, name string) error {
	_, active, err := m.Backend.State(ctx, name)
	if err != nil || !active {
		return err
	}
	if err := m.Backend.Stop(ctx, name); err != nil {
		return err
	}
	pid, serving, err := Serving(home)
	if err != nil || !serving {
		return err
	}
	if m.Backend.OrphansServe() && pid != 0 {
		kill := m.Kill
		if kill == nil {
			kill = killPID
		}
		if err := kill(pid); err != nil {
			return fmt.Errorf("stop the service's serve (pid %d): %w", pid, err)
		}
	}
	deadline := time.Now().Add(m.stopWait())
	for {
		pid, serving, err := Serving(home)
		if err != nil || !serving {
			return err
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("the service %s was stopped, but its serve (pid %d) still runs", name, pid)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(m.pollEvery()):
		}
	}
}

// Uninstall stops the home's service and removes it.
func (m *Manager) Uninstall(ctx context.Context, home, label string) (Status, error) {
	name, err := m.name(home, label)
	if err != nil {
		return Status{}, err
	}
	if err := m.stop(ctx, home, name); err != nil {
		return Status{}, err
	}
	if err := m.Backend.Remove(ctx, name); err != nil {
		return Status{}, err
	}
	if err := m.Registry.Remove(home); err != nil {
		return Status{}, err
	}
	return m.status(ctx, home, name)
}

func killPID(pid int) error {
	p, err := os.FindProcess(pid)
	if err != nil {
		return err
	}
	return p.Kill()
}
