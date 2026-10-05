package store

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"reflect"
	"sync"
	"testing"
	"time"
)

// fakeReader is aimem's read scope for this service: receipts by proof and,
// for proofless updates, by key, and the hold status, set by a test when the
// fake "aimem" commits a step.
type fakeReader struct {
	mu       sync.Mutex
	receipts map[string]ScopeReceiptLookup
	byKey    map[string]ScopeReceiptLookup
	hold     ScopeHold
	err      error
	reads    int
	// answered, if set, runs once after a lookup has taken its answer and
	// before it returns: the world may change while an answer is in flight.
	answered func()
}

func newFakeReader() *fakeReader {
	return &fakeReader{receipts: map[string]ScopeReceiptLookup{}, byKey: map[string]ScopeReceiptLookup{},
		hold: ScopeHold{State: ScopeNone}}
}

// commitUpdate records that aimem committed an update under request key key.
func (f *fakeReader) commitUpdate(key string, r ScopeReceipt) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.byKey[requestKeyDigest(key)] = ScopeReceiptLookup{State: ScopeCommitted, Receipt: &r}
}

// setHold sets the hold status the read scope answers.
func (f *fakeReader) setHold(h ScopeHold) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.hold = h
}

func p1Of(proof string) string {
	sum := sha256.Sum256([]byte(proof))
	return "p1_" + base64.RawURLEncoding.EncodeToString(sum[:])
}

// commit records that aimem committed step under proof.
func (f *fakeReader) commit(proof string, r ScopeReceipt) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.receipts[p1Of(proof)] = ScopeReceiptLookup{State: ScopeCommitted, Receipt: &r}
}

func (f *fakeReader) ReceiptByProof(_ context.Context, digest string) (ScopeReceiptLookup, error) {
	f.mu.Lock()
	answered := f.answered
	f.answered = nil
	f.mu.Unlock()
	if answered != nil {
		defer answered()
	}
	return f.lookup(digest)
}

func (f *fakeReader) lookup(digest string) (ScopeReceiptLookup, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reads++
	if f.err != nil {
		return ScopeReceiptLookup{}, f.err
	}
	if r, ok := f.receipts[digest]; ok {
		return r, nil
	}
	return ScopeReceiptLookup{State: ScopeNone}, nil
}

func (f *fakeReader) ReceiptByKey(_ context.Context, _ TaskRef, _ ReservationOp, digest string) (ScopeReceiptLookup, error) {
	f.mu.Lock()
	answered := f.answered
	f.answered = nil
	f.mu.Unlock()
	if answered != nil {
		defer answered()
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reads++
	if f.err != nil {
		return ScopeReceiptLookup{}, f.err
	}
	if r, ok := f.byKey[digest]; ok {
		return r, nil
	}
	return ScopeReceiptLookup{State: ScopeNone}, nil
}

func (f *fakeReader) HoldStatus(context.Context, TaskRef) (ScopeHold, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return ScopeHold{}, f.err
	}
	return f.hold, nil
}

// receiptFor is the read scope's receipt for a committed step.
func receiptFor(a Attempt, st Step, reservationID, fence string, revision int64) ScopeReceipt {
	return ScopeReceipt{ID: "rcpt-" + st.RequestKey, Operation: string(st.Operation), TaskID: a.Task.TaskID,
		RequestKeyDigest: requestKeyDigest(st.RequestKey), ReservationID: reservationID, Fence: fence,
		TaskRevision: revision, MemberUserID: "user-x", VerifiedMode: "team", CommittedAt: "2026-09-28T12:00:00Z"}
}

// stepEnv is a team whose members drive their own steps: aicrewd calls no
// mutating port, and learns outcomes from the fake read scope.
type stepEnv struct {
	execTeam
	reader *fakeReader
	now    *time.Time
}

func newStepEnv(t *testing.T) stepEnv {
	t.Helper()
	s, _ := openTemp(t)
	now := time.Now().UTC()
	s.now = func() time.Time { return now }
	e := newExecTeam(t, s)
	if _, err := s.db.Exec(`UPDATE agents SET linked_hub_id = 'hub-a'`); err != nil {
		t.Fatal(err)
	}
	return stepEnv{execTeam: e, reader: newFakeReader(), now: &now}
}

