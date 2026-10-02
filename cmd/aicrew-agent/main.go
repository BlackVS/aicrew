// Command aicrew-agent is an agent's aicrew client. It holds no store and
// needs no operator authority: it proves the agent's aimem identity and
// keeps the agent's team session through aicrewd's client session API.
//
//	aicrew-agent run -client claude|opencode -home DIR [-- ARGS]
//	                                        run the client in the team session, then leave
//	aicrew-agent session start  -home DIR   enter or resume, then keep the session until interrupted
//	aicrew-agent session status -home DIR   show the recorded session, without secrets
//	aicrew-agent session leave  -home DIR   prove afresh, resume and leave
//	aicrew-agent step OP [-home DIR] [-attempt ID] [-task ID] [-body JSON|-]
//	                                        ask the running launcher for a step
//	aicrew-agent join -label LABEL [-home DIR] -url URL -tls-trust-mode M -tls-trust-value V -aimem-hub NAME -client C
//	                                        redeem an invitation and prepare the agent home
//	aicrew-agent check (-home DIR | -label LABEL) [-client C]
//	                                        check dependencies and the client wiring
//	aicrew-agent version [-json]            report this build
//
// The configuration is the "aicrew" section of <DIR>/agent.json.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"os/signal"
	"syscall"

	"github.com/BlackVS/aicrew/internal/agent"
)

// Exit codes.
const (
	exitOK       = 0
	exitFailed   = 1
	exitUsage    = 2
	exitWorkKept = 3 // the session was kept: the member has open work
)

const usage = `usage: aicrew-agent session start|status|leave -home DIR
       aicrew-agent run -client claude|opencode -home DIR [-- CLIENT ARGS]
       aicrew-agent step OP [-home DIR] [-attempt ID] [-task ID] [-body JSON|-]
       aicrew-agent inbox [-home DIR] [-limit N] [-ack ID,ID...] [-json]
       aicrew-agent join -label LABEL [-home DIR] -url URL -tls-trust-mode M -tls-trust-value V -aimem-hub NAME -client C
       aicrew-agent check (-home DIR | -label LABEL) [-client C] [-json]
       aicrew-agent version [-json]`

// clients are the agent clients run can start, by the name -client takes.
var clients = map[string]bool{"claude": true, "opencode": true}

// runClient starts the agent's team session, runs the client in it and
// leaves when the client exits. It exits with the client's code, or with
// exitWorkKept when open work kept the session.
func runClient(ctx context.Context, args []string, stdio agent.Stdio, sigs <-chan os.Signal, build engineFor) int {
	fs := flag.NewFlagSet("aicrew-agent run", flag.ContinueOnError)
	fs.SetOutput(stdio.Err)
	name := fs.String("client", "", "the client to run: claude or opencode")
	home := fs.String("home", "", "the agent home directory")
	if err := fs.Parse(args); err != nil || *home == "" || !clients[*name] {
		fmt.Fprintln(stdio.Err, usage)
		return exitUsage
	}
	log := slog.New(slog.NewTextHandler(stdio.Err, nil))
	cfg, err := agent.LoadConfig(*home)
	if err != nil {
		log.Error("configuration refused", "error", err.Error())
		return exitFailed
	}
	path := cfg.ClientCommand
	if path == "" {
		if path, err = exec.LookPath(*name); err != nil {
			log.Error("client not found", "client", *name, "error", err.Error())
			return exitFailed
		}
	}
	e, err := build(cfg, log)
	if err != nil {
		log.Error("client refused", "error", err.Error())
		return exitFailed
	}
	code, err := agent.RunClient(ctx, e, agent.Client{Path: path, Args: fs.Args()}, stdio, sigs)
	if rc := finish(log, err); rc != exitOK {
		return rc
	}
	return code
}

