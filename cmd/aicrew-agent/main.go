// Command aicrew-agent is an agent's aicrew client. It holds no store and
// needs no operator authority: it proves the agent's aimem identity and
// keeps the agent's team session through aicrewd's client session API.
//
//	aicrew-agent run --client claude|opencode --home DIR [--no-start] [-- ARGS]
//	                                        run the client in the team session, then leave
//	aicrew-agent session start  --home DIR   enter or resume, then keep the session until interrupted
//	aicrew-agent session status [--home DIR] show the recorded session, the role and the team's projects, without secrets
//	aicrew-agent session leave  --home DIR   prove afresh, resume and leave
//	aicrew-agent step OP [--home DIR] [--attempt ID] [--task ID] [--body JSON|-]
//	                                        ask the running launcher for a step
//	aicrew-agent join --label LABEL [--home DIR] --url URL --tls-trust-mode M --tls-trust-value V --aimem-hub NAME --client C
//	                                        redeem an invitation and prepare the agent home
//	aicrew-agent check (--home DIR | --label LABEL) [--client C]
//	                                        check dependencies and the client wiring
//	aicrew-agent version [--json]            report this build
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

const usage = `usage: aicrew-agent session start|status|leave --home DIR   (status: --home defaults to $AICREW_AGENT_HOME)
       aicrew-agent run --client claude|opencode --home DIR [--no-start] [-- CLIENT ARGS]
       aicrew-agent step OP [--home DIR] [--attempt ID] [--task ID] [--body JSON|-]
       aicrew-agent inbox [--home DIR] [--limit N] [--ack ID,ID...] [--json]
       aicrew-agent wait-inbox [--home DIR]   (the Claude Code Stop hook join installs)
       aicrew-agent join --label LABEL [--home DIR] --url URL --tls-trust-mode M --tls-trust-value V --aimem-hub NAME --client C
       aicrew-agent check (--home DIR | --label LABEL) [--client C] [--json]
       aicrew-agent clone [--home DIR] --repository HTTPS_URL --attempt ID --base COMMIT --branch NAME [--json]
       aicrew-agent digest [--home DIR] --repository HTTPS_URL --commit COMMIT --manifest PATH [--json]
       aicrew-agent version [--json]`

// clients are the agent clients run can start, by the name --client takes.
var clients = map[string]bool{"claude": true, "opencode": true}

