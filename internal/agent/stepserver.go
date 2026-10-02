package agent

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/BlackVS/aicrew/internal/privatefile"
)

// The step channel (crew-execution b2b, D-b2-1). The launcher holds the
// session token; the model's client asks it for steps through a local
// socket in the agent home's private state directory, with `aicrew-agent
// step …`. The token never crosses the socket: the launcher runs each step
// and answers with its outcome.
//
// The socket's privacy rests on its directory. On Unix the directory is mode
// 0700 and the socket 0600. On Windows the directory has a protected DACL for
// the agent home's owner (with SYSTEM and Administrators), which the socket
// inherits. The launcher refuses to serve from a directory it cannot make
// and verify private.

// HomeEnv names the agent home in the client's environment, so that `step`
// finds its launcher.
const HomeEnv = "AICREW_AGENT_HOME"

// maxStepRequest bounds one request on the socket.
const maxStepRequest = 256 << 10

// stepTimeout bounds one step's service: a begin, aimem's commands and a
// settle.
const stepTimeout = 10 * time.Minute

// StepSocket is the step channel's socket in an agent home.
func StepSocket(home string) string { return filepath.Join(home, "state", "step.sock") }

// StepCall is one request on the step channel.
type StepCall struct {
	Version   int             `json:"version"`
	Op        string          `json:"op"`
	AttemptID string          `json:"attempt_id,omitempty"`
	TaskID    string          `json:"task_id,omitempty"`
	Body      json.RawMessage `json:"body,omitempty"`
}

// Where an answered call stands.
const (
	StepDone    = "done"    // committed, or answered
	StepPending = "pending" // recorded, not settled: recover it later
	StepRefused = "refused" // refused, or settled as not committed
	StepFailed  = "failed"  // not answered: aicrewd, aimem or the channel failed
)

// StepAnswer is the channel's one answer: a result, or a refusal.
type StepAnswer struct {
	OK         bool            `json:"ok"`
	Status     string          `json:"status"`
	Result     json.RawMessage `json:"result,omitempty"`
	Error      *StepError      `json:"error,omitempty"`
	NextAction string          `json:"next_action,omitempty"`
}

// StepError is a refusal: aicrewd's, or the channel's own.
type StepError struct {
	Code       string `json:"code"`
	Message    string `json:"message"`
	Retryable  bool   `json:"retryable"`
	NextAction string `json:"next_action,omitempty"`
}

// LocalAPI is the part of aicrewd's session API for the local steps: no
// reservation, no proof.
type LocalAPI interface {
	LocalStep(ctx context.Context, key, token, path string, body []byte) (json.RawMessage, error)
	Inbox(ctx context.Context, token string, limit int) (json.RawMessage, error)
}

// stepOps are the reservation steps: the begin route and where the aimem
// task comes from.
var stepOps = map[string]struct {
	action   string // the route's action; "" for the attempts root
	root     string // for a step that creates its attempt: its path under attempts
	fromBody bool   // the task is the body's task.task_id
}{
	"offer":    {root: "", fromBody: true},
	"claim":    {root: "/claim", fromBody: true},
	"accept":   {action: "accept"},
	"withdraw": {action: "withdraw"},
	"work":     {action: "work"},
	"release":  {action: "release"},
	"finalize": {action: "finalize"},
}

// localOps are the local steps: no reservation.
var localOps = map[string]bool{"decline": true, "review": true, "stop": true, "confirm-stop": true, "confirm-delivery": true}

// StepServer serves the step channel for one launcher.
type StepServer struct {
	home   string
	driver *Driver
	local  LocalAPI
	log    *slog.Logger
	ln     *net.UnixListener
	// sock is the socket file this server created. Close removes the path
	// only while it is still that file: a newer launcher of the same home
	// may have replaced it, and its socket must survive this one's exit.
	sock os.FileInfo
	mu   sync.Mutex // one step at a time
	wg   sync.WaitGroup
	// ctx ends at Close: a step it interrupts stays recorded.
	ctx    context.Context
	cancel context.CancelFunc
}

// ServeSteps makes the agent home's state directory private, verifies it,
// and serves the step channel on its socket until Close.
func ServeSteps(home string, d *Driver, local LocalAPI, log *slog.Logger) (*StepServer, error) {
	dir := filepath.Dir(StepSocket(home))
	if err := privatefile.MakeDir(dir); err != nil {
		return nil, fmt.Errorf("make the step channel's directory private: %w", err)
	}
	if err := privatefile.CheckDir(dir); err != nil {
		return nil, fmt.Errorf("the step channel's directory is not private: %w", err)
	}
	path := StepSocket(home)
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("remove a stale step socket: %w", err)
	}
	ln, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		return nil, fmt.Errorf("serve the step channel: %w", err)
	}
	// The listener would unlink the path by name when it closes, whoever
	// owns it by then; Close removes it only while it is still ours.
	ln.SetUnlinkOnClose(false)
	sock, err := os.Lstat(path)
	if err == nil && runtime.GOOS != "windows" {
		err = os.Chmod(path, 0o600)
	}
	if err != nil {
		ln.Close()
		os.Remove(path)
		return nil, err
	}
	s := &StepServer{home: home, driver: d, local: local, log: log, ln: ln, sock: sock}
	s.ctx, s.cancel = context.WithCancel(context.Background())
	s.wg.Add(1)
	go s.accept()
	return s, nil
}

