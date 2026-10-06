package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// The wake-up (task 01a0d6d7-1aed; docs/CLIENT-WAKE-PROBE.md): a Claude Code
// Stop hook in the home runs `aicrew-agent wait-inbox`, which asks the
// launcher to wait on the member's inbox. A message that arrives while it
// waits blocks the stop, and the member goes on with it; the inbox read and
// acknowledgement stay the only delivery, so the hint is idempotent.

const (
	inboxPendingPath = inboxPath + "/pending"
	// DefaultWakeWait is how long one turn end waits for the inbox.
	DefaultWakeWait = 600 * time.Second
	// maxWakeWait bounds one wait, whatever the request asks.
	maxWakeWait = 30 * time.Minute
	// DefaultIdleHours bounds the keep-alive: after this long without a
	// message the hook lets the session stop.
	DefaultIdleHours = 8
)

// wakePoll is how often the launcher looks at the inbox while it waits;
// tests shorten it.
var wakePoll = 3 * time.Second

// WakeResult is the launcher's answer to a wait: the messages pending, or
// none, with why the wait ended without one.
type WakeResult struct {
	Pending []PendingMessage `json:"pending"`
	// Ended is "timeout", "session" (the session cannot read the inbox:
	// closed, resumed elsewhere or refused) or "stopping" (the launcher is
	// shutting down) when nothing is pending.
	Ended string `json:"ended,omitempty"`
}

// PendingMessage is an unacknowledged message, without its content.
type PendingMessage struct {
	ID        string `json:"id"`
	Seq       int64  `json:"seq"`
	Kind      string `json:"kind"`
	AttemptID string `json:"attempt_id,omitempty"`
}

// waitInbox waits up to the call's seconds for a pending message in the
// member's inbox, looking every wakePoll as the launcher's session. It never
// fails the call: a refused read ends the wait as "session", and a read that
// does not answer is tried again until the wait ends.
func (s *StepServer) waitInbox(ctx context.Context, call StepCall) StepAnswer {
	var in struct {
		Seconds int `json:"seconds"`
	}
	if len(call.Body) > 0 && json.Unmarshal(call.Body, &in) != nil {
		return refuse("invalid_request", `The wait body is not {"seconds": N}.`, "Pass a number of seconds.")
	}
	read := func(ctx context.Context) (json.RawMessage, error) {
		return s.local.Read(ctx, s.driver.Session.stepToken(), inboxPendingPath)
	}
	return done(waitPending(ctx, read, wakeWait(in.Seconds)))
}

// waitPending reads the pending list every wakePoll until a message waits,
// the wait ends, or ctx ends. A refused read (the session closed, resumed
// elsewhere or refused) ends it as "session"; a read that did not answer, or
// a retryable refusal, is tried again.
func waitPending(ctx context.Context, read func(context.Context) (json.RawMessage, error), wait time.Duration) WakeResult {
	none := func(why string) WakeResult { return WakeResult{Pending: []PendingMessage{}, Ended: why} }
	deadline := time.Now().Add(wait)
	for {
		raw, err := read(ctx)
		var ref *Refusal
		switch {
		case err == nil:
			var out struct {
				Pending []PendingMessage `json:"pending"`
			}
			if json.Unmarshal(raw, &out) == nil && len(out.Pending) > 0 {
				return WakeResult{Pending: out.Pending}
			}
		case errors.As(err, &ref) && !ref.Retryable:
			return none("session")
		}
		left := time.Until(deadline)
		if left <= 0 {
			return none("timeout")
		}
		select {
		case <-ctx.Done():
			return none("stopping")
		case <-time.After(min(wakePoll, left)):
		}
	}
}

// wakeWait is a requested wait in seconds, defaulted and bounded.
func wakeWait(seconds int) time.Duration {
	if seconds <= 0 {
		return DefaultWakeWait
	}
	return min(time.Duration(seconds)*time.Second, maxWakeWait)
}

// WakeSettings are the home's wake-up settings, agent.json's "wake" object.
type WakeSettings struct {
	// KeepAlive asks the member for a one-word reply when a wait ends with
	// nothing pending, so its next turn end waits again.
	KeepAlive bool
	// IdleHours bounds the keep-alive.
	IdleHours int
	// Wait is one turn end's wait.
	Wait time.Duration
}

