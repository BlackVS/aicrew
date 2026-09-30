package server

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/BlackVS/aicrew/internal/store"
)

// Invitation redemption over HTTPS (1a81-3).

type redeemEnv struct {
	*running
	v      *fakeVerifier
	op     store.Caller
	teamID string
}

func setupRedeem(t *testing.T, withVerifier bool) *redeemEnv {
	t.Helper()
	v := &fakeVerifier{ids: map[string]store.VerifiedIdentity{}}
	r := start(t, func(s *Server) {
		if withVerifier {
			s.verifier = v
		}
	})
	op, err := store.OperatorCaller("op-test")
	if err != nil {
		t.Fatal(err)
	}
	team, err := r.store.CreateTeam(context.Background(), op, "team", store.NewTeam{Name: "crew"})
	if err != nil {
		t.Fatal(err)
	}
	return &redeemEnv{running: r, v: v, op: op, teamID: team.ID}
}

// identity makes the fake aimem vouch for receipt as user.
func (e *redeemEnv) identity(receipt, user string) string {
	e.v.mu.Lock()
	e.v.ids[receipt] = store.VerifiedIdentity{HubID: "hub-test", UserID: user, TokenID: "tok-" + user}
	e.v.mu.Unlock()
	return receipt
}

// invite issues an invitation and returns its code.
func (e *redeemEnv) invite(t *testing.T, key string, req store.InvitationRequest) (store.Invitation, string) {
	t.Helper()
	code, err := store.GenerateInvitationCode()
	if err != nil {
		t.Fatal(err)
	}
	if req.TeamID == "" {
		req.TeamID = e.teamID
	}
	req.HubID = "hub-test"
	inv, err := e.store.IssueInvitation(context.Background(), e.op, key, req, code)
	if err != nil {
		t.Fatal(err)
	}
	return inv, code.Reveal()
}

func (e *redeemEnv) join(t *testing.T, key string) (store.Invitation, string) {
	t.Helper()
	return e.invite(t, key, store.InvitationRequest{Purpose: store.PurposeJoin, Role: store.RoleWorker, Label: "newcomer"})
}

func (e *redeemEnv) post(t *testing.T, path, key string, body any) reply {
	t.Helper()
	b, _ := json.Marshal(body)
	return (&apiEnv{running: e.running}).send(t, http.MethodPost, path, "application/json", key, "", string(b))
}

func (e *redeemEnv) begin(t *testing.T, key, code string) reply {
	t.Helper()
	return e.post(t, InvitationBeginPath, key, map[string]string{"code": code})
}

func (e *redeemEnv) complete(t *testing.T, key, code, challengeID, receipt string) reply {
	t.Helper()
	return e.post(t, InvitationCompletePath, key, map[string]string{"code": code, "challenge_id": challengeID, "receipt": receipt})
}

// noSecrets fails when any of secrets appears in text.
func noSecrets(t *testing.T, where, text string, secrets ...string) {
	t.Helper()
	for _, s := range secrets {
		if s != "" && (strings.Contains(text, s) || strings.Contains(text, strings.ReplaceAll(s, "-", ""))) {
			t.Fatalf("%s carries a secret: %s", where, text)
		}
	}
}

