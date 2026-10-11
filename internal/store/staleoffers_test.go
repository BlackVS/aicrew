package store

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"
)

// staleNotice is a stale-offer message as the store holds it.
type staleNotice struct {
	text       string
	sender     string
	recipients []string
}

// staleNotices are the stale-offer messages about an attempt, oldest first.
func staleNotices(t *testing.T, s *Store, attemptID string) []staleNotice {
	t.Helper()
	rows, err := s.db.Query(`SELECT m.id, m.text, m.sender_agent_id FROM messages m
		WHERE m.attempt_id = ? AND m.kind = 'lifecycle' AND instr(m.text, ?) > 0 ORDER BY m.seq`,
		attemptID, staleOfferMark(attemptID))
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []staleNotice
	var ids []string
	for rows.Next() {
		var id string
		var n staleNotice
		if err := rows.Scan(&id, &n.text, &n.sender); err != nil {
			t.Fatal(err)
		}
		ids, out = append(ids, id), append(out, n)
	}
	for i, id := range ids {
		r, err := s.db.Query(`SELECT agent_id FROM message_recipients WHERE message_id = ? ORDER BY agent_id`, id)
		if err != nil {
			t.Fatal(err)
		}
		for r.Next() {
			var a string
			if err := r.Scan(&a); err != nil {
				t.Fatal(err)
			}
			out[i].recipients = append(out[i].recipients, a)
		}
		r.Close()
	}
	return out
}

// checkStaleNotice requires n to tell only the coordinator about attempt a,
// offered to worker, with the remedy.
func checkStaleNotice(t *testing.T, n staleNotice, e execTeam, a Attempt, why string) {
	t.Helper()
	if !slices.Equal(n.recipients, []string{e.lead.agent.ID}) {
		t.Errorf("the notice went to %v, want the coordinator %s only", n.recipients, e.lead.agent.ID)
	}
	for _, s := range []string{"attempt " + a.ID, taskName(a.Task), e.builder.agent.Label, why,
		"Withdraw it and offer the task again."} {
		if !strings.Contains(n.text, s) {
			t.Errorf("the notice %q lacks %q", n.text, s)
		}
	}
}

// A coordinator session that starts or resumes fences the offers made under
// an earlier generation: aicrewd tells the coordinator about each in the same
// transaction (pilot finding 2026-10-11). An accept refused offer_stale after
// that adds nothing; a closed offer is not announced again.
func TestStaleOfferNoticeWhenTheCoordinatorIsFenced(t *testing.T) {
	ctx := context.Background()
	s, _ := openTemp(t)
	e := newExecTeam(t, s)
	a := e.offer(t, "o1", "task-1")
	if n := staleNotices(t, s, a.ID); len(n) != 0 {
		t.Fatalf("a fresh offer has stale notices: %+v", n)
	}

	resumed, err := s.ResumeSession(ctx, e.lead.caller, "resume-lead", e.lead.sess.ID)
	if err != nil {
		t.Fatal(err)
	}
	e.lead.sess = resumed
	n := staleNotices(t, s, a.ID)
	if len(n) != 1 {
		t.Fatalf("after the coordinator resumed: %d stale notices, want 1", len(n))
	}
	checkStaleNotice(t, n[0], e, a, "made under an earlier coordinator session")
	items := read(t, s, e.lead, 10)
	if !slices.ContainsFunc(items, func(i InboxItem) bool { return i.Message.AttemptID == a.ID && i.Message.Text == n[0].text }) {
		t.Errorf("the coordinator's inbox lacks the notice: %+v", items)
	}

	for _, key := range []string{"a1", "a2"} {
		if _, err := s.AcceptOffer(ctx, e.builder.caller, e.port, key, a.ID, e.acceptReq()); !errors.Is(err, ErrOfferStale) {
			t.Fatalf("accept after the coordinator changed: got %v, want ErrOfferStale", err)
		}
	}
	if n := staleNotices(t, s, a.ID); len(n) != 1 {
		t.Fatalf("refused accepts after the notice: %d stale notices, want 1", len(n))
	}

	// A new coordinator session (after the operator stopped the old one)
	// fences it again: the offer is still outstanding, so it is announced
	// again. Once released, it is not.
	if _, err := s.StopSession(ctx, operator(t), "stop-lead", e.lead.sess.ID); err != nil {
		t.Fatal(err)
	}
	if e.lead.sess, err = s.StartSession(ctx, e.lead.caller, "start-lead-2", e.tm.ID); err != nil {
		t.Fatal(err)
	}
	if n := staleNotices(t, s, a.ID); len(n) != 2 {
		t.Fatalf("after a new coordinator session: %d stale notices, want 2", len(n))
	}
	if _, err := e.release(t, "r1", a); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ResumeSession(ctx, e.lead.caller, "resume-lead-2", e.lead.sess.ID); err != nil {
		t.Fatal(err)
	}
	if n := staleNotices(t, s, a.ID); len(n) != 2 {
		t.Fatalf("a released offer was announced again: %d stale notices", len(n))
	}
}

