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

// fakeInbox stands in for aicrewd's inbox routes behind the launcher.
type fakeInbox struct {
	page  string
	acked []string
}

func (f *fakeInbox) Inbox(context.Context, string, int) (json.RawMessage, error) {
	return json.RawMessage(f.page), nil
}

func (f *fakeInbox) LocalStep(_ context.Context, _, _, path string, body []byte) (json.RawMessage, error) {
	var in struct {
		IDs []string `json:"ids"`
	}
	_ = json.Unmarshal(body, &in)
	f.acked = append(f.acked, in.IDs...)
	return json.RawMessage(`{"acknowledged":["` + strings.Join(in.IDs, `","`) + `"],"already":[],"path":"` + path + `"}`), nil
}

func runInbox(home string, args ...string) (int, string, string) {
	var out, errOut bytes.Buffer
	env := func(name string) string {
		if name == agent.HomeEnv {
			return home
		}
		return ""
	}
	code := inbox(context.Background(), args, &out, &errOut, env)
	return code, out.String(), errOut.String()
}

// The inbox command reads a page through the launcher and prints each
// message with the attempt it names, and acknowledges the IDs it is given;
// without a launcher it says so.
func TestInboxCommand(t *testing.T) {
	home := shortHome(t)
	if code, _, stderr := runInbox(home); code != stepFailed || !strings.Contains(stderr, "no launcher serves") {
		t.Fatalf("no launcher: %d %q", code, stderr)
	}
	for _, args := range [][]string{{"extra"}, {"-limit", "0"}, {"-limit", "101"}, {"-ack", "a,,b"}, {"-ack", ","}} {
		if code, _, _ := runInbox(home, args...); code != exitUsage {
			t.Fatalf("%q: exit %d, want usage", args, code)
		}
	}
	if code, _, _ := runInbox(""); code != exitUsage {
		t.Fatal("no home: not a usage error")
	}

	f := &fakeInbox{page: `{"messages":[{"id":"m-1","seq":4,"kind":"lifecycle","sender_agent_id":"agent-lead",
		"task":{"hub_id":"hub-a","project_id":"pilot","task_id":"task-7"},"attempt_id":"att-9",
		"text":"lead offered task hub-a/pilot/task-7 to builder.","deliveries":1}]}`}
	srv, err := agent.ServeSteps(home, &agent.Driver{Home: home, Session: &agent.Engine{}}, f,
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()

	code, stdout, _ := runInbox(home)
	if code != stepDone || !strings.Contains(stdout, "#4 m-1 lifecycle from agent-lead, task hub-a/pilot/task-7, attempt att-9") ||
		!strings.Contains(stdout, "lead offered task hub-a/pilot/task-7 to builder.") || !strings.Contains(stdout, "inbox -ack") {
		t.Fatalf("inbox: %d %s", code, stdout)
	}
	code, stdout, _ = runInbox(home, "-json")
	var ans agent.StepAnswer
	if code != stepDone || json.Unmarshal([]byte(stdout), &ans) != nil || !strings.Contains(string(ans.Result), "att-9") {
		t.Fatalf("inbox -json: %d %s", code, stdout)
	}
	if code, stdout, _ = runInbox(home, "-ack", "m-1, m-2"); code != stepDone || strings.Join(f.acked, ",") != "m-1,m-2" ||
		!strings.Contains(stdout, "/v1/crew/inbox/ack") {
		t.Fatalf("ack: %d %s (acked %v)", code, stdout, f.acked)
	}

	f.page = `{"messages":[]}`
	if code, stdout, _ = runInbox(home); code != stepDone || !strings.Contains(stdout, "No unacknowledged messages.") {
		t.Fatalf("an empty inbox: %d %s", code, stdout)
	}
}
