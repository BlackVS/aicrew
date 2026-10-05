package server

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"time"

	"github.com/BlackVS/aicrew/internal/store"
	"github.com/BlackVS/aicrew/internal/verifier"
)

// The client session API (docs/CREW-CONTRACT.md, "Client session API"). An
// agent's client asks for a challenge, enters or resumes a team session with
// an aimem proof through an RFC 8693 token exchange, refreshes its
// aimem-scoped handle through a second exchange, reads its session and
// leaves. The routes are thin: every rule lives in the store operations
// they call, and they call no operation that trusts a caller it is given.

const (
	ChallengesPath = "/v1/crew/challenges"
	TokenPath      = "/v1/crew/token"
	SessionPath    = "/v1/crew/session"
	LeavePath      = "/v1/crew/session/leave"

	// GrantTokenExchange is RFC 8693's grant type.
	GrantTokenExchange = "urn:ietf:params:oauth:grant-type:token-exchange"
	// AccessTokenType is RFC 8693's access token type: aicrew's session
	// token.
	AccessTokenType = "urn:ietf:params:oauth:token-type:access_token"
	// ProofTokenType and HandleTokenType are aicrew's token types for an
	// aimem proof receipt and an aimem-scoped handle.
	ProofTokenType  = "https://github.com/BlackVS/aicrew/blob/main/docs/CREW-CONTRACT.md#token-type-aimem-proof-receipt"
	HandleTokenType = "https://github.com/BlackVS/aicrew/blob/main/docs/CREW-CONTRACT.md#token-type-aimem-handle"

	// Rate limits: per client address on the unauthenticated routes, per
	// session on handle refresh.
	ChallengesPerMinute = 10
	ExchangesPerMinute  = 20
	RefreshesPerMinute  = 6

	maxChallengeBody = 1 << 10
	maxTokenBody     = 4 << 10
	maxLeaveBody     = 64
)

var (
	idShape  = regexp.MustCompile(`^[A-Za-z0-9._:-]{1,128}$`)
	keyShape = regexp.MustCompile(`^[\x21-\x7e]{1,128}$`)
)

