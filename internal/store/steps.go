package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"regexp"
	"time"
)

// Two-phase coordinated steps (crew-execution b1; docs/CREW-CONTRACT.md,
// "Coordination facts"). Aicrew never mutates a reservation: under the
// operator's D4(a), the acting member's own aimem connection sends each
// mutation. So a step runs as two calls:
//
//   - begin records the intent and its capacity, as the one-shot operations
//     do, and returns the step to send: the request key, the expected
//     revision, the reservation and fence, the holder, and the coordination
//     proof. The proof exists only in that answer.
//   - settle learns the outcome. The member's report is only a hint: aicrew
//     confirms it through aimem's read scope, which shows the receipts of
//     proofs aicrew issued. A committed receipt applies the transition. With
//     no receipt, a report of a refusal or an unknown outcome voids the step
//     (its proofs end, so aimem refuses any late use), and the step settles
//     as not committed once the read scope still shows nothing
//     NoneFinalAfter after its proofs ended (coordination.v1 §2, "When none
//     is final"). Until then, and whenever no reader is configured, the step
//     stays pending.

// NoneFinalAfter is how long after a step's proofs end a read-scope "none"
// becomes final: aimem's 5 s answer age plus its 2 s budget and a margin.
const NoneFinalAfter = 10 * time.Second

const (
	opReplaceProof = "attempt.proof_replace"
	opVoidStep     = "attempt.void_step"
)

// Step is one coordinated step as begin returns it: coordination.v1's begin
// response. CoordinationProof is a secret, returned by this answer only.
// TargetState, Reason, Blocker, TerminalEvidence, Intent and ResultRef are the
// values the member sends aimem with the step (D-b1b-2): a stopped attempt's
// release carries the target, reason and blocker; a finalize its target,
// reason and the confirmed delivery's references; a work update its intent,
// target and blocker or result reference. No other step carries them. An
// update has no coordination fact, so it has no proof.
type Step struct {
	Operation         ReservationOp      `json:"operation"`
	RequestKey        string             `json:"request_key"`
	ExpectedRevision  int64              `json:"expected_revision"`
	ReservationID     string             `json:"reservation_id,omitempty"`
	Fence             string             `json:"fence,omitempty"`
	Holder            *ReservationHolder `json:"holder,omitempty"`
	TargetState       string             `json:"target_state,omitempty"`
	Reason            string             `json:"reason,omitempty"`
	Blocker           string             `json:"blocker,omitempty"`
	TerminalEvidence  []string           `json:"terminal_evidence,omitempty"`
	Intent            string             `json:"intent,omitempty"`
	ResultRef         string             `json:"result_ref,omitempty"`
	CoordinationProof string             `json:"coordination_proof,omitempty"`
}

func stepFor(a Attempt, proof string) Step {
	req := reservationRequest(a, proof)
	st := Step{Operation: a.PendingOp, RequestKey: a.PendingKey, ExpectedRevision: req.ExpectedRevision,
		ReservationID: req.ReservationID, Fence: req.Fence, Holder: req.Holder, CoordinationProof: proof}
	switch {
	case a.PendingOp == ReservationRelease && a.Stop == StopConfirmed:
		st.TargetState, st.Reason, st.Blocker = req.Owned.State, req.Reason, req.Owned.Blocker
	case a.PendingOp == ReservationFinalize:
		st.TargetState, st.Reason, st.TerminalEvidence = req.Owned.State, req.Reason, req.TerminalEvidence
	case a.PendingOp == ReservationUpdate:
		st.Intent, st.TargetState, st.Blocker, st.ResultRef = req.Intent, req.Owned.State, req.Owned.Blocker, req.Owned.ResultRef
	}
	return st
}

// BeginOffer begins an offer: the coordinator's claim of the task for a named
// worker, whose capacity the intent takes.
func (s *Store) BeginOffer(ctx context.Context, c Caller, key string, in OfferRequest) (Attempt, Step, error) {
	var proof string
	return s.beginNew(ctx, c, offerCommand(c, key, in, &proof), &proof, FactOffer, in.SessionID, in.Generation, nil)
}

// beginNew begins a step that creates its attempt: an offer or an
// independent claim. Identical requests in flight wait for each other.
func (s *Store) beginNew(ctx context.Context, c Caller, cmd command, proof *string, kind FactKind, sessionID string,
	generation int64, g guard) (Attempt, Step, error) {
	release, err := s.flights.acquire(ctx, c, cmd, s.flightWait)
	if err != nil {
		return Attempt{}, Step{}, err
	}
	defer release()
	return s.begin(ctx, c, cmd, "", proof, kind, sessionID, generation, g)
}

