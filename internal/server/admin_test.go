package server

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/BlackVS/aicrew/internal/opapi"
	"github.com/BlackVS/aicrew/internal/optoken"
	"github.com/BlackVS/aicrew/internal/store"
)

// admin sends one operator request with the given bearer and decodes the
// answer into out when out is set.
func (e *apiEnv) admin(t *testing.T, method, path, token string, body any, out any) reply {
	t.Helper()
	b := ""
	ct := ""
	if body != nil {
		raw, _ := json.Marshal(body)
		b, ct = string(raw), "application/json"
	}
	got := e.send(t, method, path, ct, "", token, b)
	if out != nil && got.status < 300 {
		if err := json.Unmarshal([]byte(got.raw), out); err != nil {
			t.Fatalf("%s %s: %v in %s", method, path, err, got.raw)
		}
	}
	return got
}

func adminRefused(t *testing.T, got reply, status int, code string) {
	t.Helper()
	if got.status != status || got.body["code"] != code {
		t.Fatalf("got %d %s, want %d %s", got.status, got.raw, status, code)
	}
}

// adminRoutes are every registered operator route.
func adminRoutes(s *Server) [][2]string {
	var out [][2]string
	for path, methods := range s.routes {
		if !strings.HasPrefix(path, opapi.Prefix) {
			continue
		}
		for m := range methods {
			out = append(out, [2]string{m, path})
		}
	}
	return out
}

// Every operator route refuses a request without the operator credential:
// none, a wrong one, a member's session token and aimem's introspection
// bearer. Nothing is changed.
func TestAdminRefusesEveryOtherCredential(t *testing.T) {
	e := setupAPI(t)
	session := e.enter(t, "enter").body["access_token"].(string)
	op, _ := store.OperatorCaller("op-test")
	_, intro, err := e.store.IssueIntrospectionCredential(context.Background(), op, "intro", "hub-test")
	if err != nil {
		t.Fatal(err)
	}
	wrong, _ := optoken.Generate()
	routes := adminRoutes(e.srv)
	// 13, the four credential routes under their names before 0.3.0, the
	// three escalation routes and the three architect-credential routes.
	if len(routes) != 23 {
		t.Fatalf("%d operator routes, want 23", len(routes))
	}
	before, _ := e.store.ListTeams(context.Background())
	for _, cred := range []struct{ name, token string }{{"none", ""}, {"wrong", wrong},
		{"member session", session}, {"introspection", intro}} {
		for _, rt := range routes {
			got := e.send(t, rt[0], rt[1], "application/json", "", cred.token, `{"name":"x"}`)
			if got.status != http.StatusUnauthorized || got.body["code"] != opapi.CodeUnauthorized {
				t.Fatalf("%s on %s %s: %d %s", cred.name, rt[0], rt[1], got.status, got.raw)
			}
			// The per-address budget is spent quickly; start each one afresh.
			e.srv.admin.failures = newLimiter(AdminFailuresPerMinute, 1)
		}
	}
	if after, _ := e.store.ListTeams(context.Background()); len(after) != len(before) {
		t.Fatal("a refused request changed the store")
	}
	logs := e.logs.String()
	if !strings.Contains(logs, `"outcome":"unauthorized"`) {
		t.Fatalf("no unauthorized outcome in the log:\n%s", logs)
	}
	for _, secret := range []string{wrong, session, intro} {
		if strings.Contains(logs, secret) {
			t.Fatalf("the log holds a refused bearer %q", secret)
		}
	}
	// The member's session still works.
	if got := e.send(t, http.MethodGet, SessionPath, "", "", session, ""); got.status != http.StatusOK {
		t.Fatalf("session after the refusals: %d %s", got.status, got.raw)
	}
}

