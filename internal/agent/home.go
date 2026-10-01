package agent

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

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
	homeDirs    = []string{"docs", "logs", "work", "repos", "worktrees"}
	privateDirs = []string{"creds", "state"}
)

// homeFile is a file the bootstrap writes. A managed file follows the
// rerun rule; an agent-owned one is created once and never changed.
type homeFile struct {
	path    string // slash-separated, relative to the home
	content string
	managed bool
}

// entryFiles are the managed client entry files and the home guidance; a
// change to one of them means an open client should restart.
func homeFiles() []homeFile {
	return []homeFile{
		{"AGENTS.md", agentsMD, true},
		{"CLAUDE.md", claudeMD, true},
		{"docs/START.md", startMD, true},
		{"docs/HANDOFF.md", handoffMD, false},
	}
}

// FileChange is one planned or applied change to a home file.
type FileChange struct {
	Path string `json:"path"`
	// Action is create, update, unchanged, conflict (the proposed version
	// is written beside the file as <path>.aicrew-new) or kept (an
	// agent-owned file that exists).
	Action string `json:"action"`
}

func digestOf(b []byte) string {
	sum := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(sum[:])
}

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

func writeAtomic(path string, data []byte) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+"-*")
	if err != nil {
		return err
	}
	_, werr := tmp.Write(data)
	cerr := tmp.Close()
	if werr != nil || cerr != nil {
		os.Remove(tmp.Name())
		return errors.Join(werr, cerr)
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		os.Remove(tmp.Name())
		return err
	}
	return nil
}

// makeLayout creates the home's directories: creds/ and state/ owner-only
// (restricted if they exist; their contents are never touched), the others
// when missing.
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
	var plan []FileChange
	for _, f := range homeFiles() {
		cur, err := os.ReadFile(filepath.Join(home, filepath.FromSlash(f.path)))
		switch {
		case errors.Is(err, os.ErrNotExist):
			plan = append(plan, FileChange{f.path, "create"})
			continue
		case err != nil:
			return nil, err
		}
		switch {
		case !f.managed:
			plan = append(plan, FileChange{f.path, "kept"})
		case bytes.Equal(cur, []byte(f.content)):
			plan = append(plan, FileChange{f.path, "unchanged"})
		case recorded[f.path] == digestOf(cur):
			plan = append(plan, FileChange{f.path, "update"})
		default:
			plan = append(plan, FileChange{f.path, "conflict"})
		}
	}
	return plan, nil
}

// applyFiles carries out a plan and records the managed digests in rec.
// It returns whether a managed file's existing content was replaced.
func applyFiles(home string, plan []FileChange, rec map[string]string) (bool, error) {
	files := map[string]homeFile{}
	for _, f := range homeFiles() {
		files[f.path] = f
	}
	replaced := false
	for _, c := range plan {
		f := files[c.Path]
		path := filepath.Join(home, filepath.FromSlash(f.path))
		switch c.Action {
		case "create":
			if err := createFile(path, f.content); err != nil {
				return replaced, err
			}
		case "update":
			if err := writeAtomic(path, []byte(f.content)); err != nil {
				return replaced, err
			}
			replaced = true
		case "conflict":
			if err := writeAtomic(path+".aicrew-new", []byte(f.content)); err != nil {
				return replaced, err
			}
			continue // the recorded digest stays the last managed write's
		}
		if f.managed && c.Action != "kept" {
			rec[f.path] = digestOf([]byte(f.content))
		}
	}
	return replaced, nil
}

// createFile creates path, refusing to replace a file that appeared since
// the plan.
func createFile(path, content string) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return err
	}
	_, werr := f.WriteString(content)
	return errors.Join(werr, f.Close())
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
4. **Handoff.** ` + "`docs/HANDOFF.md`" + ` is yours. It is never managed or overwritten.
5. **Readiness.** Before work, run ` + "`aicrew-agent check -home <this directory>`" + `:
   it checks the versions of ` + "`aimem`" + `, ai-skills and your client, and that
   your client sees aimem's MCP server and the required skills. Then start
   the session: ` + "`aicrew-agent session start -home <this directory>`" + `. When
   something is missing or refused, stop and ask the operator instead of
   improvising.

` + managedNote

const handoffMD = "# Handoff\n\n" +
	"This file belongs to the agent. `aicrew-agent join` created it once and never\n" +
	"changes it.\n"
