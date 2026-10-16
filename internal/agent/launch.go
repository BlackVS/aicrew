package agent

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// SessionEnv is the variable through which aimem's MCP server in the
// client's process tree finds this conversation's session file.
const SessionEnv = "AIMEM_TEAM_SESSION"

// termGrace is how long a client has to exit after the launcher forwards
// SIGTERM, before it is killed.
const termGrace = 10 * time.Second

// Client is the agent client the launcher runs: Claude Code or OpenCode.
type Client struct {
	Path string // the executable
	Args []string
}

// Stdio is the client's standard streams; normally the launcher's own.
type Stdio struct {
	In       io.Reader
	Out, Err io.Writer
}

// ClientArgs are the arguments `run` starts client with in home (3a60).
// Claude Code gets the first instruction as its initial prompt, ahead of the
// operator's own arguments, unless noStart: the managed /crew-start command
// when the home holds it (ef73), else the instruction's text. OpenCode's
// first argument is a project directory, so it gets none (1af8). extra is
// passed through unchanged.
func ClientArgs(client, home string, noStart bool, extra []string) []string {
	if client != "claude" || noStart {
		return extra
	}
	first := FirstInstruction()
	if HasCommand(home, "crew-start") {
		first = "/crew-start"
	}
	return append([]string{first}, extra...)
}

// ScopedEnv returns env with SessionEnv set to path, replacing any value it
// already had. Only the client's process tree receives it: nothing else in
// the environment, the user's shell or the host changes. No secret is ever
// added.
func ScopedEnv(env []string, path string) []string { return withEnv(env, SessionEnv, path) }

// InsideLauncher reports whether getenv is the environment of a client that
// `aicrew-agent run` started: the launcher sets both SessionEnv and HomeEnv
// there, and holds that home's team session itself.
func InsideLauncher(getenv func(string) string) bool {
	return getenv(SessionEnv) != "" && getenv(HomeEnv) != ""
}

// clientEnviron is the environment of the client a launcher starts for
// home, `run`'s or each `serve` turn's: this process's environment with the
// session file, the home's aimem installation and HomeEnv, and this
// aicrew-agent's directory first on PATH.
func clientEnviron(e *Engine, home string) []string {
	return withSelfOnPath(withEnv(HomeAimemEnv(ScopedEnv(os.Environ(), e.AimemFile()), home), HomeEnv, home))
}

// StopHookBlockCapEnv is Claude Code's limit on consecutive stops a Stop
// hook blocks: past it, Claude Code overrides the hook and ends the turn
// (9 by default), which ended the keep-alive and left the member idle in
// the pilot (2026-10-12). StopHookBlockCap is what `run` sets: the
// keep-alive's own bound is the home's idle_hours (WakeSettings).
const (
	StopHookBlockCapEnv = "CLAUDE_CODE_STOP_HOOK_BLOCK_CAP"
	StopHookBlockCap    = 100000
)

// withStopHookCap returns env with StopHookBlockCapEnv at StopHookBlockCap,
// unless it holds a larger number already. An interim measure until every
// home runs the turn loop (`serve`), which has no Stop hook.
func withStopHookCap(env []string) []string {
	for _, kv := range env {
		n, v, _ := strings.Cut(kv, "=")
		if n == StopHookBlockCapEnv || (runtime.GOOS == "windows" && strings.EqualFold(n, StopHookBlockCapEnv)) {
			if cur, err := strconv.Atoi(strings.TrimSpace(v)); err == nil && cur >= StopHookBlockCap {
				return env
			}
		}
	}
	return withEnv(env, StopHookBlockCapEnv, strconv.Itoa(StopHookBlockCap))
}

// withSelfOnPath returns env with the directory of this aicrew-agent first
// on PATH (pilot finding 2026-10-12): the guidance has a member run
// `aicrew-agent` by name, and the user's PATH need not hold this one, or
// holds another. The rest of PATH is kept as it was. On Windows the
// variable keeps the name it had (Path or PATH), and one remains.
func withSelfOnPath(env []string) []string {
	dir := filepath.Dir(selfExecutable())
	if !filepath.IsAbs(dir) {
		return env
	}
	name, cur := "PATH", ""
	for _, kv := range env {
		n, v, _ := strings.Cut(kv, "=")
		if n == "PATH" || (runtime.GOOS == "windows" && strings.EqualFold(n, "PATH")) {
			name, cur = n, v
		}
	}
	if list := filepath.SplitList(cur); len(list) > 0 && samePathDir(list[0], dir) {
		return env
	}
	v := dir
	if cur != "" {
		v += string(os.PathListSeparator) + cur
	}
	return withEnv(env, name, v)
}

