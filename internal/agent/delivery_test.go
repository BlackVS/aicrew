package agent

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/BlackVS/aicrew/internal/automation"
	"github.com/BlackVS/aicrew/internal/store"
)

// The fault seen on the first team's cycle (task 01a1fa19-19a4): a
// coordinator's turn, under the per-turn settings join writes, may not run
// the installed delivery check, and may not hand it the home's forge
// credential either, since every command naming creds/ is denied.
func TestCoordinatorTurnDeniesDeliveryCheck(t *testing.T) {
	e := setupJoin(t)
	e.join(t, e.opts(), &recCrew{}, activeAimem(), e.invite(t, "inv-1", store.RoleCoordinator))
	doc := turnPermissions(t, e.home, CoordinatorTurnSettingsFile)
	turn := automation.NewAllowList(doc.Allow, doc.Deny)
	check := []string{"python3", ".claude/skills/verify-delivery/verify_delivery.py", "--pr", testPull}
	if err := turn.Admit(check[0], check[1:]); !errors.Is(err, automation.ErrRefused) {
		t.Errorf("the coordinator's turn admits the delivery check: %v", err)
	}
	// Admitting the check would not do: its credential is denied by name.
	withCred := append([]string{"env", "GITHUB_TOKEN_FILE=creds/github.com.token"}, check...)
	if err := turn.Admit(withCred[0], withCred[1:]); err == nil || !strings.Contains(err.Error(), "is denied") {
		t.Errorf("the coordinator's turn does not deny the credential's path: %v", err)
	}
}

type turnPerms struct {
	Allow []string `json:"allow"`
	Deny  []string `json:"deny"`
}

