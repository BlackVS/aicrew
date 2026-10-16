package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/BlackVS/aicrew/internal/agent"
	"github.com/BlackVS/aicrew/internal/forge"
)

const forgeUsage = `usage: aicrew-agent forge pr open    [--home DIR] [--repository URL] --title TITLE --body-file FILE|- [--draft] [--head BRANCH] [--base BRANCH] [--json]
       aicrew-agent forge pr show    [--home DIR] [--repository URL] --pr NUMBER [--json]
       aicrew-agent forge pr label   add|remove [--home DIR] [--repository URL] --pr NUMBER LABEL... [--json]
       aicrew-agent forge pr comment [--home DIR] [--repository URL] --pr NUMBER --body-file FILE|- [--json]
       aicrew-agent forge pr ready   [--home DIR] [--repository URL] --pr NUMBER [--json]
  Opens, reads, labels and comments on a pull request with this home's own
  forge credential for the repository's host (join --cred), and prints the
  pull request's number and URL. The repository is the current directory's
  origin unless --repository names it; open's head is the current branch
  and its base the repository's default branch unless --head and --base
  name them. --draft opens a draft: GitHub's draft flag, or the title prefix
  "WIP: " on Gitea and "Draft: " on GitLab. ready marks it ready once the
  gates are green: it drops GitHub's draft flag and, in every dialect, a
  draft title prefix (WIP:, [WIP], Draft:, [Draft], (Draft)). Labels must
  exist already: none is created. show also prints the head commit's checks,
  with a log excerpt of at most 8 KiB for a failed job where the forge
  offers its log, and the comments with their authors and bodies, oldest
  first; a review's header line ("reviewed at head SHA") is marked when it
  names the head. Never use another forge client or a credential by hand.
  Each command reports the pull request to the launcher, which then watches
  it: a change that needs a decision (a review asking for changes, CI
  failing, both gates green, a merge or a close) arrives in the inbox as an
  action.needed message with its content and wakes the member. Do not wait
  with a background watch: end the turn.
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
	case "show", "label", "comment", "ready":
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
		((verb == "show" || verb == "ready") && fs.NArg() != 0) ||
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
	case "ready":
		rep, err = f.Ready(ctx, pr)
	case "label":
		rep, err = f.Label(ctx, pr, add, fs.Args())
	case "comment":
		var body string
		if body, err = agent.ReadPullText(f.Home, bodyFile, stdin); err == nil {
			rep, err = f.Comment(ctx, pr, body)
		}
	}
	// Everything printed passes through RedactForgeTokens: an input quoted
	// before a check could refuse it never shows a token.
	if err != nil {
		fmt.Fprintln(stderr, agent.RedactForgeTokens(f.Home, "forge pr "+verb+": "+err.Error()))
		return exitFailed
	}
	var out strings.Builder
	if *asJSON {
		raw, _ := json.MarshalIndent(rep, "", "  ")
		fmt.Fprintln(&out, string(raw))
	} else {
		printPull(&out, rep)
	}
	fmt.Fprint(stdout, agent.RedactForgeTokens(f.Home, out.String()))
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
	if rep.Watch != "" {
		fmt.Fprintf(w, "watch: %s\n", rep.Watch)
	}
	if rep.Content != nil {
		printContent(w, *rep.Content)
	}
}

// printContent prints a pull request's checks and comments. Every body and
// excerpt is indented under its heading, so a line in it cannot pass for
// one of these.
func printContent(w io.Writer, c forge.PullContent) {
	if len(c.Checks) == 0 {
		fmt.Fprintln(w, "checks: none reported")
	} else {
		fmt.Fprintf(w, "checks: %d\n", len(c.Checks))
	}
	for _, ch := range c.Checks {
		word := ch.State
		if ch.Conclusion != "" && ch.Conclusion != ch.State {
			word += " (" + ch.Conclusion + ")"
		}
		fmt.Fprintf(w, "  - %s: %s", ch.Name, word)
		if ch.URL != "" {
			fmt.Fprintf(w, " %s", ch.URL)
		}
		fmt.Fprintln(w)
		switch {
		case ch.Log == nil:
		case ch.Log.None != "":
			fmt.Fprintf(w, "    no log excerpt: %s\n", ch.Log.None)
		default:
			how := "log excerpt"
			if ch.Log.Cut {
				how += " (part of the log)"
			}
			fmt.Fprintf(w, "    %s:\n", how)
			indent(w, ch.Log.Excerpt, "    | ")
		}
	}
	fmt.Fprintf(w, "comments: %d\n", len(c.Comments))
	for _, cm := range c.Comments {
		fmt.Fprintf(w, "  - %s %s by %s", cm.At.UTC().Format(time.RFC3339), cm.Kind, cm.Author)
		if cm.State != "" {
			fmt.Fprintf(w, ", %s", cm.State)
		}
		if cm.Path != "" {
			fmt.Fprintf(w, ", on %s:%d", cm.Path, cm.Line)
		}
		if r := cm.Review; r != nil {
			at := "not at the head"
			if r.AtHead {
				at = "at the head"
			}
			fmt.Fprintf(w, ", %s review of %s (%s)", r.Reviewer, r.Head, at)
		}
		if cm.URL != "" {
			fmt.Fprintf(w, " %s", cm.URL)
		}
		fmt.Fprintln(w)
		body := cm.Body
		if cm.Cut {
			body += "\n[cut at 32 KiB]"
		}
		indent(w, body, "    | ")
	}
}

// indent writes each line of text after prefix.
func indent(w io.Writer, text, prefix string) {
	for line := range strings.SplitSeq(strings.TrimRight(text, "\n"), "\n") {
		fmt.Fprintf(w, "%s%s\n", prefix, line)
	}
}
