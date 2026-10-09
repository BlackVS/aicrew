package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"

	"github.com/BlackVS/aicrew/internal/architect"
)

const architectUsage = `usage:
  aicrew architect init --dir DIR --project PROJECT [--project PROJECT ...] [--hub HUB]

init writes the architect directory: the operator's own Claude Code session
that plans with the operator and writes epics and tasks to the aimem board
(docs/DESIGN-CONTROL-PLANE.md, section 7). It calls no service. A rerun
updates a managed file only while it is unchanged since its last managed
write; otherwise it writes the new version beside it as <file>.aicrew-new.
`

// runArchitect is "aicrew architect": 0 on success, 1 on a failure or a
// conflict to merge, 2 on a usage error.
func runArchitect(args []string, stdout, stderr io.Writer) int {
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
	if err := fs.Parse(args[1:]); err != nil || fs.NArg() != 0 || *dir == "" || len(projects) == 0 {
		fmt.Fprint(stderr, architectUsage)
		return 2
	}
	rep, err := architect.Init(architect.Options{Dir: *dir, Projects: projects, Hub: *hub})
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
	return 0
}
