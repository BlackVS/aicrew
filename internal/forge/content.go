package forge

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// A pull request's content (docs/DESIGN-TURNS.md, section 4.1; pilot finding
// 2026-10-15: a worker woken by a forge event could not read the review it
// was woken for, nor the failed check). Beside its state, a pull request
// carries its comments, with their bodies and authors, and the checks of its
// head commit, each with its conclusion and, for a failed job, an excerpt of
// its log. All of it is read with the member's own token; a job's log is
// fetched from the place the forge redirects to without the token, which
// never leaves the forge's host.

// The kinds of a pull request comment.
const (
	// CommentIssue is a comment on the conversation: GitHub's and Gitea's
	// issue comments, GitLab's notes.
	CommentIssue = "comment"
	// CommentReview is a submitted review with its verdict state.
	CommentReview = "review"
	// CommentInline is a comment on a line of the diff.
	CommentInline = "inline"
)

// The reviewers a review comment's header line names.
const (
	// ReviewerLocal is the author's own review: "[oh-code-review (LEVEL)
	// review] reviewed at head SHA".
	ReviewerLocal = "local"
	// ReviewerExternal is the external reviewer's: "[hands-bot review]
	// reviewed at head SHA".
	ReviewerExternal = "external"
)

// Bounds on what is read and kept.
const (
	// MaxCommentBody bounds one comment's body; a longer one is cut.
	MaxCommentBody = 32 << 10
	// MaxExcerpt bounds one failed job's log excerpt.
	MaxExcerpt = 8 << 10
	// maxExcerpts bounds how many failed jobs' logs one read fetches.
	maxExcerpts = 5
	// maxLog bounds how much of one job's log is read to find its excerpt.
	maxLog = 8 << 20
	// maxPages bounds the pages read of one list; contentPage is GitHub's
	// and GitLab's page size, giteaPage Gitea's.
	maxPages    = 10
	contentPage = 100
	giteaPage   = 50
	// excerptContext is the lines kept before a log's first error.
	excerptContext = 10
)

// PullComment is one comment on a pull request.
type PullComment struct {
	Kind   string    `json:"kind"`
	Author string    `json:"author"`
	At     time.Time `json:"at"`
	URL    string    `json:"url,omitempty"`
	// State is a review's verdict as the forge words it, such as APPROVED
	// or CHANGES_REQUESTED.
	State string `json:"state,omitempty"`
	// Path and Line place an inline comment.
	Path string `json:"path,omitempty"`
	Line int64  `json:"line,omitempty"`
	Body string `json:"body"`
	// Cut says the body was longer than MaxCommentBody.
	Cut bool `json:"cut,omitempty"`
	// Review is what the body's header line says, when it is a review
	// posted as a comment.
	Review *ReviewHeader `json:"review,omitempty"`
}

// ReviewHeader is a review comment's header line.
type ReviewHeader struct {
	Reviewer string `json:"reviewer"`
	// Level is a local review's level, such as high.
	Level string `json:"level,omitempty"`
	// Head is the commit the header names; AtHead says it is the pull
	// request's head commit.
	Head   string `json:"head"`
	AtHead bool   `json:"at_head"`
	// Verdict is the review's verdict, such as READY_FOR_HUMAN_MERGE or
	// RETURN_TO_IMPLEMENTATION, as its verdict line or the line after it
	// names it; "" when the body names none.
	Verdict string `json:"verdict,omitempty"`
}

// Check is one check of the head commit: a GitHub check run or commit
// status, a Gitea commit status, or a job of GitLab's head pipeline.
type Check struct {
	Name string `json:"name"`
	// State is ChecksPending, ChecksSuccess or ChecksFailure.
	State string `json:"state"`
	// Conclusion is the forge's own word, such as timed_out or canceled.
	Conclusion string `json:"conclusion,omitempty"`
	URL        string `json:"url,omitempty"`
	// Log is a failed check's log excerpt, or why there is none.
	Log *JobLog `json:"log,omitempty"`
	// JobID is the failed job's ID on the forge: a GitHub Actions job or
	// a GitLab job, which a log read and a re-run (RerunFailed) name; 0
	// for a commit status and a check that is no such job.
	JobID int64 `json:"job_id,omitempty"`
	// RunnerFailure says the failure is the runner's, not the change's
	// (RunnerFailure): the forge's conclusion or failure reason names it.
	RunnerFailure bool `json:"runner_failure,omitempty"`
}

