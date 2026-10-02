package server

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/BlackVS/aicrew/internal/privatefile"
	"github.com/BlackVS/aicrew/internal/privatefile/privatefiletest"
	"github.com/BlackVS/aicrew/internal/store"
	"github.com/BlackVS/aicrew/internal/verifier"
)

// fakeVerifier stands in for aimem: it vouches for the receipts it knows.
// When gate is set, Redeem waits on it after signalling inside.
type fakeVerifier struct {
	mu     sync.Mutex
	ids    map[string]store.VerifiedIdentity
	inside chan struct{}
	gate   chan struct{}
}

func (f *fakeVerifier) Redeem(ctx context.Context, req store.RedeemRequest) (store.VerifiedIdentity, error) {
	f.mu.Lock()
	id, ok := f.ids[req.Receipt.Reveal()]
	inside, gate := f.inside, f.gate
	f.mu.Unlock()
	if gate != nil {
		close(inside)
		<-gate
	}
	if !ok {
		return store.VerifiedIdentity{}, &verifier.Error{Code: "proof_invalid", Reason: "refused"}
	}
	return id, nil
}

// receipt registers a well-formed receipt for the seeded identity.
func (f *fakeVerifier) receipt(n int) string {
	r := "amr1_" + strings.Repeat(string(rune('A'+n%26)), 43)
	f.mu.Lock()
	f.ids[r] = store.VerifiedIdentity{HubID: "hub-test", UserID: "user-1", TokenID: "tok-1"}
	f.mu.Unlock()
	return r
}

type apiEnv struct {
	*running
	v       *fakeVerifier
	agentID string
	teamID  string
	n       int
}

// setupAPI starts the service with a fake aimem and one linked member of a
// team, not yet in session.
func setupAPI(t *testing.T, opts ...func(*Server)) *apiEnv {
	t.Helper()
	ctx := context.Background()
	v := &fakeVerifier{ids: map[string]store.VerifiedIdentity{}}
	r := start(t, func(s *Server) {
		s.verifier = v
		for _, o := range opts {
			o(s)
		}
	})
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
	if _, err := db.Exec(`UPDATE agents SET linked_hub_id = 'hub-test', linked_user_id = 'user-1', linked_token_id = 'tok-1' WHERE id = ?`,
		agent.ID); err != nil {
		t.Fatal(err)
	}
	return &apiEnv{running: r, v: v, agentID: agent.ID, teamID: team.ID}
}

type reply struct {
	status int
	header http.Header
	body   map[string]any
	raw    string
}

