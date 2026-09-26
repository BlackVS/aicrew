package store

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"sync"
	"testing"
	"time"
)

var testPin = TrustedProcess{
	Identity:          ProcessIdentity{Repository: "github.com/example/process", Commit: "abc1234", Manifest: "process.yaml"},
	InstructionDigest: "sha256:worker-instructions-v1",
}

// execTeam is a team with a coordinator and a worker in session, and a fake
// reservation service.
type execTeam struct {
	s       *Store
	tm      Team
	lead    crewMember
	builder crewMember
	port    *fakeReservations
}

func newExecTeam(t *testing.T, s *Store) execTeam {
	t.Helper()
	tm := mustTeam(t, s, "t1", "crew")
	return execTeam{s: s, tm: tm,
		lead:    joinCrew(t, s, tm.ID, "lead", RoleCoordinator),
		builder: joinCrew(t, s, tm.ID, "builder", RoleWorker),
		port:    newFakeReservations(t)}
}

func task(id string) TaskRef { return TaskRef{HubID: "hub-a", ProjectID: "project-a", TaskID: id} }

func (e execTeam) offerReq(taskID string) OfferRequest {
	return OfferRequest{
		SessionID: e.lead.sess.ID, Generation: e.lead.sess.Generation, WorkerAgentID: e.builder.agent.ID,
		Task: task(taskID), ExpectedRevision: 3, BaseCommit: "base-commit-1", Branch: "work/" + taskID,
		Process: testPin, ExpiresAt: e.s.now().Add(time.Hour),
	}
}

func (e execTeam) acceptReq() AcceptRequest {
	return AcceptRequest{SessionID: e.builder.sess.ID, Generation: e.builder.sess.Generation,
		Selected: testPin, InstructionDigest: testPin.InstructionDigest}
}

func (e execTeam) offer(t *testing.T, key, taskID string) Attempt {
	t.Helper()
	a, err := e.s.OfferTask(context.Background(), e.lead.caller, e.port, key, e.offerReq(taskID))
	if err != nil {
		t.Fatalf("offer %s: %v", taskID, err)
	}
	return a
}

func (e execTeam) accept(t *testing.T, key string, a Attempt) Attempt {
	t.Helper()
	out, err := e.s.AcceptOffer(context.Background(), e.builder.caller, e.port, key, a.ID, e.acceptReq())
	if err != nil {
		t.Fatalf("accept: %v", err)
	}
	return out
}

func (e execTeam) release(t *testing.T, key string, a Attempt) (Attempt, error) {
	t.Helper()
	return e.s.ReleaseOffer(context.Background(), e.lead.caller, e.port, key, a.ID, e.lead.sess.ID, e.lead.sess.Generation)
}