// JobLog is a failed job's log excerpt: at most MaxExcerpt bytes, from a
// few lines before the first line that reads as an error, or the log's end
// when none does.
type JobLog struct {
	Excerpt string `json:"excerpt,omitempty"`
	// Cut says the log holds more than the excerpt.
	Cut bool `json:"cut,omitempty"`
	// None is why there is no excerpt.
	None string `json:"none,omitempty"`
}

// PullContent is a pull request's comments, oldest first, and its head
// commit's checks.
type PullContent struct {
	Checks   []Check       `json:"checks"`
	Comments []PullComment `json:"comments"`
}

// GetPullContent reads pull request n of the repository at path with its
// content.
func (c *Client) GetPullContent(ctx context.Context, host string, k Kind, token, path string, n int64) (Pull, PullContent, error) {
	var raw struct {
		wirePull
		wireStatus
	}
	if err := c.get(ctx, host, k, token, pullPath(k, path, n), &raw); err != nil {
		return Pull{}, PullContent{}, err
	}
	p, err := raw.wirePull.pull(k)
	if err != nil {
		return Pull{}, PullContent{}, err
	}
	out := PullContent{Checks: []Check{}, Comments: []PullComment{}}
	if out.Comments, err = c.pullComments(ctx, host, k, token, path, p); err != nil {
		return Pull{}, PullContent{}, fmt.Errorf("read the comments: %w", err)
	}
	switch {
	case k == GitLab && raw.HeadPipeline != nil && raw.HeadPipeline.ID > 0:
		out.Checks, err = c.pipelineJobs(ctx, host, token, path, raw.HeadPipeline.ID)
	case k != GitLab && p.HeadCommit != "":
		out.Checks, err = c.commitChecks(ctx, host, k, token, path, p.HeadCommit)
	}
	if err != nil {
		return Pull{}, PullContent{}, fmt.Errorf("read the checks: %w", err)
	}
	c.excerpts(ctx, host, k, token, path, out.Checks)
	return p, out, nil
}

// pullComments reads the comments of p, in every list the dialect keeps
// them in, oldest first.
func (c *Client) pullComments(ctx context.Context, host string, k Kind, token, path string, p Pull) ([]PullComment, error) {
	num := strconv.FormatInt(p.Number, 10)
	issue := repoPath(k, path) + "/issues/" + num + "/comments"
	pull := pullPath(k, path, p.Number)
	var out []PullComment
	switch k {
	case GitHub:
		var issues, reviews, inline []wireComment
		for _, l := range []struct {
			path string
			into *[]wireComment
		}{{issue, &issues}, {pull + "/reviews", &reviews}, {pull + "/comments", &inline}} {
			if err := pages(ctx, c, host, k, token, l.path, contentPage, l.into); err != nil {
				return nil, err
			}
		}
		out = appendComments(out, issues, CommentIssue)
		out = appendComments(out, reviews, CommentReview)
		out = appendComments(out, inline, CommentInline)
	case Gitea:
		var issues, reviews []wireComment
		if err := pages(ctx, c, host, k, token, issue, giteaPage, &issues); err != nil {
			return nil, err
		}
		if err := pages(ctx, c, host, k, token, pull+"/reviews", giteaPage, &reviews); err != nil {
			return nil, err
		}
		out = appendComments(out, issues, CommentIssue)
		out = appendComments(out, reviews, CommentReview)
		for _, r := range reviews {
			if r.CommentsCount == 0 {
				continue
			}
			var inline []wireComment
			if err := c.get(ctx, host, k, token, pull+"/reviews/"+strconv.FormatInt(r.ID, 10)+"/comments", &inline); err != nil {
				return nil, err
			}
			out = appendComments(out, inline, CommentInline)
		}
	case GitLab:
		var notes []wireComment
		if err := pages(ctx, c, host, k, token, pull+"/notes?sort=asc&order_by=created_at", contentPage, &notes); err != nil {
			return nil, err
		}
		for i := range notes {
			if notes[i].System {
				continue
			}
			kind := CommentIssue
			if notes[i].Type == "DiffNote" {
				kind = CommentInline
			}
			if notes[i].HTMLURL == "" {
				notes[i].HTMLURL = p.URL + "#note_" + strconv.FormatInt(notes[i].ID, 10)
			}
			out = appendComments(out, notes[i:i+1], kind)
		}
	}
	for i := range out {
		out[i].Review = reviewHeader(out[i].Body, p.HeadCommit)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].At.Before(out[j].At) })
	if out == nil {
		out = []PullComment{}
	}
	return out, nil
}

