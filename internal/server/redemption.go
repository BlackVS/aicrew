package server

import (
	"errors"
	"net/http"
	"sync"
	"time"

	"github.com/BlackVS/aicrew/internal/store"
)

// Invitation redemption over HTTPS (docs/ONBOARDING-CONTRACT.md,
// "Redemption"; docs/CREW-CONTRACT.md, "Invitation redemption"). The holder of
// an invitation code begins redemption for a challenge, proves its aimem
// identity against it with its own aimem credential, and completes with the
// receipt. Both routes are unauthenticated: the code is the authority, and
// the store checks it inside every command. The code and the receipt travel
// only in the JSON body over TLS; neither is logged, audited, echoed in a
// refusal or kept, and a refusal never says which of the invitation's
// conditions failed.

const (
	InvitationBeginPath    = "/v1/crew/invitations/begin"
	InvitationCompletePath = "/v1/crew/invitations/complete"

	// Per client address: a begin counts an attempt against the invitation,
	// so it gets the challenge budget; a completion the exchange budget.
	BeginsPerMinute      = ChallengesPerMinute
	CompletionsPerMinute = ExchangesPerMinute

	maxBeginBody    = 1 << 10
	maxCompleteBody = 4 << 10
)

// redemptionState is the Server's redemption state: per-address limits and
// the refusal counts.
type redemptionState struct {
	begin, complete *limiter
	refusals        refusalCounter
}

// registerRedemption registers the two routes and their limits.
func (s *Server) registerRedemption() {
	s.redemption.begin = newLimiter(BeginsPerMinute, time.Minute)
	s.redemption.complete = newLimiter(CompletionsPerMinute, time.Minute)
	s.handleOwnBody(http.MethodPost, InvitationBeginPath, s.beginInvitation)
	s.handleOwnBody(http.MethodPost, InvitationCompletePath, s.completeInvitation)
}

// refusalCounter counts the redemption routes' refusals by code, in memory,
// for the operator's log. It holds nothing from a request: no code, key,
// digest or address.
type refusalCounter struct {
	mu     sync.Mutex
	counts map[string]int64
}

func (c *refusalCounter) add(code string) int64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.counts == nil {
		c.counts = map[string]int64{}
	}
	c.counts[code]++
	return c.counts[code]
}

// refuseRedemption counts the refusal, logs the running total for its code
// and answers with the envelope.
func (s *Server) refuseRedemption(w http.ResponseWriter, r *http.Request, code string, retryAfter time.Duration) {
	n := s.redemption.refusals.add(code)
	s.log.Info("invitation redemption refused", "route", s.routeOf(r), "code", code, "count", n)
	s.refuseSession(w, r, code, false, retryAfter)
}

// redemptionCode maps a redemption error to its refusal code.
func redemptionCode(err error) string {
	switch {
	case errors.Is(err, store.ErrInvitationInvalid):
		return "invitation_invalid"
	case errors.Is(err, store.ErrIdentityAlreadyLinked):
		return "identity_already_linked"
	case errors.Is(err, store.ErrRoleConflict):
		return "role_conflict"
	}
	return refusalCode(err)
}

// beginInvitation starts redeeming an invitation: {"code": ...} with an
// Idempotency-Key, answered with a challenge for the holder's aimem proof.
func (s *Server) beginInvitation(w http.ResponseWriter, r *http.Request) {
	if ok, wait := s.redemption.begin.allow(clientAddr(r)); !ok {
		s.refuseRedemption(w, r, "rate_limited", wait)
		return
	}
	key, ok := idempotencyKey(r)
	if !ok || r.URL.RawQuery != "" || !mediaType(r, "application/json") {
		s.refuseRedemption(w, r, "invalid_request", 0)
		return
	}
	body, ok := readBody(r, maxBeginBody)
	var req struct {
		Code string `json:"code"`
	}
	if !ok || decodeStrict(body, &req) != nil || req.Code == "" {
		s.refuseRedemption(w, r, "invalid_request", 0)
		return
	}
	ch, err := s.store.BeginRedemption(r.Context(), key, store.NewSecret(req.Code))
	if err != nil {
		s.refuseRedemption(w, r, redemptionCode(err), 0)
		return
	}
	writeJSON(w, http.StatusOK, challengeReply{ChallengeID: ch.ID, HubID: ch.HubID, ServiceID: s.cfg.ServiceID,
		ExpiresAt: rfc3339(ch.ExpiresAt)})
}

// completionReply is what a completed redemption returns: the agent and its
// new membership, never a secret. The session starts next, by proof.
type completionReply struct {
	AgentID string `json:"agent_id"`
	TeamID  string `json:"team_id"`
	Role    string `json:"role"`
	HubID   string `json:"hub_id"`
	UserID  string `json:"user_id"`
	Created bool   `json:"created"`
	Rebound bool   `json:"rebound"`
	Rotated bool   `json:"rotated"`
}

// completeInvitation finishes redeeming an invitation:
// {"code": ..., "challenge_id": ..., "receipt": ...} with an Idempotency-Key.
func (s *Server) completeInvitation(w http.ResponseWriter, r *http.Request) {
	if ok, wait := s.redemption.complete.allow(clientAddr(r)); !ok {
		s.refuseRedemption(w, r, "rate_limited", wait)
		return
	}
	key, ok := idempotencyKey(r)
	if !ok || r.URL.RawQuery != "" || !mediaType(r, "application/json") {
		s.refuseRedemption(w, r, "invalid_request", 0)
		return
	}
	body, ok := readBody(r, maxCompleteBody)
	var req struct {
		Code        string `json:"code"`
		ChallengeID string `json:"challenge_id"`
		Receipt     string `json:"receipt"`
	}
	if !ok || decodeStrict(body, &req) != nil || req.Code == "" || req.Receipt == "" || !idShape.MatchString(req.ChallengeID) {
		s.refuseRedemption(w, r, "invalid_request", 0)
		return
	}
	if s.verifier == nil {
		s.refuseRedemption(w, r, "aimem_unconfigured", 0)
		return
	}
	res, err := s.store.CompleteRedemption(r.Context(), s.verifier, key, store.NewSecret(req.Code), req.ChallengeID,
		store.NewSecret(req.Receipt))
	if err != nil {
		s.refuseRedemption(w, r, redemptionCode(err), 0)
		return
	}
	writeJSON(w, http.StatusOK, completionReply{AgentID: res.AgentID, TeamID: res.Membership.TeamID,
		Role: string(res.Membership.Role), HubID: res.Identity.HubID, UserID: res.Identity.UserID,
		Created: res.Created, Rebound: res.Rebound, Rotated: res.Rotated})
}