func (e stepEnv) advance(d time.Duration) { *e.now = e.now.Add(d) }

func (e stepEnv) begin(t *testing.T, key, taskID string) (Attempt, Step) {
	t.Helper()
	req := e.offerReq(taskID)
	req.ExpiresAt = e.s.now().Add(time.Hour)
	a, st, err := e.s.BeginOffer(context.Background(), e.lead.caller, key, req)
	if err != nil {
		t.Fatalf("begin offer %s: %v", taskID, err)
	}
	return a, st
}

func (e stepEnv) settle(t *testing.T, a Attempt, st Step, hint StepHint) (Attempt, Settlement, error) {
	t.Helper()
	return e.s.SettleStep(context.Background(), e.lead.caller, e.reader, a.ID, st.RequestKey, report(hint))
}

// report is the member's report of hint; a refusal carries aimem's code.
func report(hint StepHint) StepReport {
	if hint == HintRefused {
		return StepReport{Outcome: hint, Code: "coordination_rejected"}
	}
	return StepReport{Outcome: hint}
}

func active(t *testing.T, s *Store, proof string) bool {
	t.Helper()
	return activeOn(t, s, proof, "hub-a")
}

func activeOn(t *testing.T, s *Store, proof, hub string) bool {
	t.Helper()
	f, err := s.CoordinationFact(context.Background(), proof, hub)
	if err != nil {
		t.Fatal(err)
	}
	return f.Active
}

// Begin records the intent and returns coordination.v1's begin response,
// with a proof that answers; aicrewd sends nothing to aimem.
func TestBeginOfferReturnsTheStep(t *testing.T) {
	e := newStepEnv(t)
	a, st := e.begin(t, "offer", "task-1")
	if a.State != AttemptOffering || !busy(t, e.s, e.builder.agent.ID) {
		t.Fatalf("attempt after begin: %+v", a)
	}
	want := Step{Operation: ReservationClaim, RequestKey: a.PendingKey, ExpectedRevision: 3,
		Holder: &ReservationHolder{Mode: "external", WorkRef: a.offerRef()}, CoordinationProof: st.CoordinationProof}
	if st.Holder == nil || *st.Holder != *want.Holder || st.Operation != want.Operation || st.RequestKey != want.RequestKey ||
		st.ExpectedRevision != want.ExpectedRevision || st.ReservationID != "" || st.Fence != "" {
		t.Fatalf("step %+v, want %+v", st, want)
	}
	if !proofShape.MatchString(st.CoordinationProof) || !active(t, e.s, st.CoordinationProof) {
		t.Fatalf("step proof %q does not answer", st.CoordinationProof)
	}
	if n := len(e.port.callLog()); n != 0 {
		t.Fatalf("aicrewd called aimem %d times", n)
	}
	// The begin response has exactly the fixture's shape (coordination.v1
	// begin_exchange).
	var fx struct {
		Begin struct {
			Response struct {
				Body json.RawMessage `json:"body"`
			} `json:"response"`
		} `json:"begin_exchange"`
	}
	raw, err := os.ReadFile("../server/testdata/coordination-v1/examples.json")
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, &fx); err != nil {
		t.Fatal(err)
	}
	var fixtureStep Step
	dec := json.NewDecoder(bytes.NewReader(fx.Begin.Response.Body))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&fixtureStep); err != nil {
		t.Fatalf("the fixture's begin response does not decode as a Step: %v", err)
	}
	ours, _ := json.Marshal(fixtureStep)
	var a1, b1 map[string]any
	_ = json.Unmarshal(ours, &a1)
	_ = json.Unmarshal(fx.Begin.Response.Body, &b1)
	if fmt.Sprint(a1) != fmt.Sprint(b1) {
		t.Fatalf("Step re-encodes the fixture as\n%s\nnot\n%s", ours, fx.Begin.Response.Body)
	}
}