// A worker's accept refused offer_stale because the worker's own session
// changed tells the coordinator, once per attempt, on the operator route and
// the step route alike. The refusal stays a refusal.
func TestStaleOfferNoticeWhenAnAcceptIsRefused(t *testing.T) {
	for _, route := range []string{"operator", "step"} {
		t.Run(route, func(t *testing.T) {
			ctx := context.Background()
			s, _ := openTemp(t)
			e := newExecTeam(t, s)
			a := e.offer(t, "o1", "task-1")
			resumed, err := s.ResumeSession(ctx, e.builder.caller, "resume-builder", e.builder.sess.ID)
			if err != nil {
				t.Fatal(err)
			}
			e.builder.sess = resumed
			if n := staleNotices(t, s, a.ID); len(n) != 0 {
				t.Fatalf("a worker's resume told the coordinator already: %+v", n)
			}
			accept := func(key string) error {
				if route == "operator" {
					_, err := s.AcceptOffer(ctx, e.builder.caller, e.port, key, a.ID, e.acceptReq())
					return err
				}
				_, _, err := s.BeginAccept(ctx, e.builder.caller, key, a.ID, e.acceptReq())
				return err
			}
			for _, key := range []string{"a1", "a2"} {
				if err := accept(key); !errors.Is(err, ErrOfferStale) {
					t.Fatalf("accept after the worker's session changed: got %v, want ErrOfferStale", err)
				}
			}
			mustState(t, s, a.ID, AttemptOffered)
			n := staleNotices(t, s, a.ID)
			if len(n) != 1 {
				t.Fatalf("after two refused accepts: %d stale notices, want 1", len(n))
			}
			checkStaleNotice(t, n[0], e, a, "accept was refused offer_stale")
		})
	}
}

// The token route, as members drive it: the coordinator's resume tells it
// about its committed offer, and a worker whose own session changed is
// refused offer_stale with no second notice.
func TestStaleOfferNoticeOnTheTokenRoute(t *testing.T) {
	ctx := context.Background()
	e := newClaimStopEnv(t)
	wb, err := e.s.AuthenticateSessionToken(ctx, e.workerTok)
	if err != nil {
		t.Fatal(err)
	}
	in := OfferInput{WorkerAgentID: wb.AgentID, Task: TaskRef{HubID: "hub-test", ProjectID: "project-t", TaskID: "task-1"},
		ExpectedRevision: 3, BaseCommit: "base-1", Branch: "work/task-1", Repository: testRepository, Process: testPin,
		ExpiresAt: e.now.Add(time.Hour)}
	a, st, err := e.s.BeginOfferWithToken(ctx, "offer-1", e.leadTok, in)
	if err != nil {
		t.Fatal(err)
	}
	e.reader.commit(st.CoordinationProof, receiptFor(a, st, "res-1", "1", 4))
	if a, _, err = e.settle(t, e.leadTok, a, st, HintCommitted); err != nil || a.State != AttemptOffered {
		t.Fatalf("settle the offer: %+v %v", a, err)
	}

	worker := e.resume(t, e.workerTok)
	for _, key := range []string{"accept-1", "accept-2"} {
		if _, _, err := e.s.BeginAcceptWithToken(ctx, key, worker, a.ID, testPin.InstructionDigest); !errors.Is(err, ErrOfferStale) {
			t.Fatalf("accept from the worker's new session: got %v, want ErrOfferStale", err)
		}
	}
	n := staleNotices(t, e.s, a.ID)
	if len(n) != 1 || !slices.Equal(n[0].recipients, []string{e.lead}) ||
		!strings.Contains(n[0].text, "accept was refused offer_stale") {
		t.Fatalf("after the worker's refused accepts: %+v, want one notice to %s", n, e.lead)
	}

	e.resume(t, e.leadTok)
	n = staleNotices(t, e.s, a.ID)
	if len(n) != 2 || !slices.Equal(n[1].recipients, []string{e.lead}) ||
		!strings.Contains(n[1].text, "made under an earlier coordinator session") {
		t.Fatalf("after the coordinator resumed: %+v, want a second notice to %s", n, e.lead)
	}
}
