package store

import (
	"context"
	"encoding/json"
	"errors"
	"sort"
	"strings"
	"sync"
	"testing"
)

// claimTeam is an execution team whose project set holds hub-a/project-a,
// with a coordinator, a worker and an independent member in session.
type claimTeam struct {
	execTeam
	solo crewMember
}

var projectA = ProjectRef{HubID: "hub-a", ProjectID: "project-a"}

func newClaimTeam(t *testing.T, s *Store) claimTeam {
	t.Helper()
	tm := mustTeam(t, s, "t1", "crew", projectA)
	e := execTeam{s: s, tm: tm,
		lead:    joinCrew(t, s, tm.ID, "lead", RoleCoordinator),
		builder: joinCrew(t, s, tm.ID, "builder", RoleWorker),
		port:    newFakeReservations(t)}
	return claimTeam{execTeam: e, solo: joinCrew(t, s, tm.ID, "solo", RoleIndependent)}
}

func (e claimTeam) claimReq(by crewMember, taskID string) ClaimRequest {
	return ClaimRequest{SessionID: by.sess.ID, Generation: by.sess.Generation, Task: task(taskID),
		ExpectedRevision: 3, BaseCommit: "base-commit-1", Branch: "work/" + taskID,
		Process: testPin, InstructionDigest: testPin.InstructionDigest}
}

func (e claimTeam) claim(t *testing.T, key string, by crewMember, taskID string) (Attempt, error) {
	t.Helper()
	return e.s.ClaimTask(context.Background(), by.caller, e.port, key, e.claimReq(by, taskID))
}

func (e claimTeam) soloWork(t *testing.T, key string, a Attempt, intent WorkIntent, detail string) (Attempt, error) {
	t.Helper()
	return e.s.UpdateWork(context.Background(), e.solo.caller, e.port, key, a.ID, WorkUpdate{
		SessionID: e.solo.sess.ID, Generation: e.solo.sess.Generation, Intent: intent, Detail: detail})
}

// A claim takes the claimer's capacity with its intent, then asks aimem to
// hold the task for this exact attempt; the committed claim runs the
// attempt, which is then submitted, reviewed by the team's coordinator and
// finalized with delivery evidence like any other.
func TestClaimLifecycle(t *testing.T) {
	s, _ := openTemp(t)
	e := newClaimTeam(t, s)
	var atIntent struct{ state, origin string }
	e.port.onMutate = func(op ReservationOp, _ ReservationRequest) {
		if op == ReservationClaim {
			if err := s.db.QueryRow(`SELECT state, origin FROM attempts WHERE worker_agent_id = ?`, e.solo.agent.ID).
				Scan(&atIntent.state, &atIntent.origin); err != nil {
				t.Errorf("read the attempt during the call: %v", err)
			}
		}
	}
	a, err := e.claim(t, "c1", e.solo, "task-1")
	if err != nil {
		t.Fatal(err)
	}
	if atIntent.state != string(AttemptClaiming) || atIntent.origin != string(OriginClaim) {
		t.Fatalf("attempt during the claim call = %+v; want a claiming claim holding the capacity", atIntent)
	}
	if a.State != AttemptRunning || a.Phase != PhaseWorking || a.Origin != OriginClaim ||
		a.CoordinatorAgentID != "" || !a.OfferExpiresAt.IsZero() || a.Process != testPin || a.ReservationID == "" {
		t.Fatalf("claimed attempt = %+v", a)
	}
	if h := e.port.holdOf(a.Task); !h.active || h.workRef != a.attemptRef() {
		t.Fatalf("aimem hold = %+v, want it to name the attempt %s", h, a.attemptRef())
	}
	calls := e.port.callLog()
	if len(calls) != 1 || calls[0].Op != ReservationClaim {
		t.Fatalf("calls = %v", calls)
	}
	if txt := lastText(t, e.execTeam, e.lead); txt != "solo claimed task hub-a/project-a/task-1 and started work." {
		t.Fatalf("claim message = %q", txt)
	}

	if _, err := e.soloWork(t, "submit", a, IntentSubmit, "https://example.invalid/pull/1"); err != nil {
		t.Fatal(err)
	}
	// The claimer does not review its own result; the coordinator does.
	if _, err := e.review(t, "self-review", a, e.solo, 1, ReviewAccept); !errors.Is(err, ErrForbidden) {
		t.Fatalf("review by the claimer: got %v, want ErrForbidden", err)
	}
	if _, err := e.review(t, "accept", a, e.lead, 1, ReviewAccept); err != nil {
		t.Fatal(err)
	}
	got, err := e.finalize(t, "final", a, e.solo, 1, devDelivery("1"))
	if err != nil || got.State != AttemptClosed || got.CloseReason != "finalized" || busy(t, s, e.solo.agent.ID) {
		t.Fatalf("finalize = %+v, %v", got, err)
	}
	if c := e.port.taskContent(a.Task); c["state"] != "DONE" || c["notes"] != "Written by a person in aimem." {
		t.Fatalf("task content = %+v", c)
	}
}

