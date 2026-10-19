package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/BlackVS/aicrew/internal/automation"
)

// aicrew automation replay (docs/DESIGN-TURNS.md 13.3, increment 2f): a
// workflow script run against the events of a member's run log
// (logs/automations.jsonl under the member's home) with a recording host.
// The launcher keeps, on each run's end record, the event the run was for
// and the facts pr() and attempt() returned; the replay answers those
// calls from the record and logs every other call as what the script
// would have done, performing nothing. It reads no hub and no forge.

const automationUsage = `usage:
  aicrew automation replay --script FILE --events LOG

  FILE is a workflow script; LOG is a member's run log (logs/automations.jsonl
  under its home). Each run recorded with its event is replayed: pr() and
  attempt() answer the recorded facts; run, step, comment, label, ready,
  submit, rerun and update_base are printed, not performed (run answers
  exit 0 with no output, and every command is admitted).
`

// runAutomation is aicrew automation: 0 on success, 1 on a failure, 2 on a
// usage error.
func runAutomation(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	if len(args) < 1 || args[0] != "replay" {
		fmt.Fprint(stderr, automationUsage)
		return 2
	}
	fs := flag.NewFlagSet("aicrew automation replay", flag.ContinueOnError)
	fs.SetOutput(stderr)
	script := fs.String("script", "", "the workflow script")
	events := fs.String("events", "", "a member's run log (logs/automations.jsonl)")
	if err := fs.Parse(args[1:]); err != nil || fs.NArg() != 0 || *script == "" || *events == "" {
		fmt.Fprint(stderr, automationUsage)
		return 2
	}
	src, err := os.ReadFile(*script)
	if err != nil {
		return failed(stderr, err)
	}
	if _, err := automation.ParseHeader(src); err != nil {
		return failed(stderr, fmt.Errorf("%s: %w", *script, err))
	}
	log, err := os.Open(*events)
	if err != nil {
		return failed(stderr, err)
	}
	defer log.Close()
	fmt.Fprintf(stdout, "replaying %s against %s: nothing is performed\n", filepath.Base(*script), *events)
	n, err := automation.Replay(ctx, filepath.Base(*script), src, log, stdout)
	if err != nil {
		return failed(stderr, err)
	}
	fmt.Fprintf(stdout, "%d event(s) replayed\n", n)
	return 0
}