// sessionRefusals are the refusal codes of the session API. oauth is the
// RFC 6749 §5.2 error the token endpoint adds; every text is fixed and
// nothing from the request is echoed.
var sessionRefusals = map[string]struct {
	status     int
	oauth      string
	retryable  bool
	message    string
	nextAction string
}{
	"invalid_request": {http.StatusBadRequest, "invalid_request", false,
		"The request is malformed.", "Correct the request."},
	"unsupported_grant_type": {http.StatusBadRequest, "unsupported_grant_type", false,
		"Only token exchange is supported.", "Use grant_type " + GrantTokenExchange + "."},
	"invalid_target": {http.StatusBadRequest, "invalid_target", false,
		"The audience or requested token type is not served here.",
		"Name this aicrew service for entry, or the session's aimem hub for a handle."},
	"invalid_scope": {http.StatusBadRequest, "invalid_scope", false,
		"Scopes are not supported.", "Omit scope."},
	"challenge_invalid": {http.StatusBadRequest, "invalid_request", false,
		"The challenge is unknown, used or expired.", "Request a new challenge and a new aimem proof."},
	"proof_invalid": {http.StatusBadRequest, "invalid_request", false,
		"aimem did not vouch for the proof.", "Obtain a new aimem receipt for the challenge, or request a new challenge."},
	"credential_inactive": {http.StatusBadRequest, "invalid_request", false,
		"The aimem credential behind the proof is not active.",
		"Recover the individual aimem credential through its authorized flow."},
	"identity_mismatch": {http.StatusForbidden, "invalid_request", false,
		"The proof names another identity than the agent's link.",
		"Stop and reconcile the configured identity; nothing is rebound automatically."},
	"identity_link_required": {http.StatusForbidden, "invalid_request", false,
		"No linked agent has this ID.", "Complete onboarding for this agent first."},
	"role_forbidden": {http.StatusForbidden, "invalid_request", false,
		"The agent cannot enter this team or resume this session.",
		"Use the agent's own team and session, or ask the operator for a membership."},
	"session_active": {http.StatusConflict, "invalid_request", false,
		"The agent already has an active session in this team.", "Resume that session with a new proof."},
	"coordinator_active": {http.StatusConflict, "invalid_request", false,
		"The team already has an active coordinator session.", "Wait for that session to end, or ask the operator."},
	"context_stale": {http.StatusForbidden, "invalid_request", false,
		"The session has ended or moved to a newer generation.", "Enter or resume again with a new proof."},
	"invalid_token": {http.StatusUnauthorized, "invalid_request", false,
		"The session token is not valid.", "Resume the session with a new proof."},
	"work_outstanding": {http.StatusConflict, "", false,
		"The member still has open work in the team.", "Reconcile the open work through aicrew, then leave again."},
	"idempotency_conflict": {http.StatusConflict, "invalid_request", false,
		"This Idempotency-Key was used with other input.", "Use a new key for a new request."},
	"refresh_replayed": {http.StatusConflict, "invalid_request", false,
		"This refresh already happened; its handle is not returned again.",
		"Refresh again with a new Idempotency-Key."},
	"rate_limited": {http.StatusTooManyRequests, "temporarily_unavailable", true,
		"Too many requests.", "Wait for Retry-After, then retry."},
	"request_in_progress": {http.StatusServiceUnavailable, "temporarily_unavailable", true,
		"An identical request is still running; nothing was applied by this one.",
		"Retry with the same key once the earlier attempt is abandoned; keep the secrets from the latest request sent."},
	"identity_unavailable": {http.StatusServiceUnavailable, "temporarily_unavailable", true,
		"aicrew or aimem could not answer now; nothing was applied.", "Retry later with the same key."},
	"aimem_unconfigured": {http.StatusServiceUnavailable, "temporarily_unavailable", false,
		"aicrew cannot verify aimem proofs.", "The operator configures and checks aicrew's aimem peer."},
	// Invitation redemption (docs/ONBOARDING-CONTRACT.md).
	"invitation_invalid": {http.StatusForbidden, "", false,
		"The invitation is not valid.", "Ask the operator for a new invitation."},
	"identity_already_linked": {http.StatusConflict, "", false,
		"This aimem identity is already linked to another agent.",
		"Ask the operator: one aimem user links to one agent, and moving it takes a rebind invitation."},
	"role_conflict": {http.StatusConflict, "", false,
		"The agent is already a member of this team in another role.", "Ask the operator: role changes are operator operations."},
	// The attempt step routes.
	"attempt_forbidden": {http.StatusForbidden, "", false,
		"The session may not act on this attempt.", "Act only on your team's attempts, in your role."},
	"task_busy": {http.StatusConflict, "", false,
		"This service already has an open attempt on the task.", "Wait for that attempt to close."},
	"agent_busy": {http.StatusConflict, "", false,
		"The worker's execution capacity is taken by an open attempt.", "Offer the task to another worker, or wait."},
	"attempt_state": {http.StatusConflict, "", false,
		"The attempt's state does not allow this step now.", "Settle the attempt's pending step, or begin the step its state allows."},
	"offer_expired": {http.StatusConflict, "", false,
		"The offer has expired.", "The coordinator withdraws the offer and may offer the task again."},
	"offer_declined": {http.StatusConflict, "", false,
		"The offer was declined.", "The coordinator withdraws the offer."},
	"offer_stale": {http.StatusConflict, "", false,
		"The offer was made under another coordinator or worker session.", "The coordinator withdraws the offer and may offer the task again."},
	"instruction_mismatch": {http.StatusConflict, "", false,
		"The instruction digest is not the offer's.", "Fetch the offer's instructions, verify them and accept again."},
	"process_changed": {http.StatusConflict, "", false,
		"The project's process selection changed since the offer.", "The coordinator withdraws the offer and may offer the task again."},
	"step_settled": {http.StatusConflict, "", false,
		"This step has already settled; there is nothing to send.", "Begin the next step with a new Idempotency-Key."},
	"supersede_limit": {http.StatusConflict, "", false,
		"The pending update has superseded as many keys as it may.",
		"Settle the pending update, or ask the team's coordinator to reconcile the attempt."},
	"delivery_unconfirmed": {http.StatusConflict, "", false,
		"No team member has confirmed the delivery of the accepted result.",
		"The team's coordinator confirms the delivery with its evidence, then finalize again."},
	"step_unknown": {http.StatusNotFound, "", false,
		"The attempt has no step with this request key.", "Settle with the request key the step's begin returned."},
	"outcome_unknown": {http.StatusServiceUnavailable, "", true,
		"aicrew could not confirm the step through aimem now; nothing was applied.", "Settle again after Retry-After."},
	"project_not_granted": {http.StatusConflict, "", false,
		"The team's hub does not grant the team the task's project; nothing was sent.",
		"Ask the hub's operator for the grant (aimem identity team grant), or choose a task of a granted project."},
	"capability_missing": {http.StatusConflict, "", false,
		"The worker has not verified the offer's repository at the access it needs; nothing was sent.",
		"Offer the task to a worker that has (GET /v1/crew/capabilities), or ask the worker to provision the forge credential (aicrew-agent join --cred) and run aicrew-agent check."},
	"repository_mismatch": {http.StatusConflict, "", false,
		"The step's repository kind, URL or access is not the one the hub binds to the task's project; nothing was sent.",
		"Read the project's repository from the hub (aimem project show) and name it in the step."},
	"hub_unavailable": {http.StatusServiceUnavailable, "", true,
		"aicrew could not read the team's grants from its hub now; nothing was sent.", "Try again after Retry-After."},
	"message_not_delivered": {http.StatusConflict, "", false,
		"A message named was never delivered to this member, so it cannot be acknowledged.",
		"Read the inbox, then acknowledge only the messages it delivered."},
	// The shared refusals, made before a session handler runs.
	"method_not_allowed": {http.StatusMethodNotAllowed, "invalid_request", false,
		"This method is not served on this path.", "Use a method the Allow header names."},
	"request_too_large": {http.StatusRequestEntityTooLarge, "invalid_request", false,
		"The declared request body is too large.", "Send the request without an oversized body."},
}