func turnPermissions(t *testing.T, home, file string) turnPerms {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(home, filepath.FromSlash(file)))
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Permissions turnPerms `json:"permissions"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	return doc.Permissions
}

// The script the launcher runs lives in the home's skills: no role's turn
// may write it with the file tools, not even under a home's own addition.
func TestTurnsMayNotWriteSkills(t *testing.T) {
	home := t.TempDir()
	writeAgentDoc(t, home, map[string]any{"turns": map[string]any{"allow": []string{"Edit(./.claude/skills/**)"}}})
	for _, coordinator := range []bool{false, true} {
		var doc struct {
			Permissions turnPerms `json:"permissions"`
		}
		if err := json.Unmarshal([]byte(turnSettings(home, coordinator)), &doc); err != nil {
			t.Fatal(err)
		}
		if !slices.Contains(doc.Permissions.Deny, "Edit(./.claude/skills/**)") {
			t.Errorf("coordinator=%v: the skills are not denied to the file tools: %v", coordinator, doc.Permissions.Deny)
		}
		for _, r := range baseTurnAllow(coordinator) {
			if strings.HasPrefix(r, "Edit(") && !strings.HasPrefix(r, "Edit(./worktrees/") && r != "Edit(./docs/HANDOFF.md)" {
				t.Errorf("coordinator=%v: a base rule writes outside the worktrees and the handoff: %s", coordinator, r)
			}
		}
	}
	var doc struct {
		Permissions turnPerms `json:"permissions"`
	}
	_ = json.Unmarshal([]byte(workerToolchainSettings(home, []string{"go"})), &doc)
	if !slices.Contains(doc.Permissions.Deny, "Edit(./.claude/skills/**)") {
		t.Errorf("a worker's toolchain settings do not deny the skills: %v", doc.Permissions.Deny)
	}
}

// fakeVerify is what the stand-in delivery script does: it is a JSON file
// the test binary, run as the interpreter, reads.
type fakeVerify struct {
	Exit   int    `json:"exit"`
	Stdout string `json:"stdout"`
	Sleep  string `json:"sleep,omitempty"`
	Big    bool   `json:"big,omitempty"`
	Record string `json:"record,omitempty"`
}

func fakeVerifyMain(args []string) int {
	raw, err := os.ReadFile(args[0])
	if err != nil {
		return 9
	}
	var f fakeVerify
	if json.Unmarshal(raw, &f) != nil {
		return 9
	}
	if f.Record != "" {
		wd, _ := os.Getwd()
		rec, _ := json.Marshal(map[string]any{"args": args[1:], "env": os.Environ(), "dir": wd})
		_ = os.WriteFile(f.Record, rec, 0o600)
	}
	if d, err := time.ParseDuration(f.Sleep); err == nil {
		time.Sleep(d)
	}
	if f.Big {
		os.Stdout.Write(make([]byte, maxDeliveryOutput+10))
	}
	os.Stdout.WriteString(f.Stdout)
	return f.Exit
}

const (
	testPull    = "https://github.com/owner/repo/pull/7"
	testAttempt = "att-1"
)

const (
	verdictConfirmed = `{"verdict":"confirmed","checks":[{"name":"post_merge_ci","status":"passed"}],` +
		`"evidence":[{"kind":"reviewed_head","ref":"https://github.com/owner/repo/commit/abc"},` +
		`{"kind":"human_merge","ref":"https://github.com/owner/repo/pull/7"},` +
		`{"kind":"post_merge_ci","ref":"https://github.com/owner/repo/actions/runs/1"}]}`
	verdictPending      = `{"verdict":"pending","checks":[{"name":"post_merge_ci","status":"pending"}]}`
	verdictNotConfirmed = `{"verdict":"not_confirmed","checks":[{"name":"post_merge_ci","status":"failed",` +
		`"detail":"go-test failed"}]}`
)

// deliveryEnv is a coordinator told of the merge of attempt att-1's
// accepted result, with the stand-in script installed and a GitHub
// credential.
func deliveryEnv(t *testing.T) *loopEnv {
	t.Helper()
	e := newLoopEnv(t)
	e.host.role = "coordinator"
	// No skill of the user running the tests is found.
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	e.loop.Interpreters = []string{self}
	writeAgentDoc(t, e.home, map[string]any{"forge": map[string]any{"github.com": map[string]any{
		"kind": "github", "account": "coord", "purpose": "repo-write", "file": "github-com.coord.repo-write"}}})
	os.MkdirAll(filepath.Join(e.home, "creds"), 0o700)
	os.WriteFile(filepath.Join(e.home, "creds", "github-com.coord.repo-write"), []byte("tok\n"), 0o600)
	e.host.open = OpenAttempts{Attempts: []OpenAttempt{{ID: testAttempt, State: "running", Phase: "accepted",
		WorkerAgentID: "worker-1", LatestResult: &LatestResult{ResultSeq: 2, Kind: "pull_request", Ref: testPull}}}}
	m := mergedMessage("m1", testAttempt)
	e.host.inbox = []inboxMessage{m}
	e.host.pending = []PendingMessage{{ID: m.ID, Seq: m.Seq, Kind: m.Kind, AttemptID: m.AttemptID}}
	setScript(t, e, fakeVerify{Exit: 4, Stdout: verdictPending})
	return e
}

func mergedMessage(id, attempt string, more ...actionSubject) inboxMessage {
	subjects := append([]actionSubject{{Kind: "pull", AttemptID: attempt, Needs: []string{"merged"},
		Pull: &briefPull{Pull: 7, URL: testPull, State: "merged"}}}, more...)
	return inboxMessage{ID: id, Seq: msgSeq.Add(1), Kind: "action.needed", AttemptID: attempt,
		Text: "The pull request was merged.", Action: &struct {
			Subjects []actionSubject `json:"subjects"`
		}{subjects}}
}

func setScript(t *testing.T, e *loopEnv, f fakeVerify) {
	t.Helper()
	p := filepath.Join(e.home, ".claude", "skills", filepath.FromSlash(deliveryScript))
	os.MkdirAll(filepath.Dir(p), 0o700)
	raw, _ := json.Marshal(f)
	if err := os.WriteFile(p, raw, 0o600); err != nil {
		t.Fatal(err)
	}
}

func (h *fakeHost) opCalls(op string) []StepCall {
	h.mu.Lock()
	defer h.mu.Unlock()
	var out []StepCall
	for _, c := range h.calls {
		if c.Op == op {
			out = append(out, c)
		}
	}
	return out
}

// The first team's case: the merge message waits when the launcher starts.
// While CI on the merge commit runs, the check reads on its schedule, no
// turn starts, nothing is counted and no hold is reported; once the check
// confirms, the launcher takes the confirm step with the check's own
// evidence and acknowledges the message.
func TestDeliveryPendingThenConfirmed(t *testing.T) {
	e := deliveryEnv(t)
	start := e.clock
	reads := 0
	for e.clock.Sub(start) < time.Hour {
		e.tick(t)
		if c := readTurnState(e.home).Deliveries[testAttempt]; c != nil {
			reads = c.Reads
		}
	}
	st := readTurnState(e.home)
	// At first sight, then 2, 5, 10 and every 15 minutes: 0, 2, 7, 17, 32, 47.
	if len(e.specs) != 0 || st.Unacked != nil || len(e.host.holds) != 0 || reads != 6 {
		t.Fatalf("pending: %d turns, count %+v, holds %v, %d reads", len(e.specs), st.Unacked, e.host.holds, reads)
	}
	if e.host.inboxReads != 1 {
		t.Errorf("the message was read %d times for the check, not once", e.host.inboxReads)
	}
	setScript(t, e, fakeVerify{Exit: 0, Stdout: verdictConfirmed})
	e.ticksTo(t, e.clock.Add(16*time.Minute))
	confirms := e.host.opCalls("confirm-delivery")
	if len(confirms) != 1 || confirms[0].AttemptID != testAttempt {
		t.Fatalf("confirm steps: %+v", confirms)
	}
	var body struct {
		ResultSeq int64          `json:"result_seq"`
		Evidence  []StepEvidence `json:"evidence"`
	}
	json.Unmarshal(confirms[0].Body, &body)
	if body.ResultSeq != 2 || len(body.Evidence) != 3 || body.Evidence[2].Ref != "https://github.com/owner/repo/actions/runs/1" {
		t.Errorf("the confirm's body: %s", confirms[0].Body)
	}
	if len(e.host.pending) != 0 || len(e.specs) != 0 || readTurnState(e.home).Deliveries[testAttempt] != nil {
		t.Errorf("after the confirm: pending %v, %d turns", e.host.pending, len(e.specs))
	}
}

// Not confirmed goes to a turn once, with the failed checks in the brief;
// the launcher reads on, and a repair made outside (a re-run CI job) is
// confirmed without another turn.
func TestDeliveryNotConfirmedThenConfirmed(t *testing.T) {
	e := deliveryEnv(t)
	setScript(t, e, fakeVerify{Exit: 3, Stdout: verdictNotConfirmed})
	e.next = func(spec TurnSpec, h *fakeHost) TurnResult {
		h.ack("m1") // the coordinator escalates and acknowledges
		return TurnResult{Outcome: TurnFinished}
	}
	e.tick(t)
	if len(e.specs) != 1 || !strings.Contains(e.specs[0].Prompt, "post_merge_ci failed (go-test failed)") ||
		!strings.Contains(e.specs[0].Prompt, "Escalate with this reason") {
		t.Fatalf("%d turns; brief: %s", len(e.specs), lastPrompt(e))
	}
	e.ticksTo(t, e.clock.Add(time.Hour))
	if len(e.specs) != 1 {
		t.Fatalf("%d turns on one not confirmed answer", len(e.specs))
	}
	setScript(t, e, fakeVerify{Exit: 0, Stdout: verdictConfirmed})
	e.ticksTo(t, e.clock.Add(16*time.Minute))
	if len(e.host.opCalls("confirm-delivery")) != 1 || len(e.specs) != 1 {
		t.Fatalf("after the repair: %d confirms, %d turns", len(e.host.opCalls("confirm-delivery")), len(e.specs))
	}
	if len(e.host.opCalls("ack")) != 1 {
		t.Errorf("the launcher acknowledges what is left: %d acks", len(e.host.opCalls("ack")))
	}
}

// A day of reads ends the check; a new message for the attempt gives it one
// more read.
func TestDeliveryDayAndNewMessage(t *testing.T) {
	e := deliveryEnv(t)
	e.next = func(TurnSpec, *fakeHost) TurnResult { return TurnResult{Outcome: TurnFinished} }
	e.tick(t)
	e.clock = e.clock.Add(deliveryDay)
	e.tick(t)
	c := readTurnState(e.home).Deliveries[testAttempt]
	if c == nil || !c.Handed || !c.Done || !strings.Contains(c.Reason, "pending for a day") || len(e.specs) != 1 {
		t.Fatalf("after a day: %+v, %d turns", c, len(e.specs))
	}
	e.host.ack("m1")
	reads := c.Reads
	e.ticksTo(t, e.clock.Add(time.Hour))
	if n := readTurnState(e.home).Deliveries[testAttempt].Reads; n != reads {
		t.Fatalf("a finished check read again: %d", n)
	}
	setScript(t, e, fakeVerify{Exit: 0, Stdout: verdictConfirmed})
	note := msg("n1", "message")
	note.AttemptID = testAttempt
	e.host.pending = append(e.host.pending, note)
	e.tick(t)
	if len(e.host.opCalls("confirm-delivery")) != 1 {
		t.Fatal("a new message for the attempt gave no read")
	}
}

// A mixed message starts the check and goes to a turn, which handles the
// other subject and acknowledges it; that does not end the check, which
// confirms later and has nothing left to acknowledge.
func TestDeliveryMixedMessage(t *testing.T) {
	e := deliveryEnv(t)
	m := mergedMessage("m1", testAttempt, actionSubject{Kind: "board", Board: &briefBoard{Revision: 3}})
	e.host.inbox = []inboxMessage{m}
	e.host.pending = []PendingMessage{{ID: m.ID, Seq: m.Seq, Kind: m.Kind, AttemptID: m.AttemptID}}
	e.next = func(_ TurnSpec, h *fakeHost) TurnResult { h.ack("m1"); return TurnResult{Outcome: TurnFinished} }
	e.tick(t)
	if len(e.specs) != 1 || !strings.Contains(e.specs[0].Prompt, "is the launcher's and is pending") {
		t.Fatalf("%d turns; brief: %s", len(e.specs), lastPrompt(e))
	}
	if c := readTurnState(e.home).Deliveries[testAttempt]; c == nil || len(c.Deferred) != 0 {
		t.Fatalf("the check: %+v", c)
	}
	setScript(t, e, fakeVerify{Exit: 0, Stdout: verdictConfirmed})
	e.ticksTo(t, e.clock.Add(16*time.Minute))
	if len(e.host.opCalls("confirm-delivery")) != 1 || len(e.host.opCalls("ack")) != 0 || len(e.specs) != 1 {
		t.Fatalf("%d confirms, %d acks, %d turns", len(e.host.opCalls("confirm-delivery")), len(e.host.opCalls("ack")),
			len(e.specs))
	}
}

// A launcher stopped between the confirm and the acknowledgement
// acknowledges after its restart, without a second confirm.
func TestDeliveryStopBetweenConfirmAndAck(t *testing.T) {
	e := deliveryEnv(t)
	setScript(t, e, fakeVerify{Exit: 0, Stdout: verdictConfirmed})
	e.host.refuse = map[string]*StepError{"ack": {Code: "unavailable", Message: "aicrewd did not answer"}}
	e.tick(t)
	if c := readTurnState(e.home).Deliveries[testAttempt]; c == nil || !c.Confirmed || len(e.specs) != 0 {
		t.Fatalf("after the confirm: %+v, %d turns", c, len(e.specs))
	}
	e.host.refuse = nil
	e.loop = &TurnLoop{Home: e.loop.Home, Client: e.loop.Client, Host: e.host, Log: e.loop.Log, Now: e.loop.Now,
		Head: e.loop.Head, Clean: e.loop.Clean, Run: e.loop.Run, Interpreters: e.loop.Interpreters}
	e.tick(t)
	if len(e.host.opCalls("confirm-delivery")) != 1 || len(e.host.pending) != 0 || len(e.specs) != 0 {
		t.Fatalf("after the restart: %d confirms, pending %v, %d turns", len(e.host.opCalls("confirm-delivery")),
			e.host.pending, len(e.specs))
	}
}

// A confirm aicrewd refuses goes to a turn once, and is not repeated.
func TestDeliveryConfirmRefused(t *testing.T) {
	e := deliveryEnv(t)
	setScript(t, e, fakeVerify{Exit: 0, Stdout: verdictConfirmed})
	e.host.refuse = map[string]*StepError{"confirm-delivery": {Code: "attempt_state", Message: "not the accepted result"}}
	e.next = func(_ TurnSpec, h *fakeHost) TurnResult { h.ack("m1"); return TurnResult{Outcome: TurnFinished} }
	e.tick(t)
	if len(e.specs) != 1 || !strings.Contains(e.specs[0].Prompt, "not the accepted result") {
		t.Fatalf("%d turns; brief: %s", len(e.specs), lastPrompt(e))
	}
	e.ticksTo(t, e.clock.Add(2*time.Hour))
	if len(e.host.opCalls("confirm-delivery")) != 1 || len(e.specs) != 1 {
		t.Fatalf("%d confirms, %d turns", len(e.host.opCalls("confirm-delivery")), len(e.specs))
	}
}

// A new message is not deferred and not sent to a turn on a guess: while
// the read fails it is held, up to a bound, then it goes to a turn as it is.
func TestDeliveryReadFails(t *testing.T) {
	e := deliveryEnv(t)
	e.host.inboxErr = errors.New("aicrewd did not answer")
	for i := 0; i < maxDeliveryReadFails-1; i++ {
		e.tick(t)
	}
	if len(e.specs) != 0 || readTurnState(e.home).Deliveries[testAttempt] != nil {
		t.Fatalf("while the read fails: %d turns", len(e.specs))
	}
	e.tick(t)
	if len(e.specs) != 1 {
		t.Fatalf("after the bound: %d turns", len(e.specs))
	}
}

// An attempt no longer accepted ends its check; its messages go to a turn.
func TestDeliveryAttemptNoLongerAccepted(t *testing.T) {
	e := deliveryEnv(t)
	e.tick(t)
	e.host.open.Attempts[0].Phase = "submitted"
	e.ticksTo(t, e.clock.Add(3*time.Minute))
	if readTurnState(e.home).Deliveries[testAttempt] != nil || len(e.specs) != 1 {
		t.Fatalf("the check stayed, %d turns", len(e.specs))
	}
}

// A worker's launcher defers nothing: the check is the coordinator's.
func TestDeliveryNotAWorkers(t *testing.T) {
	e := deliveryEnv(t)
	e.host.role = "worker"
	e.tick(t)
	if len(e.specs) != 1 || len(readTurnState(e.home).Deliveries) != 0 {
		t.Fatalf("a worker: %d turns, checks %v", len(e.specs), readTurnState(e.home).Deliveries)
	}
}

// On Gitea the check needs the reviews' authors: without the external one
// it is not confirmed with a reason that names the field, and with both it
// runs with them.
func TestDeliveryGiteaAuthors(t *testing.T) {
	e := deliveryEnv(t)
	const pull = "https://git.example.org/owner/repo/pulls/7"
	writeAgentDoc(t, e.home, map[string]any{"forge": map[string]any{"git.example.org": map[string]any{
		"kind": "gitea", "account": "coord", "purpose": "repo-write", "file": "git-example-org.coord.repo-write"}}})
	os.WriteFile(filepath.Join(e.home, "creds", "git-example-org.coord.repo-write"), []byte("tok\n"), 0o600)
	e.host.inbox[0].Action.Subjects[0].Pull.URL = pull
	e.next = func(_ TurnSpec, h *fakeHost) TurnResult { h.ack("m1"); return TurnResult{Outcome: TurnFinished} }
	e.tick(t)
	if len(e.specs) != 1 || !strings.Contains(e.specs[0].Prompt, "forge.git.example.org.external_reviewer") {
		t.Fatalf("%d turns; brief: %s", len(e.specs), lastPrompt(e))
	}
	writeAgentDoc(t, e.home, map[string]any{"forge": map[string]any{"git.example.org": map[string]any{
		"kind": "gitea", "account": "coord", "purpose": "repo-write", "file": "git-example-org.coord.repo-write",
		"external_reviewer": "hands-bot"}}})
	e.host.members = json.RawMessage(`{"members":[{"agent_id":"worker-1","capabilities":[{"host":"git.example.org",` +
		`"kind":"gitea","account":"worker-acct"}]}]}`)
	record := filepath.Join(t.TempDir(), "run.json")
	setScript(t, e, fakeVerify{Exit: 0, Stdout: verdictConfirmed, Record: record})
	e.ticksTo(t, e.clock.Add(16*time.Minute))
	var run struct {
		Args []string `json:"args"`
		Env  []string `json:"env"`
	}
	raw, _ := os.ReadFile(record)
	json.Unmarshal(raw, &run)
	want := []string{"--pr", pull, "--forge", "gitea", "--api-base", "https://git.example.org/api/v1",
		"--review-author", "local=worker-acct", "--review-author", "local=coord", "--review-author", "external=hands-bot"}
	if !slices.Equal(run.Args, want) || len(e.host.opCalls("confirm-delivery")) != 1 {
		t.Fatalf("args %q, %d confirms", run.Args, len(e.host.opCalls("confirm-delivery")))
	}
	if !slices.Contains(run.Env, "GITEA_TOKEN_FILE="+filepath.Join(e.home, "creds", "git-example-org.coord.repo-write")) {
		t.Errorf("env %q", run.Env)
	}
}

// The subprocess: the interpreter in isolated mode with a fixed argument
// list, the state directory as its working directory, and an environment of
// PATH and the token file's variable (with SSL_CERT_FILE when set, and
// SYSTEMROOT on Windows); the token's value is in none of them.
func TestDeliveryRunEnvironment(t *testing.T) {
	e := deliveryEnv(t)
	t.Setenv("AICREW_SECRET_PROBE", "leak")
	t.Setenv("SSL_CERT_FILE", "/sim/tls/ca.pem")
	record := filepath.Join(t.TempDir(), "run.json")
	setScript(t, e, fakeVerify{Exit: 0, Stdout: verdictConfirmed, Record: record})
	if r := e.loop.verify(context.Background(), deliveryRun{Pull: testPull}); r.Verdict != deliveryConfirmed {
		t.Fatalf("the run: %+v", r)
	}
	var run struct {
		Args []string `json:"args"`
		Env  []string `json:"env"`
		Dir  string   `json:"dir"`
	}
	raw, _ := os.ReadFile(record)
	json.Unmarshal(raw, &run)
	if !slices.Equal(run.Args, []string{"--pr", testPull}) {
		t.Errorf("args %q", run.Args)
	}
	allowed := map[string]bool{"PATH": true, "GITHUB_TOKEN_FILE": true, "SSL_CERT_FILE": true, "SYSTEMROOT": true}
	names := map[string]bool{}
	for _, kv := range run.Env {
		name, _, _ := strings.Cut(kv, "=")
		name = strings.ToUpper(name)
		names[name] = true
		if !allowed[name] && !(runtime.GOOS == "windows" && strings.HasPrefix(name, "=")) {
			t.Errorf("the check's environment has %s", kv)
		}
		if strings.Contains(kv, "tok") && !strings.HasPrefix(kv, "GITHUB_TOKEN_FILE=") {
			t.Errorf("the token's value reached the environment: %s", name)
		}
	}
	if !names["GITHUB_TOKEN_FILE"] || !names["SSL_CERT_FILE"] {
		t.Errorf("the check's environment lacks its variables: %q", run.Env)
	}
	if d, _ := filepath.EvalSymlinks(run.Dir); d != mustEval(t, filepath.Join(e.home, "state")) {
		t.Errorf("the check ran in %s", run.Dir)
	}
}

// Every fault of the check itself has a named reason and never confirms.
func TestDeliveryFaults(t *testing.T) {
	e := deliveryEnv(t)
	run := func() deliveryResult { return e.loop.verify(context.Background(), deliveryRun{Pull: testPull}) }
	cases := []struct {
		name   string
		script *fakeVerify
		interp []string
		want   string
	}{
		{"exit 2", &fakeVerify{Exit: 2, Stdout: verdictConfirmed}, nil, "exit code 2"},
		{"unparsable", &fakeVerify{Exit: 0, Stdout: "not json"}, nil, "without a verdict that parses"},
		{"oversized", &fakeVerify{Exit: 0, Stdout: verdictConfirmed, Big: true}, nil, "over"},
		{"confirmed with exit 3", &fakeVerify{Exit: 3, Stdout: verdictConfirmed}, nil, `exited 3 with the verdict "confirmed"`},
		{"exit 0 not confirmed", &fakeVerify{Exit: 0, Stdout: verdictNotConfirmed}, nil, `exited 0 with the verdict`},
		{"confirmed without evidence", &fakeVerify{Exit: 0, Stdout: `{"verdict":"confirmed"}`}, nil, "exited 0"},
		{"no interpreter", &fakeVerify{Exit: 0, Stdout: verdictConfirmed}, []string{"aicrew-no-such-python"}, "no interpreter"},
	}
	for _, c := range cases {
		setScript(t, e, *c.script)
		e.loop.Interpreters = c.interp
		if c.interp == nil {
			self, _ := os.Executable()
			e.loop.Interpreters = []string{self}
		}
		if r := run(); r.Verdict != deliveryFault || !strings.Contains(r.Reason, c.want) {
			t.Errorf("%s: %+v", c.name, r)
		}
	}
	self, _ := os.Executable()
	e.loop.Interpreters = []string{self}
	os.Remove(filepath.Join(e.home, ".claude", "skills", filepath.FromSlash(deliveryScript)))
	if r := run(); r.Verdict != deliveryFault || !strings.Contains(r.Reason, "is not installed") {
		t.Errorf("no script: %+v", r)
	}
	setScript(t, e, fakeVerify{Exit: 0, Stdout: verdictConfirmed, Sleep: "30s"})
	old := deliveryTimeout
	deliveryTimeout = 300 * time.Millisecond
	defer func() { deliveryTimeout = old }()
	if r := run(); r.Verdict != deliveryFault || !strings.Contains(r.Reason, "did not end within") {
		t.Errorf("timeout: %+v", r)
	}
}

func lastPrompt(e *loopEnv) string {
	if len(e.specs) == 0 {
		return ""
	}
	return e.specs[len(e.specs)-1].Prompt
}

// A count from a state that predates the check (an upgraded home) loses
// the held message without being cleared; one left with nothing to wait
// for is cleared.
func TestDeliveryLeavesTheCount(t *testing.T) {
	e := deliveryEnv(t)
	note := inboxMessage{ID: "n1", Seq: msgSeq.Add(1), Kind: "note", AttemptID: testAttempt, Text: "About the attempt."}
	e.host.inbox = append(e.host.inbox, note)
	e.host.pending = append(e.host.pending, PendingMessage{ID: note.ID, Seq: note.Seq, Kind: note.Kind, AttemptID: testAttempt})
	u := &unackedState{Messages: []string{"m1", "n1"}, Stuck: []string{"m1", "n1"}, Served: note.Seq, Turns: 1,
		Since: e.clock, LastEnded: e.clock}
	if err := writeTurnState(e.home, turnState{Unacked: u}); err != nil {
		t.Fatal(err)
	}
	e.tick(t)
	st := readTurnState(e.home)
	if st.Unacked == nil || !slices.Equal(st.Unacked.Stuck, []string{"n1"}) || slices.Contains(st.Unacked.Messages, "m1") {
		t.Fatalf("the count: %+v", st.Unacked)
	}

	e = deliveryEnv(t)
	u = &unackedState{Messages: []string{"m1"}, Stuck: []string{"m1"}, Served: e.host.pending[0].Seq, Turns: 1,
		Since: e.clock, LastEnded: e.clock}
	if err := writeTurnState(e.home, turnState{Unacked: u}); err != nil {
		t.Fatal(err)
	}
	e.tick(t)
	if st := readTurnState(e.home); st.Unacked != nil || len(e.specs) != 0 {
		t.Fatalf("a count of the held message alone: %+v, %d turns", st.Unacked, len(e.specs))
	}
}
