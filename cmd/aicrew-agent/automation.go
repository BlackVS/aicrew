package main

import (
	"context"
	"fmt"
	"io"

	"github.com/BlackVS/aicrew/internal/agent"
)

// automationCmd is `aicrew-agent automation run`: the child process the
// launcher starts for one workflow script (docs/DESIGN-TURNS.md, 10.3).
// The launcher writes the request to its standard input; the process takes
// no flag, reads no file and no environment, and answers on its standard
// output.
func automationCmd(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	if len(args) != 1 || args[0] != "run" {
		fmt.Fprintln(stderr, usage)
		return exitUsage
	}
	return agent.AutomationChild(context.Background(), stdin, stdout, stderr)
}
