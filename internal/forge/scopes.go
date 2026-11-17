package forge

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"slices"
	"strings"
)

// ScopeError is a refusal that names the scopes the token lacks, as Gitea
// words it. It unwraps to ErrRejected for a read and ErrForbidden for a
// write, so a caller that knows neither still classifies it.
type ScopeError struct {
	Scopes []string
	base   error
}

func (e *ScopeError) Error() string {
	return "the token lacks the scope " + strings.Join(e.Scopes, " or ")
}

func (e *ScopeError) Unwrap() error { return e.base }

// giteaScopeRefusal is Gitea's refusal of a token without a scope the
// route requires (observed on 1.27.3): "token does not have at least one
// of required scope(s), required=[write:issue], token scope=...".
var giteaScopeRefusal = regexp.MustCompile(`token does not have at least one of required scope\(s\), required=\[([a-z:, ]+)\]`)

// missingScopes is the scopes a refusal message names as required, or nil
// when it names none.
func missingScopes(k Kind, msg string) []string {
	if k != Gitea {
		return nil
	}
	m := giteaScopeRefusal.FindStringSubmatch(msg)
	if m == nil {
		return nil
	}
	var out []string
	for _, s := range strings.Split(m[1], ",") {
		if s = strings.TrimSpace(s); s != "" {
			out = append(out, s)
		}
	}
	return out
}

// The states of one of a token's rights.
const (
	RightPresent    = "present"     // the forge's answer shows the token holds it
	RightMissing    = "missing"     // the forge's answer names it as missing
	RightNotChecked = "not_checked" // the forge's answer shows neither
)

// RightCheck is one right of a token on a repository, as the forge's
// answer showed it.
type RightCheck struct {
	Right  string `json:"right"`
	State  string `json:"state"`
	Detail string `json:"detail,omitempty"`
}

// giteaProbe is a request that reads whether a token holds a scope without
// changing anything: it names number 0, which Gitea gives to no issue or
// pull request (they are numbered from 1), and carries no body.
type giteaProbe struct {
	scope, method, path string
	// status and message are the answer, observed on Gitea 1.27.3, that
	// shows the scope is there: the route passed the scope check and
	// stopped at the record or the body. Both must match exactly.
	status  int
	message string
}

var giteaProbes = []giteaProbe{
	{scope: "write:repository", method: http.MethodPatch, path: "/pulls/0", status: http.StatusNotFound, message: "not found"},
	{scope: "write:issue", method: http.MethodPost, path: "/issues/0/labels", status: http.StatusUnprocessableEntity, message: "[]: Empty Content-Type"},
}

// TokenRights reads what token itself may write to the repository at path,
// where the forge tells it without a change: a Gitea token's write scopes by
// two refused requests, a classic GitHub token's scopes from the header the
// forge sends with the repository. Anything else is RightNotChecked with the
// reason; an answer is never read as more than it shows.
func (c *Client) TokenRights(ctx context.Context, host string, k Kind, token, path string) []RightCheck {
	switch k {
	case Gitea:
		out := make([]RightCheck, 0, len(giteaProbes))
		for _, p := range giteaProbes {
			out = append(out, c.giteaRight(ctx, host, token, path, p))
		}
		return out
	case GitHub:
		return []RightCheck{c.githubRepoScope(ctx, host, token, path)}
	}
	return []RightCheck{{Right: "token", State: RightNotChecked,
		Detail: "the token's own rights are not read on " + string(k)}}
}

func (c *Client) giteaRight(ctx context.Context, host, token, path string, p giteaProbe) RightCheck {
	r := RightCheck{Right: p.scope, State: RightNotChecked}
	req, err := http.NewRequestWithContext(ctx, p.method, apiBase(host, Gitea)+repoPath(Gitea, path)+p.path, nil)
	if err != nil {
		r.Detail = err.Error()
		return r
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", "token "+token)
	resp, err := c.http().Do(req)
	if err != nil {
		r.Detail = fmt.Sprintf("%s: %s", ErrUnreachable, reason(err))
		return r
	}
	defer resp.Body.Close()
	var body struct {
		Message string `json:"message"`
	}
	json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(&body)
	msg := oneLine(body.Message)
	switch {
	case resp.StatusCode == p.status && msg == p.message:
		r.State = RightPresent
	case resp.StatusCode == http.StatusForbidden && slices.Contains(missingScopes(Gitea, msg), p.scope):
		r.State, r.Detail = RightMissing, "the forge refuses it: "+msg
	default:
		r.Detail = fmt.Sprintf("the forge answered %s %s with status %d %q, not as Gitea 1.27.3 does",
			p.method, p.path, resp.StatusCode, msg)
	}
	return r
}

// githubRepoScope is the classic token's scope for a repository's work, from
// the X-OAuth-Scopes header GitHub documents for classic tokens: repo, or
// public_repo on a public repository. A token whose answer lists no scope (a
// fine-grained token, which GitHub documents with no such header) is not
// checked.
func (c *Client) githubRepoScope(ctx context.Context, host, token, path string) RightCheck {
	r := RightCheck{Right: "repo", State: RightNotChecked}
	var repo struct {
		Private bool `json:"private"`
	}
	h, err := c.doURLHeader(ctx, http.MethodGet, apiBase(host, GitHub)+repoPath(GitHub, path), GitHub, token, nil, &repo)
	if err != nil {
		r.Detail = err.Error()
		return r
	}
	var scopes []string
	for _, v := range h.Values("X-OAuth-Scopes") {
		for _, s := range strings.Split(v, ",") {
			if s = strings.TrimSpace(s); s != "" {
				scopes = append(scopes, s)
			}
		}
	}
	switch {
	case len(scopes) == 0:
		r.Detail = "GitHub names no scopes for this token (a fine-grained token): its permissions are not checked; " +
			"docs/BRING-UP.md lists the ones it needs"
	case slices.Contains(scopes, "repo"), !repo.Private && slices.Contains(scopes, "public_repo"):
		r.State = RightPresent
	default:
		r.State, r.Detail = RightMissing, "the classic token's scopes are "+strings.Join(scopes, ", ")+", without repo"
	}
	return r
}
