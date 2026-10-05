//go:build realaimem

package realaimem

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/BlackVS/aicrew/internal/optoken"
)

// bootstrap brings up the hub and aicrewd and provisions the team, in the
// pilot runbook's order (aimem docs/PILOT-HUB-RUNBOOK.md at the pin):
//  1. TLS: the run's CA, a leaf for the hub and one for aicrewd;
//  2. the hub, terminating TLS itself;
//  3. the project (tasks on, process selected, its repository bound);
//  4. aicrew registered as the identity peer, with the hub's ID read back;
//  5. aicrew's four aimem credentials (identity.redeem, reservation.read,
//     team.register, team.read);
//  6. aicrewd, started with that hub as a named aimem block, and through
//     its operator API with `aicrew`: the team on that hub, which aicrewd
//     registers there (team.register), and the hub's outbound credential
//     (introspection and coordination);
//  7. the team's grant, by the name aicrewd registered: the team's only
//     project set, which aicrewd reads live at every offer and claim;
//  8. the members' aimem users and tokens, and their aicrew invitations;
//  9. the reconciliation loop, and the peer check end to end;
//  10. each member's aimem client, `aicrew-agent join`, and its launcher.
func (h *harness) bootstrap(specs ...memberSpec) {
	t := h.t
	t.Helper()
	h.newCA()
	h.forge = h.startForge()
	hubCert, hubKey, hubPin := h.leaf("hub")
	aCert, aKey, aPin := h.leaf("aicrewd")
	h.hubPin, h.aicrewdPin = hubPin, aPin
	h.hubPort, h.aicrewdPort = freePort(t), freePort(t)
	h.hubURL = fmt.Sprintf("https://127.0.0.1:%d", h.hubPort)
	h.aicrewdURL = fmt.Sprintf("https://127.0.0.1:%d", h.aicrewdPort)
	// The fault proxies (b4b-1): the members reach the hub, and the members
	// and the hub reach aicrewd, through them. Each presents its target's
	// own run key, so the members' CA trust and the SPKI pins still hold.
	h.hubProxy = h.newFaultProxy("hub", hubCert, hubKey, h.hubURL)
	h.aicrewdProxy = h.newFaultProxy("aicrewd", aCert, aKey, h.aicrewdURL)
	h.adminToken = secret(t, "e2e-admin-")
	h.adminFile = filepath.Join(h.root, "admin.token")
	h.knowSecret(h.adminToken)
	h.writePrivate(h.adminFile, []byte(h.adminToken+"\n"))

	// 2. The hub.
	hubDir := h.mkdir(filepath.Join(h.root, "hub"))
	introFile := filepath.Join(hubDir, "introspection.token") // written by aicrew in step 5
	hubEnv := h.isolatedEnv(hubDir,
		fmt.Sprintf("AIMEM_HTTP_LISTEN=127.0.0.1:%d", h.hubPort),
		"AIMEM_HTTP_TOKEN="+h.adminToken,
		"AIMEM_TLS_CERT="+hubCert, "AIMEM_TLS_KEY="+hubKey,
		"AIMEM_INTROSPECTION_TOKEN_FILE="+introFile)
	h.hubProc = h.start("aimem-hub", hubEnv, hubDir, filepath.Join(h.bin, "aimem"), "serve")
	h.waitFor("the hub's TLS listener", 60*time.Second, func() bool {
		resp, err := h.httpClient.Get(h.hubURL + "/v1/status")
		if err != nil {
			return false
		}
		resp.Body.Close()
		return resp.StatusCode == http.StatusOK
	})
	// The host-side commands reach the hub through its local socket.
	host := h.hostEnv()

	// 3. The project, and its process: a real repository, so the instruction
	// digest is the manifest's at the pinned commit.
	h.makeProcess()
	h.must(host, nil, h.aimem(), "tasks", "on", "-p", projectID)
	h.must(host, nil, h.aimem(), "process", "select", processRepo, processCommit, processManifest, "-p", projectID)
	// The project's repository: recorded, never fetched; every offer and
	// claim names it, and aicrewd checks it against the hub's.
	h.must(host, nil, h.aimem(), "project", "repo", "set", "--project", projectID, "--kind", repoKind, "--url", h.forge.repoURL(),
		"--access", "write")

	// 4. The peer, and the hub's ID.
	h.must(host, nil, h.identity("peer", "register", serviceID,
		"--endpoint", h.aicrewdProxy.url+"/v1/crew/introspect", "--peer-trust-pin", h.aicrewdPin)...)
	list := h.must(host, nil, h.identity("peer", "list")...)
	m := regexp.MustCompile(`(?m)^` + regexp.QuoteMeta(serviceID) + `\s+enabled\s+hub\s+(\S+)`).FindStringSubmatch(list)
	if m == nil {
		t.Fatalf("the hub's ID is not in the peer list:\n%s", list)
	}
	h.hubID = m[1]

	// 5. aicrew's aimem credentials, one per operation.
	aDir := h.mkdir(filepath.Join(h.root, "aicrewd"))
	h.storePath = filepath.Join(aDir, "aicrew.db")
	redeem, read := filepath.Join(aDir, "redeem.secret"), filepath.Join(aDir, "read.secret")
	register, teamRead := filepath.Join(aDir, "team-register.secret"), filepath.Join(aDir, "team-read.secret")
	for op, file := range map[string]string{"identity.redeem": redeem, "reservation.read": read, "team.register": register,
		"team.read": teamRead} {
		h.must(host, nil, h.identity("cred", "issue", serviceID, "--expires", "30d", "--output", file, "--operation", op)...)
		h.knowSecretFile(file)
	}

	// 6. aicrewd starts with the hub as a named aimem block; aicrewd reaches
	// the hub through the fault proxy too (F6 holds its reads back), and the
	// proxy presents the hub's own run key. The operator administers through
	// its API, with `aicrew`, while it runs: the team on that hub, then
	// aimem's introspection credential.
	aEnv := h.isolatedEnv(aDir)
	opToken, err := optoken.Generate()
	if err != nil {
		t.Fatal(err)
	}
	opFile := filepath.Join(aDir, "operator.token")
	if err := optoken.Write(opFile, opToken); err != nil {
		t.Fatal(err)
	}
	h.knowSecretFile(opFile)
	cfg := map[string]any{
		"store_path": h.storePath, "listen_addr": fmt.Sprintf("127.0.0.1:%d", h.aicrewdPort),
		"tls_cert_file": aCert, "tls_key_file": aKey, "service_id": serviceID, "operator_token_file": opFile,
		"aimem_hubs": []map[string]any{{"name": hubName, "hub_id": h.hubID, "base_url": h.hubProxy.url,
			"tls_trust_mode": "spki_sha256", "tls_trust_value": h.hubPin, "redemption_token_file": redeem,
			"read_token_file": read, "team_register_token_file": register, "team_read_token_file": teamRead}},
	}
	cfgPath := filepath.Join(aDir, "aicrewd.json")
	h.startAicrewd(aEnv, aDir, cfgPath, cfg)
	opEnv := append(append([]string{}, aEnv...), "AICREW_URL="+h.aicrewdURL, "AICREW_TLS_TRUST_MODE=spki_sha256",
		"AICREW_TLS_TRUST_VALUE="+h.aicrewdPin, "AICREW_OPERATOR_TOKEN_FILE="+opFile)
	h.checkIsolated(opEnv)
	var team struct {
		ID           string `json:"id"`
		Registration *struct {
			State string `json:"state"`
		} `json:"registration"`
	}
	if err := json.Unmarshal([]byte(h.must(opEnv, nil, filepath.Join(h.bin, "aicrew"), "team", "create",
		"-name", "e2e", "-hub", hubName)), &team); err != nil || team.ID == "" {
		t.Fatalf("aicrew team create printed no team: %v", err)
	}
	if team.Registration == nil || team.Registration.State != "registered" {
		t.Fatalf("aicrewd did not register the team on the hub: %+v", team.Registration)
	}
	h.teamID = team.ID
	h.must(opEnv, nil, filepath.Join(h.bin, "aicrew"), "hub-credential", "issue",
		"-hub", h.hubID, "--output", introFile)
	h.knowSecretFile(introFile)

	// 7. The team's grant: the profile is the one aicrewd registered, named
	// by the team's name within the peer.
	h.must(host, nil, h.identity("team", "grant", "--peer", serviceID, "--team-name", "e2e", "--project", projectID)...)

	// 8. The members' users, tokens and invitations.
	var mems []*member
	for _, sp := range specs {
		mems = append(mems, h.prepareMember(sp, opEnv))
	}

	// 9. The reconciliation loop, and the peer check end to end.
	h.waitFor("aicrewd's reconciliation loop", 10*time.Second, func() bool {
		b, _ := os.ReadFile(h.aicrewdLog)
		return bytes.Contains(b, []byte("reconcile: started"))
	})
	out := h.must(host, nil, h.identity("peer", "check", serviceID)...)
	if !strings.Contains(out, "introspection works") {
		t.Fatalf("the hub's introspection of aicrewd: %s", out)
	}

	// 10. The members join.
	for _, mem := range mems {
		h.joinMember(mem)
	}
}

