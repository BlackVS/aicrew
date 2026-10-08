package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"regexp"
	"sort"
	"strings"

	"github.com/BlackVS/aicrew/internal/opapi"
	"github.com/BlackVS/aicrew/internal/opclient"
)

// aicrew team setup: a team on its hub with its projects, in one command
// (01a119aa-85ea). It creates the team when it does not exist, registers it
// on its hub unless it is already registered there, reads its grants from
// the hub live, and prints, for each project the hub does not grant yet,
// the grant command the hub's admin runs. Run again, it changes nothing a
// previous run settled.

const teamSetupUsage = `usage:
  aicrew team setup TEAM --hub ALIAS --project PROJECT [--project PROJECT ...]
` + connUsage

// projectShape is a project name aicrewd can hold as a grant (the store's
// reference rule); the hub's grant of any other name is never usable.
var projectShape = regexp.MustCompile(`^[A-Za-z0-9._:-]{1,128}$`)

// projectList is a repeatable --project.
type projectList []string

func (p *projectList) String() string { return strings.Join(*p, ",") }
func (p *projectList) Set(v string) error {
	if !projectShape.MatchString(v) {
		return fmt.Errorf("a project name is 1 to 128 characters from [A-Za-z0-9._:-]")
	}
	for _, q := range *p {
		if q == v {
			return nil
		}
	}
	*p = append(*p, v)
	return nil
}

// runTeamSetup is aicrew team setup: 0 when the team is registered and every
// project granted, 3 when grants are still missing, 1 on a failure or a
// refusal, 2 on a usage error.
func runTeamSetup(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	if len(args) < 1 || strings.HasPrefix(args[0], "-") {
		fmt.Fprint(stderr, teamSetupUsage)
		return 2
	}
	name := args[0]
	fs := flag.NewFlagSet("aicrew team setup", flag.ContinueOnError)
	fs.SetOutput(stderr)
	cn := addConn(fs)
	hub := fs.String("hub", "", "the alias of the team's hub (aicrewd.json aimem_hubs[].name)")
	var projects projectList
	fs.Var(&projects, "project", "a project the team works on; repeat it for each")
	if err := fs.Parse(args[1:]); err != nil || fs.NArg() != 0 || *hub == "" || len(projects) == 0 {
		fmt.Fprint(stderr, teamSetupUsage)
		return 2
	}
	cl, ok := cn.connect(stderr)
	if !ok {
		return 1
	}
	t, code := setupRegistered(ctx, cl, name, *hub, stdout, stderr)
	if code != 0 {
		return code
	}
	var g opapi.TeamGrants
	if err := cl.Post(ctx, opapi.TeamGrantsPath, opapi.TeamGrantsRequest{ID: t.ID}, &g); err != nil {
		return failed(stderr, err)
	}
	return reportGrants(stdout, g, projects)
}

// setupRegistered is the team named name, created on hub when it does not
// exist, and registered there unless it already is. Its code is 0, or the
// exit status of the failure it reported.
func setupRegistered(ctx context.Context, cl *opclient.Client, name, hub string, stdout, stderr io.Writer) (opapi.Team, int) {
	var teams []opapi.TeamSummary
	if err := cl.Get(ctx, opapi.TeamsPath, nil, &teams); err != nil {
		return opapi.Team{}, failed(stderr, err)
	}
	var t opapi.Team
	found := false
	for _, s := range teams {
		if s.Name == name {
			t, found = s.Team, true
		}
	}
	switch {
	case !found:
		if err := cl.Post(ctx, opapi.TeamsPath, opapi.TeamRequest{Name: name, Hub: hub}, &t); err != nil {
			return t, failed(stderr, err)
		}
		fmt.Fprintf(stdout, "team %s created on hub %s (%s)\n", t.Name, hub, t.ID)
	case t.Hub != "" && t.Hub != hub:
		fmt.Fprintf(stderr, "aicrew: team %s is on hub %s, not %s; a team keeps its hub\n", name, t.Hub, hub)
		return t, 1
	case t.Registration != nil && t.Registration.State == "registered" && t.Hub == hub:
		fmt.Fprintf(stdout, "team %s is already registered on hub %s\n", t.Name, hub)
		return t, 0
	default:
		if err := cl.Post(ctx, opapi.TeamRegisterPath, opapi.TeamRegisterRequest{ID: t.ID, Hub: hub}, &t); err != nil {
			return t, failed(stderr, err)
		}
	}
	r := t.Registration
	switch {
	case r == nil:
		fmt.Fprintf(stderr, "aicrew: hub %s has no team.register credential (aimem_hubs[].team_register_token_file): run aicrew hub add %s\n", hub, hub)
		return t, 1
	case r.State != "registered":
		fmt.Fprintf(stderr, "aicrew: hub %s refused the registration of team %s with %s: %s\n", hub, t.Name, r.State, r.Detail)
		return t, 1
	}
	fmt.Fprintf(stdout, "team %s is registered on hub %s\n", t.Name, hub)
	return t, 0
}

// reportGrants prints each wanted project's grant, or the command that
// grants it, and the projects the hub grants beyond them.
func reportGrants(stdout io.Writer, g opapi.TeamGrants, projects []string) int {
	granted := map[string]opapi.Grant{}
	for _, gr := range g.Grants {
		granted[gr.ProjectID] = gr
	}
	if g.GrantsState == "disabled" {
		fmt.Fprintf(stdout, "the hub has disabled team %s's profile: no project is granted until its admin enables it\n", g.Name)
	}
	var missing []string
	for _, p := range projects {
		gr, ok := granted[p]
		if !ok {
			missing = append(missing, p)
			continue
		}
		fmt.Fprintf(stdout, "granted: %s\n", p)
		if gr.Repository == nil {
			fmt.Fprintf(stdout, "  %s has no repository on the hub: aimem project repo set (on the hub)\n", p)
		}
		if gr.Process == nil {
			fmt.Fprintf(stdout, "  %s has no selected process on the hub: select one on the hub\n", p)
		}
		delete(granted, p)
	}
	also := make([]string, 0, len(granted))
	for p := range granted {
		also = append(also, p)
	}
	sort.Strings(also)
	for _, p := range also {
		fmt.Fprintf(stdout, "also granted: %s\n", p)
	}
	if len(missing) == 0 {
		fmt.Fprintf(stdout, "every project is granted to team %s\n", g.Name)
		return 0
	}
	fmt.Fprintf(stdout, "not granted yet; on the hub, as its admin, run:\n")
	for _, p := range missing {
		fmt.Fprintf(stdout, "  aimem identity team grant --peer %s --team-name %s --project %s\n", g.ServiceID, g.Name, p)
	}
	fmt.Fprintf(stdout, "then run this command again to check\n")
	return exitIncomplete
}
