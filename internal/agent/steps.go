package agent

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// The client step driver (crew-execution b2; docs/CREW-CONTRACT.md, "Attempt
// steps"). Under D4(a) the member's own aimem connection sends every
// reservation mutation, and aicrewd confirms it through its read scope. The
// driver runs one step with no model involved:
//
//  1. begin: the step's begin route, under an Idempotency-Key recorded first;
//  2. record: a durable pending-step record in the agent home, which holds
//     the step's nonsecret values and the proof's SHA-256, never the proof
//     or the token;
//  3. compose: the reservation body from the begin response and, for a step
//     that carries the task's complete content, the task as the member's
//     aimem connection reads it at the step's expected revision, with only
//     the fields aicrew owns changed;
//  4. send: `aimem reservation OP --task T --key K`, the body on stdin, under
//     the bound team session, its exit code mapped to a report;
//  5. settle: the report through the settle route. A settled step's record
//     is removed; a pending one stays for Recover.
//
// A restarted driver finishes every recorded step with the same keys: the
// begin's Idempotency-Key and aimem's request key never change.

// StepPhase is how far a recorded step has gone.
type StepPhase string

const (
	// PhaseBegin: the begin was about to be sent. Its reply may have been
	// lost; the same Idempotency-Key recovers it.
	PhaseBegin StepPhase = "begin"
	// PhaseBegun: the step's values are recorded; aimem may or may not have
	// been sent the step. aimem's request key makes a resend safe.
	PhaseBegun StepPhase = "begun"
	// PhaseSent: aimem answered, and the report is recorded; only the
	// settle remains.
	PhaseSent StepPhase = "sent"
)

// Step is a begin response: coordination.v1's begin_exchange body, with the
// values the member sends aimem (D-b1b-2).
type Step struct {
	Operation         string          `json:"operation"`
	RequestKey        string          `json:"request_key"`
	ExpectedRevision  int64           `json:"expected_revision"`
	ReservationID     string          `json:"reservation_id,omitempty"`
	Fence             string          `json:"fence,omitempty"`
	Holder            json.RawMessage `json:"holder,omitempty"`
	TargetState       string          `json:"target_state,omitempty"`
	Reason            string          `json:"reason,omitempty"`
	Blocker           string          `json:"blocker,omitempty"`
	TerminalEvidence  []string        `json:"terminal_evidence,omitempty"`
	Intent            string          `json:"intent,omitempty"`
	ResultRef         string          `json:"result_ref,omitempty"`
	CoordinationProof string          `json:"coordination_proof,omitempty"`
}

// StepRequest is one step to drive: its begin route (relative to aicrewd's
// base URL), the begin's JSON body, and the aimem task the step acts on.
type StepRequest struct {
	Path   string          `json:"path"`
	Body   json.RawMessage `json:"body"`
	TaskID string          `json:"task_id"`
}

// StepReport is the member's report of what aimem did: a hint only.
type StepReport struct {
	Outcome string `json:"outcome"`
	Code    string `json:"code,omitempty"`
}

// StepResult is where a driven step stands.
type StepResult struct {
	AttemptID  string        `json:"attempt_id"`
	RequestKey string        `json:"request_key"`
	Operation  string        `json:"operation"`
	Report     StepReport    `json:"report"`
	Settled    bool          `json:"settled"`
	Outcome    string        `json:"outcome,omitempty"`
	RetryAfter time.Duration `json:"retry_after,omitempty"`
}

// pendingStep is the durable record of a step in flight. It never holds a
// secret: the proof is kept only as its SHA-256.
type pendingStep struct {
	Version     int         `json:"version"`
	BeginKey    string      `json:"begin_key"`
	Request     StepRequest `json:"request"`
	Phase       StepPhase   `json:"phase"`
	AttemptID   string      `json:"attempt_id,omitempty"`
	Step        *Step       `json:"step,omitempty"`
	ProofSHA256 string      `json:"proof_sha256,omitempty"`
	Report      *StepReport `json:"report,omitempty"`
	UpdatedAt   time.Time   `json:"updated_at"`
}

