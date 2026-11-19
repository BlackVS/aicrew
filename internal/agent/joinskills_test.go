package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/BlackVS/aicrew/internal/filelock"
	"github.com/BlackVS/aicrew/internal/store"
)

// skillsFakeEnv makes this test binary answer as aimem's `skills` command:
// "team" prints the usage of an aimem whose install acts as the team
// conversation, "old" the usage of one that does not, and "install" records
// an install's arguments, directory and variables in the file that
// skillsFakeLog names, then answers as AICREW_SKILLS_FAKE_ANSWER says.
const (
	skillsFakeEnv = "AICREW_SKILLS_FAKE_AIMEM"
	skillsFakeLog = "AICREW_SKILLS_FAKE_LOG"
)

func init() {
	mode := os.Getenv(skillsFakeEnv)
	if mode == "" {
		return
	}
	args := os.Args[1:]
	switch {
	case len(args) == 1 && args[0] == "skills" && mode == "team":
		fmt.Fprintln(os.Stderr, "usage: aimem skills install ... With AIMEM_TEAM_SESSION set, list and install read the hub as that team conversation")
		os.Exit(1)
	case len(args) == 1 && args[0] == "skills" && mode == "silent":
		os.Exit(1)
	case len(args) == 1 && args[0] == "skills":
		fmt.Fprintln(os.Stderr, "usage: aimem skills install [-p <project>] [--user] [--tool <claude>]")
		os.Exit(1)
	case len(args) > 1 && args[0] == "skills" && args[1] == "install":
		wd, _ := os.Getwd()
		b, _ := json.Marshal(map[string]any{"args": args, "dir": wd, "session": os.Getenv(SessionEnv),
			"state": os.Getenv(StateDirEnv), "socket": os.Getenv(SocketEnv)})
		os.WriteFile(os.Getenv(skillsFakeLog), b, 0o600)
		fmt.Println("installed oh-code-review 1.43.0 (release) for claude: .claude/skills/oh-code-review")
		os.Exit(0)
	}
	fmt.Fprintf(os.Stderr, "unexpected arguments %q\n", args)
	os.Exit(3)
}

// skillsRun is one join's use of the skills collaborators.
type skillsRun struct {
	supports    bool
	supportsErr error
	openErr     error
	projects    []string
	held        bool // the conversation was found held, not opened
	stdout      string
	stderr      string
	installErr  error
	closeErr    error
	opened      int
	closed      int
	installs    []string // "file project"
	checks      int
	installedOK bool // the check sees the skills once an install succeeded
}

func (r *skillsRun) deps() SkillsDeps {
	return SkillsDeps{
		Supports: func(context.Context, string, string) (bool, error) { return r.supports, r.supportsErr },
		Open: func(context.Context, string) (Conversation, error) {
			if r.openErr != nil {
				return Conversation{}, r.openErr
			}
			r.opened++
			c := Conversation{File: "/run/aimem/sess-1.json", SessionID: "sess-1", Projects: r.projects}
			if !r.held {
				c.Close = func(context.Context) error { r.closed++; return r.closeErr }
			}
			return c, nil
		},
		Install: func(_ context.Context, _, _, file, project string) (string, string, error) {
			r.installs = append(r.installs, file+" "+project)
			if r.installErr == nil {
				r.installedOK = true
			}
			return r.stdout, r.stderr, r.installErr
		},
	}
}

// check blocks on the missing skill until an install succeeded.
func (r *skillsRun) check(_ context.Context, o CheckOptions, doc *agentDoc, _ bool) (CheckReport, error) {
	r.checks++
	sel, err := selectClients(o.Clients, *doc)
	if err != nil {
		return CheckReport{}, err
	}
	doc.set(doc.top, "clients", sel)
	if r.installedOK {
		return CheckReport{Status: JoinReady}, nil
	}
	return CheckReport{Status: JoinBlocked, Reason: "claude_skills",
		Clients: []ClientReport{{ComponentReport: ComponentReport{Name: "claude"}, MissingSkills: []string{"oh-code-review"}}}}, nil
}

func (e *joinEnv) joinSkills(t *testing.T, r *skillsRun) JoinReport {
	t.Helper()
	code := e.invite(t, "inv-"+t.Name(), store.RoleWorker)
	reads := 0
	deps := e.deps(&recCrew{}, activeAimem(), &reads, code)
	deps.check, deps.skills = r.check, r.deps()
	rep, err := Join(context.Background(), e.opts(), deps)
	if err != nil {
		t.Fatalf("join: %v (%+v)", err, rep)
	}
	return rep
}