// envelope is the context contract's refusal envelope; on the token endpoint
// it also carries RFC 6749's error members.
type envelope struct {
	Error            string `json:"error,omitempty"`
	ErrorDescription string `json:"error_description,omitempty"`
	Code             string `json:"code"`
	Message          string `json:"message"`
	Retryable        bool   `json:"retryable"`
	NextAction       string `json:"next_action"`
	CorrelationID    string `json:"correlation_id"`
}

// refuseSession answers with the envelope. On the token endpoint (oauth) it
// adds the RFC 6749 members; there a refused subject token, or a request
// the policy will not honour, is 400 invalid_request (RFC 8693 §2.2.2), and
// only a conflict, a rate limit or unavailability keeps its own status.
// The route and code are logged; nothing the client sent is.
func (s *Server) refuseSession(w http.ResponseWriter, r *http.Request, code string, oauth bool, retryAfter time.Duration) {
	s.refuseSessionSaying(w, r, code, oauth, retryAfter, "")
}

// refuseSessionSaying is refuseSession with message, when not empty, in
// place of the code's own: a refusal that names what it refers to, never
// anything the client sent as a secret.
func (s *Server) refuseSessionSaying(w http.ResponseWriter, r *http.Request, code string, oauth bool, retryAfter time.Duration,
	message string) {
	ref := sessionRefusals[code]
	status := ref.status
	body := envelope{Code: code, Message: ref.message, Retryable: ref.retryable, NextAction: ref.nextAction}
	if message != "" {
		body.Message = message
	}
	if oauth && ref.oauth != "" {
		body.Error, body.ErrorDescription = ref.oauth, ref.message
		if status == http.StatusUnauthorized || status == http.StatusForbidden {
			status = http.StatusBadRequest
		}
	}
	if !oauth && code == "invalid_token" {
		w.Header().Set("WWW-Authenticate", `Bearer error="invalid_token"`)
	}
	if retryAfter > 0 {
		w.Header().Set("Retry-After", strconv.Itoa(int((retryAfter+time.Second-1)/time.Second)))
	}
	var id [8]byte
	_, _ = rand.Read(id[:])
	body.CorrelationID = "corr-" + hex.EncodeToString(id[:])
	s.log.Info("refused", "route", s.routeOf(r), "code", code, "correlation_id", body.CorrelationID)
	w.Header().Set("Connection", "close")
	writeJSON(w, status, body)
}

