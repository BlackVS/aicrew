package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"reflect"
	"time"
)

// Identity proof, per the context contract and docs/ONBOARDING-CONTRACT.md.
//
// The store issues a challenge bound to an existing agent record or to an
// invitation. The client obtains a single-use aimem receipt for it with its
// own individual aimem credential. A Verifier redeems the receipt with aimem
// and returns the verified identity; only then does the store change a link,
// inside one transaction that rechecks the challenge. The receipt is a Secret
// and never reaches digests, audit or storage.

var (
	// ErrChallengeInvalid refuses an unknown, expired, consumed or superseded
	// challenge, or one bound to something else.
	ErrChallengeInvalid = errors.New("challenge_invalid")
	// ErrIdentityMismatch refuses a proof whose hub or user does not match
	// what the challenge, invitation or existing link requires.
	ErrIdentityMismatch = errors.New("identity_mismatch")
	// ErrIdentityAlreadyLinked refuses linking an aimem user that is already
	// linked to another agent.
	ErrIdentityAlreadyLinked = errors.New("identity_already_linked")
	// ErrRoleConflict refuses a membership that already exists with another
	// role; role changes are operator operations.
	ErrRoleConflict = errors.New("role_conflict")
	// ErrWorkOutstanding refuses an identity change while the agent holds
	// work that must be reconciled first.
	ErrWorkOutstanding = errors.New("work_outstanding")
)

// challengeTTL is the context contract's upper bound for a challenge.
const challengeTTL = 5 * time.Minute

// Secret holds a value that must never be printed, stored or audited, such as
// an aimem receipt. Formatting and JSON encoding show only a placeholder.
type Secret struct{ value string }

// NewSecret wraps a secret value.
func NewSecret(v string) Secret { return Secret{value: v} }

// Reveal returns the secret value, for handing to a Verifier only.
func (s Secret) Reveal() string { return s.value }

func (Secret) String() string               { return "[redacted]" }
func (Secret) GoString() string             { return "store.Secret{[redacted]}" }
func (Secret) MarshalJSON() ([]byte, error) { return []byte(`"[redacted]"`), nil }

// VerifiedIdentity is what aimem vouches for after redeeming a receipt.
type VerifiedIdentity struct {
	HubID   string `json:"hub_id"`
	UserID  string `json:"user_id"`
	TokenID string `json:"token_id"`
}

// RedeemRequest asks aimem to redeem one receipt for one challenge. A retry
// must reuse RequestKey so aimem returns the original result.
type RedeemRequest struct {
	ChallengeID string
	HubID       string
	Receipt     Secret
	RequestKey  string
}

// Verifier redeems aimem receipts. A production implementation calls aimem
// service-to-service; none exists yet, and tests use fakes. An implementation
// returns an error, never a partial identity, when verification fails or
// aimem is unavailable.
type Verifier interface {
	Redeem(ctx context.Context, req RedeemRequest) (VerifiedIdentity, error)
}

type challengeKind string

const (
	challengeAgent      challengeKind = "agent"
	challengeInvitation challengeKind = "invitation"
)

// Challenge is an identity-proof challenge. It carries no authority.
type Challenge struct {
	ID           string        `json:"id"`
	Kind         challengeKind `json:"kind"`
	AgentID      string        `json:"agent_id,omitempty"`
	InvitationID string        `json:"invitation_id,omitempty"`
	HubID        string        `json:"hub_id"`
	State        string        `json:"state"`
	ExpiresAt    time.Time     `json:"expires_at"`
	CreatedAt    time.Time     `json:"created_at"`
}

// ProofResult reports a completed re-proof of a linked agent.
type ProofResult struct {
	ChallengeID string           `json:"challenge_id"`
	AgentID     string           `json:"agent_id"`
	Identity    VerifiedIdentity `json:"identity"`
	Rotated     bool             `json:"rotated"`
}

const (
	opIssueAgentChallenge = "identity.issue_agent_challenge"
	opCompleteAgentProof  = "identity.complete_agent_proof"
)