// RecoverPending finishes, in the background, the steps a previous run left
// recorded; the channel's steps wait for it.
func (s *StepServer) RecoverPending() {
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		s.mu.Lock()
		defer s.mu.Unlock()
		ctx, cancel := context.WithTimeout(s.ctx, stepTimeout)
		defer cancel()
		if _, err := s.driver.Recover(ctx); err != nil {
			s.log.Warn("a recorded step is not finished; `aicrew-agent step recover` retries it", "error", err.Error())
		}
	}()
}

// Close stops serving and removes its socket, unless a newer launcher of
// the same home has replaced it since. A step in progress is interrupted
// and stays recorded.
func (s *StepServer) Close() error {
	err := s.ln.Close()
	s.cancel()
	s.wg.Wait()
	if cur, lerr := os.Lstat(StepSocket(s.home)); lerr == nil && os.SameFile(cur, s.sock) {
		os.Remove(StepSocket(s.home))
	}
	return err
}

func (s *StepServer) accept() {
	defer s.wg.Done()
	for {
		conn, err := s.ln.Accept()
		if err != nil {
			return
		}
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			s.serve(conn)
		}()
	}
}

// serve answers one request on one connection.
func (s *StepServer) serve(conn net.Conn) {
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(stepTimeout))
	var call StepCall
	line, err := bufio.NewReaderSize(io.LimitReader(conn, maxStepRequest+1), 64<<10).ReadBytes('\n')
	var ans StepAnswer
	switch {
	case (err != nil && !errors.Is(err, io.EOF)) || len(line) > maxStepRequest:
		ans = refuse("invalid_request", "The request could not be read.", "Send one JSON request line.")
	case json.Unmarshal(line, &call) != nil || call.Version != 1:
		ans = refuse("invalid_request", "The request is not a version 1 step call.", "Use aicrew-agent step.")
	default:
		s.mu.Lock()
		ctx, cancel := context.WithTimeout(s.ctx, stepTimeout)
		ans = s.handle(ctx, call)
		cancel()
		s.mu.Unlock()
	}
	b, _ := json.Marshal(ans)
	conn.Write(append(b, '\n'))
}

func refuse(code, message, next string) StepAnswer {
	return StepAnswer{Status: StepRefused, Error: &StepError{Code: code, Message: message, NextAction: next}}
}

func done(v any) StepAnswer {
	b, _ := json.Marshal(v)
	return StepAnswer{OK: true, Status: StepDone, Result: b}
}

// answerErr turns an error into the channel's answer: aicrewd's refusal as
// it is, anything else as unavailable.
func answerErr(err error) StepAnswer {
	var ref *Refusal
	if errors.As(err, &ref) {
		return StepAnswer{Status: StepRefused,
			Error: &StepError{Code: ref.Code, Message: ref.Message, Retryable: ref.Retryable, NextAction: ref.NextAction}}
	}
	return StepAnswer{Status: StepFailed, Error: &StepError{Code: "unavailable", Message: err.Error(), Retryable: true,
		NextAction: "Retry the step later; a recorded step is finished by `aicrew-agent step recover`."}}
}

