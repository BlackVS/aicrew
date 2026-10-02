package agent

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/BlackVS/aicrew/internal/filelock"
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
	// Home is the agent home whose aimem installation every call uses
	// (D-STORE), whatever the caller's environment names.
	Home string
}

// environ is the environment of an aimem call: the caller's, with the
// home's installation and the given team session file, or none: a session
// file the caller inherited belongs to another installation.
func (a ExecAimem) environ(sessionFile string) []string {
	env := os.Environ()
	if a.Home != "" {
		env = HomeAimemEnv(env, a.Home)
	}
	if sessionFile != "" {
		return ScopedEnv(env, sessionFile)
	}
	return withoutEnv(env, SessionEnv)
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
	cmd.Env = a.environ("")
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
	cmd.Env = a.environ("")
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

// serialAimem runs aimem's lifecycle commands for one session one at a time,
// across processes: an open, a refresh, a close or a status for a session
// holds an exclusive lock on a file named for that session under the agent
// home, and waits while another holds it. A close issued during a refresh,
// by this process or another client of the same agent home, therefore runs
// only once the refresh has finished. Overlapping them could let a close
// drop the file a refresh has just written, or a late refresh recreate a
// binding a close has just removed. Different sessions do not wait on each
// other; a proof is bound to no session and is not serialized.
type serialAimem struct {
	Aimem
	dir string
}

// Serialize wraps a so that its lifecycle commands never overlap for one
// session, with the locks under lockDir (the agent home's state/locks).
func Serialize(a Aimem, lockDir string) Aimem {
	if s, ok := a.(*serialAimem); ok {
		return s
	}
	return &serialAimem{Aimem: a, dir: lockDir}
}

func (s *serialAimem) lock(ctx context.Context, sessionID string) (func(), error) {
	if err := os.MkdirAll(s.dir, 0o700); err != nil {
		return nil, err
	}
	sum := sha256.Sum256([]byte(sessionID))
	return filelock.Lock(ctx, filepath.Join(s.dir, "aimem-"+hex.EncodeToString(sum[:8])+".lock"))
}

func (s *serialAimem) Open(ctx context.Context, serviceID, teamID, sessionID, handle string) (string, error) {
	unlock, err := s.lock(ctx, sessionID)
	if err != nil {
		return "", err
	}
	defer unlock()
	return s.Aimem.Open(ctx, serviceID, teamID, sessionID, handle)
}

func (s *serialAimem) Refresh(ctx context.Context, sessionID, handle string) error {
	unlock, err := s.lock(ctx, sessionID)
	if err != nil {
		return err
	}
	defer unlock()
	return s.Aimem.Refresh(ctx, sessionID, handle)
}

func (s *serialAimem) Close(ctx context.Context, sessionID string) error {
	unlock, err := s.lock(ctx, sessionID)
	if err != nil {
		return err
	}
	defer unlock()
	return s.Aimem.Close(ctx, sessionID)
}

func (s *serialAimem) Status(ctx context.Context, sessionID string) (string, bool, error) {
	unlock, err := s.lock(ctx, sessionID)
	if err != nil {
		return "", false, err
	}
	defer unlock()
	return s.Aimem.Status(ctx, sessionID)
}

// Reservation runs `aimem reservation ARGS…` under the team session file,
// with stdin (a body, which may carry a proof) on a pipe. It returns aimem's
// exit code and its one JSON document; only a failure to run aimem at all is
// an error.
func (a ExecAimem) Reservation(ctx context.Context, sessionFile string, stdin []byte, args ...string) (int, []byte, error) {
	cmd := exec.CommandContext(ctx, a.Command, append([]string{"reservation"}, args...)...)
	cmd.Env = a.environ(sessionFile)
	cmd.Stdin = bytes.NewReader(stdin)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	var exit *exec.ExitError
	switch {
	case errors.As(err, &exit):
		return exit.ExitCode(), out, nil
	case err != nil:
		return 0, nil, fmt.Errorf("aimem could not be run: %w", err)
	}
	return 0, out, nil
}

// GetTask reads a task through `aimem mcp` under the team session file: the
// same binding, context header and online verification as the reservation
// CLI, which reads the task the same way.
func (a ExecAimem) GetTask(ctx context.Context, sessionFile, taskID string) (TaskDoc, error) {
	cmd := exec.CommandContext(ctx, a.Command, "mcp")
	cmd.Env = a.environ(sessionFile)
	var in bytes.Buffer
	for _, m := range []map[string]any{
		{"jsonrpc": "2.0", "id": 1, "method": "initialize", "params": map[string]any{"protocolVersion": "2024-11-05",
			"capabilities": map[string]any{}, "clientInfo": map[string]any{"name": "aicrew-agent", "version": "1"}}},
		{"jsonrpc": "2.0", "method": "notifications/initialized"},
		{"jsonrpc": "2.0", "id": 2, "method": "tools/call", "params": map[string]any{"name": "get_task",
			"arguments": map[string]any{"id": taskID}}},
	} {
		b, _ := json.Marshal(m)
		in.Write(append(b, '\n'))
	}
	cmd.Stdin = &in
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil && len(out) == 0 {
		return TaskDoc{}, fmt.Errorf("aimem mcp could not be run: %w", err)
	}
	for _, line := range bytes.Split(out, []byte("\n")) {
		var msg struct {
			ID     json.RawMessage `json:"id"`
			Result struct {
				Content []struct {
					Text string `json:"text"`
				} `json:"content"`
				IsError bool `json:"isError"`
			} `json:"result"`
			Error *struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		if json.Unmarshal(line, &msg) != nil || string(msg.ID) != "2" {
			continue
		}
		if msg.Error != nil || msg.Result.IsError || len(msg.Result.Content) == 0 {
			return TaskDoc{}, errors.New("aimem could not read the task")
		}
		var doc TaskDoc
		if err := json.Unmarshal([]byte(msg.Result.Content[0].Text), &doc.Fields); err != nil {
			return TaskDoc{}, errors.New("aimem's task is not the expected JSON")
		}
		if err := json.Unmarshal(doc.Fields["revision"], &doc.Revision); err != nil || doc.Revision < 1 {
			return TaskDoc{}, errors.New("aimem's task has no revision")
		}
		return doc, nil
	}
	return TaskDoc{}, errors.New("aimem mcp gave no answer to get_task")
}
