package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strings"
	"time"

	"github.com/BlackVS/aicrew/internal/filelock"
	"github.com/BlackVS/aicrew/internal/forge"
	"github.com/BlackVS/aicrew/internal/version"
)

// The dependency and client check (1a81-5a): aimem, ai-skills and the
// selected clients against the supported set, the client wiring in the agent
// home, and each selected client's own view of aimem's MCP server and the
// required skills. It installs nothing: a blocked report carries the exact,
// pinned steps.

// Clients the check knows.
var knownClients = []string{"claude", "opencode"}

// CheckOptions is one check of an agent home.
type CheckOptions struct {
	Home string
	// Clients are the selected clients; empty means the ones agent.json
	// records from an earlier join or check.
	Clients []string
	Out     io.Writer
	// Timeout bounds each client command; zero means 90 s.
	Timeout time.Duration
	// Forge verifies the recorded forge credentials; nil means the real
	// forges.
	Forge ForgeAPI
}

// ComponentReport is one dependency's detected version against the set.
type ComponentReport struct {
	Name     string `json:"name"`
	Found    string `json:"found,omitempty"`
	State    string `json:"state"`
	Required string `json:"required,omitempty"`
	Detail   string `json:"detail,omitempty"`
}

// ClientReport is a selected client: its version and what it discovers.
type ClientReport struct {
	ComponentReport
	Path            string   `json:"path,omitempty"`
	MCP             string   `json:"aimem_mcp,omitempty"`
	MissingSkills   []string `json:"missing_skills,omitempty"`
	ApprovalPending bool     `json:"approval_pending,omitempty"`
}

// CheckReport is the check's result. It never carries a secret.
type CheckReport struct {
	Status     string            `json:"status"`
	Reason     string            `json:"reason,omitempty"`
	Agent      version.Info      `json:"aicrew_agent"`
	Components []ComponentReport `json:"components"`
	Clients    []ClientReport    `json:"clients"`
	Wiring     []FileChange      `json:"wiring,omitempty"`
	Forge      []ForgeCheck      `json:"forge,omitempty"`
	// Projects are the team's requirements as the launcher verified and
	// reported them (3.6); absent when no launcher serves the home.
	Projects []CapabilityRow `json:"projects,omitempty"`
	// OperatorCLI is the operator CLI `aicrew` beside this aicrew-agent,
	// which the member one-liners install with it. A member does not need
	// it, so it never blocks.
	OperatorCLI  *OperatorCLI `json:"operator_cli,omitempty"`
	Notices      []string     `json:"notices,omitempty"`
	Instructions []string     `json:"instructions,omitempty"`
}

// OperatorCLI is the `aicrew` found beside aicrew-agent: its path and the
// release it reports. State is same (aicrew-agent's release), other,
// missing, or failed (it could not be run or read).
type OperatorCLI struct {
	Path    string `json:"path"`
	Version string `json:"version,omitempty"`
	State   string `json:"state"`
}

// operatorCLIDir is the directory the check looks for `aicrew` in: the
// running aicrew-agent's own, where the one-liners install both. Tests
// replace it.
var operatorCLIDir = func() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	if real, err := filepath.EvalSymlinks(exe); err == nil {
		exe = real
	}
	return filepath.Dir(exe), nil
}

// checkOperatorCLI reports the `aicrew` beside aicrew-agent, with a notice
// when it is missing or another release. It never blocks.
func (c *checker) checkOperatorCLI(ctx context.Context) {
	dir, err := operatorCLIDir()
	if err != nil {
		return
	}
	name := "aicrew"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	r := &OperatorCLI{Path: filepath.Join(dir, name)}
	c.rep.OperatorCLI = r
	if _, err := os.Stat(r.Path); err != nil {
		r.State = "missing"
		c.notice(fmt.Sprintf("the operator CLI aicrew is not beside aicrew-agent in %s; a member does not need it, but "+
			"`aicrew architect init` and `aicrew escalations` do: rerun the member one-liner, which installs both", dir))
		return
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, r.Path, "version").Output()
	if f := strings.Fields(string(out)); err == nil && len(f) >= 2 {
		r.Version = f[1]
	}
	switch agent := version.Get().Version; {
	case r.Version == "":
		r.State = "failed"
		c.notice(fmt.Sprintf("%s did not report its version: rerun the member one-liner", r.Path))
	case r.Version == agent:
		r.State = "same"
	default:
		r.State = "other"
		c.notice(fmt.Sprintf("the operator CLI %s is %s, and aicrew-agent is %s: rerun the member one-liner for one release of both",
			r.Path, r.Version, agent))
	}
}