// wireComment is a comment, review or note in any dialect's answer.
type wireComment struct {
	ID      int64  `json:"id"`
	Body    string `json:"body"`
	HTMLURL string `json:"html_url"`
	User    *struct {
		Login string `json:"login"`
	} `json:"user"`
	Author *struct {
		Username string `json:"username"`
	} `json:"author"`
	CreatedAt   time.Time `json:"created_at"`
	SubmittedAt time.Time `json:"submitted_at"`
	State       string    `json:"state"`
	Path        string    `json:"path"`
	Line        int64     `json:"line"`
	Original    int64     `json:"original_line"`
	// Position is GitHub's and Gitea's line in the diff, and Location
	// GitLab's place of a diff note: one JSON field, read by UnmarshalJSON.
	Position int64 `json:"-"`
	// Gitea's review: how many inline comments it carries.
	CommentsCount int64 `json:"comments_count"`
	// GitLab's note: a system note, and a diff note's place.
	System   bool   `json:"system"`
	Type     string `json:"type"`
	Location *struct {
		NewPath string `json:"new_path"`
		NewLine int64  `json:"new_line"`
	} `json:"-"`
}

// UnmarshalJSON reads a comment whose position is GitHub's and Gitea's line
// number or GitLab's object.
func (w *wireComment) UnmarshalJSON(b []byte) error {
	type plain wireComment
	var v struct {
		plain
		Position any `json:"position"`
	}
	if err := json.Unmarshal(b, &v); err != nil {
		return err
	}
	*w = wireComment(v.plain)
	switch pos := v.Position.(type) {
	case float64:
		w.Position = int64(pos)
	case map[string]any:
		path, _ := pos["new_path"].(string)
		line, _ := pos["new_line"].(float64)
		w.Location = &struct {
			NewPath string `json:"new_path"`
			NewLine int64  `json:"new_line"`
		}{path, int64(line)}
	}
	return nil
}

// appendComments converts the dialect's comments of one kind. A review with
// no body and no verdict is left out: a GitHub review that only holds inline
// comments, which are listed as inline ones, and Gitea's request for a
// review. Every field but the body is one printable line.
func appendComments(out []PullComment, ws []wireComment, kind string) []PullComment {
	for _, w := range ws {
		state := strings.ToUpper(w.State)
		if kind == CommentReview && strings.TrimSpace(w.Body) == "" &&
			(state == "" || state == "COMMENTED" || state == "COMMENT" || state == "PENDING" || state == "REQUEST_REVIEW") {
			continue
		}
		pc := PullComment{Kind: kind, URL: oneLine(w.HTMLURL), At: w.CreatedAt}
		if kind == CommentReview {
			pc.State = oneLine(state)
			if !w.SubmittedAt.IsZero() {
				pc.At = w.SubmittedAt
			}
		}
		switch {
		case w.User != nil:
			pc.Author = oneLine(w.User.Login)
		case w.Author != nil:
			pc.Author = oneLine(w.Author.Username)
		}
		if kind == CommentInline {
			pc.Path, pc.Line = w.Path, first64(w.Line, w.Original, w.Position)
			if w.Location != nil {
				pc.Path, pc.Line = w.Location.NewPath, w.Location.NewLine
			}
			pc.Path = oneLine(pc.Path)
		}
		pc.Body, pc.Cut = bounded(cleanText(w.Body), MaxCommentBody)
		out = append(out, pc)
	}
	return out
}

