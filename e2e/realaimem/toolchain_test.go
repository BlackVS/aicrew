//go:build realaimem

package realaimem

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// T1: the toolchain of a worker's turn (pilot finding #18). The operator
// selects dev-go for the team's project with `aicrew team toolchain`;
// aicrewd carries it on the projects route, and the worker's launcher
// records it in the home's team record as its session starts, which the
// turn loop composes the worker's per-turn settings from. A clear takes it
// away at the next start. The e2e runs no headless client, so the turn's
// settings file itself is the agent tests' and the real probe's
// (docs/TURN-PROBE.md).
func (h *harness) t1Toolchain(t *testing.T) {
	sc := h.report.scenario(t, "T1")
	worker := h.members["worker"]
	aicrew := filepath.Join(h.bin, "aicrew")
	r := h.run(h.opEnv, nil, aicrew, "team", "toolchain", "--team-name", "e2e", "--project", projectID, "--toolchain", "dev-cobol")
	sc.check("aicrew refuses a toolchain it does not know", r.code == 2 && strings.Contains(r.stderr, "is not one of dev-go"), r.stderr)
	var team struct {
		Toolchains map[string]string `json:"toolchains"`
	}
	out := h.must(h.opEnv, nil, aicrew, "team", "toolchain", "--team-name", "e2e", "--project", projectID, "--toolchain", "dev-go")
	sc.check("the operator selects dev-go for the project", json.Unmarshal([]byte(out), &team) == nil &&
		team.Toolchains[projectID] == "dev-go", out)

	h.crashLauncher(worker)
	h.restartLauncher(worker)
	sc.check("the worker's session start records the project's toolchain", eventually(30*time.Second, func() bool { return recordedToolchain(worker.home) == "dev-go" }), recordedToolchain(worker.home))

	h.must(h.opEnv, nil, aicrew, "team", "toolchain", "--team-name", "e2e", "--project", projectID, "--clear")
	h.crashLauncher(worker)
	h.restartLauncher(worker)
	sc.check("cleared, the next start records none", eventually(30*time.Second, func() bool { return recordedToolchain(worker.home) == "" }), recordedToolchain(worker.home))
	sc.finish()
}

// eventually reports whether cond holds within limit.
func eventually(limit time.Duration, cond func() bool) bool {
	for deadline := time.Now().Add(limit); !cond(); time.Sleep(200 * time.Millisecond) {
		if time.Now().After(deadline) {
			return false
		}
	}
	return true
}

// recordedToolchain is the toolchain the home's team record carries for the
// e2e project, or "".
func recordedToolchain(home string) string {
	raw, err := os.ReadFile(filepath.Join(home, "state", "team.json"))
	if err != nil {
		return ""
	}
	var rec struct {
		Projects []struct {
			ProjectID string `json:"project_id"`
			Toolchain string `json:"toolchain"`
		} `json:"projects"`
	}
	if json.Unmarshal(raw, &rec) != nil {
		return ""
	}
	for _, p := range rec.Projects {
		if p.ProjectID == projectID {
			return p.Toolchain
		}
	}
	return ""
}