// anyCaller admits every caller: challenges carry no authority (context
// contract), and completing one requires a receipt only aimem can vouch for.
func anyCaller(Caller) error { return nil }

type agentChallengeRequest struct {
	AgentID string `json:"agent_id"`
}

// IssueAgentChallenge issues a challenge for re-proving an already linked
// agent, for session start or credential rotation. An unlinked agent is
// linked through an invitation instead.
func (s *Store) IssueAgentChallenge(ctx context.Context, c Caller, key, agentID string) (Challenge, error) {
	var out Challenge
	err := s.run(ctx, c, command{
		op: opIssueAgentChallenge, scope: agentID, key: key,
		input: agentChallengeRequest{AgentID: agentID}, authorize: anyCaller,
		apply: func(ctx context.Context, tx *sql.Tx, now time.Time) (any, error) {
			agent, err := getAgent(ctx, tx, agentID)
			if err != nil {
				return nil, err
			}
			if agent.Linked == nil {
				return nil, fmt.Errorf("agent %s: %w", agentID, ErrIdentityLinkRequired)
			}
			return insertChallenge(ctx, tx, challengeAgent, agentID, "", agent.Linked.HubID, now, time.Time{})
		},
	}, &out)
	return out, err
}

type agentProofRequest struct {
	ChallengeID string `json:"challenge_id"`
}

// CompleteAgentProof redeems a receipt for an agent challenge and requires
// the verified identity to be the one already linked to that agent. The same
// user with a new credential is a rotation: the link records the new token
// and the agent's active sessions are rebound to it under a new generation.
//
// The verifier runs before any write; if it fails or is unavailable, nothing
// changes. A retry with the same key returns the recorded result; one that
// overlaps its still running original waits for it first (inflight.go).
func (s *Store) CompleteAgentProof(ctx context.Context, c Caller, v Verifier, key, challengeID string, receipt Secret) (ProofResult, error) {
	var out ProofResult
	cmd := command{
		op: opCompleteAgentProof, scope: challengeID, key: key,
		input: agentProofRequest{ChallengeID: challengeID}, authorize: anyCaller,
	}
	release, err := s.flights.acquire(ctx, c, cmd, s.flightWait)
	if err != nil {
		return out, err
	}
	defer release()
	if done, err := s.receiptExists(ctx, c, cmd); err != nil || done {
		if err != nil {
			return out, err
		}
		cmd.apply = func(context.Context, *sql.Tx, time.Time) (any, error) {
			return nil, fmt.Errorf("challenge %s: %w", challengeID, ErrChallengeInvalid)
		}
		return out, s.run(ctx, c, cmd, &out)
	}

	if s.afterReceiptLookup != nil {
		s.afterReceiptLookup(cmd.op)
	}
	ch, err := s.pendingChallenge(ctx, challengeID, challengeAgent)
	if err != nil {
		return out, err
	}
	verified, err := verifyChallenge(ctx, v, ch, receipt, "agent-proof:"+challengeID+":"+key)
	if err != nil {
		return out, err
	}
	cmd.check = func(ctx context.Context, tx *sql.Tx) error {
		_, err := currentChallenge(ctx, tx, challengeID, challengeAgent, s.now())
		return err
	}
	cmd.apply = func(ctx context.Context, tx *sql.Tx, now time.Time) (any, error) {
		agent, err := getAgent(ctx, tx, ch.AgentID)
		if err != nil {
			return nil, err
		}
		if agent.Linked == nil {
			return nil, fmt.Errorf("agent %s: %w", agent.ID, ErrIdentityLinkRequired)
		}
		if agent.Linked.HubID != verified.HubID || agent.Linked.UserID != verified.UserID {
			return nil, fmt.Errorf("agent %s is linked to another identity: %w", agent.ID, ErrIdentityMismatch)
		}
		rotated, err := rotateIfNeeded(ctx, tx, agent, verified, now)
		if err != nil {
			return nil, err
		}
		if err := consumeChallenge(ctx, tx, challengeID, now); err != nil {
			return nil, err
		}
		return ProofResult{ChallengeID: challengeID, AgentID: agent.ID, Identity: verified, Rotated: rotated}, nil
	}
	return out, s.run(ctx, c, cmd, &out)
}

