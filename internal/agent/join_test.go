package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/BlackVS/aicrew/internal/filelock"
	"github.com/BlackVS/aicrew/internal/invitecode"
	"github.com/BlackVS/aicrew/internal/privatefile"
	"github.com/BlackVS/aicrew/internal/server"
	"github.com/BlackVS/aicrew/internal/store"
	"github.com/BlackVS/aicrew/internal/tlstrust"
	"github.com/BlackVS/aicrew/internal/verifier"
)

// The client bootstrap (1a81-4) against a real aicrewd over pinned TLS, with
// aimem replaced in process (the proof) or by this test binary (the
// credential status command).

// joinFakeEnv makes this test binary answer as `aimem hub credential`.
const joinFakeEnv = "AICREW_JOIN_FAKE_AIMEM"

func init() {
	mode := os.Getenv(joinFakeEnv)
	if mode == "" {
		return
	}
	guardOrExit("aimem")
	args := os.Args[1:]
	if len(args) != 4 || args[0] != "hub" || args[1] != "credential" || args[3] != "--json" {
		fmt.Fprintf(os.Stderr, "unexpected arguments %q\n", args)
		os.Exit(3)
	}
	switch mode {
	case "old":
		fmt.Fprintln(os.Stderr, "usage: aimem hub <url> <token>                      set/replace the default hub")
		os.Exit(1)
	case "unknown-hub":
		fmt.Fprintf(os.Stderr, "hub %q is not configured on this machine\n", args[2])
		os.Exit(1)
	case "active":
		fmt.Printf(`{"hub": %q, "credential": "set", "state": "active", "scope": "user", "user_id": "user-1", "token_id": "tok-1"}`+"\n", args[2])
	case "garbled":
		fmt.Println("not json")
	case "incomplete":
		fmt.Println("{}")
	}
	os.Exit(0)
}

// joinVerifier vouches for the fake receipt of a challenge as its user.
type joinVerifier struct {
	mu      sync.Mutex
	hub     string
	user    string
	redeems int
}

