package automation

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

// fakeHost records what a script asked of the host.
type fakeHost struct {
	pr       PR
	attempt  Attempt
	runs     []string
	results  map[string]RunResult
	steps    []string
	comments []string
	block    time.Duration // how long Run takes
	// The transitions: the labels added (+name) and removed (-name) in
	// order, the ready calls, the result references submitted, and the
	// re-runs, the first with reruns as its jobs and the rest refused.
	labels    []string
	ready     int
	submitted []string
	reruns    []string
	rerunAt   int
}

func (h *fakeHost) Label(_ context.Context, add, remove []string) error {
	for _, l := range remove {
		h.labels = append(h.labels, "-"+l)
	}
	for _, l := range add {
		h.labels = append(h.labels, "+"+l)
	}
	return nil
}
func (h *fakeHost) Ready(context.Context) error { h.ready++; return nil }
func (h *fakeHost) Submit(_ context.Context, url string) error {
	h.submitted = append(h.submitted, url)
	return nil
}
func (h *fakeHost) Rerun(context.Context) (RerunResult, error) {
	h.rerunAt++
	if h.rerunAt > 1 {
		return RerunResult{Reason: "the failed jobs were re-run at this head already"}, nil
	}
	if len(h.reruns) == 0 {
		return RerunResult{Reason: "the forge offers no re-run"}, nil
	}
	return RerunResult{Reran: true, Jobs: h.reruns}, nil
}

func (h *fakeHost) PR(context.Context) (PR, error)           { return h.pr, nil }
func (h *fakeHost) Attempt(context.Context) (Attempt, error) { return h.attempt, nil }
func (h *fakeHost) Run(ctx context.Context, cmd string, args []string) (RunResult, error) {
	line := CommandLine(cmd, args)
	h.runs = append(h.runs, line)
	if h.block > 0 {
		select {
		case <-time.After(h.block):
		case <-ctx.Done():
			return RunResult{}, ctx.Err()
		}
	}
	return h.results[line], nil
}
func (h *fakeHost) Step(_ context.Context, kind, text string) error {
	h.steps = append(h.steps, kind+": "+text)
	return nil
}
func (h *fakeHost) Comment(_ context.Context, body string) error {
	h.comments = append(h.comments, body)
	return nil
}

// gitOnly admits git and bash scripts, and denies git's global config, as
// the managed turn settings do.
var gitOnly = NewAllowList([]string{"Bash(git *)", "Bash(bash scripts/*.sh *)", "Read(./**)"},
	[]string{"Bash(*config --global*)", "Bash(*creds/*)"})

func run(t *testing.T, src string, h *fakeHost, lim Limits) (*Turn, error) {
	t.Helper()
	if h == nil {
		h = &fakeHost{}
	}
	return Run(context.Background(), Script{Name: "test.star", Source: []byte(src), Host: h, Allow: gitOnly, Limits: lim},
		Event{Kinds: []string{"forge.changed"}, IDs: []string{"ev-1"}})
}

