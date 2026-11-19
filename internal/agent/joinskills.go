package agent

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"slices"
	"strings"
)

// A member gets its skills inside its team's conversation (task
// 01a1f3a6-77d3): when the check finds the required skills missing, join
// opens a conversation as the home's agent, has the home's aimem install
// the skills the project's process pins from the hub as that conversation
// (AIMEM_TEAM_SESSION), always closes the conversation it opened, and
// checks again. The member's user needs no personal grant: aimem reads the
// hub under the team's grant, and never with the installation's own
// credential. The hub's skills set is the hub administrator's import.

// skillsTeamAimem is the first aimem whose `skills install` acts as the
// team conversation that AIMEM_TEAM_SESSION names. An older one ignores the
// variable and asks the hub with the home's own credential, so join never
// runs it: it reports that aimem must be upgraded first.
const skillsTeamAimem = "0.11.5"

// The reasons a join reports when it could not install the skills.
const (
	SkillsAimemBelow   = "skills_aimem_below"    // the home's aimem predates the team path
	SkillsNoProject    = "skills_no_project"     // the team is granted no project
	SkillsConversation = "skills_conversation"   // the conversation could not be opened or used
	SkillsNotImported  = "skills_not_on_hub"     // the hub holds no set with every pinned skill
	SkillsGrant        = "skills_grant"          // the team's grant does not cover the project
	SkillsNoProcess    = "skills_no_process"     // the project selects no process
	SkillsFailed       = "skills_install_failed" // anything else aimem reported
)

// SkillsReport is what join did about the skills.
type SkillsReport struct {
	Project string `json:"project,omitempty"`
	// Installed is aimem's report of each skill it wrote.
	Installed []string `json:"installed,omitempty"`
	// Session is the conversation the install ran in.
	Session string `json:"session,omitempty"`
	// Opened is true when join opened the conversation (and closed it).
	Opened bool `json:"opened,omitempty"`
}

// Conversation is a team conversation held for the install.
type Conversation struct {
	// File is the aimem session file, the value of AIMEM_TEAM_SESSION.
	File string
	// SessionID names the conversation in the report.
	SessionID string
	// Projects are the team's granted projects, in aicrewd's order.
	Projects []string
	// Close ends a conversation join opened; nil for one it found held.
	Close func(context.Context) error
}

// SkillsDeps are join's collaborators for the install; tests replace them.
type SkillsDeps struct {
	// Supports reports whether the home's aimem acts as a team
	// conversation in `skills install`.
	Supports func(ctx context.Context, aimem, home string) (bool, error)
	// Open opens a conversation as the home's agent, or takes the one a
	// running member holds.
	Open func(ctx context.Context, home string) (Conversation, error)
	// Install runs `aimem skills install -p project --tool claude` in the
	// home as the conversation of file; it returns aimem's output.
	Install func(ctx context.Context, aimem, home, file, project string) (stdout, stderr string, err error)
}

// realSkillsDeps are the real collaborators.
func realSkillsDeps() SkillsDeps {
	return SkillsDeps{Supports: aimemSkillsTeam, Open: openConversation, Install: aimemSkillsInstall}
}

// skillsMissing reports whether the check blocks on skills that an install
// from the hub provides: a client that does not see them, no installation
// where the clients look, one older than the minimum, or one whose version
// record cannot be used (the install writes the record anew).
func skillsMissing(rep CheckReport) bool {
	for _, c := range rep.Clients {
		if len(c.MissingSkills) > 0 {
			return true
		}
	}
	for _, c := range rep.Components {
		if c.Name == "ai-skills" && (c.State == StateBelow || c.State == StateMissing || c.State == StateFailed) {
			return true
		}
	}
	return false
}

// componentFound is the version the check found for a component.
func componentFound(rep CheckReport, name string) string {
	for _, c := range rep.Components {
		if c.Name == name {
			return c.Found
		}
	}
	return ""
}

// skillsFailure is an install that did not happen, with what a person does.
type skillsFailure struct{ reason, instruction string }

