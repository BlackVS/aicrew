package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

// crewMember is a linked team member with an active session.
type crewMember struct {
	agent  Agent
	caller Caller
	sess   Session
}

func joinCrew(t *testing.T, s *Store, teamID, label string, role Role) crewMember {
	t.Helper()
	a, c := member(t, s, teamID, label, role)
	sess, err := s.StartSession(context.Background(), c, "start-"+label, teamID)
	if err != nil {
		t.Fatalf("start session %s: %v", label, err)
	}
	return crewMember{agent: a, caller: c, sess: sess}
}

// crew is a team with three members in session: a coordinator and two
// workers.
func crew(t *testing.T, s *Store) (Team, crewMember, crewMember, crewMember) {
	t.Helper()
	tm := mustTeam(t, s, "t1", "crew", ProjectRef{HubID: "hub-a", ProjectID: "docs"})
	return tm, joinCrew(t, s, tm.ID, "lead", RoleCoordinator),
		joinCrew(t, s, tm.ID, "builder", RoleWorker), joinCrew(t, s, tm.ID, "tester", RoleWorker)
}

func send(t *testing.T, s *Store, from crewMember, key string, in NewMessage) Message {
	t.Helper()
	in.SessionID, in.Generation = from.sess.ID, from.sess.Generation
	m, err := s.SendMessage(context.Background(), from.caller, key, in)
	if err != nil {
		t.Fatalf("send %s: %v", key, err)
	}
	return m
}

func read(t *testing.T, s *Store, m crewMember, limit int) []InboxItem {
	t.Helper()
	items, err := s.ReadInbox(context.Background(), m.caller, m.sess.ID, m.sess.Generation, limit)
	if err != nil {
		t.Fatalf("read inbox: %v", err)
	}
	return items
}

func ack(t *testing.T, s *Store, m crewMember, key string, ids ...string) AckResult {
	t.Helper()
	res, err := s.AckMessages(context.Background(), m.caller, key, m.sess.ID, m.sess.Generation, ids)
	if err != nil {
		t.Fatalf("acknowledge %v: %v", ids, err)
	}
	return res
}

func seqs(items []InboxItem) []int64 {
	out := []int64{}
	for _, it := range items {
		out = append(out, it.Seq)
	}
	return out
}

func ids(items []InboxItem) []string {
	out := []string{}
	for _, it := range items {
		out = append(out, it.ID)
	}
	return out
}

func sentence(i int) string {
	return fmt.Sprintf("Status update %d: the build finished and the tests passed.", i)
}

