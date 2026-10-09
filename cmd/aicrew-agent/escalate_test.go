package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/BlackVS/aicrew/internal/agent"
)

// fakeEscalations stands in for aicrewd's escalation route behind the
// launcher, recording what reached it.
type fakeEscalations struct {
	fakeInbox
	path string
	body string
	key  string
}

func (f *fakeEscalations) LocalStep(_ context.Context, key, _, path string, body []byte) (json.RawMessage, error) {
	f.key, f.path, f.body = key, path, string(body)
	return json.RawMessage(`{"id":"esc-1","category":"scope"}`), nil
}

func runEscalate(home, stdin string, args ...string) (int, string, string) {
	var out, errOut bytes.Buffer
	env := func(name string) string {
		if name == agent.HomeEnv {
			return home
		}
		return ""
	}
	code := escalate(context.Background(), args, strings.NewReader(stdin), &out, &errOut, env)
	return code, out.String(), errOut.String()
}

// escalate passes the request's JSON, from a file or standard input,
// through the launcher to aicrewd's escalation route, with its own key;
// a missing body, a body that is not JSON, or no launcher is refused.
func TestEscalateCommand(t *testing.T) {
	home := shortHome(t)
	body := `{"task":{"hub_id":"h","project_id":"p","task_id":"t"},"category":"scope"}`
	if code, _, stderr := runEscalate(home, body, "--body", "-"); code != stepFailed || !strings.Contains(stderr, "no launcher serves") {
		t.Fatalf("no launcher: %d %q", code, stderr)
	}
	for _, c := range []struct {
		stdin string
		args  []string
	}{{body, nil}, {body, []string{"--body", "-", "extra"}}, {"not json", []string{"--body", "-"}},
		{body, []string{"--body", filepath.Join(home, "missing.json")}}} {
		if code, _, _ := runEscalate(home, c.stdin, c.args...); code != exitUsage {
			t.Fatalf("%q %q: exit %d, want usage", c.stdin, c.args, code)
		}
	}

	f := &fakeEscalations{}
	srv, err := agent.ServeSteps(home, &agent.Driver{Home: home, Session: &agent.Engine{}}, f,
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()

	code, stdout, _ := runEscalate(home, body, "--body", "-")
	if code != stepDone || f.path != "/v1/crew/escalations" || f.body != body || f.key == "" ||
		!strings.Contains(stdout, `"esc-1"`) {
		t.Fatalf("escalate: %d %s, reached %s with %q", code, stdout, f.path, f.body)
	}
	file := filepath.Join(home, "esc.json")
	if err := os.WriteFile(file, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	first := f.key
	if code, _, _ := runEscalate(home, "", "--body", file); code != stepDone || f.body != body || f.key == first {
		t.Fatalf("escalate from a file: %d, key %q after %q", code, f.key, first)
	}
	// The launcher refuses a body that is JSON but not an object.
	if code, stdout, _ := runEscalate(home, `[1,2]`, "--body", "-"); code == stepDone || !strings.Contains(stdout, "invalid_request") {
		t.Fatalf("an array: %d %s", code, stdout)
	}
}
