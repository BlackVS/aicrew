package agent

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

// The toolchain lines of check on a worker's home (task 01a1f86e-70b9): a
// project with a repository and no toolchain warns, a selected toolchain
// whose tool is missing blocks, both in place is ready; another role's
// home, a project without a repository and a record without a role have no
// line.
func TestCheckToolchains(t *testing.T) {
	e := setupCheck(t, readyTools, "aimem", "claude")
	e.installSkills(t, "1.26.1", "oh-code-review")
	if rep := e.check(t, "claude"); rep.Status != JoinRestartRequired {
		t.Fatalf("first check %+v", rep)
	}
	found, saved := true, lookTool
	t.Cleanup(func() { lookTool = saved })
	lookTool = func(name string) (string, error) {
		if name != "go" {
			t.Errorf("looked up %q", name)
		}
		if found {
			return "/usr/bin/go", nil
		}
		return "", errors.New("not found")
	}
	team := func(role, toolchain string, repo bool) {
		t.Helper()
		p := map[string]any{"hub_id": "hub-1", "project_id": "aicrew", "toolchain": toolchain, "repository": nil}
		if repo {
			p["repository"] = map[string]string{"kind": "github", "url": "https://github.com/o/aicrew", "access": "write"}
		}
		raw, _ := json.Marshal(map[string]any{"version": 1, "team_id": "team-7", "role": role, "projects": []any{p}})
		var rec TeamRecord
		if err := json.Unmarshal(raw, &rec); err != nil {
			t.Fatal(err)
		}
		if err := SaveTeam(e.home, rec); err != nil {
			t.Fatal(err)
		}
	}

	team("worker", "", true)
	rep := e.check(t)
	if rep.Status != JoinReady || len(rep.Warnings) != 1 || len(rep.Instructions) != 0 {
		t.Fatalf("no toolchain: %+v", rep)
	}
	for _, want := range []string{"project aicrew has no toolchain selected",
		"`aicrew team toolchain --team team-7 --project aicrew --toolchain dev-go`",
		"`aicrew-agent service restart --home " + e.home + "`"} {
		if !strings.Contains(rep.Warnings[0], want) {
			t.Errorf("the warning lacks %q: %s", want, rep.Warnings[0])
		}
	}

	team("worker", "dev-go", true)
	found = false
	rep = e.check(t)
	if rep.Status != JoinBlocked || rep.Reason != ReasonToolchainToolMissing || len(rep.Warnings) != 0 ||
		len(rep.Instructions) != 1 || !strings.Contains(rep.Instructions[0], "install `go` on this host") ||
		!strings.Contains(rep.Instructions[0], "aicrew-agent service restart --home "+e.home) {
		t.Fatalf("tool missing: %+v", rep)
	}

	found = true
	if rep = e.check(t); rep.Status != JoinReady || len(rep.Warnings) != 0 || len(rep.Instructions) != 0 {
		t.Fatalf("both in place: %+v", rep)
	}

	for _, c := range []struct {
		name, role string
		repo       bool
	}{{"a coordinator's home", "coordinator", true}, {"a project without a repository", "worker", false},
		{"a record without a role", "", true}} {
		team(c.role, "", c.repo)
		found = false
		if rep = e.check(t); rep.Status != JoinReady || len(rep.Warnings) != 0 {
			t.Errorf("%s: %+v", c.name, rep)
		}
	}
}
