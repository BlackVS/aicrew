package server

import (
	"bytes"
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/BlackVS/aicrew/internal/store"
)

// identityFixture is the part of aimem's identity-v1 matrix (testdata) that
// concerns introspection.
type identityFixture struct {
	Exchanges []struct {
		Case      string `json:"case"`
		Operation string `json:"operation"`
		HTTP      struct {
			Method  string            `json:"method"`
			Path    string            `json:"path"`
			Owner   string            `json:"owner"`
			Headers map[string]string `json:"headers"`
		} `json:"http"`
		Request  map[string]any `json:"request"`
		Response struct {
			Status int            `json:"status"`
			Body   map[string]any `json:"body"`
		} `json:"response"`
	} `json:"exchanges"`
	IntrospectionVersion struct {
		Cases []struct {
			Case     string  `json:"case"`
			Header   *string `json:"header"`
			Body     *int    `json:"body"`
			Accepted bool    `json:"accepted"`
		} `json:"cases"`
		Rejection struct {
			AicrewStatus int    `json:"aicrew_status"`
			AicrewCode   string `json:"aicrew_code"`
		} `json:"rejection"`
	} `json:"introspection_version"`
	Bounds struct {
		IntrospectionMaxResponseBytes int `json:"introspection_max_response_bytes"`
	} `json:"bounds"`
}

func loadIdentityFixture(t *testing.T) identityFixture {
	t.Helper()
	raw, err := os.ReadFile("testdata/identity-v1/examples.json")
	if err != nil {
		t.Fatal(err)
	}
	var fx identityFixture
	if err := json.Unmarshal(raw, &fx); err != nil {
		t.Fatal(err)
	}
	return fx
}

func (fx identityFixture) exchange(t *testing.T, name string) (method, path string, headers map[string]string, request, body map[string]any, status int) {
	t.Helper()
	for _, e := range fx.Exchanges {
		if e.Case == name {
			return e.HTTP.Method, e.HTTP.Path, e.HTTP.Headers, e.Request, e.Response.Body, e.Response.Status
		}
	}
	t.Fatalf("the fixture has no %s exchange", name)
	return
}

// introspectEnv is a running service with one introspection credential for
// its hub and one active session with a handle.
type introspectEnv struct {
	*running
	hub, bearer, handle string
	agent               store.Agent
	team                store.Team
	sess                store.Session
	agentCaller         store.Caller
}

// link identifies the seeded agent in aimem, as a verified link would.
type link struct{ hub, user, token string }

func setupIntrospect(t *testing.T, serviceID string, l link) introspectEnv {
	t.Helper()
	ctx := context.Background()
	r := startWith(t, serviceID, nil)
	op, err := store.OperatorCaller("op-test")
	if err != nil {
		t.Fatal(err)
	}
	agent, err := r.store.CreateAgent(ctx, op, "agent", store.NewAgent{Label: "builder"})
	if err != nil {
		t.Fatal(err)
	}
	team, err := r.store.CreateTeam(ctx, op, "team", store.NewTeam{Name: "crew"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.store.AddMember(ctx, op, "member", team.ID, agent.ID, store.RoleWorker); err != nil {
		t.Fatal(err)
	}
	// A verified link is made through the proof flow; seed its outcome.
	db, err := sql.Open("sqlite", r.storePath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`UPDATE agents SET linked_hub_id = ?, linked_user_id = ?, linked_token_id = ? WHERE id = ?`,
		l.hub, l.user, l.token, agent.ID); err != nil {
		t.Fatal(err)
	}
	ac, err := store.AgentCaller(agent.ID)
	if err != nil {
		t.Fatal(err)
	}
	sess, err := r.store.StartSession(ctx, ac, "start", team.ID)
	if err != nil {
		t.Fatal(err)
	}
	_, bearer, err := r.store.IssueIntrospectionCredential(ctx, op, "cred", l.hub)
	if err != nil {
		t.Fatal(err)
	}
	_, handle, err := r.store.IssueSessionHandle(ctx, ac, "handle", sess.ID, sess.Generation, serviceID)
	if err != nil {
		t.Fatal(err)
	}
	return introspectEnv{running: r, hub: l.hub, bearer: bearer, handle: handle, agent: agent, team: team, sess: sess, agentCaller: ac}
}

func newNonce(t *testing.T) string {
	t.Helper()
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		t.Fatal(err)
	}
	return "n-" + hex.EncodeToString(b[:])
}

func randomHandle(t *testing.T) string {
	t.Helper()
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		t.Fatal(err)
	}
	return "acs1_" + base64.RawURLEncoding.EncodeToString(b[:])
}

