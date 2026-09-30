package server

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/BlackVS/aicrew/internal/store"
)

// coordFixture is the part of aimem's coordination-v1 matrix (testdata) the
// tests replay.
type coordFixture struct {
	Bounds struct {
		MaxResponseBytes int `json:"coordination_max_response_bytes"`
	} `json:"bounds"`
	Digests struct {
		RequestKey struct {
			Cases []struct{ Raw, Digest string } `json:"cases"`
		} `json:"request_key"`
		Proof struct {
			Cases []struct{ Proof, Digest string } `json:"cases"`
		} `json:"proof"`
	} `json:"digests"`
	SampleSecrets map[string]string `json:"sample_secrets"`
	Version       struct {
		Cases []struct {
			Case     string          `json:"case"`
			Header   *string         `json:"header"`
			Body     json.RawMessage `json:"body"`
			Accepted bool            `json:"accepted"`
		} `json:"cases"`
	} `json:"coordination_version"`
	FactKinds map[string]struct {
		Operation string   `json:"operation"`
		Roles     []string `json:"roles"`
		Requires  []string `json:"requires"`
	} `json:"fact_kinds"`
	Exchanges []struct {
		Case     string          `json:"case"`
		Request  json.RawMessage `json:"request"`
		Response struct {
			Status int             `json:"status"`
			Body   json.RawMessage `json:"body"`
		} `json:"response"`
	} `json:"exchanges"`
	Inactive []struct {
		State    string `json:"state"`
		Response struct {
			Body json.RawMessage `json:"body"`
		} `json:"response"`
	} `json:"inactive"`
	ProcessPin struct {
		Kinds     []string `json:"kinds"`
		Fields    []string `json:"fields"`
		Malformed []struct {
			Case    string          `json:"case"`
			Process json.RawMessage `json:"process"`
		} `json:"malformed"`
	} `json:"process_pin"`
}

func loadCoordFixture(t *testing.T) coordFixture {
	t.Helper()
	raw, err := os.ReadFile("testdata/coordination-v1/examples.json")
	if err != nil {
		t.Fatal(err)
	}
	var fx coordFixture
	if err := json.Unmarshal(raw, &fx); err != nil {
		t.Fatal(err)
	}
	return fx
}

func strictDecode(raw []byte, v any) error {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return err
	}
	if dec.Decode(&struct{}{}) != io.EOF {
		return errors.New("trailing data")
	}
	return nil
}

func k1(key string) string {
	sum := sha256.Sum256([]byte(key))
	return "k1_" + base64.RawURLEncoding.EncodeToString(sum[:])
}

// e1 is coordination.v1's evidence digest (C5-w3), written here from the
// fixture's scheme so the test does not check aicrew's digest against itself.
func e1(refs []string) string {
	h := sha256.New()
	for _, r := range refs {
		h.Write([]byte{byte(len(r) >> 24), byte(len(r) >> 16), byte(len(r) >> 8), byte(len(r))})
		h.Write([]byte(r))
	}
	return "e1_" + base64.RawURLEncoding.EncodeToString(h.Sum(nil))
}

var mirrorEvidenceDigest = regexp.MustCompile(`^e1_[A-Za-z0-9_-]{43}$`)

func p1(proof string) string {
	sum := sha256.Sum256([]byte(proof))
	return "p1_" + base64.RawURLEncoding.EncodeToString(sum[:])
}