func main() {
	if len(os.Args) > 1 && os.Args[1] == "run" {
		// The client handles the terminal's interrupts; the launcher only
		// watches signals, forwarding SIGTERM, and outlives the client.
		sigs := make(chan os.Signal, 4)
		signal.Notify(sigs, os.Interrupt, syscall.SIGTERM)
		os.Exit(runClient(context.Background(), os.Args[2:], agent.Stdio{In: os.Stdin, Out: os.Stdout, Err: os.Stderr},
			sigs, defaultEngine))
	}
	if len(os.Args) > 1 && os.Args[1] == "step" {
		os.Exit(step(context.Background(), os.Args[2:], os.Stdin, os.Stdout, os.Stderr, os.Getenv))
	}
	if len(os.Args) > 1 && os.Args[1] == "inbox" {
		os.Exit(inbox(context.Background(), os.Args[2:], os.Stdout, os.Stderr, os.Getenv))
	}
	if len(os.Args) > 1 && os.Args[1] == "join" {
		os.Exit(join(context.Background(), os.Args[2:], agent.IsTerminal(os.Stdin), os.Stdout, os.Stderr,
			joinDeps(os.Stderr)))
	}
	if len(os.Args) > 1 && os.Args[1] == "check" {
		os.Exit(check(context.Background(), os.Args[2:], os.Stdout, os.Stderr))
	}
	if len(os.Args) > 1 && os.Args[1] == "version" {
		os.Exit(versionCmd(os.Args[2:], os.Stdout, os.Stderr))
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	// The first interrupt starts a clean leave; a second one stops at once.
	go func() {
		<-ctx.Done()
		stop()
	}()
	code := run(ctx, os.Args[1:], os.Stdout, os.Stderr, defaultEngine)
	stop()
	os.Exit(code)
}

// engineFor builds the engine of a loaded configuration; tests replace it.
type engineFor func(cfg agent.Config, log *slog.Logger) (*agent.Engine, error)

func defaultEngine(cfg agent.Config, log *slog.Logger) (*agent.Engine, error) {
	crew, err := agent.NewCrew(cfg, nil)
	if err != nil {
		return nil, err
	}
	return agent.NewEngine(cfg, crew, agent.ExecAimem{Command: cfg.AimemCommand, Hub: cfg.AimemHub}, log), nil
}

func run(ctx context.Context, args []string, stdout, stderr io.Writer, build engineFor) int {
	if len(args) < 2 || args[0] != "session" {
		fmt.Fprintln(stderr, usage)
		return exitUsage
	}
	verb := args[1]
	fs := flag.NewFlagSet("aicrew-agent session "+verb, flag.ContinueOnError)
	fs.SetOutput(stderr)
	home := fs.String("home", "", "the agent home directory")
	if err := fs.Parse(args[2:]); err != nil || *home == "" || fs.NArg() != 0 {
		fmt.Fprintln(stderr, usage)
		return exitUsage
	}
	log := slog.New(slog.NewTextHandler(stderr, nil))
	cfg, err := agent.LoadConfig(*home)
	if err != nil {
		log.Error("configuration refused", "error", err.Error())
		return exitFailed
	}
	switch verb {
	case "status":
		return status(ctx, cfg, stdout, log)
	case "start", "leave":
	default:
		fmt.Fprintln(stderr, usage)
		return exitUsage
	}
	e, err := build(cfg, log)
	if err != nil {
		log.Error("client refused", "error", err.Error())
		return exitFailed
	}
	if verb == "leave" {
		return finish(log, e.LeaveRecorded(ctx))
	}
	if err := e.Start(ctx); err != nil {
		return finish(log, err)
	}
	fmt.Fprintf(stdout, "AIMEM_TEAM_SESSION=%s\n", e.AimemFile())
	return finish(log, e.Run(ctx))
}

func status(ctx context.Context, cfg agent.Config, stdout io.Writer, log *slog.Logger) int {
	st, ok, err := agent.LoadState(cfg.Home)
	if err != nil {
		log.Error("state refused", "error", err.Error())
		return exitFailed
	}
	if !ok {
		fmt.Fprintln(stdout, `{"session": null}`)
		return exitOK
	}
	a := agent.Serialize(agent.ExecAimem{Command: cfg.AimemCommand, Hub: cfg.AimemHub}, agent.LockDir(cfg.Home))
	path, bound, err := a.Status(ctx, st.SessionID)
	view := map[string]any{"session": st, "aimem_bound": bound}
	if err != nil {
		view["aimem_error"] = err.Error()
	} else if bound {
		view["aimem_session_file"] = path
	}
	out, _ := json.MarshalIndent(view, "", "  ")
	fmt.Fprintln(stdout, string(out))
	return exitOK
}

func finish(log *slog.Logger, err error) int {
	var kept *agent.WorkOutstanding
	switch {
	case err == nil:
		return exitOK
	case errors.As(err, &kept):
		log.Warn(kept.Error())
		return exitWorkKept
	}
	log.Error("stopped", "error", err.Error())
	return exitFailed
}