// reviewShape is a review comment's header line: the local review's
// "[oh-code-review (high) review] reviewed at head SHA" and the external
// reviewer's "[hands-bot review] reviewed at head SHA".
var reviewShape = regexp.MustCompile(`^\[(?:oh-code-review(?: \(([a-z]+)\))? review|(hands-bot) review)\] reviewed at head ([0-9a-f]{7,40})\b`)

// reviewHeader reads body's first non-empty line as a review's header, or
// nil.
func reviewHeader(body, head string) *ReviewHeader {
	line := strings.TrimSpace(body)
	if i := strings.IndexByte(line, '\n'); i >= 0 {
		line = strings.TrimSpace(line[:i])
	}
	m := reviewShape.FindStringSubmatch(line)
	if m == nil {
		return nil
	}
	h := &ReviewHeader{Reviewer: ReviewerLocal, Level: m[1], Head: m[3]}
	if m[2] != "" {
		h.Reviewer = ReviewerExternal
	}
	h.AtHead = head != "" && strings.HasPrefix(head, h.Head)
	h.Verdict = reviewVerdict(body)
	return h
}

var (
	// verdictLine is a line that names the verdict, as a heading
	// ("VERDICT") or a label ("**Verdict:** READY_FOR_HUMAN_MERGE").
	verdictLine = regexp.MustCompile(`(?i)^[\s*#_]*verdict[\s*:_]*(.*)$`)
	// verdictWord is a verdict: an upper-case word with underscores.
	verdictWord = regexp.MustCompile(`^[*_\s]*([A-Z][A-Z_]{3,})\b`)
)

// reviewVerdict is the verdict a review body names: the word after its
// verdict label, or on the first non-empty line after a verdict heading.
func reviewVerdict(body string) string {
	lines := strings.Split(body, "\n")
	for i, l := range lines {
		m := verdictLine.FindStringSubmatch(strings.TrimSpace(l))
		if m == nil {
			continue
		}
		if w := verdictWord.FindStringSubmatch(m[1]); w != nil {
			return w[1]
		}
		for _, next := range lines[i+1:] {
			if strings.TrimSpace(next) == "" {
				continue
			}
			if w := verdictWord.FindStringSubmatch(next); w != nil {
				return w[1]
			}
			break
		}
	}
	return ""
}

// commitChecks reads GitHub's and Gitea's checks of commit sha: its commit
// statuses and, on GitHub, every page of its check runs.
func (c *Client) commitChecks(ctx context.Context, host string, k Kind, token, path, sha string) ([]Check, error) {
	commit := repoPath(k, path) + "/commits/" + url.PathEscape(sha)
	var cs struct {
		Statuses []struct {
			Context   string `json:"context"`
			State     string `json:"state"`
			Status    string `json:"status"`
			TargetURL string `json:"target_url"`
		} `json:"statuses"`
	}
	if err := c.get(ctx, host, k, token, commit+"/status", &cs); err != nil {
		return nil, err
	}
	out := []Check{}
	for _, s := range cs.Statuses {
		word := first(s.State, s.Status)
		ch := newCheck(s.Context, statusChecks(word), word, s.TargetURL)
		if ch.State == ChecksFailure {
			ch.Log = &JobLog{None: "a commit status carries no log through the forge's API; its URL shows it"}
			ch.RunnerFailure = RunnerFailure(k, word)
		}
		out = append(out, ch)
	}
	if k != GitHub {
		return out, nil
	}
	runs, err := c.checkRuns(ctx, host, token, commit)
	if err != nil {
		return nil, err
	}
	for _, r := range runs {
		ch := newCheck(r.Name, runChecks(r.Status, r.Conclusion), first(r.Conclusion, r.Status), r.HTMLURL)
		if ch.State == ChecksFailure {
			ch.Log = &JobLog{None: "the check run is not a GitHub Actions job: its URL shows it"}
			ch.RunnerFailure = RunnerFailure(GitHub, r.Conclusion)
			if r.App.Slug == "github-actions" {
				ch.Log = &JobLog{}
				ch.JobID = r.ID
			}
		}
		out = append(out, ch)
	}
	return out, nil
}