// Only an independent member claims, from its current session, a task in
// the team's project set, with the pinned instructions and free capacity.
// A refused claim records nothing and calls nothing.
func TestClaimAuthority(t *testing.T) {
	ctx := context.Background()
	s, _ := openTemp(t)
	e := newClaimTeam(t, s)
	refused := func(what string, c Caller, req ClaimRequest, want error) {
		t.Helper()
		before := count(t, s, "attempts")
		if _, err := s.ClaimTask(ctx, c, e.port, "k-"+what, req); !errors.Is(err, want) {
			t.Errorf("%s: got %v, want %v", what, err, want)
		}
		if count(t, s, "attempts") != before || len(e.port.callLog()) != 0 {
			t.Errorf("%s: recorded or sent something: %d attempts, calls %v", what, count(t, s, "attempts"), e.port.callLog())
		}
	}
	refused("worker", e.builder.caller, e.claimReq(e.builder, "task-1"), ErrForbidden)
	refused("coordinator", e.lead.caller, e.claimReq(e.lead, "task-1"), ErrForbidden)
	stale := e.claimReq(e.solo, "task-1")
	stale.Generation++
	refused("stale generation", e.solo.caller, stale, ErrContextStale)
	other := e.claimReq(e.solo, "task-1")
	other.Task.ProjectID = "project-b"
	refused("project outside the set", e.solo.caller, other, ErrInvalid)
	digest := e.claimReq(e.solo, "task-1")
	digest.InstructionDigest = "sha256:other-instructions"
	refused("other instructions", e.solo.caller, digest, ErrProcessMismatch)
	noPin := e.claimReq(e.solo, "task-1")
	noPin.Process = TrustedProcess{}
	refused("no pin", e.solo.caller, noPin, ErrInvalid)
	noBranch := e.claimReq(e.solo, "task-1")
	noBranch.Branch = ""
	refused("no branch", e.solo.caller, noBranch, ErrInvalid)
	outsider := mustAgent(t, s, "outsider", "outsider")
	refused("not a member", agentCaller(t, outsider.ID), e.claimReq(e.solo, "task-1"), ErrForbidden)

	if _, err := e.claim(t, "c1", e.solo, "task-1"); err != nil {
		t.Fatal(err)
	}
	if _, err := e.claim(t, "c2", e.solo, "task-2"); !errors.Is(err, ErrAgentBusy) {
		t.Fatalf("a second claim: got %v, want ErrAgentBusy", err)
	}
	// The same key replays the first claim.
	if a, err := e.claim(t, "c1", e.solo, "task-1"); err != nil || a.State != AttemptRunning {
		t.Fatalf("replay of the claim = %+v, %v", a, err)
	}
}

