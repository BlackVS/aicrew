package forge

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

// Pull requests with a member's own token (pilot finding 2026-10-11): a
// member opens, reads, labels and comments on its pull requests through
// aicrew-agent, never through another forge client or a token it handles
// itself. GitLab calls them merge requests; here they are pull requests in
// every dialect, numbered as the forge numbers them in the repository
// (GitLab's iid).

// NewPull is a pull request to open: from the branch Head to the branch Base
// of the same repository.
type NewPull struct {
	Title string
	Body  string
	Head  string
	Base  string
	// Draft opens it as a draft: GitHub's draft flag, or the title prefix
	// Gitea (DraftPrefix) and GitLab read as one.
	Draft bool
}

// Pull is a pull request as the forge reports it.
type Pull struct {
	Number int64  `json:"number"`
	URL    string `json:"url"`
	Title  string `json:"title"`
	// State is open, closed or merged.
	State       string   `json:"state"`
	Draft       bool     `json:"draft"`
	Head        string   `json:"head"`
	HeadCommit  string   `json:"head_commit"`
	Base        string   `json:"base"`
	Labels      []string `json:"labels"`
	MergeCommit string   `json:"merge_commit,omitempty"`
	// Mergeable says the forge reports that the pull request merges into
	// its base without a conflict. False also covers a forge that has not
	// computed it yet (GitHub's null) and a pull request that is not open.
	Mergeable bool `json:"mergeable"`
}

// Comment is a comment posted on a pull request.
type Comment struct {
	ID  int64  `json:"id"`
	URL string `json:"url"`
}

// The pull request states.
const (
	PullOpen   = "open"
	PullClosed = "closed"
	PullMerged = "merged"
)

// DraftPrefix is the title prefix that marks a draft in a dialect without a
// draft flag on create: Gitea's work-in-progress prefix, and GitLab's.
func DraftPrefix(k Kind) string {
	switch k {
	case Gitea:
		return "WIP: "
	case GitLab:
		return "Draft: "
	}
	return ""
}

// hasDraftPrefix reports whether title already starts with one of the
// dialect's draft prefixes, in any case.
func hasDraftPrefix(k Kind, title string) bool {
	t := strings.ToLower(strings.TrimSpace(title))
	var prefixes []string
	switch k {
	case Gitea:
		prefixes = []string{"wip:", "[wip]"}
	case GitLab:
		prefixes = []string{"draft:", "[draft]", "(draft)"}
	}
	for _, p := range prefixes {
		if strings.HasPrefix(t, p) {
			return true
		}
	}
	return false
}

// draftPrefixes are the title prefixes that mark a draft in any dialect:
// Gitea's and GitLab's, which a process may also use on GitHub.
var draftPrefixes = []string{"wip:", "[wip]", "draft:", "[draft]", "(draft)"}

// stripDraft is title without its draft prefixes, in any case, and whether
// it had one.
func stripDraft(title string) (string, bool) {
	t, cut := strings.TrimSpace(title), false
	for {
		found := false
		for _, p := range draftPrefixes {
			if len(t) >= len(p) && strings.EqualFold(t[:len(p)], p) {
				t, cut, found = strings.TrimSpace(t[len(p):]), true, true
				break
			}
		}
		if !found {
			return t, cut
		}
	}
}

// ErrNotOpen is a pull request that is closed or merged: it cannot be
// marked ready.
var ErrNotOpen = errors.New("the pull request is not open")

// ErrLabelUnknown is a label the repository (or its organization or group)
// does not define. Labels are never created here: a typo would otherwise
// make a new, inert label.
var ErrLabelUnknown = errors.New("the repository defines no such label")

// OpenPull opens a pull request in the repository at path.
func (c *Client) OpenPull(ctx context.Context, host string, k Kind, token, path string, p NewPull) (Pull, error) {
	title := p.Title
	if p.Draft && k != GitHub && !hasDraftPrefix(k, title) {
		title = DraftPrefix(k) + title
	}
	var in any
	switch k {
	case GitHub:
		in = map[string]any{"title": title, "body": p.Body, "head": p.Head, "base": p.Base, "draft": p.Draft}
	case Gitea:
		in = map[string]any{"title": title, "body": p.Body, "head": p.Head, "base": p.Base}
	case GitLab:
		in = map[string]any{"title": title, "description": p.Body, "source_branch": p.Head, "target_branch": p.Base}
	default:
		return Pull{}, fmt.Errorf("unknown forge dialect %q", k)
	}
	var raw wirePull
	if err := c.do(ctx, http.MethodPost, host, k, token, pullsPath(k, path), in, &raw); err != nil {
		return Pull{}, err
	}
	return raw.pull(k)
}

