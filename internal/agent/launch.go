package agent

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"runtime"
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

// ScopedEnv returns env with SessionEnv set to path, replacing any value it
// already had. Only the client's process tree receives it: nothing else in
// the environment, the user's shell or the host changes. No secret is ever
// added.
func ScopedEnv(env []string, path string) []string {
	out := make([]string, 0, len(env)+1)
	for _, kv := range env {
		name, _, _ := strings.Cut(kv, "=")
		if name == SessionEnv || (runtime.GOOS == "windows" && strings.EqualFold(name, SessionEnv)) {
			continue
		}
		out = append(out, kv)
	}
	return append(out, SessionEnv+"="+path)
}

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
	cmd := exec.Command(c.Path, c.Args...)
	cmd.Dir = e.Cfg.Home
	cmd.Env = ScopedEnv(os.Environ(), e.AimemFile())
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
