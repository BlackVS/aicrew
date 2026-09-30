package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/BlackVS/aicrew/internal/privatefile"
	"github.com/BlackVS/aicrew/internal/store"
)

// aicrew invitation: the operator's invitations (docs/ONBOARDING-CONTRACT.md,
// "Invitations"). Like introspection-credential it opens the store file
// directly, so aicrewd is stopped while it runs.
//
// The invitation code is a bearer capability. It is generated here, shown
// once and never kept: only its digest is stored. It is printed only to a
// terminal, or written to a new private -code-file; it is never taken from or
// put into an argument, and a refused issue never shows it.

const invitationUsage = `usage:
  aicrew invitation issue  -store PATH -team TEAM -role ROLE -hub HUB [-purpose join|link|rebind]
                           [-label LABEL] [-agent AGENT] [-expect-user USER] [-expires DURATION] [-code-file PATH]
  aicrew invitation list   -store PATH [-team TEAM]
  aicrew invitation revoke -store PATH -id INVITATION
`

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

// invitationView is what the command prints: metadata, never the code or its
// digest.
type invitationView struct {
	ID             string    `json:"id"`
	Purpose        string    `json:"purpose"`
	TeamID         string    `json:"team_id"`
	Role           string    `json:"role"`
	HubID          string    `json:"hub_id"`
	AgentID        string    `json:"agent_id,omitempty"`
	ExpectedUserID string    `json:"expected_user_id,omitempty"`
	Label          string    `json:"label,omitempty"`
	IssuedBy       string    `json:"issued_by"`
	State          string    `json:"state"`
	Expired        bool      `json:"expired"`
	Attempts       int       `json:"attempts"`
	Revision       int64     `json:"revision"`
	ExpiresAt      time.Time `json:"expires_at"`
	CreatedAt      time.Time `json:"created_at"`
	CodeFile       string    `json:"code_file,omitempty"`
}

