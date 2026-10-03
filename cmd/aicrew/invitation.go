package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/url"
	"os"

	"github.com/BlackVS/aicrew/internal/opapi"
	"github.com/BlackVS/aicrew/internal/privatefile"
)

// aicrew invitation: the operator's invitations (docs/ONBOARDING-CONTRACT.md,
// "Invitations"), through aicrewd's operator API.
//
// The invitation code is a bearer capability. aicrewd generates it, answers
// it once and keeps only its digest. This command prints it only to a
// terminal, or writes it to a new private -code-file; it is never taken from
// or put into an argument, and a refused issue never has one.

const invitationUsage = `usage:
  aicrew invitation issue  -team TEAM -role ROLE -hub HUB [-purpose join|link|rebind]
                           [-label LABEL] [-agent AGENT] [-expect-user USER] [-expires DURATION] [-code-file PATH]
  aicrew invitation list   [-team TEAM]
  aicrew invitation revoke -id INVITATION
` + connUsage

// isTerminal reports whether w is an interactive terminal: the only place
// the invitation code is printed. Tests replace it.
var isTerminal = func(w io.Writer) bool {
	f, ok := w.(*os.File)
	if !ok {
		return false
	}
	fi, err := f.Stat()
	return err == nil && fi.Mode()&os.ModeCharDevice != 0
}

// writeCode writes the code to its file; tests replace it to fail.
var writeCode = func(f *os.File, code string) error {
	if _, err := io.WriteString(f, code+"\n"); err != nil {
		return err
	}
	return f.Sync()
}

// invitationView is what issue prints: metadata, never the code.
type invitationView struct {
	opapi.Invitation
	CodeFile string `json:"code_file,omitempty"`
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
	team := fs.String("team", "", "team ID")
	role := fs.String("role", "", "coordinator, worker or independent")
	hub := fs.String("hub", "", "the aimem hub ID the invitation is for")
	purpose := fs.String("purpose", "join", "join, link or rebind")
	label := fs.String("label", "", "label of the agent a join creates")
	agent := fs.String("agent", "", "the agent a link or rebind is for")
	expectUser := fs.String("expect-user", "", "the aimem user ID the proof must name")
	expires := fs.Duration("expires", 0, "lifetime (default 24h, at most 72h)")
	codeFile := fs.String("code-file", "", "new private file that receives the code, instead of the terminal")
	id := fs.String("id", "", "invitation ID")
	if err := fs.Parse(args[1:]); err != nil || fs.NArg() != 0 {
		fmt.Fprint(stderr, invitationUsage)
		return 2
	}
	set := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { set[f.Name] = true })
	usage := func() int { fmt.Fprint(stderr, invitationUsage); return 2 }
	var req opapi.InvitationRequest
	switch verb {
	case "issue":
		if !allowedFlags(set, "team", "role", "hub", "purpose", "label", "agent", "expect-user", "expires", "code-file") ||
			*team == "" || *role == "" || *hub == "" {
			return usage()
		}
		req = opapi.InvitationRequest{Purpose: *purpose, TeamID: *team, Role: *role, HubID: *hub, AgentID: *agent,
			ExpectedUserID: *expectUser, Label: *label}
		if *expires != 0 {
			req.TTL = expires.String()
		}
		switch *purpose {
		case "join":
			if *label == "" || *agent != "" {
				fmt.Fprintln(stderr, "aicrew: a join invitation names -label for the agent it creates, and no -agent")
				return 2
			}
		case "link", "rebind":
			if *agent == "" {
				fmt.Fprintf(stderr, "aicrew: a %s invitation names the -agent it is for\n", *purpose)
				return 2
			}
		default:
			return usage()
		}
		if *purpose == "rebind" && *expectUser == "" {
			fmt.Fprintln(stderr, "aicrew: a rebind invitation requires -expect-user: it moves the agent to that aimem user")
			return 2
		}
		if *codeFile == "" && !isTerminal(stdout) {
			fmt.Fprintln(stderr, "aicrew: the invitation code is shown only on a terminal; use -code-file PATH to write it to a new private file")
			return 2
		}
	case "list":
		if !allowedFlags(set, "team") {
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
	out := json.NewEncoder(stdout)
	out.SetIndent("", "  ")

	switch verb {
	case "list":
		q := url.Values{}
		if *team != "" {
			q.Set("team", *team)
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
		fmt.Fprintln(stderr, "aicrew: warning: no -expect-user; anyone who holds the code and an aimem credential for this hub can redeem it")
	}
	var f *os.File
	if *codeFile != "" {
		var err error
		if f, err = privatefile.Create(*codeFile); err != nil {
			fmt.Fprintln(stderr, "aicrew: create the code file (it must not exist):", err)
			return 1
		}
	}
	var inv opapi.Invitation
	if err := cl.Post(ctx, opapi.InvitationsPath, req, &inv); err != nil {
		if f != nil {
			f.Close()
			os.Remove(*codeFile)
		}
		return failed(stderr, err)
	}
	code := inv.Code
	inv.Code = ""
	v := invitationView{Invitation: inv}
	if f != nil {
		if werr := errors.Join(writeCode(f, code), f.Close()); werr != nil {
			os.Remove(*codeFile)
			// The code is lost: revoke the invitation just issued.
			if rerr := cl.Post(ctx, opapi.InvitationRevokePath, opapi.IDRequest{ID: inv.ID}, nil); rerr != nil {
				fmt.Fprintf(stderr, "aicrew: writing the code file failed (%v), and revoking invitation %s failed too (%v); revoke it before anything else\n", werr, inv.ID, rerr)
				return 1
			}
			fmt.Fprintf(stderr, "aicrew: writing the code file failed (%v); invitation %s was revoked\n", werr, inv.ID)
			return 1
		}
		v.CodeFile = *codeFile
		_ = out.Encode(v)
		return 0
	}
	_ = out.Encode(v)
	fmt.Fprintf(stdout, "\nInvitation code, shown once. Give it privately to the person running the agent:\n\n    %s\n\n", code)
	return 0
}
