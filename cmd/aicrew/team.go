package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/BlackVS/aicrew/internal/store"
)

// aicrew team: the operator's teams and their intended projects
// (01a0f611-9e71). Like the other operator commands it opens the store file
// directly, so aicrewd is stopped while it runs. Membership comes from
// invitations; this command only reads it.

const teamUsage = `usage:
  aicrew team create   -store PATH -name NAME [-project HUB/PROJECT ...]
  aicrew team list     -store PATH
  aicrew team show     -store PATH -team TEAM
  aicrew team projects -store PATH -team TEAM -expect-revision N [-project HUB/PROJECT ...]
  aicrew team rename   -store PATH -team TEAM -expect-revision N -name NAME
`

// projectFlags collects repeated -project HUB/PROJECT values. A value
// without a '/' keeps its text as the hub and an empty project, which the
// store refuses as invalid.
type projectFlags []store.ProjectRef

func (p *projectFlags) String() string { return "" }

func (p *projectFlags) Set(v string) error {
	hub, project, _ := strings.Cut(v, "/")
	*p = append(*p, store.ProjectRef{HubID: hub, ProjectID: project})
	return nil
}

// teamDetail is what show prints: the team and its current members.
type teamDetail struct {
	store.Team
	Members []memberView `json:"members"`
}

type memberView struct {
	AgentID   string     `json:"agent_id"`
	Role      store.Role `json:"role"`
	Revision  int64      `json:"revision"`
	CreatedAt time.Time  `json:"created_at"`
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
	storePath := fs.String("store", "", "path to the aicrew store")
	name := fs.String("name", "", "team name: 1-32 lowercase letters, digits or '-'")
	team := fs.String("team", "", "team ID")
	expect := fs.Int64("expect-revision", 0, "the team revision the change applies to, as show or list printed it")
	var projects projectFlags
	fs.Var(&projects, "project", "an intended project, HUB/PROJECT (repeatable)")
	if err := fs.Parse(args[1:]); err != nil || fs.NArg() != 0 || *storePath == "" {
		fmt.Fprint(stderr, teamUsage)
		return 2
	}
	set := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { set[f.Name] = true })
	only := func(allowed ...string) bool {
		ok := map[string]bool{"store": true}
		for _, a := range allowed {
			ok[a] = true
		}
		for name := range set {
			if !ok[name] {
				return false
			}
		}
		return true
	}
	usage := func() int { fmt.Fprint(stderr, teamUsage); return 2 }
	switch verb {
	case "create":
		if !only("name", "project") || *name == "" {
			return usage()
		}
	case "list":
		if !only() {
			return usage()
		}
	case "show":
		if !only("team") || *team == "" {
			return usage()
		}
	case "projects":
		if !only("team", "expect-revision", "project") || *team == "" || !set["expect-revision"] {
			return usage()
		}
	case "rename":
		if !only("team", "expect-revision", "name") || *team == "" || !set["expect-revision"] || *name == "" {
			return usage()
		}
	default:
		return usage()
	}

	st, err := store.Open(ctx, *storePath)
	if err != nil {
		if errors.Is(err, store.ErrStoreInUse) {
			fmt.Fprintln(stderr, "aicrew: the store is in use; stop aicrewd, then run this again")
		} else {
			fmt.Fprintln(stderr, "aicrew: open the store:", err)
		}
		return 1
	}
	defer st.Close()
	op, err := store.OperatorCaller("aicrew-cli")
	if err != nil {
		fmt.Fprintln(stderr, "aicrew:", err)
		return 1
	}
	out := json.NewEncoder(stdout)
	out.SetIndent("", "  ")
	fail := func(err error) int {
		if errors.Is(err, store.ErrRevisionConflict) {
			fmt.Fprintf(stderr, "aicrew: revision_conflict: the team changed since revision %d; run aicrew team show, then try again with its revision: %v\n", *expect, err)
			return 1
		}
		fmt.Fprintln(stderr, "aicrew:", err)
		return 1
	}
	// nameTaken refuses a name another team already has. This process holds
	// the store exclusively, so no team can appear between the check and the
	// write.
	nameTaken := func(self string) int {
		teams, err := st.ListTeams(ctx)
		if err != nil {
			return fail(err)
		}
		for _, t := range teams {
			if t.Name == *name && t.ID != self {
				fmt.Fprintf(stderr, "aicrew: team_exists: team %s is already named %q\n", t.ID, *name)
				return 1
			}
		}
		return 0
	}

	switch verb {
	case "create":
		if code := nameTaken(""); code != 0 {
			return code
		}
		t, err := st.CreateTeam(ctx, op, newKey(), store.NewTeam{Name: *name, Projects: projects})
		if err != nil {
			return fail(err)
		}
		_ = out.Encode(t)
	case "list":
		teams, err := st.ListTeams(ctx)
		if err != nil {
			return fail(err)
		}
		_ = out.Encode(teams)
	case "show":
		t, err := st.GetTeam(ctx, *team)
		if err != nil {
			return fail(err)
		}
		ms, err := st.ListMembers(ctx, *team)
		if err != nil {
			return fail(err)
		}
		d := teamDetail{Team: t, Members: []memberView{}}
		for _, m := range ms {
			d.Members = append(d.Members, memberView{AgentID: m.AgentID, Role: m.Role, Revision: m.Revision, CreatedAt: m.CreatedAt})
		}
		_ = out.Encode(d)
	case "projects":
		if projects == nil {
			projects = projectFlags{}
		}
		t, err := st.SetTeamProjects(ctx, op, newKey(), *team, *expect, projects)
		if err != nil {
			return fail(err)
		}
		_ = out.Encode(t)
	case "rename":
		if code := nameTaken(*team); code != 0 {
			return code
		}
		t, err := st.RenameTeam(ctx, op, newKey(), *team, *expect, *name)
		if err != nil {
			return fail(err)
		}
		_ = out.Encode(t)
	}
	return 0
}
