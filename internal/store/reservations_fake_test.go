package store

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"sync"
	"testing"
)

// This file holds the test-only reservation service. It has two parts, kept
// apart on purpose:
//
//   - Contract behavior, taken from aimem's reviewed reservation wire v1 and
//     its example matrix (testdata/reservation-v1, a verbatim copy): the
//     request and response shapes, receipt states, refusal codes with their
//     retryable flags, identical replay, fence advance and a closed
//     reservation's shape. This is reviewed design, not an observation of a
//     live aimem server.
//   - Simulated faults (simFault): a reply lost after commit, a failure
//     before commit, an unresolved receipt and an unavailable status read.
//     They model what a real network and server may do; nothing here claims
//     aimem behaves this way at a given moment.

// fixtureMatrix is the part of examples.json the tests use.
type fixtureMatrix struct {
	Version   int    `json:"version"`
	Status    string `json:"status"`
	Mutations []struct {
		Operation  string          `json:"operation"`
		RequestKey string          `json:"request_key"`
		Request    json.RawMessage `json:"request"`
		Response   json.RawMessage `json:"response"`
	} `json:"mutations"`
	Reconciliation []struct {
		Case         string `json:"case"`
		ReceiptState string `json:"receipt_state"`
		Replayed     bool   `json:"replayed"`
	} `json:"reconciliation"`
	StatusExample          json.RawMessage `json:"status_example"`
	RefusalEnvelopeExample json.RawMessage `json:"refusal_envelope_example"`
	Refusals               []struct {
		Case       string  `json:"case"`
		Code       string  `json:"code"`
		Retryable  bool    `json:"retryable"`
		ActiveMode *string `json:"active_mode"`
		NextAction string  `json:"next_action"`
	} `json:"refusals"`
}

var (
	fixtureOnce sync.Once
	fixture     fixtureMatrix
	fixtureErr  error
)

func loadFixture(t *testing.T) fixtureMatrix {
	t.Helper()
	fixtureOnce.Do(func() {
		raw, err := os.ReadFile(filepath.Join("testdata", "reservation-v1", "examples.json"))
		if err != nil {
			fixtureErr = err
			return
		}
		fixtureErr = json.Unmarshal(raw, &fixture)
	})
	if fixtureErr != nil {
		t.Fatalf("load reservation fixture: %v", fixtureErr)
	}
	return fixture
}

// fixtureRefusal returns the fixture's refusal for a case, as aimem would
// send it.
func fixtureRefusal(t *testing.T, refusalCase string) *ReservationRefusal {
	t.Helper()
	for _, r := range loadFixture(t).Refusals {
		if r.Case == refusalCase {
			ref := &ReservationRefusal{Code: r.Code, Retryable: r.Retryable, NextAction: r.NextAction,
				Message: "Refused as in the reservation fixture.", CorrelationID: "correlation-" + r.Case}
			if r.ActiveMode != nil {
				ref.ActiveMode = *r.ActiveMode
			}
			return ref
		}
	}
	t.Fatalf("no fixture refusal case %q", refusalCase)
	return nil
}

// simFault is one simulated failure, used once.
type simFault string

const (
	faultNone simFault = ""
	// faultLostReply commits the mutation, then loses the reply.
	faultLostReply simFault = "lost reply after commit"
	// faultBeforeCommit fails the call before anything is committed.
	faultBeforeCommit simFault = "failure before commit"
)

var errSimulatedTransport = errors.New("simulated transport failure")

type fakeHold struct {
	id      string
	fence   int
	workRef string
	active  bool
}

type fakeCall struct {
	Op  ReservationOp
	Key string
}

type fakeReservations struct {
	t        *testing.T
	mu       sync.Mutex
	holds    map[string]*fakeHold
	revision map[string]int64
	receipts map[string]ReservationResult
	inputs   map[string][]byte
	nextID   int

	// Simulated behavior, set by tests.
	faults     map[ReservationOp]simFault
	refuse     map[ReservationOp]*ReservationRefusal
	unresolved map[string]bool // request keys whose receipt lookup stays unresolved
	statusErr  error
	receiptErr error
	onMutate   func(op ReservationOp, req ReservationRequest)
	// garble, if set, alters one committed reply before it is returned: a
	// simulated inconsistent reply. What is recorded stays correct.
	garble func(res *ReservationResult)

	calls   []fakeCall
	lookups int
}

func newFakeReservations(t *testing.T) *fakeReservations {
	loadFixture(t)
	return &fakeReservations{
		t: t, holds: map[string]*fakeHold{}, revision: map[string]int64{},
		receipts: map[string]ReservationResult{}, inputs: map[string][]byte{},
		faults: map[ReservationOp]simFault{}, refuse: map[ReservationOp]*ReservationRefusal{},
		unresolved: map[string]bool{},
	}
}

