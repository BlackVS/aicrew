//go:build realclaude

package agent

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/BlackVS/aicrew/internal/store"
)

// The A3 smoke with real Claude Code (go test -tags realclaude -run
// TestRealClaudeTurns ./internal/agent): a disposable home in turns mode,
// aicrewd and its store in the test, the built aicrew-agent on PATH, and the
// claude on PATH, logged in, with its smallest model. A message gives one
// turn that reads the inbox and acknowledges it; a second message gives a
// turn that resumes the same conversation; idle ticks give no turn.
func TestRealClaudeTurns(t *testing.T) {
	claude, err := exec.LookPath("claude")
	if err != nil {
		t.Skip("no claude on PATH")
	}
	ctx := context.Background()
	s := setupStepsWith(t, crewOptions{role: store.RoleWorker, steps: true, shortHome: true})
	home := s.cfg.Home

	bin := t.TempDir()
	exe := filepath.Join(bin, "aicrew-agent")
	if runtime.GOOS == "windows" {
		exe += ".exe"
	}
	if out, err := exec.Command("go", "build", "-o", exe, "github.com/BlackVS/aicrew/cmd/aicrew-agent").CombinedOutput(); err != nil {
		t.Fatalf("build aicrew-agent: %v\n%s", err, out)
	}
	prev := selfExecutable
	selfExecutable = func() string { return exe }
	t.Cleanup(func() { selfExecutable = prev })

	doc, _, err := readAgentDoc(home)
	if err != nil {
		t.Fatal(err)
	}
	// The binding the launcher holds, as join records it, so the member's
	// own aicrew-agent commands read it.
	for k, v := range map[string]string{"url": s.cfg.URL, "tls_trust_mode": s.cfg.Trust.Mode,
		"tls_trust_value": s.cfg.Trust.Value, "agent_id": s.cfg.AgentID, "team_id": s.cfg.TeamID} {
		doc.set(doc.aicrew, k, v)
	}
	doc.setWakeMode(WakeTurns)
	doc.set(doc.top, "turns", map[string]any{"model": "haiku", "turn_budget_usd": 0.5, "turn_timeout": "5m"})
	if err := doc.write(home); err != nil {
		t.Fatal(err)
	}
	if err := makeLayout(home); err != nil {
		t.Fatal(err)
	}
	rec := map[string]string{}
	plan, err := planFiles(home, rec)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := applyFiles(home, plan, rec); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(home, ".claude", "settings.json")); strings.Contains(string(b), "wait-inbox") {
		t.Fatal("the turns home has a Stop hook")
	}

	if keep := os.Getenv("AICREW_SMOKE_KEEP"); keep != "" {
		t.Cleanup(func() { _ = os.CopyFS(keep, os.DirFS(filepath.Join(home, "logs"))) })
	}
	srv := s.serve(t)
	env := withEnv(os.Environ(), "PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	env = withEnv(withEnv(env, SessionEnv, filepath.Join(home, "aimem", "smoke-session")), HomeEnv, home)
	loop := &TurnLoop{Home: home, Client: claude, Env: env, Host: srv, Run: RunClaudeTurn, Log: s.d.Log, Now: time.Now,
		Head: worktreeHead}

	send := func(key, text string) {
		t.Helper()
		if _, err := s.store.SendMessage(ctx, s.coord, key, store.NewMessage{SessionID: s.coordSess.ID,
			Generation: s.coordSess.Generation, To: s.agentID, Text: text}); err != nil {
			t.Fatal(err)
		}
	}
	recordsOf := func() []TurnRecord {
		raw, _ := os.ReadFile(filepath.Join(home, filepath.FromSlash(TurnLogFile)))
		var out []TurnRecord
		for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
			var r TurnRecord
			if json.Unmarshal([]byte(line), &r) == nil {
				out = append(out, r)
			}
		}
		return out
	}
	turnUntil := func(n int) []TurnRecord {
		t.Helper()
		deadline := time.Now().Add(10 * time.Minute)
		for time.Now().Before(deadline) {
			loop.Tick(ctx)
			if r := recordsOf(); len(r) >= n {
				return r
			}
			time.Sleep(turnGap)
		}
		t.Fatalf("no turn %d within 10 minutes", n)
		return nil
	}
	pendingCount := func() int {
		p, _, err := srv.PendingNow(ctx)
		if err != nil {
			t.Fatal(err)
		}
		return len(p)
	}

	send("smoke-1", "Smoke test one: read this message with aicrew-agent inbox, acknowledge it, and end your turn.")
	recs := turnUntil(1)
	t.Logf("turn 1: %+v", recs[0])
	if recs[0].Outcome != TurnFinished || recs[0].Resumed || pendingCount() != 0 {
		t.Fatalf("the first turn: %+v, %d pending", recs[0], pendingCount())
	}
	send("smoke-2", "Smoke test two: read and acknowledge this message too, then end your turn.")
	recs = turnUntil(2)
	t.Logf("turn 2: %+v", recs[1])
	if recs[1].Outcome != TurnFinished || !recs[1].Resumed || recs[1].Conversation != recs[0].Conversation || pendingCount() != 0 {
		t.Fatalf("the second turn: %+v, %d pending", recs[1], pendingCount())
	}
	for range 3 {
		time.Sleep(turnGap)
		loop.Tick(ctx)
	}
	if recs = recordsOf(); len(recs) != 2 {
		t.Fatalf("idle ticks gave turns: %+v", recs[2:])
	}
}
