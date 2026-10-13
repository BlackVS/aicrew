package agent

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
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
// no team session, since a probe joins none and an inherited session file
// belongs to another installation, and this aicrew-agent first on PATH, as
// a launched client has it (withSelfOnPath).
func checkProbeEnv(env []string, home string) []string {
	return withSelfOnPath(withoutEnv(HomeAimemEnv(env, home), SessionEnv))
}

// claudeSettings is the home's managed .claude/settings.json: the home's
// aimem installation, the Stop hook that wakes the member when its inbox
// changes (task 01a0d6d7-1aed), and the deny rules.
func claudeSettings(home string) string {
	doc := map[string]any{"env": aimemVarMap(home), "permissions": map[string]any{"deny": denyRules()}}
	if ReadWakeSettings(home).Mode != WakeTurns {
		doc["hooks"] = wakeHooks(home)
	}
	b, _ := json.MarshalIndent(doc, "", "  ")
	return string(b) + "\n"
}

// TurnSettingsFile is the home's managed per-turn settings, relative to the
// home: the allow list a headless turn runs under, beneath the deny rules.
// The turn loop passes it with --settings, since a project's own allow rules
// do not apply to a headless turn (docs/TURN-PROBE.md); the deny rules apply
// from both files.
const TurnSettingsFile = ".claude/turn-settings.json"

// CoordinatorTurnSettingsFile is the per-turn settings of a coordinator's
// turn: TurnSettingsFile's rules and the coordinator's own (pilot finding
// #15, 2026-10-13).
const CoordinatorTurnSettingsFile = ".claude/turn-settings-coordinator.json"

// TurnSettingsFor is the per-turn settings file of a member in role.
func TurnSettingsFor(role string) string {
	if role == "coordinator" {
		return CoordinatorTurnSettingsFile
	}
	return TurnSettingsFile
}

// turnSettings is the content of the per-turn settings, the coordinator's
// when coordinator is set. A file passed with --settings anchors a rule's
// leading "/" at its own directory, .claude/, not at the home as the home's
// .claude/settings.json does (measured with Claude Code 2.1.296), so its
// path rules are anchored with "./" at the turn's working directory, the
// home.
func turnSettings(home string, coordinator bool) string {
	deny := denyRules()
	for i, r := range deny {
		deny[i] = homeAnchored(r)
	}
	b, _ := json.MarshalIndent(map[string]any{"permissions": map[string]any{"allow": turnAllow(home, coordinator),
		"deny": deny}}, "", "  ")
	return string(b) + "\n"
}

// aimemReadTools are the aimem MCP tools every member's turn may call: the
// reads of the board and its documents. In dontAsk mode an MCP tool without
// an allow rule is denied, which kept the pilot's coordinator from reading
// the READY task it was to offer (finding #15).
var aimemReadTools = []string{"get_task", "list_tasks", "get_task_comment", "list_task_comments", "get_task_history",
	"get_epic", "list_epics", "list_docs", "read_doc", "get_design_doc", "process_context", "project_repository",
	"writing_rule"}

// coordinatorTurnAllow is what only a coordinator's turn may do besides:
// mirror an escalation with add_task_comment, triage with triage_task (ROLES.md,
// the coordinator's rules), and the aimem reads /crew-offer names.
var coordinatorTurnAllow = []string{"mcp__aimem__add_task_comment", "mcp__aimem__triage_task",
	"Bash(aimem process show*)", "Bash(aimem project show*)"}

// homeAnchored rewrites a Read or Edit rule anchored with "/" at the
// settings' project into one anchored with "./" at the working directory.
func homeAnchored(rule string) string {
	for _, tool := range []string{"Read(", "Edit("} {
		if strings.HasPrefix(rule, tool+"/") && !strings.HasPrefix(rule, tool+"//") {
			return tool + "./" + strings.TrimPrefix(rule, tool+"/")
		}
	}
	return rule
}

