package agent

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/BlackVS/aicrew/internal/forge"
	"github.com/BlackVS/aicrew/internal/forge/fakeforge"
)

// The skill set's two readers of a review's header, copied from its
// sources (its two verdict readers are product constants, skillVerdictWord
// and skillReady): a copy does not follow a change there by itself.
var (
	// localHeader is verify-delivery's DEFAULT_REVIEWS "local" pattern,
	// which forge-gates' gates and wait-review use too.
	localHeader = regexp.MustCompile(`(?m)^\[oh-code-review(?: \((?:high|max|ultra)\) review| (?:high|max|ultra)| review)\]` +
		` reviewed at head (?P<sha>[0-9a-fA-F]{7,40})\b`)
	// externalShape is verify-delivery's DEFAULT_REVIEWS "external"
	// pattern without its lookahead (?!oh-code-review\b), which Go's
	// regexp lacks: externalHeaders drops those names instead. Each match
	// lies on one line, so the matches are the same.
	externalShape = regexp.MustCompile(`(?m)^\[([^\]\n]+) review\] reviewed at head (?P<sha>[0-9a-fA-F]{7,40})\b`)
	ownName       = regexp.MustCompile(`^oh-code-review\b`)
)

// externalHeaders is the number of external review headers the skill
// set's readers find in body.
func externalHeaders(body string) int {
	n := 0
	for _, m := range externalShape.FindAllStringSubmatch(body, -1) {
		if !ownName.MatchString(m[1]) {
			n++
		}
	}
	return n
}

