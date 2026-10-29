package store

import (
	"context"
	"strings"
	"testing"
	"time"
)

// pickups is the review_pickup subjects waiting in the token's inbox, which
// it then acknowledges.
func pickups(t *testing.T, s *Store, token string) []ActionSubject {
	t.Helper()
	items, err := s.ReadInboxWithToken(context.Background(), token, 100)
	if err != nil {
		t.Fatal(err)
	}
	var out []ActionSubject
	var read []InboxItem
	for _, it := range items {
		if it.Action == nil {
			continue
		}
		for _, sub := range it.Action.Subjects {
			if sub.Kind == "review_pickup" {
				out = append(out, sub)
				read = append(read, it)
			}
		}
	}
	if len(read) > 0 {
		ackTok(t, s, token, read)
	}
	return out
}

// reviewWaitOf is the wait the attempt's pull request facts hold.
func reviewWaitOf(t *testing.T, s *Store, attemptID string) *PullWait {
	t.Helper()
	tx, err := s.db.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback() //nolint:errcheck // read only
	f, err := readPullFacts(context.Background(), tx, attemptID)
	if err != nil || f == nil {
		t.Fatalf("the facts: %+v, %v", f, err)
	}
	return f.Wait
}

// tick moves the clock to at and delivers twice: the first tick fires the
// timers due, the second, EventQuiet later, runs their queues.
func tick(t *testing.T, s *Store, now *time.Time, at time.Time) {
	t.Helper()
	*now = at
	deliver(t, s)
	settle(t, s, now)
}

// A review request that waits at the head for ReviewPickupAfter gives the
// team's coordinators one message naming the pull request, the attempt and
// the wait, and the worker none. It is not sent again, not by later ticks,
// not after a restarted launcher observes the same pull request again. The
// team state shows the wait, overdue once past the bound.
func TestReviewPickupReported(t *testing.T) {
	ctx := context.Background()
	e := newClaimStopEnv(t)
	a, _ := e.runningClaim(t, "task-1")
	o := observed(a.Branch)
	start := *e.now
	if _, err := e.s.ObservePullWithToken(ctx, e.indepTk, o); err != nil {
		t.Fatal(err)
	}
	w := reviewWaitOf(t, e.s, a.ID)
	if w == nil || w.Pull != 7 || w.Head != head1 || !w.Since.Equal(start) {
		t.Fatalf("the wait at the baseline: %+v", w)
	}
	tick(t, e.s, e.now, start.Add(ReviewPickupAfter-time.Minute))
	if got := pickups(t, e.s, e.leadTok); len(got) != 0 {
		t.Fatalf("a message before the bound: %+v", got)
	}
	// A restarted launcher's baseline: the same pull request, and then a
	// new comment; the wait goes on from where it began.
	o.Baseline = true
	if _, err := e.s.ObservePullWithToken(ctx, e.indepTk, o); err != nil {
		t.Fatal(err)
	}
	o.Baseline, o.Comments = false, 1
	if _, err := e.s.ObservePullWithToken(ctx, e.indepTk, o); err != nil {
		t.Fatal(err)
	}
	if w := reviewWaitOf(t, e.s, a.ID); w == nil || !w.Since.Equal(start) {
		t.Fatalf("the wait after the launcher restarted: %+v", w)
	}
	tick(t, e.s, e.now, start.Add(ReviewPickupAfter+time.Second))
	got := pickups(t, e.s, e.leadTok)
	if len(got) != 1 || got[0].AttemptID != a.ID || got[0].Wait == nil || got[0].Wait.Head != head1 ||
		got[0].Pull == nil || got[0].Pull.Pull != 7 {
		t.Fatalf("the coordinator's message: %+v", got)
	}
	for _, want := range []string{"pull request #7", o.URL, a.ID, "task-1", "`review-this` has waited at head 0123456",
		"(30 minutes)", "no `hands-reviewing`"} {
		if !strings.Contains(got[0].Text, want) {
			t.Errorf("the text does not say %q: %s", want, got[0].Text)
		}
	}
	for _, tok := range []string{e.indepTk, e.workerTok} {
		if got := pickups(t, e.s, tok); len(got) != 0 {
			t.Fatalf("a non-coordinator was told: %+v", got)
		}
	}
	tick(t, e.s, e.now, start.Add(3*ReviewPickupAfter))
	if got := pickups(t, e.s, e.leadTok); len(got) != 0 {
		t.Fatalf("told twice: %+v", got)
	}

	st, err := e.s.GetTeamState(ctx, operator(t), e.teamID)
	if err != nil {
		t.Fatal(err)
	}
	var seen bool
	for _, m := range st.Members {
		if m.OpenAttempt != nil && m.OpenAttempt.ID == a.ID {
			rw := m.OpenAttempt.ReviewWait
			seen = rw != nil && rw.Head == head1 && rw.Since.Equal(start)
		}
	}
	if !seen {
		t.Fatalf("the team state does not show the wait: %+v", st.Members)
	}
}

