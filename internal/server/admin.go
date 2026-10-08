package server

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/BlackVS/aicrew/internal/hubteams"
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
// Failed authentications are counted per client address. Every attempt
// takes a token from the address's budget before its bearer is compared, in
// one atomic step, and a successful one gives it back: concurrent attempts
// can never compare more bearers than the budget holds, and an address
// without a token is refused before any comparison. Every
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
	// names serializes team creation, renaming and registration, so two
	// concurrent requests can never both take one name (the store does not
	// enforce names' uniqueness, the operator commands always have), and a
	// registration never sends a name a rename has since replaced.
	names sync.Mutex
}

func (s *Server) registerAdmin() {
	s.admin.failures = newLimiter(AdminFailuresPerMinute, time.Minute)
	// The credential routes are served under both names for one release.
	for _, p := range []struct{ list, rotate, revoke string }{
		{opapi.CredentialsPath, opapi.CredentialRotatePath, opapi.CredentialRevokePath},
		{opapi.LegacyCredentialsPath, opapi.LegacyCredentialRotatePath, opapi.LegacyCredentialRevokePath},
	} {
		s.handle(http.MethodGet, p.list, s.operator("credential.list", s.listCredentials))
		s.handle(http.MethodPost, p.list, s.operator("credential.issue", s.issueCredential))
		s.handle(http.MethodPost, p.rotate, s.operator("credential.rotate", s.rotateCredential))
		s.handle(http.MethodPost, p.revoke, s.operator("credential.revoke", s.revokeCredential))
	}
	s.handle(http.MethodGet, opapi.TeamsPath, s.operator("team.list", s.listTeams))
	s.handle(http.MethodPost, opapi.TeamsPath, s.operator("team.create", s.createTeam))
	s.handle(http.MethodGet, opapi.TeamPath, s.operator("team.show", s.showTeam))
	s.handle(http.MethodPost, opapi.TeamRenamePath, s.operator("team.rename", s.renameTeam))
	s.handle(http.MethodPost, opapi.TeamRegisterPath, s.operator("team.register", s.registerTeamRoute))
	s.handle(http.MethodPost, opapi.TeamGrantsPath, s.operator("team.grants", s.readTeamGrants))
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
		if ok, wait := s.admin.failures.allow(addr); !ok {
			s.adminLog(action, opapi.CodeRateLimited, "")
			w.Header().Set("Retry-After", strconv.Itoa(int(wait.Seconds())+1))
			adminRefuse(w, http.StatusTooManyRequests, opapi.CodeRateLimited, "too many failed operator authentications")
			return
		}
		want, err := optoken.Read(s.cfg.OperatorTokenFile)
		if err != nil {
			// The file's error names the file, never its content.
			s.log.Error("the operator token file cannot be read", "error", err.Error())
			s.admin.failures.refund(addr) // the service's fault, not the client's
			s.adminLog(action, opapi.CodeUnavailable, "")
			adminRefuse(w, http.StatusServiceUnavailable, opapi.CodeUnavailable, "the operator credential is unavailable on the service")
			return
		}
		got := bearer(r)
		if got == "" || subtle.ConstantTimeCompare([]byte(got), []byte(want)) != 1 {
			// The attempt's token stays taken: it counts as a failure.
			s.adminLog(action, opapi.CodeUnauthorized, "")
			w.Header().Set("WWW-Authenticate", `Bearer realm="aicrew-operator"`)
			adminRefuse(w, http.StatusUnauthorized, opapi.CodeUnauthorized, "the operator credential is missing or wrong")
			return
		}
		s.admin.failures.refund(addr)
		op, err := store.OperatorCaller(operatorCallerID)
		if err != nil {
			s.adminLog(action, opapi.CodeInternal, "")
			adminRefuse(w, http.StatusInternalServerError, opapi.CodeInternal, "")
			return
		}
		// id is one the store returned, never the request's text, which a
		// refused request may have filled with anything.
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

// The API's records, converted from the store's: opapi depends on nothing of
// aicrew's, so its client links no store.

func credentialOf(c store.IntrospectionCredential, now time.Time) opapi.Credential {
	return opapi.Credential{ID: c.ID, HubID: c.HubID, Active: c.Active(now), CreatedAt: c.CreatedAt,
		ExpiresAt: c.ExpiresAt, RevokedAt: c.RevokedAt, Operations: c.Operations}
}

func invitationOf(inv store.Invitation, now time.Time, names map[string]string) opapi.Invitation {
	open := inv.State == store.InvitationIssued || inv.State == store.InvitationLocked
	return opapi.Invitation{ID: inv.ID, Purpose: string(inv.Purpose), TeamID: inv.TeamID, TeamName: names[inv.TeamID], Role: string(inv.Role),
		HubID: inv.HubID, AgentID: inv.AgentID, ExpectedUserID: inv.ExpectedUserID, Label: inv.Label,
		IssuedBy: inv.IssuedBy, State: string(inv.State), Expired: open && !now.Before(inv.ExpiresAt),
		Attempts: inv.Attempts, Revision: inv.Revision, ExpiresAt: inv.ExpiresAt, CreatedAt: inv.CreatedAt}
}

func teamOf(t store.Team) opapi.Team {
	grants := make([]opapi.Grant, 0, len(t.Grants))
	for _, g := range t.Grants {
		v := opapi.Grant{HubID: g.HubID, ProjectID: g.ProjectID}
		if r := g.Repository; r != nil {
			v.Repository = &opapi.GrantRepository{Kind: r.Kind, URL: r.URL, Host: r.Host, Access: r.Access}
		}
		if p := g.Process; p != nil {
			v.Process = &opapi.GrantProcess{Repo: p.Repo, Commit: p.Commit, Manifest: p.Manifest}
		}
		grants = append(grants, v)
	}
	v := opapi.Team{ID: t.ID, Name: t.Name, Grants: grants, GrantsState: t.GrantsState, GrantsReadAt: t.GrantsReadAt,
		Revision: t.Revision, CoordinatorGeneration: t.CoordinatorGeneration, CreatedAt: t.CreatedAt, UpdatedAt: t.UpdatedAt,
		Hub: t.Hub}
	if r := t.Registration; r != nil {
		v.Registration = &opapi.TeamRegistration{State: r.State, Detail: r.Detail, Name: r.Name, At: r.At}
	}
	return v
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
			out = append(out, credentialOf(c, now))
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
	v := credentialOf(c, time.Now().UTC())
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
		return adminFail(w, err), ""
	}
	writeJSON(w, http.StatusOK, credentialOf(c, time.Now().UTC()))
	return "revoked", c.ID
}