func (e *apiEnv) send(t *testing.T, method, path, contentType, key, bearerToken string, body string) reply {
	t.Helper()
	req, err := http.NewRequest(method, e.url(path), strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	if key != "" {
		req.Header.Set("Idempotency-Key", key)
	}
	if bearerToken != "" {
		req.Header.Set("Authorization", "Bearer "+bearerToken)
	}
	resp, err := e.client.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	out := reply{status: resp.StatusCode, header: resp.Header, raw: string(raw)}
	_ = json.Unmarshal(raw, &out.body)
	return out
}

func (e *apiEnv) challenge(t *testing.T, key, agentID string) reply {
	t.Helper()
	b, _ := json.Marshal(map[string]string{"agent_id": agentID})
	return e.send(t, http.MethodPost, ChallengesPath, "application/json", key, "", string(b))
}

func (e *apiEnv) exchange(t *testing.T, key string, form url.Values) reply {
	t.Helper()
	return e.send(t, http.MethodPost, TokenPath, "application/x-www-form-urlencoded", key, "", form.Encode())
}

// entryForm is a complete entry exchange for a fresh challenge and receipt.
func (e *apiEnv) entryForm(t *testing.T) url.Values {
	t.Helper()
	e.n++
	ch := e.challenge(t, "c-"+strings.Repeat("x", e.n), e.agentID)
	if ch.status != http.StatusOK {
		t.Fatalf("challenge: %d %s", ch.status, ch.raw)
	}
	return url.Values{
		"grant_type": {GrantTokenExchange}, "subject_token": {e.v.receipt(e.n)}, "subject_token_type": {ProofTokenType},
		"audience": {"aicrew-test"}, "requested_token_type": {AccessTokenType},
		"challenge_id": {ch.body["challenge_id"].(string)}, "team_id": {e.teamID},
	}
}

func (e *apiEnv) enter(t *testing.T, key string) reply {
	t.Helper()
	got := e.exchange(t, key, e.entryForm(t))
	if got.status != http.StatusOK {
		t.Fatalf("entry: %d %s", got.status, got.raw)
	}
	return got
}

func refreshForm(token string) url.Values {
	return url.Values{"grant_type": {GrantTokenExchange}, "subject_token": {token}, "subject_token_type": {AccessTokenType},
		"audience": {"hub-test"}, "requested_token_type": {HandleTokenType}}
}

func (e *apiEnv) handleActive(t *testing.T, handle string) bool {
	t.Helper()
	got, err := e.store.Introspect(context.Background(), handle, "hub-test", "aicrew-test")
	if err != nil {
		t.Fatal(err)
	}
	return got.Active
}

func refused(t *testing.T, got reply, status int, code string) {
	t.Helper()
	if got.status != status || got.body["code"] != code {
		t.Fatalf("got %d %s, want %d %s", got.status, got.raw, status, code)
	}
	for _, f := range []string{"message", "next_action", "correlation_id"} {
		if s, _ := got.body[f].(string); s == "" {
			t.Fatalf("refusal lacks %s: %s", f, got.raw)
		}
	}
}

// The whole client sequence: challenge, entry, status, refresh, leave, a
// replayed leave, and the token dead afterwards.
func TestSessionAPIFlow(t *testing.T) {
	e := setupAPI(t)
	ch := e.challenge(t, "c1", e.agentID)
	if ch.status != http.StatusOK || ch.body["hub_id"] != "hub-test" || ch.body["service_id"] != "aicrew-test" ||
		ch.body["challenge_id"] == "" || ch.body["expires_at"] == "" {
		t.Fatalf("challenge = %d %s", ch.status, ch.raw)
	}
	form := url.Values{"grant_type": {GrantTokenExchange}, "subject_token": {e.v.receipt(1)},
		"subject_token_type": {ProofTokenType}, "audience": {"aicrew-test"},
		"challenge_id": {ch.body["challenge_id"].(string)}, "team_id": {e.teamID}}
	got := e.exchange(t, "enter", form)
	if got.status != http.StatusOK {
		t.Fatalf("entry = %d %s", got.status, got.raw)
	}
	token, _ := got.body["access_token"].(string)
	handle, _ := got.body["aimem_handle"].(string)
	session, _ := got.body["session"].(map[string]any)
	if !strings.HasPrefix(token, "ast1_") || !strings.HasPrefix(handle, "acs1_") || got.body["token_type"] != "Bearer" ||
		got.body["issued_token_type"] != AccessTokenType || session["team_id"] != e.teamID || session["generation"] != "1" {
		t.Fatalf("entry reply = %s", got.raw)
	}
	if in := got.body["expires_in"].(float64); in < 8*3600-60 || in > 8*3600 {
		t.Fatalf("expires_in = %v", in)
	}
	if in := got.body["aimem_handle_expires_in"].(float64); in < 14*60 || in > 15*60 {
		t.Fatalf("aimem_handle_expires_in = %v", in)
	}
	if got.header.Get("Cache-Control") != "no-store" || !e.handleActive(t, handle) {
		t.Fatal("no no-store, or the first handle is inactive")
	}

	st := e.send(t, http.MethodGet, SessionPath, "", "", token, "")
	if st.status != http.StatusOK || st.body["hub_id"] != "hub-test" || st.body["user_id"] != "user-1" ||
		strings.Contains(st.raw, token) || strings.Contains(st.raw, handle) {
		t.Fatalf("status = %d %s", st.status, st.raw)
	}

	ref := e.exchange(t, "refresh-1", refreshForm(token))
	newHandle, _ := ref.body["access_token"].(string)
	if ref.status != http.StatusOK || !strings.HasPrefix(newHandle, "acs1_") || ref.body["token_type"] != "N_A" ||
		ref.body["issued_token_type"] != HandleTokenType || !e.handleActive(t, newHandle) {
		t.Fatalf("refresh = %d %s", ref.status, ref.raw)
	}
	refused(t, e.exchange(t, "refresh-1", refreshForm(token)), http.StatusConflict, "refresh_replayed")

	left := e.send(t, http.MethodPost, LeavePath, "application/json", "leave", token, "")
	if left.status != http.StatusOK || left.body["state"] != "left" {
		t.Fatalf("leave = %d %s", left.status, left.raw)
	}
	if again := e.send(t, http.MethodPost, LeavePath, "application/json", "leave", token, "{}"); again.status != http.StatusOK || again.raw != left.raw {
		t.Fatalf("replayed leave = %d %s", again.status, again.raw)
	}
	gone := e.send(t, http.MethodGet, SessionPath, "", "", token, "")
	refused(t, gone, http.StatusUnauthorized, "invalid_token")
	if gone.header.Get("WWW-Authenticate") != `Bearer error="invalid_token"` {
		t.Fatalf("WWW-Authenticate = %q", gone.header.Get("WWW-Authenticate"))
	}
	if e.handleActive(t, newHandle) {
		t.Fatal("a handle outlived the session")
	}
	for name, secret := range map[string]string{"token": token, "handle": handle, "receipt": form.Get("subject_token")} {
		if strings.Contains(e.logs.String(), secret) {
			t.Fatalf("the log carries the %s", name)
		}
	}
}

// Resume with a proof fences the previous token and handle.
func TestResumeExchange(t *testing.T) {
	e := setupAPI(t)
	first := e.enter(t, "enter")
	form := e.entryForm(t)
	form.Del("team_id")
	form.Set("session_id", first.body["session"].(map[string]any)["id"].(string))
	got := e.exchange(t, "resume", form)
	if got.status != http.StatusOK || got.body["session"].(map[string]any)["generation"] != "2" {
		t.Fatalf("resume = %d %s", got.status, got.raw)
	}
	refused(t, e.send(t, http.MethodGet, SessionPath, "", "", first.body["access_token"].(string), ""),
		http.StatusUnauthorized, "invalid_token")
	if e.handleActive(t, first.body["aimem_handle"].(string)) {
		t.Fatal("the fenced handle is active")
	}
}

// Every refusal of the token endpoint carries the envelope and RFC 6749's
// members, a refused subject token being invalid_request (RFC 8693 §2.2.2),
// and nothing it refuses changes anything.
func TestTokenEndpointRefusals(t *testing.T) {
	// TestRateLimits covers the limit.
	e := setupAPI(t, func(s *Server) { s.tokenLimit = newLimiter(1000, time.Minute) })
	valid := e.entryForm(t)
	with := func(f func(url.Values)) url.Values {
		v := url.Values{}
		for k, vs := range valid {
			v[k] = append([]string(nil), vs...)
		}
		f(v)
		return v
	}
	cases := []struct {
		name   string
		form   url.Values
		status int
		code   string
		oauth  string
	}{
		{"wrong grant", with(func(v url.Values) { v.Set("grant_type", "client_credentials") }), 400, "unsupported_grant_type", "unsupported_grant_type"},
		{"repeated parameter", with(func(v url.Values) { v.Add("team_id", e.teamID) }), 400, "invalid_request", "invalid_request"},
		{"scope", with(func(v url.Values) { v.Set("scope", "all") }), 400, "invalid_scope", "invalid_scope"},
		{"resource", with(func(v url.Values) { v.Set("resource", "https://x") }), 400, "invalid_target", "invalid_target"},
		{"actor token", with(func(v url.Values) { v.Set("actor_token", "x") }), 400, "invalid_request", "invalid_request"},
		{"unknown subject type", with(func(v url.Values) { v.Set("subject_token_type", "urn:x") }), 400, "invalid_request", "invalid_request"},
		{"no subject token", with(func(v url.Values) { v.Del("subject_token") }), 400, "invalid_request", "invalid_request"},
		{"wrong audience", with(func(v url.Values) { v.Set("audience", "aicrew-other") }), 400, "invalid_target", "invalid_target"},
		{"no audience", with(func(v url.Values) { v.Del("audience") }), 400, "invalid_target", "invalid_target"},
		{"wrong requested type", with(func(v url.Values) { v.Set("requested_token_type", HandleTokenType) }), 400, "invalid_target", "invalid_target"},
		{"team and session", with(func(v url.Values) { v.Set("session_id", "s") }), 400, "invalid_request", "invalid_request"},
		{"neither team nor session", with(func(v url.Values) { v.Del("team_id") }), 400, "invalid_request", "invalid_request"},
		{"bad challenge id", with(func(v url.Values) { v.Set("challenge_id", "a b") }), 400, "invalid_request", "invalid_request"},
		{"unknown challenge", with(func(v url.Values) { v.Set("challenge_id", "missing") }), 400, "challenge_invalid", "invalid_request"},
		{"refused proof", with(func(v url.Values) { v.Set("subject_token", "amr1_"+strings.Repeat("z", 43)) }), 400, "proof_invalid", "invalid_request"},
		{"handle as receipt", with(func(v url.Values) { v.Set("subject_token", "acs1_"+strings.Repeat("z", 43)) }), 400, "proof_invalid", "invalid_request"},
		{"refresh with a handle", with(func(v url.Values) {
			v.Set("subject_token_type", AccessTokenType)
			v.Set("subject_token", "acs1_"+strings.Repeat("z", 43))
			v.Set("requested_token_type", HandleTokenType)
			v.Set("audience", "hub-test")
		}), 400, "invalid_token", "invalid_request"},
		{"another identity's proof", with(func(v url.Values) {
			other := "amr1_" + strings.Repeat("o", 43)
			e.v.mu.Lock()
			e.v.ids[other] = store.VerifiedIdentity{HubID: "hub-test", UserID: "user-other", TokenID: "tok-o"}
			e.v.mu.Unlock()
			v.Set("subject_token", other)
		}), 400, "identity_mismatch", "invalid_request"},
		{"refresh without the handle type", with(func(v url.Values) { v.Set("subject_token_type", AccessTokenType) }), 400, "invalid_target", "invalid_target"},
	}
	for i, c := range cases {
		got := e.exchange(t, "k-"+strings.Repeat("r", i+1), c.form)
		if got.status != c.status || got.body["code"] != c.code || got.body["error"] != c.oauth ||
			got.body["error_description"] == "" || got.body["correlation_id"] == "" {
			t.Errorf("%s: got %d %s, want %d %s/%s", c.name, got.status, got.raw, c.status, c.code, c.oauth)
		}
		if strings.Contains(got.raw, valid.Get("subject_token")) {
			t.Errorf("%s: the refusal echoes the receipt", c.name)
		}
	}
	// Transport-level refusals.
	body := valid.Encode()
	for name, r := range map[string]reply{
		"no key":       e.send(t, http.MethodPost, TokenPath, "application/x-www-form-urlencoded", "", "", body),
		"json body":    e.send(t, http.MethodPost, TokenPath, "application/json", "k", "", body),
		"query string": e.send(t, http.MethodPost, TokenPath+"?subject_token=x", "application/x-www-form-urlencoded", "k", "", body),
	} {
		if r.status != 400 || r.body["code"] != "invalid_request" || r.body["error"] != "invalid_request" {
			t.Errorf("%s: got %d %s", name, r.status, r.raw)
		}
	}
	// A body over the limit: declared on a socket with the headers alone,
	// and declared or undeclared through the handler chain (see direct).
	oversize := body + "&pad=" + strings.Repeat("a", maxTokenBody)
	code, raw := e.head(t, fmt.Sprintf("POST %s HTTP/1.1\r\nHost: x\r\nContent-Type: application/x-www-form-urlencoded\r\nIdempotency-Key: k\r\nContent-Length: %d\r\n\r\n",
		TokenPath, len(oversize)))
	oversized := map[string]reply{"declared, headers only": {status: code, raw: string(raw)}}
	for name, b := range map[string]io.Reader{
		"declared":   strings.NewReader(oversize),
		"undeclared": unsized{strings.NewReader(oversize)},
	} {
		req := httptest.NewRequest(http.MethodPost, TokenPath, b)
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.Header.Set("Idempotency-Key", "k")
		rec := e.direct(t, req)
		oversized[name] = reply{status: rec.Code, raw: rec.Body.String()}
	}
	for name, r := range oversized {
		_ = json.Unmarshal([]byte(r.raw), &r.body)
		if r.status != 400 || r.body["code"] != "invalid_request" || r.body["error"] != "invalid_request" {
			t.Errorf("oversize body, %s: got %d %s", name, r.status, r.raw)
		}
	}
	// The valid form still enters: nothing above consumed the challenge.
	if got := e.exchange(t, "finally", valid); got.status != http.StatusOK {
		t.Fatalf("entry after the refusals = %d %s", got.status, got.raw)
	}
}

// A refresh names the session's aimem hub as its audience.
func TestRefreshAudience(t *testing.T) {
	e := setupAPI(t)
	token := e.enter(t, "enter").body["access_token"].(string)
	f := refreshForm(token)
	f.Set("audience", "hub-other")
	got := e.exchange(t, "r", f)
	if got.status != 400 || got.body["code"] != "invalid_target" {
		t.Fatalf("refresh for another hub = %d %s", got.status, got.raw)
	}
}

// An unknown agent and an unlinked one get the same refusal.
func TestChallengeRoute(t *testing.T) {
	e := setupAPI(t)
	op, _ := store.OperatorCaller("op-test")
	unlinked, err := e.store.CreateAgent(context.Background(), op, "agent-2", store.NewAgent{Label: "unlinked"})
	if err != nil {
		t.Fatal(err)
	}
	a := e.challenge(t, "k1", "01a0ffff-0000-7000-8000-000000000000")
	b := e.challenge(t, "k2", unlinked.ID)
	refused(t, a, http.StatusForbidden, "identity_link_required")
	refused(t, b, http.StatusForbidden, "identity_link_required")
	strip := func(r reply) map[string]any { delete(r.body, "correlation_id"); return r.body }
	if ja, _ := json.Marshal(strip(a)); string(ja) != func() string { jb, _ := json.Marshal(strip(b)); return string(jb) }() {
		t.Fatalf("the refusals differ: %s / %s", a.raw, b.raw)
	}
	for name, r := range map[string]reply{
		"no key":        e.send(t, http.MethodPost, ChallengesPath, "application/json", "", "", `{"agent_id":"x"}`),
		"form":          e.send(t, http.MethodPost, ChallengesPath, "application/x-www-form-urlencoded", "k", "", "agent_id=x"),
		"unknown field": e.send(t, http.MethodPost, ChallengesPath, "application/json", "k", "", `{"agent_id":"x","team":"y"}`),
		"bad id":        e.send(t, http.MethodPost, ChallengesPath, "application/json", "k", "", `{"agent_id":"a b"}`),
	} {
		refused(t, r, http.StatusBadRequest, "invalid_request")
		_ = name
	}
}

// Without an aimem hub configured, entry is refused and nothing else breaks.
func TestEntryWithoutAimem(t *testing.T) {
	e := setupAPI(t, func(s *Server) { s.verifier = nil })
	form := e.entryForm(t)
	refused(t, e.exchange(t, "k", form), http.StatusServiceUnavailable, "aimem_unconfigured")
}

// D-2a over HTTP: an identical entry in flight is refused at once.
func TestInFlightDuplicateOverHTTP(t *testing.T) {
	e := setupAPI(t)
	form := e.entryForm(t)
	inside, gate := make(chan struct{}), make(chan struct{})
	e.v.mu.Lock()
	e.v.inside, e.v.gate = inside, gate
	e.v.mu.Unlock()
	first := make(chan reply, 1)
	go func() { first <- e.exchange(t, "same", form) }()
	<-inside
	e.v.mu.Lock()
	e.v.gate = nil
	e.v.mu.Unlock()
	dup := e.exchange(t, "same", form)
	close(gate)
	refused(t, dup, http.StatusServiceUnavailable, "request_in_progress")
	if dup.body["retryable"] != true || dup.body["error"] != "temporarily_unavailable" {
		t.Fatalf("duplicate = %s", dup.raw)
	}
	orig := <-first
	if orig.status != http.StatusOK {
		t.Fatalf("original = %d %s", orig.status, orig.raw)
	}
	if st := e.send(t, http.MethodGet, SessionPath, "", "", orig.body["access_token"].(string), ""); st.status != http.StatusOK {
		t.Fatalf("the original's token = %d %s", st.status, st.raw)
	}
}

func TestSessionRouteRefusals(t *testing.T) {
	e := setupAPI(t)
	token := e.enter(t, "enter").body["access_token"].(string)
	noAuth := e.send(t, http.MethodGet, SessionPath, "", "", "", "")
	refused(t, noAuth, http.StatusUnauthorized, "invalid_token")
	handle := "acs1_" + strings.Repeat("h", 43)
	refused(t, e.send(t, http.MethodGet, SessionPath, "", "", handle, ""), http.StatusUnauthorized, "invalid_token")
	refused(t, e.send(t, http.MethodPost, LeavePath, "application/json", "", token, ""), http.StatusBadRequest, "invalid_request")
	refused(t, e.send(t, http.MethodPost, LeavePath, "application/json", "k", token, `{"force":true}`), http.StatusBadRequest, "invalid_request")
	refused(t, e.send(t, http.MethodPost, LeavePath, "application/json", "k", "", ""), http.StatusUnauthorized, "invalid_token")
	if st := e.send(t, http.MethodGet, SessionPath, "", "", token, ""); st.status != http.StatusOK {
		t.Fatalf("the token after refused leaves = %d %s", st.status, st.raw)
	}
}

func TestRefusalCodeMapping(t *testing.T) {
	for err, want := range map[error]string{
		store.ErrWorkOutstanding:                               "work_outstanding",
		store.ErrContextStale:                                  "context_stale",
		store.ErrSessionActive:                                 "session_active",
		store.ErrCoordinatorActive:                             "coordinator_active",
		store.ErrForbidden:                                     "role_forbidden",
		store.ErrIdempotencyConflict:                           "idempotency_conflict",
		store.ErrInProgress:                                    "request_in_progress",
		store.ErrIdentityMismatch:                              "identity_mismatch",
		&verifier.Error{Code: "credential_inactive"}:           "credential_inactive",
		&verifier.Error{Code: "peer_unauthenticated"}:          "aimem_unconfigured",
		&verifier.Error{Code: "rate_limited", Retryable: true}: "identity_unavailable",
		io.ErrUnexpectedEOF:                                    "identity_unavailable",
	} {
		if got := refusalCode(err); got != want {
			t.Errorf("%v: %s, want %s", err, got, want)
		}
	}
	for code, r := range sessionRefusals {
		if r.retryable != (r.status == http.StatusServiceUnavailable || r.status == http.StatusTooManyRequests) && code != "aimem_unconfigured" {
			t.Errorf("%s: retryable %v with status %d", code, r.retryable, r.status)
		}
		if strings.Contains(strings.ToLower(r.nextAction), "personal") {
			t.Errorf("%s: the next action points to personal credentials", code)
		}
	}
}

// frozenClock holds the server's limiters at one instant, so that a burst is
// judged against the limits however long the runner takes over it; advance
// moves the instant on. The handlers read it while the test advances it.
type frozenClock struct{ ns atomic.Int64 }

func newFrozenClock() *frozenClock {
	c := &frozenClock{}
	c.ns.Store(time.Unix(1_000_000, 0).UnixNano())
	return c
}

func (c *frozenClock) now() time.Time          { return time.Unix(0, c.ns.Load()) }
func (c *frozenClock) advance(d time.Duration) { c.ns.Add(int64(d)) }

// drive sets the clock of s's limiters; it runs after New builds them and
// before the server serves.
func (c *frozenClock) drive(s *Server) {
	for _, l := range []*limiter{s.challengeLimit, s.tokenLimit, s.refreshLimit} {
		l.now = c.now
	}
}

// D-2e: the per-address and per-session limits.
func TestRateLimits(t *testing.T) {
	if ChallengesPerMinute != 10 || ExchangesPerMinute != 20 || RefreshesPerMinute != 6 {
		t.Fatal("the rate limits differ from D-2e's 10, 20 and 6 a minute")
	}
	clock := newFrozenClock()
	e := setupAPI(t, clock.drive)
	for i := 0; i < ChallengesPerMinute; i++ {
		if got := e.challenge(t, "k"+strings.Repeat("c", i+1), e.agentID); got.status != http.StatusOK {
			t.Fatalf("challenge %d: %d %s", i, got.status, got.raw)
		}
	}
	got := e.challenge(t, "over", e.agentID)
	refused(t, got, http.StatusTooManyRequests, "rate_limited")
	if got.header.Get("Retry-After") == "" {
		t.Fatal("no Retry-After")
	}
	// The limit is the frozen clock's: once it moves past one refill, a
	// challenge is admitted again.
	clock.advance(time.Minute / ChallengesPerMinute)
	if got := e.challenge(t, "after-refill", e.agentID); got.status != http.StatusOK {
		t.Fatalf("challenge after a refill: %d %s", got.status, got.raw)
	}

	e2 := setupAPI(t, newFrozenClock().drive)
	token := e2.enter(t, "enter").body["access_token"].(string)
	for i := 0; i < RefreshesPerMinute; i++ {
		if got := e2.exchange(t, "r"+strings.Repeat("r", i+1), refreshForm(token)); got.status != http.StatusOK {
			t.Fatalf("refresh %d: %d %s", i, got.status, got.raw)
		}
	}
	got = e2.exchange(t, "r-over", refreshForm(token))
	refused(t, got, http.StatusTooManyRequests, "rate_limited")
	if got.body["error"] != "temporarily_unavailable" {
		t.Fatalf("token endpoint 429 = %s", got.raw)
	}
	for i := 0; i < ExchangesPerMinute-RefreshesPerMinute-2; i++ {
		e2.exchange(t, "x"+strings.Repeat("x", i+1), url.Values{"grant_type": {"none"}})
	}
	refused(t, e2.exchange(t, "x-over", url.Values{"grant_type": {"none"}}), http.StatusTooManyRequests, "rate_limited")
}

func TestLimiterRefills(t *testing.T) {
	now := time.Unix(1_000_000, 0)
	l := newLimiter(2, time.Minute)
	l.now = func() time.Time { return now }
	for i := 0; i < 2; i++ {
		if ok, _ := l.allow("a"); !ok {
			t.Fatal("refused within the burst")
		}
	}
	ok, wait := l.allow("a")
	if ok || wait <= 0 || wait > 30*time.Second {
		t.Fatalf("over the burst: %v, wait %v", ok, wait)
	}
	if ok, _ := l.allow("b"); !ok {
		t.Fatal("another key shares the bucket")
	}
	now = now.Add(30 * time.Second)
	if ok, _ := l.allow("a"); !ok {
		t.Fatal("no refill after half a window")
	}
	if ok, _ := l.allow("a"); ok {
		t.Fatal("refilled more than the rate")
	}
}

// The routes agents and aimem reach call only the store operations that
// authenticate by proof, session token or peer credential: never one that
// trusts a caller it is given.
func TestExposureGuard(t *testing.T) {
	allowed := map[string]bool{
		"AuthenticateIntrospection": true, "Introspect": true,
		"AuthenticateCoordination": true, "CoordinationFact": true,
		"IssueAgentChallenge": true, "EnterSession": true, "ResumeSessionWithProof": true,
		"RefreshHandle": true, "LeaveWithToken": true, "AuthenticateSessionToken": true,
		"BeginOfferWithToken": true, "BeginAcceptWithToken": true, "DeclineWithToken": true,
		"BeginWithdrawWithToken": true, "SettleWithToken": true, "BeginClaimWithToken": true,
		"RequestStopWithToken": true, "ConfirmStopWithToken": true, "BeginStopReleaseWithToken": true,
		"ReviewWithToken": true, "ConfirmDeliveryWithToken": true, "BeginFinalizeWithToken": true,
		"BeginWorkWithToken": true, "SupersedeWorkWithToken": true,
		// The member's inbox, as the token's session (pilot G1).
		"ReadInboxWithToken": true, "AckWithToken": true,
		// Invitation redemption authenticates by the invitation code, which
		// the store checks inside every command (1a81-3).
		"BeginRedemption": true, "CompleteRedemption": true,
	}
	forbiddenPkg := map[string]bool{"AgentCaller": true, "OperatorCaller": true}
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	checked := 0
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		src, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		f, err := parser.ParseFile(fset, name, src, 0)
		if err != nil {
			t.Fatal(err)
		}
		ast.Inspect(f, func(n ast.Node) bool {
			sel, ok := n.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			if inner, ok := sel.X.(*ast.SelectorExpr); ok && inner.Sel.Name == "store" {
				checked++
				if !allowed[sel.Sel.Name] {
					t.Errorf("%s: the service calls store.%s", fset.Position(sel.Pos()), sel.Sel.Name)
				}
			}
			if pkg, ok := sel.X.(*ast.Ident); ok && pkg.Name == "store" && forbiddenPkg[sel.Sel.Name] {
				t.Errorf("%s: the service constructs store.%s", fset.Position(sel.Pos()), sel.Sel.Name)
			}
			return true
		})
	}
	if checked < len(allowed) {
		t.Fatalf("found only %d store calls; the guard is not looking at the service", checked)
	}
}

