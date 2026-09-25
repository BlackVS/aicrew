package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// The invitation store (docs/ONBOARDING-CONTRACT.md, "Invitations").
//
// Operators issue and revoke invitations. The store keeps only the digest of
// each code, and never the code itself, not even in audit or retry receipts.
// A lost issue reply is retried with the same key and code, which replays the
// same invitation. A lost code cannot be recovered: the operator revokes the
// invitation and issues a new one.
//
// Redemption (crew-onboarding) composes the package-internal functions at the
// end of this file with issueInvitationChallenge and bindInvitation inside
// its own command transactions, so an invitation's state changes commit
// together with the challenge, link and membership they cause.

var (
	// ErrInvitationInvalid is the single, non-disclosing refusal for a
	// presented code: unknown, malformed, expired, revoked, redeemed and
	// locked invitations all look the same to the holder.
	ErrInvitationInvalid = errors.New("invitation_invalid")
	// ErrInvitationFinal refuses revoking an invitation that is already
	// redeemed or revoked.
	ErrInvitationFinal = errors.New("invitation already redeemed or revoked")
)

const (
	defaultInvitationTTL  = 24 * time.Hour
	maxInvitationTTL      = 72 * time.Hour
	maxInvitationAttempts = 5
)

// InvitationState is the lifecycle state of an invitation. Expiry is not a
// state: an issued invitation past its deadline is refused.
type InvitationState string

const (
	InvitationIssued   InvitationState = "issued"
	InvitationRedeemed InvitationState = "redeemed"
	InvitationRevoked  InvitationState = "revoked"
	// InvitationLocked: the attempt limit is used up, so no new redemption
	// may begin. A challenge from the last allowed attempt may still finish.
	InvitationLocked InvitationState = "locked"
)

// InvitationRequest is what an operator asks for. The code is passed
// separately, as a Secret.
type InvitationRequest struct {
	Purpose        InvitationPurpose
	TeamID         string
	Role           Role
	HubID          string
	AgentID        string        // required for link and rebind
	ExpectedUserID string        // required for rebind; optional otherwise
	Label          string        // required for join
	TTL            time.Duration // zero means the 24-hour default; at most 72 hours
}

// Invitation is the stored invitation. It never contains the code or its
// digest.
type Invitation struct {
	ID             string            `json:"id"`
	Purpose        InvitationPurpose `json:"purpose"`
	TeamID         string            `json:"team_id"`
	Role           Role              `json:"role"`
	HubID          string            `json:"hub_id"`
	AgentID        string            `json:"agent_id,omitempty"`
	ExpectedUserID string            `json:"expected_user_id,omitempty"`
	Label          string            `json:"label,omitempty"`
	IssuedBy       string            `json:"issued_by"`
	State          InvitationState   `json:"state"`
	Attempts       int               `json:"attempts"`
	Revision       int64             `json:"revision"`
	ExpiresAt      time.Time         `json:"expires_at"`
	CreatedAt      time.Time         `json:"created_at"`
	UpdatedAt      time.Time         `json:"updated_at"`
}

const (
	opIssueInvitation  = "invitation.issue"
	opRevokeInvitation = "invitation.revoke"
)

// issueInput is what gets digested and audited for an issue command: the
// request and the code's digest, never the code.
type issueInput struct {
	Purpose        InvitationPurpose `json:"purpose"`
	TeamID         string            `json:"team_id"`
	Role           Role              `json:"role"`
	HubID          string            `json:"hub_id"`
	AgentID        string            `json:"agent_id"`
	ExpectedUserID string            `json:"expected_user_id"`
	Label          string            `json:"label"`
	TTLSeconds     int64             `json:"ttl_seconds"`
	CodeDigest     string            `json:"code_digest"`
}