// The route's request and reply types take exactly the fixture's shapes, and
// its digests are the contract's.
func TestCoordinationFixtureShapes(t *testing.T) {
	fx := loadCoordFixture(t)
	if len(fx.Exchanges) != 6 {
		t.Fatalf("the fixture has %d exchanges", len(fx.Exchanges))
	}
	pinned := map[string]bool{}
	for _, k := range fx.ProcessPin.Kinds {
		pinned[k] = true
	}
	for _, ex := range fx.Exchanges {
		var req coordinationRequest
		if err := strictDecode(ex.Request, &req); err != nil || req.Version == nil || *req.Version != 1 {
			t.Fatalf("%s: request %s: %v", ex.Case, ex.Request, err)
		}
		var reply coordinationReply
		if err := strictDecode(ex.Response.Body, &reply); err != nil || !reply.Active || reply.Fact.Kind != ex.Case {
			t.Fatalf("%s: reply: %v", ex.Case, err)
		}
		// Our reply, re-encoded, is the fixture's byte for byte in content.
		ours, _ := json.Marshal(reply)
		var a, b any
		_ = json.Unmarshal(ours, &a)
		_ = json.Unmarshal(ex.Response.Body, &b)
		if fmt.Sprint(a) != fmt.Sprint(b) {
			t.Fatalf("%s: re-encoded reply\n%s\ndiffers from the fixture\n%s", ex.Case, ours, ex.Response.Body)
		}
		if (reply.Fact.Process != nil) != pinned[ex.Case] {
			t.Fatalf("%s: process present = %v", ex.Case, reply.Fact.Process != nil)
		}
		if p := reply.Fact.Process; p != nil && !store.ValidProcessIdentity(store.ProcessIdentity{
			Repository: p.Repo, Commit: p.Commit, Manifest: p.Manifest}) {
			t.Fatalf("%s: the fixture's pin fails aicrew's validator: %+v", ex.Case, p)
		}
	}
	for _, in := range fx.Inactive {
		var reply inactiveReply
		if err := strictDecode(in.Response.Body, &reply); err != nil || reply.Active {
			t.Fatalf("inactive %s: %v", in.State, err)
		}
	}
	for _, c := range fx.Digests.RequestKey.Cases {
		if got := k1(c.Raw); got != c.Digest {
			t.Fatalf("k1(%q) = %s, want %s", c.Raw, got, c.Digest)
		}
	}
	for _, c := range fx.Digests.Proof.Cases {
		proof := fx.SampleSecrets[strings.Trim(c.Proof, "{}")]
		if got := p1(proof); got != c.Digest {
			t.Fatalf("p1(%s) = %s, want %s", c.Proof, got, c.Digest)
		}
	}
	if strings.Join(fx.ProcessPin.Fields, ",") != "repo,commit,manifest" {
		t.Fatalf("pin fields = %v", fx.ProcessPin.Fields)
	}
	// Every malformed pin the fixture lists is refused: a pin with another
	// field cannot be carried by aicrew's pin at all, and the others fail
	// aicrew's validator.
	for _, m := range fx.ProcessPin.Malformed {
		var pin processPin
		if err := strictDecode(m.Process, &pin); err != nil {
			continue
		}
		if store.ValidProcessIdentity(store.ProcessIdentity{Repository: pin.Repo, Commit: pin.Commit, Manifest: pin.Manifest}) {
			t.Fatalf("malformed pin %s accepted: %s", m.Case, m.Process)
		}
	}
}

// The largest reply the route can give stays within aimem's read bound.
func TestCoordinationReplySize(t *testing.T) {
	fx := loadCoordFixture(t)
	long := func(c string, n int) string { return strings.Repeat(c, n) }
	id := long("i", 128)
	reply := coordinationReply{Nonce: "n-" + long("0", 32), Active: true, ServiceID: id, HubID: id,
		Fact: coordinationFact{Kind: "accepted_for_finalization", Operation: "finalize", TaskID: id,
			RequestKeyDigest: k1("key"), Member: factMember{UserID: id, AgentID: id, TeamID: id, Role: "independent",
				SessionID: id, Generation: strconv.FormatInt(1<<62, 10)},
			OfferRef: "aicrew-offer-" + id, AttemptRef: "aicrew-attempt-" + id,
			IntendedWorker: &factWorker{UserID: id, AgentID: id},
			Process:        &processPin{Repo: "https://" + long("r", 504), Commit: long("a", 40), Manifest: long("m", 256)},
			EvidenceDigest: e1([]string{"a"}), ExpiresAt: "2026-09-28T12:00:00Z"}}
	b, _ := json.Marshal(reply)
	if len(b) > fx.Bounds.MaxResponseBytes {
		t.Fatalf("largest reply is %d bytes; aimem reads at most %d", len(b), fx.Bounds.MaxResponseBytes)
	}
}

// coordEnv is a running service, a team on the fixture's hub with a
// coordinator, a worker and an independent member in session, a coordination
// credential for aimem, and a fake aimem that asks the route about every
// coordinated step before it commits.
type coordEnv struct {
	*running
	op                  store.Caller
	bearer              string
	team                store.Team
	lead, worker, indep member
	aimem               *fakeAimem
}

type member struct {
	agent  store.Agent
	caller store.Caller
	sess   store.Session
	// token is the member's session token: members enter through the
	// proof flow, as a client does.
	token string
}

const coordHub, coordService = "hub-example", "aicrew-example"

var coordPin = store.TrustedProcess{
	Identity: store.ProcessIdentity{Repository: "https://git.example/team/process.git",
		Commit: "3f2a9c1e5b7d4a6c8e0f1a2b3c4d5e6f7a8b9c0d", Manifest: "process/manifest.json"},
	InstructionDigest: "sha256:worker-instructions-v1",
}

func setupCoordination(t *testing.T) *coordEnv {
	t.Helper()
	return setupCoordinationWith(t, true)
}