func (v *joinVerifier) Redeem(_ context.Context, req store.RedeemRequest) (store.VerifiedIdentity, error) {
	if req.Receipt.Reveal() != fakeReceipt(req.ChallengeID) {
		return store.VerifiedIdentity{}, &verifier.Error{Code: "proof_invalid", Reason: "refused"}
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	v.redeems++
	return store.VerifiedIdentity{HubID: v.hub, UserID: v.user, TokenID: "tok-" + v.user}, nil
}

// fakeJoinAimem is aimem in process: its credential status and its proofs.
type fakeJoinAimem struct {
	mu        sync.Mutex
	cred      CredentialStatus
	known     bool
	credErr   error
	proofErr  error
	badProofs int // the first proofs give a receipt aimem's hub would refuse
	proofs    int
	onCheck   func() // runs during the credential check
}

func activeAimem() *fakeJoinAimem {
	return &fakeJoinAimem{known: true, cred: CredentialStatus{Hub: "main", Credential: "set", State: "active",
		Scope: "user", UserID: "user-1", TokenID: "tok-1"}}
}

func (f *fakeJoinAimem) Credential(context.Context) (CredentialStatus, bool, error) {
	if f.onCheck != nil {
		f.onCheck()
	}
	return f.cred, f.known, f.credErr
}

func (f *fakeJoinAimem) Proof(_ context.Context, _, _, challengeID string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.proofs++
	if f.proofErr != nil {
		return "", f.proofErr
	}
	if f.badProofs > 0 {
		f.badProofs--
		return "amr1_refused", nil
	}
	return fakeReceipt(challengeID), nil
}

// recCrew records the redemption calls, and can lose completion replies
// after aicrewd has answered them.
type recCrew struct {
	InvitationAPI
	mu           sync.Mutex
	beginKeys    []string
	completeKeys []string
	lose         int
	refuse       map[string]error // a refusal per call ("begin", "complete"), once
}

func (c *recCrew) BeginInvitation(ctx context.Context, key, code string) (Challenge, error) {
	c.mu.Lock()
	c.beginKeys = append(c.beginKeys, key)
	err := c.refuse["begin"]
	delete(c.refuse, "begin")
	c.mu.Unlock()
	if err != nil {
		return Challenge{}, err
	}
	return c.InvitationAPI.BeginInvitation(ctx, key, code)
}

func (c *recCrew) CompleteInvitation(ctx context.Context, key, code, challengeID, receipt string) (LinkResult, error) {
	c.mu.Lock()
	c.completeKeys = append(c.completeKeys, key)
	err := c.refuse["complete"]
	delete(c.refuse, "complete")
	c.mu.Unlock()
	if err != nil {
		return LinkResult{}, err
	}
	res, err := c.InvitationAPI.CompleteInvitation(ctx, key, code, challengeID, receipt)
	c.mu.Lock()
	defer c.mu.Unlock()
	if err == nil && c.lose > 0 {
		c.lose--
		return LinkResult{}, &TransportError{Err: errors.New("the reply was lost")}
	}
	return res, err
}

type joinEnv struct {
	store  *store.Store
	path   string
	op     store.Caller
	teamID string
	url    string
	pin    string
	ver    *joinVerifier
	home   string
	out    bytes.Buffer
}

func setupJoin(t *testing.T) *joinEnv {
	t.Helper()
	ctx := context.Background()
	dir := t.TempDir()
	certFile, keyFile, pin := writeCert(t, dir)
	e := &joinEnv{path: filepath.Join(dir, "aicrew.db"), pin: pin, ver: &joinVerifier{hub: "hub-test", user: "user-1"},
		home: filepath.Join(dir, "agents", "builder")}
	foreignInstallation(t, e.home)
	st, err := store.Open(ctx, e.path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	e.store = st
	e.op, _ = store.OperatorCaller("op-test")
	tm, err := st.CreateTeam(ctx, e.op, "team", store.NewTeam{Name: "crew"})
	if err != nil {
		t.Fatal(err)
	}
	e.teamID = tm.ID
	srv, err := server.New(server.Config{StorePath: e.path, ListenAddr: "127.0.0.1:0", TLSCertFile: certFile,
		TLSKeyFile: keyFile, ServiceID: "aicrew-test", ShutdownTimeout: server.Duration(5 * time.Second)},
		st, slog.New(slog.NewTextHandler(new(bytes.Buffer), nil)), server.WithVerifier(e.ver))
	if err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	sctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- srv.Serve(sctx, ln) }()
	t.Cleanup(func() { cancel(); <-done })
	e.url = "https://" + ln.Addr().String()
	return e
}

// invite issues a join invitation and returns its code as shown to the
// operator.
func (e *joinEnv) invite(t *testing.T, key string, role store.Role) string {
	t.Helper()
	code, err := store.GenerateInvitationCode()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.store.IssueInvitation(context.Background(), e.op, key, store.InvitationRequest{
		Purpose: store.PurposeJoin, TeamID: e.teamID, Role: role, HubID: "hub-test", Label: "builder"}, code); err != nil {
		t.Fatal(err)
	}
	return code.Reveal()
}

func (e *joinEnv) opts() JoinOptions {
	return JoinOptions{Home: e.home, Label: "builder", URL: e.url,
		Trust: tlstrust.Binding{Mode: tlstrust.SPKI, Value: e.pin}, AimemHub: "main", Terminal: true,
		Clients: []string{"claude"}}
}

// deps wires the real client through crew, aimem in process, and a prompt
// that answers with codes in turn; reads counts the prompts.
func (e *joinEnv) deps(crew *recCrew, am *fakeJoinAimem, reads *int, codes ...string) JoinDeps {
	return JoinDeps{
		Crew: func(cfg Config) (InvitationAPI, error) {
			c, err := NewCrew(cfg, nil)
			crew.InvitationAPI = c
			return crew, err
		},
		Aimem: func(string, string, string) JoinAimem { return am },
		ReadCode: func() (string, error) {
			*reads++
			if *reads > len(codes) {
				return "", errors.New("no more input")
			}
			return codes[*reads-1], nil
		},
		Out:   &e.out,
		Sleep: func(context.Context, time.Duration) error { return nil },
		check: readyCheck,
	}
}

func (e *joinEnv) join(t *testing.T, o JoinOptions, crew *recCrew, am *fakeJoinAimem, codes ...string) (JoinReport, int) {
	t.Helper()
	reads := 0
	rep, err := Join(context.Background(), o, e.deps(crew, am, &reads, codes...))
	if err != nil {
		t.Fatalf("join: %v (%+v)", err, rep)
	}
	return rep, reads
}

// noCodeAnywhere fails when the code, in any of its forms, or a receipt
// appears in any file under the home, the output or the report.
func (e *joinEnv) noCodeAnywhere(t *testing.T, rep JoinReport, code string) {
	t.Helper()
	norm, _ := invitecode.Normalize(code)
	forms := []string{code, strings.ReplaceAll(code, "-", ""), norm, "amr1_"}
	check := func(where, text string) {
		t.Helper()
		for _, f := range forms {
			if strings.Contains(text, f) {
				t.Fatalf("%s carries a secret (%q)", where, f)
			}
		}
	}
	raw, _ := json.Marshal(rep)
	check("the report", string(raw))
	check("the output", e.out.String())
	filepath.WalkDir(e.home, func(path string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			b, _ := os.ReadFile(path)
			check(path, string(b))
		}
		return nil
	})
}

func readJSON(t *testing.T, path string) map[string]any {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	return m
}