// homeLockName serializes join and check runs on one home.
const homeLockName = "aicrew-join.lock"

// ErrUnknownClient is a selected client the check does not know: a usage
// error.
var ErrUnknownClient = errors.New("unknown client")

// ErrNotHome is a directory without an agent.json.
var ErrNotHome = errors.New("not an agent home: no agent.json (run aicrew-agent join first)")

// Check runs the check on a home that join prepared, under the home lock.
func Check(ctx context.Context, o CheckOptions) (CheckReport, error) {
	if abs, err := filepath.Abs(o.Home); err == nil {
		o.Home = abs
	}
	doc, exists, err := readAgentDoc(o.Home)
	if err != nil {
		return CheckReport{}, err
	}
	if !exists {
		return CheckReport{}, ErrNotHome
	}
	if err := makeLayout(o.Home); err != nil {
		return CheckReport{}, err
	}
	unlock, err := filelock.Lock(ctx, filepath.Join(o.Home, "state", homeLockName))
	if err != nil {
		return CheckReport{}, err
	}
	defer unlock()
	if doc, _, err = readAgentDoc(o.Home); err != nil {
		return CheckReport{}, err
	}
	rep, err := runCheck(ctx, o, &doc, false)
	if err != nil {
		return rep, err
	}
	return rep, doc.write(o.Home)
}

// clients reads agent.json's "clients": the clients the home is for.
func (d agentDoc) clients() []string {
	var c []string
	_ = json.Unmarshal(d.top["clients"], &c)
	return c
}

// selectClients checks the selection: the given clients, else the recorded.
func selectClients(given []string, d agentDoc) ([]string, error) {
	sel := given
	if len(sel) == 0 {
		sel = d.clients()
	}
	var out []string
	for _, c := range sel {
		c = strings.TrimSpace(c)
		if !slices.Contains(knownClients, c) {
			return nil, fmt.Errorf("%w %q (claude or opencode)", ErrUnknownClient, c)
		}
		if !slices.Contains(out, c) {
			out = append(out, c)
		}
	}
	return out, nil
}

type checker struct {
	o        CheckOptions
	doc      *agentDoc
	set      SupportedSet
	rep      CheckReport
	disc     discoveryEnv
	blockers []string
}

func (c *checker) block(reason, instruction string) {
	if c.rep.Reason == "" {
		c.rep.Reason = reason
	}
	c.blockers = append(c.blockers, instruction)
}

func (c *checker) notice(s string) { c.rep.Notices = append(c.rep.Notices, s) }

// runCheck checks a home whose agent.json the caller holds (and writes
// afterwards). newHome: the home was linked by this run, so newly created
// wiring needs no restart.
func runCheck(ctx context.Context, o CheckOptions, doc *agentDoc, newHome bool) (CheckReport, error) {
	if o.Out == nil {
		o.Out = io.Discard
	}
	if o.Timeout == 0 {
		o.Timeout = 90 * time.Second
	}
	set, err := LoadSupported()
	if err != nil {
		return CheckReport{}, err
	}
	c := &checker{o: o, doc: doc, set: set, rep: CheckReport{Agent: version.Get()}}
	sel, err := selectClients(o.Clients, *doc)
	if err != nil {
		return CheckReport{}, err
	}
	sink, closeSink, err := modelSink()
	if err != nil {
		return CheckReport{}, err
	}
	defer closeSink()
	c.disc = discoveryEnv{env: checkProbeEnv(clientEnv(os.Environ(), sink), o.Home), timeout: o.Timeout, progress: o.Out}

	c.checkAimem(ctx)
	c.checkOperatorCLI(ctx)
	if len(sel) == 0 {
		c.block("no_client", "select the client this home is for: rerun with `--client claude` or `--client opencode`")
	} else {
		doc.set(doc.top, "clients", sel)
	}
	rec := doc.managed()
	changed := false
	for _, name := range sel {
		if c.wire(name, rec) {
			changed = true
		}
		c.checkClient(ctx, name, len(sel) == 1)
	}
	doc.set(doc.top, "managed", rec)
	c.checkInstallation(sel)
	c.checkSkills(sel)
	c.checkForgeCreds(ctx)

	c.rep.Instructions = c.blockers
	switch {
	case len(c.blockers) > 0:
		c.rep.Status = JoinBlocked
	case changed && !newHome:
		c.rep.Status = JoinRestartRequired
		c.notice("the client wiring changed: restart any client already open in this home")
	default:
		c.rep.Status = JoinReady
	}
	return c.rep, nil
}

