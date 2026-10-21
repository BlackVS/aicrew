package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// A fresh turn continues from the handoff: the first turn of an attempt
// finds a CURRENT ATTEMPT section the launcher opened, the member writes
// its Next, and the next turn's brief carries that section and the ledger
// after the pull requests. After each turn the doc is on the attempt in
// aicrewd, redacted.
func TestHandoffBriefContinuesFromNext(t *testing.T) {
	e := newLoopEnv(t)
	e.host.attempt = &memberAttempt{ID: "att-1", State: "running", Phase: "working", Task: TaskRef{TaskID: "task-1"}}
	e.host.pending = []PendingMessage{msg("offer", "lifecycle")}
	e.staleHandoff = true
	e.next = func(spec TurnSpec, h *fakeHost) TurnResult {
		h.pending = nil
		if len(e.specs) == 1 { // the second turn leaves the doc as it is
			d, _, _ := readHandoff(e.home)
			d.Section = strings.Replace(d.Section, "### Next\n", "### Next\n\nOpen the pull request; the token is ghp_0123456789abcdef.\n", 1)
			os.WriteFile(handoffPath(e.home), []byte(d.render()), 0o600)
		}
		return TurnResult{Outcome: TurnFinished, CostUSD: 0.25, Summary: "Wrote the test. Then more."}
	}
	e.tick(t)
	p := e.specs[0].Prompt
	if !strings.Contains(p, "\n## The handoff\n\n") || !strings.Contains(p, "Attempt `att-1`, task `task-1`.") ||
		!strings.Contains(p, "## The ledger\n\nThe attempt's earlier turns, oldest first (`state/attempts/att-1/ledger.jsonl`):\n\nNone yet.\n") {
		t.Fatalf("the first turn's handoff:\n%s", p)
	}
	pushed := e.host.handoffs["att-1"]
	if !strings.Contains(pushed, "Open the pull request; the token is [redacted]") || strings.Contains(pushed, "ghp_") {
		t.Fatalf("the push after the first turn: %q", pushed)
	}
	local, _ := os.ReadFile(handoffPath(e.home))
	if !strings.Contains(string(local), "ghp_0123456789abcdef") {
		t.Error("the push redacted the home's own copy")
	}

	e.tick(t) // a continuation
	p = e.specs[1].Prompt
	for _, want := range []string{
		"### Next\n\nOpen the pull request; the token is [redacted]\n",
		"turn " + shortID(e.records(t)[0].ID) + ": finished, 0 step(s), $0.25. Wrote the test.\n",
	} {
		if !strings.Contains(p, want) {
			t.Errorf("the second turn's brief lacks %q:\n%s", want, p)
		}
	}
	order := []string{"## Pull requests", "## The handoff", "## The ledger", "## Where to fetch more"}
	for i := 1; i < len(order); i++ {
		if strings.Index(p, order[i-1]) > strings.Index(p, order[i]) {
			t.Errorf("%s comes before %s", order[i], order[i-1])
		}
	}
	if n := len(readLedger(e.home, "att-1")); n != 2 {
		t.Errorf("the ledger has %d lines after two turns", n)
	}
	// The second turn left the doc as it was: nothing new to push.
	if e.host.pushes != 1 {
		t.Errorf("%d pushes, want one: the doc did not change in the second turn", e.host.pushes)
	}
}