// A scratch home is linked and prepared: agent.json names the new agent and
// team, the layout exists with private creds/ and state/, the guidance is
// written with its digests, the join state is gone, the configuration loads,
// and the code appears nowhere.
func TestJoinEndToEnd(t *testing.T) {
	e := setupJoin(t)
	code := e.invite(t, "inv-1", store.RoleWorker)
	if _, err := os.Stat(e.home); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the home exists before the join: %v", err)
	}
	crew := &recCrew{}
	rep, reads := e.join(t, e.opts(), crew, activeAimem(), strings.ToLower(code))
	if rep.Status != JoinReady || !rep.Redeemed || rep.Role != "worker" || rep.TeamID != e.teamID || reads != 1 {
		t.Fatalf("report %+v, %d reads", rep, reads)
	}
	if !strings.Contains(rep.Next, "aicrew-agent session start -home ") {
		t.Fatalf("next command %q", rep.Next)
	}
	a, err := e.store.GetAgent(context.Background(), rep.AgentID)
	if err != nil || a.Linked == nil || a.Linked.UserID != "user-1" || a.Label != "builder" {
		t.Fatalf("agent %+v, %v", a, err)
	}
	cfg, err := LoadConfig(e.home)
	if err != nil || cfg.AgentID != rep.AgentID || cfg.TeamID != e.teamID || cfg.AimemHub != "main" || cfg.URL != e.url {
		t.Fatalf("config %+v, %v", cfg, err)
	}
	doc := readJSON(t, filepath.Join(e.home, "agent.json"))
	if doc["layout"] != float64(1) || doc["label"] != "builder" {
		t.Fatalf("agent.json %v", doc)
	}
	managed, _ := doc["managed"].(map[string]any)
	for _, f := range []string{"AGENTS.md", "CLAUDE.md", "docs/START.md", "docs/ROLES.md", ".claude/settings.json"} {
		b, err := os.ReadFile(filepath.Join(e.home, filepath.FromSlash(f)))
		if err != nil || managed[f] != digestOf(b) {
			t.Fatalf("%s: digest %v, %v", f, managed[f], err)
		}
	}
	// The carrier names the home's own installation, by absolute paths.
	if b, _ := os.ReadFile(filepath.Join(e.home, ".claude", "settings.json")); string(b) != claudeSettings(e.home) ||
		!strings.Contains(string(b), strconvQuote(filepath.Join(e.home, "aimem"))) {
		t.Fatalf("settings.json %s", b)
	}
	if _, ok := managed["docs/HANDOFF.md"]; ok {
		t.Fatal("the agent-owned handoff is recorded as managed")
	}
	for _, d := range append(append([]string{}, homeDirs...), privateDirs...) {
		if fi, err := os.Stat(filepath.Join(e.home, d)); err != nil || !fi.IsDir() {
			t.Fatalf("%s/: %v", d, err)
		}
	}
	for _, d := range privateDirs {
		if err := privatefile.CheckDir(filepath.Join(e.home, d)); err != nil {
			t.Fatalf("%s/ is not private: %v", d, err)
		}
	}
	if ents, _ := os.ReadDir(filepath.Join(e.home, "creds")); len(ents) != 0 {
		t.Fatalf("creds/ holds %d entries", len(ents))
	}
	if _, err := os.Stat(joinStatePath(e.home)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the join state is left: %v", err)
	}
	e.noCodeAnywhere(t, rep, code)
}

// A rerun of a linked home asks for no code and redeems nothing: unchanged
// files stay, a managed file changed since its last managed write is
// updated (restart required), a locally edited one gets a .aicrew-new, the
// handoff, unknown files, creds/ and unknown agent.json keys are untouched.
func TestJoinRerunLinkedHome(t *testing.T) {
	e := setupJoin(t)
	code := e.invite(t, "inv-1", store.RoleWorker)
	first, _ := e.join(t, e.opts(), &recCrew{}, activeAimem(), code)

	rep, reads := e.join(t, JoinOptions{Home: e.home, Terminal: false}, &recCrew{}, activeAimem())
	if rep.Status != JoinReady || rep.Redeemed || reads != 0 || rep.AgentID != first.AgentID {
		t.Fatalf("unchanged rerun: %+v, %d reads", rep, reads)
	}
	for _, c := range rep.Changes {
		if c.Action != "unchanged" && c.Action != "kept" {
			t.Fatalf("unchanged rerun planned %+v", c)
		}
	}

	path := func(p string) string { return filepath.Join(e.home, filepath.FromSlash(p)) }
	old := "# an older managed version\n"
	os.WriteFile(path("CLAUDE.md"), []byte(old), 0o644)
	edited := "# edited by the operator\n"
	os.WriteFile(path("AGENTS.md"), []byte(edited), 0o644)
	os.WriteFile(path("docs/HANDOFF.md"), []byte("my notes\n"), 0o644)
	os.WriteFile(path("notes.txt"), []byte("unknown\n"), 0o644)
	os.WriteFile(path(".claude/settings.json"), []byte(`{"env": {}}`+"\n"), 0o644)
	os.WriteFile(path("creds/aimem.main.agent"), []byte("material\n"), 0o600)
	doc := readJSON(t, path("agent.json"))
	doc["managed"].(map[string]any)["CLAUDE.md"] = digestOf([]byte(old))
	doc["notes"] = map[string]any{"owner": "operator"}
	doc["aicrew"].(map[string]any)["client_command"] = "/opt/claude"
	raw, _ := json.Marshal(doc)
	os.WriteFile(path("agent.json"), raw, 0o644)

	rep, reads = e.join(t, JoinOptions{Home: e.home, URL: e.url}, &recCrew{}, activeAimem())
	if rep.Status != JoinRestartRequired || reads != 0 || !strings.Contains(rep.Instruction, ".aicrew-new") {
		t.Fatalf("rerun after edits: %+v", rep)
	}
	want := map[string]string{"AGENTS.md": "conflict", "CLAUDE.md": "update", "docs/START.md": "unchanged",
		"docs/ROLES.md": "unchanged", "docs/HANDOFF.md": "kept", ".claude/settings.json": "conflict"}
	for _, c := range rep.Changes {
		if want[c.Path] != c.Action {
			t.Fatalf("%s: %s, want %s", c.Path, c.Action, want[c.Path])
		}
	}
	for p, content := range map[string]string{"CLAUDE.md": claudeMD, "AGENTS.md": edited, "AGENTS.md.aicrew-new": agentsMD,
		"docs/HANDOFF.md": "my notes\n", "notes.txt": "unknown\n", "creds/aimem.main.agent": "material\n",
		".claude/settings.json.aicrew-new": claudeSettings(e.home)} {
		if b, _ := os.ReadFile(path(p)); string(b) != content {
			t.Fatalf("%s is %q", p, b)
		}
	}
	doc = readJSON(t, path("agent.json"))
	managed := doc["managed"].(map[string]any)
	if managed["CLAUDE.md"] != digestOf([]byte(claudeMD)) || managed["AGENTS.md"] != digestOf([]byte(agentsMD)) {
		t.Fatalf("managed digests %v", managed)
	}
	if doc["notes"] == nil || doc["aicrew"].(map[string]any)["client_command"] != "/opt/claude" {
		t.Fatalf("agent.json lost keys: %v", doc)
	}
	if _, err := LoadConfig(e.home); err != nil {
		t.Fatal(err)
	}

	// The conflict persists until it is merged; nothing else changes.
	rep, _ = e.join(t, JoinOptions{Home: e.home}, &recCrew{}, activeAimem())
	if rep.Status != JoinReady || rep.Changes[0].Action != "conflict" {
		t.Fatalf("second rerun: %+v", rep)
	}
}

