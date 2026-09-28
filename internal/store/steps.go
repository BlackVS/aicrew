package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
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
type Step struct {
	Operation         ReservationOp      `json:"operation"`
	RequestKey        string             `json:"request_key"`
	ExpectedRevision  int64              `json:"expected_revision"`
	ReservationID     string             `json:"reservation_id,omitempty"`
	Fence             string             `json:"fence,omitempty"`
	Holder            *ReservationHolder `json:"holder,omitempty"`
	CoordinationProof string             `json:"coordination_proof"`
}

func stepFor(a Attempt, proof string) Step {
	req := reservationRequest(a, proof)
	return Step{Operation: a.PendingOp, RequestKey: a.PendingKey, ExpectedRevision: req.ExpectedRevision,
		ReservationID: req.ReservationID, Fence: req.Fence, Holder: req.Holder, CoordinationProof: proof}
}

// BeginOffer begins an offer: the coordinator's claim of the task for a named
// worker, whose capacity the intent takes.
func (s *Store) BeginOffer(ctx context.Context, c Caller, key string, in OfferRequest) (Attempt, Step, error) {
	var proof string
	cmd := offerCommand(c, key, in, &proof)
	release, err := s.flights.acquire(ctx, c, cmd, s.flightWait)
	if err != nil {
		return Attempt{}, Step{}, err
	}
	defer release()
	return s.begin(ctx, c, cmd, "", &proof, FactOffer, in.SessionID, in.Generation)
}

// BeginAccept begins the named worker's acceptance: the transfer of the
// offer's hold to its attempt.
func (s *Store) BeginAccept(ctx context.Context, c Caller, key, attemptID string, in AcceptRequest) (Attempt, Step, error) {
	var proof string
	return s.begin(ctx, c, acceptCommand(c, key, attemptID, in, &proof), attemptID, &proof,
		FactAcceptedAttempt, in.SessionID, in.Generation)
}

// BeginRelease begins the release of an offer never accepted (declined,
// withdrawn or expired), by the team's current coordinator.
func (s *Store) BeginRelease(ctx context.Context, c Caller, key, attemptID, sessionID string, generation int64) (Attempt, Step, error) {
	var proof string
	return s.begin(ctx, c, releaseCommand(c, key, attemptID, sessionID, generation, &proof), attemptID, &proof,
		FactNeverAccepted, sessionID, generation)
}