// A message delivered but not acknowledged is delivered again after the
// store restarts and the client resumes its session.
func TestRestartRedeliversUnacknowledged(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "aicrew.db")
	s := mustOpen(t, path)
	_, lead, builder, _ := crew(t, s)
	for i := 1; i <= 3; i++ {
		send(t, s, lead, fmt.Sprintf("m%d", i), NewMessage{Text: sentence(i)})
	}
	first := read(t, s, builder, 2)
	if got := seqs(first); !reflect.DeepEqual(got, []int64{1, 2}) {
		t.Fatalf("first read = %v, want [1 2]", got)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	s = mustOpen(t, path)
	defer s.Close()
	resumed, err := s.ResumeSession(ctx, builder.caller, "resume", builder.sess.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.ReadInbox(ctx, builder.caller, builder.sess.ID, builder.sess.Generation, 10); !errors.Is(err, ErrContextStale) {
		t.Fatalf("read at the old generation: got %v, want ErrContextStale", err)
	}
	builder.sess = resumed
	again := read(t, s, builder, 10)
	if got := seqs(again); !reflect.DeepEqual(got, []int64{1, 2, 3}) {
		t.Fatalf("read after restart = %v, want [1 2 3]", got)
	}
	for i, want := range []int64{2, 2, 1} {
		if again[i].Deliveries != want {
			t.Errorf("message %d delivered %d times, want %d", again[i].Seq, again[i].Deliveries, want)
		}
	}
	if !again[0].FirstDeliveredAt.Equal(first[0].FirstDeliveredAt) {
		t.Errorf("first delivery moved from %v to %v", first[0].FirstDeliveredAt, again[0].FirstDeliveredAt)
	}
	ack(t, s, builder, "a1", ids(again)...)
	if rest := read(t, s, builder, 10); len(rest) != 0 {
		t.Fatalf("read after acknowledging everything = %v", seqs(rest))
	}
}

// Neither acknowledgement order nor a small page can move a read past an
// unacknowledged message, and only delivered messages can be acknowledged.
func TestReadsNeverSkipUnacknowledged(t *testing.T) {
	ctx := context.Background()
	s, _ := openTemp(t)
	_, lead, builder, tester := crew(t, s)
	for i := 1; i <= 5; i++ {
		send(t, s, lead, fmt.Sprintf("m%d", i), NewMessage{Text: sentence(i)})
	}
	all := read(t, s, builder, 5)

	ack(t, s, builder, "a3", all[2].ID)
	if got := seqs(read(t, s, builder, 2)); !reflect.DeepEqual(got, []int64{1, 2}) {
		t.Fatalf("after acknowledging 3 only: %v, want [1 2]", got)
	}
	if got := seqs(read(t, s, builder, 2)); !reflect.DeepEqual(got, []int64{1, 2}) {
		t.Fatalf("a small page without acknowledgement moved on: %v, want [1 2]", got)
	}
	ack(t, s, builder, "a1", all[0].ID)
	if got := seqs(read(t, s, builder, 2)); !reflect.DeepEqual(got, []int64{2, 4}) {
		t.Fatalf("after acknowledging 1: %v, want [2 4]", got)
	}

	// Acknowledging twice is a no-op that reports the message.
	if res := ack(t, s, builder, "a1-again", all[0].ID); len(res.Acknowledged) != 0 || !reflect.DeepEqual(res.Already, []string{all[0].ID}) {
		t.Fatalf("second acknowledgement = %+v", res)
	}

	// A message never read cannot be acknowledged, and the refusal records
	// nothing for the other IDs in the same request.
	undelivered := send(t, s, lead, "m6", NewMessage{Text: sentence(6)})
	if _, err := s.AckMessages(ctx, builder.caller, "a-mixed", builder.sess.ID, builder.sess.Generation,
		[]string{all[1].ID, undelivered.ID}); !errors.Is(err, ErrNotDelivered) {
		t.Fatalf("acknowledging an undelivered message: got %v, want ErrNotDelivered", err)
	}
	if got := seqs(read(t, s, builder, 1)); !reflect.DeepEqual(got, []int64{2}) {
		t.Fatalf("a refused acknowledgement changed the inbox: %v, want [2]", got)
	}
	// Nor can a message addressed to someone else, or an unknown one.
	private := send(t, s, lead, "m7", NewMessage{To: tester.agent.ID, Text: "Please review the release notes before noon."})
	read(t, s, tester, 10)
	for _, id := range []string{private.ID, "no-such-message"} {
		if _, err := s.AckMessages(ctx, builder.caller, "a-"+id, builder.sess.ID, builder.sess.Generation, []string{id}); !errors.Is(err, ErrNotDelivered) {
			t.Errorf("acknowledging %s: got %v, want ErrNotDelivered", id, err)
		}
	}
}

// A stale, ended, removed or re-roled member can neither send, read nor
// acknowledge, and a refused call writes nothing.
func TestStaleOrRemovedMembersRefused(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name   string
		change func(t *testing.T, s *Store, tm Team, m crewMember)
	}{
		{"stale generation", func(t *testing.T, s *Store, _ Team, m crewMember) {
			if _, err := s.ResumeSession(ctx, m.caller, "resume", m.sess.ID); err != nil {
				t.Fatal(err)
			}
		}},
		{"left session", func(t *testing.T, s *Store, _ Team, m crewMember) {
			if _, err := s.LeaveSession(ctx, m.caller, "leave", m.sess.ID, m.sess.Generation); err != nil {
				t.Fatal(err)
			}
		}},
		{"removed member", func(t *testing.T, s *Store, tm Team, m crewMember) {
			ms, err := getMembership(ctx, s.rdb, tm.ID, m.agent.ID)
			if err != nil {
				t.Fatal(err)
			}
			if err := s.RemoveMember(ctx, operator(t), "remove", tm.ID, m.agent.ID, ms.Revision); err != nil {
				t.Fatal(err)
			}
		}},
		{"role changed", func(t *testing.T, s *Store, tm Team, m crewMember) {
			ms, err := getMembership(ctx, s.rdb, tm.ID, m.agent.ID)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := s.SetMemberRole(ctx, operator(t), "role", tm.ID, m.agent.ID, ms.Revision, RoleIndependent); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, _ := openTemp(t)
			tm, lead, builder, _ := crew(t, s)
			delivered := send(t, s, lead, "m1", NewMessage{Text: sentence(1)})
			read(t, s, builder, 10)
			tc.change(t, s, tm, builder)

			messages, recipients := count(t, s, "messages"), count(t, s, "message_recipients WHERE deliveries > 0 OR acknowledged_at IS NOT NULL")
			_, err := s.SendMessage(ctx, builder.caller, "m2", NewMessage{
				SessionID: builder.sess.ID, Generation: builder.sess.Generation, Text: sentence(2)})
			if !errors.Is(err, ErrContextStale) {
				t.Errorf("send: got %v, want ErrContextStale", err)
			}
			if _, err := s.ReadInbox(ctx, builder.caller, builder.sess.ID, builder.sess.Generation, 10); !errors.Is(err, ErrContextStale) {
				t.Errorf("read: got %v, want ErrContextStale", err)
			}
			if _, err := s.AckMessages(ctx, builder.caller, "a1", builder.sess.ID, builder.sess.Generation, []string{delivered.ID}); !errors.Is(err, ErrContextStale) {
				t.Errorf("acknowledge: got %v, want ErrContextStale", err)
			}
			if count(t, s, "messages") != messages ||
				count(t, s, "message_recipients WHERE deliveries > 0 OR acknowledged_at IS NOT NULL") != recipients ||
				count(t, s, "message_recipients WHERE deliveries > 1") != 0 {
				t.Error("a refused call changed the inbox")
			}
		})
	}
}

