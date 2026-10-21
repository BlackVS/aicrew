package agent

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/BlackVS/aicrew/internal/managedfiles"
	"github.com/BlackVS/aicrew/internal/privatefile"
)

// The agent home (docs/WORKSPACE.md): its layout, the files the bootstrap
// manages, and its agent.json. The bootstrap creates what is missing,
// updates a managed file only while it is unchanged since its last managed
// write, and never changes, moves or deletes anything else.

// LayoutVersion is the home layout this client creates and understands.
const LayoutVersion = 1

// homeDirs are created when missing; privateDirs are also made owner-only.
var (
	homeDirs    = []string{"docs", "logs", "work", "repos", "worktrees", ".claude", ".claude/commands"}
	privateDirs = []string{"creds", "state", aimemDirName}
)

// homeFile is a file the bootstrap writes (managedfiles.File).
type homeFile = managedfiles.File

// homeFiles are the managed client entry files, the home guidance and the
// Claude Code settings carrying the home's aimem installation; a change to
// one of them means an open client should restart.
func homeFiles(home string) []homeFile {
	return append([]homeFile{
		{Path: "AGENTS.md", Content: agentsMD, Managed: true},
		{Path: "CLAUDE.md", Content: claudeMD, Managed: true},
		{Path: "docs/START.md", Content: startMD, Managed: true},
		{Path: "docs/ROLES.md", Content: rolesMD(), Managed: true},
		{Path: "docs/HANDOFF.md", Content: handoffMD},
		{Path: ".claude/settings.json", Content: claudeSettings(home), Managed: true},
		{Path: TurnSettingsFile, Content: turnSettings(home, false), Managed: true},
		{Path: CoordinatorTurnSettingsFile, Content: turnSettings(home, true), Managed: true},
	}, commandFiles()...)
}

// FileChange is one planned or applied change to a home file
// (managedfiles.Change).
type FileChange = managedfiles.Change

func digestOf(b []byte) string { return managedfiles.Digest(b) }

// agentDoc is agent.json, read so that every key and aicrew field the
// bootstrap does not own is written back unchanged.
type agentDoc struct {
	top    map[string]json.RawMessage
	aicrew map[string]json.RawMessage
}

func agentJSONPath(home string) string { return filepath.Join(home, "agent.json") }

// readAgentDoc reads agent.json; a missing file is an empty document.
func readAgentDoc(home string) (agentDoc, bool, error) {
	d := agentDoc{top: map[string]json.RawMessage{}, aicrew: map[string]json.RawMessage{}}
	f, err := os.Open(agentJSONPath(home))
	if errors.Is(err, os.ErrNotExist) {
		return d, false, nil
	}
	if err != nil {
		return d, false, fmt.Errorf("agent.json: %w", err)
	}
	defer f.Close()
	raw, err := io.ReadAll(io.LimitReader(f, maxAgentJSON+1))
	if err != nil {
		return d, false, fmt.Errorf("agent.json: %w", err)
	}
	if len(raw) > maxAgentJSON {
		return d, false, errors.New("agent.json is larger than 64 KiB")
	}
	if err := json.Unmarshal(raw, &d.top); err != nil || d.top == nil {
		return d, false, errors.New("agent.json is not a JSON object")
	}
	if a, ok := d.top["aicrew"]; ok && string(a) != "null" {
		if err := json.Unmarshal(a, &d.aicrew); err != nil || d.aicrew == nil {
			return d, false, errors.New("agent.json's aicrew section is not a JSON object")
		}
	}
	return d, true, nil
}

// str is a string field of a section, or "" when absent or not a string.
func str(m map[string]json.RawMessage, key string) string {
	var s string
	if json.Unmarshal(m[key], &s) != nil {
		return ""
	}
	return s
}

func (d agentDoc) layout() (int, bool) {
	raw, ok := d.top["layout"]
	if !ok {
		return 0, false
	}
	var v int
	if json.Unmarshal(raw, &v) != nil {
		return -1, true
	}
	return v, true
}