// setupCoordinationWith is setupCoordination, with the fake aimem's read
// scope given to the service or not.
func setupCoordinationWith(t *testing.T, withReader bool) *coordEnv {
	t.Helper()
	ctx := context.Background()
	aimem := &fakeAimem{t: t, current: coordPin.Identity, holds: map[string]*fakeHold{}, revision: 3,
		kinds: loadCoordFixture(t).FactKinds, receipts: map[string]store.ScopeReceiptLookup{},
		byKey: map[string]store.ScopeReceiptLookup{}}
	r := startWith(t, coordService, func(s *Server) {
		switch {
		case withReader && readOverHTTPS:
			s.reader = httpsReadScope(t, aimem)
		case withReader:
			s.reader = aimem
		}
	})
	op, err := store.OperatorCaller("op-test")
	if err != nil {
		t.Fatal(err)
	}
	team, err := r.store.CreateTeam(ctx, op, "team", store.NewTeam{Name: "crew",
		Projects: []store.ProjectRef{{HubID: coordHub, ProjectID: "project-example"}}})
	if err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", r.storePath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	join := func(label string, role store.Role) member {
		a, err := r.store.CreateAgent(ctx, op, "agent-"+label, store.NewAgent{Label: label})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := r.store.AddMember(ctx, op, "member-"+label, team.ID, a.ID, role); err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec(`UPDATE agents SET linked_hub_id = ?, linked_user_id = ?, linked_token_id = ? WHERE id = ?`,
			coordHub, "user-"+label, "token-"+label, a.ID); err != nil {
			t.Fatal(err)
		}
		c, err := store.AgentCaller(a.ID)
		if err != nil {
			t.Fatal(err)
		}
		entry, secrets := enterAs(t, r, a, label, team.ID)
		return member{agent: a, caller: c, sess: entry.Session, token: secrets.Token.Reveal()}
	}
	e := &coordEnv{running: r, op: op, team: team,
		lead: join("lead", store.RoleCoordinator), worker: join("worker", store.RoleWorker),
		indep: join("indep", store.RoleIndependent)}
	_, e.bearer, err = r.store.IssueIntrospectionCredential(ctx, op, "cred", coordHub)
	if err != nil {
		t.Fatal(err)
	}
	aimem.env, e.aimem = e, aimem
	return e
}

// enterAs enters agent a into the team through the proof flow, with a
// verifier that vouches for a's linked identity, and returns the entry and
// its secrets.
func enterAs(t *testing.T, r *running, a store.Agent, label, teamID string) (store.SessionEntry, store.EntrySecrets) {
	t.Helper()
	ctx := context.Background()
	ch, err := r.store.IssueAgentChallenge(ctx, store.Caller{}, "challenge-"+label+"-"+strconv.Itoa(entries), a.ID)
	if err != nil {
		t.Fatal(err)
	}
	entries++
	receipt := fmt.Sprintf("amr1_%s-%043d", label, entries)
	v := &fakeVerifier{ids: map[string]store.VerifiedIdentity{
		receipt: {HubID: coordHub, UserID: "user-" + label, TokenID: "token-" + label}}}
	entry, secrets, err := r.store.EnterSession(ctx, v, "enter-"+label, ch.ID, store.NewSecret(receipt), teamID, coordService)
	if err != nil {
		t.Fatalf("enter %s: %v", label, err)
	}
	return entry, secrets
}

var entries int

// ask sends one coordination request and returns the status and raw reply.
func (e *coordEnv) ask(t *testing.T, bearer, hub, nonce, proof string) (int, []byte) {
	t.Helper()
	body, _ := json.Marshal(map[string]any{"version": 1, "hub_id": hub, "nonce": nonce, "proof": proof})
	req, err := http.NewRequest(http.MethodPost, e.url(CoordinationPath), bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+bearer)
	req.Header.Set(CoordinationVersionHeader, "1")
	req.Header.Set("Content-Type", "application/json")
	resp, err := e.client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	return resp.StatusCode, raw
}

type fakeHold struct {
	id      string
	fence   int
	workRef string
	worker  *factWorker
	active  bool
}

// seenFact is a fact the fake aimem accepted, with the raw reply it came in.
type seenFact struct {
	op    store.ReservationOp
	proof string
	fact  coordinationFact
	raw   []byte
}