// A begin replayed with the same key gets a replacement proof for the same
// intent; the replaced one ends at once, yet a transition aimem committed
// under it still settles the step.
func TestBeginReplayReplacesTheProof(t *testing.T) {
	e := newStepEnv(t)
	ctx := context.Background()
	req := e.offerReq("task-1")
	req.ExpiresAt = e.s.now().Add(time.Hour)
	a, first, err := e.s.BeginOffer(ctx, e.lead.caller, "offer", req)
	if err != nil {
		t.Fatal(err)
	}
	e.advance(time.Second) // the replacement is the later proof, whatever the digests
	again, second, err := e.s.BeginOffer(ctx, e.lead.caller, "offer", req)
	if err != nil {
		t.Fatal(err)
	}
	if again.ID != a.ID || second.RequestKey != first.RequestKey || second.CoordinationProof == first.CoordinationProof {
		t.Fatalf("replay: attempt %s key %s proof reused %v", again.ID, second.RequestKey, second.CoordinationProof == first.CoordinationProof)
	}
	if active(t, e.s, first.CoordinationProof) || !active(t, e.s, second.CoordinationProof) {
		t.Fatal("the replaced proof still answers, or the replacement does not")
	}
	e.reader.commit(first.CoordinationProof, receiptFor(a, first, "res-1", "1", 4))
	got, st, err := e.settle(t, a, first, HintUnknown)
	if err != nil || !st.Settled || got.State != AttemptOffered || got.ReservationID != "res-1" {
		t.Fatalf("settle under the replaced proof: %+v %+v %v", got, st, err)
	}
	// A replay after the step settled reports it, with nothing to send.
	after, step, err := e.s.BeginOffer(ctx, e.lead.caller, "offer", req)
	if err != nil || after.ID != a.ID || after.State != AttemptOffered || !reflect.DeepEqual(step, Step{}) {
		t.Fatalf("replay after settle gave %+v %+v %v", after, step, err)
	}
}

// The offer family runs begin then settle: a committed receipt in the read
// scope applies each transition; nothing is taken from the member's word.
func TestOfferFamilySettlesFromTheReadScope(t *testing.T) {
	ctx := context.Background()
	e := newStepEnv(t)
	a, st := e.begin(t, "offer", "task-1")
	e.reader.commit(st.CoordinationProof, receiptFor(a, st, "res-1", "1", 4))
	a, set, err := e.settle(t, a, st, HintCommitted)
	if err != nil || !set.Settled || a.State != AttemptOffered || a.ReservationID != "res-1" || a.Fence != "1" || a.TaskRevision != 4 {
		t.Fatalf("offer settled: %+v %+v %v", a, set, err)
	}
	if active(t, e.s, st.CoordinationProof) {
		t.Fatal("the offer's proof outlived its step")
	}
	if again, set, err := e.settle(t, a, st, HintCommitted); err != nil || !set.Settled || again.State != AttemptOffered {
		t.Fatalf("settling a settled step again: %+v %+v %v", again, set, err)
	}

	a, acc, err := e.s.BeginAccept(ctx, e.builder.caller, "accept", a.ID, e.acceptReq())
	if err != nil || acc.Operation != ReservationTransfer || acc.ReservationID != "res-1" || acc.Fence != "1" ||
		acc.Holder == nil || acc.Holder.WorkRef != a.attemptRef() {
		t.Fatalf("begin accept: %+v %v", acc, err)
	}
	e.reader.commit(acc.CoordinationProof, receiptFor(a, acc, "res-1", "2", 5))
	if a, set, err = e.s.SettleStep(ctx, e.builder.caller, e.reader, a.ID, acc.RequestKey, report(HintCommitted)); err != nil ||
		!set.Settled || a.State != AttemptRunning || a.Fence != "2" {
		t.Fatalf("accept settled: %+v %+v %v", a, set, err)
	}

	// Decline is local: no step, no call. The coordinator then releases
	// the declined offer as never accepted.
	d := newStepEnv(t)
	o, ost := d.begin(t, "offer", "task-2")
	d.reader.commit(ost.CoordinationProof, receiptFor(o, ost, "res-2", "1", 4))
	if o, _, err = d.settle(t, o, ost, HintCommitted); err != nil || o.State != AttemptOffered {
		t.Fatalf("offer: %+v %v", o, err)
	}
	if o, err = d.s.DeclineOffer(ctx, d.builder.caller, "decline", o.ID, d.builder.sess.ID, d.builder.sess.Generation); err != nil || !o.Declined {
		t.Fatalf("decline: %+v %v", o, err)
	}
	o, rel, err := d.s.BeginRelease(ctx, d.lead.caller, "release", o.ID, d.lead.sess.ID, d.lead.sess.Generation)
	if err != nil || rel.Operation != ReservationRelease {
		t.Fatalf("begin release of a declined offer: %+v %v", rel, err)
	}
	if f, err := d.s.CoordinationFact(ctx, rel.CoordinationProof, "hub-a"); err != nil || f.Kind != FactNeverAccepted {
		t.Fatalf("the release's fact: %+v %v", f, err)
	}
	d.reader.commit(rel.CoordinationProof, receiptFor(o, rel, "", "2", 5))
	if o, set, err := d.settle(t, o, rel, HintCommitted); err != nil || !set.Settled || o.State != AttemptClosed ||
		o.CloseReason != "declined" {
		t.Fatalf("declined offer released: %+v %+v %v", o, set, err)
	}
}