// A script reads the host's facts and acts through it: main returns None
// when the event is handled, or a turn as a value with no side effect.
func TestHostAPI(t *testing.T) {
	h := &fakeHost{
		pr: PR{Number: 7, Head: "0123456", Base: "main", Branch: "feat/x", Behind: true, Labels: []string{"review-this"},
			Reviews: []Review{{Reviewer: "external", Head: "0123456", Verdict: "READY_FOR_HUMAN_MERGE", Body: "ok"}},
			Checks:  []Check{{Name: "go-test", Conclusion: "failure", Excerpt: "--- FAIL"}}},
		attempt: Attempt{ID: "att-1", Phase: "working", TaskID: "task-1", TaskSize: "S", BaseCommit: "abc", SpentUSD: 1.5, BudgetUSD: 10},
		results: map[string]RunResult{"git rev-parse HEAD": {Exit: 0, Output: "0123456\n"}},
	}
	src := `
def main(event):
    p = pr()
    a = attempt()
    if event.kinds != ["forge.changed"] or event.ids != ["ev-1"]:
        fail("event %s" % event)
    if not p.behind or p.reviews[0].verdict != "READY_FOR_HUMAN_MERGE" or p.checks[0].excerpt != "--- FAIL":
        fail("pr %s" % p)
    if a.task.size != "S" or a.spent_usd != 1.5:
        fail("attempt %s" % a)
    r = run("git", ["rev-parse", "HEAD"])
    step("base_update", "%s at %s, exit %d" % (p.branch, r.output.strip(), r.exit))
    comment("updated #%d" % p.number)
    return None
`
	turn, err := run(t, src, h, Limits{})
	if err != nil || turn != nil {
		t.Fatalf("handled: %+v, %v", turn, err)
	}
	if strings.Join(h.runs, "|") != "git rev-parse HEAD" || strings.Join(h.steps, "|") != "base_update: feat/x at 0123456, exit 0" ||
		strings.Join(h.comments, "|") != "updated #7" {
		t.Fatalf("the host saw runs %q, steps %q, comments %q", h.runs, h.steps, h.comments)
	}
	turn, err = run(t, `
def main(event):
    return turn("fix", "Updating feat/x from main conflicts")
`, &fakeHost{}, Limits{})
	if err != nil || turn == nil || turn.Kind != "fix" || turn.Content != "Updating feat/x from main conflicts" {
		t.Fatalf("a turn: %+v, %v", turn, err)
	}
	// A turn built and dropped asks nothing.
	if turn, err = run(t, "def main(event):\n    turn(\"fix\", \"x\")\n    return None\n", &fakeHost{}, Limits{}); err != nil || turn != nil {
		t.Fatalf("a dropped turn: %+v, %v", turn, err)
	}
}

// The host API is exactly 10.3's: a script reaches no file, network, clock
// or environment, loads nothing, has no while loop or recursion, and any
// other name fails to resolve.
func TestHermetic(t *testing.T) {
	for name, src := range map[string]string{
		"a file":         "def main(event):\n    return open(\"/etc/passwd\")\n",
		"the network":    "def main(event):\n    return http.get(\"https://example.com\")\n",
		"the clock":      "def main(event):\n    return time.now()\n",
		"the env":        "def main(event):\n    return getenv(\"HOME\")\n",
		"os":             "def main(event):\n    return os.environ\n",
		"a load":         "load(\"x.star\", \"y\")\ndef main(event):\n    return None\n",
		"a while loop":   "def main(event):\n    while True:\n        pass\n",
		"recursion":      "def f(n):\n    return f(n)\ndef main(event):\n    return f(1)\n",
		"no main":        "x = 1\n",
		"a bad return":   "def main(event):\n    return 1\n",
		"exec":           "def main(event):\n    exec(\"x\")\n",
		"globals change": "x = [1]\ndef main(event):\n    x.append(2)\n",
	} {
		if _, err := run(t, src, nil, Limits{}); err == nil {
			t.Errorf("%s: the script ran", name)
		}
	}
	// The text's faults are told apart from a run's.
	for name, c := range map[string]struct {
		src  string
		want error
	}{
		"an undefined name": {"def main(event):\n    return open(\"x\")\n", ErrInvalid},
		"a while loop":      {"def main(event):\n    while True:\n        pass\n", ErrInvalid},
		"a syntax error":    {"def main(event)\n", ErrInvalid},
		"no main":           {"x = 1\n", ErrInvalid},
		"recursion":         {"def f(n):\n    return f(n)\ndef main(event):\n    return f(1)\n", ErrScript},
		"fail()":            {"def main(event):\n    fail(\"no\")\n", ErrScript},
		"a frozen global":   {"x = [1]\ndef main(event):\n    x.append(2)\n", ErrScript},
	} {
		if _, err := run(t, c.src, nil, Limits{}); !errors.Is(err, c.want) {
			t.Errorf("%s: %v, want %v", name, err, c.want)
		}
	}
	// What a script may use: the universe's pure builtins.
	if _, err := run(t, "def main(event):\n    if len(sorted([3, 1])) != 2 or \"a,b\".split(\",\")[1] != \"b\":\n        fail(\"x\")\n", nil, Limits{}); err != nil {
		t.Errorf("pure builtins: %v", err)
	}
}

