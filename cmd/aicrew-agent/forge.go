package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"strings"

	"github.com/BlackVS/aicrew/internal/agent"
	"github.com/BlackVS/aicrew/internal/forge"
)

const forgeUsage = `usage: aicrew-agent forge pr open    [--home DIR] [--repository URL] --title TITLE --body-file FILE|- [--draft] [--head BRANCH] [--base BRANCH] [--json]
       aicrew-agent forge pr show    [--home DIR] [--repository URL] --pr NUMBER [--json]
       aicrew-agent forge pr label   add|remove [--home DIR] [--repository URL] --pr NUMBER LABEL... [--json]
       aicrew-agent forge pr comment [--home DIR] [--repository URL] --pr NUMBER --body-file FILE|- [--json]
  Opens, reads, labels and comments on a pull request with this home's own
  forge credential for the repository's host (join --cred), and prints the
  pull request's number and URL. The repository is the current directory's
  origin unless --repository names it; open's head is the current branch
  and its base the repository's default branch unless --head and --base
  name them. --draft opens a draft: GitHub's draft flag, or the title prefix
  "WIP: " on Gitea and "Draft: " on GitLab. Labels must exist already: none
  is created. Never use another forge client or a credential by hand.
  --home defaults to $` + agent.HomeEnv + `, which the launcher gives its client.`

// newPullAPI is the forge client `forge pr` uses; tests replace it.
var newPullAPI = func() agent.PullAPI { return forge.NewClient() }

// forgeCmd is `aicrew-agent forge pr ...`.
func forgeCmd(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer, getenv func(string) string) int {
	if len(args) < 2 || args[0] != "pr" {
		fmt.Fprintln(stderr, forgeUsage)
		return exitUsage
	}
	verb, rest := args[1], args[2:]
	add := false
	if verb == "label" {
		if len(rest) == 0 || (rest[0] != "add" && rest[0] != "remove") {
			fmt.Fprintln(stderr, forgeUsage)
			return exitUsage
		}
		add, rest = rest[0] == "add", rest[1:]
	}
	fs := flag.NewFlagSet("aicrew-agent forge pr "+verb, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	f := agent.ForgePR{}
	fs.StringVar(&f.Home, "home", getenv(agent.HomeEnv), "")
	fs.StringVar(&f.Repository, "repository", "", "")
	asJSON := fs.Bool("json", false, "")
	var o agent.OpenPull
	var bodyFile string
	var pr int64
	switch verb {
	case "open":
		fs.StringVar(&o.Title, "title", "", "")
		fs.StringVar(&bodyFile, "body-file", "", "")
		fs.BoolVar(&o.Draft, "draft", false, "")
		fs.StringVar(&o.Head, "head", "", "")
		fs.StringVar(&o.Base, "base", "", "")
	case "show", "label", "comment":
		fs.Int64Var(&pr, "pr", 0, "")
		if verb == "comment" {
			fs.StringVar(&bodyFile, "body-file", "", "")
		}
	default:
		fmt.Fprintln(stderr, forgeUsage)
		return exitUsage
	}
	err := fs.Parse(rest)
	usage := err != nil || f.Home == "" ||
		(verb == "open" && (o.Title == "" || bodyFile == "" || fs.NArg() != 0)) ||
		(verb != "open" && pr <= 0) ||
		(verb == "show" && fs.NArg() != 0) ||
		(verb == "comment" && (bodyFile == "" || fs.NArg() != 0)) ||
		(verb == "label" && fs.NArg() == 0)
	if usage {
		fmt.Fprintln(stderr, forgeUsage)
		return exitUsage
	}
	f.API = newPullAPI()
	var rep agent.PullReport
	switch verb {
	case "open":
		if o.Body, err = agent.ReadPullText(f.Home, bodyFile, stdin); err == nil {
			rep, err = f.Open(ctx, o)
		}
	case "show":
		rep, err = f.Show(ctx, pr)
	case "label":
		rep, err = f.Label(ctx, pr, add, fs.Args())
	case "comment":
		var body string
		if body, err = agent.ReadPullText(f.Home, bodyFile, stdin); err == nil {
			rep, err = f.Comment(ctx, pr, body)
		}
	}
	if err != nil {
		fmt.Fprintln(stderr, "forge pr "+verb+":", err)
		return exitFailed
	}
	if *asJSON {
		out, _ := json.MarshalIndent(rep, "", "  ")
		fmt.Fprintln(stdout, string(out))
		return exitOK
	}
	printPull(stdout, rep)
	return exitOK
}

func printPull(w io.Writer, rep agent.PullReport) {
	p := rep.Pull
	fmt.Fprintf(w, "Pull request %d: %s\n", p.Number, p.URL)
	state := p.State
	if p.Draft {
		state += ", draft"
	}
	fmt.Fprintf(w, "title: %s\nstate: %s\n", p.Title, state)
	fmt.Fprintf(w, "head: %s %s\nbase: %s\n", p.Head, p.HeadCommit, p.Base)
	if p.MergeCommit != "" {
		fmt.Fprintf(w, "merge commit: %s\n", p.MergeCommit)
	}
	fmt.Fprintf(w, "labels: %s\n", strings.Join(p.Labels, ", "))
	if rep.Comment != nil {
		fmt.Fprintf(w, "comment: %s\n", rep.Comment.URL)
	}
}