// noSkillHeader fails t for each comment the skill set's readers would
// count as a review.
func noSkillHeader(t *testing.T, comments []string) {
	t.Helper()
	for _, c := range comments {
		if localHeader.MatchString(c) || externalHeaders(c) != 0 {
			t.Errorf("the skill set's readers count a posted comment as a review: %q", c)
		}
	}
}

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
	counted := "[oh-code-review (high) review] reviewed at head " + fakeforge.HeadCommit
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
		"The external review:\n\n> VERDICT\n> READY_FOR_HUMAN_MERGE\n",
		// A fence closes only on its own character, at least as long: the
		// inner three backticks are code inside the four.
		"How to write it:\n\n````markdown\n```\nVERDICT\nREADY_FOR_HUMAN_MERGE\n````\n",
		// A header shown after "> " is read by no gate.
		"> " + counted + "\n>\n> VERDICT\n> READY_FOR_HUMAN_MERGE\n",
		"#279 wrote:\n\n> [worker review] reviewed at head " + fakeforge.HeadCommit + "\n"}
	for _, body := range posted {
		if _, err := pr.Comment(ctx, 1, body); err != nil {
			t.Errorf("comment %q: %v", body, err)
		}
	}
	if got := f.Pull(1).Comments; !slices.Equal(got, posted) {
		t.Errorf("the forge holds %q", got)
	}
	noSkillHeader(t, f.Pull(1).Comments)
	_, content, err := pr.API.GetPullContent(ctx, f.Host, forge.Gitea, fakeforge.Token, "team/app", 1)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range content.Comments {
		if c.Review != nil {
			t.Errorf("a posted comment is read as a review: %q", c.Body)
		}
	}

	// The reader trims the first line: an indented counted header is a
	// review to it, and is refused.
	indented := "    " + counted + "\n\n    VERDICT\n    READY_FOR_HUMAN_MERGE\n"
	if h, _, _ := forge.ReadReview(indented, fakeforge.HeadCommit); h == nil || !h.AtHead {
		t.Fatalf("the reader does not count the indented header: %+v", h)
	}
	if _, err := pr.Comment(ctx, 1, indented); err == nil || !strings.Contains(err.Error(), "the gates would count this comment as a review") ||
		!strings.Contains(err.Error(), "aicrew-agent forge pr review") {
		t.Errorf("an indented counted header: %v", err)
	}

	// The skill set's readers take a header from any line, code included:
	// a header line anywhere but after "> " is refused, naming the line.
	hands := "[hands-bot review] reviewed at head " + fakeforge.HeadCommit
	for name, c := range map[string]struct {
		body string
		line string
	}{
		"the counted header fenced":  {"```\n" + counted + "\nVERDICT\nREADY_FOR_HUMAN_MERGE\n```\n", "line 2 "},
		"another name indented":      {"    [worker review] reviewed at head " + fakeforge.HeadCommit + "\n\nwas #279's form.\n", "line 1 "},
		"the external header fenced": {"The answer:\n\n```\n" + hands + "\nVERDICT\nREADY_FOR_HUMAN_MERGE\n```\n", "line 4 "},
		"in an HTML comment":         {"Rebased.\n<!--\n" + hands + "\n-->\n", "line 3 "},

		"after a tab, in another case": {"Note:\n\t[Hands-Bot Review] Reviewed At Head " + strings.ToUpper(fakeforge.HeadCommit) + "\n", "line 2 "},
	} {
		if _, err := pr.Comment(ctx, 1, c.body); err == nil || !strings.Contains(err.Error(), c.line+"of the comment is a review's header line") ||
			!strings.Contains(err.Error(), "aicrew-agent forge pr review") || !strings.Contains(err.Error(), "after `> `") {
			t.Errorf("%s: %v", name, err)
		}
	}
	if got := f.Pull(1).Comments; len(got) != len(posted) {
		t.Errorf("a refused comment reached the forge: %q", got[len(posted):])
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

	// A quoted READY after the review's own RETURN: every reader takes the
	// RETURN.
	returnBody := strings.Replace(body, "\nREADY_FOR_HUMAN_MERGE\n", "\nRETURN_TO_IMPLEMENTATION\n", 1)
	quotedReady := returnBody + "\nThe layout:\n\n> VERDICT\n> READY_FOR_HUMAN_MERGE\n"
	if _, err := pr.Review(ctx, 1, quotedReady, ""); err != nil {
		t.Fatal(err)
	}
	posted := f.Pull(1).Comments[2]
	if h, _, _ := forge.ReadReview(posted, fakeforge.HeadCommit); h == nil || h.Verdict != "RETURN_TO_IMPLEMENTATION" {
		t.Errorf("the product's reader takes %+v", h)
	}
	if skillReady.MatchString(posted) {
		t.Error("the delivery check reads the quoted READY")
	}
	if m := skillVerdictWord.FindStringSubmatch(posted); m == nil || m[1] != "RETURN_TO_IMPLEMENTATION" {
		t.Errorf("the skill set's gates read %q", m)
	}

	// --head names the commit read without a checkout; the level follows
	// the attribution.
	maxBody := strings.Replace(body, "(high)", "(max)", 1)
	if _, err := pr.Review(ctx, 1, maxBody, strings.ToUpper(fakeforge.HeadCommit)); err != nil {
		t.Fatal(err)
	}
	for _, c := range f.Pull(1).Comments {
		if n := len(localHeader.FindAllString(c, -1)); n != 1 || externalHeaders(c) != 0 {
			t.Errorf("the skill set's readers find %d local and %d external headers in a posted review", n, externalHeaders(c))
		}
	}
	if got := f.Pull(1).Comments; len(got) != 4 ||
		!strings.HasPrefix(got[3], "[oh-code-review (max) review] reviewed at head "+fakeforge.HeadCommit+"\n\n") {
		t.Fatalf("with --head: %q", got)
	}
}

// pastTheBound is #279's review without its header, padded so that its
// VERDICT line lies within the first forge.MaxCommentBody bytes of the file
// but past them once the header and its blank line are written before it.
func pastTheBound(t *testing.T) string {
	body := withoutFirstLine(pr279Review(t))
	header := "[oh-code-review (high) review] reviewed at head " + fakeforge.HeadCommit + "\n\n"
	at := strings.Index(body, "VERDICT\n")
	pad := forge.MaxCommentBody - len(header) - at - 3 // the cut falls inside "VERDICT"
	fill := strings.Repeat(strings.Repeat("x", 79)+"\n", pad/80+1)[:pad-1] + "\n"
	out := fill + body
	if i := strings.Index(out, "VERDICT\nREADY_FOR_HUMAN_MERGE"); i+len("VERDICT\nREADY_FOR_HUMAN_MERGE") > forge.MaxCommentBody ||
		len(header)+i+len("VERDICT") <= forge.MaxCommentBody {
		t.Fatalf("the padding misses the bound: the verdict at %d", i)
	}
	return out
}