// A run that passes its step limit or its wall clock is stopped and fails.
func TestLimits(t *testing.T) {
	loop := "def main(event):\n    for i in range(100000000):\n        pass\n"
	_, err := run(t, loop, nil, Limits{Steps: 10_000})
	if !errors.Is(err, ErrScript) || !strings.Contains(err.Error(), "too many steps") {
		t.Fatalf("the step limit: %v", err)
	}
	start := time.Now()
	_, err = run(t, loop, nil, Limits{Steps: 1 << 62, Wall: 100 * time.Millisecond})
	if !errors.Is(err, ErrScript) || time.Since(start) > 5*time.Second || !strings.Contains(err.Error(), "wall-clock") {
		t.Fatalf("the wall clock, in the interpreter: %v after %v", err, time.Since(start))
	}
	// A slow command counts against the same clock.
	h := &fakeHost{block: time.Minute}
	start = time.Now()
	_, err = run(t, "def main(event):\n    run(\"git\", [\"fetch\"])\n", h, Limits{Wall: 100 * time.Millisecond})
	if !errors.Is(err, ErrScript) || time.Since(start) > 5*time.Second {
		t.Fatalf("the wall clock, in run(): %v after %v", err, time.Since(start))
	}
	if MaxSteps != 1_000_000 || MaxWall != 2*time.Minute {
		t.Errorf("the default limits are %d steps and %v", MaxSteps, MaxWall)
	}
}

// run() takes only what the allow list admits; a refused command fails the
// script before the host sees it.
func TestRunAllowList(t *testing.T) {
	for _, c := range []struct {
		src    string
		admits bool
	}{
		{`run("git", ["merge", "--no-edit", "origin/main"])`, true},
		{`run("bash", ["scripts/patch-identity.sh", "origin/main"])`, true},
		{`run("curl", ["https://example.com"])`, false},
		{`run("git", ["config", "--global", "user.name", "x"])`, false},
		{`run("bash", ["-c", "rm -rf /"])`, false},
		{`run("/usr/bin/git", ["status"])`, false},
		{`run("git status", [])`, false},
		{`run("git", ["show", "HEAD:creds/x"])`, false},
	} {
		h := &fakeHost{}
		_, err := run(t, "def main(event):\n    "+c.src+"\n", h, Limits{})
		if c.admits != (err == nil) || (!c.admits && len(h.runs) != 0) {
			t.Errorf("%s: %v, the host ran %q", c.src, err, h.runs)
		}
		if !c.admits && !strings.Contains(err.Error(), ErrRefused.Error()) {
			t.Errorf("%s: %v, want a refusal", c.src, err)
		}
	}
}

// What the host gives is bounded, and what a script hands it is checked.
func TestHostBounds(t *testing.T) {
	h := &fakeHost{
		pr: PR{Reviews: []Review{{Body: strings.Repeat("r", 2*MaxText)}},
			Checks: []Check{{Name: "x", Excerpt: strings.Repeat("e", 2*MaxExcerpt)}}},
		results: map[string]RunResult{"git log": {Output: strings.Repeat("o", 2*MaxRunOutput)}},
	}
	src := `
def main(event):
    p = pr()
    if len(p.reviews[0].body) != %d or len(p.checks[0].excerpt) != %d:
        fail("pr not bounded")
    if len(run("git", ["log"]).output) != %d:
        fail("run not bounded")
`
	src = strings.Replace(strings.Replace(strings.Replace(src, "%d", "16384", 1), "%d", "8192", 1), "%d", "65536", 1)
	if _, err := run(t, src, h, Limits{}); err != nil {
		t.Fatalf("bounded results: %v", err)
	}
	for name, src := range map[string]string{
		"a long comment":   "def main(event):\n    comment(\"x\" * 16385)\n",
		"an empty comment": "def main(event):\n    comment(\" \")\n",
		"a bad step kind":  "def main(event):\n    step(\"Base Update\", \"x\")\n",
		"a long turn":      "def main(event):\n    return turn(\"fix\", \"x\" * 16385)\n",
		"a bad turn kind":  "def main(event):\n    return turn(\"\", \"x\")\n",
	} {
		h := &fakeHost{}
		if _, err := run(t, src, h, Limits{}); err == nil || len(h.comments) != 0 || len(h.steps) != 0 {
			t.Errorf("%s: %v, the host saw %q %q", name, err, h.comments, h.steps)
		}
	}
}

