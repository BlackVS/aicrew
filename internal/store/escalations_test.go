package store

import (
	"context"
	"errors"
	"strings"
	"testing"
)

var escTask = TaskRef{HubID: "hub-test", ProjectID: "project-t", TaskID: "task-1"}

func escRequest() EscalationRequest {
	return EscalationRequest{Task: escTask, Category: "scope", Urgency: "today",
		Question:       "The task asks for a CLI flag the design does not name; add it or drop it?",
		Context:        "Acceptance criterion 2 names --dry-run; section 4 does not.",
		Options:        []EscalationOption{{"add it", "one more flag to document"}, {"drop it", "criterion 2 is amended"}},
		Recommendation: "add it: it is cheap and the criterion asks for it"}
}

// escTeam is a team granting project-t, with a coordinator and a worker in
// session through tokens.
type escTeam struct {
	s                  *Store
	tm                 Team
	lead, worker       Agent
	leadTok, workerTok string
	leadSess           string
}

func newEscTeam(t *testing.T) escTeam {
	t.Helper()
	s, _ := openTemp(t)
	clock(s)
	tm := mustTeam(t, s, "t1", "crew", ProjectRef{HubID: "hub-test", ProjectID: "project-t"})
	p := newProver(t, s)
	lead, _ := member(t, s, tm.ID, "lead", RoleCoordinator)
	worker, _ := member(t, s, tm.ID, "worker", RoleWorker)
	le, ls := p.enter("e-lead", lead, tm.ID)
	_, ws := p.enter("e-worker", worker, tm.ID)
	return escTeam{s: s, tm: tm, lead: lead, worker: worker, leadTok: ls.Token.Reveal(), workerTok: ws.Token.Reveal(),
		leadSess: le.Session.ID}
}