// installSkills installs the skills inside a team conversation. It returns
// the report, or the failure to report instead.
func (j *joiner) installSkills(ctx context.Context, crep CheckReport) (SkillsReport, *skillsFailure) {
	home, aimem := j.o.Home, j.aimemCommand()
	rerun := "; then rerun `" + joinRerun(home) + "`"
	ok, err := j.skills.Supports(ctx, aimem, home)
	if err != nil || !ok {
		detail := "it predates the team path"
		if err != nil {
			detail = "it could not be asked: " + tail(err.Error(), 200)
		}
		return SkillsReport{}, &skillsFailure{SkillsAimemBelow, "the home's aimem cannot install the skills inside the " +
			"team's conversation (" + detail + "): " + skillsAimemInstruction(componentFound(crep, "aimem"), j.o.ExportBase) + rerun}
	}
	fmt.Fprintln(j.deps.Out, "Skills: installing them from the hub inside the team's conversation.")
	conv, err := j.skills.Open(ctx, home)
	if err != nil {
		return SkillsReport{}, &skillsFailure{SkillsConversation, "the team's conversation for the skills could not be " +
			"opened: " + tail(err.Error(), 300) + rerun}
	}
	rep := SkillsReport{Session: conv.SessionID, Opened: conv.Close != nil}
	defer func() {
		if conv.Close == nil {
			return
		}
		if err := conv.Close(context.WithoutCancel(ctx)); err != nil {
			fmt.Fprintf(j.deps.Out, "  the conversation %s was not closed: %s\n", conv.SessionID, tail(err.Error(), 200))
		}
	}()
	if len(conv.Projects) == 0 {
		return rep, &skillsFailure{SkillsNoProject, "the team is granted no project, so no process names the skills " +
			"to install: the operator grants the team a project (`aicrew team setup` prints the grant the hub's " +
			"administrator gives)" + rerun}
	}
	// The process of the first granted project decides: aicrewd lists the
	// grants in a stable order, and each project's process pins its own set.
	rep.Project = conv.Projects[0]
	stdout, stderr, err := j.skills.Install(ctx, aimem, home, conv.File, rep.Project)
	if err != nil {
		return rep, skillsInstallFailure(home, conv, rep.Project, stdout, stderr, err)
	}
	for _, line := range strings.Split(stdout, "\n") {
		if strings.HasPrefix(line, "installed ") {
			rep.Installed = append(rep.Installed, strings.TrimSpace(line))
			fmt.Fprintln(j.deps.Out, "  "+strings.TrimSpace(line))
		}
	}
	return rep, nil
}

// skillsInstallFailure maps aimem's answer to a reason and the step that
// fixes it. Each case is recognised by aimem's own text (aimem PR #404, "For
// a caller"); aimem's lines are passed on, since they name the conversation
// and, for a missing set, each skill's import command.
func skillsInstallFailure(home string, conv Conversation, project, stdout, stderr string, err error) *skillsFailure {
	said := strings.TrimSpace(stderr)
	if said == "" {
		said = strings.TrimSpace(stdout)
	}
	if said == "" {
		said = err.Error()
	}
	said = tail(said, 1500)
	rerun := "; then rerun `" + joinRerun(home) + "`"
	switch {
	case strings.Contains(said, "not every required skill can be installed from the hub"):
		return &skillsFailure{SkillsNotImported, fmt.Sprintf("the hub holds no skills set with every skill the process of "+
			"project %s pins. The hub's administrator imports the set on the hub's host, as aimem names it here: %s%s",
			project, said, rerun)}
	case strings.Contains(said, "grant_denied") || strings.Contains(said, "needs a grant on the project"):
		return &skillsFailure{SkillsGrant, fmt.Sprintf("the hub refused project %s to the team's conversation: the team's "+
			"access profile needs a grant on it, which `aicrew team setup` prints for the hub's administrator. "+
			"aimem said: %s%s", project, said, rerun)}
	case strings.Contains(said, "no process is selected for project"):
		return &skillsFailure{SkillsNoProcess, fmt.Sprintf("project %s selects no process on the hub, so nothing names the "+
			"skills: %s%s", project, said, rerun)}
	case strings.Contains(said, "team context is not verified") || strings.Contains(said, "team session file cannot be used"):
		next := rerun
		if conv.Close == nil {
			// A session recorded in the home that the hub no longer knows:
			// leaving it closes aimem's binding and clears the record.
			next = "; end the recorded session with `aicrew-agent session leave --home " + shellArg(absHome(home)) +
				"`, then rerun `" + joinRerun(home) + "`"
		}
		return &skillsFailure{SkillsConversation, "the hub did not accept the team's conversation " + conv.SessionID +
			": " + said + next}
	}
	return &skillsFailure{SkillsFailed, fmt.Sprintf("aimem did not install the skills for project %s: %s%s", project, said, rerun)}
}