// The operator administers teams, credentials and invitations while a
// member's session stays alive across the calls. Secrets are answered once:
// a list never carries a bearer or a code.
func TestAdminOperations(t *testing.T) {
	e := setupAPI(t)
	tok := e.opToken
	session := e.enter(t, "enter").body["access_token"].(string)
	alive := func(step string) {
		t.Helper()
		if got := e.send(t, http.MethodGet, SessionPath, "", "", session, ""); got.status != http.StatusOK {
			t.Fatalf("member session after %s: %d %s", step, got.status, got.raw)
		}
	}

	// Teams.
	var team opapi.Team
	if got := e.admin(t, http.MethodPost, opapi.TeamsPath, tok,
		opapi.TeamRequest{Name: "pilot"}, &team); got.status != http.StatusCreated || team.ID == "" || team.Grants == nil || len(team.Grants) != 0 {
		t.Fatalf("team create: %d %s", got.status, got.raw)
	}
	adminRefused(t, e.admin(t, http.MethodPost, opapi.TeamsPath, tok, opapi.TeamRequest{Name: "pilot"}, nil),
		http.StatusConflict, opapi.CodeTeamExists)
	adminRefused(t, e.admin(t, http.MethodPost, opapi.TeamsPath, tok, opapi.TeamRequest{Name: "Bad Name"}, nil),
		http.StatusBadRequest, opapi.CodeInvalid)
	var teams []opapi.TeamSummary
	if got := e.admin(t, http.MethodGet, opapi.TeamsPath, tok, nil, &teams); got.status != http.StatusOK || len(teams) != 2 {
		t.Fatalf("team list: %d %s", got.status, got.raw)
	}
	var detail opapi.TeamDetail
	if got := e.admin(t, http.MethodGet, opapi.TeamPath+"?id="+e.teamID, tok, nil, &detail); got.status != http.StatusOK ||
		len(detail.Members) != 1 || detail.Members[0].AgentID != e.agentID {
		t.Fatalf("team show: %d %s", got.status, got.raw)
	}
	adminRefused(t, e.admin(t, http.MethodGet, opapi.TeamPath+"?id=nope", tok, nil, nil), http.StatusNotFound, opapi.CodeNotFound)
	// The project list of earlier releases is gone: the hub's grants are
	// the team's projects.
	if got := e.send(t, http.MethodPost, opapi.TeamsPath, "application/json", "", tok,
		`{"name":"listed","projects":[{"hub_id":"hub-test","project_id":"aicrew"}]}`); got.status != http.StatusBadRequest {
		t.Fatalf("a team with a project list: %d %s", got.status, got.raw)
	}
	adminRefused(t, e.admin(t, http.MethodPost, opapi.TeamRenamePath, tok,
		opapi.TeamRenameRequest{TeamID: team.ID, ExpectedRevision: team.Revision - 1, Name: "second-name"}, nil),
		http.StatusConflict, opapi.CodeRevisionConflict)
	adminRefused(t, e.admin(t, http.MethodPost, opapi.TeamRenamePath, tok,
		opapi.TeamRenameRequest{TeamID: team.ID, ExpectedRevision: team.Revision, Name: "crew"}, nil),
		http.StatusConflict, opapi.CodeTeamExists)
	if got := e.admin(t, http.MethodPost, opapi.TeamRenamePath, tok,
		opapi.TeamRenameRequest{TeamID: team.ID, ExpectedRevision: team.Revision, Name: "second-name"}, &team); got.status != http.StatusOK || team.Name != "second-name" {
		t.Fatalf("team rename: %d %s", got.status, got.raw)
	}
	alive("the team operations")

	// Introspection credentials: the bearer is answered once and works.
	var cred opapi.Credential
	if got := e.admin(t, http.MethodPost, opapi.CredentialsPath, tok, opapi.CredentialRequest{HubID: "hub-x"}, &cred); got.status != http.StatusCreated ||
		cred.Bearer == "" || !cred.Active || len(cred.Operations) != 2 {
		t.Fatalf("credential issue: %d %s", got.status, got.raw)
	}
	if hub, err := e.store.AuthenticateIntrospection(context.Background(), cred.Bearer); err != nil || hub != "hub-x" {
		t.Fatalf("the issued bearer authenticates as %q, %v", hub, err)
	}
	adminRefused(t, e.admin(t, http.MethodPost, opapi.CredentialsPath, tok,
		opapi.CredentialRequest{HubID: "hub-x", Operations: []string{"everything"}}, nil), http.StatusBadRequest, opapi.CodeInvalid)
	var rotated opapi.Credential
	if got := e.admin(t, http.MethodPost, opapi.CredentialRotatePath, tok,
		opapi.CredentialRequest{HubID: "hub-x", Operations: []string{"introspection"}}, &rotated); got.status != http.StatusCreated ||
		rotated.Bearer == "" || rotated.Replaces != cred.ID || len(rotated.Operations) != 1 {
		t.Fatalf("credential rotate: %d %s", got.status, got.raw)
	}
	adminRefused(t, e.admin(t, http.MethodPost, opapi.CredentialRotatePath, tok, opapi.CredentialRequest{HubID: "hub-x"}, nil),
		http.StatusConflict, opapi.CodeRotateNeedsOne)
	var creds []opapi.Credential
	got := e.admin(t, http.MethodGet, opapi.CredentialsPath+"?hub=hub-x", tok, nil, &creds)
	if got.status != http.StatusOK || len(creds) != 2 || strings.Contains(got.raw, cred.Bearer) || strings.Contains(got.raw, `"bearer"`) {
		t.Fatalf("credential list: %d %s", got.status, got.raw)
	}
	var revoked opapi.Credential
	if got := e.admin(t, http.MethodPost, opapi.CredentialRevokePath, tok, opapi.IDRequest{ID: cred.ID}, &revoked); got.status != http.StatusOK ||
		revoked.Active || revoked.RevokedAt.IsZero() {
		t.Fatalf("credential revoke: %d %s", got.status, got.raw)
	}
	alive("the credential operations")

	// The routes' names before 0.3.0 serve the same operations for one release.
	var legacy opapi.Credential
	if got := e.admin(t, http.MethodPost, opapi.LegacyCredentialsPath, tok, opapi.CredentialRequest{HubID: "hub-l"}, &legacy); got.status != http.StatusCreated ||
		legacy.Bearer == "" || !legacy.Active {
		t.Fatalf("legacy credential issue: %d %s", got.status, got.raw)
	}
	var legacyRotated opapi.Credential
	if got := e.admin(t, http.MethodPost, opapi.LegacyCredentialRotatePath, tok, opapi.CredentialRequest{HubID: "hub-l"}, &legacyRotated); got.status != http.StatusCreated ||
		legacyRotated.Replaces != legacy.ID {
		t.Fatalf("legacy credential rotate: %d %s", got.status, got.raw)
	}
	var legacyList []opapi.Credential
	if got := e.admin(t, http.MethodGet, opapi.LegacyCredentialsPath+"?hub=hub-l", tok, nil, &legacyList); got.status != http.StatusOK || len(legacyList) != 2 {
		t.Fatalf("legacy credential list: %d %s", got.status, got.raw)
	}
	if got := e.admin(t, http.MethodPost, opapi.LegacyCredentialRevokePath, tok, opapi.IDRequest{ID: legacy.ID}, &revoked); got.status != http.StatusOK || revoked.Active {
		t.Fatalf("legacy credential revoke: %d %s", got.status, got.raw)
	}
	alive("the legacy credential routes")

	// Invitations: the code is answered once and redeemable.
	var inv opapi.Invitation
	if got := e.admin(t, http.MethodPost, opapi.InvitationsPath, tok, opapi.InvitationRequest{Purpose: "join",
		TeamID: e.teamID, Role: "worker", HubID: "hub-test", Label: "second", TTL: "2h"}, &inv); got.status != http.StatusCreated ||
		inv.Code == "" || inv.State != "issued" || inv.IssuedBy != operatorCallerID {
		t.Fatalf("invitation issue: %d %s", got.status, got.raw)
	}
	if _, err := e.store.BeginRedemption(context.Background(), "redeem-check", store.NewSecret(inv.Code)); err != nil {
		t.Fatalf("the answered code does not redeem: %v", err)
	}
	adminRefused(t, e.admin(t, http.MethodPost, opapi.InvitationsPath, tok, opapi.InvitationRequest{Purpose: "join",
		TeamID: e.teamID, Role: "worker", HubID: "hub-test", Label: "third", TTL: "soon"}, nil), http.StatusBadRequest, opapi.CodeInvalid)
	var invs []opapi.Invitation
	got = e.admin(t, http.MethodGet, opapi.InvitationsPath+"?team="+e.teamID, tok, nil, &invs)
	if got.status != http.StatusOK || len(invs) != 1 || strings.Contains(got.raw, inv.Code) || strings.Contains(got.raw, `"code":"`) {
		t.Fatalf("invitation list: %d %s", got.status, got.raw)
	}
	if got := e.admin(t, http.MethodPost, opapi.InvitationRevokePath, tok, opapi.IDRequest{ID: inv.ID}, &inv); got.status != http.StatusOK || inv.State != "revoked" {
		t.Fatalf("invitation revoke: %d %s", got.status, got.raw)
	}
	adminRefused(t, e.admin(t, http.MethodPost, opapi.InvitationRevokePath, tok, opapi.IDRequest{ID: inv.ID}, nil),
		http.StatusConflict, opapi.CodeInvitationFinal)
	pasted := "pasted-" + cred.Bearer
	adminRefused(t, e.admin(t, http.MethodPost, opapi.CredentialRevokePath, tok, opapi.IDRequest{ID: pasted}, nil),
		http.StatusNotFound, opapi.CodeNotFound)
	alive("the invitation operations")

	// The log names each action and its outcome, and never a secret or a
	// body.
	logs := e.logs.String()
	for _, want := range []string{`"msg":"operator","action":"team.create","outcome":"created"`,
		`"action":"credential.issue","outcome":"issued"`, `"action":"invitation.revoke","outcome":"revoked"`,
		`"action":"team.create","outcome":"team_exists"`} {
		if !strings.Contains(logs, want) {
			t.Fatalf("the log lacks %s:\n%s", want, logs)
		}
	}
	for _, secret := range []string{tok, cred.Bearer, rotated.Bearer, inv.Code, session, "second-name", pasted} {
		if strings.Contains(logs, secret) {
			t.Fatalf("the log holds a secret or a body value %q", secret)
		}
	}
}