type introspectCall struct {
	method      string
	path        string
	bearer      string // empty: no Authorization header
	version     *string
	contentType string
	body        []byte
}

func (e introspectEnv) call(t *testing.T, c introspectCall) (int, http.Header, []byte) {
	t.Helper()
	if c.method == "" {
		c.method = http.MethodPost
	}
	if c.path == "" {
		c.path = IntrospectPath
	}
	req, err := http.NewRequest(c.method, e.url(c.path), bytes.NewReader(c.body))
	if err != nil {
		t.Fatal(err)
	}
	if c.bearer != "" {
		req.Header.Set("Authorization", "Bearer "+c.bearer)
	}
	if c.version != nil {
		req.Header.Set(VersionHeader, *c.version)
	}
	if c.contentType != "" {
		req.Header.Set("Content-Type", c.contentType)
	}
	start := time.Now()
	resp, err := e.client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if d := time.Since(start); d > time.Second {
		t.Errorf("introspection took %s; aimem's budget is 2 s", d)
	}
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, resp.Header, b
}

func one() *string { v := "1"; return &v }

// ask sends aimem's request: the version in both places, this hub, a fresh
// nonce and the handle.
func (e introspectEnv) ask(t *testing.T, handle string) (int, []byte, string) {
	t.Helper()
	nonce := newNonce(t)
	body, _ := json.Marshal(map[string]any{"version": 1, "hub_id": e.hub, "nonce": nonce, "handle": handle})
	code, _, reply := e.call(t, introspectCall{bearer: e.bearer, version: one(), contentType: "application/json", body: body})
	return code, reply, nonce
}

// Mirror of aimem's introspect.verify (internal/introspect at 68fcf3a): the
// checks the reference consumer applies to aicrew's reply. It returns the
// active context, or ("", inactive=true) for a valid inactive reply, or an
// error naming the check that failed.
var (
	mirrorID         = regexp.MustCompile(`^[A-Za-z0-9._:-]{1,128}$`)
	mirrorGeneration = regexp.MustCompile(`^[1-9][0-9]{0,19}$`)
	mirrorRoles      = map[string]bool{"coordinator": true, "worker": true, "independent": true}
)

type mirrorReply struct {
	Nonce     *string `json:"nonce"`
	Active    *bool   `json:"active"`
	ServiceID *string `json:"service_id"`
	HubID     *string `json:"hub_id"`
	Identity  *struct {
		UserID  *string `json:"user_id"`
		TokenID *string `json:"token_id"`
	} `json:"identity"`
	AgentID         *string `json:"agent_id"`
	TeamID          *string `json:"team_id"`
	Role            *string `json:"role"`
	SessionID       *string `json:"session_id"`
	Generation      *string `json:"generation"`
	HandleExpiresAt *string `json:"handle_expires_at"`
}

func sv(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

func mirrorVerify(data []byte, nonce, service, hub string, now time.Time) (active bool, err error) {
	if len(data) > 16384 {
		return false, errors.New("oversize")
	}
	var r mirrorReply
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&r); err != nil {
		return false, fmt.Errorf("malformed: %w", err)
	}
	if dec.Decode(&struct{}{}) != io.EOF {
		return false, errors.New("malformed: trailing data")
	}
	if r.Nonce == nil || *r.Nonce != nonce {
		return false, errors.New("nonce_mismatch")
	}
	if r.Active == nil {
		return false, errors.New("malformed: no active")
	}
	if !*r.Active {
		if r.ServiceID != nil || r.HubID != nil || r.Identity != nil || r.AgentID != nil || r.TeamID != nil ||
			r.Role != nil || r.SessionID != nil || r.Generation != nil || r.HandleExpiresAt != nil {
			return false, errors.New("malformed: an inactive reply carries more than its nonce")
		}
		return false, nil
	}
	if r.Identity == nil {
		return false, errors.New("malformed: no identity")
	}
	for _, id := range []string{sv(r.ServiceID), sv(r.HubID), sv(r.Identity.UserID), sv(r.Identity.TokenID),
		sv(r.AgentID), sv(r.TeamID), sv(r.SessionID)} {
		if !mirrorID.MatchString(id) {
			return false, fmt.Errorf("malformed: ID %q", id)
		}
	}
	if sv(r.ServiceID) != service {
		return false, errors.New("wrong_service")
	}
	if sv(r.HubID) != hub {
		return false, errors.New("wrong_hub")
	}
	if !mirrorGeneration.MatchString(sv(r.Generation)) {
		return false, errors.New("bad_generation")
	}
	exp, err := time.Parse(time.RFC3339, sv(r.HandleExpiresAt))
	if err != nil {
		return false, errors.New("malformed: expiry")
	}
	if !exp.After(now) {
		return false, errors.New("expired")
	}
	if !mirrorRoles[sv(r.Role)] {
		return false, errors.New("bad_role")
	}
	return true, nil
}

