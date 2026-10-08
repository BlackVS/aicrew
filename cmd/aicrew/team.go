package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/url"

	"github.com/BlackVS/aicrew/internal/opapi"
)

// aicrew team: the operator's teams, their hubs and the projects the hubs
// grant them (01a0f611-9e71, 354c-1a), through aicrewd's operator API.
// Membership comes from invitations; this command only reads it.

const teamUsage = `usage:
  aicrew team create   --name NAME [--hub ALIAS]
  aicrew team list
  aicrew team show     (--team TEAM | --team-name NAME)
  aicrew team rename   (--team TEAM | --team-name NAME) --expect-revision N --name NAME
  aicrew team register (--team TEAM | --team-name NAME) [--hub ALIAS]
  aicrew team setup    TEAM --hub ALIAS --project PROJECT [--project PROJECT ...]
` + connUsage

// projectsRemoved explains the project list of earlier releases.
const projectsRemoved = "aicrew: a team's projects are the grants its hub holds " +
	"(aimem identity team grant); team projects and --project are removed\n"

// removedFlag accepts a removed flag's values so that its use is explained
// rather than refused as unknown.
type removedFlag struct{}

func (removedFlag) String() string   { return "" }
func (removedFlag) Set(string) error { return nil }

// registrationNote says on stderr why the team's registration on its hub
// did not complete, and reports whether it did (or none was attempted). The
// team exists either way; aicrew team register retries.
func registrationNote(stderr io.Writer, t opapi.Team) bool {
	r := t.Registration
	if r == nil || r.State == "registered" {
		return true
	}
	fmt.Fprintf(stderr, "aicrew: team %s is not registered on hub %s (%s): %s\n", t.Name, t.Hub, r.State, r.Detail)
	return false
}

// runTeam is aicrew team: 0 on success, 1 on a failure, 2 on a usage error.
func runTeam(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	if len(args) < 1 {
		fmt.Fprint(stderr, teamUsage)
		return 2
	}
	verb := args[0]
	if verb == "setup" {
		return runTeamSetup(ctx, args[1:], stdout, stderr)
	}
	fs := flag.NewFlagSet("aicrew team "+verb, flag.ContinueOnError)
	fs.SetOutput(stderr)
	cn := addConn(fs)
	name := fs.String("name", "", "team name: 1-32 lowercase letters, digits or '-'")
	tf := addTeamFlags(fs)
	expect := fs.Int64("expect-revision", 0, "the team revision the change applies to, as show or list printed it")
	hub := fs.String("hub", "", "the alias of the team's aimem block (aicrewd.json aimem_hubs[].name)")
	fs.Var(removedFlag{}, "project", "removed: a team's projects are its hub's grants")
	if err := fs.Parse(args[1:]); err != nil || fs.NArg() != 0 {
		fmt.Fprint(stderr, teamUsage)
		return 2
	}
	set := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { set[f.Name] = true })
	usage := func() int { fmt.Fprint(stderr, teamUsage); return 2 }
	if verb == "projects" || set["project"] {
		fmt.Fprint(stderr, projectsRemoved)
		return usage()
	}
	teamGiven, teamOK := tf.given(stderr)
	if !teamOK {
		return 2
	}
	switch verb {
	case "create":
		if !allowedFlags(set, "name", "hub") || *name == "" || (set["hub"] && *hub == "") {
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
	case "rename":
		if !allowedFlags(set, "team", "team-name", "expect-revision", "name") || !teamGiven || !set["expect-revision"] || *name == "" {
			return usage()
		}
	case "register":
		if !allowedFlags(set, "team", "team-name", "hub") || !teamGiven || (set["hub"] && *hub == "") {
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
		if err := cl.Post(ctx, opapi.TeamsPath, opapi.TeamRequest{Name: *name, Hub: *hub}, &t); err != nil {
			return failed(stderr, err)
		}
		_ = out.Encode(t)
		registrationNote(stderr, t)
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
	case "rename":
		var t opapi.Team
		if err := cl.Post(ctx, opapi.TeamRenamePath, opapi.TeamRenameRequest{TeamID: team, ExpectedRevision: *expect,
			Name: *name}, &t); err != nil {
			return revisionHint(stderr, err, *expect)
		}
		_ = out.Encode(t)
		registrationNote(stderr, t)
	case "register":
		var t opapi.Team
		if err := cl.Post(ctx, opapi.TeamRegisterPath, opapi.TeamRegisterRequest{ID: team, Hub: *hub}, &t); err != nil {
			return failed(stderr, err)
		}
		_ = out.Encode(t)
		if !registrationNote(stderr, t) {
			return 1
		}
	}
	return 0
}
