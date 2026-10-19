package automation

import (
	"context"
	"io/fs"
	"path"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/BlackVS/aicrew/defaults"
	"github.com/BlackVS/aicrew/internal/events"
)

// The release's default automations (docs/DESIGN-TURNS.md 13.1, increment
// 2d-d): their entries parse as release defaults at the points the design
// names, each script carries the header, and none names a host, an
// endpoint or an instance: a default is universal release content.
func TestDefaultEntries(t *testing.T) {
	entries := defaultEntries(t)
	var names []string
	for _, e := range entries {
		names = append(names, e.Name+"@"+e.Point)
	}
	if !slices.Equal(names, []string{"review-request@after-push", "ci-rerun@ci-failed", "gates-green@gates-green", "base-update@after-ci"}) {
		t.Fatalf("the defaults: %v", names)
	}
	if !slices.Equal(entries[0].Events, []string{events.KindForgeChanged}) || entries[1].Events != nil || entries[2].Events != nil ||
		!slices.Equal(entries[3].Events, []string{events.KindForgeChanged}) {
		t.Fatalf("the defaults' events: %+v", entries)
	}
	instance := regexp.MustCompile(`://|[A-Za-z0-9-]+\.(com|dev|io|net|org)\b|github|gitlab|gitea`)
	for _, e := range entries {
		src := defaultScript(t, e)
		if h, err := ParseHeader(src); err != nil || h.HostAPI != 1 {
			t.Errorf("%s: the header: %+v, %v", e.Script, h, err)
		}
		if m := instance.FindString(string(src)); m != "" {
			t.Errorf("%s names %q: a default is universal", e.Script, m)
		}
	}
}

// gates-green lifts the draft and submits the pull request's URL; it does
// nothing for an attempt submitted already, and reads no label: the
// launcher's edge keeps a review in flight from it.
func TestDefaultGatesGreen(t *testing.T) {
	src := defaultSource(t, "gates-green")
	url := "https://forge.example.test/team/app/pulls/7"
	h := &fakeHost{pr: PR{Number: 7, URL: url, Head: "0123456", Draft: true, Labels: []string{"hands-reviewed"}},
		attempt: Attempt{ID: "att-1", Phase: "working"}}
	if _, err := runDefault(src, h, Event{Kinds: []string{PointGatesGreen}}); err != nil {
		t.Fatal(err)
	}
	if h.ready != 1 || !slices.Equal(h.submitted, []string{url}) || len(h.labels) != 0 {
		t.Errorf("ready %d, submitted %v, labels %v", h.ready, h.submitted, h.labels)
	}
	h = &fakeHost{pr: PR{Number: 7, URL: url, Head: "0123456"}, attempt: Attempt{ID: "att-1", Phase: "submitted"}}
	if _, err := runDefault(src, h, Event{Kinds: []string{PointGatesGreen}}); err != nil {
		t.Fatal(err)
	}
	if h.ready != 0 || len(h.submitted) != 0 || len(h.steps) != 1 || !strings.Contains(h.steps[0], "submitted already") {
		t.Errorf("an attempt submitted already: ready %d, submitted %v, steps %v", h.ready, h.submitted, h.steps)
	}
	// The launcher takes the point only from an observation without a
	// review in flight; the script reads no label, so one that landed
	// between the observation and the run does not strand the head.
	h = &fakeHost{pr: PR{Number: 7, URL: url, Head: "0123456", Draft: true, Labels: []string{"hands-reviewing"}},
		attempt: Attempt{ID: "att-1", Phase: "working"}}
	if _, err := runDefault(src, h, Event{Kinds: []string{PointGatesGreen}}); err != nil {
		t.Fatal(err)
	}
	if h.ready != 1 || !slices.Equal(h.submitted, []string{url}) {
		t.Errorf("a label after the observation: ready %d, submitted %v", h.ready, h.submitted)
	}
}