// A lost reply leaves the claim reconciling with the capacity held, and the
// receipt for the same key runs it, even when the status read cannot see the
// hold. A call that was not committed, or a final refusal, closes the claim
// and frees the capacity. An unresolved receipt keeps it reconciling.
func TestClaimOutcomes(t *testing.T) {
	ctx := context.Background()
	t.Run("lost reply", func(t *testing.T) {
		s, _ := openTemp(t)
		e := newClaimTeam(t, s)
		e.port.faults[ReservationClaim] = faultLostReply
		a, err := e.claim(t, "c1", e.solo, "task-1")
		if !errors.Is(err, ErrOutcomeUnknown) || a.State != AttemptReconciling || !busy(t, s, e.solo.agent.ID) {
			t.Fatalf("claim with a lost reply = %+v, %v", a, err)
		}
		e.port.unresolved[a.PendingKey] = true
		if got, err := s.ReconcileAttempt(ctx, operator(t), blindStatus{e.port}, a.ID); !errors.Is(err, ErrOutcomeUnknown) || got.State != AttemptReconciling {
			t.Fatalf("unresolved receipt = %+v, %v", got, err)
		}
		delete(e.port.unresolved, a.PendingKey)
		got, err := s.ReconcileAttempt(ctx, operator(t), blindStatus{e.port}, a.ID)
		if err != nil || got.State != AttemptRunning || got.ReservationID == "" || !busy(t, s, e.solo.agent.ID) {
			t.Fatalf("reconciled claim = %+v, %v", got, err)
		}
		// A retry of the claim with its key reports the step, and sends nothing.
		n := len(e.port.callLog())
		if got, err := e.claim(t, "c1", e.solo, "task-1"); err != nil || got.State != AttemptRunning || len(e.port.callLog()) != n {
			t.Fatalf("retry after reconciliation = %+v, %v, calls %v", got, err, e.port.callLog())
		}
	})
	t.Run("not committed", func(t *testing.T) {
		s, _ := openTemp(t)
		e := newClaimTeam(t, s)
		e.port.faults[ReservationClaim] = faultBeforeCommit
		a, err := e.claim(t, "c1", e.solo, "task-1")
		if !errors.Is(err, ErrOutcomeUnknown) || !busy(t, s, e.solo.agent.ID) {
			t.Fatalf("claim that failed before commit = %+v, %v", a, err)
		}
		got, err := s.ReconcileAttempt(ctx, operator(t), e.port, a.ID)
		if err != nil || got.State != AttemptClosed || got.CloseReason != "claim not_committed" || busy(t, s, e.solo.agent.ID) {
			t.Fatalf("reconciled claim = %+v, %v; want closed, capacity freed", got, err)
		}
	})
	for _, tc := range []struct{ name, refusal, reason string }{
		{"competing hold", "competing_hold", "claim reservation_conflict"},
		{"dependency denied", "dependency_denied", "claim dependency_unresolved"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, _ := openTemp(t)
			e := newClaimTeam(t, s)
			e.port.refuse[ReservationClaim] = fixtureRefusal(t, tc.refusal)
			a, err := e.claim(t, "c1", e.solo, "task-1")
			if err == nil {
				t.Fatal("a refused claim succeeded")
			}
			got := mustState(t, s, a.ID, AttemptClosed)
			if got.CloseReason != tc.reason || busy(t, s, e.solo.agent.ID) {
				t.Fatalf("refused claim = %+v; want closed as %q with the capacity freed", got, tc.reason)
			}
		})
	}
	t.Run("held by an offer", func(t *testing.T) {
		s, _ := openTemp(t)
		e := newClaimTeam(t, s)
		offer := e.offer(t, "o1", "task-1")
		a, err := e.claim(t, "c1", e.solo, "task-1")
		if err == nil || mustState(t, s, a.ID, AttemptClosed).CloseReason != "claim reservation_conflict" {
			t.Fatalf("claim of an offered task = %+v, %v", a, err)
		}
		if h := e.port.holdOf(offer.Task); !h.active || h.workRef != offer.offerRef() {
			t.Fatalf("the offer's hold changed: %+v", h)
		}
		mustState(t, s, offer.ID, AttemptOffered)
	})
}

// A status read never closes a claimed attempt: with the hold invisible or
// released outside aicrew, it stays running with the capacity.
func TestStatusNeverClosesAClaim(t *testing.T) {
	ctx := context.Background()
	s, _ := openTemp(t)
	e := newClaimTeam(t, s)
	a, err := e.claim(t, "c1", e.solo, "task-1")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.ReconcileAttempt(ctx, operator(t), e.port, a.ID); err != nil {
		t.Fatalf("reconcile with the hold in place: %v", err)
	}
	if _, err := s.ReconcileAttempt(ctx, operator(t), blindStatus{e.port}, a.ID); !errors.Is(err, ErrOutcomeUnknown) {
		t.Fatalf("invisible hold: got %v, want ErrOutcomeUnknown", err)
	}
	e.port.recoveryRelease(a.Task)
	if _, err := s.ReconcileAttempt(ctx, operator(t), e.port, a.ID); !errors.Is(err, ErrOutcomeUnknown) {
		t.Fatalf("hold released outside aicrew: got %v, want ErrOutcomeUnknown", err)
	}
	if got := mustState(t, s, a.ID, AttemptRunning); !busy(t, s, e.solo.agent.ID) || got.Origin != OriginClaim {
		t.Fatalf("attempt = %+v; want running with the capacity", got)
	}
}

