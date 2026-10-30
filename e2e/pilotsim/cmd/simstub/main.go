// Command simstub is the pilot simulation's external reviewer and CI on
// sim-gitea (docs/DESIGN-PILOT-SIMULATION.md, section 2.5), one binary in
// two containers:
//
//	simstub hands -scenario FILE -token-file FILE -api URL [-ack FILE]   (sim-hands)
//	simstub ci    -scenario FILE -token-file FILE -api URL [-ack FILE]   (sim-ci)
//
// hands speaks the external-review label protocol: on an open pull request
// labelled review-this it puts hands-reviewing on, then takes review-this
// off; after the scenario's review time it posts one comment headed
// "[hands-bot review] reviewed at head <full SHA>" with a VERDICT line and
// the scenario's verdict, puts hands-reviewed on and takes hands-reviewing
// off. A head that moved meanwhile gets no comment, and hands-reviewing
// comes off so the pull request can be requested again.
//
// ci posts a commit status under its context on every branch head that has
// none yet: the pull requests' heads and, after a merge, the merge commit.
// Its state is the scenario's, success unless the scenario says otherwise.
//
// Both read the scenario file again on every poll, so the runner changes a
// verdict or a CI state by rewriting it, and act only with their own token.
// A stub writes the scenario's generation to its -ack file when it first
// loads it, before the poll that uses it. A stub polls one at a time, so
// once the ack names a generation, the poll that read an older scenario
// has finished: the runner waits for it before it acts on the new one.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"
)

// The protocol's labels and verdicts.
const (
	labelRequest   = "review-this"
	labelReviewing = "hands-reviewing"
	labelReviewed  = "hands-reviewed"

	verdictReady  = "READY_FOR_HUMAN_MERGE"
	verdictReturn = "RETURN_TO_IMPLEMENTATION"

	reviewHeader = "[hands-bot review]"
)

// scenario is the part of the scenario file the stubs read.
type scenario struct {
	Generation  int64    `json:"generation"`
	Repos       []string `json:"repos"`
	PollSeconds float64  `json:"poll_seconds"`
	Hands       struct {
		Verdict string `json:"verdict"`
		// FirstVerdict, when set, is the verdict of a pull request's
		// first review: one with no review of this stub's yet.
		FirstVerdict  string  `json:"first_verdict"`
		ReviewSeconds float64 `json:"review_seconds"`
	} `json:"hands"`
	CI struct {
		State   string `json:"state"`
		Context string `json:"context"`
	} `json:"ci"`
}

func loadScenario(path string) (scenario, error) {
	var s scenario
	b, err := os.ReadFile(path)
	if err != nil {
		return s, err
	}
	if err := json.Unmarshal(b, &s); err != nil {
		return s, fmt.Errorf("%s: %w", path, err)
	}
	if s.PollSeconds <= 0 {
		s.PollSeconds = 2
	}
	if s.Hands.Verdict == "" {
		s.Hands.Verdict = verdictReady
	}
	for _, v := range []string{s.Hands.Verdict, s.Hands.FirstVerdict} {
		if v != "" && v != verdictReady && v != verdictReturn {
			return s, fmt.Errorf("%s: hands verdict %q is neither %s nor %s", path, v, verdictReady, verdictReturn)
		}
	}
	if s.CI.State == "" {
		s.CI.State = "success"
	}
	switch s.CI.State {
	case "success", "failure", "error", "pending", "warning":
	default:
		return s, fmt.Errorf("%s: ci.state %q is not a Gitea commit status", path, s.CI.State)
	}
	if s.CI.Context == "" {
		s.CI.Context = "sim-ci"
	}
	for _, r := range s.Repos {
		if strings.Count(r, "/") != 1 {
			return s, fmt.Errorf("%s: repository %q is not OWNER/NAME", path, r)
		}
	}
	return s, nil
}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	os.Exit(run(ctx, os.Args[1:], os.Stdout, os.Stderr))
}

