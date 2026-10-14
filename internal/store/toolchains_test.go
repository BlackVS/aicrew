package store

import (
	"context"
	"errors"
	"testing"
)

// The operator selects a toolchain for a team's project; the member's
// projects carry it, a change advances the team's revision, a repeat
// changes nothing, a clear removes it, and an unknown name, a bad project
// or a caller other than the operator is refused.
func TestTeamToolchains(t *testing.T) {
	ctx := context.Background()
	e := newClaimStopEnv(t)
	s, op := e.s, operator(t)
	before, err := s.GetTeam(ctx, e.teamID)
	if err != nil || before.Toolchains != nil {
		t.Fatalf("before: %+v, %v", before.Toolchains, err)
	}
	tm, err := s.SetTeamToolchain(ctx, op, "k1", e.teamID, "project-t", "dev-go")
	if err != nil || tm.Toolchains["project-t"] != "dev-go" || tm.Revision != before.Revision+1 {
		t.Fatalf("set: %+v, %v", tm, err)
	}
	again, err := s.SetTeamToolchain(ctx, op, "k2", e.teamID, "project-t", "dev-go")
	if err != nil || again.Revision != tm.Revision {
		t.Fatalf("a repeat: revision %d (was %d), %v", again.Revision, tm.Revision, err)
	}
	projects, err := s.TeamProjectsWithToken(ctx, e.workerTok)
	if err != nil || len(projects) != 1 || projects[0].ProjectID != "project-t" || projects[0].Toolchain != "dev-go" {
		t.Fatalf("the member's projects: %+v, %v", projects, err)
	}
	if req, err := s.RequirementsWithToken(ctx, e.workerTok); err != nil || len(req) != 0 {
		t.Fatalf("the requirements: %+v, %v", req, err)
	}
	for name, call := range map[string]func() error{
		"unknown": func() error {
			_, err := s.SetTeamToolchain(ctx, op, "k3", e.teamID, "project-t", "dev-cobol")
			return err
		},
		"project": func() error { _, err := s.SetTeamToolchain(ctx, op, "k4", e.teamID, "a project", "dev-go"); return err },
		"no team": func() error {
			_, err := s.SetTeamToolchain(ctx, op, "k5", "no-such-team", "project-t", "dev-go")
			return err
		},
		"reconcil": func() error {
			_, err := s.SetTeamToolchain(ctx, ReconcilerCaller(), "k6", e.teamID, "project-t", "")
			return err
		},
	} {
		err := call()
		want := map[string]error{"unknown": ErrInvalid, "project": ErrInvalid, "no team": ErrNotFound, "reconcil": ErrForbidden}[name]
		if !errors.Is(err, want) {
			t.Errorf("%s: %v, want %v", name, err, want)
		}
	}
	cleared, err := s.SetTeamToolchain(ctx, op, "k7", e.teamID, "project-t", "")
	if err != nil || cleared.Toolchains != nil || cleared.Revision != tm.Revision+1 {
		t.Fatalf("clear: %+v, %v", cleared, err)
	}
	if projects, err := s.TeamProjectsWithToken(ctx, e.workerTok); err != nil || projects[0].Toolchain != "" {
		t.Fatalf("after the clear: %+v, %v", projects, err)
	}
}