// One capacity across teams: a member who claimed in one team cannot be
// offered work in another, nor claim again; an offer and a claim racing for
// one agent leave exactly one open attempt.
func TestClaimCapacityAcrossTeams(t *testing.T) {
	ctx := context.Background()
	setup := func(t *testing.T) (claimTeam, execTeam) {
		s, _ := openTemp(t)
		e := newClaimTeam(t, s)
		t2 := mustTeam(t, s, "t2", "other", projectA)
		lead2 := joinCrew(t, s, t2.ID, "lead2", RoleCoordinator)
		if _, err := s.AddMember(ctx, operator(t), "member-solo-t2", t2.ID, e.solo.agent.ID, RoleWorker); err != nil {
			t.Fatal(err)
		}
		other := execTeam{s: s, tm: t2, lead: lead2, builder: e.solo, port: e.port}
		return e, other
	}
	t.Run("claim then offer", func(t *testing.T) {
		e, other := setup(t)
		if _, err := e.claim(t, "c1", e.solo, "task-1"); err != nil {
			t.Fatal(err)
		}
		if _, err := other.s.OfferTask(ctx, other.lead.caller, other.port, "o1", other.offerReq("task-2")); !errors.Is(err, ErrAgentBusy) {
			t.Fatalf("offer to a member with a claim: got %v, want ErrAgentBusy", err)
		}
	})
	t.Run("offer then claim", func(t *testing.T) {
		e, other := setup(t)
		if _, err := other.s.OfferTask(ctx, other.lead.caller, other.port, "o1", other.offerReq("task-2")); err != nil {
			t.Fatal(err)
		}
		if _, err := e.claim(t, "c1", e.solo, "task-1"); !errors.Is(err, ErrAgentBusy) {
			t.Fatalf("claim with an open offer: got %v, want ErrAgentBusy", err)
		}
	})
	t.Run("race", func(t *testing.T) {
		e, other := setup(t)
		errs := make([]error, 2)
		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			_, errs[0] = e.claim(t, "c1", e.solo, "task-1")
		}()
		go func() {
			defer wg.Done()
			_, errs[1] = other.s.OfferTask(ctx, other.lead.caller, other.port, "o1", other.offerReq("task-2"))
		}()
		wg.Wait()
		wins := 0
		for _, err := range errs {
			switch {
			case err == nil:
				wins++
			case !errors.Is(err, ErrAgentBusy):
				t.Errorf("unexpected error %v", err)
			}
		}
		var open int
		if err := e.s.db.QueryRow(`SELECT COUNT(*) FROM attempts WHERE worker_agent_id = ? AND state != 'closed'`,
			e.solo.agent.ID).Scan(&open); err != nil {
			t.Fatal(err)
		}
		if wins != 1 || open != 1 {
			t.Fatalf("wins = %d, open attempts = %d, errors %v; want exactly one", wins, open, errs)
		}
	})
}

// The claimer cannot leave while its claim is open, reconciling or running.
func TestClaimBlocksLeave(t *testing.T) {
	ctx := context.Background()
	s, _ := openTemp(t)
	e := newClaimTeam(t, s)
	leave := func(key string) error {
		_, err := s.LeaveSession(ctx, e.solo.caller, key, e.solo.sess.ID, e.solo.sess.Generation)
		return err
	}
	e.port.faults[ReservationClaim] = faultLostReply
	a, _ := e.claim(t, "c1", e.solo, "task-1")
	if err := leave("leave-1"); !errors.Is(err, ErrWorkOutstanding) {
		t.Fatalf("leave while the claim reconciles: got %v, want ErrWorkOutstanding", err)
	}
	if _, err := s.ReconcileAttempt(ctx, operator(t), e.port, a.ID); err != nil {
		t.Fatal(err)
	}
	if err := leave("leave-2"); !errors.Is(err, ErrWorkOutstanding) {
		t.Fatalf("leave with a running claim: got %v, want ErrWorkOutstanding", err)
	}
}