// A home serves one team: a rerun naming another binding is refused before
// anything is read or changed.
func TestJoinLinkedHomeRefusesAnotherBinding(t *testing.T) {
	e := setupJoin(t)
	e.join(t, e.opts(), &recCrew{}, activeAimem(), e.invite(t, "inv-1", store.RoleWorker))
	before, _ := os.ReadFile(filepath.Join(e.home, "agent.json"))
	for _, o := range []JoinOptions{
		{Home: e.home, URL: "https://other.example:8443"},
		{Home: e.home, AimemHub: "other"},
		{Home: e.home, Trust: tlstrust.Binding{Mode: tlstrust.SPKI, Value: "sha256-other"}},
	} {
		rep, reads := e.join(t, o, &recCrew{}, activeAimem())
		if rep.Status != JoinBlocked || rep.Reason != "home_linked" || reads != 0 {
			t.Fatalf("%+v: %+v", o, rep)
		}
	}
	if after, _ := os.ReadFile(filepath.Join(e.home, "agent.json")); !bytes.Equal(before, after) {
		t.Fatal("a refused rerun changed agent.json")
	}
}

// A lost completion reply is recovered with the same completion key, in the
// run and across runs, and aimem redeems one receipt: a replayed completion
// is answered from aicrewd's record.
func TestJoinLostCompletionReply(t *testing.T) {
	t.Run("in the run", func(t *testing.T) {
		e := setupJoin(t)
		code := e.invite(t, "inv-1", store.RoleWorker)
		crew := &recCrew{lose: 2}
		rep, _ := e.join(t, e.opts(), crew, activeAimem(), code)
		if rep.Status != JoinReady || len(crew.completeKeys) != 3 || len(crew.beginKeys) != 1 {
			t.Fatalf("%+v; begins %v, completions %v", rep, crew.beginKeys, crew.completeKeys)
		}
		if crew.completeKeys[0] != crew.completeKeys[1] || crew.completeKeys[1] != crew.completeKeys[2] {
			t.Fatalf("completion keys changed: %v", crew.completeKeys)
		}
		if e.ver.redeems != 1 {
			t.Fatalf("aimem redeemed %d receipts", e.ver.redeems)
		}
	})
	t.Run("across runs", func(t *testing.T) {
		e := setupJoin(t)
		code := e.invite(t, "inv-1", store.RoleWorker)
		first := &recCrew{lose: maxRetries + 1}
		rep, _ := e.join(t, e.opts(), first, activeAimem(), code)
		if rep.Status != JoinBlocked || rep.Reason != "aicrew_unreachable" {
			t.Fatalf("first run: %+v", rep)
		}
		raw, err := os.ReadFile(joinStatePath(e.home))
		if err != nil || strings.Contains(string(raw), "amr1_") || strings.Contains(string(raw), strings.ReplaceAll(code, "-", "")) {
			t.Fatalf("join state %s, %v", raw, err)
		}
		if _, err := LoadConfig(e.home); err == nil {
			t.Fatal("an unlinked home's configuration loads")
		}
		second := &recCrew{}
		rep, _ = e.join(t, e.opts(), second, activeAimem(), code)
		if rep.Status != JoinReady || len(second.beginKeys) != 0 || second.completeKeys[0] != first.completeKeys[0] {
			t.Fatalf("rerun: %+v; begins %v, completions %v then %v", rep, second.beginKeys,
				first.completeKeys, second.completeKeys)
		}
		if e.ver.redeems != 1 {
			t.Fatalf("aimem redeemed %d receipts", e.ver.redeems)
		}
		e.noCodeAnywhere(t, rep, code)
	})
}

// A refused receipt gets a new one for the same challenge; a challenge that
// expired or was superseded gets a new begin with a new key; a transient
// begin failure is retried with the same key.
func TestJoinRecovery(t *testing.T) {
	e := setupJoin(t)
	code := e.invite(t, "inv-1", store.RoleWorker)
	am := activeAimem()
	am.badProofs = 3 // the first is spent on the injected challenge_invalid
	crew := &recCrew{refuse: map[string]error{
		"begin":    &TransportError{Err: errors.New("reset")},
		"complete": &Refusal{Code: "challenge_invalid"},
	}}
	rep, _ := e.join(t, e.opts(), crew, am, code)
	if rep.Status != JoinReady {
		t.Fatalf("%+v", rep)
	}
	// begin (lost), begin again with the same key, then after
	// challenge_invalid a new key.
	if len(crew.beginKeys) != 3 || crew.beginKeys[0] != crew.beginKeys[1] || crew.beginKeys[1] == crew.beginKeys[2] {
		t.Fatalf("begin keys %v", crew.beginKeys)
	}
	// complete refused as challenge_invalid, then two refused receipts and
	// the accepted one, all with the new challenge's key.
	k := crew.completeKeys
	if len(k) != 4 || k[0] == k[1] || k[1] != k[2] || k[2] != k[3] || am.proofs != 4 {
		t.Fatalf("completion keys %v, %d proofs", k, am.proofs)
	}
}