// A retried send returns the original message only while the sender's
// session is still at the generation it was sent from.
func TestSendRetryNeedsTheSameGeneration(t *testing.T) {
	ctx := context.Background()
	s, _ := openTemp(t)
	_, lead, _, _ := crew(t, s)
	in := NewMessage{SessionID: lead.sess.ID, Generation: lead.sess.Generation, Text: sentence(1)}
	first, err := s.SendMessage(ctx, lead.caller, "m1", in)
	if err != nil {
		t.Fatal(err)
	}
	if again, err := s.SendMessage(ctx, lead.caller, "m1", in); err != nil || !reflect.DeepEqual(again, first) {
		t.Fatalf("retry = %+v, %v; want the original", again, err)
	}
	changed := in
	changed.Text = "This is a different message under the same key."
	if _, err := s.SendMessage(ctx, lead.caller, "m1", changed); !errors.Is(err, ErrIdempotencyConflict) {
		t.Fatalf("changed input: got %v, want ErrIdempotencyConflict", err)
	}
	if _, err := s.ResumeSession(ctx, lead.caller, "resume", lead.sess.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SendMessage(ctx, lead.caller, "m1", in); !errors.Is(err, ErrContextStale) {
		t.Fatalf("retry after the generation changed: got %v, want ErrContextStale", err)
	}
	if n := count(t, s, "messages"); n != 1 {
		t.Fatalf("messages = %d, want 1", n)
	}
}

// Concurrent sends get distinct, gapless sequence numbers; concurrent retries
// of one send create one message; concurrent readers that acknowledge what
// they read lose nothing and acknowledge nothing twice.
func TestConcurrentSendsAndAcknowledgements(t *testing.T) {
	ctx := context.Background()
	s, _ := openTemp(t)
	_, lead, builder, tester := crew(t, s)
	const n = 12
	var wg sync.WaitGroup
	sent := make([]Message, 2*n)
	errs := make([]error, 2*n)
	for i := range 2 * n {
		from := lead
		if i%2 == 1 {
			from = tester
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			sent[i], errs[i] = s.SendMessage(ctx, from.caller, fmt.Sprintf("m%d", i), NewMessage{
				SessionID: from.sess.ID, Generation: from.sess.Generation, To: builder.agent.ID, Text: sentence(i)})
		}()
	}
	wg.Wait()
	got := map[int64]bool{}
	for i, err := range errs {
		if err != nil {
			t.Fatalf("send %d: %v", i, err)
		}
		got[sent[i].Seq] = true
	}
	for seq := int64(1); seq <= 2*n; seq++ {
		if !got[seq] {
			t.Fatalf("sequence numbers %v are not exactly 1..%d", got, 2*n)
		}
	}

	same := make([]Message, n)
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			same[i], errs[i] = s.SendMessage(ctx, lead.caller, "retried", NewMessage{
				SessionID: lead.sess.ID, Generation: lead.sess.Generation, To: builder.agent.ID,
				Text: "The deployment window moved to Thursday afternoon."})
		}()
	}
	wg.Wait()
	for i := range n {
		if errs[i] != nil || !reflect.DeepEqual(same[i], same[0]) {
			t.Fatalf("retried send %d = %+v, %v; want %+v", i, same[i], errs[i], same[0])
		}
	}
	if total := count(t, s, "messages"); total != 2*n+1 {
		t.Fatalf("messages = %d, want %d", total, 2*n+1)
	}

	// Two readers share the builder's session and acknowledge what they read.
	var mu sync.Mutex
	acked := map[string]int{}
	for r := range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for round := 0; ; round++ {
				items, err := s.ReadInbox(ctx, builder.caller, builder.sess.ID, builder.sess.Generation, 3)
				if err != nil {
					t.Errorf("reader %d: %v", r, err)
					return
				}
				if len(items) == 0 {
					return
				}
				res, err := s.AckMessages(ctx, builder.caller, fmt.Sprintf("r%d-%d", r, round),
					builder.sess.ID, builder.sess.Generation, ids(items))
				if err != nil {
					t.Errorf("reader %d: %v", r, err)
					return
				}
				mu.Lock()
				for _, id := range res.Acknowledged {
					acked[id]++
				}
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if len(acked) != 2*n+1 {
		t.Fatalf("%d messages acknowledged, want %d", len(acked), 2*n+1)
	}
	for id, times := range acked {
		if times != 1 {
			t.Errorf("message %s recorded as acknowledged %d times", id, times)
		}
	}
	if rest := read(t, s, builder, 10); len(rest) != 0 {
		t.Fatalf("messages left after concurrent acknowledgement: %v", seqs(rest))
	}
}

