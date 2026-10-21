package agent

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Each turn kind names ROLES.md sections that exist, and the brief names
// the sections for the turn's kind.
func TestTurnKindSections(t *testing.T) {
	md := rolesMD()
	review := &struct {
		Subjects []actionSubject `json:"subjects"`
	}{Subjects: []actionSubject{{Kind: "pull", AttemptID: "att-1", Review: &briefReview{Author: "hands", Verdict: "BLOCKER"}}}}
	failed := &struct {
		Subjects []actionSubject `json:"subjects"`
	}{Subjects: []actionSubject{{Kind: "pull", AttemptID: "att-1", Failed: []briefCheck{{Name: "go-test"}}}}}
	working := &memberAttempt{ID: "att-1", State: "running", Phase: "working", Task: TaskRef{TaskID: "task-1"}}
	rework := &memberAttempt{ID: "att-1", State: "running", Phase: "rework", Task: TaskRef{TaskID: "task-1"}}
	for _, c := range []struct {
		name     string
		role     string
		attempt  *memberAttempt
		inbox    []inboxMessage
		kind     string
		sections []string
	}{
		{"a coordinator", "coordinator", nil, []inboxMessage{{ID: "m1", Kind: "action.needed", Action: review}}, kindCoordinator,
			[]string{sectionEveryMember, sectionWriting, sectionCoordinator}},
		{"a worker's offer", "worker", nil, []inboxMessage{{ID: "m1", Kind: "lifecycle"}}, kindWorkerOffer,
			[]string{sectionEveryMember, sectionWriting, sectionWorkerOffer}},
		{"a worker's attempt", "worker", working, []inboxMessage{{ID: "m1", Kind: "action.needed", Action: failed}},
			kindWorkerAttempt, []string{sectionEveryMember, sectionWriting, sectionWorkerAttempt}},
		{"a review at the attempt", "worker", working, []inboxMessage{{ID: "m1", Kind: "action.needed", Action: review}},
			kindWorkerReview, []string{sectionEveryMember, sectionWriting, sectionWorkerAttempt, sectionWorkerReview}},
		{"a rework decision", "worker", rework, []inboxMessage{{ID: "m1", Kind: "lifecycle"}}, kindWorkerReview,
			[]string{sectionEveryMember, sectionWriting, sectionWorkerAttempt, sectionWorkerReview}},
		{"an independent member's claim", "independent", nil, []inboxMessage{{ID: "m1", Kind: "message"}},
			kindIndependent, []string{sectionEveryMember, sectionWriting, sectionIndependent}},
		{"an independent member's attempt", "independent", working, []inboxMessage{{ID: "m1", Kind: "message"}},
			kindWorkerAttempt, []string{sectionEveryMember, sectionWriting, sectionIndependent, sectionWorkerAttempt}},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got := turnKindOf(c.role, c.attempt, c.inbox); got != c.kind {
				t.Fatalf("the kind: %s, want %s", got, c.kind)
			}
			got := kindSections(c.role, c.kind)
			if strings.Join(got, "|") != strings.Join(c.sections, "|") {
				t.Fatalf("the sections: %q, want %q", got, c.sections)
			}
			for _, s := range got {
				if strings.Count(md, "\n## "+s+"\n") != 1 {
					t.Errorf("ROLES.md has no single section %q", s)
				}
			}

			e := newLoopEnv(t)
			e.host.role, e.host.attempt, e.host.inbox = c.role, c.attempt, c.inbox
			for _, m := range c.inbox {
				e.host.pending = append(e.host.pending, msg(m.ID, m.Kind))
			}
			e.tick(t)
			if len(e.specs) != 1 {
				t.Fatal("no turn")
			}
			want := "this is a " + c.kind + " turn, whose sections are \"" + strings.Join(c.sections, "\", \"") + "\"."
			if !strings.Contains(e.specs[0].Prompt, want) {
				t.Fatalf("the brief does not name the sections %q:\n%s", want, e.specs[0].Prompt)
			}
		})
	}
}