// baseTurnAllow is what every member's turn may do without asking, beyond
// the commands the client itself judges read-only: read, search and list
// files in the home and its worktrees, edit its worktrees and its handoff
// (all anchored at the home with "./"; the deny rules keep creds/ and the
// hub file out), run git, run aicrew-agent by name or by the path of the
// one writing the settings, and call aimem's read tools. A coordinator's
// turn also has coordinatorTurnAllow.
func baseTurnAllow(coordinator bool) []string {
	rules := []string{"Read(./**)", "Grep(./**)", "Glob(./**)", "Edit(./worktrees/**)", "Edit(./docs/HANDOFF.md)",
		"Bash(git *)", "Bash(aicrew-agent *)"}
	if self := filepath.ToSlash(selfExecutable()); self != "aicrew-agent" {
		rules = append(rules, "Bash("+self+" *)")
	}
	for _, t := range aimemReadTools {
		rules = append(rules, "mcp__aimem__"+t)
	}
	if coordinator {
		rules = append(rules, coordinatorTurnAllow...)
	}
	return rules
}

// turnAllow is the base allow list and the home's own additions, agent.json's
// turns.allow, which name the project's build and test commands. An addition
// that is not a narrow rule (TurnRuleProblem) is left out, and check names it.
func turnAllow(home string, coordinator bool) []string {
	rules := baseTurnAllow(coordinator)
	for _, r := range ReadTurnAllow(home) {
		if r = homeAnchored(r); TurnRuleProblem(r) == "" && !slices.Contains(rules, r) {
			rules = append(rules, r)
		}
	}
	return rules
}

// ReadTurnAllow is agent.json's turns.allow, as written.
func ReadTurnAllow(home string) []string {
	doc, ok, err := readAgentDoc(home)
	if err != nil || !ok {
		return nil
	}
	var t struct {
		Allow []string `json:"allow"`
	}
	if raw, ok := doc.top["turns"]; ok {
		_ = json.Unmarshal(raw, &t)
	}
	return t.Allow
}

// turnRuleShape is one Claude Code permission rule: a tool and a pattern.
var turnRuleShape = regexp.MustCompile(`^(Bash|Edit|Read|WebFetch)\(([^()\r\n]{1,200})\)$`)

// TurnRuleProblem says why an addition to the allow list is refused, or ""
// when it is a narrow rule: a tool with a pattern that names something, never
// a whole tool ("Bash", "Bash(*)") and never PowerShell, whose allow rules
// admit nothing narrower than the whole tool (docs/TURN-PROBE.md).
func TurnRuleProblem(rule string) string {
	m := turnRuleShape.FindStringSubmatch(rule)
	if m == nil {
		return "not a Bash, Edit, Read or WebFetch rule with a pattern"
	}
	if strings.Trim(m[2], "*/. :") == "" {
		return "a pattern that admits the whole tool"
	}
	return ""
}

// denyPatterns are what a member's shell commands never name: the home's
// credential files and aimem's hub file, git's global and system
// configuration, and TLS verification switches. Nothing in a member's role
// needs them: aicrew-agent and aimem read the credentials, and aicrew-agent
// writes every git setting a clone needs into that clone.
var denyPatterns = []string{
	"*creds/*", `*creds\*`, "*aimem/hub.json*", `*aimem\hub.json*`,
	"*config --global*", "*config --system*",
	"*sslVerify*", "*sslverify*", "*GIT_SSL_NO_VERIFY*",
}

// denyRules is the managed permissions.deny list (task 01a1171d-c51c), in
// Claude Code's rule syntax: Read and Edit rules anchored at the home (a
// leading "/" is relative to the settings' project), and each pattern for
// the Bash and PowerShell tools, where "*" matches any text. Claude Code
// refuses a denied call whatever the model decides and whatever the
// permission mode; it stops an accidental violation through these tools,
// not a determined bypass through another command.
func denyRules() []string {
	rules := []string{"Read(/creds/**)", "Edit(/creds/**)", "Read(/aimem/hub.json)", "Edit(/aimem/hub.json)"}
	for _, tool := range []string{"Bash", "PowerShell"} {
		for _, p := range denyPatterns {
			rules = append(rules, tool+"("+p+")")
		}
	}
	return rules
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
