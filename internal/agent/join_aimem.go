package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"strings"
)

// CredentialStatus is `aimem hub credential <hub> --json`: whether this
// installation holds an individual credential for the hub and what the hub
// says about it. It carries no part of a secret.
type CredentialStatus struct {
	Hub        string `json:"hub"`
	Credential string `json:"credential"` // set, none or other
	State      string `json:"state"`      // active, refused, unreachable or absent
	Scope      string `json:"scope,omitempty"`
	UserID     string `json:"user_id,omitempty"`
	TokenID    string `json:"token_id,omitempty"`
	Detail     string `json:"detail,omitempty"`
}

// JoinAimem returns the bootstrap's view of the aimem executable.
func (a ExecAimem) JoinAimem() JoinAimem { return joinExec{a} }

type joinExec struct{ ExecAimem }

// Credential always names the hub. An aimem without the command answers
// with its hub usage, which means "unknown"; without a name, such an aimem
// would take `hub credential --json` for `hub <url> <token>` and overwrite
// its default hub.
func (e joinExec) Credential(ctx context.Context) (CredentialStatus, bool, error) {
	if e.Hub == "" {
		return CredentialStatus{}, false, errors.New("the aimem hub name is required")
	}
	cmd := exec.CommandContext(ctx, e.Command, "hub", "credential", e.Hub, "--json")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	msg := strings.TrimSpace(stderr.String())
	var exit *exec.ExitError
	switch {
	case errors.As(err, &exit) && strings.HasPrefix(msg, "usage: aimem hub"):
		return CredentialStatus{}, false, nil
	case errors.As(err, &exit):
		if len(msg) > 512 {
			msg = msg[:512]
		}
		return CredentialStatus{}, false, fmt.Errorf("aimem hub credential failed: %s", msg)
	case err != nil:
		return CredentialStatus{}, false, fmt.Errorf("aimem could not be run: %w", err)
	}
	// The command ran and answered: an answer that is not a status confirms
	// nothing, so it stops the run like any other failure.
	var st CredentialStatus
	if json.Unmarshal(out, &st) != nil || st.Credential == "" || st.State == "" {
		return CredentialStatus{}, false, errors.New("aimem hub credential gave no credential status")
	}
	return st, true, nil
}
