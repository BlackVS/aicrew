package agent

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/BlackVS/aicrew/internal/store"
)

func shortWakePoll(t *testing.T) {
	t.Helper()
	old := wakePoll
	wakePoll = 50 * time.Millisecond
	t.Cleanup(func() { wakePoll = old })
}

// The launcher's wait ends "timeout" on an empty inbox, and returns as soon
// as a message is waiting, by id and kind, without delivering it: a read
// afterwards delivers it for the first time.
func TestWaitInboxWakesOnAMessage(t *testing.T) {
	shortWakePoll(t)
	ctx := context.Background()
	s := setupStepsWith(t, crewOptions{role: store.RoleWorker, steps: true, shortHome: true})
	s.serve(t)
	one, _ := json.Marshal(map[string]int{"seconds": 1})
	var answers []byte
	ans := s.call(t, &answers, StepCall{Op: "wait-inbox", Body: one})
	var res WakeResult
	if !ans.OK || json.Unmarshal(ans.Result, &res) != nil || len(res.Pending) != 0 || res.Ended != "timeout" {
		t.Fatalf("an empty inbox: %+v %s", ans, ans.Result)
	}

	go func() {
		time.Sleep(300 * time.Millisecond)
		s.store.SendMessage(ctx, s.coord, "wake-1", store.NewMessage{SessionID: s.coordSess.ID,
			Generation: s.coordSess.Generation, To: s.agentID, Text: "Please look at the parser task next."})
	}()
	ten, _ := json.Marshal(map[string]int{"seconds": 10})
	start := time.Now()
	ans = s.call(t, &answers, StepCall{Op: "wait-inbox", Body: ten})
	if !ans.OK || json.Unmarshal(ans.Result, &res) != nil || len(res.Pending) != 1 || res.Pending[0].Kind != "message" ||
		time.Since(start) > 5*time.Second {
		t.Fatalf("a message during the wait: %+v %s after %s", ans, ans.Result, time.Since(start))
	}
	if strings.Contains(string(ans.Result), "parser") {
		t.Fatal("the wait carried the message's text")
	}
	inbox := s.call(t, &answers, StepCall{Op: "inbox"})
	var page struct {
		Messages []store.InboxItem `json:"messages"`
	}
	if json.Unmarshal(inbox.Result, &page) != nil || len(page.Messages) != 1 || page.Messages[0].Deliveries != 1 ||
		page.Messages[0].ID != res.Pending[0].ID {
		t.Fatalf("the read after the wait: %s", inbox.Result)
	}
}

// The Stop hook blocks with what arrived, keeps an idle member waiting with
// a one-word turn until the idle bound passes, honours keep_alive false, and
// never blocks without a launcher.
func TestStopHookDecisions(t *testing.T) {
	shortWakePoll(t)
	ctx := context.Background()
	s := setupStepsWith(t, crewOptions{role: store.RoleWorker, steps: true, shortHome: true})
	setWake(t, s.cfg.Home, `{"wait_seconds": 1, "idle_hours": 2}`)
	clock := time.Date(2026, 10, 6, 9, 0, 0, 0, time.UTC)
	now := func() time.Time { return clock }

	if d := StopHook(ctx, s.cfg.Home, "sess-a", now); d != nil {
		t.Fatalf("no launcher: %+v", d)
	}
	s.serve(t)
	if _, err := s.store.SendMessage(ctx, s.coord, "hook-1", store.NewMessage{SessionID: s.coordSess.ID,
		Generation: s.coordSess.Generation, To: s.agentID, Text: "The review is ready."}); err != nil {
		t.Fatal(err)
	}
	d := StopHook(ctx, s.cfg.Home, "sess-a", now)
	if d == nil || d.Decision != "block" || !strings.Contains(d.Reason, "1 new message") || !strings.Contains(d.Reason, "aicrew-agent inbox") ||
		strings.Contains(d.Reason, "review is ready") {
		t.Fatalf("a waiting message: %+v", d)
	}
	// Acknowledge it, so nothing is pending; the next stops keep the member
	// waiting until two idle hours have passed since the last message.
	var answers []byte
	page := s.call(t, &answers, StepCall{Op: "inbox"})
	var msgs struct {
		Messages []store.InboxItem `json:"messages"`
	}
	_ = json.Unmarshal(page.Result, &msgs)
	ack, _ := json.Marshal(map[string][]string{"ids": {msgs.Messages[0].ID}})
	s.call(t, &answers, StepCall{Op: "ack", Body: ack})

	clock = clock.Add(time.Hour)
	if d := StopHook(ctx, s.cfg.Home, "sess-a", now); d == nil || !strings.Contains(d.Reason, "waiting") {
		t.Fatalf("keep-alive within the idle bound: %+v", d)
	}
	clock = clock.Add(90 * time.Minute)
	if d := StopHook(ctx, s.cfg.Home, "sess-a", now); d != nil {
		t.Fatalf("keep-alive after the idle bound: %+v", d)
	}
	// A new client session starts a new idle period.
	if d := StopHook(ctx, s.cfg.Home, "sess-b", now); d == nil || !strings.Contains(d.Reason, "waiting") {
		t.Fatalf("keep-alive in a new session: %+v", d)
	}
	setWake(t, s.cfg.Home, `{"wait_seconds": 1, "keep_alive": false}`)
	if d := StopHook(ctx, s.cfg.Home, "sess-b", now); d != nil {
		t.Fatalf("keep_alive false: %+v", d)
	}
}

