package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"

	"github.com/BlackVS/aicrew/internal/agent"
)

const digestUsage = `usage: aicrew-agent digest [--home DIR] --repository HTTPS_URL --commit COMMIT --manifest PATH [--json]
  Prints the instruction digest of a process pin: sha256: and the hex SHA-256
  of the manifest's bytes at the commit. Fetches the process repository into
  repos/ first, with the member's credential for its host if the home holds
  one (join --cred), through the clone's own credential helper.
  --home defaults to $` + agent.HomeEnv + `, which the launcher gives its client.`

// digest is `aicrew-agent digest`.
func digest(ctx context.Context, args []string, stdout, stderr io.Writer, getenv func(string) string) int {
	fs := flag.NewFlagSet("aicrew-agent digest", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	var o agent.DigestOptions
	fs.StringVar(&o.Home, "home", getenv(agent.HomeEnv), "")
	fs.StringVar(&o.Repository, "repository", "", "")
	fs.StringVar(&o.Commit, "commit", "", "")
	fs.StringVar(&o.Manifest, "manifest", "", "")
	asJSON := fs.Bool("json", false, "")
	if err := fs.Parse(args); err != nil || fs.NArg() != 0 || o.Home == "" || o.Repository == "" ||
		o.Commit == "" || o.Manifest == "" {
		fmt.Fprintln(stderr, digestUsage)
		return exitUsage
	}
	o.Out = stderr
	rep, err := agent.Digest(ctx, o)
	if err != nil {
		fmt.Fprintln(stderr, "digest:", err)
		return exitFailed
	}
	if *asJSON {
		out, _ := json.MarshalIndent(rep, "", "  ")
		fmt.Fprintln(stdout, string(out))
	} else {
		fmt.Fprintln(stdout, rep.Digest)
	}
	return exitOK
}
