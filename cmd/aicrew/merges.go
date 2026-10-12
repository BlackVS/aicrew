package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/url"

	"github.com/BlackVS/aicrew/internal/opapi"
)

// aicrew merges: the pull requests that wait for the human merge
// (docs/DESIGN-CONTROL-PLANE.md, section 4.2; increment C3), through
// aicrewd's operator API, with the operator credential or an architect
// credential in the token file. A team is named by its ID, as for the
// escalations: resolving a name reads the team routes, which an architect
// credential does not reach.

const mergesUsage = `usage:
  aicrew merges [--team TEAM]
` + connUsage

// runMerges is aicrew merges: 0 on success, 1 on a failure, 2 on a usage
// error.
func runMerges(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("aicrew merges", flag.ContinueOnError)
	fs.SetOutput(stderr)
	cn := addConn(fs)
	team := fs.String("team", "", "the team's ID; every team when absent")
	if err := fs.Parse(args); err != nil || fs.NArg() != 0 {
		fmt.Fprint(stderr, mergesUsage)
		return 2
	}
	cl, ok := cn.connect(stderr)
	if !ok {
		return 1
	}
	q := url.Values{}
	if *team != "" {
		q.Set("team", *team)
	}
	var list []opapi.PendingMerge
	if err := cl.Get(ctx, opapi.MergesPath, q, &list); err != nil {
		return failed(stderr, err)
	}
	out := json.NewEncoder(stdout)
	out.SetIndent("", "  ")
	_ = out.Encode(list)
	return 0
}
