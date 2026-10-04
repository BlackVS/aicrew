package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/url"

	"github.com/BlackVS/aicrew/internal/opapi"
)

// aicrew invitation: the operator's invitations (docs/ONBOARDING-CONTRACT.md,
// "Invitations"), through aicrewd's operator API.
//
// The invitation code is a bearer capability. aicrewd generates it, answers
// it once and keeps only its digest. This command writes it only where
// --output names (secretout.go), never to a terminal; it is never taken from
// or put into an argument, and a refused issue never has one.

const invitationUsage = `usage:
  aicrew invitation issue  (--team TEAM | --team-name NAME) --role ROLE --hub HUB --output PATH|-
                           [--purpose join|link|rebind] [--label LABEL] [--agent AGENT]
                           [--expect-user USER] [--expires DURATION]
  aicrew invitation list   [--team TEAM | --team-name NAME]
  aicrew invitation revoke --id INVITATION
` + connUsage

// invitationView is what issue prints: metadata, never the code.
type invitationView struct {
	opapi.Invitation
	Output string `json:"output,omitempty"`
}

// runInvitation is aicrew invitation: 0 on success, 1 on a failure, 2 on a
// usage error.
func runInvitation(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	if len(args) < 1 {
		fmt.Fprint(stderr, invitationUsage)
		return 2
	}
	verb := args[0]
	fs := flag.NewFlagSet("aicrew invitation "+verb, flag.ContinueOnError)
	fs.SetOutput(stderr)
	cn := addConn(fs)
	tf := addTeamFlags(fs)
	role := fs.String("role", "", "coordinator, worker or independent")
	hub := fs.String("hub", "", "the aimem hub ID the invitation is for")
	purpose := fs.String("purpose", "join", "join, link or rebind")
	label := fs.String("label", "", "label of the agent a join creates")
	agent := fs.String("agent", "", "the agent a link or rebind is for")
	expectUser := fs.String("expect-user", "", "the aimem user ID the proof must name")
	expires := fs.Duration("expires", 0, "lifetime (default 24h, at most 72h)")
	outFlags := addOutput(fs, "code-file", "code")
	id := fs.String("id", "", "invitation ID")
	if err := fs.Parse(args[1:]); err != nil || fs.NArg() != 0 {
		fmt.Fprint(stderr, invitationUsage)
		return 2
	}
	set := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { set[f.Name] = true })
	usage := func() int { fmt.Fprint(stderr, invitationUsage); return 2 }
	output, outOK := outFlags.value(set, stderr)
	if !outOK {
		return 2
	}
	teamGiven, teamOK := tf.given(stderr)
	if !teamOK {
		return 2
	}
	var req opapi.InvitationRequest
	switch verb {
	case "issue":
		if !allowedFlags(set, "team", "team-name", "role", "hub", "purpose", "label", "agent", "expect-user", "expires", "output",
			"code-file") || !teamGiven || *role == "" || *hub == "" {
			return usage()
		}
		req = opapi.InvitationRequest{Purpose: *purpose, Role: *role, HubID: *hub, AgentID: *agent,
			ExpectedUserID: *expectUser, Label: *label}
		if *expires != 0 {
			req.TTL = expires.String()
		}
		switch *purpose {
		case "join":
			if *label == "" || *agent != "" {
				fmt.Fprintln(stderr, "aicrew: a join invitation names --label for the agent it creates, and no --agent")
				return 2
			}
		case "link", "rebind":
			if *agent == "" {
				fmt.Fprintf(stderr, "aicrew: a %s invitation names the --agent it is for\n", *purpose)
				return 2
			}
		default:
			return usage()
		}
		if *purpose == "rebind" && *expectUser == "" {
			fmt.Fprintln(stderr, "aicrew: a rebind invitation requires --expect-user: it moves the agent to that aimem user")
			return 2
		}
		if output == "" {
			fmt.Fprintln(stderr, "aicrew: the invitation code is written only where --output names: a new file, or - for a pipe; it never reaches the terminal")
			return 2
		}
	case "list":
		if !allowedFlags(set, "team", "team-name") {
			return usage()
		}
	case "revoke":
		if !allowedFlags(set, "id") || *id == "" {
			return usage()
		}
	default:
		return usage()
	}
	cl, ok := cn.connect(stderr)
	if !ok {
		return 1
	}
	team, err := tf.resolve(ctx, cl)
	if err != nil {
		return failed(stderr, err)
	}
	req.TeamID = team
	out := json.NewEncoder(stdout)
	out.SetIndent("", "  ")

	switch verb {
	case "list":
		q := url.Values{}
		if team != "" {
			q.Set("team", team)
		}
		var invs []opapi.Invitation
		if err := cl.Get(ctx, opapi.InvitationsPath, q, &invs); err != nil {
			return failed(stderr, err)
		}
		_ = out.Encode(invs)
		return 0
	case "revoke":
		var inv opapi.Invitation
		if err := cl.Post(ctx, opapi.InvitationRevokePath, opapi.IDRequest{ID: *id}, &inv); err != nil {
			return failed(stderr, err)
		}
		_ = out.Encode(inv)
		return 0
	}

	// issue.
	if (req.Purpose == "join" || req.Purpose == "link") && *expectUser == "" {
		fmt.Fprintln(stderr, "aicrew: warning: no --expect-user; anyone who holds the code and an aimem credential for this hub can redeem it")
	}
	so, err := openSecretOutput(output, stdout)
	if err != nil {
		fmt.Fprintln(stderr, "aicrew:", err)
		return 1
	}
	var inv opapi.Invitation
	err = cl.Post(ctx, opapi.InvitationsPath, req, &inv)
	if err == nil && (inv.ID == "" || inv.Code == "") {
		err = errors.New("aicrewd's answer lacks the invitation's ID or code")
	}
	if err != nil {
		so.discard()
		return issueFailed(ctx, stderr, cl, err, "invitation", inv.ID, opapi.InvitationRevokePath,
			"aicrew invitation list --team "+team)
	}
	code := inv.Code
	inv.Code = ""
	if werr := so.write(code); werr != nil {
		// The code is lost: revoke the invitation just issued.
		if rerr := cl.Post(ctx, opapi.InvitationRevokePath, opapi.IDRequest{ID: inv.ID}, nil); rerr != nil {
			fmt.Fprintf(stderr, "aicrew: writing the code failed (%v), and revoking invitation %s failed too (%v); revoke it before anything else\n", werr, inv.ID, rerr)
			return 1
		}
		fmt.Fprintf(stderr, "aicrew: writing the code failed (%v); invitation %s was revoked\n", werr, inv.ID)
		return 1
	}
	info := json.NewEncoder(so.info(stderr))
	info.SetIndent("", "  ")
	_ = info.Encode(invitationView{Invitation: inv, Output: output})
	return 0
}
