package agent

import (
	"testing"

	"github.com/BlackVS/aicrew/internal/automation"
)

// A workflow script's run() is checked against the worker turn's own list:
// the managed rules, the team's toolchain and the deny rules. The base
// update's commands (docs/DESIGN-TURNS.md, 10.4) are admitted by it, and a
// command the turn may not run is refused to the script too.
func TestTurnAllowList(t *testing.T) {
	home := t.TempDir()
	plain := TurnAllowList(home, nil)
	goTeam := TurnAllowList(home, []string{"dev-go"})
	for _, c := range []struct {
		cmd          string
		args         []string
		plain, devGo bool
	}{
		{"git", []string{"fetch", "origin", "main"}, true, true},
		{"git", []string{"merge", "--no-edit", "origin/main"}, true, true},
		{"git", []string{"merge", "--abort"}, true, true},
		{"git", []string{"reset", "--keep", "0123456"}, true, true},
		{"git", []string{"rev-parse", "HEAD"}, true, true},
		{"git", []string{"push", "origin", "HEAD:feat/x"}, true, true},
		{"bash", []string{"scripts/patch-identity.sh", "origin/main"}, false, true},
		{"go", []string{"test", "./..."}, false, true},
		{"git", []string{"config", "--global", "user.email", "x"}, false, false},
		{"git", []string{"-c", "http.sslVerify=false", "fetch"}, false, false},
		{"git", []string{"show", "HEAD:creds/github.token"}, false, false},
		{"curl", []string{"https://example.com"}, false, false},
		{"bash", []string{"-c", "echo hi"}, false, false},
	} {
		line := automation.CommandLine(c.cmd, c.args)
		if got := plain.Admit(c.cmd, c.args) == nil; got != c.plain {
			t.Errorf("without a toolchain, %s admitted %v", line, got)
		}
		if got := goTeam.Admit(c.cmd, c.args) == nil; got != c.devGo {
			t.Errorf("with dev-go, %s admitted %v", line, got)
		}
	}
}
