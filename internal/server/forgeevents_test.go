package server

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"
)

// The worker reports its running attempt's pull request: the first report
// is the baseline, and a change that needs a decision (here CI failing
// while the result is submitted) is one journaled event and, when aicrewd's
// loop runs its queue, one action.needed message to the worker and the
// coordinator, read as the inbox delivers it.
// Another member, a pull request of no attempt and a malformed report are
// refused.
func TestObservePullRoute(t *testing.T) {
	e := setupCoordination(t)
	e.store.SetEventTiming(time.Nanosecond, time.Nanosecond)
	id := e.runningOffer(t)
	obs := map[string]any{"host": "git.example.test", "repository": "crew/project-example", "number": 7,
		"url": "https://git.example.test/crew/project-example/pulls/7", "state": "open", "head": "work/task-1",
		"head_commit": "0123456789abcdef0123456789abcdef01234567", "labels": []string{"review-this"}, "checks": "pending",
		"comments": 0}
	got := e.call(t, e.worker.token, PullsPath, "", obs)
	if got.status != http.StatusOK || got.body["baseline"] != true || got.body["attempt_id"] != id {
		t.Fatalf("the baseline: %d %s", got.status, got.raw)
	}
	e.submit(t, id, "submit-1")
	obs["labels"], obs["checks"] = []string{"hands-reviewed"}, "failure"
	got = e.call(t, e.worker.token, PullsPath, "", obs)
	if got.status != http.StatusOK || got.body["event_id"] == nil {
		t.Fatalf("a review: %d %s", got.status, got.raw)
	}
	time.Sleep(time.Millisecond)
	(&Server{store: e.store, log: e.srv.log}).deliverEvents(context.Background())
	for _, m := range []member{e.worker, e.lead} {
		var inbox struct {
			Messages []struct {
				ID    string `json:"id"`
				Forge *struct {
					Pull    int64    `json:"pull"`
					Changes []string `json:"changes"`
				} `json:"forge"`
			} `json:"messages"`
		}
		r := e.get(t, m.token, InboxPath+"?limit=100")
		if r.status != http.StatusOK || json.Unmarshal([]byte(r.raw), &inbox) != nil {
			t.Fatalf("inbox: %d %s", r.status, r.raw)
		}
		n := 0
		for _, msg := range inbox.Messages {
			if msg.Forge != nil && msg.Forge.Pull == 7 && len(msg.Forge.Changes) == 2 {
				n++
			}
		}
		if n != 1 {
			t.Errorf("%s's inbox holds %d forge events: %s", m.agent.ID, n, r.raw)
		}
	}
	refused(t, e.call(t, e.indep.token, PullsPath, "", obs), http.StatusNotFound, "no_attempt")
	obs["head"] = "work/other"
	refused(t, e.call(t, e.worker.token, PullsPath, "", obs), http.StatusNotFound, "no_attempt")
	obs["head"], obs["checks"] = "work/task-1", "green"
	refused(t, e.call(t, e.worker.token, PullsPath, "", obs), http.StatusBadRequest, "invalid_request")
	refused(t, e.call(t, e.worker.token, PullsPath, "", map[string]any{"number": 7, "extra": 1}), http.StatusBadRequest,
		"invalid_request")
	refused(t, e.call(t, "", PullsPath, "", obs), http.StatusUnauthorized, "invalid_token")
}

// The session status names the member's open attempt as its worker, for the
// turn loop's continuation: the worker's running attempt with its phase and
// branch, and none for the coordinator.
func TestSessionStatusNamesTheAttempt(t *testing.T) {
	e := setupCoordination(t)
	id := e.runningOffer(t)
	var st struct {
		Attempt *struct {
			ID, State, Phase, Branch string
		} `json:"attempt"`
	}
	got := e.get(t, e.worker.token, SessionPath)
	if got.status != http.StatusOK || json.Unmarshal([]byte(got.raw), &st) != nil || st.Attempt == nil || st.Attempt.ID != id ||
		st.Attempt.State != "running" || st.Attempt.Phase != "working" || st.Attempt.Branch != "work/task-1" {
		t.Fatalf("the worker's status: %d %s", got.status, got.raw)
	}
	got = e.get(t, e.lead.token, SessionPath)
	st.Attempt = nil
	if json.Unmarshal([]byte(got.raw), &st) != nil || st.Attempt != nil {
		t.Fatalf("the coordinator's status: %s", got.raw)
	}
}
