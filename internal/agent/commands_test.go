package agent

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/BlackVS/aicrew/internal/store"
)

// The managed commands come from the same steps as ROLES.md (ef73): every
// step a command teaches is ROLES.md's own line for it, every name is
// crew-…, a role's command checks the role first, and each role has its
// commands.
func TestCommandsComeFromTheRoles(t *testing.T) {
	roles := rolesMD()
	byRole := map[string]int{}
	seen := map[string]bool{}
	for _, c := range crewCommands {
		if !strings.HasPrefix(c.Name, "crew-") || claudeBuiltins[c.Name] || seen[c.Name] {
			t.Errorf("%s: not a crew- name of its own", c.Name)
		}
		seen[c.Name] = true
		md := commandMD(c)
		for _, op := range c.Ops {
			s, ok := guideStepByOp(op)
			if !ok {
				t.Fatalf("%s teaches %s, which ROLES.md does not", c.Name, op)
			}
			if line := stepLine(s); !strings.Contains(md, line) || !strings.Contains(roles, line) {
				t.Errorf("%s: its %s step differs from ROLES.md's", c.Name, op)
			}
		}
		if c.Role != "" {
			byRole[c.Role]++
			if !strings.Contains(md, "This command is for the "+c.Role+" role") || !strings.Contains(md, "aicrew-agent session status") {
				t.Errorf("%s does not check the role first", c.Name)
			}
		}
		if (c.Hint != "") != strings.Contains(c.Body, "$ARGUMENTS") {
			t.Errorf("%s: its argument hint and its use of $ARGUMENTS disagree", c.Name)
		}
		if !strings.HasPrefix(md, "---\ndescription: ") || !strings.Contains(md, managedNote[len("This file"):]) {
			t.Errorf("%s: no description or managed note:\n%s", c.Name, md)
		}
		for _, banned := range []string{"session start --", "token"} {
			if strings.Contains(md, banned) {
				t.Errorf("%s names %q", c.Name, banned)
			}
		}
		if !strings.Contains(md, credentialRule) {
			t.Errorf("%s does not carry the credential rule", c.Name)
		}
	}
	for _, r := range guideRoles {
		if byRole[r.Role] == 0 {
			t.Errorf("the %s role has no command", r.Role)
		}
	}
	for _, name := range []string{"crew-start", "crew-inbox", "crew-triage", "crew-offer", "crew-review", "crew-accept",
		"crew-submit", "crew-claim", "crew-handoff", "crew-escalate"} {
		if !seen[name] {
			t.Errorf("no /%s", name)
		}
	}
}

// join writes the commands as managed files: a rerun leaves them, a local
// edit gets a .aicrew-new, and a command that would shadow one of the
// member's own is reported and never written.
func TestJoinWritesTheCommands(t *testing.T) {
	user := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", user)
	e := setupJoin(t)
	code := e.invite(t, "inv-1", store.RoleWorker)
	rep, _ := e.join(t, e.opts(), &recCrew{}, activeAimem(), code)
	if rep.Status != JoinReady {
		t.Fatalf("join: %+v", rep)
	}
	path := func(p string) string { return filepath.Join(e.home, filepath.FromSlash(p)) }
	for _, c := range crewCommands {
		if b, err := os.ReadFile(path(commandPath(c.Name))); err != nil || string(b) != commandMD(c) {
			t.Fatalf("%s: %v %q", c.Name, err, b)
		}
	}
	if !HasCommand(e.home, "crew-start") {
		t.Fatal("HasCommand does not find crew-start")
	}

	os.WriteFile(path(commandPath("crew-start")), []byte("my own start\n"), 0o644)
	os.MkdirAll(filepath.Join(user, "commands"), 0o700)
	os.WriteFile(filepath.Join(user, "commands", "crew-review.md"), []byte("mine\n"), 0o600)
	os.Remove(path(commandPath("crew-review")))
	rep, _ = e.join(t, JoinOptions{Home: e.home}, &recCrew{}, activeAimem())
	got := map[string]FileChange{}
	for _, c := range rep.Changes {
		got[c.Path] = c
	}
	if c := got[commandPath("crew-start")]; c.Action != "conflict" {
		t.Fatalf("an edited command: %+v", c)
	}
	if b, _ := os.ReadFile(path(commandPath("crew-start") + ".aicrew-new")); string(b) != commandMD(crewCommands[0]) {
		t.Fatalf("the edited command's new version: %q", b)
	}
	if c := got[commandPath("crew-review")]; c.Action != "collision" || !strings.Contains(c.Detail, "crew-review.md") {
		t.Fatalf("a shadowing command: %+v", c)
	}
	if _, err := os.Stat(path(commandPath("crew-review"))); !os.IsNotExist(err) {
		t.Fatal("a shadowing command was written")
	}
	if !strings.Contains(rep.Instruction, "would shadow") || !strings.Contains(rep.Instruction, ".aicrew-new") {
		t.Fatalf("the rerun's instruction: %q", rep.Instruction)
	}
	if c := got[commandPath("crew-offer")]; c.Action != "unchanged" {
		t.Fatalf("an untouched command: %+v", c)
	}
}

// A name off the crew- prefix, or a built-in, is a collision whatever the
// user scope holds.
func TestCommandCollisionRefusesBuiltins(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	for _, name := range []string{"review", "init", "help", "crew"} {
		if commandCollision(name) == "" {
			t.Errorf("%s is not refused", name)
		}
	}
	if c := commandCollision("crew-start"); c != "" {
		t.Errorf("crew-start collides with %s", c)
	}
}

// START.md names every managed command.
func TestStartNamesTheCommands(t *testing.T) {
	for _, c := range crewCommands {
		if !strings.Contains(startMD, "`/"+c.Name+"`") {
			t.Errorf("START.md does not name /%s", c.Name)
		}
	}
}
