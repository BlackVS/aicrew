package server

import (
	"context"
	"net/http"
	"net/url"
	"testing"

	"github.com/BlackVS/aicrew/internal/opapi"
	"github.com/BlackVS/aicrew/internal/store"
)

// GET /v1/admin/merges lists the accepted results that wait for the human
// merge, for the operator and for an architect credential, by team or for
// every team; a result only submitted is not listed, and an unknown query
// parameter or team is refused.
func TestMergesRoute(t *testing.T) {
	e := setupCoordination(t)
	ctx := context.Background()
	id := e.runningOffer(t)
	seq := e.submit(t, id, "submit-1")
	var list []opapi.PendingMerge
	if got := e.adminCall(t, http.MethodGet, opapi.MergesPath, e.opToken, nil, nil, &list); got.status != http.StatusOK || len(list) != 0 {
		t.Fatalf("a submitted result: %d %s", got.status, got.raw)
	}
	b, err := e.store.AuthenticateSessionToken(ctx, e.lead.token)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.store.ReviewResult(ctx, e.lead.caller, "review-1", id, store.ResultReview{SessionID: b.SessionID,
		Generation: b.Generation, ResultSeq: seq, Decision: store.ReviewAccept}); err != nil {
		t.Fatal(err)
	}
	got := e.adminCall(t, http.MethodGet, opapi.MergesPath, e.opToken, nil, nil, &list)
	if got.status != http.StatusOK || len(list) != 1 || list[0].AttemptID != id || list[0].ResultRef != "https://forge.example/pull/7" ||
		list[0].WorkerLabel == "" || list[0].Task.TaskID != "task-1" || list[0].TeamName == "" {
		t.Fatalf("an accepted result: %d %s", got.status, got.raw)
	}
	var cred opapi.ArchitectCredential
	if got := e.adminCall(t, http.MethodPost, opapi.ArchitectCredentialsPath, e.opToken, nil,
		opapi.ArchitectCredentialRequest{Label: "planning"}, &cred); got.status != http.StatusCreated {
		t.Fatalf("issue: %d %s", got.status, got.raw)
	}
	list = nil
	if got := e.adminCall(t, http.MethodGet, opapi.MergesPath, cred.Bearer, url.Values{"team": {teamIDOf(t, e)}}, nil, &list); got.status != http.StatusOK ||
		len(list) != 1 {
		t.Fatalf("the architect reads by team: %d %s", got.status, got.raw)
	}
	adminRefused(t, e.adminCall(t, http.MethodGet, opapi.MergesPath, e.opToken, url.Values{"team": {"no-such-team"}}, nil, nil),
		http.StatusNotFound, opapi.CodeNotFound)
	adminRefused(t, e.adminCall(t, http.MethodGet, opapi.MergesPath, e.opToken, url.Values{"open": {"1"}}, nil, nil),
		http.StatusBadRequest, opapi.CodeInvalid)
}

// teamIDOf is the ID of the coordination test's team.
func teamIDOf(t *testing.T, e *coordEnv) string {
	t.Helper()
	b, err := e.store.AuthenticateSessionToken(context.Background(), e.lead.token)
	if err != nil {
		t.Fatal(err)
	}
	return b.TeamID
}