// A claimed attempt stops by a2b's rules: the coordinator requests, the
// claimer confirms and releases; the claimer cannot request its own stop.
func TestClaimStops(t *testing.T) {
	ctx := context.Background()
	s, _ := openTemp(t)
	e := newClaimTeam(t, s)
	a, err := e.claim(t, "c1", e.solo, "task-1")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.requestStop(t, "self-stop", a, e.solo, "done for today"); !errors.Is(err, ErrForbidden) {
		t.Fatalf("stop requested by the claimer: got %v, want ErrForbidden", err)
	}
	if _, err := e.requestStop(t, "stop", a, e.lead, "priorities changed"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ConfirmStop(ctx, e.solo.caller, "confirm", a.ID, e.solo.sess.ID, e.solo.sess.Generation); err != nil {
		t.Fatal(err)
	}
	got, err := s.ReleaseStopped(ctx, e.solo.caller, e.port, "release", a.ID, StopRelease{
		SessionID: e.solo.sess.ID, Generation: e.solo.sess.Generation, Target: ReleaseReady})
	if err != nil || got.State != AttemptClosed || got.CloseReason != "stopped" || busy(t, s, e.solo.agent.ID) {
		t.Fatalf("release = %+v, %v", got, err)
	}
}

// The claim request sends only fields the fixture's independent-worker
// claim shows, with the external holder naming the attempt.
func TestClaimRequestShape(t *testing.T) {
	fx := loadFixture(t)
	var example map[string]json.RawMessage
	for _, m := range fx.Mutations {
		if m.Operation == "claim" && m.ActorCase == "independent_worker_external_attempt" {
			if err := json.Unmarshal(m.Request, &example); err != nil {
				t.Fatal(err)
			}
		}
	}
	if example == nil {
		t.Fatal("the fixture has no independent-worker claim")
	}
	a := Attempt{ID: "x", Origin: OriginClaim, PendingOp: ReservationClaim, PendingKey: "k", TaskRevision: 3}
	req := reservationRequest(a)
	if req.Holder == nil || req.Holder.Mode != "external" || req.Holder.WorkRef != "aicrew-attempt-x" {
		t.Fatalf("holder = %+v, want external naming the attempt", req.Holder)
	}
	body, _ := json.Marshal(req)
	var sent map[string]json.RawMessage
	if err := json.Unmarshal(body, &sent); err != nil {
		t.Fatal(err)
	}
	for field := range sent {
		if _, ok := example[field]; !ok {
			t.Errorf("claim request field %q is not in the fixture's independent-worker claim", field)
		}
	}
	if len(sent) != len(example) {
		t.Errorf("claim request fields %v, fixture %v", keys(sent), keys(example))
	}
}

func keys(m map[string]json.RawMessage) []string {
	out := []string{}
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// The schema records a coordinator exactly for an offer, and knows only the
// two origins.
func TestAttemptOriginIsChecked(t *testing.T) {
	s, _ := openTemp(t)
	e := newClaimTeam(t, s)
	claimed, err := e.claim(t, "c1", e.solo, "task-1")
	if err != nil {
		t.Fatal(err)
	}
	offered := e.offer(t, "o1", "task-2")
	for _, bad := range []struct{ what, stmt, id string }{
		{"a claim with a coordinator", `UPDATE attempts SET coordinator_agent_id = worker_agent_id WHERE id = ?`, claimed.ID},
		{"an offer without one", `UPDATE attempts SET coordinator_agent_id = NULL WHERE id = ?`, offered.ID},
		{"an unknown origin", `UPDATE attempts SET origin = 'adopted' WHERE id = ?`, claimed.ID},
	} {
		if _, err := s.db.Exec(bad.stmt, bad.id); err == nil {
			t.Errorf("the schema accepted %s", bad.what)
		}
	}
}

// A committed reply that does not name this claim's hold is not trusted:
// the claim reconciles with the capacity held, and the recorded receipt then
// runs it.
func TestInconsistentClaimReplyIsNotTrusted(t *testing.T) {
	ctx := context.Background()
	for name, garble := range map[string]func(*ReservationResult){
		"the offer's work ref": func(r *ReservationResult) {
			r.Reservation.OwnWorkRef = strings.Replace(r.Reservation.OwnWorkRef, "attempt", "offer", 1)
		},
		"another work ref": func(r *ReservationResult) { r.Reservation.OwnWorkRef = "aicrew-attempt-other" },
		"inactive hold":    func(r *ReservationResult) { r.Reservation.Active = false },
	} {
		t.Run(name, func(t *testing.T) {
			s, _ := openTemp(t)
			e := newClaimTeam(t, s)
			e.port.garble = garble
			a, err := e.claim(t, "c1", e.solo, "task-1")
			if !errors.Is(err, ErrOutcomeUnknown) || a.State != AttemptReconciling || !busy(t, s, e.solo.agent.ID) {
				t.Fatalf("inconsistent reply = %+v, %v; want reconciling with the capacity held", a, err)
			}
			if got, err := s.ReconcileAttempt(ctx, e.solo.caller, e.port, a.ID); err != nil || got.State != AttemptRunning {
				t.Fatalf("reconcile from the recorded receipt = %+v, %v", got, err)
			}
		})
	}
}

// A member who becomes the team's coordinator still never reviews its own
// result nor requests its own stop, whether its attempt began as a claim or
// as an offer; the operator may still stop it.
func TestPromotedWorkerCannotReviewOrStopItself(t *testing.T) {
	ctx := context.Background()
	for _, origin := range []AttemptOrigin{OriginClaim, OriginOffer} {
		t.Run(string(origin), func(t *testing.T) {
			s, _ := openTemp(t)
			e := newClaimTeam(t, s)
			worker := e.solo
			var a Attempt
			var err error
			if origin == OriginClaim {
				a, err = e.claim(t, "c1", worker, "task-1")
			} else {
				worker = e.builder
				a, err = s.AcceptOffer(ctx, worker.caller, e.port, "a1", e.offer(t, "o1", "task-1").ID, e.acceptReq())
			}
			if err != nil {
				t.Fatal(err)
			}
			if _, err := s.UpdateWork(ctx, worker.caller, e.port, "submit", a.ID, WorkUpdate{
				SessionID: worker.sess.ID, Generation: worker.sess.Generation,
				Intent: IntentSubmit, Detail: "https://example.invalid/pull/1"}); err != nil {
				t.Fatal(err)
			}
			// The coordinator leaves; the operator makes the worker coordinator
			// and it starts a coordinator session.
			if _, err := s.StopSession(ctx, operator(t), "end-lead", e.lead.sess.ID); err != nil {
				t.Fatal(err)
			}
			ms, err := getMembership(ctx, s.rdb, e.tm.ID, worker.agent.ID)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := s.SetMemberRole(ctx, operator(t), "promote", e.tm.ID, worker.agent.ID, ms.Revision, RoleCoordinator); err != nil {
				t.Fatal(err)
			}
			promoted := worker
			if promoted.sess, err = s.StartSession(ctx, worker.caller, "start-as-lead", e.tm.ID); err != nil || promoted.sess.Role != RoleCoordinator {
				t.Fatalf("coordinator session = %+v, %v", promoted.sess, err)
			}
			if _, err := e.review(t, "self-review", a, promoted, 1, ReviewAccept); !errors.Is(err, ErrForbidden) {
				t.Fatalf("review of its own result as coordinator: got %v, want ErrForbidden", err)
			}
			if _, err := e.requestStop(t, "self-stop", a, promoted, "done"); !errors.Is(err, ErrForbidden) {
				t.Fatalf("stop of its own attempt as coordinator: got %v, want ErrForbidden", err)
			}
			if got := mustState(t, s, a.ID, AttemptRunning); got.Phase != PhaseSubmitted || got.AcceptedResult != 0 || got.Stop != StopNone {
				t.Fatalf("attempt = %+v; want it unchanged", got)
			}
			if _, err := s.RequestStop(ctx, operator(t), "op-stop", a.ID, StopRequest{Reason: "reassigning"}); err != nil {
				t.Fatalf("operator stop: %v", err)
			}
		})
	}
}
