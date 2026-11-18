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
  aicrew escalations wait   [--team TEAM] [--interval SECONDS] [--seen FILE]
ESCALATION is the ID that list prints, or a prefix of it that starts no
other escalation's ID, of at least 8 characters.
wait reads the open escalations every --interval seconds (60 by default, at
least 15) and returns with those not in the seen set, one line each, oldest
first. The seen set is --seen, else $AICREW_ESCALATIONS_SEEN (the architect
directory's settings name it), else state/escalations-seen.json under the
current directory. Five failed reads in a row end it with exit 1. The list
holds at most 200 open escalations, newest first: with more open, the oldest
are told only as newer ones are answered, and a full list never drops an
escalation from the seen set.
` + connUsage

// runEscalations is aicrew escalations: 0 on success, 1 on a failure, 2 on
// a usage error.
func runEscalations(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	if len(args) < 1 {
		fmt.Fprint(stderr, escalationsUsage)
		return 2
	}
	verb := args[0]
	if verb == "wait" {
		return runEscalationsWait(ctx, args[1:], stdout, stderr)
	}
	fs := flag.NewFlagSet("aicrew escalations "+verb, flag.ContinueOnError)
	fs.SetOutput(stderr)
	cn := addConn(fs)
	team := fs.String("team", "", "the team's ID")
	open := fs.Bool("open", false, "only the escalations not yet answered")
	id := fs.String("id", "", "the escalation's ID, or a unique prefix of it")
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