// checkForgeCreds adds the forge table; a credential that is not verified
// is a notice, never a blocker.
func (c *checker) checkForgeCreds(ctx context.Context) {
	api := c.o.Forge
	if api == nil {
		api = forge.NewClient()
	}
	c.rep.Forge = checkForge(ctx, c.o.Home, c.doc, api)
	for _, f := range c.rep.Forge {
		if f.State != ForgeVerified {
			c.notice(fmt.Sprintf("forge credential for %s (%s): %s: %s; work on that host is refused "+
				"until it is fixed, and the home stays usable", f.Host, f.Account, f.State, f.Detail))
		}
	}
	c.checkProjects(ctx)
}

// checkProjects asks the home's launcher, when one serves it, to verify the
// team's requirements and report them to aicrewd (only the launcher holds
// the session), and adds the rows. A requirement not verified is a notice,
// never a blocker: it narrows which offers the member can take.
func (c *checker) checkProjects(ctx context.Context) {
	ans, err := CallStep(ctx, c.o.Home, StepCall{Op: "capabilities"})
	switch {
	case errors.Is(err, ErrNoLauncher):
		c.notice("the team's projects are verified by the running launcher (aicrew-agent launch): " +
			"rerun check while it runs to see them and report them to aicrewd")
		return
	case err != nil:
		c.notice("the launcher did not verify the team's projects: " + err.Error())
		return
	case !ans.OK:
		msg := ans.Status
		if ans.Error != nil {
			msg = ans.Error.Code + ": " + ans.Error.Message
		}
		c.notice("the launcher did not verify the team's projects: " + msg)
		return
	}
	if err := json.Unmarshal(ans.Result, &c.rep.Projects); err != nil {
		c.notice("the launcher's answer about the team's projects is not readable")
		return
	}
	for _, p := range c.rep.Projects {
		if p.State != CapabilityVerified {
			c.notice(fmt.Sprintf("project %s needs %s access to %s (%s): %s: %s; offers that need it are refused, "+
				"and the home stays usable", p.Project, p.Required, p.Repository, p.Host, p.State, p.Detail))
		}
	}
}

func (c *checker) aimemCommand() string {
	if s := str(c.doc.aicrew, "aimem_command"); s != "" {
		return s
	}
	return "aimem"
}

func (c *checker) checkAimem(ctx context.Context) {
	comp := c.set.Components["aimem"]
	r := ComponentReport{Name: "aimem", Required: comp.minimum() + " or later"}
	defer func() { c.rep.Components = append(c.rep.Components, r) }()
	path, err := exec.LookPath(c.aimemCommand())
	if err != nil {
		r.State, r.Detail = StateMissing, err.Error()
		c.block("aimem_missing", aimemInstruction(comp, ""))
		return
	}
	out, errOut, err := discoveryEnv{env: checkProbeEnv(os.Environ(), c.o.Home), timeout: 30 * time.Second}.capture(ctx, c.o.Home, path, "version")
	if err != nil {
		r.State, r.Detail = StateFailed, tail(errOut+out+err.Error(), 300)
		c.block("aimem_failed", "`aimem version` failed ("+r.Detail+"): check the aimem installation, then rerun `aicrew-agent check`")
		return
	}
	r.Found = strings.TrimSpace(out)
	c.classify(&r, comp, r.Found, func() string { return aimemInstruction(comp, r.Found) })
	if r.State != StateBelow && r.State != StateFailed {
		c.checkCredential(ctx, path)
	}
}

// unstampedBuild is a version output whose last word is "dev": a source
// build without version stamping (`aimem dev`).
var unstampedBuild = regexp.MustCompile(`(?:^|\s)dev$`)

// classify sets a component's state from the version text and records the
// blocker or notice that goes with it.
func (c *checker) classify(r *ComponentReport, comp supportedComponent, found string, instruction func() string) {
	v, dev, ok := parseVersion(found)
	label := versionText.FindString(found)
	if !ok && unstampedBuild.MatchString(strings.TrimSpace(found)) {
		// A source build without version stamping reports "dev" (aimem's
		// default): a development build, like a git-describe-stamped one.
		dev, ok, label = true, true, "dev"
	}
	switch {
	case !ok:
		r.State = StateFailed
		r.Detail = "no version in its output"
		c.block(r.Name+"_failed", fmt.Sprintf("%s reported no version (%q): check its installation", r.Name, tail(found, 80)))
		return
	case dev:
		r.State = StateUnknown
		r.Detail = "a development build, not a release"
		c.notice(fmt.Sprintf("%s %s is a development build: its version is unknown, so it neither blocks nor counts as supported", r.Name, label))
		return
	}
	st, rg := comp.classify(v)
	r.State = st
	switch st {
	case StateBelow:
		c.block(r.Name+"_below", instruction())
	case StateNewer:
		c.notice(fmt.Sprintf("%s %s is newer than the tested %s: it should work; report any problem", r.Name, v, rg.Tested))
	}
}

