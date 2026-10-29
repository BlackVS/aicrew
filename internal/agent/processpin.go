package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"regexp"
	"strings"
)

// HubPin is a project's current process selection as an offer pins it,
// as aimem's `process pin --json` prints it: the process repository, the
// full commit, the manifest, the whole set's digest, and the instruction
// digest (`sha256:` and the SHA-256 of the manifest file's bytes).
type HubPin struct {
	Project           string `json:"project"`
	Repository        string `json:"repository"`
	Commit            string `json:"commit"`
	Manifest          string `json:"manifest"`
	SetDigest         string `json:"set_digest"`
	InstructionDigest string `json:"instruction_digest"`
}

var (
	fullCommit        = regexp.MustCompile(`^[0-9a-f]{40}$`)
	instructionDigest = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)
)

// check reports what makes the pin unfit for an offer of the project.
func (p HubPin) check(project string) error {
	switch {
	case p.Project != project:
		return fmt.Errorf("aimem answered for project %q, not %q", p.Project, project)
	case p.Repository == "" || p.Manifest == "":
		return errors.New("aimem's pin names no repository or manifest")
	case !fullCommit.MatchString(p.Commit):
		return fmt.Errorf("aimem's pin commit %q is not a full commit", p.Commit)
	case !instructionDigest.MatchString(p.InstructionDigest):
		return fmt.Errorf("aimem's instruction digest %q is not sha256: and 64 hex digits", p.InstructionDigest)
	}
	return nil
}

// ProcessPin reads the project's current pin from the hub through the
// home's aimem installation and its own credential, from the home's root,
// as the client's own aimem calls run. aimem's error text says why there
// is none: no selection, a selection without a stored set, or a refused
// credential.
func (a ExecAimem) ProcessPin(ctx context.Context, project string) (HubPin, error) {
	cmd := exec.CommandContext(ctx, a.Command, "process", "pin", "-p", project, "--json")
	cmd.Env = a.environ("")
	if a.Home != "" {
		cmd.Dir = absHome(a.Home)
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		var exit *exec.ExitError
		if errors.As(err, &exit) {
			return HubPin{}, fmt.Errorf("aimem process pin failed: %s", tail(strings.TrimSpace(stderr.String()), 512))
		}
		return HubPin{}, fmt.Errorf("aimem could not be run: %w", err)
	}
	var pin HubPin
	if err := json.Unmarshal(out, &pin); err != nil {
		return HubPin{}, fmt.Errorf("aimem process pin printed no pin: %w", err)
	}
	if err := pin.check(project); err != nil {
		return HubPin{}, err
	}
	return pin, nil
}

// PinResolution says what FillProcess did with the body.
type PinResolution struct {
	Project string `json:"project"`
	Pin     HubPin `json:"pin"`
	Kept    bool   `json:"kept,omitempty"` // the body named its own pin and digest
}

// FillProcess fills an offer's `process` pin and `instruction_digest` from
// the hub's current selection for the body's task.project_id, read with
// read. A body naming both is kept as it is, without a read; one naming
// only one of them is refused.
func FillProcess(ctx context.Context, body []byte, read func(context.Context, string) (HubPin, error)) ([]byte, PinResolution, error) {
	var res PinResolution
	fields := map[string]json.RawMessage{}
	if len(bytes.TrimSpace(body)) > 0 {
		if err := json.Unmarshal(body, &fields); err != nil || fields == nil {
			return nil, res, errors.New("the body is not a JSON object")
		}
	}
	_, hasProcess := fields["process"]
	_, hasDigest := fields["instruction_digest"]
	if hasProcess && hasDigest {
		res.Kept = true
		return body, res, nil
	}
	if hasProcess || hasDigest {
		// The hub's digest need not be the digest of the body's own pin.
		return nil, res, errors.New("the body names only one of process and instruction_digest: name both, or neither to fill both from the hub")
	}
	var task struct {
		ProjectID string `json:"project_id"`
	}
	if raw, ok := fields["task"]; ok {
		_ = json.Unmarshal(raw, &task)
	}
	if task.ProjectID == "" {
		return nil, res, errors.New("the body names no task.project_id, so there is no project to read the process pin for")
	}
	res.Project = task.ProjectID
	pin, err := read(ctx, task.ProjectID)
	if err != nil {
		return nil, res, err
	}
	res.Pin = pin
	fields["process"], _ = json.Marshal(map[string]string{"repo": pin.Repository, "commit": pin.Commit, "manifest": pin.Manifest})
	fields["instruction_digest"], _ = json.Marshal(pin.InstructionDigest)
	out, err := json.Marshal(fields)
	return out, res, err
}
