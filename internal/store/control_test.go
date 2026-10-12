package store

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"
)

// Each member state follows from the pause, the session and what aicrewd
// last saw of it.
func TestDeriveState(t *testing.T) {
	now := time.Now()
	recent, idle := now.Add(-5*time.Second), now.Add(-time.Hour)
	for _, c := range []struct {
		name                 string
		paused, session      bool
		lastSeen, lastWaited time.Time
		want                 string
	}{
		{"paused wins over a waiting session", true, true, recent, recent, StatePaused},
		{"paused without a session", true, false, time.Time{}, time.Time{}, StatePaused},
		{"no session", false, false, recent, recent, StateOffline},
		{"its wake read the pending list just now", false, true, recent, recent, StateWaiting},
		{"heard from within the working window, not waiting", false, true, now.Add(-10 * time.Minute), now.Add(-10 * time.Minute), StateWorking},
		{"its last pending read is past the waiting window", false, true, now.Add(-20 * time.Second), now.Add(-20 * time.Second), StateWorking},
		{"never waited, heard from just now", false, true, recent, time.Time{}, StateWorking},
		{"not heard from within the working window", false, true, idle, idle, StateOffline},
		{"never heard from", false, true, time.Time{}, time.Time{}, StateOffline},
	} {
		if got := DeriveState(c.paused, c.session, c.lastSeen, c.lastWaited, now); got != c.want {
			t.Errorf("%s: %s, want %s", c.name, got, c.want)
		}
	}
}

// A team's pause, and a member's, hold back the member's pending read, so its
// wake starts no turn; the messages stay and are listed again on the
// resume. The session status names the pause in force. Pause and resume are
// idempotent and the operator's alone.
func TestPauseHoldsThePendingRead(t *testing.T) {
	ctx := context.Background()
	e := newEscTeam(t)
	op := operator(t)
	if _, err := e.s.Nudge(ctx, op, "n-1", e.tm.ID, e.worker.ID, "the upgraded agent is installed"); err != nil {
		t.Fatal(err)
	}
	if got := pendingKinds(t, e.s, e.workerTok); len(got) != 1 {
		t.Fatalf("pending before the pause: %+v", got)
	}

	res, err := e.s.SetPause(ctx, op, "p-1", e.tm.ID, "", true, "the hub is upgraded")
	if err != nil || !res.Changed || res.Paused == nil || res.Paused.Reason != "the hub is upgraded" ||
		res.Paused.PausedBy != "operator:op-1" || res.Paused.AgentID != "" {
		t.Fatalf("team pause: %+v %v", res, err)
	}
	if again, err := e.s.SetPause(ctx, op, "p-2", e.tm.ID, "", true, "again"); err != nil || again.Changed ||
		again.Paused.Reason != "the hub is upgraded" {
		t.Fatalf("pausing a paused team: %+v %v", again, err)
	}
	for who, tok := range map[string]string{"worker": e.workerTok, "coordinator": e.leadTok} {
		p, err := e.s.PendingWithToken(ctx, tok)
		if err != nil || len(p.Pending) != 0 || p.Paused == nil || p.Paused.AgentID != "" || p.SessionID == "" {
			t.Fatalf("%s's pending read while the team is paused: %+v %v", who, p, err)
		}
	}
	if p, err := e.s.PauseFor(ctx, e.tm.ID, e.worker.ID); err != nil || p == nil || p.Reason != "the hub is upgraded" {
		t.Fatalf("the worker's pause in force: %+v %v", p, err)
	}

	if res, err := e.s.SetPause(ctx, op, "r-1", e.tm.ID, "", false, ""); err != nil || !res.Changed || res.Paused != nil {
		t.Fatalf("team resume: %+v %v", res, err)
	}
	if again, err := e.s.SetPause(ctx, op, "r-2", e.tm.ID, "", false, ""); err != nil || again.Changed {
		t.Fatalf("resuming a running team: %+v %v", again, err)
	}
	if got := pendingKinds(t, e.s, e.workerTok); len(got) != 1 {
		t.Fatalf("pending after the resume: %+v", got)
	}

	// A member's own pause holds that member only.
	if res, err := e.s.SetPause(ctx, op, "p-3", e.tm.ID, e.worker.ID, true, ""); err != nil || !res.Changed ||
		res.Paused.AgentID != e.worker.ID {
		t.Fatalf("member pause: %+v %v", res, err)
	}
	if p, err := e.s.PendingWithToken(ctx, e.workerTok); err != nil || len(p.Pending) != 0 || p.Paused == nil ||
		p.Paused.AgentID != e.worker.ID {
		t.Fatalf("the paused worker's pending read: %+v %v", p, err)
	}
	if _, err := e.s.Nudge(ctx, op, "n-2", e.tm.ID, e.lead.ID, "for the coordinator"); err != nil {
		t.Fatal(err)
	}
	if p, err := e.s.PendingWithToken(ctx, e.leadTok); err != nil || len(p.Pending) != 1 || p.Paused != nil {
		t.Fatalf("the coordinator is not paused: %+v %v", p, err)
	}
	// The team's pause is the one in force while both hold.
	if _, err := e.s.SetPause(ctx, op, "p-4", e.tm.ID, "", true, "both"); err != nil {
		t.Fatal(err)
	}
	if p, _ := e.s.PauseFor(ctx, e.tm.ID, e.worker.ID); p == nil || p.AgentID != "" {
		t.Fatalf("the pause in force under both: %+v", p)
	}
	if _, err := e.s.SetPause(ctx, op, "r-3", e.tm.ID, "", false, ""); err != nil {
		t.Fatal(err)
	}
	if p, _ := e.s.PauseFor(ctx, e.tm.ID, e.worker.ID); p == nil || p.AgentID != e.worker.ID {
		t.Fatalf("the member's pause outlives the team's: %+v", p)
	}
	if _, err := e.s.SetPause(ctx, op, "r-4", e.tm.ID, e.worker.ID, false, ""); err != nil {
		t.Fatal(err)
	}
	if got := pendingKinds(t, e.s, e.workerTok); len(got) != 1 {
		t.Fatalf("pending after the member's resume: %+v", got)
	}
}

