package agent

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/BlackVS/aicrew/internal/forge"
	"github.com/BlackVS/aicrew/internal/forge/fakeforge"
)

// pr279Review is the review the first team's worker posted on pull request
// #279 with a header of its own, which no gate counted.
func pr279Review(t *testing.T) string {
	t.Helper()
	raw, err := os.ReadFile("testdata/pr279-review.md")
	if err != nil {
		t.Fatal(err)
	}
	return strings.ReplaceAll(string(raw), "\r\n", "\n")
}

// withoutFirstLine is body without its first line and the blank lines
// after it.
func withoutFirstLine(body string) string {
	_, rest, _ := strings.Cut(body, "\n")
	return strings.TrimLeft(rest, "\n")
}

// A body is a review when its first line is shaped like a review's header,
// whatever the reviewer's name, or when it names a verdict as a review
// does; an ordinary comment, a delta note and a quotation are not.
func TestReviewLike(t *testing.T) {
	review := pr279Review(t)
	for name, c := range map[string]struct {
		body string
		want bool
	}{
		"#279's review":                {review, true},
		"#279's review without header": {withoutFirstLine(review), true},
		"the counted header alone":     {"[oh-code-review (high) review] reviewed at head " + fakeforge.HeadCommit + "\n", true},
		"another name, a heading mark": {"## [Worker Review] reviewed at head abc1234\n\nLooks fine.\n", true},
		"a verdict label":              {"Done.\n\n**VERDICT:** READY_FOR_HUMAN_MERGE\n", true},
		"a verdict heading":            {"Checked.\n\n## VERDICT\n\nRETURN_TO_IMPLEMENTATION\n", true},
		"an ordinary comment":          {"Rebased on main; the tests pass again.\n", false},
		"a delta note": {"Base-only update: the old head 1111111111111111111111111111111111111111, the new head " +
			"2222222222222222222222222222222222222222. Patch identities, before and after: aaaa1111 aaaa1111 " +
			"(git diff $(git merge-base origin/main HEAD) HEAD | git patch-id --stable). The reviews stand.\n", false},
		"a quoted external verdict": {"The external review answered:\n\n> [hands-bot review] reviewed at head " +
			fakeforge.HeadCommit + "\n> VERDICT\n> READY_FOR_HUMAN_MERGE\n\nI take it as it is.\n", false},
		"a quoted header first": {"> [hands-bot review] reviewed at head " + fakeforge.HeadCommit + "\n\nNoted.\n", false},
		"a review shown as code": {"The form the gates read:\n\n```\n[oh-code-review (high) review] reviewed at head SHA\n" +
			"VERDICT\nREADY_FOR_HUMAN_MERGE\n```\n", false},
		"the word in prose": {"The verdict is up to the coordinator.\n", false},
	} {
		if got := reviewLike(c.body); got != c.want {
			t.Errorf("%s: reviewLike = %v, want %v", name, got, c.want)
		}
	}
}

// reviewTest is a pull request on a fake forge and a ForgePR whose
// checkout stands on checkout.
func reviewTest(t *testing.T, checkout string) (*fakeforge.Forge, ForgePR) {
	t.Helper()
	f := fakeforge.New(t, "gitea")
	home := t.TempDir()
	provisionTestForge(t, home, f.Host, fakeforge.Token)
	pr := ForgePR{Home: home, Repository: "https://" + f.Host + "/team/app.git", API: &forge.Client{HTTP: f.Client},
		Git: func(_ context.Context, _ string, _ []string, args ...string) (string, error) {
			if slices.Equal(args, []string{"rev-parse", "--verify", "HEAD^{commit}"}) && checkout != "" {
				return checkout + "\n", nil
			}
			return "", errors.New("git " + strings.Join(args, " "))
		},
		Step: func(context.Context, string, StepCall) (StepAnswer, error) {
			return StepAnswer{OK: true, Status: StepDone, Result: json.RawMessage(`{}`)}, nil
		}}
	if _, err := pr.Open(context.Background(), OpenPull{Title: "Fix", Head: "feat/x", Base: "main"}); err != nil {
		t.Fatal(err)
	}
	return f, pr
}

// comment refuses #279's review and names the review command and the
// quotation; it posts an ordinary comment, a delta note and a quotation of
// the external review.
func TestCommentRefusesAReview(t *testing.T) {
	ctx := context.Background()
	f, pr := reviewTest(t, "")
	_, err := pr.Comment(ctx, 1, pr279Review(t))
	if err == nil || !strings.Contains(err.Error(), "aicrew-agent forge pr review --pr N --body-file FILE") ||
		!strings.Contains(err.Error(), ReviewHeaderForm) || !strings.Contains(err.Error(), "`> `") {
		t.Fatalf("#279's review as a comment: %v", err)
	}
	if got := f.Pull(1).Comments; len(got) != 0 {
		t.Fatalf("a refused comment reached the forge: %q", got)
	}
	posted := []string{"Rebased on main; the tests pass again.\n",
		"Base-only update: old head 1111111, new head 2222222, patch identities aaaa1111 and aaaa1111.\n",
		"The external review:\n\n> VERDICT\n> READY_FOR_HUMAN_MERGE\n"}
	for _, body := range posted {
		if _, err := pr.Comment(ctx, 1, body); err != nil {
			t.Errorf("comment %q: %v", body, err)
		}
	}
	if got := f.Pull(1).Comments; !slices.Equal(got, posted) {
		t.Errorf("the forge holds %q", got)
	}
}