// review refuses a head that is not the forge's, naming both, and a body
// that is not a review as the gates read it; nothing reaches the forge.
func TestReviewRefuses(t *testing.T) {
	ctx := context.Background()
	other := strings.Repeat("1", 40)
	body := withoutFirstLine(pr279Review(t))
	returnBody := strings.Replace(body, "\nREADY_FOR_HUMAN_MERGE\n", "\nRETURN_TO_IMPLEMENTATION\n", 1)
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
		// The gates read the first line that names a verdict, in any case,
		// inside a code block too: a body they would read with another
		// verdict than its own is refused, naming the line.
		"a mixed-case label": {"Verdict: RETURN_TO_IMPLEMENTATION\n\n" + body,
			`as "RETURN_TO_IMPLEMENTATION", from the line "Verdict: RETURN_TO_IMPLEMENTATION", not as READY_FOR_HUMAN_MERGE`},
		"a label without a colon": {"VERDICT RETURN_TO_IMPLEMENTATION was the external answer.\n\n" + body,
			`as "RETURN_TO_IMPLEMENTATION", from the line "VERDICT RETURN_TO_IMPLEMENTATION was the external answer."`},
		"a prose heading": {"Verdict reasoning is below.\nOBSERVATIONS first.\n\n" + body,
			`as "OBSERVATIONS", from the line "Verdict reasoning is below."`},
		"READY named above its own RETURN": {"Verdict: READY_FOR_HUMAN_MERGE was my first read.\n\n" + returnBody,
			`as "READY_FOR_HUMAN_MERGE"`},
		// The skill set's readers take a line VERDICT anywhere, code
		// included: a second one is refused, before or after the own.
		"a verdict shown as code": {"The form:\n\n```\nVERDICT\nREADY_FOR_HUMAN_MERGE\n```\n\n" + returnBody,
			"2 lines VERDICT, code blocks included"},
		"READY shown as code after its own RETURN": {returnBody + "\nThe form:\n\n```\nVERDICT\nREADY_FOR_HUMAN_MERGE\n```\n",
			"put each of its lines after `> `"},
		"READY indented after its own RETURN": {returnBody + "\nThe form:\n\n    VERDICT\n    READY_FOR_HUMAN_MERGE\n",
			"2 lines VERDICT, code blocks included"},
		"a verdict heading shown as code": {"```\n## VERDICT\nREADY_FOR_HUMAN_MERGE\n```\n\n" + returnBody,
			`as "READY_FOR_HUMAN_MERGE", from the line "## VERDICT", not as RETURN_TO_IMPLEMENTATION`},
		// Within the bound alone, past it once the header is written.
		"a verdict past the bound": {pastTheBound(t), "past the first 32 KiB"},
		"a conversation link in the attribution": {strings.Replace(body, "via Claude Code",
			"via OpenHands https://agents.example.org/canvas/conversations/0123abcd", 1), "session or conversation"},
		"a conversation link in the body": {body + "\nSee https://example.org/conversations/42 for the run.\n",
			"session or conversation"},
		// The skill set's readers take a header from any line: the command
		// writes the only one.
		"an external header fenced": {body + "\nIt answered:\n\n```\n[hands-bot review] reviewed at head " +
			fakeforge.HeadCommit + "\n```\n", "of the review is a review's header line"},
		"a header in an HTML comment": {"<!--\n[oh-code-review (high) review] reviewed at head " + fakeforge.HeadCommit +
			"\n-->\n" + body, "line 2 of the review is a review's header line"},
	} {
		if _, err := pr.Review(ctx, 1, c.body, fakeforge.HeadCommit); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: %v, want %q", name, err, c.want)
		}
	}
	if got := f.Pull(1).Comments; len(got) != 0 {
		t.Fatalf("a refused review reached the forge: %q", got)
	}
}

// odd are characters a pasted or generated file may hold that readers
// treat differently: as a space or not, as a line end or not, or dropped.
var odd = map[string]string{
	"U+00A0": "\u00a0", "VT": "\v", "FF": "\f", "U+0085": "\u0085", "U+2028": "\u2028",
	"U+2029": "\u2029", "U+200B": "\u200b", "U+FEFF": "\ufeff", "bare CR": "\r", "NUL": "\x00",
}

