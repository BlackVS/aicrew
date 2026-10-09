// Command aicrew is the operator's console client of aicrewd's operator API
// (docs/DEVELOPMENT.md, "The operator API"). It never opens the store:
// aicrewd keeps running while the operator administers, from any machine
// that reaches it over TLS with the operator credential.
//
//	aicrew hub-credential issue  --hub HUB --output PATH|- [--operations LIST]
//	aicrew hub-credential rotate --hub HUB --output PATH|- [--operations LIST]
//	aicrew hub-credential list   [--hub HUB]
//	aicrew hub-credential revoke --id ID
//	aicrew invitation issue|list|revoke ... (see invitation.go)
//	aicrew team create|list|show|rename|register|setup ... (see team.go, teamsetup.go)
//	aicrew hub add NAME ... (see hub.go)
//	aicrew operator-token new --output PATH|-
//	aicrew version [--json]
//
// hub-credential is the credential an aimem hub uses to call this service.
// Its name before 0.3.0, introspection-credential, still works,
// with a notice, until 0.5.0 removes it.
//
// Every command but hub, operator-token and version takes the connection flags
// (conn.go). --operations names what the new credential permits,
// comma-separated: introspection (identity.v1 session introspection),
// coordination (coordination.v1 facts), or both, which is the default.
//
// A credential's bearer is written only where --output names (secretout.go)
// and never to a terminal; the command prints the credential's metadata as
// JSON. If the bearer cannot be written, the credential just issued is
// revoked.
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
	"strings"
	"time"

	"github.com/BlackVS/aicrew/internal/opapi"
	"github.com/BlackVS/aicrew/internal/opclient"
	"github.com/BlackVS/aicrew/internal/optoken"
	"github.com/BlackVS/aicrew/internal/version"
)

func main() {
	os.Exit(run(context.Background(), os.Args[1:], os.Stdout, os.Stderr))
}

const usage = `usage:
  aicrew hub-credential issue  --hub HUB --output PATH|- [--operations LIST]
  aicrew hub-credential rotate --hub HUB --output PATH|- [--operations LIST]
  aicrew hub-credential list   [--hub HUB]
  aicrew hub-credential revoke --id ID
  aicrew invitation issue|list|revoke ...   (run "aicrew invitation" for its usage)
  aicrew team create|list|show|rename|register|setup ...   (run "aicrew team" for its usage)
  aicrew hub add NAME ...   (run "aicrew hub" for its usage)
  aicrew operator-token new --output PATH|-
  aicrew architect init --dir DIR --project PROJECT ...   (run "aicrew architect" for its usage)
  aicrew version [--json]
` + connUsage