// startAicrewd writes aicrewd's configuration and starts it, waiting for its
// listener.
func (h *harness) startAicrewd(env []string, dir, cfgPath string, cfg map[string]any) {
	h.t.Helper()
	raw, _ := json.MarshalIndent(cfg, "", "  ")
	os.Remove(cfgPath)
	h.writePrivate(cfgPath, raw)
	h.aicrewdProc = h.start("aicrewd", env, dir, filepath.Join(h.bin, "aicrewd"), "-config", cfgPath)
	h.aicrewdLog = h.aicrewdProc.log
	h.waitFor("aicrewd's listener", 30*time.Second, func() bool {
		resp, err := h.httpClient.Get(h.aicrewdURL + "/healthz")
		if err != nil {
			return false
		}
		resp.Body.Close()
		return resp.StatusCode == http.StatusOK
	})
}

// memberSpec names a member and its team role.
type memberSpec struct{ name, role string }

func (h *harness) aimem() string { return filepath.Join(h.bin, "aimem") }

// hostEnv is the hub host's environment: the hub's state and socket, for
// `aimem tasks`, `process` and `access`, and the identity commands.
func (h *harness) hostEnv() []string {
	return h.isolatedEnv(filepath.Join(h.root, "hub"))
}

// identity is an `aimem identity …` command over the hub's TLS listener,
// as the hub's admin.
func (h *harness) identity(args ...string) []string {
	return append(append([]string{h.aimem(), "identity"}, args...),
		"--hub", h.hubURL, "--admin-token-file", h.adminFile, "--hub-ca-file", h.caFile)
}

