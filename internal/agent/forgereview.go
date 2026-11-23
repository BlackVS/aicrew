package agent

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
)

// A member's review is read by the gates in one form only: a first line
// "[oh-code-review (LEVEL) review] reviewed at head FULL_SHA", a VERDICT
// line with the verdict alone on the next non-blank line, and the review
// skill's attribution line (internal/forge/content.go, reviewShape; the
// review skill set's gates and the delivery check read the same form). A
// model that writes the header itself can choose its own, and its review is
// then counted by none of them (task 01a1fa07-5db2, pull request #279), so
// `forge pr review` writes the header and `forge pr comment` refuses a body
// that is a review.

// ReviewHeaderForm is the header line the gates read, in words.
const ReviewHeaderForm = "[oh-code-review (high) review] reviewed at head FULL_SHA"

var (
	// headerLike is a first line shaped like any review's header:
	// "[NAME] reviewed at head", whatever the name, after an optional
	// Markdown heading or emphasis mark, in any case. A quotation ("> ...")
	// is not a header.
	headerLike = regexp.MustCompile(`(?i)^[\s#*_]*\[[^\]\n]{1,100}\][\s*_]*reviewed at head\b`)
	// fence opens or closes a Markdown code block.
	fence = regexp.MustCompile("^\\s{0,3}(```|~~~)")
	// verdictHeading is a line that is the verdict's heading alone.
	verdictHeading = regexp.MustCompile(`^[\s#*_]*VERDICT[\s*_:]*$`)
	// verdictLabel is a line that names the verdict after a label.
	verdictLabel = regexp.MustCompile(`^[\s#*_]*VERDICT[\s*_]*:[\s*_]*(READY_FOR_HUMAN_MERGE|RETURN_TO_IMPLEMENTATION|REVIEW_COULD_NOT_RUN)\b`)
	// verdictValue is a line that is a verdict word, marks around it
	// allowed.
	verdictValue = regexp.MustCompile(`^[\s*_]*(READY_FOR_HUMAN_MERGE|RETURN_TO_IMPLEMENTATION|REVIEW_COULD_NOT_RUN)[\s*_]*$`)
	// attribution is the review skill's attribution line; its level is the
	// header's.
	attribution = regexp.MustCompile(`(?m)^Review: oh-code-review \(([a-z]+)\) via \S.*$`)
	// sessionLink is a link into an agent client's session or conversation.
	sessionLink = regexp.MustCompile(`(?i)https?://(?:[a-z0-9-]+\.)*(?:claude\.ai|chatgpt\.com|chat\.openai\.com)/\S+`)
	// fullSHA is a full commit id.
	fullSHA = regexp.MustCompile(`^[0-9a-f]{40}$`)
)

// ownLines is body's lines with every quoted line ("> ...") and every
// line of a code block, its fences included, made blank: a quotation of
// another's review, or a review shown as code, is not the author's own.
func ownLines(body string) []string {
	lines := strings.Split(body, "\n")
	in := false
	for i, l := range lines {
		switch {
		case fence.MatchString(l):
			in = !in
			lines[i] = ""
		case in || strings.HasPrefix(strings.TrimLeft(l, " \t"), ">"):
			lines[i] = ""
		}
	}
	return lines
}

// reviewLike reports whether body reads as a review: its first non-empty
// line is shaped like a review's header, or it names a verdict as a
// review does (a VERDICT heading with a verdict word on the next non-blank
// line, or a VERDICT label with the word after it). Quoted lines and code
// blocks count for neither (ownLines).
func reviewLike(body string) bool {
	if first, ok := nextLine(strings.Split(body, "\n")); ok && headerLike.MatchString(first) {
		return true
	}
	lines := ownLines(body)
	for i, l := range lines {
		if verdictLabel.MatchString(l) {
			return true
		}
		if verdictHeading.MatchString(l) {
			if next, ok := nextLine(lines[i+1:]); ok && verdictValue.MatchString(next) {
				return true
			}
		}
	}
	return false
}

// nextLine is the first non-blank line of lines.
func nextLine(lines []string) (string, bool) {
	for _, l := range lines {
		if strings.TrimSpace(l) != "" {
			return l, true
		}
	}
	return "", false
}

// errReviewByComment refuses a review posted as an ordinary comment.
var errReviewByComment = errors.New("the comment is a review (a review header, or a VERDICT line with its verdict): " +
	"post your review with `aicrew-agent forge pr review --pr N --body-file FILE`, which writes the header the gates " +
	"read (" + ReviewHeaderForm + "), since a review with another header is counted by no gate; or, to quote " +
	"another's review, put each quoted line after `> `.")