func run(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	usage := "usage: simstub hands|ci -scenario FILE -token-file FILE -api URL"
	if len(args) == 0 || (args[0] != "hands" && args[0] != "ci") {
		fmt.Fprintln(stderr, usage)
		return 2
	}
	mode := args[0]
	fs := flag.NewFlagSet("simstub "+mode, flag.ContinueOnError)
	fs.SetOutput(stderr)
	scenarioFile := fs.String("scenario", "", "the scenario file")
	tokenFile := fs.String("token-file", "", "this stub's own Gitea token")
	api := fs.String("api", "", "Gitea's API base, https://HOST/api/v1")
	ackFile := fs.String("ack", "", "where to write the generation of the scenario in use")
	once := fs.Bool("once", false, "one poll, then exit (tests)")
	if err := fs.Parse(args[1:]); err != nil {
		return 2
	}
	if *scenarioFile == "" || *tokenFile == "" || *api == "" || fs.NArg() > 0 {
		fmt.Fprintln(stderr, usage)
		return 2
	}
	tok, err := os.ReadFile(*tokenFile)
	if err != nil {
		fmt.Fprintln(stderr, "simstub:", err)
		return 1
	}
	g := &gitea{base: strings.TrimRight(*api, "/"), token: strings.TrimSpace(string(tok)), client: &http.Client{Timeout: 30 * time.Second}}
	st := &stub{g: g, out: stdout, sleep: sleepCtx, seen: map[string]bool{}}
	poll := st.handsPoll
	if mode == "ci" {
		poll = st.ciPoll
	}
	acked := int64(-1)
	for {
		s, err := loadScenario(*scenarioFile)
		if err == nil && *ackFile != "" && s.Generation != acked {
			if err = writeAck(*ackFile, s.Generation); err == nil {
				acked = s.Generation
			}
		}
		if err != nil {
			fmt.Fprintln(stderr, "simstub:", err)
			if *once {
				return 1
			}
		} else if err := poll(ctx, s); err != nil {
			// A failed poll is logged and retried: Gitea may be restarting.
			fmt.Fprintf(stderr, "simstub %s: %v\n", mode, err)
			if *once {
				return 1
			}
		}
		if *once {
			return 0
		}
		wait := 2 * time.Second
		if err == nil {
			wait = time.Duration(s.PollSeconds * float64(time.Second))
		}
		if sleepCtx(ctx, wait) != nil {
			return 0
		}
	}
}