func invitationOf(inv store.Invitation, now time.Time) invitationView {
	open := inv.State == store.InvitationIssued || inv.State == store.InvitationLocked
	return invitationView{ID: inv.ID, Purpose: string(inv.Purpose), TeamID: inv.TeamID, Role: string(inv.Role),
		HubID: inv.HubID, AgentID: inv.AgentID, ExpectedUserID: inv.ExpectedUserID, Label: inv.Label,
		IssuedBy: inv.IssuedBy, State: string(inv.State), Expired: open && !now.Before(inv.ExpiresAt),
		Attempts: inv.Attempts, Revision: inv.Revision, ExpiresAt: inv.ExpiresAt, CreatedAt: inv.CreatedAt}
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
	storePath := fs.String("store", "", "path to the aicrew store")
	team := fs.String("team", "", "team ID")
	role := fs.String("role", "", "coordinator, worker or independent")
	hub := fs.String("hub", "", "the aimem hub ID the invitation is for")
	purpose := fs.String("purpose", string(store.PurposeJoin), "join, link or rebind")
	label := fs.String("label", "", "label of the agent a join creates")
	agent := fs.String("agent", "", "the agent a link or rebind is for")
	expectUser := fs.String("expect-user", "", "the aimem user ID the proof must name")
	expires := fs.Duration("expires", 0, "lifetime (default 24h, at most 72h)")
	codeFile := fs.String("code-file", "", "new private file that receives the code, instead of the terminal")
	id := fs.String("id", "", "invitation ID")
	if err := fs.Parse(args[1:]); err != nil || fs.NArg() != 0 || *storePath == "" {
		fmt.Fprint(stderr, invitationUsage)
		return 2
	}
	set := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { set[f.Name] = true })
	only := func(allowed ...string) bool {
		ok := map[string]bool{"store": true}
		for _, a := range allowed {
			ok[a] = true
		}
		for name := range set {
			if !ok[name] {
				return false
			}
		}
		return true
	}
	usage := func() int { fmt.Fprint(stderr, invitationUsage); return 2 }
	var req store.InvitationRequest
	switch verb {
	case "issue":
		if !only("team", "role", "hub", "purpose", "label", "agent", "expect-user", "expires", "code-file") ||
			*team == "" || *role == "" || *hub == "" {
			return usage()
		}
		req = store.InvitationRequest{Purpose: store.InvitationPurpose(*purpose), TeamID: *team, Role: store.Role(*role),
			HubID: *hub, AgentID: *agent, ExpectedUserID: *expectUser, Label: *label, TTL: *expires}
		switch req.Purpose {
		case store.PurposeJoin:
			if *label == "" || *agent != "" {
				fmt.Fprintln(stderr, "aicrew: a join invitation names -label for the agent it creates, and no -agent")
				return 2
			}
		case store.PurposeLink, store.PurposeRebind:
			if *agent == "" {
				fmt.Fprintf(stderr, "aicrew: a %s invitation names the -agent it is for\n", req.Purpose)
				return 2
			}
		default:
			return usage()
		}
		if req.Purpose == store.PurposeRebind && *expectUser == "" {
			fmt.Fprintln(stderr, "aicrew: a rebind invitation requires -expect-user: it moves the agent to that aimem user")
			return 2
		}
		if *codeFile == "" && !isTerminal(stdout) {
			fmt.Fprintln(stderr, "aicrew: the invitation code is shown only on a terminal; use -code-file PATH to write it to a new private file")
			return 2
		}
	case "list":
		if !only("team") {
			return usage()
		}
	case "revoke":
		if !only("id") || *id == "" {
			return usage()
		}
	default:
		return usage()
	}

	st, err := store.Open(ctx, *storePath)
	if err != nil {
		if errors.Is(err, store.ErrStoreInUse) {
			fmt.Fprintln(stderr, "aicrew: the store is in use; stop aicrewd, then run this again")
		} else {
			fmt.Fprintln(stderr, "aicrew: open the store:", err)
		}
		return 1
	}
	defer st.Close()
	op, err := store.OperatorCaller("aicrew-cli")
	if err != nil {
		fmt.Fprintln(stderr, "aicrew:", err)
		return 1
	}
	out := json.NewEncoder(stdout)
	out.SetIndent("", "  ")
	now := time.Now().UTC()

	switch verb {
	case "list":
		invs, err := st.ListInvitations(ctx, op)
		if err != nil {
			fmt.Fprintln(stderr, "aicrew:", err)
			return 1
		}
		shown := []invitationView{}
		for _, inv := range invs {
			if *team == "" || inv.TeamID == *team {
				shown = append(shown, invitationOf(inv, now))
			}
		}
		_ = out.Encode(shown)
		return 0
	case "revoke":
		inv, err := st.GetInvitation(ctx, *id)
		if err == nil {
			inv, err = st.RevokeInvitation(ctx, op, fmt.Sprintf("revoke-%s-%d", inv.ID, inv.Revision), inv.ID, inv.Revision)
		}
		if err != nil {
			fmt.Fprintln(stderr, "aicrew:", err)
			return 1
		}
		_ = out.Encode(invitationOf(inv, now))
		return 0
	}

	// issue.
	if (req.Purpose == store.PurposeJoin || req.Purpose == store.PurposeLink) && *expectUser == "" {
		fmt.Fprintln(stderr, "aicrew: warning: no -expect-user; anyone who holds the code and an aimem credential for this hub can redeem it")
	}
	var f *os.File
	if *codeFile != "" {
		if f, err = privatefile.Create(*codeFile); err != nil {
			fmt.Fprintln(stderr, "aicrew: create the code file (it must not exist):", err)
			return 1
		}
	}
	code, err := store.GenerateInvitationCode()
	var inv store.Invitation
	if err == nil {
		inv, err = st.IssueInvitation(ctx, op, newKey(), req, code)
	}
	if err != nil {
		if f != nil {
			f.Close()
			os.Remove(*codeFile)
		}
		fmt.Fprintln(stderr, "aicrew:", err)
		return 1
	}
	v := invitationOf(inv, now)
	if f != nil {
		if werr := errors.Join(writeCode(f, code.Reveal()), f.Close()); werr != nil {
			os.Remove(*codeFile)
			// The code is lost: revoke the invitation just issued.
			if _, rerr := st.RevokeInvitation(ctx, op, "revoke-"+inv.ID, inv.ID, inv.Revision); rerr != nil {
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
	fmt.Fprintf(stdout, "\nInvitation code, shown once. Give it privately to the person running the agent:\n\n    %s\n\n", code.Reveal())
	return 0
}