// IssueInvitation records an invitation for a code the caller generated with
// GenerateInvitationCode. Operator only. The store validates the code's form
// and keeps its digest; the code is not stored anywhere.
func (s *Store) IssueInvitation(ctx context.Context, c Caller, key string, req InvitationRequest, code Secret) (Invitation, error) {
	in := issueInput{
		Purpose: req.Purpose, TeamID: req.TeamID, Role: req.Role, HubID: req.HubID,
		AgentID: req.AgentID, ExpectedUserID: req.ExpectedUserID, Label: req.Label,
		TTLSeconds: int64(req.TTL / time.Second),
	}
	var out Invitation
	err := s.run(ctx, c, command{
		op: opIssueInvitation, key: key, input: &in, authorize: requireOperator,
		validate: func() error {
			digest, err := invitationCodeDigest(code)
			if err != nil {
				return err
			}
			in.CodeDigest = digest
			if req.TTL == 0 {
				in.TTLSeconds = int64(defaultInvitationTTL / time.Second)
			}
			if req.TTL < 0 || req.TTL > maxInvitationTTL {
				return fmt.Errorf("%w: invitation lifetime must be at most %s", ErrInvalid, maxInvitationTTL)
			}
			// The same rules the scope enforces at redemption, checked now.
			_, err = newInvitationScope("pending", req.Purpose, req.TeamID, req.Role,
				req.HubID, req.AgentID, req.ExpectedUserID, req.Label)
			return err
		},
		apply: func(ctx context.Context, tx *sql.Tx, now time.Time) (any, error) {
			if _, err := getTeam(ctx, tx, in.TeamID); err != nil {
				return nil, err
			}
			if in.AgentID != "" {
				if _, err := getAgent(ctx, tx, in.AgentID); err != nil {
					return nil, err
				}
			}
			var one int
			err := tx.QueryRowContext(ctx, `SELECT 1 FROM invitations WHERE code_digest = ?`, in.CodeDigest).Scan(&one)
			if err == nil {
				return nil, fmt.Errorf("invitation code already used: %w", ErrExists)
			}
			if !errors.Is(err, sql.ErrNoRows) {
				return nil, fmt.Errorf("check code: %w", err)
			}
			id, err := newID(now)
			if err != nil {
				return nil, err
			}
			at := formatTime(now)
			expires := formatTime(now.Add(time.Duration(in.TTLSeconds) * time.Second))
			if _, err := tx.ExecContext(ctx,
				`INSERT INTO invitations (id, code_digest, purpose, team_id, role, hub_id, agent_id,
				        expected_user_id, label, issued_by, state, attempts, revision, expires_at, created_at, updated_at)
				 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 'issued', 0, 1, ?, ?, ?)`,
				id, in.CodeDigest, string(in.Purpose), in.TeamID, string(in.Role), in.HubID, in.AgentID,
				in.ExpectedUserID, in.Label, c.id, expires, at, at); err != nil {
				return nil, fmt.Errorf("insert invitation: %w", err)
			}
			return getInvitation(ctx, tx, id)
		},
	}, &out)
	return out, err
}

type revokeInput struct {
	InvitationID     string `json:"invitation_id"`
	ExpectedRevision int64  `json:"expected_revision"`
}

// RevokeInvitation revokes an invitation that has not been redeemed.
// Operator only. A redeemed invitation is final; undoing its effect means
// removing the membership.
func (s *Store) RevokeInvitation(ctx context.Context, c Caller, key, invitationID string, expectedRevision int64) (Invitation, error) {
	in := revokeInput{InvitationID: invitationID, ExpectedRevision: expectedRevision}
	var out Invitation
	err := s.run(ctx, c, command{
		op: opRevokeInvitation, scope: invitationID, key: key, input: in, authorize: requireOperator,
		apply: func(ctx context.Context, tx *sql.Tx, now time.Time) (any, error) {
			inv, err := getInvitation(ctx, tx, invitationID)
			if err != nil {
				return nil, err
			}
			if inv.State == InvitationRedeemed || inv.State == InvitationRevoked {
				return nil, fmt.Errorf("invitation %s is %s: %w", invitationID, inv.State, ErrInvitationFinal)
			}
			if inv.Revision != expectedRevision {
				return nil, fmt.Errorf("invitation %s: %w", invitationID, ErrRevisionConflict)
			}
			if err := setInvitationState(ctx, tx, inv, InvitationRevoked, inv.Attempts, now); err != nil {
				return nil, err
			}
			return getInvitation(ctx, tx, invitationID)
		},
	}, &out)
	return out, err
}

// GetInvitation reads an invitation for operator views. It never includes
// the code or its digest.
func (s *Store) GetInvitation(ctx context.Context, id string) (Invitation, error) {
	var inv Invitation
	err := s.snapshot(ctx, func(q querier) error {
		var err error
		inv, err = getInvitation(ctx, q, id)
		return err
	})
	return inv, err
}

// --- package-internal operations for redemption ---

// resolveForBegin finds the invitation for a presented code when a holder
// starts redeeming it. The invitation must be issued (not locked) and
// unexpired. Every refusal is ErrInvitationInvalid.
func resolveForBegin(ctx context.Context, q querier, code Secret, now time.Time) (Invitation, InvitationScope, error) {
	return resolveInvitation(ctx, q, code, now, InvitationIssued)
}

// resolveForCompletion finds the invitation when a holder completes
// redemption. A locked invitation still completes its last allowed attempt;
// the challenge check ensures only the newest challenge counts.
func resolveForCompletion(ctx context.Context, q querier, code Secret, now time.Time) (Invitation, InvitationScope, error) {
	return resolveInvitation(ctx, q, code, now, InvitationIssued, InvitationLocked)
}

