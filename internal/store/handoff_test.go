package store

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// The worker pushes its session doc to its running attempt and reads it
// back; each push replaces the doc. No other member reads or writes it,
// and the push is refused once a stop is requested or the attempt is no
// longer running, while the worker can still read what was kept.
func TestAttemptHandoff(t *testing.T) {
	ctx := context.Background()
	e := newClaimStopEnv(t)
	a, _ := e.runningClaim(t, "task-1")

	h, err := e.s.HandoffWithToken(ctx, e.indepTk, a.ID)
	if err != nil || h.AttemptID != a.ID || h.Body != "" || !h.UpdatedAt.IsZero() {
		t.Fatalf("an attempt without a handoff: %+v, %v", h, err)
	}
	doc := "# Handoff\n\n## CURRENT ATTEMPT\n\n### Next\n\nWrite the store test.\n"
	if h, err = e.s.PutHandoffWithToken(ctx, e.indepTk, a.ID, doc); err != nil || h.AttemptID != a.ID || h.UpdatedAt.IsZero() {
		t.Fatalf("the push: %+v, %v", h, err)
	}
	*e.now = e.now.Add(time.Minute)
	doc2 := strings.Replace(doc, "Write the store test.", "Open the pull request.", 1)
	if _, err = e.s.PutHandoffWithToken(ctx, e.indepTk, a.ID, doc2); err != nil {
		t.Fatalf("the second push: %v", err)
	}
	if h, err = e.s.HandoffWithToken(ctx, e.indepTk, a.ID); err != nil || h.Body != doc2 || !h.UpdatedAt.Equal(e.now.UTC()) {
		t.Fatalf("the read after two pushes: %+v, %v", h, err)
	}

	// Not another member's, nor an unknown attempt's.
	for name, tok := range map[string]string{"the coordinator": e.leadTok, "another worker": e.workerTok} {
		if _, err := e.s.PutHandoffWithToken(ctx, tok, a.ID, doc); !errors.Is(err, ErrNoAttempt) {
			t.Errorf("%s's push: %v", name, err)
		}
		if _, err := e.s.HandoffWithToken(ctx, tok, a.ID); !errors.Is(err, ErrNoAttempt) {
			t.Errorf("%s's read: %v", name, err)
		}
	}
	if _, err := e.s.PutHandoffWithToken(ctx, e.indepTk, "no-such-attempt", doc); !errors.Is(err, ErrNoAttempt) {
		t.Errorf("an unknown attempt: %v", err)
	}
	if _, err := e.s.PutHandoffWithToken(ctx, "not-a-token", a.ID, doc); err == nil {
		t.Error("a push without a valid token was accepted")
	}

	// The doc's bounds.
	for name, body := range map[string]string{
		"an empty doc":          " \n",
		"a doc over the bound":  strings.Repeat("x", MaxHandoffBytes+1),
		"a doc that is not UTF": "a\xffb",
		"a doc with a NUL":      "a\x00b",
	} {
		if _, err := e.s.PutHandoffWithToken(ctx, e.indepTk, a.ID, body); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: %v", name, err)
		}
	}
	if _, err := e.s.PutHandoffWithToken(ctx, e.indepTk, a.ID, strings.Repeat("x", MaxHandoffBytes)); err != nil {
		t.Errorf("a doc at the bound: %v", err)
	}
	if _, err := e.s.PutHandoffWithToken(ctx, e.indepTk, a.ID, doc2); err != nil {
		t.Fatal(err)
	}

	// A requested stop refuses the push; the kept doc stays readable.
	if _, err := e.s.RequestStopWithToken(ctx, "stop-1", e.leadTok, a.ID, "priorities changed"); err != nil {
		t.Fatal(err)
	}
	if _, err := e.s.PutHandoffWithToken(ctx, e.indepTk, a.ID, doc); !errors.Is(err, ErrAttemptState) {
		t.Fatalf("a push while a stop is requested: %v", err)
	}
	if h, err = e.s.HandoffWithToken(ctx, e.indepTk, a.ID); err != nil || h.Body != doc2 {
		t.Fatalf("the read while a stop is requested: %+v, %v", h, err)
	}

	// An attempt that is no longer running refuses the push; the worker
	// still reads what was kept.
	if _, err := e.s.db.Exec(`UPDATE attempts SET state = 'closed' WHERE id = ?`, a.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := e.s.PutHandoffWithToken(ctx, e.indepTk, a.ID, doc); !errors.Is(err, ErrAttemptState) {
		t.Fatalf("a push to a closed attempt: %v", err)
	}
	if h, err = e.s.HandoffWithToken(ctx, e.indepTk, a.ID); err != nil || h.Body != doc2 {
		t.Fatalf("the read of a closed attempt: %+v, %v", h, err)
	}
}

