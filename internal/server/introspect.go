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
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/BlackVS/aicrew/internal/store"
)

// Session introspection, identity.v1 §3 (docs/CREW-CONTRACT.md, "Session
// introspection"). Aimem calls this route with its introspection credential
// to learn whether an aimem-scoped handle names a current aicrew session.
// Every value in an active answer comes from aicrew's own state; the request
// supplies only the handle, the hub it asks for and a nonce to echo.

const (
	// IntrospectPath is the route aimem registers as the full https URL.
	IntrospectPath = "/v1/crew/introspect"
	// VersionHeader carries identity.v1's version; the body carries it too.
	VersionHeader = "X-Aimem-Identity-Version"

	maxIntrospectBody = 4 << 10
)

var nonceShape = regexp.MustCompile(`^n-[0-9a-f]{32}$`)

type introspectRequest struct {
	Version *int   `json:"version"`
	HubID   string `json:"hub_id"`
	Nonce   string `json:"nonce"`
	Handle  string `json:"handle"`
}

type introspectIdentity struct {
	UserID  string `json:"user_id"`
	TokenID string `json:"token_id"`
}

type activeReply struct {
	Nonce           string             `json:"nonce"`
	Active          bool               `json:"active"`
	ServiceID       string             `json:"service_id"`
	HubID           string             `json:"hub_id"`
	Identity        introspectIdentity `json:"identity"`
	AgentID         string             `json:"agent_id"`
	TeamID          string             `json:"team_id"`
	Role            string             `json:"role"`
	SessionID       string             `json:"session_id"`
	Generation      string             `json:"generation"`
	HandleExpiresAt string             `json:"handle_expires_at"`
}

// inactiveReply is the whole answer for every other state: it carries no
// reason.
type inactiveReply struct {
	Nonce  string `json:"nonce"`
	Active bool   `json:"active"`
}

// refusal is the context contract's envelope. Its message and next action
// are fixed texts; nothing from the request is echoed.
type refusal struct {
	Code          string `json:"code"`
	Message       string `json:"message"`
	Retryable     bool   `json:"retryable"`
	NextAction    string `json:"next_action"`
	CorrelationID string `json:"correlation_id"`
}

var refusals = map[string]struct {
	status     int
	message    string
	retryable  bool
	nextAction string
}{
	"invalid_request": {http.StatusBadRequest, "The introspection request is malformed.", false,
		"Correct the request or use a supported version."},
	"unsupported_version": {http.StatusBadRequest, "The identity version is missing or unsupported.", false,
		"Correct the request or use a supported version."},
	"peer_unauthenticated": {http.StatusUnauthorized, "The introspection credential is not valid.", false,
		"Operator checks the peer registration and credential."},
	"peer_forbidden": {http.StatusForbidden, "The introspection credential is not bound to this hub.", false,
		"Operator checks the peer's permitted operations."},
	"identity_unavailable": {http.StatusServiceUnavailable, "Aicrew could not answer now.", true,
		"Retry later with the same context; nothing was applied."},
}

func (s *Server) refuse(w http.ResponseWriter, code string) {
	r := refusals[code]
	var id [8]byte
	_, _ = rand.Read(id[:])
	writeJSON(w, r.status, refusal{Code: code, Message: r.message, Retryable: r.retryable,
		NextAction: r.nextAction, CorrelationID: "corr-" + hex.EncodeToString(id[:])})
}

// bearer returns the single Authorization header's bearer credential.
func bearer(r *http.Request) string {
	values := r.Header.Values("Authorization")
	if len(values) != 1 {
		return ""
	}
	scheme, token, ok := strings.Cut(values[0], " ")
	if !ok || !strings.EqualFold(scheme, "Bearer") {
		return ""
	}
	return token
}

// headerVersionOK requires exactly one version header, and it must be 1.
func headerVersionOK(r *http.Request) bool {
	values := r.Header.Values(VersionHeader)
	return len(values) == 1 && values[0] == "1"
}

// introspect answers one introspection. In order: the peer's credential,
// the request's form and version, the hub it asks for, then the handle. A
// refusal evaluates no handle.
func (s *Server) introspect(w http.ResponseWriter, r *http.Request) {
	hub, err := s.store.AuthenticateIntrospection(r.Context(), bearer(r))
	if err != nil {
		if errors.Is(err, store.ErrUnauthenticated) {
			s.refuse(w, "peer_unauthenticated")
		} else {
			s.refuse(w, "identity_unavailable")
		}
		return
	}
	if mt, _, err := mime.ParseMediaType(r.Header.Get("Content-Type")); err != nil || mt != "application/json" {
		s.refuse(w, "invalid_request")
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, maxIntrospectBody+1))
	if err != nil || len(body) > maxIntrospectBody {
		s.refuse(w, "invalid_request")
		return
	}
	if !headerVersionOK(r) {
		s.refuse(w, "unsupported_version")
		return
	}
	var req introspectRequest
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil || dec.Decode(&struct{}{}) != io.EOF {
		s.refuse(w, "invalid_request")
		return
	}
	if req.Version == nil || *req.Version != 1 {
		s.refuse(w, "unsupported_version")
		return
	}
	if req.HubID != hub {
		s.refuse(w, "peer_forbidden")
		return
	}
	if !nonceShape.MatchString(req.Nonce) {
		s.refuse(w, "invalid_request")
		return
	}
	got, err := s.store.Introspect(r.Context(), req.Handle, hub, s.cfg.ServiceID)
	if err != nil {
		s.refuse(w, "identity_unavailable")
		return
	}
	if !got.Active {
		writeJSON(w, http.StatusOK, inactiveReply{Nonce: req.Nonce, Active: false})
		return
	}
	writeJSON(w, http.StatusOK, activeReply{
		Nonce: req.Nonce, Active: true, ServiceID: s.cfg.ServiceID, HubID: hub,
		Identity: introspectIdentity{UserID: got.UserID, TokenID: got.TokenID},
		AgentID:  got.AgentID, TeamID: got.TeamID, Role: string(got.Role), SessionID: got.SessionID,
		Generation: strconv.FormatInt(got.Generation, 10),
		// Truncated to the second, so it is never later than the true end.
		HandleExpiresAt: got.ExpiresAt.UTC().Truncate(time.Second).Format(time.RFC3339),
	})
}
