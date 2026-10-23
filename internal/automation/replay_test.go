package automation

import (
	"bytes"
	"context"
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"github.com/BlackVS/aicrew/internal/events"
)

// A replay runs the script once per run record that carries an event,
// pr() and attempt() answering the recorded facts, every other call
// logged and not performed; a script that fails is reported per event
// and the replay goes on. Step records and runs without an event are
// skipped, and a line that is not a record is the error.
func TestReplay(t *testing.T) {
	const script = `# aicrew-automation: {"host_api": 1}
def main(event):
    p = pr()
    a = attempt()
    if "forge.changed" in event.kinds:
        fail("no change handling at " + p.head)
    step("seen", "head %s of attempt %s, %d labels" % (p.head, a.id, len(p.labels)))
    r = run("git", ["status", "--short"])
    comment("a note on " + p.head)
    label(add = ["review-this"], remove = ["hands-reviewed"])
    u = update_base()
    if event.base_only:
        return None
    return turn("fix", "fix it: " + u.reason)
`
	type rec struct {
		Run     string `json:"run"`
		Script  string `json:"script"`
		Kind    string `json:"kind,omitempty"`
		Outcome string `json:"outcome,omitempty"`
		Event   *Event `json:"event,omitempty"`
		Facts   *Facts `json:"facts,omitempty"`
	}
	lines := []rec{
		{Run: "r1", Script: "s.star", Kind: "step"},
		{Run: "r1", Script: "s.star", Outcome: "handled", Event: &Event{Kinds: []string{PointAfterPush}, IDs: []string{"pull#7@0123456"}},
			Facts: &Facts{PR: &PR{Number: 7, Head: "0123456", Labels: []string{"hands-reviewed"}}, Attempt: &Attempt{ID: "att-1"}}},
		{Run: "r2", Script: "s.star", Outcome: "handled", Event: &Event{Kinds: []string{events.KindForgeChanged}, IDs: []string{"pull#7@0123456"},
			Change: &ForgeChange{LabelsAdded: []string{"review-this"}}}, Facts: &Facts{PR: &PR{Number: 7, Head: "0123456"}}},
		{Run: "r3", Script: "s.star", Outcome: "handled"},
		{Run: "r4", Script: "s.star", Outcome: "turn", Event: &Event{Kinds: []string{PointAfterPush}, IDs: []string{"pull#7@89abcde"}, BaseOnly: true}},
	}
	var log bytes.Buffer
	for _, l := range lines {
		b, _ := json.Marshal(l)
		log.Write(append(b, '\n'))
	}
	var out bytes.Buffer
	n, err := Replay(context.Background(), "s.star", []byte(script), &log, &out)
	if err != nil || n != 3 {
		t.Fatalf("replayed %d, %v\n%s", n, err, out.String())
	}
	want := []string{
		"event 1: after-push pull#7@0123456 (run r1 of s.star, recorded as handled)",
		"  step seen: head 0123456 of attempt att-1, 1 labels",
		"  run: git status --short",
		"  comment: a note on 0123456",
		"  label: add [review-this], remove [hands-reviewed]",
		"  update_base: not performed in a replay",
		"  -> turn fix: fix it: not performed in a replay",
		"event 2: forge.changed pull#7@0123456 (run r2 of s.star, recorded as handled)",
		"  attempt(): the run recorded no attempt; an empty one",
		"  -> failed: the script failed: fail: no change handling at 0123456",
		"event 3: after-push pull#7@89abcde (run r4 of s.star, recorded as turn)",
		"  a base-only update",
		"  pr(): the run recorded no pull request; an empty one",
		"  attempt(): the run recorded no attempt; an empty one",
		"  step seen: head of attempt , 0 labels",
		"  run: git status --short",
		"  comment: a note on",
		"  label: add [review-this], remove [hands-reviewed]",
		"  update_base: not performed in a replay",
		"  -> handled",
	}
	got := strings.Split(strings.TrimSpace(out.String()), "\n")
	for i, w := range want {
		if i >= len(got) || !strings.HasPrefix(got[i], w) {
			t.Fatalf("line %d: got %q, want %q\n%s", i+1, lineAt(got, i), w, out.String())
		}
	}
	if len(got) != len(want) {
		t.Fatalf("%d lines, want %d:\n%s", len(got), len(want), out.String())
	}

	// A line that is not a record is the error, after the events before it.
	out.Reset()
	n, err = Replay(context.Background(), "s.star", []byte(script), strings.NewReader(`{"run":"r1","script":"s.star","outcome":"handled","event":{"kinds":["after-push"],"ids":["x"]}}`+"\nnot json\n"), &out)
	if err == nil || n != 1 || !strings.Contains(err.Error(), "line 2 of the log") {
		t.Fatalf("replayed %d, %v", n, err)
	}
	// A script that does not load is reported per event, not returned.
	out.Reset()
	n, err = Replay(context.Background(), "s.star", []byte("def main(event):\n    return undefined_name\n"),
		strings.NewReader(`{"run":"r1","script":"s.star","outcome":"handled","event":{"kinds":["after-push"],"ids":["x"]}}`+"\n"), &out)
	if err != nil || n != 1 || !strings.Contains(out.String(), "  -> failed: the script is not valid") {
		t.Fatalf("replayed %d, %v\n%s", n, err, out.String())
	}
}

