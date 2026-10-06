package main

import (
	"context"
	"encoding/json"
	"flag"
	"io"
	"time"

	"github.com/BlackVS/aicrew/internal/agent"
)

// waitInbox is `aicrew-agent wait-inbox --home DIR`, the Claude Code Stop
// hook the home's managed settings install (task 01a0d6d7-1aed). It reads
// the hook's input on standard input, asks the home's launcher to wait on
// the member's inbox, and prints the decision that keeps the member going
// with what arrived, or nothing when the session may stop. It always exits
// 0: a hook that fails must never wedge the client.
func waitInbox(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer, getenv func(string) string) int {
	fs := flag.NewFlagSet("aicrew-agent wait-inbox", flag.ContinueOnError)
	fs.SetOutput(stderr)
	home := fs.String("home", getenv(agent.HomeEnv), "the agent home directory")
	if err := fs.Parse(args); err != nil || *home == "" {
		return 0
	}
	// The hook's input names the client's session, which scopes the
	// keep-alive's idle bound; the rest is not needed.
	var in struct {
		SessionID string `json:"session_id"`
	}
	raw, _ := io.ReadAll(io.LimitReader(stdin, 1<<20))
	_ = json.Unmarshal(raw, &in)
	if d := agent.StopHook(ctx, *home, in.SessionID, time.Now); d != nil {
		b, _ := json.Marshal(d)
		stdout.Write(b)
	}
	return 0
}