// wire plans and applies a client's MCP entry. It reports whether the
// client's configuration changed.
func (c *checker) wire(client string, rec map[string]string) bool {
	w := wiringFor(client, c.aimemCommand(), c.o.Home)
	ch, doc, err := planWiring(c.o.Home, w, rec[w.managedKey()])
	if err == nil {
		err = applyWiring(c.o.Home, w, ch, doc)
	}
	if err != nil {
		ch.Action = "failed"
		c.block("wiring_failed", fmt.Sprintf("%s could not be written: %v", w.file, err))
	}
	fmt.Fprintf(c.o.Out, "  %-9s %s\n", ch.Action, ch.Path)
	c.rep.Wiring = append(c.rep.Wiring, ch)
	switch ch.Action {
	case "create", "update", "unchanged":
		rec[w.managedKey()] = digestOf(canonical(w.value))
	case "conflict":
		c.notice(fmt.Sprintf("%s was edited locally, so the proposed version was written beside it as %s.aicrew-new: merge it by hand", w.file, w.file))
	}
	return ch.Action == "create" || ch.Action == "update"
}

func (c *checker) checkClient(ctx context.Context, name string, single bool) {
	comp := c.set.Components[name]
	r := ClientReport{ComponentReport: ComponentReport{Name: name, Required: comp.minimum() + " or later"}}
	defer func() { c.rep.Clients = append(c.rep.Clients, r) }()
	path, err := lookClient(name, str(c.doc.aicrew, "client_command"), single)
	if err != nil {
		r.State, r.Detail = StateMissing, err.Error()
		c.block(name+"_missing", clientInstruction(name, comp, ""))
		return
	}
	r.Path = path
	out, errOut, err := c.disc.capture(ctx, c.o.Home, path, "--version")
	if err != nil {
		r.State, r.Detail = StateFailed, tail(errOut+out+err.Error(), 300)
		c.block(name+"_failed", fmt.Sprintf("`%s --version` failed (%s): check the installation", name, r.Detail))
		return
	}
	r.Found = strings.TrimSpace(out)
	c.classify(&r.ComponentReport, comp, r.Found, func() string { return clientInstruction(name, comp, r.Found) })
	if r.State == StateBelow || r.State == StateFailed {
		return
	}
	v, _, _ := parseVersion(r.Found)
	fmt.Fprintf(c.o.Out, "Asking %s what it sees in this home (no model call). This starts %s in the home, which can "+
		"take a minute or two on a cold start; each step is reported when it ends:\n", name, name)
	var d Discovery
	if name == "opencode" {
		d, err = opencodeDiscover(ctx, c.disc, path, c.o.Home, v[0], c.set.RequiredSkills)
	} else {
		d, err = claudeDiscover(ctx, c.disc, path, c.o.Home)
	}
	if err != nil {
		r.Detail = tail(err.Error(), 300)
		if strings.Contains(err.Error(), "Database is not empty") {
			c.block(name+"_conflict", "OpenCode 1.x refuses this user's data, which OpenCode 2 has written (they share it): "+
				"use one OpenCode version per OS user; this check does not resolve it")
			return
		}
		c.block(name+"_discovery", fmt.Sprintf("%s could not report what it sees in this home: %s", name, r.Detail))
		return
	}
	r.MCP, r.ApprovalPending = d.AimemMCP, d.ApprovalPending
	for _, n := range d.Notes {
		c.notice(n)
	}
	if d.AimemMCP != "connected" {
		detail := d.AimemMCP
		if d.MCPDetail != "" {
			detail += ": " + d.MCPDetail
		}
		c.block(name+"_mcp", fmt.Sprintf("%s does not have aimem's MCP server running in this home (%s): check that `%s mcp` "+
			"runs, and the wiring above", name, detail, c.aimemCommand()))
	}
	for _, s := range c.set.RequiredSkills {
		if !d.Skills[s] {
			r.MissingSkills = append(r.MissingSkills, s)
		}
	}
	if len(r.MissingSkills) > 0 {
		c.block(name+"_skills", fmt.Sprintf("%s does not see the required skills %s: %s", name,
			strings.Join(r.MissingSkills, ", "), skillsInstruction(c.set.Components["ai-skills"], "", r.MissingSkills)))
	}
	if d.ApprovalPending {
		c.notice(fmt.Sprintf("Claude Code will ask at the first interactive start in this home to trust the folder and to "+
			"approve the project MCP server aimem: accept both (`aicrew-agent run --client claude --home %s`)", quoteArg(c.o.Home)))
	}
}

