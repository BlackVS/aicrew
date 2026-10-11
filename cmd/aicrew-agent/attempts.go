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

const attemptsUsage = `usage: aicrew-agent attempts [--home DIR] [--json]
  Lists the team's open attempts through the launcher, for the team's
  coordinator to reconcile: each attempt's task, worker and state, whether
  an offer can still be accepted, and what to do next. The launcher records
  the same list in ` + agent.OpenAttemptsFile + ` when the coordinator's session starts.
  --home defaults to $` + agent.HomeEnv + `, which the launcher gives its client.`

// attempts lists the team's open attempts through the agent home's launcher,
// which holds the session. Its exit codes are step's.
func attempts(ctx context.Context, args []string, stdout, stderr io.Writer, getenv func(string) string) int {
	fs := flag.NewFlagSet("aicrew-agent attempts", flag.ContinueOnError)
	fs.SetOutput(stderr)
	home := fs.String("home", getenv(agent.HomeEnv), "the agent home directory")
	asJSON := fs.Bool("json", false, "print the launcher's answer as JSON")
	if err := fs.Parse(args); err != nil || *home == "" || fs.NArg() != 0 {
		fmt.Fprintln(stderr, attemptsUsage)
		return exitUsage
	}
	ans, err := agent.CallStep(ctx, *home, agent.StepCall{Op: "attempts"})
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
	var open agent.OpenAttempts
	if err := json.Unmarshal(ans.Result, &open); err != nil {
		fmt.Fprintln(stderr, "the launcher's answer is not a list of attempts:", err)
		return stepFailed
	}
	writeOpenAttempts(stdout, open)
	return stepExit(ans.Status)
}

func writeOpenAttempts(w io.Writer, open agent.OpenAttempts) {
	if len(open.Attempts) == 0 {
		fmt.Fprintln(w, "The team has no open attempts.")
		return
	}
	for _, a := range open.Attempts {
		fmt.Fprintln(w, a.Line())
	}
}

// printOpenAttempts records a coordinator's open attempts at session start
// and prints them to stderr; stdout carries only the session's variable.
func printOpenAttempts(ctx context.Context, e *agent.Engine, stderr io.Writer) {
	open, ok, err := e.RecordOpenAttempts(ctx)
	switch {
	case err != nil:
		fmt.Fprintf(stderr, "The team's open attempts could not be read: %v\n", err)
	case ok:
		fmt.Fprintf(stderr, "The team's open attempts, also in %s; reconcile them first:\n", agent.OpenAttemptsFile)
		writeOpenAttempts(stderr, open)
	}
}