// StepAPI is the part of aicrewd's session API the driver uses.
type StepAPI interface {
	// BeginStep sends a begin route and returns the step and the attempt's
	// ID. A replay of a step that has settled is a *Refusal
	// (step_settled or attempt_state).
	BeginStep(ctx context.Context, key, token, path string, body []byte) (Step, string, error)
	// SettleStep reports a step and returns the settlement; a pending step
	// is not an error.
	SettleStep(ctx context.Context, token, attemptID string, requestKey string, report StepReport) (Settlement, error)
}

// Settlement is a settle's answer.
type Settlement struct {
	Settled    bool
	Outcome    string
	RetryAfter time.Duration
}

// TaskDoc is an aimem task as get_task reads it: its revision and its fields.
type TaskDoc struct {
	Revision int64
	Fields   map[string]json.RawMessage
}

// ReservationCLI is the member's aimem connection: the reservation CLI and a
// task read, under the bound team session.
type ReservationCLI interface {
	// Reservation runs `aimem reservation ARGS…` with stdin and returns
	// aimem's exit code and its one JSON document.
	Reservation(ctx context.Context, sessionFile string, stdin []byte, args ...string) (int, []byte, error)
	// GetTask reads a task over the same team session.
	GetTask(ctx context.Context, sessionFile, taskID string) (TaskDoc, error)
}

// stepSession is the launcher's live session: its token and aimem binding.
type stepSession interface {
	stepToken() string
	stepAimemFile() string
	stepAgent() string
}

func (e *Engine) stepToken() string {
	e.live.RLock()
	defer e.live.RUnlock()
	return e.token
}

func (e *Engine) stepAimemFile() string {
	e.live.RLock()
	defer e.live.RUnlock()
	return e.aimemFile
}

// stepAgent is the member's agent ID, which a submitted result names.
func (e *Engine) stepAgent() string { return e.Cfg.AgentID }

func (e *Engine) setAimemFile(path string) {
	e.live.Lock()
	e.aimemFile = path
	e.live.Unlock()
}

// Driver drives steps for one agent home.
type Driver struct {
	Home    string
	Crew    StepAPI
	Aimem   ReservationCLI
	Session stepSession
	Log     *slog.Logger
	Now     func() time.Time
	Sleep   func(ctx context.Context, d time.Duration) error
	// Retries bounds the resends of a step aimem refused as retryable.
	Retries int
	// composed, when set by tests, sees and may change each composed body
	// before it is sent: a seeded fault.
	composed func(op string, body map[string]any)
	// crash, when set by tests, ends a drive at a named point, as a killed
	// driver would: "begun" (recorded, not sent), "answered" (aimem
	// answered, the answer not recorded) or "sent" (recorded, not settled).
	crash func(point string) bool
}

// errCrashed ends a drive at a test's crash point.
var errCrashed = errors.New("the driver stopped (test crash point)")

func (d *Driver) crashed(point string) bool { return d.crash != nil && d.crash(point) }

// NewDriver assembles a driver over an engine's session.
func NewDriver(home string, crew StepAPI, aimem ReservationCLI, e *Engine, log *slog.Logger) *Driver {
	return &Driver{Home: home, Crew: crew, Aimem: aimem, Session: e, Log: log, Now: time.Now, Sleep: sleepCtx, Retries: 3}
}

func stepsDir(home string) string { return filepath.Join(home, "state", "steps") }

var beginKeyShape = regexp.MustCompile(`^step-[0-9a-f]{32}$`)

func (d *Driver) recordPath(beginKey string) string {
	return filepath.Join(stepsDir(d.Home), beginKey+".json")
}

// save replaces a step's record atomically, readable by its owner only.
func (d *Driver) save(p *pendingStep) error {
	p.Version, p.UpdatedAt = 1, d.Now().UTC()
	dir := stepsDir(d.Home)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	raw, err := json.MarshalIndent(p, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".step-*.json")
	if err != nil {
		return err
	}
	_, werr := tmp.Write(append(raw, '\n'))
	cerr := tmp.Close()
	if werr != nil || cerr != nil {
		os.Remove(tmp.Name())
		return errors.Join(werr, cerr)
	}
	if err := os.Rename(tmp.Name(), d.recordPath(p.BeginKey)); err != nil {
		os.Remove(tmp.Name())
		return err
	}
	return nil
}