// A member's report is only a hint: "committed" with no receipt waits, and
// never settles the step as committed.
func TestSettleNeverTrustsTheReport(t *testing.T) {
	e := newStepEnv(t)
	a, st := e.begin(t, "offer", "task-1")
	got, set, err := e.settle(t, a, st, HintCommitted)
	if err != nil || set.Settled || got.State != AttemptOffering || !active(t, e.s, st.CoordinationProof) {
		t.Fatalf("an unconfirmed commit report: %+v %+v %v", got, set, err)
	}
	// The receipt may appear any moment: the member asks again soon, not
	// only once the proof has expired.
	if set.RetryAfter != NoneFinalAfter {
		t.Fatalf("retry after %s while the proof lives, want %s", set.RetryAfter, NoneFinalAfter)
	}
	// Once the proof has expired, and NoneFinalAfter more has passed, the
	// read scope's none is final whatever the member said.
	e.advance(ProofLifetime + NoneFinalAfter)
	got, set, err = e.settle(t, a, st, HintCommitted)
	if err != nil || !set.Settled || got.State != AttemptClosed || busy(t, e.s, e.builder.agent.ID) {
		t.Fatalf("an expired, uncommitted step: %+v %+v %v", got, set, err)
	}
}

// A refused or unknown outcome voids the step: its proof ends at once, and
// the step settles as not committed only NoneFinalAfter later.
func TestSettleVoidsAndWaits(t *testing.T) {
	for _, hint := range []StepHint{HintRefused, HintUnknown} {
		t.Run(string(hint), func(t *testing.T) {
			e := newStepEnv(t)
			a, st := e.begin(t, "offer", "task-1")
			got, set, err := e.settle(t, a, st, hint)
			if err != nil || set.Settled || set.RetryAfter != NoneFinalAfter || got.State != AttemptOffering {
				t.Fatalf("first settle: %+v %+v %v", got, set, err)
			}
			if active(t, e.s, st.CoordinationProof) {
				t.Fatal("a voided step's proof still answers")
			}
			e.advance(NoneFinalAfter - time.Second)
			if _, set, err = e.settle(t, a, st, hint); err != nil || set.Settled || set.RetryAfter != time.Second {
				t.Fatalf("settle before none is final: %+v %v", set, err)
			}
			e.advance(time.Second)
			got, set, err = e.settle(t, a, st, hint)
			if err != nil || !set.Settled || got.State != AttemptClosed || got.CloseReason != "claim not_committed" ||
				busy(t, e.s, e.builder.agent.ID) {
				t.Fatalf("settle once none is final: %+v %+v %v", got, set, err)
			}
		})
	}
}