// begin runs a step's intent. A replay of the same key finds the intent
// recorded but not its proof, which is never stored: while the step is still
// pending, the replay gets a replacement proof for the same intent and key,
// and the earlier proof ends at once (D-b1a-1), so aimem refuses it if it was
// already on its way. A replay after the step settled reports that step's
// outcome, with no step to send.
func (s *Store) begin(ctx context.Context, c Caller, cmd command, attemptID string, proof *string, kind FactKind,
	sessionID string, generation int64) (Attempt, Step, error) {
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
	if *proof == "" {
		if *proof, err = s.replaceProof(ctx, c, cur, kind, sessionID, generation); err != nil {
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
// same intent, acted by the same session.
func (s *Store) replaceProof(ctx context.Context, c Caller, a Attempt, kind FactKind, sessionID string, generation int64) (string, error) {
	key, err := newID(s.now())
	if err != nil {
		return "", err
	}
	var proof string
	var out stepRef
	err = s.run(ctx, c, command{
		op: opReplaceProof, scope: a.ID, key: key, input: stepRef{a.ID, a.PendingKey}, authorize: requireAgent,
		check: pendingStep(a),
		apply: func(ctx context.Context, tx *sql.Tx, now time.Time) (any, error) {
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

const (
	HintCommitted StepHint = "committed"
	HintRefused   StepHint = "refused"
	HintUnknown   StepHint = "unknown"
)

// Settlement says whether a settle settled the step and, if not, when to ask
// again.
type Settlement struct {
	Settled    bool
	RetryAfter time.Duration
}

// SettleStep settles the attempt's pending coordinated step with requestKey.
// The caller must be the operator or an active member of the attempt's team.
// reader may be nil while aicrew has no read scope: the step then stays
// pending, whatever the member reports (D-b1a-2).
func (s *Store) SettleStep(ctx context.Context, c Caller, reader ReservationReader, attemptID, requestKey string,
	hint StepHint) (Attempt, Settlement, error) {
	switch hint {
	case HintCommitted, HintRefused, HintUnknown:
	default:
		return Attempt{}, Settlement{}, fmt.Errorf("%w: the outcome must be committed, refused or unknown", ErrInvalid)
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
	if a.PendingKey != requestKey {
		// Not the pending step: an earlier step of this attempt, whose
		// recorded outcome answers, or no step of this attempt at all.
		out, err := s.stepOutcome(ctx, a, requestKey)
		return out, Settlement{Settled: !errors.Is(err, ErrOutcomeUnknown)}, err
	}
	proofs, err := s.stepProofs(ctx, a.ID, requestKey)
	if err != nil {
		return a, Settlement{}, err
	}
	if len(proofs) == 0 {
		return a, Settlement{}, fmt.Errorf("%w: step %s has no coordination proof to settle by", ErrInvalid, requestKey)
	}
	pending := Settlement{RetryAfter: NoneFinalAfter}
	if reader != nil {
		// A proof replaced by a replay may still have been used: any of
		// the intent's proofs can carry the committed transition.
		for _, p := range proofs {
			digest, err := proofDigestP1(p.digest)
			if err != nil {
				return a, Settlement{}, err
			}
			look, err := reader.ReceiptByProof(ctx, digest)
			if err != nil {
				return a, pending, fmt.Errorf("attempt %s: read scope: %w: %w", a.ID, ErrOutcomeUnknown, err)
			}
			switch look.State {
			case ScopeCommitted:
				res, err := resultFromReceipt(a, look.Receipt)
				if err != nil {
					return a, pending, err
				}
				out, err := s.settle(ctx, c, a, callOutcome{kind: outcomeCommitted, result: res})
				return out, Settlement{Settled: true}, err
			case ScopeNone:
			default:
				return a, pending, fmt.Errorf("attempt %s: read scope answered %q: %w", a.ID, look.State, ErrOutcomeUnknown)
			}
		}
	}
	// Nothing committed yet. A report of a refusal or an unknown outcome
	// ends the step's proofs, so nothing can commit under them any more; a
	// report of a commit that the read scope does not show yet waits.
	if hint != HintCommitted {
		if err := s.voidStep(ctx, c, a); err != nil {
			return a, Settlement{}, err
		}
		if proofs, err = s.stepProofs(ctx, a.ID, requestKey); err != nil {
			return a, Settlement{}, err
		}
	}
	if reader == nil {
		return a, pending, nil
	}
	now := s.now()
	var last time.Time
	for _, p := range proofs {
		end := p.expires
		if !p.ended.IsZero() && p.ended.Before(end) {
			end = p.ended
		}
		if end.After(now) {
			return a, pending, nil // a proof still lives: aimem may still commit under it
		}
		if end.After(last) {
			last = end
		}
	}
	if final := last.Add(NoneFinalAfter); now.Before(final) {
		return a, Settlement{RetryAfter: final.Sub(now)}, nil
	}
	out, err := s.settle(ctx, c, a, callOutcome{kind: outcomeNotCommitted})
	return out, Settlement{Settled: true}, err
}

// voidStep ends the pending step's live proofs.
func (s *Store) voidStep(ctx context.Context, c Caller, a Attempt) error {
	key, err := newID(s.now())
	if err != nil {
		return err
	}
	var out stepRef
	return s.run(ctx, c, command{
		op: opVoidStep, scope: a.ID, key: key, input: stepRef{a.ID, a.PendingKey}, authorize: anyCaller,
		check: pendingStep(a),
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

// stepProofs lists every proof issued for one step, replaced ones included.
func (s *Store) stepProofs(ctx context.Context, attemptID, requestKey string) ([]stepProof, error) {
	var out []stepProof
	err := s.snapshot(ctx, func(q querier) error {
		rows, err := q.QueryContext(ctx,
			`SELECT digest, expires_at, ended_at FROM coordination_proofs WHERE attempt_id = ? AND request_key = ?
			 ORDER BY issued_at, digest`, attemptID, requestKey)
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
// step's: its operation, its request key's digest and its task.
func resultFromReceipt(a Attempt, r *ScopeReceipt) (ReservationResult, error) {
	if r == nil || r.Operation != string(a.PendingOp) || r.RequestKeyDigest != requestKeyDigest(a.PendingKey) ||
		r.TaskID != a.Task.TaskID || r.VerifiedMode != "team" {
		return ReservationResult{}, fmt.Errorf("attempt %s: the read scope's receipt is not the pending step's: %w", a.ID, ErrOutcomeUnknown)
	}
	st := ReservationState{Fence: r.Fence}
	switch a.PendingOp {
	case ReservationClaim:
		st.ID, st.Active, st.HolderMode, st.OwnWorkRef = r.ReservationID, true, "external", a.claimRef()
	case ReservationTransfer:
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
