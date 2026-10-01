package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/BlackVS/aicrew/internal/agent"
)

// noJoinDeps fails the test if the bootstrap reaches aicrewd, aimem or the
// prompt.
func noJoinDeps(t *testing.T) agent.JoinDeps {
	return agent.JoinDeps{
		Crew: func(agent.Config) (agent.InvitationAPI, error) {
			t.Fatal("aicrewd was called")
			return nil, nil
		},
		Aimem: func(string, string) agent.JoinAimem {
			t.Fatal("aimem was called")
			return nil
		},
		ReadCode: func() (string, error) {
			t.Fatal("the prompt was shown")
			return "", nil
		},
	}
}

var joinFlags = []string{"-label", "builder", "-url", "https://aicrew.example", "-tls-trust-mode", "ca_dns",
	"-tls-trust-value", "aicrew.example", "-aimem-hub", "main", "-client", "claude"}

// The code is accepted from no flag, and incomplete or malformed options
// are usage errors.
func TestJoinUsage(t *testing.T) {
	home := filepath.Join(t.TempDir(), "h")
	for _, args := range [][]string{
		nil,
		{"-home", home, "extra"},
		append([]string{"-home", home, "-code", "ABCD"}, joinFlags...),
		{"-home", home, "-label", "builder"},
		{"-home", home, "-label", "builder", "-url", "http://aicrew.example", "-tls-trust-mode", "ca_dns",
			"-tls-trust-value", "aicrew.example", "-aimem-hub", "main", "-client", "claude"},
	} {
		var out, errb bytes.Buffer
		if code := join(context.Background(), args, true, &out, &errb, noJoinDeps(t)); code != exitUsage {
			t.Errorf("%v: exit %d (%s)", args, code, errb.String())
		}
	}
	if _, err := os.Stat(home); !os.IsNotExist(err) {
		t.Fatal("a usage error created the home")
	}
}

// Off a terminal the bootstrap refuses before anything is created; a
// linked home is refreshed without a terminal, aicrewd or aimem.
func TestJoinCommand(t *testing.T) {
	home := filepath.Join(t.TempDir(), "h")
	var out, errb bytes.Buffer
	code := join(context.Background(), append([]string{"-home", home, "-json"}, joinFlags...), false, &out, &errb,
		noJoinDeps(t))
	var rep agent.JoinReport
	if code != exitFailed || json.Unmarshal(out.Bytes(), &rep) != nil || rep.Reason != "no_terminal" {
		t.Fatalf("off a terminal: exit %d, %s %s", code, out.String(), errb.String())
	}
	if _, err := os.Stat(home); !os.IsNotExist(err) {
		t.Fatal("the refused run created the home")
	}

	os.MkdirAll(home, 0o755)
	cfg := `{"layout": 1, "label": "builder", "aicrew": {"url": "https://aicrew.example", "tls_trust_mode": "ca_dns",
		"tls_trust_value": "aicrew.example", "agent_id": "agent-1", "team_id": "team-1", "aimem_hub": "main"}}`
	os.WriteFile(filepath.Join(home, "agent.json"), []byte(cfg), 0o600)
	// A home linked before clients were recorded: the refresh writes the
	// managed files, and the check stops it until a client is selected,
	// before asking any client anything.
	out.Reset()
	if code := join(context.Background(), []string{"-home", home}, false, &out, &errb, noJoinDeps(t)); code != exitFailed {
		t.Fatalf("refresh: exit %d, %s %s", code, out.String(), errb.String())
	}
	if !bytes.Contains(out.Bytes(), []byte("status: blocked")) || !bytes.Contains(out.Bytes(), []byte("-client claude")) {
		t.Fatalf("refresh output %s", out.String())
	}
	if _, err := os.Stat(filepath.Join(home, "AGENTS.md")); err != nil {
		t.Fatalf("the refresh did not write the managed files: %v", err)
	}
	out.Reset()
	code = join(context.Background(), []string{"-home", home, "-url", "https://other.example"}, false, &out, &errb,
		noJoinDeps(t))
	if code != exitFailed || !bytes.Contains(out.Bytes(), []byte("home_linked")) {
		t.Fatalf("another binding: exit %d, %s", code, out.String())
	}
}