// Until aicrew has a read scope, a step stays pending: a refusal report
// voids it, but nothing settles it.
// A "none" is final only when the lookup started after the grace period: a
// lookup asked earlier may return after a late commit it did not see.
func TestSettleNoneIsFinalOnlyWhenAskedLate(t *testing.T) {
	e := newStepEnv(t)
	a, st := e.begin(t, "offer", "task-1")
	if _, set, err := e.settle(t, a, st, HintRefused); err != nil || set.Settled {
		t.Fatalf("void: %+v %v", set, err)
	}
	e.advance(time.Second)
	e.reader.answered = func() {
		e.advance(NoneFinalAfter + time.Second)
		e.reader.commit(st.CoordinationProof, receiptFor(a, st, "res-1", "1", 4))
	}
	if got, set, err := e.settle(t, a, st, HintRefused); err != nil || set.Settled || got.State != AttemptOffering {
		t.Fatalf("a none asked inside the grace period settled the step: %+v %+v %v", got, set, err)
	}
	if got, set, err := e.settle(t, a, st, HintRefused); err != nil || !set.Settled || got.State != AttemptOffered {
		t.Fatalf("the late commit: %+v %+v %v", got, set, err)
	}
}

func TestSettleWithoutAReaderStaysPending(t *testing.T) {
	e := newStepEnv(t)
	a, st := e.begin(t, "offer", "task-1")
	for i := 0; i < 2; i++ {
		got, set, err := e.s.SettleStep(context.Background(), e.lead.caller, nil, a.ID, st.RequestKey, report(HintRefused))
		if err != nil || set.Settled || got.State != AttemptOffering {
			t.Fatalf("settle without a reader: %+v %+v %v", got, set, err)
		}
		e.advance(time.Hour)
	}
	if active(t, e.s, st.CoordinationProof) {
		t.Fatal("the refused step's proof still answers")
	}
}

// A receipt that is not the pending step's never settles it, and a read
// scope that fails leaves the step pending.
func TestSettleRefusesAForeignReceipt(t *testing.T) {
	e := newStepEnv(t)
	a, st := e.begin(t, "offer", "task-1")
	wrong := receiptFor(a, st, "res-1", "1", 4)
	wrong.RequestKeyDigest = requestKeyDigest("another-key")
	e.reader.commit(st.CoordinationProof, wrong)
	if got, set, err := e.settle(t, a, st, HintCommitted); !errors.Is(err, ErrOutcomeUnknown) || set.Settled || got.State != AttemptOffering {
		t.Fatalf("a foreign receipt: %+v %+v %v", got, set, err)
	}
	e.reader.err = io.ErrUnexpectedEOF
	if _, set, err := e.settle(t, a, st, HintRefused); !errors.Is(err, ErrOutcomeUnknown) || set.Settled {
		t.Fatalf("a failing read scope: %+v %v", set, err)
	}
}

// A refused transfer returns the offer; the coordinator can then release it.
func TestRefusedAcceptThenWithdraw(t *testing.T) {
	ctx := context.Background()
	e := newStepEnv(t)
	a, st := e.begin(t, "offer", "task-1")
	e.reader.commit(st.CoordinationProof, receiptFor(a, st, "res-1", "1", 4))
	if a, _, _ = e.settle(t, a, st, HintCommitted); a.State != AttemptOffered {
		t.Fatalf("offer: %+v", a)
	}
	a, acc, err := e.s.BeginAccept(ctx, e.builder.caller, "accept", a.ID, e.acceptReq())
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := e.s.SettleStep(ctx, e.builder.caller, e.reader, a.ID, acc.RequestKey, report(HintRefused)); err != nil {
		t.Fatal(err)
	}
	e.advance(NoneFinalAfter)
	if a, set, err := e.s.SettleStep(ctx, e.builder.caller, e.reader, a.ID, acc.RequestKey, report(HintRefused)); err != nil ||
		!set.Settled || a.State != AttemptOffered {
		t.Fatalf("refused accept: %+v %+v %v", a, set, err)
	}
	a, rel, err := e.s.BeginRelease(ctx, e.lead.caller, "withdraw", a.ID, e.lead.sess.ID, e.lead.sess.Generation)
	if err != nil || rel.Operation != ReservationRelease || rel.ReservationID != "res-1" || rel.Fence != "1" || rel.Holder != nil {
		t.Fatalf("begin release: %+v %v", rel, err)
	}
	e.reader.commit(rel.CoordinationProof, receiptFor(a, rel, "", "2", 5))
	if a, set, err := e.settle(t, a, rel, HintCommitted); err != nil || !set.Settled || a.State != AttemptClosed ||
		a.CloseReason != "withdrawn" || busy(t, e.s, e.builder.agent.ID) {
		t.Fatalf("withdraw settled: %+v %+v %v", a, set, err)
	}
}