// verifyChallenge redeems a receipt and checks that the answer is a complete
// identity on the challenge's hub. It writes nothing.
func verifyChallenge(ctx context.Context, v Verifier, ch Challenge, receipt Secret, requestKey string) (VerifiedIdentity, error) {
	if v == nil {
		return VerifiedIdentity{}, fmt.Errorf("%w: no verifier", ErrInvalid)
	}
	if receipt.Reveal() == "" {
		return VerifiedIdentity{}, fmt.Errorf("%w: empty receipt", ErrInvalid)
	}
	id, err := v.Redeem(ctx, RedeemRequest{ChallengeID: ch.ID, HubID: ch.HubID, Receipt: receipt, RequestKey: requestKey})
	if err != nil {
		return VerifiedIdentity{}, fmt.Errorf("verify challenge %s: %w", ch.ID, err)
	}
	if id.HubID == "" || id.UserID == "" || id.TokenID == "" {
		return VerifiedIdentity{}, fmt.Errorf("verify challenge %s: incomplete identity: %w", ch.ID, ErrIdentityMismatch)
	}
	if err := validateText(reflect.ValueOf(id)); err != nil {
		return VerifiedIdentity{}, err
	}
	if id.HubID != ch.HubID {
		return VerifiedIdentity{}, fmt.Errorf("verify challenge %s: hub %s, want %s: %w", ch.ID, id.HubID, ch.HubID, ErrIdentityMismatch)
	}
	return id, nil
}

// rotateIfNeeded records a new credential for the same identity and rebinds
// the agent's active sessions to it, advancing each session's generation so
// commands under the old credential are fenced.
func rotateIfNeeded(ctx context.Context, tx *sql.Tx, agent Agent, id VerifiedIdentity, now time.Time) (bool, error) {
	if agent.Linked.TokenID == id.TokenID {
		return false, nil
	}
	at := formatTime(now)
	if _, err := tx.ExecContext(ctx,
		`UPDATE agents SET linked_token_id = ?, revision = revision + 1, updated_at = ? WHERE id = ?`,
		id.TokenID, at, agent.ID); err != nil {
		return false, fmt.Errorf("rotate credential: %w", err)
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE sessions SET token_id = ?, generation = generation + 1, updated_at = ?
		 WHERE agent_id = ? AND state = 'active'`,
		id.TokenID, at, agent.ID); err != nil {
		return false, fmt.Errorf("rebind sessions: %w", err)
	}
	return true, nil
}

// setLink writes a verified identity to an agent record.
func setLink(ctx context.Context, tx *sql.Tx, agentID string, id VerifiedIdentity, now time.Time) error {
	if _, err := tx.ExecContext(ctx,
		`UPDATE agents SET linked_hub_id = ?, linked_user_id = ?, linked_token_id = ?,
		        revision = revision + 1, updated_at = ?
		 WHERE id = ?`,
		id.HubID, id.UserID, id.TokenID, formatTime(now), agentID); err != nil {
		return fmt.Errorf("set link: %w", err)
	}
	return nil
}

// agentByIdentity finds the agent linked to an aimem identity, if any.
func agentByIdentity(ctx context.Context, q querier, hubID, userID string) (Agent, error) {
	var id string
	err := q.QueryRowContext(ctx,
		`SELECT id FROM agents WHERE linked_hub_id = ? AND linked_user_id = ?`, hubID, userID).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return Agent{}, fmt.Errorf("identity %s/%s: %w", hubID, userID, ErrNotFound)
	}
	if err != nil {
		return Agent{}, fmt.Errorf("find linked agent: %w", err)
	}
	return getAgent(ctx, q, id)
}

// insertChallenge stores a pending challenge that expires after challengeTTL,
// or at notAfter if that is sooner.
func insertChallenge(ctx context.Context, tx *sql.Tx, kind challengeKind, agentID, invitationID, hubID string,
	now, notAfter time.Time) (Challenge, error) {
	id, err := newID(now)
	if err != nil {
		return Challenge{}, err
	}
	var agentArg, invitationArg any
	if agentID != "" {
		agentArg = agentID
	}
	if invitationID != "" {
		invitationArg = invitationID
	}
	expires := now.Add(challengeTTL)
	if !notAfter.IsZero() && notAfter.Before(expires) {
		expires = notAfter
	}
	at := formatTime(now)
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO challenges (id, kind, agent_id, invitation_id, hub_id, state, expires_at, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, 'pending', ?, ?, ?)`,
		id, string(kind), agentArg, invitationArg, hubID, formatTime(expires), at, at); err != nil {
		return Challenge{}, fmt.Errorf("insert challenge: %w", err)
	}
	return getChallenge(ctx, tx, id)
}

