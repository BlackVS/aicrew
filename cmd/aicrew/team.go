package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/url"
	"strings"

	"github.com/BlackVS/aicrew/internal/opapi"
)

// aicrew team: the operator's teams and their intended projects
// (01a0f611-9e71), through aicrewd's operator API. Membership comes from
// invitations; this command only reads it.

const teamUsage = `usage:
  aicrew team create   --name NAME [--project HUB/PROJECT ...]
  aicrew team list
  aicrew team show     (--team TEAM | --team-name NAME)
  aicrew team projects (--team TEAM | --team-name NAME) --expect-revision N [--project HUB/PROJECT ...]
  aicrew team rename   (--team TEAM | --team-name NAME) --expect-revision N --name NAME
` + connUsage

// projectFlags collects repeated --project HUB/PROJECT values. A value
// without a '/' keeps its text as the hub and an empty project, which the
// service refuses as invalid.
type projectFlags []opapi.ProjectRef

func (p *projectFlags) String() string { return "" }

func (p *projectFlags) Set(v string) error {
	hub, project, _ := strings.Cut(v, "/")
	*p = append(*p, opapi.ProjectRef{HubID: hub, ProjectID: project})
	return nil
}

// runTeam is aicrew team: 0 on success, 1 on a failure, 2 on a usage error.
func runTeam(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	if len(args) < 1 {
		fmt.Fprint(stderr, teamUsage)
		return 2
	}
	verb := args[0]
	fs := flag.NewFlagSet("aicrew team "+verb, flag.ContinueOnError)
	fs.SetOutput(stderr)
	cn := addConn(fs)
	name := fs.String("name", "", "team name: 1-32 lowercase letters, digits or '-'")
	tf := addTeamFlags(fs)
	expect := fs.Int64("expect-revision", 0, "the team revision the change applies to, as show or list printed it")
	var projects projectFlags
	fs.Var(&projects, "project", "an intended project, HUB/PROJECT (repeatable)")
	if err := fs.Parse(args[1:]); err != nil || fs.NArg() != 0 {
		fmt.Fprint(stderr, teamUsage)
		return 2
	}
	set := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { set[f.Name] = true })
	usage := func() int { fmt.Fprint(stderr, teamUsage); return 2 }
	teamGiven, teamOK := tf.given(stderr)
	if !teamOK {
		return 2
	}
	switch verb {
	case "create":
		if !allowedFlags(set, "name", "project") || *name == "" {
			return usage()
		}
	case "list":
		if !allowedFlags(set) {
			return usage()
		}
	case "show":
		if !allowedFlags(set, "team", "team-name") || !teamGiven {
			return usage()
		}
	case "projects":
		if !allowedFlags(set, "team", "team-name", "expect-revision", "project") || !teamGiven || !set["expect-revision"] {
			return usage()
		}
	case "rename":
		if !allowedFlags(set, "team", "team-name", "expect-revision", "name") || !teamGiven || !set["expect-revision"] || *name == "" {
			return usage()
		}
	default:
		return usage()
	}
	cl, ok := cn.connect(stderr)
	if !ok {
		return 1
	}
	team, err := tf.resolve(ctx, cl)
	if err != nil {
		return failed(stderr, err)
	}
	out := json.NewEncoder(stdout)
	out.SetIndent("", "  ")

	switch verb {
	case "create":
		var t opapi.Team
		if err := cl.Post(ctx, opapi.TeamsPath, opapi.TeamRequest{Name: *name, Projects: projects}, &t); err != nil {
			return failed(stderr, err)
		}
		_ = out.Encode(t)
	case "list":
		var teams []opapi.TeamSummary
		if err := cl.Get(ctx, opapi.TeamsPath, nil, &teams); err != nil {
			return failed(stderr, err)
		}
		_ = out.Encode(teams)
	case "show":
		var d opapi.TeamDetail
		if err := cl.Get(ctx, opapi.TeamPath, url.Values{"id": {team}}, &d); err != nil {
			return failed(stderr, err)
		}
		_ = out.Encode(d)
	case "projects":
		if projects == nil {
			projects = projectFlags{}
		}
		var t opapi.Team
		if err := cl.Post(ctx, opapi.TeamProjectsPath, opapi.TeamProjectsRequest{TeamID: team, ExpectedRevision: *expect,
			Projects: projects}, &t); err != nil {
			return revisionHint(stderr, err, *expect)
		}
		_ = out.Encode(t)
	case "rename":
		var t opapi.Team
		if err := cl.Post(ctx, opapi.TeamRenamePath, opapi.TeamRenameRequest{TeamID: team, ExpectedRevision: *expect,
			Name: *name}, &t); err != nil {
			return revisionHint(stderr, err, *expect)
		}
		_ = out.Encode(t)
	}
	return 0
}
