//go:build realclaude

package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/BlackVS/aicrew/internal/opapi"
	"github.com/BlackVS/aicrew/internal/optoken"
)

// The smoke of DESIGN-CONTROL-PLANE 7.7's first increment with real Claude
// Code (go test -tags realclaude -run TestRealClaudeArchitectIsTold
// ./cmd/aicrew), with a logged-in claude on PATH and its smallest model:
//   - a disposable architect directory written by the built aicrew's
//     architect init --url, with an architect token in its creds/;
//   - the list and show routes of aicrewd's operator API, a stand-in in the
//     test (the whole channel through a real aicrewd is the simulation's
//     escalation scenario);
//   - one prompt to start the session. The session starts the wait as a
//     background command and goes idle; an escalation is then raised, and
//     the session tells of it with no further input.
//
// AICREW_SMOKE_KEEP=FILE keeps the session's event stream.
func TestRealClaudeArchitectIsTold(t *testing.T) {
	claude, err := exec.LookPath("claude")
	if err != nil {
		t.Skip("no claude on PATH")
	}
	lr := serveListRoute(t)
	url, pin := os.Getenv("AICREW_URL"), os.Getenv("AICREW_TLS_TRUST_VALUE")

	// Not t.TempDir: on Windows a wait the session started in the background
	// may still run the binary for a moment after the session ends.
	bin, err := os.MkdirTemp("", "aicrew-smoke-bin-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(bin) })
	exe := filepath.Join(bin, "aicrew")
	if runtime.GOOS == "windows" {
		exe += ".exe"
	}
	if out, err := exec.Command("go", "build", "-o", exe, "github.com/BlackVS/aicrew/cmd/aicrew").CombinedOutput(); err != nil {
		t.Fatalf("build aicrew: %v\n%s", err, out)
	}
	dir := filepath.Join(t.TempDir(), "architect")
	if out, err := exec.Command(exe, "architect", "init", "--dir", dir, "--project", "crew-app",
		"--url", url, "--tls-trust-mode", "spki_sha256", "--tls-trust-value", pin).CombinedOutput(); err != nil {
		t.Fatalf("architect init: %v\n%s", err, out)
	}
	tok, _ := optoken.Generate()
	if err := optoken.Write(filepath.Join(dir, "creds", "aicrew.architect"), tok); err != nil {
		t.Fatal(err)
	}

	// The session's environment: the built aicrew first on PATH, and no
	// AICREW_* of the test's, so the kit's settings alone connect it.
	var env []string
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(strings.ToUpper(kv), "AICREW_") && !strings.HasPrefix(strings.ToUpper(kv), "PATH=") {
			env = append(env, kv)
		}
	}
	env = append(env, "PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	cmd := exec.Command(claude, "-p", "--input-format", "stream-json", "--output-format", "stream-json", "--verbose",
		"--model", "haiku", "--allowedTools", "Bash(aicrew escalations:*)", "Read")
	cmd.Dir, cmd.Env = dir, env
	stdin, _ := cmd.StdinPipe()
	stdout, _ := cmd.StdoutPipe()
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	// The session ends when its input closes, and stops its background
	// commands; it is killed only if it does not end in time.
	t.Cleanup(func() {
		_ = stdin.Close()
		done := make(chan error, 1)
		go func() { done <- cmd.Wait() }()
		select {
		case <-done:
		case <-time.After(time.Minute):
			_ = cmd.Process.Kill()
			<-done
		}
	})

	var keep io.Writer = io.Discard
	if path := os.Getenv("AICREW_SMOKE_KEEP"); path != "" {
		f, err := os.Create(path)
		if err != nil {
			t.Fatal(err)
		}
		defer f.Close()
		keep = f
	}
	type event struct {
		Type, Subtype string
		Message       struct {
			Content []struct {
				Type, Text string
				Input      json.RawMessage
			}
		}
	}
	events := make(chan event, 256)
	go func() {
		sc := bufio.NewScanner(stdout)
		sc.Buffer(make([]byte, 1<<20), 16<<20)
		for sc.Scan() {
			fmt.Fprintln(keep, sc.Text())
			var ev event
			if json.Unmarshal(sc.Bytes(), &ev) == nil {
				events <- ev
			}
		}
		close(events)
	}()
	// until reads events until ok says so, logging what the session says
	// and runs, and fails after the timeout.
	until := func(what string, timeout time.Duration, ok func(event) bool) {
		t.Helper()
		deadline := time.After(timeout)
		for {
			select {
			case ev, open := <-events:
				if !open {
					t.Fatalf("the session ended before %s", what)
				}
				for _, c := range ev.Message.Content {
					switch c.Type {
					case "text":
						t.Logf("session says: %s", c.Text)
					case "tool_use":
						t.Logf("session runs: %s", c.Input)
					}
				}
				if ok(ev) {
					return
				}
			case <-deadline:
				t.Fatalf("no %s within %v", what, timeout)
			}
		}
	}

	start := map[string]any{"type": "user", "message": map[string]any{"role": "user",
		"content": "Start this session as docs/ARCHITECT.md says, then end your turn."}}
	b, _ := json.Marshal(start)
	if _, err := stdin.Write(append(b, '\n')); err != nil {
		t.Fatal(err)
	}
	waitStarted := false
	until("idle session with the wait running", 5*time.Minute, func(ev event) bool {
		for _, c := range ev.Message.Content {
			if c.Type == "tool_use" && strings.Contains(string(c.Input), "escalations wait") &&
				strings.Contains(string(c.Input), `"run_in_background":true`) {
				waitStarted = true
			}
		}
		return ev.Type == "result" && waitStarted
	})

	// Raised while the session is idle; nothing is sent to it.
	raised := opapi.Escalation{ID: "01a1smok-e000-7000-8000-000000000001", TeamID: "team-smoke", Category: "scope",
		Urgency: "now", Question: "Should the smoke flag be added to criterion 2?",
		Task:           opapi.EscalationTask{ProjectID: "crew-app", TaskID: "01a1task-0000"},
		Options:        []opapi.EscalationOption{{Option: "add it", Consequence: "one more flag"}, {Option: "amend", Consequence: "criteria change"}},
		Recommendation: "add it", CreatedAt: time.Now().UTC()}
	lr.raise(raised)
	t.Logf("raised %s at %s", raised.ID, time.Now().UTC().Format(time.RFC3339))
	told := time.Now()
	until("turn that tells of the escalation", 4*time.Minute, func(ev event) bool {
		for _, c := range ev.Message.Content {
			if c.Type == "text" && (strings.Contains(c.Text, "smoke flag") || strings.Contains(c.Text, "01a1smok")) {
				return true
			}
		}
		return false
	})
	t.Logf("told %v after the raise", time.Since(told).Round(time.Second))

	seen, err := readSeenIDs(filepath.Join(dir, "state", "escalations-seen.json"))
	if err != nil || len(seen) != 1 || seen[0] != raised.ID {
		t.Fatalf("the seen set: %v %v", seen, err)
	}
	entries, err := os.ReadDir(filepath.Join(dir, "creds"))
	if err != nil || len(entries) != 1 || entries[0].Name() != "aicrew.architect" {
		t.Fatalf("creds/ holds %v %v", entries, err)
	}
	if lr.answered() {
		t.Fatal("the session answered without the operator's word")
	}

	// The client leaves a background command running when its input
	// closes, so the session stops the wait it restarted before the end.
	stop := map[string]any{"type": "user", "message": map[string]any{"role": "user",
		"content": "This session ends now. Stop the background `aicrew escalations wait`, then end your turn."}}
	b, _ = json.Marshal(stop)
	if _, err := stdin.Write(append(b, '\n')); err != nil {
		t.Fatal(err)
	}
	until("end of the last turn", 3*time.Minute, func(ev event) bool { return ev.Type == "result" })
}