// pendingChallenge reads a challenge before verification, so an obviously
// unusable one is refused without calling aimem. The transaction rechecks it.
func (s *Store) pendingChallenge(ctx context.Context, id string, kind challengeKind) (Challenge, error) {
	var ch Challenge
	err := s.snapshot(ctx, func(q querier) error {
		var err error
		ch, err = currentChallenge(ctx, q, id, kind, s.now())
		return err
	})
	return ch, err
}

// currentChallenge requires a pending, unexpired challenge of the given kind.
func currentChallenge(ctx context.Context, q querier, id string, kind challengeKind, now time.Time) (Challenge, error) {
	ch, err := getChallenge(ctx, q, id)
	if errors.Is(err, ErrNotFound) {
		return Challenge{}, fmt.Errorf("challenge %s: %w", id, ErrChallengeInvalid)
	}
	if err != nil {
		return Challenge{}, err
	}
	if ch.Kind != kind || ch.State != "pending" || !now.Before(ch.ExpiresAt) {
		return Challenge{}, fmt.Errorf("challenge %s is %s %s, expires %s: %w",
			id, ch.State, ch.Kind, ch.ExpiresAt.Format(time.RFC3339), ErrChallengeInvalid)
	}
	return ch, nil
}

func consumeChallenge(ctx context.Context, tx *sql.Tx, id string, now time.Time) error {
	res, err := tx.ExecContext(ctx,
		`UPDATE challenges SET state = 'consumed', updated_at = ? WHERE id = ? AND state = 'pending'`,
		formatTime(now), id)
	if err != nil {
		return fmt.Errorf("consume challenge: %w", err)
	}
	if n, err := res.RowsAffected(); err != nil {
		return err
	} else if n != 1 {
		return fmt.Errorf("challenge %s: %w", id, ErrChallengeInvalid)
	}
	return nil
}

func getChallenge(ctx context.Context, q querier, id string) (Challenge, error) {
	var (
		ch                        Challenge
		kind                      string
		agentID, invitationID     sql.NullString
		expires, created, updated string
	)
	err := q.QueryRowContext(ctx,
		`SELECT id, kind, agent_id, invitation_id, hub_id, state, expires_at, created_at, updated_at
		 FROM challenges WHERE id = ?`, id).
		Scan(&ch.ID, &kind, &agentID, &invitationID, &ch.HubID, &ch.State, &expires, &created, &updated)
	if errors.Is(err, sql.ErrNoRows) {
		return Challenge{}, fmt.Errorf("challenge %s: %w", id, ErrNotFound)
	}
	if err != nil {
		return Challenge{}, fmt.Errorf("read challenge: %w", err)
	}
	ch.Kind, ch.AgentID, ch.InvitationID = challengeKind(kind), agentID.String, invitationID.String
	if ch.ExpiresAt, err = parseTime(expires); err != nil {
		return Challenge{}, err
	}
	if ch.CreatedAt, err = parseTime(created); err != nil {
		return Challenge{}, err
	}
	return ch, nil
}