// fakeAimem is the reservation service under test: before it commits a
// coordinated step, it asks aicrew's route about the proof and applies
// aimem's acceptance checks (coordination.v1, "Acceptance by aimem"), then
// the process pin against the project's current selection.
type fakeAimem struct {
	t        *testing.T
	env      *coordEnv
	mu       sync.Mutex
	current  store.ProcessIdentity
	holds    map[string]*fakeHold
	revision int64
	seq      int
	kinds    map[string]struct {
		Operation string   `json:"operation"`
		Roles     []string `json:"roles"`
		Requires  []string `json:"requires"`
	}
	seen    []seenFact
	refused []string
	// receipts is the read scope: the receipt committed under each proof,
	// by its p1_ digest, and byKey each update's, by its k1_ digest.
	receipts map[string]store.ScopeReceiptLookup
	byKey    map[string]store.ScopeReceiptLookup
	// before runs just before aimem asks aicrew, to change state mid-step.
	before func()
}

func (f *fakeAimem) refuse(code, why string) (store.ReservationResult, error) {
	f.refused = append(f.refused, code+": "+why)
	return store.ReservationResult{}, &store.ReservationRefusal{Code: code, Message: why}
}

func (f *fakeAimem) Mutate(_ context.Context, op store.ReservationOp, req store.ReservationRequest) (store.ReservationResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	h := f.holds[req.Task.TaskID]
	if op == store.ReservationFinalize && len(req.TerminalEvidence) == 0 {
		// aimem's ledger: finalizing to DONE needs terminal evidence.
		return f.refuse("invalid_request", "DONE requires terminal evidence of reviewed delivery and human merge")
	}
	if op != store.ReservationUpdate {
		if f.before != nil {
			f.before()
		}
		fact, raw, code, why := f.verify(op, req, h)
		if code != "" {
			return f.refuse(code, why)
		}
		f.seen = append(f.seen, seenFact{op: op, proof: req.CoordinationProof, fact: fact, raw: raw})
		if fact.IntendedWorker != nil {
			defer func() { f.holds[req.Task.TaskID].worker = fact.IntendedWorker }()
		}
	} else if req.CoordinationProof != "" {
		return f.refuse("invalid_request", "an update carries no coordination proof")
	}
	f.seq++
	f.revision++
	switch op {
	case store.ReservationClaim:
		h = &fakeHold{id: fmt.Sprintf("res-%d", f.seq), fence: 1, workRef: req.Holder.WorkRef, active: true}
		f.holds[req.Task.TaskID] = h
	case store.ReservationTransfer:
		h.fence++
		h.workRef = req.Holder.WorkRef
	case store.ReservationRelease, store.ReservationFinalize:
		h.fence++
		h.active = false
	}
	st := store.ReservationState{Fence: strconv.Itoa(h.fence), Active: h.active}
	if h.active {
		st.ID, st.HolderMode, st.OwnWorkRef = h.id, "external", h.workRef
	}
	return store.ReservationResult{Receipt: store.ReservationReceipt{ID: fmt.Sprintf("rcpt-%d", f.seq),
		State: store.ReceiptCommitted, Operation: string(op), RequestKey: req.RequestKey, VerifiedMode: "team"},
		TaskRevision: f.revision, Reservation: st}, nil
}

func (f *fakeAimem) Receipt(context.Context, store.TaskRef, store.ReservationOp, string) (store.ReceiptLookup, error) {
	return store.ReceiptLookup{State: store.ReceiptNotCommitted}, nil
}

func (f *fakeAimem) Status(context.Context, store.TaskRef) (store.HoldStatus, error) {
	return store.HoldStatus{State: "none"}, nil
}

var (
	mirrorCommit = regexp.MustCompile(`^[0-9a-f]{40}$`)
	mirrorNonce  = 0
)

// mirrorRef is aimem's process.Ref.Validate at 8ac4ef1, repeated here so the
// test does not check aicrew's validator against itself.
func mirrorRef(p processPin) bool {
	r := p.Repo
	if r == "" || len(r) > 512 || strings.ContainsAny(r, " \t\r\n\"'") || strings.HasPrefix(r, "-") {
		return false
	}
	if !strings.HasPrefix(r, "https://") && !strings.HasPrefix(r, "ssh://") && !strings.HasPrefix(r, "git@") {
		return false
	}
	m := p.Manifest
	if !mirrorCommit.MatchString(p.Commit) || m == "" || len(m) > 256 || strings.HasPrefix(m, "/") ||
		strings.Contains(m, "\\") || strings.ContainsAny(m, " \t\r\n") {
		return false
	}
	return path.Clean(m) == m && m != "." && !strings.HasPrefix(m, "../") && !strings.Contains(m, "/../")
}