// joinRerun is the join that resumes a home: its home is enough, since
// agent.json holds everything else.
func joinRerun(home string) string { return "aicrew-agent join --home " + shellArg(absHome(home)) }

// skillsAimemInstruction installs the first aimem with the team path, or
// the tested release when that is later, as the check's own aimem step
// does (with the export base, from the export), and without its "rerun the
// check": join reruns.
func skillsAimemInstruction(found, base string) string {
	c := supportedComponent{Ranges: []versionRange{{Min: skillsTeamAimem, Tested: skillsTeamAimem}}}
	if set, err := LoadSupported(); err == nil {
		if t := set.Components["aimem"].installRelease(found); t != "" && atLeast(t, skillsTeamAimem) {
			c.Ranges[0].Tested = t
		}
	}
	return strings.TrimSuffix(aimemInstruction(c, found, base), "; then rerun `aicrew-agent check`")
}

// aimemSkillsTeam asks the home's aimem whether `skills install` acts as
// the team conversation: its usage says so from the release that does.
func aimemSkillsTeam(ctx context.Context, aimem, home string) (bool, error) {
	cmd := exec.CommandContext(ctx, aimem, "skills")
	cmd.Env = withoutEnv(HomeAimemEnv(os.Environ(), home), SessionEnv)
	cmd.Dir = absHome(home)
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	err := cmd.Run()
	var exit *exec.ExitError
	if err != nil && !errors.As(err, &exit) {
		return false, err
	}
	return strings.Contains(out.String(), SessionEnv), nil
}

// aimemSkillsInstall runs the install in the home as the conversation of
// file. The home's own installation is the one aimem uses; the session file
// alone selects the team path.
func aimemSkillsInstall(ctx context.Context, aimem, home, file, project string) (string, string, error) {
	cmd := exec.CommandContext(ctx, aimem, "skills", "install", "-p", project, "--tool", "claude")
	cmd.Env = withEnv(HomeAimemEnv(os.Environ(), home), SessionEnv, file)
	cmd.Dir = absHome(home)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	return stdout.String(), stderr.String(), err
}

// openConversation holds a team conversation for the install. A home whose
// member runs (its service, or a client started by run) holds one already:
// join uses that one as it is, since a new proof would fence it. Otherwise
// join enters the team as the home's agent, reads the team's projects, and
// leaves again when the install is done.
func openConversation(ctx context.Context, home string) (Conversation, error) {
	cfg, err := LoadConfig(home)
	if err != nil {
		return Conversation{}, err
	}
	st, held, err := LoadState(home)
	if err != nil {
		return Conversation{}, err
	}
	if held && st.AgentID == cfg.AgentID && st.AimemFile != "" {
		team, ok, err := LoadTeam(home)
		if err != nil || !ok {
			return Conversation{}, fmt.Errorf("the home records session %s but not its team's projects; "+
				"end it with `aicrew-agent session leave --home %s` and rerun join", st.SessionID, shellArg(absHome(home)))
		}
		return Conversation{File: st.AimemFile, SessionID: st.SessionID, Projects: projectIDs(team.Projects)}, nil
	}
	crew, err := NewCrew(cfg, nil)
	if err != nil {
		return Conversation{}, err
	}
	e := NewEngine(cfg, crew, ExecAimem{Command: cfg.AimemCommand, Hub: cfg.AimemHub, Home: cfg.Home},
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err := e.Start(ctx); err != nil {
		return Conversation{}, err
	}
	conv := Conversation{File: e.AimemFile(), SessionID: e.SessionID(), Close: e.Leave}
	if err := RecordTeam(ctx, home, cfg.AimemHub, crew, e.stepToken()); err != nil {
		_ = e.Leave(context.WithoutCancel(ctx))
		return Conversation{}, fmt.Errorf("the team's projects: %w", err)
	}
	team, _, err := LoadTeam(home)
	if err != nil {
		_ = e.Leave(context.WithoutCancel(ctx))
		return Conversation{}, err
	}
	conv.Projects = projectIDs(team.Projects)
	return conv, nil
}

func projectIDs(ps []TeamProject) []string {
	var out []string
	for _, p := range ps {
		if p.ProjectID != "" && !slices.Contains(out, p.ProjectID) {
			out = append(out, p.ProjectID)
		}
	}
	return out
}
