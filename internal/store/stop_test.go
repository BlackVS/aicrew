package store

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

// gate holds the next reservation call of op in flight, after the port has
// received it, until release is called.
func (e execTeam) gate(op ReservationOp) (entered <-chan struct{}, release func()) {
	in, out := make(chan struct{}), make(chan struct{})
	var once sync.Once
	e.port.onMutate = func(got ReservationOp, _ ReservationRequest) {
		if got == op {
			once.Do(func() { close(in); <-out })
		}
	}
	return in, func() { close(out) }
}

// A stop is recorded, with its message, while a reservation call is in
// flight on the attempt; the call's settle, in any outcome, keeps the stop
// and applies it: an update settles as usual, a committed finalize closes
// the attempt, and a refused finalize voids the acceptance it kept.
func TestStopWhileACallIsInFlight(t *testing.T) {
	ctx := context.Background()
	submit := func(e execTeam, a Attempt) error {
		_, err := e.work(t, "submit", a, IntentSubmit, "https://example.invalid/pull/1")
		return err
	}
	finalize := func(e execTeam, a Attempt) error {
		_, err := e.finalize(t, "final", a, e.builder, 1, devDelivery("1"))
		return err
	}
	stillStopping := func(t *testing.T, got Attempt, phase AttemptPhase) {
		t.Helper()
		if got.State != AttemptRunning || got.Stop != StopRequested || got.Phase != phase || got.AcceptedResult != 0 {
			t.Fatalf("attempt = %+v; want running in %s, still stopping, with no acceptance", got, phase)
		}
	}
	finalized := func(t *testing.T, got Attempt, _ AttemptPhase) {
		t.Helper()
		if got.State != AttemptClosed || got.CloseReason != "finalized" || got.FinalizedResult != 1 {
			t.Fatalf("attempt = %+v; want finalized with result 1", got)
		}
	}
	for _, tc := range []struct {
		name      string
		op        ReservationOp
		call      func(e execTeam, a Attempt) error
		accepted  bool
		fault     simFault
		refuse    bool
		reconcile bool // the call's outcome is unknown and is reconciled
		check     func(t *testing.T, got Attempt, phase AttemptPhase)
	}{
		{"update committed", ReservationUpdate, submit, false, faultNone, false, false, stillStopping},
		{"update unknown", ReservationUpdate, submit, false, faultLostReply, false, true, stillStopping},
		{"finalize committed", ReservationFinalize, finalize, true, faultNone, false, false, finalized},
		{"finalize unknown, committed", ReservationFinalize, finalize, true, faultLostReply, false, true, finalized},
		{"finalize not committed", ReservationFinalize, finalize, true, faultBeforeCommit, false, true, stillStopping},
		{"finalize refused", ReservationFinalize, finalize, true, faultNone, true, false, stillStopping},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e, a := running(t)
			if tc.accepted {
				e.mustWork(t, "submit", a, IntentSubmit, "https://example.invalid/pull/1")
				if _, err := e.review(t, "accept", a, e.lead, 1, ReviewAccept); err != nil {
					t.Fatal(err)
				}
			}
			e.port.faults[tc.op] = tc.fault
			if tc.refuse {
				e.port.refuse[tc.op] = fixtureRefusal(t, "stale_worker")
			}
			// Waiting on the step lock would fail fast rather than pass late.
			e.s.flightWait = 200 * time.Millisecond
			entered, release := e.gate(tc.op)
			done := make(chan error, 1)
			go func() { done <- tc.call(e, a) }()
			<-entered

			got, err := e.requestStop(t, "stop", a, e.lead, "priorities changed")
			txt := lastText(t, e, e.builder)
			release()
			callErr := <-done
			if err != nil || got.Stop != StopRequested || got.PendingKey == "" {
				t.Fatalf("stop with a call in flight = %+v, %v", got, err)
			}
			if tc.accepted && got.AcceptedResult != 1 {
				t.Fatalf("stop with a finalize in flight dropped its acceptance: %+v", got)
			}
			if txt != "lead requested a stop of task hub-a/project-a/task-1 for builder: priorities changed" {
				t.Fatalf("stop message before the call returned = %q", txt)
			}
			switch {
			case tc.reconcile:
				if !errors.Is(callErr, ErrOutcomeUnknown) {
					t.Fatalf("call = %v, want an unknown outcome", callErr)
				}
				if cur := mustState(t, e.s, a.ID, AttemptReconciling); cur.Stop != StopRequested {
					t.Fatalf("reconciling attempt lost its stop: %+v", cur)
				}
				if _, err := e.s.ReconcileAttempt(ctx, e.builder.caller, e.port, a.ID); err != nil {
					t.Fatal(err)
				}
			case tc.refuse:
				if callErr == nil {
					t.Fatal("the refused call succeeded")
				}
			case callErr != nil:
				t.Fatalf("call: %v", callErr)
			}
			cur, err := e.s.GetAttempt(ctx, a.ID)
			if err != nil {
				t.Fatal(err)
			}
			tc.check(t, cur, PhaseSubmitted)
		})
	}
}