// Every stop condition ends the run blocked with its instruction, keeps the
// join state for a rerun, and leaves the code nowhere.
func TestJoinStops(t *testing.T) {
	for code, instruction := range stops {
		t.Run(code, func(t *testing.T) {
			e := setupJoin(t)
			invite := e.invite(t, "inv-1", store.RoleWorker)
			crew := &recCrew{refuse: map[string]error{"complete": &Refusal{Status: 403, Code: code, Message: "m"}}}
			rep, _ := e.join(t, e.opts(), crew, activeAimem(), invite)
			if rep.Status != JoinBlocked || rep.Reason != code || rep.Instruction != instruction {
				t.Fatalf("%+v", rep)
			}
			if _, err := os.Stat(joinStatePath(e.home)); err != nil {
				t.Fatalf("the join state is gone: %v", err)
			}
			e.noCodeAnywhere(t, rep, invite)
		})
	}
	t.Run("invitation_invalid from aicrewd", func(t *testing.T) {
		e := setupJoin(t)
		unissued, _ := store.GenerateInvitationCode()
		rep, _ := e.join(t, e.opts(), &recCrew{}, activeAimem(), unissued.Reveal())
		if rep.Status != JoinBlocked || rep.Reason != "invitation_invalid" {
			t.Fatalf("%+v", rep)
		}
	})
	t.Run("identity_mismatch from aicrewd", func(t *testing.T) {
		e := setupJoin(t)
		e.ver.hub = "hub-other"
		rep, _ := e.join(t, e.opts(), &recCrew{}, activeAimem(), e.invite(t, "inv-1", store.RoleWorker))
		if rep.Status != JoinBlocked || rep.Reason != "identity_mismatch" {
			t.Fatalf("%+v", rep)
		}
	})
	t.Run("role_conflict from aicrewd", func(t *testing.T) {
		e := setupJoin(t)
		first, _ := e.join(t, e.opts(), &recCrew{}, activeAimem(), e.invite(t, "inv-1", store.RoleWorker))
		// The same aimem user, invited again into the same team with
		// another role, from a fresh home.
		e.home = filepath.Join(filepath.Dir(e.home), "second")
		rep, _ := e.join(t, e.opts(), &recCrew{}, activeAimem(), e.invite(t, "inv-2", store.RoleIndependent))
		if rep.Status != JoinBlocked || rep.Reason != "role_conflict" || first.AgentID == "" {
			t.Fatalf("%+v", rep)
		}
	})
}

// D1: the credential is checked before the prompt, and a missing, refused
// or unconfirmed one stops the run before anything is created or spent. An
// aimem that cannot report it leaves the check to the proof.
func TestJoinCredentialCheck(t *testing.T) {
	for _, c := range []struct {
		name   string
		am     *fakeJoinAimem
		reason string
	}{
		{"none", &fakeJoinAimem{known: true, cred: CredentialStatus{Credential: "none", State: "absent"}}, "credential_missing"},
		{"other", &fakeJoinAimem{known: true, cred: CredentialStatus{Credential: "other", State: "absent"}}, "credential_missing"},
		{"refused", &fakeJoinAimem{known: true, cred: CredentialStatus{Credential: "set", State: "refused"}}, "credential_refused"},
		{"unreachable", &fakeJoinAimem{known: true, cred: CredentialStatus{Credential: "set", State: "unreachable",
			Detail: "dial tcp: refused"}}, "aimem_unreachable"},
		{"unprovisioned", &fakeJoinAimem{credErr: errors.New(`aimem hub credential failed: hub "main" is not configured`)},
			"credential_missing"},
		{"aimem failed", &fakeJoinAimem{credErr: errors.New(`aimem hub credential failed: permission denied`)},
			"aimem_failed"},
	} {
		t.Run(c.name, func(t *testing.T) {
			e := setupJoin(t)
			crew := &recCrew{}
			rep, reads := e.join(t, e.opts(), crew, c.am)
			if rep.Status != JoinBlocked || rep.Reason != c.reason || reads != 0 || len(crew.beginKeys) != 0 {
				t.Fatalf("%+v, %d reads, begins %v", rep, reads, crew.beginKeys)
			}
			if _, err := os.Stat(e.home); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("a blocked credential check created the home")
			}
		})
	}
	t.Run("unknown, then the proof names the missing credential", func(t *testing.T) {
		e := setupJoin(t)
		am := &fakeJoinAimem{proofErr: errors.New("aimem identity proof failed: the hub has no individual credential " +
			"(aimem hub task-token); team mode uses only the installation's user-scoped credential")}
		rep, reads := e.join(t, e.opts(), &recCrew{}, am, e.invite(t, "inv-1", store.RoleWorker))
		if rep.Status != JoinBlocked || rep.Reason != "credential_missing" || reads != 1 ||
			!strings.Contains(rep.Instruction, "aimem hub task-token main") {
			t.Fatalf("%+v", rep)
		}
	})
	t.Run("unknown, then another proof failure", func(t *testing.T) {
		e := setupJoin(t)
		am := &fakeJoinAimem{proofErr: errors.New("aimem identity proof failed: hub unreachable")}
		rep, _ := e.join(t, e.opts(), &recCrew{}, am, e.invite(t, "inv-1", store.RoleWorker))
		if rep.Status != JoinBlocked || rep.Reason != "proof_failed" {
			t.Fatalf("%+v", rep)
		}
	})
}

