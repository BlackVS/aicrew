package reconcile

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/BlackVS/aicrew/internal/aimemread"
	"github.com/BlackVS/aicrew/internal/store"
)

// fakeStore settles a pending step by one proof read, and records closures.
type fakeStore struct {
	mu      sync.Mutex
	cands   []store.ReconcileCandidate
	closed  map[string]store.ScopeHold
	settled map[string]bool
}

func (f *fakeStore) ReconcileCandidates(context.Context) ([]store.ReconcileCandidate, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []store.ReconcileCandidate
	for _, c := range f.cands {
		if !f.settled[c.AttemptID] {
			if _, done := f.closed[c.AttemptID]; !done {
				out = append(out, c)
			}
		}
	}
	return out, nil
}

func (f *fakeStore) ReconcileStep(ctx context.Context, r store.ReservationReader, id string) (store.Attempt, store.Settlement, error) {
	look, err := r.ReceiptByProof(ctx, "p1_"+id)
	if err != nil {
		return store.Attempt{}, store.Settlement{}, fmt.Errorf("attempt %s: read scope: %w: %w", id, store.ErrOutcomeUnknown, err)
	}
	if look.State != store.ScopeCommitted {
		return store.Attempt{}, store.Settlement{}, nil
	}
	f.mu.Lock()
	f.settled[id] = true
	f.mu.Unlock()
	return store.Attempt{}, store.Settlement{Settled: true, Outcome: "committed"}, nil
}

func (f *fakeStore) CloseRecovered(_ context.Context, id string, h store.ScopeHold) (store.Attempt, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.closed[id] = h
	return store.Attempt{}, true, nil
}

// fakeReader answers every read with answer, or err; it counts reads.
type fakeReader struct {
	mu     sync.Mutex
	reads  int
	err    error
	commit bool
	hold   store.ScopeHold
	order  []string
}

func (f *fakeReader) ReceiptByProof(_ context.Context, digest string) (store.ScopeReceiptLookup, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reads++
	f.order = append(f.order, digest)
	if f.err != nil {
		return store.ScopeReceiptLookup{}, f.err
	}
	if f.commit {
		return store.ScopeReceiptLookup{State: store.ScopeCommitted}, nil
	}
	return store.ScopeReceiptLookup{State: store.ScopeNone}, nil
}

func (f *fakeReader) ReceiptByKey(context.Context, store.TaskRef, store.ReservationOp, string) (store.ScopeReceiptLookup, error) {
	return store.ScopeReceiptLookup{State: store.ScopeNone}, nil
}

func (f *fakeReader) HoldStatus(_ context.Context, task store.TaskRef) (store.ScopeHold, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reads++
	f.order = append(f.order, "hold:"+task.TaskID)
	if f.err != nil {
		return store.ScopeHold{}, f.err
	}
	return f.hold, nil
}

type clock struct{ t time.Time }

func (c *clock) now() time.Time { return c.t }

func newLoop(st *fakeStore, r *fakeReader, c *clock) *Loop {
	l := New(st, r, slog.New(slog.NewTextHandler(io.Discard, nil)))
	l.Now = c.now
	return l
}

func candidates(pending, holds int) []store.ReconcileCandidate {
	var out []store.ReconcileCandidate
	for i := 0; i < pending; i++ {
		out = append(out, store.ReconcileCandidate{AttemptID: fmt.Sprintf("step-%03d", i), Pending: true})
	}
	for i := 0; i < holds; i++ {
		out = append(out, store.ReconcileCandidate{AttemptID: fmt.Sprintf("hold-%03d", i),
			Task: store.TaskRef{TaskID: fmt.Sprintf("task-%03d", i)}})
	}
	return out
}

func newFakeStore(c []store.ReconcileCandidate) *fakeStore {
	return &fakeStore{cands: c, closed: map[string]store.ScopeHold{}, settled: map[string]bool{}}
}

// The loop's reads never exceed PerMinute in any rolling minute.
func TestBudget(t *testing.T) {
	c := &clock{t: time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)}
	r := &fakeReader{}
	l := newLoop(newFakeStore(candidates(100, 20)), r, c)
	l.Round(context.Background())
	if r.reads != PerMinute {
		t.Fatalf("the first round read %d times", r.reads)
	}
	for i := 0; i < 3; i++ {
		c.t = c.t.Add(Tick)
		l.Round(context.Background())
	}
	if r.reads != PerMinute {
		t.Fatalf("within the minute: %d reads", r.reads)
	}
	c.t = c.t.Add(Tick + time.Second)
	l.Round(context.Background())
	if r.reads != 2*PerMinute {
		t.Fatalf("after the minute: %d reads", r.reads)
	}
}

// aimem's rate_limited or request_in_progress pauses the loop for its
// Retry-After, at least a tick.
func TestBackOff(t *testing.T) {
	for _, code := range []string{"rate_limited", "request_in_progress"} {
		t.Run(code, func(t *testing.T) {
			c := &clock{t: time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)}
			r := &fakeReader{err: &aimemread.Error{Code: code, Retryable: true, RetryAfter: 40 * time.Second}}
			l := newLoop(newFakeStore(candidates(5, 0)), r, c)
			l.Round(context.Background())
			if r.reads != 1 {
				t.Fatalf("the round went on after being asked to wait: %d reads", r.reads)
			}
			c.t = c.t.Add(39 * time.Second)
			l.Round(context.Background())
			if r.reads != 1 {
				t.Fatalf("read during the pause: %d", r.reads)
			}
			c.t = c.t.Add(2 * time.Second)
			r.err = nil
			l.Round(context.Background())
			if r.reads != 6 {
				t.Fatalf("after the pause: %d reads", r.reads)
			}
		})
	}
	// Another read error does not pause.
	c := &clock{t: time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)}
	r := &fakeReader{err: errors.New("read_unavailable")}
	l := newLoop(newFakeStore(candidates(5, 0)), r, c)
	l.Round(context.Background())
	if r.reads != 5 {
		t.Fatalf("an unavailable read stopped the round: %d reads", r.reads)
	}
}

