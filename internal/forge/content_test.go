package forge

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/BlackVS/aicrew/internal/forge/fakeforge"
)

// goTestLog is a failed job's log as GitHub writes it: timestamps, a colour
// escape, setup lines, a test failure, and the runner's error at the end.
func goTestLog(setup int) string {
	var b strings.Builder
	for i := range setup {
		fmt.Fprintf(&b, "2026-10-15T09:00:%02d.1234567Z setup line %d\n", i%60, i)
	}
	b.WriteString("2026-10-15T09:01:00.0000000Z \x1b[36;1mgo test ./...\x1b[0m\r\n")
	b.WriteString("2026-10-15T09:01:01.0000000Z --- FAIL: TestParser (0.01s)\n")
	b.WriteString("2026-10-15T09:01:01.0000000Z     parser_test.go:12: got 1, want 2\n")
	b.WriteString("2026-10-15T09:01:02.0000000Z FAIL\tgithub.com/team/app\t0.02s\n")
	b.WriteString("2026-10-15T09:01:03.0000000Z ##[error]Process completed with exit code 1.\n")
	return b.String()
}

// Each dialect reads a pull request's comments with bodies and authors,
// oldest first, recognises the review headers at the head, and reads the
// head's checks with a failed job's log excerpt where the forge offers one.
func TestPullContent(t *testing.T) {
	ctx := context.Background()
	for _, k := range []Kind{GitHub, Gitea, GitLab} {
		t.Run(string(k), func(t *testing.T) {
			f := fakeforge.New(t, string(k))
			c := &Client{HTTP: f.Client}
			if _, err := c.OpenPull(ctx, f.Host, k, fakeforge.Token, fakeforge.Repo,
				NewPull{Title: "Fix", Head: "feat/x", Base: "main"}); err != nil {
				t.Fatal(err)
			}
			p, content, err := c.GetPullContent(ctx, f.Host, k, fakeforge.Token, fakeforge.Repo, 1)
			if err != nil {
				t.Fatal(err)
			}
			if p.Number != 1 || len(content.Comments) != 0 || len(content.Checks) != 0 {
				t.Fatalf("a new pull request: %+v %+v", p, content)
			}
			if _, err := c.CommentPull(ctx, f.Host, k, fakeforge.Token, fakeforge.Repo, 1,
				"[oh-code-review (high) review] reviewed at head "+fakeforge.HeadCommit[:7]+"\n\nVerdict: READY"); err != nil {
				t.Fatal(err)
			}
			f.SetPull(1, func(p *fakeforge.Pull) {
				p.SystemNotes = true
				p.CommentAs("hands-bot", "[hands-bot review] reviewed at head 0123456789abcdef0123456789abcdef01234567\n\nBLOCKERS\n- old")
				p.Review(fakeforge.Review{Author: "alice", State: "CHANGES_REQUESTED", Body: "Please fix the parser.",
					Inline: []fakeforge.Inline{{Path: "parser.go", Line: 12, Body: "This loops forever."}}})
				p.CommentAs("hands-bot", "[hands-bot review] reviewed at head "+fakeforge.HeadCommit+"\n\nVERDICT\nREADY\x1b[0m")
				p.Review(fakeforge.Review{Author: "bob", State: "REQUEST_REVIEW"})
				p.Checks = []fakeforge.Check{
					{Name: "go-lint", Status: "completed", Conclusion: "success"},
					{Name: "go-test", Status: "completed", Conclusion: "failure", Log: goTestLog(40)},
					{Name: "go-race\x1b[2J\nforged: line", Status: "in_progress"},
				}
			})
			_, content, err = c.GetPullContent(ctx, f.Host, k, fakeforge.Token, fakeforge.Repo, 1)
			if err != nil {
				t.Fatal(err)
			}
			var got []string
			for _, cm := range content.Comments {
				line := cm.Kind + " " + cm.Author
				if cm.Review != nil {
					line += fmt.Sprintf(" %s@%s:%v", cm.Review.Reviewer, cm.Review.Head[:7], cm.Review.AtHead)
				}
				if cm.Path != "" {
					line += fmt.Sprintf(" %s:%d", cm.Path, cm.Line)
				}
				got = append(got, line)
			}
			review := "review alice"
			if k == GitLab {
				review = "comment alice" // GitLab keeps a review's body as a note
			}
			want := []string{"comment member local@" + fakeforge.HeadCommit[:7] + ":true", "comment hands-bot external@0123456:false",
				review, "inline alice parser.go:12", "comment hands-bot external@" + fakeforge.HeadCommit[:7] + ":true"}
			if strings.Join(got, "\n") != strings.Join(want, "\n") {
				t.Errorf("comments:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
			}
			for _, cm := range content.Comments {
				if cm.At.IsZero() || cm.Body == "" || strings.Contains(cm.Body, "\x1b") {
					t.Errorf("comment %+v: no time, no body, or an escape", cm)
				}
				if cm.Kind == CommentReview && cm.State != "CHANGES_REQUESTED" {
					t.Errorf("a review's state: %q", cm.State)
				}
			}
			states := map[string]string{}
			var failed Check
			for _, ch := range content.Checks {
				states[ch.Name] = ch.State
				if ch.Name == "go-test" {
					failed = ch
				}
			}
			if states["go-lint"] != ChecksSuccess || states["go-test"] != ChecksFailure || states["go-race [2J forged: line"] != ChecksPending {
				t.Errorf("checks: %+v", content.Checks)
			}
			if failed.Log == nil {
				t.Fatalf("a failed check without a log fact: %+v", failed)
			}
			if k == Gitea {
				if failed.Log.Excerpt != "" || failed.Log.None == "" {
					t.Errorf("Gitea's failed status: %+v", failed.Log)
				}
			} else if !strings.HasPrefix(failed.Log.Excerpt, "setup line 31\n") ||
				!strings.Contains(failed.Log.Excerpt, "--- FAIL: TestParser") || !strings.Contains(failed.Log.Excerpt, "##[error]") ||
				!failed.Log.Cut || strings.ContainsAny(failed.Log.Excerpt, "\x1b\r") {
				t.Errorf("the excerpt:\n%s\n(cut %v)", failed.Log.Excerpt, failed.Log.Cut)
			}
			if f.Unauthorized != 0 || f.LogAuthorized != 0 {
				t.Errorf("%d requests went without the token, %d log requests with it", f.Unauthorized, f.LogAuthorized)
			}
		})
	}
}

// GitHub's check runs are read across pages: a failure on the second page
// is the head's conclusion, and its log is read.
func TestCheckRunPages(t *testing.T) {
	ctx := context.Background()
	f := fakeforge.New(t, "github")
	c := &Client{HTTP: f.Client}
	if _, err := c.OpenPull(ctx, f.Host, GitHub, fakeforge.Token, fakeforge.Repo, NewPull{Title: "Fix", Head: "feat/x", Base: "main"}); err != nil {
		t.Fatal(err)
	}
	checks := make([]fakeforge.Check, 150)
	for i := range checks {
		checks[i] = fakeforge.Check{Name: fmt.Sprintf("job-%03d", i), Status: "completed", Conclusion: "success"}
	}
	checks[120] = fakeforge.Check{Name: "job-120", Status: "completed", Conclusion: "failure", Log: "panic: boom\n"}
	f.SetPull(1, func(p *fakeforge.Pull) { p.Checks = checks })
	st, err := c.GetPullStatus(ctx, f.Host, GitHub, fakeforge.Token, fakeforge.Repo, 1)
	if err != nil || st.Checks != ChecksFailure {
		t.Fatalf("status: %+v, %v", st, err)
	}
	_, content, err := c.GetPullContent(ctx, f.Host, GitHub, fakeforge.Token, fakeforge.Repo, 1)
	if err != nil || len(content.Checks) != 150 {
		t.Fatalf("%d checks, %v", len(content.Checks), err)
	}
	if ch := content.Checks[120]; ch.State != ChecksFailure || ch.Log == nil || ch.Log.Excerpt != "panic: boom" || ch.Log.Cut {
		t.Errorf("the failure on page 2: %+v %+v", ch, ch.Log)
	}
}

// A log the forge refuses, a check run of another app, and failed jobs past
// the excerpt limit each say why they carry no excerpt.
func TestNoExcerpt(t *testing.T) {
	ctx := context.Background()
	f := fakeforge.New(t, "github")
	c := &Client{HTTP: f.Client}
	if _, err := c.OpenPull(ctx, f.Host, GitHub, fakeforge.Token, fakeforge.Repo, NewPull{Title: "Fix", Head: "feat/x", Base: "main"}); err != nil {
		t.Fatal(err)
	}
	checks := []fakeforge.Check{{Name: "external", Status: "completed", Conclusion: "failure", NotActions: true}}
	for i := range maxExcerpts + 1 {
		checks = append(checks, fakeforge.Check{Name: fmt.Sprintf("job-%d", i), Status: "completed", Conclusion: "timed_out", Log: "error: slow\n"})
	}
	f.SetPull(1, func(p *fakeforge.Pull) { p.Checks = checks })
	_, content, err := c.GetPullContent(ctx, f.Host, GitHub, fakeforge.Token, fakeforge.Repo, 1)
	if err != nil {
		t.Fatal(err)
	}
	if l := content.Checks[0].Log; l == nil || !strings.Contains(l.None, "not a GitHub Actions job") {
		t.Errorf("another app's run: %+v", l)
	}
	for i, ch := range content.Checks[1:] {
		want := "error: slow"
		if i == maxExcerpts {
			if ch.Log == nil || !strings.Contains(ch.Log.None, "only the first") {
				t.Errorf("job %d past the limit: %+v", i, ch.Log)
			}
			continue
		}
		if ch.Conclusion != "timed_out" || ch.Log == nil || ch.Log.Excerpt != want {
			t.Errorf("job %d: %+v %+v", i, ch, ch.Log)
		}
	}
	f.LogsForbidden = true
	if _, content, err = c.GetPullContent(ctx, f.Host, GitHub, fakeforge.Token, fakeforge.Repo, 1); err != nil {
		t.Fatal(err)
	}
	if l := content.Checks[1].Log; l == nil || !strings.Contains(l.None, "refuses") {
		t.Errorf("a refused log: %+v", l)
	}
}

// An excerpt is at most MaxExcerpt bytes of whole lines, from a few lines
// before the first failure, else the runner's error, else the log's end.
func TestExcerpt(t *testing.T) {
	long := strings.Repeat("x", 100)
	var lines []string
	for i := range 500 {
		lines = append(lines, fmt.Sprintf("%03d %s", i, long))
	}
	withFail := append(append([]string{}, lines[:200]...), "--- FAIL: TestX")
	withFail = append(withFail, lines[200:]...)
	for _, c := range []struct {
		name, log, start string
		cut              bool
	}{
		{"failure", strings.Join(withFail, "\n"), "190 ", true},
		{"runner error", strings.Join(lines[:50], "\n") + "\n##[error]exit 1\n", "040 ", true},
		{"no error", strings.Join(lines, "\n"), "", true},
		{"short", "one\nerror: two\nthree\n", "one", false},
		{"an error in a word is none", "fail-on-error: true\nall good\n", "fail-on-error", false},
		// A key ending in error: before a long setup does not hide the
		// later failure.
		{"a key is no error", "fail-on-error: true\n" + strings.Join(withFail, "\n"), "190 ", true},
		{"a tool's error", strings.Join(lines[:30], "\n") + "\nmain.go:3:2: error: undefined\n", "020 ", true},
	} {
		got, cut := excerpt(c.log)
		if len(got) > MaxExcerpt || cut != c.cut || !strings.HasPrefix(got, c.start) {
			t.Errorf("%s: %d bytes, cut %v, starts %q", c.name, len(got), cut, got[:min(20, len(got))])
		}
		if c.name == "no error" && !strings.HasSuffix(got, lines[499]) {
			t.Errorf("no error: the excerpt does not end the log")
		}
	}
	if got, cut := excerpt(strings.Repeat("é", MaxExcerpt)); len(got) > MaxExcerpt || !cut || !strings.HasPrefix(got, "é") ||
		strings.ContainsRune(got, '�') {
		t.Errorf("one long line: %d bytes, cut %v", len(got), cut)
	}
	if got, _ := excerpt(""); got != "" {
		t.Errorf("an empty log: %q", got)
	}
}

// Only a header on the body's first line names a review; its head is
// compared with the pull request's.
func TestReviewHeader(t *testing.T) {
	head := fakeforge.HeadCommit
	for _, c := range []struct {
		body           string
		reviewer, lvl  string
		atHead, isNone bool
	}{
		{"[oh-code-review (max) review] reviewed at head " + head[:12] + "\nbody", ReviewerLocal, "max", true, false},
		{"\n  [oh-code-review review] reviewed at head " + head, ReviewerLocal, "", true, false},
		{"[hands-bot review] reviewed at head 0123456", ReviewerExternal, "", false, false},
		{"Quoting: [hands-bot review] reviewed at head " + head, "", "", false, true},
		{"[hands-bot review] reviewed at head XYZ", "", "", false, true},
	} {
		h := reviewHeader(c.body, head)
		if c.isNone {
			if h != nil {
				t.Errorf("%q read as %+v", c.body, h)
			}
			continue
		}
		if h == nil || h.Reviewer != c.reviewer || h.Level != c.lvl || h.AtHead != c.atHead {
			t.Errorf("%q read as %+v", c.body, h)
		}
	}
}
