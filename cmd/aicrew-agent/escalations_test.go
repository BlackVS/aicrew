package main

import (
	"bytes"
	"context"
	"encoding/json"
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

// fakePages stands in for aicrewd's paged route behind the launcher: the
// answer for each path read, and the paths in order.
type fakePages struct {
	pages map[string]string
	paths []string
}

func (f *fakePages) Read(_ context.Context, _, path string) (json.RawMessage, error) {
	f.paths = append(f.paths, path)
	return json.RawMessage(f.pages[path]), nil
}

func (f *fakePages) Inbox(context.Context, string, int) (json.RawMessage, error) { return nil, nil }

func (f *fakePages) LocalStep(context.Context, string, string, string, []byte) (json.RawMessage, error) {
	return nil, nil
}

func escJSON(id, question string) string {
	return `{"id":"` + id + `","task":{"hub_id":"hub-a","project_id":"pilot","task_id":"task-3dc2"},"category":"intent",` +
		`"urgency":"today","question":"` + question + `","coordinator_agent_id":"a","created_at":"2026-10-11T09:00:00Z"}`
}

// The escalations command follows every page of aicrewd's list through the
// launcher and prints each open escalation with its task, category and
// question; without a launcher it says so.
func TestEscalationsCommand(t *testing.T) {
	home := shortHome(t)
	if code, _, stderr := runEscalations(home); code != stepFailed || !strings.Contains(stderr, "no launcher serves") {
		t.Fatalf("no launcher: %d %q", code, stderr)
	}
	if code, _, _ := runEscalations(home, "extra"); code != exitUsage {
		t.Fatal("an argument: not a usage error")
	}
	f := &fakePages{pages: map[string]string{
		"/v1/crew/escalations":             `{"escalations":[` + escJSON("esc-1", "Am I paused?") + `],"next":"esc-1"}`,
		"/v1/crew/escalations?after=esc-1": `{"escalations":[` + escJSON("esc-2", "Which base?") + `]}`,
	}}
	srv, err := agent.ServeSteps(home, &agent.Driver{Home: home, Session: &agent.Engine{}}, f,
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()
	code, stdout, _ := runEscalations(home)
	want := "- Escalation `esc-1` on task `hub-a/pilot/task-3dc2` (intent, today), raised 2026-10-11T09:00:00Z: Am I paused?\n" +
		"- Escalation `esc-2` on task `hub-a/pilot/task-3dc2` (intent, today), raised 2026-10-11T09:00:00Z: Which base?"
	if code != stepDone || strings.TrimSpace(stdout) != want {
		t.Fatalf("escalations: %d\n%s\nwant\n%s", code, stdout, want)
	}
	if len(f.paths) != 2 || f.paths[1] != "/v1/crew/escalations?after=esc-1" {
		t.Errorf("the launcher read %v", f.paths)
	}
	if code, stdout, _ = runEscalations(home, "-json"); code != stepDone || strings.Count(stdout, `"id":`) != 2 {
		t.Errorf("escalations --json: %d %s", code, stdout)
	}
	f.pages["/v1/crew/escalations?after=esc-1"] = `{"escalations":[],"next":"esc-1"}`
	if code, _, stderr := runEscalations(home); code != stepFailed || !strings.Contains(stderr, "do not advance") {
		t.Errorf("pages that do not advance: %d %s", code, stderr)
	}
	f.pages["/v1/crew/escalations"] = `{"escalations":[]}`
	if code, stdout, _ = runEscalations(home); code != stepDone || strings.TrimSpace(stdout) != "No escalation is open." {
		t.Errorf("none open: %d %s", code, stdout)
	}
}
