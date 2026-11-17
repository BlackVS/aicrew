package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"

	"github.com/BlackVS/aicrew/internal/agent"
)

const preflightUsage = `usage: aicrew-agent preflight --url URL --tls-trust-mode ca_dns|spki_sha256 --tls-trust-value VALUE [--client claude|opencode] [--json]
  Before join, with join's own values: whether aicrewd answers at URL with
  a certificate this trust accepts, and whether the client has a login
  stored. It sends no credential and spends nothing: no invitation, no
  session, no model call. A stored login is not a valid one: only check's
  turn-model line, after join, proves that (one minimal model call).
  Exits 0 when both are fine, 1 when either is not.`

// preflightDeps are the real outside world; tests replace them.
var preflightDeps = agent.PreflightDeps{}

func preflight(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("aicrew-agent preflight", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	var o agent.PreflightOptions
	fs.StringVar(&o.URL, "url", "", "")
	fs.StringVar(&o.Trust.Mode, "tls-trust-mode", "", "")
	fs.StringVar(&o.Trust.Value, "tls-trust-value", "", "")
	fs.StringVar(&o.Client, "client", "", "")
	asJSON := fs.Bool("json", false, "")
	if err := fs.Parse(args); err != nil || fs.NArg() != 0 || o.URL == "" || o.Trust.Mode == "" || o.Trust.Value == "" {
		fmt.Fprintln(stderr, preflightUsage)
		return exitUsage
	}
	rep, err := agent.Preflight(ctx, o, preflightDeps)
	if err != nil {
		fmt.Fprintln(stderr, "aicrew-agent preflight:", err)
		return exitUsage
	}
	if *asJSON {
		b, _ := json.MarshalIndent(rep, "", "  ")
		fmt.Fprintln(stdout, string(b))
	} else {
		fmt.Fprintf(stdout, "aicrewd: %s: %s\n", rep.Aicrewd.State, rep.Aicrewd.Detail)
		if rep.Login != nil {
			fmt.Fprintf(stdout, "login (%s): %s: %s\n", rep.Login.Client, rep.Login.State, rep.Login.Detail)
		}
	}
	if !rep.OK {
		return exitFailed
	}
	return exitOK
}