// managed is the record of managed writes. A record that is absent, null
// (which decodes to a nil map) or not an object is empty: every existing
// managed file then counts as unrecorded and is never overwritten.
func (d agentDoc) managed() map[string]string {
	var m map[string]string
	if json.Unmarshal(d.top["managed"], &m) != nil || m == nil {
		return map[string]string{}
	}
	return m
}

func (d agentDoc) set(m map[string]json.RawMessage, key string, v any) {
	raw, _ := json.Marshal(v)
	m[key] = raw
}

// setWakeMode sets agent.json's wake.mode, keeping the wake object's other
// settings.
func (d agentDoc) setWakeMode(mode string) {
	wake := map[string]json.RawMessage{}
	_ = json.Unmarshal(d.top["wake"], &wake)
	if wake == nil {
		wake = map[string]json.RawMessage{}
	}
	d.set(wake, "mode", mode)
	d.set(d.top, "wake", wake)
}

// linked reports whether the home already names its agent and team.
func (d agentDoc) linked() bool {
	return str(d.aicrew, "agent_id") != "" && str(d.aicrew, "team_id") != ""
}

// write replaces agent.json atomically.
func (d agentDoc) write(home string) error {
	d.set(d.top, "aicrew", d.aicrew)
	raw, err := json.MarshalIndent(d.top, "", "  ")
	if err != nil {
		return err
	}
	return writeAtomic(agentJSONPath(home), append(raw, '\n'))
}

func writeAtomic(path string, data []byte) error { return managedfiles.WriteAtomic(path, data) }

// makeLayout creates the home's directories: creds/, state/ and aimem/
// owner-only (restricted if they exist; their contents are never touched),
// the others when missing.
func makeLayout(home string) error {
	for _, d := range homeDirs {
		if err := os.MkdirAll(filepath.Join(home, d), 0o755); err != nil {
			return err
		}
	}
	for _, d := range privateDirs {
		p := filepath.Join(home, d)
		if err := privatefile.MakeDir(p); err != nil {
			return fmt.Errorf("make %s/ owner-only: %w", d, err)
		}
		if err := privatefile.CheckDir(p); err != nil {
			return fmt.Errorf("%s/ is not owner-only: %w", d, err)
		}
	}
	return nil
}

// planFiles decides each home file's change against the recorded digests.
func planFiles(home string, recorded map[string]string) ([]FileChange, error) {
	return managedfiles.Plan(home, homeFiles(home), recorded)
}

// applyFiles carries out a plan and records the managed digests in rec.
// It returns whether a managed file's existing content was replaced.
func applyFiles(home string, plan []FileChange, rec map[string]string) (bool, error) {
	return managedfiles.Apply(home, homeFiles(home), plan, rec)
}

const managedNote = "This file is managed by `aicrew-agent join`. A rerun updates it only while it is\n" +
	"unchanged since its last managed write; otherwise it writes the new version\n" +
	"beside it as `<file>.aicrew-new`.\n"

const agentsMD = "# Agent home\n\n" +
	"This directory is an aicrew agent home. Read `docs/START.md` before any work.\n\n" +
	managedNote

const claudeMD = "# Claude Code entry point\n\n@AGENTS.md\n\n" + managedNote

