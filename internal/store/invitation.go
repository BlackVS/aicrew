package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"reflect"
	"time"
)

// Invitation-bound linking (docs/ONBOARDING-CONTRACT.md).
//
// The invitation store and the redemption command belong to crew-onboarding
// and do not exist yet. This file fixes the boundary they will use:
//
//   - An InvitationScope is the already-validated scope of one invitation.
//     Its fields are unexported and only newInvitationScope builds one, so no
//     code outside this package can fabricate a scope. The invitation store
//     builds it after checking the invitation code, state and expiry.
//   - issueInvitationChallenge and bindInvitation run inside the caller's
//     command transaction. Redemption calls them in the same transaction
//     that marks the invitation redeemed, so the link, any new agent, the
//     membership, the invitation state, the audit record and the receipt
//     commit together or not at all.
//   - Verification (verifyChallenge) happens before that transaction and
//     writes nothing, so failed or unavailable verification changes nothing.

type InvitationPurpose string

const (
	PurposeJoin   InvitationPurpose = "join"   // a new or already linked agent joins a team
	PurposeLink   InvitationPurpose = "link"   // an existing unlinked agent record is linked
	PurposeRebind InvitationPurpose = "rebind" // an existing agent moves to a different user
)

// InvitationScope is the validated scope of one invitation.
type InvitationScope struct {
	invitationID   string
	purpose        InvitationPurpose
	teamID         string
	role           Role
	hubID          string
	agentID        string // required for link and rebind
	expectedUserID string // required for rebind; optional otherwise
	label          string // label for a new agent created by join
}

func newInvitationScope(invitationID string, purpose InvitationPurpose, teamID string, role Role,
	hubID, agentID, expectedUserID, label string) (InvitationScope, error) {
	sc := InvitationScope{
		invitationID: invitationID, purpose: purpose, teamID: teamID, role: role,
		hubID: hubID, agentID: agentID, expectedUserID: expectedUserID, label: label,
	}
	switch {
	case invitationID == "" || teamID == "":
		return InvitationScope{}, fmt.Errorf("%w: invitation scope needs an invitation and a team", ErrInvalid)
	case !role.valid():
		return InvitationScope{}, fmt.Errorf("%w: role %q", ErrInvalid, role)
	case !refPattern.MatchString(hubID):
		return InvitationScope{}, fmt.Errorf("%w: hub %q", ErrInvalid, hubID)
	}
	switch purpose {
	case PurposeJoin:
		if agentID != "" {
			return InvitationScope{}, fmt.Errorf("%w: join invitations do not name an agent", ErrInvalid)
		}
		if err := validateLabel("agent label", label); err != nil {
			return InvitationScope{}, err
		}
	case PurposeLink:
		if agentID == "" {
			return InvitationScope{}, fmt.Errorf("%w: link invitations name the agent to link", ErrInvalid)
		}
	case PurposeRebind:
		if agentID == "" || expectedUserID == "" {
			return InvitationScope{}, fmt.Errorf("%w: rebind invitations name the agent and pin the new user", ErrInvalid)
		}
	default:
		return InvitationScope{}, fmt.Errorf("%w: purpose %q", ErrInvalid, purpose)
	}
	for _, v := range []string{invitationID, teamID, agentID, expectedUserID} {
		if err := validateText(reflect.ValueOf(v)); err != nil {
			return InvitationScope{}, err
		}
	}
	return sc, nil
}

// LinkResult reports what an invitation-bound proof changed.
type LinkResult struct {
	ChallengeID string           `json:"challenge_id"`
	AgentID     string           `json:"agent_id"`
	Identity    VerifiedIdentity `json:"identity"`
	Membership  Membership       `json:"membership"`
	Created     bool             `json:"created"` // a new agent record was created
	Rebound     bool             `json:"rebound"` // the agent moved to a different user
	Rotated     bool             `json:"rotated"` // same user, new credential
}

// issueInvitationChallenge issues a challenge for an invitation inside the
// caller's transaction. It supersedes any earlier pending challenge for the
// same invitation.
func issueInvitationChallenge(ctx context.Context, tx *sql.Tx, sc InvitationScope, now time.Time) (Challenge, error) {
	if _, err := tx.ExecContext(ctx,
		`UPDATE challenges SET state = 'superseded', updated_at = ?
		 WHERE invitation_id = ? AND state = 'pending'`,
		formatTime(now), sc.invitationID); err != nil {
		return Challenge{}, fmt.Errorf("supersede challenges: %w", err)
	}
	return insertChallenge(ctx, tx, challengeInvitation, "", sc.invitationID, sc.hubID, now)
}