// reviewLevel checks a review body as `forge pr review` posts it and
// returns its level, read from the attribution line: the body must not
// begin with a header of its own, must carry exactly one VERDICT heading
// with a verdict word alone on the next non-blank line and no VERDICT label,
// and one attribution line at level high, max or ultra, and no session
// link. The verdict is checked over every line, code blocks included: the
// gates take the first verdict line they find, inside a code block too
// (internal/forge/content.go, reviewVerdict), so a verdict shown as code
// would be read as the review's. A quoted line is no verdict line to them.
func reviewLevel(body string) (string, error) {
	if first, ok := nextLine(strings.Split(body, "\n")); !ok {
		return "", errors.New("the review is empty")
	} else if headerLike.MatchString(first) {
		return "", errors.New("the review file begins with a header of its own: remove that line, the command " +
			"writes the header (" + ReviewHeaderForm + ")")
	}
	lines := strings.Split(body, "\n")
	verdicts := 0
	for i, l := range lines {
		if verdictLabel.MatchString(l) {
			return "", errors.New("the review names its verdict after a label (VERDICT: ...), which the gates may " +
				"read first, also in a code block: give it only as a line VERDICT with the verdict alone on the next line")
		}
		if strings.TrimSpace(l) != "VERDICT" {
			continue
		}
		verdicts++
		next, ok := nextLine(lines[i+1:])
		if !ok || !verdictValue.MatchString(next) || strings.TrimSpace(next) != strings.Trim(strings.TrimSpace(next), "*_") {
			return "", errors.New("the line after VERDICT must hold only READY_FOR_HUMAN_MERGE, " +
				"RETURN_TO_IMPLEMENTATION or REVIEW_COULD_NOT_RUN")
		}
	}
	if verdicts != 1 {
		return "", fmt.Errorf("the review has %d lines VERDICT, code blocks included: it needs exactly one, alone "+
			"on its line, with the verdict alone on the next line; quote another's verdict with `> `", verdicts)
	}
	ms := attribution.FindAllStringSubmatch(body, -1)
	if len(ms) != 1 {
		return "", errors.New("the review needs exactly one attribution line `Review: oh-code-review (high) via CLIENT`")
	}
	level := ms[0][1]
	if level != "high" && level != "max" && level != "ultra" {
		return "", fmt.Errorf("the attribution names the level %q: the gates count a review at high, max or ultra", level)
	}
	if sessionLink.MatchString(body) {
		return "", errors.New("the review holds a link into an agent's session or conversation: remove it")
	}
	return level, nil
}

// Review posts body on pull request n as the member's review, with the
// header the gates read written by this command. head is the commit the
// review read; empty means the commit the checkout in f.Dir stands on. The
// review is refused unless head is the pull request's head on the forge.
func (f *ForgePR) Review(ctx context.Context, n int64, body, head string) (PullReport, error) {
	level, err := reviewLevel(body)
	if err != nil {
		return PullReport{}, err
	}
	t, err := f.target(ctx)
	if err != nil {
		return PullReport{}, err
	}
	if t.holds(body, head) {
		return PullReport{}, errHoldsToken
	}
	from := "--head"
	if head == "" {
		from = "your checkout's HEAD"
		if head, err = f.git(ctx, "rev-parse", "--verify", "HEAD^{commit}"); err != nil || head == "" {
			return PullReport{}, errors.New("this directory is no checkout with a commit: run the command in the " +
				"worktree whose commit you reviewed, or pass --head SHA")
		}
	}
	head = strings.ToLower(head)
	if !fullSHA.MatchString(head) {
		return PullReport{}, fmt.Errorf("%s is %q, not a full commit id", from, head)
	}
	rep, err := f.show(ctx, t, n)
	if err != nil {
		return PullReport{}, err
	}
	if forgeHead := rep.Pull.HeadCommit; forgeHead != head {
		return PullReport{}, fmt.Errorf("you reviewed %s (%s), but the head of pull request %d is %s on the forge: "+
			"review the forge's head, or push the commit you reviewed, then post the review", head, from, n, forgeHead)
	}
	header := "[oh-code-review (" + level + ") review] reviewed at head " + head
	c, err := f.API.CommentPull(ctx, t.host, t.kind(), t.token, t.path, n, header+"\n\n"+strings.TrimLeft(body, "\r\n"))
	if err != nil {
		return PullReport{}, fmt.Errorf("post the review on pull request %d of %s on %s: %w", n, t.path, t.host, err)
	}
	rep.Comment = &c
	rep.Watch = f.watch(ctx, t, n)
	return rep, nil
}