// Whatever review or comment posts, every reader reads as the command
// means it; anything else is refused. Each odd character is put at each
// place a reader's pattern looks at, and each case either is refused or
// posts a text that the product's reader and the skill set's readers
// (header, verdict word, READY) all read the same way.
func TestPostedTextSatisfiesTheReaders(t *testing.T) {
	ctx := context.Background()
	f, pr := reviewTest(t, fakeforge.HeadCommit)
	ready := withoutFirstLine(pr279Review(t))
	hands := "[hands-bot review] reviewed at head " + fakeforge.HeadCommit
	counted := "[oh-code-review (high) review] reviewed at head " + fakeforge.HeadCommit
	at := func(body, old, new string) string {
		if strings.Count(body, old) != 1 {
			t.Fatalf("%q is not once in the body", old)
		}
		return strings.Replace(body, old, new, 1)
	}
	posted, refused := 0, 0
	for _, verdict := range []string{"READY_FOR_HUMAN_MERGE", "RETURN_TO_IMPLEMENTATION"} {
		body := at(ready, "\nREADY_FOR_HUMAN_MERGE\n", "\n"+verdict+"\n")
		for name, c := range odd {
			for place, text := range map[string]string{
				"before VERDICT":         at(body, "\nVERDICT\n", "\n"+c+"VERDICT\n"),
				"after VERDICT":          at(body, "\nVERDICT\n", "\nVERDICT"+c+"\n"),
				"alone between":          at(body, "\nVERDICT\n", "\nVERDICT\n"+c+"\n"),
				"before the word":        at(body, "\nVERDICT\n", "\nVERDICT\n"+c),
				"after the word":         at(body, "\n"+verdict+"\n", "\n"+verdict+c+"\n"),
				"inside a fenced header": body + "\n```\n[hands" + c + "-bot review] reviewed at head " + fakeforge.HeadCommit + "\n```\n",
				"before a fenced header": body + "\n```\n" + c + hands + "\n```\n",
			} {
				before := len(f.Pull(1).Comments)
				_, err := pr.Review(ctx, 1, text, "")
				comments := f.Pull(1).Comments
				if err != nil {
					refused++
					if len(comments) != before {
						t.Errorf("review, %s %s, %s: refused but posted", name, place, verdict)
					}
					continue
				}
				posted++
				if len(comments) != before+1 {
					t.Fatalf("review, %s %s, %s: no comment", name, place, verdict)
				}
				out := comments[before]
				if n := len(localHeader.FindAllString(out, -1)); n != 1 || externalHeaders(out) != 0 {
					t.Errorf("review, %s %s, %s: %d local and %d external headers", name, place, verdict, n, externalHeaders(out))
				}
				if m := skillVerdictWord.FindStringSubmatch(out); m == nil || m[1] != verdict {
					t.Errorf("review, %s %s, %s: the skill set's gates read %q", name, place, verdict, m)
				}
				if ok := skillReady.MatchString(out); ok != (verdict == "READY_FOR_HUMAN_MERGE") {
					t.Errorf("review, %s %s, %s: the delivery check reads READY: %v", name, place, verdict, ok)
				}
				if h, _, _ := forge.ReadReview(out, fakeforge.HeadCommit); h == nil || h.Reviewer != forge.ReviewerLocal ||
					h.Level != "high" || !h.AtHead || h.Verdict != verdict {
					t.Errorf("review, %s %s, %s: the product's reader reads %+v", name, place, verdict, h)
				}
			}
		}
	}
	for name, c := range odd {
		for place, text := range map[string]string{
			"inside a fenced header": "```\n[oh-code-review" + c + " (high) review] reviewed at head " + fakeforge.HeadCommit + "\n```\n",
			"before a fenced header": "```\n" + c + counted + "\n```\n",
			"inside a header at column 0": "[oh-code-review" + c + " (high) review] reviewed at head " + fakeforge.HeadCommit +
				"\n\nVERDICT\nREADY_FOR_HUMAN_MERGE\n",
			"before a header at column 0": c + counted + "\n\nVERDICT\nREADY_FOR_HUMAN_MERGE\n",
		} {
			before := len(f.Pull(1).Comments)
			_, err := pr.Comment(ctx, 1, text)
			comments := f.Pull(1).Comments
			if err != nil {
				refused++
				if len(comments) != before {
					t.Errorf("comment, %s %s: refused but posted", name, place)
				}
				continue
			}
			posted++
			out := comments[before]
			if localHeader.MatchString(out) || externalHeaders(out) != 0 {
				t.Errorf("comment, %s %s: the skill set's readers count it as a review", name, place)
			}
			if h, _, _ := forge.ReadReview(out, fakeforge.HeadCommit); h != nil {
				t.Errorf("comment, %s %s: the product's reader counts it: %+v", name, place, h)
			}
		}
	}
	t.Logf("%d cases posted, %d refused", posted, refused)
	if posted == 0 || refused == 0 {
		t.Errorf("%d cases posted, %d refused: the test no longer tells the two apart", posted, refused)
	}
}

