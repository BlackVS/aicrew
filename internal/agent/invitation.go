package agent

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"time"
)

// Invitation redemption, the client's side (docs/ONBOARDING-CONTRACT.md,
// "Redemption"). The code and the receipt travel only in the JSON body over
// the pinned TLS connection.

const (
	invitationBeginPath    = "/v1/crew/invitations/begin"
	invitationCompletePath = "/v1/crew/invitations/complete"
)

// LinkResult is a completed redemption: the agent and its new membership.
// It carries no secret; the session starts afterwards, by proof.
type LinkResult struct {
	AgentID string `json:"agent_id"`
	TeamID  string `json:"team_id"`
	Role    string `json:"role"`
	HubID   string `json:"hub_id"`
	UserID  string `json:"user_id"`
	Created bool   `json:"created"`
	Rebound bool   `json:"rebound"`
	Rotated bool   `json:"rotated"`
}

// InvitationAPI is the redemption API as the bootstrap uses it.
type InvitationAPI interface {
	BeginInvitation(ctx context.Context, key, code string) (Challenge, error)
	CompleteInvitation(ctx context.Context, key, code, challengeID, receipt string) (LinkResult, error)
}

func (c *Crew) BeginInvitation(ctx context.Context, key, code string) (Challenge, error) {
	body, _ := json.Marshal(map[string]string{"code": code})
	var out struct {
		ChallengeID string `json:"challenge_id"`
		HubID       string `json:"hub_id"`
		ServiceID   string `json:"service_id"`
		ExpiresAt   string `json:"expires_at"`
	}
	if err := c.do(ctx, http.MethodPost, invitationBeginPath, "application/json", key, "", body, &out); err != nil {
		return Challenge{}, err
	}
	exp, err := time.Parse(time.RFC3339, out.ExpiresAt)
	if err != nil || !idShape.MatchString(out.ChallengeID) || out.HubID == "" || out.ServiceID == "" {
		return Challenge{}, &TransportError{Err: errors.New("the challenge reply is incomplete")}
	}
	return Challenge{ID: out.ChallengeID, HubID: out.HubID, ServiceID: out.ServiceID, ExpiresAt: exp}, nil
}

func (c *Crew) CompleteInvitation(ctx context.Context, key, code, challengeID, receipt string) (LinkResult, error) {
	body, _ := json.Marshal(map[string]string{"code": code, "challenge_id": challengeID, "receipt": receipt})
	var out LinkResult
	if err := c.do(ctx, http.MethodPost, invitationCompletePath, "application/json", key, "", body, &out); err != nil {
		return LinkResult{}, err
	}
	if !idShape.MatchString(out.AgentID) || !idShape.MatchString(out.TeamID) || out.Role == "" {
		return LinkResult{}, &TransportError{Err: errors.New("the completion reply is incomplete")}
	}
	return out, nil
}
