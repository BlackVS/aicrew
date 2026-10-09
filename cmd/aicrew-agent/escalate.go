package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/BlackVS/aicrew/internal/agent"
)

const escalateUsage = `usage: aicrew-agent escalate [--home DIR] --body FILE|-
  Raises an escalation as the team's coordinator, through the launcher: a
  question the coordinator cannot settle within its role, for the architect
  to answer (docs/DESIGN-CONTROL-PLANE.md, section 7.5). The body is the
  request's JSON: task, category, question, context, two to four options
  with their consequences, recommendation, blocked (a member's agent ID, or
  none) and urgency. The answer arrives in the coordinator's inbox, and the
  blocked member's, as a lifecycle message carrying the escalation.
  --home defaults to $` + agent.HomeEnv + `, which the launcher gives its client.`

// maxEscalateBody bounds the body read from a file or standard input.
const maxEscalateBody = 64 << 10

// escalate raises an escalation through the agent home's launcher, which
// holds the session. Its exit codes are step's.
func escalate(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer, getenv func(string) string) int {
	fs := flag.NewFlagSet("aicrew-agent escalate", flag.ContinueOnError)
	fs.SetOutput(stderr)
	home := fs.String("home", getenv(agent.HomeEnv), "the agent home directory")
	bodyFrom := fs.String("body", "", "the request's JSON: a file, or - for standard input")
	if err := fs.Parse(args); err != nil || *home == "" || *bodyFrom == "" || fs.NArg() != 0 {
		fmt.Fprintln(stderr, escalateUsage)
		return exitUsage
	}
	var r io.Reader = stdin
	if *bodyFrom != "-" {
		f, err := os.Open(*bodyFrom)
		if err != nil {
			fmt.Fprintln(stderr, "aicrew-agent escalate:", err)
			return exitUsage
		}
		defer f.Close()
		r = f
	}
	body, err := io.ReadAll(io.LimitReader(r, maxEscalateBody+1))
	if err != nil || len(body) > maxEscalateBody || !json.Valid(body) {
		fmt.Fprintln(stderr, "aicrew-agent escalate: the body is not a JSON request of at most 64 KiB")
		return exitUsage
	}
	ans, err := agent.CallStep(ctx, *home, agent.StepCall{Op: "escalate", Body: body})
	if err != nil {
		fmt.Fprintln(stderr, err)
		if errors.Is(err, agent.ErrNoLauncher) {
			fmt.Fprintln(stderr, "no launcher serves", *home)
		}
		return stepFailed
	}
	out, _ := json.MarshalIndent(ans, "", "  ")
	fmt.Fprintln(stdout, string(out))
	return stepExit(ans.Status)
}
