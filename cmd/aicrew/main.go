// Command aicrew is aicrew's operator command line.
//
//	aicrew introspection-credential issue  -store PATH -hub HUB -secret-file PATH
//	aicrew introspection-credential rotate -store PATH -hub HUB -secret-file PATH
//	aicrew introspection-credential list   -store PATH [-hub HUB]
//	aicrew introspection-credential revoke -store PATH -id ID
//
// It opens the store file directly. Only one process may hold a store, so run
// it while aicrewd is stopped. A credential's bearer is written only to the
// new private file named by -secret-file and never printed; the output is the
// credential's metadata as JSON.
package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
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

func main() {
	os.Exit(run(context.Background(), os.Args[1:], os.Stdout, os.Stderr))
}

const usage = `usage:
  aicrew introspection-credential issue  -store PATH -hub HUB -secret-file PATH
  aicrew introspection-credential rotate -store PATH -hub HUB -secret-file PATH
  aicrew introspection-credential list   -store PATH [-hub HUB]
  aicrew introspection-credential revoke -store PATH -id ID
`

// writeSecret writes the bearer to its file; tests replace it to fail.
var writeSecret = func(f *os.File, bearer string) error {
	if _, err := io.WriteString(f, bearer+"\n"); err != nil {
		return err
	}
	return f.Sync()
}

// run is the command: 0 on success, 1 on a failure, 2 on a usage error.
func run(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	if len(args) < 2 || args[0] != "introspection-credential" {
		fmt.Fprint(stderr, usage)
		return 2
	}
	verb := args[1]
	fs := flag.NewFlagSet("aicrew introspection-credential "+verb, flag.ContinueOnError)
	fs.SetOutput(stderr)
	storePath := fs.String("store", "", "path to the aicrew store")
	hub := fs.String("hub", "", "the aimem hub the credential is bound to")
	secretFile := fs.String("secret-file", "", "new private file that receives the bearer")
	id := fs.String("id", "", "credential ID")
	if err := fs.Parse(args[2:]); err != nil || fs.NArg() != 0 || *storePath == "" {
		fmt.Fprint(stderr, usage)
		return 2
	}
	switch verb {
	case "issue", "rotate":
		if *hub == "" || *secretFile == "" || *id != "" {
			fmt.Fprint(stderr, usage)
			return 2
		}
	case "list":
		if *secretFile != "" || *id != "" {
			fmt.Fprint(stderr, usage)
			return 2
		}
	case "revoke":
		if *id == "" || *hub != "" || *secretFile != "" {
			fmt.Fprint(stderr, usage)
			return 2
		}
	default:
		fmt.Fprint(stderr, usage)
		return 2
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
		creds, err := st.ListIntrospectionCredentials(ctx, op)
		if err != nil {
			fmt.Fprintln(stderr, "aicrew:", err)
			return 1
		}
		shown := []credentialView{}
		for _, c := range creds {
			if *hub == "" || c.HubID == *hub {
				shown = append(shown, view(c, now))
			}
		}
		_ = out.Encode(shown)
		return 0
	case "revoke":
		c, err := st.RevokeIntrospectionCredential(ctx, op, "revoke-"+*id, *id)
		if err != nil {
			fmt.Fprintln(stderr, "aicrew:", err)
			return 1
		}
		_ = out.Encode(view(c, now))
		return 0
	}

	// issue or rotate.
	creds, err := st.ListIntrospectionCredentials(ctx, op)
	if err != nil {
		fmt.Fprintln(stderr, "aicrew:", err)
		return 1
	}
	var active []store.IntrospectionCredential
	for _, c := range creds {
		if c.HubID == *hub && c.Active(now) {
			active = append(active, c)
		}
	}
	if verb == "rotate" && len(active) != 1 {
		fmt.Fprintf(stderr, "aicrew: rotate needs exactly one active credential for the hub; it has %d\n", len(active))
		return 1
	}
	f, err := privatefile.Create(*secretFile)
	if err != nil {
		fmt.Fprintln(stderr, "aicrew: create the secret file (it must not exist):", err)
		return 1
	}
	c, bearer, err := st.IssueIntrospectionCredential(ctx, op, newKey(), *hub)
	if err != nil {
		f.Close()
		os.Remove(*secretFile)
		fmt.Fprintln(stderr, "aicrew:", err)
		return 1
	}
	if werr := errors.Join(writeSecret(f, bearer), f.Close()); werr != nil {
		os.Remove(*secretFile)
		// The bearer is lost: revoke only the credential just issued.
		if _, rerr := st.RevokeIntrospectionCredential(ctx, op, "revoke-"+c.ID, c.ID); rerr != nil {
			fmt.Fprintf(stderr, "aicrew: writing the secret file failed (%v), and revoking credential %s failed too (%v); revoke it before anything else\n", werr, c.ID, rerr)
			return 1
		}
		fmt.Fprintf(stderr, "aicrew: writing the secret file failed (%v); credential %s was revoked\n", werr, c.ID)
		return 1
	}
	v := view(c, now)
	v.SecretFile = *secretFile
	if verb == "rotate" {
		v.Replaces = active[0].ID
		v.Next = "Give aimem the new credential, then revoke " + active[0].ID + "."
	}
	_ = out.Encode(v)
	return 0
}

// credentialView is what the command prints: metadata, never a bearer.
type credentialView struct {
	ID         string    `json:"id"`
	HubID      string    `json:"hub_id"`
	Active     bool      `json:"active"`
	CreatedAt  time.Time `json:"created_at"`
	ExpiresAt  time.Time `json:"expires_at"`
	RevokedAt  time.Time `json:"revoked_at,omitzero"`
	SecretFile string    `json:"secret_file,omitempty"`
	Replaces   string    `json:"replaces,omitempty"`
	Next       string    `json:"next,omitempty"`
}

func view(c store.IntrospectionCredential, now time.Time) credentialView {
	return credentialView{ID: c.ID, HubID: c.HubID, Active: c.Active(now), CreatedAt: c.CreatedAt,
		ExpiresAt: c.ExpiresAt, RevokedAt: c.RevokedAt}
}

// newKey is a fresh command key: an operator's issue is never replayed.
func newKey() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	return "cli-" + hex.EncodeToString(b[:])
}