// ExecAimem's credential check always names the hub (an older aimem would
// take `hub credential --json` for `hub <url> <token>`), reads the JSON, and
// treats an older aimem's hub usage as unknown.
func TestExecAimemCredential(t *testing.T) {
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	am := ExecAimem{Command: self, Hub: "main"}.JoinAimem()
	for mode, want := range map[string]struct {
		known bool
		err   bool
	}{"active": {true, false}, "old": {false, false}, "garbled": {false, true}, "incomplete": {false, true},
		"unknown-hub": {false, true}} {
		t.Setenv(joinFakeEnv, mode)
		st, known, err := am.Credential(context.Background())
		if known != want.known || (err != nil) != want.err {
			t.Fatalf("%s: %+v, %v, %v", mode, st, known, err)
		}
		if mode == "active" && (st.Credential != "set" || st.State != "active" || st.UserID != "user-1") {
			t.Fatalf("active: %+v", st)
		}
		if mode == "unknown-hub" && !strings.Contains(err.Error(), `hub "main" is not configured`) {
			t.Fatalf("unknown hub: %v", err)
		}
	}
	if _, _, err := (ExecAimem{Command: self}).JoinAimem().Credential(context.Background()); err == nil {
		t.Fatal("a credential check without a hub name ran")
	}
}

// The code is read only at a terminal's prompt, checked before it costs an
// attempt, and a malformed one is asked again and at last refused.
func TestJoinCodeEntry(t *testing.T) {
	t.Run("off a terminal", func(t *testing.T) {
		e := setupJoin(t)
		o := e.opts()
		o.Terminal = false
		crew := &recCrew{}
		rep, reads := e.join(t, o, crew, activeAimem(), e.invite(t, "inv-1", store.RoleWorker))
		if rep.Status != JoinBlocked || rep.Reason != "no_terminal" || reads != 0 || len(crew.beginKeys) != 0 {
			t.Fatalf("%+v, %d reads", rep, reads)
		}
		if _, err := os.Stat(e.home); !errors.Is(err, os.ErrNotExist) {
			t.Fatal("the refused run created the home")
		}
	})
	t.Run("malformed, then valid", func(t *testing.T) {
		e := setupJoin(t)
		code := e.invite(t, "inv-1", store.RoleWorker)
		typo := code[:len(code)-1] + map[bool]string{true: "1", false: "2"}[code[len(code)-1] != '1']
		rep, reads := e.join(t, e.opts(), &recCrew{}, activeAimem(), "", typo, code)
		if rep.Status != JoinReady || reads != 3 || !strings.Contains(e.out.String(), "not a valid invitation code") {
			t.Fatalf("%+v, %d reads", rep, reads)
		}
	})
	t.Run("malformed three times", func(t *testing.T) {
		e := setupJoin(t)
		crew := &recCrew{}
		rep, reads := e.join(t, e.opts(), crew, activeAimem(), "x", "y", "z", "never read")
		if rep.Status != JoinBlocked || rep.Reason != "code_malformed" || reads != maxCodeTries || len(crew.beginKeys) != 0 {
			t.Fatalf("%+v, %d reads", rep, reads)
		}
	})
	t.Run("a new code starts afresh", func(t *testing.T) {
		e := setupJoin(t)
		first := &recCrew{refuse: map[string]error{"complete": &Refusal{Code: "invitation_invalid"}}}
		e.join(t, e.opts(), first, activeAimem(), e.invite(t, "inv-1", store.RoleWorker))
		second := &recCrew{}
		rep, _ := e.join(t, e.opts(), second, activeAimem(), e.invite(t, "inv-2", store.RoleWorker))
		if rep.Status != JoinReady || second.beginKeys[0] == first.beginKeys[0] {
			t.Fatalf("%+v; begins %v then %v", rep, first.beginKeys, second.beginKeys)
		}
	})
}

