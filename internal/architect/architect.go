// Package architect writes the architect directory (docs/DESIGN-CONTROL-PLANE.md,
// section 7): the operator's own interactive Claude Code session, which
// plans with the operator and writes epics and tasks to the aimem board in
// personal mode. It is not an agent home: it holds no aicrew membership,
// team session, forge credential or Stop hook, and the control plane does
// not run it. `aicrew architect init` writes it under the managed-file
// rerun rule (internal/managedfiles).
package architect

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/BlackVS/aicrew/internal/managedfiles"
	"github.com/BlackVS/aicrew/internal/privatefile"
)

// Layout is the architect directory's layout version.
const Layout = 1

const (
	recordFile   = "architect.json"
	maxRecord    = 64 << 10
	credsDirName = "creds"
)

// Options name the directory and what the architect plans.
type Options struct {
	Dir      string
	Projects []string // the aimem projects it plans; the first binds a new directory (.aimem.json)
	Hub      string   // the aimem hub alias, when the user's installation has more than one
}

// Report is what init did.
type Report struct {
	Dir     string                `json:"dir"`
	Status  string                `json:"status"` // ready, or conflict when a managed file was edited locally
	Changes []managedfiles.Change `json:"changes"`
	Note    string                `json:"note,omitempty"`
}

var (
	projectRE = regexp.MustCompile(`^[A-Za-z0-9._:-]{1,128}$`) // the store's project reference rule
	hubRE     = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)
)

func (o Options) validate() error {
	if o.Dir == "" {
		return errors.New("--dir is required")
	}
	if len(o.Projects) == 0 {
		return errors.New("at least one --project is required")
	}
	seen := map[string]bool{}
	for _, p := range o.Projects {
		if !projectRE.MatchString(p) {
			return fmt.Errorf("project %q: a project name is 1 to 128 characters from [A-Za-z0-9._:-]", p)
		}
		if seen[p] {
			return fmt.Errorf("project %q is named twice", p)
		}
		seen[p] = true
	}
	if o.Hub != "" && !hubRE.MatchString(o.Hub) {
		return fmt.Errorf("hub %q: use letters, digits, '.', '_' or '-' (at most 64)", o.Hub)
	}
	return nil
}

// Init creates or refreshes the architect directory.
func Init(o Options) (Report, error) {
	if err := o.validate(); err != nil {
		return Report{}, err
	}
	dir, err := filepath.Abs(o.Dir)
	if err != nil {
		return Report{}, err
	}
	rec, err := readRecord(dir)
	if err != nil {
		return Report{}, err
	}
	for _, d := range []string{"docs", ".claude/commands"} {
		if err := os.MkdirAll(filepath.Join(dir, filepath.FromSlash(d)), 0o755); err != nil {
			return Report{}, err
		}
	}
	creds := filepath.Join(dir, credsDirName)
	if err := privatefile.MakeDir(creds); err != nil {
		return Report{}, fmt.Errorf("make creds/ owner-only: %w", err)
	}
	if err := privatefile.CheckDir(creds); err != nil {
		return Report{}, fmt.Errorf("creds/ is not owner-only: %w", err)
	}
	files := Files(o)
	plan, err := managedfiles.Plan(dir, files, rec.Managed)
	if err != nil {
		return Report{}, err
	}
	if _, err := managedfiles.Apply(dir, files, plan, rec.Managed); err != nil {
		return Report{}, err
	}
	rec.Layout, rec.Projects, rec.Hub = Layout, o.Projects, o.Hub
	if err := writeRecord(dir, rec); err != nil {
		return Report{}, err
	}
	rep := Report{Dir: dir, Status: "ready", Changes: plan}
	for _, c := range plan {
		if c.Action == "conflict" {
			rep.Status = "conflict"
			rep.Note = "a managed file was edited locally, so its new version was written beside it as " +
				"<file>.aicrew-new: merge it by hand, then run init again"
		}
	}
	return rep, nil
}

// record is architect.json: nonsecret, with every key init does not own
// written back unchanged.
type record struct {
	Layout   int
	Projects []string
	Hub      string
	Managed  map[string]string
	other    map[string]json.RawMessage
}