// review-request clears hands-reviewed and adds review-this once at a new
// head; a base-only update, hands-reviewing present, review-this present
// and an external review at the head leave the labels alone; and a head
// without any label (the in-flight read) gets one add and nothing more. On
// forge.changed it adds review-this once after the external reviewer's
// could-not-run notice and once after hands-review-failed, removing that
// label; a failure while a review is in progress or a request is in
// flight, a change without a failure, the empty label read and a second
// failure at the head add nothing.
func TestDefaultReviewRequest(t *testing.T) {
	src := defaultSource(t, "review-request")
	head := "0123456789abcdef0123456789abcdef01234567"
	changed := func(c ForgeChange) Event {
		return Event{Kinds: []string{events.KindForgeChanged}, Change: &c}
	}
	notice := changed(ForgeChange{LabelsRemoved: []string{"hands-reviewing"}, ReviewFailures: 1, Comments: []ChangedComment{
		{Author: "hands-bot", Kind: "comment", Body: "[hands-bot review] could not run: the runner is down", Notice: "review-failed"}}})
	cases := []struct {
		name   string
		labels []string
		review *Review
		event  Event
		want   []string
	}{
		{"a new head", []string{"hands-reviewed"}, nil, Event{Kinds: []string{PointAfterPush}}, []string{"-hands-reviewed", "+review-this"}},
		{"a first head", nil, nil, Event{Kinds: []string{PointAfterPush}}, []string{"+review-this"}},
		{"a base-only update", []string{"hands-reviewed"}, nil, Event{Kinds: []string{PointAfterPush}, BaseOnly: true}, nil},
		{"a review in progress", []string{"hands-reviewing"}, nil, Event{Kinds: []string{PointAfterPush}}, nil},
		{"a request in flight", []string{"review-this"}, nil, Event{Kinds: []string{PointAfterPush}}, nil},
		{"a review at the head", []string{"hands-reviewed"}, &Review{Reviewer: "external", Head: head[:12], Verdict: "READY_FOR_HUMAN_MERGE"},
			Event{Kinds: []string{PointAfterPush}}, nil},
		{"a review at an earlier head", []string{"hands-reviewed"}, &Review{Reviewer: "external", Head: "fedcba987654", Verdict: "READY_FOR_HUMAN_MERGE"},
			Event{Kinds: []string{PointAfterPush}}, []string{"-hands-reviewed", "+review-this"}},
		{"the failure notice", nil, nil, notice, []string{"+review-this"}},
		{"the failure label", []string{"hands-review-failed"}, nil,
			changed(ForgeChange{LabelsAdded: []string{"hands-review-failed"}, ReviewFailures: 1}), []string{"-hands-review-failed", "+review-this"}},
		{"a failure while a review is in progress", []string{"hands-reviewing"}, nil, notice, nil},
		{"a failure while a request is in flight", []string{"review-this"}, nil, notice, nil},
		{"a change without a failure", []string{"hands-reviewed"}, nil,
			changed(ForgeChange{Checks: "success", LabelsAdded: []string{"hands-reviewed"}, Comments: []ChangedComment{{Author: "hands-bot", Kind: "comment", Body: "reviewed"}}}), nil},
		{"the empty label read", nil, nil, changed(ForgeChange{LabelsRemoved: []string{"review-this"}}), nil},
		{"a second failure at the head", []string{"hands-review-failed"}, nil,
			changed(ForgeChange{LabelsAdded: []string{"hands-review-failed"}, ReviewFailures: 2}), nil},
	}
	for _, c := range cases {
		h := &fakeHost{pr: PR{Number: 7, Head: head, Labels: c.labels}}
		if c.review != nil {
			h.pr.Reviews = []Review{*c.review}
		}
		if _, err := runDefault(src, h, c.event); err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if !slices.Equal(h.labels, c.want) {
			t.Errorf("%s: labels %v, want %v", c.name, h.labels, c.want)
		}
		if c.name == "a second failure at the head" && !slices.Contains(h.steps, "review_request: the external review failed again at this head; the request is not repeated") {
			t.Errorf("%s: steps %v", c.name, h.steps)
		}
	}
}