// A member without the skills gets them by join: inside a conversation join
// opens, for the team's first granted project, then the check again; the
// conversation is closed.
func TestJoinInstallsSkillsInTheTeamConversation(t *testing.T) {
	e := setupJoin(t)
	r := &skillsRun{supports: true, projects: []string{"alpha", "beta"},
		stdout: "installed oh-code-review 1.43.0 (release) for claude: /h/.claude/skills/oh-code-review\n1 skill installed\n"}
	rep := e.joinSkills(t, r)
	if rep.Status != JoinReady || rep.Reason != "" || rep.Skills == nil || rep.Skills.Project != "alpha" ||
		!rep.Skills.Opened || len(rep.Skills.Installed) != 1 || !strings.HasPrefix(rep.Skills.Installed[0], "installed oh-code-review") {
		t.Fatalf("report %+v, skills %+v", rep, rep.Skills)
	}
	if !slices.Equal(r.installs, []string{"/run/aimem/sess-1.json alpha"}) || r.opened != 1 || r.closed != 1 || r.checks != 2 {
		t.Fatalf("installs %q, opened %d, closed %d, checks %d", r.installs, r.opened, r.closed, r.checks)
	}
}

// Each way the install cannot happen is reported with its own reason, the
// step a person takes, and the rerun; a conversation join opened is closed
// in every case, and an aimem that predates the team path is never run.
func TestJoinSkillsFailures(t *testing.T) {
	notOnHub := "not every required skill can be installed from the hub:\n  oh-code-review 1.43.0 (release): the hub " +
		"holds no release set with it; on the hub host: aimem skills import --from-archive aiskills-1.43.0.tar.gz"
	for _, c := range []struct {
		name   string
		run    skillsRun
		reason string
		want   []string
		opened bool
	}{
		{"aimem below", skillsRun{}, SkillsAimemBelow,
			[]string{"aimem " + skillsTeamAimem + " or later is required", "verifying installer", "aicrew-agent join --home "}, false},
		// An aimem that cannot be asked counts as one without the team path,
		// also when the question somehow said yes: it is never run.
		{"aimem not asked", skillsRun{supports: true, supportsErr: errors.New("exec: \"aimem\": file not found")}, SkillsAimemBelow,
			[]string{"it could not be asked", "file not found", "aimem " + skillsTeamAimem + " or later is required"}, false},
		{"open refused", skillsRun{supports: true, openErr: errors.New("aicrewd refused the entry: proof_invalid")},
			SkillsConversation, []string{"could not be opened", "proof_invalid", "aicrew-agent join --home "}, false},
		// join's own entry refused beside a member's session: nothing of
		// join's exists, so no leave, only the rerun once the member runs.
		{"member holds a session", skillsRun{supports: true, openErr: &Refusal{Code: "session_active"}},
			SkillsConversation, []string{"A member of this home holds a session in the team: rerun `aicrew-agent join --home ",
				"join then installs inside that session"}, false},
		// The close finds join's token ended: not ready, and the step for a
		// member that may hold the session.
		{"taken over at close", skillsRun{supports: true, projects: []string{"alpha"}, closeErr: errTakenOver,
			stdout: "installed oh-code-review 1.43.0 (release) for claude: /h/.claude/skills/oh-code-review\n"},
			SkillsConversation, []string{"the skills are installed, but the team's conversation sess-1 that join opened is no longer join's",
				"aicrew-agent session status --home ", "aicrew-agent session leave --home ", "aicrew-agent join --home ",
				"the skills are installed, but"}, true},
		// An open that leaves a session recorded: the leave step stays whole
		// however long the error is.
		{"open leaves a session", skillsRun{supports: true, openErr: &leaveFirst{session: "sess-9",
			err: errors.New("aimem cannot bind: " + strings.Repeat("x", 600))}},
			SkillsConversation, []string{"could not be opened", "end session sess-9 with `aicrew-agent session leave --home ",
				"aicrew-agent join --home "}, false},
		{"no project", skillsRun{supports: true}, SkillsNoProject,
			[]string{"granted no project", "aicrew team setup"}, true},
		{"set missing", skillsRun{supports: true, projects: []string{"alpha"}, stderr: notOnHub, installErr: errors.New("exit status 1")},
			SkillsNotImported, []string{"hub's administrator imports", "aimem skills import --from-archive aiskills-1.43.0.tar.gz",
				"project alpha", "aicrew-agent join --home "}, true},
		{"grant", skillsRun{supports: true, projects: []string{"alpha"}, installErr: errors.New("exit status 1"),
			stderr: "team conversation sess-1 of team t: the hub refused to read project alpha's process selection: grant_denied: no; " +
				"the team's access profile needs a grant on the project (on the hub host: aimem identity team grant); " +
				"nothing was read outside the team conversation"},
			SkillsGrant, []string{"aicrew team setup", "grant_denied", "aicrew-agent join --home "}, true},
		// aimem's own answer at the pinned release, placeholders included.
		{"no process", skillsRun{supports: true, projects: []string{"alpha"}, installErr: errors.New("exit status 1"),
			stderr: "no process is selected for project alpha; an admin selects one on the hub host: aimem process select <repo> <tag> -p alpha"},
			SkillsNoProcess, []string{"selects no process", "the hub's administrator selects the team's process for project alpha",
				"`process select`", "aicrew-agent join --home "}, true},
		// A long list of missing skills keeps its reason and every import line.
		{"set missing, long", skillsRun{supports: true, projects: []string{"alpha"}, stderr: longNotOnHub(), installErr: errors.New("exit status 1")},
			SkillsNotImported, []string{"hub's administrator imports", "skill-00 1.43.0 (release)", "skill-99 1.43.0 (release) is not on the hub: not imported; the admin imports the skills release on the hub host: `aimem skills import --from-archive aiskills-1.43.0.tar.gz` (docs/ADMIN-MANUAL.md), or the process pins the current one",
				"aicrew-agent join --home "}, true},
		// A recorded session whose binding the hub reports for another
		// user, team or session: leave it first.
		{"held and mismatched", skillsRun{supports: true, held: true, projects: []string{"alpha"}, installErr: errors.New("exit status 1"),
			stderr: "team conversation sess-1 of team t: the hub reports a different user, team or session than the team session file names; " +
				"restart the conversation through aicrew"},
			SkillsConversation, []string{"aicrew-agent session leave --home ", "aicrew-agent join --home "}, false},
		// The close failing after an install that worked: not ready.
		{"close fails", skillsRun{supports: true, projects: []string{"alpha"}, closeErr: errors.New("aicrewd cannot be reached"),
			stdout: "installed oh-code-review 1.43.0 (release) for claude: /h/.claude/skills/oh-code-review\n"},
			SkillsConversation, []string{"the skills are installed, but the team's conversation sess-1 that join opened was not closed",
				"aicrewd cannot be reached", "aicrew-agent session leave --home ", "aicrew-agent join --home "}, true},
		// The close failing after an install that failed: both, the leave first.
		{"install and close fail", skillsRun{supports: true, projects: []string{"alpha"}, closeErr: errors.New("aicrewd cannot be reached"),
			installErr: errors.New("exit status 1"), stderr: "disk full"},
			SkillsFailed, []string{"was not closed", "aicrew-agent session leave --home ", "did not install the skills for project alpha: disk full"}, true},
		{"not verified", skillsRun{supports: true, projects: []string{"alpha"}, installErr: errors.New("exit status 1"),
			stderr: "team conversation sess-1 of team t: the team context is not verified: context_stale; restart the conversation " +
				"through aicrew; nothing was read outside the team conversation"},
			SkillsConversation, []string{"did not accept the team's conversation sess-1", "aicrew-agent join --home "}, true},
		{"held and not verified", skillsRun{supports: true, held: true, projects: []string{"alpha"}, installErr: errors.New("exit status 1"),
			stderr: "team conversation sess-1 of team t: the team context is not verified: context_stale"},
			SkillsConversation, []string{"aicrew-agent session leave --home ", "aicrew-agent join --home "}, false},
		{"other", skillsRun{supports: true, projects: []string{"alpha"}, installErr: errors.New("exit status 1"), stderr: "disk full"},
			SkillsFailed, []string{"did not install the skills for project alpha: disk full"}, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			e := setupJoin(t)
			r := c.run
			rep := e.joinSkills(t, &r)
			if rep.Status != JoinBlocked || rep.Reason != c.reason {
				t.Fatalf("status %s reason %q: %+v", rep.Status, rep.Reason, rep)
			}
			for _, w := range c.want {
				if !strings.Contains(rep.Instruction, w) {
					t.Fatalf("the instruction lacks %q: %s", w, rep.Instruction)
				}
			}
			if strings.ContainsAny(rep.Instruction, "<>") {
				t.Fatalf("the instruction has a placeholder: %s", rep.Instruction)
			}
			if c.opened && (r.opened != 1 || r.closed != 1) {
				t.Fatalf("opened %d, closed %d", r.opened, r.closed)
			}
			if !c.opened && r.closed != 0 {
				t.Fatalf("closed %d conversations join did not open", r.closed)
			}
			if c.reason == SkillsAimemBelow && (r.opened != 0 || len(r.installs) != 0) {
				t.Fatalf("an aimem without the team path was used: opened %d, installs %q", r.opened, r.installs)
			}
			if r.checks != 1 {
				t.Fatalf("%d checks after a failed install", r.checks)
			}
		})
	}
}

