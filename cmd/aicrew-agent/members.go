package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"

	"github.com/BlackVS/aicrew/internal/agent"
)

const membersUsage = `usage: aicrew-agent members [--home DIR] [--json]
  Lists the team's members through the launcher: each one's agent ID, label
  and role, and the repositories its last capability report verified. The
  team's coordinator names a worker's agent ID from here as an offer's
  worker_agent_id.
  --home defaults to $` + agent.HomeEnv + `, which the launcher gives its client.`

// members lists the team's members through the agent home's launcher, which
// holds the session. Its exit codes are step's.
func members(ctx context.Context, args []string, stdout, stderr io.Writer, getenv func(string) string) int {
	fs := flag.NewFlagSet("aicrew-agent members", flag.ContinueOnError)
	fs.SetOutput(stderr)
	home := fs.String("home", getenv(agent.HomeEnv), "the agent home directory")
	asJSON := fs.Bool("json", false, "print the launcher's answer as JSON")
	if err := fs.Parse(args); err != nil || *home == "" || fs.NArg() != 0 {
		fmt.Fprintln(stderr, membersUsage)
		return exitUsage
	}
	ans, err := agent.CallStep(ctx, *home, agent.StepCall{Op: "members"})
	if err != nil {
		fmt.Fprintln(stderr, err)
		if errors.Is(err, agent.ErrNoLauncher) {
			fmt.Fprintln(stderr, "no launcher serves", *home)
		}
		return stepFailed
	}
	if *asJSON || !ans.OK {
		out, _ := json.MarshalIndent(ans, "", "  ")
		fmt.Fprintln(stdout, string(out))
		return stepExit(ans.Status)
	}
	var team agent.TeamMembers
	if err := json.Unmarshal(ans.Result, &team); err != nil {
		fmt.Fprintln(stderr, "the launcher's answer is not a list of members:", err)
		return stepFailed
	}
	if len(team.Members) == 0 {
		fmt.Fprintln(stdout, "The team has no members.")
	}
	for _, m := range team.Members {
		fmt.Fprintln(stdout, m.Line())
	}
	return stepExit(ans.Status)
}