// Options and home checks refuse before anything is created.
func TestJoinRefusesBadOptionsAndLayouts(t *testing.T) {
	e := setupJoin(t)
	for name, o := range map[string]JoinOptions{
		"no url":       {Home: e.home, Label: "builder", AimemHub: "main", Terminal: true},
		"http url":     {Home: e.home, Label: "builder", URL: "http://x", AimemHub: "main", Terminal: true},
		"bad label":    {Home: e.home, Label: "Builder!", URL: e.url, AimemHub: "main", Terminal: true},
		"flag-ish hub": {Home: e.home, Label: "builder", URL: e.url, AimemHub: "-x", Terminal: true},
		"no hub":       {Home: e.home, Label: "builder", URL: e.url, Terminal: true},
	} {
		reads := 0
		if name != "no url" {
			o.Trust = tlstrust.Binding{Mode: tlstrust.SPKI, Value: e.pin}
		}
		rep, err := Join(context.Background(), o, e.deps(&recCrew{}, activeAimem(), &reads))
		if !errors.Is(err, ErrJoinUsage) || rep.Status != JoinBlocked || reads != 0 {
			t.Fatalf("%s: %+v, %v", name, rep, err)
		}
	}
	if _, err := os.Stat(e.home); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("a refused run created the home")
	}
	os.MkdirAll(e.home, 0o755)
	os.WriteFile(filepath.Join(e.home, "agent.json"), []byte(`{"layout": 2}`), 0o644)
	rep, _ := e.join(t, e.opts(), &recCrew{}, activeAimem())
	if rep.Status != JoinBlocked || rep.Reason != "layout_unsupported" {
		t.Fatalf("%+v", rep)
	}
	os.WriteFile(filepath.Join(e.home, "agent.json"), []byte(`[]`), 0o644)
	rep, _ = e.join(t, e.opts(), &recCrew{}, activeAimem())
	if rep.Status != JoinBlocked || rep.Reason != "agent_json_invalid" {
		t.Fatalf("%+v", rep)
	}
}

// Runs on one home are serialized, and a run that waited reads the home
// again: one linked meanwhile by another run is only refreshed, with no
// prompt and no redemption, and its agent.json keeps the other run's IDs.
func TestJoinSerializesRunsPerHome(t *testing.T) {
	e := setupJoin(t)
	os.MkdirAll(filepath.Join(e.home, "state"), 0o700)
	unlock, err := filelock.Lock(context.Background(), filepath.Join(e.home, "state", "aicrew-join.lock"))
	if err != nil {
		t.Fatal(err)
	}
	checked := make(chan struct{})
	am := activeAimem()
	am.onCheck = func() {
		// Another run links the home after this one read it.
		linked := fmt.Sprintf(`{"layout": 1, "label": "builder", "aicrew": {"url": %q, "tls_trust_mode": "spki_sha256",
			"tls_trust_value": %q, "aimem_hub": "main", "agent_id": "agent-other", "team_id": "team-other"}}`, e.url, e.pin)
		os.WriteFile(filepath.Join(e.home, "agent.json"), []byte(linked), 0o600)
		close(checked)
	}
	type result struct {
		rep   JoinReport
		reads int
		err   error
	}
	done := make(chan result, 1)
	go func() {
		reads := 0
		rep, err := Join(context.Background(), e.opts(), e.deps(&recCrew{}, am, &reads, "never read"))
		done <- result{rep, reads, err}
	}()
	<-checked
	select {
	case r := <-done:
		t.Fatalf("the run did not wait for the other: %+v", r.rep)
	case <-time.After(200 * time.Millisecond):
	}
	unlock()
	r := <-done
	if r.err != nil || r.rep.Status != JoinReady || r.rep.Redeemed || r.reads != 0 || r.rep.AgentID != "agent-other" {
		t.Fatalf("%+v, %d reads, %v", r.rep, r.reads, r.err)
	}
	cfg, err := LoadConfig(e.home)
	if err != nil || cfg.AgentID != "agent-other" || cfg.TeamID != "team-other" {
		t.Fatalf("config %+v, %v", cfg, err)
	}
}

// An aimem that runs the credential command but answers with something
// other than a status confirms nothing: the run stops before the prompt,
// the home and any invitation attempt. Only an aimem without the command
// (its hub usage) is left to the proof.
func TestJoinMalformedCredentialAnswerStops(t *testing.T) {
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	for _, mode := range []string{"garbled", "incomplete"} {
		t.Run(mode, func(t *testing.T) {
			e := setupJoin(t)
			t.Setenv(joinFakeEnv, mode)
			crew := &recCrew{}
			reads := 0
			deps := e.deps(crew, nil, &reads, e.invite(t, "inv-1", store.RoleWorker))
			deps.Aimem = func(_, hub, home string) JoinAimem { return ExecAimem{Command: self, Hub: hub, Home: home}.JoinAimem() }
			rep, err := Join(context.Background(), e.opts(), deps)
			if err != nil || rep.Status != JoinBlocked || rep.Reason != "aimem_failed" || reads != 0 || len(crew.beginKeys) != 0 {
				t.Fatalf("%+v, %v, %d reads, begins %v", rep, err, reads, crew.beginKeys)
			}
			if _, err := os.Stat(e.home); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("the refused run created the home")
			}
		})
	}
}

// join reads the credential from the home's installation (D-STORE): one that
// does not know the hub yet is a home to provision, and the instruction
// names the home's two variables.
func TestJoinUnprovisionedHome(t *testing.T) {
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	e := setupJoin(t)
	t.Setenv(joinFakeEnv, "unknown-hub")
	crew := &recCrew{}
	reads := 0
	deps := e.deps(crew, nil, &reads, e.invite(t, "inv-1", store.RoleWorker))
	deps.Aimem = func(_, hub, home string) JoinAimem { return ExecAimem{Command: self, Hub: hub, Home: home}.JoinAimem() }
	rep, err := Join(context.Background(), e.opts(), deps)
	if err != nil || rep.Status != JoinBlocked || rep.Reason != "credential_missing" || reads != 0 {
		t.Fatalf("%+v, %v", rep, err)
	}
	for _, want := range []string{"AIMEM_STATE_DIR=" + AimemDir(e.home), "AIMEM_SOCKET=" + AimemSocket(e.home),
		"aimem hub add main", "aimem hub task-token main"} {
		if !strings.Contains(rep.Instruction, want) {
			t.Fatalf("instruction %q lacks %q", rep.Instruction, want)
		}
	}
}

