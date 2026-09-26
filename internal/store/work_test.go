package store

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"sync"
	"testing"
)

func devDelivery(tag string) TrustedDelivery {
	return TrustedDelivery{Required: DevelopmentDelivery, Evidence: []Evidence{
		{Kind: "reviewed_head", Ref: "https://example.invalid/pull/1#review-" + tag},
		{Kind: "human_merge", Ref: "https://example.invalid/commit/merge-" + tag},
		{Kind: "post_merge_ci", Ref: "https://example.invalid/actions/runs/" + tag},
	}}
}

// running returns a team with an accepted, running attempt.
func running(t *testing.T) (execTeam, Attempt) {
	t.Helper()
	s, _ := openTemp(t)
	e := newExecTeam(t, s)
	a := e.accept(t, "accept", e.offer(t, "offer", "task-1"))
	if a.Phase != PhaseWorking {
		t.Fatalf("accepted attempt phase = %q, want working", a.Phase)
	}
	return e, a
}

func (e execTeam) work(t *testing.T, key string, a Attempt, intent WorkIntent, detail string) (Attempt, error) {
	t.Helper()
	return e.s.UpdateWork(context.Background(), e.builder.caller, e.port, key, a.ID, WorkUpdate{
		SessionID: e.builder.sess.ID, Generation: e.builder.sess.Generation, Intent: intent, Detail: detail})
}

func (e execTeam) mustWork(t *testing.T, key string, a Attempt, intent WorkIntent, detail string) Attempt {
	t.Helper()
	got, err := e.work(t, key, a, intent, detail)
	if err != nil {
		t.Fatalf("%s: %v", intent, err)
	}
	return got
}

func (e execTeam) review(t *testing.T, key string, a Attempt, lead crewMember, seq int64, d ReviewDecision) (Attempt, error) {
	t.Helper()
	return e.s.ReviewResult(context.Background(), lead.caller, key, a.ID, ResultReview{
		SessionID: lead.sess.ID, Generation: lead.sess.Generation, ResultSeq: seq, Decision: d})
}

func (e execTeam) finalize(t *testing.T, key string, a Attempt, by crewMember, seq int64, d TrustedDelivery) (Attempt, error) {
	t.Helper()
	return e.s.FinalizeWork(context.Background(), by.caller, e.port, key, a.ID, FinalizeRequest{
		SessionID: by.sess.ID, Generation: by.sess.Generation, ResultSeq: seq, Delivery: d})
}

func resultRefs(t *testing.T, s *Store, a Attempt) []string {
	t.Helper()
	rs, err := s.AttemptResults(context.Background(), a.ID)
	if err != nil {
		t.Fatal(err)
	}
	refs := []string{}
	for _, r := range rs {
		refs = append(refs, r.ResultRef)
	}
	return refs
}

// The whole work lifecycle: block, resume, submit, review, finalize. Updates
// keep the hold and its fence; finalize closes it and frees the capacity; the
// task content keeps fields aicrew does not own.
func TestWorkLifecycle(t *testing.T) {
	e, a := running(t)
	fence := a.Fence
	a = e.mustWork(t, "w1", a, IntentBlock, "The test database is unavailable.")
	if a.Phase != PhaseBlocked || a.Fence != fence {
		t.Fatalf("after block: %+v", a)
	}
	a = e.mustWork(t, "w2", a, IntentResume, "")
	a = e.mustWork(t, "w3", a, IntentSubmit, "https://example.invalid/pull/1")
	if a.Phase != PhaseSubmitted || a.Fence != fence {
		t.Fatalf("after submit: %+v", a)
	}
	if got := resultRefs(t, e.s, a); !reflect.DeepEqual(got, []string{"https://example.invalid/pull/1"}) {
		t.Fatalf("results = %v", got)
	}
	a, err := e.review(t, "r1", a, e.lead, 1, ReviewAccept)
	if err != nil || a.Phase != PhaseAccepted || a.AcceptedResult != 1 {
		t.Fatalf("accept = %+v, %v", a, err)
	}
	a, err = e.finalize(t, "f1", a, e.lead, 1, devDelivery("1"))
	if err != nil || a.State != AttemptClosed || a.CloseReason != "finalized" || a.Phase != PhaseFinalized || a.FinalizedResult != 1 {
		t.Fatalf("finalize = %+v, %v", a, err)
	}
	if busy(t, e.s, e.builder.agent.ID) {
		t.Fatal("finalized attempt still holds the capacity")
	}
	if h := e.port.holdOf(a.Task); h.active {
		t.Fatal("finalize left the hold active")
	}
	content := e.port.taskContent(a.Task)
	if content["state"] != "DONE" || content["notes"] != "Written by a person in aimem." || content["title"] != "Example task" {
		t.Fatalf("task content after finalize = %v", content)
	}
	texts := []string{}
	for _, it := range read(t, e.s, e.lead, 20) {
		texts = append(texts, it.Text)
	}
	want := []string{
		"builder accepted task hub-a/project-a/task-1 and started work.",
		"builder blocked task hub-a/project-a/task-1: The test database is unavailable.",
		"builder resumed work on task hub-a/project-a/task-1.",
		"builder submitted a result for task hub-a/project-a/task-1: https://example.invalid/pull/1",
		"Task hub-a/project-a/task-1 was finalized as done with result 1.",
	}
	if !reflect.DeepEqual(texts, want) {
		t.Errorf("lead reads %q, want %q", texts, want)
	}
}

