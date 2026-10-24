package server

import (
	"net/http"
	"net/url"
	"testing"
	"time"

	"github.com/BlackVS/aicrew/internal/opapi"
)

// GET /v1/admin/attempt/handoff reads an attempt's handoff for the operator
// and for an architect credential, none before the first push; a member's
// session token, an unknown attempt and an unknown parameter are refused.
// The coordinator's open attempts and the operator's team state carry the
// push time.
func TestAttemptHandoffAdminRoute(t *testing.T) {
	e := setupCoordination(t)
	id := e.runningOffer(t)
	q := url.Values{"id": {id}}

	var h opapi.AttemptHandoff
	if got := e.adminCall(t, http.MethodGet, opapi.AttemptHandoffPath, e.opToken, q, nil, &h); got.status != http.StatusOK ||
		h.AttemptID != id || h.TeamID != teamIDOf(t, e) || h.Handoff != "" || !h.UpdatedAt.IsZero() {
		t.Fatalf("no handoff yet: %d %s", got.status, got.raw)
	}
	open := e.get(t, e.lead.token, AttemptsPath)
	if a := open.body["attempts"].([]any)[0].(map[string]any); open.status != http.StatusOK || a["handoff_updated_at"] != nil {
		t.Fatalf("the open attempts before a push: %d %s", open.status, open.raw)
	}

	doc := "# Handoff\n\n## CURRENT ATTEMPT\n\n### Next\n\nOpen the pull request.\n"
	if got := e.call(t, e.worker.token, AttemptsPath+"/"+id+"/handoff", "", map[string]string{"handoff": doc}); got.status != http.StatusOK {
		t.Fatalf("the push: %d %s", got.status, got.raw)
	}
	if got := e.adminCall(t, http.MethodGet, opapi.AttemptHandoffPath, e.opToken, q, nil, &h); got.status != http.StatusOK ||
		h.Handoff != doc || h.UpdatedAt.IsZero() {
		t.Fatalf("the operator's read: %d %s", got.status, got.raw)
	}
	var cred opapi.ArchitectCredential
	if got := e.adminCall(t, http.MethodPost, opapi.ArchitectCredentialsPath, e.opToken, nil,
		opapi.ArchitectCredentialRequest{Label: "planning"}, &cred); got.status != http.StatusCreated {
		t.Fatalf("issue: %d %s", got.status, got.raw)
	}
	var ah opapi.AttemptHandoff
	if got := e.adminCall(t, http.MethodGet, opapi.AttemptHandoffPath, cred.Bearer, q, nil, &ah); got.status != http.StatusOK ||
		ah.Handoff != doc {
		t.Fatalf("the architect's read: %d %s", got.status, got.raw)
	}

	open = e.get(t, e.lead.token, AttemptsPath)
	if a := open.body["attempts"].([]any)[0].(map[string]any); open.status != http.StatusOK ||
		a["handoff_updated_at"] != h.UpdatedAt.Format(time.RFC3339Nano) {
		t.Fatalf("the open attempts after a push (pushed %s): %d %s", h.UpdatedAt, open.status, open.raw)
	}
	var st opapi.TeamState
	if got := e.adminCall(t, http.MethodGet, opapi.TeamStatePath, e.opToken, url.Values{"id": {teamIDOf(t, e)}}, nil, &st); got.status != http.StatusOK {
		t.Fatalf("the team state: %d %s", got.status, got.raw)
	}
	found := false
	for _, m := range st.Members {
		if a := m.OpenAttempt; a != nil && a.ID == id {
			found = a.HandoffUpdatedAt.Equal(h.UpdatedAt)
		}
	}
	if !found {
		t.Fatalf("the team state does not carry the push time %s: %+v", h.UpdatedAt, st.Members)
	}

	adminRefused(t, e.adminCall(t, http.MethodGet, opapi.AttemptHandoffPath, e.worker.token, q, nil, nil),
		http.StatusUnauthorized, opapi.CodeUnauthorized)
	adminRefused(t, e.adminCall(t, http.MethodGet, opapi.AttemptHandoffPath, e.opToken, url.Values{"id": {"no-such-attempt"}}, nil, nil),
		http.StatusNotFound, opapi.CodeNotFound)
	adminRefused(t, e.adminCall(t, http.MethodGet, opapi.AttemptHandoffPath, e.opToken, nil, nil, nil),
		http.StatusBadRequest, opapi.CodeInvalid)
	adminRefused(t, e.adminCall(t, http.MethodGet, opapi.AttemptHandoffPath, e.opToken, url.Values{"id": {id}, "team": {"x"}}, nil, nil),
		http.StatusBadRequest, opapi.CodeInvalid)
}