// Replacing the operator token file rotates the credential on the next
// call, with no restart; a file that becomes unreadable fails closed.
func TestAdminTokenRotation(t *testing.T) {
	e := setupAPI(t)
	old := e.opToken
	if got := e.admin(t, http.MethodGet, opapi.TeamsPath, old, nil, nil); got.status != http.StatusOK {
		t.Fatalf("before: %d %s", got.status, got.raw)
	}
	next, _ := optoken.Generate()
	os.Remove(e.opFile)
	if err := optoken.Write(e.opFile, next); err != nil {
		t.Fatal(err)
	}
	adminRefused(t, e.admin(t, http.MethodGet, opapi.TeamsPath, old, nil, nil), http.StatusUnauthorized, opapi.CodeUnauthorized)
	if got := e.admin(t, http.MethodGet, opapi.TeamsPath, next, nil, nil); got.status != http.StatusOK {
		t.Fatalf("after the replacement: %d %s", got.status, got.raw)
	}
	os.Remove(e.opFile)
	// The service's own fault never spends the client's budget: retries
	// stay 503, never 429, and the restored file works at once.
	for i := 0; i < 2*AdminFailuresPerMinute; i++ {
		adminRefused(t, e.admin(t, http.MethodGet, opapi.TeamsPath, next, nil, nil), http.StatusServiceUnavailable, opapi.CodeUnavailable)
	}
	if err := optoken.Write(e.opFile, next); err != nil {
		t.Fatal(err)
	}
	if got := e.admin(t, http.MethodGet, opapi.TeamsPath, next, nil, nil); got.status != http.StatusOK {
		t.Fatalf("after the file is restored: %d %s", got.status, got.raw)
	}
}

