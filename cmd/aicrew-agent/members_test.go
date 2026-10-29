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

func runMembers(home string, args ...string) (int, string, string) {
	var out, errOut bytes.Buffer
	env := func(name string) string {
		if name == agent.HomeEnv {
			return home
		}
		return ""
	}
	code := members(context.Background(), args, &out, &errOut, env)
	return code, out.String(), errOut.String()
}

// The members command reads the team's members and their capability reports
// through the launcher and prints each one's agent ID with the repositories it
// verified, the ID a coordinator names as an offer's worker; a refused
// session prints the refusal, and without a launcher it says so.
func TestMembersCommand(t *testing.T) {
	home := shortHome(t)
	if code, _, stderr := runMembers(home); code != stepFailed || !strings.Contains(stderr, "no launcher serves") {
		t.Fatalf("no launcher: %d %q", code, stderr)
	}
	if code, _, _ := runMembers(home, "extra"); code != exitUsage {
		t.Fatal("an argument: not a usage error")
	}

	f := &fakeAttempts{answer: `{"members":[
		{"agent_id":"agent-a","label":"architect","role":"coordinator","capabilities":[]},
		{"agent_id":"agent-b","label":"builder","role":"worker","reported_at":"2026-10-29T10:00:00Z",
		 "capabilities":[{"host":"github.com","kind":"forge","account":"bot",
		  "repositories":[{"url":"https://github.com/o/r","access":"write"},{"url":"https://github.com/o/p","access":"read"}]}]},
		{"agent_id":"agent-c","label":"helper","role":"worker","reported_at":"2026-10-29T11:00:00Z","capabilities":[]}]}`}
	srv, err := agent.ServeSteps(home, &agent.Driver{Home: home, Session: &agent.Engine{}}, f,
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()

	code, stdout, _ := runMembers(home)
	want := "agent-a  architect (coordinator)  no capability report\n" +
		"agent-b  builder (worker)  verified at 2026-10-29T10:00:00Z: https://github.com/o/r (write), https://github.com/o/p (read)\n" +
		"agent-c  helper (worker)  no repository verified at 2026-10-29T11:00:00Z"
	if code != stepDone || strings.TrimSpace(stdout) != want {
		t.Fatalf("members: %d\n%s\nwant\n%s", code, stdout, want)
	}
	if len(f.paths) != 1 || f.paths[0] != "/v1/crew/capabilities" {
		t.Errorf("the launcher read %v", f.paths)
	}
	if code, stdout, _ = runMembers(home, "-json"); code != stepDone || !strings.Contains(stdout, `"agent_id": "agent-b"`) {
		t.Errorf("members --json: %d %s", code, stdout)
	}
	f.err = &agent.Refusal{Code: "invalid_token", Message: "The session token is not valid.", NextAction: "Start a session."}
	if code, stdout, _ = runMembers(home); code != stepRefused || !strings.Contains(stdout, `"code": "invalid_token"`) {
		t.Errorf("a refused session: %d %s", code, stdout)
	}
	f.err = nil
	f.answer = `{"members":[]}`
	if code, stdout, _ = runMembers(home); code != stepDone || strings.TrimSpace(stdout) != "The team has no members." {
		t.Errorf("no members: %d %s", code, stdout)
	}
}