func (s *StepServer) handle(ctx context.Context, call StepCall) StepAnswer {
	switch {
	case call.Op == "recover":
		results, err := s.driver.Recover(ctx)
		return recovered(results, err)
	case call.Op == "pending":
		pending, err := s.driver.Pending()
		if err != nil {
			return answerErr(err)
		}
		return done(pending)
	case call.Op == "inbox":
		// The member's inbox (pilot G1): {"limit": N}, or no body.
		var in struct {
			Limit int `json:"limit"`
		}
		if len(call.Body) > 0 && json.Unmarshal(call.Body, &in) != nil {
			return refuse("invalid_request", "The inbox body is not {\"limit\": N}.", "Pass -limit.")
		}
		if in.Limit == 0 {
			in.Limit = 20
		}
		out, err := s.local.Inbox(ctx, s.driver.Session.stepToken(), in.Limit)
		if err != nil {
			return answerErr(err)
		}
		return StepAnswer{OK: true, Status: StepDone, Result: out}
	case call.Op == "ack":
		// Acknowledge delivered messages: {"ids": [...]}.
		var in struct {
			IDs []string `json:"ids"`
		}
		if json.Unmarshal(call.Body, &in) != nil || len(in.IDs) == 0 {
			return refuse("invalid_request", "An acknowledgement names the messages' IDs.", "Pass -ack ID,ID.")
		}
		out, err := s.local.LocalStep(ctx, newKey("ack"), s.driver.Session.stepToken(), inboxPath+"/ack", call.Body)
		if err != nil {
			return answerErr(err)
		}
		return StepAnswer{OK: true, Status: StepDone, Result: out}
	case localOps[call.Op]:
		if !attemptIDShape(call.AttemptID) {
			return refuse("invalid_request", "A local step needs the attempt's ID.", "Pass -attempt.")
		}
		out, err := s.local.LocalStep(ctx, newKey(call.Op), s.driver.Session.stepToken(),
			attemptsPath+"/"+url.PathEscape(call.AttemptID)+"/"+call.Op, bodyOrEmpty(call.Body))
		if err != nil {
			return answerErr(err)
		}
		return StepAnswer{OK: true, Status: StepDone, Result: out}
	}
	op, ok := stepOps[call.Op]
	if !ok {
		return refuse("invalid_request", "Unknown step "+call.Op+".", "Use offer, accept, withdraw, claim, work, release, finalize, "+
			"decline, review, stop, confirm-stop, confirm-delivery, recover, pending, inbox or ack.")
	}
	req := StepRequest{Body: bodyOrEmpty(call.Body), TaskID: call.TaskID}
	if op.fromBody {
		req.Path = attemptsPath + op.root
		var b struct {
			Task struct {
				TaskID string `json:"task_id"`
			} `json:"task"`
		}
		if json.Unmarshal(req.Body, &b) != nil || b.Task.TaskID == "" {
			return refuse("invalid_request", "The body names no task.", "Pass the step's body with its task.")
		}
		req.TaskID = b.Task.TaskID
		if call.Op == "offer" {
			// Before anything is recorded or begun (1aad G1).
			evidence, err := s.driver.Dependencies(ctx, req.TaskID)
			var open *DependenciesOpen
			if errors.As(err, &open) {
				return refuse("dependencies_open", "The task has dependencies that are not DONE: "+strings.Join(open.Open, ", ")+".",
					"Offer the task once its dependencies are DONE.")
			}
			if err != nil {
				return answerErr(err)
			}
			if req.Body, err = withDependencies(req.Body, evidence); err != nil {
				return refuse("invalid_request", "The offer's body is not a JSON object.", "Pass the offer's body.")
			}
		}
	} else {
		if !attemptIDShape(call.AttemptID) || call.TaskID == "" {
			return refuse("invalid_request", "This step needs the attempt's ID and the aimem task's ID.", "Pass -attempt and -task.")
		}
		req.Path = attemptsPath + "/" + url.PathEscape(call.AttemptID) + "/" + op.action
	}
	res, err := s.driver.Run(ctx, req)
	if err != nil {
		return answerErr(err)
	}
	ans := done(res)
	switch {
	case res.Settled && res.Outcome == "committed":
	case res.Settled:
		ans.OK, ans.Status = false, StepRefused
		ans.NextAction = "The step did not commit. Begin it again, or reconcile the attempt."
	default:
		ans.Status = StepPending
		ans.NextAction = "The step is pending: run `aicrew-agent step recover` after " + res.RetryAfter.String() + "."
	}
	return ans
}

func bodyOrEmpty(b json.RawMessage) json.RawMessage {
	if len(b) == 0 {
		return json.RawMessage("{}")
	}
	return b
}

func attemptIDShape(id string) bool {
	return id != "" && len(id) <= 128 && !strings.ContainsAny(id, "/\\ ?#%")
}

// ErrNoLauncher reports that no launcher serves the agent home's step
// channel.
var ErrNoLauncher = errors.New("no aicrew-agent launcher serves this agent home; start the client with `aicrew-agent run`")

// CallStep sends one call to the launcher of home and returns its answer.
func CallStep(ctx context.Context, home string, call StepCall) (StepAnswer, error) {
	call.Version = 1
	var d net.Dialer
	conn, err := d.DialContext(ctx, "unix", StepSocket(home))
	if err != nil {
		return StepAnswer{}, ErrNoLauncher
	}
	defer conn.Close()
	b, _ := json.Marshal(call)
	if _, err := conn.Write(append(b, '\n')); err != nil {
		return StepAnswer{}, fmt.Errorf("the step channel: %w", err)
	}
	var ans StepAnswer
	if err := json.NewDecoder(io.LimitReader(conn, maxStepRequest)).Decode(&ans); err != nil {
		return StepAnswer{}, fmt.Errorf("the step channel's answer: %w", err)
	}
	return ans, nil
}

// recovered answers a recovery by its worst step: failed if one failed,
// refused if one settled as not committed, pending if one is still pending,
// and done only when every step committed.
func recovered(results []StepResult, err error) StepAnswer {
	ans := done(results)
	if err != nil {
		ans.OK, ans.Status, ans.Error = false, StepFailed, answerErr(err).Error
		return ans
	}
	for _, r := range results {
		switch {
		case r.Settled && r.Outcome != "committed":
			ans.OK, ans.Status = false, StepRefused
			ans.NextAction = "A recovered step did not commit. Begin it again, or reconcile the attempt."
		case !r.Settled && ans.Status == StepDone:
			ans.Status = StepPending
			ans.NextAction = "A recovered step is still pending: run `aicrew-agent step recover` later."
		}
	}
	return ans
}
