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

// maxEscalationPages bounds how many pages one listing follows.
const maxEscalationPages = 1000

// escalations lists every open escalation through the agent home's
// launcher, which holds the session, following aicrewd's pages. Its exit
// codes are step's.
func escalations(ctx context.Context, args []string, stdout, stderr io.Writer, getenv func(string) string) int {
	fs := flag.NewFlagSet("aicrew-agent escalations", flag.ContinueOnError)
	fs.SetOutput(stderr)
	home := fs.String("home", getenv(agent.HomeEnv), "the agent home directory")
	asJSON := fs.Bool("json", false, "print the open escalations as JSON")
	if err := fs.Parse(args); err != nil || *home == "" || fs.NArg() != 0 {
		fmt.Fprintln(stderr, escalationsUsage)
		return exitUsage
	}
	all := []json.RawMessage{}
	var shown []openEscalation
	after := ""
	for range maxEscalationPages {
		call := agent.StepCall{Op: "escalations"}
		if after != "" {
			call.Body, _ = json.Marshal(map[string]string{"after": after})
		}
		ans, err := agent.CallStep(ctx, *home, call)
		if err != nil {
			fmt.Fprintln(stderr, err)
			if errors.Is(err, agent.ErrNoLauncher) {
				fmt.Fprintln(stderr, "no launcher serves", *home)
			}
			return stepFailed
		}
		if !ans.OK {
			out, _ := json.MarshalIndent(ans, "", "  ")
			fmt.Fprintln(stdout, string(out))
			return stepExit(ans.Status)
		}
		var page struct {
			Escalations []json.RawMessage `json:"escalations"`
			Next        string            `json:"next"`
		}
		if err := json.Unmarshal(ans.Result, &page); err != nil {
			fmt.Fprintln(stderr, "the launcher's answer is not a page of escalations:", err)
			return stepFailed
		}
		for _, raw := range page.Escalations {
			var e openEscalation
			if err := json.Unmarshal(raw, &e); err != nil {
				fmt.Fprintln(stderr, "the launcher's answer is not a page of escalations:", err)
				return stepFailed
			}
			all, shown = append(all, raw), append(shown, e)
		}
		if page.Next == "" {
			return printEscalations(stdout, all, shown, *asJSON)
		}
		if page.Next == after {
			fmt.Fprintln(stderr, "aicrewd's pages do not advance past", after)
			return stepFailed
		}
		after = page.Next
	}
	fmt.Fprintf(stderr, "more than %d pages of open escalations\n", maxEscalationPages)
	return stepFailed
}

func printEscalations(stdout io.Writer, all []json.RawMessage, shown []openEscalation, asJSON bool) int {
	if asJSON {
		out, _ := json.MarshalIndent(map[string]any{"escalations": all}, "", "  ")
		fmt.Fprintln(stdout, string(out))
		return stepDone
	}
	if len(shown) == 0 {
		fmt.Fprintln(stdout, "No escalation is open.")
		return stepDone
	}
	for _, e := range shown {
		fmt.Fprintf(stdout, "- Escalation `%s` on task `%s/%s/%s` (%s, %s), raised %s: %s\n", e.ID, e.Task.HubID,
			e.Task.ProjectID, e.Task.TaskID, e.Category, e.Urgency, e.CreatedAt, e.Question)
	}
	return stepDone
}