// Nothing closes while aimem is unavailable or busy, however long; a hold
// read that answers anything but closed closes nothing either.
func TestClosesOnlyOnAClosedHoldItRead(t *testing.T) {
	c := &clock{t: time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)}
	st := newFakeStore(candidates(0, 3))
	r := &fakeReader{err: &aimemread.Error{Code: "request_in_progress", Retryable: true}}
	l := newLoop(st, r, c)
	for i := 0; i < 240; i++ { // an hour of rounds
		l.Round(context.Background())
		c.t = c.t.Add(Tick)
	}
	r.err = &aimemread.Error{Code: aimemread.CodeUnavailable, Retryable: true}
	for i := 0; i < 240; i++ {
		l.Round(context.Background())
		c.t = c.t.Add(Tick)
	}
	for _, h := range []store.ScopeHold{{State: store.ScopeNone}, {State: store.ScopeHeld, ReservationID: "r", Fence: "2"}} {
		r.err, r.hold = nil, h
		l.Round(context.Background())
		c.t = c.t.Add(time.Minute)
	}
	if len(st.closed) != 0 {
		t.Fatalf("closed without a closed hold: %v", st.closed)
	}
	r.hold = store.ScopeHold{State: store.ScopeClosed, ReservationID: "r", ClosingFence: "3", ClosedBy: "recovery_release"}
	if _, closed := l.Round(context.Background()); closed != 3 || st.closed["hold-001"] != r.hold {
		t.Fatalf("a closed hold: %d closed, %v", closed, st.closed)
	}
}

// Pending steps come first; holds are checked least recently checked
// first, so a budget smaller than the list still reaches every hold.
func TestOrder(t *testing.T) {
	c := &clock{t: time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)}
	st := newFakeStore(candidates(2, 40))
	r := &fakeReader{hold: store.ScopeHold{State: store.ScopeHeld, ReservationID: "r", Fence: "2"}}
	l := newLoop(st, r, c)
	l.Round(context.Background())
	if r.order[0] != "p1_step-000" || r.order[1] != "p1_step-001" || r.order[2] != "hold:task-000" {
		t.Fatalf("the first reads: %v", r.order[:3])
	}
	seen := map[string]bool{}
	for i := 0; i < 3; i++ {
		c.t = c.t.Add(time.Minute + time.Second)
		l.Round(context.Background())
	}
	for _, o := range r.order {
		seen[o] = true
	}
	for i := 0; i < 40; i++ {
		if !seen[fmt.Sprintf("hold:task-%03d", i)] {
			t.Fatalf("hold %d never checked in four minutes: %v", i, r.order)
		}
	}
}

// Run rounds until its context ends.
func TestRunStops(t *testing.T) {
	st := newFakeStore(candidates(1, 0))
	r := &fakeReader{commit: true}
	l := New(st, r, slog.New(slog.NewTextHandler(io.Discard, nil)))
	l.Tick = 10 * time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- l.Run(ctx) }()
	deadline := time.After(5 * time.Second)
	for {
		st.mu.Lock()
		ok := st.settled["step-000"]
		st.mu.Unlock()
		if ok {
			break
		}
		select {
		case <-deadline:
			t.Fatal("the loop never settled")
		case <-time.After(5 * time.Millisecond):
		}
	}
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("Run: %v", err)
	}
}

// slowStore's pending steps each cost two reads and never resolve, like an
// abandoned update whose hold does not move; its holds close when read.
type slowStore struct{ *fakeStore }

func (s slowStore) ReconcileStep(ctx context.Context, r store.ReservationReader, id string) (store.Attempt, store.Settlement, error) {
	for i := 0; i < 2; i++ {
		if _, err := r.HoldStatus(ctx, store.TaskRef{TaskID: id}); err != nil {
			return store.Attempt{}, store.Settlement{}, fmt.Errorf("attempt %s: read scope: %w: %w", id, store.ErrOutcomeUnknown, err)
		}
	}
	return store.Attempt{}, store.Settlement{RetryAfter: time.Second}, nil
}

// Steps that stay unresolved and cost more than a window cannot starve the
// work behind them: every candidate is taken least recently checked first.
func TestUnresolvedStepsDoNotStarve(t *testing.T) {
	c := &clock{t: time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)}
	st := slowStore{newFakeStore(candidates(20, 1))}
	r := &fakeReader{hold: store.ScopeHold{State: store.ScopeClosed, ReservationID: "r", ClosingFence: "3", ClosedBy: "recovery_release"}}
	l := New(st, r, slog.New(slog.NewTextHandler(io.Discard, nil)))
	l.Now = c.now
	if _, closed := l.Round(context.Background()); closed != 0 {
		t.Fatal("the first window reached the hold behind 40 reads of steps")
	}
	c.t = c.t.Add(time.Minute + time.Second)
	if _, closed := l.Round(context.Background()); closed != 1 {
		t.Fatalf("the second window did not reach the hold: %d reads, %v", r.reads, r.order)
	}
}