// A coordinator's brief carries the team's open attempts and its whole
// handoff; a handoff over the bound is cut at its end with a line naming
// the cut, and the file in the home stays whole.
func TestCoordinatorBrief(t *testing.T) {
	e := newLoopEnv(t)
	e.host.role = "coordinator"
	e.host.pending = []PendingMessage{msg("m1", "action.needed")}
	e.host.inbox = []inboxMessage{{ID: "m1", Kind: "action.needed"}}
	e.host.open = OpenAttempts{Attempts: []OpenAttempt{{ID: "att-9", State: "running", Phase: "working",
		Task: TaskRef{HubID: "hub-a", ProjectID: "proj", TaskID: "task-9"}, WorkerLabel: "worker-1",
		NextAction: "Follow the running attempt."}}}
	if err := os.MkdirAll(filepath.Dir(handoffPath(e.home)), 0o700); err != nil {
		t.Fatal(err)
	}
	notes := "# Handoff\n\nOffered task-9 to worker-1; the token ghp_0123456789abcdef is not to be shown.\n"
	if err := os.WriteFile(handoffPath(e.home), []byte(notes), 0o600); err != nil {
		t.Fatal(err)
	}
	e.next = func(TurnSpec, *fakeHost) TurnResult {
		e.host.pending = nil
		return TurnResult{Outcome: TurnFinished}
	}
	e.tick(t)
	if len(e.specs) != 1 {
		t.Fatal("no turn")
	}
	p := e.specs[0].Prompt
	for _, want := range []string{
		"\n## The team\n\nThe team's open attempts as aicrewd lists them now:\n\n" + e.host.open.Attempts[0].Line() + "\n",
		"\n## The handoff\n\nYour `docs/HANDOFF.md` as this turn starts",
		"Offered task-9 to worker-1; the token [redacted] is not to be shown.",
	} {
		if !strings.Contains(p, want) {
			t.Fatalf("the coordinator's brief lacks %q:\n%s", want, p)
		}
	}
	if strings.Contains(p, "ghp_0123456789abcdef") || strings.Contains(p, "## The ledger") {
		t.Fatalf("the coordinator's brief:\n%s", p)
	}

	// Over the bound: the start is kept and the end cut, with a line.
	long := "# Handoff\n\n" + strings.Repeat("a line of the coordinator's notes\n", 400) + "THE END\n"
	if err := os.WriteFile(handoffPath(e.home), []byte(long), 0o600); err != nil {
		t.Fatal(err)
	}
	e.host.open, e.host.openErr = OpenAttempts{}, errors.New("aicrewd is not reached")
	e.host.pending = []PendingMessage{msg("m2", "message")}
	e.host.inbox = []inboxMessage{{ID: "m2", Kind: "message"}}
	e.tick(t)
	if len(e.specs) != 2 {
		t.Fatal("no second turn")
	}
	p = e.specs[1].Prompt
	if !strings.Contains(p, "# Handoff\n\na line of the coordinator's notes\n") || strings.Contains(p, "THE END") ||
		!strings.Contains(p, "bytes of `docs/HANDOFF.md` were cut at the brief's bound; the file in the home has them.)") ||
		!strings.Contains(p, "The team's open attempts could not be read for the brief (aicrewd is not reached): "+
			"run `aicrew-agent attempts`.") {
		t.Fatalf("the cut brief:\n%s", p)
	}
	start := strings.Index(p, "## The handoff")
	end := strings.Index(p, "(The last ")
	if start < 0 || end < start || end-start > maxCoordinatorHandoff+200 {
		t.Fatalf("the handoff part is %d bytes", end-start)
	}
	if got, _ := os.ReadFile(handoffPath(e.home)); string(got) != long {
		t.Fatal("the coordinator's handoff in the home changed")
	}

	// A crowded brief: the handoff takes only the room the other parts
	// leave, so the brief keeps its last part with the sections.
	e.host.openErr = nil
	for i := range 20 {
		e.host.open.Attempts = append(e.host.open.Attempts, OpenAttempt{ID: fmt.Sprintf("att-%d", i), State: "running",
			WorkerLabel: "worker-1", NextAction: strings.Repeat("Follow it. ", 80)})
	}
	e.host.pending = []PendingMessage{msg("m3", "message")}
	e.host.inbox = []inboxMessage{{ID: "m3", Kind: "message"}}
	e.tick(t)
	p = strings.TrimPrefix(e.specs[2].Prompt, "/crew-turn ")
	if len(p) > maxBriefBytes || !strings.Contains(p, "were cut at the brief's bound") ||
		!strings.Contains(p, "this is a coordinator turn, whose sections are") ||
		strings.Contains(p, "(The brief was cut at its bound") {
		t.Fatalf("the crowded brief (%d bytes):\n%s", len(p), p)
	}
}

// /crew-turn reads no managed doc in full, and it and ROLES.md carry the
// repetition rule; START.md puts the home's docs and the process before a
// repository's instruction files.
func TestLeanStartGuidance(t *testing.T) {
	var turn crewCommand
	for _, c := range crewCommands {
		if c.Name == "crew-turn" {
			turn = c
		}
	}
	flat := func(s string) string { return strings.Join(strings.Fields(s), " ") }
	body := flat(turn.Body)
	if strings.Contains(body, "docs/START.md") || !strings.Contains(body, "Read no managed doc in full") ||
		!strings.Contains(body, "the brief names the sections of `docs/ROLES.md` for this turn's kind") {
		t.Fatalf("/crew-turn's reading:\n%s", turn.Body)
	}
	if !strings.Contains(body, "A procedure you work out again is not automated in the turn") ||
		!strings.Contains(body, "`[automate]`") {
		t.Fatalf("/crew-turn lacks the repetition rule:\n%s", turn.Body)
	}
	md := rolesMD()
	every := md[strings.Index(md, "## "+sectionEveryMember):strings.Index(md, "## "+sectionWriting)]
	for _, want := range []string{"do not automate it in your turn", "a skill", "a repository tool", "a harness feature",
		"under Open questions in its handoff", "\"Automate\" part of its pull request's body", "headed `[automate]`",
		"the architect files the task"} {
		if !strings.Contains(flat(every), want) {
			t.Errorf("ROLES.md's member rules lack %q", want)
		}
	}
	start := flat(startMD)
	if !strings.Contains(start, "Where a repository's instruction files differ from this home's docs or the process, "+
		"the home's docs and the process come first.") {
		t.Fatalf("START.md lacks the precedence sentence:\n%s", startMD)
	}
}