func (d *Driver) drop(p *pendingStep) error {
	err := os.Remove(d.recordPath(p.BeginKey))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}

// Pending lists the recorded steps, oldest first.
func (d *Driver) Pending() ([]*pendingStep, error) {
	entries, err := os.ReadDir(stepsDir(d.Home))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []*pendingStep
	for _, e := range entries {
		name, ok := strings.CutSuffix(e.Name(), ".json")
		if !ok || !beginKeyShape.MatchString(name) {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(stepsDir(d.Home), e.Name()))
		if err != nil {
			return nil, err
		}
		var p pendingStep
		if err := json.Unmarshal(raw, &p); err != nil || p.Version != 1 || p.BeginKey != name {
			return nil, fmt.Errorf("%s is not a step record", e.Name())
		}
		out = append(out, &p)
	}
	return out, nil
}

// Run drives a new step to its settle.
func (d *Driver) Run(ctx context.Context, req StepRequest) (StepResult, error) {
	var b [16]byte
	_, _ = rand.Read(b[:])
	p := &pendingStep{BeginKey: "step-" + hex.EncodeToString(b[:]), Request: req, Phase: PhaseBegin}
	if err := d.save(p); err != nil {
		return StepResult{}, fmt.Errorf("record the step: %w", err)
	}
	return d.drive(ctx, p, true)
}

// Recover finishes every recorded step with its recorded keys. A step that
// fails does not hold up the others; the first failure is returned.
func (d *Driver) Recover(ctx context.Context) ([]StepResult, error) {
	pending, err := d.Pending()
	if err != nil {
		return nil, err
	}
	var out []StepResult
	var first error
	for _, p := range pending {
		r, err := d.drive(ctx, p, false)
		if err != nil {
			if first == nil {
				first = err
			}
			continue
		}
		out = append(out, r)
	}
	return out, first
}

// drive takes a recorded step from its phase to its settle. fresh is true
// for a step's first begin, whose answer is known to be the answer to the
// only begin sent under its key.
func (d *Driver) drive(ctx context.Context, p *pendingStep, fresh bool) (StepResult, error) {
	var proof string
	if p.Phase == PhaseBegin || p.Phase == PhaseBegun {
		// The proof is never recorded: a step not yet answered by aimem
		// begins again under its key, which returns the same step with a
		// replacement proof (D-b1a-1), or the same update.
		st, attemptID, err := d.Crew.BeginStep(ctx, p.BeginKey, d.Session.stepToken(), p.Request.Path, p.Request.Body)
		var ref *Refusal
		switch {
		case errors.As(err, &ref) && (ref.Code == "step_settled" || ref.Code == "attempt_state") && p.Step != nil:
			// The step settled meanwhile: settle reports its outcome.
			p.Phase, p.Report = PhaseSent, &StepReport{Outcome: "unknown"}
		case errors.As(err, &ref) && !ref.Retryable && fresh:
			// aicrewd refused the step's first begin: nothing began, so
			// there is nothing to finish. A recovery's refusal proves nothing
			// about an earlier begin whose reply was lost (it may have
			// committed), so it never drops the record.
			if derr := d.drop(p); derr != nil {
				return StepResult{}, errors.Join(err, derr)
			}
			return StepResult{}, err
		case err != nil:
			return StepResult{}, err
		default:
			if p.Step != nil && st.RequestKey != p.Step.RequestKey {
				return StepResult{}, fmt.Errorf("the step's begin answered request key %s, recorded %s", st.RequestKey, p.Step.RequestKey)
			}
			proof = st.CoordinationProof
			st.CoordinationProof = ""
			p.AttemptID, p.Step, p.Phase = attemptID, &st, PhaseBegun
			if proof != "" {
				sum := sha256.Sum256([]byte(proof))
				p.ProofSHA256 = hex.EncodeToString(sum[:])
			}
			if err := d.save(p); err != nil {
				return StepResult{}, fmt.Errorf("record the step: %w", err)
			}
			if d.crashed("begun") {
				return StepResult{}, errCrashed
			}
		}
	}
	if p.Phase == PhaseBegun {
		rep, err := d.send(ctx, p, proof)
		if err != nil {
			return StepResult{}, err
		}
		if d.crashed("answered") {
			return StepResult{}, errCrashed
		}
		p.Report, p.Phase = &rep, PhaseSent
		if err := d.save(p); err != nil {
			return StepResult{}, fmt.Errorf("record the step: %w", err)
		}
		if d.crashed("sent") {
			return StepResult{}, errCrashed
		}
	}
	return d.settle(ctx, p)
}

