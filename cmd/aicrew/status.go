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

// aicrew status: each team's state (docs/DESIGN-CONTROL-PLANE.md, section
// 4.2), what aicrew team state prints, for every team or one, through
// aicrewd's operator API with the operator credential or an architect
// credential in the token file. A team is named by its ID, as for the
// merges: resolving a name reads the team routes, which an architect
// credential does not reach.

const statusUsage = `usage:
  aicrew status [--team TEAM]
` + connUsage

// runStatus is aicrew status: 0 on success, 1 on a failure, 2 on a usage
// error.
func runStatus(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("aicrew status", flag.ContinueOnError)
	fs.SetOutput(stderr)
	cn := addConn(fs)
	team := fs.String("team", "", "the team's ID; every team when absent")
	if err := fs.Parse(args); err != nil || fs.NArg() != 0 {
		fmt.Fprint(stderr, statusUsage)
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
	var list []opapi.TeamState
	if err := cl.Get(ctx, opapi.TeamStatesPath, q, &list); err != nil {
		return failed(stderr, err)
	}
	out := json.NewEncoder(stdout)
	out.SetIndent("", "  ")
	_ = out.Encode(list)
	return 0
}
