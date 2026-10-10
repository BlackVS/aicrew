package agent

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/BlackVS/aicrew/internal/store"
)

// The dependency and client check (1a81-5a). This test binary stands in for
// aimem, claude and opencode: it is linked into a directory under those
// names and behaves as the real tools did when measured (Claude Code 2.1.286,
// OpenCode 1.18.32 and 2.0.19), configured by fakeToolsEnv.

const fakeToolsEnv = "AICREW_FAKE_TOOLS"

// fakeTools configures the stand-ins.
type fakeTools struct {
	Aimem string `json:"aimem,omitempty"` // `aimem version` output; "" fails
	// Credential is the home installation's credential for its hub: ""
	// (active), "none", "refused", "unreachable" or "unconfigured".
	Credential    string `json:"credential,omitempty"`
	Claude        string `json:"claude,omitempty"` // `claude --version`
	ClaudeMCP     string `json:"claude_mcp,omitempty"`
	ClaudePending bool   `json:"claude_pending,omitempty"`
	ClaudeHang    bool   `json:"claude_hang,omitempty"`
	// ClaudeRetries: after the init event the run keeps retrying its model
	// request, as Claude Code does against the sink, and never ends by
	// itself. On a SIGTERM it writes TermMark and exits.
	ClaudeRetries bool   `json:"claude_retries,omitempty"`
	TermMark      string `json:"term_mark,omitempty"`
	OpenCode      string `json:"opencode,omitempty"` // `opencode --version`
	OpenCodeMCP   string `json:"opencode_mcp,omitempty"`
	OpenCodeDB    bool   `json:"opencode_db,omitempty"` // 1.x refuses data 2 wrote
}

func init() {
	raw := os.Getenv(fakeToolsEnv)
	if raw == "" {
		return
	}
	var f fakeTools
	if json.Unmarshal([]byte(raw), &f) != nil {
		os.Exit(70)
	}
	name := strings.TrimSuffix(filepath.Base(os.Args[0]), ".exe")
	args := os.Args[1:]
	if name == "aimem" || name == "claude" || name == "opencode" {
		guardOrExit(name)
		if v, ok := os.LookupEnv(SessionEnv); ok {
			fmt.Fprintf(os.Stderr, "%s: inherited team session %q\n", name, v)
			os.Exit(97)
		}
	}
	switch name {
	case "aimem":
		if len(args) == 1 && args[0] == "version" && f.Aimem != "" {
			fmt.Println(f.Aimem)
			os.Exit(0)
		}
		if len(args) == 4 && args[0] == "hub" && args[1] == "credential" && args[3] == "--json" {
			os.Exit(fakeCredential(f.Credential, args[2]))
		}
		fmt.Fprintln(os.Stderr, "aimem: broken")
		os.Exit(1)
	case "claude":
		os.Exit(fakeClaude(f, args))
	case "opencode":
		os.Exit(fakeOpenCode(f, args))
	}
}

// fakeCredential answers `aimem hub credential HUB --json`.
func fakeCredential(mode, hub string) int {
	cred, state := "set", "active"
	switch mode {
	case "unconfigured":
		fmt.Fprintf(os.Stderr, "hub %q is not configured on this machine\n", hub)
		return 1
	case "none":
		cred, state = "none", "absent"
	case "refused", "unreachable":
		state = mode
	}
	fmt.Printf(`{"hub": %q, "credential": %q, "state": %q, "scope": "user", "user_id": "user-1"}`+"\n", hub, cred, state)
	return 0
}

// providerGuard refuses to run a client stand-in whose model endpoint is not
// the check's local sink, or that inherited the calling session's variables.
func providerGuard() error {
	u, err := url.Parse(os.Getenv("ANTHROPIC_BASE_URL"))
	if err != nil || u.Hostname() != "127.0.0.1" {
		return fmt.Errorf("model endpoint %q is not local", os.Getenv("ANTHROPIC_BASE_URL"))
	}
	if os.Getenv("ANTHROPIC_API_KEY") != "dummy-not-a-key" {
		return errors.New("the API key is not the dummy")
	}
	for _, kv := range os.Environ() {
		if strings.HasPrefix(strings.ToUpper(kv), "CLAUDE") && !strings.HasPrefix(strings.ToUpper(kv), "CLAUDE_CONFIG_DIR=") {
			return fmt.Errorf("inherited %s", kv[:strings.IndexByte(kv, '=')])
		}
	}
	// The model request a real client makes after discovery must fail.
	if c, err := net.DialTimeout("tcp", u.Host, time.Second); err == nil {
		c.SetDeadline(time.Now().Add(time.Second))
		fmt.Fprintf(c, "POST /v1/messages HTTP/1.1\r\nHost: x\r\n\r\n")
		if n, _ := c.Read(make([]byte, 1)); n > 0 {
			c.Close()
			return errors.New("the model endpoint answered")
		}
		c.Close()
	}
	return nil
}

// skillsSeen lists the skills a client would find: the home's and the
// user's skills directories, the user's under CLAUDE_CONFIG_DIR when set,
// as Claude Code reads them.
func skillsSeen() []string {
	cwd, _ := os.Getwd()
	uh, _ := os.UserHomeDir()
	user := filepath.Join(uh, ".claude")
	if d := os.Getenv("CLAUDE_CONFIG_DIR"); d != "" {
		user = d
	}
	var out []string
	for _, d := range []string{filepath.Join(cwd, ".claude", "skills"), filepath.Join(user, "skills")} {
		ents, _ := os.ReadDir(d)
		for _, e := range ents {
			if _, err := os.Stat(filepath.Join(d, e.Name(), "SKILL.md")); err == nil && !slices.Contains(out, e.Name()) {
				out = append(out, e.Name())
			}
		}
	}
	return out
}

func entryIn(file, parent string) bool {
	raw, err := os.ReadFile(file)
	if err != nil {
		return false
	}
	var doc map[string]map[string]json.RawMessage
	_ = json.Unmarshal(raw, &doc)
	_, ok := doc[parent]["aimem"]
	return ok
}

