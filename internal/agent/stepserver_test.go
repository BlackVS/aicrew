package agent

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/BlackVS/aicrew/internal/privatefile"
	"github.com/BlackVS/aicrew/internal/privatefile/privatefiletest"
	"github.com/BlackVS/aicrew/internal/store"
)

func (s *stepEnv) serve(t *testing.T) *StepServer {
	t.Helper()
	crew, err := NewCrew(s.cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	srv, err := ServeSteps(s.cfg.Home, s.d, crew, slog.New(slog.NewTextHandler(s.logs, nil)))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { srv.Close() })
	return srv
}

// call sends one call and keeps every answer's bytes, for the secret check.
func (s *stepEnv) call(t *testing.T, answers *[]byte, c StepCall) StepAnswer {
	t.Helper()
	ans, err := CallStep(context.Background(), s.cfg.Home, c)
	if err != nil {
		t.Fatalf("%s: %v", c.Op, err)
	}
	b, _ := json.Marshal(ans)
	*answers = append(*answers, b...)
	return ans
}

func stepResult(t *testing.T, ans StepAnswer) StepResult {
	t.Helper()
	var r StepResult
	if err := json.Unmarshal(ans.Result, &r); err != nil {
		t.Fatalf("result %s: %v", ans.Result, err)
	}
	return r
}

// A member's client drives a claim, a local confirm-stop and the release
// under the stopped fact through the launcher's channel; nothing secret
// crosses it.
func TestStepChannel(t *testing.T) {
	ctx := context.Background()
	s := setupStepsWith(t, crewOptions{role: store.RoleIndependent, steps: true, shortHome: true})
	s.serve(t)
	var answers []byte

	body, _ := json.Marshal(s.claimBody())
	ans := s.call(t, &answers, StepCall{Op: "claim", Body: body})
	r := stepResult(t, ans)
	if !ans.OK || ans.Status != StepDone || !r.Settled || r.Outcome != "committed" || r.AttemptID == "" {
		t.Fatalf("claim: %+v", ans)
	}
	id := r.AttemptID

	// A local step aicrewd refuses comes back as its refusal.
	if ans := s.call(t, &answers, StepCall{Op: "decline", AttemptID: id}); ans.OK || ans.Status != StepRefused ||
		ans.Error == nil || ans.Error.Code == "" || ans.Error.Code == "unavailable" {
		t.Fatalf("decline of a claim: %+v", ans)
	}

	if _, err := s.store.RequestStop(ctx, s.coord, "stop-1", id, store.StopRequest{SessionID: s.coordSess.ID,
		Generation: s.coordSess.Generation, Reason: "priorities changed"}); err != nil {
		t.Fatal(err)
	}
	if ans := s.call(t, &answers, StepCall{Op: "confirm-stop", AttemptID: id}); !ans.OK || ans.Status != StepDone {
		t.Fatalf("confirm-stop: %+v", ans)
	}
	if a := s.attempt(t, id); a.Stop != store.StopConfirmed {
		t.Fatalf("the stop is not confirmed: %+v", a)
	}

	body, _ = json.Marshal(map[string]string{"target": "BLOCKED", "blocker": "waiting on design"})
	ans = s.call(t, &answers, StepCall{Op: "release", AttemptID: id, TaskID: "task-1", Body: body})
	if r := stepResult(t, ans); ans.Status != StepDone || !r.Settled || r.Outcome != "committed" {
		t.Fatalf("release: %+v", ans)
	}
	if a := s.attempt(t, id); a.State != store.AttemptClosed || a.CloseReason != "stopped" {
		t.Fatalf("attempt after release: %+v", a)
	}
	if ft := s.fakeTask(t); contentString(t, ft, "state") != "BLOCKED" {
		t.Fatalf("the task after release: %v", ft.Content)
	}
	if ans := s.call(t, &answers, StepCall{Op: "pending"}); ans.Status != StepDone || string(ans.Result) != "null" {
		t.Fatalf("pending after the steps: %+v", ans)
	}

	proofs, _ := os.ReadFile(filepath.Join(s.root, "proofs.secret"))
	secrets := append(strings.Fields(string(proofs)), s.e.token)
	if len(secrets) < 3 {
		t.Fatalf("the steps saw %d proofs", len(secrets)-1)
	}
	for _, v := range secrets {
		if v == "" || strings.Contains(string(answers), v) {
			t.Fatalf("a secret crossed the step channel (%d bytes of answers)", len(answers))
		}
	}
	s.noSecrets(t, secrets...)
}