// newExecTeamNamed is a further execution team in the same store.
func newExecTeamNamed(t *testing.T, s *Store, name string) execTeam {
	t.Helper()
	tm := mustTeam(t, s, name, "crew-"+name, projectA)
	return execTeam{s: s, tm: tm,
		lead:    joinCrew(t, s, tm.ID, "lead-"+name, RoleCoordinator),
		builder: joinCrew(t, s, tm.ID, "builder-"+name, RoleWorker),
		port:    newFakeReservations(t)}
}

func (e execTeam) requestStop(t *testing.T, key string, a Attempt, by crewMember, reason string) (Attempt, error) {
	t.Helper()
	return e.s.RequestStop(context.Background(), by.caller, key, a.ID, StopRequest{
		SessionID: by.sess.ID, Generation: by.sess.Generation, Reason: reason})
}

func (e execTeam) confirmStop(t *testing.T, key string, a Attempt) (Attempt, error) {
	t.Helper()
	return e.s.ConfirmStop(context.Background(), e.builder.caller, key, a.ID, e.builder.sess.ID, e.builder.sess.Generation)
}

func (e execTeam) releaseStopped(t *testing.T, key string, a Attempt, target ReleaseTarget, blocker string) (Attempt, error) {
	t.Helper()
	return e.s.ReleaseStopped(context.Background(), e.builder.caller, e.port, key, a.ID, StopRelease{
		SessionID: e.builder.sess.ID, Generation: e.builder.sess.Generation, Target: target, Blocker: blocker})
}

// mustStop requires the attempt to be running with the given stop state, its
// hold in place in aimem and the worker's capacity taken.
func (e execTeam) mustStop(t *testing.T, a Attempt, want StopState) Attempt {
	t.Helper()
	got := mustState(t, e.s, a.ID, AttemptRunning)
	if got.Stop != want || !busy(t, e.s, e.builder.agent.ID) || !e.port.holdOf(a.Task).active {
		t.Fatalf("attempt = stop %q, busy %v, hold %+v; want stop %q with the hold and the capacity kept",
			got.Stop, busy(t, e.s, e.builder.agent.ID), e.port.holdOf(a.Task), want)
	}
	return got
}

func lastText(t *testing.T, e execTeam, m crewMember) string {
	t.Helper()
	items := read(t, e.s, m, 50)
	if len(items) == 0 {
		t.Fatal("no messages")
	}
	return items[len(items)-1].Text
}