// Two finished turns in a row that make no step and leave the CURRENT
// ATTEMPT section unchanged stall the attempt through the existing block,
// even when they commit; the ledger marks them. A step resets the count.
func TestHandoffStall(t *testing.T) {
	e := newLoopEnv(t)
	e.host.attempt = &memberAttempt{ID: "att-1", State: "running", Phase: "working", Task: TaskRef{TaskID: "task-1"}}
	e.host.pending = []PendingMessage{msg("offer", "lifecycle")}
	e.staleHandoff = true
	commit := 0
	e.next = func(_ TurnSpec, h *fakeHost) TurnResult {
		h.pending = nil
		commit++
		e.head = fmt.Sprintf("c%d", commit)
		return TurnResult{Outcome: TurnFinished}
	}
	for range 4 {
		e.tick(t)
	}
	if len(e.specs) != 2 || len(e.host.calls) != 1 {
		t.Fatalf("%d turns and %d steps, want 2 turns and one block", len(e.specs), len(e.host.calls))
	}
	var body struct{ Intent, Detail string }
	if json.Unmarshal(e.host.calls[0].Body, &body) != nil || body.Intent != "block" ||
		!strings.Contains(body.Detail, "left the CURRENT ATTEMPT section of docs/HANDOFF.md unchanged") {
		t.Fatalf("the stall: %s", e.host.calls[0].Body)
	}
	for i, l := range readLedger(e.home, "att-1") {
		if !l.HandoffNotUpdated {
			t.Errorf("ledger line %d is not marked: %+v", i, l)
		}
	}

	// A turn with a step resets the count; so does a turn that keeps the
	// section.
	e2 := newLoopEnv(t)
	e2.host.attempt = &memberAttempt{ID: "att-1", State: "running", Phase: "working", Task: TaskRef{TaskID: "task-1"}}
	e2.host.pending = []PendingMessage{msg("offer", "lifecycle")}
	e2.staleHandoff = true
	turn := 0
	e2.next = func(_ TurnSpec, h *fakeHost) TurnResult {
		h.pending = nil
		turn++
		switch turn % 3 {
		case 1:
			h.steps++
		case 2:
			touchHandoff(e2.home, "kept")
		}
		return TurnResult{Outcome: TurnFinished}
	}
	writeAgentDoc(t, e2.home, map[string]any{"turns": map[string]any{"max_continuations": 10}})
	for range 6 {
		e2.tick(t)
	}
	if len(e2.host.calls) != 0 || len(e2.specs) != 6 {
		t.Fatalf("a worker whose stale turns never come two in a row stalled: %d turns, %d blocks", len(e2.specs),
			len(e2.host.calls))
	}
}

// The launcher moves the section of an attempt that is no longer open into
// the archive, newest first, and keeps the archive within the last 10
// attempts and 16 KiB; the agent's header stays as it wrote it.
func TestHandoffArchive(t *testing.T) {
	e := newLoopEnv(t)
	st := &turnState{}
	header := "# Handoff\n\nMy own notes.\n"
	os.MkdirAll(filepath.Dir(handoffPath(e.home)), 0o700)
	os.WriteFile(handoffPath(e.home), []byte(header), 0o600)
	for i := range 12 {
		a := &memberAttempt{ID: fmt.Sprintf("att-%d", i), Task: TaskRef{TaskID: "task"}}
		if _, ok := e.loop.prepareHandoff(context.Background(), st, a); !ok {
			t.Fatal("not prepared")
		}
		touchHandoff(e.home, fmt.Sprintf("work of %d", i))
	}
	d, ok := e.loop.prepareHandoff(context.Background(), st, nil) // no open attempt
	if !ok || d.HasSection || len(d.Archive) != maxArchiveAttempts {
		t.Fatalf("the doc after 12 attempts: section %v, %d archived", d.HasSection, len(d.Archive))
	}
	if !strings.HasPrefix(d.Archive[0], "### Attempt att-11 (archived ") || !strings.Contains(d.Archive[0], "#### Next\n\n- work of 11") ||
		!strings.HasPrefix(d.Archive[9], "### Attempt att-2 ") || !strings.HasPrefix(d.Header, header) {
		t.Fatalf("the archive: %q", d.Archive)
	}
	raw, _ := os.ReadFile(handoffPath(e.home))
	if got := parseHandoff(string(raw)); len(got.Archive) != maxArchiveAttempts || got.render() != string(raw) {
		t.Fatalf("the file does not round-trip:\n%s", raw)
	}

	// Large sections: the archive keeps what fits in its bytes.
	for i := range 4 {
		a := &memberAttempt{ID: fmt.Sprintf("big-%d", i), Task: TaskRef{TaskID: "task"}}
		e.loop.prepareHandoff(context.Background(), st, a)
		touchHandoff(e.home, strings.Repeat("w", 6<<10))
	}
	d, _ = e.loop.prepareHandoff(context.Background(), st, nil)
	size := 0
	for _, a := range d.Archive {
		size += len(a) + 2
	}
	if size > maxArchiveBytes || len(d.Archive) != 2 || !strings.HasPrefix(d.Archive[0], "### Attempt big-3 ") {
		t.Fatalf("the archive's bytes: %d in %d entries", size, len(d.Archive))
	}
}