// keySet lists a JSON object's keys, nested objects as "parent.child".
func keySet(m map[string]any, prefix string) []string {
	var out []string
	for k, v := range m {
		out = append(out, prefix+k)
		if sub, ok := v.(map[string]any); ok {
			out = append(out, keySet(sub, prefix+k+".")...)
		}
	}
	sort.Strings(out)
	return out
}

func decodeObject(t *testing.T, b []byte) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("reply %q: %v", b, err)
	}
	return m
}

var testLink = link{hub: "hub-example", user: "user-example", token: "token-example-1"}

// The route, method and headers are the fixture's, and an active handle's
// reply has exactly the fixture's fields, every one from aicrew's state, and
// passes the reference consumer's checks.
func TestIntrospectActive(t *testing.T) {
	fx := loadIdentityFixture(t)
	method, path, headers, request, body, status := fx.exchange(t, "introspection_active")
	if method != http.MethodPost || path != IntrospectPath {
		t.Fatalf("fixture route %s %s, want POST %s", method, path, IntrospectPath)
	}
	if _, ok := headers[VersionHeader]; !ok || headers["Authorization"] == "" || len(headers) != 2 {
		t.Fatalf("fixture headers %v", headers)
	}
	wantRequest := []string{"handle", "hub_id", "nonce", "version"}
	if got := keySet(request, ""); !reflect.DeepEqual(got, wantRequest) {
		t.Fatalf("fixture request fields %v, want %v", got, wantRequest)
	}

	e := setupIntrospect(t, "aicrew-example", testLink)
	code, reply, nonce := e.ask(t, e.handle)
	if code != status {
		t.Fatalf("status %d %s, want %d", code, reply, status)
	}
	got := decodeObject(t, reply)
	if !reflect.DeepEqual(keySet(got, ""), keySet(body, "")) {
		t.Fatalf("reply fields %v, fixture %v", keySet(got, ""), keySet(body, ""))
	}
	active, err := mirrorVerify(reply, nonce, "aicrew-example", e.hub, time.Now())
	if err != nil || !active {
		t.Fatalf("the reference checks refused the reply: %v (%s)", err, reply)
	}
	want := map[string]any{"nonce": nonce, "active": true, "service_id": "aicrew-example", "hub_id": "hub-example",
		"identity": map[string]any{"user_id": "user-example", "token_id": "token-example-1"},
		"agent_id": e.agent.ID, "team_id": e.team.ID, "role": "worker", "session_id": e.sess.ID,
		"generation": fmt.Sprint(e.sess.Generation), "handle_expires_at": got["handle_expires_at"]}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("reply %v, want %v", got, want)
	}
	exp, _ := time.Parse(time.RFC3339, got["handle_expires_at"].(string))
	if d := time.Until(exp); d <= 0 || d > store.HandleLifetime {
		t.Fatalf("handle_expires_at %v is not within the handle lifetime", exp)
	}
}

// Every inactive state answers exactly the fixture's two fields.
func TestIntrospectInactive(t *testing.T) {
	ctx := context.Background()
	fx := loadIdentityFixture(t)
	_, _, _, _, body, status := fx.exchange(t, "introspection_inactive")
	e := setupIntrospect(t, "aicrew-example", testLink)
	inactive := func(name, handle string) {
		t.Helper()
		code, reply, nonce := e.ask(t, handle)
		if code != status || !reflect.DeepEqual(keySet(decodeObject(t, reply), ""), keySet(body, "")) {
			t.Fatalf("%s: %d %s, want the fixture's inactive reply", name, code, reply)
		}
		if active, err := mirrorVerify(reply, nonce, "aicrew-example", e.hub, time.Now()); err != nil || active {
			t.Fatalf("%s: reference checks = %v, %v", name, active, err)
		}
	}
	inactive("unknown handle", randomHandle(t))
	inactive("malformed handle", "not-a-handle")
	inactive("empty handle", "")
	// A generation change revokes the session's handles.
	if _, err := e.store.ResumeSession(ctx, e.agentCaller, "resume", e.sess.ID); err != nil {
		t.Fatal(err)
	}
	inactive("after resume", e.handle)
}