// Results are an immutable history, and rework invalidates an acceptance:
// the next result needs its own review and finalize names it.
func TestReworkInvalidatesAcceptance(t *testing.T) {
	e, a := running(t)
	a = e.mustWork(t, "w1", a, IntentSubmit, "https://example.invalid/pull/1")
	if _, err := e.review(t, "r1", a, e.lead, 1, ReviewAccept); err != nil {
		t.Fatal(err)
	}
	a, err := e.review(t, "r2", a, e.lead, 1, ReviewRework)
	if err != nil || a.Phase != PhaseRework || a.AcceptedResult != 0 || a.AcceptedBySession != "" {
		t.Fatalf("rework = %+v, %v", a, err)
	}
	if _, err := e.finalize(t, "f1", a, e.lead, 1, devDelivery("1")); !errors.Is(err, ErrAttemptState) {
		t.Fatalf("finalize after rework: got %v, want ErrAttemptState", err)
	}
	if _, err := e.work(t, "w-submit", a, IntentSubmit, "https://example.invalid/pull/2"); !errors.Is(err, ErrAttemptState) {
		t.Fatalf("submit without resuming from rework: got %v, want ErrAttemptState", err)
	}
	a = e.mustWork(t, "w2", a, IntentResume, "")
	a = e.mustWork(t, "w3", a, IntentSubmit, "https://example.invalid/pull/2")
	if got := resultRefs(t, e.s, a); !reflect.DeepEqual(got, []string{"https://example.invalid/pull/1", "https://example.invalid/pull/2"}) {
		t.Fatalf("results = %v, want both, unchanged", got)
	}
	if _, err := e.review(t, "r3", a, e.lead, 1, ReviewAccept); !errors.Is(err, ErrAttemptState) {
		t.Fatalf("accepting the earlier result: got %v, want ErrAttemptState", err)
	}
	if _, err := e.review(t, "r4", a, e.lead, 2, ReviewAccept); err != nil {
		t.Fatal(err)
	}
	if _, err := e.finalize(t, "f2", a, e.lead, 1, devDelivery("1")); !errors.Is(err, ErrAttemptState) {
		t.Fatalf("finalize naming the earlier result: got %v, want ErrAttemptState", err)
	}
	if got, err := e.finalize(t, "f3", a, e.lead, 2, devDelivery("2")); err != nil || got.FinalizedResult != 2 {
		t.Fatalf("finalize of result 2 = %+v, %v", got, err)
	}
}