func TestRouteInventory(t *testing.T) {
	r := start(t, nil)
	var got []string
	for path, methods := range r.srv.routes {
		for m := range methods {
			got = append(got, m+" "+path)
		}
	}
	sort.Strings(got)
	want := []string{"GET /healthz", "GET /v1/crew/inbox", "GET /v1/crew/session", "POST /v1/crew/attempts", "POST /v1/crew/attempts/claim",
		"POST /v1/crew/attempts/{id}/accept", "POST /v1/crew/attempts/{id}/confirm-delivery",
		"POST /v1/crew/attempts/{id}/confirm-stop", "POST /v1/crew/attempts/{id}/decline", "POST /v1/crew/attempts/{id}/finalize",
		"POST /v1/crew/attempts/{id}/release", "POST /v1/crew/attempts/{id}/review", "POST /v1/crew/attempts/{id}/settle",
		"POST /v1/crew/attempts/{id}/stop", "POST /v1/crew/attempts/{id}/withdraw", "POST /v1/crew/attempts/{id}/work", "POST /v1/crew/challenges", "POST /v1/crew/coordination",
		"POST /v1/crew/inbox/ack", "POST /v1/crew/introspect", "POST /v1/crew/invitations/begin", "POST /v1/crew/invitations/complete",
		"POST /v1/crew/session/leave", "POST /v1/crew/token"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("routes = %v, want %v", got, want)
	}
}