// newCheck is a check whose forge-given fields are each one printable line.
func newCheck(name, state, conclusion, url string) Check {
	return Check{Name: oneLine(name), State: state, Conclusion: oneLine(conclusion), URL: oneLine(url)}
}

// wireRun is one GitHub check run.
type wireRun struct {
	ID         int64  `json:"id"`
	Name       string `json:"name"`
	Status     string `json:"status"`
	Conclusion string `json:"conclusion"`
	HTMLURL    string `json:"html_url"`
	App        struct {
		Slug string `json:"slug"`
	} `json:"app"`
}

// checkRuns reads every page of a GitHub commit's check runs, up to
// maxPages, following total_count (01a1301d-55e3).
func (c *Client) checkRuns(ctx context.Context, host, token, commit string) ([]wireRun, error) {
	var out []wireRun
	for page := 1; page <= maxPages; page++ {
		var runs struct {
			TotalCount int64     `json:"total_count"`
			CheckRuns  []wireRun `json:"check_runs"`
		}
		if err := c.get(ctx, host, GitHub, token, fmt.Sprintf("%s/check-runs?per_page=%d&page=%d", commit, contentPage, page), &runs); err != nil {
			return nil, err
		}
		out = append(out, runs.CheckRuns...)
		if len(runs.CheckRuns) == 0 || int64(len(out)) >= runs.TotalCount {
			break
		}
	}
	return out, nil
}

// pipelineJobs reads the jobs of GitLab's pipeline id.
func (c *Client) pipelineJobs(ctx context.Context, host, token, path string, id int64) ([]Check, error) {
	var jobs []struct {
		ID           int64  `json:"id"`
		Name         string `json:"name"`
		Stage        string `json:"stage"`
		Status       string `json:"status"`
		WebURL       string `json:"web_url"`
		AllowFailure bool   `json:"allow_failure"`
		// FailureReason is GitLab's word for why a job failed, such as
		// script_failure or runner_system_failure.
		FailureReason string `json:"failure_reason"`
	}
	p := fmt.Sprintf("%s/pipelines/%d/jobs", repoPath(GitLab, path), id)
	if err := pages(ctx, c, host, GitLab, token, p, contentPage, &jobs); err != nil {
		return nil, err
	}
	out := []Check{}
	for _, j := range jobs {
		ch := newCheck(j.Name, pipelineChecks(j.Status), j.Status, j.WebURL)
		switch {
		case ch.State == ChecksNone:
			ch.State = ChecksSuccess // a skipped job
		case ch.State == ChecksFailure && j.AllowFailure:
			ch.State, ch.Conclusion = ChecksSuccess, j.Status+" (allowed to fail)"
		case ch.State == ChecksFailure:
			ch.Log, ch.JobID = &JobLog{}, j.ID
			ch.RunnerFailure = RunnerFailure(GitLab, j.FailureReason)
		}
		out = append(out, ch)
	}
	return out, nil
}