func fakeClaude(f fakeTools, args []string) int {
	if len(args) == 1 && args[0] == "--version" {
		fmt.Println(f.Claude)
		return 0
	}
	if err := providerGuard(); err != nil {
		fmt.Fprintln(os.Stderr, "fake claude:", err)
		return 9
	}
	if len(args) == 2 && args[0] == "mcp" && args[1] == "list" {
		fmt.Println("Checking MCP server health…\n\nclaude.ai Docs: https://example.invalid/mcp - ✔ Connected")
		if entryIn(".mcp.json", "mcpServers") {
			if f.ClaudePending {
				fmt.Println("aimem: aimem mcp - ⏸ Pending approval (run `claude` to approve)")
			} else {
				fmt.Println("aimem: aimem mcp - ✔ Connected")
			}
		}
		return 0
	}
	if len(args) > 0 && args[0] == "-p" {
		if f.ClaudeHang {
			time.Sleep(time.Hour)
		}
		servers := []map[string]string{}
		if entryIn(".mcp.json", "mcpServers") {
			st := f.ClaudeMCP
			if st == "" {
				st = "connected"
			}
			servers = append(servers, map[string]string{"name": "aimem", "status": st, "source": "project"})
		}
		init, _ := json.Marshal(map[string]any{"type": "system", "subtype": "init", "mcp_servers": servers,
			"skills": skillsSeen(), "tools": []string{}})
		fmt.Println(string(init))
		if f.ClaudeRetries {
			waitForTerm(f.TermMark)
		}
		fmt.Println(`{"type":"result","subtype":"error_during_execution","is_error":true}`)
		return 1
	}
	fmt.Fprintf(os.Stderr, "fake claude: unexpected %q\n", args)
	return 3
}

func fakeOpenCode(f fakeTools, args []string) int {
	if len(args) == 1 && args[0] == "--version" {
		fmt.Println(f.OpenCode)
		return 0
	}
	if err := providerGuard(); err != nil {
		fmt.Fprintln(os.Stderr, "fake opencode:", err)
		return 9
	}
	if f.OpenCodeDB {
		fmt.Fprintln(os.Stderr, "Error: Database is not empty and has no session table")
		return 1
	}
	status := f.OpenCodeMCP
	if status == "" {
		status = "connected"
	}
	switch {
	case len(args) == 2 && args[0] == "mcp" && args[1] == "list":
		fmt.Println("┌  MCP Servers\n│")
		if entryIn("opencode.json", "mcp") {
			if status == "connected" {
				fmt.Println("●  \x1b[32m✓\x1b[0m aimem \x1b[2mconnected\x1b[0m\n│      aimem mcp")
			} else {
				fmt.Println("●  ✗ aimem failed\n│      MCP error -32000: Connection closed")
			}
		}
		fmt.Println("│\n└  1 server(s)")
		return 0
	case len(args) == 2 && args[0] == "debug" && args[1] == "skill":
		var list []map[string]string
		for _, s := range skillsSeen() {
			list = append(list, map[string]string{"name": s, "location": "x"})
		}
		raw, _ := json.Marshal(list)
		fmt.Printf("INFO loading\n%s\n", raw)
		return 0
	case len(args) == 3 && args[0] == "serve" && args[1] == "--port":
		return fakeServe(args[2], status)
	}
	fmt.Fprintf(os.Stderr, "fake opencode: unexpected %q\n", args)
	return 3
}

// fakeServe is OpenCode 2's server: its catalogs load after two polls.
func fakeServe(port, status string) int {
	polls := 0
	cwd, _ := os.Getwd()
	mux := http.NewServeMux()
	auth := func(w http.ResponseWriter, r *http.Request) bool {
		u, p, ok := r.BasicAuth()
		if !ok || u != "opencode" || p != "pw-123" {
			w.WriteHeader(http.StatusUnauthorized)
			return false
		}
		// The same directory under another name (macOS's /var is a link to
		// /private/var) is the same directory.
		dir, err := filepath.EvalSymlinks(r.URL.Query().Get("location[directory]"))
		self, _ := filepath.EvalSymlinks(cwd)
		if err != nil || dir != self {
			http.Error(w, "unknown directory", http.StatusNotFound)
			return false
		}
		return true
	}
	mux.HandleFunc("/api/skill", func(w http.ResponseWriter, r *http.Request) {
		if !auth(w, r) {
			return
		}
		polls++
		data := []map[string]string{}
		if polls > 2 {
			for _, s := range skillsSeen() {
				data = append(data, map[string]string{"id": s})
			}
		}
		json.NewEncoder(w).Encode(map[string]any{"data": data})
	})
	mux.HandleFunc("/api/mcp", func(w http.ResponseWriter, r *http.Request) {
		if !auth(w, r) {
			return
		}
		data := []map[string]any{}
		if entryIn("opencode.json", "mcp") {
			st := map[string]string{"status": status}
			if status != "connected" {
				st["error"] = "Connection closed"
			}
			data = append(data, map[string]any{"name": "aimem", "status": st})
		}
		json.NewEncoder(w).Encode(map[string]any{"data": data})
	})
	ln, err := net.Listen("tcp", "127.0.0.1:"+port)
	if err != nil {
		return 4
	}
	fmt.Println("opencode server listening\nserver password pw-123")
	http.Serve(ln, mux)
	return 0
}

// checkEnv is a scratch agent home, user home and PATH of stand-ins.
type checkEnv struct {
	home, user, bin string
}

