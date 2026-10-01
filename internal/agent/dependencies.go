package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

// Dependency evidence before an offer (1aad G1; docs/CREW-CONTRACT.md,
// "Cross-project references"). Before a coordinator's offer is begun, the
// driver reads the offered task's dependencies over the member's own aimem
// connection. A dependency it cannot read counts as open: unknown is not
// DONE. An open dependency refuses the offer locally, before any begin; a
// task whose dependencies are all DONE is offered with the evidence of that
// read, which aicrewd records in the offer's audit. The read only avoids a
// useless offer: it never makes a task eligible, and aimem's refusal of the
// claim stays authoritative.

// DependencyEvidence is one dependency as the driver read it.
type DependencyEvidence struct {
	TaskID   string `json:"task_id"`
	State    string `json:"state"`
	Revision int64  `json:"revision"`
}

// DependenciesOpen refuses an offer whose task has a dependency that is not
// DONE, or that could not be read.
type DependenciesOpen struct {
	Open []string // "ID (state)", or "ID (unreadable)"
}

func (e *DependenciesOpen) Error() string {
	return "the task has open dependencies: " + strings.Join(e.Open, ", ")
}

// Dependencies reads taskID's dependencies over the member's aimem
// connection and returns their evidence, or *DependenciesOpen.
func (d *Driver) Dependencies(ctx context.Context, taskID string) ([]DependencyEvidence, error) {
	file := d.Session.stepAimemFile()
	task, err := d.Aimem.GetTask(ctx, file, taskID)
	if err != nil {
		return nil, &DependenciesOpen{Open: []string{taskID + " (the offered task is unreadable)"}}
	}
	var ids []string
	if raw := task.Fields["dependencies"]; len(raw) > 0 && string(raw) != "null" {
		if err := json.Unmarshal(raw, &ids); err != nil {
			return nil, &DependenciesOpen{Open: []string{taskID + " (its dependencies are unreadable)"}}
		}
	}
	evidence := []DependencyEvidence{}
	var open []string
	for _, id := range ids {
		dep, err := d.Aimem.GetTask(ctx, file, id)
		if err != nil {
			open = append(open, id+" (unreadable)")
			continue
		}
		var state string
		if json.Unmarshal(dep.Fields["state"], &state) != nil || state != "DONE" {
			open = append(open, fmt.Sprintf("%s (%s)", id, orUnknown(state)))
			continue
		}
		evidence = append(evidence, DependencyEvidence{TaskID: id, State: state, Revision: dep.Revision})
	}
	if len(open) > 0 {
		return nil, &DependenciesOpen{Open: open}
	}
	return evidence, nil
}

func orUnknown(s string) string {
	if s == "" {
		return "unknown"
	}
	return s
}

// withDependencies sets the offer body's dependency_evidence to evidence,
// replacing any the caller supplied: only the driver's own read is evidence.
func withDependencies(body json.RawMessage, evidence []DependencyEvidence) (json.RawMessage, error) {
	var b map[string]json.RawMessage
	if err := json.Unmarshal(body, &b); err != nil {
		return nil, fmt.Errorf("the offer's body: %w", err)
	}
	ev, err := json.Marshal(evidence)
	if err != nil {
		return nil, err
	}
	b["dependency_evidence"] = ev
	return json.Marshal(b)
}