// run() output is text: bytes that are not UTF-8 become U+FFFD, and the
// bound keeps the start of the output, even when an invalid byte sits at
// its very start (hands' follow-up on #166: such output was cut to "").
func TestBoundKeepsText(t *testing.T) {
	const rep = "\xef\xbf\xbd" // U+FFFD
	big := "\xffok" + strings.Repeat("o", 2*MaxRunOutput)
	got, cut := Bound(big, MaxRunOutput)
	if !cut || !strings.HasPrefix(got, rep+"okoo") || len(got) > MaxRunOutput || !utf8.ValidString(got) {
		t.Fatalf("an invalid byte at the start: cut %v, %d bytes, starts %q", cut, len(got), got[:min(len(got), 8)])
	}
	// A cut through a multi-byte rune drops its bytes; a U+FFFD of the
	// text, whole at the end, stays.
	if got, _ := Bound("ab\xc3\xa9", 3); got != "ab" {
		t.Errorf("a split rune: %q", got)
	}
	if got, _ := Bound("a"+rep+"b", 4); got != "a"+rep {
		t.Errorf("a whole U+FFFD at the cut: %q", got)
	}
	if got, cut := Bound("short\xfe", 100); cut || got != "short"+rep {
		t.Errorf("short output with an invalid byte: %q %v", got, cut)
	}
	h := &fakeHost{results: map[string]RunResult{"git log": {Output: big}}}
	src := "def main(event):\n    o = run(\"git\", [\"log\"]).output\n" +
		"    if not o.startswith(\"" + rep + "ok\") or len(o) > 65536:\n        fail(\"output: %d bytes\" % len(o))\n"
	if _, err := run(t, src, h, Limits{}); err != nil {
		t.Fatalf("run() output: %v", err)
	}
}

// The transitions of 4.2 as host calls (increment 2d-c): label() removes
// and adds, ready() and submit() take no judgement, rerun() says what it
// re-ran, and pr() names the pull request's URL and each failed check's
// runner failure. A bad argument fails the script before the host is asked.
func TestHostTransitions(t *testing.T) {
	h := &fakeHost{
		pr: PR{Number: 7, URL: "https://forge.example.test/team/app/pulls/7", Head: "0123456", Labels: []string{"hands-reviewed"},
			Checks: []Check{{Name: "build", Conclusion: "failure", RunnerFailure: true}, {Name: "test", Conclusion: "failure"}}},
		reruns: []string{"build", "test"},
	}
	src := `
def main(event):
    p = pr()
    if [c.runner_failure for c in p.checks] != [True, False]:
        fail("runner failures: %s" % p.checks)
    label(add=["review-this"], remove=["hands-reviewed"])
    label(remove=["hands-reviewing"])
    r = rerun()
    if not r.reran or r.jobs != ["build", "test"] or r.reason != "":
        fail("the first rerun: %s" % r)
    r = rerun()
    if r.reran or r.jobs != [] or "already" not in r.reason:
        fail("the second rerun: %s" % r)
    ready()
    submit(p.url)
`
	if _, err := run(t, src, h, Limits{}); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(h.labels, []string{"-hands-reviewed", "+review-this", "-hands-reviewing"}) || h.ready != 1 ||
		!slices.Equal(h.submitted, []string{"https://forge.example.test/team/app/pulls/7"}) || h.rerunAt != 2 {
		t.Errorf("the host saw labels %v, ready %d, submitted %v, reruns %d", h.labels, h.ready, h.submitted, h.rerunAt)
	}
	for name, body := range map[string]string{
		"no label":         `label()`,
		"a long label":     `label(add=["` + strings.Repeat("x", MaxLabel+1) + `"])`,
		"a two-line label": "label(add=[\"a\\nb\"])",
		"too many labels":  `label(remove=[str(i) for i in range(MaxLabels + 1)])`,
		"a non-URL":        `submit("pull 7")`,
		"a URL with space": `submit("https://forge.example.test/a b")`,
		"ready with args":  `ready(1)`,
		"rerun with args":  `rerun(now=True)`,
	} {
		h := &fakeHost{}
		body = strings.ReplaceAll(body, "MaxLabels", fmt.Sprint(MaxLabels))
		_, err := run(t, "def main(event):\n    "+body+"\n", h, Limits{})
		if !errors.Is(err, ErrScript) || len(h.labels)+h.ready+len(h.submitted)+h.rerunAt != 0 {
			t.Errorf("%s: %v (host saw %v %d %v %d)", name, err, h.labels, h.ready, h.submitted, h.rerunAt)
		}
	}
}