// excerpts fetches the logs of the failed jobs whose forge offers them, at
// most maxExcerpts, and cuts each to its excerpt. A log that cannot be read
// says why; it never fails the read of the pull request.
func (c *Client) excerpts(ctx context.Context, host string, k Kind, token, path string, checks []Check) {
	fetched := 0
	for i := range checks {
		ch := &checks[i]
		if ch.JobID == 0 {
			continue
		}
		if fetched == maxExcerpts {
			ch.Log = &JobLog{None: fmt.Sprintf("only the first %d failed jobs' logs are read: its URL shows it", maxExcerpts)}
			continue
		}
		fetched++
		log, err := c.jobLog(ctx, host, k, token, path, ch.JobID)
		switch {
		case errors.Is(err, ErrNotFound):
			ch.Log = &JobLog{None: "the forge has no log for this job"}
		case errors.Is(err, ErrRejected):
			ch.Log = &JobLog{None: "the forge refuses this job's log to the token"}
		case err != nil:
			ch.Log = &JobLog{None: "its log could not be read: " + oneLine(err.Error())}
		default:
			ex, cut := excerpt(log)
			ch.Log = &JobLog{Excerpt: ex, Cut: cut}
			if ex == "" {
				ch.Log = &JobLog{None: "the job's log is empty"}
			}
		}
	}
}

// jobLog reads job id's log: GitLab's trace, or GitHub's Actions job log,
// which GitHub answers with a redirect to a signed address on another host.
// That address is fetched without the token.
func (c *Client) jobLog(ctx context.Context, host string, k Kind, token, path string, id int64) (string, error) {
	if k == GitLab {
		return c.getText(ctx, apiBase(host, k)+fmt.Sprintf("%s/jobs/%d/trace", repoPath(k, path), id), k, token)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		apiBase(host, k)+fmt.Sprintf("%s/actions/jobs/%d/logs", repoPath(k, path), id), nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	cl := *c.http()
	cl.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	resp, err := cl.Do(req)
	if err != nil {
		return "", fmt.Errorf("%w: %s", ErrUnreachable, reason(err))
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusOK:
		return readLog(resp.Body)
	case http.StatusFound, http.StatusMovedPermanently, http.StatusTemporaryRedirect, http.StatusSeeOther:
	default:
		return "", statusErr(resp.StatusCode)
	}
	loc, err := resp.Location()
	if err != nil || loc.Scheme != "https" {
		return "", errors.New("the forge redirected the log to an address that is not https")
	}
	return c.getText(ctx, loc.String(), "", "")
}

// getText GETs the text at u, with the token when k names a dialect and
// without one when k is empty.
func (c *Client) getText(ctx context.Context, u string, k Kind, token string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return "", err
	}
	switch k {
	case GitHub:
		req.Header.Set("Authorization", "Bearer "+token)
	case Gitea:
		req.Header.Set("Authorization", "token "+token)
	case GitLab:
		req.Header.Set("PRIVATE-TOKEN", token)
	}
	cl := c.http()
	if k == "" {
		// No token goes with it, so any https redirect may be followed.
		cl.CheckRedirect = func(req *http.Request, via []*http.Request) error {
			if len(via) >= 5 || req.URL.Scheme != "https" {
				return errors.New("refused a redirect")
			}
			return nil
		}
	}
	resp, err := cl.Do(req)
	if err != nil {
		return "", fmt.Errorf("%w: %s", ErrUnreachable, reason(err))
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", statusErr(resp.StatusCode)
	}
	return readLog(resp.Body)
}

func readLog(r io.Reader) (string, error) {
	raw, err := io.ReadAll(io.LimitReader(r, maxLog))
	if err != nil {
		return "", fmt.Errorf("%w: %s", ErrUnreachable, reason(err))
	}
	return string(raw), nil
}

func statusErr(code int) error {
	switch {
	case code == http.StatusUnauthorized || code == http.StatusForbidden:
		return ErrRejected
	case code == http.StatusNotFound || code == http.StatusGone:
		return ErrNotFound
	case code >= 500:
		return fmt.Errorf("%w: status %d", ErrUnreachable, code)
	}
	return fmt.Errorf("the forge answered status %d", code)
}

