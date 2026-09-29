package store

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"
)

// updateReceipt is the read scope's receipt for an update committed under key
// on a's hold, moving the task's revision.
func updateReceipt(a Attempt, key string) ScopeReceipt {
	return ScopeReceipt{ID: "rcpt-" + key, Operation: string(ReservationUpdate), TaskID: a.Task.TaskID,
		RequestKeyDigest: requestKeyDigest(key), ReservationID: a.ReservationID, Fence: a.Fence,
		TaskRevision: a.TaskRevision + 1, MemberUserID: "user-x", VerifiedMode: "team", CommittedAt: "2026-09-29T04:00:00Z"}
}

// heldAs is the hold status of a's hold at fence and revision.
func heldAs(a Attempt, fence string, revision int64) ScopeHold {
	return ScopeHold{State: ScopeHeld, ReservationID: a.ReservationID, Fence: fence, HolderMode: "external",
		OwnWorkRef: a.attemptRef(), TaskRevision: revision}
}

func (e claimStopEnv) beginWork(t *testing.T, a Attempt, key string, intent WorkIntent, detail string) (Attempt, Step) {
	t.Helper()
	a, st, err := e.s.BeginWorkWithToken(context.Background(), key, e.indepTk, a.ID, intent, detail)
	if err != nil {
		t.Fatalf("begin %s: %v", intent, err)
	}
	return a, st
}

func (e claimStopEnv) settleKey(t *testing.T, a Attempt, key string, hint StepHint) (Attempt, Settlement, error) {
	t.Helper()
	return e.s.SettleWithToken(context.Background(), e.indepTk, e.reader, a.ID, key, report(hint))
}

// A work update is a proofless step: its begin returns the values the member
// sends aimem and no proof, a retried begin answers the same step, and settle
// applies it from the read scope's receipt by key.
func TestWorkUpdateSettlesFromTheReadScope(t *testing.T) {
	ctx := context.Background()
	e := newClaimStopEnv(t)
	a, _ := e.runningClaim(t, "task-1")
	e.reader.setHold(heldAs(a, a.Fence, a.TaskRevision))
	a, st := e.beginWork(t, a, "submit-1", IntentSubmit, "https://forge.example/pull/7")
	if st.Operation != ReservationUpdate || st.CoordinationProof != "" || st.Intent != "submit" || st.TargetState != "REVIEW" ||
		st.ResultRef != "https://forge.example/pull/7" || st.ReservationID != a.ReservationID || st.Fence != a.Fence ||
		st.ExpectedRevision != a.TaskRevision || st.Holder != nil {
		t.Fatalf("begin submit: %+v", st)
	}
	if _, again := e.beginWork(t, a, "submit-1", IntentSubmit, "https://forge.example/pull/7"); !reflect.DeepEqual(again, st) {
		t.Fatalf("a retried begin: %+v, want %+v", again, st)
	}
	if got, set, err := e.settleKey(t, a, st.RequestKey, HintCommitted); err != nil || set.Settled || set.RetryAfter != NoneFinalAfter ||
		got.PendingKey != st.RequestKey {
		t.Fatalf("a committed report without a receipt: %+v %+v %v", got, set, err)
	}
	e.reader.commitUpdate(st.RequestKey, updateReceipt(a, st.RequestKey))
	a, set, err := e.settleKey(t, a, st.RequestKey, HintUnknown)
	if err != nil || !set.Settled || set.Outcome != "committed" || a.Phase != PhaseSubmitted || a.TaskRevision != 5 {
		t.Fatalf("settle submit: %+v %+v %v", a, set, err)
	}
	if res, err := e.s.AttemptResults(ctx, a.ID); err != nil || len(res) != 1 || res[0].ResultRef != "https://forge.example/pull/7" {
		t.Fatalf("results: %+v %v", res, err)
	}
	if _, _, err := e.s.BeginWorkWithToken(ctx, "block-l", e.leadTok, a.ID, IntentBlock, "x"); !errors.Is(err, ErrForbidden) && !errors.Is(err, ErrNotFound) {
		t.Fatalf("an update by another member: %v", err)
	}
	if got, set, err := e.s.SettleWithToken(ctx, e.indepTk, nil, a.ID, st.RequestKey, report(HintCommitted)); err != nil || !set.Settled || got.Phase != PhaseSubmitted {
		t.Fatalf("settling a settled update again: %+v %+v %v", got, set, err)
	}
}