func mustState(t *testing.T, s *Store, id string, want AttemptState) Attempt {
	t.Helper()
	a, err := s.GetAttempt(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	if a.State != want {
		t.Fatalf("attempt %s is %s (%s), want %s", id, a.State, a.CloseReason, want)
	}
	return a
}

func busy(t *testing.T, s *Store, agentID string) bool {
	t.Helper()
	var n int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM attempts WHERE worker_agent_id = ? AND state != 'closed'`, agentID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n > 0
}

// The offer path follows the contract order: the intent (with the worker's
// capacity) is committed before aimem is called, and the confirmed result
// with its lifecycle message after.
func TestOfferAndAcceptFollowTheContractOrder(t *testing.T) {
	s, _ := openTemp(t)
	e := newExecTeam(t, s)
	e.port.onMutate = func(op ReservationOp, req ReservationRequest) {
		var a Attempt
		err := s.snapshot(context.Background(), func(q querier) error {
			return q.QueryRowContext(context.Background(),
				`SELECT id FROM attempts WHERE pending_key = ?`, req.RequestKey).Scan(&a.ID)
		})
		if err != nil {
			t.Errorf("%s was sent before its intent was committed: %v", op, err)
		}
		if req.CoordinationProof == "" {
			t.Errorf("%s carries no coordination reference", op)
		}
	}
	a := e.offer(t, "o1", "task-1")
	if a.State != AttemptOffered || a.Fence != "1" || a.ReservationID == "" || a.PendingKey != "" {
		t.Fatalf("after offer: %+v", a)
	}
	if a.Process != testPin || a.Task != task("task-1") || a.CoordinatorGeneration != e.lead.sess.CoordinatorGeneration {
		t.Fatalf("offer did not record its context: %+v", a)
	}
	if !busy(t, s, e.builder.agent.ID) {
		t.Fatal("an offered worker's capacity is free")
	}
	items := read(t, s, e.builder, 10)
	if len(items) != 1 || items[0].Kind != KindLifecycle || items[0].Text != "lead offered task hub-a/project-a/task-1 to builder." {
		t.Fatalf("builder reads %+v, want the offer announcement", items)
	}

	a = e.accept(t, "a1", a)
	if a.State != AttemptRunning || a.Fence != "2" {
		t.Fatalf("after accept: %+v", a)
	}
	if h := e.port.holdOf(a.Task); !h.active || h.workRef != a.attemptRef() {
		t.Fatalf("aimem hold after accept = %+v", h)
	}
	want := []fakeCall{{ReservationClaim, requestKey(a.ID, ReservationClaim, 1)}, {ReservationTransfer, requestKey(a.ID, ReservationTransfer, 2)}}
	if got := e.port.callLog(); !reflect.DeepEqual(got, want) {
		t.Fatalf("calls = %v, want %v", got, want)
	}
	for _, op := range []string{opOfferTask, opAcceptOffer} {
		if n := count(t, s, "audit WHERE operation = '"+op+"'"); n != 1 {
			t.Errorf("audit records for %s = %d, want 1", op, n)
		}
	}
	if n := count(t, s, "audit WHERE operation = '"+opSettle+"'"); n != 2 {
		t.Errorf("settle audit records = %d, want 2", n)
	}
}

// One execution capacity per agent across teams: of two coordinators
// offering the same worker at once, one wins, and aimem is asked once.
func TestOneCapacityPerAgentAcrossTeams(t *testing.T) {
	ctx := context.Background()
	s, _ := openTemp(t)
	e := newExecTeam(t, s)
	t2 := mustTeam(t, s, "t2", "crew-two")
	lead2 := joinCrew(t, s, t2.ID, "lead2", RoleCoordinator)
	if _, err := s.AddMember(ctx, operator(t), "member-builder-t2", t2.ID, e.builder.agent.ID, RoleWorker); err != nil {
		t.Fatal(err)
	}
	offers := []struct {
		c   Caller
		req OfferRequest
	}{{e.lead.caller, e.offerReq("task-1")}, {lead2.caller, e.offerReq("task-2")}}
	offers[1].req.SessionID, offers[1].req.Generation = lead2.sess.ID, lead2.sess.Generation

	errs := make([]error, 2)
	var wg sync.WaitGroup
	for i := range offers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, errs[i] = s.OfferTask(ctx, offers[i].c, e.port, fmt.Sprintf("o%d", i), offers[i].req)
		}()
	}
	wg.Wait()
	wins, busyRefusals := 0, 0
	for _, err := range errs {
		switch {
		case err == nil:
			wins++
		case errors.Is(err, ErrAgentBusy):
			busyRefusals++
		default:
			t.Errorf("unexpected error %v", err)
		}
	}
	if wins != 1 || busyRefusals != 1 {
		t.Fatalf("wins = %d, busy refusals = %d; want 1 and 1", wins, busyRefusals)
	}
	if calls := e.port.callLog(); len(calls) != 1 {
		t.Fatalf("aimem calls = %v, want one claim", calls)
	}
}

// A reply lost after aimem committed leaves the attempt reconciling with the
// worker's capacity held; no other transition is sent until a receipt lookup
// with the same key completes the step.
func TestLostReplyAtEachStep(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		op        ReservationOp
		afterward AttemptState
	}{
		{ReservationClaim, AttemptOffered},
		{ReservationTransfer, AttemptRunning},
		{ReservationRelease, AttemptClosed},
	} {
		t.Run(string(tc.op), func(t *testing.T) {
			s, _ := openTemp(t)
			e := newExecTeam(t, s)
			var a Attempt
			var err error
			offer := e.offerReq("task-1") // a retry sends the identical request
			step := func() {
				switch tc.op {
				case ReservationClaim:
					a, err = s.OfferTask(ctx, e.lead.caller, e.port, "o1", offer)
				case ReservationTransfer:
					a, err = s.AcceptOffer(ctx, e.builder.caller, e.port, "a1", a.ID, e.acceptReq())
				case ReservationRelease:
					a, err = e.release(t, "r1", a)
				}
			}
			if tc.op != ReservationClaim {
				a = e.offer(t, "o1", "task-1")
			}
			e.port.faults[tc.op] = faultLostReply
			step()
			if !errors.Is(err, ErrOutcomeUnknown) || a.State != AttemptReconciling || a.PendingOp != tc.op {
				t.Fatalf("%s with a lost reply = %+v, %v; want reconciling", tc.op, a, err)
			}
			if !busy(t, s, e.builder.agent.ID) {
				t.Fatal("the worker's capacity was freed while the outcome is unknown")
			}
			calls := len(e.port.callLog())
			if _, err := s.AcceptOffer(ctx, e.builder.caller, e.port, "a-while", a.ID, e.acceptReq()); !errors.Is(err, ErrAttemptState) {
				t.Errorf("accept while reconciling: got %v, want ErrAttemptState", err)
			}
			if _, err := e.release(t, "r-while", a); !errors.Is(err, ErrAttemptState) {
				t.Errorf("release while reconciling: got %v, want ErrAttemptState", err)
			}
			if got := len(e.port.callLog()); got != calls {
				t.Fatalf("a transition was sent while reconciling: %v", e.port.callLog()[calls:])
			}
			// Retrying the same step reconciles; it does not call aimem again.
			step()
			if err != nil || a.State != tc.afterward {
				t.Fatalf("retry of the %s step = %+v, %v; want %s", tc.op, a, err, tc.afterward)
			}
			if got := len(e.port.callLog()); got != calls {
				t.Fatalf("the retry called aimem again: %v", e.port.callLog()[calls:])
			}
		})
	}
}

// Every unknown outcome keeps the capacity and reconciles by receipt: a
// failure before commit is closed as not committed, an unresolved receipt or
// a failed lookup stays reconciling, and a retryable refusal is unknown too.
func TestUnknownOutcomesAreReconciled(t *testing.T) {
	ctx := context.Background()
	s, _ := openTemp(t)
	e := newExecTeam(t, s)

	e.port.faults[ReservationClaim] = faultBeforeCommit
	a, err := s.OfferTask(ctx, e.lead.caller, e.port, "o1", e.offerReq("task-1"))
	if !errors.Is(err, ErrOutcomeUnknown) || a.State != AttemptReconciling {
		t.Fatalf("failure before commit = %+v, %v", a, err)
	}
	e.port.unresolved[a.PendingKey] = true
	if a, err = s.ReconcileAttempt(ctx, e.lead.caller, e.port, a.ID); !errors.Is(err, ErrOutcomeUnknown) || a.State != AttemptReconciling {
		t.Fatalf("unresolved receipt = %+v, %v; want still reconciling", a, err)
	}
	delete(e.port.unresolved, a.PendingKey)
	e.port.receiptErr = errSimulatedTransport
	if a, err = s.ReconcileAttempt(ctx, e.lead.caller, e.port, a.ID); !errors.Is(err, ErrOutcomeUnknown) || a.State != AttemptReconciling {
		t.Fatalf("failed receipt lookup = %+v, %v; want still reconciling", a, err)
	}
	e.port.receiptErr = nil
	if !busy(t, s, e.builder.agent.ID) {
		t.Fatal("capacity freed before the outcome was known")
	}
	a, err = s.ReconcileAttempt(ctx, e.lead.caller, e.port, a.ID)
	if err != nil || a.State != AttemptClosed || a.CloseReason != "claim not_committed" || busy(t, s, e.builder.agent.ID) {
		t.Fatalf("not committed = %+v, %v; want closed with the capacity free", a, err)
	}
	if calls := e.port.callLog(); len(calls) != 1 {
		t.Fatalf("reconciliation sent a mutation: %v", calls)
	}

	e.port.refuse[ReservationClaim] = fixtureRefusal(t, "introspection_outage")
	a, err = s.OfferTask(ctx, e.lead.caller, e.port, "o2", e.offerReq("task-2"))
	if !errors.Is(err, ErrOutcomeUnknown) || a.State != AttemptReconciling || !busy(t, s, e.builder.agent.ID) {
		t.Fatalf("retryable refusal = %+v, %v; want reconciling with the capacity held", a, err)
	}
}

// A refusal that is not retryable is a known outcome: a refused claim closes
// and frees the capacity, and a refused transfer or release leaves the offer.
func TestFinalRefusals(t *testing.T) {
	ctx := context.Background()
	s, _ := openTemp(t)
	e := newExecTeam(t, s)

	e.port.standaloneHold(task("task-1"), "someone-else")
	a, err := s.OfferTask(ctx, e.lead.caller, e.port, "o1", e.offerReq("task-1"))
	var refusal *ReservationRefusal
	if !errors.As(err, &refusal) || refusal.Code != "reservation_conflict" {
		t.Fatalf("claim of a held task: got %v, want reservation_conflict", err)
	}
	if a.State != AttemptClosed || a.CloseReason != "claim reservation_conflict" || busy(t, s, e.builder.agent.ID) {
		t.Fatalf("refused claim = %+v; want closed with the capacity free", a)
	}

	a = e.offer(t, "o2", "task-2")
	e.port.refuse[ReservationTransfer] = fixtureRefusal(t, "stale_worker")
	if a, err = s.AcceptOffer(ctx, e.builder.caller, e.port, "a1", a.ID, e.acceptReq()); !errors.As(err, &refusal) {
		t.Fatalf("refused transfer: got %v", err)
	}
	if a.State != AttemptOffered || a.LastRefusal != "stale_fence" || a.PendingKey != "" {
		t.Fatalf("after a refused transfer: %+v; want the offer back", a)
	}
	// Retrying the refused acceptance reports the refusal, not success.
	if again, err := s.AcceptOffer(ctx, e.builder.caller, e.port, "a1", a.ID, e.acceptReq()); !errors.Is(err, ErrAttemptState) || again.State != AttemptOffered {
		t.Fatalf("retry of a refused acceptance = %+v, %v; want ErrAttemptState", again, err)
	}
	e.port.refuse[ReservationRelease] = fixtureRefusal(t, "stale_worker")
	if a, err = e.release(t, "r1", a); !errors.As(err, &refusal) || a.State != AttemptOffered {
		t.Fatalf("refused release = %+v, %v; want the offer back", a, err)
	}
}

// A committed reply that does not match the pending request, or names a
// hold other than the one expected, is not trusted: the attempt reconciles,
// and the recorded receipt then completes it.
func TestInconsistentReplyIsNotTrusted(t *testing.T) {
	ctx := context.Background()
	for name, garble := range map[string]func(*ReservationResult){
		"other request key": func(r *ReservationResult) { r.Receipt.RequestKey = "someone-elses-key" },
		"other work ref":    func(r *ReservationResult) { r.Reservation.OwnWorkRef = "aicrew-offer-other" },
		"inactive hold":     func(r *ReservationResult) { r.Reservation.Active = false },
	} {
		t.Run(name, func(t *testing.T) {
			s, _ := openTemp(t)
			e := newExecTeam(t, s)
			e.port.garble = garble
			a, err := s.OfferTask(ctx, e.lead.caller, e.port, "o1", e.offerReq("task-1"))
			if !errors.Is(err, ErrOutcomeUnknown) || a.State != AttemptReconciling || !busy(t, s, e.builder.agent.ID) {
				t.Fatalf("inconsistent reply = %+v, %v; want reconciling with the capacity held", a, err)
			}
			if got, err := s.ReconcileAttempt(ctx, e.lead.caller, e.port, a.ID); err != nil || got.State != AttemptOffered {
				t.Fatalf("reconcile from the recorded receipt = %+v, %v", got, err)
			}
		})
	}
}

// A retried command reports its own step's outcome. A refused step stays
// refused on retry even after a later command with another key succeeded.
func TestRetryReportsItsOwnStep(t *testing.T) {
	ctx := context.Background()
	s, _ := openTemp(t)
	e := newExecTeam(t, s)
	a := e.offer(t, "o1", "task-1")

	e.port.refuse[ReservationRelease] = fixtureRefusal(t, "stale_worker")
	if _, err := e.release(t, "r1", a); err == nil {
		t.Fatal("the refused release succeeded")
	}
	e.port.refuse[ReservationTransfer] = fixtureRefusal(t, "stale_worker")
	if _, err := s.AcceptOffer(ctx, e.builder.caller, e.port, "a1", a.ID, e.acceptReq()); err == nil {
		t.Fatal("the refused acceptance succeeded")
	}
	if got := e.accept(t, "a2", a); got.State != AttemptRunning {
		t.Fatalf("second acceptance = %+v", got)
	}
	if got, err := s.AcceptOffer(ctx, e.builder.caller, e.port, "a1", a.ID, e.acceptReq()); !errors.Is(err, ErrAttemptState) || got.State != AttemptRunning {
		t.Fatalf("retry of the refused acceptance after another succeeded = %+v, %v; want ErrAttemptState", got, err)
	}
	if got, err := s.AcceptOffer(ctx, e.builder.caller, e.port, "a2", a.ID, e.acceptReq()); err != nil || got.State != AttemptRunning {
		t.Fatalf("retry of the successful acceptance = %+v, %v; want success", got, err)
	}
	if _, err := e.release(t, "r1", a); !errors.Is(err, ErrAttemptState) {
		t.Fatalf("retry of the refused release: got %v, want ErrAttemptState", err)
	}
}

// When aimem shows the hold released by recovery, reconciliation closes the
// attempt and frees the capacity. A failed status read, or a hold aicrew
// does not recognize, changes nothing.
func TestRecoveryReleaseClosesTheAttempt(t *testing.T) {
	ctx := context.Background()
	s, _ := openTemp(t)
	e := newExecTeam(t, s)
	a := e.accept(t, "a1", e.offer(t, "o1", "task-1"))

	if got, err := s.ReconcileAttempt(ctx, e.builder.caller, e.port, a.ID); err != nil || got.State != AttemptRunning {
		t.Fatalf("reconcile with the hold in place = %+v, %v", got, err)
	}
	e.port.statusErr = errSimulatedTransport
	if got, err := s.ReconcileAttempt(ctx, e.builder.caller, e.port, a.ID); !errors.Is(err, ErrOutcomeUnknown) || got.State != AttemptRunning {
		t.Fatalf("status unavailable = %+v, %v; want unchanged", got, err)
	}
	e.port.statusErr = nil
	e.port.recoveryRelease(a.Task)
	e.port.standaloneHold(a.Task, "a-new-holder")
	if got, err := s.ReconcileAttempt(ctx, e.builder.caller, e.port, a.ID); !errors.Is(err, ErrOutcomeUnknown) || got.State != AttemptRunning {
		t.Fatalf("unrecognized hold = %+v, %v; want unchanged, for operator recovery", got, err)
	}
	e.port.recoveryRelease(a.Task)
	got, err := s.ReconcileAttempt(ctx, e.builder.caller, e.port, a.ID)
	if err != nil || got.State != AttemptClosed || got.CloseReason != "recovered" || busy(t, s, e.builder.agent.ID) {
		t.Fatalf("after recovery release = %+v, %v; want closed as recovered", got, err)
	}
	lead := read(t, s, e.lead, 10)
	if len(lead) == 0 || lead[len(lead)-1].Text != "Aimem no longer holds task hub-a/project-a/task-1 for builder, so the attempt was closed as recovered." {
		t.Fatalf("lead reads %+v, want the recovery announcement", lead)
	}
}

// Neither a timeout, nor expiry, nor a hold with no local attempt releases
// or adopts anything.
func TestNoReleaseOrAdoptionWithoutEvidence(t *testing.T) {
	ctx := context.Background()
	s, _ := openTemp(t)
	e := newExecTeam(t, s)
	now := time.Now().UTC()
	s.now = func() time.Time { return now }
	a := e.offer(t, "o1", "task-1")

	now = now.Add(2 * time.Hour) // the offer has expired
	if _, err := s.AcceptOffer(ctx, e.builder.caller, e.port, "a1", a.ID, e.acceptReq()); !errors.Is(err, ErrOfferExpired) {
		t.Fatalf("accept after expiry: got %v, want ErrOfferExpired", err)
	}
	if h := e.port.holdOf(a.Task); !h.active || h.workRef != a.offerRef() {
		t.Fatalf("expiry changed the aimem hold: %+v", h)
	}
	mustState(t, s, a.ID, AttemptOffered)

	e.port.faults[ReservationRelease] = faultBeforeCommit
	if a, err := e.release(t, "r1", a); !errors.Is(err, ErrOutcomeUnknown) || a.State != AttemptReconciling {
		t.Fatalf("release that timed out = %+v, %v", a, err)
	}
	if h := e.port.holdOf(a.Task); !h.active {
		t.Fatal("a timed-out release released the hold")
	}

	orphan := task("task-orphan")
	e.port.standaloneHold(orphan, "aicrew-offer-unknown")
	calls := len(e.port.callLog())
	if _, err := s.ReconcileAttempt(ctx, e.lead.caller, e.port, "unknown-attempt"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("reconcile with no local attempt: got %v, want ErrNotFound", err)
	}
	if h := e.port.holdOf(orphan); !h.active || len(e.port.callLog()) != calls {
		t.Fatal("a hold with no local attempt was released or adopted")
	}
}

// An attempt left reconciling survives a restart and reconciles afterwards.
func TestRestartWhileReconciling(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "aicrew.db")
	s := mustOpen(t, path)
	e := newExecTeam(t, s)
	e.port.faults[ReservationClaim] = faultLostReply
	a, err := s.OfferTask(ctx, e.lead.caller, e.port, "o1", e.offerReq("task-1"))
	if !errors.Is(err, ErrOutcomeUnknown) {
		t.Fatalf("offer: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s = mustOpen(t, path)
	defer s.Close()
	mustState(t, s, a.ID, AttemptReconciling)
	got, err := s.ReconcileAttempt(ctx, e.lead.caller, e.port, a.ID)
	if err != nil || got.State != AttemptOffered || got.Fence != "1" {
		t.Fatalf("reconcile after restart = %+v, %v", got, err)
	}
}

// Stale sessions are refused before anything is written or sent, and an
// offer made under a coordinator generation that has since changed cannot
// be accepted; the current coordinator releases it.
func TestStaleSessionsAndCoordinatorChange(t *testing.T) {
	ctx := context.Background()
	s, _ := openTemp(t)
	e := newExecTeam(t, s)

	req := e.offerReq("task-1")
	req.SessionID, req.Generation = e.builder.sess.ID, e.builder.sess.Generation
	if _, err := s.OfferTask(ctx, e.builder.caller, e.port, "o-worker", req); !errors.Is(err, ErrForbidden) {
		t.Errorf("offer by a worker: got %v, want ErrForbidden", err)
	}
	req = e.offerReq("task-1")
	req.Generation++
	if _, err := s.OfferTask(ctx, e.lead.caller, e.port, "o-stale", req); !errors.Is(err, ErrContextStale) {
		t.Errorf("offer from a stale generation: got %v, want ErrContextStale", err)
	}
	a := e.offer(t, "o1", "task-1")
	stale := e.acceptReq()
	stale.Generation++
	if _, err := s.AcceptOffer(ctx, e.builder.caller, e.port, "a-stale", a.ID, stale); !errors.Is(err, ErrContextStale) {
		t.Errorf("accept from a stale generation: got %v, want ErrContextStale", err)
	}
	if n := count(t, s, "attempts"); n != 1 || len(e.port.callLog()) != 1 {
		t.Fatalf("refusals wrote %d attempts and sent %v", n, e.port.callLog())
	}

	// The coordinator restarts: its generation, and the team's, advance.
	resumed, err := s.ResumeSession(ctx, e.lead.caller, "resume-lead", e.lead.sess.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.AcceptOffer(ctx, e.builder.caller, e.port, "a1", a.ID, e.acceptReq()); !errors.Is(err, ErrOfferStale) {
		t.Fatalf("accept after the coordinator changed: got %v, want ErrOfferStale", err)
	}
	if _, err := e.release(t, "r-old", a); !errors.Is(err, ErrContextStale) {
		t.Errorf("release from the old coordinator generation: got %v, want ErrContextStale", err)
	}
	e.lead.sess = resumed
	got, err := e.release(t, "r1", a)
	if err != nil || got.State != AttemptClosed || got.CloseReason != "withdrawn" || busy(t, s, e.builder.agent.ID) {
		t.Fatalf("release by the current coordinator = %+v, %v", got, err)
	}
}

// Acceptance must match the process recorded on the offer: a changed
// selection is refused for reconciliation or re-issue, never re-pinned.
func TestAcceptanceMatchesTheRecordedProcess(t *testing.T) {
	ctx := context.Background()
	s, _ := openTemp(t)
	e := newExecTeam(t, s)

	bad := e.offerReq("task-1")
	bad.Process.InstructionDigest = ""
	if _, err := s.OfferTask(ctx, e.lead.caller, e.port, "o-bad", bad); !errors.Is(err, ErrInvalid) {
		t.Fatalf("offer without an instruction digest: got %v, want ErrInvalid", err)
	}
	a := e.offer(t, "o1", "task-1")

	changed := e.acceptReq()
	changed.Selected.Identity.Commit = "def5678"
	if _, err := s.AcceptOffer(ctx, e.builder.caller, e.port, "a-changed", a.ID, changed); !errors.Is(err, ErrProcessChanged) {
		t.Fatalf("accept after the selection changed: got %v, want ErrProcessChanged", err)
	}
	changed = e.acceptReq()
	changed.Selected.InstructionDigest = "sha256:worker-instructions-v2"
	changed.InstructionDigest = changed.Selected.InstructionDigest
	if _, err := s.AcceptOffer(ctx, e.builder.caller, e.port, "a-changed-2", a.ID, changed); !errors.Is(err, ErrProcessChanged) {
		t.Fatalf("accept after the instructions changed: got %v, want ErrProcessChanged", err)
	}
	mismatch := e.acceptReq()
	mismatch.InstructionDigest = "sha256:something-else"
	if _, err := s.AcceptOffer(ctx, e.builder.caller, e.port, "a-mismatch", a.ID, mismatch); !errors.Is(err, ErrProcessMismatch) {
		t.Fatalf("accept with other instructions: got %v, want ErrProcessMismatch", err)
	}
	if got := mustState(t, s, a.ID, AttemptOffered); got.Process != testPin {
		t.Fatalf("the recorded process changed: %+v", got.Process)
	}
	if len(e.port.callLog()) != 1 {
		t.Fatalf("refused acceptances called aimem: %v", e.port.callLog())
	}
	e.accept(t, "a1", a)
}

// A decline is recorded locally and blocks acceptance; the hold stays until
// the coordinator releases it.
func TestDeclineThenRelease(t *testing.T) {
	ctx := context.Background()
	s, _ := openTemp(t)
	e := newExecTeam(t, s)
	a := e.offer(t, "o1", "task-1")
	declined, err := s.DeclineOffer(ctx, e.builder.caller, "d1", a.ID, e.builder.sess.ID, e.builder.sess.Generation)
	if err != nil || !declined.Declined || declined.State != AttemptOffered {
		t.Fatalf("decline = %+v, %v", declined, err)
	}
	if h := e.port.holdOf(a.Task); !h.active || len(e.port.callLog()) != 1 {
		t.Fatal("a decline released the hold")
	}
	if _, err := s.DeclineOffer(ctx, e.builder.caller, "d2", a.ID, e.builder.sess.ID, e.builder.sess.Generation); !errors.Is(err, ErrAttemptState) {
		t.Errorf("second decline: got %v, want ErrAttemptState", err)
	}
	if _, err := s.AcceptOffer(ctx, e.builder.caller, e.port, "a1", a.ID, e.acceptReq()); !errors.Is(err, ErrOfferDeclined) {
		t.Errorf("accept after decline: got %v, want ErrOfferDeclined", err)
	}
	got, err := e.release(t, "r1", a)
	if err != nil || got.State != AttemptClosed || got.CloseReason != "declined" {
		t.Fatalf("release after decline = %+v, %v", got, err)
	}
	if h := e.port.holdOf(a.Task); h.active {
		t.Fatal("the hold survived the release")
	}
	texts := []string{}
	for _, it := range read(t, s, e.lead, 10) {
		texts = append(texts, it.Text)
	}
	want := []string{"builder declined the offer of task hub-a/project-a/task-1.", "The offer of task hub-a/project-a/task-1 to builder was released."}
	if !reflect.DeepEqual(texts, want) {
		t.Errorf("lead reads %q, want %q", texts, want)
	}
}

// Concurrent retries of one acceptance transfer the hold once.
func TestConcurrentSameKeyAcceptance(t *testing.T) {
	ctx := context.Background()
	s, _ := openTemp(t)
	e := newExecTeam(t, s)
	a := e.offer(t, "o1", "task-1")
	const n = 6
	results := make([]Attempt, n)
	errs := make([]error, n)
	var wg sync.WaitGroup
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			results[i], errs[i] = s.AcceptOffer(ctx, e.builder.caller, e.port, "a1", a.ID, e.acceptReq())
		}()
	}
	wg.Wait()
	for i := range n {
		if errs[i] != nil || results[i].State != AttemptRunning {
			t.Fatalf("acceptance %d = %+v, %v", i, results[i], errs[i])
		}
	}
	transfers := 0
	for _, c := range e.port.callLog() {
		if c.Op == ReservationTransfer {
			transfers++
		}
	}
	if transfers != 1 {
		t.Fatalf("transfers sent = %d, want 1", transfers)
	}
}

// Open work refuses leaving the team and rebinding the identity.
func TestOpenWorkBlocksLeaveAndRebind(t *testing.T) {
	ctx := context.Background()
	s, _ := openTemp(t)
	e := newExecTeam(t, s)
	a := e.offer(t, "o1", "task-1")

	if _, err := s.LeaveSession(ctx, e.lead.caller, "leave-lead", e.lead.sess.ID, e.lead.sess.Generation); !errors.Is(err, ErrWorkOutstanding) {
		t.Errorf("coordinator leave with an open offer: got %v, want ErrWorkOutstanding", err)
	}
	e.accept(t, "a1", a)
	if _, err := s.LeaveSession(ctx, e.builder.caller, "leave-builder", e.builder.sess.ID, e.builder.sess.Generation); !errors.Is(err, ErrWorkOutstanding) {
		t.Errorf("worker leave with running work: got %v, want ErrWorkOutstanding", err)
	}
	if _, err := s.LeaveSession(ctx, e.lead.caller, "leave-lead-2", e.lead.sess.ID, e.lead.sess.Generation); err != nil {
		t.Errorf("coordinator leave once the offer runs: %v", err)
	}
	// Identity rebind through a real rebind invitation: refused for the
	// worker with running work, allowed for the coordinator, whose offer
	// now runs and is no longer its open work.
	v := newFakeVerifier()
	rebind := func(key string, m crewMember, user string) error {
		t.Helper()
		sc := scope(t, "inv-"+key, PurposeRebind, e.tm.ID, RoleWorker, m.agent.ID, user, "")
		if m.agent.ID == e.lead.agent.ID {
			sc = scope(t, "inv-"+key, PurposeRebind, e.tm.ID, RoleCoordinator, m.agent.ID, user, "")
		}
		ch, err := s.issueForTest(ctx, "issue-"+key, sc)
		if err != nil {
			t.Fatal(err)
		}
		_, err = s.redeemForTest(ctx, "redeem-"+key, sc, v, ch.ID, v.receipt("rc-"+key, "hub-a", user, "tok-"+key))
		return err
	}
	if err := rebind("builder", e.builder, "user-new-builder"); !errors.Is(err, ErrWorkOutstanding) {
		t.Fatalf("rebind of a worker with running work: got %v, want ErrWorkOutstanding", err)
	}
	if err := rebind("lead", e.lead, "user-new-lead"); err != nil {
		t.Fatalf("rebind of a coordinator with no open offer: %v", err)
	}
}

// Only the operator or a member of the attempt's team may reconcile it.
func TestReconcileAuthorization(t *testing.T) {
	ctx := context.Background()
	s, _ := openTemp(t)
	e := newExecTeam(t, s)
	a := e.offer(t, "o1", "task-1")
	outsider := mustAgent(t, s, "outsider", "outsider")
	if _, err := s.ReconcileAttempt(ctx, agentCaller(t, outsider.ID), e.port, a.ID); !errors.Is(err, ErrForbidden) {
		t.Fatalf("reconcile by an outsider: got %v, want ErrForbidden", err)
	}
	if _, err := s.ReconcileAttempt(ctx, operator(t), e.port, a.ID); err != nil {
		t.Fatalf("reconcile by the operator: %v", err)
	}
}

// staleAccept requires acceptance to be refused as offer_stale without
// changing the offer, the aimem hold or the worker's capacity.
func staleAccept(t *testing.T, e execTeam, a Attempt, worker crewMember) {
	t.Helper()
	calls := len(e.port.callLog())
	req := AcceptRequest{SessionID: worker.sess.ID, Generation: worker.sess.Generation,
		Selected: testPin, InstructionDigest: testPin.InstructionDigest}
	if _, err := e.s.AcceptOffer(context.Background(), worker.caller, e.port, "accept-"+worker.sess.ID+"-"+fmt.Sprint(worker.sess.Generation), a.ID, req); !errors.Is(err, ErrOfferStale) {
		t.Fatalf("accept after the worker's context changed: got %v, want ErrOfferStale", err)
	}
	mustState(t, e.s, a.ID, AttemptOffered)
	if h := e.port.holdOf(a.Task); !h.active || h.workRef != a.offerRef() {
		t.Fatalf("the refusal changed the aimem hold: %+v", h)
	}
	if !busy(t, e.s, worker.agent.ID) {
		t.Fatal("the refusal freed the worker's capacity")
	}
	if got := e.port.callLog(); len(got) != calls {
		t.Fatalf("the refused acceptance called aimem: %v", got[calls:])
	}
}

// An offer is bound to the worker's session context when it was issued. A
// resume, a replacement session or a credential rotation makes it stale;
// the current coordinator's release still closes it and frees the capacity.
func TestWorkerContextChangeMakesOffersStale(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name   string
		change func(t *testing.T, e execTeam) Session
	}{
		{"resume", func(t *testing.T, e execTeam) Session {
			sess, err := e.s.ResumeSession(ctx, e.builder.caller, "resume-builder", e.builder.sess.ID)
			if err != nil {
				t.Fatal(err)
			}
			return sess
		}},
		{"replacement session", func(t *testing.T, e execTeam) Session {
			if _, err := e.s.StopSession(ctx, operator(t), "stop-builder", e.builder.sess.ID); err != nil {
				t.Fatal(err)
			}
			sess, err := e.s.StartSession(ctx, e.builder.caller, "restart-builder", e.tm.ID)
			if err != nil {
				t.Fatal(err)
			}
			return sess
		}},
		{"credential rotation", func(t *testing.T, e execTeam) Session {
			v := newFakeVerifier()
			ch, err := e.s.IssueAgentChallenge(ctx, Caller{}, "challenge-builder", e.builder.agent.ID)
			if err != nil {
				t.Fatal(err)
			}
			rc := v.receipt("rc-rotate", ch.HubID, "user-"+e.builder.agent.ID, "token-rotated")
			if res, err := e.s.CompleteAgentProof(ctx, Caller{}, v, "proof-builder", ch.ID, rc); err != nil || !res.Rotated {
				t.Fatalf("rotation = %+v, %v", res, err)
			}
			sess, err := e.s.GetSession(ctx, e.builder.sess.ID)
			if err != nil {
				t.Fatal(err)
			}
			return sess
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, _ := openTemp(t)
			e := newExecTeam(t, s)
			a := e.offer(t, "o1", "task-1")
			if a.WorkerSessionID != e.builder.sess.ID || a.WorkerGeneration != e.builder.sess.Generation {
				t.Fatalf("the offer did not record the worker's session: %+v", a)
			}
			e.builder.sess = tc.change(t, e)
			staleAccept(t, e, a, e.builder)

			// The current coordinator releases it; a re-issued offer, bound to
			// the worker's new context, can be accepted.
			if got, err := e.release(t, "r1", a); err != nil || got.State != AttemptClosed || busy(t, s, e.builder.agent.ID) {
				t.Fatalf("release of the stale offer = %+v, %v", got, err)
			}
			again := e.offer(t, "o2", "task-1")
			if got := e.accept(t, "a2", again); got.State != AttemptRunning {
				t.Fatalf("re-issued offer = %+v", got)
			}
		})
	}
}

// An offer made while the worker has no session can be accepted from the
// worker's first session, but not after that session advances.
func TestOfferToAWorkerWithoutSession(t *testing.T) {
	ctx := context.Background()
	for _, resume := range []bool{false, true} {
		t.Run(fmt.Sprintf("resumed=%v", resume), func(t *testing.T) {
			s, _ := openTemp(t)
			tm := mustTeam(t, s, "t1", "crew")
			lead := joinCrew(t, s, tm.ID, "lead", RoleCoordinator)
			agent, caller := member(t, s, tm.ID, "builder", RoleWorker)
			e := execTeam{s: s, tm: tm, lead: lead, builder: crewMember{agent: agent, caller: caller}, port: newFakeReservations(t)}
			a := e.offer(t, "o1", "task-1")
			if a.WorkerSessionID != "" || a.WorkerGeneration != 0 {
				t.Fatalf("offer recorded a worker session that does not exist: %+v", a)
			}
			sess, err := s.StartSession(ctx, caller, "start-builder", tm.ID)
			if err != nil {
				t.Fatal(err)
			}
			e.builder.sess = sess
			if resume {
				if e.builder.sess, err = s.ResumeSession(ctx, caller, "resume-builder", sess.ID); err != nil {
					t.Fatal(err)
				}
				staleAccept(t, e, a, e.builder)
				return
			}
			if got := e.accept(t, "a1", a); got.State != AttemptRunning {
				t.Fatalf("acceptance from the first session = %+v", got)
			}
		})
	}
}