func (s *Server) listTeams(w http.ResponseWriter, r *http.Request, _ store.Caller) (string, string) {
	teams, err := s.store.ListTeams(r.Context())
	if err != nil {
		return adminFail(w, err), ""
	}
	out := make([]opapi.TeamSummary, 0, len(teams))
	for _, t := range teams {
		out = append(out, opapi.TeamSummary{Team: teamOf(t.Team), Members: t.Members})
	}
	writeJSON(w, http.StatusOK, out)
	return "ok", ""
}

// nameTaken refuses a name another team has. The caller holds names, and op
// is the operator's caller: only an authenticated handler has one.
func (s *Server) nameTaken(w http.ResponseWriter, r *http.Request, _ store.Caller, name, self string) (string, bool) {
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
	if code, taken := s.nameTaken(w, r, op, req.Name, ""); taken {
		return code, ""
	}
	if req.Hub != "" {
		if msg := s.hubProblem(req.Hub); msg != "" {
			adminRefuse(w, http.StatusBadRequest, opapi.CodeInvalid, msg)
			return opapi.CodeInvalid, ""
		}
	}
	t, err := s.store.CreateTeam(r.Context(), op, commandKey(), store.NewTeam{Name: req.Name, Hub: req.Hub})
	if err != nil {
		return adminFail(w, err), ""
	}
	t = s.registerTeam(r.Context(), op, t)
	writeJSON(w, http.StatusCreated, teamOf(t))
	return "created", t.ID
}

// hubProblem says why alias cannot be a team's hub, or "" when it can: a
// configured hub with a hub ID.
func (s *Server) hubProblem(alias string) string {
	b, ok := s.hubs[alias]
	switch {
	case !ok:
		return fmt.Sprintf("hub %q is not an aimem block of aicrewd.json (aimem_hubs[].name)", alias)
	case b.id == "":
		return fmt.Sprintf("hub %q is the aimem block before 0.3.0, which names no hub_id: move it into aimem_hubs", alias)
	}
	return ""
}