func resolveInvitation(ctx context.Context, q querier, code Secret, now time.Time, allowed ...InvitationState) (Invitation, InvitationScope, error) {
	digest, err := invitationCodeDigest(code)
	if err != nil {
		return Invitation{}, InvitationScope{}, ErrInvitationInvalid
	}
	var id string
	err = q.QueryRowContext(ctx, `SELECT id FROM invitations WHERE code_digest = ?`, digest).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return Invitation{}, InvitationScope{}, ErrInvitationInvalid
	}
	if err != nil {
		return Invitation{}, InvitationScope{}, fmt.Errorf("find invitation: %w", err)
	}
	inv, err := getInvitation(ctx, q, id)
	if err != nil {
		return Invitation{}, InvitationScope{}, err
	}
	usable := false
	for _, st := range allowed {
		usable = usable || inv.State == st
	}
	if !usable || !now.Before(inv.ExpiresAt) {
		return Invitation{}, InvitationScope{}, ErrInvitationInvalid
	}
	sc, err := newInvitationScope(inv.ID, inv.Purpose, inv.TeamID, inv.Role,
		inv.HubID, inv.AgentID, inv.ExpectedUserID, inv.Label)
	if err != nil {
		return Invitation{}, InvitationScope{}, fmt.Errorf("stored invitation %s: %w", inv.ID, err)
	}
	sc.expiresAt = inv.ExpiresAt
	return inv, sc, nil
}

// recordBeginAttempt counts one redemption begin. The attempt that reaches
// the limit locks the invitation against further begins; a begin on a used
// up invitation is refused. Retries of a begin replay its receipt and are
// not counted again.
func recordBeginAttempt(ctx context.Context, tx *sql.Tx, inv Invitation, now time.Time) (Invitation, error) {
	if inv.State != InvitationIssued || inv.Attempts >= maxInvitationAttempts {
		return Invitation{}, ErrInvitationInvalid
	}
	attempts := inv.Attempts + 1
	state := InvitationIssued
	if attempts >= maxInvitationAttempts {
		state = InvitationLocked
	}
	if err := setInvitationState(ctx, tx, inv, state, attempts, now); err != nil {
		return Invitation{}, err
	}
	return getInvitation(ctx, tx, inv.ID)
}

// markInvitationRedeemed makes the invitation final. It runs in the same
// transaction as bindInvitation.
func markInvitationRedeemed(ctx context.Context, tx *sql.Tx, inv Invitation, now time.Time) (Invitation, error) {
	if inv.State != InvitationIssued && inv.State != InvitationLocked {
		return Invitation{}, ErrInvitationInvalid
	}
	if err := setInvitationState(ctx, tx, inv, InvitationRedeemed, inv.Attempts, now); err != nil {
		return Invitation{}, err
	}
	return getInvitation(ctx, tx, inv.ID)
}

// setInvitationState changes state under the invitation's current revision,
// so a concurrent change makes it fail instead of being overwritten.
func setInvitationState(ctx context.Context, tx *sql.Tx, inv Invitation, state InvitationState, attempts int, now time.Time) error {
	res, err := tx.ExecContext(ctx,
		`UPDATE invitations SET state = ?, attempts = ?, revision = revision + 1, updated_at = ?
		 WHERE id = ? AND revision = ?`,
		string(state), attempts, formatTime(now), inv.ID, inv.Revision)
	if err != nil {
		return fmt.Errorf("update invitation: %w", err)
	}
	if n, err := res.RowsAffected(); err != nil {
		return err
	} else if n != 1 {
		return fmt.Errorf("invitation %s: %w", inv.ID, ErrRevisionConflict)
	}
	return nil
}

func getInvitation(ctx context.Context, q querier, id string) (Invitation, error) {
	var (
		inv                       Invitation
		purpose, role, state      string
		expires, created, updated string
	)
	err := q.QueryRowContext(ctx,
		`SELECT id, purpose, team_id, role, hub_id, agent_id, expected_user_id, label, issued_by,
		        state, attempts, revision, expires_at, created_at, updated_at
		 FROM invitations WHERE id = ?`, id).
		Scan(&inv.ID, &purpose, &inv.TeamID, &role, &inv.HubID, &inv.AgentID, &inv.ExpectedUserID, &inv.Label,
			&inv.IssuedBy, &state, &inv.Attempts, &inv.Revision, &expires, &created, &updated)
	if errors.Is(err, sql.ErrNoRows) {
		return Invitation{}, fmt.Errorf("invitation %s: %w", id, ErrNotFound)
	}
	if err != nil {
		return Invitation{}, fmt.Errorf("read invitation: %w", err)
	}
	inv.Purpose, inv.Role, inv.State = InvitationPurpose(purpose), Role(role), InvitationState(state)
	if inv.ExpiresAt, err = parseTime(expires); err != nil {
		return Invitation{}, err
	}
	if inv.CreatedAt, err = parseTime(created); err != nil {
		return Invitation{}, err
	}
	if inv.UpdatedAt, err = parseTime(updated); err != nil {
		return Invitation{}, err
	}
	return inv, nil
}
