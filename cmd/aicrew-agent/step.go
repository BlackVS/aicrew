package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"

	"github.com/BlackVS/aicrew/internal/agent"
	"github.com/BlackVS/aicrew/internal/forge"
)

// The step command's exit codes (its own; run's exitWorkKept does not apply).
const (
	stepDone    = 0
	stepFailed  = 1 // no launcher, or aicrewd, aimem or the channel failed
	stepRefused = 3 // refused, or settled as not committed
	stepPending = 4 // recorded, not settled: `step recover` later
)

const stepUsage = `usage: aicrew-agent step OP [--home DIR] [--attempt ID] [--task ID] [--body JSON|-]
                         [--repository CLONE_URL] [--process]
  OP: offer claim accept withdraw work release finalize
      decline review answer stop confirm-stop confirm-delivery
      recover pending
  --home defaults to $` + agent.HomeEnv + `, which the launcher gives its client.
  --repository (offer and claim): fill the body's repository object: its
  url and kind when the body names none, the default branch, and that
  branch's head as base_commit when it has none, read through the forge
  with this home's own credential for its host.
  --process (offer): fill the body's process pin and instruction_digest
  from the hub's current process selection for task.project_id, read with
  aimem process pin through this home's aimem installation. A body that
  names both keeps them; one that names only one of them is refused.`

// newForge is the forge client step reads with; tests replace it.
var newForge = func() agent.BaseAPI { return forge.NewClient() }

// readPin reads a project's process pin through the home's aimem; tests
// replace it.
var readPin = func(home string) (func(context.Context, string) (agent.HubPin, error), error) {
	cfg, err := agent.LoadConfig(home)
	if err != nil {
		return nil, err
	}
	return agent.ExecAimem{Command: cfg.AimemCommand, Hub: cfg.AimemHub, Home: cfg.Home}.ProcessPin, nil
}

// step asks the agent home's launcher for one step and prints its answer.
func step(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer, getenv func(string) string) int {
	if len(args) == 0 || args[0] == "" || args[0][0] == '-' {
		fmt.Fprintln(stderr, stepUsage)
		return exitUsage
	}
	op := args[0]
	fs := flag.NewFlagSet("aicrew-agent step "+op, flag.ContinueOnError)
	fs.SetOutput(stderr)
	home := fs.String("home", getenv(agent.HomeEnv), "the agent home directory")
	attempt := fs.String("attempt", "", "the attempt's ID")
	task := fs.String("task", "", "the aimem task's ID")
	body := fs.String("body", "", "the step's JSON body, or - to read it from stdin")
	repository := fs.String("repository", "", "offer and claim: fill the body's repository from this clone URL's forge")
	process := fs.Bool("process", false, "offer: fill the body's process pin and instruction digest from the hub")
	if err := fs.Parse(args[1:]); err != nil || *home == "" || fs.NArg() != 0 {
		fmt.Fprintln(stderr, stepUsage)
		return exitUsage
	}
	raw := []byte(*body)
	if *body == "-" {
		var err error
		if raw, err = io.ReadAll(io.LimitReader(stdin, 256<<10)); err != nil {
			fmt.Fprintln(stderr, "read the body:", err)
			return exitUsage
		}
	}
	if len(raw) > 0 && !json.Valid(raw) {
		fmt.Fprintln(stderr, "the body is not JSON")
		return exitUsage
	}
	if *repository != "" {
		if op != "offer" && op != "claim" {
			fmt.Fprintln(stderr, "--repository resolves the base of an offer or a claim only")
			return exitUsage
		}
		resolved, res, err := agent.ResolveBase(ctx, *home, newForge(), *repository, raw)
		if err != nil {
			fmt.Fprintln(stderr, "resolve the base commit:", err)
			return stepFailed
		}
		raw = resolved
		if res.Kept {
			fmt.Fprintf(stderr, "base_commit %s kept from the body\n", res.BaseCommit)
		} else {
			fmt.Fprintf(stderr, "base_commit %s: the head of %s on %s/%s\n", res.BaseCommit, res.DefaultBranch, res.Host, res.Repository)
		}
	}
	if *process {
		if op != "offer" {
			fmt.Fprintln(stderr, "--process fills an offer's process pin only")
			return exitUsage
		}
		read, err := readPin(*home)
		if err != nil {
			fmt.Fprintln(stderr, "read the process pin:", err)
			return stepFailed
		}
		filled, res, err := agent.FillProcess(ctx, raw, read)
		if err != nil {
			fmt.Fprintln(stderr, "read the process pin:", err)
			return stepFailed
		}
		raw = filled
		if res.Kept {
			fmt.Fprintln(stderr, "process and instruction_digest kept from the body")
		} else {
			fmt.Fprintf(stderr, "process %s@%s %s, instruction_digest %s: project %s's selection on the hub\n",
				res.Pin.Repository, res.Pin.Commit, res.Pin.Manifest, res.Pin.InstructionDigest, res.Project)
		}
	}
	ans, err := agent.CallStep(ctx, *home, agent.StepCall{Op: op, AttemptID: *attempt, TaskID: *task, Body: raw})
	if err != nil {
		fmt.Fprintln(stderr, err)
		if errors.Is(err, agent.ErrNoLauncher) {
			fmt.Fprintln(stderr, "no launcher serves", *home)
		}
		return stepFailed
	}
	out, _ := json.MarshalIndent(ans, "", "  ")
	fmt.Fprintln(stdout, string(out))
	return stepExit(ans.Status)
}

func stepExit(status string) int {
	switch status {
	case agent.StepDone:
		return stepDone
	case agent.StepPending:
		return stepPending
	case agent.StepRefused:
		return stepRefused
	}
	return stepFailed
}