// refusalCode maps a store or verifier error to its refusal code.
func refusalCode(err error) string {
	var verr *verifier.Error
	switch {
	case errors.As(err, &verr):
		switch {
		case verr.Retryable:
			return "identity_unavailable"
		case verr.Code == "proof_invalid" || verr.Code == "invalid_request":
			return "proof_invalid"
		case verr.Code == "credential_inactive":
			return "credential_inactive"
		}
		return "aimem_unconfigured"
	case errors.Is(err, store.ErrInProgress):
		return "request_in_progress"
	case errors.Is(err, store.ErrIdempotencyConflict):
		return "idempotency_conflict"
	case errors.Is(err, store.ErrChallengeInvalid):
		return "challenge_invalid"
	case errors.Is(err, store.ErrIdentityMismatch):
		return "identity_mismatch"
	case errors.Is(err, store.ErrIdentityLinkRequired):
		return "identity_link_required"
	case errors.Is(err, store.ErrForbidden), errors.Is(err, store.ErrNotFound):
		return "role_forbidden"
	case errors.Is(err, store.ErrSessionActive):
		return "session_active"
	case errors.Is(err, store.ErrCoordinatorActive):
		return "coordinator_active"
	case errors.Is(err, store.ErrContextStale):
		return "context_stale"
	case errors.Is(err, store.ErrTokenInvalid):
		return "invalid_token"
	case errors.Is(err, store.ErrWorkOutstanding):
		return "work_outstanding"
	case errors.Is(err, store.ErrInvalid):
		return "invalid_request"
	}
	return "identity_unavailable"
}

// idempotencyKey returns the single Idempotency-Key header.
func idempotencyKey(r *http.Request) (string, bool) {
	v := r.Header.Values("Idempotency-Key")
	if len(v) != 1 || !keyShape.MatchString(v[0]) {
		return "", false
	}
	return v[0], true
}

func mediaType(r *http.Request, want string) bool {
	mt, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	return err == nil && mt == want
}

// readBody reads at most max bytes of the body; a longer one fails.
func readBody(r *http.Request, max int64) ([]byte, bool) {
	if r.ContentLength > max {
		return nil, false
	}
	b, err := io.ReadAll(io.LimitReader(r.Body, max+1))
	return b, err == nil && int64(len(b)) <= max
}

func expiresIn(t time.Time) int64 {
	if d := time.Until(t); d > 0 {
		return int64(d / time.Second)
	}
	return 0
}

func rfc3339(t time.Time) string { return t.UTC().Truncate(time.Second).Format(time.RFC3339) }

type challengeReply struct {
	ChallengeID string `json:"challenge_id"`
	HubID       string `json:"hub_id"`
	ServiceID   string `json:"service_id"`
	ExpiresAt   string `json:"expires_at"`
}

// challenge issues an identity-proof challenge for a linked agent. It is
// unauthenticated, because a challenge carries no authority, and
// rate-limited. An unknown agent and an unlinked one get the same refusal;
// a challenge issued does confirm that the agent ID is linked.
func (s *Server) challenge(w http.ResponseWriter, r *http.Request) {
	if ok, wait := s.challengeLimit.allow(clientAddr(r)); !ok {
		s.refuseSession(w, r, "rate_limited", false, wait)
		return
	}
	key, ok := idempotencyKey(r)
	if !ok || !mediaType(r, "application/json") {
		s.refuseSession(w, r, "invalid_request", false, 0)
		return
	}
	body, ok := readBody(r, maxChallengeBody)
	var req struct {
		AgentID string `json:"agent_id"`
	}
	if !ok || decodeStrict(body, &req) != nil || !idShape.MatchString(req.AgentID) {
		s.refuseSession(w, r, "invalid_request", false, 0)
		return
	}
	ch, err := s.store.IssueAgentChallenge(r.Context(), store.Caller{}, key, req.AgentID)
	if err != nil {
		code := refusalCode(err)
		if errors.Is(err, store.ErrNotFound) {
			code = "identity_link_required"
		}
		s.refuseSession(w, r, code, false, 0)
		return
	}
	writeJSON(w, http.StatusOK, challengeReply{ChallengeID: ch.ID, HubID: ch.HubID, ServiceID: s.cfg.ServiceID,
		ExpiresAt: rfc3339(ch.ExpiresAt)})
}

func decodeStrict(b []byte, v any) error {
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return err
	}
	if dec.Decode(&struct{}{}) != io.EOF {
		return errors.New("one JSON object expected")
	}
	return nil
}

type sessionView struct {
	ID             string `json:"id"`
	TeamID         string `json:"team_id"`
	AgentID        string `json:"agent_id"`
	Role           string `json:"role"`
	State          string `json:"state"`
	Generation     string `json:"generation"`
	TokenExpiresAt string `json:"token_expires_at,omitempty"`
}