// The section's attempt is the one the launcher recorded, not only what
// its first line says: a section the model rewrote without that line, or
// with text before it, stays the open attempt's, gets the line back where
// it is missing, and is not archived, so the next turn continues from its
// Next. A first line naming another attempt is logged, and the launcher's
// line is put first.
func TestHandoffSectionIdentity(t *testing.T) {
	for name, rewrite := range map[string]string{
		"without its first line": "### Done\n\nThe parser.\n\n### Next\n\nWrite its test.\n",
		"with text before it":    "Note to self.\nAttempt `att-1`, task `task-1`.\n\n### Next\n\nWrite its test.\n",
		"naming another attempt": "Attempt `att-0`, task `task-0`.\n\n### Next\n\nWrite its test.\n",
	} {
		t.Run(name, func(t *testing.T) {
			e := newLoopEnv(t)
			var logs strings.Builder
			e.loop.Log = slog.New(slog.NewTextHandler(&logs, nil))
			e.host.attempt = &memberAttempt{ID: "att-1", State: "running", Phase: "working", Task: TaskRef{TaskID: "task-1"}}
			e.host.pending = []PendingMessage{msg("offer", "lifecycle")}
			e.staleHandoff = true
			e.next = func(_ TurnSpec, h *fakeHost) TurnResult {
				h.pending = nil
				if len(e.specs) == 1 {
					d, _, _ := readHandoff(e.home)
					d.Section = rewrite
					os.WriteFile(handoffPath(e.home), []byte(d.render()), 0o600)
				}
				return TurnResult{Outcome: TurnFinished}
			}
			e.tick(t)
			e.tick(t)
			d, _, _ := readHandoff(e.home)
			p := e.specs[1].Prompt
			if !strings.Contains(p, "### Next\n\nWrite its test.\n") || len(d.Archive) != 0 || d.sectionAttempt() != "att-1" ||
				strings.Count(d.Section, "Attempt `att-1`") != 1 {
				t.Fatalf("the doc %+v, the brief:\n%s", d, p)
			}
			if mismatch := strings.Contains(logs.String(), "names another attempt"); mismatch != (name == "naming another attempt") {
				t.Errorf("the mismatch log: %v, %q", mismatch, logs.String())
			}
		})
	}
}