// runClient starts the agent's team session, runs the client in it and
// leaves when the client exits. It exits with the client's code, or with
// exitWorkKept when open work kept the session.
func runClient(ctx context.Context, args []string, stdio agent.Stdio, sigs <-chan os.Signal, build engineFor,
	getenv func(string) string) int {
	if refusedInsideLauncher("run", getenv, stdio.Err) {
		return exitUsage
	}
	fs := flag.NewFlagSet("aicrew-agent run", flag.ContinueOnError)
	fs.SetOutput(stdio.Err)
	name := fs.String("client", "", "the client to run: claude or opencode")
	home := fs.String("home", "", "the agent home directory")
	noStart := fs.Bool("no-start", false, "start Claude Code without the first instruction")
	if err := fs.Parse(args); err != nil || *home == "" || !clients[*name] {
		fmt.Fprintln(stdio.Err, usage)
		return exitUsage
	}
	log := slog.New(slog.NewTextHandler(stdio.Err, nil))
	if *name != "claude" && !*noStart {
		log.Info("this client starts without a first instruction: type it", "client", *name)
	}
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
	code, err := agent.RunClient(ctx, e, agent.Client{Path: path, Args: agent.ClientArgs(*name, cfg.Home, *noStart, fs.Args())}, stdio, sigs)
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
			sigs, defaultEngine, os.Getenv))
	}
	if len(os.Args) > 1 && os.Args[1] == "step" {
		os.Exit(step(context.Background(), os.Args[2:], os.Stdin, os.Stdout, os.Stderr, os.Getenv))
	}
	if len(os.Args) > 1 && os.Args[1] == "wait-inbox" {
		os.Exit(waitInbox(context.Background(), os.Args[2:], os.Stdin, os.Stdout, os.Stderr, os.Getenv))
	}
	if len(os.Args) > 1 && os.Args[1] == "inbox" {
		os.Exit(inbox(context.Background(), os.Args[2:], os.Stdout, os.Stderr, os.Getenv))
	}
	if len(os.Args) > 1 && os.Args[1] == "join" {
		os.Exit(join(context.Background(), os.Args[2:], agent.IsTerminal(os.Stdin), os.Stdout, os.Stderr,
			joinDeps(os.Stderr)))
	}
	if len(os.Args) > 1 && os.Args[1] == "clone" {
		os.Exit(clone(context.Background(), os.Args[2:], os.Stdout, os.Stderr, os.Getenv))
	}
	if len(os.Args) > 1 && os.Args[1] == "digest" {
		os.Exit(digest(context.Background(), os.Args[2:], os.Stdout, os.Stderr, os.Getenv))
	}
	if len(os.Args) > 1 && os.Args[1] == "git-credential" {
		os.Exit(gitCredential(os.Args[2:], os.Stdin, os.Stdout, os.Stderr, agent.IsTerminal(os.Stdout)))
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
	code := run(ctx, os.Args[1:], os.Stdout, os.Stderr, defaultEngine, os.Getenv)
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
	return agent.NewEngine(cfg, crew, agent.ExecAimem{Command: cfg.AimemCommand, Hub: cfg.AimemHub, Home: cfg.Home}, log), nil
}

func run(ctx context.Context, args []string, stdout, stderr io.Writer, build engineFor, getenv func(string) string) int {
	if len(args) < 2 || args[0] != "session" {
		fmt.Fprintln(stderr, usage)
		return exitUsage
	}
	verb := args[1]
	if (verb == "start" || verb == "leave") && refusedInsideLauncher("session "+verb, getenv, stderr) {
		return exitUsage
	}
	fs := flag.NewFlagSet("aicrew-agent session "+verb, flag.ContinueOnError)
	fs.SetOutput(stderr)
	// Inside a client run started, status finds the launcher's home itself.
	defaultHome := ""
	if verb == "status" {
		defaultHome = getenv(agent.HomeEnv)
	}
	home := fs.String("home", defaultHome, "the agent home directory")
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

// refusedInsideLauncher refuses cmd, a command that proves afresh, when it
// runs inside a client that `run` started. The launcher holds that home's
// team session, and a new proof would resume it under a new generation and
// fence the launcher's token, cutting the client off from its inbox and
// steps. It reports whether it refused; nothing has been read or changed.
func refusedInsideLauncher(cmd string, getenv func(string) string, stderr io.Writer) bool {
	if !agent.InsideLauncher(getenv) {
		return false
	}
	fmt.Fprintf(stderr, "aicrew-agent %s: refused inside a client started by aicrew-agent run: "+
		"the launcher holds this home's team session, and a new proof would fence it.\n"+
		"next: use aicrew-agent inbox and aicrew-agent step; to restart the session, "+
		"exit the client and run aicrew-agent run again from a terminal\n", cmd)
	return true
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
	a := agent.Serialize(agent.ExecAimem{Command: cfg.AimemCommand, Hub: cfg.AimemHub, Home: cfg.Home}, agent.LockDir(cfg.Home))
	path, bound, err := a.Status(ctx, st.SessionID)
	view := map[string]any{"session": st, "role": st.Role, "aimem_bound": bound, "team": nil}
	// The team's projects as the launcher read them at its session's start.
	if team, ok, terr := agent.LoadTeam(cfg.Home); terr != nil {
		view["team_error"] = terr.Error()
	} else if ok {
		view["team"] = team
	} else {
		view["team_note"] = "not recorded yet: the launcher records the team's projects shortly after its session starts"
	}
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
