package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
)

// homeGuardEnv names, for the stand-ins this test binary plays (aimem, the
// clients, the launched client), the agent home whose aimem installation
// they must see. A stand-in that sees any other refuses to run, so every
// test that sets it proves that no aimem process started for the home sees
// the user's own installation (D-STORE).
const homeGuardEnv = "AICREW_FAKE_AIMEM_HOME"

// homeGuard checks the two variables against the expected home.
func homeGuard() error {
	home := os.Getenv(homeGuardEnv)
	if home == "" {
		return nil
	}
	for _, v := range aimemVars(home) {
		if got := os.Getenv(v[0]); got != v[1] {
			return fmt.Errorf("%s is %q, not the home's %q", v[0], got, v[1])
		}
	}
	return nil
}

// guardOrExit stops a stand-in that sees a foreign installation.
func guardOrExit(who string) {
	if err := homeGuard(); err != nil {
		fmt.Fprintf(os.Stderr, "%s: foreign aimem installation: %v\n", who, err)
		os.Exit(97)
	}
}

// foreignInstallation gives the test's own environment the user's aimem
// installation, as a member's shell would have, and makes every stand-in
// insist on the home's.
func foreignInstallation(t *testing.T, home string) (stateDir string) {
	t.Helper()
	user := filepath.Join(t.TempDir(), "user-aimem")
	t.Setenv(StateDirEnv, user)
	t.Setenv(SocketEnv, filepath.Join(user, "aimem.sock"))
	t.Setenv(homeGuardEnv, home)
	return user
}

func TestHomeAimemEnvReplacesInherited(t *testing.T) {
	home := filepath.Join(t.TempDir(), "agents", "builder")
	in := []string{"PATH=/bin", "AIMEM_STATE_DIR=/user/state", "AIMEM_SOCKET=/run/user/1/aimem.sock", "AIMEM_HUB=x"}
	if runtime.GOOS == "windows" {
		in = append(in, "aimem_state_dir=C:\\other")
	}
	got := HomeAimemEnv(in, home)
	var state, sock []string
	for _, kv := range got {
		k, v, _ := strings.Cut(kv, "=")
		switch strings.ToUpper(k) {
		case StateDirEnv:
			state = append(state, v)
		case SocketEnv:
			sock = append(sock, v)
		}
	}
	want := filepath.Join(home, "aimem")
	if len(state) != 1 || state[0] != want || len(sock) != 1 || sock[0] != filepath.Join(want, "aimem.sock") {
		t.Fatalf("state %q, socket %q in %q", state, sock, got)
	}
	if !filepath.IsAbs(state[0]) {
		t.Fatalf("relative state root %q", state[0])
	}
	if !strings.Contains(strings.Join(got, "\n"), "AIMEM_HUB=x") || in[1] != "AIMEM_STATE_DIR=/user/state" {
		t.Fatal("other variables lost, or the input changed")
	}
}

// A relative home is carried as an absolute path.
func TestAimemDirIsAbsolute(t *testing.T) {
	t.Chdir(t.TempDir())
	if d := AimemDir("rel-home"); !filepath.IsAbs(d) || filepath.Base(filepath.Dir(d)) != "rel-home" {
		t.Fatalf("aimem dir %q", d)
	}
}

func TestClaudeSettingsCarriesTheInstallation(t *testing.T) {
	home := filepath.Join(t.TempDir(), "h")
	var doc struct {
		Env map[string]string `json:"env"`
	}
	if err := json.Unmarshal([]byte(claudeSettings(home)), &doc); err != nil {
		t.Fatal(err)
	}
	if len(doc.Env) != 2 || doc.Env[StateDirEnv] != AimemDir(home) || doc.Env[SocketEnv] != AimemSocket(home) {
		t.Fatalf("settings env %v", doc.Env)
	}
	w := wiringFor("claude", "aimem", home)
	env := w.value.(map[string]any)["env"].(map[string]string)
	if env[StateDirEnv] != AimemDir(home) || env[SocketEnv] != AimemSocket(home) {
		t.Fatalf(".mcp.json env %v", env)
	}
}

// Every ExecAimem call runs against the home's installation, whatever the
// caller's environment names: the stand-in refuses otherwise.
func TestExecAimemUsesTheHomeInstallation(t *testing.T) {
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	home := filepath.Join(t.TempDir(), "home")
	foreignInstallation(t, home)
	t.Setenv(joinFakeEnv, "active")
	t.Setenv(SessionEnv, filepath.Join(t.TempDir(), "foreign-session.json"))
	if got := (ExecAimem{Home: home}).environ(""); strings.Contains(strings.Join(got, "\n"), SessionEnv+"=") {
		t.Fatal("an inherited team session reaches an aimem call that names none")
	}
	if got := (ExecAimem{Home: home}).environ("own.json"); !slices.Contains(got, SessionEnv+"=own.json") {
		t.Fatal("the call's own team session is missing")
	}
	if st, known, err := (ExecAimem{Command: self, Hub: "main", Home: home}).JoinAimem().Credential(context.Background()); err != nil || !known || st.Credential != "set" {
		t.Fatalf("%+v, %v, %v", st, known, err)
	}
	// Without the home, the call sees the user's installation and the
	// stand-in refuses: the guard is real.
	if _, _, err := (ExecAimem{Command: self, Hub: "main"}).JoinAimem().Credential(context.Background()); err == nil ||
		!strings.Contains(err.Error(), "foreign aimem installation") {
		t.Fatalf("a call without the home was not refused: %v", err)
	}
}