const startMD = `# Start here

This is an aicrew agent home: your configuration, guidance and workspaces.
Repository instructions live in each repository, not here.

1. **Which instructions win.** An explicit operator instruction first; then
   the repository's own AGENTS.md or CLAUDE.md for work in that repository;
   then the project process selected in aimem; then this guidance.
2. **Credentials.** ` + "`creds/`" + ` holds credential material only, and secrets never
   leave it or your client's own storage: not in files, arguments, logs,
   worktrees or chat. A missing or refused credential stops the operation;
   never fall back to another credential.
3. **Worktrees.** Every change happens in its own worktree under
   ` + "`worktrees/`" + `, created from an explicitly chosen base commit, one per
   attempt. The clones under ` + "`repos/`" + ` stay clean.
   ` + "`aicrew-agent clone --repository URL --attempt ID --base COMMIT --branch NAME`" + `
   makes both with your own forge credential and commit identity.
4. **Handoff.** ` + "`docs/HANDOFF.md`" + ` is yours, and never overwritten. In a home the
   turn loop runs, it keeps two parts of a worker's: the ` + "`## CURRENT ATTEMPT`" + `
   section, which it opens for each attempt and you keep current, and the
   ` + "`## Archive`" + ` of closed attempts, which it trims.
5. **Readiness.** Before work, run ` + "`aicrew-agent check --home <this directory>`" + `:
   it checks the versions of ` + "`aimem`" + `, ai-skills and your client, and that
   your client sees aimem's MCP server and the required skills. When
   something is missing or refused, stop and ask the operator instead of
   improvising.
6. **The launcher holds the session.** In a home joined with
   ` + "`--wake stop-hook`" + ` (the default), your client is started from a terminal by
   ` + "`aicrew-agent run --client <client> --home <this directory>`" + `. In a home
   joined with ` + "`--wake turns`" + `, the member's service, which join installed,
   runs the turn loop: ` + "`aicrew-agent service start --home <this directory>`" + `.
   Either enters the team session and keeps it while the client runs. Inside
   the client, act through ` + "`aicrew-agent inbox`" + ` and ` + "`aicrew-agent step`" + `
   (` + "`check`" + ` and ` + "`session status`" + ` only read). Never run
   ` + "`aicrew-agent session start`" + `, ` + "`session leave`" + ` or ` + "`run`" + ` from inside it:
   a new proof would fence the launcher's session and cut you off from your
   inbox, so they are refused there. To restart the session, exit the client
   and start it again with the same command.
7. **Your role.** ` + "`docs/ROLES.md`" + ` teaches your role's steps, the inbox rule and
   how to write for others. Read it at the start of every session.
8. **Your team's projects.** ` + "`aicrew-agent session status`" + ` prints your role and
   your team's projects (hub, project and repository), as the launcher read
   them from aicrewd when the session started. A coordinator lists a
   project's tasks with aimem's task tools in team mode, and triages them as
   ` + "`docs/ROLES.md`" + ` says. A worker takes the project, the task and the
   repository from the offer, never from this list.
   A coordinator's launcher also records the team's open attempts in
   ` + "`" + OpenAttemptsFile + "`" + ` when the session starts. Reconcile them before anything
   else, as ` + "`docs/ROLES.md`" + ` says under "Reconcile first";
   ` + "`aicrew-agent attempts`" + ` reads the current list.
9. **Your commands.** The home's ` + "`/crew-*`" + ` commands carry these steps: ` + "`/crew-start`" + `,
   ` + "`/crew-inbox`" + `, ` + "`/crew-handoff`" + `; for a coordinator ` + "`/crew-triage`" + `, ` + "`/crew-offer`" + `,
   ` + "`/crew-review`" + ` and ` + "`/crew-escalate`" + `; for a worker ` + "`/crew-accept`" + ` and ` + "`/crew-submit`" + `; for an independent
   member ` + "`/crew-claim`" + `. In a home the turn loop runs (` + "`aicrew-agent serve`" + `), each
   turn starts with ` + "`/crew-turn`" + `.

` + managedNote

const handoffMD = "# Handoff\n\n" +
	"This file belongs to the agent. `aicrew-agent join` created it once and never\n" +
	"changes it. In a home the turn loop runs, the loop keeps two parts of a\n" +
	"worker's: the CURRENT ATTEMPT section, which it opens for each attempt and the\n" +
	"agent keeps current, and the Archive of closed attempts. The rest is the agent's.\n"

// HomeLabel is the home's label as join recorded it, or else its
// directory's name: what names the home's service.
func HomeLabel(home string) string {
	if d, ok, err := readAgentDoc(home); err == nil && ok {
		if l := str(d.top, "label"); l != "" {
			return l
		}
	}
	return filepath.Base(filepath.Clean(home))
}