// ci-rerun re-runs the failed jobs once when a check failed on the runner,
// says so when the head was re-run already, and leaves a failure of the
// change alone.
func TestDefaultCIRerun(t *testing.T) {
	src := defaultSource(t, "ci-rerun")
	h := &fakeHost{pr: PR{Number: 7, Head: "0123456", Checks: []Check{{Name: "lint", Conclusion: "success"},
		{Name: "build", Conclusion: "failure", RunnerFailure: true}, {Name: "test", Conclusion: "failure"}}},
		reruns: []string{"build", "test"}}
	if _, err := runDefault(src, h, Event{Kinds: []string{PointCIFailed}}); err != nil {
		t.Fatal(err)
	}
	if h.rerunAt != 1 || !slices.Equal(h.steps, []string{"ci_rerun: runner failure of build: re-ran build, test"}) {
		t.Errorf("the first runner failure: re-runs %d, steps %v", h.rerunAt, h.steps)
	}
	// A second runner failure at the head: rerun() refuses, the script
	// records why and fails nothing.
	h.steps = nil
	if _, err := runDefault(src, h, Event{Kinds: []string{PointCIFailed}}); err != nil {
		t.Fatal(err)
	}
	if h.rerunAt != 2 || len(h.steps) != 1 || !strings.Contains(h.steps[0], "not re-run: the failed jobs were re-run at this head already") {
		t.Errorf("the second runner failure: re-runs %d, steps %v", h.rerunAt, h.steps)
	}
	h = &fakeHost{pr: PR{Number: 7, Head: "0123456", Checks: []Check{{Name: "test", Conclusion: "failure", Excerpt: "FAIL"}}}}
	if _, err := runDefault(src, h, Event{Kinds: []string{PointCIFailed}}); err != nil {
		t.Fatal(err)
	}
	if h.rerunAt != 0 || len(h.steps) != 0 {
		t.Errorf("a failure of the change: re-runs %d, steps %v", h.rerunAt, h.steps)
	}
}

func defaultEntries(t *testing.T) []Entry {
	t.Helper()
	raw, err := fs.ReadFile(defaults.Automations, defaults.AutomationsFile)
	if err != nil {
		t.Fatal(err)
	}
	entries, err := ParseDefaults(raw)
	if err != nil {
		t.Fatal(err)
	}
	return entries
}

func defaultScript(t *testing.T, e Entry) []byte {
	t.Helper()
	src, err := fs.ReadFile(defaults.Automations, path.Join("automations", e.Script))
	if err != nil {
		t.Fatal(err)
	}
	return src
}