func setupCheck(t *testing.T, f fakeTools, tools ...string) *checkEnv {
	t.Helper()
	dir := t.TempDir()
	e := &checkEnv{home: filepath.Join(dir, "agents", "builder"), user: filepath.Join(dir, "user"), bin: filepath.Join(dir, "bin")}
	for _, d := range []string{e.home, e.user, e.bin} {
		os.MkdirAll(d, 0o755)
	}
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	ext := ""
	if runtime.GOOS == "windows" {
		ext = ".exe"
	}
	for _, name := range tools {
		dst := filepath.Join(e.bin, name+ext)
		// A running executable's hard link cannot be removed on Windows.
		if runtime.GOOS == "windows" || os.Link(self, dst) != nil {
			raw, err := os.ReadFile(self)
			if err != nil || os.WriteFile(dst, raw, 0o755) != nil {
				t.Fatal("cannot place the stand-in", name)
			}
		}
	}
	t.Setenv("PATH", e.bin)
	t.Setenv("HOME", e.user)
	t.Setenv("USERPROFILE", e.user)
	t.Setenv("XDG_CONFIG_HOME", "")
	// The developer's own Claude Code directory must not reach the check:
	// unset, and restored afterwards.
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	os.Unsetenv("CLAUDE_CONFIG_DIR")
	raw, _ := json.Marshal(f)
	t.Setenv(fakeToolsEnv, string(raw))
	t.Setenv("CLAUDECODE", "1") // the calling session's variables must not reach a client
	foreignInstallation(t, e.home)
	t.Setenv(SessionEnv, filepath.Join(e.user, "foreign-session.json")) // another installation's session
	doc := `{"layout": 1, "label": "builder", "aicrew": {"url": "https://aicrew.example", "tls_trust_mode": "ca_dns",
		"tls_trust_value": "aicrew.example", "agent_id": "agent-1", "team_id": "team-1", "aimem_hub": "main"}}`
	os.WriteFile(filepath.Join(e.home, "agent.json"), []byte(doc), 0o600)
	// join's carrier for clients started in the home by hand.
	os.MkdirAll(filepath.Join(e.home, ".claude"), 0o755)
	os.WriteFile(filepath.Join(e.home, ".claude", "settings.json"), []byte(claudeSettings(e.home)), 0o644)
	return e
}

// installSkills puts skills at user level, with an installer record when
// version is set.
func (e *checkEnv) installSkills(t *testing.T, version string, skills ...string) {
	t.Helper()
	dir := filepath.Join(e.user, ".claude", "skills")
	for _, s := range skills {
		os.MkdirAll(filepath.Join(dir, s), 0o755)
		os.WriteFile(filepath.Join(dir, s, "SKILL.md"), []byte("---\nname: "+s+"\n---\n"), 0o644)
	}
	if version != "" {
		os.WriteFile(filepath.Join(dir, ".ai-skills.json"), []byte(`{"version": "`+version+`"}`), 0o644)
	}
}

func (e *checkEnv) check(t *testing.T, clients ...string) CheckReport {
	t.Helper()
	rep, err := Check(context.Background(), CheckOptions{Home: e.home, Clients: clients, Timeout: 20 * time.Second})
	if err != nil {
		t.Fatalf("check: %v", err)
	}
	return rep
}

// snapshot digests every file under a directory.
func snapshot(t *testing.T, dir string) map[string]string {
	t.Helper()
	out := map[string]string{}
	filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			raw, _ := os.ReadFile(p)
			sum := sha256.Sum256(raw)
			out[p] = hex.EncodeToString(sum[:])
		}
		return nil
	})
	return out
}

func component(rep CheckReport, name string) ComponentReport {
	for _, c := range rep.Components {
		if c.Name == name {
			return c
		}
	}
	for _, c := range rep.Clients {
		if c.Name == name {
			return c.ComponentReport
		}
	}
	return ComponentReport{}
}

func hasNotice(rep CheckReport, sub string) bool {
	for _, n := range rep.Notices {
		if strings.Contains(n, sub) {
			return true
		}
	}
	return false
}

func hasInstruction(rep CheckReport, sub string) bool {
	for _, n := range rep.Instructions {
		if strings.Contains(n, sub) {
			return true
		}
	}
	return false
}

var readyTools = fakeTools{Aimem: "aimem v0.10.0", Claude: "2.1.286 (Claude Code)", OpenCode: "1.18.32"}

// A CLAUDE_CONFIG_DIR in the invoking shell, as a member on a shared account
// has, does not leak into the fixture: the baseline home is still ready.
func TestCheckFixtureIgnoresShellClaudeConfigDir(t *testing.T) {
	shell := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", shell)
	e := setupCheck(t, readyTools, "aimem", "claude")
	if _, ok := os.LookupEnv("CLAUDE_CONFIG_DIR"); ok {
		t.Fatal("setupCheck kept the shell's CLAUDE_CONFIG_DIR")
	}
	e.installSkills(t, "1.26.1", "oh-code-review")
	if rep := e.check(t, "claude"); rep.Status != JoinRestartRequired || len(rep.Instructions) != 0 {
		t.Fatalf("%+v", rep)
	}
}

