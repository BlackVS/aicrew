package agent

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"sync/atomic"
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
	// The wait returns as soon as the message is pending: the wake is the
	// event, and its 30 seconds fail only a hang (01a20d9b-a3a8).
	thirty, _ := json.Marshal(map[string]int{"seconds": 30})
	ans = s.call(t, &answers, StepCall{Op: "wait-inbox", Body: thirty})
	if !ans.OK || json.Unmarshal(ans.Result, &res) != nil || len(res.Pending) != 1 || res.Pending[0].Kind != "message" {
		t.Fatalf("a message during the wait: %+v %s", ans, ans.Result)
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

// While the operator pauses the team, or the member, a message that arrives
// starts no turn: the Stop hook keeps the member waiting and says it is
// paused, without the message. On the resume the next stop blocks with it.
func TestStopHookWhilePaused(t *testing.T) {
	shortWakePoll(t)
	ctx := context.Background()
	s := setupStepsWith(t, crewOptions{role: store.RoleWorker, steps: true, shortHome: true})
	setWake(t, s.cfg.Home, `{"wait_seconds": 1}`)
	now := time.Now
	s.serve(t)
	for _, target := range []string{"", s.agentID} {
		if _, err := s.store.SetPause(ctx, s.operator, "pause-"+target, s.teamID, target, true, "upgrading"); err != nil {
			t.Fatal(err)
		}
		if _, err := s.store.SendMessage(ctx, s.coord, "while-"+target, store.NewMessage{SessionID: s.coordSess.ID,
			Generation: s.coordSess.Generation, To: s.agentID, Text: "Start the next task."}); err != nil {
			t.Fatal(err)
		}
		who := map[string]string{"": "your team", s.agentID: "you"}[target]
		d := StopHook(ctx, s.cfg.Home, "sess-p", now)
		if d == nil || d.Decision != "block" || !strings.Contains(d.Reason, "The operator paused "+who+` ("upgrading")`) ||
			strings.Contains(d.Reason, "new message") || !strings.Contains(d.Reason, "waiting") {
			t.Fatalf("a message while paused (%q): %+v", target, d)
		}
		if _, err := s.store.SetPause(ctx, s.operator, "resume-"+target, s.teamID, target, false, ""); err != nil {
			t.Fatal(err)
		}
		d = StopHook(ctx, s.cfg.Home, "sess-p", now)
		if d == nil || d.Decision != "block" || !strings.Contains(d.Reason, "1 new message") {
			t.Fatalf("the stop after the resume (%q): %+v", target, d)
		}
		var answers []byte
		page := s.call(t, &answers, StepCall{Op: "inbox"})
		var msgs struct {
			Messages []store.InboxItem `json:"messages"`
		}
		_ = json.Unmarshal(page.Result, &msgs)
		ack, _ := json.Marshal(map[string][]string{"ids": {msgs.Messages[0].ID}})
		s.call(t, &answers, StepCall{Op: "ack", Body: ack})
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
	if r := waitPending(ctx, refused, nil, time.Minute); r.Ended != "session" || len(r.Pending) != 0 {
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
	if r := waitPending(ctx, flaky, nil, time.Minute); len(r.Pending) != 1 || tries != 3 {
		t.Fatalf("a read that failed twice: %+v after %d tries", r, tries)
	}
	// A read that never answers ends the wait at its own deadline. The proof
	// is the number of reads, not a clock: one read every wakePoll from the
	// start to the deadline is at most wait/wakePoll+1, and a loaded runner
	// can only make it fewer; a wait that ran past its deadline reads more.
	// The guard fails only a wait that never ends (01a20d9b-a3a8).
	var downReads atomic.Int32
	down := func(context.Context) (json.RawMessage, error) {
		downReads.Add(1)
		return nil, errors.New("connection refused")
	}
	const short = 300 * time.Millisecond
	ended := make(chan WakeResult, 1)
	go func() { ended <- waitPending(ctx, down, nil, short) }()
	select {
	case r := <-ended:
		if most := int32(short/wakePoll) + 1; r.Ended != "timeout" || downReads.Load() > most {
			t.Fatalf("a read that never answers: %+v after %d reads, at most %d", r, downReads.Load(), most)
		}
	case <-time.After(30 * time.Second):
		t.Fatalf("a wait over a read that never answers did not end (%d reads)", downReads.Load())
	}
	stopping, cancel := context.WithCancel(ctx)
	go func() { time.Sleep(100 * time.Millisecond); cancel() }()
	empty := func(context.Context) (json.RawMessage, error) { return json.RawMessage(`{"pending":[]}`), nil }
	if r := waitPending(stopping, empty, nil, time.Minute); r.Ended != "stopping" {
		t.Fatalf("the launcher stopping: %+v", r)
	}
}

// The wait returns on the read that finds a message, not at its deadline:
// with an hour's wait and the message on the second read, it reads exactly
// twice. A wait that kept reading until its deadline reads more; one that
// slept on fails the guard, which a slow runner never reaches
// (01a20d9b-a3a8).
func TestWaitPendingReturnsOnTheReadThatFindsAMessage(t *testing.T) {
	shortWakePoll(t)
	var reads atomic.Int32
	read := func(context.Context) (json.RawMessage, error) {
		if reads.Add(1) < 2 {
			return json.RawMessage(`{"pending":[]}`), nil
		}
		return json.RawMessage(`{"pending":[{"id":"m1","seq":1,"kind":"message"}]}`), nil
	}
	done := make(chan WakeResult, 1)
	go func() { done <- waitPending(context.Background(), read, nil, time.Hour) }()
	select {
	case r := <-done:
		if len(r.Pending) != 1 || r.Pending[0].ID != "m1" || reads.Load() != 2 {
			t.Fatalf("the wait: %+v after %d reads, want the message after 2", r, reads.Load())
		}
	case <-time.After(30 * time.Second):
		t.Fatalf("the wait did not return on the read that found the message (%d reads)", reads.Load())
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
	if r := waitPending(context.Background(), limited, nil, time.Minute); len(r.Pending) != 1 {
		t.Fatalf("a rate-limited read: %+v", r)
	}
}

// A forge poll that does not answer ends with the wait: a short wait ends on
// time however slow the forge (01a1301d-55e3).
func TestWaitPendingBoundsTheForgePoll(t *testing.T) {
	empty := func(context.Context) (json.RawMessage, error) { return json.RawMessage(`{"pending":[]}`), nil }
	var polled, cut bool
	slow := func(ctx context.Context) {
		polled = true
		select {
		case <-ctx.Done():
			cut = true
		case <-time.After(time.Minute):
		}
	}
	start := time.Now()
	r := waitPending(context.Background(), empty, slow, time.Second)
	// The poll ended through its context, the wait's deadline: that is the
	// proof. The bound, half the poll's minute, fails only a poll the wait
	// did not end, never a slow runner (01a20d9b-a3a8).
	if took := time.Since(start); !polled || !cut || r.Ended != "timeout" || took > 30*time.Second {
		t.Fatalf("a 1-second wait with a slow forge: %+v after %v (polled %v, cut %v)", r, took, polled, cut)
	}
}