func taskKey(t TaskRef) string { return taskName(t) }

func (f *fakeReservations) Mutate(_ context.Context, op ReservationOp, req ReservationRequest) (ReservationResult, error) {
	if f.onMutate != nil {
		f.onMutate(op, req)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, fakeCall{op, req.RequestKey})
	fault := f.faults[op]
	delete(f.faults, op)
	if fault == faultBeforeCommit {
		return ReservationResult{}, errSimulatedTransport
	}
	if ref := f.refuse[op]; ref != nil {
		delete(f.refuse, op)
		return ReservationResult{}, ref
	}

	// Contract behavior: identical replay returns the recorded outcome;
	// changed input under the same key is a conflict.
	id := string(op) + "|" + req.RequestKey
	body, _ := json.Marshal(req)
	if prev, ok := f.receipts[id]; ok {
		if !bytes.Equal(body, f.inputs[id]) {
			return ReservationResult{}, &ReservationRefusal{Code: "idempotency_conflict", Message: "Changed input."}
		}
		prev.Receipt.Replayed = true
		return prev, nil
	}
	task := taskKey(req.Task)
	h := f.holds[task]
	if f.revision[task] == 0 {
		f.revision[task] = req.ExpectedRevision
	}
	switch op {
	case ReservationClaim:
		if h != nil && h.active {
			return ReservationResult{}, fixtureRefusal(f.t, "competing_hold")
		}
		f.nextID++
		fence := 1
		if h != nil {
			fence = h.fence + 1
		}
		h = &fakeHold{id: fmt.Sprintf("reservation-%d", f.nextID), fence: fence, workRef: req.Holder.WorkRef, active: true}
		f.holds[task] = h
	case ReservationTransfer, ReservationRelease:
		if h == nil || !h.active || h.id != req.ReservationID || strconv.Itoa(h.fence) != req.Fence {
			return ReservationResult{}, fixtureRefusal(f.t, "stale_worker")
		}
		h.fence++
		if op == ReservationTransfer {
			h.workRef = req.Holder.WorkRef
		} else {
			h.active, h.id, h.workRef = false, "", ""
			f.revision[task]++
		}
	}
	res := ReservationResult{
		Receipt: ReservationReceipt{
			ID: fmt.Sprintf("receipt-%s-%d", op, len(f.receipts)+1), State: ReceiptCommitted,
			Operation: string(op), RequestKey: req.RequestKey, ActorID: "actor-example", VerifiedMode: "team",
		},
		TaskRevision: f.revision[task],
		Reservation:  ReservationState{ID: h.id, Fence: strconv.Itoa(h.fence), Active: h.active},
	}
	if h.active {
		res.Reservation.HolderMode, res.Reservation.OwnWorkRef = "external", h.workRef
	}
	f.receipts[id], f.inputs[id] = res, body
	if fault == faultLostReply {
		return ReservationResult{}, errSimulatedTransport
	}
	if f.garble != nil {
		f.garble(&res)
		f.garble = nil
	}
	return res, nil
}

func (f *fakeReservations) Receipt(_ context.Context, _ TaskRef, op ReservationOp, key string) (ReceiptLookup, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lookups++
	if f.receiptErr != nil {
		return ReceiptLookup{}, f.receiptErr
	}
	if f.unresolved[key] {
		return ReceiptLookup{State: ReceiptUnresolved}, nil
	}
	if res, ok := f.receipts[string(op)+"|"+key]; ok {
		res.Receipt.Replayed = true
		return ReceiptLookup{State: ReceiptCommitted, Result: &res}, nil
	}
	return ReceiptLookup{State: ReceiptNotCommitted}, nil
}

func (f *fakeReservations) Status(_ context.Context, task TaskRef) (HoldStatus, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.statusErr != nil {
		return HoldStatus{}, f.statusErr
	}
	h := f.holds[taskKey(task)]
	if h == nil || !h.active {
		return HoldStatus{State: "none"}, nil
	}
	return HoldStatus{State: "held", ReservationID: h.id, Fence: strconv.Itoa(h.fence),
		OwnWorkRef: h.workRef, TaskRevision: f.revision[taskKey(task)]}, nil
}

// holdOf returns the fake's hold on a task.
func (f *fakeReservations) holdOf(task TaskRef) fakeHold {
	f.mu.Lock()
	defer f.mu.Unlock()
	if h := f.holds[taskKey(task)]; h != nil {
		return *h
	}
	return fakeHold{}
}