// writeAck replaces path with the generation, by a rename, so a reader
// never sees half of it.
func writeAck(path string, gen int64) error {
	tmp := path + ".new"
	if err := os.WriteFile(tmp, []byte(fmt.Sprintf("%d\n", gen)), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// stub holds a stub's state between polls.
type stub struct {
	g     *gitea
	out   io.Writer
	sleep func(context.Context, time.Duration) error
	// seen: "repo@sha" heads ci has found its status on.
	seen map[string]bool
}

type label struct {
	ID   int64  `json:"id"`
	Name string `json:"name"`
}

type pull struct {
	Number int64   `json:"number"`
	State  string  `json:"state"`
	Labels []label `json:"labels"`
	Head   struct {
		SHA string `json:"sha"`
	} `json:"head"`
}

func (p pull) has(name string) bool {
	for _, l := range p.Labels {
		if l.Name == name {
			return true
		}
	}
	return false
}

func (st *stub) handsPoll(ctx context.Context, s scenario) error {
	for _, repo := range s.Repos {
		var pulls []pull
		if err := st.g.do(ctx, "GET", "/repos/"+repo+"/pulls?state=open&limit=50", nil, &pulls); err != nil {
			return err
		}
		for _, p := range pulls {
			if !p.has(labelRequest) || p.has(labelReviewing) {
				continue
			}
			if err := st.review(ctx, s, repo, p); err != nil {
				return err
			}
		}
	}
	return nil
}

// review serves one request on pull request p of repo.
func (st *stub) review(ctx context.Context, s scenario, repo string, p pull) error {
	ids, err := st.labelIDs(ctx, repo)
	if err != nil {
		return err
	}
	issue := fmt.Sprintf("/repos/%s/issues/%d/labels", repo, p.Number)
	// The new label goes on before the old one comes off, so the pull
	// request always carries one of the protocol's labels.
	if err := st.g.do(ctx, "POST", issue, map[string][]int64{"labels": {ids[labelReviewing]}}, nil); err != nil {
		return err
	}
	if err := st.g.do(ctx, "DELETE", fmt.Sprintf("%s/%d", issue, ids[labelRequest]), nil, nil); err != nil {
		return err
	}
	head := p.Head.SHA
	fmt.Fprintf(st.out, "hands: %s#%d: reviewing %s\n", repo, p.Number, head)
	if err := st.sleep(ctx, time.Duration(s.Hands.ReviewSeconds*float64(time.Second))); err != nil {
		return err
	}
	var now pull
	if err := st.g.do(ctx, "GET", fmt.Sprintf("/repos/%s/pulls/%d", repo, p.Number), nil, &now); err != nil {
		return err
	}
	if now.Head.SHA != head {
		fmt.Fprintf(st.out, "hands: %s#%d: the head moved from %s to %s; no review, the request is released\n",
			repo, p.Number, head, now.Head.SHA)
		return st.g.do(ctx, "DELETE", fmt.Sprintf("%s/%d", issue, ids[labelReviewing]), nil, nil)
	}
	verdict := s.Hands.Verdict
	if s.Hands.FirstVerdict != "" {
		first, err := st.firstReview(ctx, repo, p.Number)
		if err != nil {
			return err
		}
		if first {
			verdict = s.Hands.FirstVerdict
		}
	}
	body := map[string]string{"body": reviewBody(head, verdict)}
	if err := st.g.do(ctx, "POST", fmt.Sprintf("/repos/%s/issues/%d/comments", repo, p.Number), body, nil); err != nil {
		return err
	}
	if err := st.g.do(ctx, "POST", issue, map[string][]int64{"labels": {ids[labelReviewed]}}, nil); err != nil {
		return err
	}
	if err := st.g.do(ctx, "DELETE", fmt.Sprintf("%s/%d", issue, ids[labelReviewing]), nil, nil); err != nil {
		return err
	}
	fmt.Fprintf(st.out, "hands: %s#%d: %s at %s\n", repo, p.Number, verdict, head)
	return nil
}

// firstReview reports whether pull request n of repo has no review of this
// stub's yet.
func (st *stub) firstReview(ctx context.Context, repo string, n int64) (bool, error) {
	var comments []struct {
		Body string `json:"body"`
	}
	if err := st.g.do(ctx, "GET", fmt.Sprintf("/repos/%s/issues/%d/comments?limit=50", repo, n), nil, &comments); err != nil {
		return false, err
	}
	for _, c := range comments {
		if strings.HasPrefix(c.Body, reviewHeader) {
			return false, nil
		}
	}
	return true, nil
}

// labelIDs maps the protocol's labels to their ids in repo; all three must
// exist, as on a real site, where the operator creates them.
func (st *stub) labelIDs(ctx context.Context, repo string) (map[string]int64, error) {
	var labels []label
	if err := st.g.do(ctx, "GET", "/repos/"+repo+"/labels?limit=50", nil, &labels); err != nil {
		return nil, err
	}
	ids := map[string]int64{}
	for _, l := range labels {
		ids[l.Name] = l.ID
	}
	for _, n := range []string{labelRequest, labelReviewing, labelReviewed} {
		if ids[n] == 0 {
			return nil, fmt.Errorf("%s has no label %s", repo, n)
		}
	}
	return ids, nil
}

// reviewBody is a review in the oh-code-review layout, at a full head SHA.
func reviewBody(head, verdict string) string {
	blockers := "- None"
	if verdict == verdictReturn {
		blockers = "- [BLOCKER] [CONFIRMED] the scenario asks for a rework (pilot simulation)"
	}
	return fmt.Sprintf(reviewHeader+` reviewed at head %s

CURRENT OBJECTIVE
The pilot simulation's stub review of this pull request.

BLOCKERS
%s

CURRENT-SCOPE IMPROVEMENTS
- None

FOLLOW-UPS
- None

OBSERVATIONS
- None

REJECTED
- None

RISK ACCEPTED
- None

VERDICT
%s

RISK
LOW

---
sim-hands, the pilot simulation's stub reviewer
`, head, blockers, verdict)
}

type branch struct {
	Name   string `json:"name"`
	Commit struct {
		ID string `json:"id"`
	} `json:"commit"`
}

func (st *stub) ciPoll(ctx context.Context, s scenario) error {
	for _, repo := range s.Repos {
		var branches []branch
		if err := st.g.do(ctx, "GET", "/repos/"+repo+"/branches?limit=50", nil, &branches); err != nil {
			return err
		}
		for _, b := range branches {
			sha := b.Commit.ID
			key := repo + "@" + sha
			if sha == "" || st.seen[key] {
				continue
			}
			var combined struct {
				Statuses []struct {
					Context string `json:"context"`
				} `json:"statuses"`
			}
			if err := st.g.do(ctx, "GET", fmt.Sprintf("/repos/%s/commits/%s/status", repo, sha), nil, &combined); err != nil {
				return err
			}
			found := false
			for _, c := range combined.Statuses {
				found = found || c.Context == s.CI.Context
			}
			if !found {
				status := map[string]string{
					"state":       s.CI.State,
					"context":     s.CI.Context,
					"description": "the pilot simulation's stub CI",
					"target_url":  fmt.Sprintf("%s/%s/commit/%s", st.g.web(), repo, sha),
				}
				if err := st.g.do(ctx, "POST", fmt.Sprintf("/repos/%s/statuses/%s", repo, sha), status, nil); err != nil {
					return err
				}
				fmt.Fprintf(st.out, "ci: %s %s (%s): %s\n", repo, sha, b.Name, s.CI.State)
			}
			st.seen[key] = true
		}
	}
	return nil
}

// gitea is the few Gitea API calls the stubs make.
type gitea struct {
	base   string
	token  string
	client *http.Client
}

// web is the forge's web root, the API base without /api/v1.
func (g *gitea) web() string { return strings.TrimSuffix(g.base, "/api/v1") }

func (g *gitea) do(ctx context.Context, method, path string, in, out any) error {
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = strings.NewReader(string(b))
	}
	req, err := http.NewRequestWithContext(ctx, method, g.base+path, body)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "token "+g.token)
	req.Header.Set("Accept", "application/json")
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := g.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return err
	}
	// A label someone else took off already is off: the stub's goal holds,
	// and an error here would leave hands-reviewing on for good.
	if method == "DELETE" && resp.StatusCode == http.StatusNotFound {
		return nil
	}
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("%s %s: HTTP %d: %s", method, path, resp.StatusCode, strings.TrimSpace(string(data[:min(len(data), 300)])))
	}
	if out == nil || len(data) == 0 {
		return nil
	}
	if err := json.Unmarshal(data, out); err != nil {
		return errors.Join(fmt.Errorf("%s %s: the answer is not the expected JSON", method, path), err)
	}
	return nil
}