// bindInvitation applies an invitation's binding rules for an identity the
// verifier has already vouched for, inside the caller's transaction. It
// either links, joins and consumes the challenge, or changes nothing and
// returns the refusal.
func (s *Store) bindInvitation(ctx context.Context, tx *sql.Tx, sc InvitationScope, challengeID string,
	id VerifiedIdentity, now time.Time) (LinkResult, error) {
	ch, err := currentChallenge(ctx, tx, challengeID, challengeInvitation, now)
	if err != nil {
		return LinkResult{}, err
	}
	if ch.InvitationID != sc.invitationID || ch.HubID != sc.hubID {
		return LinkResult{}, fmt.Errorf("challenge %s belongs to another invitation: %w", challengeID, ErrChallengeInvalid)
	}
	if id.HubID != sc.hubID {
		return LinkResult{}, fmt.Errorf("verified hub %s, invitation hub %s: %w", id.HubID, sc.hubID, ErrIdentityMismatch)
	}
	if sc.expectedUserID != "" && id.UserID != sc.expectedUserID {
		return LinkResult{}, fmt.Errorf("verified user is not the pinned user: %w", ErrIdentityMismatch)
	}
	holder, err := agentByIdentity(ctx, tx, id.HubID, id.UserID)
	holderFound := err == nil
	if err != nil && !errors.Is(err, ErrNotFound) {
		return LinkResult{}, err
	}

	res := LinkResult{ChallengeID: challengeID, Identity: id}
	switch sc.purpose {
	case PurposeJoin:
		if holderFound {
			// The user already has an agent: it joins, no duplicate is made.
			res.AgentID = holder.ID
			if res.Rotated, err = rotateIfNeeded(ctx, tx, holder, id, now); err != nil {
				return LinkResult{}, err
			}
			break
		}
		agentID, err := newID(now)
		if err != nil {
			return LinkResult{}, err
		}
		at := formatTime(now)
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO agents (id, label, model, client, client_version, revision, created_at, updated_at)
			 VALUES (?, ?, '', '', '', 1, ?, ?)`,
			agentID, sc.label, at, at); err != nil {
			return LinkResult{}, fmt.Errorf("insert agent: %w", err)
		}
		if err := setLink(ctx, tx, agentID, id, now); err != nil {
			return LinkResult{}, err
		}
		res.AgentID, res.Created = agentID, true

	case PurposeLink:
		bound, err := getAgent(ctx, tx, sc.agentID)
		if err != nil {
			return LinkResult{}, err
		}
		switch {
		case bound.Linked != nil && (bound.Linked.HubID != id.HubID || bound.Linked.UserID != id.UserID):
			return LinkResult{}, fmt.Errorf("agent %s is linked to another user: %w", bound.ID, ErrIdentityMismatch)
		case bound.Linked != nil:
			if res.Rotated, err = rotateIfNeeded(ctx, tx, bound, id, now); err != nil {
				return LinkResult{}, err
			}
		case holderFound:
			return LinkResult{}, fmt.Errorf("user is linked to agent %s: %w", holder.ID, ErrIdentityAlreadyLinked)
		default:
			if err := setLink(ctx, tx, bound.ID, id, now); err != nil {
				return LinkResult{}, err
			}
		}
		res.AgentID = bound.ID

	case PurposeRebind:
		bound, err := getAgent(ctx, tx, sc.agentID)
		if err != nil {
			return LinkResult{}, err
		}
		if holderFound && holder.ID != bound.ID {
			return LinkResult{}, fmt.Errorf("user is linked to agent %s: %w", holder.ID, ErrIdentityAlreadyLinked)
		}
		if holderFound {
			// Already linked to this user: nothing to rebind.
			if res.Rotated, err = rotateIfNeeded(ctx, tx, bound, id, now); err != nil {
				return LinkResult{}, err
			}
		} else {
			busy, err := s.outstandingWork(ctx, tx, bound.ID)
			if err != nil {
				return LinkResult{}, err
			}
			if busy {
				return LinkResult{}, fmt.Errorf("agent %s: %w", bound.ID, ErrWorkOutstanding)
			}
			if err := endAgentSessions(ctx, tx, bound.ID, now); err != nil {
				return LinkResult{}, err
			}
			if err := setLink(ctx, tx, bound.ID, id, now); err != nil {
				return LinkResult{}, err
			}
			res.Rebound = true
		}
		res.AgentID = bound.ID
	}

	if res.Membership, err = ensureMembership(ctx, tx, sc.teamID, res.AgentID, sc.role, now); err != nil {
		return LinkResult{}, err
	}
	if err := consumeChallenge(ctx, tx, challengeID, now); err != nil {
		return LinkResult{}, err
	}
	return res, nil
}

// ensureMembership makes the agent an active member with the role, accepting
// an existing membership only if it already has that role.
func ensureMembership(ctx context.Context, tx *sql.Tx, teamID, agentID string, role Role, now time.Time) (Membership, error) {
	m, err := getMembership(ctx, tx, teamID, agentID)
	if err == nil {
		if m.Role != role {
			return Membership{}, fmt.Errorf("agent %s is a %s in team %s: %w", agentID, m.Role, teamID, ErrRoleConflict)
		}
		return m, nil
	}
	if !errors.Is(err, ErrNotFound) {
		return Membership{}, err
	}
	return addMembership(ctx, tx, teamID, agentID, role, now)
}

// endAgentSessions ends every active session of the agent, advancing each
// generation (and the coordinator generation for a coordinator session).
func endAgentSessions(ctx context.Context, tx *sql.Tx, agentID string, now time.Time) error {
	rows, err := tx.QueryContext(ctx,
		`SELECT `+sessionColumns+` FROM sessions WHERE agent_id = ? AND state = 'active'`, agentID)
	if err != nil {
		return fmt.Errorf("list agent sessions: %w", err)
	}
	var active []Session
	for rows.Next() {
		sess, err := scanSession(rows)
		if err != nil {
			rows.Close()
			return err
		}
		active = append(active, sess)
	}
	if err := rows.Close(); err != nil {
		return err
	}
	for _, sess := range active {
		if err := endSession(ctx, tx, sess, SessionEnded, now); err != nil {
			return err
		}
	}
	return nil
}
