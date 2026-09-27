package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"sync"
)

// Aimem is aimem's client commands for an aicrew team session
// (aimem's DESIGN-AIFORGE-CONTEXT.md, "E5a local session binding" and
// "E5b"). Secrets pass only on stdin or a stdout pipe, never in arguments.
type Aimem interface {
	// Proof obtains a proof receipt for a challenge, read from a pipe.
	Proof(ctx context.Context, serviceID, hubID, challengeID string) (string, error)
	// Open binds a new session file to the handle and returns its path.
	Open(ctx context.Context, serviceID, teamID, sessionID, handle string) (string, error)
	// Refresh replaces the session file's handle.
	Refresh(ctx context.Context, sessionID, handle string) error
	// Close drops the session file once aimem's hub confirms the session
	// has ended; aimem keeps it otherwise and Close fails.
	Close(ctx context.Context, sessionID string) error
	// Status reports whether aimem holds a file for the session, and its
	// path.
	Status(ctx context.Context, sessionID string) (path string, open bool, err error)
}

// ExecAimem runs the aimem executable.
type ExecAimem struct {
	Command string
	Hub     string // aimem's hub name; empty for its default
}

func (a ExecAimem) hubArgs() []string {
	if a.Hub == "" {
		return nil
	}
	return []string{"--hub-name", a.Hub}
}

// run executes one aimem command. stdin, when set, carries a secret; stdout
// is read through a pipe. aimem's own error text, never a secret, explains a
// failure.
func (a ExecAimem) run(ctx context.Context, stdin string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, a.Command, args...)
	if stdin != "" {
		cmd.Stdin = strings.NewReader(stdin + "\n")
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		msg := strings.TrimSpace(stderr.String())
		if len(msg) > 512 {
			msg = msg[:512]
		}
		var exit *exec.ExitError
		if errors.As(err, &exit) {
			return "", fmt.Errorf("aimem %s failed: %s", args[0]+" "+args[1], msg)
		}
		return "", fmt.Errorf("aimem could not be run: %w", err)
	}
	return string(out), nil
}

func (a ExecAimem) Proof(ctx context.Context, serviceID, hubID, challengeID string) (string, error) {
	out, err := a.run(ctx, "", append([]string{"identity", "proof", "--peer", serviceID, "--hub-id", hubID,
		"--challenge", challengeID}, a.hubArgs()...)...)
	if err != nil {
		return "", err
	}
	receipt := strings.TrimSpace(out)
	if !strings.HasPrefix(receipt, "amr1_") || strings.ContainsAny(receipt, " \n") {
		return "", errors.New("aimem identity proof gave no receipt")
	}
	return receipt, nil
}

func (a ExecAimem) Open(ctx context.Context, serviceID, teamID, sessionID, handle string) (string, error) {
	out, err := a.run(ctx, handle, append([]string{"team-session", "open", "--service", serviceID, "--team", teamID,
		"--session", sessionID}, a.hubArgs()...)...)
	if err != nil {
		return "", err
	}
	path := strings.TrimSpace(out)
	if path == "" || strings.Contains(path, "\n") {
		return "", errors.New("aimem team-session open printed no path")
	}
	return path, nil
}

func (a ExecAimem) Refresh(ctx context.Context, sessionID, handle string) error {
	_, err := a.run(ctx, handle, "team-session", "refresh", sessionID)
	return err
}

func (a ExecAimem) Close(ctx context.Context, sessionID string) error {
	_, err := a.run(ctx, "", "team-session", "close", sessionID)
	return err
}

func (a ExecAimem) Status(ctx context.Context, sessionID string) (string, bool, error) {
	cmd := exec.CommandContext(ctx, a.Command, "team-session", "status", sessionID)
	out, err := cmd.Output()
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		return "", false, nil // not open here, or unusable
	}
	if err != nil {
		return "", false, fmt.Errorf("aimem could not be run: %w", err)
	}
	var st struct {
		Path string `json:"path"`
	}
	if json.Unmarshal(out, &st) != nil || st.Path == "" {
		return "", false, errors.New("aimem team-session status gave no path")
	}
	return st.Path, true, nil
}

// serialAimem runs aimem's lifecycle commands for one session one at a time:
// an open, a refresh, a close or a status for a session waits for any other
// in flight for the same session, so a close issued during a refresh runs
// only once the refresh has finished. Overlapping them could let a close
// drop the file a refresh has just written. Different sessions do not wait
// on each other; a proof is bound to no session and is not serialized.
type serialAimem struct {
	Aimem
	mu    sync.Mutex
	locks map[string]*sync.Mutex
}

// Serialize wraps a so that its lifecycle commands never overlap for one
// session.
func Serialize(a Aimem) Aimem {
	if s, ok := a.(*serialAimem); ok {
		return s
	}
	return &serialAimem{Aimem: a, locks: map[string]*sync.Mutex{}}
}

func (s *serialAimem) lock(sessionID string) func() {
	s.mu.Lock()
	l, ok := s.locks[sessionID]
	if !ok {
		l = &sync.Mutex{}
		s.locks[sessionID] = l
	}
	s.mu.Unlock()
	l.Lock()
	return l.Unlock
}

func (s *serialAimem) Open(ctx context.Context, serviceID, teamID, sessionID, handle string) (string, error) {
	defer s.lock(sessionID)()
	return s.Aimem.Open(ctx, serviceID, teamID, sessionID, handle)
}

func (s *serialAimem) Refresh(ctx context.Context, sessionID, handle string) error {
	defer s.lock(sessionID)()
	return s.Aimem.Refresh(ctx, sessionID, handle)
}

func (s *serialAimem) Close(ctx context.Context, sessionID string) error {
	defer s.lock(sessionID)()
	return s.Aimem.Close(ctx, sessionID)
}

func (s *serialAimem) Status(ctx context.Context, sessionID string) (string, bool, error) {
	defer s.lock(sessionID)()
	return s.Aimem.Status(ctx, sessionID)
}
