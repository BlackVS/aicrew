package main

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/BlackVS/aicrew/internal/agent"
	"github.com/BlackVS/aicrew/internal/service"
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
	svc := checkService(ctx, *home)
	if *asJSON {
		raw, _ := json.MarshalIndent(checkOutput{CheckReport: rep, Service: svc}, "", "  ")
		fmt.Fprintln(stdout, string(raw))
	} else {
		fmt.Fprintf(stdout, "status: %s\n", rep.Status)
		printCheck(stdout, rep)
		printServiceCheck(stdout, svc)
	}
	if rep.Status == agent.JoinBlocked {
		return exitFailed
	}
	return exitOK
}

// serviceCheck is the home's service as check reports it.
type serviceCheck struct {
	State  string          `json:"state"` // not_used, unavailable, or the service's state
	Detail string          `json:"detail,omitempty"`
	Status *service.Status `json:"status,omitempty"`
	ToDo   string          `json:"to_do,omitempty"`
}

// checkOutput is check's JSON report: the check's, and the service's.
type checkOutput struct {
	agent.CheckReport
	Service serviceCheck `json:"service"`
}

// checkService reports the home's service; tests replace it.
var checkService = func(ctx context.Context, home string) serviceCheck {
	abs, err := filepath.Abs(home)
	if err != nil {
		return serviceCheck{State: "unavailable", Detail: err.Error()}
	}
	if agent.ReadWakeSettings(abs).Mode != agent.WakeTurns {
		return serviceCheck{State: "not_used", Detail: "a stop-hook home: the member runs " +
			agent.StartCommand(abs, agent.HomeClients(abs)) + " in a terminal"}
	}
	m, err := newServiceManager(os.Getenv)
	if err != nil {
		return serviceCheck{State: "unavailable", Detail: err.Error(),
			ToDo: "start the member in a terminal with: aicrew-agent serve --home " + abs}
	}
	return serviceCheckOf(m.Status(ctx, abs, agent.HomeLabel(abs)))
}

func serviceCheckOf(st service.Status, err error) serviceCheck {
	if err != nil {
		return serviceCheck{State: "unavailable", Detail: err.Error()}
	}
	c := serviceCheck{State: st.State(), Status: &st}
	switch {
	case st.HandServe:
		c.ToDo = "stop the serve started by hand, then: aicrew-agent service start --home " + st.Home
	case !st.Installed:
		c.ToDo = agent.ServiceCommand(st.Home)
	case !st.Active:
		c.ToDo = "aicrew-agent service start --home " + st.Home
	}
	return c
}

func printServiceCheck(w io.Writer, c serviceCheck) {
	switch {
	case c.Status != nil:
		fmt.Fprintf(w, "service: %s: %s (%s)\n", c.Status.Name, c.State, c.Status.Kind)
	case c.Detail != "":
		fmt.Fprintf(w, "service: %s (%s)\n", strings.ReplaceAll(c.State, "_", " "), c.Detail)
	default:
		fmt.Fprintf(w, "service: %s\n", c.State)
	}
	if c.ToDo != "" {
		fmt.Fprintf(w, "to do: %s\n", c.ToDo)
	}
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
	if m := rep.TurnModel; m != nil {
		line := "turn model: " + cmp.Or(m.Configured, "(the client's default)")
		if m.Resolved != "" {
			line += " -> " + m.Resolved
		}
		fmt.Fprintf(w, "%s (%s)\n", line, m.State)
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
	if a := rep.Automations; a != nil {
		printAutomations(w, a)
	}
	for _, n := range rep.Notices {
		fmt.Fprintf(w, "notice: %s\n", n)
	}
	for _, i := range rep.Instructions {
		fmt.Fprintf(w, "to do: %s\n", i)
	}
}

// printAutomations prints the effective automations: each entry's name,
// when it runs, its source and the commit its script was read at.
func printAutomations(w io.Writer, a *agent.AutomationsReport) {
	if a.Attempt != "" {
		fmt.Fprintf(w, "automations of attempt %s:\n", a.Attempt)
	} else {
		fmt.Fprintln(w, "automations (the release's defaults; no open attempt names a process pin):")
	}
	if len(a.Entries) == 0 {
		fmt.Fprintln(w, "  none")
		return
	}
	fmt.Fprintf(w, "  %-24s %-28s %-10s %s\n", "name", "runs at", "source", "commit")
	for _, e := range a.Entries {
		at := strings.Join(append([]string{e.Point}, e.Events...), ", ")
		at = strings.TrimPrefix(at, ", ")
		if at == "" {
			at = "-"
		}
		commit := e.Commit
		if commit == "" {
			commit = "-"
		}
		fmt.Fprintf(w, "  %-24s %-28s %-10s %s", e.Name, at, e.Source, commit)
		if len(e.Command) != 0 {
			fmt.Fprintf(w, "  %s", strings.Join(e.Command, " "))
		}
		fmt.Fprintln(w)
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