// BeginAccept begins the named worker's acceptance: the transfer of the
// offer's hold to its attempt.
func (s *Store) BeginAccept(ctx context.Context, c Caller, key, attemptID string, in AcceptRequest) (Attempt, Step, error) {
	var proof string
	return s.begin(ctx, c, acceptCommand(c, key, attemptID, in, true, &proof), attemptID, &proof,
		FactAcceptedAttempt, in.SessionID, in.Generation, nil)
}

// BeginRelease begins the release of an offer never accepted (declined,
// withdrawn or expired), by the team's current coordinator.
func (s *Store) BeginRelease(ctx context.Context, c Caller, key, attemptID, sessionID string, generation int64) (Attempt, Step, error) {
	var proof string
	return s.begin(ctx, c, releaseCommand(c, key, attemptID, sessionID, generation, &proof), attemptID, &proof,
		FactNeverAccepted, sessionID, generation, nil)
}

// begin runs a step's intent. A replay of the same key finds the intent
// recorded but not its proof, which is never stored: while the step is still
// pending, the replay gets a replacement proof for the same intent and key,
// and the earlier proof ends at once (D-b1a-1), so aimem refuses it if it was
// already on its way. A replay after the step settled reports that step's
// outcome, with no step to send. g, if set, is checked inside the
// replacement's transaction too. A step with no fact (kind "", an update)
// has no proof to replace: its replay answers the same step.
func (s *Store) begin(ctx context.Context, c Caller, cmd command, attemptID string, proof *string, kind FactKind,
	sessionID string, generation int64, g guard) (Attempt, Step, error) {
	if attemptID != "" {
		unlock, err := s.lockAttempt(ctx, attemptID)
		if err != nil {
			return Attempt{}, Step{}, err
		}
		defer unlock()
	}
	var recorded Attempt
	if err := s.run(ctx, c, cmd, &recorded); err != nil {
		return Attempt{}, Step{}, err
	}
	if attemptID == "" {
		unlock, err := s.lockAttempt(ctx, recorded.ID)
		if err != nil {
			return Attempt{}, Step{}, err
		}
		defer unlock()
	}
	cur, err := s.GetAttempt(ctx, recorded.ID)
	if err != nil {
		return Attempt{}, Step{}, err
	}
	if cur.PendingKey == "" || cur.PendingKey != recorded.PendingKey {
		out, err := s.stepOutcome(ctx, cur, recorded.PendingKey)
		return out, Step{}, err
	}
	if *proof == "" && kind != "" {
		if s.beforeProofReplace != nil {
			s.beforeProofReplace()
		}
		if *proof, err = s.replaceProof(ctx, c, cur, kind, sessionID, generation, g); err != nil {
			return cur, Step{}, err
		}
	}
	return cur, stepFor(cur, *proof), nil
}

type stepRef struct {
	AttemptID  string `json:"attempt_id"`
	RequestKey string `json:"request_key"`
}

// replaceProof ends the pending step's proofs and issues a new one for the
// same intent, acted by sessionID at generation: the session that began the
// step, perhaps resumed since. Inside the same transaction, the session must
// still act the step by the rule of its fact kind (stillActs), so authority
// is rechecked where the replacement commits. A pending independent claim
// is the claiming session's own: its recorded generation moves with the
// session's resume.
func (s *Store) replaceProof(ctx context.Context, c Caller, a Attempt, kind FactKind, sessionID string, generation int64,
	g guard) (string, error) {
	key, err := newID(s.now())
	if err != nil {
		return "", err
	}
	var proof string
	var out stepRef
	err = s.run(ctx, c, command{
		op: opReplaceProof, scope: a.ID, key: key, input: stepRef{a.ID, a.PendingKey}, authorize: requireAgent,
		check: g.then(pendingStep(a)),
		apply: func(ctx context.Context, tx *sql.Tx, now time.Time) (any, error) {
			if kind == FactIndependentClaim {
				if _, err := tx.ExecContext(ctx,
					`UPDATE attempts SET worker_generation = ? WHERE id = ? AND worker_session_id = ?`,
					generation, a.ID, sessionID); err != nil {
					return nil, fmt.Errorf("carry the claim forward: %w", err)
				}
			}
			if err := stillActs(ctx, tx, a.ID, kind, sessionID, generation); err != nil {
				return nil, err
			}
			if err := endProofs(ctx, tx, a.ID, now); err != nil {
				return nil, err
			}
			var err error
			proof, err = issueProof(ctx, tx, a, kind, sessionID, generation, now)
			return stepRef{a.ID, a.PendingKey}, err
		},
	}, &out)
	return proof, err
}