// A step a previous run left recorded is finished when the channel starts,
// and `recover` finishes another one on request.
func TestStepChannelRecovers(t *testing.T) {
	s := setupStepsWith(t, crewOptions{role: store.RoleIndependent, steps: true, shortHome: true})
	s.d.crash = func(p string) bool { return p == "sent" }
	if _, err := s.run(t, attemptsPath+"/claim", s.claimBody()); !errors.Is(err, errCrashed) {
		t.Fatalf("the crash: %v", err)
	}
	s.d.crash = nil
	// The launcher's own start of the channel recovers.
	srv := serveSteps(s.e)
	if srv == nil {
		t.Fatal("the launcher serves no step channel")
	}
	t.Cleanup(func() { srv.Close() })
	waitFor(t, "the recorded claim to be recovered", func() bool {
		p, err := s.d.Pending()
		return err == nil && len(p) == 0
	})
	if h := loadFakeReservations(s.root).Holds; len(h) != 1 {
		t.Fatalf("the recovered claim holds %d reservations", len(h))
	}

	var answers []byte
	id := s.attemptOfTask(t)
	s.d.crash = func(p string) bool { return p == "begun" }
	if _, err := s.work(t, id, "submit", "done"); !errors.Is(err, errCrashed) {
		t.Fatalf("the crash: %v", err)
	}
	s.d.crash = nil
	if ans := s.call(t, &answers, StepCall{Op: "pending"}); !strings.Contains(string(ans.Result), `"phase":"begun"`) {
		t.Fatalf("pending: %s", ans.Result)
	}
	ans := s.call(t, &answers, StepCall{Op: "recover"})
	var rs []StepResult
	if err := json.Unmarshal(ans.Result, &rs); err != nil || ans.Status != StepDone || len(rs) != 1 ||
		!rs[0].Settled || rs[0].Outcome != "committed" {
		t.Fatalf("recover: %+v", ans)
	}
}

// The channel refuses what it cannot serve, clearly: another home's client
// finds no launcher, and a malformed call is refused as such.
func TestStepChannelRefusals(t *testing.T) {
	s := setupStepsWith(t, crewOptions{role: store.RoleIndependent, steps: true, shortHome: true})
	other, err := os.MkdirTemp("", "ah")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(other)
	if _, err := CallStep(context.Background(), other, StepCall{Op: "pending"}); !errors.Is(err, ErrNoLauncher) {
		t.Fatalf("no launcher: %v", err)
	}
	s.serve(t)
	if _, err := CallStep(context.Background(), other, StepCall{Op: "pending"}); !errors.Is(err, ErrNoLauncher) {
		t.Fatalf("another home's client reached this launcher: %v", err)
	}

	var answers []byte
	id := s.claimedThrough(t, &answers)
	for _, c := range []StepCall{
		{Op: "unknown"},
		{Op: "confirm-stop"},
		{Op: "confirm-stop", AttemptID: "../settle"},
		{Op: "release", TaskID: "task-1"},
		{Op: "release", AttemptID: id, Body: json.RawMessage(`{"target":"READY"}`)},
		{Op: "claim", Body: json.RawMessage(`{"task":{}}`)},
	} {
		ans := s.call(t, &answers, c)
		if ans.OK || ans.Status != StepRefused || ans.Error == nil || ans.Error.Code != "invalid_request" {
			t.Fatalf("%+v: %+v", c, ans)
		}
	}
	if p, _ := s.d.Pending(); len(p) != 0 {
		t.Fatalf("a refused call recorded a step: %v", p)
	}

	// A call that is not a version 1 request is refused, not served.
	conn, err := net.Dial("unix", StepSocket(s.cfg.Home))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	conn.Write([]byte(`{"version":2,"op":"pending"}` + "\n"))
	var ans StepAnswer
	if err := json.NewDecoder(conn).Decode(&ans); err != nil || ans.Status != StepRefused || ans.Error.Code != "invalid_request" {
		t.Fatalf("version 2: %+v %v", ans, err)
	}
}

