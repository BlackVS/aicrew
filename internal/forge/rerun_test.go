package forge

import (
	"context"
	"errors"
	"slices"
	"testing"

	"github.com/BlackVS/aicrew/internal/forge/fakeforge"
)

// The runner-failure set per dialect (docs/DESIGN-TURNS.md, 4.2): the
// forge's own words for a cancelled job, a lost runner or a setup timeout,
// and nothing a change can cause.
func TestRunnerFailure(t *testing.T) {
	for _, tc := range []struct {
		kind Kind
		word string
		want bool
	}{
		{GitHub, "cancelled", true}, {GitHub, "timed_out", true}, {GitHub, "startup_failure", true}, {GitHub, "stale", true},
		{GitHub, "failure", false}, {GitHub, "action_required", false}, {GitHub, "neutral", false}, {GitHub, "", false},
		{GitLab, "runner_system_failure", true}, {GitLab, "stuck_or_timeout_failure", true}, {GitLab, "scheduler_failure", true},
		{GitLab, "script_failure", false}, {GitLab, "failed", false}, {GitLab, "canceled", false}, {GitLab, "", false},
		{Gitea, "cancelled", true}, {Gitea, "failure", false}, {Gitea, "error", false}, {Gitea, "", false},
		{Kind("other"), "cancelled", false},
	} {
		if got := RunnerFailure(tc.kind, tc.word); got != tc.want {
			t.Errorf("RunnerFailure(%s, %q) = %v, want %v", tc.kind, tc.word, got, tc.want)
		}
	}
}

// The checks a pull request's content reads carry the job ID a re-run
// needs and whether the failure is the runner's: GitHub from the check
// run's conclusion, GitLab from the job's failure reason; Gitea's commit
// statuses name no job and report a cancelled run as a failure, so neither
// is known there. RerunFailed then re-runs the failed jobs where the
// dialect has a call: GitHub's failed jobs of each workflow run once,
// GitLab's jobs each, Gitea none.
func TestRerunFailed(t *testing.T) {
	ctx := context.Background()
	for _, k := range []Kind{GitHub, Gitea, GitLab} {
		t.Run(string(k), func(t *testing.T) {
			f := fakeforge.New(t, string(k))
			c := &Client{HTTP: f.Client}
			if _, err := c.OpenPull(ctx, f.Host, k, fakeforge.Token, fakeforge.Repo, NewPull{Title: "Fix", Head: "feat/x", Base: "main"}); err != nil {
				t.Fatal(err)
			}
			f.SetPull(1, func(p *fakeforge.Pull) {
				p.Checks = []fakeforge.Check{
					{Name: "lint", Status: "completed", Conclusion: "success"},
					{Name: "test", Status: "completed", Conclusion: "failure", Log: "FAIL\n"},
					{Name: "build", Status: "completed", Conclusion: "cancelled", FailureReason: "runner_system_failure", Log: "lost\n"},
					{Name: "deploy", Status: "completed", Conclusion: "timed_out", FailureReason: "stuck_or_timeout_failure", Run: 2, Log: "slow\n"},
				}
			})
			_, content, err := c.GetPullContent(ctx, f.Host, k, fakeforge.Token, fakeforge.Repo, 1)
			if err != nil {
				t.Fatal(err)
			}
			type seen struct {
				name   string
				job    bool
				runner bool
			}
			var got []seen
			for _, ch := range content.Checks {
				got = append(got, seen{ch.Name, ch.JobID != 0, ch.RunnerFailure})
			}
			want := []seen{{"lint", false, false}, {"test", k != Gitea, false}, {"build", k != Gitea, k != Gitea}, {"deploy", k != Gitea, k != Gitea}}
			if !slices.Equal(got, want) {
				t.Fatalf("the checks: %+v, want %+v", got, want)
			}
			names, err := c.RerunFailed(ctx, f.Host, k, fakeforge.Token, fakeforge.Repo, content.Checks)
			switch k {
			case Gitea:
				if !errors.Is(err, ErrNoRerun) || len(f.Reruns) != 0 {
					t.Fatalf("Gitea re-ran: %v %v", names, err)
				}
			case GitHub:
				if err != nil || !slices.Equal(names, []string{"test", "build", "deploy"}) || !slices.Equal(f.Reruns, []string{"run 1", "run 2"}) {
					t.Fatalf("GitHub: %v %v, re-runs %v", names, err, f.Reruns)
				}
			case GitLab:
				if err != nil || !slices.Equal(names, []string{"test", "build", "deploy"}) || !slices.Equal(f.Reruns, []string{"job 5001", "job 5002", "job 5003"}) {
					t.Fatalf("GitLab: %v %v, re-runs %v", names, err, f.Reruns)
				}
			}
			// Nothing failed, or nothing with a job: no call is sent.
			if _, err := c.RerunFailed(ctx, f.Host, k, fakeforge.Token, fakeforge.Repo, []Check{{Name: "x", State: ChecksFailure}}); !errors.Is(err, ErrNoRerun) {
				t.Errorf("a failed check without a job: %v", err)
			}
		})
	}
}