// base-update calls update_base() at after-ci unless an external review
// is in flight, without reading pr().behind (the call's own answer is the
// fresh reading of the base), and turns the launcher's answer into the
// worker's turn: a conflict is a fix turn carrying it, a changed identity
// a fix turn with the reason; a refused push, a clean update and a branch
// up to date ask for nothing. A call that reports an updated branch with
// a reason (the completion of a cut update, or a note not posted) is
// followed by one more. On forge.changed it acts only when the change
// removed hands-reviewing, the label is not back and every check at the
// head has concluded; otherwise it steps, or does nothing for a change
// that is not the review's end.
func TestDefaultBaseUpdate(t *testing.T) {
	src := defaultSource(t, "base-update")
	conflict := &Conflict{Base: "main", Files: []string{"a.go", "b.go"}, Hunks: "<<<<<<< HEAD\n"}
	ended := &ForgeChange{LabelsRemoved: []string{"hands-reviewing"}}
	green := []Check{{Name: "go-test", Conclusion: "success"}, {Name: "lint", Conclusion: "failure"}}
	inProgress := "base_update: an external review is in progress; the update of this head follows its result"
	cases := []struct {
		name    string
		event   Event
		behind  bool
		labels  []string
		checks  []Check
		update  UpdateBaseResult
		updates int
		turn    *Turn
		steps   []string
	}{
		{"up to date", Event{Kinds: []string{PointAfterCI}}, false, nil, green, UpdateBaseResult{OldHead: "0123456", Reason: "the branch is up to date with main"}, 1, nil, nil},
		{"a review in progress", Event{Kinds: []string{PointAfterCI}}, true, []string{"hands-reviewing"}, green, UpdateBaseResult{}, 0, nil, []string{inProgress}},
		{"a conflict", Event{Kinds: []string{PointAfterCI}}, true, []string{"hands-reviewed"}, green,
			UpdateBaseResult{OldHead: "0123456", Conflict: conflict, Reason: "merging main into feat/x conflicts in a.go, b.go"}, 1,
			&Turn{Kind: "fix", Content: "Updating feat/x from main conflicts in a.go, b.go. Resolve the conflict and push the branch.", Conflict: conflict}, nil},
		{"a changed identity", Event{Kinds: []string{PointAfterCI}}, true, nil, green, UpdateBaseResult{OldHead: "0123456", IdentityChanged: true,
			Reason: "merging main into feat/x changes the patch identity from aaaa to bbbb; update the branch by hand"}, 1,
			&Turn{Kind: "fix", Content: "Updating feat/x from main would change the patch identity, so the reviews would not stand: " +
				"merging main into feat/x changes the patch identity from aaaa to bbbb; update the branch by hand"}, nil},
		{"a refused push", Event{Kinds: []string{PointAfterCI}}, true, nil, green,
			UpdateBaseResult{OldHead: "0123456", Reason: "the push of feat/x was refused, the branch moved during the update"}, 1, nil, nil},
		{"a clean update", Event{Kinds: []string{PointAfterCI}}, true, []string{"hands-reviewed"}, green,
			UpdateBaseResult{Updated: true, OldHead: "0123456", NewHead: "89abcde"}, 1, nil, nil},
		{"a completed cut update", Event{Kinds: []string{PointAfterCI}}, true, nil, green,
			UpdateBaseResult{Updated: true, OldHead: "0123456", NewHead: "89abcde", Reason: "completed the update cut after its push"}, 2, nil, nil},
		{"the review ended, CI concluded", Event{Kinds: []string{events.KindForgeChanged}, Change: ended}, true, []string{"hands-reviewed"}, green,
			UpdateBaseResult{Updated: true, OldHead: "0123456", NewHead: "89abcde"}, 1, nil, nil},
		{"the review ended, no check at the head", Event{Kinds: []string{events.KindForgeChanged}, Change: ended}, true, []string{"hands-reviewed"}, nil,
			UpdateBaseResult{OldHead: "0123456", Reason: "the branch is up to date with main"}, 1, nil, nil},
		{"the review ended, a check still running", Event{Kinds: []string{events.KindForgeChanged}, Change: ended}, true, []string{"hands-reviewed"},
			[]Check{{Name: "go-test", Conclusion: "success"}, {Name: "lint", Conclusion: "pending"}}, UpdateBaseResult{}, 0, nil,
			[]string{"base_update: the external review ended while lint is still running; the update of this head follows CI"}},
		{"the review ended and began again", Event{Kinds: []string{events.KindForgeChanged}, Change: ended}, true, []string{"hands-reviewing"}, green,
			UpdateBaseResult{}, 0, nil, []string{inProgress}},
		{"a change that is not the review's end", Event{Kinds: []string{events.KindForgeChanged}, Change: &ForgeChange{LabelsAdded: []string{"review-this"}}},
			true, nil, green, UpdateBaseResult{}, 0, nil, nil},
	}
	for _, c := range cases {
		h := &fakeHost{pr: PR{Number: 7, Head: "0123456", Base: "main", Branch: "feat/x", Behind: c.behind, Labels: c.labels, Checks: c.checks}, update: c.update}
		turn, err := runDefault(src, h, c.event)
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if h.updates != c.updates || len(h.labels)+len(h.comments)+len(h.runs) != 0 {
			t.Errorf("%s: %d update calls (want %d), labels %v, comments %v, runs %v", c.name, h.updates, c.updates, h.labels, h.comments, h.runs)
		}
		if !slices.Equal(h.steps, c.steps) {
			t.Errorf("%s: steps %v, want %v", c.name, h.steps, c.steps)
		}
		switch {
		case (turn == nil) != (c.turn == nil):
			t.Errorf("%s: turn %+v, want %+v", c.name, turn, c.turn)
		case turn != nil && (turn.Kind != c.turn.Kind || turn.Content != c.turn.Content):
			t.Errorf("%s: turn %+v, want %+v", c.name, turn, c.turn)
		case turn != nil && (turn.Conflict == nil) != (c.turn.Conflict == nil):
			t.Errorf("%s: the turn's conflict %+v, want %+v", c.name, turn.Conflict, c.turn.Conflict)
		case turn != nil && turn.Conflict != nil && (turn.Conflict.Base != "main" || !slices.Equal(turn.Conflict.Files, conflict.Files) || turn.Conflict.Hunks != conflict.Hunks):
			t.Errorf("%s: the turn's conflict %+v", c.name, turn.Conflict)
		}
	}
}

func defaultSource(t *testing.T, name string) []byte {
	t.Helper()
	for _, e := range defaultEntries(t) {
		if e.Name == name {
			return defaultScript(t, e)
		}
	}
	t.Fatalf("no default %s", name)
	return nil
}

func runDefault(src []byte, h *fakeHost, ev Event) (*Turn, error) {
	if ev.IDs == nil {
		ev.IDs = []string{"pull@head"}
	}
	return Run(context.Background(), Script{Name: "default.star", Source: src, Host: h, Allow: gitOnly}, ev)
}