// claudeUserDir is Claude Code's user configuration directory:
// CLAUDE_CONFIG_DIR when set (a member's own, on a shared account), else
// ~/.claude.
func claudeUserDir(uh string) string {
	if d := os.Getenv("CLAUDE_CONFIG_DIR"); d != "" {
		return d
	}
	return filepath.Join(uh, ".claude")
}

// skillDirs are where a client reads skills, in its order.
func skillDirs(client, home string) []string {
	uh, _ := os.UserHomeDir()
	dirs := []string{filepath.Join(home, ".claude", "skills")}
	if client == "opencode" {
		cfg := os.Getenv("XDG_CONFIG_HOME")
		if cfg == "" {
			cfg = filepath.Join(uh, ".config")
		}
		dirs = append(dirs, filepath.Join(home, ".agents", "skills"), filepath.Join(home, ".opencode", "skills"),
			filepath.Join(cfg, "opencode", "skills"))
	}
	if client == "claude" {
		dirs = append(dirs, filepath.Join(claudeUserDir(uh), "skills"))
	} else {
		dirs = append(dirs, filepath.Join(uh, ".claude", "skills"))
	}
	if client == "opencode" {
		dirs = append(dirs, filepath.Join(uh, ".agents", "skills"))
	}
	return dirs
}

// checkSkills checks every distinct ai-skills installation the selected
// clients take the first required skill from: for each client, the first
// of its skill directories holding the skill. Each is reported and judged
// by its installer's .ai-skills.json (decision K5; written from ai-skills
// https://github.com/BlackVS/aiskills/issues/24 on). An absent record is an
// unknown version; a record that exists but cannot be read, decoded or
// names no version is blocked.
func (c *checker) checkSkills(clients []string) {
	comp := c.set.Components["ai-skills"]
	required := comp.minimum() + " or later"
	if len(c.set.RequiredSkills) == 0 || len(clients) == 0 {
		c.rep.Components = append(c.rep.Components, ComponentReport{Name: "ai-skills", Required: required, State: StateUnknown})
		return
	}
	skill := c.set.RequiredSkills[0]
	seen := map[string]bool{}
	unknownNoticed := false
	for _, client := range clients {
		for _, dir := range skillDirs(client, c.o.Home) {
			if _, err := os.Stat(filepath.Join(dir, skill, "SKILL.md")); err != nil {
				continue
			}
			if !seen[dir] {
				seen[dir] = true
				r := c.skillsInstallation(comp, dir, &unknownNoticed)
				r.Required = required
				c.rep.Components = append(c.rep.Components, r)
			}
			break // the first directory holding the skill is this client's installation
		}
	}
	if len(seen) == 0 {
		// Not installed where the clients look; the discovery reports which
		// client misses it.
		c.rep.Components = append(c.rep.Components, ComponentReport{Name: "ai-skills", Required: required, State: StateMissing})
	}
}

// skillsInstallation judges one installation by its record.
func (c *checker) skillsInstallation(comp supportedComponent, dir string, unknownNoticed *bool) ComponentReport {
	r := ComponentReport{Name: "ai-skills", Detail: "installed in " + dir}
	record := filepath.Join(dir, ".ai-skills.json")
	raw, err := os.ReadFile(record)
	if errors.Is(err, os.ErrNotExist) {
		r.State, r.Detail = StateUnknown, "installed in "+dir+" without a version record"
		if !*unknownNoticed {
			*unknownNoticed = true
			c.notice("the ai-skills version is unknown: its installer records no version yet " +
				"(https://github.com/BlackVS/aiskills/issues/24), so it neither blocks nor counts as supported")
		}
		return r
	}
	var m struct {
		Version string `json:"version"`
	}
	if err == nil {
		if jerr := json.Unmarshal(raw, &m); jerr != nil {
			err = jerr
		} else if m.Version == "" {
			err = errors.New("it names no version")
		}
	}
	if err != nil {
		r.State = StateFailed
		r.Detail = "the record " + record + " cannot be used: " + tail(err.Error(), 200)
		c.block("ai-skills_record", fmt.Sprintf("the ai-skills installation in %s has a version record that cannot be used "+
			"(%s): reinstall it: %s", dir, tail(err.Error(), 200), skillsInstruction(comp, "", c.set.RequiredSkills)))
		return r
	}
	r.Found = m.Version
	c.classify(&r, comp, m.Version, func() string { return skillsInstruction(comp, m.Version, c.set.RequiredSkills) })
	return r
}