// stillActs checks that sessionID at generation may act the attempt's
// pending step: the rule of its fact kind, as aimem would be answered for a
// proof of that session (CoordinationFact).
func stillActs(ctx context.Context, q querier, attemptID string, kind FactKind, sessionID string, generation int64) error {
	a, err := getAttempt(ctx, q, attemptID)
	if err != nil {
		return err
	}
	member, ok, err := actingMember(ctx, q, a, proofRecord{sessionID: sessionID, generation: generation}, a.Task.HubID)
	if err != nil {
		return err
	}
	if ok {
		f := Fact{Kind: kind, Member: member}
		if ok, err = f.fill(ctx, q, a, a.Task.HubID); err != nil {
			return err
		}
	}
	if !ok {
		return fmt.Errorf("attempt %s: the session no longer acts its %s step: %w", attemptID, kind, ErrForbidden)
	}
	return nil
}

// pendingStep checks, in the command's transaction, that a's step is still
// the attempt's pending one.
func pendingStep(a Attempt) func(context.Context, *sql.Tx) error {
	return func(ctx context.Context, tx *sql.Tx) error {
		cur, err := getAttempt(ctx, tx, a.ID)
		if err != nil {
			return err
		}
		if cur.PendingKey != a.PendingKey {
			return fmt.Errorf("attempt %s: step %s is no longer pending: %w", a.ID, a.PendingKey, ErrAttemptState)
		}
		return nil
	}
}

// StepHint is the acting member's report of its mutation: a hint only.
type StepHint string

// StepReport is the acting member's report: the outcome it saw and, for a
// refusal, aimem's refusal code. The store records it with the step's void;
// it never decides the outcome.
type StepReport struct {
	Outcome StepHint `json:"outcome"`
	Code    string   `json:"code,omitempty"`
	// reconciled marks the reconciler's settle, which carries no member's
	// report: it voids nothing and settles only on what the read scope
	// shows (ReconcileStep).
	reconciled bool
}

var refusalCodeShape = regexp.MustCompile(`^[a-z][a-z0-9_]{0,63}$`)

func (r StepReport) validate() error {
	if r.reconciled {
		return nil
	}
	switch r.Outcome {
	case HintRefused:
		if !refusalCodeShape.MatchString(r.Code) {
			return fmt.Errorf("%w: a refusal needs aimem's refusal code", ErrInvalid)
		}
	case HintCommitted, HintUnknown:
		if r.Code != "" {
			return fmt.Errorf("%w: only a refusal carries a code", ErrInvalid)
		}
	default:
		return fmt.Errorf("%w: the outcome must be committed, refused or unknown", ErrInvalid)
	}
	return nil
}

const (
	HintCommitted StepHint = "committed"
	HintRefused   StepHint = "refused"
	HintUnknown   StepHint = "unknown"
)

// Settlement says whether a settle settled the step and its recorded
// outcome (committed or not_committed; refused only for a step aicrewd sent
// to aimem itself), and, if not, when to ask again.
type Settlement struct {
	Settled    bool
	Outcome    string
	RetryAfter time.Duration
}

// ErrStepUnknown refuses a request key that names no step of the attempt.
var ErrStepUnknown = errors.New("step_unknown")

// SettleStep settles the attempt's pending coordinated step with requestKey.
// The caller must be the operator or an active member of the attempt's team.
// reader may be nil while aicrew has no read scope: the step then stays
// pending, whatever the member reports (D-b1a-2).
func (s *Store) SettleStep(ctx context.Context, c Caller, reader ReservationReader, attemptID, requestKey string,
	report StepReport) (Attempt, Settlement, error) {
	return s.settleStep(ctx, c, reader, attemptID, requestKey, report, nil)
}