// The operator's caller and an architect's read an attempt's handoff, in
// any state of the attempt; a member's caller is refused. The coordinator's
// open attempts and the team state carry the push time, zero before the
// first push.
func TestAttemptHandoffForOperator(t *testing.T) {
	ctx := context.Background()
	e := newClaimStopEnv(t)
	a, _ := e.runningClaim(t, "task-1")
	op, err := OperatorCaller("operator")
	if err != nil {
		t.Fatal(err)
	}

	h, err := e.s.AttemptHandoff(ctx, op, a.ID)
	if err != nil || h.AttemptID != a.ID || h.TeamID != a.TeamID || h.Body != "" || !h.UpdatedAt.IsZero() {
		t.Fatalf("an attempt without a handoff: %+v, %v", h, err)
	}
	pushed := func() (open, state time.Time) {
		t.Helper()
		o, err := e.s.OpenAttemptsWithToken(ctx, e.leadTok)
		if err != nil || len(o.Attempts) != 1 {
			t.Fatalf("the open attempts: %+v, %v", o, err)
		}
		st, err := e.s.GetTeamState(ctx, op, a.TeamID)
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range st.Members {
			if m.OpenAttempt != nil && m.OpenAttempt.ID == a.ID {
				state = m.OpenAttempt.HandoffUpdatedAt
			}
		}
		return o.Attempts[0].HandoffUpdatedAt, state
	}
	if open, state := pushed(); !open.IsZero() || !state.IsZero() {
		t.Fatalf("a push time before the first push: %s, %s", open, state)
	}

	doc := "# Handoff\n\n## CURRENT ATTEMPT\n\n### Next\n\nOpen the pull request.\n"
	if _, err := e.s.PutHandoffWithToken(ctx, e.indepTk, a.ID, doc); err != nil {
		t.Fatal(err)
	}
	if h, err = e.s.AttemptHandoff(ctx, op, a.ID); err != nil || h.Body != doc || !h.UpdatedAt.Equal(e.now.UTC()) {
		t.Fatalf("the operator's read: %+v, %v", h, err)
	}
	if open, state := pushed(); !open.Equal(h.UpdatedAt) || !state.Equal(h.UpdatedAt) {
		t.Fatalf("the push time %s: open attempts %s, team state %s", h.UpdatedAt, open, state)
	}

	agent, err := AgentCaller(a.WorkerAgentID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.s.AttemptHandoff(ctx, agent, a.ID); !errors.Is(err, ErrForbidden) {
		t.Errorf("a member's caller: %v", err)
	}
	if _, err := e.s.AttemptHandoff(ctx, op, "no-such-attempt"); !errors.Is(err, ErrNotFound) {
		t.Errorf("an unknown attempt: %v", err)
	}
	if _, err := e.s.AttemptHandoff(ctx, op, ""); !errors.Is(err, ErrInvalid) {
		t.Errorf("no attempt: %v", err)
	}

	// A closed attempt keeps its handoff for the operator.
	if _, err := e.s.db.Exec(`UPDATE attempts SET state = 'closed' WHERE id = ?`, a.ID); err != nil {
		t.Fatal(err)
	}
	if h, err = e.s.AttemptHandoff(ctx, op, a.ID); err != nil || h.Body != doc {
		t.Fatalf("the read of a closed attempt: %+v, %v", h, err)
	}
}
