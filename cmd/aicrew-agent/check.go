package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"

	"github.com/BlackVS/aicrew/internal/agent"
	"github.com/BlackVS/aicrew/internal/version"
)

const checkUsage = `usage: aicrew-agent check (--home DIR | --label LABEL) [--client claude|opencode[,…]] [--json]
       Checks aimem, ai-skills and the clients against the supported set, keeps the
       home's client wiring, and asks each selected client what it sees (no model call).`

// check is `aicrew-agent check`. It exits 0 when the home is ready (or ready
// after a restart of an open client), 1 when blocked or failed, 2 on usage.
func check(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("aicrew-agent check", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	home := fs.String("home", "", "")
	label := fs.String("label", "", "")
	clients := fs.String("client", "", "")
	asJSON := fs.Bool("json", false, "")
	if err := fs.Parse(args); err != nil || fs.NArg() != 0 || (*home == "" && *label == "") {
		fmt.Fprintln(stderr, checkUsage)
		return exitUsage
	}
	if *home == "" {
		h, err := agent.DefaultHome(*label)
		if err != nil {
			fmt.Fprintln(stderr, "no default home:", err)
			return exitFailed
		}
		*home = h
	}
	out := stderr
	if *asJSON {
		out = io.Discard
	}
	fmt.Fprintln(out, "Client wiring:")
	rep, err := agent.Check(ctx, agent.CheckOptions{Home: *home, Clients: splitList(*clients), Out: out})
	switch {
	case errors.Is(err, agent.ErrUnknownClient):
		fmt.Fprintln(stderr, err)
		fmt.Fprintln(stderr, checkUsage)
		return exitUsage
	case errors.Is(err, agent.ErrNotHome):
		fmt.Fprintln(stderr, err)
		return exitFailed
	case err != nil:
		fmt.Fprintln(stderr, "check failed:", err)
		return exitFailed
	}
	if *asJSON {
		raw, _ := json.MarshalIndent(rep, "", "  ")
		fmt.Fprintln(stdout, string(raw))
	} else {
		fmt.Fprintf(stdout, "status: %s\n", rep.Status)
		printCheck(stdout, rep)
	}
	if rep.Status == agent.JoinBlocked {
		return exitFailed
	}
	return exitOK
}

func printCheck(w io.Writer, rep agent.CheckReport) {
	fmt.Fprintf(w, "aicrew-agent: %s\n", rep.Agent.Version)
	if o := rep.OperatorCLI; o != nil {
		fmt.Fprintf(w, "aicrew (operator CLI): %s (%s)\n", orNone(o.Version), o.State)
	}
	for _, c := range rep.Components {
		found := c.Found
		if found == "" && c.State != agent.StateMissing {
			found = c.Detail
		}
		fmt.Fprintf(w, "%s: %s (%s)\n", c.Name, orNone(found), c.State)
	}
	for _, c := range rep.Clients {
		line := fmt.Sprintf("%s: %s (%s)", c.Name, orNone(c.Found), c.State)
		if c.MCP != "" {
			line += ", aimem MCP " + c.MCP
		}
		fmt.Fprintln(w, line)
	}
	if len(rep.Forge) > 0 {
		fmt.Fprintf(w, "%-28s %-24s %-11s %s\n", "forge host", "account", "purpose", "state")
		for _, f := range rep.Forge {
			fmt.Fprintf(w, "%-28s %-24s %-11s %s\n", f.Host, f.Account, f.Purpose, f.State)
		}
	}
	if len(rep.Projects) > 0 {
		fmt.Fprintf(w, "%-20s %-44s %-24s %-20s %-8s %s\n", "project", "repository", "host", "account", "required", "state")
		for _, p := range rep.Projects {
			fmt.Fprintf(w, "%-20s %-44s %-24s %-20s %-8s %s\n", p.Project, p.Repository, p.Host, orNone(p.Account), p.Required, p.State)
		}
	}
	for _, n := range rep.Notices {
		fmt.Fprintf(w, "notice: %s\n", n)
	}
	for _, i := range rep.Instructions {
		fmt.Fprintf(w, "to do: %s\n", i)
	}
}

func orNone(s string) string {
	if s == "" {
		return "not found"
	}
	return s
}

// versionCmd is `aicrew-agent version [--json]`.
func versionCmd(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("aicrew-agent version", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	asJSON := fs.Bool("json", false, "")
	if err := fs.Parse(args); err != nil || fs.NArg() != 0 {
		fmt.Fprintln(stderr, "usage: aicrew-agent version [--json]")
		return exitUsage
	}
	version.Print(stdout, "aicrew-agent", *asJSON)
	return exitOK
}
