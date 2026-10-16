package forge

import (
	"context"
	"net/url"
)

// A pull request's status for forge events (docs/CREW-CONTRACT.md, "Forge
// events"): the pull request itself, the CI conclusion of its head commit
// and how many comments it carries. Reading it is one to three reads with
// the member's token, and no write.

// The CI conclusions of a head commit.
const (
	ChecksNone    = "none"    // no check or status reported
	ChecksPending = "pending" // at least one still running, none failed
	ChecksSuccess = "success" // every one succeeded
	ChecksFailure = "failure" // at least one failed
)

// PullStatus is a pull request with its head commit's CI conclusion and its
// comment count (GitHub's conversation and review comments, Gitea's
// comments, GitLab's user notes).
type PullStatus struct {
	Pull
	Checks   string `json:"checks"`
	Comments int64  `json:"comments"`
}

// wireStatus is the counts and pipeline a pull request's answer carries.
type wireStatus struct {
	Comments       int64 `json:"comments"`
	ReviewComments int64 `json:"review_comments"`
	UserNotes      int64 `json:"user_notes_count"`
	HeadPipeline   *struct {
		ID     int64  `json:"id"`
		Status string `json:"status"`
	} `json:"head_pipeline"`
}

// combinedStatus is GitHub's and Gitea's combined commit status.
type combinedStatus struct {
	State      string `json:"state"`
	TotalCount int64  `json:"total_count"`
}

// GetPullStatus reads pull request n of the repository at path, with its
// head commit's CI conclusion and its comment count.
func (c *Client) GetPullStatus(ctx context.Context, host string, k Kind, token, path string, n int64) (PullStatus, error) {
	var raw struct {
		wirePull
		wireStatus
	}
	if err := c.get(ctx, host, k, token, pullPath(k, path, n), &raw); err != nil {
		return PullStatus{}, err
	}
	p, err := raw.wirePull.pull(k)
	if err != nil {
		return PullStatus{}, err
	}
	st := PullStatus{Pull: p, Checks: ChecksNone}
	switch k {
	case GitLab:
		st.Comments = raw.UserNotes
		if raw.HeadPipeline != nil {
			st.Checks = pipelineChecks(raw.HeadPipeline.Status)
		}
		return st, nil
	case GitHub:
		st.Comments = raw.Comments + raw.ReviewComments
	default:
		st.Comments = raw.Comments
	}
	if p.HeadCommit == "" {
		return st, nil
	}
	commit := repoPath(k, path) + "/commits/" + url.PathEscape(p.HeadCommit)
	var cs combinedStatus
	if err := c.get(ctx, host, k, token, commit+"/status", &cs); err != nil {
		return PullStatus{}, err
	}
	var conclusions []string
	if cs.TotalCount > 0 {
		conclusions = append(conclusions, statusChecks(cs.State))
	}
	if k == GitHub {
		runs, err := c.checkRuns(ctx, host, token, commit)
		if err != nil {
			return PullStatus{}, err
		}
		for _, r := range runs {
			conclusions = append(conclusions, runChecks(r.Status, r.Conclusion))
		}
	}
	st.Checks = combine(conclusions)
	return st, nil
}

// combine is the conclusion of several: a failure wins, then a pending one.
func combine(cs []string) string {
	out := ChecksNone
	for _, c := range cs {
		switch {
		case c == ChecksFailure:
			return ChecksFailure
		case c == ChecksPending:
			out = ChecksPending
		case c == ChecksSuccess && out == ChecksNone:
			out = ChecksSuccess
		}
	}
	return out
}

// statusChecks reads a combined commit status's state.
func statusChecks(state string) string {
	switch state {
	case "success":
		return ChecksSuccess
	case "failure", "error":
		return ChecksFailure
	default:
		return ChecksPending
	}
}

// runChecks reads a GitHub check run.
func runChecks(status, conclusion string) string {
	if status != "completed" {
		return ChecksPending
	}
	switch conclusion {
	case "success", "neutral", "skipped":
		return ChecksSuccess
	default:
		return ChecksFailure
	}
}

// pipelineChecks reads a GitLab pipeline's status.
func pipelineChecks(status string) string {
	switch status {
	case "success":
		return ChecksSuccess
	case "failed", "canceled":
		return ChecksFailure
	case "skipped":
		return ChecksNone
	default:
		return ChecksPending
	}
}
