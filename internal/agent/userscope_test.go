package agent

import (
	"bytes"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The OS user's own Claude Code configuration that reaches a member's
// turns is named item by item; the required skills, an env-only
// settings.json, an empty and a missing directory are not (01a157c9-f75a).
func TestUserScopeFindings(t *testing.T) {
	required := []string{"oh-code-review"}
	skill := func(dir, name string) {
		os.MkdirAll(filepath.Join(dir, "skills", name), 0o755)
		os.WriteFile(filepath.Join(dir, "skills", name, "SKILL.md"), []byte("---\nname: "+name+"\n---\n"), 0o644)
	}
	settings := func(body string) func(string) {
		return func(dir string) { os.WriteFile(filepath.Join(dir, "settings.json"), []byte(body), 0o644) }
	}
	for _, tc := range []struct {
		name  string
		setup func(dir string)
		want  []string // substrings of the one finding; nil for none
	}{
		{"skills", func(dir string) { skill(dir, "my-own"); skill(dir, "oh-code-review") }, []string{"the skills my-own in"}},
		{"permissions", settings(`{"permissions": {"allow": ["Bash(git:*)"]}}`), []string{"permissions in", "settings.json"}},
		{"hooks", settings(`{"hooks": {"Stop": []}}`), []string{"hooks in", "settings.json"}},
		{"instructions", func(dir string) { os.WriteFile(filepath.Join(dir, "CLAUDE.md"), []byte("# mine\n"), 0o644) }, []string{"the instructions", "CLAUDE.md"}},
		{"required skills only", func(dir string) { skill(dir, "oh-code-review") }, nil},
		{"a skill directory without SKILL.md", func(dir string) { os.MkdirAll(filepath.Join(dir, "skills", "draft"), 0o755) }, nil},
		{"env-only settings", settings(`{"env": {"X": "1"}, "permissions": { }, "hooks": {
		}}`), nil},
		{"empty", func(string) {}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			tc.setup(dir)
			got := userScopeFindings(dir, required)
			if tc.want == nil {
				if got != nil {
					t.Fatalf("findings %q", got)
				}
				return
			}
			if len(got) != 1 {
				t.Fatalf("findings %q", got)
			}
			for _, w := range tc.want {
				if !strings.Contains(got[0], w) {
					t.Fatalf("%q lacks %q", got[0], w)
				}
			}
		})
	}
	if got := userScopeFindings(filepath.Join(t.TempDir(), "missing"), required); got != nil {
		t.Fatalf("a missing directory: %q", got)
	}
}

// check reports the warning as a notice and its status stays the same;
// serve logs the same text at its start.
func TestUserScopeWarningInCheckAndServe(t *testing.T) {
	e := setupCheck(t, readyTools, "aimem", "claude")
	e.installSkills(t, "1.26.1", "oh-code-review")
	e.check(t, "claude") // creates the wiring
	clean := e.check(t)
	if clean.Status != JoinReady || hasNotice(clean, "OS user") {
		t.Fatalf("a clean user profile: %+v", clean)
	}
	var logs bytes.Buffer
	warnUserScope(slog.New(slog.NewTextHandler(&logs, nil)))
	if logs.Len() != 0 {
		t.Fatalf("serve warned on a clean profile: %s", logs.String())
	}

	os.WriteFile(filepath.Join(e.user, ".claude", "CLAUDE.md"), []byte("# the operator's own\n"), 0o644)
	rep := e.check(t)
	if rep.Status != clean.Status || len(rep.Instructions) != 0 {
		t.Fatalf("the warning changed the status: %+v", rep)
	}
	want := userScopeWarning([]string{"oh-code-review"})
	if !strings.Contains(want, "CLAUDE.md") || !strings.Contains(want, "one agent home per OS user") ||
		!strings.Contains(want, "CLAUDE_CONFIG_DIR") || !hasNotice(rep, want) {
		t.Fatalf("notices %q, want %q", rep.Notices, want)
	}
	warnUserScope(slog.New(slog.NewTextHandler(&logs, nil)))
	if !strings.Contains(logs.String(), "level=WARN") || !strings.Contains(logs.String(), "CLAUDE.md") {
		t.Fatalf("serve's log: %s", logs.String())
	}
}
