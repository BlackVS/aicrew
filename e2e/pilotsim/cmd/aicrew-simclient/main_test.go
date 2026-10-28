package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/BlackVS/aicrew/internal/agent"
)

// The test binary doubles as an MCP server for the handshake tests.
func TestMain(m *testing.M) {
	switch os.Getenv("SIMCLIENT_TEST_MCP") {
	case "answer":
		sc := bufio.NewScanner(os.Stdin)
		for sc.Scan() {
			var req struct {
				ID     int    `json:"id"`
				Method string `json:"method"`
			}
			if json.Unmarshal(sc.Bytes(), &req) == nil && req.Method == "initialize" {
				fmt.Println(`{"jsonrpc":"2.0","method":"notifications/message","params":{}}`)
				fmt.Printf(`{"jsonrpc":"2.0","id":%d,"result":{"protocolVersion":"2024-11-05","capabilities":{}}}`+"\n", req.ID)
			}
		}
		os.Exit(0)
	case "refuse":
		os.Exit(1)
	}
	os.Exit(m.Run())
}

// home writes a home whose .mcp.json runs the test binary as aimem in mode.
func home(t *testing.T, mode string) string {
	t.Helper()
	dir := t.TempDir()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	if mode != "" {
		doc := map[string]any{"mcpServers": map[string]any{"aimem": map[string]any{
			"command": exe, "args": []string{"mcp"}, "env": map[string]string{"SIMCLIENT_TEST_MCP": mode}}}}
		raw, _ := json.Marshal(doc)
		if err := os.WriteFile(filepath.Join(dir, ".mcp.json"), raw, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	skill := filepath.Join(dir, ".claude", "skills", "oh-code-review")
	if err := os.MkdirAll(skill, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(skill, "SKILL.md"), []byte("---\nname: oh-code-review\n---\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// A directory without SKILL.md is no skill.
	if err := os.MkdirAll(filepath.Join(dir, ".claude", "skills", "notes"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	return dir
}

func call(t *testing.T, dir string, args ...string) (int, string, string) {
	t.Helper()
	var out, errb bytes.Buffer
	code := run(context.Background(), args, dir, &out, &errb)
	return code, out.String(), errb.String()
}

func TestVersionIsTheTestedClaude(t *testing.T) {
	code, out, _ := call(t, t.TempDir(), "--version")
	want, err := testedClaude()
	if code != 0 || err != nil || out != want+" (Claude Code)\n" {
		t.Fatalf("--version: exit %d %q (%v)", code, out, err)
	}
}

func TestModelProbeFinishes(t *testing.T) {
	for _, model := range []string{"", "claude-opus-5-5"} {
		code, out, _ := call(t, t.TempDir(), agent.ModelProbeArgs(model)...)
		res := agent.ReadTurnStream(strings.NewReader(out), func() {})
		want := model
		if want == "" {
			want = simModel
		}
		if code != 0 || res.Outcome != agent.TurnFinished || len(res.Models) != 1 || res.Models[0] != want ||
			res.ClientTotalUSD != 0 {
			t.Errorf("model %q: exit %d, %+v\n%s", model, code, res, out)
		}
	}
}

// initOf runs the check's init probe in dir and decodes its init event.
func initOf(t *testing.T, dir string) (servers []mcpEntry, skills []string) {
	t.Helper()
	code, out, _ := call(t, dir, "-p", "hi", "--output-format", "stream-json", "--verbose", "--max-turns", "1",
		"--no-session-persistence")
	if code != 0 {
		t.Fatalf("init probe: exit %d", code)
	}
	for _, line := range strings.Split(out, "\n") {
		if !strings.Contains(line, `"subtype":"init"`) {
			continue
		}
		var ev struct {
			MCPServers []mcpEntry `json:"mcp_servers"`
			Skills     []string   `json:"skills"`
		}
		if err := json.Unmarshal([]byte(line), &ev); err != nil {
			t.Fatal(err)
		}
		return ev.MCPServers, ev.Skills
	}
	t.Fatalf("no init event:\n%s", out)
	return nil, nil
}

func TestDiscoveryReachesTheHomesAimem(t *testing.T) {
	cases := []struct {
		mode, status, list string
	}{
		{"answer", "connected", "- ✓ Connected"},
		{"refuse", "failed", "- ✗ Failed to connect"},
		{"", "", "No MCP servers configured."},
	}
	for _, c := range cases {
		t.Run(cmpOr(c.mode, "absent"), func(t *testing.T) {
			dir := home(t, c.mode)
			servers, skills := initOf(t, dir)
			if c.status == "" && len(servers) != 0 || c.status != "" && (len(servers) != 1 || servers[0] !=
				(mcpEntry{"aimem", c.status})) {
				t.Errorf("mcp_servers %+v, want aimem %q", servers, c.status)
			}
			if len(skills) != 1 || skills[0] != "oh-code-review" {
				t.Errorf("skills %v, want [oh-code-review]", skills)
			}
			code, out, _ := call(t, dir, "mcp", "list")
			if code != 0 || !strings.Contains(out, c.list) {
				t.Errorf("mcp list: exit %d\n%s", code, out)
			}
			if c.mode != "" && !strings.Contains(out, "aimem: ") {
				t.Errorf("mcp list names no aimem line:\n%s", out)
			}
		})
	}
}

func TestAnythingElseIsNotSimulated(t *testing.T) {
	for _, args := range [][]string{nil, {"-p", "do the task", "--output-format", "stream-json"}, {"mcp", "add"}} {
		code, out, errOut := call(t, t.TempDir(), args...)
		if code != 2 || out != "" || !strings.Contains(errOut, "not simulated") {
			t.Errorf("%q: exit %d %q %q", args, code, out, errOut)
		}
	}
}

func cmpOr(a, b string) string {
	if a != "" {
		return a
	}
	return b
}