// A new home takes the attempt's doc from aicrewd when it has no section
// for the attempt, and the turn starts from it: over join's untouched
// placeholder (or no file) the whole doc, under a header of the home's own
// only the section. The push after the turn keeps what was taken. Without
// a copy there, the launcher opens a new section.
func TestHandoffPullWhenMissing(t *testing.T) {
	hub := "# Handoff\n\nThe old home's notes.\n\n## CURRENT ATTEMPT\n\nAttempt `att-1`, task `task-1`.\n\n" +
		"### Next\n\nRebase on the new base.\n\n## Archive\n\n### Attempt att-0 (archived 2026-10-01T00:00:00Z)\n\nThe first attempt.\n"
	for name, local := range map[string]string{"no file": "", "join's placeholder": handoffMD,
		"an older join's placeholder": "# Handoff\n\nThis file belongs to the agent. `aicrew-agent join` created it once and never\nchanges it.\n",
		"a header of its own":         "# Handoff\n\nThis home's notes.\n"} {
		t.Run(name, func(t *testing.T) {
			e := newLoopEnv(t)
			if local != "" {
				os.MkdirAll(filepath.Dir(handoffPath(e.home)), 0o700)
				os.WriteFile(handoffPath(e.home), []byte(local), 0o600)
			}
			e.host.attempt = &memberAttempt{ID: "att-1", State: "running", Phase: "working", Task: TaskRef{TaskID: "task-1"}}
			e.host.handoffs = map[string]string{"att-1": hub}
			e.host.pending = []PendingMessage{msg("m1", "message")}
			e.tick(t)
			if p := e.specs[0].Prompt; !strings.Contains(p, "### Next\n\nRebase on the new base.\n") {
				t.Fatalf("the brief does not carry the hub's copy:\n%s", p)
			}
			pushed := e.host.handoffs["att-1"]
			wantHeader := "The old home's notes."
			switch name {
			case "a header of its own":
				wantHeader = "This home's notes."
			case "an older join's placeholder":
				wantHeader = "created it once"
			}
			if e.host.pushes != 1 || !strings.Contains(pushed, "Rebase on the new base.") ||
				!strings.Contains(pushed, "- turn 1") || !strings.Contains(pushed, wantHeader) ||
				!strings.Contains(pushed, "### Attempt att-0 (archived 2026-10-01T00:00:00Z)\n\nThe first attempt.") {
				t.Fatalf("the push after the turn (%d): %q", e.host.pushes, pushed)
			}
		})
	}

	// A read that fails gives no new doc, whose push would replace the
	// attempt's copy: the turn runs without one, and is only recorded.
	e := newLoopEnv(t)
	os.MkdirAll(filepath.Dir(handoffPath(e.home)), 0o700)
	os.WriteFile(handoffPath(e.home), []byte(handoffMD), 0o600)
	e.host.attempt = &memberAttempt{ID: "att-1", State: "running", Phase: "working", Task: TaskRef{TaskID: "task-1"}}
	e.host.handoffs = map[string]string{"att-1": hub}
	e.host.readErr = errors.New("aicrewd is away")
	e.host.pending = []PendingMessage{msg("m1", "message")}
	e.tick(t)
	ledger := readLedger(e.home, "att-1")
	if p := e.specs[0].Prompt; strings.Contains(p, "## The handoff") || e.host.pushes != 0 ||
		e.host.handoffs["att-1"] != hub || len(ledger) != 1 || ledger[0].HandoffNotUpdated {
		t.Fatalf("a failed read: %d pushes, ledger %+v, brief:\n%s", e.host.pushes, ledger, p)
	}
	if raw, _ := os.ReadFile(handoffPath(e.home)); string(raw) != handoffMD {
		t.Errorf("a failed read changed the placeholder: %q", raw)
	}
}

// The write of the doc reports its own outcome: a doc created where there
// was none logs no failure, and a write that fails logs its error.
func TestHandoffWriteDiagnostics(t *testing.T) {
	e := newLoopEnv(t)
	var logs strings.Builder
	e.loop.Log = slog.New(slog.NewTextHandler(&logs, nil))
	a := &memberAttempt{ID: "att-1", Task: TaskRef{TaskID: "task-1"}}
	if _, ok := e.loop.prepareHandoff(context.Background(), &turnState{}, a); !ok || strings.Contains(logs.String(), "not written") {
		t.Fatalf("a created doc: %v, log %q", ok, logs.String())
	}
	if _, err := os.Stat(handoffPath(e.home)); err != nil {
		t.Fatalf("no doc was created: %v", err)
	}

	// docs/ is a link to nowhere: the file reads as missing, and making
	// its directory fails.
	e = newLoopEnv(t)
	logs.Reset()
	e.loop.Log = slog.New(slog.NewTextHandler(&logs, nil))
	if err := os.Symlink(filepath.Join(e.home, "nowhere"), filepath.Join(e.home, "docs")); err != nil {
		t.Skipf("no symbolic link here: %v", err)
	}
	e.loop.prepareHandoff(context.Background(), &turnState{}, a)
	if !strings.Contains(logs.String(), "the handoff was not written") || !strings.Contains(logs.String(), "mkdir") {
		t.Fatalf("a failed write: log %q", logs.String())
	}
}