// pages reads the list at p page by page into out, until a short page or
// maxPages.
func pages[T any](ctx context.Context, c *Client, host string, k Kind, token, p string, size int, out *[]T) error {
	sep := "?"
	if strings.Contains(p, "?") {
		sep = "&"
	}
	for page := 1; page <= maxPages; page++ {
		var batch []T
		q := fmt.Sprintf("%s%sper_page=%d&page=%d", p, sep, size, page)
		if k == Gitea {
			q = fmt.Sprintf("%s%slimit=%d&page=%d", p, sep, size, page)
		}
		if err := c.get(ctx, host, k, token, q, &batch); err != nil {
			return err
		}
		*out = append(*out, batch...)
		if len(batch) < size {
			return nil
		}
	}
	return nil
}

// The lines a log's excerpt starts near: a test's or a program's failure,
// then an error a tool reports, then the runner's own error annotation.
var (
	// An "error:" counts at the start of a line or after a space, a colon,
	// a bracket or a parenthesis, as a tool prints it, never as the end of
	// a word such as the key fail-on-error:.
	failureLine = regexp.MustCompile(`^\s*(--- FAIL|FAIL\b|panic:|fatal error:)|(?i)(^|[\s:\[(])error:|\bnpm ERR!`)
	runnerError = regexp.MustCompile(`^##\[error\]`)
	// timestamp is the time GitHub prefixes each line of a job's log with.
	timestamp = regexp.MustCompile(`^\d{4}-\d\d-\d\dT\d\d:\d\d:\d\d(\.\d+)?Z `)
)

// excerpt is at most MaxExcerpt bytes of log, in whole lines where they
// fit: from excerptContext lines before its first failure line, or else its
// first runner error, or else its end. cut says the log holds more.
func excerpt(log string) (string, bool) {
	lines := strings.Split(strings.TrimRight(cleanText(log), "\n"), "\n")
	for i, l := range lines {
		lines[i] = timestamp.ReplaceAllString(l, "")
	}
	if len(lines) == 1 && lines[0] == "" {
		return "", false
	}
	start := -1
	for _, re := range []*regexp.Regexp{failureLine, runnerError} {
		for i, l := range lines {
			if re.MatchString(l) {
				start = max(0, i-excerptContext)
				break
			}
		}
		if start >= 0 {
			break
		}
	}
	if start < 0 {
		// No line reads as an error: the end of the log, in whole lines.
		size, i := 0, len(lines)
		for i > 0 && size+len(lines[i-1])+1 <= MaxExcerpt {
			i--
			size += len(lines[i]) + 1
		}
		if i == len(lines) {
			ex, _ := bounded(lines[i-1], MaxExcerpt)
			return ex, true
		}
		return strings.Join(lines[i:], "\n"), i > 0
	}
	var b strings.Builder
	end := start
	for end < len(lines) && b.Len()+len(lines[end])+1 <= MaxExcerpt {
		b.WriteString(lines[end])
		b.WriteByte('\n')
		end++
	}
	if end == start {
		ex, _ := bounded(lines[start], MaxExcerpt)
		return ex, true
	}
	return strings.TrimSuffix(b.String(), "\n"), start > 0 || end < len(lines)
}

// ansi is a terminal escape sequence.
var ansi = regexp.MustCompile(`\x1b\[[0-9;?]*[ -/]*[@-~]|\x1b[@-Z\\-_]`)

// cleanText is s as printable text: valid UTF-8, without terminal escapes,
// carriage returns or control characters other than newline and tab.
func cleanText(s string) string {
	s = ansi.ReplaceAllString(strings.ToValidUTF8(s, "\uFFFD"), "")
	return strings.Map(func(r rune) rune {
		switch {
		case r == '\n' || r == '\t':
			return r
		case r == '\r':
			return -1
		case r < 0x20 || r == 0x7f || (r >= 0x80 && r < 0xa0):
			return -1
		}
		return r
	}, s)
}

// bounded is s cut to at most n bytes on a rune boundary, and whether it
// was cut.
func bounded(s string, n int) (string, bool) {
	if len(s) <= n {
		return s, false
	}
	s = s[:n]
	for len(s) > 0 && !utf8.ValidString(s) {
		s = s[:len(s)-1]
	}
	return s, true
}

func first64(v ...int64) int64 {
	for _, x := range v {
		if x != 0 {
			return x
		}
	}
	return 0
}
