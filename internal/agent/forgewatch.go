package agent

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"time"

	"github.com/BlackVS/aicrew/internal/forge"
)

// Forge events (docs/CREW-CONTRACT.md, "Forge events"; pilot finding
// 2026-10-12: a member waiting for a review with a background watch was
// never woken, since only an inbox message ends the Stop hook's wait).
// aicrewd reads no forge. Every `forge pr` command reports the pull
// request it acted on to aicrewd through the launcher, as its baseline, and
// the launcher keeps it in PullsFile. While the member waits for its inbox,
// the launcher reads each of those pull requests every forgePoll with the
// home's own credential and reports what it sees; aicrewd turns a change
// into a forge.event message, which ends the wait like any other message.

// PullsFile is the home file, relative to the home, that lists the pull
// requests the launcher watches. It holds no secret.
const PullsFile = "state/pulls.json"

const (
	pullsPath = "/v1/crew/pulls"
	// maxWatched bounds the pull requests the launcher watches; the oldest
	// goes first.
	maxWatched = 20
	// pollBudget bounds one pass over the watched pull requests, so a slow
	// forge cannot hold the member's wait past its end.
	pollBudget = 30 * time.Second
)

// forgePoll is how often the launcher reads the watched pull requests while
// the member waits; tests shorten it.
var forgePoll = 60 * time.Second

// PullStatusAPI reads a pull request's status; forge.Client is the real
// one.
type PullStatusAPI interface {
	GetPullStatus(ctx context.Context, host string, k forge.Kind, token, path string, n int64) (forge.PullStatus, error)
}

// newPullStatusAPI is the forge the launcher's watch reads; tests replace
// it.
var newPullStatusAPI = func() PullStatusAPI { return forge.NewClient() }

// PullObservation is a pull request as a member observed it, in the shape
// aicrewd's route takes.
type PullObservation struct {
	Host       string   `json:"host"`
	Repository string   `json:"repository"`
	Number     int64    `json:"number"`
	URL        string   `json:"url"`
	State      string   `json:"state"`
	Head       string   `json:"head"`
	HeadCommit string   `json:"head_commit"`
	Labels     []string `json:"labels"`
	Checks     string   `json:"checks"`
	Comments   int64    `json:"comments"`
	// Baseline marks a forge pr command's own report, which announces
	// nothing.
	Baseline bool `json:"baseline,omitempty"`
}

func observation(host, path string, st forge.PullStatus) PullObservation {
	labels := st.Labels
	if labels == nil {
		labels = []string{}
	}
	return PullObservation{Host: host, Repository: path, Number: st.Number, URL: st.URL, State: st.State, Head: st.Head,
		HeadCommit: st.HeadCommit, Labels: labels, Checks: st.Checks, Comments: st.Comments}
}

// watchedPull is one pull request the launcher watches.
type watchedPull struct {
	Host       string `json:"host"`
	Repository string `json:"repository"`
	Number     int64  `json:"number"`
}

type pullsDoc struct {
	Pulls []watchedPull `json:"pulls"`
}

func pullsPathIn(home string) string { return filepath.Join(home, filepath.FromSlash(PullsFile)) }

// pullsMu serializes the launcher's changes to PullsFile.
var pullsMu sync.Mutex

func readWatched(home string) []watchedPull {
	raw, err := os.ReadFile(pullsPathIn(home))
	if err != nil {
		return nil
	}
	var d pullsDoc
	if json.Unmarshal(raw, &d) != nil {
		return nil
	}
	return d.Pulls
}

// setWatched adds p to the watched pull requests, or removes it.
func setWatched(home string, p watchedPull, watch bool) error {
	pullsMu.Lock()
	defer pullsMu.Unlock()
	cur := slices.DeleteFunc(readWatched(home), func(w watchedPull) bool { return w == p })
	if watch {
		cur = append(cur, p)
		if len(cur) > maxWatched {
			cur = cur[len(cur)-maxWatched:]
		}
	}
	b, _ := json.MarshalIndent(pullsDoc{Pulls: cur}, "", "  ")
	if err := os.MkdirAll(filepath.Dir(pullsPathIn(home)), 0o700); err != nil {
		return err
	}
	return writeAtomic(pullsPathIn(home), append(b, '\n'))
}

// observePull reports an observation to aicrewd as the launcher's session,
// and keeps the pull request watched while it is open and an attempt's;
// one that is no running attempt's of the member, or is merged or closed,
// is no longer watched.
func (s *StepServer) observePull(ctx context.Context, body []byte) (json.RawMessage, error) {
	var o PullObservation
	if err := json.Unmarshal(body, &o); err != nil || o.Number < 1 || o.Host == "" || o.Repository == "" {
		return nil, &Refusal{Code: "invalid_request", Message: "The observation names no pull request."}
	}
	w := watchedPull{Host: o.Host, Repository: o.Repository, Number: o.Number}
	out, err := s.local.LocalStep(ctx, newKey("pull"), s.driver.Session.stepToken(), pullsPath, body)
	var ref *Refusal
	switch {
	case errors.As(err, &ref) && ref.Code == "no_attempt":
		_ = setWatched(s.home, w, false)
		return nil, err
	case err != nil:
		return nil, err
	}
	if werr := setWatched(s.home, w, o.State == forge.PullOpen); werr != nil {
		s.log.Warn("forge events: record the watched pull request", "error", werr.Error())
	}
	return out, nil
}

// pollPulls reads each watched pull request with the home's credential and
// reports it. A pull request the home cannot read is left for the next
// poll; nothing here ends the member's wait.
func (s *StepServer) pollPulls(ctx context.Context) {
	watched := readWatched(s.home)
	if len(watched) == 0 {
		return
	}
	doc, _, err := readAgentDoc(s.home)
	if err != nil {
		s.log.Warn("forge events: read agent.json", "error", err.Error())
		return
	}
	ctx, cancel := context.WithTimeout(ctx, pollBudget)
	defer cancel()
	api := newPullStatusAPI()
	for _, w := range watched {
		e, tok, err := forgeTokenFor(s.home, doc, w.Host)
		if err != nil || !forge.Kind(e.Kind).Valid() {
			s.log.Warn("forge events: no credential for the pull request's host", "host", w.Host)
			continue
		}
		st, err := api.GetPullStatus(ctx, w.Host, forge.Kind(e.Kind), tok, w.Repository, w.Number)
		if err != nil {
			s.log.Warn("forge events: read a pull request", "host", w.Host, "repository", w.Repository, "pull", w.Number,
				"error", RedactForgeTokens(s.home, err.Error()))
			continue
		}
		body, _ := json.Marshal(observation(w.Host, w.Repository, st))
		if _, err := s.observePull(ctx, body); err != nil {
			s.log.Warn("forge events: report a pull request", "pull", w.Number, "error", err.Error())
		}
	}
}
