package forge

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
)

// ErrNoRerun is a re-run the forge does not offer through its API for the
// checks it was asked about: a dialect whose checks are commit statuses
// (Gitea), or failed checks that are no jobs the forge can re-run.
var ErrNoRerun = errors.New("the forge offers no re-run of these checks through its API")

// RunnerFailure reports whether word, the forge's own word for a failed
// check, names a failure of the runner rather than of the change
// (docs/DESIGN-TURNS.md, 4.2: cancelled, a lost runner, a setup timeout):
// GitHub's cancelled, timed_out, startup_failure and stale conclusions;
// GitLab's runner_system_failure, stuck_or_timeout_failure and
// scheduler_failure failure reasons; Gitea's cancelled status. A job that
// failed on the change is none of these.
func RunnerFailure(k Kind, word string) bool {
	switch k {
	case GitHub:
		return word == "cancelled" || word == "timed_out" || word == "startup_failure" || word == "stale"
	case GitLab:
		return word == "runner_system_failure" || word == "stuck_or_timeout_failure" || word == "scheduler_failure"
	case Gitea:
		return word == "cancelled"
	}
	return false
}

// RerunFailed re-runs the failed jobs among checks in the repository at
// path, where the dialect has a call for it, and returns the names of the
// jobs it re-ran. GitHub re-runs the failed jobs of each workflow run a
// failed job belongs to (the run read from the job, then one re-run of the
// run's failed jobs); GitLab retries each failed job. Gitea's checks are
// commit statuses, which name no job, so there and for checks without a job
// ID the answer is ErrNoRerun and nothing is sent.
func (c *Client) RerunFailed(ctx context.Context, host string, k Kind, token, path string, checks []Check) ([]string, error) {
	var jobs []Check
	for _, ch := range checks {
		if ch.State == ChecksFailure && ch.JobID != 0 {
			jobs = append(jobs, ch)
		}
	}
	if len(jobs) == 0 || (k != GitHub && k != GitLab) {
		return nil, ErrNoRerun
	}
	var names []string
	switch k {
	case GitHub:
		runs := map[int64]bool{}
		for _, j := range jobs {
			var job struct {
				RunID int64 `json:"run_id"`
			}
			if err := c.get(ctx, host, k, token, repoPath(k, path)+"/actions/jobs/"+strconv.FormatInt(j.JobID, 10), &job); err != nil {
				return names, fmt.Errorf("read job %d: %w", j.JobID, err)
			}
			if job.RunID == 0 {
				return names, fmt.Errorf("job %d names no workflow run", j.JobID)
			}
			if !runs[job.RunID] {
				runs[job.RunID] = true
				if err := c.do(ctx, http.MethodPost, host, k, token,
					repoPath(k, path)+"/actions/runs/"+strconv.FormatInt(job.RunID, 10)+"/rerun-failed-jobs", nil, nil); err != nil {
					return names, fmt.Errorf("re-run the failed jobs of run %d: %w", job.RunID, err)
				}
			}
			names = append(names, j.Name)
		}
	case GitLab:
		for _, j := range jobs {
			if err := c.do(ctx, http.MethodPost, host, k, token,
				repoPath(k, path)+"/jobs/"+strconv.FormatInt(j.JobID, 10)+"/retry", nil, nil); err != nil {
				return names, fmt.Errorf("retry job %d: %w", j.JobID, err)
			}
			names = append(names, j.Name)
		}
	}
	return names, nil
}
