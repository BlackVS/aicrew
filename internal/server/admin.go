package server

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/BlackVS/aicrew/internal/opapi"
	"github.com/BlackVS/aicrew/internal/optoken"
	"github.com/BlackVS/aicrew/internal/store"
)

// The operator API (package opapi): team, invitation and introspection
// credential administration over aicrewd's own listener, so the service never
// stops for it. Every route requires the operator credential, a bearer read
// from operator_token_file on every call: replacing the file rotates it with
// no restart. A member's session handle or aimem's introspection bearer is
// never accepted here, and these routes are never consulted for them.
//
// Failed authentications are counted per client address; once an address
// has spent its budget it is refused before its bearer is compared. Every
// operator action is logged with its action, outcome and the ID it touched,
// never a body, a bearer or a code.

const (
	// AdminFailuresPerMinute bounds failed operator authentications per
	// client address.
	AdminFailuresPerMinute = 10
	maxAdminBody           = 16 << 10
	// operatorCallerID is the store's record of who acted.
	operatorCallerID = "aicrew-operator"
)

// adminState is the operator API's own state.
type adminState struct {
	failures *limiter
	// names serializes team creation and renaming, so two concurrent
	// requests can never both take one name: the store does not enforce
	// names' uniqueness, the operator commands always have.
	names sync.Mutex
}

func (s *Server) registerAdmin() {
	s.admin.failures = newLimiter(AdminFailuresPerMinute, time.Minute)
	s.handle(http.MethodGet, opapi.CredentialsPath, s.operator("credential.list", s.listCredentials))
	s.handle(http.MethodPost, opapi.CredentialsPath, s.operator("credential.issue", s.issueCredential))
	s.handle(http.MethodPost, opapi.CredentialRotatePath, s.operator("credential.rotate", s.rotateCredential))
	s.handle(http.MethodPost, opapi.CredentialRevokePath, s.operator("credential.revoke", s.revokeCredential))
	s.handle(http.MethodGet, opapi.TeamsPath, s.operator("team.list", s.listTeams))
	s.handle(http.MethodPost, opapi.TeamsPath, s.operator("team.create", s.createTeam))
	s.handle(http.MethodGet, opapi.TeamPath, s.operator("team.show", s.showTeam))
	s.handle(http.MethodPost, opapi.TeamProjectsPath, s.operator("team.projects", s.setTeamProjects))
	s.handle(http.MethodPost, opapi.TeamRenamePath, s.operator("team.rename", s.renameTeam))
	s.handle(http.MethodGet, opapi.InvitationsPath, s.operator("invitation.list", s.listInvitations))
	s.handle(http.MethodPost, opapi.InvitationsPath, s.operator("invitation.issue", s.issueInvitation))
	s.handle(http.MethodPost, opapi.InvitationRevokePath, s.operator("invitation.revoke", s.revokeInvitation))
}

// checkOperatorToken reads the operator token file as a call would, so a
// missing, readable-by-others or malformed file is found at start.
func (s *Server) checkOperatorToken() error {
	if s.cfg.OperatorTokenFile == "" {
		return errors.New("operator_token_file is required")
	}
	_, err := optoken.Read(s.cfg.OperatorTokenFile)
	return err
}

// adminAction is one operator handler. It answers the request and returns
// the outcome and the ID it touched, for the log.
type adminAction func(w http.ResponseWriter, r *http.Request, op store.Caller) (outcome, id string)

// operator wraps an action with the operator's authentication and its log.
func (s *Server) operator(action string, h adminAction) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		addr := clientAddr(r)
		if spent, wait := s.admin.failures.spent(addr); spent {
			s.adminLog(action, opapi.CodeRateLimited, "")
			w.Header().Set("Retry-After", strconv.Itoa(int(wait.Seconds())+1))
			adminRefuse(w, http.StatusTooManyRequests, opapi.CodeRateLimited, "too many failed operator authentications")
			return
		}
		want, err := optoken.Read(s.cfg.OperatorTokenFile)
		if err != nil {
			// The file's error names the file, never its content.
			s.log.Error("the operator token file cannot be read", "error", err.Error())
			s.adminLog(action, opapi.CodeUnavailable, "")
			adminRefuse(w, http.StatusServiceUnavailable, opapi.CodeUnavailable, "the operator credential is unavailable on the service")
			return
		}
		got := bearer(r)
		if got == "" || subtle.ConstantTimeCompare([]byte(got), []byte(want)) != 1 {
			s.admin.failures.allow(addr)
			s.adminLog(action, opapi.CodeUnauthorized, "")
			w.Header().Set("WWW-Authenticate", `Bearer realm="aicrew-operator"`)
			adminRefuse(w, http.StatusUnauthorized, opapi.CodeUnauthorized, "the operator credential is missing or wrong")
			return
		}
		op, err := store.OperatorCaller(operatorCallerID)
		if err != nil {
			s.adminLog(action, opapi.CodeInternal, "")
			adminRefuse(w, http.StatusInternalServerError, opapi.CodeInternal, "")
			return
		}
		outcome, id := h(w, r, op)
		s.adminLog(action, outcome, id)
	}
}