func TestRecipientsAndVisibility(t *testing.T) {
	ctx := context.Background()
	s, _ := openTemp(t)
	tm, lead, builder, tester := crew(t, s)

	direct := send(t, s, lead, "direct", NewMessage{To: builder.agent.ID, Text: "Please take the parser review next."})
	team := send(t, s, lead, "team", NewMessage{Text: "The team meeting starts at ten."})
	if got := ids(read(t, s, builder, 10)); !reflect.DeepEqual(got, []string{direct.ID, team.ID}) {
		t.Errorf("builder reads %v, want the direct and the team message", got)
	}
	if got := ids(read(t, s, tester, 10)); !reflect.DeepEqual(got, []string{team.ID}) {
		t.Errorf("tester reads %v, want only the team message", got)
	}
	if got := read(t, s, lead, 10); len(got) != 0 {
		t.Errorf("the sender reads its own messages: %v", seqs(got))
	}
	late := joinCrew(t, s, tm.ID, "reviewer", RoleWorker)
	if got := read(t, s, late, 10); len(got) != 0 {
		t.Errorf("a later member reads earlier messages: %v", seqs(got))
	}

	for name, in := range map[string]NewMessage{
		"to self":         {To: lead.agent.ID, Text: sentence(1)},
		"to a non-member": {To: "agent-unknown", Text: sentence(1)},
		"foreign project": {Project: &ProjectRef{HubID: "hub-a", ProjectID: "other"}, Text: sentence(1)},
		"blank text":      {Text: " \n\t "},
		"oversized text":  {Text: strings.Repeat("A long sentence. ", maxMessageText)},
		"invalid UTF-8":   {Text: "The build \xff failed."},
		"partial task":    {Task: &TaskRef{HubID: "hub-a"}, Text: sentence(1)},
	} {
		in.SessionID, in.Generation = lead.sess.ID, lead.sess.Generation
		if _, err := s.SendMessage(ctx, lead.caller, "bad-"+name, in); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: got %v, want ErrInvalid", name, err)
		}
	}

	// A project-scoped message disappears from reads while its project is
	// out of the team's set, and comes back with it.
	docs := ProjectRef{HubID: "hub-a", ProjectID: "docs"}
	scoped := send(t, s, lead, "scoped", NewMessage{Project: &docs, Text: "The documentation freeze begins tomorrow."})
	if got := ids(read(t, s, tester, 10)); !reflect.DeepEqual(got, []string{team.ID, scoped.ID}) {
		t.Fatalf("tester reads %v before the project leaves", got)
	}
	cur, err := s.GetTeam(ctx, tm.ID)
	if err != nil {
		t.Fatal(err)
	}
	cur, err = s.SetTeamProjects(ctx, operator(t), "projects-1", tm.ID, cur.Revision, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := ids(read(t, s, tester, 10)); !reflect.DeepEqual(got, []string{team.ID}) {
		t.Errorf("tester reads %v after the project left, want the team message only", got)
	}
	if _, err := s.SetTeamProjects(ctx, operator(t), "projects-2", tm.ID, cur.Revision, []ProjectRef{docs}); err != nil {
		t.Fatal(err)
	}
	if got := ids(read(t, s, tester, 10)); !reflect.DeepEqual(got, []string{team.ID, scoped.ID}) {
		t.Errorf("tester reads %v after the project returned", got)
	}

	// A member removed and added again still has its unacknowledged messages.
	ms, err := getMembership(ctx, s.rdb, tm.ID, tester.agent.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.RemoveMember(ctx, operator(t), "remove-tester", tm.ID, tester.agent.ID, ms.Revision); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SendMessage(ctx, lead.caller, "to-removed", NewMessage{SessionID: lead.sess.ID,
		Generation: lead.sess.Generation, To: tester.agent.ID, Text: sentence(9)}); !errors.Is(err, ErrInvalid) {
		t.Errorf("message to a removed member: got %v, want ErrInvalid", err)
	}
	// A team-wide message sent while the tester is out never reaches it.
	send(t, s, lead, "while-removed", NewMessage{Text: "The tester is away, so the builder covers reviews today."})
	if _, err := s.AddMember(ctx, operator(t), "readd-tester", tm.ID, tester.agent.ID, RoleWorker); err != nil {
		t.Fatal(err)
	}
	tester.sess, err = s.StartSession(ctx, tester.caller, "restart-tester", tm.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got := ids(read(t, s, tester, 10)); !reflect.DeepEqual(got, []string{team.ID, scoped.ID}) {
		t.Errorf("re-added tester reads %v, want its unacknowledged messages and nothing sent while it was out", got)
	}
}

func TestSoleMemberHasNoRecipients(t *testing.T) {
	s, _ := openTemp(t)
	tm := mustTeam(t, s, "t1", "solo")
	lone := joinCrew(t, s, tm.ID, "lone", RoleIndependent)
	_, err := s.SendMessage(context.Background(), lone.caller, "m1", NewMessage{
		SessionID: lone.sess.ID, Generation: lone.sess.Generation, Text: sentence(1)})
	if !errors.Is(err, ErrInvalid) {
		t.Fatalf("got %v, want ErrInvalid", err)
	}
}

// A lifecycle message commits with the transition that announces it, and
// rolls back with it.
func TestLifecycleMessageSharesTheTransition(t *testing.T) {
	ctx := context.Background()
	s, _ := openTemp(t)
	tm, lead, builder, tester := crew(t, s)
	transition := func(key string) error {
		return s.run(ctx, lead.caller, command{
			op: "test.transition", scope: tm.ID, key: key, input: struct{ Key string }{key}, authorize: requireAgent,
			apply: func(ctx context.Context, tx *sql.Tx, now time.Time) (any, error) {
				return postLifecycle(ctx, tx, tm.ID, lead.agent.ID,
					"The coordinator offered the parser task to the builder.",
					&TaskRef{HubID: "hub-a", ProjectID: "docs", TaskID: "task-7"}, now)
			},
		}, nil)
	}
	s.beforeReceipt = func(string) error { return errors.New("the transition failed") }
	if err := transition("t1"); err == nil {
		t.Fatal("the failing transition committed")
	}
	if n := count(t, s, "messages"); n != 0 {
		t.Fatalf("a rolled-back transition left %d messages", n)
	}
	s.beforeReceipt = nil
	if err := transition("t2"); err != nil {
		t.Fatal(err)
	}
	for _, m := range []crewMember{builder, tester} {
		items := read(t, s, m, 10)
		if len(items) != 1 || items[0].Kind != KindLifecycle || items[0].Task == nil || items[0].Task.TaskID != "task-7" {
			t.Errorf("%s reads %+v, want one lifecycle message about task-7", m.agent.Label, items)
		}
	}
	if got := read(t, s, lead, 10); len(got) != 0 {
		t.Errorf("the actor reads its own lifecycle message")
	}
}

// The message keeps who sent it, from which session and generation, with
// the profile the sender reported then, and its text exactly as written.
func TestMessageRecordsSenderAndText(t *testing.T) {
	ctx := context.Background()
	s, _ := openTemp(t)
	tm, lead, builder, _ := crew(t, s)
	profile := Profile{Model: "model-a", Client: "client-b", ClientVersion: "1.2.3"}
	setProfile := func(key string, p Profile) {
		t.Helper()
		cur, err := s.GetAgent(ctx, lead.agent.ID)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s.SetAgentProfile(ctx, operator(t), key, lead.agent.ID, cur.Revision, p); err != nil {
			t.Fatal(err)
		}
	}
	setProfile("profile-1", profile)
	text := "Hello team.\n\nThe release candidate is ready for review. Please check the changelog, " +
		"then reply with any blocking issue before 16:00 UTC. Merci, and thank you."
	task := &TaskRef{HubID: "hub-a", ProjectID: "docs", TaskID: "task-42"}
	sent := send(t, s, lead, "m1", NewMessage{Task: task, Text: text})
	setProfile("profile-2", Profile{Model: "model-z"}) // later changes do not rewrite the message
	got := read(t, s, builder, 10)
	if len(got) != 1 {
		t.Fatalf("read %d messages, want 1", len(got))
	}
	m := got[0].Message
	if m.Text != text || m.SenderProfile != profile || m.SenderAgentID != lead.agent.ID ||
		m.SenderSessionID != lead.sess.ID || m.SenderGeneration != lead.sess.Generation ||
		m.TeamID != tm.ID || !reflect.DeepEqual(m.Task, task) || m.Kind != KindMessage {
		t.Fatalf("stored message = %+v, want the sender's snapshot and the exact text", m)
	}
	if !reflect.DeepEqual(m, sent) {
		t.Errorf("read %+v, sent %+v", m, sent)
	}
	ack(t, s, builder, "a1", m.ID)
	for op, want := range map[string]int{opSendMessage: 1, opAckMessages: 1} {
		if n := count(t, s, "audit WHERE operation = '"+op+"'"); n != want {
			t.Errorf("audit records for %s = %d, want %d", op, n, want)
		}
	}
	var callerID string
	if err := s.db.QueryRow(`SELECT caller_id FROM audit WHERE operation = ?`, opSendMessage).Scan(&callerID); err != nil || callerID != lead.agent.ID {
		t.Errorf("send audit caller = %q, %v", callerID, err)
	}
	// Reading is not a command and writes no audit record.
	if n := count(t, s, "audit WHERE operation LIKE 'inbox.read%'"); n != 0 {
		t.Errorf("reads wrote %d audit records", n)
	}
}
