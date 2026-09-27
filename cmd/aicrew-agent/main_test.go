package main

import (
	"bytes"
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/BlackVS/aicrew/internal/agent"
)

func noEngine(agent.Config, *slog.Logger) (*agent.Engine, error) {
	panic("no engine should be built")
}

func TestUsage(t *testing.T) {
	for _, args := range [][]string{nil, {"session"}, {"run"}, {"session", "start"}, {"session", "start", "-home"},
		{"session", "start", "-home", "x", "extra"}} {
		var out, errb bytes.Buffer
		if code := run(context.Background(), args, &out, &errb, noEngine); code != exitUsage {
			t.Errorf("%v: exit %d", args, code)
		}
	}
}

func TestBadConfigAndStatus(t *testing.T) {
	var out, errb bytes.Buffer
	if code := run(context.Background(), []string{"session", "start", "-home", t.TempDir()}, &out, &errb, noEngine); code != exitFailed {
		t.Fatalf("a home without agent.json: exit %d", code)
	}
	home := t.TempDir()
	cfg := `{"aicrew": {"url": "https://aicrew.example", "tls_trust_mode": "ca_dns", "tls_trust_value": "aicrew.example",
		"agent_id": "agent-1", "team_id": "team-1", "aimem_command": "aimem-not-installed-here"}}`
	if err := os.WriteFile(filepath.Join(home, "agent.json"), []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	if code := run(context.Background(), []string{"session", "status", "-home", home}, &out, &errb, noEngine); code != exitOK ||
		!strings.Contains(out.String(), `"session": null`) {
		t.Fatalf("status without a session: exit %d, %s", code, out.String())
	}
	if code := run(context.Background(), []string{"session", "fly", "-home", home}, &out, &errb, noEngine); code != exitUsage {
		t.Fatalf("an unknown verb: exit %d", code)
	}
}

func TestFinishCodes(t *testing.T) {
	log := slog.New(slog.NewTextHandler(new(bytes.Buffer), nil))
	if finish(log, nil) != exitOK || finish(log, &agent.WorkOutstanding{NextAction: "x"}) != exitWorkKept ||
		finish(log, agent.ErrSessionEnded) != exitFailed {
		t.Fatal("exit codes")
	}
}