// prepareMember provisions one member as the runbook's step 6 and aicrew's
// onboarding do: an aimem user with an admin-issued user token, and an
// aicrew invitation pinned to that user, issued through aicrewd's operator
// API while it runs.
// The token and the code stay in memory.
func (h *harness) prepareMember(sp memberSpec, aEnv []string) *member {
	t := h.t
	t.Helper()
	name, role := sp.name, sp.role
	host := h.hostEnv()
	var user struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal([]byte(h.must(host, nil, h.aimem(), "access", "user-add", "e2e-"+name)), &user); err != nil || user.ID == "" {
		t.Fatalf("user-add %s: %v", name, err)
	}
	var tok struct {
		Secret string `json:"secret"`
		Token  struct {
			Scope string `json:"scope"`
		} `json:"token"`
	}
	expiry := time.Now().Add(30 * 24 * time.Hour).UTC().Format(time.RFC3339)
	if err := json.Unmarshal([]byte(h.must(host, nil, h.aimem(), "access", "token-issue-user", user.ID, "e2e-"+name, expiry)), &tok); err != nil ||
		tok.Token.Scope != "user" || !strings.HasPrefix(tok.Secret, "aimem_user_") {
		t.Fatalf("token-issue-user %s: scope %q %v", name, tok.Token.Scope, err)
	}

	dir := h.mkdir(filepath.Join(h.root, "m", name))
	mem := &member{name: name, role: role, dir: dir, userID: user.ID, home: filepath.Join(dir, "a"), token: tok.Secret}
	h.knowSecret(tok.Secret)
	// The member's home reads the run's forge under the run's CA.
	mem.env = h.isolatedEnv(dir, "SSL_CERT_FILE="+h.caFile)
	// The member's aimem installation is the one in its agent home (D-STORE),
	// which aicrew-agent gives every aimem process it starts there: the
	// operator provisions it with the home's two variables.
	installation := h.mkdir(filepath.Join(mem.home, "aimem"))
	for i, kv := range mem.env {
		switch k, _, _ := strings.Cut(kv, "="); k {
		case "AIMEM_STATE_DIR":
			mem.env[i] = k + "=" + installation
		case "AIMEM_SOCKET":
			mem.env[i] = k + "=" + filepath.Join(installation, "aimem.sock")
		}
	}
	h.checkIsolated(mem.env)
	codeFile := filepath.Join(h.mkdir(filepath.Join(h.root, "codes")), name+".code")
	h.must(aEnv, nil, filepath.Join(h.bin, "aicrew"), "invitation", "issue",
		"-team", h.teamID, "-role", role, "-hub", h.hubID, "-label", name,
		"-expect-user", user.ID, "--output", codeFile)
	code, err := os.ReadFile(codeFile)
	if err != nil {
		t.Fatal(err)
	}
	os.Remove(codeFile)
	mem.code = strings.TrimSpace(string(code))
	h.knowSecret(mem.code)
	return mem
}