func lineAt(lines []string, i int) string {
	if i < len(lines) {
		return lines[i]
	}
	return ""
}

// The reads are recorded and replayed in order: a script that reads pr()
// twice around label() sees both answers and does what the run did. A read
// past the recorded ones answers the last again and says so, reads the
// replay did not make are listed, and a record of the single-answer shape
// of 2f answers its one answer to every read, as it did then.
func TestReplayOrderedReads(t *testing.T) {
	const script = `# aicrew-automation: {"host_api": 1}
def main(event):
    p = pr()
    if "hands-reviewed" in p.labels:
        label(add = ["review-this"], remove = ["hands-reviewed"])
    p = pr()
    step("after", "%s %s" % (p.head, ",".join(p.labels)))
    attempt()
    attempt()
    return None
`
	before := PR{Number: 7, Head: "h1", Labels: []string{"hands-reviewed"}}
	after := PR{Number: 7, Head: "h1", Labels: []string{"review-this"}}
	ev := &Event{Kinds: []string{PointAfterPush}, IDs: []string{"pull#7@h1"}}
	type rec struct {
		Run     string `json:"run"`
		Script  string `json:"script"`
		Outcome string `json:"outcome"`
		Event   *Event `json:"event"`
		Facts   *Facts `json:"facts"`
	}
	lines := []rec{
		{Run: "r1", Script: "s.star", Outcome: "handled", Event: ev,
			Facts: &Facts{PRs: []PR{before, after}, Attempts: []Attempt{{ID: "att-1"}}}},
		{Run: "r2", Script: "s.star", Outcome: "handled", Event: ev, Facts: &Facts{PR: &after}},
		{Run: "r3", Script: "s.star", Outcome: "handled", Event: ev,
			Facts: &Facts{PRs: []PR{before}, PRsDropped: 3, Attempts: []Attempt{{ID: "att-1"}, {ID: "att-2"}, {ID: "att-3"}}}},
	}
	var log bytes.Buffer
	for _, l := range lines {
		b, _ := json.Marshal(l)
		log.Write(append(b, '\n'))
	}
	var out bytes.Buffer
	n, err := Replay(context.Background(), "s.star", []byte(script), &log, &out)
	if err != nil || n != 3 {
		t.Fatalf("replayed %d, %v\n%s", n, err, out.String())
	}
	want := []string{
		"event 1: after-push pull#7@h1 (run r1 of s.star, recorded as handled)",
		"  label: add [review-this], remove [hands-reviewed]",
		"  step after: h1 review-this",
		"  attempt(): read 2, the run recorded 1; the last again",
		"  -> handled",
		"event 2: after-push pull#7@h1 (run r2 of s.star, recorded as handled)",
		"  step after: h1 review-this",
		"  attempt(): the run recorded no attempt; an empty one",
		"  attempt(): the run recorded no attempt; an empty one",
		"  -> handled",
		"event 3: after-push pull#7@h1 (run r3 of s.star, recorded as handled)",
		"  label: add [review-this], remove [hands-reviewed]",
		"  pr(): read 2, the run kept 1 of 4; the last again",
		"  step after: h1 hands-reviewed",
		"  pr(): 2 of the run's 4 reads not made",
		"  attempt(): 1 of the run's 3 reads not made",
		"  -> handled",
	}
	if got := strings.Split(strings.TrimSpace(out.String()), "\n"); !slices.Equal(got, want) {
		t.Fatalf("got\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

// A run keeps at most maxFactReads reads of each kind and counts the rest;
// the counts stay off a record that dropped nothing.
func TestFactsKeepReadsInOrder(t *testing.T) {
	var f Facts
	for i := range maxFactReads + 2 {
		f.AddPR(PR{Number: int64(i)})
	}
	f.AddAttempt(Attempt{ID: "att-1"})
	if len(f.PRs) != maxFactReads || f.PRs[0].Number != 0 || f.PRs[maxFactReads-1].Number != maxFactReads-1 ||
		f.PRsDropped != 2 || len(f.Attempts) != 1 || f.AttemptsDropped != 0 {
		t.Fatalf("kept %d reads, dropped %d; attempts %d", len(f.PRs), f.PRsDropped, len(f.Attempts))
	}
	raw, _ := json.Marshal(Facts{PRs: []PR{{Number: 7}}})
	if s := string(raw); strings.Contains(s, "dropped") || strings.Contains(s, `"pr"`) || !strings.Contains(s, `"prs"`) {
		t.Fatalf("the record: %s", s)
	}
}