// Succession or a resume does not carry a review: a coordinator finalizes
// only from the session and generation that accepted the result.
func TestFinalizeNeedsTheReviewingSession(t *testing.T) {
	ctx := context.Background()
	t.Run("resumed coordinator", func(t *testing.T) {
		e, a := running(t)
		a = e.mustWork(t, "w1", a, IntentSubmit, "https://example.invalid/pull/1")
		if _, err := e.review(t, "r1", a, e.lead, 1, ReviewAccept); err != nil {
			t.Fatal(err)
		}
		resumed, err := e.s.ResumeSession(ctx, e.lead.caller, "resume-lead", e.lead.sess.ID)
		if err != nil {
			t.Fatal(err)
		}
		e.lead.sess = resumed
		if _, err := e.finalize(t, "f1", a, e.lead, 1, devDelivery("1")); !errors.Is(err, ErrForbidden) {
			t.Fatalf("finalize after resume without review: got %v, want ErrForbidden", err)
		}
		if _, err := e.review(t, "r2", a, e.lead, 1, ReviewAccept); err != nil {
			t.Fatal(err)
		}
		if _, err := e.finalize(t, "f2", a, e.lead, 1, devDelivery("1")); err != nil {
			t.Fatalf("finalize after its own review: %v", err)
		}
	})
	t.Run("successor coordinator", func(t *testing.T) {
		e, a := running(t)
		a = e.mustWork(t, "w1", a, IntentSubmit, "https://example.invalid/pull/1")
		if _, err := e.review(t, "r1", a, e.lead, 1, ReviewAccept); err != nil {
			t.Fatal(err)
		}
		if _, err := e.s.StopSession(ctx, operator(t), "stop-lead", e.lead.sess.ID); err != nil {
			t.Fatal(err)
		}
		successor := joinCrew(t, e.s, e.tm.ID, "successor", RoleCoordinator)
		if _, err := e.finalize(t, "f1", a, successor, 1, devDelivery("1")); !errors.Is(err, ErrForbidden) {
			t.Fatalf("successor finalize without review: got %v, want ErrForbidden", err)
		}
		if _, err := e.review(t, "r2", a, successor, 1, ReviewAccept); err != nil {
			t.Fatal(err)
		}
		if got, err := e.finalize(t, "f2", a, successor, 1, devDelivery("1")); err != nil || got.State != AttemptClosed {
			t.Fatalf("successor finalize after review = %+v, %v", got, err)
		}
	})
}

// The holder may finalize its own accepted result, under the same delivery
// requirement; delivery evidence must cover every required kind.
func TestHolderFinalizeAndDeliveryEvidence(t *testing.T) {
	e, a := running(t)
	a = e.mustWork(t, "w1", a, IntentSubmit, "https://example.invalid/pull/1")
	for _, seq := range []int64{0, 1} {
		// Result 0 matches an attempt with no acceptance; the phase refuses it.
		if _, err := e.finalize(t, fmt.Sprintf("f0-%d", seq), a, e.builder, seq, devDelivery("1")); !errors.Is(err, ErrAttemptState) {
			t.Fatalf("finalize of result %d before acceptance: got %v, want ErrAttemptState", seq, err)
		}
	}
	if _, err := e.review(t, "r1", a, e.lead, 1, ReviewAccept); err != nil {
		t.Fatal(err)
	}
	calls := len(e.port.callLog())
	missing := devDelivery("1")
	missing.Evidence = missing.Evidence[:2]
	for name, d := range map[string]TrustedDelivery{
		"missing post-merge CI": missing,
		"no requirement":        {Evidence: devDelivery("1").Evidence},
		"empty reference":       {Required: []string{"reviewed_head"}, Evidence: []Evidence{{Kind: "reviewed_head"}}},
	} {
		if _, err := e.finalize(t, "f-"+name, a, e.builder, 1, d); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: got %v, want ErrInvalid", name, err)
		}
	}
	if len(e.port.callLog()) != calls {
		t.Fatalf("refused finalizes called aimem: %v", e.port.callLog())
	}
	got, err := e.finalize(t, "f1", a, e.builder, 1, devDelivery("1"))
	if err != nil || got.State != AttemptClosed || got.TerminalEvidence == "" {
		t.Fatalf("holder finalize = %+v, %v", got, err)
	}
}

// Of a worker and its coordinator finalizing at once, one wins and aimem is
// asked once.
func TestCompetingFinalizes(t *testing.T) {
	e, a := running(t)
	a = e.mustWork(t, "w1", a, IntentSubmit, "https://example.invalid/pull/1")
	if _, err := e.review(t, "r1", a, e.lead, 1, ReviewAccept); err != nil {
		t.Fatal(err)
	}
	errs := make([]error, 2)
	var wg sync.WaitGroup
	for i, by := range []crewMember{e.builder, e.lead} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, errs[i] = e.finalize(t, fmt.Sprintf("f%d", i), a, by, 1, devDelivery("1"))
		}()
	}
	wg.Wait()
	wins := 0
	for _, err := range errs {
		switch {
		case err == nil:
			wins++
		case !errors.Is(err, ErrAttemptState):
			t.Errorf("unexpected error %v", err)
		}
	}
	finalizes := 0
	for _, c := range e.port.callLog() {
		if c.Op == ReservationFinalize {
			finalizes++
		}
	}
	if wins != 1 || finalizes != 1 {
		t.Fatalf("wins = %d, finalize calls = %d; want 1 and 1", wins, finalizes)
	}
}