func pendingKinds(t *testing.T, s *Store, token string) []PendingMessage {
	t.Helper()
	p, err := s.PendingInboxWithToken(context.Background(), token)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// The coordinator raises; the operator lists, reads and answers once; the
// answer's message reaches the coordinator and the blocked member, carrying
// the decision.
func TestEscalationRaiseAndAnswer(t *testing.T) {
	ctx := context.Background()
	e := newEscTeam(t)
	in := escRequest()
	in.Blocked = e.worker.ID
	esc, err := e.s.RaiseEscalationWithToken(ctx, "raise-1", e.leadTok, in)
	if err != nil || esc.ID == "" || esc.TeamID != e.tm.ID || esc.Coordinator != e.lead.ID || esc.Answer != nil {
		t.Fatalf("raise: %+v %v", esc, err)
	}
	if again, err := e.s.RaiseEscalationWithToken(ctx, "raise-1", e.leadTok, in); err != nil || again.ID != esc.ID {
		t.Fatalf("a replay: %+v %v", again, err)
	}
	if _, err := e.s.RaiseEscalationWithToken(ctx, "raise-1", e.leadTok, escRequest()); !errors.Is(err, ErrIdempotencyConflict) {
		t.Fatalf("the same key with another request: %v", err)
	}
	op := operator(t)
	open, err := e.s.ListEscalations(ctx, op, "", true)
	if err != nil || len(open) != 1 || open[0].ID != esc.ID || open[0].Question != in.Question || len(open[0].Options) != 2 {
		t.Fatalf("open list: %+v %v", open, err)
	}
	if before := pendingKinds(t, e.s, e.leadTok); len(before) != 0 {
		t.Fatalf("the coordinator has messages before the answer: %+v", before)
	}

	ans := EscalationAnswer{Decision: "add it", Rationale: "criterion 2 stands; document the flag"}
	got, err := e.s.AnswerEscalation(ctx, op, "answer-1", esc.ID, ans)
	if err != nil || got.Answer == nil || got.Answer.Decision != "add it" || got.Answer.AnsweredBy != "operator:op-1" {
		t.Fatalf("answer: %+v %v", got, err)
	}
	if _, err := e.s.AnswerEscalation(ctx, op, "answer-2", esc.ID, EscalationAnswer{Decision: "drop it", Rationale: "x"}); !errors.Is(err, ErrEscalationAnswered) {
		t.Fatalf("a second answer: %v", err)
	}
	if again, err := e.s.AnswerEscalation(ctx, op, "answer-1", esc.ID, ans); err != nil || again.Answer.Decision != "add it" {
		t.Fatalf("a replay of the answer: %+v %v", again, err)
	}
	if open, _ := e.s.ListEscalations(ctx, op, e.tm.ID, true); len(open) != 0 {
		t.Fatalf("still open after the answer: %+v", open)
	}
	if all, _ := e.s.ListEscalations(ctx, op, e.tm.ID, false); len(all) != 1 || all[0].Answer == nil {
		t.Fatalf("the full list: %+v", all)
	}
	for who, tok := range map[string]string{"coordinator": e.leadTok, "worker": e.workerTok} {
		pend := pendingKinds(t, e.s, tok)
		if len(pend) != 1 || pend[0].Kind != KindLifecycle {
			t.Fatalf("%s pending: %+v", who, pend)
		}
		items, err := e.s.ReadInboxWithToken(ctx, tok, 10)
		if err != nil || len(items) != 1 {
			t.Fatalf("%s inbox: %+v %v", who, items, err)
		}
		m := items[0]
		if m.Escalation == nil || m.Escalation.ID != esc.ID || m.Escalation.Decision != "add it" ||
			m.Escalation.Rationale != ans.Rationale || m.Task == nil || *m.Task != escTask ||
			!strings.Contains(m.Text, esc.ID) || !strings.Contains(m.Text, "add it") {
			t.Fatalf("%s message: %+v", who, m)
		}
	}
}

// Only the team's current coordinator raises, on a granted project, naming
// its own team's attempt and an active member; the request is validated.
func TestEscalationRaiseRefusals(t *testing.T) {
	ctx := context.Background()
	e := newEscTeam(t)
	if _, err := e.s.RaiseEscalationWithToken(ctx, "w-1", e.workerTok, escRequest()); !errors.Is(err, ErrForbidden) {
		t.Fatalf("a worker raises: %v", err)
	}
	other := escRequest()
	other.Task.ProjectID = "not-granted"
	if _, err := e.s.RaiseEscalationWithToken(ctx, "g-1", e.leadTok, other); !errors.Is(err, ErrProjectNotGranted) {
		t.Fatalf("an ungranted project: %v", err)
	}
	for name, mutate := range map[string]func(*EscalationRequest){
		"category":       func(r *EscalationRequest) { r.Category = "whim" },
		"urgency":        func(r *EscalationRequest) { r.Urgency = "soon" },
		"no question":    func(r *EscalationRequest) { r.Question = "  " },
		"one option":     func(r *EscalationRequest) { r.Options = r.Options[:1] },
		"five options":   func(r *EscalationRequest) { r.Options = append(r.Options, r.Options[0], r.Options[0], r.Options[0]) },
		"no consequence": func(r *EscalationRequest) { r.Options[0].Consequence = "" },
		"no task":        func(r *EscalationRequest) { r.Task.TaskID = "" },
		"long question":  func(r *EscalationRequest) { r.Question = strings.Repeat("q", maxEscalationQuestion+1) },
		"bad utf-8":      func(r *EscalationRequest) { r.Context = "\xff" },
		"blocked self":   func(r *EscalationRequest) { r.Blocked = e.lead.ID },
		"blocked other":  func(r *EscalationRequest) { r.Blocked = "no-such-agent" },
		"foreign attempt": func(r *EscalationRequest) {
			r.AttemptID = "no-such-attempt"
		},
	} {
		r := escRequest()
		mutate(&r)
		if _, err := e.s.RaiseEscalationWithToken(ctx, "v-"+name, e.leadTok, r); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: %v, want invalid", name, err)
		}
	}
	if _, err := e.s.RaiseEscalationWithToken(ctx, "t-1", "acs_not_a_token", escRequest()); err == nil {
		t.Fatal("a wrong token raises")
	}
}