// registerTeam registers t on its hub through team.register, when the team
// names a hub whose block holds a team.register credential, and records the
// outcome on the team. The team exists whatever the hub answers; a refusal
// or an unreachable hub is recorded with what to do, and
// `aicrew team register` retries. op is the operator's caller: only an
// authenticated handler has one.
func (s *Server) registerTeam(ctx context.Context, op store.Caller, t store.Team) store.Team {
	b, ok := s.hubs[t.Hub]
	if t.Hub == "" || !ok || b.teams == nil || !b.teams.CanRegister() {
		return t
	}
	reg := store.TeamRegistration{State: "registered"}
	r, err := b.teams.Register(ctx, t.ID, t.Name)
	if err == nil {
		reg.Name = r.TeamName
	} else {
		reg.State, reg.Detail = registrationOutcome(hubteams.Code(err), t.Name)
	}
	recorded, rerr := s.store.RecordTeamRegistration(ctx, op, commandKey(), t.ID, reg)
	if rerr != nil {
		s.log.Error("record a team registration", "team", t.ID, "err", rerr)
		return t
	}
	return recorded
}

// registrationOutcome is a refused registration's state and what to do.
func registrationOutcome(code, name string) (string, string) {
	switch code {
	case "team_name_taken":
		return code, fmt.Sprintf("another team of this service holds the name %q on the hub: rename this team, then run aicrew team register", name)
	case "profile_disabled":
		return code, "the hub's operator disabled this team's profile: re-enable it on the hub, then run aicrew team register"
	case hubteams.CodeUnavailable, "rate_limited", "request_in_progress", "identity_unavailable":
		return hubteams.CodeUnavailable, "the hub did not answer: run aicrew team register later"
	case "peer_forbidden", "peer_unknown":
		return code, "the hub does not know this service's team.register credential as this service_id's: " +
			"check service_id in aicrewd.json against aimem identity peer list, then run aicrew team register"
	case "peer_unauthenticated":
		return code, "the hub does not accept the team.register credential: it is not the hub's, or it is revoked; " +
			"provision the peer again (aimem identity peer provision), then run aicrew team register"
	case "":
		return "failed", "the registration failed: run aicrew team register"
	}
	return code, "the hub refused the registration (" + code + "): check the hub's peer and its team.register credential, then run aicrew team register"
}

// registerTeamRoute is POST /v1/admin/team/register: register a team on its
// hub again.
func (s *Server) registerTeamRoute(w http.ResponseWriter, r *http.Request, op store.Caller) (string, string) {
	var req opapi.TeamRegisterRequest
	if !adminBody(w, r, &req) {
		return opapi.CodeInvalid, ""
	}
	s.admin.names.Lock()
	defer s.admin.names.Unlock()
	t, err := s.store.GetTeam(r.Context(), req.ID)
	if err != nil {
		return adminFail(w, err), ""
	}
	if req.Hub != "" && req.Hub != t.Hub {
		if msg := s.hubProblem(req.Hub); msg != "" {
			adminRefuse(w, http.StatusBadRequest, opapi.CodeInvalid, msg)
			return opapi.CodeInvalid, t.ID
		}
		if t, err = s.store.SetTeamHub(r.Context(), op, commandKey(), t.ID, req.Hub); err != nil {
			return adminFail(w, err), req.ID
		}
	}
	b, ok := s.hubs[t.Hub]
	if t.Hub == "" || !ok || b.teams == nil || !b.teams.CanRegister() {
		adminRefuse(w, http.StatusConflict, opapi.CodeInvalid,
			"the team names no hub with a team.register credential (aimem_hubs[].team_register_token_file)")
		return opapi.CodeInvalid, t.ID
	}
	t = s.registerTeam(r.Context(), op, t)
	writeJSON(w, http.StatusOK, teamOf(t))
	return "registered", t.ID
}

