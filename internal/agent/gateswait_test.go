package agent

import (
	"slices"
	"testing"
	"time"

	"github.com/BlackVS/aicrew/internal/automation"
	"github.com/BlackVS/aicrew/internal/forge"
	"github.com/BlackVS/aicrew/internal/store"
)

// The worker's wait on its launcher's gates-green submit (01a206f8-24d0)
// reads the condition the gates-green edge fires on, through the same
// function, from the launcher's own observation, and lasts gatesGreenWait
// from the first observation at which it held for the head.
func TestGatesGreenWait(t *testing.T) {
	h1, h2 := "0123456789abcdef0123456789abcdef01234567", "89abcdef0123456789abcdef0123456789abcdef"
	key := pullKey("git.example.test", "crew/p", 4)
	green := func(head string) PullObservation {
		return PullObservation{Host: "git.example.test", Repository: "crew/p", Number: 4, State: forge.PullOpen,
			HeadCommit: head, Checks: forge.ChecksSuccess, Labels: []string{"hands-reviewed"},
			Automations: []string{"review-request", automationGatesGreen}}
	}
	ready := func(head string) map[string]pointReview {
		return map[string]pointReview{forge.ReviewerLocal: {Head: head, Verdict: verdictReady},
			forge.ReviewerExternal: {Head: head, Verdict: verdictReady}}
	}
	// observe takes o through the edges, as the launcher's notice of an
	// observation does, keeps the state, and makes o the last observation,
	// one aicrewd took.
	observe := func(s *StepServer, p *pullPoints, o PullObservation, f pullFacts) []string {
		t.Helper()
		got := p.edges(o, f, map[string]bool{}, key)
		for _, pt := range got {
			p.Fired[pt] = append(p.Fired[pt], edgeOf(pt, o, p.Head))
		}
		doc := pointsDoc{Pulls: map[string]*pullPoints{key: p}}
		if err := writePoints(s.home, doc, key); err != nil {
			t.Fatal(err)
		}
		o.reported = true
		s.seenMu.Lock()
		s.last = []PullObservation{o}
		s.seenMu.Unlock()
		return got
	}
	fresh := func() (*StepServer, *pullPoints) {
		return &StepServer{home: t.TempDir()}, &pullPoints{Attempt: "att-1", Fired: map[string][]string{}}
	}

	if automationGatesGreen != store.AutomationGatesGreen {
		t.Fatalf("the launcher names gates-green %q, aicrewd %q", automationGatesGreen, store.AutomationGatesGreen)
	}

	// The condition holds: the edge fires, and the wait starts with it.
	s, p := fresh()
	if got := observe(s, p, green(h1), pullFacts{Reviews: ready(h1)}); !slices.Contains(got, automation.PointGatesGreen) {
		t.Fatalf("the gates-green edge did not fire: %v", got)
	}
	since := p.GreenSince
	if p.GreenHead != h1 || since.IsZero() {
		t.Fatalf("the wait's start: %q %v", p.GreenHead, since)
	}
	for _, c := range []struct {
		name    string
		attempt string
		head    string
		at      time.Duration
		want    bool
	}{
		{"at its start", "att-1", h1, 0, true},
		{"before the bound", "att-1", h1, gatesGreenWait() - time.Second, true},
		{"at the bound", "att-1", h1, gatesGreenWait(), false},
		{"another attempt", "att-2", h1, 0, false},
		{"another head", "att-1", h2, 0, false},
		{"no head", "att-1", "", 0, false},
	} {
		if got := s.GatesGreenWait(c.attempt, c.head, since.Add(c.at)); got != c.want {
			t.Errorf("%s: %v, want %v", c.name, got, c.want)
		}
	}
	// The same observation, not taken by aicrewd: the points may not hold
	// what it showed, so there is no wait.
	s.seenMu.Lock()
	s.last[0].reported = false
	s.seenMu.Unlock()
	if s.GatesGreenWait("att-1", h1, since) {
		t.Error("a wait on an observation aicrewd did not take")
	}
	// A later observation that leaves the condition ends the wait, and one
	// that holds it again keeps the first start: the count is per head.
	o := green(h1)
	o.Checks = forge.ChecksPending
	observe(s, p, o, pullFacts{})
	if s.GatesGreenWait("att-1", h1, since) {
		t.Error("a wait while the checks are pending again")
	}
	observe(s, p, green(h1), pullFacts{})
	if !p.GreenSince.Equal(since) || !s.GatesGreenWait("att-1", h1, since) {
		t.Errorf("the same head started a new count: %v, was %v", p.GreenSince, since)
	}
	// A new head starts a new count once the condition holds there.
	p.GreenSince = since.Add(-time.Hour)
	observe(s, p, green(h2), pullFacts{Reviews: ready(h2)})
	if p.GreenHead != h2 || !p.GreenSince.After(since.Add(-time.Hour)) || s.GatesGreenWait("att-1", h1, p.GreenSince) ||
		!s.GatesGreenWait("att-1", h2, p.GreenSince) {
		t.Errorf("the new head's count: %q %v", p.GreenHead, p.GreenSince)
	}

	// Nothing to wait on: the worker's own review missing or not READY at
	// the head, the external one missing, gates-green not in the set or the
	// set unread, an external review in flight.
	for _, c := range []struct {
		name    string
		o       func(PullObservation) PullObservation
		reviews map[string]pointReview
	}{
		{"no review of the worker", nil, map[string]pointReview{forge.ReviewerExternal: {Head: h1, Verdict: verdictReady}}},
		{"the worker's review not READY", nil, map[string]pointReview{
			forge.ReviewerLocal:    {Head: h1, Verdict: "RETURN_TO_IMPLEMENTATION", Blocker: true},
			forge.ReviewerExternal: {Head: h1, Verdict: verdictReady}}},
		{"the worker's review of another head", nil, map[string]pointReview{
			forge.ReviewerLocal:    {Head: h2, Verdict: verdictReady},
			forge.ReviewerExternal: {Head: h1, Verdict: verdictReady}}},
		{"no external review", nil, map[string]pointReview{forge.ReviewerLocal: {Head: h1, Verdict: verdictReady}}},
		{"gates-green not in the set", func(o PullObservation) PullObservation {
			o.Automations = []string{"review-request"}
			return o
		}, ready(h1)},
		{"the set unread", func(o PullObservation) PullObservation { o.Automations = nil; return o }, ready(h1)},
		{"an external review in flight", func(o PullObservation) PullObservation {
			o.Labels = []string{labelReviewing}
			return o
		}, ready(h1)},
		{"CI failed", func(o PullObservation) PullObservation { o.Checks = forge.ChecksFailure; return o }, ready(h1)},
	} {
		s, p := fresh()
		o := green(h1)
		if c.o != nil {
			o = c.o(o)
		}
		observe(s, p, o, pullFacts{Reviews: c.reviews})
		if s.GatesGreenWait("att-1", h1, time.Now()) {
			t.Errorf("%s: the worker waits on the launcher", c.name)
		}
	}
}