// A home that does not lack the skills opens no conversation.
func TestJoinWithSkillsOpensNoConversation(t *testing.T) {
	e := setupJoin(t)
	r := &skillsRun{supports: true, installedOK: true, projects: []string{"alpha"}}
	if rep := e.joinSkills(t, r); rep.Status != JoinReady || rep.Skills != nil || r.opened != 0 || len(r.installs) != 0 {
		t.Fatalf("report %+v, opened %d, installs %q", rep, r.opened, r.installs)
	}
}

// The real install runs the home's aimem in the home, with the home's
// installation and the conversation's session file, and with exactly the
// arguments of aimem's caller contract; the capability question reads
// aimem's usage, never with a session.
func TestExecSkillsInstallAndCapability(t *testing.T) {
	home := filepath.Join(t.TempDir(), "agents", "builder")
	os.MkdirAll(home, 0o700)
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	logFile := filepath.Join(t.TempDir(), "install.json")
	t.Setenv(skillsFakeLog, logFile)
	t.Setenv(SessionEnv, "/somewhere/else.json") // an inherited session is never the one used

	t.Setenv(skillsFakeEnv, "old")
	if ok, err := aimemSkillsTeam(context.Background(), exe, home); err != nil || ok {
		t.Fatalf("an aimem without the team path: %v %v", ok, err)
	}
	// A usage worded otherwise, or silent, is no team path.
	t.Setenv(skillsFakeEnv, "silent")
	if ok, err := aimemSkillsTeam(context.Background(), exe, home); err != nil || ok {
		t.Fatalf("an aimem whose usage says nothing: %v %v", ok, err)
	}
	// An aimem that cannot be run is an error, which join reports as
	// skills_aimem_below without running anything.
	if ok, err := aimemSkillsTeam(context.Background(), filepath.Join(t.TempDir(), "no-aimem"), home); err == nil || ok {
		t.Fatalf("an aimem that cannot be run: %v %v", ok, err)
	}
	t.Setenv(skillsFakeEnv, "team")
	if ok, err := aimemSkillsTeam(context.Background(), exe, home); err != nil || !ok {
		t.Fatalf("an aimem with the team path: %v %v", ok, err)
	}
	stdout, _, err := aimemSkillsInstall(context.Background(), exe, home, "/run/aimem/sess-1.json", "alpha")
	if err != nil || !strings.HasPrefix(stdout, "installed oh-code-review") {
		t.Fatalf("install: %v %q", err, stdout)
	}
	raw, _ := os.ReadFile(logFile)
	var got struct {
		Args                        []string
		Dir, Session, State, Socket string
	}
	json.Unmarshal(raw, &got)
	same := func(a, b string) bool {
		if runtime.GOOS == "windows" {
			return strings.EqualFold(filepath.Clean(a), filepath.Clean(b))
		}
		ea, _ := filepath.EvalSymlinks(a)
		eb, _ := filepath.EvalSymlinks(b)
		return ea == eb
	}
	if !slices.Equal(got.Args, []string{"skills", "install", "-p", "alpha", "--tool", "claude"}) || !same(got.Dir, home) ||
		got.Session != "/run/aimem/sess-1.json" || got.State != AimemDir(home) || got.Socket != AimemSocket(home) {
		t.Fatalf("install ran as %+v", got)
	}
}

