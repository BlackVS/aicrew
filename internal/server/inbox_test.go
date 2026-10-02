package server

import (
	"encoding/json"
	"io"
	"net/http"
	"testing"
	"time"
)

// get sends a GET as a member's client.
func (e *coordEnv) get(t *testing.T, token, path string) reply {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, e.url(path), nil)
	if err != nil {
		t.Fatal(err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := e.client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	out := reply{status: resp.StatusCode, header: resp.Header, raw: string(b)}
	_ = json.Unmarshal(b, &out.body)
	return out
}

type inboxMessage struct {
	ID        string `json:"id"`
	Seq       int64  `json:"seq"`
	Kind      string `json:"kind"`
	AttemptID string `json:"attempt_id"`
	Text      string `json:"text"`
	Task      *struct {
		TaskID string `json:"task_id"`
	} `json:"task"`
	Deliveries int64 `json:"deliveries"`
}

func inboxOf(t *testing.T, got reply) []inboxMessage {
	t.Helper()
	if got.status != http.StatusOK {
		t.Fatalf("inbox: %d %s", got.status, got.raw)
	}
	var out struct {
		Messages []inboxMessage `json:"messages"`
	}
	if err := json.Unmarshal([]byte(got.raw), &out); err != nil || out.Messages == nil {
		t.Fatalf("inbox reply %s: %v", got.raw, err)
	}
	return out.Messages
}

// A worker reads the offer from its own inbox, with the attempt it names,
// accepts by that ID, and acknowledges what it read; nothing is relayed.
func TestInboxNamesTheOfferedAttempt(t *testing.T) {
	e := setupCoordination(t)
	id, _ := e.offerAndSettle(t, "offer-1", "task-1", time.Now().Add(time.Hour))

	msgs := inboxOf(t, e.get(t, e.worker.token, InboxPath))
	var offer *inboxMessage
	for i := range msgs {
		if msgs[i].Kind == "lifecycle" && msgs[i].AttemptID == id {
			offer = &msgs[i]
		}
	}
	if offer == nil || offer.Task == nil || offer.Task.TaskID != "task-1" || offer.Deliveries != 1 {
		t.Fatalf("the worker's inbox holds no offer naming attempt %s: %+v", id, msgs)
	}
	// The worker accepts by the ID its inbox named.
	e.accept(t, offer.AttemptID, "accept-1")

	// Unacknowledged, it is delivered again, first.
	again := inboxOf(t, e.get(t, e.worker.token, InboxPath+"?limit=1"))
	if len(again) != 1 || again[0].ID != msgs[0].ID || again[0].Deliveries != 2 {
		t.Fatalf("a second read = %+v, want the oldest message again", again)
	}
	var ids []string
	for _, m := range msgs {
		ids = append(ids, m.ID)
	}
	ack := e.call(t, e.worker.token, InboxAckPath, "ack-1", map[string]any{"ids": ids})
	if ack.status != http.StatusOK || len(ack.body["acknowledged"].([]any)) != len(ids) {
		t.Fatalf("ack: %d %s", ack.status, ack.raw)
	}
	// The same key and IDs replay the same answer; a new key reports them
	// as acknowledged before.
	if replay := e.call(t, e.worker.token, InboxAckPath, "ack-1", map[string]any{"ids": ids}); replay.raw != ack.raw {
		t.Fatalf("a replayed ack answered %s, not %s", replay.raw, ack.raw)
	}
	twice := e.call(t, e.worker.token, InboxAckPath, "ack-2", map[string]any{"ids": ids})
	if twice.status != http.StatusOK || len(twice.body["already"].([]any)) != len(ids) {
		t.Fatalf("ack again: %d %s", twice.status, twice.raw)
	}
	for _, m := range inboxOf(t, e.get(t, e.worker.token, InboxPath)) {
		for _, acked := range ids {
			if m.ID == acked {
				t.Fatalf("an acknowledged message %s was delivered again", acked)
			}
		}
	}
}

// The inbox routes act only for a valid session token, and refuse in the
// session API's envelope: no token, a bad limit or query, an undelivered or
// unknown message, a missing key, and a session fenced by a resume.
func TestInboxRefusals(t *testing.T) {
	e := setupCoordination(t)
	e.offerAndSettle(t, "offer-1", "task-1", time.Now().Add(time.Hour))
	code := func(got reply) string {
		c, _ := got.body["code"].(string)
		return c
	}

	if got := e.get(t, "", InboxPath); got.status != http.StatusUnauthorized || code(got) != "invalid_token" {
		t.Fatalf("no token: %d %s", got.status, got.raw)
	}
	for _, q := range []string{"?limit=0", "?limit=101", "?limit=x", "?limit=1&limit=2", "?cursor=5"} {
		if got := e.get(t, e.worker.token, InboxPath+q); got.status != http.StatusBadRequest || code(got) != "invalid_request" {
			t.Errorf("%s: %d %s", q, got.status, got.raw)
		}
	}
	if got := e.call(t, e.worker.token, InboxAckPath, "ack-x", map[string]any{"ids": []string{"no-such-message"}}); got.status != http.StatusConflict ||
		code(got) != "message_not_delivered" {
		t.Fatalf("unknown message: %d %s", got.status, got.raw)
	}
	// The lead's own announcement went to the worker, not to the lead.
	msgs := inboxOf(t, e.get(t, e.worker.token, InboxPath))
	if got := e.call(t, e.lead.token, InboxAckPath, "ack-lead", map[string]any{"ids": []string{msgs[0].ID}}); code(got) != "message_not_delivered" {
		t.Fatalf("another member's message: %d %s", got.status, got.raw)
	}
	if got := e.call(t, e.worker.token, InboxAckPath, "", map[string]any{"ids": []string{msgs[0].ID}}); code(got) != "invalid_request" {
		t.Fatalf("no Idempotency-Key: %d %s", got.status, got.raw)
	}
	if got := e.call(t, e.worker.token, InboxAckPath, "ack-empty", map[string]any{"ids": []string{}}); code(got) != "invalid_request" {
		t.Fatalf("no IDs: %d %s", got.status, got.raw)
	}
	old := e.worker.token
	e.resume(t, e.worker)
	if got := e.get(t, old, InboxPath); got.status == http.StatusOK {
		t.Fatalf("a token fenced by the resume still reads the inbox: %s", got.raw)
	}
}
