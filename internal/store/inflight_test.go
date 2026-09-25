package store

import (
	"context"
	"errors"
	"reflect"
	"sync"
	"testing"
	"time"
)

// waiting reports how many commands wait on in-flight entries.
func (f *inflight) waiting() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, e := range f.entries {
		n += e.waiters
	}
	return n
}

// waitForWaiters blocks until n commands wait on the store's in-flight
// entries, failing the test if that does not happen.
func waitForWaiters(t *testing.T, s *Store, n int) bool {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for s.flights.waiting() != n {
		if time.Now().After(deadline) {
			t.Errorf("waiting commands = %d, want %d", s.flights.waiting(), n)
			return false
		}
		time.Sleep(time.Millisecond)
	}
	return true
}

type outcome[T any] struct {
	res T
	err error
}

// overlap forces the interleaving that made an overlapping retry refuse
// (CI run 36153163175). The original is held inside verification, its
// receipt lookup and pre-checks done and its commit not. The retry starts
// and is paused at the furthest point it can reach: just after its own
// receipt lookup, or waiting for the original. The original then commits,
// and only after that does the retry go on. Without the in-flight entry the
// retry's lookup precedes the commit and its pre-check follows it.
func overlap[T any](t *testing.T, s *Store, held chan struct{}, original <-chan outcome[T],
	retry func() <-chan outcome[T]) (outcome[T], outcome[T]) {
	t.Helper()
	var once sync.Once
	looked, resume := make(chan struct{}), make(chan struct{})
	s.afterReceiptLookup = func(string) {
		once.Do(func() { close(looked) })
		<-resume
	}
	second := retry()
	deadline := time.Now().Add(5 * time.Second)
	for paused := false; !paused; {
		select {
		case <-looked:
			paused = true
		default:
			paused = s.flights.waiting() == 1
		}
		if !paused && time.Now().After(deadline) {
			t.Fatal("the retry neither waited nor reached its receipt lookup")
		}
		time.Sleep(time.Millisecond)
	}
	close(held)
	first := <-original
	close(resume)
	return first, <-second
}

// holdInVerification makes the verifier's next call signal and then wait
// until the returned channel is closed.
func holdInVerification(v *fakeVerifier) (verifying <-chan struct{}, held chan struct{}) {
	in, hold := make(chan struct{}), make(chan struct{})
	v.onRedeem = func() {
		v.onRedeem = nil
		close(in)
		<-hold
	}
	return in, hold
}

type redemption struct {
	s    *Store
	v    *fakeVerifier
	code Secret
	ch   Challenge
	rc   Secret
}

func newRedemption(t *testing.T) redemption {
	t.Helper()
	s, _ := openTemp(t)
	v := newFakeVerifier()
	tm := mustTeam(t, s, "t1", "crew")
	code := newCode(t)
	issueJoin(t, s, "k1", tm.ID, code)
	ch, err := s.BeginRedemption(context.Background(), "b1", code)
	if err != nil {
		t.Fatal(err)
	}
	return redemption{s: s, v: v, code: code, ch: ch, rc: v.receipt("rc-1", "hub-a", "user-1", "tok-1")}
}

func (r redemption) complete(ctx context.Context, key, challengeID string) <-chan outcome[LinkResult] {
	done := make(chan outcome[LinkResult], 1)
	go func() {
		res, err := r.s.CompleteRedemption(ctx, r.v, key, r.code, challengeID, r.rc)
		done <- outcome[LinkResult]{res, err}
	}()
	return done
}

func TestOverlappingRedemptionRetryReplays(t *testing.T) {
	r := newRedemption(t)
	ctx := context.Background()
	verifying, held := holdInVerification(r.v)
	original := r.complete(ctx, "c1", r.ch.ID)
	<-verifying
	first, retry := overlap(t, r.s, held, original, func() <-chan outcome[LinkResult] {
		return r.complete(ctx, "c1", r.ch.ID)
	})
	if first.err != nil {
		t.Fatal(first.err)
	}
	if retry.err != nil || !reflect.DeepEqual(retry.res, first.res) {
		t.Fatalf("overlapping retry = %+v, %v; want %+v", retry.res, retry.err, first.res)
	}
	if r.v.calls != 1 {
		t.Errorf("verifier calls = %d, want 1: the retry must answer from the receipt", r.v.calls)
	}
	if count(t, r.s, "agents") != 1 || count(t, r.s, "memberships") != 1 {
		t.Errorf("agents = %d, memberships = %d, want one effect", count(t, r.s, "agents"), count(t, r.s, "memberships"))
	}
}