// A home with supported tools is wired and ready: the MCP entry is written
// into the home only, the client sees aimem's server and the required skill,
// the clients and the entry's digest are recorded, and nothing under the
// user's home changes. A rerun changes nothing.
func TestCheckReadyClaude(t *testing.T) {
	e := setupCheck(t, readyTools, "aimem", "claude")
	e.installSkills(t, "1.26.1", "oh-code-review")
	before := snapshot(t, e.user)
	rep := e.check(t, "claude")
	// The first check creates the wiring, so a client already open in the
	// home must restart; a rerun is plainly ready.
	if rep.Status != JoinRestartRequired || len(rep.Instructions) != 0 {
		t.Fatalf("%+v", rep)
	}
	for name, want := range map[string]string{"aimem": StateSupported, "ai-skills": StateSupported, "claude": StateSupported} {
		if got := component(rep, name).State; got != want {
			t.Fatalf("%s: %s, want %s (%+v)", name, got, want, rep)
		}
	}
	if rep.Clients[0].MCP != "connected" || len(rep.Clients[0].MissingSkills) != 0 || rep.Wiring[0].Action != "create" {
		t.Fatalf("client %+v wiring %+v", rep.Clients[0], rep.Wiring)
	}
	var mcp map[string]map[string]map[string]any
	raw, _ := os.ReadFile(filepath.Join(e.home, ".mcp.json"))
	if json.Unmarshal(raw, &mcp) != nil || mcp["mcpServers"]["aimem"]["command"] != "aimem" ||
		fmt.Sprint(mcp["mcpServers"]["aimem"]["args"]) != "[mcp]" ||
		fmt.Sprint(mcp["mcpServers"]["aimem"]["env"]) != fmt.Sprint(map[string]any{StateDirEnv: AimemDir(e.home), SocketEnv: AimemSocket(e.home)}) {
		t.Fatalf(".mcp.json %s", raw)
	}
	if _, err := os.Stat(filepath.Join(e.home, "opencode.json")); err == nil {
		t.Fatal("opencode.json written for a Claude-only home")
	}
	// check writes no hook and leaves join's settings.json as it is.
	if _, err := os.Stat(filepath.Join(e.home, ".claude", "settings.local.json")); err == nil {
		t.Fatal("a hook file settings.local.json was written")
	}
	if b, _ := os.ReadFile(filepath.Join(e.home, ".claude", "settings.json")); string(b) != claudeSettings(e.home) {
		t.Fatalf("settings.json is %s", b)
	}
	doc := readJSON(t, filepath.Join(e.home, "agent.json"))
	if fmt.Sprint(doc["clients"]) != "[claude]" || doc["managed"].(map[string]any)[".mcp.json#mcpServers.aimem"] == nil {
		t.Fatalf("agent.json %v", doc)
	}
	if after := snapshot(t, e.user); fmt.Sprint(after) != fmt.Sprint(before) {
		t.Fatalf("the user's home changed:\n%v\n%v", before, after)
	}
	rep = e.check(t) // the recorded client
	if rep.Status != JoinReady || rep.Wiring[0].Action != "unchanged" || fmt.Sprint(snapshot(t, e.user)) != fmt.Sprint(before) {
		t.Fatalf("rerun %+v", rep)
	}
}

// The project server that Claude Code has not yet approved for interactive
// use is reported: the check itself never waits for the trust prompt.
func TestCheckClaudeApprovalPending(t *testing.T) {
	f := readyTools
	f.ClaudePending = true
	e := setupCheck(t, f, "aimem", "claude")
	e.installSkills(t, "1.26.1", "oh-code-review")
	rep := e.check(t, "claude")
	if rep.Status != JoinRestartRequired || !rep.Clients[0].ApprovalPending || !hasNotice(rep, "trust the folder") {
		t.Fatalf("%+v", rep)
	}
}

// waitForTerm stands in for Claude Code retrying its model request: it
// blocks until a SIGTERM, then writes mark and exits.
func waitForTerm(mark string) {
	c := make(chan os.Signal, 1)
	signal.Notify(c, syscall.SIGTERM)
	select {
	case <-c:
		if mark != "" {
			os.WriteFile(mark, []byte("terminated"), 0o644)
		}
		os.Exit(143)
	case <-time.After(time.Hour):
	}
}

// The init probe ends the run once it has read the init event, instead of
// waiting for a run that never ends by itself (01a11c7d-1de3): with a
// 60-second ceiling, the check returns in seconds, and the client got its
// termination: a SIGTERM where there is one, a kill on Windows. Each step is
// reported with its elapsed time after the cold-start note, in order
// (01a11c79-b9a8).
func TestCheckEndsTheInitProbeAtTheInitEvent(t *testing.T) {
	f := readyTools
	f.ClaudeRetries = true
	f.TermMark = filepath.Join(t.TempDir(), "terminated")
	e := setupCheck(t, f, "aimem", "claude")
	e.installSkills(t, "1.26.1", "oh-code-review")
	var out bytes.Buffer
	start := time.Now()
	rep, err := Check(context.Background(), CheckOptions{Home: e.home, Clients: []string{"claude"}, Timeout: 60 * time.Second, Out: &out})
	took := time.Since(start)
	if err != nil || rep.Clients[0].MCP != "connected" || took > 20*time.Second {
		t.Fatalf("%+v, %v, after %s", rep, err, took)
	}
	if runtime.GOOS != "windows" {
		if b, _ := os.ReadFile(f.TermMark); string(b) != "terminated" {
			t.Fatal("the client did not get a SIGTERM")
		}
	}
	s := out.String()
	note := strings.Index(s, "Asking claude what it sees in this home (no model call). This starts claude in the home, "+
		"which can take a minute or two on a cold start")
	initLine := strings.Index(s, "  claude init probe: ")
	listLine := strings.Index(s, "  claude mcp list: ")
	if note < 0 || initLine < note || listLine < initLine ||
		!strings.Contains(s, "init event read, aimem MCP server connected") || !strings.Contains(s, ", aimem connected") {
		t.Fatalf("the progress lines:\n%s", s)
	}
}

// A client that never answers is stopped at the bound and reported.
func TestCheckClientHangIsBounded(t *testing.T) {
	f := readyTools
	f.ClaudeHang = true
	e := setupCheck(t, f, "aimem", "claude")
	e.installSkills(t, "1.26.1", "oh-code-review")
	start := time.Now()
	var out bytes.Buffer
	rep, err := Check(context.Background(), CheckOptions{Home: e.home, Clients: []string{"claude"}, Timeout: 2 * time.Second, Out: &out})
	if err != nil || rep.Status != JoinBlocked || rep.Reason != "claude_discovery" || time.Since(start) > 20*time.Second {
		t.Fatalf("%+v, %v, %s", rep, err, time.Since(start))
	}
	if !strings.Contains(out.String(), "  claude init probe: stopped at its 2s ceiling\n") {
		t.Fatalf("the ceiling is not reported:\n%s", out.String())
	}
}