type entryReply struct {
	AccessToken          string      `json:"access_token"`
	IssuedTokenType      string      `json:"issued_token_type"`
	TokenType            string      `json:"token_type"`
	ExpiresIn            int64       `json:"expires_in"`
	Session              sessionView `json:"session"`
	AimemHandle          string      `json:"aimem_handle"`
	AimemHandleExpiresIn int64       `json:"aimem_handle_expires_in"`
}

type handleReply struct {
	AccessToken     string `json:"access_token"`
	IssuedTokenType string `json:"issued_token_type"`
	TokenType       string `json:"token_type"`
	ExpiresIn       int64  `json:"expires_in"`
}

// token is the RFC 8693 token-exchange endpoint. Its form must name every
// parameter at most once; parameters it does not know are ignored, as RFC
// 6749 asks, except the RFC 8693 ones it does not serve.
func (s *Server) token(w http.ResponseWriter, r *http.Request) {
	if ok, wait := s.tokenLimit.allow(clientAddr(r)); !ok {
		s.refuseSession(w, r, "rate_limited", true, wait)
		return
	}
	key, ok := idempotencyKey(r)
	if !ok || r.URL.RawQuery != "" || !mediaType(r, "application/x-www-form-urlencoded") {
		s.refuseSession(w, r, "invalid_request", true, 0)
		return
	}
	body, ok := readBody(r, maxTokenBody)
	if !ok {
		s.refuseSession(w, r, "invalid_request", true, 0)
		return
	}
	form, err := url.ParseQuery(string(body))
	if err != nil {
		s.refuseSession(w, r, "invalid_request", true, 0)
		return
	}
	for _, v := range form {
		if len(v) != 1 {
			s.refuseSession(w, r, "invalid_request", true, 0)
			return
		}
	}
	switch {
	case form.Get("grant_type") != GrantTokenExchange:
		s.refuseSession(w, r, "unsupported_grant_type", true, 0)
	case form.Has("scope"):
		s.refuseSession(w, r, "invalid_scope", true, 0)
	case form.Has("resource"):
		s.refuseSession(w, r, "invalid_target", true, 0)
	case form.Has("actor_token") || form.Has("actor_token_type") || form.Get("subject_token") == "":
		s.refuseSession(w, r, "invalid_request", true, 0)
	case form.Get("subject_token_type") == ProofTokenType:
		s.enter(w, r, key, form)
	case form.Get("subject_token_type") == AccessTokenType:
		s.refresh(w, r, key, form)
	default:
		s.refuseSession(w, r, "invalid_request", true, 0)
	}
}

// enter exchanges an aimem proof for a session token and the first handle:
// entry into team_id, or resume of session_id.
func (s *Server) enter(w http.ResponseWriter, r *http.Request, key string, form url.Values) {
	if rt := form.Get("requested_token_type"); (rt != "" && rt != AccessTokenType) || form.Get("audience") != s.cfg.ServiceID {
		s.refuseSession(w, r, "invalid_target", true, 0)
		return
	}
	challengeID, teamID, sessionID := form.Get("challenge_id"), form.Get("team_id"), form.Get("session_id")
	if !idShape.MatchString(challengeID) || (teamID == "") == (sessionID == "") {
		s.refuseSession(w, r, "invalid_request", true, 0)
		return
	}
	if s.verifier == nil {
		s.refuseSession(w, r, "aimem_unconfigured", true, 0)
		return
	}
	receipt := store.NewSecret(form.Get("subject_token"))
	var (
		entry   store.SessionEntry
		secrets store.EntrySecrets
		err     error
	)
	if teamID != "" {
		entry, secrets, err = s.store.EnterSession(r.Context(), s.verifier, key, challengeID, receipt, teamID, s.cfg.ServiceID)
	} else {
		entry, secrets, err = s.store.ResumeSessionWithProof(r.Context(), s.verifier, key, challengeID, receipt, sessionID, s.cfg.ServiceID)
	}
	if err != nil {
		s.refuseSession(w, r, refusalCode(err), true, 0)
		return
	}
	writeJSON(w, http.StatusOK, entryReply{
		AccessToken: secrets.Token.Reveal(), IssuedTokenType: AccessTokenType, TokenType: "Bearer",
		ExpiresIn: expiresIn(entry.Token.ExpiresAt),
		Session: sessionView{ID: entry.Session.ID, TeamID: entry.Session.TeamID, AgentID: entry.Session.AgentID,
			Role: string(entry.Session.Role), State: string(entry.Session.State),
			Generation: strconv.FormatInt(entry.Session.Generation, 10), TokenExpiresAt: rfc3339(entry.Token.ExpiresAt)},
		AimemHandle: secrets.Handle.Reveal(), AimemHandleExpiresIn: expiresIn(entry.Handle.ExpiresAt),
	})
}

