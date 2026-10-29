package store

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// blocked blocks the claim's running attempt a with reason, settled from the
// read scope.
func (e claimStopEnv) blocked(t *testing.T, a Attempt, reason string) Attempt {
	t.Helper()
	e.reader.setHold(heldAs(a, a.Fence, a.TaskRevision))
	a, st := e.beginWork(t, a, "block-1", IntentBlock, reason)
	e.reader.commitUpdate(st.RequestKey, updateReceipt(a, st.RequestKey))
	a, set, err := e.settleKey(t, a, st.RequestKey, HintUnknown)
	if err != nil || !set.Settled || a.Phase != PhaseBlocked {
		t.Fatalf("block: %+v %+v %v", a, set, err)
	}
	return a
}

// answers is the answer subjects in the token's inbox, which it then
// acknowledges.
func answers(t *testing.T, s *Store, token string) []ActionSubject {
	t.Helper()
	items, err := s.ReadInboxWithToken(context.Background(), token, 100)
	if err != nil {
		t.Fatal(err)
	}
	var out []ActionSubject
	var read []InboxItem
	for _, it := range items {
		if it.Action == nil {
			continue
		}
		for _, sub := range it.Action.Subjects {
			if sub.Kind == "answer" {
				out = append(out, sub)
				read = append(read, it)
			}
		}
	}
	if len(read) > 0 {
		ackTok(t, s, token, read)
	}
	return out
}

// The coordinator's answer to a blocked attempt wakes its worker once, with
// one action.needed message carrying the answer; the coordinator is not
// woken by its own answer. A replay under the same key journals one event,
// and the same key with another text is refused.
func TestAnswerWakesTheBlockedWorker(t *testing.T) {
	ctx := context.Background()
	e := newClaimStopEnv(t)
	a, _ := e.runningClaim(t, "task-1")
	a = e.blocked(t, a, "The runner's disk is full.")
	settle(t, e.s, e.now)
	answers(t, e.s, e.leadTok)
	const text = "Retry with the cache cleared: the disk was cleaned at 10:00."
	got, err := e.s.AnswerWithToken(ctx, "answer-1", e.leadTok, a.ID, text)
	if err != nil || got.ID != a.ID || got.Phase != PhaseBlocked {
		t.Fatalf("the answer: %+v %v", got, err)
	}
	if _, err := e.s.AnswerWithToken(ctx, "answer-1", e.leadTok, a.ID, text); err != nil {
		t.Fatalf("the replay: %v", err)
	}
	if _, err := e.s.AnswerWithToken(ctx, "answer-1", e.leadTok, a.ID, "Something else."); !errors.Is(err, ErrIdempotencyConflict) {
		t.Fatalf("the same key with another text: %v", err)
	}
	var n int
	if err := e.s.db.QueryRow(`SELECT COUNT(*) FROM events WHERE kind = 'attempt.answered'`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("%d answer events, %v", n, err)
	}
	settle(t, e.s, e.now)
	w := answers(t, e.s, e.indepTk)
	if len(w) != 1 || w[0].AttemptID != a.ID || !strings.Contains(w[0].Text, "lead answered your blocked report on task") ||
		!strings.Contains(w[0].Text, text) || !strings.Contains(w[0].Text, "aicrew-agent step work") {
		t.Fatalf("the worker's message: %+v", w)
	}
	if c := answers(t, e.s, e.leadTok); len(c) != 0 {
		t.Fatalf("the coordinator was woken by its own answer: %+v", c)
	}
	settle(t, e.s, e.now)
	if w := answers(t, e.s, e.indepTk); len(w) != 0 {
		t.Fatalf("woken twice: %+v", w)
	}
}

// An answer is refused from a member who is not the coordinator, from
// another team's coordinator, to an attempt that is not blocked, and when it
// is blank or larger than an answer may be. A refused answer journals
// nothing.
func TestAnswerRefusals(t *testing.T) {
	ctx := context.Background()
	e := newClaimStopEnv(t)
	a, _ := e.runningClaim(t, "task-1")
	if _, err := e.s.AnswerWithToken(ctx, "early", e.leadTok, a.ID, "Too early."); !errors.Is(err, ErrAttemptState) {
		t.Fatalf("an answer to a working attempt: %v", err)
	}
	a = e.blocked(t, a, "Waiting on the schema decision.")
	tm2 := mustTeam(t, e.s, "t2", "crew-two", ProjectRef{HubID: "hub-test", ProjectID: "project-t"})
	lead2, _ := member(t, e.s, tm2.ID, "lead-two", RoleCoordinator)
	_, ls2 := e.p.enter("e-lead-two", lead2, tm2.ID)
	for name, c := range map[string]struct {
		token, text string
		want        error
	}{
		"the worker":            {e.indepTk, "Answering myself.", ErrForbidden},
		"another worker":        {e.workerTok, "Not mine to answer.", ErrForbidden},
		"another team":          {ls2.Token.Reveal(), "Not my team.", ErrNotFound},
		"blank":                 {e.leadTok, "  \n", ErrInvalid},
		"larger than an answer": {e.leadTok, strings.Repeat("x", maxAnswerText+1), ErrInvalid},
	} {
		if _, err := e.s.AnswerWithToken(ctx, "refused-"+name, c.token, a.ID, c.text); !errors.Is(err, c.want) {
			t.Errorf("%s: %v, want %v", name, err, c.want)
		}
	}
	var n int
	if err := e.s.db.QueryRow(`SELECT COUNT(*) FROM events WHERE kind = 'attempt.answered'`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("%d answer events after refusals, %v", n, err)
	}
}