// The socket is reachable only through a directory only the agent home's
// owner can reach: serving restricts an exposed one first, and the socket
// itself is private.
func TestStepChannelIsPrivate(t *testing.T) {
	home, err := os.MkdirTemp("", "ah")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(home)
	state := filepath.Join(home, "state")
	if err := os.Mkdir(state, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := privatefiletest.Expose(state); err != nil {
		t.Fatal(err)
	}
	if privatefile.CheckDir(state) == nil {
		t.Fatal("the exposed directory passed the check")
	}
	srv, err := ServeSteps(home, &Driver{Home: home}, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	if err := privatefile.CheckDir(state); err != nil {
		t.Fatalf("served from a directory that is not private: %v", err)
	}
	assertSocketPrivate(t, StepSocket(home))
	if ans, err := CallStep(context.Background(), home, StepCall{Op: "pending"}); err != nil || ans.Status != StepDone {
		t.Fatalf("pending: %+v %v", ans, err)
	}
	srv.Close()
	if _, err := os.Lstat(StepSocket(home)); !os.IsNotExist(err) {
		t.Fatalf("the socket outlived the server: %v", err)
	}
	if _, err := CallStep(context.Background(), home, StepCall{Op: "pending"}); !errors.Is(err, ErrNoLauncher) {
		t.Fatalf("after close: %v", err)
	}
}

// The launcher serves the channel to its client, which finds it through
// AICREW_AGENT_HOME, and removes it when the client exits.
func TestRunClientServesSteps(t *testing.T) {
	c := setupCrewWith(t, crewOptions{role: store.RoleWorker, shortHome: true})
	probeEnv(t, "0")
	t.Setenv("AICREW_PROBE_STEP", "1")
	t.Setenv(HomeEnv, "inherited-from-the-user-shell")
	dir := t.TempDir()
	e := c.engine(t)
	code, err := RunClient(context.Background(), e, Client{Path: selfPath(t), Args: []string{"probe-client", dir}},
		Stdio{Out: io.Discard, Err: io.Discard}, nil)
	if err != nil || code != 0 {
		t.Fatalf("run: code %d, %v", code, err)
	}
	r := readReport(t, dir)
	home, _ := filepath.Abs(c.cfg.Home)
	n := 0
	for _, kv := range r.Env {
		if name, v, _ := strings.Cut(kv, "="); strings.EqualFold(name, HomeEnv) {
			n++
			if v != home {
				t.Fatalf("the client saw %s=%q, want %q", HomeEnv, v, home)
			}
		}
	}
	if n != 1 {
		t.Fatalf("%d %s entries in the client's environment", n, HomeEnv)
	}
	b, _ := os.ReadFile(filepath.Join(dir, "step.json"))
	var ans StepAnswer
	if json.Unmarshal(b, &ans) != nil || ans.Status != StepDone {
		t.Fatalf("the client's step call: %s", b)
	}
	if _, err := os.Lstat(StepSocket(c.cfg.Home)); !os.IsNotExist(err) {
		t.Fatalf("the socket outlived the launcher: %v", err)
	}
}

// claimedThrough claims task-1 through the channel.
func (s *stepEnv) claimedThrough(t *testing.T, answers *[]byte) string {
	t.Helper()
	body, _ := json.Marshal(s.claimBody())
	ans := s.call(t, answers, StepCall{Op: "claim", Body: body})
	if ans.Status != StepDone {
		t.Fatalf("claim: %+v", ans)
	}
	return stepResult(t, ans).AttemptID
}

// A step that settles as not committed is answered as refused; an update
// aimem refused stays pending (it is never voided), through recovery too.
func TestStepChannelOutcomes(t *testing.T) {
	var answers []byte
	s := setupStepsWith(t, crewOptions{role: store.RoleIndependent, steps: true, shortHome: true})
	s.serve(t)
	id := s.claimedThrough(t, &answers)
	// The task moves before the update is read: nothing is sent, and aicrewd
	// settles it as not committed at once.
	s.setFault(t, "mcp", "bump")
	body, _ := json.Marshal(map[string]string{"intent": "submit", "detail": "https://forge.example/pull/7"})
	ans := s.call(t, &answers, StepCall{Op: "work", AttemptID: id, TaskID: "task-1", Body: body})
	if r := stepResult(t, ans); ans.OK || ans.Status != StepRefused || !r.Settled || r.Outcome != "not_committed" ||
		ans.NextAction == "" {
		t.Fatalf("a stale update: %+v", ans)
	}

	s = setupStepsWith(t, crewOptions{role: store.RoleIndependent, steps: true, shortHome: true})
	s.serve(t)
	id = s.claimedThrough(t, &answers)
	s.setFault(t, "update", "3:task_conflict")
	body, _ = json.Marshal(map[string]string{"intent": "block", "detail": "waiting"})
	ans = s.call(t, &answers, StepCall{Op: "work", AttemptID: id, TaskID: "task-1", Body: body})
	if r := stepResult(t, ans); !ans.OK || ans.Status != StepPending || r.Settled || !strings.Contains(ans.NextAction, "recover") {
		t.Fatalf("a refused update: %+v", ans)
	}
	if ans := s.call(t, &answers, StepCall{Op: "recover"}); !ans.OK || ans.Status != StepPending {
		t.Fatalf("recovering a pending update: %+v", ans)
	}
}
