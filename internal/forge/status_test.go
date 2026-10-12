package forge

import (
	"context"
	"slices"
	"testing"

	"github.com/BlackVS/aicrew/internal/forge/fakeforge"
)

// Each dialect's status read gives the pull request's state, labels, head
// commit, the head's CI conclusion and its comment count, as the forge
// changes them.
func TestPullStatus(t *testing.T) {
	ctx := context.Background()
	for _, k := range []Kind{GitHub, Gitea, GitLab} {
		t.Run(string(k), func(t *testing.T) {
			f := fakeforge.New(t, string(k))
			c := &Client{HTTP: f.Client}
			if _, err := c.OpenPull(ctx, f.Host, k, fakeforge.Token, fakeforge.Repo,
				NewPull{Title: "Fix", Head: "feat/x", Base: "main"}); err != nil {
				t.Fatal(err)
			}
			read := func() PullStatus {
				t.Helper()
				st, err := c.GetPullStatus(ctx, f.Host, k, fakeforge.Token, fakeforge.Repo, 1)
				if err != nil {
					t.Fatal(err)
				}
				return st
			}
			st := read()
			if st.Number != 1 || st.State != PullOpen || st.Checks != ChecksNone || st.Comments != 0 ||
				st.HeadCommit != fakeforge.HeadCommit || len(st.Labels) != 0 {
				t.Fatalf("a new pull request: %+v", st)
			}
			for _, ci := range []string{ChecksPending, ChecksFailure, ChecksSuccess} {
				f.SetPull(1, func(p *fakeforge.Pull) { p.CI = ci })
				if got := read().Checks; got != ci {
					t.Errorf("CI %s read as %s", ci, got)
				}
			}
			f.SetPull(1, func(p *fakeforge.Pull) { p.AddLabel("hands-reviewed"); p.Comment("READY"); p.Comment("ok") })
			if st = read(); st.Comments != 2 || !slices.Equal(st.Labels, []string{"hands-reviewed"}) {
				t.Errorf("after a review: %+v", st)
			}
			f.SetPull(1, func(p *fakeforge.Pull) { p.State = "merged" })
			if st = read(); st.State != PullMerged || st.MergeCommit == "" {
				t.Errorf("merged: %+v", st)
			}
			if f.Unauthorized != 0 {
				t.Errorf("%d requests went without the token", f.Unauthorized)
			}
		})
	}
}

// Several conclusions combine to one: a failure wins, then a pending one.
func TestCombineChecks(t *testing.T) {
	for _, c := range []struct {
		in   []string
		want string
	}{
		{nil, ChecksNone},
		{[]string{ChecksSuccess, ChecksSuccess}, ChecksSuccess},
		{[]string{ChecksSuccess, ChecksPending}, ChecksPending},
		{[]string{ChecksPending, ChecksFailure, ChecksSuccess}, ChecksFailure},
		{[]string{ChecksNone, ChecksSuccess}, ChecksSuccess},
	} {
		if got := combine(c.in); got != c.want {
			t.Errorf("combine(%v) = %s, want %s", c.in, got, c.want)
		}
	}
	if runChecks("queued", "") != ChecksPending || runChecks("completed", "timed_out") != ChecksFailure ||
		statusChecks("error") != ChecksFailure || pipelineChecks("manual") != ChecksPending {
		t.Error("a conclusion is misread")
	}
}