// The service does not start with an aimem section whose bearer file is
// missing or readable by others, and says so without the content.
func TestNewChecksRedemptionCredential(t *testing.T) {
	certFile, keyFile, _ := testCert(t)
	st, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "aicrew.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	tokenFile := filepath.Join(t.TempDir(), "redemption.token")
	cfg := Config{ListenAddr: "127.0.0.1:0", TLSCertFile: certFile, TLSKeyFile: keyFile, ServiceID: "aicrew-test",
		ShutdownTimeout: Duration(time.Second), Aimem: &AimemConfig{BaseURL: "https://hub.example",
			TLSTrustMode: "ca_dns", TLSTrustValue: "hub.example", RedemptionTokenFile: tokenFile}}
	log := slogDiscard()
	if _, err := New(cfg, st, log); err == nil || !strings.Contains(err.Error(), "redemption_token_file") {
		t.Fatalf("missing bearer file: %v", err)
	}
	if err := os.WriteFile(tokenFile, []byte("secret-bearer\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := privatefiletest.Expose(tokenFile); err != nil {
		t.Fatal(err)
	}
	_, err = New(cfg, st, log)
	if err == nil || strings.Contains(err.Error(), "secret-bearer") {
		t.Fatalf("exposed bearer file: %v", err)
	}
	os.Remove(tokenFile)
	pf, err := privatefile.Create(tokenFile)
	if err != nil {
		t.Fatal(err)
	}
	pf.WriteString("secret-bearer\n")
	pf.Close()
	srv, err := New(cfg, st, log)
	if err != nil || srv.verifier == nil {
		t.Fatalf("private bearer file: %v", err)
	}
}

func slogDiscard() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// The refusals made before a session handler runs, a method the path does
// not serve and a declared body over the service's limit, carry the
// envelope; on the token endpoint also RFC 6749's members. Other paths keep
// the bare code.
func TestSessionSharedRefusals(t *testing.T) {
	e := setupAPI(t)
	tok := e.send(t, http.MethodGet, TokenPath, "", "", "", "")
	refused(t, tok, http.StatusMethodNotAllowed, "method_not_allowed")
	if tok.body["error"] != "invalid_request" || tok.body["error_description"] == "" || tok.header.Get("Allow") != http.MethodPost {
		t.Fatalf("GET on the token endpoint: %s, Allow %q", tok.raw, tok.header.Get("Allow"))
	}
	leave := e.send(t, http.MethodPut, LeavePath, "", "", "", "")
	refused(t, leave, http.StatusMethodNotAllowed, "method_not_allowed")
	if _, ok := leave.body["error"]; ok || leave.header.Get("Allow") != http.MethodPost {
		t.Fatalf("PUT on leave: %s, Allow %q", leave.raw, leave.header.Get("Allow"))
	}
	// Declared only: the refusal comes from the length, and no body is sent.
	code, raw := e.head(t, fmt.Sprintf("GET %s HTTP/1.1\r\nHost: x\r\nContent-Length: %d\r\n\r\n", SessionPath, MaxBodyBytes+1))
	big := reply{status: code, raw: string(raw)}
	_ = json.Unmarshal(raw, &big.body)
	refused(t, big, http.StatusRequestEntityTooLarge, "request_too_large")
	if _, ok := big.body["error"]; ok {
		t.Fatalf("an oversized body on the session route carries RFC 6749 members: %s", big.raw)
	}
	health := e.send(t, http.MethodPost, "/healthz", "", "", "", "")
	if health.status != http.StatusMethodNotAllowed || strings.TrimSpace(health.raw) != `{"code":"method_not_allowed"}` {
		t.Fatalf("POST /healthz = %d %s; want the bare 405 code", health.status, health.raw)
	}
}