// A task with an open attempt of this service, in any team, takes no new
// offer or claim (01a0e639); once the attempt closes, it does.
func TestOpenAttemptGuardsTheTask(t *testing.T) {
	ctx := context.Background()
	e := newStepEnv(t)
	a, st := e.begin(t, "offer", "task-1")
	other := newExecTeamNamed(t, e.s, "t2")
	req := other.offerReq("task-1")
	req.ExpiresAt = e.s.now().Add(time.Hour)
	if _, _, err := e.s.BeginOffer(ctx, other.lead.caller, "offer-other", req); !errors.Is(err, ErrTaskBusy) {
		t.Fatalf("an offer by another team on the same task: %v", err)
	}
	if _, _, err := e.settle(t, a, st, HintRefused); err != nil {
		t.Fatal(err)
	}
	e.advance(NoneFinalAfter)
	if a, set, err := e.settle(t, a, st, HintRefused); err != nil || !set.Settled || a.State != AttemptClosed {
		t.Fatalf("closing the first offer: %+v %+v %v", a, set, err)
	}
	b, _, err := e.s.BeginOffer(ctx, other.lead.caller, "offer-other-2", req)
	if err != nil {
		t.Fatalf("an offer once the task's attempt closed: %v", err)
	}
	// Another attempt's settled step is no step of this one.
	if got, set, err := e.s.SettleStep(ctx, other.lead.caller, e.reader, b.ID, st.RequestKey, report(HintRefused)); !errors.Is(err, ErrStepUnknown) ||
		set.Settled || got.State != AttemptOffering {
		t.Fatalf("settling another attempt's step: %+v %+v %v", got, set, err)
	}
}

// Only the operator or an active member of the team settles a step.
func TestSettleNeedsATeamMember(t *testing.T) {
	e := newStepEnv(t)
	a, st := e.begin(t, "offer", "task-1")
	other := newExecTeamNamed(t, e.s, "t2")
	if _, _, err := e.s.SettleStep(context.Background(), other.lead.caller, e.reader, a.ID, st.RequestKey, report(HintRefused)); !errors.Is(err, ErrForbidden) {
		t.Fatalf("settle by another team's member: %v", err)
	}
	if _, _, err := e.s.SettleStep(context.Background(), e.lead.caller, e.reader, a.ID, st.RequestKey, report("maybe")); !errors.Is(err, ErrInvalid) {
		t.Fatalf("an unknown outcome: %v", err)
	}
	if !active(t, e.s, st.CoordinationProof) {
		t.Fatal("a refused settle voided the step")
	}
}

