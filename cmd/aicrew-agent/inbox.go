package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"strings"

	"github.com/BlackVS/aicrew/internal/agent"
)

const inboxUsage = `usage: aicrew-agent inbox [--home DIR] [--limit N] [--ack ID,ID...] [--json]
  Reads the member's oldest unacknowledged messages through the launcher, or
  with --ack acknowledges the messages it delivered. A message stays in every
  later read until it is acknowledged.
  --home defaults to $` + agent.HomeEnv + `, which the launcher gives its client.`

// inboxMessage is what the command prints of one delivered message.
type inboxMessage struct {
	ID            string `json:"id"`
	Seq           int64  `json:"seq"`
	Kind          string `json:"kind"`
	SenderAgentID string `json:"sender_agent_id"`
	AttemptID     string `json:"attempt_id"`
	Task          *struct {
		HubID     string `json:"hub_id"`
		ProjectID string `json:"project_id"`
		TaskID    string `json:"task_id"`
	} `json:"task"`
	Text       string `json:"text"`
	Deliveries int64  `json:"deliveries"`
}

// inbox reads or acknowledges the member's inbox through the agent home's
// launcher, which holds the session. Its exit codes are step's.
func inbox(ctx context.Context, args []string, stdout, stderr io.Writer, getenv func(string) string) int {
	fs := flag.NewFlagSet("aicrew-agent inbox", flag.ContinueOnError)
	fs.SetOutput(stderr)
	home := fs.String("home", getenv(agent.HomeEnv), "the agent home directory")
	limit := fs.Int("limit", 20, "how many messages to read, 1 to 100")
	ack := fs.String("ack", "", "comma-separated IDs of delivered messages to acknowledge")
	asJSON := fs.Bool("json", false, "print the launcher's answer as JSON")
	if err := fs.Parse(args); err != nil || *home == "" || fs.NArg() != 0 || *limit < 1 || *limit > 100 {
		fmt.Fprintln(stderr, inboxUsage)
		return exitUsage
	}
	call := agent.StepCall{Op: "inbox"}
	if *ack != "" {
		var ids []string
		for _, id := range strings.Split(*ack, ",") {
			if id = strings.TrimSpace(id); id == "" {
				fmt.Fprintln(stderr, inboxUsage)
				return exitUsage
			}
			ids = append(ids, id)
		}
		call = agent.StepCall{Op: "ack"}
		call.Body, _ = json.Marshal(map[string][]string{"ids": ids})
	} else {
		call.Body, _ = json.Marshal(map[string]int{"limit": *limit})
	}
	ans, err := agent.CallStep(ctx, *home, call)
	if err != nil {
		fmt.Fprintln(stderr, err)
		if errors.Is(err, agent.ErrNoLauncher) {
			fmt.Fprintln(stderr, "no launcher serves", *home)
		}
		return stepFailed
	}
	if *asJSON || !ans.OK || call.Op == "ack" {
		out, _ := json.MarshalIndent(ans, "", "  ")
		fmt.Fprintln(stdout, string(out))
		return stepExit(ans.Status)
	}
	var page struct {
		Messages []inboxMessage `json:"messages"`
	}
	if json.Unmarshal(ans.Result, &page) != nil {
		fmt.Fprintln(stderr, "the launcher's answer is not an inbox page")
		return stepFailed
	}
	if len(page.Messages) == 0 {
		fmt.Fprintln(stdout, "No unacknowledged messages.")
		return stepDone
	}
	for _, m := range page.Messages {
		fmt.Fprintf(stdout, "#%d %s %s from %s", m.Seq, m.ID, m.Kind, m.SenderAgentID)
		if m.Task != nil {
			fmt.Fprintf(stdout, ", task %s/%s/%s", m.Task.HubID, m.Task.ProjectID, m.Task.TaskID)
		}
		if m.AttemptID != "" {
			fmt.Fprintf(stdout, ", attempt %s", m.AttemptID)
		}
		fmt.Fprintf(stdout, " (delivered %d times)\n  %s\n", m.Deliveries, m.Text)
	}
	fmt.Fprintln(stdout, "Acknowledge what you handled: aicrew-agent inbox --ack ID,ID")
	return stepDone
}