// The install is tried for every way the check finds the skills missing,
// and only for those.
func TestSkillsMissing(t *testing.T) {
	skills := func(state string) CheckReport {
		return CheckReport{Components: []ComponentReport{{Name: "ai-skills", State: state}}}
	}
	for state, want := range map[string]bool{StateMissing: true, StateBelow: true, StateFailed: true,
		StateSupported: false, StateNewer: false, StateUnknown: false} {
		if got := skillsMissing(skills(state)); got != want {
			t.Errorf("ai-skills %s: %v, want %v", state, got, want)
		}
	}
	client := CheckReport{Clients: []ClientReport{{MissingSkills: []string{"oh-code-review"}}}}
	if !skillsMissing(client) || skillsMissing(CheckReport{}) {
		t.Fatal("a client's missing skill decides")
	}
}

// longNotOnHub is aimem's answer for a set missing a hundred skills, over
// 16 KiB, longer than any part of an answer join shows for another reason.
func longNotOnHub() string {
	var b strings.Builder
	b.WriteString("not every required skill can be installed from the hub:")
	for i := 0; i < 100; i++ {
		fmt.Fprintf(&b, "\n  skill-%02d 1.43.0 (release) is not on the hub: not imported; the admin imports the skills release on "+
			"the hub host: `aimem skills import --from-archive aiskills-1.43.0.tar.gz` (docs/ADMIN-MANUAL.md), or the process pins the current one", i)
	}
	return b.String()
}