// The read scope's answers decode exactly from the fixture's exchanges.
func TestReadScopeShapes(t *testing.T) {
	raw, err := os.ReadFile("../server/testdata/coordination-v1/examples.json")
	if err != nil {
		t.Fatal(err)
	}
	var fx struct {
		ReadScope struct {
			Exchanges []struct {
				Case     string `json:"case"`
				Response struct {
					Body json.RawMessage `json:"body"`
				} `json:"response"`
			} `json:"exchanges"`
		} `json:"read_scope"`
	}
	if err := json.Unmarshal(raw, &fx); err != nil {
		t.Fatal(err)
	}
	if len(fx.ReadScope.Exchanges) == 0 {
		t.Fatal("the fixture has no read-scope exchanges")
	}
	for _, ex := range fx.ReadScope.Exchanges {
		var probe struct {
			Receipt json.RawMessage `json:"receipt"`
		}
		_ = json.Unmarshal(ex.Response.Body, &probe)
		var v any = &ScopeHold{}
		if probe.Receipt != nil || ex.Case == "receipt_none" {
			v = &ScopeReceiptLookup{}
		}
		dec := json.NewDecoder(bytes.NewReader(ex.Response.Body))
		dec.DisallowUnknownFields()
		if err := dec.Decode(v); err != nil {
			t.Fatalf("%s: %v", ex.Case, err)
		}
		ours, _ := json.Marshal(v)
		var a1, b1 map[string]any
		_ = json.Unmarshal(ours, &a1)
		_ = json.Unmarshal(ex.Response.Body, &b1)
		if fmt.Sprint(a1) != fmt.Sprint(b1) {
			t.Fatalf("%s re-encodes as %s, not %s", ex.Case, ours, ex.Response.Body)
		}
	}
	if d, err := proofDigestP1(secretDigest("acp1_x")); err != nil || d != p1Of("acp1_x") {
		t.Fatalf("p1 digest %q, %v", d, err)
	}
}

// The token operations authenticate the session token inside each command,
// replays included: a token that died while its session kept its
// generation, here by expiring, begins nothing, not even as a replay.
func TestStepOperationsRecheckTheToken(t *testing.T) {
	ctx := context.Background()
	s, _ := openTemp(t)
	now := clock(s)
	tm := mustTeam(t, s, "t1", "crew", ProjectRef{HubID: "hub-test", ProjectID: "project-t"})
	p := newProver(t, s)
	lead, _ := member(t, s, tm.ID, "lead", RoleCoordinator)
	worker, _ := member(t, s, tm.ID, "worker", RoleWorker)
	entry, sec := p.enter("e-lead", lead, tm.ID)
	p.enter("e-worker", worker, tm.ID)
	token := sec.Token.Reveal()
	in := OfferInput{WorkerAgentID: worker.ID, Task: TaskRef{HubID: "hub-test", ProjectID: "project-t", TaskID: "task-1"},
		ExpectedRevision: 3, BaseCommit: "base-1", Branch: "work/task-1", Repository: testRepository, Process: testPin,
		ExpiresAt: now.Add(time.Hour)}
	a, st, err := s.BeginOfferWithToken(ctx, "offer-1", token, in)
	if err != nil || st.CoordinationProof == "" || a.CoordinatorSessionID != entry.Session.ID {
		t.Fatalf("begin offer: %+v %+v %v", a, st, err)
	}
	*now = entry.Token.ExpiresAt
	if _, _, err := s.BeginOfferWithToken(ctx, "offer-1", token, in); !errors.Is(err, ErrTokenInvalid) {
		t.Fatalf("a replay with an expired token: %v", err)
	}
	if _, _, err := s.BeginWithdrawWithToken(ctx, "withdraw-1", token, a.ID); !errors.Is(err, ErrTokenInvalid) {
		t.Fatalf("a withdraw with an expired token: %v", err)
	}
	if _, _, err := s.SettleWithToken(ctx, token, newFakeReader(), a.ID, st.RequestKey, report(HintUnknown)); !errors.Is(err, ErrTokenInvalid) {
		t.Fatalf("a settle with an expired token: %v", err)
	}
}