// GetPull reads pull request n of the repository at path.
func (c *Client) GetPull(ctx context.Context, host string, k Kind, token, path string, n int64) (Pull, error) {
	var raw wirePull
	if err := c.get(ctx, host, k, token, pullPath(k, path, n), &raw); err != nil {
		return Pull{}, err
	}
	return raw.pull(k)
}

// ReadyPull marks pull request n ready for review and merge: it drops
// GitHub's draft flag, and in every dialect the draft prefixes of its title
// (stripDraft), which is how Gitea and GitLab mark a draft and how a
// process may mark one on GitHub. A pull request that is ready already is
// not written. One that is closed or merged is ErrNotOpen, unchanged.
func (c *Client) ReadyPull(ctx context.Context, host string, k Kind, token, path string, n int64) (Pull, error) {
	var raw wirePull
	if err := c.get(ctx, host, k, token, pullPath(k, path, n), &raw); err != nil {
		return Pull{}, err
	}
	p, err := raw.pull(k)
	if err != nil {
		return Pull{}, err
	}
	if p.State != PullOpen {
		return Pull{}, fmt.Errorf("%w: it is %s", ErrNotOpen, p.State)
	}
	title, prefixed := stripDraft(p.Title)
	if prefixed && title == "" {
		return Pull{}, fmt.Errorf("its title %q is only a draft prefix", p.Title)
	}
	if !prefixed && !(k == GitHub && raw.Draft) {
		return p, nil
	}
	if k == GitHub && raw.Draft {
		if err := c.markReady(ctx, host, token, raw.NodeID); err != nil {
			return Pull{}, err
		}
	}
	if prefixed {
		method := http.MethodPatch
		if k == GitLab {
			method = http.MethodPut
		}
		if err := c.do(ctx, method, host, k, token, pullPath(k, path, n), map[string]any{"title": title}, nil); err != nil {
			return Pull{}, err
		}
	}
	return c.GetPull(ctx, host, k, token, path, n)
}

// graphqlURL is GitHub's GraphQL endpoint on host.
func graphqlURL(host string) string {
	if host == "github.com" {
		return "https://api.github.com/graphql"
	}
	return "https://" + host + "/api/graphql"
}

