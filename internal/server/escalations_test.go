package server

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/BlackVS/aicrew/internal/opapi"
)

// adminCall sends one operator API request with the bearer and decodes a
// success into out.
func (e *coordEnv) adminCall(t *testing.T, method, path, bearer string, query url.Values, body, out any) reply {
	t.Helper()
	var rd io.Reader
	if body != nil {
		raw, _ := json.Marshal(body)
		rd = bytes.NewReader(raw)
	}
	u := e.url(path)
	if len(query) > 0 {
		u += "?" + query.Encode()
	}
	req, err := http.NewRequest(method, u, rd)
	if err != nil {
		t.Fatal(err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	resp, err := e.client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	got := reply{status: resp.StatusCode, header: resp.Header, raw: string(b)}
	_ = json.Unmarshal(b, &got.body)
	if out != nil && got.status < 300 {
		if err := json.Unmarshal(b, out); err != nil {
			t.Fatalf("%s %s: %v in %s", method, path, err, b)
		}
	}
	return got
}

func escalationBody() map[string]any {
	return map[string]any{
		"task":     map[string]string{"hub_id": coordHub, "project_id": "project-example", "task_id": "task-1"},
		"category": "scope", "urgency": "today",
		"question": "Add the flag criterion 2 names, or amend the criterion?",
		"options": []map[string]string{{"option": "add it", "consequence": "one more flag"},
			{"option": "amend", "consequence": "the criterion changes"}},
		"recommendation": "add it",
	}
}

// The coordinator raises over the session API; an architect credential
// lists and answers over the operator API; the answer reaches the
// coordinator's and the blocked worker's inbox, carrying the decision.
func TestEscalationRoutes(t *testing.T) {
	e := setupCoordination(t)
	body := escalationBody()
	body["blocked"] = e.worker.agent.ID
	got := e.call(t, e.lead.token, EscalationsPath, "raise-1", body)
	if got.status != http.StatusOK || got.body["id"] == nil || got.body["coordinator_agent_id"] != e.lead.agent.ID {
		t.Fatalf("raise: %d %s", got.status, got.raw)
	}
	id := got.body["id"].(string)
	if again := e.call(t, e.lead.token, EscalationsPath, "raise-1", body); again.status != http.StatusOK || again.body["id"] != id {
		t.Fatalf("a replay: %d %s", again.status, again.raw)
	}

	var cred opapi.ArchitectCredential
	if got := e.adminCall(t, http.MethodPost, opapi.ArchitectCredentialsPath, e.opToken, nil,
		opapi.ArchitectCredentialRequest{Label: "planning"}, &cred); got.status != http.StatusCreated || !strings.HasPrefix(cred.Bearer, "aar_") {
		t.Fatalf("issue: %d %s", got.status, got.raw)
	}
	var list []opapi.ArchitectCredential
	if got := e.adminCall(t, http.MethodGet, opapi.ArchitectCredentialsPath, e.opToken, nil, nil, &list); got.status != http.StatusOK ||
		len(list) != 1 || list[0].Bearer != "" || !list[0].Active {
		t.Fatalf("list never carries the bearer: %d %s", got.status, got.raw)
	}

	var open []opapi.Escalation
	if got := e.adminCall(t, http.MethodGet, opapi.EscalationsPath, cred.Bearer, url.Values{"open": {"1"}}, nil, &open); got.status != http.StatusOK ||
		len(open) != 1 || open[0].ID != id || open[0].Blocked != e.worker.agent.ID || len(open[0].Options) != 2 {
		t.Fatalf("the architect lists: %d %s", got.status, got.raw)
	}
	var one opapi.Escalation
	if got := e.adminCall(t, http.MethodGet, opapi.EscalationPath, cred.Bearer, url.Values{"id": {id}}, nil, &one); got.status != http.StatusOK || one.ID != id {
		t.Fatalf("the architect reads: %d %s", got.status, got.raw)
	}
	var answered opapi.Escalation
	ans := opapi.AnswerRequest{ID: id, Decision: "add it", Rationale: "criterion 2 stands"}
	if got := e.adminCall(t, http.MethodPost, opapi.EscalationAnswerPath, cred.Bearer, nil, ans, &answered); got.status != http.StatusOK ||
		answered.Answer == nil || answered.Answer.AnsweredBy != "architect:"+cred.ID {
		t.Fatalf("the architect answers: %d %s", got.status, got.raw)
	}
	adminRefused(t, e.adminCall(t, http.MethodPost, opapi.EscalationAnswerPath, e.opToken, nil, ans, nil),
		http.StatusConflict, opapi.CodeEscalationAnswered)

	for _, m := range []member{e.lead, e.worker} {
		got := e.get(t, m.token, InboxPath+"?limit=10")
		var page struct {
			Messages []struct {
				Kind       string `json:"kind"`
				Text       string `json:"text"`
				Escalation *struct {
					ID       string `json:"id"`
					Decision string `json:"decision"`
				} `json:"escalation"`
			} `json:"messages"`
		}
		if got.status != http.StatusOK || json.Unmarshal([]byte(got.raw), &page) != nil {
			t.Fatalf("inbox: %d %s", got.status, got.raw)
		}
		found := false
		for _, msg := range page.Messages {
			if msg.Escalation != nil && msg.Escalation.ID == id && msg.Escalation.Decision == "add it" && msg.Kind == "lifecycle" {
				found = true
			}
		}
		if !found {
			t.Fatalf("%s has no answer in its inbox: %s", m.agent.Label, got.raw)
		}
	}
	// The independent member was neither raised for nor blocked.
	if got := e.get(t, e.indep.token, InboxPath+"/pending"); strings.Contains(got.raw, "lifecycle") {
		t.Fatalf("the independent member got the answer: %s", got.raw)
	}
}

// A worker cannot raise; a request without a key, with an unknown field or
// an invalid category is refused; an ungranted project is
// project_not_granted.
func TestEscalationRaiseRefusals(t *testing.T) {
	e := setupCoordination(t)
	refused(t, e.call(t, e.worker.token, EscalationsPath, "w-1", escalationBody()), http.StatusForbidden, "role_forbidden")
	refused(t, e.call(t, e.lead.token, EscalationsPath, "", escalationBody()), http.StatusBadRequest, "invalid_request")
	extra := escalationBody()
	extra["surprise"] = 1
	refused(t, e.call(t, e.lead.token, EscalationsPath, "x-1", extra), http.StatusBadRequest, "invalid_request")
	bad := escalationBody()
	bad["category"] = "whim"
	got := e.call(t, e.lead.token, EscalationsPath, "c-1", bad)
	refused(t, got, http.StatusBadRequest, "invalid_request")
	if !strings.Contains(got.raw, "category") {
		t.Fatalf("the refusal does not say what is wrong: %s", got.raw)
	}
	ungranted := escalationBody()
	ungranted["task"] = map[string]string{"hub_id": coordHub, "project_id": "other", "task_id": "t"}
	refused(t, e.call(t, e.lead.token, EscalationsPath, "g-1", ungranted), http.StatusConflict, "project_not_granted")
	refused(t, e.call(t, "", EscalationsPath, "n-1", escalationBody()), http.StatusUnauthorized, "invalid_token")
}

// An architect credential reaches the three escalation routes and nothing
// else: every other operator route refuses it as unauthorized, and so do the
// escalation routes once it is revoked. A member's session token reaches
// none of them.
func TestArchitectCredentialReachesOnlyEscalations(t *testing.T) {
	e := setupCoordination(t)
	var cred opapi.ArchitectCredential
	if got := e.adminCall(t, http.MethodPost, opapi.ArchitectCredentialsPath, e.opToken, nil,
		opapi.ArchitectCredentialRequest{Label: "planning"}, &cred); got.status != http.StatusCreated {
		t.Fatalf("issue: %d %s", got.status, got.raw)
	}
	escalation := map[string]bool{opapi.EscalationsPath: true, opapi.EscalationPath: true, opapi.EscalationAnswerPath: true}
	for _, rt := range adminRoutes(e.srv) {
		e.srv.admin.failures = newLimiter(AdminFailuresPerMinute, 1)
		got := e.adminCall(t, rt[0], rt[1], cred.Bearer, nil, map[string]string{"name": "x"}, nil)
		if escalation[rt[1]] {
			if got.status == http.StatusUnauthorized {
				t.Fatalf("the architect is refused on %s %s: %s", rt[0], rt[1], got.raw)
			}
			continue
		}
		if got.status != http.StatusUnauthorized || got.body["code"] != opapi.CodeUnauthorized {
			t.Fatalf("the architect reached %s %s: %d %s", rt[0], rt[1], got.status, got.raw)
		}
	}
	for path := range escalation {
		e.srv.admin.failures = newLimiter(AdminFailuresPerMinute, 1)
		method := http.MethodGet
		if path == opapi.EscalationAnswerPath {
			method = http.MethodPost
		}
		if got := e.adminCall(t, method, path, e.lead.token, nil, nil, nil); got.status != http.StatusUnauthorized {
			t.Fatalf("a member's session token on %s: %d %s", path, got.status, got.raw)
		}
	}
	if got := e.adminCall(t, http.MethodPost, opapi.ArchitectCredentialRevokePath, e.opToken, nil, opapi.IDRequest{ID: cred.ID}, nil); got.status != http.StatusOK {
		t.Fatalf("revoke: %d %s", got.status, got.raw)
	}
	e.srv.admin.failures = newLimiter(AdminFailuresPerMinute, 1)
	adminRefused(t, e.adminCall(t, http.MethodGet, opapi.EscalationsPath, cred.Bearer, nil, nil, nil),
		http.StatusUnauthorized, opapi.CodeUnauthorized)
	if strings.Contains(e.logs.String(), cred.Bearer) {
		t.Fatal("the log holds the architect's bearer")
	}
}