// verify asks the route and applies aimem's checks. It returns the fact, or
// the refusal aimem would give: context_unavailable for a peer failure,
// coordination_rejected for a fact that fails, process_mismatch for a pin
// that does not match the current selection.
func (f *fakeAimem) verify(op store.ReservationOp, req store.ReservationRequest, h *fakeHold) (coordinationFact, []byte, string, string) {
	mirrorNonce++
	nonce := fmt.Sprintf("n-%032x", mirrorNonce)
	status, raw := f.env.ask(f.t, f.env.bearer, coordHub, nonce, req.CoordinationProof)
	if status != http.StatusOK || len(raw) > 16384 {
		return coordinationFact{}, raw, "context_unavailable", fmt.Sprintf("status %d, %d bytes", status, len(raw))
	}
	var inactive inactiveReply
	if strictDecode(raw, &inactive) == nil {
		if inactive.Nonce != nonce || inactive.Active {
			return coordinationFact{}, raw, "context_unavailable", "inactive reply of the wrong shape"
		}
		return coordinationFact{}, raw, "coordination_rejected", "inactive"
	}
	var reply coordinationReply
	if err := strictDecode(raw, &reply); err != nil || !reply.Active {
		return coordinationFact{}, raw, "context_unavailable", fmt.Sprintf("reply shape: %v", err)
	}
	if reply.Nonce != nonce || reply.ServiceID != coordService || reply.HubID != coordHub {
		return coordinationFact{}, raw, "context_unavailable", "nonce, service or hub"
	}
	fact := reply.Fact
	kind, ok := f.kinds[fact.Kind]
	if !ok {
		return fact, raw, "context_unavailable", "unknown kind"
	}
	present := map[string]bool{"offer_ref": fact.OfferRef != "", "attempt_ref": fact.AttemptRef != "",
		"intended_worker": fact.IntendedWorker != nil, "process": fact.Process != nil,
		"evidence_digest": fact.EvidenceDigest != ""}
	for field := range present {
		required := false
		for _, r := range kind.Requires {
			required = required || r == field
		}
		if present[field] != required {
			return fact, raw, "context_unavailable", fmt.Sprintf("%s on %s: present %v", field, fact.Kind, present[field])
		}
	}
	if fact.Process != nil && !mirrorRef(*fact.Process) {
		return fact, raw, "context_unavailable", "malformed process"
	}
	if fact.EvidenceDigest != "" && !mirrorEvidenceDigest.MatchString(fact.EvidenceDigest) {
		return fact, raw, "context_unavailable", "malformed evidence digest"
	}
	expires, err := time.Parse(time.RFC3339, fact.ExpiresAt)
	roleOK := false
	for _, r := range kind.Roles {
		roleOK = roleOK || r == fact.Member.Role
	}
	switch {
	case err != nil || !time.Now().Before(expires):
		return fact, raw, "coordination_rejected", "expired"
	case fact.Operation != string(op) || kind.Operation != string(op) || !roleOK:
		return fact, raw, "coordination_rejected", "operation, kind or role"
	case fact.TaskID != req.Task.TaskID:
		return fact, raw, "coordination_rejected", "task"
	case fact.RequestKeyDigest != k1(req.RequestKey):
		return fact, raw, "coordination_rejected", "request key digest"
	}
	holdRef := ""
	if h != nil && h.active {
		holdRef = h.workRef
	}
	var refsOK bool
	switch fact.Kind {
	case "offer":
		refsOK = req.Holder != nil && req.Holder.WorkRef == fact.OfferRef
	case "accepted_attempt":
		refsOK = holdRef == fact.OfferRef && req.Holder != nil && req.Holder.WorkRef == fact.AttemptRef &&
			h.worker != nil && h.worker.UserID == fact.Member.UserID && h.worker.AgentID == fact.Member.AgentID
	case "never_accepted":
		refsOK = holdRef == fact.OfferRef
	case "stopped", "accepted_for_finalization":
		refsOK = holdRef == fact.AttemptRef
	case "independent_claim":
		refsOK = req.Holder != nil && req.Holder.WorkRef == fact.AttemptRef
	}
	if !refsOK {
		return fact, raw, "coordination_rejected", "references"
	}
	if p := fact.Process; p != nil && (p.Repo != f.current.Repository || p.Commit != f.current.Commit ||
		p.Manifest != f.current.Manifest) {
		return fact, raw, "process_mismatch", "the pin is not the current selection"
	}
	if fact.Kind == "accepted_for_finalization" && fact.EvidenceDigest != e1(req.TerminalEvidence) {
		return fact, raw, "evidence_mismatch", "the terminal evidence is not the confirmed evidence"
	}
	return fact, raw, "", ""
}