func TestCheckOpenCode(t *testing.T) {
	for _, v := range []string{"1.18.32", "opencode v2.0.19"} {
		t.Run(v, func(t *testing.T) {
			f := readyTools
			f.OpenCode = v
			e := setupCheck(t, f, "aimem", "opencode")
			e.installSkills(t, "1.26.1", "oh-code-review")
			before := snapshot(t, e.user)
			rep := e.check(t, "opencode")
			if rep.Status != JoinRestartRequired || rep.Clients[0].MCP != "connected" || rep.Wiring[0].Action != "create" {
				t.Fatalf("%+v", rep)
			}
			doc := readJSON(t, filepath.Join(e.home, "opencode.json"))
			entry := doc["mcp"].(map[string]any)["aimem"].(map[string]any)
			if doc["$schema"] == nil || entry["type"] != "local" || fmt.Sprint(entry["command"]) != "[aimem mcp]" || entry["enabled"] != true {
				t.Fatalf("opencode.json %v", doc)
			}
			if fmt.Sprint(snapshot(t, e.user)) != fmt.Sprint(before) {
				t.Fatal("the user's home changed")
			}
		})
	}
	t.Run("failed server", func(t *testing.T) {
		f := readyTools
		f.OpenCodeMCP = "failed"
		e := setupCheck(t, f, "aimem", "opencode")
		e.installSkills(t, "1.26.1", "oh-code-review")
		rep := e.check(t, "opencode")
		if rep.Status != JoinBlocked || rep.Reason != "opencode_mcp" || !hasInstruction(rep, "Connection closed") {
			t.Fatalf("%+v", rep)
		}
	})
	t.Run("1.x on data 2 wrote", func(t *testing.T) {
		f := readyTools
		f.OpenCodeDB = true
		e := setupCheck(t, f, "aimem", "opencode")
		e.installSkills(t, "1.26.1", "oh-code-review")
		rep := e.check(t, "opencode")
		if rep.Status != JoinBlocked || rep.Reason != "opencode_conflict" || !hasInstruction(rep, "one OpenCode version per OS user") {
			t.Fatalf("%+v", rep)
		}
	})
}

// Every component's outcome against the set, with its exact instruction.
func TestCheckOutcomes(t *testing.T) {
	cases := []struct {
		name        string
		tools       []string
		mutate      func(*fakeTools)
		skills      string // ai-skills record; "-" installs no skill
		status      string
		reason      string
		instruction string
		notice      string
	}{
		{"aimem missing", []string{"claude"}, nil, "1.26.1", JoinBlocked, "aimem_missing", "Install aimem v0.10.0 with its verifying installer", ""},
		{"aimem newer", []string{"aimem", "claude"}, func(f *fakeTools) { f.Aimem = "aimem v0.10.1" }, "1.26.1", JoinRestartRequired, "",
			"", "aimem 0.10.1 is newer than the tested 0.10.0"},
		{"aimem v0.9.2", []string{"aimem", "claude"}, func(f *fakeTools) { f.Aimem = "aimem v0.9.2" }, "1.26.1", JoinBlocked,
			"aimem_below", "aimem 0.10.0 or later is required", ""},
		{"aimem source build", []string{"aimem", "claude"}, func(f *fakeTools) { f.Aimem = "aimem v0.7.3-52-gabc1234" }, "1.26.1",
			JoinRestartRequired, "", "", "aimem v0.7.3-52-gabc1234 is a development build"},
		{"aimem unstamped source build", []string{"aimem", "claude"}, func(f *fakeTools) { f.Aimem = "aimem dev" }, "1.26.1",
			JoinRestartRequired, "", "", "aimem dev is a development build"},
		{"aimem no version", []string{"aimem", "claude"}, func(f *fakeTools) { f.Aimem = "aimem unknown" }, "1.26.1",
			JoinBlocked, "aimem_failed", "reported no version", ""},
		{"aimem dev-like word", []string{"aimem", "claude"}, func(f *fakeTools) { f.Aimem = "aimem devel" }, "1.26.1",
			JoinBlocked, "aimem_failed", "reported no version", ""},
		{"aimem broken", []string{"aimem", "claude"}, func(f *fakeTools) { f.Aimem = "" }, "1.26.1", JoinBlocked, "aimem_failed",
			"`aimem version` failed", ""},
		{"claude missing", []string{"aimem"}, nil, "1.26.1", JoinBlocked, "claude_missing", "npm install -g @anthropic-ai/claude-code@2.1.286", ""},
		{"claude below", []string{"aimem", "claude"}, func(f *fakeTools) { f.Claude = "2.1.100 (Claude Code)" }, "1.26.1", JoinBlocked,
			"claude_below", "Claude Code 2.1.283 or later is required", ""},
		{"claude newer", []string{"aimem", "claude"}, func(f *fakeTools) { f.Claude = "2.2.0 (Claude Code)" }, "1.26.1", JoinRestartRequired, "",
			"", "claude 2.2.0 is newer than the tested 2.1.286"},
		{"claude MCP failed", []string{"aimem", "claude"}, func(f *fakeTools) { f.ClaudeMCP = "failed" }, "1.26.1", JoinBlocked,
			"claude_mcp", "does not have aimem's MCP server running", ""},
		{"skill missing", []string{"aimem", "claude"}, nil, "-", JoinBlocked, "claude_skills", "SHA256SUMS", ""},
		{"ai-skills unrecorded", []string{"aimem", "claude"}, nil, "", JoinRestartRequired, "", "", "the ai-skills version is unknown"},
		{"ai-skills below", []string{"aimem", "claude"}, nil, "1.20.0", JoinBlocked, "ai-skills_below", "SHA256SUMS", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := readyTools
			if c.mutate != nil {
				c.mutate(&f)
			}
			e := setupCheck(t, f, c.tools...)
			if c.skills != "-" {
				e.installSkills(t, c.skills, "oh-code-review")
			}
			rep := e.check(t, "claude")
			if rep.Status != c.status || rep.Reason != c.reason ||
				(c.instruction != "" && !hasInstruction(rep, c.instruction)) || (c.notice != "" && !hasNotice(rep, c.notice)) {
				raw, _ := json.MarshalIndent(rep, "", " ")
				t.Fatalf("%s", raw)
			}
		})
	}
}