// The whole redemption: begin, a lost begin reply retried with the same key,
// completion, a lost completion reply retried with the same key, and the same
// answer after a restart between the commit and the reply. The code and the
// receipt appear in no reply and no log line.
func TestInvitationRedemptionOverHTTPS(t *testing.T) {
	e := setupRedeem(t, true)
	_, code := e.join(t, "inv-1")
	receipt := e.identity("amr1_"+strings.Repeat("R", 43), "user-7")

	ch := e.begin(t, "b1", code)
	if ch.status != http.StatusOK || ch.body["hub_id"] != "hub-test" || ch.body["service_id"] != "aicrew-test" ||
		ch.body["challenge_id"] == "" || ch.body["expires_at"] == "" {
		t.Fatalf("begin: %d %s", ch.status, ch.raw)
	}
	challengeID := ch.body["challenge_id"].(string)
	if again := e.begin(t, "b1", code); again.status != http.StatusOK || again.body["challenge_id"] != challengeID {
		t.Fatalf("begin retried with the same key: %d %s", again.status, again.raw)
	}

	done := e.complete(t, "c1", code, challengeID, receipt)
	if done.status != http.StatusOK || done.body["team_id"] != e.teamID || done.body["role"] != "worker" ||
		done.body["created"] != true || done.body["user_id"] != "user-7" || done.body["hub_id"] != "hub-test" || done.body["agent_id"] == "" {
		t.Fatalf("complete: %d %s", done.status, done.raw)
	}
	if again := e.complete(t, "c1", code, challengeID, receipt); again.status != http.StatusOK || again.raw != done.raw {
		t.Fatalf("complete retried with the same key: %d %s", again.status, again.raw)
	}
	// Redeemed: a completion under a new key is refused without disclosure.
	refused(t, e.complete(t, "c2", code, challengeID, receipt), http.StatusForbidden, "invitation_invalid")

	// A restart between the commit and the reply: the retry is answered from
	// the recorded result, and aimem is not asked again.
	restarted := e.restart(t)
	restarted.v.mu.Lock()
	restarted.v.ids = map[string]store.VerifiedIdentity{} // aimem now vouches for nothing
	restarted.v.mu.Unlock()
	if again := restarted.complete(t, "c1", code, challengeID, receipt); again.status != http.StatusOK || again.raw != done.raw {
		t.Fatalf("complete retried after a restart: %d %s", again.status, again.raw)
	}
	for _, text := range []string{ch.raw, done.raw, e.logs.String(), restarted.logs.String()} {
		noSecrets(t, "a reply or the log", text, code, receipt)
	}
}

