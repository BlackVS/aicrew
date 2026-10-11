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

const escalationsUsage = `usage: aicrew-agent escalations [--home DIR] [--json]
  Lists the open (unanswered) escalations through the launcher: the team's,
  for its coordinator, and a member's own otherwise. The coordinator checks
  them before raising a question, and never raises one an open escalation
  already asks.
  --home defaults to $` + agent.HomeEnv + `, which the launcher gives its client.`

// openEscalation is what the command prints of one open escalation.
type openEscalation struct {
	ID       string `json:"id"`
	Category string `json:"category"`
	Question string `json:"question"`
	Urgency  string `json:"urgency"`
	Task     struct {
		HubID     string `json:"hub_id"`
		ProjectID string `json:"project_id"`
		TaskID    string `json:"task_id"`
	} `json:"task"`
	CreatedAt string `json:"created_at"`
}

// escalations lists the open escalations through the agent home's
// launcher, which holds the session. Its exit codes are step's.
func escalations(ctx context.Context, args []string, stdout, stderr io.Writer, getenv func(string) string) int {
	fs := flag.NewFlagSet("aicrew-agent escalations", flag.ContinueOnError)
	fs.SetOutput(stderr)
	home := fs.String("home", getenv(agent.HomeEnv), "the agent home directory")
	asJSON := fs.Bool("json", false, "print the launcher's answer as JSON")
	if err := fs.Parse(args); err != nil || *home == "" || fs.NArg() != 0 {
		fmt.Fprintln(stderr, escalationsUsage)
		return exitUsage
	}
	ans, err := agent.CallStep(ctx, *home, agent.StepCall{Op: "escalations"})
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
	var page struct {
		Escalations []openEscalation `json:"escalations"`
	}
	if err := json.Unmarshal(ans.Result, &page); err != nil {
		fmt.Fprintln(stderr, "the launcher's answer is not a list of escalations:", err)
		return stepFailed
	}
	if len(page.Escalations) == 0 {
		fmt.Fprintln(stdout, "No escalation is open.")
		return stepExit(ans.Status)
	}
	for _, e := range page.Escalations {
		fmt.Fprintf(stdout, "- Escalation `%s` on task `%s/%s/%s` (%s, %s), raised %s: %s\n", e.ID, e.Task.HubID,
			e.Task.ProjectID, e.Task.TaskID, e.Category, e.Urgency, e.CreatedAt, e.Question)
	}
	return stepExit(ans.Status)
}
