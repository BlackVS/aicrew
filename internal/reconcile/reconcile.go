// Package reconcile is aicrewd's reconciliation loop (crew-execution b3b).
// With no member online, it settles the steps members left pending and
// closes attempts whose reservation aimem closed outside aicrew, acting only
// on aimem's read scope, as the store's reconciler caller:
//
//   - every Tick it lists the candidates, pending steps first and then the
//     open attempts holding a reservation, and works through them oldest (or
//     least recently checked) first;
//   - its reads share a budget of PerMinute in any rolling minute, the rest
//     of the credential's limit being left to members' settles; when the
//     budget is spent the round stops;
//   - when aimem answers rate_limited or request_in_progress, the loop
//     pauses for aimem's Retry-After (at least a tick) before reading again;
//   - it closes an attempt as recovered only on a hold answer it read, and
//     the store closes only on the scope's closure evidence for that exact
//     reservation. An error, or no answer however long, closes nothing.
package reconcile

import (
	"context"
	"errors"
	"log/slog"
	"sort"
	"sync"
	"time"

	"github.com/BlackVS/aicrew/internal/aimemread"
	"github.com/BlackVS/aicrew/internal/store"
)

const (
	// Tick is how often a round runs.
	Tick = 15 * time.Second
	// PerMinute bounds the loop's reads in any rolling minute: half of the
	// read credential's 60, the rest left to members' settles.
	PerMinute = 30
)

// Store is the part of the store the loop acts through.
type Store interface {
	ReconcileCandidates(ctx context.Context) ([]store.ReconcileCandidate, error)
	ReconcileStep(ctx context.Context, reader store.ReservationReader, attemptID string) (store.Attempt, store.Settlement, error)
	CloseRecovered(ctx context.Context, attemptID string, hold store.ScopeHold) (store.Attempt, bool, error)
}

// Loop is the reconciliation loop over one store and one read scope.
type Loop struct {
	Store     Store
	Reader    store.ReservationReader
	Log       *slog.Logger
	Tick      time.Duration
	PerMinute int
	Now       func() time.Time

	mu      sync.Mutex
	reads   []time.Time // the loop's reads in the last minute
	paused  time.Time   // no read before this
	checked map[string]time.Time
}

// New returns a loop with the default tick and budget.
func New(st Store, reader store.ReservationReader, log *slog.Logger) *Loop {
	return &Loop{Store: st, Reader: reader, Log: log, Tick: Tick, PerMinute: PerMinute, Now: time.Now}
}

// errBudget stops a round: the loop's reads for this minute are spent, or
// aimem asked it to wait.
var errBudget = errors.New("reconcile: the read budget is spent or aimem asked to wait")

// Run runs a round every Tick until ctx ends.
func (l *Loop) Run(ctx context.Context) error {
	l.Log.Info("reconcile: started", "tick", l.Tick.String(), "reads_per_minute", l.PerMinute)
	t := time.NewTicker(l.Tick)
	defer t.Stop()
	for {
		l.Round(ctx)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.C:
		}
	}
}

// Round is one pass over the candidates. It returns how many steps it
// settled and attempts it closed.
func (l *Loop) Round(ctx context.Context) (settled, closed int) {
	cands, err := l.Store.ReconcileCandidates(ctx)
	if err != nil {
		l.Log.Warn("reconcile: list the candidates", "error", err.Error())
		return 0, 0
	}
	// Every candidate, a pending step or a hold, is taken least recently
	// checked first, so candidates that stay unresolved cannot use up
	// every minute's budget ahead of the others; pending steps come first
	// among equals.
	l.mu.Lock()
	if l.checked == nil {
		l.checked = map[string]time.Time{}
	}
	sort.SliceStable(cands, func(i, j int) bool {
		ci, cj := l.checked[cands[i].AttemptID], l.checked[cands[j].AttemptID]
		if !ci.Equal(cj) {
			return ci.Before(cj)
		}
		return cands[i].Pending && !cands[j].Pending
	})
	l.mu.Unlock()
	reader := budgeted{l}
	for _, c := range cands {
		if c.Pending {
			_, set, err := l.Store.ReconcileStep(ctx, reader, c.AttemptID)
			if errors.Is(err, errBudget) {
				return settled, closed
			}
			l.mark(c.AttemptID)
			if err != nil && !errors.Is(err, store.ErrOutcomeUnknown) {
				l.Log.Warn("reconcile: settle a step", "attempt_id", c.AttemptID, "error", err.Error())
			}
			if set.Settled {
				settled++
			}
			continue
		}
		hold, err := reader.HoldStatus(ctx, c.Task)
		if errors.Is(err, errBudget) {
			return settled, closed
		}
		l.mark(c.AttemptID)
		if err != nil || hold.State != store.ScopeClosed {
			continue
		}
		if _, ok, err := l.Store.CloseRecovered(ctx, c.AttemptID, hold); err != nil {
			l.Log.Warn("reconcile: close as recovered", "attempt_id", c.AttemptID, "error", err.Error())
		} else if ok {
			closed++
			l.Log.Info("reconcile: closed as recovered", "attempt_id", c.AttemptID, "closed_by", hold.ClosedBy)
		}
	}
	return settled, closed
}

// mark records that a candidate was just checked.
func (l *Loop) mark(id string) {
	l.mu.Lock()
	l.checked[id] = l.Now()
	l.mu.Unlock()
}

// take spends one read of the budget, or reports errBudget.
func (l *Loop) take() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.Now()
	if now.Before(l.paused) {
		return errBudget
	}
	i := 0
	for i < len(l.reads) && !l.reads[i].After(now.Add(-time.Minute)) {
		i++
	}
	l.reads = l.reads[i:]
	if len(l.reads) >= l.PerMinute {
		return errBudget
	}
	l.reads = append(l.reads, now)
	return nil
}

// backOff pauses the loop when aimem asked it to wait.
func (l *Loop) backOff(err error) {
	var e *aimemread.Error
	if !errors.As(err, &e) || (e.Code != "rate_limited" && e.Code != "request_in_progress") {
		return
	}
	wait := max(e.RetryAfter, l.Tick)
	l.mu.Lock()
	if until := l.Now().Add(wait); until.After(l.paused) {
		l.paused = until
	}
	l.mu.Unlock()
}

// budgeted is the loop's read scope: every read spends the budget, and
// aimem's request to wait pauses the loop.
type budgeted struct{ l *Loop }

func (b budgeted) ReceiptByProof(ctx context.Context, digest string) (store.ScopeReceiptLookup, error) {
	if err := b.l.take(); err != nil {
		return store.ScopeReceiptLookup{}, err
	}
	out, err := b.l.Reader.ReceiptByProof(ctx, digest)
	b.l.backOff(err)
	return out, err
}

func (b budgeted) ReceiptByKey(ctx context.Context, task store.TaskRef, op store.ReservationOp, digest string) (store.ScopeReceiptLookup, error) {
	if err := b.l.take(); err != nil {
		return store.ScopeReceiptLookup{}, err
	}
	out, err := b.l.Reader.ReceiptByKey(ctx, task, op, digest)
	b.l.backOff(err)
	return out, err
}

func (b budgeted) HoldStatus(ctx context.Context, task store.TaskRef) (store.ScopeHold, error) {
	if err := b.l.take(); err != nil {
		return store.ScopeHold{}, err
	}
	out, err := b.l.Reader.HoldStatus(ctx, task)
	b.l.backOff(err)
	return out, err
}
