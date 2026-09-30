package server

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"strconv"
	"time"

	"github.com/BlackVS/aicrew/internal/store"
)

// Coordination facts, coordination.v1 (aimem docs/DESIGN-AIFORGE-
// COORDINATION-WIRE.md at 8ac4ef1; docs/CREW-CONTRACT.md, "Coordination
// facts"). Before it commits a coordinated reservation step, aimem asks this
// route about the step's proof, with its introspection credential. Every
// value in an active answer comes from aicrew's own state; the request
// supplies only the proof, the hub it asks for and a nonce to echo.

const (
	// CoordinationPath is the route, on the introspection endpoint's origin.
	CoordinationPath = "/v1/crew/coordination"
	// CoordinationVersionHeader carries coordination.v1's version; the body
	// carries it too.
	CoordinationVersionHeader = "X-Aimem-Coordination-Version"

	maxCoordinationBody = 4 << 10
)

var coordinationRefusals = map[string]refusalText{
	"invalid_request": {http.StatusBadRequest, "The coordination request is malformed.", false,
		"Correct the request or use a supported version."},
	"unsupported_version": {http.StatusBadRequest, "The coordination version is missing or unsupported.", false,
		"Correct the request or use a supported version."},
	"peer_unauthenticated": {http.StatusUnauthorized, "The peer credential is not valid for coordination facts.", false,
		"Operator checks the peer registration and the credential's operations."},
	"peer_forbidden": {http.StatusForbidden, "The peer credential is not bound to this hub.", false,
		"Operator checks the peer's permitted operations."},
	"identity_unavailable": {http.StatusServiceUnavailable, "Aicrew could not answer now.", true,
		"Retry later with the same context; nothing was applied."},
}

type coordinationRequest struct {
	Version *int   `json:"version"`
	HubID   string `json:"hub_id"`
	Nonce   string `json:"nonce"`
	Proof   string `json:"proof"`
}

type factMember struct {
	UserID     string `json:"user_id"`
	AgentID    string `json:"agent_id"`
	TeamID     string `json:"team_id"`
	Role       string `json:"role"`
	SessionID  string `json:"session_id"`
	Generation string `json:"generation"`
}

type factWorker struct {
	UserID  string `json:"user_id"`
	AgentID string `json:"agent_id"`
}

// processPin is exactly coordination.v1's three fields, copied verbatim from
// aicrew's record: aimem compares them byte for byte.
type processPin struct {
	Repo     string `json:"repo"`
	Commit   string `json:"commit"`
	Manifest string `json:"manifest"`
}

type coordinationFact struct {
	Kind             string      `json:"kind"`
	Operation        string      `json:"operation"`
	TaskID           string      `json:"task_id"`
	RequestKeyDigest string      `json:"request_key_digest"`
	Member           factMember  `json:"member"`
	OfferRef         string      `json:"offer_ref,omitempty"`
	AttemptRef       string      `json:"attempt_ref,omitempty"`
	IntendedWorker   *factWorker `json:"intended_worker,omitempty"`
	Process          *processPin `json:"process,omitempty"`
	EvidenceDigest   string      `json:"evidence_digest,omitempty"`
	ExpiresAt        string      `json:"expires_at"`
}

type coordinationReply struct {
	Nonce     string           `json:"nonce"`
	Active    bool             `json:"active"`
	ServiceID string           `json:"service_id"`
	HubID     string           `json:"hub_id"`
	Fact      coordinationFact `json:"fact"`
}

// coordination answers one question about a proof. In order: the peer's
// credential, which must permit coordination facts; the request's form and
// version; the hub it asks for; then the proof. A refusal evaluates no proof.
func (s *Server) coordination(w http.ResponseWriter, r *http.Request) {
	hub, err := s.store.AuthenticateCoordination(r.Context(), bearer(r))
	if err != nil {
		if errors.Is(err, store.ErrUnauthenticated) {
			s.refuseFrom(w, coordinationRefusals, "peer_unauthenticated")
		} else {
			s.refuseFrom(w, coordinationRefusals, "identity_unavailable")
		}
		return
	}
	if mt, _, err := mime.ParseMediaType(r.Header.Get("Content-Type")); err != nil || mt != "application/json" {
		s.refuseFrom(w, coordinationRefusals, "invalid_request")
		return
	}
	if r.ContentLength > maxCoordinationBody {
		s.refuseFrom(w, coordinationRefusals, "invalid_request")
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, maxCoordinationBody+1))
	if err != nil || len(body) > maxCoordinationBody {
		s.refuseFrom(w, coordinationRefusals, "invalid_request")
		return
	}
	if values := r.Header.Values(CoordinationVersionHeader); len(values) != 1 || values[0] != "1" {
		s.refuseFrom(w, coordinationRefusals, "unsupported_version")
		return
	}
	var req coordinationRequest
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil || dec.Decode(&struct{}{}) != io.EOF {
		s.refuseFrom(w, coordinationRefusals, "invalid_request")
		return
	}
	if req.Version == nil || *req.Version != 1 {
		s.refuseFrom(w, coordinationRefusals, "unsupported_version")
		return
	}
	if req.HubID != hub {
		s.refuseFrom(w, coordinationRefusals, "peer_forbidden")
		return
	}
	if !nonceShape.MatchString(req.Nonce) {
		s.refuseFrom(w, coordinationRefusals, "invalid_request")
		return
	}
	f, err := s.store.CoordinationFact(r.Context(), req.Proof, hub)
	if err != nil {
		s.refuseFrom(w, coordinationRefusals, "identity_unavailable")
		return
	}
	if !f.Active {
		writeJSON(w, http.StatusOK, inactiveReply{Nonce: req.Nonce, Active: false})
		return
	}
	fact := coordinationFact{
		Kind: string(f.Kind), Operation: string(f.Operation), TaskID: f.Task.TaskID,
		RequestKeyDigest: f.RequestKeyDigest,
		Member: factMember{UserID: f.Member.UserID, AgentID: f.Member.AgentID, TeamID: f.Member.TeamID,
			Role: string(f.Member.Role), SessionID: f.Member.SessionID,
			Generation: strconv.FormatInt(f.Member.Generation, 10)},
		OfferRef: f.OfferRef, AttemptRef: f.AttemptRef, EvidenceDigest: f.EvidenceDigest,
		// Truncated to the second, so it is never later than the true end.
		ExpiresAt: f.ExpiresAt.UTC().Truncate(time.Second).Format(time.RFC3339),
	}
	if f.IntendedWorker != nil {
		fact.IntendedWorker = &factWorker{UserID: f.IntendedWorker.UserID, AgentID: f.IntendedWorker.AgentID}
	}
	if f.Process != nil {
		p := f.Process.Identity
		fact.Process = &processPin{Repo: p.Repository, Commit: p.Commit, Manifest: p.Manifest}
	}
	writeJSON(w, http.StatusOK, coordinationReply{Nonce: req.Nonce, Active: true, ServiceID: s.cfg.ServiceID,
		HubID: hub, Fact: fact})
}