// joinMember gives the member's aimem the hub, runs the real
// `aicrew-agent join` at a terminal, and starts the member's launcher,
// whose step channel the scenarios use.
func (h *harness) joinMember(mem *member) {
	t := h.t
	t.Helper()
	name := mem.name
	// join provisions the member's aimem installation in its home itself:
	// the hub, with the run's CA copied into the home, and the member's user
	// token as its checkpoint and task credential, read from an owner-only
	// file and given to aimem on its standard input, never as an argument.
	tokenFile := filepath.Join(mem.dir, "member.token")
	h.writePrivate(tokenFile, []byte(mem.token+"\n"))
	h.knowSecretFile(tokenFile)
	mem.token = ""
	// The member's own forge token, verified and written by join (--cred).
	forgeTok := h.forge.member(name)
	h.knowSecret(forgeTok)
	forgeFile := filepath.Join(mem.dir, "forge.token")
	h.writePrivate(forgeFile, []byte(forgeTok+"\n"))
	h.join(mem, mem.code, "-aimem-url", h.hubProxy.url, "-aimem-token-file", tokenFile, "-aimem-ca-file", h.caFile,
		"--cred", h.forge.host+"="+forgeFile)
	mem.code = ""
	os.Remove(tokenFile)
	os.Remove(forgeFile)
	var cred struct {
		Credential string `json:"credential"`
		State      string `json:"state"`
	}
	if out := h.must(mem.env, nil, h.aimem(), "hub", "credential", hubName, "--json"); json.Unmarshal([]byte(out), &cred) != nil ||
		cred.Credential != "set" || cred.State != "active" {
		t.Fatalf("%s's aimem credential: %s", name, out)
	}

	// The launcher's client is a stand-in that only keeps the session open:
	// the scenarios drive the steps through its channel.
	h.setClientCommand(mem, "/bin/sleep")
	mem.launcher = h.start("launcher-"+name, mem.env, mem.home, filepath.Join(h.bin, "aicrew-agent"),
		"run", "-client", "claude", "-home", mem.home, "--", "86400")
	h.waitFor(name+"'s step channel", 60*time.Second, func() bool {
		r := h.run(mem.env, nil, filepath.Join(h.bin, "aicrew-agent"), "step", "pending", "-home", mem.home)
		return r.code == 0
	})
	h.members[name] = mem
}