// recoveryRelease models an authorized recovery principal releasing a hold
// in aimem, outside aicrew.
func (f *fakeReservations) recoveryRelease(task TaskRef) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if h := f.holds[taskKey(task)]; h != nil {
		h.active, h.id, h.workRef = false, "", ""
		h.fence++
	}
}

// standaloneHold models a hold taken by someone outside aicrew.
func (f *fakeReservations) standaloneHold(task TaskRef, workRef string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.nextID++
	f.holds[taskKey(task)] = &fakeHold{id: fmt.Sprintf("reservation-%d", f.nextID), fence: 1, workRef: workRef, active: true}
}

func (f *fakeReservations) callLog() []fakeCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]fakeCall(nil), f.calls...)
}

// The fake and the store's port types follow the pinned fixture: every
// response and envelope field the fixture shows decodes into the port
// types without leftovers, the store sends only request fields the fixture
// knows, and refusal retryability comes from the fixture.
func TestReservationFixtureConformance(t *testing.T) {
	fx := loadFixture(t)
	if fx.Version != 1 || fx.Status != "proposal_only" {
		t.Fatalf("fixture version %d, status %q; expected version 1, proposal_only", fx.Version, fx.Status)
	}
	strict := func(raw json.RawMessage, v any) error {
		dec := json.NewDecoder(bytes.NewReader(raw))
		dec.DisallowUnknownFields()
		return dec.Decode(v)
	}
	// Request fields the fixture shows for each operation, across all of
	// its examples (a standalone claim has no coordination proof; an
	// external one does).
	known := map[ReservationOp]map[string]bool{}
	for _, m := range fx.Mutations {
		op := ReservationOp(m.Operation)
		if op != ReservationClaim && op != ReservationTransfer && op != ReservationRelease {
			continue
		}
		var res ReservationResult
		if err := strict(m.Response, &res); err != nil {
			t.Errorf("%s response does not fit ReservationResult: %v", m.Operation, err)
		}
		if res.Receipt.State != ReceiptCommitted || res.Receipt.RequestKey != m.RequestKey || res.Reservation.Fence == "" {
			t.Errorf("%s fixture response = %+v", m.Operation, res)
		}
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(m.Request, &fields); err != nil {
			t.Fatal(err)
		}
		if known[op] == nil {
			known[op] = map[string]bool{}
		}
		for f := range fields {
			known[op][f] = true
		}
	}
	for _, op := range []ReservationOp{ReservationClaim, ReservationTransfer, ReservationRelease} {
		if known[op] == nil {
			t.Errorf("fixture has no %s example", op)
			continue
		}
		a := Attempt{ID: "x", PendingOp: op, PendingKey: "k", ReservationID: "r", Fence: "1", TaskRevision: 3}
		ours, _ := json.Marshal(reservationRequest(a))
		var sent map[string]json.RawMessage
		if err := json.Unmarshal(ours, &sent); err != nil {
			t.Fatal(err)
		}
		for field := range sent {
			if !known[op][field] {
				t.Errorf("%s request field %q is not in the fixture", op, field)
			}
		}
	}
	var st HoldStatus
	if err := strict(fx.StatusExample, &st); err != nil || st.State != "held" {
		t.Errorf("status example = %+v, %v", st, err)
	}
	var env ReservationRefusal
	if err := strict(fx.RefusalEnvelopeExample, &env); err != nil || env.Code == "" {
		t.Errorf("refusal envelope = %+v, %v", env, err)
	}
	states := map[string]bool{}
	for _, r := range fx.Reconciliation {
		states[r.ReceiptState] = true
	}
	for _, want := range []string{ReceiptCommitted, ReceiptNotCommitted, ReceiptUnresolved} {
		if !states[want] {
			t.Errorf("fixture reconciliation has no %q case", want)
		}
	}
	// Refusal handling follows the fixture's retryable flag: only a
	// refusal that is not retryable is a known outcome.
	codes := []string{}
	for _, r := range fx.Refusals {
		got := classify(Attempt{PendingOp: ReservationClaim}, ReservationResult{}, fixtureRefusal(t, r.Case))
		want := outcomeRefused
		if r.Retryable {
			want = outcomeUnknown
		}
		if got.kind != want {
			t.Errorf("refusal %s (%s, retryable %v) classified %s, want %s", r.Case, r.Code, r.Retryable, got.kind, want)
		}
		codes = append(codes, r.Code)
	}
	sort.Strings(codes)
	if len(codes) == 0 {
		t.Fatal("fixture has no refusals")
	}
}