// markReady drops the draft flag of the GitHub pull request with the node
// ID id: GitHub's REST API cannot, its GraphQL mutation can. A refusal
// GraphQL types FORBIDDEN is ErrForbidden.
func (c *Client) markReady(ctx context.Context, host, token, id string) error {
	if id == "" {
		return errors.New("the forge's answer names no pull request ID")
	}
	in := map[string]any{
		"query":     "mutation($id:ID!){markPullRequestReadyForReview(input:{pullRequestId:$id}){pullRequest{isDraft}}}",
		"variables": map[string]any{"id": id},
	}
	var out struct {
		Data struct {
			Mark *struct {
				PullRequest struct {
					IsDraft bool `json:"isDraft"`
				} `json:"pullRequest"`
			} `json:"markPullRequestReadyForReview"`
		} `json:"data"`
		Errors []struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"errors"`
	}
	if err := c.doURL(ctx, http.MethodPost, graphqlURL(host), GitHub, token, in, &out); err != nil {
		return err
	}
	if len(out.Errors) > 0 {
		forbidden := false
		msgs := make([]string, 0, len(out.Errors))
		for _, e := range out.Errors {
			forbidden = forbidden || e.Type == "FORBIDDEN"
			msgs = append(msgs, e.Message)
		}
		msg := oneLine(strings.Join(msgs, "; "))
		if forbidden {
			return fmt.Errorf("%w: %s", ErrForbidden, msg)
		}
		return fmt.Errorf("the forge refused it: %s", msg)
	}
	if out.Data.Mark == nil || out.Data.Mark.PullRequest.IsDraft {
		return errors.New("the forge's answer does not show the pull request ready")
	}
	return nil
}

// AddLabels adds labels the repository already defines to pull request n.
// A label it does not define is ErrLabelUnknown, and nothing is added.
func (c *Client) AddLabels(ctx context.Context, host string, k Kind, token, path string, n int64, labels []string) error {
	switch k {
	case GitHub:
		// GitHub would create a label it does not know: check each first.
		for _, l := range labels {
			if err := c.get(ctx, host, k, token, repoPath(k, path)+"/labels/"+url.PathEscape(l), &struct{}{}); err != nil {
				return labelErr(err, l)
			}
		}
		return c.do(ctx, http.MethodPost, host, k, token, issueLabelsPath(k, path, n),
			map[string]any{"labels": labels}, nil)
	case Gitea:
		ids, err := c.giteaLabelIDs(ctx, host, token, path, labels)
		if err != nil {
			return err
		}
		return c.do(ctx, http.MethodPost, host, k, token, issueLabelsPath(k, path, n),
			map[string]any{"labels": ids}, nil)
	case GitLab:
		// GitLab, too, would create an unknown label; its single-label read
		// includes the ancestor groups' labels.
		for _, l := range labels {
			if err := c.get(ctx, host, k, token, repoPath(k, path)+"/labels/"+url.PathEscape(l), &struct{}{}); err != nil {
				return labelErr(err, l)
			}
		}
		return c.do(ctx, http.MethodPut, host, k, token, pullPath(k, path, n),
			map[string]any{"add_labels": strings.Join(labels, ",")}, nil)
	}
	return fmt.Errorf("unknown forge dialect %q", k)
}

// RemoveLabels removes labels from pull request n. A label the pull request
// does not carry is skipped, so a removal can be repeated.
func (c *Client) RemoveLabels(ctx context.Context, host string, k Kind, token, path string, n int64, labels []string) error {
	var raw wirePull
	if err := c.get(ctx, host, k, token, pullPath(k, path, n), &raw); err != nil {
		return err
	}
	carried := map[string]int64{}
	for _, l := range raw.labels() {
		carried[l.name] = l.id
	}
	var gone []string
	for _, l := range labels {
		id, ok := carried[l]
		if !ok {
			continue
		}
		switch k {
		case GitHub:
			if err := c.do(ctx, http.MethodDelete, host, k, token,
				issueLabelsPath(k, path, n)+"/"+url.PathEscape(l), nil, nil); err != nil && !errors.Is(err, ErrNotFound) {
				return err
			}
		case Gitea:
			if err := c.do(ctx, http.MethodDelete, host, k, token,
				issueLabelsPath(k, path, n)+"/"+strconv.FormatInt(id, 10), nil, nil); err != nil && !errors.Is(err, ErrNotFound) {
				return err
			}
		case GitLab:
			gone = append(gone, l)
		}
	}
	if len(gone) > 0 {
		return c.do(ctx, http.MethodPut, host, k, token, pullPath(k, path, n),
			map[string]any{"remove_labels": strings.Join(gone, ",")}, nil)
	}
	return nil
}

// CommentPull posts body as a comment on pull request n.
func (c *Client) CommentPull(ctx context.Context, host string, k Kind, token, path string, n int64, body string) (Comment, error) {
	var r struct {
		ID      int64  `json:"id"`
		HTMLURL string `json:"html_url"`
	}
	p := repoPath(k, path) + "/issues/" + strconv.FormatInt(n, 10) + "/comments"
	if k == GitLab {
		p = pullPath(k, path, n) + "/notes"
	}
	if err := c.do(ctx, http.MethodPost, host, k, token, p, map[string]any{"body": body}, &r); err != nil {
		return Comment{}, err
	}
	if r.ID == 0 {
		return Comment{}, errors.New("the forge's answer names no comment")
	}
	cm := Comment{ID: r.ID, URL: r.HTMLURL}
	if k == GitLab {
		// A note has no URL of its own: it is an anchor on the request's page.
		pr, err := c.GetPull(ctx, host, k, token, path, n)
		if err != nil {
			return cm, err
		}
		cm.URL = pr.URL + "#note_" + strconv.FormatInt(r.ID, 10)
	}
	return cm, nil
}

func labelErr(err error, label string) error {
	if errors.Is(err, ErrNotFound) {
		return fmt.Errorf("%w: %q", ErrLabelUnknown, label)
	}
	return err
}

// giteaLabelIDs maps label names to Gitea's IDs, from the repository's
// labels and, when its owner is an organization, the organization's.
func (c *Client) giteaLabelIDs(ctx context.Context, host, token, path string, names []string) ([]int64, error) {
	known := map[string]int64{}
	owner, _, _ := strings.Cut(path, "/")
	for _, list := range []string{repoPath(Gitea, path) + "/labels", "/orgs/" + url.PathEscape(owner) + "/labels"} {
		for page := 1; page <= maxLabelPages; page++ {
			var ls []struct {
				ID   int64  `json:"id"`
				Name string `json:"name"`
			}
			err := c.get(ctx, host, Gitea, token, fmt.Sprintf("%s?page=%d&limit=%d", list, page, labelPage), &ls)
			if errors.Is(err, ErrNotFound) && strings.HasPrefix(list, "/orgs/") {
				break // the owner is a user, with no organization labels
			}
			if err != nil {
				return nil, err
			}
			for _, l := range ls {
				if _, dup := known[l.Name]; !dup {
					known[l.Name] = l.ID
				}
			}
			if len(ls) < labelPage {
				break
			}
		}
	}
	ids := make([]int64, 0, len(names))
	for _, n := range names {
		id, ok := known[n]
		if !ok {
			return nil, fmt.Errorf("%w: %q", ErrLabelUnknown, n)
		}
		ids = append(ids, id)
	}
	return ids, nil
}

// Gitea's label lists are read in pages of labelPage, at most maxLabelPages
// of each.
const (
	labelPage     = 50
	maxLabelPages = 20
)

func pullsPath(k Kind, path string) string {
	if k == GitLab {
		return repoPath(k, path) + "/merge_requests"
	}
	return repoPath(k, path) + "/pulls"
}

func pullPath(k Kind, path string, n int64) string {
	return pullsPath(k, path) + "/" + strconv.FormatInt(n, 10)
}

// issueLabelsPath is a pull request's labels in GitHub and Gitea, which
// label it as an issue.
func issueLabelsPath(k Kind, path string, n int64) string {
	return repoPath(k, path) + "/issues/" + strconv.FormatInt(n, 10) + "/labels"
}

// wirePull is a pull request in any dialect's answer.
type wirePull struct {
	Number  int64  `json:"number"`
	IID     int64  `json:"iid"`
	NodeID  string `json:"node_id"`
	HTMLURL string `json:"html_url"`
	WebURL  string `json:"web_url"`
	Title   string `json:"title"`
	State   string `json:"state"`
	Draft   bool   `json:"draft"`
	WIP     bool   `json:"work_in_progress"`
	Merged  bool   `json:"merged"`
	Head    struct {
		Ref string `json:"ref"`
		SHA string `json:"sha"`
	} `json:"head"`
	Base struct {
		Ref string `json:"ref"`
	} `json:"base"`
	SourceBranch   string `json:"source_branch"`
	TargetBranch   string `json:"target_branch"`
	SHA            string `json:"sha"`
	MergeCommitSHA string `json:"merge_commit_sha"`
	SquashCommit   string `json:"squash_commit_sha"`
	// Mergeable is GitHub's (null until computed) and Gitea's; GitLab says
	// it in merge_status.
	Mergeable   *bool  `json:"mergeable"`
	MergeStatus string `json:"merge_status"`
	// Labels are objects in GitHub and Gitea, names in GitLab.
	Labels []any `json:"labels"`
}

type wireLabel struct {
	id   int64
	name string
}

func (w wirePull) labels() []wireLabel {
	var out []wireLabel
	for _, l := range w.Labels {
		switch v := l.(type) {
		case string:
			out = append(out, wireLabel{name: v})
		case map[string]any:
			name, _ := v["name"].(string)
			id, _ := v["id"].(float64)
			if name != "" {
				out = append(out, wireLabel{id: int64(id), name: name})
			}
		}
	}
	return out
}

func (w wirePull) pull(k Kind) (Pull, error) {
	p := Pull{Number: w.Number, URL: w.HTMLURL, Title: w.Title, Draft: w.Draft, Head: w.Head.Ref,
		HeadCommit: w.Head.SHA, Base: w.Base.Ref, Labels: []string{}}
	switch k {
	case GitLab:
		p.Number, p.URL, p.Head, p.HeadCommit, p.Base = w.IID, w.WebURL, w.SourceBranch, w.SHA, w.TargetBranch
		p.Draft = w.Draft || w.WIP
		p.MergeCommit = first(w.MergeCommitSHA, w.SquashCommit)
		p.Mergeable = w.MergeStatus == "can_be_merged"
		switch w.State {
		case "opened", "locked":
			p.State = PullOpen
		case "merged":
			p.State = PullMerged
		default:
			p.State = PullClosed
		}
	default:
		p.State = PullOpen
		if w.State == "closed" {
			p.State = PullClosed
		}
		if w.Merged {
			p.State, p.MergeCommit = PullMerged, w.MergeCommitSHA
		}
		if k == Gitea {
			p.Draft = p.Draft || hasDraftPrefix(k, w.Title)
		}
		p.Mergeable = w.Mergeable != nil && *w.Mergeable
	}
	for _, l := range w.labels() {
		p.Labels = append(p.Labels, l.name)
	}
	if p.Number <= 0 || p.URL == "" {
		return Pull{}, errors.New("the forge's answer names no pull request")
	}
	return p, nil
}