// A bare carriage return is refused by both commands, whatever it
// separates; a CRLF body posts.
func TestBareCarriageReturn(t *testing.T) {
	ctx := context.Background()
	f, pr := reviewTest(t, fakeforge.HeadCommit)
	body := withoutFirstLine(pr279Review(t))
	inName := body + "\n```\n[hands\rbot review] reviewed at head " + fakeforge.HeadCommit + "\n```\n"
	if externalHeaders(inName) != 1 {
		t.Fatalf("the skill set's external pattern does not read the header: %q", inName)
	}
	if _, err := pr.Review(ctx, 1, inName, ""); err == nil || !strings.Contains(err.Error(), "the review holds a carriage return without a line feed") {
		t.Errorf("review, a CR inside a header's name: %v", err)
	}
	for _, c := range []string{"Rebased.\r[hands-bot review] reviewed at head " + fakeforge.HeadCommit + "\r\n",
		"Rebased.\n```\n[hands\rbot review] reviewed at head " + fakeforge.HeadCommit + "\n```\n", "One.\rTwo.\n"} {
		if _, err := pr.Comment(ctx, 1, c); err == nil || !strings.Contains(err.Error(), "the comment holds a carriage return without a line feed") {
			t.Errorf("comment %q: %v", c, err)
		}
	}
	if got := f.Pull(1).Comments; len(got) != 0 {
		t.Fatalf("a refused body reached the forge: %q", got)
	}
	crlf := strings.ReplaceAll(body, "\n", "\r\n")
	if _, err := pr.Review(ctx, 1, crlf, ""); err != nil {
		t.Errorf("review, CRLF line ends: %v", err)
	}
	if _, err := pr.Comment(ctx, 1, "Rebased.\r\nThe tests pass.\r\n"); err != nil {
		t.Errorf("comment, CRLF line ends: %v", err)
	}
}

// A non-breaking space around VERDICT or its word passes the trimmed
// count and the product's reader, but not the skill set's readers: the
// review is refused.
func TestReviewRefusesAVerdictTheSkillsMiss(t *testing.T) {
	ctx := context.Background()
	f, pr := reviewTest(t, fakeforge.HeadCommit)
	body := withoutFirstLine(pr279Review(t))
	skills := "the skill set's gates and delivery check would not read READY_FOR_HUMAN_MERGE"
	for name, c := range map[string]struct{ text, want string }{
		"before VERDICT": {strings.Replace(body, "\nVERDICT\n", "\n\u00a0VERDICT\n", 1), skills},
		"after VERDICT":  {strings.Replace(body, "\nVERDICT\n", "\nVERDICT\u00a0\n", 1), skills},
		"a line between": {strings.Replace(body, "\nVERDICT\n", "\nVERDICT\n\u00a0\n", 1), skills},
		// The word's own line allows ASCII spaces only, before the skill
		// set's readers are asked.
		"before the word": {strings.Replace(body, "\nVERDICT\n", "\nVERDICT\n\u00a0", 1), "the line after VERDICT must hold only"},
	} {
		if _, err := pr.Review(ctx, 1, c.text, ""); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: %v, want %q", name, err, c.want)
		}
	}
	if got := f.Pull(1).Comments; len(got) != 0 {
		t.Fatalf("a refused review reached the forge: %q", got)
	}
}

// skillsRead requires, for a READY review, the delivery check's READY as
// well as the gates' word: they differ on what may follow the word.
func TestSkillsReadReady(t *testing.T) {
	for text, ok := range map[string]bool{
		"x\nVERDICT\nREADY_FOR_HUMAN_MERGE\n":   true,
		"x\nVERDICT\n\t\nREADY_FOR_HUMAN_MERGE": true,
		"x\nVERDICT\nREADY_FOR_HUMAN_MERGEx\n":  false,
		"x\nVERDICT\nREADY_FOR_HUMAN_MERGE1\n":  false,
		"x\nVERDICT\n READY_FOR_HUMAN_MERGE\n":  false,
	} {
		if err := skillsRead(text, "READY_FOR_HUMAN_MERGE"); (err == nil) != ok {
			t.Errorf("%q: %v", text, err)
		}
	}
}