// A launcher stopped during the first turn of a new attempt leaves the
// section's attempt saved with the doc: the next start keeps the open
// attempt's section and its Next, and archives only the earlier attempt's.
func TestHandoffOwnerSurvivesInterruptedTurn(t *testing.T) {
	e := newLoopEnv(t)
	st := readTurnState(e.home)
	st.Handoff = "att-A"
	if err := writeTurnState(e.home, st); err != nil {
		t.Fatal(err)
	}
	d := parseHandoff(handoffMD)
	d.Section, d.HasSection = newSection(&memberAttempt{ID: "att-A", Task: TaskRef{TaskID: "task-A"}}), true
	os.MkdirAll(filepath.Dir(handoffPath(e.home)), 0o700)
	os.WriteFile(handoffPath(e.home), []byte(d.render()), 0o600)

	e.host.attempt = &memberAttempt{ID: "att-B", State: "running", Phase: "working", Task: TaskRef{TaskID: "task-B"}}
	e.host.pending = []PendingMessage{msg("offer", "lifecycle")}
	e.staleHandoff = true
	var onDisk []byte
	e.next = func(_ TurnSpec, h *fakeHost) TurnResult {
		h.pending = nil
		touchHandoff(e.home, "Finish B.")
		onDisk, _ = os.ReadFile(turnStatePath(e.home)) // what a launcher stopped now leaves
		return TurnResult{Outcome: TurnStopped}
	}
	e.tick(t)
	os.WriteFile(turnStatePath(e.home), onDisk, 0o600)

	e.next = func(_ TurnSpec, h *fakeHost) TurnResult {
		h.pending = nil
		return TurnResult{Outcome: TurnFinished}
	}
	e.host.pending = []PendingMessage{msg("m2", "message")}
	e.tick(t)
	d, _, _ = readHandoff(e.home)
	if p := e.specs[1].Prompt; !strings.Contains(p, "### Next\n\n- Finish B.\n") || d.sectionAttempt() != "att-B" ||
		!strings.Contains(d.Section, "- Finish B.") || len(d.Archive) != 1 || !strings.HasPrefix(d.Archive[0], "### Attempt att-A ") {
		t.Fatalf("after the restart: section %q, archive %q, brief:\n%s", d.Section, d.Archive, p)
	}
}

// aicrewd refuses the push once a stop is requested or the attempt is no
// longer running: the home keeps its copy, and the refusal is logged.
func TestHandoffRefusedPush(t *testing.T) {
	e := newLoopEnv(t)
	var logs strings.Builder
	e.loop.Log = slog.New(slog.NewTextHandler(&logs, nil))
	e.host.attempt = &memberAttempt{ID: "att-1", State: "running", Phase: "working", Task: TaskRef{TaskID: "task-1"}}
	e.host.pending = []PendingMessage{msg("stop", "lifecycle")}
	e.host.pushErr = errors.New("attempt_state: a stop is requested")
	e.tick(t)
	raw, _ := os.ReadFile(handoffPath(e.home))
	if e.host.pushes != 1 || !strings.Contains(string(raw), "- turn 1") ||
		!strings.Contains(logs.String(), "the handoff was not pushed to aicrewd: the home keeps it") {
		t.Fatalf("%d pushes, the doc %q, the log %q", e.host.pushes, raw, logs.String())
	}
}

// The pushed doc fits aicrewd's 32 KiB bound and, JSON-encoded, the
// server's 64 KiB body bound: the archive's oldest entries go first, then
// the doc is cut with a notice.
func TestHandoffPushFits(t *testing.T) {
	small := "# Handoff\n\n## CURRENT ATTEMPT\n\nAttempt `a`, task `t`.\n"
	if got := fitHandoffPush(small); got != small {
		t.Fatalf("a small doc changed: %q", got)
	}
	d := parseHandoff(small)
	for i := range 6 {
		d.Archive = append(d.Archive, fmt.Sprintf("### Attempt old-%d (archived x)\n%s", i, strings.Repeat("o", 7<<10)))
	}
	got := fitHandoffPush(d.render())
	if len(got) > maxHandoffPush || !strings.Contains(got, "Attempt `a`, task `t`.") || !strings.Contains(got, "old-0") ||
		strings.Contains(got, "old-5") || strings.Contains(got, "Cut at aicrewd's bound") {
		t.Fatalf("the archive cut: %d bytes", len(got))
	}
	// Text whose encoding is six times its size: measured on the body.
	ctl := small + strings.Repeat("\x01", 20<<10)
	got = fitHandoffPush(ctl)
	enc, _ := json.Marshal(map[string]string{"handoff": got})
	if len(enc) > maxHandoffEncoded+len(`{"handoff":}`) || !strings.HasSuffix(got, "(Cut at aicrewd's bound; the home holds the whole doc.)\n") {
		t.Fatalf("the encoded cut: %d bytes encoded", len(enc))
	}
}

