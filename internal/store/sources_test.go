package store

import (
	"context"
	"strings"
	"testing"
	"time"
)

// openAttemptOf is the worker's open attempt as the operator's team state
// shows it.
func openAttemptOf(t *testing.T, e execTeam) *MemberAttempt {
	t.Helper()
	st, err := e.s.GetTeamState(context.Background(), operator(t), e.tm.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range st.Members {
		if m.AgentID == e.builder.agent.ID {
			return m.OpenAttempt
		}
	}
	t.Fatal("the worker is not in the team state")
	return nil
}

// A blocked report keeps its reason and start on the attempt, which the
// team state shows apart from a hub's grant block; leaving the blocked
// phase clears it and leaves the hub's block as it was.
func TestBlockedReportFacts(t *testing.T) {
	e, a := running(t)
	now := clock(e.s)
	if oa := openAttemptOf(t, e); oa == nil || oa.Block != nil || oa.HubBlock != nil {
		t.Fatalf("a working attempt: %+v", oa)
	}
	e.mustWork(t, "block", a, IntentBlock, "The test database is unavailable.")
	oa := openAttemptOf(t, e)
	if oa == nil || oa.Phase != PhaseBlocked || oa.Block == nil || oa.Block.Reason != "The test database is unavailable." ||
		!oa.Block.Since.Equal(*now) || oa.HubBlock != nil {
		t.Fatalf("a blocked attempt: %+v", oa)
	}
	grant(t, e.s, e.tm.ID) // the hub revokes project-a
	if oa = openAttemptOf(t, e); oa.Block == nil || oa.HubBlock == nil || oa.HubBlock.Reason != BlockedGrantRevoked {
		t.Fatalf("a blocked attempt the hub blocks too: %+v", oa)
	}
	grant(t, e.s, e.tm.ID, ProjectRef{HubID: "hub-a", ProjectID: "project-a"})
	*now = now.Add(time.Minute)
	e.mustWork(t, "resume", a, IntentResume, "")
	if oa = openAttemptOf(t, e); oa.Phase != PhaseWorking || oa.Block != nil || oa.HubBlock != nil {
		t.Fatalf("a resumed attempt: %+v", oa)
	}
}

// A worker's blocked report wakes the team's coordinators once, through the
// blocked-wake subscription, with the reason and the attempt's last
// observed pull request; the report's broadcast does not reach them.
func TestBlockedReportWakesTheCoordinator(t *testing.T) {
	ctx := context.Background()
	e, a := running(t)
	now := clock(e.s)
	if _, err := e.s.db.Exec(`INSERT INTO attempt_pulls (attempt_id, number, url, state, head_commit, labels, checks, comments, observed_at)
		VALUES (?, 7, 'https://git.example.test/crew/project-a/pulls/7', 'open', '0123456789abcdef0123456789abcdef01234567',
		'["hands-reviewed"]', 'failure', 3, ?)`, a.ID, formatTime(*now)); err != nil {
		t.Fatal(err)
	}
	before := len(read(t, e.s, e.lead, 50))
	e.mustWork(t, "block", a, IntentBlock, "CI fails on a test I cannot read.")
	if got := read(t, e.s, e.lead, 50); len(got) != before {
		t.Fatalf("the coordinator was told before the queue ran: %+v", got[before:])
	}
	runs := settle(t, e.s, now)
	if len(runs) != 1 || runs[0].Subscription != "blocked-wake" || runs[0].Subject != "attempt:"+a.ID {
		t.Fatalf("the runs: %+v", runs)
	}
	got := read(t, e.s, e.lead, 50)[before:]
	want := "builder reported task hub-a/project-a/task-1 blocked: CI fails on a test I cannot read. " +
		"Its pull request #7 (https://git.example.test/crew/project-a/pulls/7) is open at 0123456789ab; labels: hands-reviewed; " +
		"CI: failure; 3 comment(s)."
	if len(got) != 1 || !strings.HasPrefix(got[0].Text, want) || got[0].AttemptID != a.ID || got[0].To != e.lead.agent.ID ||
		!strings.HasSuffix(got[0].Text, "Relay what the worker needs, or escalate with these facts attached.") {
		t.Fatalf("the coordinator's messages: %+v", got)
	}
	if w := read(t, e.s, e.builder, 50); len(w) == 0 || strings.Contains(w[len(w)-1].Text, "reported task") {
		t.Fatalf("the worker was woken for its own report: %+v", w)
	}
	// A resume is journaled, and no subscription queues it.
	e.mustWork(t, "resume", a, IntentResume, "")
	if runs := settle(t, e.s, now); len(runs) != 0 {
		t.Fatalf("the resume's run: %+v", runs)
	}
	var phases []string
	rows, err := e.s.db.QueryContext(ctx, `SELECT json_extract(payload, '$.to') FROM events WHERE kind = 'attempt.changed' ORDER BY rowid`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var p string
		rows.Scan(&p)
		phases = append(phases, p)
	}
	rows.Close()
	if strings.Join(phases, ",") != "blocked,working" {
		t.Fatalf("the attempt's events: %v", phases)
	}
}

// A blocked report as long as a message is accepted: its event carries the
// start of the reason, within the payload bound, and the attempt the whole.
func TestLongBlockedReport(t *testing.T) {
	e, a := running(t)
	prefix, err := workMessage(context.Background(), e.s.db, a, IntentBlock, "")
	if err != nil {
		t.Fatal(err)
	}
	reason := strings.Repeat("y", maxMessageText-len(prefix))
	e.mustWork(t, "block", a, IntentBlock, reason)
	if oa := openAttemptOf(t, e); oa.Block == nil || oa.Block.Reason != reason {
		t.Fatal("the attempt does not keep the whole reason")
	}
	var n int
	if err := e.s.db.QueryRow(`SELECT length(json_extract(payload, '$.reason')) FROM events WHERE kind = 'attempt.changed'`).Scan(&n); err != nil ||
		n > maxEventReason+3 {
		t.Fatalf("the event's reason: %d bytes, %v", n, err)
	}
}

// Session starts, resumes and ends are journaled as member.changed events;
// no default subscription wakes anyone for them.
func TestMemberEvents(t *testing.T) {
	ctx := context.Background()
	s, _ := openTemp(t)
	e := newExecTeam(t, s)
	if _, err := s.ResumeSession(ctx, e.builder.caller, "resume", e.builder.sess.ID); err != nil {
		t.Fatal(err)
	}
	cur, err := s.GetSession(ctx, e.builder.sess.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.LeaveSession(ctx, e.builder.caller, "leave", cur.ID, cur.Generation); err != nil {
		t.Fatal(err)
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT json_extract(payload, '$.change') FROM events WHERE kind = 'member.changed' AND subject = ? ORDER BY rowid`,
		"member:"+e.builder.agent.ID)
	if err != nil {
		t.Fatal(err)
	}
	var changes []string
	for rows.Next() {
		var c string
		rows.Scan(&c)
		changes = append(changes, c)
	}
	rows.Close()
	if strings.Join(changes, ",") != "started,resumed,left" {
		t.Fatalf("the worker's member events: %v", changes)
	}
	var queued int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM event_deliveries d JOIN events e ON e.id = d.event_id
		WHERE e.kind = 'member.changed'`).Scan(&queued); err != nil || queued != 0 {
		t.Fatalf("member events queued for %d deliveries, %v", queued, err)
	}
}

// A stored deadline journals one timer.due event on the first delivery
// after it passes, across a restart; set again, it fires again.
func TestTimerFiresOnce(t *testing.T) {
	ctx := context.Background()
	s, path := openTemp(t)
	now := clock(s)
	set := func(s *Store, due time.Time) {
		t.Helper()
		tx, err := s.db.BeginTx(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		if err := setTimer(ctx, tx, "review-pickup/attempt-1", "team-1", "attempt:attempt-1",
			map[string]string{"why": "no review pickup"}, due); err != nil {
			t.Fatal(err)
		}
		if err := tx.Commit(); err != nil {
			t.Fatal(err)
		}
	}
	fired := func(s *Store) int {
		t.Helper()
		var n int
		if err := s.db.QueryRow(`SELECT COUNT(*) FROM events WHERE source = 'timer' AND kind = 'timer.due'`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	due := now.Add(30 * time.Minute)
	set(s, due)
	deliver(t, s)
	if n := fired(s); n != 0 {
		t.Fatalf("%d events before the deadline", n)
	}
	// aicrewd restarts before the deadline.
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s2, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	later := due.Add(time.Second)
	s2.now = func() time.Time { return later }
	deliver(t, s2)
	deliver(t, s2)
	if n := fired(s2); n != 1 {
		t.Fatalf("%d events after the deadline, want 1", n)
	}
	var at, payload string
	if err := s2.db.QueryRow(`SELECT at, payload FROM events WHERE source = 'timer'`).Scan(&at, &payload); err != nil ||
		at != formatTime(due) || payload != `{"why":"no review pickup"}` {
		t.Fatalf("the timer's event: %s %s %v", at, payload, err)
	}
	set(s2, later.Add(time.Minute))
	later = later.Add(2 * time.Minute)
	deliver(t, s2)
	if n := fired(s2); n != 2 {
		t.Fatalf("a timer set again: %d events, want 2", n)
	}
}
