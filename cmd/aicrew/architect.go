package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"

	"github.com/BlackVS/aicrew/internal/architect"
	"github.com/BlackVS/aicrew/internal/opapi"
)

const architectUsage = `usage:
  aicrew architect init --dir DIR --project PROJECT [--project PROJECT ...] [--hub HUB]
                        [--url URL --tls-trust-mode MODE --tls-trust-value VALUE]
  aicrew architect credential issue  --label LABEL --output PATH|-
  aicrew architect credential list
  aicrew architect credential revoke --id ID

init writes the architect directory: the operator's own Claude Code session
that plans with the operator and writes epics and tasks to the aimem board
(docs/DESIGN-CONTROL-PLANE.md, section 7). It calls no service. A rerun
updates a managed file only while it is unchanged since its last managed
write; otherwise it writes the new version beside it as <file>.aicrew-new.
With --url and the trust flags, the session's settings name aicrewd and the
architect credential's file, DIR/creds/aicrew.architect, so its
"aicrew escalations" commands work there.

credential issues, lists and revokes architect credentials, with the
operator credential: each reads and answers escalations, and nothing else.
`

// runArchitect is "aicrew architect": 0 on success, 1 on a failure or a
// conflict to merge, 2 on a usage error.
func runArchitect(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	if len(args) >= 1 && args[0] == "credential" {
		return runArchitectCredential(ctx, args[1:], stdout, stderr)
	}
	if len(args) < 1 || args[0] != "init" {
		fmt.Fprint(stderr, architectUsage)
		return 2
	}
	fs := flag.NewFlagSet("aicrew architect init", flag.ContinueOnError)
	fs.SetOutput(stderr)
	dir := fs.String("dir", "", "the architect directory")
	var projects projectList
	fs.Var(&projects, "project", "an aimem project the architect plans; repeat it for each (the first binds a new directory)")
	hub := fs.String("hub", "", "the aimem hub alias of the user's installation, when it has more than one")
	crewURL := fs.String("url", "", "aicrewd's https origin, for the session's escalation commands")
	mode := fs.String("tls-trust-mode", "", "ca_dns or spki_sha256")
	value := fs.String("tls-trust-value", "", "the host name, or sha256- and the pin")
	if err := fs.Parse(args[1:]); err != nil || fs.NArg() != 0 || *dir == "" || len(projects) == 0 {
		fmt.Fprint(stderr, architectUsage)
		return 2
	}
	if (*crewURL == "") != (*mode == "") || (*mode == "") != (*value == "") {
		fmt.Fprintln(stderr, "aicrew: --url, --tls-trust-mode and --tls-trust-value go together")
		return 2
	}
	rep, err := architect.Init(architect.Options{Dir: *dir, Projects: projects, Hub: *hub,
		Crew: architect.Connection{URL: *crewURL, TrustMode: *mode, TrustValue: *value}})
	if err != nil {
		fmt.Fprintf(stderr, "aicrew: architect init: %v\n", err)
		return 1
	}
	b, _ := json.MarshalIndent(rep, "", "  ")
	fmt.Fprintln(stdout, string(b))
	if rep.Status != "ready" {
		fmt.Fprintln(stderr, "aicrew: "+rep.Note)
		return 1
	}
	fmt.Fprintf(stderr, "aicrew: the architect directory is ready; start Claude Code in %s\n", rep.Dir)
	if *crewURL != "" {
		fmt.Fprintf(stderr, "aicrew: put the architect credential in %s: aicrew architect credential issue --label LABEL --output %s\n",
			rep.CredentialFile, rep.CredentialFile)
	}
	return 0
}

const architectCredentialUsage = `usage:
  aicrew architect credential issue  --label LABEL --output PATH|-
  aicrew architect credential list
  aicrew architect credential revoke --id ID
` + connUsage

// runArchitectCredential manages architect credentials with the operator
// credential. The bearer is written only where --output names, as every
// secret this client reveals.
func runArchitectCredential(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	if len(args) < 1 {
		fmt.Fprint(stderr, architectCredentialUsage)
		return 2
	}
	verb := args[0]
	fs := flag.NewFlagSet("aicrew architect credential "+verb, flag.ContinueOnError)
	fs.SetOutput(stderr)
	cn := addConn(fs)
	label := fs.String("label", "", "a name for the credential: lowercase letters, digits and -")
	output := fs.String("output", "", "new owner-only file that receives the bearer, or - for a pipe")
	id := fs.String("id", "", "the credential's ID")
	if err := fs.Parse(args[1:]); err != nil || fs.NArg() != 0 {
		fmt.Fprint(stderr, architectCredentialUsage)
		return 2
	}
	set := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { set[f.Name] = true })
	usage := func() int { fmt.Fprint(stderr, architectCredentialUsage); return 2 }
	switch verb {
	case "issue":
		if !allowedFlags(set, "label", "output") || *label == "" {
			return usage()
		}
		if *output == "" {
			fmt.Fprintln(stderr, "aicrew: the bearer is written only where --output names: a new file, or - for a pipe; it never reaches the terminal")
			return 2
		}
	case "list":
		if !allowedFlags(set) {
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
		var list []opapi.ArchitectCredential
		if err := cl.Get(ctx, opapi.ArchitectCredentialsPath, nil, &list); err != nil {
			return failed(stderr, err)
		}
		_ = out.Encode(list)
		return 0
	case "revoke":
		var c opapi.ArchitectCredential
		if err := cl.Post(ctx, opapi.ArchitectCredentialRevokePath, opapi.IDRequest{ID: *id}, &c); err != nil {
			return failed(stderr, err)
		}
		_ = out.Encode(c)
		return 0
	}

	// issue.
	so, err := openSecretOutput(*output, stdout)
	if err != nil {
		fmt.Fprintln(stderr, "aicrew:", err)
		return 1
	}
	var c opapi.ArchitectCredential
	err = cl.Post(ctx, opapi.ArchitectCredentialsPath, opapi.ArchitectCredentialRequest{Label: *label}, &c)
	if err == nil && (c.ID == "" || c.Bearer == "") {
		err = errors.New("aicrewd's answer lacks the credential's ID or bearer")
	}
	if err != nil {
		so.discard()
		return issueFailed(ctx, stderr, cl, err, "architect credential", c.ID, opapi.ArchitectCredentialRevokePath,
			"aicrew architect credential list")
	}
	bearer := c.Bearer
	c.Bearer = ""
	if werr := so.write(bearer); werr != nil {
		// The bearer is lost: revoke the credential just issued.
		if rerr := cl.Post(ctx, opapi.ArchitectCredentialRevokePath, opapi.IDRequest{ID: c.ID}, nil); rerr != nil {
			fmt.Fprintf(stderr, "aicrew: writing the bearer failed (%v), and revoking credential %s failed too (%v); revoke it before anything else\n", werr, c.ID, rerr)
			return 1
		}
		fmt.Fprintf(stderr, "aicrew: writing the bearer failed (%v); credential %s was revoked\n", werr, c.ID)
		return 1
	}
	info := json.NewEncoder(so.info(stderr))
	info.SetIndent("", "  ")
	_ = info.Encode(struct {
		opapi.ArchitectCredential
		Output string `json:"output"`
	}{c, *output})
	return 0
}
