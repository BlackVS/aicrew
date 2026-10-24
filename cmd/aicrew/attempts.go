package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/url"
	"strings"
	"time"

	"github.com/BlackVS/aicrew/internal/forge"
	"github.com/BlackVS/aicrew/internal/opapi"
)

// aicrew attempts handoff: an attempt's handoff, the worker's session doc
// as its launcher last pushed it (280c), through aicrewd's operator API,
// with the operator credential or an architect credential in the token
// file. The text is the worker's, so the plain print removes terminal
// escapes and control characters other than newline and tab; --json prints
// it as aicrewd answers.

const attemptsUsage = `usage:
  aicrew attempts handoff --id ATTEMPT [--json]
` + connUsage

// runAttempts is aicrew attempts: 0 on success, 1 on a failure, 2 on a
// usage error.
func runAttempts(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 || args[0] != "handoff" {
		fmt.Fprint(stderr, attemptsUsage)
		return 2
	}
	fs := flag.NewFlagSet("aicrew attempts handoff", flag.ContinueOnError)
	fs.SetOutput(stderr)
	cn := addConn(fs)
	id := fs.String("id", "", "the attempt's ID")
	asJSON := fs.Bool("json", false, "print the handoff as JSON")
	if err := fs.Parse(args[1:]); err != nil || fs.NArg() != 0 || *id == "" {
		fmt.Fprint(stderr, attemptsUsage)
		return 2
	}
	cl, ok := cn.connect(stderr)
	if !ok {
		return 1
	}
	var h opapi.AttemptHandoff
	if err := cl.Get(ctx, opapi.AttemptHandoffPath, url.Values{"id": {*id}}, &h); err != nil {
		return failed(stderr, err)
	}
	if *asJSON {
		out := json.NewEncoder(stdout)
		out.SetIndent("", "  ")
		_ = out.Encode(h)
		return 0
	}
	if h.UpdatedAt.IsZero() {
		fmt.Fprintf(stdout, "Attempt %s of team %s has no handoff.\n", h.AttemptID, h.TeamID)
		return 0
	}
	fmt.Fprintf(stdout, "Handoff of attempt %s of team %s, pushed %s:\n\n%s\n", h.AttemptID, h.TeamID,
		h.UpdatedAt.UTC().Format(time.RFC3339), strings.TrimRight(forge.CleanText(h.Handoff), "\n"))
	return 0
}