// The version must be 1 in both the header and the body (the fixture's five
// cases); a refusal evaluates nothing.
func TestIntrospectVersionCases(t *testing.T) {
	fx := loadIdentityFixture(t)
	e := setupIntrospect(t, "aicrew-example", testLink)
	if len(fx.IntrospectionVersion.Cases) != 5 {
		t.Fatalf("fixture version cases = %d", len(fx.IntrospectionVersion.Cases))
	}
	for _, vc := range fx.IntrospectionVersion.Cases {
		t.Run(vc.Case, func(t *testing.T) {
			req := map[string]any{"hub_id": e.hub, "nonce": newNonce(t), "handle": e.handle}
			if vc.Body != nil {
				req["version"] = *vc.Body
			}
			b, _ := json.Marshal(req)
			code, _, reply := e.call(t, introspectCall{bearer: e.bearer, version: vc.Header, contentType: "application/json", body: b})
			if vc.Accepted {
				if code != http.StatusOK || !bytes.Contains(reply, []byte(`"active":true`)) {
					t.Fatalf("accepted case = %d %s", code, reply)
				}
				return
			}
			if code != fx.IntrospectionVersion.Rejection.AicrewStatus || decodeObject(t, reply)["code"] != fx.IntrospectionVersion.Rejection.AicrewCode {
				t.Fatalf("refused case = %d %s, want %d %s", code, reply, fx.IntrospectionVersion.Rejection.AicrewStatus, fx.IntrospectionVersion.Rejection.AicrewCode)
			}
			if bytes.Contains(reply, []byte(`"active"`)) {
				t.Fatalf("a version refusal evaluated the handle: %s", reply)
			}
		})
	}
	// Two version headers are not one.
	req, _ := http.NewRequest(http.MethodPost, e.url(IntrospectPath), bytes.NewReader(
		[]byte(fmt.Sprintf(`{"version":1,"hub_id":%q,"nonce":%q,"handle":%q}`, e.hub, newNonce(t), e.handle))))
	req.Header.Set("Authorization", "Bearer "+e.bearer)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Add(VersionHeader, "1")
	req.Header.Add(VersionHeader, "1")
	resp, err := e.client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("repeated version header = %d, want 400", resp.StatusCode)
	}
}

// refusalOf checks the envelope and returns its code.
func refusalOf(t *testing.T, reply []byte) string {
	t.Helper()
	m := decodeObject(t, reply)
	want := []string{"code", "correlation_id", "message", "next_action", "retryable"}
	if got := keySet(m, ""); !reflect.DeepEqual(got, want) {
		t.Fatalf("refusal fields %v, want %v", got, want)
	}
	return m["code"].(string)
}

// Only an active credential bound to the asked hub is answered.
func TestIntrospectPeerAuthentication(t *testing.T) {
	ctx := context.Background()
	e := setupIntrospect(t, "aicrew-example", testLink)
	op, _ := store.OperatorCaller("op-test")
	body, _ := json.Marshal(map[string]any{"version": 1, "hub_id": e.hub, "nonce": newNonce(t), "handle": e.handle})
	for name, bearer := range map[string]string{
		"no credential":      "",
		"unknown credential": "aicrew_introspect_" + strings.Repeat("0", 64),
		"malformed":          "not-a-credential",
	} {
		code, _, reply := e.call(t, introspectCall{bearer: bearer, version: one(), contentType: "application/json", body: body})
		if code != http.StatusUnauthorized || refusalOf(t, reply) != "peer_unauthenticated" {
			t.Fatalf("%s = %d %s, want 401 peer_unauthenticated", name, code, reply)
		}
	}
	// A credential for another hub asking about this one.
	_, other, err := e.store.IssueIntrospectionCredential(ctx, op, "cred-b", "hub-b")
	if err != nil {
		t.Fatal(err)
	}
	code, _, reply := e.call(t, introspectCall{bearer: other, version: one(), contentType: "application/json", body: body})
	if code != http.StatusForbidden || refusalOf(t, reply) != "peer_forbidden" {
		t.Fatalf("another hub's credential = %d %s, want 403 peer_forbidden", code, reply)
	}
	// A revoked credential.
	creds, err := e.store.ListIntrospectionCredentials(ctx, op)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range creds {
		if c.HubID == e.hub {
			if _, err := e.store.RevokeIntrospectionCredential(ctx, op, "revoke", c.ID); err != nil {
				t.Fatal(err)
			}
		}
	}
	code, _, reply = e.call(t, introspectCall{bearer: e.bearer, version: one(), contentType: "application/json", body: body})
	if code != http.StatusUnauthorized || refusalOf(t, reply) != "peer_unauthenticated" {
		t.Fatalf("a revoked credential = %d %s, want 401", code, reply)
	}
	// Two Authorization headers are refused.
	req, _ := http.NewRequest(http.MethodPost, e.url(IntrospectPath), bytes.NewReader(body))
	req.Header.Add("Authorization", "Bearer "+other)
	req.Header.Add("Authorization", "Bearer "+other)
	req.Header.Set(VersionHeader, "1")
	req.Header.Set("Content-Type", "application/json")
	resp, err := e.client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("two Authorization headers = %d, want 401", resp.StatusCode)
	}
}