// check's verdict uses the home's installation: its credential for the
// hub, read with the home's variables (the stand-in refuses any other).
func TestCheckHomeCredential(t *testing.T) {
	for _, c := range []struct{ mode, status, reason, text string }{
		{"", JoinRestartRequired, "", ""},
		{"none", JoinBlocked, "credential_missing", "AIMEM_STATE_DIR="},
		{"unconfigured", JoinBlocked, "credential_missing", "aimem hub add main"},
		{"refused", JoinBlocked, "credential_refused", "aimem hub task-token main"},
		{"unreachable", JoinRestartRequired, "", ""},
	} {
		t.Run("mode "+c.mode, func(t *testing.T) {
			f := readyTools
			f.Credential = c.mode
			e := setupCheck(t, f, "aimem", "claude")
			e.installSkills(t, "1.26.1", "oh-code-review")
			rep := e.check(t, "claude")
			if rep.Status != c.status || rep.Reason != c.reason || (c.text != "" && !hasInstruction(rep, c.text)) {
				t.Fatalf("%+v", rep)
			}
			if c.text != "" && !hasInstruction(rep, AimemDir(e.home)) {
				t.Fatalf("the instruction does not name the home's installation: %v", rep.Instructions)
			}
			if c.mode == "unreachable" && !hasNotice(rep, "could not confirm the home's credential") {
				t.Fatalf("notices %v", rep.Notices)
			}
		})
	}
}

// A carrier that does not name the home's installation blocks: a client
// started in the home by hand would use another installation.
func TestCheckCarrierMismatch(t *testing.T) {
	t.Run("settings.json", func(t *testing.T) {
		e := setupCheck(t, readyTools, "aimem", "claude")
		e.installSkills(t, "1.26.1", "oh-code-review")
		os.WriteFile(filepath.Join(e.home, ".claude", "settings.json"), []byte(`{"env": {"AIMEM_STATE_DIR": "/elsewhere"}}`), 0o644)
		rep := e.check(t, "claude")
		if rep.Status != JoinBlocked || rep.Reason != "carrier_mismatch" || !hasInstruction(rep, ".claude/settings.json does not set") {
			t.Fatalf("%+v", rep)
		}
	})
	t.Run("settings.json missing", func(t *testing.T) {
		e := setupCheck(t, readyTools, "aimem", "claude")
		e.installSkills(t, "1.26.1", "oh-code-review")
		os.Remove(filepath.Join(e.home, ".claude", "settings.json"))
		if rep := e.check(t, "claude"); rep.Reason != "carrier_mismatch" || !hasInstruction(rep, "aicrew-agent join --home") {
			t.Fatalf("%+v", rep)
		}
	})
	t.Run(".mcp.json edited", func(t *testing.T) {
		e := setupCheck(t, readyTools, "aimem", "claude")
		e.installSkills(t, "1.26.1", "oh-code-review")
		// A locally edited entry without the env is left as it is (a
		// conflict), and so disagrees with the home.
		os.WriteFile(filepath.Join(e.home, ".mcp.json"), []byte(`{"mcpServers": {"aimem": {"command": "aimem", "args": ["mcp", "-v"]}}}`), 0o644)
		rep := e.check(t, "claude")
		if rep.Status != JoinBlocked || rep.Reason != "carrier_mismatch" || !hasInstruction(rep, ".mcp.json (mcpServers.aimem)") {
			t.Fatalf("%+v", rep)
		}
		if _, err := os.Stat(filepath.Join(e.home, ".mcp.json.aicrew-new")); err != nil {
			t.Fatal("no proposed .mcp.json beside the edited one")
		}
	})
	t.Run("opencode only", func(t *testing.T) {
		// OpenCode has no carrier: neither Claude Code file is required.
		e := setupCheck(t, readyTools, "aimem", "opencode")
		os.Remove(filepath.Join(e.home, ".claude", "settings.json"))
		if rep := e.check(t, "opencode"); rep.Reason == "carrier_mismatch" {
			t.Fatalf("%+v", rep)
		}
	})
}

