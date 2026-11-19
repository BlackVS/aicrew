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
func (j *joiner) installSkills(ctx context.Context, crep CheckReport) (rep SkillsReport, fail *skillsFailure) {
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
		next := rerun
		var left *leaveFirst
		switch {
		case errors.As(err, &left):
			next = "; " + left.step(home)
		case codeOf(err) == "session_active":
			// aicrewd refused join's own entry: nothing of join's exists, so
			// there is nothing to leave.
			next = ". A member of this home holds a session in the team: rerun `" + joinRerun(home) +
				"` when it has started, and join then installs inside that session"
		}
		return SkillsReport{}, &skillsFailure{SkillsConversation, "the team's conversation for the skills could not be " +
			"opened: " + tail(err.Error(), 300) + next}
	}
	rep = SkillsReport{Session: conv.SessionID, Opened: conv.Close != nil}
	defer func() {
		if conv.Close == nil {
			return
		}
		if err := conv.Close(context.WithoutCancel(ctx)); err != nil {
			fail = notClosed(home, conv.SessionID, err, fail)
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
	// The whole answer is classified, and a missing set keeps every line:
	// each one is a skill's import command, one per skill the process pins.
	// The others show its end.
	rerun := "; then rerun `" + joinRerun(home) + "`"
	switch {
	case strings.Contains(said, "not every required skill can be installed from the hub"):
		return &skillsFailure{SkillsNotImported, fmt.Sprintf("the hub holds no skills set with every skill the process of "+
			"project %s pins. The hub's administrator imports the set on the hub's host, as aimem names it here: %s%s",
			project, said, rerun)}
	case strings.Contains(said, "grant_denied") || strings.Contains(said, "needs a grant on the project"):
		return &skillsFailure{SkillsGrant, fmt.Sprintf("the hub refused project %s to the team's conversation: the team's "+
			"access profile needs a grant on it, which `aicrew team setup` prints for the hub's administrator. "+
			"aimem said: %s%s", project, tail(said, 1500), rerun)}
	case strings.Contains(said, "no process is selected for project"):
		// aimem's own step names the selection with placeholders; the
		// administrator's step is said here in words instead.
		return &skillsFailure{SkillsNoProcess, fmt.Sprintf("project %s selects no process on the hub, so nothing names the "+
			"skills: the hub's administrator selects the team's process for project %s on the hub's host (aimem's "+
			"`process select`, with the process repository and the release the team works with)%s", project, project, rerun)}
	case conversationRefused(said):
		next := rerun
		if conv.Close == nil {
			// A session recorded in the home that the hub no longer knows:
			// leaving it closes aimem's binding and clears the record.
			next = "; end the recorded session with `aicrew-agent session leave --home " + shellArg(absHome(home)) +
				"`, then rerun `" + joinRerun(home) + "`"
		}
		return &skillsFailure{SkillsConversation, "the hub did not accept the team's conversation " + conv.SessionID +
			": " + tail(said, 1500) + next}
	}
	return &skillsFailure{SkillsFailed, fmt.Sprintf("aimem did not install the skills for project %s: %s%s", project,
		tail(said, 1500), rerun)}
}

// conversationRefused recognises aimem's answers that the team session
// cannot be used (aimem PR #404): not verified by the hub, a binding the hub
// reports for another user, team or session, a file it cannot read, or a
// hub the file names that is not this machine's. Each ends with aimem's
// own step, to restart the conversation through aicrew.
func conversationRefused(said string) bool {
	for _, s := range []string{
		"team context is not verified",
		"team session file cannot be used",
		"reports a different user, team or session",
		"restart the conversation through aicrew",
	} {
		if strings.Contains(said, s) {
			return true
		}
	}
	return false
}

// notClosed is the failure of a conversation join opened and could not
// close: the session stays recorded in the home, so the step that ends it
// comes first, and join is run again after it.
func notClosed(home, session string, err error, fail *skillsFailure) *skillsFailure {
	if errors.Is(err, errTakenOver) {
		// join cannot tell a member that resumed its conversation from a
		// token that ended with nobody holding the session.
		step := (&leaveFirst{session: session, starting: true}).step(home)
		if fail != nil {
			return &skillsFailure{fail.reason, "the team's conversation " + session + " that join opened is no longer " +
				"join's (its token ended): " + step + ", after: " + fail.instruction}
		}
		return &skillsFailure{SkillsConversation, "the skills are installed, but the team's conversation " + session +
			" that join opened is no longer join's: its token ended, because the home's member took the session over or " +
			"the session ended with nobody holding it. " + strings.ToUpper(step[:1]) + step[1:]}
	}
	leave := fmt.Sprintf("the team's conversation %s that join opened was not closed: %s; end it with "+
		"`aicrew-agent session leave --home %s`", session, tail(err.Error(), 300), shellArg(absHome(home)))
	if fail != nil {
		return &skillsFailure{fail.reason, leave + ", then: " + fail.instruction}
	}
	return &skillsFailure{SkillsConversation, "the skills are installed, but " + leave + ", then rerun `" + joinRerun(home) + "`"}
}

// leaveFirst is a failure to hold the conversation that leaves a session
// recorded in the home: the report names the step that ends it before the
// rerun, outside the part of the error it shortens.
type leaveFirst struct {
	err     error
	session string
	// starting is a session recorded without aimem's binding: a member may
	// be starting in the home, and then its own start ends it.
	starting bool
}

func (l *leaveFirst) Error() string { return l.err.Error() }
func (l *leaveFirst) Unwrap() error { return l.err }

// step is what a person does before join runs again.
func (l *leaveFirst) step(home string) string {
	leave := "end session " + l.session + " with `aicrew-agent session leave --home " + shellArg(absHome(home)) + "`"
	if l.starting {
		leave = "when the home's member has started (`aicrew-agent session status --home " + shellArg(absHome(home)) +
			"` shows `aimem_bound` true), or, if no member runs in this home, after you " + leave + ","
	} else {
		leave += ","
	}
	return leave + " then rerun `" + joinRerun(home) + "`"
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
	return holdConversation(ctx, home, cfg)
}

// holdConversation decides whose conversation the install runs in: a
// session recorded in the home is never entered beside or resumed.
func holdConversation(ctx context.Context, home string, cfg Config) (Conversation, error) {
	st, held, err := LoadState(home)
	if err != nil {
		return Conversation{}, err
	}
	if held {
		return heldConversation(home, cfg, st)
	}
	crew, err := NewCrew(cfg, nil)
	if err != nil {
		return Conversation{}, err
	}
	e := NewEngine(cfg, crew, ExecAimem{Command: cfg.AimemCommand, Hub: cfg.AimemHub, Home: cfg.Home},
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	return enterConversation(ctx, home, cfg.AimemHub, e, crew)
}

// enterConversation enters the team with e and reads its projects. The
// conversation is join's from the moment aicrewd let it in: the engine
// records the session before aimem binds it, so a failure after the entry
// (the binding, the projects) leaves the session again, and says how to
// end it when that fails too.
func enterConversation(ctx context.Context, home, hub string, e *Engine, crew SessionReader) (Conversation, error) {
	abandon := func(err error) (Conversation, error) {
		if e.SessionID() == "" {
			return Conversation{}, err
		}
		if lerr := joinLeave(e)(context.WithoutCancel(ctx)); lerr != nil {
			return Conversation{}, &leaveFirst{session: e.SessionID(), starting: errors.Is(lerr, errTakenOver),
				err: fmt.Errorf("%w; the conversation %s join entered was not left either: %s", err, e.SessionID(),
					tail(lerr.Error(), 200))}
		}
		return Conversation{}, err
	}
	// A fresh entry only, never the engine's Start: Start resumes a session
	// recorded in the home, which would fence a member that recorded one
	// since join read the home. Beside a member's active session aicrewd
	// refuses this entry (session_active), and nothing of join's exists.
	if err := e.enter(ctx, ""); err != nil {
		return abandon(err)
	}
	if err := e.bind(ctx); err != nil {
		return abandon(err)
	}
	conv := Conversation{File: e.AimemFile(), SessionID: e.SessionID(), Close: joinLeave(e)}
	if err := RecordTeam(ctx, home, hub, crew, e.stepToken()); err != nil {
		return abandon(fmt.Errorf("the team's projects: %w", err))
	}
	team, _, err := LoadTeam(home)
	if err != nil {
		return abandon(err)
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

// heldConversation is the conversation a session recorded in the home
// stands for. join never enters beside it nor resumes it: either would
// fence whoever holds it. A session aimem has bound is used as it is; one
// recorded without the binding belongs to a member still starting, or to a
// start that failed, and join stops.
func heldConversation(home string, cfg Config, st State) (Conversation, error) {
	switch {
	case st.AgentID != cfg.AgentID:
		return Conversation{}, &leaveFirst{session: st.SessionID,
			err: fmt.Errorf("the home records session %s of another agent (%s)", st.SessionID, st.AgentID)}
	case st.AimemFile == "":
		return Conversation{}, &leaveFirst{session: st.SessionID, starting: true,
			err: fmt.Errorf("the home records session %s without aimem's binding: a member is starting in it, or "+
				"its start failed", st.SessionID)}
	}
	team, ok, err := LoadTeam(home)
	if err != nil || !ok {
		// A member's launcher records the team's projects shortly after its
		// session is bound: a member starting, as with an unbound record.
		why := "the home records session " + st.SessionID + " but not its team's projects yet"
		if err != nil {
			why += ": " + err.Error()
		}
		return Conversation{}, &leaveFirst{session: st.SessionID, starting: true, err: errors.New(why)}
	}
	return Conversation{File: st.AimemFile, SessionID: st.SessionID, Projects: projectIDs(team.Projects)}, nil
}

// errTakenOver is join's leave refused for its token: a member resumed the
// conversation (a resume fences the earlier token), or the session ended
// with nobody holding it. join cannot tell which.
var errTakenOver = errors.New("join's token for the conversation has ended")

// joinLeave ends the conversation join entered, with join's own token only.
// Unlike the engine's Leave it never proves again to take the session back:
// a member that resumed it holds it now, and its binding and the home's
// record are the member's. A leave refused for open work, or not answered,
// is an error as in the engine's Leave.
func joinLeave(e *Engine) func(context.Context) error {
	return func(ctx context.Context) error {
		key := newKey("leave")
		for attempt := 0; ; attempt++ {
			_, err := e.Crew.Leave(ctx, key, e.token)
			if err == nil {
				break
			}
			var r *Refusal
			switch code := codeOf(err); {
			case code == "invalid_token":
				return errTakenOver
			case code == "work_outstanding" && errors.As(err, &r):
				return &WorkOutstanding{NextAction: r.NextAction}
			case !Retryable(err) || attempt+1 >= leaveAttempts:
				return err
			}
			if err := e.Sleep(ctx, backoff(attempt, retryAfter(err))); err != nil {
				return err
			}
		}
		if err := e.Aimem.Close(ctx, e.session.ID); err != nil {
			return fmt.Errorf("left the session, but aimem kept its binding: %w", err)
		}
		e.live.Lock()
		e.token = ""
		e.live.Unlock()
		// Only while the record still names join's session: a member that
		// started during the close has recorded its own (state.go).
		return ClearStateOf(e.Cfg.Home, e.session.ID)
	}
}