// samePathDir reports whether two PATH entries name one directory as
// written, in any case on Windows.
func samePathDir(a, b string) bool {
	a, b = filepath.Clean(a), filepath.Clean(b)
	return a == b || (runtime.GOOS == "windows" && strings.EqualFold(a, b))
}

// withEnv returns env with name set to value, replacing any value it had.
func withEnv(env []string, name, value string) []string {
	return append(withoutEnv(env, name), name+"="+value)
}

// withoutEnv returns env without name.
func withoutEnv(env []string, name string) []string {
	out := make([]string, 0, len(env)+1)
	for _, kv := range env {
		n, _, _ := strings.Cut(kv, "=")
		if n == name || (runtime.GOOS == "windows" && strings.EqualFold(n, name)) {
			continue
		}
		out = append(out, kv)
	}
	return out
}

// serveSteps serves the step channel for the client and finishes the steps
// a previous run left recorded. A launcher that cannot serve it still runs
// the client, whose `aicrew-agent step` then finds no launcher.
func serveSteps(e *Engine) *StepServer {
	crew, ok := e.Crew.(interface {
		StepAPI
		LocalAPI
	})
	if !ok || e.Reservations == nil {
		return nil
	}
	s, err := ServeSteps(e.Cfg.Home, NewDriver(e.Cfg.Home, crew, e.Reservations, e, e.Log), crew, e.Log)
	if err != nil {
		e.Log.Warn("the step channel is not served", "error", err.Error())
		return nil
	}
	s.RecoverPending()
	if home, err := filepath.Abs(e.Cfg.Home); err == nil {
		s.env = clientEnviron(e, home)
	}
	// A coordinator reconciles its open attempts first: the list is
	// recorded before the client starts, so its first turn reads it.
	if o, ok, err := e.RecordOpenAttempts(context.Background()); err != nil {
		e.Log.Warn("the open attempts were not read", "error", err.Error())
	} else if ok {
		e.Log.Info("open attempts recorded", "file", OpenAttemptsFile, "attempts", len(o.Attempts))
	}
	// The team's projects are recorded before the first turn, which takes
	// a worker's toolchains from them (pilot finding #18). A failure is
	// logged and leaves the last record.
	ctx, cancel := context.WithTimeout(context.Background(), teamRecordBudget)
	if err := s.RecordTeam(ctx); err != nil {
		e.Log.Warn("the team's projects were not recorded", "error", err.Error())
	}
	cancel()
	// The session's start reports the home's capabilities (3.6); a failure
	// is logged and never holds the session back. The report belongs to
	// the step server: its Close ends the report and waits for it.
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		ctx, cancel := context.WithTimeout(s.ctx, capabilityBudget)
		defer cancel()
		if _, err := s.ReportCapabilities(ctx); err != nil {
			e.Log.Warn("the capabilities were not reported", "error", err.Error())
		}
	}()
	return s
}

// capabilityBudget bounds the capability report at a session's start.
const capabilityBudget = 2 * time.Minute

// teamRecordBudget bounds the read of the team's projects at a session's
// start, which the session waits for.
const teamRecordBudget = 30 * time.Second

