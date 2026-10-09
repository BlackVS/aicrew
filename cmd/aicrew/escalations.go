package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/url"

	"github.com/BlackVS/aicrew/internal/opapi"
)

// aicrew escalations: the coordinator's escalations and their answers
// (docs/DESIGN-CONTROL-PLANE.md, section 7.5), through aicrewd's operator
// API, with the operator credential or an architect credential in the
// token file. A team is named by its ID: resolving a name reads the team
// routes, which an architect credential does not reach.

const escalationsUsage = `usage:
  aicrew escalations list   [--team TEAM] [--open]
  aicrew escalations show   --id ESCALATION
  aicrew escalations answer --id ESCALATION --decision TEXT --rationale TEXT
` + connUsage

// runEscalations is aicrew escalations: 0 on success, 1 on a failure, 2 on
// a usage error.
func runEscalations(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	if len(args) < 1 {
		fmt.Fprint(stderr, escalationsUsage)
		return 2
	}
	verb := args[0]
	fs := flag.NewFlagSet("aicrew escalations "+verb, flag.ContinueOnError)
	fs.SetOutput(stderr)
	cn := addConn(fs)
	team := fs.String("team", "", "the team's ID")
	open := fs.Bool("open", false, "only the escalations not yet answered")
	id := fs.String("id", "", "the escalation's ID")
	decision := fs.String("decision", "", "the decision, as the operator gave it")
	rationale := fs.String("rationale", "", "why, and any constraint the decision adds")
	if err := fs.Parse(args[1:]); err != nil || fs.NArg() != 0 {
		fmt.Fprint(stderr, escalationsUsage)
		return 2
	}
	set := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { set[f.Name] = true })
	usage := func() int { fmt.Fprint(stderr, escalationsUsage); return 2 }
	switch verb {
	case "list":
		if !allowedFlags(set, "team", "open") {
			return usage()
		}
	case "show":
		if !allowedFlags(set, "id") || *id == "" {
			return usage()
		}
	case "answer":
		if !allowedFlags(set, "id", "decision", "rationale") || *id == "" || *decision == "" || *rationale == "" {
			return usage()
		}
	default:
		return usage()
	}
	cl, ok := cn.connect(stderr)
	if !ok {
		return 1
	}
	out := json.NewEncoder(stdout)
	out.SetIndent("", "  ")
	switch verb {
	case "list":
		q := url.Values{}
		if *team != "" {
			q.Set("team", *team)
		}
		if *open {
			q.Set("open", "1")
		}
		var list []opapi.Escalation
		if err := cl.Get(ctx, opapi.EscalationsPath, q, &list); err != nil {
			return failed(stderr, err)
		}
		_ = out.Encode(list)
	case "show":
		var e opapi.Escalation
		if err := cl.Get(ctx, opapi.EscalationPath, url.Values{"id": {*id}}, &e); err != nil {
			return failed(stderr, err)
		}
		_ = out.Encode(e)
	case "answer":
		var e opapi.Escalation
		if err := cl.Post(ctx, opapi.EscalationAnswerPath,
			opapi.AnswerRequest{ID: *id, Decision: *decision, Rationale: *rationale}, &e); err != nil {
			return failed(stderr, err)
		}
		_ = out.Encode(e)
	}
	return 0
}