// The service does not start without a usable operator token file, and
// says so without the content.
func TestNewChecksOperatorToken(t *testing.T) {
	certFile, keyFile, _ := testCert(t)
	st, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "aicrew.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	dir := t.TempDir()
	tok, _ := optoken.Generate()
	weak := filepath.Join(dir, "weak")
	f, _ := os.Create(weak)
	f.WriteString("aop_short\n")
	f.Close()
	cases := map[string]string{"missing": filepath.Join(dir, "missing"), "none": ""}
	if runtime.GOOS != "windows" {
		os.Chmod(weak, 0o600)
		shared := filepath.Join(dir, "shared")
		os.WriteFile(shared, []byte(tok+"\n"), 0o644)
		cases["shared"] = shared
		cases["malformed"] = weak
	}
	for name, path := range cases {
		cfg := Config{ListenAddr: "127.0.0.1:0", TLSCertFile: certFile, TLSKeyFile: keyFile, ServiceID: "aicrew-test",
			OperatorTokenFile: path}
		_, err := New(cfg, st, slog.New(slog.NewJSONHandler(io.Discard, nil)))
		if err == nil || !strings.Contains(err.Error(), "operator_token_file") || strings.Contains(err.Error(), tok) {
			t.Fatalf("%s: %v", name, err)
		}
	}
}

