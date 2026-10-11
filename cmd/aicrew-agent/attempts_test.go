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

// fakeAttempts stands in for aicrewd's open-attempts route behind the
// launcher, and records the path it was asked for.
type fakeAttempts struct {
	answer string
	paths  []string
}

func (f *fakeAttempts) Read(_ context.Context, _, path string) (json.RawMessage, error) {
	f.paths = append(f.paths, path)
	return json.RawMessage(f.answer), nil
}

func (f *fakeAttempts) Inbox(context.Context, string, int) (json.RawMessage, error) { return nil, nil }

func (f *fakeAttempts) LocalStep(context.Context, string, string, string, []byte) (json.RawMessage, error) {
	return nil, nil
}

func runAttempts(home string, args ...string) (int, string, string) {
	var out, errOut bytes.Buffer
	env := func(name string) string {
		if name == agent.HomeEnv {
			return home
		}
		return ""
	}
	code := attempts(context.Background(), args, &out, &errOut, env)
	return code, out.String(), errOut.String()
}

// The attempts command reads aicrewd's list through the launcher and prints
// each attempt with why an offer can no longer be accepted and what to do;
// without a launcher it says so.
func TestAttemptsCommand(t *testing.T) {
	home := shortHome(t)
	if code, _, stderr := runAttempts(home); code != stepFailed || !strings.Contains(stderr, "no launcher serves") {
		t.Fatalf("no launcher: %d %q", code, stderr)
	}
	if code, _, _ := runAttempts(home, "extra"); code != exitUsage {
		t.Fatal("an argument: not a usage error")
	}

	f := &fakeAttempts{answer: `{"coordinator_generation":4,"attempts":[{"id":"att-9","state":"offered",
		"task":{"hub_id":"hub-a","project_id":"pilot","task_id":"task-3dc2"},"worker_agent_id":"agent-b","worker_label":"builder",
		"coordinator_generation":3,"stale":true,"expired":false,"declined":false,
		"next_action":"Withdraw the offer, then offer the task again if it is still to be done."}]}`}
	srv, err := agent.ServeSteps(home, &agent.Driver{Home: home, Session: &agent.Engine{}}, f,
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()

	code, stdout, _ := runAttempts(home)
	want := "- Attempt `att-9`: task `hub-a/pilot/task-3dc2`, worker builder, offered, stale: made under an earlier " +
		"coordinator session. Next: Withdraw the offer, then offer the task again if it is still to be done."
	if code != stepDone || strings.TrimSpace(stdout) != want {
		t.Fatalf("attempts: %d\n%s\nwant\n%s", code, stdout, want)
	}
	if len(f.paths) != 1 || f.paths[0] != "/v1/crew/attempts" {
		t.Errorf("the launcher read %v", f.paths)
	}
	if code, stdout, _ = runAttempts(home, "-json"); code != stepDone || !strings.Contains(stdout, `"stale": true`) {
		t.Errorf("attempts --json: %d %s", code, stdout)
	}
	f.answer = `{"coordinator_generation":4,"attempts":[]}`
	if code, stdout, _ = runAttempts(home); code != stepDone || strings.TrimSpace(stdout) != "The team has no open attempts." {
		t.Errorf("no attempts: %d %s", code, stdout)
	}
}