// The ai-skills instruction installs from the verified release archive,
// never from the unverified one-line boot.
func TestSkillsInstructionVerifies(t *testing.T) {
	set, _ := LoadSupported()
	defer func(g string) { goos = g }(goos)
	for _, g := range []string{"linux", "darwin", "windows"} {
		goos = g
		s := skillsInstruction(set.Components["ai-skills"], "", []string{"oh-code-review"})
		if !strings.Contains(s, "SHA256SUMS") || strings.Contains(s, "boot.") || !strings.Contains(s, "aiskills-1.26.1") {
			t.Fatalf("%s: %s", g, s)
		}
		a := aimemInstruction(supportedComponent{Ranges: []versionRange{{Min: "0.7.4", Tested: "0.7.4"}}}, "")
		if !strings.Contains(a, "AIMEM_VERSION") || !strings.Contains(a, "v0.7.4") {
			t.Fatalf("%s: %s", g, a)
		}
	}
}

// The wiring rule: an entry edited by hand is left alone with the proposed
// file beside it; one still at its last managed value is updated (restart
// required); other keys in the file are kept; a file that is not a JSON
// object is never rewritten.
func TestCheckWiringRules(t *testing.T) {
	e := setupCheck(t, readyTools, "aimem", "claude")
	e.installSkills(t, "1.26.1", "oh-code-review")
	mcpPath := filepath.Join(e.home, ".mcp.json")
	os.WriteFile(mcpPath, []byte(`{"mcpServers": {"other": {"command": "x"}}, "keep": 1}`), 0o644)
	rep := e.check(t, "claude")
	doc := readJSON(t, mcpPath)
	if rep.Status != JoinRestartRequired || rep.Wiring[0].Action != "create" || doc["keep"] == nil ||
		doc["mcpServers"].(map[string]any)["other"] == nil {
		t.Fatalf("%+v %v", rep, doc)
	}

	// Edited by hand: kept, proposal beside it.
	edited := `{"mcpServers": {"aimem": {"command": "/opt/aimem", "args": ["mcp", "--debug"]}}}`
	os.WriteFile(mcpPath, []byte(edited), 0o644)
	rep = e.check(t, "claude")
	if rep.Wiring[0].Action != "conflict" || !hasNotice(rep, ".mcp.json.aicrew-new") {
		t.Fatalf("%+v", rep)
	}
	if raw, _ := os.ReadFile(mcpPath); string(raw) != edited {
		t.Fatalf("the edited file was rewritten: %s", raw)
	}
	if _, err := os.Stat(mcpPath + ".aicrew-new"); err != nil {
		t.Fatal(err)
	}

	// At its last managed value, with a new aimem command: updated.
	os.Remove(mcpPath)
	e.check(t, "claude")
	d := readJSON(t, filepath.Join(e.home, "agent.json"))
	d["aicrew"].(map[string]any)["aimem_command"] = filepath.Join(e.bin, "aimem")
	raw, _ := json.Marshal(d)
	os.WriteFile(filepath.Join(e.home, "agent.json"), raw, 0o600)
	rep = e.check(t, "claude")
	if rep.Wiring[0].Action != "update" || rep.Status != JoinRestartRequired {
		t.Fatalf("%+v", rep)
	}

	// Not a JSON object: never rewritten.
	os.WriteFile(mcpPath, []byte("[1, 2]"), 0o644)
	rep = e.check(t, "claude")
	if raw, _ := os.ReadFile(mcpPath); string(raw) != "[1, 2]" || rep.Wiring[0].Action != "conflict" {
		t.Fatalf("%+v %s", rep, raw)
	}
}

func TestCheckSelection(t *testing.T) {
	e := setupCheck(t, readyTools, "aimem", "claude")
	e.installSkills(t, "1.26.1", "oh-code-review")
	if rep := e.check(t); rep.Status != JoinBlocked || rep.Reason != "no_client" || len(rep.Wiring) != 0 {
		t.Fatalf("%+v", rep)
	}
	if _, err := Check(context.Background(), CheckOptions{Home: e.home, Clients: []string{"cursor"}}); err == nil {
		t.Fatal("an unknown client was accepted")
	}
	if _, err := Check(context.Background(), CheckOptions{Home: t.TempDir()}); !errors.Is(err, ErrNotHome) {
		t.Fatalf("a directory without agent.json: %v", err)
	}
}

func TestSupportedSet(t *testing.T) {
	set, err := LoadSupported()
	if err != nil || !slices.Contains(set.RequiredSkills, "oh-code-review") {
		t.Fatalf("%+v, %v", set, err)
	}
	oc := set.Components["opencode"]
	for v, want := range map[string]string{"1.18.32": StateSupported, "1.18.31": StateBelow, "1.20.0": StateNewer,
		"2.0.18": StateSupported, "2.0.19": StateSupported, "2.0.17": StateBelow, "2.1.0": StateNewer, "3.0.0": StateNewer,
		"0.9.0": StateBelow} {
		s, _, _ := parseVersion(v)
		if got, _ := oc.classify(s); got != want {
			t.Fatalf("opencode %s: %s, want %s", v, got, want)
		}
	}
	am := set.Components["aimem"]
	for v, want := range map[string]string{"0.8.0": StateBelow, "0.9.2": StateBelow, "0.9.10": StateBelow, "0.10.0": StateSupported, "0.10.1": StateNewer, "0.11.0": StateNewer, "1.0.0": StateNewer} {
		s, _, _ := parseVersion(v)
		if got, _ := am.classify(s); got != want {
			t.Fatalf("aimem %s: %s, want %s", v, got, want)
		}
	}
	for _, bad := range []string{`{"version": 2}`, `{"version": 1, "components": {}}`,
		`{"version": 1, "extra": 1, "components": {}}`} {
		if _, err := parseSupported([]byte(bad)); err == nil {
			t.Fatalf("accepted %s", bad)
		}
	}
	for s, want := range map[string]bool{"aimem v0.7.3": false, "aimem v0.7.3-52-gabc": true, "2.1.286 (Claude Code)": false,
		"opencode v2.0.19": false} {
		if _, dev, ok := parseVersion(s); !ok || dev != want {
			t.Fatalf("%q: dev %v ok %v", s, dev, ok)
		}
	}
}