// settle reports the step. A settled step's record is dropped; a pending one
// is kept for Recover, never waited on here.
func (d *Driver) settle(ctx context.Context, p *pendingStep) (StepResult, error) {
	res := StepResult{AttemptID: p.AttemptID, RequestKey: p.Step.RequestKey, Operation: p.Step.Operation, Report: *p.Report}
	set, err := d.Crew.SettleStep(ctx, d.Session.stepToken(), p.AttemptID, p.Step.RequestKey, *p.Report)
	if err != nil {
		return res, err
	}
	res.Settled, res.Outcome, res.RetryAfter = set.Settled, set.Outcome, set.RetryAfter
	if set.Settled {
		if err := d.drop(p); err != nil {
			return res, fmt.Errorf("drop the settled step's record: %w", err)
		}
	}
	return res, nil
}

// contentOps are the operations whose body carries the task's complete
// content.
var contentOps = map[string]bool{"update": true, "release": true, "finalize": true}

// send composes the step's body and runs it through aimem, and maps aimem's
// answer to a report.
func (d *Driver) send(ctx context.Context, p *pendingStep, proof string) (StepReport, error) {
	st, file := p.Step, d.Session.stepAimemFile()
	var task *TaskDoc
	if contentOps[st.Operation] {
		t, err := d.Aimem.GetTask(ctx, file, p.Request.TaskID)
		if err != nil {
			return StepReport{}, fmt.Errorf("read the task: %w", err)
		}
		if t.Revision != st.ExpectedRevision {
			// Never write content read at another revision: send nothing,
			// and let the step be begun again.
			d.Log.Info("step not sent: the task moved", "operation", st.Operation, "key", st.RequestKey)
			return StepReport{Outcome: "refused", Code: "stale_revision"}, nil
		}
		task = &t
	}
	body, err := composeBody(st, proof, task, resultNote(p.AttemptID, d.Session.stepAgent()))
	if err != nil {
		return StepReport{}, err
	}
	if d.composed != nil {
		d.composed(st.Operation, body)
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return StepReport{}, err
	}
	args := []string{st.Operation, "--task", p.Request.TaskID, "--key", st.RequestKey}
	for attempt := 0; ; attempt++ {
		code, out, err := d.Aimem.Reservation(ctx, file, raw, args...)
		if err != nil {
			return StepReport{}, err
		}
		switch code {
		case 0:
			return StepReport{Outcome: "committed"}, nil
		case 3:
			return StepReport{Outcome: "refused", Code: refusalOf(out)}, nil
		case 4:
			if attempt < d.Retries {
				if err := d.Sleep(ctx, backoff(attempt, 0)); err != nil {
					return StepReport{}, err
				}
				continue
			}
			return StepReport{Outcome: "unknown"}, nil
		case 5:
			return d.reconcile(ctx, file, p), nil
		case 2:
			d.Log.Error("aimem refused the driver's command as usage", "operation", st.Operation)
			return StepReport{Outcome: "unknown"}, nil
		default:
			return StepReport{Outcome: "unknown"}, nil
		}
	}
}

// reconcile reads aimem's receipt for a step whose reply was lost (exit 5).
func (d *Driver) reconcile(ctx context.Context, file string, p *pendingStep) StepReport {
	code, out, err := d.Aimem.Reservation(ctx, file, nil, "receipt", p.Step.Operation, "--task", p.Request.TaskID,
		"--key", p.Step.RequestKey)
	if err != nil || code != 0 {
		return StepReport{Outcome: "unknown"}
	}
	var r struct {
		State string `json:"state"`
	}
	if json.Unmarshal(out, &r) != nil {
		return StepReport{Outcome: "unknown"}
	}
	switch r.State {
	case "committed":
		return StepReport{Outcome: "committed"}
	case "not_committed":
		return StepReport{Outcome: "refused", Code: "not_committed"}
	}
	return StepReport{Outcome: "unknown"}
}