// Request and confirmation are local and keep the hold and the capacity;
// the holder's release closes the attempt, frees the capacity and writes
// only aicrew's fields into the task.
func TestStopLifecycle(t *testing.T) {
	for _, tc := range []struct {
		target  ReleaseTarget
		blocker string
		message string
	}{
		{ReleaseReady, "", "builder released stopped task hub-a/project-a/task-1 to READY."},
		{ReleaseBlocked, "needs a schema decision", "builder released stopped task hub-a/project-a/task-1 as BLOCKED: needs a schema decision"},
	} {
		t.Run(string(tc.target), func(t *testing.T) {
			e, a := running(t)
			calls := len(e.port.callLog())

			got, err := e.requestStop(t, "stop", a, e.lead, "priorities changed")
			if err != nil || got.Stop != StopRequested || got.StopReason != "priorities changed" ||
				got.StopSession != e.lead.sess.ID || got.StopBy == "" || got.StopAt.IsZero() {
				t.Fatalf("request = %+v, %v", got, err)
			}
			if txt := lastText(t, e, e.builder); txt != "lead requested a stop of task hub-a/project-a/task-1 for builder: priorities changed" {
				t.Fatalf("stop request message = %q", txt)
			}
			e.mustStop(t, a, StopRequested)
			if _, err := e.confirmStop(t, "confirm", a); err != nil {
				t.Fatal(err)
			}
			if txt := lastText(t, e, e.lead); txt != "builder confirmed the stop of task hub-a/project-a/task-1." {
				t.Fatalf("confirmation message = %q", txt)
			}
			e.mustStop(t, a, StopConfirmed)
			if len(e.port.callLog()) != calls {
				t.Fatalf("request or confirmation called aimem: %v", e.port.callLog()[calls:])
			}

			got, err = e.releaseStopped(t, "release", a, tc.target, tc.blocker)
			if err != nil || got.State != AttemptClosed || got.CloseReason != "stopped" || busy(t, e.s, e.builder.agent.ID) {
				t.Fatalf("release = %+v, %v", got, err)
			}
			if e.port.holdOf(a.Task).active {
				t.Fatal("the hold is still active after the release")
			}
			content := e.port.taskContent(a.Task)
			if content["state"] != string(tc.target) || content["notes"] != "Written by a person in aimem." || content["title"] != "Example task" {
				t.Fatalf("task content = %+v", content)
			}
			if blocker, _ := content["blocker"].(string); blocker != tc.blocker {
				t.Fatalf("blocker = %q, want %q", blocker, tc.blocker)
			}
			if txt := lastText(t, e, e.lead); txt != tc.message {
				t.Fatalf("release message = %q", txt)
			}
			if _, err := e.releaseStopped(t, "release-2", a, tc.target, tc.blocker); !errors.Is(err, ErrAttemptState) {
				t.Fatalf("a second release: got %v, want ErrAttemptState", err)
			}
		})
	}
}

