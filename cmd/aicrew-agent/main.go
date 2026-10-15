// Command aicrew-agent is an agent's aicrew client. It holds no store and
// needs no operator authority: it proves the agent's aimem identity and
// keeps the agent's team session through aicrewd's client session API.
//
//	aicrew-agent run --client claude|opencode --home DIR [--no-start] [-- ARGS]
//	                                        run the client in the team session, then leave
//	aicrew-agent session start  --home DIR   enter or resume, then keep the session until interrupted
//	aicrew-agent session status [--home DIR] show the recorded session, the role and the team's projects, without secrets
//	aicrew-agent session leave  --home DIR   prove afresh, resume and leave
//	aicrew-agent service install|uninstall|start|stop|restart|status (--home DIR | --all)
//	                                        the turn loop as this account's service
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
	"path/filepath"
	"syscall"

	"github.com/BlackVS/aicrew/internal/agent"
	"github.com/BlackVS/aicrew/internal/service"
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
       aicrew-agent serve --home DIR [--log FILE]   (the turn loop: headless Claude Code turns; join --wake turns first)
       aicrew-agent service install|uninstall|start|stop|restart|status (--home DIR | --all) [--json]
                                        (serve as this account's service; run "aicrew-agent service" for its usage)
       aicrew-agent step OP [--home DIR] [--attempt ID] [--task ID] [--body JSON|-]
       aicrew-agent inbox [--home DIR] [--limit N] [--ack ID,ID...] [--json]
       aicrew-agent escalate [--home DIR] --body FILE|-   (the coordinator, through the launcher)
       aicrew-agent attempts [--home DIR] [--json]   (the coordinator: the team's open attempts)
       aicrew-agent escalations [--home DIR] [--json]   (the open escalations, before raising another)
       aicrew-agent wait-inbox [--home DIR]   (the Claude Code Stop hook join installs)
       aicrew-agent join --label LABEL [--home DIR] --url URL --tls-trust-mode M --tls-trust-value V --aimem-hub NAME --client C
       aicrew-agent check (--home DIR | --label LABEL) [--client C] [--json]
       aicrew-agent clone [--home DIR] --repository HTTPS_URL --attempt ID --base COMMIT --branch NAME [--json]
       aicrew-agent digest [--home DIR] --repository HTTPS_URL --commit COMMIT --manifest PATH [--json]
       aicrew-agent forge pr open|show|label|comment|ready ...   (pull requests with the home's credential; run "aicrew-agent forge" for its usage)
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
	if agent.ReadWakeSettings(cfg.Home).Mode == agent.WakeTurns {
		log.Error("this home is in turns mode: its settings have no Stop hook, so an interactive client would " +
			"never be woken. Run `aicrew-agent serve --home " + cfg.Home + "`, or make it interactive again with " +
			"`aicrew-agent join --home " + cfg.Home + " --wake stop-hook`")
		return exitUsage
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

// serveHome runs the home's turn loop (aicrew-agent serve) until a stop or
// the session's end.
func serveHome(ctx context.Context, args []string, stderr io.Writer, sigs <-chan os.Signal, build engineFor,
	getenv func(string) string) int {
	if refusedInsideLauncher("serve", getenv, stderr) {
		return exitUsage
	}
	fs := flag.NewFlagSet("aicrew-agent serve", flag.ContinueOnError)
	fs.SetOutput(stderr)
	home := fs.String("home", "", "the agent home directory")
	logFile := fs.String("log", "", "also append the log to this file (the service's HOME/logs/serve.log)")
	if err := fs.Parse(args); err != nil || *home == "" || fs.NArg() != 0 {
		fmt.Fprintln(stderr, usage)
		return exitUsage
	}
	out := stderr
	if *logFile != "" {
		f, err := openLog(*logFile)
		if err != nil {
			fmt.Fprintln(stderr, "aicrew-agent serve: the log file:", err)
			return exitFailed
		}
		defer f.Close()
		out = io.MultiWriter(stderr, f)
	}
	log := slog.New(slog.NewTextHandler(out, nil))
	cfg, err := agent.LoadConfig(*home)
	if err != nil {
		log.Error("configuration refused", "error", err.Error())
		return exitFailed
	}
	path := cfg.ClientCommand
	if path == "" {
		if path, err = exec.LookPath("claude"); err != nil {
			log.Error("client not found", "client", "claude", "error", err.Error())
			return exitFailed
		}
	}
	e, err := build(cfg, log)
	if err != nil {
		log.Error("client refused", "error", err.Error())
		return exitFailed
	}
	if agent.ReadWakeSettings(cfg.Home).Mode == agent.WakeTurns {
		// One serve per home, whoever starts it: a second would fence the
		// first's session. Exit 2 keeps a service from restarting into it.
		release, err := service.HoldServe(cfg.Home)
		var running *service.ServingError
		if errors.As(err, &running) {
			log.Error(running.Error())
			return exitUsage
		}
		if err != nil {
			log.Error("the serve lock", "error", err.Error())
			return exitFailed
		}
		defer release()
	}
	err = agent.ServeHome(ctx, e, path, sigs)
	if errors.Is(err, agent.ErrNotTurnsHome) {
		log.Error(err.Error())
		return exitUsage
	}
	return finish(log, err)
}

func main() {
	if len(os.Args) > 1 && os.Args[1] == "service" {
		os.Exit(serviceCmd(context.Background(), os.Args[2:], os.Stdout, os.Stderr, os.Getenv))
	}
	if len(os.Args) > 1 && os.Args[1] == "serve" {
		sigs := make(chan os.Signal, 4)
		signal.Notify(sigs, os.Interrupt, syscall.SIGTERM)
		os.Exit(serveHome(context.Background(), os.Args[2:], os.Stderr, sigs, defaultEngine, os.Getenv))
	}
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
	if len(os.Args) > 1 && os.Args[1] == "escalate" {
		os.Exit(escalate(context.Background(), os.Args[2:], os.Stdin, os.Stdout, os.Stderr, os.Getenv))
	}
	if len(os.Args) > 1 && os.Args[1] == "escalations" {
		os.Exit(escalations(context.Background(), os.Args[2:], os.Stdout, os.Stderr, os.Getenv))
	}
	if len(os.Args) > 1 && os.Args[1] == "attempts" {
		os.Exit(attempts(context.Background(), os.Args[2:], os.Stdout, os.Stderr, os.Getenv))
	}
	if len(os.Args) > 1 && os.Args[1] == "inbox" {
		os.Exit(inbox(context.Background(), os.Args[2:], os.Stdout, os.Stderr, os.Getenv))
	}
	if len(os.Args) > 1 && os.Args[1] == "join" {
		os.Exit(join(context.Background(), os.Args[2:], agent.IsTerminal(os.Stdin), os.Stdout, os.Stderr,
			joinDeps(os.Stderr), realInstallService(os.Getenv)))
	}
	if len(os.Args) > 1 && os.Args[1] == "clone" {
		os.Exit(clone(context.Background(), os.Args[2:], os.Stdout, os.Stderr, os.Getenv))
	}
	if len(os.Args) > 1 && os.Args[1] == "forge" {
		os.Exit(forgeCmd(context.Background(), os.Args[2:], os.Stdin, os.Stdout, os.Stderr, os.Getenv))
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
	case "start":
		// A member's client is started by run; this alone holds a session
		// with no client, so it says so and names that command.
		fmt.Fprintf(stderr, "aicrew-agent session start holds this home's team session until interrupted, and starts no client.\n"+
			"To start the member's client, which holds the session itself, run instead: %s\n",
			agent.StartCommand(*home, agent.HomeClients(*home)))
	case "leave":
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
	printOpenAttempts(ctx, e, stderr)
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

// openLog opens a log file for appending, owner-only, creating its
// directory.
func openLog(path string) (*os.File, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	return os.OpenFile(path, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o600)
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