// The parsers on output captured from the real clients.
func TestDiscoveryParsers(t *testing.T) {
	out := "Checking MCP server health…\n\nclaude.ai Claude Docs: https://api.anthropic.com/v1/pages/mcp - ✔ Connected\n" +
		"aimem: C:\\Users\\u\\AppData\\Local\\aimem\\bin\\aimem.EXE mcp - ⏸ Pending approval (run `claude` to approve)\n"
	if st, _ := parseClaudeMCPList(out); st != "pending" {
		t.Fatalf("claude mcp list: %s", st)
	}
	oc := "┌  MCP Servers\n│\n●  ✗ aimem failed\n│      MCP error -32000: Connection closed\n│       mcp\n│\n└  1 server(s)\n"
	if st, d := parseOpencodeMCPList(oc); st != "failed" || d != "MCP error -32000: Connection closed" {
		t.Fatalf("opencode mcp list: %s %q", st, d)
	}
	if st, _ := parseOpencodeMCPList("●  \x1b[32m✓\x1b[0m aimem \x1b[2mconnected\x1b[0m\n"); st != "connected" {
		t.Fatalf("opencode connected: %s", st)
	}
	d, err := parseClaudeInit([]byte(`{"type":"system","subtype":"init","mcp_servers":[{"name":"aimem","status":"connected","source":"project"}],"skills":["oh-code-review"]}`))
	if err != nil || d.AimemMCP != "connected" || !d.Skills["oh-code-review"] {
		t.Fatalf("%+v %v", d, err)
	}
	env := clientEnv([]string{"PATH=x", "CLAUDECODE=1", "claude_code_x=1", "ANTHROPIC_AUTH_TOKEN=t", "HOME=h"}, "http://127.0.0.1:1")
	if strings.Contains(strings.Join(env, " "), "CLAUDECODE") || strings.Contains(strings.Join(env, " "), "AUTH_TOKEN") ||
		!slices.Contains(env, "ANTHROPIC_BASE_URL=http://127.0.0.1:1") || !slices.Contains(env, "PATH=x") {
		t.Fatalf("%v", env)
	}
	if _, err := io.Discard.Write(nil); err != nil || !bytes.Equal(canonical(map[string]any{"b": 1, "a": 2}), []byte(`{"a":2,"b":1}`)) {
		t.Fatal("canonical")
	}
}

// join ends with the real check (K7): a home linked by this run is ready
// with its new wiring (no client was open in it), and a blocked check
// blocks the join while the home stays linked.
func TestJoinRunsTheCheck(t *testing.T) {
	t.Run("ready", func(t *testing.T) {
		ce := setupCheck(t, readyTools, "aimem", "claude")
		ce.installSkills(t, "1.26.1", "oh-code-review")
		e := setupJoin(t)
		reads := 0
		deps := e.deps(&recCrew{}, activeAimem(), &reads, e.invite(t, "inv-1", store.RoleWorker))
		deps.check = nil // the real check
		rep, err := Join(context.Background(), e.opts(), deps)
		if err != nil || rep.Status != JoinReady || rep.Check == nil || rep.Check.Status != JoinReady ||
			rep.Check.Wiring[0].Action != "create" {
			t.Fatalf("%+v %+v, %v", rep, rep.Check, err)
		}
		if _, err := os.Stat(filepath.Join(e.home, ".mcp.json")); err != nil {
			t.Fatal(err)
		}
		cfg, err := LoadConfig(e.home)
		if err != nil || cfg.AgentID != rep.AgentID {
			t.Fatalf("%+v %v", cfg, err)
		}
	})
	t.Run("blocked", func(t *testing.T) {
		ce := setupCheck(t, readyTools, "aimem") // no claude
		ce.installSkills(t, "1.26.1", "oh-code-review")
		e := setupJoin(t)
		reads := 0
		deps := e.deps(&recCrew{}, activeAimem(), &reads, e.invite(t, "inv-1", store.RoleWorker))
		deps.check = nil
		rep, err := Join(context.Background(), e.opts(), deps)
		if err != nil || rep.Status != JoinBlocked || rep.Reason != "claude_missing" || !rep.Redeemed || rep.AgentID == "" {
			t.Fatalf("%+v, %v", rep, err)
		}
		if _, err := LoadConfig(e.home); err != nil {
			t.Fatalf("the linked home's configuration: %v", err)
		}
	})
}

// A client file whose MCP parent is null (or another non-object) is the
// user's: a conflict with the proposal beside it, never a crash.
func TestCheckWiringNullParent(t *testing.T) {
	for _, c := range []struct{ client, file, doc string }{
		{"claude", ".mcp.json", `{"mcpServers": null}`},
		{"opencode", "opencode.json", `{"mcp": null, "theme": "dark"}`},
		{"claude", ".mcp.json", `{"mcpServers": [1]}`},
	} {
		t.Run(c.file+" "+c.doc, func(t *testing.T) {
			e := setupCheck(t, readyTools, "aimem", c.client)
			e.installSkills(t, "1.26.1", "oh-code-review")
			path := filepath.Join(e.home, c.file)
			os.WriteFile(path, []byte(c.doc), 0o644)
			rep := e.check(t, c.client)
			if rep.Wiring[0].Action != "conflict" || !hasNotice(rep, c.file+".aicrew-new") {
				t.Fatalf("%+v", rep)
			}
			if raw, _ := os.ReadFile(path); string(raw) != c.doc {
				t.Fatalf("the file was rewritten: %s", raw)
			}
			proposal := readJSON(t, path+".aicrew-new")
			parent := map[string]string{".mcp.json": "mcpServers", "opencode.json": "mcp"}[c.file]
			if p, ok := proposal[parent].(map[string]any); !ok || p["aimem"] == nil {
				t.Fatalf("proposal %v", proposal)
			}
		})
	}
}