func TestOverlappingProofRetryReplays(t *testing.T) {
	s, _ := openTemp(t)
	ctx := context.Background()
	v := newFakeVerifier()
	tm := mustTeam(t, s, "t1", "crew")
	res := joinAs(t, s, v, "inv-1", tm.ID, RoleWorker, "builder", "user-1", "tok-1")
	ch, err := s.IssueAgentChallenge(ctx, Caller{}, "i1", res.AgentID)
	if err != nil {
		t.Fatal(err)
	}
	// A new credential for the same user: the proof rotates the link, an
	// effect that must happen once.
	rc := v.receipt("rc-2", "hub-a", "user-1", "tok-2")
	calls := v.calls
	prove := func() <-chan outcome[ProofResult] {
		done := make(chan outcome[ProofResult], 1)
		go func() {
			res, err := s.CompleteAgentProof(ctx, Caller{}, v, "p1", ch.ID, rc)
			done <- outcome[ProofResult]{res, err}
		}()
		return done
	}
	verifying, held := holdInVerification(v)
	original := prove()
	<-verifying
	first, retry := overlap(t, s, held, original, prove)
	if first.err != nil || !first.res.Rotated {
		t.Fatalf("proof = %+v, %v; want a rotation", first.res, first.err)
	}
	if retry.err != nil || retry.res != first.res {
		t.Fatalf("overlapping retry = %+v, %v; want %+v", retry.res, retry.err, first.res)
	}
	if v.calls != calls+1 {
		t.Errorf("verifier calls = %d, want %d", v.calls, calls+1)
	}
	var proofs int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM audit WHERE operation = ?`, opCompleteAgentProof).Scan(&proofs); err != nil {
		t.Fatal(err)
	}
	if proofs != 1 {
		t.Errorf("proof audit records = %d, want 1", proofs)
	}
}

// If the original fails, the waiting retry is evaluated on its own: it asks
// aimem itself and gets its own answer, and a failed verification still
// creates nothing. A later retry with the same key then succeeds.
func TestRetryAfterFailedOriginalIsEvaluatedAgain(t *testing.T) {
	r := newRedemption(t)
	ctx := context.Background()
	var retry <-chan outcome[LinkResult]
	r.v.onRedeem = func() {
		r.v.onRedeem = nil
		retry = r.complete(ctx, "c1", r.ch.ID)
		waitForWaiters(t, r.s, 1)
		r.v.mu.Lock()
		r.v.fail = errAimemUnavailable
		r.v.mu.Unlock()
	}
	if _, err := r.s.CompleteRedemption(ctx, r.v, "c1", r.code, r.ch.ID, r.rc); !errors.Is(err, errAimemUnavailable) {
		t.Fatalf("original = %v, want aimem unavailable", err)
	}
	if got := <-retry; !errors.Is(got.err, errAimemUnavailable) {
		t.Fatalf("waiting retry = %+v, %v; want its own aimem unavailable", got.res, got.err)
	}
	if r.v.calls != 2 {
		t.Errorf("verifier calls = %d, want 2: the retry must verify for itself", r.v.calls)
	}
	if count(t, r.s, "agents") != 0 || count(t, r.s, "memberships") != 0 || count(t, r.s, "receipts WHERE operation = 'redemption.complete'") != 0 {
		t.Fatal("failed verification created state")
	}
	r.v.mu.Lock()
	r.v.fail = nil
	r.v.mu.Unlock()
	if _, err := r.s.CompleteRedemption(ctx, r.v, "c1", r.code, r.ch.ID, r.rc); err != nil {
		t.Fatalf("retry after the outage: %v", err)
	}
}

func TestOverlappingConflictingReuseIsRefused(t *testing.T) {
	r := newRedemption(t)
	ctx := context.Background()
	var reuse <-chan outcome[LinkResult]
	r.v.onRedeem = func() {
		r.v.onRedeem = nil
		reuse = r.complete(ctx, "c1", "another-challenge")
		waitForWaiters(t, r.s, 1)
	}
	if _, err := r.s.CompleteRedemption(ctx, r.v, "c1", r.code, r.ch.ID, r.rc); err != nil {
		t.Fatal(err)
	}
	if got := <-reuse; !errors.Is(got.err, ErrIdempotencyConflict) {
		t.Fatalf("conflicting reuse = %+v, %v; want ErrIdempotencyConflict", got.res, got.err)
	}
}

// A waiting retry stops when its context ends or the wait bound passes,
// changing nothing; the original is unaffected.
func TestWaitingRetryIsBounded(t *testing.T) {
	for _, tc := range []struct {
		name   string
		bound  time.Duration
		cancel bool
		want   error
	}{
		{"cancelled", time.Hour, true, context.Canceled},
		{"bound", 20 * time.Millisecond, false, ErrInProgress},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newRedemption(t)
			r.s.flightWait = tc.bound
			ctx := context.Background()
			waitCtx, cancel := context.WithCancel(ctx)
			defer cancel()
			var stopped outcome[LinkResult]
			r.v.onRedeem = func() {
				r.v.onRedeem = nil
				retry := r.complete(waitCtx, "c1", r.ch.ID)
				if tc.cancel {
					waitForWaiters(t, r.s, 1)
					cancel()
				}
				// The original stays in verification until the retry gave up.
				stopped = <-retry
			}
			first, err := r.s.CompleteRedemption(ctx, r.v, "c1", r.code, r.ch.ID, r.rc)
			if err != nil || first.AgentID == "" {
				t.Fatalf("original = %+v, %v", first, err)
			}
			if !errors.Is(stopped.err, tc.want) {
				t.Fatalf("waiting retry = %+v, %v; want %v", stopped.res, stopped.err, tc.want)
			}
			if r.v.calls != 1 || count(t, r.s, "agents") != 1 {
				t.Errorf("verifier calls = %d, agents = %d; want 1, 1", r.v.calls, count(t, r.s, "agents"))
			}
		})
	}
}

// Different keys do not wait: a second key completes while the first is
// still in verification, and the first then loses to it.
func TestDifferentKeysDoNotWait(t *testing.T) {
	r := newRedemption(t)
	ctx := context.Background()
	var other outcome[LinkResult]
	r.v.onRedeem = func() {
		r.v.onRedeem = nil
		other = <-r.complete(ctx, "c2", r.ch.ID)
	}
	if _, err := r.s.CompleteRedemption(ctx, r.v, "c1", r.code, r.ch.ID, r.rc); !errors.Is(err, ErrInvitationInvalid) {
		t.Fatalf("first key = %v, want ErrInvitationInvalid after the second key won", err)
	}
	if other.err != nil {
		t.Fatalf("second key = %v", other.err)
	}
	if count(t, r.s, "agents") != 1 {
		t.Errorf("agents = %d, want 1", count(t, r.s, "agents"))
	}
}

func TestInflightKeysOnCallerAndCommand(t *testing.T) {
	var f inflight
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	op, err := OperatorCaller("op-1")
	if err != nil {
		t.Fatal(err)
	}
	cmd := command{op: "x", scope: "s", key: "k"}
	release, err := f.acquire(ctx, Caller{}, cmd, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	for _, other := range []struct {
		c   Caller
		cmd command
	}{
		{op, cmd},
		{Caller{}, command{op: "y", scope: "s", key: "k"}},
		{Caller{}, command{op: "x", scope: "t", key: "k"}},
		{Caller{}, command{op: "x", scope: "s", key: "l"}},
	} {
		rel, err := f.acquire(ctx, other.c, other.cmd, time.Hour)
		if err != nil {
			t.Fatalf("%+v waited on another entry: %v", other.cmd, err)
		}
		rel()
	}
	if _, err := f.acquire(ctx, Caller{}, cmd, time.Hour); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("identical command = %v, want to wait until its context ends", err)
	}
	release()
	rel, err := f.acquire(context.Background(), Caller{}, cmd, time.Hour)
	if err != nil {
		t.Fatalf("after release: %v", err)
	}
	rel()
}
