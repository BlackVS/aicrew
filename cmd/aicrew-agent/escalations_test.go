package main

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"strings"
	"testing"

	"github.com/BlackVS/aicrew/internal/agent"
)

func runEscalations(home string, args ...string) (int, string, string) {
	var out, errOut bytes.Buffer
	env := func(name string) string {
		if name == agent.HomeEnv {
			return home
		}
		return ""
	}
	code := escalations(context.Background(), args, &out, &errOut, env)
	return code, out.String(), errOut.String()
}

// The escalations command reads the open escalations through the launcher
// and prints each with its task, category and question; without a launcher
// it says so.
func TestEscalationsCommand(t *testing.T) {
	home := shortHome(t)
	if code, _, stderr := runEscalations(home); code != stepFailed || !strings.Contains(stderr, "no launcher serves") {
		t.Fatalf("no launcher: %d %q", code, stderr)
	}
	if code, _, _ := runEscalations(home, "extra"); code != exitUsage {
		t.Fatal("an argument: not a usage error")
	}
	f := &fakeAttempts{answer: `{"escalations":[{"id":"esc-1","team_id":"t","coordinator_agent_id":"a",
		"task":{"hub_id":"hub-a","project_id":"pilot","task_id":"task-3dc2"},"category":"intent","urgency":"today",
		"question":"Am I paused?","options":[],"recommendation":"no","created_at":"2026-10-11T09:00:00Z"}]}`}
	srv, err := agent.ServeSteps(home, &agent.Driver{Home: home, Session: &agent.Engine{}}, f,
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()
	code, stdout, _ := runEscalations(home)
	want := "- Escalation `esc-1` on task `hub-a/pilot/task-3dc2` (intent, today), raised 2026-10-11T09:00:00Z: Am I paused?"
	if code != stepDone || strings.TrimSpace(stdout) != want {
		t.Fatalf("escalations: %d\n%s\nwant\n%s", code, stdout, want)
	}
	if len(f.paths) != 1 || f.paths[0] != "/v1/crew/escalations" {
		t.Errorf("the launcher read %v", f.paths)
	}
	f.answer = `{"escalations":[]}`
	if code, stdout, _ = runEscalations(home); code != stepDone || strings.TrimSpace(stdout) != "No escalation is open." {
		t.Errorf("none open: %d %s", code, stdout)
	}
}