// run is the command: 0 on success, 1 on a failure, 2 on a usage error.
func run(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	if len(args) >= 1 && args[0] == "invitation" {
		return runInvitation(ctx, args[1:], stdout, stderr)
	}
	if len(args) >= 1 && args[0] == "team" {
		return runTeam(ctx, args[1:], stdout, stderr)
	}
	if len(args) >= 1 && args[0] == "hub" {
		return runHub(ctx, args[1:], stdout, stderr, time.Now())
	}
	if len(args) >= 1 && args[0] == "architect" {
		return runArchitect(args[1:], stdout, stderr)
	}
	if len(args) >= 1 && args[0] == "operator-token" {
		return runOperatorToken(args[1:], stdout, stderr)
	}
	if len(args) >= 1 && args[0] == "version" {
		fs := flag.NewFlagSet("aicrew version", flag.ContinueOnError)
		fs.SetOutput(stderr)
		asJSON := fs.Bool("json", false, "print the build as JSON")
		if err := fs.Parse(args[1:]); err != nil || fs.NArg() != 0 {
			fmt.Fprint(stderr, usage)
			return 2
		}
		version.Print(stdout, "aicrew", *asJSON)
		return 0
	}
	if len(args) < 2 || (args[0] != "hub-credential" && args[0] != "introspection-credential") {
		fmt.Fprint(stderr, usage)
		return 2
	}
	if args[0] == "introspection-credential" {
		fmt.Fprintln(stderr, "aicrew: introspection-credential is now hub-credential; the old name is removed in 0.5.0")
	}
	verb := args[1]
	fs := flag.NewFlagSet("aicrew hub-credential "+verb, flag.ContinueOnError)
	fs.SetOutput(stderr)
	cn := addConn(fs)
	hub := fs.String("hub", "", "the aimem hub the credential is bound to")
	outFlags := addOutput(fs, "secret-file", "bearer")
	id := fs.String("id", "", "credential ID")
	opsFlag := fs.String("operations", "", "what the credential permits: introspection, coordination, or both (default)")
	if err := fs.Parse(args[2:]); err != nil || fs.NArg() != 0 {
		fmt.Fprint(stderr, usage)
		return 2
	}
	set := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { set[f.Name] = true })
	output, outOK := outFlags.value(set, stderr)
	if !outOK {
		return 2
	}
	ops, opsOK := parseOps(*opsFlag)
	switch verb {
	case "issue", "rotate":
		if *hub == "" || output == "" || *id != "" || !opsOK {
			fmt.Fprint(stderr, usage)
			return 2
		}
	case "list":
		if output != "" || *id != "" || *opsFlag != "" {
			fmt.Fprint(stderr, usage)
			return 2
		}
	case "revoke":
		if *id == "" || *hub != "" || output != "" || *opsFlag != "" {
			fmt.Fprint(stderr, usage)
			return 2
		}
	default:
		fmt.Fprint(stderr, usage)
		return 2
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
		if *hub != "" {
			q.Set("hub", *hub)
		}
		var creds []opapi.Credential
		if err := cl.Get(ctx, opapi.CredentialsPath, q, &creds); err != nil {
			return failed(stderr, err)
		}
		_ = out.Encode(creds)
		return 0
	case "revoke":
		var c opapi.Credential
		if err := cl.Post(ctx, opapi.CredentialRevokePath, opapi.IDRequest{ID: *id}, &c); err != nil {
			return failed(stderr, err)
		}
		_ = out.Encode(c)
		return 0
	}

	// issue or rotate: the output is ready before anything is issued.
	so, err := openSecretOutput(output, stdout)
	if err != nil {
		fmt.Fprintln(stderr, "aicrew:", err)
		return 1
	}
	path := opapi.CredentialsPath
	if verb == "rotate" {
		path = opapi.CredentialRotatePath
	}
	var c opapi.Credential
	err = cl.Post(ctx, path, opapi.CredentialRequest{HubID: *hub, Operations: ops}, &c)
	if err == nil && (c.ID == "" || c.Bearer == "" || (verb == "rotate" && c.Replaces == "")) {
		err = errors.New("aicrewd's answer lacks the credential's ID, bearer or the credential it replaces")
	}
	if err != nil {
		so.discard()
		return issueFailed(ctx, stderr, cl, err, "credential", c.ID, opapi.CredentialRevokePath,
			"aicrew hub-credential list --hub "+*hub)
	}
	bearer := c.Bearer
	c.Bearer = ""
	if werr := so.write(bearer); werr != nil {
		// The bearer is lost: revoke only the credential just issued.
		if rerr := cl.Post(ctx, opapi.CredentialRevokePath, opapi.IDRequest{ID: c.ID}, nil); rerr != nil {
			fmt.Fprintf(stderr, "aicrew: writing the bearer failed (%v), and revoking credential %s failed too (%v); revoke it before anything else\n", werr, c.ID, rerr)
			return 1
		}
		fmt.Fprintf(stderr, "aicrew: writing the bearer failed (%v); credential %s was revoked\n", werr, c.ID)
		return 1
	}
	v := credentialView{Credential: c, Output: output}
	if verb == "rotate" {
		v.Next = "Give aimem the new credential, then revoke " + c.Replaces + "."
	}
	info := json.NewEncoder(so.info(stderr))
	info.SetIndent("", "  ")
	_ = info.Encode(v)
	return 0
}

// credentialView is what issue and rotate print: metadata, never a bearer.
type credentialView struct {
	opapi.Credential
	Output string `json:"output,omitempty"`
	Next   string `json:"next,omitempty"`
}

// parseOps reads --operations: the API's operation names. An empty flag is
// an empty list: the service's default, both.
func parseOps(flag string) ([]string, bool) {
	ops := []string{}
	if flag == "" {
		return ops, true
	}
	for _, name := range strings.Split(flag, ",") {
		switch n := strings.TrimSpace(name); n {
		case "introspection", "coordination":
			ops = append(ops, n)
		default:
			return nil, false
		}
	}
	return ops, true
}

// runOperatorToken is aicrew operator-token new: it writes a new operator
// credential where --output names, for aicrewd.json's operator_token_file
// and the operator's own copy. It calls no service; the token never reaches
// a terminal.
func runOperatorToken(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("aicrew operator-token new", flag.ContinueOnError)
	fs.SetOutput(stderr)
	outFlags := addOutput(fs, "file", "token")
	if len(args) < 1 || args[0] != "new" || fs.Parse(args[1:]) != nil || fs.NArg() != 0 {
		fmt.Fprint(stderr, usage)
		return 2
	}
	set := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { set[f.Name] = true })
	output, ok := outFlags.value(set, stderr)
	if !ok || output == "" {
		fmt.Fprint(stderr, usage)
		return 2
	}
	tok, err := optoken.Generate()
	if err != nil {
		fmt.Fprintln(stderr, "aicrew: generate the operator token:", err)
		return 1
	}
	so, err := openSecretOutput(output, stdout)
	if err == nil {
		err = so.write(tok)
	}
	if err != nil {
		fmt.Fprintln(stderr, "aicrew: write the operator token:", err)
		return 1
	}
	where := output
	if output == "-" {
		where = "standard output"
	}
	fmt.Fprintf(so.info(stderr), "Wrote a new operator token to %s. Name it as operator_token_file in "+
		"aicrewd.json on the service's host, and keep the operator's copy owner-only.\n", where)
	return 0
}

// revisionHint explains a revision conflict.
func revisionHint(stderr io.Writer, err error, expect int64) int {
	if opclient.Code(err) == opapi.CodeRevisionConflict {
		fmt.Fprintf(stderr, "aicrew: %v; the team changed since revision %d: run aicrew team show, then try again with its revision\n", err, expect)
		return 1
	}
	return failed(stderr, err)
}