// setWake sets agent.json's wake settings.
func setWake(t *testing.T, home, wake string) {
	t.Helper()
	doc, _, err := readAgentDoc(home)
	if err != nil {
		t.Fatal(err)
	}
	doc.top["wake"] = json.RawMessage(wake)
	if err := doc.write(home); err != nil {
		t.Fatal(err)
	}
}

// The managed settings name the Stop hook for this home, with a timeout a
// minute past the home's wait.
func TestClaudeSettingsCarryTheWakeHook(t *testing.T) {
	home := t.TempDir()
	old := selfExecutable
	exe := filepath.Join(t.TempDir(), "tools", "aicrew-agent")
	selfExecutable = func() string { return exe }
	t.Cleanup(func() { selfExecutable = old })
	var doc struct {
		Hooks struct {
			Stop []struct {
				Hooks []struct {
					Type    string `json:"type"`
					Command string `json:"command"`
					Timeout int    `json:"timeout"`
				} `json:"hooks"`
			} `json:"Stop"`
		} `json:"hooks"`
	}
	if err := json.Unmarshal([]byte(claudeSettings(home)), &doc); err != nil || len(doc.Hooks.Stop) != 1 {
		t.Fatalf("settings: %v %s", err, claudeSettings(home))
	}
	h := doc.Hooks.Stop[0].Hooks[0]
	want := `"` + filepath.ToSlash(exe) + `" wait-inbox --home "` + filepath.ToSlash(home) + `"`
	if h.Type != "command" || h.Command != want ||
		h.Timeout != int(DefaultWakeWait/time.Second)+60 {
		t.Fatalf("hook = %+v", h)
	}
}

// The wait never wedges the client: a refused read ends it as "session"; a
// read that does not answer is tried again until the wait ends; the
// launcher stopping ends it as "stopping".
func TestWaitPendingEnds(t *testing.T) {
	shortWakePoll(t)
	ctx := context.Background()
	refused := func(context.Context) (json.RawMessage, error) {
		return nil, &Refusal{Code: "invalid_token", Message: "the session has ended"}
	}
	if r := waitPending(ctx, refused, time.Minute); r.Ended != "session" || len(r.Pending) != 0 {
		t.Fatalf("a refused read: %+v", r)
	}
	tries := 0
	flaky := func(context.Context) (json.RawMessage, error) {
		tries++
		if tries < 3 {
			return nil, errors.New("connection refused")
		}
		return json.RawMessage(`{"pending":[{"id":"m1","seq":1,"kind":"lifecycle"}]}`), nil
	}
	if r := waitPending(ctx, flaky, time.Minute); len(r.Pending) != 1 || tries != 3 {
		t.Fatalf("a read that failed twice: %+v after %d tries", r, tries)
	}
	down := func(context.Context) (json.RawMessage, error) { return nil, errors.New("connection refused") }
	start := time.Now()
	if r := waitPending(ctx, down, 300*time.Millisecond); r.Ended != "timeout" || time.Since(start) > 2*time.Second {
		t.Fatalf("a read that never answers: %+v after %s", r, time.Since(start))
	}
	stopping, cancel := context.WithCancel(ctx)
	go func() { time.Sleep(100 * time.Millisecond); cancel() }()
	empty := func(context.Context) (json.RawMessage, error) { return json.RawMessage(`{"pending":[]}`), nil }
	if r := waitPending(stopping, empty, time.Minute); r.Ended != "stopping" {
		t.Fatalf("the launcher stopping: %+v", r)
	}
}

// A retryable refusal (a rate limit) is tried again, not taken for an ended
// session.
func TestWaitPendingRetriesARetryableRefusal(t *testing.T) {
	shortWakePoll(t)
	tries := 0
	limited := func(context.Context) (json.RawMessage, error) {
		tries++
		if tries < 2 {
			return nil, &Refusal{Code: "rate_limited", Retryable: true}
		}
		return json.RawMessage(`{"pending":[{"id":"m1","seq":1,"kind":"message"}]}`), nil
	}
	if r := waitPending(context.Background(), limited, time.Minute); len(r.Pending) != 1 {
		t.Fatalf("a rate-limited read: %+v", r)
	}
}
