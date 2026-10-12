package agent

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/BlackVS/aicrew/internal/store"
)

// writeAgentDoc writes agent.json for a home from v.
func writeAgentDoc(t *testing.T, home string, v map[string]any) {
	t.Helper()
	raw, _ := json.Marshal(v)
	if err := os.WriteFile(filepath.Join(home, "agent.json"), raw, 0o600); err != nil {
		t.Fatal(err)
	}
}

// A home in turns mode has managed settings without the Stop hook, and its
// per-turn settings hold the base allow list, the home's narrow additions
// and the deny rules; a broad or PowerShell addition is left out. A home
// in the default mode keeps the Stop hook.
func TestTurnsModeSettings(t *testing.T) {
	home := t.TempDir()
	settings := func() map[string]any {
		var doc map[string]any
		if err := json.Unmarshal([]byte(claudeSettings(home)), &doc); err != nil {
			t.Fatal(err)
		}
		return doc
	}
	if _, ok := settings()["hooks"]; !ok || ReadWakeSettings(home).Mode != WakeStopHook {
		t.Fatal("a home without agent.json has no Stop hook")
	}
	writeAgentDoc(t, home, map[string]any{"wake": map[string]any{"mode": "turns", "wait_seconds": 30},
		"turns": map[string]any{"allow": []string{"Bash(go test *)", "Bash", "Bash(*)", "PowerShell(Get-*)",
			"Edit(/worktrees/**)", "Bash(make *)"}}})
	doc := settings()
	if _, ok := doc["hooks"]; ok || ReadWakeSettings(home).Mode != WakeTurns || ReadWakeSettings(home).Wait.Seconds() != 30 {
		t.Fatalf("turns mode: %v", doc)
	}
	if deny := doc["permissions"].(map[string]any)["deny"].([]any); len(deny) != len(denyRules()) {
		t.Fatal("turns mode dropped the deny rules")
	}
	var turn struct {
		Permissions struct {
			Allow []string `json:"allow"`
			Deny  []string `json:"deny"`
		} `json:"permissions"`
	}
	if err := json.Unmarshal([]byte(turnSettings(home)), &turn); err != nil {
		t.Fatal(err)
	}
	for _, r := range append(baseTurnAllow(), "Bash(go test *)", "Bash(make *)") {
		if !slices.Contains(turn.Permissions.Allow, r) {
			t.Errorf("the allow list lacks %s: %v", r, turn.Permissions.Allow)
		}
	}
	for _, r := range []string{"Bash", "Bash(*)", "PowerShell(Get-*)"} {
		if slices.Contains(turn.Permissions.Allow, r) {
			t.Errorf("the allow list holds %s", r)
		}
	}
	if n := strings.Count(strings.Join(turn.Permissions.Allow, "\n"), "Edit(/worktrees/**)"); n != 1 {
		t.Errorf("a repeated rule appears %d times", n)
	}
	if !slices.Equal(turn.Permissions.Deny, denyRules()) {
		t.Error("the per-turn settings lack the deny rules")
	}
	for rule, ok := range map[string]bool{"Bash(go test ./...)": true, "Read(/docs/**)": true, "WebFetch(domain:example.org)": true,
		"Bash": false, "Bash(*)": false, "Edit(/**)": false, "Bash( * )": false, "PowerShell(Get-*)": false,
		"Bash(a\nb)": false, "Write(/x)": false} {
		if got := TurnRuleProblem(rule) == ""; got != ok {
			t.Errorf("TurnRuleProblem(%q) accepts=%v, want %v", rule, got, ok)
		}
	}
}

// join --wake turns rewrites the managed settings without the Stop hook and
// records the mode, keeping the wake object's other settings; --wake
// stop-hook brings the hook back. check notes a Stop hook in a turns-mode
// home and each turns.allow rule it leaves out.
func TestJoinWakeMode(t *testing.T) {
	e := setupJoin(t)
	code := e.invite(t, "inv-1", store.RoleWorker)
	e.join(t, e.opts(), &recCrew{}, activeAimem(), code)
	path := func(p string) string { return filepath.Join(e.home, filepath.FromSlash(p)) }
	if b, _ := os.ReadFile(path(TurnSettingsFile)); string(b) != turnSettings(e.home) {
		t.Fatal("join did not write the per-turn settings")
	}
	doc := readJSON(t, path("agent.json"))
	doc["wake"] = map[string]any{"wait_seconds": 45}
	raw, _ := json.Marshal(doc)
	os.WriteFile(path("agent.json"), raw, 0o600)

	rep, _ := e.join(t, JoinOptions{Home: e.home, Wake: WakeTurns}, &recCrew{}, activeAimem())
	if rep.Status == JoinBlocked {
		t.Fatalf("join --wake turns: %+v", rep)
	}
	b, _ := os.ReadFile(path(".claude/settings.json"))
	if strings.Contains(string(b), "wait-inbox") || string(b) != claudeSettings(e.home) {
		t.Fatalf("turns mode kept the Stop hook: %s", b)
	}
	if w := readJSON(t, path("agent.json"))["wake"].(map[string]any); w["mode"] != "turns" || w["wait_seconds"] != float64(45) {
		t.Fatalf("wake = %v", w)
	}
	c := &checker{o: CheckOptions{Home: e.home}}
	c.checkWakeHook()
	if len(c.rep.Notices) != 0 {
		t.Fatalf("a consistent turns-mode home: %q", c.rep.Notices)
	}
	// A Stop hook left in a turns-mode home, and a broad addition.
	os.WriteFile(path(".claude/settings.json"), []byte(`{"hooks": {"Stop": []}}`), 0o600)
	doc = readJSON(t, path("agent.json"))
	doc["turns"] = map[string]any{"allow": []string{"Bash(*)"}}
	raw, _ = json.Marshal(doc)
	os.WriteFile(path("agent.json"), raw, 0o600)
	c = &checker{o: CheckOptions{Home: e.home}}
	c.checkWakeHook()
	if len(c.rep.Notices) != 2 || !strings.Contains(c.rep.Notices[0], "turns mode") || !strings.Contains(c.rep.Notices[1], `"Bash(*)"`) {
		t.Fatalf("notices %q", c.rep.Notices)
	}
	os.WriteFile(path(".claude/settings.json"), []byte(claudeSettings(e.home)), 0o600)

	rep, _ = e.join(t, JoinOptions{Home: e.home, Wake: WakeStopHook}, &recCrew{}, activeAimem())
	if b, _ := os.ReadFile(path(".claude/settings.json")); rep.Status == JoinBlocked || !strings.Contains(string(b), "wait-inbox") {
		t.Fatalf("join --wake stop-hook: %+v\n%s", rep, b)
	}
	if rep, err := Join(t.Context(), JoinOptions{Home: e.home, Wake: "hook"}, e.deps(&recCrew{}, activeAimem(), new(int))); err == nil ||
		!strings.Contains(rep.Instruction, "--wake") {
		t.Fatalf("an unknown mode: %+v %v", rep, err)
	}
}