// settleStep is SettleStep with g, if set, checked inside the transaction of
// every write it makes: the void and the settlement.
func (s *Store) settleStep(ctx context.Context, c Caller, reader ReservationReader, attemptID, requestKey string,
	report StepReport, g guard) (Attempt, Settlement, error) {
	if err := report.validate(); err != nil {
		return Attempt{}, Settlement{}, err
	}
	unlock, err := s.lockAttempt(ctx, attemptID)
	if err != nil {
		return Attempt{}, Settlement{}, err
	}
	defer unlock()
	a, err := s.GetAttempt(ctx, attemptID)
	if err != nil {
		return Attempt{}, Settlement{}, err
	}
	if err := s.mayReconcile(ctx, c, a); err != nil {
		return Attempt{}, Settlement{}, err
	}
	reader = ReaderFor(reader, a.Task.HubID)
	if a.PendingKey != requestKey && a.PendingOp == ReservationUpdate {
		// A superseded key settles its update step.
		if alias, err := s.supersededKey(ctx, a.ID, requestKey); err != nil {
			return a, Settlement{}, err
		} else if alias {
			requestKey = a.PendingKey
		}
	}
	if a.PendingKey == requestKey && a.PendingOp == ReservationUpdate {
		return s.settleUpdate(ctx, c, reader, a, g)
	}
	if a.PendingKey != requestKey {
		// Not the pending step: an earlier step of this attempt, whose
		// recorded outcome answers, or no step of this attempt at all.
		outcome, _, err := s.recordedOutcome(ctx, a, requestKey)
		if err != nil {
			return a, Settlement{}, err
		}
		return a, Settlement{Settled: true, Outcome: outcome}, nil
	}
	proofs, err := s.stepProofs(ctx, a.ID, requestKey)
	if err != nil {
		return a, Settlement{}, err
	}
	if len(proofs) == 0 {
		return a, Settlement{}, fmt.Errorf("%w: step %s has no coordination proof to settle by", ErrInvalid, requestKey)
	}
	pending := Settlement{RetryAfter: NoneFinalAfter}
	// A read-scope "none" is final only for lookups that start once the
	// grace period is over: a lookup asked earlier may answer after a late
	// commit it did not see.
	asked := s.now()
	if reader != nil {
		// A proof replaced by a replay may still have been used: any of
		// the intent's proofs can carry the committed transition. The
		// newest, the only one that may still live, is read first; a proof
		// whose "none" is already final is not read again.
		finals, err := s.scanFinals(ctx, a.ID)
		if err != nil {
			return a, Settlement{}, err
		}
		for _, p := range proofs {
			if finals[proofLookup(p.digest)] {
				continue
			}
			digest, err := proofDigestP1(p.digest)
			if err != nil {
				return a, Settlement{}, err
			}
			readAt := s.now()
			look, err := reader.ReceiptByProof(ctx, digest)
			if err != nil {
				return a, pending, fmt.Errorf("attempt %s: read scope: %w: %w", a.ID, ErrOutcomeUnknown, err)
			}
			switch look.State {
			case ScopeCommitted:
				res, err := resultFromReceipt(a, look.Receipt, a.PendingKey)
				if err != nil {
					return a, pending, err
				}
				out, err := s.settleGuarded(ctx, c, a, callOutcome{kind: outcomeCommitted, result: res}, g)
				return out, Settlement{Settled: true, Outcome: string(outcomeCommitted)}, err
			case ScopeNone:
				// A proof that ended or expired commits nothing more once
				// the grace period after its end has passed: a "none" read
				// from then on is final for it (none_finality).
				if !readAt.Before(p.end().Add(NoneFinalAfter)) {
					if err := s.markScanFinal(ctx, a.ID, proofLookup(p.digest), readAt); err != nil {
						return a, pending, err
					}
				}
			default:
				return a, pending, fmt.Errorf("attempt %s: read scope answered %q: %w", a.ID, look.State, ErrOutcomeUnknown)
			}
		}
	}
	// Nothing committed yet. A report of a refusal or an unknown outcome
	// ends the step's proofs, so nothing can commit under them any more; a
	// report of a commit that the read scope does not show yet waits.
	if report.Outcome != HintCommitted && !report.reconciled {
		if err := s.voidStep(ctx, c, a, report, g); err != nil {
			return a, Settlement{}, err
		}
		if proofs, err = s.stepProofs(ctx, a.ID, requestKey); err != nil {
			return a, Settlement{}, err
		}
	}
	if reader == nil {
		return a, pending, nil
	}
	var last time.Time
	for _, p := range proofs {
		end := p.end()
		if end.After(asked) {
			return a, pending, nil // a proof still lives: aimem may still commit under it
		}
		if end.After(last) {
			last = end
		}
	}
	if final := last.Add(NoneFinalAfter); asked.Before(final) {
		return a, Settlement{RetryAfter: max(final.Sub(s.now()), 0)}, nil
	}
	out, err := s.settleGuarded(ctx, c, a, callOutcome{kind: outcomeNotCommitted}, g)
	return out, Settlement{Settled: true, Outcome: string(outcomeNotCommitted)}, err
}

