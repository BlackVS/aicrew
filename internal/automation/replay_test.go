package automation

import (
	"bytes"
	"context"
	"encoding/json"
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