// The proofless finality rule, case by case as coordination.v1's
// delayed_update table lists them. A "none" stays unresolved until the hold's
// fence or revision is observed past the request's, with the receipt read
// after that observation; no report is trusted, and nothing is voided.
func TestProoflessUpdateFinality(t *testing.T) {
	e := newClaimStopEnv(t)
	a, _ := e.runningClaim(t, "task-1")
	fence, revision := a.Fence, a.TaskRevision
	n := 0
	block := func() (Attempt, Step) {
		t.Helper()
		n++
		e.reader.setHold(heldAs(a, fence, revision))
		return e.beginWork(t, a, "block-"+string(rune('a'+n)), IntentBlock, "waiting")
	}
	pendingAfter := func(what string, a Attempt, st Step, hint StepHint) {
		t.Helper()
		if got, set, err := e.settleKey(t, a, st.RequestKey, hint); err != nil || set.Settled || got.PendingKey != st.RequestKey {
			t.Fatalf("%s: %+v %+v %v", what, got, set, err)
		}
	}

	// none_long_after_abandoning and evidence_not_past_the_request: no
	// evidence, however long, and whatever the member reports.
	a, st := block()
	pendingAfter("none, no evidence", a, st, HintRefused)
	*e.now = e.now.Add(time.Hour)
	pendingAfter("none, an hour later", a, st, HintUnknown)
	e.reader.setHold(ScopeHold{State: ScopeHeld, ReservationID: "another", Fence: "9", TaskRevision: 99})
	pendingAfter("another reservation moved", a, st, HintUnknown)

	// fence_moved_then_none.
	e.reader.setHold(heldAs(a, "2", revision))
	if a, set, err := e.settleKey(t, a, st.RequestKey, HintUnknown); err != nil || !set.Settled || set.Outcome != "not_committed" ||
		a.Phase != PhaseWorking || a.PendingKey != "" {
		t.Fatalf("fence moved, then none: %+v %+v %v", a, set, err)
	}

	// revision_moved_then_none.
	a, st = block()
	e.reader.setHold(heldAs(a, fence, revision+1))
	if a, set, err := e.settleKey(t, a, st.RequestKey, HintUnknown); err != nil || !set.Settled || set.Outcome != "not_committed" {
		t.Fatalf("revision moved, then none: %+v %+v %v", a, set, err)
	}

	// A hold closed past the request's fence is evidence too.
	a, st = block()
	e.reader.setHold(ScopeHold{State: ScopeClosed, ReservationID: a.ReservationID, ClosingFence: "2"})
	if a, set, err := e.settleKey(t, a, st.RequestKey, HintUnknown); err != nil || !set.Settled || set.Outcome != "not_committed" {
		t.Fatalf("closed past the fence, then none: %+v %+v %v", a, set, err)
	}

	// none_read_before_the_evidence: the update commits while its "none" is
	// in flight, moving the revision. The none was read before the
	// evidence, so the step stays unresolved; the next settle finds the
	// commit (update_committed_late).
	a, st = block()
	e.reader.answered = func() {
		e.reader.commitUpdate(st.RequestKey, updateReceipt(a, st.RequestKey))
		e.reader.setHold(heldAs(a, fence, revision+1))
	}
	pendingAfter("none read before the evidence", a, st, HintUnknown)
	if a, set, err := e.settleKey(t, a, st.RequestKey, HintUnknown); err != nil || !set.Settled || set.Outcome != "committed" ||
		a.Phase != PhaseBlocked {
		t.Fatalf("the late commit: %+v %+v %v", a, set, err)
	}
}

// An update aimem refused without moving anything is superseded by a new key
// for the same update. Every key is an alias of one step: a commit under any
// of them settles it, and each keeps the step's outcome.
func TestSupersedeStuckUpdate(t *testing.T) {
	ctx := context.Background()
	e := newClaimStopEnv(t)
	a, _ := e.runningClaim(t, "task-1")
	e.reader.setHold(heldAs(a, a.Fence, a.TaskRevision))
	a, first := e.beginWork(t, a, "block-1", IntentBlock, "waiting on design")
	if _, set, err := e.settleKey(t, a, first.RequestKey, HintRefused); err != nil || set.Settled {
		t.Fatalf("a refused update: %+v %v", set, err)
	}
	supersede := func(key, supersedes string, intent WorkIntent, detail string) (Attempt, Step, error) {
		return e.s.SupersedeWorkWithToken(ctx, key, e.indepTk, a.ID, supersedes, intent, detail)
	}
	if _, _, err := supersede("s-0", "no-such-key", IntentBlock, "waiting on design"); !errors.Is(err, ErrAttemptState) {
		t.Fatalf("superseding another key: %v", err)
	}
	if _, _, err := supersede("s-0", first.RequestKey, IntentBlock, "another blocker"); !errors.Is(err, ErrInvalid) {
		t.Fatalf("superseding with another detail: %v", err)
	}
	a, second, err := supersede("s-1", first.RequestKey, IntentBlock, "waiting on design")
	if err != nil || second.RequestKey == first.RequestKey || second.ExpectedRevision != first.ExpectedRevision ||
		second.Fence != first.Fence || second.Intent != "block" || second.Blocker != "waiting on design" {
		t.Fatalf("supersede: %+v %v", second, err)
	}
	if _, again, err := supersede("s-1", first.RequestKey, IntentBlock, "waiting on design"); err != nil || !reflect.DeepEqual(again, second) {
		t.Fatalf("a retried supersede: %+v %v", again, err)
	}
	// The old key's send, delayed, is the one that commits; settling under
	// the new key finds it.
	e.reader.commitUpdate(first.RequestKey, updateReceipt(a, first.RequestKey))
	a, set, err := e.settleKey(t, a, second.RequestKey, HintUnknown)
	if err != nil || !set.Settled || set.Outcome != "committed" || a.Phase != PhaseBlocked {
		t.Fatalf("settle under the new key: %+v %+v %v", a, set, err)
	}
	for _, k := range []string{first.RequestKey, second.RequestKey} {
		if _, set, err := e.settleKey(t, a, k, HintUnknown); err != nil || !set.Settled || set.Outcome != "committed" {
			t.Fatalf("settling key %s again: %+v %v", k, set, err)
		}
	}
	if keys, err := e.s.stepKeys(ctx, Attempt{ID: a.ID}); err != nil || len(keys) != 1 {
		t.Fatalf("superseded keys left after the step settled: %v %v", keys, err)
	}

	// A superseded key settles the pending step too.
	a, third := e.beginWork(t, a, "resume-1", IntentResume, "")
	a, fourth, err := supersede("s-2", third.RequestKey, IntentResume, "")
	if err != nil {
		t.Fatal(err)
	}
	e.reader.commitUpdate(fourth.RequestKey, updateReceipt(a, fourth.RequestKey))
	if a, set, err = e.settleKey(t, a, third.RequestKey, HintUnknown); err != nil || !set.Settled || a.Phase != PhaseWorking {
		t.Fatalf("settle under the superseded key: %+v %+v %v", a, set, err)
	}
}