// Malformed requests are refused as invalid_request, and only POST is
// served.
func TestIntrospectRequestForm(t *testing.T) {
	e := setupIntrospect(t, "aicrew-example", testLink)
	valid := func() map[string]any {
		return map[string]any{"version": 1, "hub_id": e.hub, "nonce": newNonce(t), "handle": e.handle}
	}
	enc := func(m map[string]any) []byte { b, _ := json.Marshal(m); return b }
	extra := valid()
	extra["expected_user"] = "someone"
	badNonce := valid()
	badNonce["nonce"] = "n-XYZ"
	for name, c := range map[string]introspectCall{
		"no content type":    {body: enc(valid())},
		"text content type":  {contentType: "text/plain", body: enc(valid())},
		"not JSON":           {contentType: "application/json", body: []byte("version=1")},
		"unknown field":      {contentType: "application/json", body: enc(extra)},
		"trailing data":      {contentType: "application/json", body: append(enc(valid()), []byte(" {}")...)},
		"bad nonce":          {contentType: "application/json", body: enc(badNonce)},
		"handle not string":  {contentType: "application/json", body: []byte(`{"version":1,"hub_id":"hub-example","nonce":"n-00000000000000000000000000000000","handle":1}`)},
		"version not number": {contentType: "application/json", body: []byte(`{"version":"1","hub_id":"hub-example","nonce":"n-00000000000000000000000000000000","handle":"x"}`)},
	} {
		c.bearer, c.version = e.bearer, one()
		code, _, reply := e.call(t, c)
		if code != http.StatusBadRequest || refusalOf(t, reply) != "invalid_request" {
			t.Fatalf("%s = %d %s, want 400 invalid_request", name, code, reply)
		}
	}
	for _, method := range []string{http.MethodGet, http.MethodHead, http.MethodPut} {
		if code, allow := e.raw(t, method, IntrospectPath); code != http.StatusMethodNotAllowed || allow != "POST" {
			t.Fatalf("%s = %d, Allow %q; want 405, Allow POST", method, code, allow)
		}
	}
}

// With every ID at its longest, the reply stays within aimem's read bound.
func TestIntrospectReplySize(t *testing.T) {
	fx := loadIdentityFixture(t)
	long := func(c string) string { return strings.Repeat(c, 128) }
	e := setupIntrospect(t, long("s"), link{hub: long("h"), user: long("u"), token: long("t")})
	code, reply, nonce := e.ask(t, e.handle)
	if code != http.StatusOK || len(reply) > fx.Bounds.IntrospectionMaxResponseBytes {
		t.Fatalf("reply %d bytes, status %d; bound %d", len(reply), code, fx.Bounds.IntrospectionMaxResponseBytes)
	}
	if active, err := mirrorVerify(reply, nonce, long("s"), long("h"), time.Now()); err != nil || !active {
		t.Fatalf("reference checks on the largest reply: %v", err)
	}
}