// A version record that exists but cannot be used blocks, with a reinstall
// instruction; only an absent record is an unknown version.
func TestCheckSkillsRecordUnusable(t *testing.T) {
	for _, record := range []string{"{", `{"commit": "abc"}`, `{"version": ""}`} {
		t.Run(record, func(t *testing.T) {
			e := setupCheck(t, readyTools, "aimem", "claude")
			e.installSkills(t, "", "oh-code-review")
			os.WriteFile(filepath.Join(e.user, ".claude", "skills", ".ai-skills.json"), []byte(record), 0o644)
			rep := e.check(t, "claude")
			if rep.Status != JoinBlocked || rep.Reason != "ai-skills_record" || !hasInstruction(rep, "reinstall it") ||
				component(rep, "ai-skills").State != StateFailed {
				t.Fatalf("%+v", rep)
			}
		})
	}
}

// Every distinct installation the selected clients read is checked: one
// below the minimum blocks whichever client reads it, in either order.
func TestCheckSkillsEveryInstallation(t *testing.T) {
	for _, order := range [][]string{{"claude", "opencode"}, {"opencode", "claude"}} {
		t.Run(strings.Join(order, ","), func(t *testing.T) {
			e := setupCheck(t, readyTools, "aimem", "claude", "opencode")
			e.installSkills(t, "1.26.1", "oh-code-review") // the user's .claude/skills: Claude Code's
			old := filepath.Join(e.home, ".opencode", "skills")
			os.MkdirAll(filepath.Join(old, "oh-code-review"), 0o755)
			os.WriteFile(filepath.Join(old, "oh-code-review", "SKILL.md"), []byte("---\nname: oh-code-review\n---\n"), 0o644)
			os.WriteFile(filepath.Join(old, ".ai-skills.json"), []byte(`{"version": "1.20.0"}`), 0o644)
			rep := e.check(t, order...)
			if rep.Status != JoinBlocked || rep.Reason != "ai-skills_below" {
				t.Fatalf("%+v", rep)
			}
			n := 0
			for _, c := range rep.Components {
				if c.Name == "ai-skills" {
					n++
				}
			}
			if n != 2 {
				t.Fatalf("%d ai-skills installations reported: %+v", n, rep.Components)
			}
		})
	}
}

// A member on a shared account sets CLAUDE_CONFIG_DIR in their shell: the
// client probes run with it, and the skills and their ai-skills record are
// judged in that member's directory, not the account's ~/.claude. Without
// the variable, the same home finds no skills.
func TestCheckHonorsClaudeConfigDir(t *testing.T) {
	e := setupCheck(t, readyTools, "aimem", "claude")
	member := filepath.Join(t.TempDir(), "member-claude")
	dir := filepath.Join(member, "skills", "oh-code-review")
	os.MkdirAll(dir, 0o755)
	os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte("---\nname: oh-code-review\n---\n"), 0o644)
	os.WriteFile(filepath.Join(member, "skills", ".ai-skills.json"), []byte(`{"version": "1.26.1"}`), 0o644)
	t.Setenv("CLAUDE_CONFIG_DIR", member)
	t.Setenv("CLAUDE_CODE_ENTRYPOINT", "cli") // the calling session's: never kept

	e.check(t, "claude") // creates the wiring
	rep := e.check(t)
	if rep.Status != JoinReady || component(rep, "ai-skills").State != StateSupported ||
		len(rep.Clients[0].MissingSkills) != 0 || rep.Clients[0].MCP != "connected" {
		t.Fatalf("with the member's CLAUDE_CONFIG_DIR: %+v", rep)
	}

	t.Setenv("CLAUDE_CONFIG_DIR", "")
	if rep := e.check(t); rep.Status != JoinBlocked || len(rep.Clients[0].MissingSkills) == 0 {
		t.Fatalf("the account's own ~/.claude holds no skills, yet: %+v", rep)
	}

	env := clientEnv([]string{"PATH=x", "CLAUDE_CONFIG_DIR=" + member, "CLAUDE_CODE_ENTRYPOINT=cli", "CLAUDECODE=1",
		"ANTHROPIC_API_KEY=k"}, "http://127.0.0.1:1")
	if !slices.Contains(env, "CLAUDE_CONFIG_DIR="+member) || slices.Contains(env, "CLAUDE_CODE_ENTRYPOINT=cli") ||
		slices.Contains(env, "CLAUDECODE=1") || slices.Contains(env, "ANTHROPIC_API_KEY=k") {
		t.Fatalf("clientEnv: %v", env)
	}
}

// check notes a home whose settings lack a managed deny rule, as join's
// rerun would restore it, and is quiet on the managed file.
func TestCheckDenyRules(t *testing.T) {
	e := setupCheck(t, readyTools, "aimem", "claude")
	e.installSkills(t, "1.26.1", "oh-code-review")
	if rep := e.check(t, "claude"); hasNotice(rep, "deny rules") {
		t.Fatalf("the managed settings: %+v", rep.Notices)
	}
	path := filepath.Join(e.home, ".claude", "settings.json")
	var doc map[string]any
	_ = json.Unmarshal([]byte(claudeSettings(e.home)), &doc)
	deny := doc["permissions"].(map[string]any)["deny"].([]any)
	for name, edit := range map[string]func(){
		"shortened": func() { doc["permissions"].(map[string]any)["deny"] = deny[1:] },
		"missing":   func() { delete(doc, "permissions") },
	} {
		edit()
		raw, _ := json.Marshal(doc)
		os.WriteFile(path, raw, 0o644)
		if rep := e.check(t, "claude"); !hasNotice(rep, "managed deny rules") || !hasNotice(rep, "join --home") {
			t.Fatalf("%s: %+v", name, rep.Notices)
		}
		_ = json.Unmarshal([]byte(claudeSettings(e.home)), &doc)
	}
}