// ReadWakeSettings reads the home's wake-up settings, with their defaults
// for what agent.json does not name.
func ReadWakeSettings(home string) WakeSettings {
	ws := WakeSettings{KeepAlive: true, IdleHours: DefaultIdleHours, Wait: DefaultWakeWait}
	doc, ok, err := readAgentDoc(home)
	if err != nil || !ok {
		return ws
	}
	var w struct {
		KeepAlive   *bool `json:"keep_alive"`
		IdleHours   *int  `json:"idle_hours"`
		WaitSeconds *int  `json:"wait_seconds"`
	}
	if raw, ok := doc.top["wake"]; ok && json.Unmarshal(raw, &w) == nil {
		if w.KeepAlive != nil {
			ws.KeepAlive = *w.KeepAlive
		}
		if w.IdleHours != nil && *w.IdleHours >= 0 {
			ws.IdleHours = *w.IdleHours
		}
		if w.WaitSeconds != nil {
			ws.Wait = wakeWait(*w.WaitSeconds)
		}
	}
	return ws
}

// wakeState is the home's record of when the member last had a message or
// started waiting in its current client session, for the keep-alive's
// bound: state/wake.json, nonsecret. A new client session starts a new idle
// period.
type wakeState struct {
	SessionID string    `json:"session_id"`
	IdleSince time.Time `json:"idle_since"`
}

func wakeStatePath(home string) string { return filepath.Join(home, "state", "wake.json") }

func readWakeState(home string) (wakeState, bool) {
	var st wakeState
	b, err := os.ReadFile(wakeStatePath(home))
	if err != nil || json.Unmarshal(b, &st) != nil || st.IdleSince.IsZero() {
		return wakeState{}, false
	}
	return st, true
}

func writeWakeState(home string, st wakeState) error {
	b, _ := json.Marshal(st)
	if err := os.MkdirAll(filepath.Dir(wakeStatePath(home)), 0o700); err != nil {
		return err
	}
	return writeAtomic(wakeStatePath(home), append(b, '\n'))
}

// HookDecision is what the Stop hook prints, or nothing when the session
// may stop.
type HookDecision struct {
	Decision string `json:"decision"`
	Reason   string `json:"reason"`
}

// StopHook is the Stop hook's whole decision: it asks the home's launcher to
// wait on the inbox and blocks the stop with what arrived; with nothing
// pending it keeps the member waiting (keep-alive) until IdleHours pass
// without a message. It returns nil when the session may stop: no launcher,
// a session that cannot read its inbox, keep-alive off or spent. It never
// fails: the client is never wedged by the hook. sessionID is the client's
// own session id from the hook's input, which scopes the idle bound.
func StopHook(ctx context.Context, home, sessionID string, now func() time.Time) *HookDecision {
	ws := ReadWakeSettings(home)
	body, _ := json.Marshal(map[string]int{"seconds": int(ws.Wait / time.Second)})
	ans, err := CallStep(ctx, home, StepCall{Op: "wait-inbox", Body: body})
	if err != nil || !ans.OK {
		return nil
	}
	var res WakeResult
	if json.Unmarshal(ans.Result, &res) != nil {
		return nil
	}
	if len(res.Pending) > 0 {
		_ = writeWakeState(home, wakeState{SessionID: sessionID, IdleSince: now()})
		return &HookDecision{Decision: "block", Reason: pendingReason(res.Pending)}
	}
	if res.Ended != "timeout" || !ws.KeepAlive {
		return nil
	}
	st, ok := readWakeState(home)
	if !ok || st.SessionID != sessionID {
		st = wakeState{SessionID: sessionID, IdleSince: now()}
		_ = writeWakeState(home, st)
	}
	if now().Sub(st.IdleSince) >= time.Duration(ws.IdleHours)*time.Hour {
		return nil
	}
	return &HookDecision{Decision: "block", Reason: "No new message in your aicrew inbox yet. " +
		"Reply with the single word: waiting. Do nothing else: your next turn end waits for the inbox again."}
}

// pendingReason tells the member what arrived and what to do, without the
// messages' content: the inbox read is the delivery.
func pendingReason(pending []PendingMessage) string {
	kinds := map[string]int{}
	var ids []string
	for _, p := range pending {
		kinds[p.Kind]++
		if len(ids) < 10 {
			ids = append(ids, p.ID)
		}
	}
	var parts []string
	for _, k := range []string{"lifecycle", "message"} {
		if n := kinds[k]; n > 0 {
			parts = append(parts, fmt.Sprintf("%d %s", n, k))
		}
	}
	return fmt.Sprintf("%d new message(s) arrived in your aicrew inbox (%s; ids %s). "+
		"Read them with `aicrew-agent inbox`, acknowledge what you read, and act on them within your role "+
		"and your accepted attempt only.", len(pending), strings.Join(parts, ", "), strings.Join(ids, ", "))
}