func (s *Server) adminLog(action, outcome, id string) {
	attrs := []any{"action", action, "outcome", outcome}
	if id != "" {
		attrs = append(attrs, "id", id)
	}
	s.log.Info("operator", attrs...)
}

func adminRefuse(w http.ResponseWriter, status int, code, message string) {
	writeJSON(w, status, opapi.Error{Code: code, Message: message})
}

// adminBody decodes a write's JSON body strictly.
func adminBody(w http.ResponseWriter, r *http.Request, v any) bool {
	if !mediaType(r, "application/json") {
		adminRefuse(w, http.StatusUnsupportedMediaType, opapi.CodeInvalid, "the body must be application/json")
		return false
	}
	b, ok := readBody(r, maxAdminBody)
	if !ok {
		adminRefuse(w, http.StatusRequestEntityTooLarge, opapi.CodeInvalid, "the body is too large")
		return false
	}
	if err := decodeStrict(b, v); err != nil {
		adminRefuse(w, http.StatusBadRequest, opapi.CodeInvalid, "the body is not the route's JSON object")
		return false
	}
	return true
}

// adminFail answers a store error and returns its code for the log. Store
// messages describe the input, never a secret.
func adminFail(w http.ResponseWriter, err error) string {
	status, code := http.StatusInternalServerError, opapi.CodeInternal
	switch {
	case errors.Is(err, store.ErrInvalid):
		status, code = http.StatusBadRequest, opapi.CodeInvalid
	case errors.Is(err, store.ErrNotFound):
		status, code = http.StatusNotFound, opapi.CodeNotFound
	case errors.Is(err, store.ErrRevisionConflict):
		status, code = http.StatusConflict, opapi.CodeRevisionConflict
	case errors.Is(err, store.ErrCredentialLimit):
		status, code = http.StatusConflict, opapi.CodeCredentialLimit
	case errors.Is(err, store.ErrInvitationFinal):
		status, code = http.StatusConflict, opapi.CodeInvitationFinal
	}
	msg := ""
	if code != opapi.CodeInternal {
		msg = err.Error()
	}
	adminRefuse(w, status, code, msg)
	return code
}

// commandKey is a fresh store key: every operator write is its own command.
// A client never sends one: a replayed issue could not return its secret,
// which exists only in the answer that issued it.
func commandKey() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	return "op-" + hex.EncodeToString(b[:])
}

func (s *Server) listCredentials(w http.ResponseWriter, r *http.Request, op store.Caller) (string, string) {
	creds, err := s.store.ListIntrospectionCredentials(r.Context(), op)
	if err != nil {
		return adminFail(w, err), ""
	}
	hub, now := r.URL.Query().Get("hub"), time.Now().UTC()
	out := []opapi.Credential{}
	for _, c := range creds {
		if hub == "" || c.HubID == hub {
			out = append(out, opapi.CredentialOf(c, now))
		}
	}
	writeJSON(w, http.StatusOK, out)
	return "ok", ""
}

// opsOf maps the request's operation names; nil for an unknown one.
func opsOf(names []string) []string {
	ops := []string{}
	for _, n := range names {
		switch n {
		case "introspection":
			ops = append(ops, store.OpIntrospection)
		case "coordination":
			ops = append(ops, store.OpCoordination)
		default:
			return nil
		}
	}
	return ops
}

func (s *Server) issueCredential(w http.ResponseWriter, r *http.Request, op store.Caller) (string, string) {
	return s.issueOrRotate(w, r, op, false)
}

func (s *Server) rotateCredential(w http.ResponseWriter, r *http.Request, op store.Caller) (string, string) {
	return s.issueOrRotate(w, r, op, true)
}