// An architect credential reads and answers escalations, and nothing else;
// a revoked or unknown one authenticates nothing.
func TestArchitectCredential(t *testing.T) {
	ctx := context.Background()
	e := newEscTeam(t)
	op := operator(t)
	cred, bearer, err := e.s.IssueArchitectCredential(ctx, op, "issue-1", "planning")
	if err != nil || !architectShape.MatchString(bearer) || cred.Label != "planning" || !cred.Active(e.s.now()) {
		t.Fatalf("issue: %+v %v", cred, err)
	}
	if _, _, err := e.s.IssueArchitectCredential(ctx, op, "issue-2", "Not A Label"); !errors.Is(err, ErrInvalid) {
		t.Fatalf("a bad label: %v", err)
	}
	arch, err := e.s.AuthenticateArchitect(ctx, bearer)
	if err != nil || arch.String() != "architect:"+cred.ID {
		t.Fatalf("authenticate: %v %v", arch, err)
	}
	esc, err := e.s.RaiseEscalationWithToken(ctx, "raise-1", e.leadTok, escRequest())
	if err != nil {
		t.Fatal(err)
	}
	if list, err := e.s.ListEscalations(ctx, arch, "", true); err != nil || len(list) != 1 {
		t.Fatalf("the architect lists: %+v %v", list, err)
	}
	if _, err := e.s.GetEscalation(ctx, arch, esc.ID); err != nil {
		t.Fatalf("the architect reads: %v", err)
	}
	got, err := e.s.AnswerEscalation(ctx, arch, "a-1", esc.ID, EscalationAnswer{Decision: "add it", Rationale: "as recommended"})
	if err != nil || got.Answer.AnsweredBy != "architect:"+cred.ID {
		t.Fatalf("the architect answers: %+v %v", got, err)
	}
	// Nothing else: credentials, invitations, teams.
	if _, _, err := e.s.IssueArchitectCredential(ctx, arch, "i-3", "more"); !errors.Is(err, ErrForbidden) {
		t.Fatalf("the architect issues an architect credential: %v", err)
	}
	if _, err := e.s.ListArchitectCredentials(ctx, arch); !errors.Is(err, ErrForbidden) {
		t.Fatalf("the architect lists credentials: %v", err)
	}
	if _, _, err := e.s.IssueIntrospectionCredential(ctx, arch, "h-1", "hub-test"); !errors.Is(err, ErrForbidden) {
		t.Fatalf("the architect issues a hub credential: %v", err)
	}
	if _, err := e.s.CreateTeam(ctx, arch, "team-x", NewTeam{Name: "other"}); !errors.Is(err, ErrForbidden) {
		t.Fatalf("the architect creates a team: %v", err)
	}
	// A member is not an escalation reader.
	lead, _ := AgentCaller(e.lead.ID)
	if _, err := e.s.ListEscalations(ctx, lead, "", false); !errors.Is(err, ErrForbidden) {
		t.Fatalf("a member lists escalations: %v", err)
	}

	if _, err := e.s.RevokeArchitectCredential(ctx, op, "revoke-1", cred.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := e.s.AuthenticateArchitect(ctx, bearer); !errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("a revoked credential: %v", err)
	}
	for _, b := range []string{"", "aar_", "aar_" + strings.Repeat("0", 64), "aop_" + strings.Repeat("0", 64)} {
		if _, err := e.s.AuthenticateArchitect(ctx, b); !errors.Is(err, ErrUnauthenticated) {
			t.Fatalf("bearer %q: %v", b, err)
		}
	}
	list, err := e.s.ListArchitectCredentials(ctx, op)
	if err != nil || len(list) != 1 || list[0].Active(e.s.now()) {
		t.Fatalf("list after revoke: %+v %v", list, err)
	}
}

// The answer reaches the team's coordinators as they are when it is
// given, not only the one that raised the request: a coordinator that
// joined since gets it too.
func TestEscalationAnswerReachesTheTeamsCoordinator(t *testing.T) {
	ctx := context.Background()
	e := newEscTeam(t)
	esc, err := e.s.RaiseEscalationWithToken(ctx, "raise-1", e.leadTok, escRequest())
	if err != nil {
		t.Fatal(err)
	}
	lead2, _ := member(t, e.s, e.tm.ID, "lead2", RoleCoordinator)
	if _, err := e.s.AnswerEscalation(ctx, operator(t), "a-1", esc.ID, EscalationAnswer{Decision: "add it", Rationale: "r"}); err != nil {
		t.Fatal(err)
	}
	var to []string
	if err := func() error {
		rows, err := e.s.db.Query(`SELECT r.agent_id FROM message_recipients r JOIN messages m ON m.id = r.message_id
			WHERE m.escalation != '' ORDER BY r.agent_id`)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var id string
			rows.Scan(&id)
			to = append(to, id)
		}
		return rows.Err()
	}(); err != nil {
		t.Fatal(err)
	}
	want := []string{e.lead.ID, lead2.ID}
	if len(to) != 2 || !(to[0] == want[0] && to[1] == want[1] || to[0] == want[1] && to[1] == want[0]) {
		t.Fatalf("recipients %v, want the coordinators %v and nobody else", to, want)
	}
}