// restart stops the service and serves the same store file again, with the
// same fake aimem.
func (e *redeemEnv) restart(t *testing.T) *redeemEnv {
	t.Helper()
	e.cancel()
	err := <-e.done
	e.done <- err // the first service's cleanup waits on it too
	e.store.Close()
	st, err := store.Open(context.Background(), e.storePath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	logs := &syncBuffer{}
	srv, err := New(e.srv.cfg, st, slog.New(slog.NewJSONHandler(logs, nil)), WithVerifier(e.v))
	if err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	r := &running{srv: srv, addr: ln.Addr().String(), logs: logs, pool: e.pool, cancel: cancel, done: make(chan error, 1),
		store: st, storePath: e.storePath, client: e.client}
	go func() { r.done <- srv.Serve(ctx, ln) }()
	t.Cleanup(func() {
		cancel()
		<-r.done
	})
	return &redeemEnv{running: r, v: e.v, op: e.op, teamID: e.teamID}
}

// Every refusal carries the envelope and no request secret; the binding
// refusals are the contract's.
func TestInvitationRedemptionRefusals(t *testing.T) {
	e := setupRedeem(t, true)
	inv, code := e.join(t, "inv-1")
	api := &apiEnv{running: e.running}
	// Malformed requests.
	refused(t, api.send(t, http.MethodPost, InvitationBeginPath, "application/json", "", "", `{"code":"x"}`), http.StatusBadRequest, "invalid_request")
	refused(t, api.send(t, http.MethodPost, InvitationBeginPath, "text/plain", "k", "", `{"code":"x"}`), http.StatusBadRequest, "invalid_request")
	refused(t, api.send(t, http.MethodPost, InvitationBeginPath, "application/json", "k", "", `{"code":"x","extra":1}`), http.StatusBadRequest, "invalid_request")
	refused(t, api.send(t, http.MethodPost, InvitationBeginPath+"?code="+code, "application/json", "k", "", `{"code":"x"}`), http.StatusBadRequest, "invalid_request")
	refused(t, api.send(t, http.MethodPost, InvitationCompletePath, "application/json", "k", "", `{"code":"x"}`), http.StatusBadRequest, "invalid_request")
	// Refusals made before the handler runs carry the envelope too, and are
	// counted like every other refusal on these routes: a method the path
	// does not serve, and a declared body over the service's limit.
	for i, path := range []string{InvitationBeginPath, InvitationCompletePath} {
		got := api.send(t, http.MethodGet, path, "", "", "", "")
		refused(t, got, http.StatusMethodNotAllowed, "method_not_allowed")
		if got.header.Get("Allow") != http.MethodPost {
			t.Fatalf("GET %s: Allow %q", path, got.header.Get("Allow"))
		}
		if n := lastCount(t, e.logs.String(), "method_not_allowed"); n != i+1 {
			t.Fatalf("GET %s: method_not_allowed counted %d, want %d", path, n, i+1)
		}
		status, raw := e.head(t, fmt.Sprintf("GET %s HTTP/1.1\r\nHost: x\r\nContent-Length: %d\r\n\r\n", path, MaxBodyBytes+1))
		big := reply{status: status, raw: string(raw)}
		_ = json.Unmarshal(raw, &big.body)
		refused(t, big, http.StatusRequestEntityTooLarge, "request_too_large")
		if n := lastCount(t, e.logs.String(), "request_too_large"); n != i+1 {
			t.Fatalf("an oversized body on %s: request_too_large counted %d, want %d", path, n, i+1)
		}
	}
	// A code that is garbage, or one never issued, is not valid.
	for _, bad := range []string{"not-a-code", "0000-0000-0000-0000-0000-0000-0000"} {
		refused(t, e.begin(t, "k-"+bad, bad), http.StatusForbidden, "invitation_invalid")
	}
	ch := e.begin(t, "b1", code)
	challengeID := ch.body["challenge_id"].(string)
	// aimem does not vouch for the receipt: nothing is linked.
	unknown := "amr1_" + strings.Repeat("U", 43)
	refused(t, e.complete(t, "c-unknown", code, challengeID, unknown), http.StatusBadRequest, "proof_invalid")
	// A revoked invitation is not valid, and the refusal does not say why.
	cur, err := e.store.GetInvitation(context.Background(), inv.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.store.RevokeInvitation(context.Background(), e.op, "rev", inv.ID, cur.Revision); err != nil {
		t.Fatal(err)
	}
	receipt := e.identity("amr1_"+strings.Repeat("R", 43), "user-7")
	gone := e.complete(t, "c1", code, challengeID, receipt)
	refused(t, gone, http.StatusForbidden, "invitation_invalid")
	refused(t, e.begin(t, "b2", code), http.StatusForbidden, "invitation_invalid")
	for _, text := range []string{gone.raw, ch.raw, e.logs.String()} {
		noSecrets(t, "a refusal or the log", text, code, receipt, unknown)
	}

	// One aimem user, one agent: linking a second agent to a linked user.
	ctx := context.Background()
	linked, err := e.store.CreateAgent(ctx, e.op, "a-linked", store.NewAgent{Label: "linked"})
	if err != nil {
		t.Fatal(err)
	}
	other, err := e.store.CreateAgent(ctx, e.op, "a-other", store.NewAgent{Label: "other"})
	if err != nil {
		t.Fatal(err)
	}
	e.seedLink(t, linked.ID, "user-8")
	_, linkCode := e.invite(t, "inv-link", store.InvitationRequest{Purpose: store.PurposeLink, Role: store.RoleWorker, AgentID: other.ID})
	lc := e.begin(t, "bl", linkCode)
	refused(t, e.complete(t, "cl", linkCode, lc.body["challenge_id"].(string), e.identity("amr1_"+strings.Repeat("L", 43), "user-8")),
		http.StatusConflict, "identity_already_linked")

	// An agent already in the team in another role.
	if _, err := e.store.AddMember(ctx, e.op, "m", e.teamID, linked.ID, store.RoleWorker); err != nil {
		t.Fatal(err)
	}
	_, coordCode := e.invite(t, "inv-coord", store.InvitationRequest{Purpose: store.PurposeJoin, Role: store.RoleCoordinator, Label: "x"})
	cc := e.begin(t, "bc", coordCode)
	refused(t, e.complete(t, "cc", coordCode, cc.body["challenge_id"].(string), e.identity("amr1_"+strings.Repeat("C", 43), "user-8")),
		http.StatusConflict, "role_conflict")
}

// lastCount is the running total the log last recorded for a refusal code
// on the redemption routes, or 0.
func lastCount(t *testing.T, logs, code string) int {
	t.Helper()
	n := 0
	for _, line := range strings.Split(logs, "\n") {
		if !strings.Contains(line, `"invitation redemption refused"`) {
			continue
		}
		var rec map[string]any
		if json.Unmarshal([]byte(line), &rec) != nil {
			t.Fatalf("log line: %s", line)
		}
		if rec["code"] == code {
			n = int(rec["count"].(float64))
		}
	}
	return n
}

func (e *redeemEnv) seedLink(t *testing.T, agentID, user string) {
	t.Helper()
	db, err := sql.Open("sqlite", e.storePath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`UPDATE agents SET linked_hub_id = 'hub-test', linked_user_id = ?, linked_token_id = ? WHERE id = ?`,
		user, "tok-"+user, agentID); err != nil {
		t.Fatal(err)
	}
}

// Without a configured aimem, completion refuses before anything is read.
func TestInvitationCompletionWithoutAimem(t *testing.T) {
	e := setupRedeem(t, false)
	_, code := e.join(t, "inv-1")
	ch := e.begin(t, "b1", code)
	refused(t, e.complete(t, "c1", code, ch.body["challenge_id"].(string), "amr1_"+strings.Repeat("R", 43)),
		http.StatusServiceUnavailable, "aimem_unconfigured")
}

// Two completions of one invitation under different keys: exactly one wins.
func TestInvitationCompletionRaceOneWinner(t *testing.T) {
	e := setupRedeem(t, true)
	_, code := e.join(t, "inv-1")
	ch := e.begin(t, "b1", code)
	challengeID := ch.body["challenge_id"].(string)
	receipts := []string{e.identity("amr1_"+strings.Repeat("A", 43), "user-7"), e.identity("amr1_"+strings.Repeat("B", 43), "user-7")}
	var wg sync.WaitGroup
	got := make([]reply, 2)
	for i := range got {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			got[i] = e.complete(t, "race-"+string(rune('a'+i)), code, challengeID, receipts[i])
		}(i)
	}
	wg.Wait()
	wins := 0
	for _, g := range got {
		switch {
		case g.status == http.StatusOK:
			wins++
		case g.body["code"] != "invitation_invalid" && g.body["code"] != "challenge_invalid":
			t.Fatalf("the loser: %d %s", g.status, g.raw)
		}
	}
	if wins != 1 {
		t.Fatalf("%d completions won: %v", wins, got)
	}
}

// The per-address limits, and the refusal counter in the log: a running
// total per code, never a code, key or digest.
func TestInvitationRedemptionLimitsAndCounter(t *testing.T) {
	if BeginsPerMinute != 10 || CompletionsPerMinute != 20 {
		t.Fatal("the redemption limits differ from 10 and 20 a minute")
	}
	e := setupRedeem(t, true)
	_, code := e.join(t, "inv-1")
	bad := "ZZZZ-ZZZZ-ZZZZ-ZZZZ-ZZZZ-ZZZZ-ZZZZ"
	for i := 0; i < BeginsPerMinute; i++ {
		refused(t, e.begin(t, "flood-"+strings.Repeat("f", i+1), bad), http.StatusForbidden, "invitation_invalid")
	}
	over := e.begin(t, "flood-over", code)
	refused(t, over, http.StatusTooManyRequests, "rate_limited")
	if over.header.Get("Retry-After") == "" {
		t.Fatal("no Retry-After")
	}
	for i := 0; i < CompletionsPerMinute; i++ {
		e.complete(t, "cf-"+strings.Repeat("c", i+1), bad, "ch-x", "amr1_"+strings.Repeat("X", 43))
	}
	refused(t, e.complete(t, "cf-over", bad, "ch-x", "amr1_"+strings.Repeat("X", 43)), http.StatusTooManyRequests, "rate_limited")

	logs := e.logs.String()
	var last map[string]any
	counted := 0
	for _, line := range strings.Split(logs, "\n") {
		if !strings.Contains(line, `"invitation redemption refused"`) {
			continue
		}
		var rec map[string]any
		if json.Unmarshal([]byte(line), &rec) != nil {
			t.Fatalf("log line: %s", line)
		}
		if rec["code"] == "invitation_invalid" {
			counted++
			if rec["count"] != float64(counted) {
				t.Fatalf("running total %v, want %d: %s", rec["count"], counted, line)
			}
		}
		last = rec
	}
	if counted != BeginsPerMinute+CompletionsPerMinute || last["code"] != "rate_limited" {
		t.Fatalf("counted %d invitation_invalid refusals; last %v", counted, last)
	}
	noSecrets(t, "the log", logs, code, bad)
}
