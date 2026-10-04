package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/BlackVS/aicrew/internal/agent"
)

const joinUsage = `usage: aicrew-agent join --label LABEL [--home DIR] --url https://HOST[:PORT]
         --tls-trust-mode ca_dns|spki_sha256 --tls-trust-value VALUE --aimem-hub NAME
         --client claude|opencode[,…] [--aimem-command PATH] [--json]
         [--aimem-url https://HUB --aimem-token-file PATH|- [--aimem-ca-file PATH]]
       The invitation code is read at a hidden prompt. The -aimem flags
       provision the home's aimem installation on the first run, the token
       read from its owner-only file or, with -, at a hidden prompt. On a linked home, only
       --home (or --label) is needed: the run refreshes the home's files and
       checks its dependencies and clients.`

// joinDeps builds the bootstrap's collaborators on the real terminal,
// aicrewd and aimem.
func joinDeps(stderr io.Writer) agent.JoinDeps {
	return agent.JoinDeps{
		Crew: func(cfg agent.Config) (agent.InvitationAPI, error) { return agent.NewCrew(cfg, nil) },
		Aimem: func(command, hub, home string) agent.JoinAimem {
			return agent.ExecAimem{Command: command, Hub: hub, Home: home}.JoinAimem()
		},
		ReadCode:  func() (string, error) { return agent.ReadHidden(os.Stdin, stderr, "Invitation code: ") },
		ReadToken: func() (string, error) { return agent.ReadHidden(os.Stdin, stderr, "The member's aimem token: ") },
		Out:       stderr,
	}
}

// join is `aicrew-agent join`. It exits 0 when the home is ready (or ready
// after a restart of an open client), 1 when blocked or failed, 2 on usage.
func join(ctx context.Context, args []string, terminal bool, stdout, stderr io.Writer, deps agent.JoinDeps) int {
	fs := flag.NewFlagSet("aicrew-agent join", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	var o agent.JoinOptions
	fs.StringVar(&o.Home, "home", "", "")
	fs.StringVar(&o.Label, "label", "", "")
	fs.StringVar(&o.URL, "url", "", "")
	fs.StringVar(&o.Trust.Mode, "tls-trust-mode", "", "")
	fs.StringVar(&o.Trust.Value, "tls-trust-value", "", "")
	fs.StringVar(&o.AimemHub, "aimem-hub", "", "")
	fs.StringVar(&o.AimemCommand, "aimem-command", "", "")
	fs.StringVar(&o.AimemURL, "aimem-url", "", "")
	fs.StringVar(&o.AimemTokenFile, "aimem-token-file", "", "")
	fs.StringVar(&o.AimemCAFile, "aimem-ca-file", "", "")
	clients := fs.String("client", "", "")
	asJSON := fs.Bool("json", false, "")
	if err := fs.Parse(args); err != nil || fs.NArg() != 0 || (o.Home == "" && o.Label == "") {
		fmt.Fprintln(stderr, joinUsage)
		return exitUsage
	}
	if o.Home == "" {
		home, err := agent.DefaultHome(o.Label)
		if err != nil {
			fmt.Fprintln(stderr, "no default home:", err)
			return exitFailed
		}
		o.Home = home
	}
	o.Terminal = terminal
	o.Clients = splitList(*clients)
	rep, err := agent.Join(ctx, o, deps)
	switch {
	case errors.Is(err, agent.ErrJoinUsage):
		fmt.Fprintln(stderr, rep.Instruction)
		fmt.Fprintln(stderr, joinUsage)
		return exitUsage
	case err != nil:
		fmt.Fprintln(stderr, "join failed:", err)
		return exitFailed
	}
	if *asJSON {
		out, _ := json.MarshalIndent(rep, "", "  ")
		fmt.Fprintln(stdout, string(out))
	} else {
		printJoinReport(stdout, rep)
	}
	if rep.Status == agent.JoinBlocked {
		return exitFailed
	}
	return exitOK
}

func printJoinReport(w io.Writer, rep agent.JoinReport) {
	fmt.Fprintf(w, "status: %s\n", rep.Status)
	if rep.Reason != "" {
		fmt.Fprintf(w, "reason: %s\n", rep.Reason)
	}
	if rep.AgentID != "" {
		fmt.Fprintf(w, "agent: %s  team: %s\n", rep.AgentID, rep.TeamID)
	}
	if rep.Instruction != "" {
		fmt.Fprintf(w, "next: %s\n", rep.Instruction)
	}
	if rep.Check != nil {
		printCheck(w, *rep.Check)
	}
	if rep.Next != "" && rep.Status != agent.JoinBlocked {
		fmt.Fprintf(w, "start the session with: %s\n", rep.Next)
	}
}

// splitList reads a comma-separated flag value.
func splitList(s string) []string {
	var out []string
	for _, f := range strings.Split(s, ",") {
		if f = strings.TrimSpace(f); f != "" {
			out = append(out, f)
		}
	}
	return out
}
