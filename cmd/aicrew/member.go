package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/url"
	"os"
	"strings"

	"github.com/BlackVS/aicrew/internal/opapi"
	"github.com/BlackVS/aicrew/internal/opclient"
)

// aicrew member: the operator's control of one member of a team
// (docs/DESIGN-CONTROL-PLANE.md, section 4.2), through aicrewd's operator
// API. pause and resume hold and release the member's turns; nudge writes
// the operator's note into the member's inbox, which wakes it. --agent is
// the member's agent ID or its label, as aicrew team state shows them.

const memberUsage = `usage:
  aicrew member pause  (--team TEAM | --team-name NAME) --agent AGENT [--reason TEXT]
  aicrew member resume (--team TEAM | --team-name NAME) --agent AGENT
  aicrew member nudge  (--team TEAM | --team-name NAME) --agent AGENT (--note TEXT | --note-file PATH|-)
` + connUsage

// maxNoteFile bounds a note read from a file or standard input; aicrewd
// refuses a note longer than 4 KiB.
const maxNoteFile = 64 << 10

// runMember is aicrew member: 0 on success, 1 on a failure, 2 on a usage
// error.
func runMember(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	if len(args) < 1 {
		fmt.Fprint(stderr, memberUsage)
		return 2
	}
	verb := args[0]
	fs := flag.NewFlagSet("aicrew member "+verb, flag.ContinueOnError)
	fs.SetOutput(stderr)
	cn := addConn(fs)
	tf := addTeamFlags(fs)
	agent := fs.String("agent", "", "the member's agent ID or label")
	reason := fs.String("reason", "", "why the member is paused, kept with the pause")
	note := fs.String("note", "", "the note to the member")
	noteFile := fs.String("note-file", "", "a file holding the note, or - for standard input")
	if err := fs.Parse(args[1:]); err != nil || fs.NArg() != 0 {
		fmt.Fprint(stderr, memberUsage)
		return 2
	}
	set := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { set[f.Name] = true })
	usage := func() int { fmt.Fprint(stderr, memberUsage); return 2 }
	teamGiven, teamOK := tf.given(stderr)
	if !teamOK {
		return 2
	}
	if !teamGiven || *agent == "" {
		return usage()
	}
	switch verb {
	case "pause":
		if !allowedFlags(set, "team", "team-name", "agent", "reason") {
			return usage()
		}
	case "resume":
		if !allowedFlags(set, "team", "team-name", "agent") {
			return usage()
		}
	case "nudge":
		if !allowedFlags(set, "team", "team-name", "agent", "note", "note-file") || set["note"] == set["note-file"] {
			return usage()
		}
	default:
		return usage()
	}
	text := *note
	if set["note-file"] {
		var err error
		if text, err = readNote(*noteFile, stdin); err != nil {
			return failed(stderr, err)
		}
	}
	cl, ok := cn.connect(stderr)
	if !ok {
		return 1
	}
	team, err := tf.resolve(ctx, cl)
	if err != nil {
		return failed(stderr, err)
	}
	agentID, err := resolveMember(ctx, cl, team, *agent)
	if err != nil {
		return failed(stderr, err)
	}
	out := json.NewEncoder(stdout)
	out.SetIndent("", "  ")
	switch verb {
	case "pause", "resume":
		path := map[string]string{"pause": opapi.MemberPausePath, "resume": opapi.MemberResumePath}[verb]
		var res opapi.PauseResult
		if err := cl.Post(ctx, path, opapi.PauseRequest{TeamID: team, AgentID: agentID, Reason: *reason}, &res); err != nil {
			return failed(stderr, err)
		}
		_ = out.Encode(res)
		pauseNote(stderr, "member", verb, res)
	case "nudge":
		var n opapi.Nudge
		if err := cl.Post(ctx, opapi.MemberNudgePath, opapi.NudgeRequest{TeamID: team, AgentID: agentID, Note: text}, &n); err != nil {
			return failed(stderr, err)
		}
		_ = out.Encode(n)
	}
	return 0
}

// readNote reads a note from a file, or from standard input for "-",
// without its trailing newline.
func readNote(path string, stdin io.Reader) (string, error) {
	var r io.Reader = stdin
	if path != "-" {
		f, err := os.Open(path)
		if err != nil {
			return "", fmt.Errorf("read the note: %w", err)
		}
		defer f.Close()
		r = f
	}
	b, err := io.ReadAll(io.LimitReader(r, maxNoteFile+1))
	if err != nil {
		return "", fmt.Errorf("read the note: %w", err)
	}
	if len(b) > maxNoteFile {
		return "", fmt.Errorf("the note is longer than %d bytes", maxNoteFile)
	}
	return strings.TrimRight(string(b), "\r\n"), nil
}

// resolveMember is the agent ID of the team's current member named by its
// agent ID or, failing that, by its label, which must name one member.
func resolveMember(ctx context.Context, cl *opclient.Client, team, agent string) (string, error) {
	var d opapi.TeamState
	if err := cl.Get(ctx, opapi.TeamStatePath, url.Values{"id": {team}}, &d); err != nil {
		return "", err
	}
	var byLabel []string
	for _, m := range d.Members {
		if m.AgentID == agent {
			return m.AgentID, nil
		}
		if m.Label == agent {
			byLabel = append(byLabel, m.AgentID)
		}
	}
	switch len(byLabel) {
	case 1:
		return byLabel[0], nil
	case 0:
		return "", fmt.Errorf("no current member of team %s has agent ID or label %q; aicrew team state lists them", d.TeamName, agent)
	default:
		return "", fmt.Errorf("%d members of team %s have label %q; name one by its agent ID", len(byLabel), d.TeamName, agent)
	}
}