func readRecord(dir string) (record, error) {
	r := record{Managed: map[string]string{}, other: map[string]json.RawMessage{}}
	f, err := os.Open(filepath.Join(dir, recordFile))
	if errors.Is(err, os.ErrNotExist) {
		return r, nil
	}
	if err != nil {
		return r, err
	}
	defer f.Close()
	raw, err := io.ReadAll(io.LimitReader(f, maxRecord+1))
	if err != nil {
		return r, err
	}
	if len(raw) > maxRecord {
		return r, errors.New(recordFile + " is larger than 64 KiB")
	}
	if err := json.Unmarshal(raw, &r.other); err != nil || r.other == nil {
		return r, errors.New(recordFile + " is not a JSON object")
	}
	if v, ok := r.other["layout"]; ok {
		var n int
		if json.Unmarshal(v, &n) != nil || n != Layout {
			return r, fmt.Errorf("%s has layout %s; this aicrew writes layout %d and does not migrate it", recordFile, v, Layout)
		}
	}
	// A record that is absent, null or not an object is empty: every
	// existing managed file then counts as unrecorded and is never
	// overwritten.
	var m map[string]string
	if json.Unmarshal(r.other["managed"], &m) == nil && m != nil {
		r.Managed = m
	}
	return r, nil
}

func writeRecord(dir string, r record) error {
	set := func(k string, v any) {
		b, _ := json.Marshal(v)
		r.other[k] = b
	}
	set("layout", r.Layout)
	set("projects", r.Projects)
	if r.Hub != "" {
		set("hub", r.Hub)
	} else {
		delete(r.other, "hub")
	}
	set("managed", r.Managed)
	raw, err := json.MarshalIndent(r.other, "", "  ")
	if err != nil {
		return err
	}
	return managedfiles.WriteAtomic(filepath.Join(dir, recordFile), append(raw, '\n'))
}

// Files are the architect directory's files: the managed guidance, client
// wiring and commands, and the architect's own notes and aimem binding,
// written once.
func Files(o Options) []managedfiles.File {
	files := []managedfiles.File{
		{Path: "AGENTS.md", Content: agentsMD, Managed: true},
		{Path: "CLAUDE.md", Content: claudeMD, Managed: true},
		{Path: "docs/ARCHITECT.md", Content: guidance(o.Projects), Managed: true},
		{Path: ".claude/settings.json", Content: settingsJSON(), Managed: true},
		{Path: ".mcp.json", Content: mcpJSON, Managed: true},
		{Path: "docs/NOTES.md", Content: notesMD},
		{Path: ".aimem.json", Content: bindingJSON(o)},
	}
	for _, c := range commands {
		files = append(files, managedfiles.File{Path: ".claude/commands/" + c.name + ".md", Content: c.markdown(), Managed: true})
	}
	return files
}

// denyRules keep the session from reading or changing the directory's
// creds/, where C2's architect credential will live. They match command
// text, so they stop an accidental read through the tools they name, not a
// determined bypass (docs/WORKSPACE.md, "Deny rules").
func denyRules() []string {
	rules := []string{"Read(/creds/**)", "Edit(/creds/**)"}
	for _, tool := range []string{"Bash", "PowerShell"} {
		rules = append(rules, tool+"(*creds/*)", tool+`(*creds\*)`)
	}
	return rules
}

func settingsJSON() string {
	b, _ := json.MarshalIndent(map[string]any{"permissions": map[string]any{"deny": denyRules()}}, "", "  ")
	return string(b) + "\n"
}

// The aimem entry names no AIMEM_* variable, so the session uses the
// user's own aimem installation in personal mode.
const mcpJSON = `{
  "mcpServers": {
    "aimem": {
      "command": "aimem",
      "args": [
        "mcp"
      ]
    }
  }
}
`

func bindingJSON(o Options) string {
	m := map[string]string{"project": o.Projects[0]}
	if o.Hub != "" {
		m["hub"] = o.Hub
	}
	b, _ := json.MarshalIndent(m, "", "  ")
	return string(b) + "\n"
}

const managedNote = "This file is managed by `aicrew architect init`. A rerun updates it only while it is\n" +
	"unchanged since its last managed write; otherwise it writes the new version\n" +
	"beside it as `<file>.aicrew-new`.\n"

const agentsMD = "# Architect directory\n\n" +
	"This directory is the operator's architect session for an aicrew crew. Read\n" +
	"`docs/ARCHITECT.md` before any work.\n\n" + managedNote

const claudeMD = "# Claude Code entry point\n\n@AGENTS.md\n\n" + managedNote

const notesMD = "# Notes\n\n" +
	"This file belongs to the architect session: plans in progress, open questions\n" +
	"and decisions not yet on the board. `aicrew architect init` created it once and\n" +
	"never changes it.\n"

func guidance(projects []string) string {
	var list strings.Builder
	for _, p := range projects {
		fmt.Fprintf(&list, "- `%s`\n", p)
	}
	return strings.Replace(guidanceMD, "{{projects}}", list.String(), 1) + "\n" + managedNote
}