// A lost reply on a work step leaves the attempt reconciling with its hold
// and capacity; the retry or a reconciliation completes it once.
func TestWorkStepLostReply(t *testing.T) {
	ctx := context.Background()
	t.Run("submit", func(t *testing.T) {
		e, a := running(t)
		e.port.faults[ReservationUpdate] = faultLostReply
		got, err := e.work(t, "w1", a, IntentSubmit, "https://example.invalid/pull/1")
		if !errors.Is(err, ErrOutcomeUnknown) || got.State != AttemptReconciling {
			t.Fatalf("submit with a lost reply = %+v, %v", got, err)
		}
		if _, err := e.review(t, "r1", a, e.lead, 1, ReviewAccept); !errors.Is(err, ErrAttemptState) {
			t.Fatalf("review while reconciling: got %v, want ErrAttemptState", err)
		}
		if got, err = e.work(t, "w1", a, IntentSubmit, "https://example.invalid/pull/1"); err != nil || got.Phase != PhaseSubmitted {
			t.Fatalf("retry = %+v, %v", got, err)
		}
		if refs := resultRefs(t, e.s, a); len(refs) != 1 {
			t.Fatalf("results after the retry = %v, want one", refs)
		}
	})
	t.Run("finalize", func(t *testing.T) {
		e, a := running(t)
		a = e.mustWork(t, "w1", a, IntentSubmit, "https://example.invalid/pull/1")
		if _, err := e.review(t, "r1", a, e.lead, 1, ReviewAccept); err != nil {
			t.Fatal(err)
		}
		e.port.faults[ReservationFinalize] = faultLostReply
		got, err := e.finalize(t, "f1", a, e.lead, 1, devDelivery("1"))
		if !errors.Is(err, ErrOutcomeUnknown) || got.State != AttemptReconciling || !busy(t, e.s, e.builder.agent.ID) {
			t.Fatalf("finalize with a lost reply = %+v, %v", got, err)
		}
		if got, err = e.s.ReconcileAttempt(ctx, e.lead.caller, e.port, a.ID); err != nil || got.State != AttemptClosed || got.Phase != PhaseFinalized {
			t.Fatalf("reconcile = %+v, %v", got, err)
		}
	})
	t.Run("no second step while one is pending", func(t *testing.T) {
		e, a := running(t)
		e.port.faults[ReservationUpdate] = faultLostReply
		if _, err := e.work(t, "w1", a, IntentBlock, "Waiting for a decision."); !errors.Is(err, ErrOutcomeUnknown) {
			t.Fatalf("block with a lost reply: %v", err)
		}
		calls := len(e.port.callLog())
		if _, err := e.work(t, "w2", a, IntentSubmit, "https://example.invalid/pull/1"); !errors.Is(err, ErrAttemptState) {
			t.Fatalf("submit while the block is unresolved: got %v, want ErrAttemptState", err)
		}
		if len(e.port.callLog()) != calls {
			t.Fatalf("a second step was sent while one was pending: %v", e.port.callLog()[calls:])
		}
	})
	t.Run("not committed", func(t *testing.T) {
		e, a := running(t)
		e.port.faults[ReservationUpdate] = faultBeforeCommit
		if _, err := e.work(t, "w1", a, IntentBlock, "Waiting for a decision."); !errors.Is(err, ErrOutcomeUnknown) {
			t.Fatalf("block before commit: %v", err)
		}
		got, err := e.s.ReconcileAttempt(ctx, e.lead.caller, e.port, a.ID)
		if err != nil || got.State != AttemptRunning || got.Phase != PhaseWorking || got.PendingKey != "" {
			t.Fatalf("reconcile of an uncommitted block = %+v, %v; want working again", got, err)
		}
	})
}