// Pause and nudge are refused to anyone but the operator, for an unknown
// team or a non-member, and with input they do not take.
func TestControlRefusals(t *testing.T) {
	ctx := context.Background()
	e := newEscTeam(t)
	op := operator(t)
	agent, err := AgentCaller(e.worker.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.s.SetPause(ctx, agent, "a-1", e.tm.ID, "", true, ""); !errors.Is(err, ErrForbidden) {
		t.Fatalf("an agent pauses: %v", err)
	}
	if _, err := e.s.Nudge(ctx, agent, "a-2", e.tm.ID, e.lead.ID, "x"); !errors.Is(err, ErrForbidden) {
		t.Fatalf("an agent nudges: %v", err)
	}
	if _, err := e.s.GetTeamState(ctx, agent, e.tm.ID); !errors.Is(err, ErrForbidden) {
		t.Fatalf("an agent reads the team state: %v", err)
	}
	for name, err := range map[string]error{
		"unknown team":        second(e.s.SetPause(ctx, op, "x-1", "no-such-team", "", true, "")),
		"non-member pause":    second(e.s.SetPause(ctx, op, "x-2", e.tm.ID, "no-such-agent", true, "")),
		"non-member nudge":    second(e.s.Nudge(ctx, op, "x-3", e.tm.ID, "no-such-agent", "x")),
		"unknown team state":  second(e.s.GetTeamState(ctx, op, "no-such-team")),
		"nudge to a non-team": second(e.s.Nudge(ctx, op, "x-4", "no-such-team", e.worker.ID, "x")),
	} {
		if !errors.Is(err, ErrNotFound) {
			t.Errorf("%s: %v, want not found", name, err)
		}
	}
	for name, err := range map[string]error{
		"a resume with a reason": second(e.s.SetPause(ctx, op, "y-1", e.tm.ID, "", false, "why")),
		"no team":                second(e.s.SetPause(ctx, op, "y-2", "", "", true, "")),
		"a long reason":          second(e.s.SetPause(ctx, op, "y-3", e.tm.ID, "", true, strings.Repeat("r", maxPauseReason+1))),
		"an empty note":          second(e.s.Nudge(ctx, op, "y-4", e.tm.ID, e.worker.ID, " ")),
		"a long note":            second(e.s.Nudge(ctx, op, "y-5", e.tm.ID, e.worker.ID, strings.Repeat("n", maxNudgeText+1))),
		"no member to nudge":     second(e.s.Nudge(ctx, op, "y-6", e.tm.ID, "", "x")),
	} {
		if !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: %v, want invalid", name, err)
		}
	}
	if p := pendingKinds(t, e.s, e.workerTok); len(p) != 0 {
		t.Fatalf("a refused call wrote a message: %+v", p)
	}
}

func second[T any](_ T, err error) error { return err }

// A nudge is exactly one message, to the named member alone, carrying the
// operator's note; a retry with the same key writes nothing more.
func TestNudgeWritesOneMessage(t *testing.T) {
	ctx := context.Background()
	e := newEscTeam(t)
	op := operator(t)
	m, err := e.s.Nudge(ctx, op, "n-1", e.tm.ID, e.worker.ID, "aicrew-agent 0.5.5 is installed: restart with run")
	if err != nil || m.ID == "" || m.Kind != KindLifecycle || m.To != e.worker.ID || m.Operator == nil {
		t.Fatalf("nudge: %+v %v", m, err)
	}
	again, err := e.s.Nudge(ctx, op, "n-1", e.tm.ID, e.worker.ID, "aicrew-agent 0.5.5 is installed: restart with run")
	if err != nil || again.ID != m.ID {
		t.Fatalf("a retry: %+v %v", again, err)
	}
	if lead := pendingKinds(t, e.s, e.leadTok); len(lead) != 0 {
		t.Fatalf("the coordinator received the worker's nudge: %+v", lead)
	}
	items, err := e.s.ReadInboxWithToken(ctx, e.workerTok, 10)
	if err != nil || len(items) != 1 {
		t.Fatalf("the worker's inbox: %+v %v", items, err)
	}
	got := items[0]
	if got.ID != m.ID || got.Operator == nil || got.Operator.Note != "aicrew-agent 0.5.5 is installed: restart with run" ||
		got.Operator.By != "operator:op-1" || got.Text != "A note from the operator: aicrew-agent 0.5.5 is installed: restart with run" ||
		got.Escalation != nil || got.Board != nil || got.Offer != nil {
		t.Fatalf("the nudge as delivered: %+v", got)
	}
}