func (s *Server) issueOrRotate(w http.ResponseWriter, r *http.Request, op store.Caller, rotate bool) (string, string) {
	var req opapi.CredentialRequest
	if !adminBody(w, r, &req) {
		return opapi.CodeInvalid, ""
	}
	ops := opsOf(req.Operations)
	if ops == nil {
		adminRefuse(w, http.StatusBadRequest, opapi.CodeInvalid, "operations are introspection and coordination")
		return opapi.CodeInvalid, ""
	}
	ctx := r.Context()
	replaces := ""
	if rotate {
		creds, err := s.store.ListIntrospectionCredentials(ctx, op)
		if err != nil {
			return adminFail(w, err), ""
		}
		now := time.Now().UTC()
		var active []string
		for _, c := range creds {
			if c.HubID == req.HubID && c.Active(now) {
				active = append(active, c.ID)
			}
		}
		if len(active) != 1 {
			adminRefuse(w, http.StatusConflict, opapi.CodeRotateNeedsOne,
				fmt.Sprintf("rotate needs exactly one active credential for the hub; it has %d", len(active)))
			return opapi.CodeRotateNeedsOne, ""
		}
		replaces = active[0]
	}
	c, bearer, err := s.store.IssueIntrospectionCredential(ctx, op, commandKey(), req.HubID, ops...)
	if err != nil {
		return adminFail(w, err), ""
	}
	v := opapi.CredentialOf(c, time.Now().UTC())
	v.Bearer, v.Replaces = bearer, replaces
	writeJSON(w, http.StatusCreated, v)
	return "issued", c.ID
}

func (s *Server) revokeCredential(w http.ResponseWriter, r *http.Request, op store.Caller) (string, string) {
	var req opapi.IDRequest
	if !adminBody(w, r, &req) {
		return opapi.CodeInvalid, ""
	}
	c, err := s.store.RevokeIntrospectionCredential(r.Context(), op, commandKey(), req.ID)
	if err != nil {
		return adminFail(w, err), req.ID
	}
	writeJSON(w, http.StatusOK, opapi.CredentialOf(c, time.Now().UTC()))
	return "revoked", c.ID
}

func (s *Server) listTeams(w http.ResponseWriter, r *http.Request, _ store.Caller) (string, string) {
	teams, err := s.store.ListTeams(r.Context())
	if err != nil {
		return adminFail(w, err), ""
	}
	writeJSON(w, http.StatusOK, teams)
	return "ok", ""
}

// nameTaken refuses a name another team has. The caller holds names.
func (s *Server) nameTaken(w http.ResponseWriter, r *http.Request, name, self string) (string, bool) {
	teams, err := s.store.ListTeams(r.Context())
	if err != nil {
		return adminFail(w, err), true
	}
	for _, t := range teams {
		if t.Name == name && t.ID != self {
			adminRefuse(w, http.StatusConflict, opapi.CodeTeamExists, fmt.Sprintf("team %s is already named %q", t.ID, name))
			return opapi.CodeTeamExists, true
		}
	}
	return "", false
}

func (s *Server) createTeam(w http.ResponseWriter, r *http.Request, op store.Caller) (string, string) {
	var req opapi.TeamRequest
	if !adminBody(w, r, &req) {
		return opapi.CodeInvalid, ""
	}
	s.admin.names.Lock()
	defer s.admin.names.Unlock()
	if code, taken := s.nameTaken(w, r, req.Name, ""); taken {
		return code, ""
	}
	t, err := s.store.CreateTeam(r.Context(), op, commandKey(), store.NewTeam{Name: req.Name, Projects: req.Projects})
	if err != nil {
		return adminFail(w, err), ""
	}
	writeJSON(w, http.StatusCreated, t)
	return "created", t.ID
}

func (s *Server) showTeam(w http.ResponseWriter, r *http.Request, _ store.Caller) (string, string) {
	id := r.URL.Query().Get("id")
	if id == "" {
		adminRefuse(w, http.StatusBadRequest, opapi.CodeInvalid, "the query names the team: ?id=TEAM")
		return opapi.CodeInvalid, ""
	}
	t, err := s.store.GetTeam(r.Context(), id)
	if err != nil {
		return adminFail(w, err), id
	}
	ms, err := s.store.ListMembers(r.Context(), id)
	if err != nil {
		return adminFail(w, err), id
	}
	d := opapi.TeamDetail{Team: t, Members: []opapi.Member{}}
	for _, m := range ms {
		d.Members = append(d.Members, opapi.Member{AgentID: m.AgentID, Role: m.Role, Revision: m.Revision, CreatedAt: m.CreatedAt})
	}
	writeJSON(w, http.StatusOK, d)
	return "ok", id
}

