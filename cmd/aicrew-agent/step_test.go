package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"strings"
	"testing"

	"github.com/BlackVS/aicrew/internal/agent"
)

func shortHome(t *testing.T) string {
	t.Helper()
	home, err := os.MkdirTemp("", "ah")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(home) })
	return home
}

func runStep(home string, stdin string, args ...string) (int, string, string) {
	var out, errOut bytes.Buffer
	env := func(name string) string {
		if name == agent.HomeEnv {
			return home
		}
		return ""
	}
	code := step(context.Background(), args, strings.NewReader(stdin), &out, &errOut, env)
	return code, out.String(), errOut.String()
}

// The step command finds the launcher through AICREW_AGENT_HOME and maps
// the answer's status to its exit code; without a launcher it says so.
func TestStepCommand(t *testing.T) {
	home := shortHome(t)
	if code, _, stderr := runStep(home, "", "pending"); code != stepFailed || !strings.Contains(stderr, "no launcher serves") {
		t.Fatalf("no launcher: %d %q", code, stderr)
	}
	for _, args := range [][]string{{}, {"-home", home}, {"pending", "extra"}, {"claim", "-body", "{not json"}} {
		if code, _, _ := runStep(home, "", args...); code != exitUsage {
			t.Fatalf("%q: exit %d, want usage", args, code)
		}
	}
	if code, _, _ := runStep("", "", "pending"); code != exitUsage {
		t.Fatal("no home: not a usage error")
	}

	srv, err := agent.ServeSteps(home, &agent.Driver{Home: home}, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()
	code, stdout, _ := runStep(home, "", "pending")
	var ans agent.StepAnswer
	if code != stepDone || json.Unmarshal([]byte(stdout), &ans) != nil || ans.Status != agent.StepDone {
		t.Fatalf("pending: %d %s", code, stdout)
	}
	// -home overrides the environment.
	if code, _, _ := runStep(shortHome(t), "", "pending", "-home", home); code != stepDone {
		t.Fatalf("-home: exit %d", code)
	}
	// A body on stdin; the channel refuses a claim that names no task.
	if code, stdout, _ := runStep(home, `{"task":{}}`, "claim", "-body", "-"); code != stepRefused ||
		!strings.Contains(stdout, "invalid_request") {
		t.Fatalf("claim without a task: %d %s", code, stdout)
	}
	if code, _, _ := runStep(home, "", "confirm-stop"); code != stepRefused {
		t.Fatalf("a local step without an attempt: exit %d", code)
	}
}

func TestStepExit(t *testing.T) {
	for status, code := range map[string]int{agent.StepDone: 0, agent.StepPending: 4, agent.StepRefused: 3,
		agent.StepFailed: 1, "": 1} {
		if got := stepExit(status); got != code {
			t.Fatalf("%q: exit %d, want %d", status, got, code)
		}
	}
}

// --repository resolves the base of an offer or a claim only, and names a
// host the home holds no credential for before any launcher is asked.
func TestStepRepository(t *testing.T) {
	home := shortHome(t)
	if code, _, stderr := runStep(home, "", "accept", "--repository", "https://github.com/team/app"); code != exitUsage ||
		!strings.Contains(stderr, "offer or a claim only") {
		t.Fatalf("accept with --repository: %d %q", code, stderr)
	}
	if code, _, stderr := runStep(home, "", "offer", "--repository", "https://github.com/team/app", "--body", "{}"); code != stepFailed ||
		!strings.Contains(stderr, "--cred github.com=FILE") {
		t.Fatalf("offer without a credential: %d %q", code, stderr)
	}
}