// Without a read scope, no update settles.
func TestWorkUpdateWithoutReaderStaysPending(t *testing.T) {
	e := newClaimStopEnv(t)
	a, _ := e.runningClaim(t, "task-1")
	a, st := e.beginWork(t, a, "submit-1", IntentSubmit, "https://forge.example/pull/7")
	e.reader.commitUpdate(st.RequestKey, updateReceipt(a, st.RequestKey))
	if got, set, err := e.s.SettleWithToken(context.Background(), e.indepTk, nil, a.ID, st.RequestKey, report(HintCommitted)); err != nil ||
		set.Settled || got.Phase != PhaseWorking {
		t.Fatalf("settle without a reader: %+v %+v %v", got, set, err)
	}
}

// Every write of a work update checks the token inside its own transaction:
// the begin, the supersede, and the settle's transition, committed or not.
func TestWorkWritesRecheckTheToken(t *testing.T) {
	ctx := context.Background()
	e := newClaimStopEnv(t)
	a, _ := e.runningClaim(t, "task-1")
	e.reader.setHold(heldAs(a, a.Fence, a.TaskRevision))
	alive := *e.now
	die := func() { *e.now = e.expires }
	dead := func(what string, err error) {
		t.Helper()
		*e.now = alive
		e.s.afterTokenLookup, e.reader.answered = nil, nil
		if !errors.Is(err, ErrTokenInvalid) {
			t.Fatalf("%s with a dead token: %v", what, err)
		}
	}
	current := func() Attempt {
		t.Helper()
		got, err := e.s.GetAttempt(ctx, a.ID)
		if err != nil {
			t.Fatal(err)
		}
		return got
	}

	e.s.afterTokenLookup = die
	_, _, err := e.s.BeginWorkWithToken(ctx, "block-1", e.indepTk, a.ID, IntentBlock, "waiting")
	dead("an update", err)
	if current().PendingKey != "" {
		t.Fatal("a dead token began an update")
	}
	a, st := e.beginWork(t, a, "block-1", IntentBlock, "waiting")
	e.s.afterTokenLookup = die
	_, _, err = e.s.SupersedeWorkWithToken(ctx, "s-1", e.indepTk, a.ID, st.RequestKey, IntentBlock, "waiting")
	dead("a supersede", err)
	if current().PendingKey != st.RequestKey {
		t.Fatal("a dead token superseded the update")
	}
	e.reader.setHold(heldAs(a, "2", a.TaskRevision))
	e.reader.answered = die
	_, _, err = e.settleKey(t, a, st.RequestKey, HintUnknown)
	dead("an update's settle as not committed", err)
	if current().PendingKey != st.RequestKey {
		t.Fatal("a dead token settled the update as not committed")
	}
	e.reader.setHold(heldAs(a, a.Fence, a.TaskRevision))
	e.reader.commitUpdate(st.RequestKey, updateReceipt(a, st.RequestKey))
	e.reader.answered = die
	_, _, err = e.settleKey(t, a, st.RequestKey, HintUnknown)
	dead("an update's settle", err)
	if got := current(); got.PendingKey != st.RequestKey || got.Phase != PhaseWorking {
		t.Fatalf("a dead token settled the update: %+v", got)
	}
}