// openProofs counts the attempt's proofs that have not ended.
func openProofs(t *testing.T, s *Store, attemptID string) int {
	t.Helper()
	var n int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM coordination_proofs WHERE attempt_id = ? AND ended_at = ''`, attemptID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// Every write a token operation makes checks the token inside its own
// transaction: a token that dies after the operation authenticated it, while
// the operation waits, replaces, voids and settles nothing.
func TestStepWritesRecheckTheToken(t *testing.T) {
	ctx := context.Background()
	s, _ := openTemp(t)
	now := clock(s)
	tm := mustTeam(t, s, "t1", "crew", ProjectRef{HubID: "hub-test", ProjectID: "project-t"})
	p := newProver(t, s)
	lead, _ := member(t, s, tm.ID, "lead", RoleCoordinator)
	worker, _ := member(t, s, tm.ID, "worker", RoleWorker)
	entry, sec := p.enter("e-lead", lead, tm.ID)
	p.enter("e-worker", worker, tm.ID)
	token, alive := sec.Token.Reveal(), *now
	die := func() { *now = entry.Token.ExpiresAt }
	in := OfferInput{WorkerAgentID: worker.ID, Task: TaskRef{HubID: "hub-test", ProjectID: "project-t", TaskID: "task-1"},
		ExpectedRevision: 3, BaseCommit: "base-1", Branch: "work/task-1", Repository: testRepository, Process: testPin,
		ExpiresAt: now.Add(time.Hour)}
	a, st, err := s.BeginOfferWithToken(ctx, "offer-1", token, in)
	if err != nil {
		t.Fatal(err)
	}

	s.beforeProofReplace = die
	if _, _, err := s.BeginOfferWithToken(ctx, "offer-1", token, in); !errors.Is(err, ErrTokenInvalid) {
		t.Fatalf("a replacement after the token died: %v", err)
	}
	s.beforeProofReplace, *now = nil, alive
	if openProofs(t, s, a.ID) != 1 || !activeOn(t, s, st.CoordinationProof, "hub-test") {
		t.Fatal("a dead token replaced the step's proof")
	}

	reader := newFakeReader()
	reader.answered = die
	if _, _, err := s.SettleWithToken(ctx, token, reader, a.ID, st.RequestKey, report(HintUnknown)); !errors.Is(err, ErrTokenInvalid) {
		t.Fatalf("a void after the token died: %v", err)
	}
	*now = alive
	if openProofs(t, s, a.ID) != 1 {
		t.Fatal("a dead token voided the step")
	}

	reader.commit(st.CoordinationProof, receiptFor(a, st, "res-1", "1", 4))
	reader.answered = die
	if _, _, err := s.SettleWithToken(ctx, token, reader, a.ID, st.RequestKey, report(HintCommitted)); !errors.Is(err, ErrTokenInvalid) {
		t.Fatalf("a settle after the token died: %v", err)
	}
	*now = alive
	if got, err := s.GetAttempt(ctx, a.ID); err != nil || got.State != AttemptOffering {
		t.Fatalf("a dead token settled the step: %+v %v", got, err)
	}
	if got, set, err := s.SettleWithToken(ctx, token, reader, a.ID, st.RequestKey, report(HintCommitted)); err != nil ||
		!set.Settled || got.State != AttemptOffered {
		t.Fatalf("the live token's settle: %+v %+v %v", got, set, err)
	}
	// A replay of the settled step replaces nothing; the replay itself
	// checks the token.
	die()
	if _, _, err := s.BeginOfferWithToken(ctx, "offer-1", token, in); !errors.Is(err, ErrTokenInvalid) {
		t.Fatalf("a replay of a settled step with a dead token: %v", err)
	}
}

// A read scope bound to another hub never reads a step: the step of a
// hub-a task stays pending against hub-b's scope, whatever it would answer,
// and settles from hub-a's.
func TestSettleReadsOnlyTheTasksHub(t *testing.T) {
	ctx := context.Background()
	e := newStepEnv(t)
	a, st := e.begin(t, "offer", "task-1")
	e.reader.commit(st.CoordinationProof, receiptFor(a, st, "res-1", "1", 4))
	e.advance(time.Hour + NoneFinalAfter)
	other := HubReader{ReservationReader: e.reader, HubID: "hub-b"}
	got, set, err := e.s.SettleStep(ctx, e.lead.caller, other, a.ID, st.RequestKey, report(HintCommitted))
	if err != nil || set.Settled || got.State != AttemptOffering || e.reader.reads != 0 {
		t.Fatalf("another hub's scope settled the step: %+v %+v %v, %d reads", got, set, err, e.reader.reads)
	}
	own := HubReader{ReservationReader: e.reader, HubID: a.Task.HubID}
	if got, set, err := e.s.SettleStep(ctx, e.lead.caller, own, a.ID, st.RequestKey, report(HintCommitted)); err != nil ||
		!set.Settled || got.State != AttemptOffered {
		t.Fatalf("the task's hub: %+v %+v %v", got, set, err)
	}
}