// voidStep ends the pending step's live proofs. Its audit record keeps the
// member's report.
func (s *Store) voidStep(ctx context.Context, c Caller, a Attempt, report StepReport, g guard) error {
	key, err := newID(s.now())
	if err != nil {
		return err
	}
	var out stepRef
	return s.run(ctx, c, command{
		op: opVoidStep, scope: a.ID, key: key, authorize: settleCallers,
		input: struct {
			stepRef
			Report StepReport `json:"report"`
		}{stepRef{a.ID, a.PendingKey}, report},
		check: g.then(pendingStep(a)),
		apply: func(ctx context.Context, tx *sql.Tx, now time.Time) (any, error) {
			if _, err := tx.ExecContext(ctx,
				`UPDATE coordination_proofs SET ended_at = ? WHERE attempt_id = ? AND request_key = ? AND ended_at = ''`,
				formatTime(now), a.ID, a.PendingKey); err != nil {
				return nil, fmt.Errorf("void step: %w", err)
			}
			return stepRef{a.ID, a.PendingKey}, nil
		},
	}, &out)
}

type stepProof struct {
	digest         string
	expires, ended time.Time
}

// end is when the proof stopped being usable: its expiry, or its end if
// earlier.
func (p stepProof) end() time.Time {
	if !p.ended.IsZero() && p.ended.Before(p.expires) {
		return p.ended
	}
	return p.expires
}

// stepProofs lists every proof issued for one step, replaced ones included.
func (s *Store) stepProofs(ctx context.Context, attemptID, requestKey string) ([]stepProof, error) {
	var out []stepProof
	err := s.snapshot(ctx, func(q querier) error {
		rows, err := q.QueryContext(ctx,
			`SELECT digest, expires_at, ended_at FROM coordination_proofs
			 WHERE attempt_id = ? AND request_key = ?
			 ORDER BY issued_at DESC, rowid DESC`, attemptID, requestKey)
		if err != nil {
			return fmt.Errorf("read step proofs: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			var p stepProof
			var expires, ended string
			if err := rows.Scan(&p.digest, &expires, &ended); err != nil {
				return err
			}
			if p.expires, err = parseTime(expires); err != nil {
				return err
			}
			if ended != "" {
				if p.ended, err = parseTime(ended); err != nil {
					return err
				}
			}
			out = append(out, p)
		}
		return rows.Err()
	})
	return out, err
}

// resultFromReceipt turns the read scope's receipt for the pending step into
// the committed result the store applies, after checking that it is this
// step's: its operation, the digest of key (the step's key, or a superseded
// alias of it) and its task.
func resultFromReceipt(a Attempt, r *ScopeReceipt, key string) (ReservationResult, error) {
	if r == nil || r.Operation != string(a.PendingOp) || r.RequestKeyDigest != requestKeyDigest(key) ||
		r.TaskID != a.Task.TaskID || r.VerifiedMode != "team" {
		return ReservationResult{}, fmt.Errorf("attempt %s: the read scope's receipt is not the pending step's: %w", a.ID, ErrOutcomeUnknown)
	}
	st := ReservationState{Fence: r.Fence}
	switch a.PendingOp {
	case ReservationClaim:
		st.ID, st.Active, st.HolderMode, st.OwnWorkRef = r.ReservationID, true, "external", a.claimRef()
	case ReservationTransfer, ReservationUpdate:
		st.ID, st.Active, st.HolderMode, st.OwnWorkRef = r.ReservationID, true, "external", a.attemptRef()
	}
	res := ReservationResult{
		Receipt: ReservationReceipt{ID: r.ID, State: ReceiptCommitted, Operation: r.Operation, RequestKey: a.PendingKey,
			VerifiedMode: r.VerifiedMode},
		TaskRevision: r.TaskRevision, Reservation: st,
	}
	if o := classify(a, res, nil); o.kind != outcomeCommitted {
		return ReservationResult{}, fmt.Errorf("attempt %s: %s: %w", a.ID, o.detail, ErrOutcomeUnknown)
	}
	return res, nil
}
