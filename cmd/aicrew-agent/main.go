// Command aicrew-agent is an agent's aicrew client. It holds no store and
// needs no operator authority: it proves the agent's aimem identity and
// keeps the agent's team session through aicrewd's client session API.
//
//	aicrew-agent session start  -home DIR   enter or resume, then keep the session until interrupted
//	aicrew-agent session status -home DIR   show the recorded session, without secrets
//	aicrew-agent session leave  -home DIR   prove afresh, resume and leave
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

const usage = `usage: aicrew-agent session start|status|leave -home DIR`

func main() {
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
		return finish(log, leave(ctx, e, cfg))
	}
	if err := e.Start(ctx); err != nil {
		return finish(log, err)
	}
	fmt.Fprintf(stdout, "AIMEM_TEAM_SESSION=%s\n", e.AimemFile())
	return finish(log, e.Run(ctx))
}

// leave proves afresh and resumes the recorded session, which fences any
// client still holding it, then leaves.
func leave(ctx context.Context, e *agent.Engine, cfg agent.Config) error {
	if _, ok, err := agent.LoadState(cfg.Home); err != nil {
		return err
	} else if !ok {
		return errors.New("no team session is recorded in this agent home")
	}
	if err := e.Start(ctx); err != nil {
		return err
	}
	return e.Leave(ctx)
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