// Work steps need the holder's current session; reviews need the current
// coordinator.
func TestWorkAuthority(t *testing.T) {
	ctx := context.Background()
	e, a := running(t)
	stale := WorkUpdate{SessionID: e.builder.sess.ID, Generation: e.builder.sess.Generation + 1, Intent: IntentSubmit, Detail: "x"}
	if _, err := e.s.UpdateWork(ctx, e.builder.caller, e.port, "w-stale", a.ID, stale); !errors.Is(err, ErrContextStale) {
		t.Errorf("update from a stale generation: got %v, want ErrContextStale", err)
	}
	if _, err := e.s.UpdateWork(ctx, e.lead.caller, e.port, "w-lead", a.ID, WorkUpdate{
		SessionID: e.lead.sess.ID, Generation: e.lead.sess.Generation, Intent: IntentSubmit, Detail: "x"}); !errors.Is(err, ErrNotFound) {
		t.Errorf("update by the coordinator: got %v, want ErrNotFound", err)
	}
	a = e.mustWork(t, "w1", a, IntentSubmit, "https://example.invalid/pull/1")
	if _, err := e.review(t, "r-worker", a, e.builder, 1, ReviewAccept); !errors.Is(err, ErrForbidden) {
		t.Errorf("review by the worker: got %v, want ErrForbidden", err)
	}
	other := joinCrew(t, e.s, e.tm.ID, "tester", RoleWorker)
	if _, err := e.review(t, "r1", a, e.lead, 1, ReviewAccept); err != nil {
		t.Fatal(err)
	}
	if _, err := e.finalize(t, "f-other", a, other, 1, devDelivery("1")); !errors.Is(err, ErrForbidden) {
		t.Errorf("finalize by another worker: got %v, want ErrForbidden", err)
	}
}

// A concurrent change to the task in aimem refuses the update without
// overwriting it; after reconciling the revision, a new update keeps the
// other fields.
func TestConcurrentTaskChangeIsNotOverwritten(t *testing.T) {
	ctx := context.Background()
	e, a := running(t)
	e.port.editTask(a.Task, "notes", "Changed by a person while the worker was busy.")
	got, err := e.work(t, "w1", a, IntentBlock, "Waiting for a decision.")
	var refusal *ReservationRefusal
	if !errors.As(err, &refusal) || refusal.Code != "revision_conflict" || got.Phase != PhaseWorking || got.State != AttemptRunning {
		t.Fatalf("update after a concurrent change = %+v, %v; want a revision conflict", got, err)
	}
	if c := e.port.taskContent(a.Task); c["notes"] != "Changed by a person while the worker was busy." || c["state"] != "IN_PROGRESS" {
		t.Fatalf("the refused update changed the task: %v", c)
	}
	refreshed, err := e.s.ReconcileAttempt(ctx, e.builder.caller, e.port, a.ID)
	if err != nil || refreshed.TaskRevision <= a.TaskRevision {
		t.Fatalf("reconcile = %+v, %v; want a refreshed revision", refreshed, err)
	}
	if _, err := e.work(t, "w2", a, IntentBlock, "Waiting for a decision."); err != nil {
		t.Fatalf("update after reconciling: %v", err)
	}
	if c := e.port.taskContent(a.Task); c["notes"] != "Changed by a person while the worker was busy." || c["state"] != "BLOCKED" {
		t.Fatalf("task after the update = %v; want the other field kept", c)
	}
}

// A committed update reply naming another fence or hold is not trusted: the
// attempt reconciles, and the recorded receipt then completes it.
func TestInconsistentUpdateReplyIsNotTrusted(t *testing.T) {
	ctx := context.Background()
	e, a := running(t)
	e.port.garble = func(r *ReservationResult) { r.Reservation.Fence = "99" }
	got, err := e.work(t, "w1", a, IntentBlock, "Waiting for a decision.")
	if !errors.Is(err, ErrOutcomeUnknown) || got.State != AttemptReconciling {
		t.Fatalf("update with an inconsistent reply = %+v, %v; want reconciling", got, err)
	}
	if got, err = e.s.ReconcileAttempt(ctx, e.builder.caller, e.port, a.ID); err != nil || got.Phase != PhaseBlocked {
		t.Fatalf("reconcile from the recorded receipt = %+v, %v", got, err)
	}
}