// A conversation join entered is left again when aimem cannot bind it: the
// engine records the session before the binding, so nobody else would.
func TestEnterConversationLeavesAfterAFailedBind(t *testing.T) {
	c := setupCrew(t)
	ctx := context.Background()
	if err := os.WriteFile(filepath.Join(c.root, "fail-open"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	e := c.engine(t)
	_, err := enterConversation(ctx, c.cfg.Home, c.cfg.AimemHub, e, e.Crew.(SessionReader))
	if err == nil || !strings.Contains(err.Error(), "cannot be reached") {
		t.Fatalf("enter with aimem failing: %v", err)
	}
	if e.SessionID() == "" {
		t.Fatal("the test did not reach the entry")
	}
	if sess, _ := c.store.GetSession(ctx, e.SessionID()); sess.State != store.SessionLeft {
		t.Fatalf("the session join entered is %s, not left", sess.State)
	}
	if _, ok, _ := LoadState(c.cfg.Home); ok {
		t.Fatal("the home still records the session")
	}
}

// When the leave after a failed entry fails too, the error names the step
// that ends the session.
func TestEnterConversationNamesTheLeave(t *testing.T) {
	c := setupCrew(t)
	ctx := context.Background()
	for _, f := range []string{"fail-open", "fail-close"} {
		if err := os.WriteFile(filepath.Join(c.root, f), nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	e := c.engine(t)
	_, err := enterConversation(ctx, c.cfg.Home, c.cfg.AimemHub, e, e.Crew.(SessionReader))
	var left *leaveFirst
	if !errors.As(err, &left) || left.session != e.SessionID() || left.starting {
		t.Fatalf("enter with aimem failing to bind and close: %v", err)
	}
	if step := left.step(c.cfg.Home); !strings.HasPrefix(step, "end session "+e.SessionID()+" with `aicrew-agent session leave --home ") {
		t.Fatalf("the step: %s", step)
	}
}

// A member starting in the home records its session before aimem binds it.
// join, interleaved there, neither enters beside it nor resumes it: the
// member's session keeps its generation, and join names the wait.
func TestJoinLeavesAStartingMemberAlone(t *testing.T) {
	c := setupCrew(t)
	ctx := context.Background()
	flag := filepath.Join(c.root, "fail-open")
	if err := os.WriteFile(flag, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	member := c.engine(t)
	if err := member.Start(ctx); err == nil {
		t.Fatal("the member's start was to stop before aimem's binding")
	}
	os.Remove(flag)
	st, ok, err := LoadState(c.cfg.Home)
	if err != nil || !ok || st.AimemFile != "" {
		t.Fatalf("no session recorded without its binding: %+v %v %v", st, ok, err)
	}
	before := c.sessionsOf(t)
	_, err = holdConversation(ctx, c.cfg.Home, c.cfg)
	var left *leaveFirst
	if !errors.As(err, &left) || !left.starting || left.session != st.SessionID {
		t.Fatalf("join beside a starting member: %v", err)
	}
	if step := left.step(c.cfg.Home); !strings.Contains(step, "aicrew-agent session status --home ") ||
		!strings.Contains(step, "aicrew-agent session leave --home ") || !strings.Contains(step, "aicrew-agent join --home ") {
		t.Fatalf("the step: %s", step)
	}
	if sess, _ := c.store.GetSession(ctx, st.SessionID); strconv.FormatInt(sess.Generation, 10) != member.session.Generation || sess.State == store.SessionLeft {
		t.Fatalf("the member's session moved: generation %d, state %s", sess.Generation, sess.State)
	}
	if c.sessionsOf(t) != before {
		t.Fatal("join entered beside the member")
	}
	// The member's own restart resumes the session it recorded.
	if err := c.engine(t).Start(ctx); err != nil {
		t.Fatalf("the member's restart: %v", err)
	}
}

// A session recorded for another agent is never used or entered.
func TestHeldConversationOfAnotherAgent(t *testing.T) {
	cfg := Config{AgentID: "a1", Home: t.TempDir()}
	_, err := heldConversation(cfg.Home, cfg, State{AgentID: "a2", SessionID: "s9", AimemFile: "/f"})
	var left *leaveFirst
	if !errors.As(err, &left) || left.starting || left.session != "s9" {
		t.Fatalf("another agent's session: %v", err)
	}
}

// (i) A member enters and records its session after join read the home:
// join's entry is fresh only, so aicrewd refuses it (session_active) and the
// member's session keeps its generation and its record. Reverting join to
// the engine's Start (which resumes the record) fails this test.
func TestJoinEntryBesideAMemberIsRefused(t *testing.T) {
	c := setupCrew(t)
	ctx := context.Background()
	member := c.engine(t)
	if err := member.Start(ctx); err != nil {
		t.Fatal(err)
	}
	st, _, _ := LoadState(c.cfg.Home)
	j := c.engine(t)
	_, err := enterConversation(ctx, c.cfg.Home, c.cfg.AimemHub, j, j.Crew.(SessionReader))
	if codeOf(err) != "session_active" {
		t.Fatalf("join's entry beside the member: %v", err)
	}
	sess, _ := c.store.GetSession(ctx, member.SessionID())
	if sess.State != store.SessionActive || strconv.FormatInt(sess.Generation, 10) != member.session.Generation {
		t.Fatalf("the member's session: %s at generation %d", sess.State, sess.Generation)
	}
	if now, ok, _ := LoadState(c.cfg.Home); !ok || now != st {
		t.Fatalf("the member's record changed: %+v", now)
	}
	if !c.handleActive(t, c.fileHandle(t, member.SessionID())) {
		t.Fatal("the member's binding was touched")
	}
}

// (ii) A member starting after join entered resumes join's recorded
// session, which fences join's token: join's close then leaves nothing,
// takes nothing back and reports it. Reverting join's close to the engine's
// Leave (which proves again and leaves) fails this test.
func TestJoinCloseLeavesATakenOverSessionAlone(t *testing.T) {
	c := setupCrew(t)
	ctx := context.Background()
	j := c.engine(t)
	conv, err := enterConversation(ctx, c.cfg.Home, c.cfg.AimemHub, j, j.Crew.(SessionReader))
	if err != nil {
		t.Fatal(err)
	}
	member := c.engine(t)
	if err := member.Start(ctx); err != nil || member.SessionID() != conv.SessionID {
		t.Fatalf("the member's start: %v (session %s, join's %s)", err, member.SessionID(), conv.SessionID)
	}
	st, _, _ := LoadState(c.cfg.Home)
	if err := conv.Close(ctx); !errors.Is(err, errTakenOver) {
		t.Fatalf("join's close of a session taken over: %v", err)
	}
	sess, _ := c.store.GetSession(ctx, member.SessionID())
	if sess.State != store.SessionActive || strconv.FormatInt(sess.Generation, 10) != member.session.Generation {
		t.Fatalf("the member's session: %s at generation %d", sess.State, sess.Generation)
	}
	if now, ok, _ := LoadState(c.cfg.Home); !ok || now != st {
		t.Fatalf("the member's record changed: %+v %v", now, ok)
	}
	if !c.handleActive(t, c.fileHandle(t, member.SessionID())) {
		t.Fatal("the member's binding was closed or fenced")
	}
}

// join's own close of a conversation nobody took over ends it as the
// engine's Leave does: aicrewd's session left, aimem's binding closed, the
// record cleared.
func TestJoinCloseLeaves(t *testing.T) {
	c := setupCrew(t)
	ctx := context.Background()
	j := c.engine(t)
	conv, err := enterConversation(ctx, c.cfg.Home, c.cfg.AimemHub, j, j.Crew.(SessionReader))
	if err != nil {
		t.Fatal(err)
	}
	if err := conv.Close(ctx); err != nil {
		t.Fatal(err)
	}
	if sess, _ := c.store.GetSession(ctx, conv.SessionID); sess.State != store.SessionLeft {
		t.Fatalf("join's session after its close: %s", sess.State)
	}
	if _, err := os.Stat(sessionFile(c.root, conv.SessionID)); !os.IsNotExist(err) {
		t.Fatal("aimem's binding was not closed")
	}
	if _, ok, _ := LoadState(c.cfg.Home); ok {
		t.Fatal("the record outlived join's conversation")
	}
}

// closeThen runs then after aimem's close: in join's close that is after
// aicrewd has committed join's leave and before the record is removed.
type closeThen struct {
	Aimem
	then func()
}

func (c closeThen) Close(ctx context.Context, id string) error {
	err := c.Aimem.Close(ctx, id)
	c.then()
	return err
}

// A member that starts while join closes its conversation (after aicrewd
// committed join's leave, so its resume of join's session is context_stale)
// enters anew and records its own session; join's close removes the home's
// record only while it names join's session, so the member's survives.
func TestJoinCloseKeepsAMembersNewRecord(t *testing.T) {
	c := setupCrew(t)
	ctx := context.Background()
	j := c.engine(t)
	conv, err := enterConversation(ctx, c.cfg.Home, c.cfg.AimemHub, j, j.Crew.(SessionReader))
	if err != nil {
		t.Fatal(err)
	}
	st, _, _ := LoadState(c.cfg.Home)
	j.Aimem = closeThen{Aimem: j.Aimem, then: func() {
		if sess, _ := c.store.GetSession(ctx, conv.SessionID); sess.State != store.SessionLeft {
			t.Errorf("aimem was closed before aicrewd's leave: %s", sess.State)
		}
		st.SessionID, st.AimemFile = "member-session", "/run/aimem/member-session.json"
		if err := SaveState(c.cfg.Home, st); err != nil {
			t.Error(err)
		}
	}}
	if err := conv.Close(ctx); err != nil {
		t.Fatal(err)
	}
	if sess, _ := c.store.GetSession(ctx, conv.SessionID); sess.State != store.SessionLeft {
		t.Fatalf("join's session after its close: %s", sess.State)
	}
	if now, ok, _ := LoadState(c.cfg.Home); !ok || now.SessionID != "member-session" {
		t.Fatalf("the member's record after join's close: %+v %v", now, ok)
	}
}

// A member bound its session but its launcher has not recorded the team's
// projects yet: join waits for it, and names the leave only for a home
// where no member runs.
func TestHeldConversationWithoutTheTeamYet(t *testing.T) {
	home := t.TempDir()
	cfg := Config{AgentID: "a1", Home: home}
	_, err := heldConversation(home, cfg, State{AgentID: "a1", SessionID: "s7", AimemFile: "/f"})
	var left *leaveFirst
	if !errors.As(err, &left) || !left.starting || left.session != "s7" {
		t.Fatalf("a bound session without the team's record: %v", err)
	}
	step := left.step(home)
	status, leave := strings.Index(step, "aicrew-agent session status"), strings.Index(step, "if no member runs in this home")
	if status < 0 || leave < 0 || status > leave {
		t.Fatalf("the step does not wait for the member first: %s", step)
	}
}

// The reviewer's reader: polling the record while join closes its
// conversation, after a member recorded its own session during the close,
// it sees join's record and then the member's, never nothing.
func TestJoinCloseNeverHidesAMembersRecord(t *testing.T) {
	c := setupCrew(t)
	ctx := context.Background()
	j := c.engine(t)
	conv, err := enterConversation(ctx, c.cfg.Home, c.cfg.AimemHub, j, j.Crew.(SessionReader))
	if err != nil {
		t.Fatal(err)
	}
	st, _, _ := LoadState(c.cfg.Home)
	var recorded atomic.Bool
	j.Aimem = closeThen{Aimem: j.Aimem, then: func() {
		st.SessionID, st.AimemFile = "member-session", "/run/aimem/member-session.json"
		if err := SaveState(c.cfg.Home, st); err != nil {
			t.Error(err)
		}
		recorded.Store(true)
	}}
	stop, done := make(chan struct{}), make(chan struct{})
	var seen []string
	var missing atomic.Bool
	go func() {
		defer close(done)
		for {
			after := recorded.Load()
			now, ok, err := LoadState(c.cfg.Home)
			if err == nil && ok && (len(seen) == 0 || seen[len(seen)-1] != now.SessionID) {
				seen = append(seen, now.SessionID)
			}
			if after && (err != nil || !ok) {
				missing.Store(true)
			}
			select {
			case <-stop:
				return
			default:
			}
		}
	}()
	err = conv.Close(ctx)
	close(stop)
	<-done
	if err != nil {
		t.Fatal(err)
	}
	if missing.Load() {
		t.Fatal("the reader found no record after the member had recorded its own")
	}
	if len(seen) < 2 || seen[0] != conv.SessionID || seen[len(seen)-1] != "member-session" {
		t.Fatalf("the reader saw %v", seen)
	}
}

// A writer that meets the record's lock waits for it, then writes.
func TestSaveStateWaitsForTheRecordsLock(t *testing.T) {
	home := t.TempDir()
	unlock, err := lockRecord(home)
	if err != nil {
		t.Fatal(err)
	}
	wrote := make(chan error, 1)
	go func() { wrote <- SaveState(home, State{AgentID: "a1", SessionID: "s1"}) }()
	select {
	case err := <-wrote:
		t.Fatalf("SaveState wrote while the lock was held: %v", err)
	case <-time.After(200 * time.Millisecond):
	}
	unlock()
	if err := <-wrote; err != nil {
		t.Fatal(err)
	}
	if st, ok, _ := LoadState(home); !ok || st.SessionID != "s1" {
		t.Fatalf("the record after the wait: %+v %v", st, ok)
	}
}

// ClearStateOf removes join's own record, keeps any other, and releases the
// lock on every return, an error included.
func TestClearStateOf(t *testing.T) {
	home := t.TempDir()
	free := func(what string) {
		t.Helper()
		unlock, err := filelock.TryLock(recordLockPath(home))
		if err != nil {
			t.Fatalf("the record's lock after %s: %v", what, err)
		}
		unlock()
	}
	if err := ClearStateOf(home, "join"); err != nil {
		t.Fatalf("a home without state: %v", err)
	}
	if err := SaveState(home, State{AgentID: "a1", SessionID: "join"}); err != nil {
		t.Fatal(err)
	}
	free("SaveState")
	if err := ClearStateOf(home, "join"); err != nil {
		t.Fatal(err)
	}
	free("removing join's record")
	if _, ok, _ := LoadState(home); ok {
		t.Fatal("join's record outlived ClearStateOf")
	}
	if err := SaveState(home, State{AgentID: "a1", SessionID: "member"}); err != nil {
		t.Fatal(err)
	}
	if err := ClearStateOf(home, "join"); err != nil {
		t.Fatal(err)
	}
	free("keeping the member's record")
	if st, ok, _ := LoadState(home); !ok || st.SessionID != "member" {
		t.Fatalf("the member's record: %+v %v", st, ok)
	}
	if err := os.WriteFile(statePath(home), []byte("not a record"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := ClearStateOf(home, "join"); err == nil {
		t.Fatal("an unreadable record was not reported")
	}
	free("an unreadable record")
	if _, err := os.Stat(statePath(home)); err != nil {
		t.Fatalf("an unreadable record was removed: %v", err)
	}
	if err := ClearState(home); err != nil {
		t.Fatal(err)
	}
	free("ClearState")
	entries, _ := os.ReadDir(filepath.Join(home, "state"))
	for _, e := range entries {
		if e.Name() != "aicrew-session.lock" {
			t.Fatalf("a file left in state: %s", e.Name())
		}
	}
}

// A held lock that does not end is reported with the lock's path.
func TestRecordLockTimeoutNamesTheLock(t *testing.T) {
	if testing.Short() {
		t.Skip("waits for the record lock's bound")
	}
	home := t.TempDir()
	unlock, err := lockRecord(home)
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	err = SaveState(home, State{AgentID: "a1", SessionID: "s1"})
	if err == nil || !strings.Contains(err.Error(), recordLockPath(home)) ||
		!strings.Contains(err.Error(), "another aicrew process has held the home's session record") {
		t.Fatalf("the error: %v", err)
	}
}