// review posts #279's review without its own header line under the header
// the gates read, at the level of its attribution and the checkout's
// commit, and the forge's reader counts it as the local review at the head.
func TestReviewPostsTheCountedHeader(t *testing.T) {
	ctx := context.Background()
	f, pr := reviewTest(t, fakeforge.HeadCommit)
	body := withoutFirstLine(pr279Review(t))
	rep, err := pr.Review(ctx, 1, body, "")
	if err != nil || rep.Comment == nil {
		t.Fatalf("review: %+v %v", rep, err)
	}
	want := "[oh-code-review (high) review] reviewed at head " + fakeforge.HeadCommit + "\n\n" + body
	if got := f.Pull(1).Comments; !slices.Equal(got, []string{want}) {
		t.Fatalf("the forge holds %q", got)
	}
	_, content, err := pr.API.GetPullContent(ctx, f.Host, forge.Gitea, fakeforge.Token, "team/app", 1)
	if err != nil || len(content.Comments) != 1 {
		t.Fatalf("read back: %v %+v", err, content)
	}
	if r := content.Comments[0].Review; r == nil || r.Reviewer != forge.ReviewerLocal || r.Level != "high" || !r.AtHead ||
		r.Verdict != "READY_FOR_HUMAN_MERGE" {
		t.Fatalf("the reader sees %+v", content.Comments[0].Review)
	}

	// A quoted verdict is no verdict line to the gates: the review's own
	// is the one read.
	quoted := "The external review answered:\n\n> VERDICT\n> RETURN_TO_IMPLEMENTATION\n\n" + body
	if _, err := pr.Review(ctx, 1, quoted, ""); err != nil {
		t.Fatal(err)
	}
	_, content, err = pr.API.GetPullContent(ctx, f.Host, forge.Gitea, fakeforge.Token, "team/app", 1)
	if err != nil || len(content.Comments) != 2 || content.Comments[1].Review == nil ||
		content.Comments[1].Review.Verdict != "READY_FOR_HUMAN_MERGE" {
		t.Fatalf("a quoted verdict: %v %+v", err, content)
	}

	// --head names the commit read without a checkout; the level follows
	// the attribution.
	maxBody := strings.Replace(body, "(high)", "(max)", 1)
	if _, err := pr.Review(ctx, 1, maxBody, strings.ToUpper(fakeforge.HeadCommit)); err != nil {
		t.Fatal(err)
	}
	if got := f.Pull(1).Comments; len(got) != 3 ||
		!strings.HasPrefix(got[2], "[oh-code-review (max) review] reviewed at head "+fakeforge.HeadCommit+"\n\n") {
		t.Fatalf("with --head: %q", got)
	}
}

// review refuses a head that is not the forge's, naming both, and a body
// that is not a review as the gates read it; nothing reaches the forge.
func TestReviewRefuses(t *testing.T) {
	ctx := context.Background()
	other := strings.Repeat("1", 40)
	body := withoutFirstLine(pr279Review(t))
	f, pr := reviewTest(t, other)
	if _, err := pr.Review(ctx, 1, body, ""); err == nil || !strings.Contains(err.Error(), other) ||
		!strings.Contains(err.Error(), fakeforge.HeadCommit) || !strings.Contains(err.Error(), "your checkout's HEAD") {
		t.Errorf("the checkout on another commit: %v", err)
	}
	if _, err := pr.Review(ctx, 1, body, other); err == nil || !strings.Contains(err.Error(), "--head") ||
		!strings.Contains(err.Error(), fakeforge.HeadCommit) {
		t.Errorf("--head another commit: %v", err)
	}
	if _, err := pr.Review(ctx, 1, body, fakeforge.HeadCommit[:12]); err == nil || !strings.Contains(err.Error(), "not a full commit id") {
		t.Errorf("an abbreviated --head: %v", err)
	}
	for name, c := range map[string]struct{ body, want string }{
		"its own header":       {pr279Review(t), "begins with a header of its own"},
		"no verdict":           {strings.Replace(body, "VERDICT\n", "", 1), "0 lines VERDICT"},
		"a verdict with prose": {strings.Replace(body, "\nREADY_FOR_HUMAN_MERGE\n", "\nREADY_FOR_HUMAN_MERGE, no blocker\n", 1), "the line after VERDICT"},
		"no attribution":       {strings.Replace(body, "Review: oh-code-review (high) via Claude Code", "", 1), "attribution line"},
		"a medium review":      {strings.Replace(body, "(high)", "(medium)", 1), `level "medium"`},
		"a session link":       {body + "\nhttps://claude.ai/code/session_x\n", "session"},
		"the token":            {body + "\n" + fakeforge.Token + "\n", "credential"},
		"empty":                {"\n\n", "empty"},
		// The gates read the first verdict, inside a code block too.
		"a verdict shown as code": {"The form:\n\n```\nVERDICT\nREADY_FOR_HUMAN_MERGE\n```\n\n" +
			strings.Replace(body, "\nREADY_FOR_HUMAN_MERGE\n", "\nRETURN_TO_IMPLEMENTATION\n", 1), "2 lines VERDICT"},
		"a verdict label as code": {"```\nVERDICT: READY_FOR_HUMAN_MERGE\n```\n\n" + body, "after a label"},
	} {
		if _, err := pr.Review(ctx, 1, c.body, fakeforge.HeadCommit); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: %v, want %q", name, err, c.want)
		}
	}
	if got := f.Pull(1).Comments; len(got) != 0 {
		t.Fatalf("a refused review reached the forge: %q", got)
	}
}