// A pause leaves attempts and sessions as they were, and the team state
// shows each member's role, session, open attempt, unacknowledged messages
// and pause, and the team's.
func TestPauseLeavesWorkAndTeamState(t *testing.T) {
	ctx := context.Background()
	s, _ := openTemp(t)
	e := newExecTeam(t, s)
	op := operator(t)
	a := e.accept(t, "accept-1", e.offer(t, "offer-1", "task-1"))
	type snapshot struct {
		attempts, sessions string
	}
	snap := func() snapshot {
		var out snapshot
		if err := s.db.QueryRow(`SELECT group_concat(id || ':' || state || ':' || phase || ':' || worker_agent_id, ',') FROM attempts`).
			Scan(&out.attempts); err != nil {
			t.Fatal(err)
		}
		if err := s.db.QueryRow(`SELECT group_concat(id || ':' || state || ':' || generation || ':' || updated_at, ',') FROM sessions`).
			Scan(&out.sessions); err != nil {
			t.Fatal(err)
		}
		return out
	}
	before := snap()
	if _, err := s.SetPause(ctx, op, "p-1", e.tm.ID, "", true, "maintenance"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SetPause(ctx, op, "p-2", e.tm.ID, e.builder.agent.ID, true, ""); err != nil {
		t.Fatal(err)
	}
	if after := snap(); !reflect.DeepEqual(before, after) {
		t.Fatalf("the pause changed work:\nbefore %+v\nafter  %+v", before, after)
	}

	st, err := s.GetTeamState(ctx, op, e.tm.ID)
	if err != nil || st.TeamID != e.tm.ID || st.TeamName != e.tm.Name || st.Paused == nil || st.Paused.Reason != "maintenance" ||
		len(st.Members) != 2 {
		t.Fatalf("team state: %+v %v", st, err)
	}
	byID := map[string]MemberState{}
	for _, m := range st.Members {
		byID[m.AgentID] = m
	}
	lead, builder := byID[e.lead.agent.ID], byID[e.builder.agent.ID]
	if lead.Role != RoleCoordinator || lead.Session == nil || lead.Session.ID != e.lead.sess.ID || lead.OpenAttempt != nil ||
		lead.Paused != nil || lead.Label != e.lead.agent.Label {
		t.Fatalf("the coordinator's state: %+v", lead)
	}
	if builder.Role != RoleWorker || builder.Session == nil || builder.Paused == nil || builder.Paused.AgentID != e.builder.agent.ID ||
		builder.OpenAttempt == nil || builder.OpenAttempt.ID != a.ID || builder.OpenAttempt.State != a.State ||
		builder.OpenAttempt.Task != a.Task || builder.Unacknowledged < 1 {
		t.Fatalf("the worker's state: %+v", builder)
	}
	// The count is the inbox's: a nudge adds one, an acknowledgement takes
	// it away.
	if _, err := s.Nudge(ctx, op, "n-1", e.tm.ID, e.lead.agent.ID, "note"); err != nil {
		t.Fatal(err)
	}
	st2, err := s.GetTeamState(ctx, op, e.tm.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range st2.Members {
		if m.AgentID == e.lead.agent.ID && m.Unacknowledged != lead.Unacknowledged+1 {
			t.Fatalf("the coordinator's count after a nudge: %d, was %d", m.Unacknowledged, lead.Unacknowledged)
		}
	}
	items, err := s.ReadInbox(ctx, e.lead.caller, e.lead.sess.ID, e.lead.sess.Generation, 100)
	if err != nil {
		t.Fatal(err)
	}
	ids := make([]string, 0, len(items))
	for _, it := range items {
		ids = append(ids, it.ID)
	}
	if _, err := s.AckMessages(ctx, e.lead.caller, "ack-1", e.lead.sess.ID, e.lead.sess.Generation, ids); err != nil {
		t.Fatal(err)
	}
	st3, _ := s.GetTeamState(ctx, op, e.tm.ID)
	for _, m := range st3.Members {
		if m.AgentID == e.lead.agent.ID && m.Unacknowledged != 0 {
			t.Fatalf("the coordinator's count after acknowledging: %d", m.Unacknowledged)
		}
	}
}
