package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"
)

// Invitation redemption (docs/ONBOARDING-CONTRACT.md, "Redemption").
//
// The holder of an invitation code begins redemption and receives a
// challenge, obtains a single-use aimem receipt for it with its own aimem
// credential, and completes. Completion calls the Verifier with no
// transaction open, then commits the invitation's redemption, the identity
// link, any new agent, the membership, the audit record and the retry
// receipt in one transaction that rechecks everything mutable first.
//
// Retries: each command's receipt is scoped to the invitation by the code's
// digest, and its input holds the digest (and, for completion, the challenge
// ID). Neither the code nor the aimem receipt is ever part of an input,
// audit record or retry receipt. The same key with the same input replays;
// the same key with different input is ErrIdempotencyConflict.

const (
	opBeginRedemption    = "redemption.begin"
	opCompleteRedemption = "redemption.complete"
)

// holder is the caller for redemption: the invitation code, not an identity,
// is the authority, and the code is checked inside every command.
var holder = Caller{}

type redemptionBegin struct {
	CodeDigest string `json:"code_digest"`
}

type redemptionComplete struct {
	CodeDigest  string `json:"code_digest"`
	ChallengeID string `json:"challenge_id"`
}

// BeginRedemption starts redeeming an invitation and returns a challenge for
// the holder to prove its aimem identity against. It counts one attempt; the
// fifth locks the invitation against further begins. The challenge expires
// after at most five minutes and never after the invitation.
//
// A retry with the same key returns the same challenge while that challenge
// and its invitation are still usable. Otherwise the retry is refused and the
// holder begins again with a new key, which counts as a new attempt.
func (s *Store) BeginRedemption(ctx context.Context, key string, code Secret) (Challenge, error) {
	digest, err := invitationCodeDigest(code)
	if err != nil {
		return Challenge{}, ErrInvitationInvalid
	}
	var out Challenge
	err = s.run(ctx, holder, command{
		op: opBeginRedemption, scope: digest, key: key,
		input: redemptionBegin{CodeDigest: digest}, authorize: anyCaller,
		replayCheck: func(ctx context.Context, tx *sql.Tx, result string) error {
			var recorded Challenge
			if err := json.Unmarshal([]byte(result), &recorded); err != nil {
				return fmt.Errorf("decode recorded challenge: %w", err)
			}
			now := s.now()
			if _, _, err := resolveForCompletion(ctx, tx, code, now); err != nil {
				return err
			}
			_, err := currentChallenge(ctx, tx, recorded.ID, challengeInvitation, now)
			return err
		},
		apply: func(ctx context.Context, tx *sql.Tx, now time.Time) (any, error) {
			inv, sc, err := resolveForBegin(ctx, tx, code, now)
			if err != nil {
				return nil, err
			}
			if _, err := recordBeginAttempt(ctx, tx, inv, now); err != nil {
				return nil, err
			}
			return issueInvitationChallenge(ctx, tx, sc, now)
		},
	}, &out)
	return out, err
}

// CompleteRedemption finishes redeeming an invitation with the aimem receipt
// the holder obtained for the challenge.
//
// Order of work:
//  0. An identical completion (same key and code) still running is waited
//     for, so a retry that overlaps its original answers from the
//     original's receipt instead of racing it (inflight.go).
//  1. A retry of an already committed completion is answered from its
//     receipt without calling aimem again.
//  2. Pre-checks on a read snapshot, writing nothing: the invitation is
//     usable and the challenge is its current one. aimem is not asked about
//     an invitation or challenge that cannot succeed.
//  3. The Verifier redeems the receipt with no transaction open. If it
//     fails or aimem is unavailable, nothing changes.
//  4. One write transaction re-resolves the invitation and rechecks the
//     challenge, so a revocation or another completion that committed in
//     the meantime wins; then it applies the binding rules and marks the
//     invitation redeemed. Link, agent, membership, invitation state, audit
//     record and retry receipt commit together or not at all.
func (s *Store) CompleteRedemption(ctx context.Context, v Verifier, key string, code Secret,
	challengeID string, receipt Secret) (LinkResult, error) {
	digest, err := invitationCodeDigest(code)
	if err != nil {
		return LinkResult{}, ErrInvitationInvalid
	}
	var out LinkResult
	cmd := command{
		op: opCompleteRedemption, scope: digest, key: key,
		input:     redemptionComplete{CodeDigest: digest, ChallengeID: challengeID},
		authorize: anyCaller,
	}
	release, err := s.flights.acquire(ctx, holder, cmd, s.flightWait)
	if err != nil {
		return out, err
	}
	defer release()
	if done, err := s.receiptExists(ctx, holder, cmd); err != nil || done {
		if err != nil {
			return out, err
		}
		// run answers from the receipt, or reports a conflicting reuse of
		// the key; apply is never reached.
		cmd.apply = func(context.Context, *sql.Tx, time.Time) (any, error) {
			return nil, fmt.Errorf("challenge %s: %w", challengeID, ErrChallengeInvalid)
		}
		return out, s.run(ctx, holder, cmd, &out)
	}

	if s.afterReceiptLookup != nil {
		s.afterReceiptLookup(cmd.op)
	}
	var ch Challenge
	err = s.snapshot(ctx, func(q querier) error {
		now := s.now()
		inv, _, err := resolveForCompletion(ctx, q, code, now)
		if err != nil {
			return err
		}
		if ch, err = currentChallenge(ctx, q, challengeID, challengeInvitation, now); err != nil {
			return err
		}
		if ch.InvitationID != inv.ID {
			return fmt.Errorf("challenge %s belongs to another invitation: %w", challengeID, ErrChallengeInvalid)
		}
		return nil
	})
	if err != nil {
		return out, err
	}

	id, err := verifyChallenge(ctx, v, ch, receipt, "redeem:"+challengeID+":"+key)
	if err != nil {
		return out, err
	}

	cmd.apply = func(ctx context.Context, tx *sql.Tx, now time.Time) (any, error) {
		inv, sc, err := resolveForCompletion(ctx, tx, code, now)
		if err != nil {
			return nil, err
		}
		res, err := s.bindInvitation(ctx, tx, sc, challengeID, id, now)
		if err != nil {
			return nil, err
		}
		if _, err := markInvitationRedeemed(ctx, tx, inv, now); err != nil {
			return nil, err
		}
		return res, nil
	}
	return out, s.run(ctx, holder, cmd, &out)
}
