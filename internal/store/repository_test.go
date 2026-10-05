package store

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
)

// An offer records its repository on the attempt, all four fields beside the
// base commit and branch, and they never change; an offer whose repository
// is not one is refused before anything is recorded.
func TestOfferRecordsItsRepository(t *testing.T) {
	s, _ := openTemp(t)
	e := newExecTeam(t, s)
	a := e.accept(t, "a1", e.offer(t, "o1", "task-1"))
	if a.Repository != testRepository || a.BaseCommit != "base-commit-1" || a.Branch != "work/task-1" {
		t.Fatalf("recorded = %+v %q %q", a.Repository, a.BaseCommit, a.Branch)
	}
	// A repository change on the hub changes nothing recorded.
	grant(t, s, e.tm.ID, projectA)
	if got, err := s.GetAttempt(context.Background(), a.ID); err != nil || got.Repository != testRepository {
		t.Fatalf("after another read = %+v, %v", got.Repository, err)
	}
	before := count(t, s, "attempts")
	for name, repo := range map[string]AttemptRepository{
		"no repository":  {},
		"unknown kind":   {Kind: "svn", URL: testRepository.URL, Access: "write", DefaultBranch: "main"},
		"plain http":     {Kind: "gitea", URL: "http://git.example.test/crew/a.git", Access: "write", DefaultBranch: "main"},
		"credentials":    {Kind: "gitea", URL: "https://user:secret@git.example.test/crew/a.git", Access: "write", DefaultBranch: "main"},
		"query":          {Kind: "gitea", URL: "https://git.example.test/crew/a.git?x=1", Access: "write", DefaultBranch: "main"},
		"access":         {Kind: "gitea", URL: testRepository.URL, Access: "admin", DefaultBranch: "main"},
		"default branch": {Kind: "gitea", URL: testRepository.URL, Access: "write"},
		"long url":       {Kind: "gitea", URL: "https://git.example.test/" + strings.Repeat("a", maxRepositoryURL), Access: "write", DefaultBranch: "main"},
	} {
		req := e.offerReq("task-2")
		req.Repository = repo
		if _, err := s.OfferTask(context.Background(), e.lead.caller, e.port, "bad-"+name, req); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: %v", name, err)
		}
	}
	if count(t, s, "attempts") != before {
		t.Fatal("a refused offer was recorded")
	}
}

// A team read blocks the team's open attempts on the hub whose project it no
// longer grants, or whose profile it shows disabled, and a read that grants
// the project again unblocks them; each change tells the team. A read of
// another hub, or the same answer again, changes nothing, and nothing is
// asked of aimem's reservations.
func TestBlockedFollowsTheHubsGrants(t *testing.T) {
	s, _ := openTemp(t)
	ctx := context.Background()
	e := newExecTeam(t, s)
	a := e.accept(t, "a1", e.offer(t, "o1", "task-1"))
	acks := 0
	ackAll := func(m crewMember, items []InboxItem) {
		t.Helper()
		acks++
		if _, err := s.AckMessages(ctx, m.caller, fmt.Sprintf("ack-%d", acks), m.sess.ID, m.sess.Generation, ids(items)); err != nil {
			t.Fatal(err)
		}
	}
	for _, m := range []crewMember{e.lead, e.builder} {
		if items := read(t, s, m, 50); len(items) > 0 { // the offer's and the acceptance's announcements
			ackAll(m, items)
		}
	}
	calls := len(e.port.callLog())
	blocked := func(want string) {
		t.Helper()
		got, err := s.GetAttempt(ctx, a.ID)
		if err != nil {
			t.Fatal(err)
		}
		switch {
		case want == "" && got.Blocked != nil:
			t.Fatalf("blocked %+v, want not blocked", got.Blocked)
		case want != "" && (got.Blocked == nil || got.Blocked.Reason != want || got.Blocked.Since.IsZero()):
			t.Fatalf("blocked %+v, want %s", got.Blocked, want)
		case got.State != a.State || got.ReservationID != a.ReservationID:
			t.Fatalf("the block changed the attempt: %+v", got)
		}
	}
	told := func(want string) {
		t.Helper()
		for _, m := range []crewMember{e.lead, e.builder} {
			items := read(t, s, m, 50)
			if len(items) != 1 || items[0].Kind != KindLifecycle || !strings.Contains(items[0].Text, want) ||
				items[0].Task == nil || items[0].Task.TaskID != "task-1" {
				t.Fatalf("%s was told %+v, want one %q", m.agent.Label, items, want)
			}
			ackAll(m, items)
		}
	}

	grant(t, s, e.tm.ID) // the hub revokes project-a
	blocked(BlockedGrantRevoked)
	told("is blocked: the hub no longer grants")
	grant(t, s, e.tm.ID)
	if items := read(t, s, e.lead, 50); len(items) != 0 {
		t.Fatalf("the same answer again told %+v", items)
	}

	other := TeamGrantsRead{HubID: "hub-b", State: GrantsDisabled, At: nextReadAt()}
	if _, err := s.RecordTeamGrants(ctx, ReconcilerCaller(), e.tm.ID, other); err != nil {
		t.Fatal(err)
	}
	blocked(BlockedGrantRevoked)

	grant(t, s, e.tm.ID, projectA)
	blocked("")
	told("no longer blocked")

	disabled := TeamGrantsRead{HubID: "hub-a", State: GrantsDisabled, At: nextReadAt()}
	if _, err := s.RecordTeamGrants(ctx, ReconcilerCaller(), e.tm.ID, disabled); err != nil {
		t.Fatal(err)
	}
	blocked(BlockedProfileDisabled)
	told("profile is disabled")

	if len(e.port.callLog()) != calls {
		t.Fatalf("a block asked aimem's reservations: %v", e.port.callLog()[calls:])
	}
}