// RunClient starts the agent's team session, runs the client as a child in
// the agent home with the session bound to it, keeps the session alive
// while the child runs, and leaves once the child exits. It returns the
// child's exit code and the session's outcome: nil, a *WorkOutstanding when
// the session was kept for open work, or an error.
//
// Until the client starts, an interrupt or SIGTERM stops the startup and
// the client never starts (ErrStopped); a stop that races the client's
// creation stops it at once. Once it runs, Ctrl-C reaches the
// child through the terminal or console, which delivers it to the whole
// foreground process group or console; the launcher does not exit on it but
// waits for the child. SIGTERM sent to the launcher is forwarded to the
// child, which is killed if it has not exited after termGrace. If the
// launcher itself dies, the child is stopped with it on Linux (parent-death
// signal) and Windows (kill-on-close job object). On macOS nothing can stop
// it, and the handle is the guarantee: no longer refreshed, it gives the
// child no team access within 15 minutes, and the next run resumes the
// session under a new generation.
func RunClient(ctx context.Context, e *Engine, c Client, stdio Stdio, signals <-chan os.Signal) (int, error) {
	if err := start(ctx, e, signals); err != nil {
		return 0, err
	}
	if steps := serveSteps(e); steps != nil {
		defer steps.Close()
	}
	home, err := filepath.Abs(e.Cfg.Home)
	if err != nil {
		home = e.Cfg.Home
	}
	cmd := exec.Command(c.Path, c.Args...)
	cmd.Dir = e.Cfg.Home
	cmd.Env = withStopHookCap(clientEnviron(e, home))
	cmd.Stdin, cmd.Stdout, cmd.Stderr = stdio.In, stdio.Out, stdio.Err
	bindToLauncher(cmd)
	// A stop that arrived after the startup ended keeps the client from
	// starting.
	beforeLaunch()
	if stopWaiting(signals) {
		return 0, leaveStopped(ctx, e)
	}
	if err := cmd.Start(); err != nil {
		// Nothing ran: the session is left as any stop would leave it.
		return 0, errors.Join(fmt.Errorf("start the client: %w", err), e.Leave(context.WithoutCancel(ctx)))
	}
	release, err := afterStart(cmd)
	if err != nil {
		e.Log.Warn("the client will not be stopped if the launcher is killed", "error", err.Error())
	}
	defer release()
	// No check before cmd.Start can be atomic with it: a stop that reached
	// the launcher while the client was being created is seen here, and the
	// client is stopped at once, before the running policy (which ignores
	// an interrupt) applies. Every stop received before cmd.Start returned
	// is in the channel by now.
	afterLaunch()
	if stopWaiting(signals) {
		e.Log.Info("stop requested as the client started; stopping it")
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		return 0, leaveStopped(ctx, e)
	}

	runCtx, stopRun := context.WithCancel(context.WithoutCancel(ctx))
	defer stopRun()
	ran := make(chan error, 1)
	go func() { ran <- e.Run(runCtx) }()
	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()

	var (
		runErr  error
		runDone bool
		kill    <-chan time.Time
	)
	for {
		select {
		case sig := <-signals:
			if sig == syscall.SIGTERM && runtime.GOOS != "windows" {
				e.Log.Info("forwarding SIGTERM to the client")
				_ = cmd.Process.Signal(syscall.SIGTERM)
				if kill == nil {
					kill = time.After(termGrace)
				}
			}
		case <-kill:
			e.Log.Warn("the client did not exit after SIGTERM; killing it")
			_ = cmd.Process.Kill()
		case err := <-ran:
			// The session ended while the client runs (an operator stop, a
			// removal). aimem already refuses the client's team tools; the
			// client keeps running until it exits.
			runErr, runDone = err, true
			if err != nil {
				e.Log.Warn("the team session is no longer held", "error", err.Error())
			}
		case err := <-exited:
			code := exitCode(err)
			if runDone {
				return code, runErr
			}
			stopRun()
			return code, <-ran
		}
	}
}

// ErrStopped reports a stop requested before the client started.
var ErrStopped = errors.New("stopped before the client started")

// start starts the session unless a stop (an interrupt or SIGTERM) comes
// first. A stop cancels the startup and its retries, and a session the
// startup had already entered is left; the client is then never started.
func start(ctx context.Context, e *Engine, signals <-chan os.Signal) error {
	startCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- e.Start(startCtx) }()
	var (
		err     error
		stopped bool
	)
	for finished := false; !finished; {
		select {
		case <-signals:
			if !stopped {
				e.Log.Info("stop requested; ending the startup")
			}
			stopped = true
			cancel()
		case err = <-done:
			finished = true
		}
	}
	if !stopped {
		return err
	}
	return leaveStopped(ctx, e)
}

// stopWaiting reports whether a stop has arrived and not been handled.
func stopWaiting(signals <-chan os.Signal) bool {
	select {
	case <-signals:
		return true
	default:
		return false
	}
}

// leaveStopped leaves the session a stopped startup had entered, if any.
func leaveStopped(ctx context.Context, e *Engine) error {
	if e.SessionID() == "" {
		return ErrStopped
	}
	return errors.Join(ErrStopped, e.Leave(context.WithoutCancel(ctx)))
}

// beforeLaunch and afterLaunch run just before the client is started and
// just after; tests use them to deliver a stop at those points.
var beforeLaunch, afterLaunch = func() {}, func() {}

// exitCode is the client's exit status as a shell reports it: a client
// ended by a signal gives 128 plus the signal's number.
func exitCode(err error) int {
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		if ws, ok := exit.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
			return 128 + int(ws.Signal())
		}
		return exit.ExitCode()
	}
	if err != nil {
		return 1
	}
	return 0
}
