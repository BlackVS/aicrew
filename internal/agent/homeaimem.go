package agent

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// The member's aimem installation lives in the agent home (decision
// D-STORE): <home>/aimem is its state root, holding hub.json with the
// member's individual credential, the team sessions and the spool. Every
// aimem process that runs for the home is pointed at it by two variables,
// carried three ways so that no process started there sees the user's own
// installation, whoever started the client:
//   - the home's managed .claude/settings.json `env`, which Claude Code
//     gives every session started in the home, with its hooks, MCP servers
//     and subprocesses;
//   - the managed .mcp.json aimem entry's `env`;
//   - the environment join, check and run give each aimem call, probe and
//     launched client, replacing any inherited value.

const (
	// StateDirEnv is aimem's state-root override.
	StateDirEnv = "AIMEM_STATE_DIR"
	// SocketEnv is aimem's local-service socket override; without it, a
	// Linux aimem would use the user's socket under XDG_RUNTIME_DIR.
	SocketEnv = "AIMEM_SOCKET"
	// aimemDirName is the installation's directory in the home.
	aimemDirName = "aimem"
	// unixSocketMax is the portable bound on a Unix socket's path (macOS
	// 104 bytes, Linux 108).
	unixSocketMax = 104
)

// absHome is the home as an absolute path.
func absHome(home string) string {
	if abs, err := filepath.Abs(home); err == nil {
		return abs
	}
	return home
}

// AimemDir is the home's aimem state root.
func AimemDir(home string) string { return filepath.Join(absHome(home), aimemDirName) }

// AimemSocket is the home's aimem socket path.
func AimemSocket(home string) string { return filepath.Join(AimemDir(home), "aimem.sock") }

// aimemVars are the two variables naming the home's installation, in a
// fixed order.
func aimemVars(home string) [][2]string {
	return [][2]string{{StateDirEnv, AimemDir(home)}, {SocketEnv, AimemSocket(home)}}
}

// aimemVarMap is aimemVars as a carrier's `env` object.
func aimemVarMap(home string) map[string]string {
	m := map[string]string{}
	for _, v := range aimemVars(home) {
		m[v[0]] = v[1]
	}
	return m
}

// HomeAimemEnv returns env with the home's two aimem variables set,
// replacing any value they had.
func HomeAimemEnv(env []string, home string) []string {
	for _, v := range aimemVars(home) {
		env = withEnv(env, v[0], v[1])
	}
	return env
}

// checkProbeEnv is the environment of a check probe: the home's installation,
// and no team session, since a probe joins none and an inherited session
// file belongs to another installation.
func checkProbeEnv(env []string, home string) []string {
	return withoutEnv(HomeAimemEnv(env, home), SessionEnv)
}

// claudeSettings is the home's managed .claude/settings.json: the home's
// aimem installation, and the Stop hook that wakes the member when its inbox
// changes (task 01a0d6d7-1aed).
func claudeSettings(home string) string {
	b, _ := json.MarshalIndent(map[string]any{"env": aimemVarMap(home), "hooks": wakeHooks(home)}, "", "  ")
	return string(b) + "\n"
}

// selfExecutable is the aicrew-agent the hook runs: the one writing the
// settings. Tests replace it.
var selfExecutable = func() string {
	p, err := os.Executable()
	if err != nil {
		return "aicrew-agent"
	}
	return p
}

// wakeHooks is the settings' hooks object: a Stop hook running
// `aicrew-agent wait-inbox` for this home, with a timeout a minute past the
// home's wait, so the hook ends on its own before Claude Code ends it.
func wakeHooks(home string) map[string]any {
	ws := ReadWakeSettings(home)
	command := fmt.Sprintf(`"%s" wait-inbox --home "%s"`, filepath.ToSlash(selfExecutable()), filepath.ToSlash(home))
	return map[string]any{"Stop": []any{map[string]any{"hooks": []any{map[string]any{
		"type": "command", "command": command, "timeout": int(ws.Wait/time.Second) + 60}}}}}
}
