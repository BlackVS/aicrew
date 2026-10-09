package server

import (
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/BlackVS/aicrew/internal/opapi"
	"github.com/BlackVS/aicrew/internal/store"
)

// Escalations (docs/DESIGN-CONTROL-PLANE.md, section 7.5; task 5598). The
// team's coordinator raises one over the session API; the architect, with
// an architect credential, or the operator lists, reads and answers them
// over the operator API. An architect credential reaches those three routes
// and nothing else: every other operator route compares its bearer with the
// operator credential alone and refuses it as unauthorized.

// EscalationsPath is the member route a coordinator raises an escalation on.
const EscalationsPath = "/v1/crew/escalations"

func (s *Server) registerEscalations() {
	s.handle(http.MethodPost, EscalationsPath, s.raiseEscalation)
	s.handle(http.MethodGet, opapi.EscalationsPath, s.escalationRoute("escalation.list", s.listEscalations))
	s.handle(http.MethodGet, opapi.EscalationPath, s.escalationRoute("escalation.show", s.showEscalation))
	s.handle(http.MethodPost, opapi.EscalationAnswerPath, s.escalationRoute("escalation.answer", s.answerEscalation))
	s.handle(http.MethodGet, opapi.ArchitectCredentialsPath, s.operator("architect.credential.list", s.listArchitectCredentials))
	s.handle(http.MethodPost, opapi.ArchitectCredentialsPath, s.operator("architect.credential.issue", s.issueArchitectCredential))
	s.handle(http.MethodPost, opapi.ArchitectCredentialRevokePath, s.operator("architect.credential.revoke", s.revokeArchitectCredential))
}

// raiseEscalation is POST /v1/crew/escalations: the token's session, which
// must be its team's current coordinator, raises an escalation.
func (s *Server) raiseEscalation(w http.ResponseWriter, r *http.Request) {
	var in store.EscalationRequest
	token, key, ok := s.stepRequest(w, r, true, &in)
	if !ok {
		return
	}
	e, err := s.store.RaiseEscalationWithToken(r.Context(), key, token, in)
	switch {
	case errors.Is(err, store.ErrProjectNotGranted):
		s.refuseSession(w, r, "project_not_granted", false, 0)
	case errors.Is(err, store.ErrInvalid):
		// The store's message names what is wrong with the input, never
		// a secret.
		s.refuseSessionSaying(w, r, "invalid_request", false, 0, strings.TrimPrefix(err.Error(), store.ErrInvalid.Error()+": "))
	case err != nil:
		s.refuseSession(w, r, refusalCode(err), false, 0)
	default:
		writeJSON(w, http.StatusOK, escalationOf(e))
	}
}

func (s *Server) listEscalations(w http.ResponseWriter, r *http.Request, op store.Caller) (string, string) {
	q := r.URL.Query()
	open := q.Get("open")
	if open != "" && open != "1" {
		adminRefuse(w, http.StatusBadRequest, opapi.CodeInvalid, "open is 1 or absent")
		return opapi.CodeInvalid, ""
	}
	list, err := s.store.ListEscalations(r.Context(), op, q.Get("team"), open == "1")
	if err != nil {
		return adminFail(w, err), ""
	}
	out := make([]opapi.Escalation, 0, len(list))
	for _, e := range list {
		out = append(out, escalationOf(e))
	}
	writeJSON(w, http.StatusOK, out)
	return "ok", ""
}

func (s *Server) showEscalation(w http.ResponseWriter, r *http.Request, op store.Caller) (string, string) {
	e, err := s.store.GetEscalation(r.Context(), op, r.URL.Query().Get("id"))
	if err != nil {
		return adminFail(w, err), ""
	}
	writeJSON(w, http.StatusOK, escalationOf(e))
	return "ok", e.ID
}

func (s *Server) answerEscalation(w http.ResponseWriter, r *http.Request, op store.Caller) (string, string) {
	var in opapi.AnswerRequest
	if !adminBody(w, r, &in) {
		return opapi.CodeInvalid, ""
	}
	e, err := s.store.AnswerEscalation(r.Context(), op, commandKey(), in.ID,
		store.EscalationAnswer{Decision: in.Decision, Rationale: in.Rationale})
	if err != nil {
		return adminFail(w, err), ""
	}
	writeJSON(w, http.StatusOK, escalationOf(e))
	return "answered", e.ID
}

func (s *Server) listArchitectCredentials(w http.ResponseWriter, r *http.Request, op store.Caller) (string, string) {
	list, err := s.store.ListArchitectCredentials(r.Context(), op)
	if err != nil {
		return adminFail(w, err), ""
	}
	now := time.Now()
	out := make([]opapi.ArchitectCredential, 0, len(list))
	for _, c := range list {
		out = append(out, architectCredentialOf(c, now))
	}
	writeJSON(w, http.StatusOK, out)
	return "ok", ""
}

func (s *Server) issueArchitectCredential(w http.ResponseWriter, r *http.Request, op store.Caller) (string, string) {
	var in opapi.ArchitectCredentialRequest
	if !adminBody(w, r, &in) {
		return opapi.CodeInvalid, ""
	}
	c, bearer, err := s.store.IssueArchitectCredential(r.Context(), op, commandKey(), in.Label)
	if err != nil {
		return adminFail(w, err), ""
	}
	out := architectCredentialOf(c, time.Now())
	out.Bearer = bearer
	writeJSON(w, http.StatusCreated, out)
	return "issued", c.ID
}

func (s *Server) revokeArchitectCredential(w http.ResponseWriter, r *http.Request, op store.Caller) (string, string) {
	var in opapi.IDRequest
	if !adminBody(w, r, &in) {
		return opapi.CodeInvalid, ""
	}
	c, err := s.store.RevokeArchitectCredential(r.Context(), op, commandKey(), in.ID)
	if err != nil {
		return adminFail(w, err), ""
	}
	writeJSON(w, http.StatusOK, architectCredentialOf(c, time.Now()))
	return "revoked", c.ID
}

func escalationOf(e store.Escalation) opapi.Escalation {
	out := opapi.Escalation{ID: e.ID, TeamID: e.TeamID, CoordinatorAgentID: e.Coordinator,
		Task:      opapi.EscalationTask{HubID: e.Task.HubID, ProjectID: e.Task.ProjectID, TaskID: e.Task.TaskID},
		AttemptID: e.AttemptID, Category: e.Category, Question: e.Question, Context: e.Context,
		Options: []opapi.EscalationOption{}, Recommendation: e.Recommendation, Blocked: e.Blocked,
		Urgency: e.Urgency, CreatedAt: e.CreatedAt}
	for _, o := range e.Options {
		out.Options = append(out.Options, opapi.EscalationOption{Option: o.Option, Consequence: o.Consequence})
	}
	if a := e.Answer; a != nil {
		out.Answer = &opapi.EscalationAnswer{Decision: a.Decision, Rationale: a.Rationale, AnsweredBy: a.AnsweredBy,
			AnsweredAt: a.AnsweredAt}
	}
	return out
}

func architectCredentialOf(c store.ArchitectCredential, now time.Time) opapi.ArchitectCredential {
	return opapi.ArchitectCredential{ID: c.ID, Label: c.Label, Active: c.Active(now), CreatedAt: c.CreatedAt,
		ExpiresAt: c.ExpiresAt, RevokedAt: c.RevokedAt}
}