func (e *coordEnv) wantMember(t *testing.T, got factMember, m member, role string) {
	t.Helper()
	want := factMember{UserID: "user-" + m.agent.Label, AgentID: m.agent.ID, TeamID: e.team.ID, Role: role,
		SessionID: m.sess.ID, Generation: strconv.FormatInt(m.sess.Generation, 10)}
	if got != want {
		t.Fatalf("member %+v, want %+v", got, want)
	}
}

func (e *coordEnv) task(id string) store.TaskRef {
	return store.TaskRef{HubID: coordHub, ProjectID: "project-example", TaskID: id}
}

func (e *coordEnv) offer(t *testing.T, key, taskID string) (store.Attempt, error) {
	t.Helper()
	return e.store.OfferTask(context.Background(), e.lead.caller, e.aimem, key, store.OfferRequest{
		SessionID: e.lead.sess.ID, Generation: e.lead.sess.Generation, WorkerAgentID: e.worker.agent.ID,
		Task: e.task(taskID), ExpectedRevision: 3, BaseCommit: "base-1", Branch: "work/" + taskID,
		Process: coordPin, ExpiresAt: time.Now().Add(time.Hour)})
}

func (e *coordEnv) last(t *testing.T) seenFact {
	t.Helper()
	if len(e.aimem.seen) == 0 {
		t.Fatalf("aimem accepted no fact; refused %v", e.aimem.refused)
	}
	return e.aimem.seen[len(e.aimem.seen)-1]
}

func delivery(tag string) store.TrustedDelivery {
	return store.TrustedDelivery{Required: store.DevelopmentDelivery, Evidence: []store.Evidence{
		{Kind: "reviewed_head", Ref: "https://example.invalid/pull/1#review-" + tag},
		{Kind: "human_merge", Ref: "https://example.invalid/commit/merge-" + tag},
		{Kind: "post_merge_ci", Ref: "https://example.invalid/actions/runs/" + tag},
	}}
}