// What outside the home names another installation is reported by name,
// never by value, and does not block.
func TestCheckForeignInstallation(t *testing.T) {
	e := setupCheck(t, readyTools, "aimem", "claude")
	e.installSkills(t, "1.26.1", "oh-code-review")
	t.Setenv("AIMEM_HUB_TOKEN", "secret-value")
	os.MkdirAll(filepath.Join(e.user, ".config", "aimem"), 0o700)
	os.WriteFile(filepath.Join(e.user, ".config", "aimem", "env"),
		[]byte("# aimem\nAIMEM_STATE_DIR=\"/user/state\"\nAIMEM_EMBED_KEY=k2\nOTHER=1\n"), 0o600)
	os.WriteFile(filepath.Join(e.user, ".claude.json"), []byte(`{"mcpServers": {"memory": {"command": "/usr/bin/aimem", "args": ["mcp"]}},
		"projects": {`+strconvQuote(filepath.ToSlash(e.home))+`: {"mcpServers": {"aimem": {"command": "aimem"}}},
		"/elsewhere": {"mcpServers": {"aimem": {"command": "aimem"}}}}}`), 0o600)
	rep := e.check(t, "claude")
	if rep.Status != JoinRestartRequired {
		t.Fatalf("%+v", rep)
	}
	for _, want := range []string{
		"the environment sets AIMEM_HUB_TOKEN, AIMEM_SOCKET, AIMEM_STATE_DIR, AIMEM_TEAM_SESSION:",
		"sets AIMEM_EMBED_KEY, AIMEM_STATE_DIR: aimem adds them",
		`("aimem" at local scope for this home, "memory" at user scope)`,
	} {
		if !hasNotice(rep, want) {
			t.Fatalf("no notice %q in %q", want, rep.Notices)
		}
	}
	if strings.Contains(strings.Join(rep.Notices, "\n"), "secret-value") || strings.Contains(strings.Join(rep.Notices, "\n"), "k2") {
		t.Fatalf("a value was reported: %q", rep.Notices)
	}
	// The home's own values are not foreign.
	t.Setenv(StateDirEnv, AimemDir(e.home))
	t.Setenv(SocketEnv, AimemSocket(e.home))
	os.Remove(filepath.Join(e.user, ".config", "aimem", "env"))
	os.Remove(filepath.Join(e.user, ".claude.json"))
	rep = e.check(t, "claude")
	if !hasNotice(rep, "the environment sets AIMEM_HUB_TOKEN, AIMEM_TEAM_SESSION:") {
		t.Fatalf("%q", rep.Notices)
	}
}

func strconvQuote(s string) string { b, _ := json.Marshal(s); return string(b) }

// aimem's installer-only knobs are not reported: no aimem process reads
// them. A variable a member process acts on still is (01a11c7d-1e08).
func TestCheckIgnoresAimemInstallerKnobs(t *testing.T) {
	for _, kv := range os.Environ() {
		if k, v, _ := strings.Cut(kv, "="); strings.HasPrefix(strings.ToUpper(k), "AIMEM_") {
			t.Setenv(k, v) // restored after the test
			os.Unsetenv(k)
		}
	}
	e := setupCheck(t, readyTools, "aimem", "claude")
	e.installSkills(t, "1.26.1", "oh-code-review")
	t.Setenv(StateDirEnv, AimemDir(e.home))
	t.Setenv(SocketEnv, AimemSocket(e.home))
	os.Unsetenv(SessionEnv) // setupCheck's foreign session; restored by its t.Setenv
	for _, k := range aimemInstallerKnobs {
		t.Setenv(k, "1")
	}
	rep := e.check(t, "claude")
	if hasNotice(rep, "the environment sets") {
		t.Fatalf("installer-only knobs reported: %q", rep.Notices)
	}
	t.Setenv(StateDirEnv, filepath.Join(t.TempDir(), "elsewhere"))
	rep = e.check(t, "claude")
	if !hasNotice(rep, "the environment sets AIMEM_STATE_DIR: ") || !hasNotice(rep, "AIMEM_USER_ONLY and AIMEM_VERSION, are not listed") {
		t.Fatalf("a foreign state directory is not reported alone: %q", rep.Notices)
	}
}

// The managed settings carry the deny rules exactly. They were verified
// against Claude Code 2.1.294: each is refused with the rules and done
// without them (PR evidence). Changing one needs that smoke again.
func TestClaudeSettingsCarryTheDenyRules(t *testing.T) {
	var doc struct {
		Permissions struct {
			Deny []string `json:"deny"`
		} `json:"permissions"`
	}
	if err := json.Unmarshal([]byte(claudeSettings(t.TempDir())), &doc); err != nil {
		t.Fatal(err)
	}
	want := []string{
		"Read(/creds/**)", "Edit(/creds/**)", "Read(/aimem/hub.json)", "Edit(/aimem/hub.json)",
		"Bash(*creds/*)", `Bash(*creds\*)`, "Bash(*aimem/hub.json*)", `Bash(*aimem\hub.json*)`,
		"Bash(*config --global*)", "Bash(*config --system*)",
		"Bash(*sslVerify*)", "Bash(*sslverify*)", "Bash(*GIT_SSL_NO_VERIFY*)",
		"PowerShell(*creds/*)", `PowerShell(*creds\*)`, "PowerShell(*aimem/hub.json*)", `PowerShell(*aimem\hub.json*)`,
		"PowerShell(*config --global*)", "PowerShell(*config --system*)",
		"PowerShell(*sslVerify*)", "PowerShell(*sslverify*)", "PowerShell(*GIT_SSL_NO_VERIFY*)",
	}
	if strings.Join(doc.Permissions.Deny, "\n") != strings.Join(want, "\n") {
		t.Fatalf("deny = %q", doc.Permissions.Deny)
	}
}