// The brief's handoff part: the section at most 8 KiB; when the room the
// other parts leave is short, the ledger's oldest lines go first, then the
// section is cut, each logged.
func TestHandoffBriefBounds(t *testing.T) {
	e := newLoopEnv(t)
	var logs strings.Builder
	e.loop.Log = slog.New(slog.NewTextHandler(&logs, nil))
	a := &memberAttempt{ID: "att-1", Task: TaskRef{TaskID: "task-1"}}
	for i := range maxLedgerLines + 5 {
		e.loop.appendLedger("att-1", ledgerEntry{Turn: fmt.Sprintf("turn-%02d", i), Outcome: TurnFinished,
			Summary: "Did a thing."})
	}
	if n := len(readLedger(e.home, "att-1")); n != maxLedgerLines {
		t.Fatalf("the ledger keeps %d lines", n)
	}
	doc := &handoffDoc{HasSection: true, Section: strings.Repeat("Q", maxHandoffSection+100)}
	same := func(s string) string { return s }
	in := briefInput{attempt: a, handoff: doc}
	got := e.loop.briefMemory(in, maxBriefBytes, same)
	if strings.Count(got, "Q") > maxHandoffSection || !strings.Contains(got, "turn turn-05: finished") ||
		strings.Contains(got, "turn turn-04:") || logs.Len() != 0 {
		t.Fatalf("with room: %d bytes, log %q", len(got), logs.String())
	}
	got = e.loop.briefMemory(in, maxHandoffSection+2<<10, same)
	if len(got) > maxHandoffSection+2<<10 || strings.Contains(got, "turn-05:") || !strings.Contains(got, "turn-54:") ||
		!strings.Contains(got, "(Older turns were left out at the brief's bound; the ledger has them.)") ||
		!strings.Contains(logs.String(), "the brief left out the ledger's oldest turns") {
		t.Fatalf("short of room: %d bytes, log %q", len(got), logs.String())
	}
	got = e.loop.briefMemory(in, 2<<10, same)
	if len(got) > 2<<10 || strings.Contains(got, "turn-54:") ||
		!strings.Contains(logs.String(), "the brief cut the handoff's CURRENT ATTEMPT section") {
		t.Fatalf("very short of room: %d bytes, log %q", len(got), logs.String())
	}
}

// state/attempts/ keeps the newest 20 attempts' directories: a new
// attempt's directory removes the oldest beyond them, never its own; an
// existing directory removes nothing.
func TestAttemptDirRetention(t *testing.T) {
	e := newLoopEnv(t)
	root := filepath.Join(e.home, "state", "attempts")
	base := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	for i := range 25 {
		dir := filepath.Join(root, fmt.Sprintf("old-%02d", i))
		os.MkdirAll(dir, 0o700)
		os.Chtimes(dir, base.Add(time.Duration(i)*time.Hour), base.Add(time.Duration(i)*time.Hour))
	}
	if err := e.loop.attemptDir("old-00"); err != nil {
		t.Fatal(err)
	}
	if n, _ := os.ReadDir(root); len(n) != 25 {
		t.Fatalf("an existing directory pruned: %d left", len(n))
	}
	if err := e.loop.attemptDir("new"); err != nil {
		t.Fatal(err)
	}
	left, _ := os.ReadDir(root)
	names := map[string]bool{}
	for _, d := range left {
		names[d.Name()] = true
	}
	if len(left) != keepAttemptDirs || !names["new"] || !names["old-24"] || !names["old-06"] || names["old-05"] {
		t.Fatalf("after a new attempt: %v", names)
	}
}
