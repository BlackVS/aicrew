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
	"strings"
	"testing"

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
				c.Close = func(context.Context) error { r.closed++; return nil }
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
		{"no process", skillsRun{supports: true, projects: []string{"alpha"}, installErr: errors.New("exit status 1"),
			stderr: "no process is selected for project alpha (on the hub host: aimem process select ...)"},
			SkillsNoProcess, []string{"selects no process", "aimem process select"}, true},
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
			if strings.Contains(rep.Instruction, "<project>") {
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