// readyCheck stands in for the dependency and client check in the join
// tests, which check_test.go covers: it records the selected clients as the
// real check does, and reports ready.
func readyCheck(_ context.Context, o CheckOptions, doc *agentDoc, _ bool) (CheckReport, error) {
	sel, err := selectClients(o.Clients, *doc)
	if err != nil {
		return CheckReport{}, err
	}
	doc.set(doc.top, "clients", sel)
	return CheckReport{Status: JoinReady}, nil
}

// A hand-edited agent.json whose managed record is null or not an object
// is refreshed without a panic: the record counts as empty, so unchanged
// managed files are recorded again and an edited one is not overwritten.
func TestJoinRefreshUnusableManagedRecord(t *testing.T) {
	for _, record := range []string{`null`, `[1]`, `"x"`} {
		t.Run(record, func(t *testing.T) {
			e := setupJoin(t)
			e.join(t, e.opts(), &recCrew{}, activeAimem(), e.invite(t, "inv-1", store.RoleWorker))
			path := filepath.Join(e.home, "agent.json")
			doc := readJSON(t, path)
			raw, _ := json.Marshal(doc)
			raw = bytes.Replace(raw, mustJSON(t, doc["managed"]), []byte(record), 1)
			os.WriteFile(path, raw, 0o600)
			edited := "# edited by the operator\n"
			os.WriteFile(filepath.Join(e.home, "AGENTS.md"), []byte(edited), 0o644)

			rep, reads := e.join(t, JoinOptions{Home: e.home}, &recCrew{}, activeAimem())
			if rep.Status != JoinReady || reads != 0 {
				t.Fatalf("%+v", rep)
			}
			actions := map[string]string{}
			for _, c := range rep.Changes {
				actions[c.Path] = c.Action
			}
			if actions["AGENTS.md"] != "conflict" || actions["CLAUDE.md"] != "unchanged" {
				t.Fatalf("%v", actions)
			}
			if b, _ := os.ReadFile(filepath.Join(e.home, "AGENTS.md")); string(b) != edited {
				t.Fatalf("the edited file was overwritten: %s", b)
			}
			managed, ok := readJSON(t, path)["managed"].(map[string]any)
			if !ok || managed["CLAUDE.md"] == nil {
				t.Fatalf("the record was not rebuilt: %v", managed)
			}
		})
	}
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// A home made before the role guidance gets docs/ROLES.md and the START.md
// that points to it on its next rerun, which reports restart_required; a
// ROLES.md the agent edited is kept, with the new version beside it
// (pilot G2).
func TestJoinAddsRoleGuidance(t *testing.T) {
	e := setupJoin(t)
	code := e.invite(t, "inv-1", store.RoleWorker)
	e.join(t, e.opts(), &recCrew{}, activeAimem(), code)
	path := func(p string) string { return filepath.Join(e.home, filepath.FromSlash(p)) }

	// Back to a home of the previous release: the older START.md, recorded
	// as its last managed write, and no ROLES.md.
	olderStart := strings.Replace(startMD, "6. **Your role.**", "", 1)
	os.WriteFile(path("docs/START.md"), []byte(olderStart), 0o644)
	os.Remove(path("docs/ROLES.md"))
	doc := readJSON(t, path("agent.json"))
	managed := doc["managed"].(map[string]any)
	managed["docs/START.md"] = digestOf([]byte(olderStart))
	delete(managed, "docs/ROLES.md")
	raw, _ := json.Marshal(doc)
	os.WriteFile(path("agent.json"), raw, 0o644)

	rep, _ := e.join(t, JoinOptions{Home: e.home}, &recCrew{}, activeAimem())
	if rep.Status != JoinRestartRequired {
		t.Fatalf("rerun of an older home: %+v", rep)
	}
	want := map[string]string{"docs/START.md": "update", "docs/ROLES.md": "create"}
	for _, c := range rep.Changes {
		if w, ok := want[c.Path]; ok && w != c.Action {
			t.Fatalf("%s: %s, want %s", c.Path, c.Action, w)
		}
	}
	start, _ := os.ReadFile(path("docs/START.md"))
	roles, _ := os.ReadFile(path("docs/ROLES.md"))
	if string(start) != startMD || !strings.Contains(string(start), "docs/ROLES.md") || string(roles) != rolesMD() {
		t.Fatal("the rerun did not write the role guidance and the START.md that points to it")
	}

	edited := string(roles) + "\nMy own note.\n"
	os.WriteFile(path("docs/ROLES.md"), []byte(edited), 0o644)
	rep, _ = e.join(t, JoinOptions{Home: e.home}, &recCrew{}, activeAimem())
	for _, c := range rep.Changes {
		if c.Path == "docs/ROLES.md" && c.Action != "conflict" {
			t.Fatalf("an edited ROLES.md: %s, want conflict", c.Action)
		}
	}
	if b, _ := os.ReadFile(path("docs/ROLES.md")); string(b) != edited {
		t.Fatal("the agent's edit of ROLES.md was overwritten")
	}
	if b, _ := os.ReadFile(path("docs/ROLES.md.aicrew-new")); string(b) != rolesMD() {
		t.Fatal("no new version beside the edited ROLES.md")
	}
}