var reportCodeShape = regexp.MustCompile(`^[a-z][a-z0-9_]{0,63}$`)

// refusalOf is the code of aimem's refusal envelope, or a generic one.
func refusalOf(out []byte) string {
	var env struct {
		Code string `json:"code"`
	}
	if json.Unmarshal(out, &env) == nil && reportCodeShape.MatchString(env.Code) {
		return env.Code
	}
	return "refused"
}

// contentFields are the fields of aimem's task content (aimem's TaskContent
// at 8ffea1a): a write replaces all of them, so the body carries every one
// as read, except those aicrew owns.
var contentFields = []string{"title", "objective", "acceptance_criteria", "non_goals", "state", "assignee", "blocker",
	"dependencies", "candidate_refs", "evidence_refs", "next_action", "archived", "epic"}

// resultNote names the attempt and the member on a submitted result's
// reference (1aad G2): the same words as the identity reference a finalize
// carries in its terminal evidence.
func resultNote(attemptID, agentID string) string {
	return "aicrew attempt " + attemptID + " by member " + agentID
}

// composeBody builds the reservation body for st: the begin response's
// values, the proof, and for update, release and finalize the complete
// content, with only the state, the blocker and a submitted result's
// reference, noted with note, set by aicrew.
func composeBody(st *Step, proof string, task *TaskDoc, note string) (map[string]any, error) {
	body := map[string]any{"expected_revision": st.ExpectedRevision}
	if st.ReservationID != "" {
		body["reservation_id"], body["fence"] = st.ReservationID, st.Fence
	}
	if len(st.Holder) > 0 {
		body["holder"] = st.Holder
	}
	if proof != "" {
		body["coordination_proof"] = proof
	}
	switch st.Operation {
	case "claim", "transfer":
		return body, nil
	case "update":
		body["intent"] = st.Intent
	case "release":
		body["reason"] = st.Reason
		if st.Reason == "" {
			body["reason"] = "offer released"
		}
	case "finalize":
		body["reason"], body["terminal_evidence"] = st.Reason, st.TerminalEvidence
	default:
		return nil, fmt.Errorf("unknown step operation %q", st.Operation)
	}
	if task == nil {
		return nil, fmt.Errorf("a %s carries the task's content", st.Operation)
	}
	content := map[string]json.RawMessage{}
	for _, f := range contentFields {
		if v, ok := task.Fields[f]; ok {
			content[f] = v
		}
	}
	target := st.TargetState
	if target == "" && st.Operation == "release" {
		target = "READY" // an offer never accepted returns the task
	}
	content["state"], _ = json.Marshal(target)
	content["blocker"], _ = json.Marshal(st.Blocker)
	if st.ResultRef != "" {
		refs, err := withRef(content["candidate_refs"], st.ResultRef, note)
		if err != nil {
			return nil, err
		}
		content["candidate_refs"] = refs
	}
	body["content"] = content
	return body, nil
}

// withRef appends a result reference with its note to candidate_refs
// unless that reference is there with that note.
func withRef(raw json.RawMessage, ref, note string) (json.RawMessage, error) {
	var refs []map[string]any
	if len(raw) > 0 && string(raw) != "null" {
		if err := json.Unmarshal(raw, &refs); err != nil {
			return nil, fmt.Errorf("the task's candidate_refs: %w", err)
		}
	}
	for _, r := range refs {
		if r["ref"] == ref && r["note"] == note {
			return json.Marshal(refs)
		}
	}
	kind := "text"
	if strings.HasPrefix(ref, "https://") || strings.HasPrefix(ref, "http://") {
		kind = "url"
	}
	refs = append(refs, map[string]any{"kind": kind, "ref": ref, "note": note})
	return json.Marshal(refs)
}
