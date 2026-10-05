package server

import (
	"context"
	"time"

	"github.com/BlackVS/aicrew/internal/hubteams"
	"github.com/BlackVS/aicrew/internal/store"
)

// GrantsRefresh is how often aicrewd reads every team's grants from each
// hub with a team.read credential, to refresh the snapshot that team show
// and the inbox's project visibility read. An offer or a claim never waits
// for it: each reads its team live.
const GrantsRefresh = time.Minute

// liveGrant reads the team's grants from its hub, once, and answers whether
// the hub grants the team the task's project: the grant and "" when it does,
// or the refusal: project_not_granted, or hub_unavailable when the hub did
// not answer. The read also refreshes the team's snapshot, which the step's own
// transaction then checks. The snapshot is never the answer: a hub that
// does not answer refuses the step.
func (s *Server) liveGrant(ctx context.Context, teamID string, task store.TaskRef) (store.TeamGrant, string) {
	var none store.TeamGrant
	t, err := s.store.GetTeam(ctx, teamID)
	if err != nil {
		return none, refusalCode(err)
	}
	b, configured := s.hubs[t.Hub]
	switch {
	case t.Hub == "":
		return none, "project_not_granted" // a team that names no hub is granted nothing
	case !configured || b.teams == nil || !b.teams.CanRead():
		s.log.Warn("team read: the team's hub has no team.read credential", "team_id", t.ID, "hub", t.Hub)
		return none, "hub_unavailable"
	case task.HubID != b.id:
		return none, "project_not_granted"
	}
	at := time.Now()
	team, err := b.teams.ReadTeam(ctx, t.ID)
	read, ok := grantsRead(b.id, team, err, at)
	if !ok {
		s.log.Warn("team read", "team_id", t.ID, "hub", t.Hub, "code", hubteams.Code(err))
		return none, "hub_unavailable"
	}
	if _, err := s.store.RecordTeamGrants(ctx, store.ReconcilerCaller(), t.ID, read); err != nil {
		s.log.Error("record a team's grants", "team_id", t.ID, "err", err)
		return none, "hub_unavailable"
	}
	for _, g := range read.Grants {
		if g.ProjectID == task.ProjectID {
			return g, ""
		}
	}
	return none, "project_not_granted"
}

// grantsRead turns a team.read answer into the snapshot it records: the
// granted projects of an enabled profile; nothing for a disabled or unknown
// one. ok is false when the hub did not answer, or refused the read itself.
func grantsRead(hubID string, team hubteams.Team, err error, at time.Time) (store.TeamGrantsRead, bool) {
	read := store.TeamGrantsRead{HubID: hubID, At: at, Grants: []store.TeamGrant{}}
	switch hubteams.Code(err) {
	case "":
		if err != nil {
			return read, false
		}
	case "not_found":
		read.State = store.GrantsNotRegistered
		return read, true
	case "profile_disabled":
		read.State = store.GrantsDisabled
		return read, true
	default:
		return read, false
	}
	if !team.Enabled {
		read.State = store.GrantsDisabled
		return read, true
	}
	read.State = store.GrantsEnabled
	seen := map[string]bool{}
	for _, p := range team.Projects {
		g := store.TeamGrant{HubID: hubID, ProjectID: p.Project}
		if r := p.Repository; r != nil {
			g.Repository = &store.GrantRepository{Kind: r.Kind, URL: r.URL, Host: r.Host, Access: r.Access}
		}
		if p := p.Process; p != nil {
			g.Process = &store.GrantProcess{Repo: p.Repo, Commit: p.Commit, Manifest: p.Manifest}
		}
		// A project no task can name, or named twice, is left out rather
		// than costing the team its other grants.
		if !g.Valid() || seen[g.ProjectID] {
			continue
		}
		seen[g.ProjectID] = true
		read.Grants = append(read.Grants, g)
	}
	return read, true
}

// refreshGrants reads every team's grants from each hub with a team.read
// credential, one read per hub, and records them. A team the hub does not
// list has no profile there. A hub that does not answer keeps its teams'
// snapshots as they were.
func (s *Server) refreshGrants(ctx context.Context) {
	teams, err := s.store.ListTeams(ctx)
	if err != nil {
		s.log.Error("refresh grants: list teams", "err", err)
		return
	}
	for alias, b := range s.hubs {
		if b.teams == nil || !b.teams.CanRead() {
			continue
		}
		var mine []store.TeamSummary
		for _, t := range teams {
			if t.Hub == alias {
				mine = append(mine, t)
			}
		}
		if len(mine) == 0 {
			continue
		}
		at := time.Now()
		all, err := b.teams.ReadTeams(ctx)
		if err != nil {
			s.log.Warn("refresh grants", "hub", alias, "code", hubteams.Code(err))
			continue
		}
		byID := make(map[string]hubteams.Team, len(all))
		for _, t := range all {
			byID[t.TeamID] = t
		}
		for _, t := range mine {
			team, listed := byID[t.ID]
			var rerr error
			if !listed {
				rerr = &hubteams.Error{Code: "not_found"}
			}
			read, _ := grantsRead(b.id, team, rerr, at)
			if _, err := s.store.RecordTeamGrants(ctx, store.ReconcilerCaller(), t.ID, read); err != nil {
				s.log.Error("refresh grants: record", "team_id", t.ID, "err", err)
			}
		}
	}
}

// runGrants refreshes the grants every GrantsRefresh until ctx ends.
func (s *Server) runGrants(ctx context.Context) {
	tick := time.NewTicker(s.grantsEvery)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			s.refreshGrants(ctx)
		}
	}
}

// readsGrants reports whether any hub has a team.read credential.
func (s *Server) readsGrants() bool {
	for _, b := range s.hubs {
		if b.teams != nil && b.teams.CanRead() {
			return true
		}
	}
	return false
}
