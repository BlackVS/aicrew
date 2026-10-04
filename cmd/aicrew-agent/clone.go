package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"

	"github.com/BlackVS/aicrew/internal/agent"
)

const cloneUsage = `usage: aicrew-agent clone [--home DIR] --repository HTTPS_URL --attempt ID --base COMMIT --branch NAME [--json]
  Clones the repository into repos/ with the member's own credential for its
  host (join --cred), and makes the attempt's worktree under worktrees/ on a
  new branch at the base commit. The clone's own git configuration names this
  executable as its credential helper and sets the member's commit identity.
  --home defaults to $` + agent.HomeEnv + `, which the launcher gives its client.`

// clone is `aicrew-agent clone`.
func clone(ctx context.Context, args []string, stdout, stderr io.Writer, getenv func(string) string) int {
	fs := flag.NewFlagSet("aicrew-agent clone", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	var o agent.CloneOptions
	fs.StringVar(&o.Home, "home", getenv(agent.HomeEnv), "")
	fs.StringVar(&o.Repository, "repository", "", "")
	fs.StringVar(&o.Attempt, "attempt", "", "")
	fs.StringVar(&o.Base, "base", "", "")
	fs.StringVar(&o.Branch, "branch", "", "")
	asJSON := fs.Bool("json", false, "")
	if err := fs.Parse(args); err != nil || fs.NArg() != 0 || o.Home == "" || o.Repository == "" ||
		o.Attempt == "" || o.Base == "" || o.Branch == "" {
		fmt.Fprintln(stderr, cloneUsage)
		return exitUsage
	}
	o.Out = stderr
	rep, err := agent.Clone(ctx, o)
	if err != nil {
		fmt.Fprintln(stderr, "clone:", err)
		return exitFailed
	}
	if *asJSON {
		out, _ := json.MarshalIndent(rep, "", "  ")
		fmt.Fprintln(stdout, string(out))
	} else {
		fmt.Fprintf(stdout, "worktree: %s\nbranch: %s\nbase: %s\n", rep.Worktree, rep.Branch, rep.Base)
	}
	return exitOK
}

// gitCredential is `aicrew-agent git-credential --home DIR get|store|erase`,
// which git runs as a clone's credential helper. It answers only on git's
// pipe: on a terminal it refuses, so a token never reaches a screen.
func gitCredential(args []string, stdin io.Reader, stdout, stderr io.Writer, terminal bool) int {
	fs := flag.NewFlagSet("aicrew-agent git-credential", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	home := fs.String("home", "", "")
	if err := fs.Parse(args); err != nil || fs.NArg() != 1 || *home == "" {
		fmt.Fprintln(stderr, "usage: aicrew-agent git-credential --home DIR get|store|erase (run by git, not by hand)")
		return exitUsage
	}
	if terminal {
		fmt.Fprintln(stderr, "aicrew-agent git-credential answers git on a pipe, never a terminal")
		return exitUsage
	}
	if err := agent.GitCredential(*home, fs.Arg(0), stdin, stdout); err != nil {
		fmt.Fprintln(stderr, "git-credential:", err)
		return exitFailed
	}
	return exitOK
}