// After a worker turn that leaves the attempt in work, the next tick reads
// the watched pull requests before it decides on a continuation: a read
// that shows the worker's review READY beside the external one, with CI
// green and gates-green in the set, makes the wait, with no further turn
// and no stall. A read that shows it late costs the continuations before
// it, within the stall rule; the turn that reaches the stall count reads
// once more before the stall. Past the wait's bound, or with no such wait,
// the continuations and the stall come as before.
func TestTurnLoopWaitsOnTheLaunchersSubmit(t *testing.T) {
	// The external review READY at the head with CI green: today's rule
	// has nothing left to wait on.
	reviewed := PullObservation{Host: "git.example.test", Repository: "crew/p", Number: 4, State: forge.PullOpen,
		HeadCommit: "h1", Labels: []string{"hands-reviewed"}, Checks: forge.ChecksSuccess,
		Review: &ObservedReview{AtHead: true, Verdict: verdictReady}}
	// start runs the worker's turn that pushed h1 and posted its review;
	// showAfter is the number of reads after that turn before one shows
	// the wait, -1 for none.
	start := func(showAfter int) *loopEnv {
		e := newLoopEnv(t)
		e.host.attempt = &memberAttempt{ID: "att-1", State: "running", Phase: "working", Task: TaskRef{TaskID: "task-1"}}
		e.host.pending = []PendingMessage{msg("offer", "lifecycle")}
		e.next = func(TurnSpec, *fakeHost) TurnResult {
			e.host.pending, e.head, e.host.observed = nil, "h1", []PullObservation{reviewed}
			return TurnResult{Outcome: TurnFinished}
		}
		e.tick(t)
		e.next = func(TurnSpec, *fakeHost) TurnResult {
			e.host.pending = nil
			return TurnResult{Outcome: TurnFinished}
		}
		reads := 0
		e.host.onPoll = func(h *fakeHost) {
			reads++
			if showAfter >= 0 && reads > showAfter {
				h.mu.Lock()
				h.greenHead = "h1"
				h.mu.Unlock()
			}
		}
		return e
	}

	for _, c := range []struct {
		name      string
		showAfter int
		turns     int // turns in all, the first included
		blocks    int
	}{
		{"the read after the turn shows it", 0, 1, 0},
		{"the forge shows it one read late", 1, 2, 0},
		{"the forge shows it two reads late", 2, 3, 0},
		// The turn that reaches the stall count is read once more before
		// the stall: a review that shows there waits (hands review of #286).
		{"the forge shows it at the read before the stall", stallTurns, 1 + stallTurns, 0},
		{"the forge shows it after the stall", stallTurns + 1, 1 + stallTurns, 1},
		{"no wait on the launcher", -1, 1 + stallTurns, 1},
	} {
		e := start(c.showAfter)
		for range 10 {
			e.tick(t)
		}
		if len(e.specs) != c.turns || len(e.host.calls) != c.blocks {
			t.Errorf("%s: %d turns and %d blocks, want %d and %d", c.name, len(e.specs), len(e.host.calls), c.turns,
				c.blocks)
		}
		// One read after each worker turn, before the continuation's or
		// the stall's decision, and none more.
		if e.host.polls != 1+c.turns {
			t.Errorf("%s: %d reads for %d turns, not one after each", c.name, e.host.polls, c.turns)
		}
	}

	// Past the bound the launcher answers no wait: the continuations and
	// the stall come as for any other turn.
	e := start(0)
	for range 4 {
		e.tick(t)
	}
	if len(e.specs) != 1 || len(e.host.calls) != 0 {
		t.Fatalf("while waiting: %d turns and %d blocks", len(e.specs), len(e.host.calls))
	}
	e.host.onPoll = nil
	e.host.mu.Lock()
	e.host.greenHead = ""
	e.host.mu.Unlock()
	for range 10 {
		e.tick(t)
	}
	if len(e.specs) != 1+stallTurns || len(e.host.calls) != 1 {
		t.Fatalf("past the bound: %d turns and %d blocks, want %d and 1", len(e.specs), len(e.host.calls), 1+stallTurns)
	}
	if st := readTurnState(e.home).Attempts["att-1"]; st == nil || !st.Stalled {
		t.Fatalf("past the bound the attempt is not stalled: %+v", st)
	}

	// A wait found by the read before the stall resets both stall counts,
	// as every wait on the gates does: past its bound, the stall comes after
	// stallTurns turns more, counted from the wait.
	e = start(stallTurns)
	for range 10 {
		e.tick(t)
	}
	if len(e.specs) != 1+stallTurns || len(e.host.calls) != 0 {
		t.Fatalf("while waiting from the read before the stall: %d turns and %d blocks", len(e.specs), len(e.host.calls))
	}
	e.host.onPoll = nil
	e.host.mu.Lock()
	e.host.greenHead = ""
	e.host.mu.Unlock()
	for range 10 {
		e.tick(t)
	}
	if len(e.specs) != 1+2*stallTurns || len(e.host.calls) != 1 {
		t.Fatalf("past the bound of a wait from the read before the stall: %d turns and %d blocks, want %d and 1",
			len(e.specs), len(e.host.calls), 1+2*stallTurns)
	}
}