// The clock resets on a pickup, an external review at the head, a new head
// and a request added again: no message at the first bound, and, where the
// request still waits, one at the bound of the new wait.
func TestReviewPickupResets(t *testing.T) {
	ctx := context.Background()
	for name, c := range map[string]struct {
		change func(o *PullObservation)
		waits  bool
	}{
		"pickup":           {func(o *PullObservation) { o.Labels = []string{"hands-reviewing"} }, false},
		"failed run":       {func(o *PullObservation) { o.Labels = []string{"hands-review-failed"} }, false},
		"review at head":   {func(o *PullObservation) { o.Content, o.Review = true, hands(head1, verdictReady) }, false},
		"new head":         {func(o *PullObservation) { o.HeadCommit = head2 }, true},
		"request re-added": {nil, true},
		"merged":           {func(o *PullObservation) { o.State = "merged" }, false},
	} {
		t.Run(name, func(t *testing.T) {
			e := newClaimStopEnv(t)
			a, _ := e.runningClaim(t, "task-1")
			o := observed(a.Branch)
			start := *e.now
			if _, err := e.s.ObservePullWithToken(ctx, e.indepTk, o); err != nil {
				t.Fatal(err)
			}
			*e.now = start.Add(10 * time.Minute)
			if c.change != nil {
				c.change(&o)
			} else {
				o.Labels = []string{}
				if _, err := e.s.ObservePullWithToken(ctx, e.indepTk, o); err != nil {
					t.Fatal(err)
				}
				if w := reviewWaitOf(t, e.s, a.ID); w != nil {
					t.Fatalf("a wait without the request: %+v", w)
				}
				o.Labels = []string{"review-this"}
			}
			if _, err := e.s.ObservePullWithToken(ctx, e.indepTk, o); err != nil {
				t.Fatal(err)
			}
			if w := reviewWaitOf(t, e.s, a.ID); (w != nil) != c.waits || (w != nil && !w.Since.Equal(start.Add(10*time.Minute))) {
				t.Fatalf("the wait after the change: %+v", w)
			}
			ack := func() {
				for _, tok := range []string{e.indepTk, e.leadTok} {
					items, err := e.s.ReadInboxWithToken(ctx, tok, 100)
					if err != nil {
						t.Fatal(err)
					}
					if len(items) > 0 {
						ackTok(t, e.s, tok, items)
					}
				}
			}
			settle(t, e.s, e.now)
			ack()
			tick(t, e.s, e.now, start.Add(ReviewPickupAfter+time.Second))
			if got := pickups(t, e.s, e.leadTok); len(got) != 0 {
				t.Fatalf("told at the first wait's bound: %+v", got)
			}
			tick(t, e.s, e.now, start.Add(10*time.Minute+ReviewPickupAfter+time.Second))
			got := pickups(t, e.s, e.leadTok)
			if c.waits && (len(got) != 1 || got[0].Wait.Head != o.HeadCommit) {
				t.Fatalf("the new wait's message: %+v", got)
			}
			if !c.waits && len(got) != 0 {
				t.Fatalf("told after the wait ended: %+v", got)
			}
			if got := pickups(t, e.s, e.indepTk); len(got) != 0 {
				t.Fatalf("the worker was told: %+v", got)
			}
		})
	}
}