// No handle or credential appears in any reply, refusal or log line.
func TestIntrospectCarriesNoSecrets(t *testing.T) {
	e := setupIntrospect(t, "aicrew-example", testLink)
	var replies [][]byte
	_, r1, _ := e.ask(t, e.handle)
	_, r2, _ := e.ask(t, randomHandle(t))
	body, _ := json.Marshal(map[string]any{"version": 1, "hub_id": "hub-other", "nonce": newNonce(t), "handle": e.handle})
	_, _, r3 := e.call(t, introspectCall{bearer: e.bearer, version: one(), contentType: "application/json", body: body})
	_, _, r4 := e.call(t, introspectCall{bearer: e.bearer + "x", version: one(), contentType: "application/json", body: body})
	replies = append(replies, r1, r2, r3, r4)
	logs := e.logs.String()
	for _, secret := range []string{e.handle, e.bearer, strings.TrimPrefix(e.bearer, "aicrew_introspect_")} {
		for i, r := range replies {
			if bytes.Contains(r, []byte(secret)) {
				t.Fatalf("reply %d contains a secret: %s", i, r)
			}
		}
		if strings.Contains(logs, secret) {
			t.Fatalf("logs contain a secret:\n%s", logs)
		}
	}
	if !strings.Contains(logs, `"route":"POST /v1/crew/introspect"`) {
		t.Fatalf("logs lack the introspection route:\n%s", logs)
	}
}

// rawDeclared sends only the headers of an introspection request declaring
// a body of n bytes, and returns the status and body of the response.
func (e introspectEnv) rawDeclared(t *testing.T, bearer string, n int) (int, []byte) {
	t.Helper()
	auth := ""
	if bearer != "" {
		auth = "Authorization: Bearer " + bearer + "\r\n"
	}
	return e.head(t, fmt.Sprintf("POST %s HTTP/1.1\r\nHost: x\r\n%sContent-Type: application/json\r\n%s: 1\r\nContent-Length: %d\r\n\r\n",
		IntrospectPath, auth, VersionHeader, n))
}

// directIntrospect sends an authenticated introspection request with body
// through the handler chain (see direct).
func (e introspectEnv) directIntrospect(t *testing.T, body io.Reader) (int, []byte) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, IntrospectPath, body)
	req.Header.Set("Authorization", "Bearer "+e.bearer)
	req.Header.Set(VersionHeader, "1")
	req.Header.Set("Content-Type", "application/json")
	rec := e.direct(t, req)
	return rec.Code, rec.Body.Bytes()
}

// The route owns its body limit: an oversized body, declared or not, is
// refused by the route's own contract after authentication, never with the
// generic 413.
func TestIntrospectOversizedBody(t *testing.T) {
	e := setupIntrospect(t, "aicrew-example", testLink)
	for _, n := range []int{MaxBodyBytes + 1, maxIntrospectBody + 1} {
		if code, reply := e.rawDeclared(t, e.bearer, n); code != http.StatusBadRequest || refusalOf(t, reply) != "invalid_request" {
			t.Fatalf("authenticated, declared %d bytes = %d %s; want 400 invalid_request", n, code, reply)
		}
		for _, bearer := range []string{"", "aicrew_introspect_" + strings.Repeat("0", 64)} {
			if code, reply := e.rawDeclared(t, bearer, n); code != http.StatusUnauthorized || refusalOf(t, reply) != "peer_unauthenticated" {
				t.Fatalf("unauthenticated (%q), declared %d bytes = %d %s; want 401", bearer, n, code, reply)
			}
		}
	}
	// A body over the route's limit, declared and undeclared: a valid
	// request padded with whitespace, so only the limit can refuse it.
	valid, _ := json.Marshal(map[string]any{"version": 1, "hub_id": e.hub, "nonce": newNonce(t), "handle": e.handle})
	padded := append(valid, bytes.Repeat([]byte(" "), maxIntrospectBody)...)
	for name, body := range map[string]io.Reader{
		"declared":   bytes.NewReader(padded),
		"undeclared": unsized{bytes.NewReader(padded)},
	} {
		if code, reply := e.directIntrospect(t, body); code != http.StatusBadRequest || refusalOf(t, reply) != "invalid_request" {
			t.Fatalf("%s body over the limit = %d %s; want 400 invalid_request", name, code, reply)
		}
	}
	// The limit itself: a padded body at exactly the limit is served.
	atLimit := append(valid, bytes.Repeat([]byte(" "), maxIntrospectBody-len(valid))...)
	if code, reply := e.directIntrospect(t, bytes.NewReader(atLimit)); code != http.StatusOK {
		t.Fatalf("a body at the limit = %d %s; want it served", code, reply)
	}
	// Other methods on the path are not the route: the generic limit applies.
	if status, code := e.declare(t, http.MethodPut, IntrospectPath, MaxBodyBytes+1); status != http.StatusRequestEntityTooLarge || code != "request_too_large" {
		t.Fatalf("PUT with an oversized body = %d %q, want the generic 413 request_too_large", status, code)
	}
}