// Failed operator authentications are limited per address; once spent, even
// the right credential is refused until the budget refills.
func TestAdminFailureLimit(t *testing.T) {
	e := setupAPI(t)
	wrong, _ := optoken.Generate()
	for i := 0; i < AdminFailuresPerMinute; i++ {
		adminRefused(t, e.admin(t, http.MethodGet, opapi.TeamsPath, wrong, nil, nil), http.StatusUnauthorized, opapi.CodeUnauthorized)
	}
	got := e.admin(t, http.MethodGet, opapi.TeamsPath, e.opToken, nil, nil)
	adminRefused(t, got, http.StatusTooManyRequests, opapi.CodeRateLimited)
	if got.header.Get("Retry-After") == "" {
		t.Fatal("no Retry-After")
	}
}

// Two concurrent creations of one name: exactly one team gets it.
func TestAdminTeamNameRace(t *testing.T) {
	e := setupAPI(t)
	var wg sync.WaitGroup
	statuses := make(chan int, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			statuses <- e.admin(t, http.MethodPost, opapi.TeamsPath, e.opToken, opapi.TeamRequest{Name: "race"}, nil).status
		}()
	}
	wg.Wait()
	close(statuses)
	created := 0
	for s := range statuses {
		switch s {
		case http.StatusCreated:
			created++
		case http.StatusConflict:
		default:
			t.Fatalf("status %d", s)
		}
	}
	if created != 1 {
		t.Fatalf("%d teams created with one name", created)
	}
}

// Concurrent rotations of a hub's one credential: exactly one replaces it.
// The store allows at most two active credentials per hub, checked inside its
// own transaction, so the others are refused whatever their interleaving.
func TestAdminRotationRace(t *testing.T) {
	e := setupAPI(t)
	if got := e.admin(t, http.MethodPost, opapi.CredentialsPath, e.opToken, opapi.CredentialRequest{HubID: "hub-r"}, nil); got.status != http.StatusCreated {
		t.Fatalf("issue: %d %s", got.status, got.raw)
	}
	var wg sync.WaitGroup
	statuses := make(chan int, 6)
	for i := 0; i < 6; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			statuses <- e.admin(t, http.MethodPost, opapi.CredentialRotatePath, e.opToken, opapi.CredentialRequest{HubID: "hub-r"}, nil).status
		}()
	}
	wg.Wait()
	close(statuses)
	rotated := 0
	for s := range statuses {
		if s == http.StatusCreated {
			rotated++
		} else if s != http.StatusConflict {
			t.Fatalf("status %d", s)
		}
	}
	if rotated != 1 {
		t.Fatalf("%d rotations of one credential", rotated)
	}
}

// A concurrent burst of wrong bearers compares no more of them than the
// budget holds: admission and charging are one step, so the rest are refused
// before any comparison. The limiter's clock is frozen, so nothing refills
// during the burst.
func TestAdminFailureLimitUnderConcurrency(t *testing.T) {
	e := setupAPI(t)
	frozen := time.Now()
	e.srv.admin.failures.now = func() time.Time { return frozen }
	wrong, _ := optoken.Generate()
	const burst = 3 * AdminFailuresPerMinute
	var wg sync.WaitGroup
	statuses := make(chan int, burst)
	for i := 0; i < burst; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			statuses <- e.admin(t, http.MethodGet, opapi.TeamsPath, wrong, nil, nil).status
		}()
	}
	wg.Wait()
	close(statuses)
	counts := map[int]int{}
	for s := range statuses {
		counts[s]++
	}
	if counts[http.StatusUnauthorized] != AdminFailuresPerMinute || counts[http.StatusTooManyRequests] != burst-AdminFailuresPerMinute {
		t.Fatalf("statuses %v: want exactly %d comparisons", counts, AdminFailuresPerMinute)
	}
	// The right credential is refused too until the budget refills, and
	// successful calls do not spend it.
	adminRefused(t, e.admin(t, http.MethodGet, opapi.TeamsPath, e.opToken, nil, nil), http.StatusTooManyRequests, opapi.CodeRateLimited)
	frozen = frozen.Add(time.Minute)
	for i := 0; i < 3*AdminFailuresPerMinute; i++ {
		if got := e.admin(t, http.MethodGet, opapi.TeamsPath, e.opToken, nil, nil); got.status != http.StatusOK {
			t.Fatalf("call %d with the right credential: %d %s", i, got.status, got.raw)
		}
	}
}
