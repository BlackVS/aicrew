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
	if !slices.Equal(names, []string{"review-request@after-push", "ci-rerun@ci-failed", "gates-green@gates-green"}) {
		t.Fatalf("the defaults: %v", names)
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
// without any label (the in-flight read) gets one add and nothing more.
func TestDefaultReviewRequest(t *testing.T) {
	src := defaultSource(t, "review-request")
	head := "0123456789abcdef0123456789abcdef01234567"
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