// Every coordinated step, driven through the real store operations, is
// verified by aimem through the route before it commits: each fact passes
// aimem's checks, names the acting member, carries the pin exactly on the
// kinds that start work, and never carries the proof.
func TestCoordinationEndToEnd(t *testing.T) {
	ctx := context.Background()
	e := setupCoordination(t)
	s := e.store
	pin := &processPin{Repo: coordPin.Identity.Repository, Commit: coordPin.Identity.Commit,
		Manifest: coordPin.Identity.Manifest}
	check := func(kind, role string, m member, wantPin bool) seenFact {
		t.Helper()
		got := e.last(t)
		if got.fact.Kind != kind {
			t.Fatalf("last fact is %s, want %s (refused %v)", got.fact.Kind, kind, e.aimem.refused)
		}
		e.wantMember(t, got.fact.Member, m, role)
		if wantPin != (got.fact.Process != nil) || (wantPin && *got.fact.Process != *pin) {
			t.Fatalf("%s: process %+v", kind, got.fact.Process)
		}
		if bytes.Contains(got.raw, []byte(got.proof)) {
			t.Fatalf("%s: the reply carries the proof", kind)
		}
		return got
	}

	a, err := e.offer(t, "offer", "task-1")
	if err != nil {
		t.Fatalf("offer: %v (refused %v)", err, e.aimem.refused)
	}
	check("offer", "coordinator", e.lead, true)
	if w := e.last(t).fact.IntendedWorker; w == nil || w.AgentID != e.worker.agent.ID || w.UserID != "user-worker" {
		t.Fatalf("intended worker %+v", w)
	}
	a, err = s.AcceptOffer(ctx, e.worker.caller, e.aimem, "accept", a.ID, store.AcceptRequest{
		SessionID: e.worker.sess.ID, Generation: e.worker.sess.Generation, Selected: coordPin,
		InstructionDigest: coordPin.InstructionDigest})
	if err != nil {
		t.Fatalf("accept: %v (refused %v)", err, e.aimem.refused)
	}
	check("accepted_attempt", "worker", e.worker, true)
	if _, err := s.UpdateWork(ctx, e.worker.caller, e.aimem, "submit", a.ID, store.WorkUpdate{
		SessionID: e.worker.sess.ID, Generation: e.worker.sess.Generation, Intent: store.IntentSubmit,
		Detail: "https://example.invalid/pull/1"}); err != nil {
		t.Fatalf("submit: %v", err)
	}
	if _, err := s.ReviewResult(ctx, e.lead.caller, "review", a.ID, store.ResultReview{SessionID: e.lead.sess.ID,
		Generation: e.lead.sess.Generation, ResultSeq: 1, Decision: store.ReviewAccept}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.FinalizeWork(ctx, e.worker.caller, e.aimem, "finalize", a.ID, store.FinalizeRequest{
		SessionID: e.worker.sess.ID, Generation: e.worker.sess.Generation, ResultSeq: 1, Delivery: delivery("1")}); err != nil {
		t.Fatalf("finalize: %v (refused %v)", err, e.aimem.refused)
	}
	check("accepted_for_finalization", "worker", e.worker, false)

	c, err := s.ClaimTask(ctx, e.indep.caller, e.aimem, "claim", store.ClaimRequest{SessionID: e.indep.sess.ID,
		Generation: e.indep.sess.Generation, Task: e.task("task-2"), ExpectedRevision: 3, BaseCommit: "base-1",
		Branch: "work/task-2", Process: coordPin, InstructionDigest: coordPin.InstructionDigest})
	if err != nil {
		t.Fatalf("claim: %v (refused %v)", err, e.aimem.refused)
	}
	check("independent_claim", "independent", e.indep, true)
	if _, err := s.RequestStop(ctx, e.lead.caller, "stop", c.ID, store.StopRequest{SessionID: e.lead.sess.ID,
		Generation: e.lead.sess.Generation, Reason: "priorities changed"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ConfirmStop(ctx, e.indep.caller, "confirm", c.ID, e.indep.sess.ID, e.indep.sess.Generation); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ReleaseStopped(ctx, e.indep.caller, e.aimem, "release", c.ID, store.StopRelease{
		SessionID: e.indep.sess.ID, Generation: e.indep.sess.Generation, Target: store.ReleaseReady}); err != nil {
		t.Fatalf("stop release: %v (refused %v)", err, e.aimem.refused)
	}
	check("stopped", "independent", e.indep, false)

	w, err := e.offer(t, "offer-3", "task-3")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.ReleaseOffer(ctx, e.lead.caller, e.aimem, "withdraw", w.ID, e.lead.sess.ID, e.lead.sess.Generation); err != nil {
		t.Fatalf("withdraw: %v (refused %v)", err, e.aimem.refused)
	}
	check("never_accepted", "coordinator", e.lead, false)

	if len(e.aimem.refused) != 0 {
		t.Fatalf("aimem refused %v", e.aimem.refused)
	}
	// Each settled step's proof now answers inactive, with no reason.
	for _, seen := range e.aimem.seen {
		status, raw := e.ask(t, e.bearer, coordHub, "n-"+strings.Repeat("a", 32), seen.proof)
		if status != http.StatusOK || strings.TrimSpace(string(raw)) != `{"nonce":"n-`+strings.Repeat("a", 32)+`","active":false}` {
			t.Fatalf("%s after it settled: %d %s", seen.fact.Kind, status, raw)
		}
	}
	for _, secret := range []string{e.bearer, e.aimem.seen[0].proof} {
		if strings.Contains(e.logs.String(), secret) {
			t.Fatal("a secret reached the logs")
		}
	}
}

// A step whose fact stops being true before aimem asks is refused as a fact
// failure, and one pinned to another version than the current selection is
// refused as process_mismatch; either is final, and the attempt settles.
func TestCoordinationRefusals(t *testing.T) {
	ctx := context.Background()
	t.Run("fact no longer true", func(t *testing.T) {
		e := setupCoordination(t)
		e.aimem.before = func() {
			if _, err := e.store.StopSession(ctx, e.op, "stop-lead", e.lead.sess.ID); err != nil {
				t.Fatal(err)
			}
		}
		a, err := e.offer(t, "offer", "task-1")
		var refusal *store.ReservationRefusal
		if !errors.As(err, &refusal) || refusal.Code != "coordination_rejected" || a.State != store.AttemptClosed {
			t.Fatalf("offer after the coordinator left: %+v, %v (refused %v)", a, err, e.aimem.refused)
		}
	})
	t.Run("process moved", func(t *testing.T) {
		e := setupCoordination(t)
		e.aimem.current.Commit = "9b8a7c6d5e4f3a2b1c0d9e8f7a6b5c4d3e2f1a0b"
		c, err := e.store.ClaimTask(ctx, e.indep.caller, e.aimem, "claim", store.ClaimRequest{SessionID: e.indep.sess.ID,
			Generation: e.indep.sess.Generation, Task: e.task("task-2"), ExpectedRevision: 3, BaseCommit: "base-1",
			Branch: "work/task-2", Process: coordPin, InstructionDigest: coordPin.InstructionDigest})
		var refusal *store.ReservationRefusal
		if !errors.As(err, &refusal) || refusal.Code != "process_mismatch" || c.State != store.AttemptClosed {
			t.Fatalf("claim under a moved selection: %+v, %v", c, err)
		}
	})
}

// The route's request rules: the credential must permit coordination, the
// request must be well formed, both versions must be 1, and the hub must be
// the credential's. A refusal carries the envelope and evaluates no proof.
func TestCoordinationRequestRules(t *testing.T) {
	ctx := context.Background()
	e := setupCoordination(t)
	fx := loadCoordFixture(t)
	nonce := "n-" + strings.Repeat("b", 32)
	send := func(header *string, body []byte, bearer, contentType string) (int, string) {
		t.Helper()
		req, _ := http.NewRequest(http.MethodPost, e.url(CoordinationPath), bytes.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+bearer)
		if header != nil {
			req.Header.Set(CoordinationVersionHeader, *header)
		}
		req.Header.Set("Content-Type", contentType)
		resp, err := e.client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		raw, _ := io.ReadAll(resp.Body)
		var r struct {
			Code string `json:"code"`
		}
		_ = json.Unmarshal(raw, &r)
		if resp.StatusCode != http.StatusOK && r.Code == "" {
			t.Fatalf("refusal without an envelope: %d %s", resp.StatusCode, raw)
		}
		return resp.StatusCode, r.Code
	}
	for _, c := range fx.Version.Cases {
		body := `{"hub_id":"` + coordHub + `","nonce":"` + nonce + `","proof":"acp1_unknown"`
		if c.Body != nil && string(c.Body) != "null" {
			body += `,"version":` + string(c.Body)
		}
		status, code := send(c.Header, []byte(body+"}"), e.bearer, "application/json")
		if c.Accepted != (status == http.StatusOK) || (!c.Accepted && code != "unsupported_version") {
			t.Fatalf("version case %s: %d %s", c.Case, status, code)
		}
	}
	one := "1"
	valid := `{"version":1,"hub_id":"` + coordHub + `","nonce":"` + nonce + `","proof":"acp1_unknown"}`
	_, introOnly, err := e.store.IssueIntrospectionCredential(ctx, e.op, "intro-only", coordHub, store.OpIntrospection)
	if err != nil {
		t.Fatal(err)
	}
	_, coordOnly, err := e.store.IssueIntrospectionCredential(ctx, e.op, "coord-only", "hub-other", store.OpCoordination)
	if err != nil {
		t.Fatal(err)
	}
	for name, c := range map[string]struct {
		body, bearer, contentType string
		status                    int
		code                      string
	}{
		"introspection-only credential": {valid, introOnly, "application/json", 401, "peer_unauthenticated"},
		"unknown credential":            {valid, "aicrew_introspect_" + strings.Repeat("0", 64), "application/json", 401, "peer_unauthenticated"},
		"another hub's credential":      {valid, coordOnly, "application/json", 403, "peer_forbidden"},
		"text body":                     {valid, e.bearer, "text/plain", 400, "invalid_request"},
		"unknown field":                 {strings.Replace(valid, `"version"`, `"expected_member":"x","version"`, 1), e.bearer, "application/json", 400, "invalid_request"},
		"trailing data":                 {valid + " {}", e.bearer, "application/json", 400, "invalid_request"},
		"bad nonce":                     {strings.Replace(valid, nonce, "n-XYZ", 1), e.bearer, "application/json", 400, "invalid_request"},
		"unknown proof":                 {valid, e.bearer, "application/json", 200, ""},
	} {
		if status, code := send(&one, []byte(c.body), c.bearer, c.contentType); status != c.status || code != c.code {
			t.Errorf("%s: %d %q, want %d %q", name, status, code, c.status, c.code)
		}
	}
	// A body over the route's limit, declared: sent as headers only, so
	// nothing is left unread when aicrewd refuses on the length.
	status, raw := e.head(t, fmt.Sprintf("POST %s HTTP/1.1\r\nHost: x\r\nAuthorization: Bearer %s\r\nContent-Type: application/json\r\n%s: 1\r\nContent-Length: %d\r\n\r\n",
		CoordinationPath, e.bearer, CoordinationVersionHeader, maxCoordinationBody+1))
	if status != http.StatusBadRequest || !strings.Contains(string(raw), `"invalid_request"`) {
		t.Fatalf("declared oversized body: %d %s", status, raw)
	}
	// The introspection route refuses a coordination-only credential.
	status, _ = e.head(t, fmt.Sprintf("POST %s HTTP/1.1\r\nHost: x\r\nAuthorization: Bearer %s\r\nContent-Type: application/json\r\n%s: 1\r\nContent-Length: 0\r\n\r\n",
		IntrospectPath, coordOnly, VersionHeader))
	if status != http.StatusUnauthorized {
		t.Fatalf("introspection with a coordination-only credential: %d", status)
	}
}
