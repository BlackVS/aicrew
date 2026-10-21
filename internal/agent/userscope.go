package agent

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// userScopeFindings lists what Claude Code's user configuration directory
// dir carries into every turn of a home run by that OS user (task
// 01a157c9-f75a): skills other than the required set, a settings.json with
// permissions or hooks, and a CLAUDE.md. The required skills reaching a
// turn is the intended state, so they are not named. Nil when there is
// nothing, or no such directory.
func userScopeFindings(dir string, required []string) []string {
	var out []string
	var skills []string
	ents, _ := os.ReadDir(filepath.Join(dir, "skills"))
	for _, e := range ents {
		if slices.Contains(required, e.Name()) {
			continue
		}
		if _, err := os.Stat(filepath.Join(dir, "skills", e.Name(), "SKILL.md")); err == nil {
			skills = append(skills, e.Name())
		}
	}
	if len(skills) > 0 {
		out = append(out, fmt.Sprintf("the skills %s in %s", strings.Join(skills, ", "), filepath.Join(dir, "skills")))
	}
	if raw, err := os.ReadFile(filepath.Join(dir, "settings.json")); err == nil {
		var doc map[string]any
		_ = json.Unmarshal(raw, &doc)
		var keys []string
		for _, k := range []string{"permissions", "hooks"} {
			switch v := doc[k].(type) {
			case nil:
			case map[string]any:
				if len(v) > 0 {
					keys = append(keys, k)
				}
			case []any:
				if len(v) > 0 {
					keys = append(keys, k)
				}
			default:
				keys = append(keys, k)
			}
		}
		if len(keys) > 0 {
			out = append(out, fmt.Sprintf("%s in %s", strings.Join(keys, " and "), filepath.Join(dir, "settings.json")))
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "CLAUDE.md")); err == nil {
		out = append(out, "the instructions "+filepath.Join(dir, "CLAUDE.md"))
	}
	return out
}

// userScopeWarning is the one warning on what the OS user's own Claude Code
// configuration carries into the member's turns; "" when nothing does.
// check reports it as a notice and serve logs it at its start.
func userScopeWarning(required []string) string {
	uh, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	found := userScopeFindings(claudeUserDir(uh), required)
	if len(found) == 0 {
		return ""
	}
	return "this home runs under an OS user whose own Claude Code configuration reaches every turn of the member: " +
		strings.Join(found, "; ") + ". The rule is one agent home per OS user (docs/WORKSPACE.md); on a development " +
		"station that shares the operator's account, this is the operator's responsibility. A configuration directory " +
		"per home (CLAUDE_CONFIG_DIR, at the cost of a provider login per home) is a possible later product feature, " +
		"not a step to take now"
}

// warnUserScope logs userScopeWarning at serve's start.
func warnUserScope(log *slog.Logger) {
	set, err := LoadSupported()
	if err != nil {
		return
	}
	if w := userScopeWarning(set.RequiredSkills); w != "" {
		log.Warn(w)
	}
}