func (s *Server) setTeamProjects(w http.ResponseWriter, r *http.Request, op store.Caller) (string, string) {
	var req opapi.TeamProjectsRequest
	if !adminBody(w, r, &req) {
		return opapi.CodeInvalid, ""
	}
	if req.Projects == nil {
		req.Projects = []store.ProjectRef{}
	}
	t, err := s.store.SetTeamProjects(r.Context(), op, commandKey(), req.TeamID, req.ExpectedRevision, req.Projects)
	if err != nil {
		return adminFail(w, err), req.TeamID
	}
	writeJSON(w, http.StatusOK, t)
	return "updated", t.ID
}

func (s *Server) renameTeam(w http.ResponseWriter, r *http.Request, op store.Caller) (string, string) {
	var req opapi.TeamRenameRequest
	if !adminBody(w, r, &req) {
		return opapi.CodeInvalid, ""
	}
	s.admin.names.Lock()
	defer s.admin.names.Unlock()
	if code, taken := s.nameTaken(w, r, req.Name, req.TeamID); taken {
		return code, req.TeamID
	}
	t, err := s.store.RenameTeam(r.Context(), op, commandKey(), req.TeamID, req.ExpectedRevision, req.Name)
	if err != nil {
		return adminFail(w, err), req.TeamID
	}
	writeJSON(w, http.StatusOK, t)
	return "renamed", t.ID
}

func (s *Server) listInvitations(w http.ResponseWriter, r *http.Request, op store.Caller) (string, string) {
	invs, err := s.store.ListInvitations(r.Context(), op)
	if err != nil {
		return adminFail(w, err), ""
	}
	team, now := r.URL.Query().Get("team"), time.Now().UTC()
	out := []opapi.Invitation{}
	for _, inv := range invs {
		if team == "" || inv.TeamID == team {
			out = append(out, opapi.InvitationOf(inv, now))
		}
	}
	writeJSON(w, http.StatusOK, out)
	return "ok", ""
}

func (s *Server) issueInvitation(w http.ResponseWriter, r *http.Request, op store.Caller) (string, string) {
	var req opapi.InvitationRequest
	if !adminBody(w, r, &req) {
		return opapi.CodeInvalid, ""
	}
	var ttl time.Duration
	if req.TTL != "" {
		d, err := time.ParseDuration(req.TTL)
		if err != nil {
			adminRefuse(w, http.StatusBadRequest, opapi.CodeInvalid, "ttl is a duration such as \"24h\"")
			return opapi.CodeInvalid, ""
		}
		ttl = d
	}
	code, err := store.GenerateInvitationCode()
	if err != nil {
		return adminFail(w, err), ""
	}
	inv, err := s.store.IssueInvitation(r.Context(), op, commandKey(), store.InvitationRequest{
		Purpose: store.InvitationPurpose(req.Purpose), TeamID: req.TeamID, Role: store.Role(req.Role), HubID: req.HubID,
		AgentID: req.AgentID, ExpectedUserID: req.ExpectedUserID, Label: req.Label, TTL: ttl}, code)
	if err != nil {
		return adminFail(w, err), ""
	}
	v := opapi.InvitationOf(inv, time.Now().UTC())
	v.Code = code.Reveal()
	writeJSON(w, http.StatusCreated, v)
	return "issued", inv.ID
}

func (s *Server) revokeInvitation(w http.ResponseWriter, r *http.Request, op store.Caller) (string, string) {
	var req opapi.IDRequest
	if !adminBody(w, r, &req) {
		return opapi.CodeInvalid, ""
	}
	inv, err := s.store.GetInvitation(r.Context(), req.ID)
	if err == nil {
		inv, err = s.store.RevokeInvitation(r.Context(), op, commandKey(), inv.ID, inv.Revision)
	}
	if err != nil {
		return adminFail(w, err), req.ID
	}
	writeJSON(w, http.StatusOK, opapi.InvitationOf(inv, time.Now().UTC()))
	return "revoked", inv.ID
}