// refresh exchanges the session token for a new aimem-scoped handle whose
// audience is the session's aimem hub.
func (s *Server) refresh(w http.ResponseWriter, r *http.Request, key string, form url.Values) {
	if form.Get("requested_token_type") != HandleTokenType {
		s.refuseSession(w, r, "invalid_target", true, 0)
		return
	}
	token := form.Get("subject_token")
	b, err := s.store.AuthenticateSessionToken(r.Context(), token)
	if err != nil {
		s.refuseSession(w, r, refusalCode(err), true, 0)
		return
	}
	if form.Get("audience") != b.HubID {
		s.refuseSession(w, r, "invalid_target", true, 0)
		return
	}
	if ok, wait := s.refreshLimit.allow(b.SessionID); !ok {
		s.refuseSession(w, r, "rate_limited", true, wait)
		return
	}
	h, handle, err := s.store.RefreshHandle(r.Context(), key, token, s.cfg.ServiceID)
	if err != nil {
		s.refuseSession(w, r, refusalCode(err), true, 0)
		return
	}
	if handle == "" {
		s.refuseSession(w, r, "refresh_replayed", true, 0)
		return
	}
	writeJSON(w, http.StatusOK, handleReply{AccessToken: handle, IssuedTokenType: HandleTokenType,
		TokenType: "N_A", ExpiresIn: expiresIn(h.ExpiresAt)})
}

// sessionToken is the Authorization header's session token.
func sessionToken(r *http.Request) string { return bearer(r) }

// sessionStatus reports what the session token stands for. It reads one
// snapshot and changes nothing.
func (s *Server) sessionStatus(w http.ResponseWriter, r *http.Request) {
	b, err := s.store.AuthenticateSessionToken(r.Context(), sessionToken(r))
	if err != nil {
		s.refuseSession(w, r, refusalCode(err), false, 0)
		return
	}
	writeJSON(w, http.StatusOK, statusReply{
		Session: sessionView{ID: b.SessionID, TeamID: b.TeamID, AgentID: b.AgentID, Role: string(b.Role),
			State: string(store.SessionActive), Generation: strconv.FormatInt(b.Generation, 10),
			TokenExpiresAt: rfc3339(b.ExpiresAt)},
		HubID: b.HubID, UserID: b.UserID, TokenID: b.TokenID,
	})
}

type statusReply struct {
	Session sessionView `json:"session"`
	HubID   string      `json:"hub_id"`
	UserID  string      `json:"user_id"`
	TokenID string      `json:"token_id"`
}

// leave ends the token's session under the store's leave rules. The body is
// empty or an empty JSON object.
func (s *Server) leave(w http.ResponseWriter, r *http.Request) {
	token := sessionToken(r)
	if token == "" {
		s.refuseSession(w, r, "invalid_token", false, 0)
		return
	}
	key, ok := idempotencyKey(r)
	body, bodyOK := readBody(r, maxLeaveBody)
	if !ok || !bodyOK || !emptyObject(body) {
		s.refuseSession(w, r, "invalid_request", false, 0)
		return
	}
	sess, err := s.store.LeaveWithToken(r.Context(), key, token)
	if err != nil {
		s.refuseSession(w, r, refusalCode(err), false, 0)
		return
	}
	writeJSON(w, http.StatusOK, sessionView{ID: sess.ID, TeamID: sess.TeamID, AgentID: sess.AgentID,
		Role: string(sess.Role), State: string(sess.State), Generation: strconv.FormatInt(sess.Generation, 10)})
}

func emptyObject(b []byte) bool {
	b = bytes.TrimSpace(b)
	if len(b) == 0 {
		return true
	}
	var m map[string]json.RawMessage
	return decodeStrict(b, &m) == nil && m != nil && len(m) == 0
}