// readTeamGrants is POST /v1/admin/team/grants: read the team's grants from
// its hub now, record them as the minute's refresh would, and answer the
// team with this service's ID. A hub that does not answer, or refuses the
// read, is refused hub_unavailable with the hub's code.
func (s *Server) readTeamGrants(w http.ResponseWriter, r *http.Request, _ store.Caller) (string, string) {
	var req opapi.TeamGrantsRequest
	if !adminBody(w, r, &req) {
		return opapi.CodeInvalid, ""
	}
	t, err := s.store.GetTeam(r.Context(), req.ID)
	if err != nil {
		return adminFail(w, err), ""
	}
	b, ok := s.hubs[t.Hub]
	if t.Hub == "" || !ok || b.teams == nil || !b.teams.CanRead() {
		adminRefuse(w, http.StatusConflict, opapi.CodeInvalid,
			"the team names no hub with a team.read credential (aimem_hubs[].team_read_token_file)")
		return opapi.CodeInvalid, t.ID
	}
	team, err := b.teams.ReadTeam(r.Context(), t.ID)
	read, answered := grantsRead(b.id, team, err, time.Now())
	if !answered {
		code := hubteams.Code(err)
		if code == "" {
			code = hubteams.CodeUnavailable
		}
		adminRefuse(w, http.StatusServiceUnavailable, opapi.CodeHubUnavailable, "the hub did not answer the team read ("+code+")")
		return opapi.CodeHubUnavailable, t.ID
	}
	if _, err := s.store.RecordTeamGrants(r.Context(), store.ReconcilerCaller(), t.ID, read); err != nil {
		return adminFail(w, err), t.ID
	}
	if t, err = s.store.GetTeam(r.Context(), t.ID); err != nil {
		return adminFail(w, err), req.ID
	}
	writeJSON(w, http.StatusOK, opapi.TeamGrants{Team: teamOf(t), ServiceID: s.cfg.ServiceID})
	return "read", t.ID
}

func (s *Server) showTeam(w http.ResponseWriter, r *http.Request, _ store.Caller) (string, string) {
	id := r.URL.Query().Get("id")
	if id == "" {
		adminRefuse(w, http.StatusBadRequest, opapi.CodeInvalid, "the query names the team: ?id=TEAM")
		return opapi.CodeInvalid, ""
	}
	t, err := s.store.GetTeam(r.Context(), id)
	if err != nil {
		return adminFail(w, err), ""
	}
	ms, err := s.store.ListMembers(r.Context(), id)
	if err != nil {
		return adminFail(w, err), ""
	}
	d := opapi.TeamDetail{Team: teamOf(t), Members: []opapi.Member{}}
	for _, m := range ms {
		d.Members = append(d.Members, opapi.Member{AgentID: m.AgentID, Role: string(m.Role), Revision: m.Revision, CreatedAt: m.CreatedAt})
	}
	writeJSON(w, http.StatusOK, d)
	return "ok", t.ID
}

func (s *Server) renameTeam(w http.ResponseWriter, r *http.Request, op store.Caller) (string, string) {
	var req opapi.TeamRenameRequest
	if !adminBody(w, r, &req) {
		return opapi.CodeInvalid, ""
	}
	s.admin.names.Lock()
	defer s.admin.names.Unlock()
	if code, taken := s.nameTaken(w, r, op, req.Name, req.TeamID); taken {
		return code, ""
	}
	t, err := s.store.RenameTeam(r.Context(), op, commandKey(), req.TeamID, req.ExpectedRevision, req.Name)
	if err != nil {
		return adminFail(w, err), ""
	}
	t = s.registerTeam(r.Context(), op, t)
	writeJSON(w, http.StatusOK, teamOf(t))
	return "renamed", t.ID
}

func (s *Server) listInvitations(w http.ResponseWriter, r *http.Request, op store.Caller) (string, string) {
	invs, err := s.store.ListInvitations(r.Context(), op)
	if err != nil {
		return adminFail(w, err), ""
	}
	names, err := s.teamNames(r.Context(), op)
	if err != nil {
		return adminFail(w, err), ""
	}
	team, now := r.URL.Query().Get("team"), time.Now().UTC()
	out := []opapi.Invitation{}
	for _, inv := range invs {
		if team == "" || inv.TeamID == team {
			out = append(out, invitationOf(inv, now, names))
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
	v := invitationOf(inv, time.Now().UTC(), s.teamNamesOrNone(r.Context(), op))
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
		return adminFail(w, err), ""
	}
	writeJSON(w, http.StatusOK, invitationOf(inv, time.Now().UTC(), s.teamNamesOrNone(r.Context(), op)))
	return "revoked", inv.ID
}

// teamNames maps every team's ID to its current name, for answers that name
// a team by both. op is the operator's caller: only an authenticated handler
// has one.
func (s *Server) teamNames(ctx context.Context, _ store.Caller) (map[string]string, error) {
	teams, err := s.store.ListTeams(ctx)
	if err != nil {
		return nil, err
	}
	names := make(map[string]string, len(teams))
	for _, t := range teams {
		names[t.ID] = t.Name
	}
	return names, nil
}

// teamNamesOrNone is teamNames for the answer of a write that has already
// committed: a failed read leaves the names out rather than failing it.
func (s *Server) teamNamesOrNone(ctx context.Context, op store.Caller) map[string]string {
	names, _ := s.teamNames(ctx, op)
	return names
}