// Who may request, confirm and release, and from which session.
func TestStopAuthority(t *testing.T) {
	ctx := context.Background()
	e, a := running(t)

	if _, err := e.requestStop(t, "by-worker", a, e.builder, "tired"); !errors.Is(err, ErrForbidden) {
		t.Errorf("stop requested by the worker: got %v, want ErrForbidden", err)
	}
	stale := e.lead
	stale.sess.Generation++
	if _, err := e.requestStop(t, "stale", a, stale, "why"); !errors.Is(err, ErrContextStale) {
		t.Errorf("stop from a stale coordinator generation: got %v, want ErrContextStale", err)
	}
	other := newExecTeamNamed(t, e.s, "t2")
	if _, err := e.requestStop(t, "other-team", a, other.lead, "why"); !errors.Is(err, ErrNotFound) {
		t.Errorf("stop by another team's coordinator: got %v, want ErrNotFound", err)
	}
	if _, err := e.s.RequestStop(ctx, operator(t), "op-session", a.ID, StopRequest{SessionID: e.lead.sess.ID, Reason: "why"}); !errors.Is(err, ErrInvalid) {
		t.Errorf("operator stop naming a session: got %v, want ErrInvalid", err)
	}
	if _, err := e.requestStop(t, "blank", a, e.lead, "  "); !errors.Is(err, ErrInvalid) {
		t.Errorf("stop without a reason: got %v, want ErrInvalid", err)
	}
	if _, err := e.requestStop(t, "long", a, e.lead, strings.Repeat("x", maxMessageText)); !errors.Is(err, ErrInvalid) {
		t.Errorf("stop reason too long for its message: got %v, want ErrInvalid", err)
	}
	if _, err := e.confirmStop(t, "early", a); !errors.Is(err, ErrAttemptState) {
		t.Errorf("confirmation with no stop requested: got %v, want ErrAttemptState", err)
	}
	e.mustStop(t, a, StopNone)

	// A successor coordinator, after a resume, may request the stop; the
	// generation before the resume may not.
	resumed, err := e.s.ResumeSession(ctx, e.lead.caller, "resume-lead", e.lead.sess.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.requestStop(t, "old-gen", a, e.lead, "why"); !errors.Is(err, ErrContextStale) {
		t.Errorf("stop from the generation before the resume: got %v, want ErrContextStale", err)
	}
	e.lead.sess = resumed
	if _, err := e.requestStop(t, "stop", a, e.lead, "priorities changed"); err != nil {
		t.Fatal(err)
	}
	if _, err := e.requestStop(t, "stop-again", a, e.lead, "again"); !errors.Is(err, ErrAttemptState) {
		t.Errorf("a second stop request: got %v, want ErrAttemptState", err)
	}
	if _, err := e.s.ConfirmStop(ctx, e.lead.caller, "confirm-lead", a.ID, e.lead.sess.ID, e.lead.sess.Generation); !errors.Is(err, ErrNotFound) {
		t.Errorf("confirmation by the coordinator: got %v, want ErrNotFound", err)
	}
	if _, err := e.releaseStopped(t, "release-early", a, ReleaseReady, ""); !errors.Is(err, ErrAttemptState) {
		t.Errorf("release before confirmation: got %v, want ErrAttemptState", err)
	}
	if _, err := e.confirmStop(t, "confirm", a); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []StopRelease{
		{Target: "CANCELLED"}, {Target: "DONE"}, {Target: ReleaseReady, Blocker: "x"}, {Target: ReleaseBlocked},
	} {
		bad.SessionID, bad.Generation = e.builder.sess.ID, e.builder.sess.Generation
		if _, err := e.s.ReleaseStopped(ctx, e.builder.caller, e.port, "bad-"+string(bad.Target), a.ID, bad); !errors.Is(err, ErrInvalid) {
			t.Errorf("release %+v: got %v, want ErrInvalid", bad, err)
		}
	}
	if _, err := e.releaseStopped(t, "long-blocker", a, ReleaseBlocked, strings.Repeat("x", maxMessageText)); !errors.Is(err, ErrInvalid) {
		t.Errorf("blocker too long for its message: got %v, want ErrInvalid", err)
	}
	lead := StopRelease{SessionID: e.lead.sess.ID, Generation: e.lead.sess.Generation, Target: ReleaseReady}
	if _, err := e.s.ReleaseStopped(ctx, e.lead.caller, e.port, "release-lead", a.ID, lead); !errors.Is(err, ErrNotFound) {
		t.Errorf("release by the coordinator: got %v, want ErrNotFound", err)
	}
	e.mustStop(t, a, StopConfirmed)
}

// The operator may request a stop without a session; the worker confirms it
// as any other.
func TestOperatorStop(t *testing.T) {
	ctx := context.Background()
	e, a := running(t)
	got, err := e.s.RequestStop(ctx, operator(t), "op-stop", a.ID, StopRequest{Reason: "incident"})
	if err != nil || got.Stop != StopRequested || got.StopSession != "" {
		t.Fatalf("operator stop = %+v, %v", got, err)
	}
	if txt := lastText(t, e, e.lead); txt != "The operator requested a stop of task hub-a/project-a/task-1 for builder: incident" {
		t.Fatalf("operator stop message = %q", txt)
	}
	if _, err := e.confirmStop(t, "confirm", a); err != nil {
		t.Fatal(err)
	}
	if got, err := e.releaseStopped(t, "release", a, ReleaseReady, ""); err != nil || got.State != AttemptClosed {
		t.Fatalf("release = %+v, %v", got, err)
	}
}

// A stop can be requested only for running work: not an offer, and not a
// closed attempt.
func TestStopNeedsRunningWork(t *testing.T) {
	s, _ := openTemp(t)
	e := newExecTeam(t, s)
	offer := e.offer(t, "o1", "task-1")
	if _, err := e.requestStop(t, "stop-offer", offer, e.lead, "why"); !errors.Is(err, ErrAttemptState) {
		t.Errorf("stop of an offer: got %v, want ErrAttemptState", err)
	}
	if _, err := e.release(t, "r1", offer); err != nil {
		t.Fatal(err)
	}
	if _, err := e.requestStop(t, "stop-closed", offer, e.lead, "why"); !errors.Is(err, ErrAttemptState) {
		t.Errorf("stop of a closed attempt: got %v, want ErrAttemptState", err)
	}
}

// Nothing but the worker's own confirmation confirms a stop: not time, a
// reconciliation, the worker's session being resumed or replaced, or a
// coordinator change. The worker cannot leave meanwhile. The worker's new
// session may confirm explicitly.
func TestOnlyTheWorkerConfirmsAStop(t *testing.T) {
	ctx := context.Background()
	e, a := running(t)
	now := time.Now().UTC()
	e.s.now = func() time.Time { return now }
	if _, err := e.requestStop(t, "stop", a, e.lead, "priorities changed"); err != nil {
		t.Fatal(err)
	}

	now = now.Add(30 * 24 * time.Hour)
	if _, err := e.s.ReconcileAttempt(ctx, operator(t), e.port, a.ID); err != nil {
		t.Fatal(err)
	}
	e.mustStop(t, a, StopRequested)
	resumed, err := e.s.ResumeSession(ctx, e.builder.caller, "resume-builder", e.builder.sess.ID)
	if err != nil {
		t.Fatal(err)
	}
	e.mustStop(t, a, StopRequested)
	if _, err := e.s.LeaveSession(ctx, e.builder.caller, "leave", resumed.ID, resumed.Generation); !errors.Is(err, ErrWorkOutstanding) {
		t.Fatalf("worker leave while stopping: got %v, want ErrWorkOutstanding", err)
	}
	// The session is lost and replaced.
	if _, err := e.s.StopSession(ctx, operator(t), "end-builder", resumed.ID); err != nil {
		t.Fatal(err)
	}
	e.mustStop(t, a, StopRequested)
	replaced, err := e.s.StartSession(ctx, e.builder.caller, "restart-builder", e.tm.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.s.ResumeSession(ctx, e.lead.caller, "resume-lead", e.lead.sess.ID); err != nil {
		t.Fatal(err)
	}
	e.mustStop(t, a, StopRequested)

	old := e.builder.sess
	if _, err := e.s.ConfirmStop(ctx, e.builder.caller, "confirm-old", a.ID, old.ID, old.Generation); err == nil {
		t.Fatal("confirmation from an ended session was accepted")
	}
	e.mustStop(t, a, StopRequested)
	e.builder.sess = replaced
	if _, err := e.confirmStop(t, "confirm", a); err != nil {
		t.Fatalf("confirmation from the worker's new session: %v", err)
	}
	e.mustStop(t, a, StopConfirmed)
}

// While a stop is requested or confirmed, no work update, review or
// finalize is accepted.
func TestStopRefusesWork(t *testing.T) {
	e, a := running(t)
	e.mustWork(t, "submit", a, IntentSubmit, "https://example.invalid/pull/1")
	if _, err := e.review(t, "accept", a, e.lead, 1, ReviewAccept); err != nil {
		t.Fatal(err)
	}
	if _, err := e.requestStop(t, "stop", a, e.lead, "priorities changed"); err != nil {
		t.Fatal(err)
	}
	refused := func(stage string) {
		t.Helper()
		if _, err := e.work(t, "resume-"+stage, a, IntentResume, ""); !errors.Is(err, ErrAttemptState) {
			t.Errorf("%s: update got %v, want ErrAttemptState", stage, err)
		}
		if _, err := e.review(t, "review-"+stage, a, e.lead, 1, ReviewRework); !errors.Is(err, ErrAttemptState) {
			t.Errorf("%s: review got %v, want ErrAttemptState", stage, err)
		}
		if _, err := e.finalize(t, "final-"+stage, a, e.builder, 1, devDelivery("1")); !errors.Is(err, ErrAttemptState) {
			t.Errorf("%s: finalize got %v, want ErrAttemptState", stage, err)
		}
	}
	refused("requested")
	if _, err := e.confirmStop(t, "confirm", a); err != nil {
		t.Fatal(err)
	}
	refused("confirmed")
	e.mustStop(t, a, StopConfirmed)
}

// A requested stop voids a recorded acceptance: the result returns to
// submitted, and stays in the history.
func TestStopVoidsAcceptance(t *testing.T) {
	e, a := running(t)
	e.mustWork(t, "submit", a, IntentSubmit, "https://example.invalid/pull/1")
	if _, err := e.review(t, "accept", a, e.lead, 1, ReviewAccept); err != nil {
		t.Fatal(err)
	}
	got, err := e.requestStop(t, "stop", a, e.lead, "priorities changed")
	if err != nil || got.Phase != PhaseSubmitted || got.AcceptedResult != 0 || got.AcceptedBySession != "" {
		t.Fatalf("stop of an accepted result = %+v, %v; want the acceptance voided", got, err)
	}
	if refs := resultRefs(t, e.s, a); len(refs) != 1 {
		t.Fatalf("results = %v", refs)
	}
}

// A step in flight does not prevent a stop request; it settles by the usual
// rules and never clears the stop. The worker confirms only once it is
// settled.
func TestStopWithAStepInFlight(t *testing.T) {
	ctx := context.Background()
	e, a := running(t)
	e.port.faults[ReservationUpdate] = faultLostReply
	if _, err := e.work(t, "submit", a, IntentSubmit, "https://example.invalid/pull/1"); !errors.Is(err, ErrOutcomeUnknown) {
		t.Fatalf("submit with a lost reply: %v", err)
	}
	if got, err := e.requestStop(t, "stop", a, e.lead, "priorities changed"); err != nil || got.State != AttemptReconciling {
		t.Fatalf("stop while reconciling = %+v, %v", got, err)
	}
	if _, err := e.confirmStop(t, "confirm-early", a); !errors.Is(err, ErrAttemptState) {
		t.Fatalf("confirmation while a step is in flight: got %v, want ErrAttemptState", err)
	}
	got, err := e.s.ReconcileAttempt(ctx, e.builder.caller, e.port, a.ID)
	if err != nil || got.Phase != PhaseSubmitted || got.Stop != StopRequested {
		t.Fatalf("reconciled submit = %+v, %v; want submitted, still stopping", got, err)
	}
	if _, err := e.confirmStop(t, "confirm", a); err != nil {
		t.Fatal(err)
	}
	if got, err := e.releaseStopped(t, "release", a, ReleaseReady, ""); err != nil || got.State != AttemptClosed {
		t.Fatalf("release = %+v, %v", got, err)
	}
}

// A finalize in flight when the stop is requested keeps its acceptance: if
// aimem committed it, the attempt is finalized; if not, the acceptance is
// voided then and the stop goes on.
func TestStopWithAFinalizeInFlight(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		fault simFault
		want  AttemptState
	}{{faultLostReply, AttemptClosed}, {faultBeforeCommit, AttemptRunning}} {
		t.Run(string(tc.fault), func(t *testing.T) {
			e, a := running(t)
			e.mustWork(t, "submit", a, IntentSubmit, "https://example.invalid/pull/1")
			if _, err := e.review(t, "accept", a, e.lead, 1, ReviewAccept); err != nil {
				t.Fatal(err)
			}
			e.port.faults[ReservationFinalize] = tc.fault
			if _, err := e.finalize(t, "final", a, e.builder, 1, devDelivery("1")); !errors.Is(err, ErrOutcomeUnknown) {
				t.Fatalf("finalize: %v", err)
			}
			got, err := e.requestStop(t, "stop", a, e.lead, "priorities changed")
			if err != nil || got.AcceptedResult != 1 {
				t.Fatalf("stop with a finalize in flight = %+v, %v; want the acceptance kept", got, err)
			}
			got, err = e.s.ReconcileAttempt(ctx, e.builder.caller, e.port, a.ID)
			if err != nil || got.State != tc.want {
				t.Fatalf("reconciled finalize = %+v, %v; want %s", got, err, tc.want)
			}
			if tc.want == AttemptClosed {
				if got.CloseReason != "finalized" || got.FinalizedResult != 1 {
					t.Fatalf("finalized attempt = %+v", got)
				}
				return
			}
			if got.AcceptedResult != 0 || got.Phase != PhaseSubmitted || got.Stop != StopRequested {
				t.Fatalf("after the finalize was not committed = %+v; want the acceptance voided, still stopping", got)
			}
		})
	}
}

// The release follows the step ordering: a lost reply is reconciled by its
// receipt, which closes the attempt and frees the capacity; an uncommitted
// call keeps the stopped attempt and its capacity; a final refusal is a
// known outcome that keeps it too.
func TestStopReleaseOutcomes(t *testing.T) {
	ctx := context.Background()
	confirmed := func(t *testing.T) (execTeam, Attempt) {
		e, a := running(t)
		if _, err := e.requestStop(t, "stop", a, e.lead, "priorities changed"); err != nil {
			t.Fatal(err)
		}
		if _, err := e.confirmStop(t, "confirm", a); err != nil {
			t.Fatal(err)
		}
		return e, a
	}

	t.Run("lost reply", func(t *testing.T) {
		e, a := confirmed(t)
		e.port.faults[ReservationRelease] = faultLostReply
		got, err := e.releaseStopped(t, "release", a, ReleaseBlocked, "waiting on a decision")
		if !errors.Is(err, ErrOutcomeUnknown) || got.State != AttemptReconciling || !busy(t, e.s, e.builder.agent.ID) {
			t.Fatalf("release with a lost reply = %+v, %v; want reconciling with the capacity", got, err)
		}
		// Status cannot settle it; the receipt for the same key does.
		if _, err := e.s.ReconcileAttempt(ctx, e.builder.caller, blindStatus{e.port}, a.ID); err != nil {
			t.Fatal(err)
		}
		got = mustState(t, e.s, a.ID, AttemptClosed)
		if got.CloseReason != "stopped" || busy(t, e.s, e.builder.agent.ID) {
			t.Fatalf("reconciled release = %+v", got)
		}
		if n := len(e.port.callLog()); e.port.callLog()[n-1].Op != ReservationRelease {
			t.Fatalf("calls = %v", e.port.callLog())
		}
	})
	t.Run("not committed", func(t *testing.T) {
		e, a := confirmed(t)
		e.port.faults[ReservationRelease] = faultBeforeCommit
		if _, err := e.releaseStopped(t, "release", a, ReleaseReady, ""); !errors.Is(err, ErrOutcomeUnknown) {
			t.Fatalf("release before commit: %v", err)
		}
		if _, err := e.s.ReconcileAttempt(ctx, e.builder.caller, e.port, a.ID); err != nil {
			t.Fatal(err)
		}
		e.mustStop(t, a, StopConfirmed)
		if got, err := e.releaseStopped(t, "release-2", a, ReleaseReady, ""); err != nil || got.State != AttemptClosed {
			t.Fatalf("release under a new key = %+v, %v", got, err)
		}
	})
	t.Run("final refusal", func(t *testing.T) {
		e, a := confirmed(t)
		e.port.refuse[ReservationRelease] = fixtureRefusal(t, "stale_worker")
		if _, err := e.releaseStopped(t, "release", a, ReleaseReady, ""); err == nil {
			t.Fatal("a refused release succeeded")
		}
		got := e.mustStop(t, a, StopConfirmed)
		if got.LastRefusal != "stale_fence" {
			t.Fatalf("last refusal = %q", got.LastRefusal)
		}
	})
	t.Run("changed task", func(t *testing.T) {
		e, a := confirmed(t)
		e.port.editTask(a.Task, "notes", "changed in aimem")
		if _, err := e.releaseStopped(t, "release", a, ReleaseReady, ""); err == nil {
			t.Fatal("a release over a concurrent change succeeded")
		}
		e.mustStop(t, a, StopConfirmed)
		if e.port.taskContent(a.Task)["notes"] != "changed in aimem" {
			t.Fatal("the concurrent change was overwritten")
		}
		// The attempt refreshes its revision from its hold and releases
		// under a new key, keeping the change.
		if _, err := e.s.ReconcileAttempt(ctx, e.builder.caller, e.port, a.ID); err != nil {
			t.Fatal(err)
		}
		if got, err := e.releaseStopped(t, "release-2", a, ReleaseReady, ""); err != nil || got.State != AttemptClosed {
			t.Fatalf("release after refreshing the revision = %+v, %v", got, err)
		}
		if c := e.port.taskContent(a.Task); c["notes"] != "changed in aimem" || c["state"] != "READY" {
			t.Fatalf("task content = %+v", c)
		}
	})
}

// A status read never closes a stopping attempt: with the hold invisible to
// the caller, or released outside aicrew, the attempt stays open with its
// capacity for operator recovery.
func TestStatusNeverClosesAStoppingAttempt(t *testing.T) {
	ctx := context.Background()
	e, a := running(t)
	check := func(want StopState) {
		t.Helper()
		if _, err := e.s.ReconcileAttempt(ctx, operator(t), blindStatus{e.port}, a.ID); !errors.Is(err, ErrOutcomeUnknown) {
			t.Fatalf("reconcile with an invisible hold: got %v, want ErrOutcomeUnknown", err)
		}
		e.mustStop(t, a, want)
	}
	if _, err := e.requestStop(t, "stop", a, e.lead, "priorities changed"); err != nil {
		t.Fatal(err)
	}
	check(StopRequested)
	if _, err := e.confirmStop(t, "confirm", a); err != nil {
		t.Fatal(err)
	}
	check(StopConfirmed)
	e.port.recoveryRelease(a.Task)
	if _, err := e.s.ReconcileAttempt(ctx, operator(t), e.port, a.ID); !errors.Is(err, ErrOutcomeUnknown) {
		t.Fatalf("reconcile after a release outside aicrew: got %v, want ErrOutcomeUnknown", err)
	}
	if got := mustState(t, e.s, a.ID, AttemptRunning); got.Stop != StopConfirmed || !busy(t, e.s, e.builder.agent.ID) {
		t.Fatalf("attempt = %+v; want it stopped and holding the capacity", got)
	}
}

// The schema refuses an unknown stop state.
func TestStopStateIsChecked(t *testing.T) {
	e, a := running(t)
	if _, err := e.s.db.Exec(`UPDATE attempts SET stop = 'cancelled' WHERE id = ?`, a.ID); err == nil {
		t.Fatal("the schema accepted an unknown stop state")
	}
}
