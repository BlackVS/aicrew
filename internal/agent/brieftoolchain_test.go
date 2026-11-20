package agent

import (
	"strings"
	"testing"
)

// A worker's brief says whether the attempt's project has a toolchain the
// operator selected, and offers the project's test command only then; the
// static turn text offers none (task 01a1f86e-70b9: a worker read "the
// project's test commands" in it and ran `go test` into a denial).
func TestBriefToolchain(t *testing.T) {
	for _, c := range crewCommands {
		if c.Name == "crew-turn" && strings.Contains(c.Body, "go test") {
			t.Fatal("the static turn text offers `go test`")
		}
	}
	home := t.TempDir()
	a := &memberAttempt{ID: "att-1", Task: TaskRef{HubID: "hub-a", ProjectID: "proj", TaskID: "task-1"}}
	brief := func(chains map[string]string) string {
		t.Helper()
		rec := TeamRecord{Role: "worker"}
		for p, c := range chains {
			rec.Projects = append(rec.Projects, TeamProject{HubID: "hub-a", ProjectID: p, Toolchain: c})
		}
		if err := SaveTeam(home, rec); err != nil {
			t.Fatal(err)
		}
		var b strings.Builder
		briefToolchain(&b, home, a)
		return b.String()
	}
	for name, chains := range map[string]map[string]string{
		"none selected":            {"proj": ""},
		"another project's":        {"other": "dev-go", "proj": ""},
		"the project not recorded": {"other": "dev-go"},
	} {
		got := brief(chains)
		if !strings.Contains(got, "Toolchain: none is selected for project proj, so do not run its build and test commands") ||
			!strings.Contains(got, "`aicrew team toolchain`") || strings.Contains(got, "go test") {
			t.Errorf("%s: %q", name, got)
		}
	}
	if got := brief(map[string]string{"proj": "dev-cobol"}); !strings.Contains(got, "dev-cobol is selected for project proj, "+
		"but this aicrew-agent does not know it") || strings.Contains(got, "go test") {
		t.Errorf("an unknown toolchain: %q", got)
	}
	if got := brief(map[string]string{"proj": "dev-go"}); got != "\nToolchain: the operator selected dev-go for project "+
		"proj, so this turn may run its commands in your attempt's worktree, for example "+
		"`cd worktrees/att-1 && go test ./...`.\n" {
		t.Errorf("dev-go: %q", got)
	}
}
