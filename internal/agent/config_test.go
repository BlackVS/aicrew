package agent

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const validAgentJSON = `{"layout": 1, "label": "builder", "clients": ["claude"],
 "aicrew": {"url": "https://aicrew.example:8443", "tls_trust_mode": "ca_dns", "tls_trust_value": "aicrew.example",
  "agent_id": "01a0ffff-0000-7000-8000-000000000001", "team_id": "01a0ffff-0000-7000-8000-000000000002"}}`

func writeAgentJSON(t *testing.T, body string) string {
	t.Helper()
	home := t.TempDir()
	if err := os.WriteFile(filepath.Join(home, "agent.json"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return home
}

func TestLoadConfig(t *testing.T) {
	c, err := LoadConfig(writeAgentJSON(t, validAgentJSON))
	if err != nil {
		t.Fatal(err)
	}
	if c.AimemCommand != "aimem" || c.Trust.Value != "aicrew.example" || c.TeamID == "" {
		t.Fatalf("config = %+v", c)
	}
	for name, body := range map[string]string{
		"no section":    `{"label": "builder"}`,
		"plain http":    strings.Replace(validAgentJSON, "https://", "http://", 1),
		"path":          strings.Replace(validAgentJSON, ":8443\"", ":8443/x\"", 1),
		"no trust":      strings.Replace(validAgentJSON, `"ca_dns"`, `""`, 1),
		"other host":    strings.Replace(validAgentJSON, `"tls_trust_value": "aicrew.example"`, `"tls_trust_value": "other.example"`, 1),
		"bad agent":     strings.Replace(validAgentJSON, `"agent_id": "01a0ffff-0000-7000-8000-000000000001"`, `"agent_id": "a b"`, 1),
		"no team":       strings.Replace(validAgentJSON, `"team_id": "01a0ffff-0000-7000-8000-000000000002"`, `"team_id": ""`, 1),
		"unknown field": strings.Replace(validAgentJSON, `"tls_trust_mode"`, `"token": "x", "tls_trust_mode"`, 1),
	} {
		if _, err := LoadConfig(writeAgentJSON(t, body)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if _, err := LoadConfig(t.TempDir()); err == nil {
		t.Error("a home without agent.json was accepted")
	}
}

func TestStateHoldsNoSecretFields(t *testing.T) {
	home := t.TempDir()
	if err := SaveState(home, State{AgentID: "a", TeamID: "t", ServiceID: "s", HubID: "h", SessionID: "sess"}); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(statePath(home))
	if err != nil {
		t.Fatal(err)
	}
	for _, word := range []string{"token", "handle", "receipt"} {
		if strings.Contains(string(raw), word) {
			t.Fatalf("the state record names a %s: %s", word, raw)
		}
	}
	st, ok, err := LoadState(home)
	if err != nil || !ok || st.SessionID != "sess" || st.Version != 1 {
		t.Fatalf("round trip: %+v %v %v", st, ok, err)
	}
	if err := ClearState(home); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := LoadState(home); ok {
		t.Fatal("cleared state still loads")
	}
}