// join runs `aicrew-agent join` at a pseudo-terminal and types the code at
// its hidden prompt.
func (h *harness) join(mem *member, code string, extra ...string) {
	t := h.t
	t.Helper()
	master, slave, err := openPTY()
	if err != nil {
		t.Fatalf("a terminal for join: %v", err)
	}
	defer master.Close()
	args := append([]string{"join", "-label", mem.name, "-home", mem.home,
		"-url", h.aicrewdProxy.url, "-tls-trust-mode", "spki_sha256", "-tls-trust-value", h.aicrewdPin,
		"-aimem-hub", hubName, "-aimem-command", filepath.Join(h.bin, "aimem-timed"), "-client", "claude", "-json"}, extra...)
	cmd := exec.Command(filepath.Join(h.bin, "aicrew-agent"), args...)
	cmd.Env = mem.env
	cmd.Stdin = slave
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &lockedBuffer{b: &errb}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	slave.Close()
	go func() { // drain the terminal's output
		buf := make([]byte, 4096)
		for {
			if _, err := master.Read(buf); err != nil {
				return
			}
		}
	}()
	deadline := time.Now().Add(90 * time.Second)
	for !strings.Contains(cmd.Stderr.(*lockedBuffer).String(), "Invitation code: ") {
		if time.Now().After(deadline) {
			cmd.Process.Kill()
			t.Fatalf("%s's join showed no prompt:\nstdout: %s\nstderr: %s", mem.name, out.String(), cmd.Stderr.(*lockedBuffer).String())
		}
		time.Sleep(200 * time.Millisecond)
	}
	if _, err := master.Write([]byte(code + "\n")); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		// A blocked join exits 1 with its report; the report decides.
		if err != nil && !json.Valid(out.Bytes()) {
			t.Fatalf("join %s: %v\n%s\n%s", mem.name, err, out.String(), cmd.Stderr.(*lockedBuffer).String())
		}
	case <-time.After(3 * time.Minute):
		cmd.Process.Kill()
		t.Fatalf("join %s did not finish", mem.name)
	}
	var rep struct {
		Status   string `json:"status"`
		AgentID  string `json:"agent_id"`
		Redeemed bool   `json:"redeemed"`
		Check    *struct {
			Status     string `json:"status"`
			Reason     string `json:"reason"`
			Components []struct {
				Name  string `json:"name"`
				State string `json:"state"`
			} `json:"components"`
		} `json:"check"`
	}
	if err := json.Unmarshal(out.Bytes(), &rep); err != nil || !rep.Redeemed || rep.AgentID == "" {
		t.Fatalf("join %s did not redeem and link the home: %v %s", mem.name, err, out.String())
	}
	// The home is linked before join checks its dependencies and client
	// (1a81-5a). The run's client is a stand-in, not Claude Code, so a check
	// blocked on the client alone is expected; anything else is not.
	switch {
	case rep.Status == "ready" || rep.Status == "restart_required":
	case rep.Status == "blocked" && rep.Check != nil && strings.HasPrefix(rep.Check.Reason, "claude_"):
		for _, c := range rep.Check.Components {
			if c.Name == "aimem" && c.State != "supported" && c.State != "newer" && c.State != "unknown" {
				t.Fatalf("join %s: the check judged aimem %s: %s", mem.name, c.State, out.String())
			}
		}
		mem.checkNote = "linked; the client check is blocked on the stand-in client (" + rep.Check.Reason + ")"
	default:
		t.Fatalf("join %s: %s", mem.name, out.String())
	}
	cfg := h.agentConfig(mem)
	mem.agentID, _ = cfg["agent_id"].(string)
	if mem.agentID == "" {
		t.Fatalf("%s's agent.json names no agent", mem.name)
	}
}

func (h *harness) agentConfig(mem *member) map[string]any {
	raw, err := os.ReadFile(filepath.Join(mem.home, "agent.json"))
	if err != nil {
		h.t.Fatal(err)
	}
	var f map[string]any
	if err := json.Unmarshal(raw, &f); err != nil {
		h.t.Fatal(err)
	}
	sec, _ := f["aicrew"].(map[string]any)
	if sec == nil {
		h.t.Fatalf("%s's agent.json has no aicrew section", mem.name)
	}
	return sec
}

// setClientCommand sets the launcher's client in the agent home's aicrew
// section (a documented field).
func (h *harness) setClientCommand(mem *member, path string) {
	file := filepath.Join(mem.home, "agent.json")
	raw, err := os.ReadFile(file)
	if err != nil {
		h.t.Fatal(err)
	}
	var f map[string]any
	if err := json.Unmarshal(raw, &f); err != nil {
		h.t.Fatal(err)
	}
	f["aicrew"].(map[string]any)["client_command"] = path
	out, _ := json.MarshalIndent(f, "", "  ")
	info, _ := os.Stat(file)
	if err := os.WriteFile(file, out, info.Mode().Perm()); err != nil {
		h.t.Fatal(err)
	}
}

// restartAicrewd starts aicrewd again on its store and configuration.
func (h *harness) restartAicrewd() {
	h.t.Helper()
	old := h.aicrewdProc
	old.stop(20 * time.Second)
	h.aicrewdProc = h.start("aicrewd", old.cmd.Env, old.cmd.Dir, old.cmd.Args...)
	h.aicrewdLog = h.aicrewdProc.log
	h.waitFor("aicrewd's listener", 30*time.Second, func() bool {
		resp, err := h.httpClient.Get(h.aicrewdURL + "/healthz")
		if err != nil {
			return false
		}
		resp.Body.Close()
		return resp.StatusCode == http.StatusOK
	})
}

// makeProcess commits a process manifest to a repository of its own and
// sets processCommit and instructionHash from it.
func (h *harness) makeProcess() {
	h.t.Helper()
	h.processDir = h.mkdir(filepath.Join(h.root, "process"))
	if err := os.MkdirAll(filepath.Join(h.processDir, "process"), 0o755); err != nil {
		h.t.Fatal(err)
	}
	manifest := `{
  "name": "e2e process",
  "roles": ["coordinator", "worker", "independent"]
}
`
	if err := os.WriteFile(filepath.Join(h.processDir, filepath.FromSlash(processManifest)), []byte(manifest), 0o644); err != nil {
		h.t.Fatal(err)
	}
	env := h.isolatedEnv(h.processDir)
	git := func(args ...string) string {
		return strings.TrimSpace(h.must(env, nil, append([]string{"git", "-C", h.processDir, "-c", "user.name=e2e",
			"-c", "user.email=e2e@example.test", "-c", "commit.gpgsign=false"}, args...)...))
	}
	git("init", "-q")
	git("add", processManifest)
	git("commit", "-q", "-m", "process")
	processCommit = git("rev-parse", "HEAD")
	instructionHash = h.processDigest(processCommit, processManifest)
}

// processDigest is the instruction digest of the process at commit, as a
// member computes it: sha256 of the manifest's exact bytes there.
func (h *harness) processDigest(commit, manifest string) string {
	h.t.Helper()
	out, err := exec.Command("git", "-C", h.processDir, "show", commit+":"+manifest).Output()
	if err != nil {
		h.t.Fatalf("read the manifest at %s: %v", commit, err)
	}
	sum := sha256.Sum256(out)
	return "sha256:" + hex.EncodeToString(sum[:])
}
