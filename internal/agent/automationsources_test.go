package agent

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/BlackVS/aicrew/internal/automation"
	"github.com/BlackVS/aicrew/internal/store"
)

const (
	testProcessRepo = "https://git.example.test/team/process.git"
	testProjectRepo = "https://git.example.test/crew/project-t.git"
)

// script is a valid workflow script whose body names v, so that two
// versions differ.
func script(v string) string {
	return automation.HeaderPrefix + `{"host_api": 1}` + "\n# " + v + "\ndef main(event):\n    return None\n"
}

// automationSources is a process repository and a project repository, as
// bare repositories the test's git maps their https URLs to, and the
// release defaults the test sets.
type automationSources struct {
	dir, process, project    string // the working copies
	processBare, projectBare string
	pin                      string // the process's first commit
}

func (a *automationSources) commit(t *testing.T, repo, bare, branch string, files map[string]string) string {
	t.Helper()
	for p, body := range files {
		f := filepath.Join(repo, filepath.FromSlash(p))
		os.MkdirAll(filepath.Dir(f), 0o755)
		if err := os.WriteFile(f, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	git(t, repo, "add", "-A")
	git(t, repo, "commit", "-q", "-m", "change")
	git(t, repo, "push", "-q", bare, "HEAD:refs/heads/"+branch)
	return git(t, repo, "rev-parse", "HEAD")
}

const testManifest = `{
  "name": "delivery",
  "automations": [
    {"name": "base-update", "source": {"process": "process/base-update.star"}, "point": "after-ci"},
    {"name": "gates-green", "source": "disabled"},
    {"name": "project-check", "source": {"repository": "automations/check.star"}, "point": "after-push", "turn": "rework"}
  ]
}`

func setupAutomationSources(t *testing.T) *automationSources {
	t.Helper()
	dir := t.TempDir()
	a := &automationSources{dir: dir, process: filepath.Join(dir, "process-src"), project: filepath.Join(dir, "project-src"),
		processBare: filepath.Join(dir, "process.git"), projectBare: filepath.Join(dir, "project.git")}
	for _, r := range [][2]string{{a.process, a.processBare}, {a.project, a.projectBare}} {
		git(t, dir, "init", "-q", "-b", "main", r[0])
		git(t, r[0], "config", "core.autocrlf", "false")
		git(t, dir, "init", "-q", "--bare", r[1])
	}
	a.pin = a.commit(t, a.process, a.processBare, "main", map[string]string{
		"process/manifest.json": testManifest, "process/base-update.star": script("process v1")})
	a.commit(t, a.project, a.projectBare, "main", map[string]string{
		"README.md": "project\n", "automations/check.star": script("base v1")})
	// The attempt's head changes the script; the head is never read.
	git(t, a.project, "checkout", "-q", "-b", "work/task-1")
	a.commit(t, a.project, a.projectBare, "work/task-1", map[string]string{"automations/check.star": script("head")})
	git(t, a.project, "checkout", "-q", "main")

	processURL, projectURL := filepath.ToSlash(a.processBare), filepath.ToSlash(a.projectBare)
	oldGit, oldSelf := automationGit, automationSelf
	automationGit = func(ctx context.Context, dir string, env []string, args ...string) (string, error) {
		env = append(env, "GIT_CONFIG_COUNT=2",
			"GIT_CONFIG_KEY_0=url."+processURL+".insteadOf", "GIT_CONFIG_VALUE_0="+testProcessRepo,
			"GIT_CONFIG_KEY_1=url."+projectURL+".insteadOf", "GIT_CONFIG_VALUE_1="+testProjectRepo)
		return execGit(ctx, dir, env, args...)
	}
	automationSelf = "/opt/aicrew/aicrew-agent"
	oldRelease, oldFS := releaseAutomations, releaseFS
	releaseAutomations = func() ([]automation.Entry, error) {
		return []automation.Entry{
			{Name: "base-update", Source: automation.SourceRelease, Script: "base-update.star", Point: automation.PointAfterCI},
			{Name: "gates-green", Source: automation.SourceRelease, Script: "gates-green.star", Point: automation.PointGatesGreen},
			{Name: "ci-rerun", Source: automation.SourceRelease, Script: "ci-rerun.star", Point: automation.PointCIFailed},
		}, nil
	}
	releaseFS = fstest.MapFS{
		"automations/base-update.star": {Data: []byte(script("release base-update"))},
		"automations/gates-green.star": {Data: []byte(script("release gates-green"))},
		"automations/ci-rerun.star":    {Data: []byte(script("release ci-rerun"))},
	}
	t.Cleanup(func() {
		automationGit, automationSelf = oldGit, oldSelf
		releaseAutomations, releaseFS = oldRelease, oldFS
	})
	return a
}

func (a *automationSources) sources(pin string) AutomationSources {
	return AutomationSources{Process: &ProcessPin{Repository: testProcessRepo, Commit: pin, Manifest: "process/manifest.json"},
		Repository: testProjectRepo, BaseBranch: "main"}
}

func entryNamed(t *testing.T, set *AutomationSet, name string) AutomationEntry {
	t.Helper()
	for _, e := range set.Entries {
		if e.Name == name {
			return e
		}
	}
	t.Fatalf("no entry %s in %+v", name, set.Entries)
	return AutomationEntry{}
}

func hasVersion(e AutomationEntry, v string) bool {
	return strings.Contains(string(e.Code), "# "+v+"\n")
}

// The effective set: the process replaces a default and disables another, a
// default it does not name applies, and its project-only name is added;
// each script is read from its source at the pin, the base branch's merged
// commit or the release, and the repository's script is the base's, not
// the one the attempt's head changed.
func TestAutomationSources(t *testing.T) {
	a := setupAutomationSources(t)
	srv := &StepServer{home: t.TempDir()}
	set, err := srv.Automations(t.Context(), a.sources(a.pin))
	if err != nil {
		t.Fatal(err)
	}
	base := git(t, a.project, "rev-parse", "main")
	var names, sources []string
	for _, e := range set.Entries {
		names, sources = append(names, e.Name), append(sources, e.Source)
		if e.Problem != "" {
			t.Errorf("%s: %s", e.Name, e.Problem)
		}
	}
	if !slices.Equal(names, []string{"base-update", "gates-green", "ci-rerun", "project-check"}) ||
		!slices.Equal(sources, []string{"process", "disabled", "release", "repository"}) {
		t.Fatalf("the effective set: %v %v", names, sources)
	}
	if e := entryNamed(t, set, "base-update"); e.Commit != a.pin || !hasVersion(e, "process v1") || e.Point != automation.PointAfterCI {
		t.Errorf("the process's base-update: %+v %q", e, e.Code)
	}
	if e := entryNamed(t, set, "gates-green"); e.Code != nil || e.Commit != "" {
		t.Errorf("the disabled gates-green: %+v", e)
	}
	if e := entryNamed(t, set, "ci-rerun"); !strings.HasPrefix(e.Commit, "release ") || !hasVersion(e, "release ci-rerun") {
		t.Errorf("the release's ci-rerun: %+v", e)
	}
	e := entryNamed(t, set, "project-check")
	if e.Commit != base || set.BaseCommit != base || set.Base != "main" || !hasVersion(e, "base v1") || e.Turn != "rework" {
		t.Errorf("the repository's project-check: %+v %q, base %s", e, e.Code, set.BaseCommit)
	}
	if hasVersion(e, "head") {
		t.Error("the script was read from the attempt's head")
	}
	// Without a pin, the release defaults alone.
	rel, err := srv.Automations(t.Context(), AutomationSources{})
	if err != nil || len(rel.Entries) != 3 || rel.Entries[0].Source != automation.SourceRelease || rel.BaseCommit != "" {
		t.Fatalf("the release defaults alone: %+v, %v", rel, err)
	}
}

// The kept set is read again, without a restart, when the base branch's
// commit changes automations/ or the pin changes, and only then.
func TestAutomationSourcesReload(t *testing.T) {
	a := setupAutomationSources(t)
	srv := &StepServer{home: t.TempDir()}
	ctx := t.Context()
	first, err := srv.Automations(ctx, a.sources(a.pin))
	if err != nil {
		t.Fatal(err)
	}
	if again, err := srv.Automations(ctx, a.sources(a.pin)); err != nil || again != first {
		t.Fatalf("nothing changed, and the set was read again: %v", err)
	}
	// A base commit that leaves automations/ alone keeps the set.
	a.commit(t, a.project, a.projectBare, "main", map[string]string{"README.md": "changed\n"})
	if kept, err := srv.Automations(ctx, a.sources(a.pin)); err != nil || kept != first {
		t.Fatalf("a base commit outside automations/ reloaded the set: %v", err)
	}
	// A base commit that changes automations/ reloads it.
	moved := a.commit(t, a.project, a.projectBare, "main", map[string]string{"automations/check.star": script("base v2")})
	reloaded, err := srv.Automations(ctx, a.sources(a.pin))
	if err != nil || reloaded == first {
		t.Fatalf("the base's automations/ changed, and the set was kept: %v", err)
	}
	if e := entryNamed(t, reloaded, "project-check"); e.Commit != moved || !hasVersion(e, "base v2") {
		t.Errorf("after the base moved: %+v %q", e, e.Code)
	}
	// A new pin reloads it.
	pin2 := a.commit(t, a.process, a.processBare, "main", map[string]string{"process/base-update.star": script("process v2")})
	repinned, err := srv.Automations(ctx, a.sources(pin2))
	if err != nil || repinned == reloaded {
		t.Fatalf("the pin changed, and the set was kept: %v", err)
	}
	if e := entryNamed(t, repinned, "base-update"); e.Commit != pin2 || !hasVersion(e, "process v2") {
		t.Errorf("after the pin changed: %+v %q", e, e.Code)
	}
	// The old pin still reads its own script.
	if back, err := srv.Automations(ctx, a.sources(a.pin)); err != nil || !hasVersion(entryNamed(t, back, "base-update"), "process v1") {
		t.Errorf("the old pin again: %v", err)
	}
}

// A script that cannot run is its entry's problem, and the set still
// resolves; a manifest the release cannot read is an error.
func TestAutomationSourcesProblems(t *testing.T) {
	a := setupAutomationSources(t)
	srv := &StepServer{home: t.TempDir()}
	ctx := t.Context()
	pin := a.commit(t, a.process, a.processBare, "main", map[string]string{
		"process/manifest.json": `{"automations": [
			{"name": "base-update", "source": {"process": "process/missing.star"}, "point": "after-ci"},
			{"name": "newer", "source": {"process": "process/newer.star"}, "point": "after-ci"},
			{"name": "unheaded", "source": {"process": "process/unheaded.star"}, "point": "after-ci"},
			{"name": "elsewhere", "source": {"process": "other/x.star"}, "point": "after-ci"}]}`,
		"other/x.star":          script("not beside the manifest"),
		"process/newer.star":    automation.HeaderPrefix + `{"host_api": 99}` + "\ndef main(event):\n    return None\n",
		"process/unheaded.star": "def main(event):\n    return None\n"})
	set, err := srv.Automations(ctx, a.sources(pin))
	if err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string]string{"base-update": "is not in the commit", "newer": "host API 99", "unheaded": "first line",
		"elsewhere": "not beside the manifest"} {
		if e := entryNamed(t, set, name); !strings.Contains(e.Problem, want) || e.Code != nil {
			t.Errorf("%s: %+v", name, e)
		}
	}
	if e := entryNamed(t, set, "ci-rerun"); e.Problem != "" {
		t.Errorf("a default the process does not name: %+v", e)
	}
	bad := a.commit(t, a.process, a.processBare, "main", map[string]string{
		"process/manifest.json": `{"automations": [{"name": "x", "source": {"release": "x.star"}, "point": "after-ci"}]}`})
	if _, err := srv.Automations(ctx, a.sources(bad)); err == nil || !strings.Contains(err.Error(), "the release's own") {
		t.Errorf("a manifest naming the release source: %v", err)
	}
	if _, err := srv.Automations(ctx, a.sources(strings.Repeat("0", 40))); err == nil || !strings.Contains(err.Error(), "is not in") {
		t.Errorf("a pin the repository does not hold: %v", err)
	}
	src := a.sources(a.pin)
	src.BaseBranch = "-x"
	if _, err := srv.Automations(ctx, src); err == nil || !strings.Contains(err.Error(), "not a branch name") {
		t.Errorf("a base branch that is not a branch name: %v", err)
	}
}

// The launcher resolves the effective set of the member's open attempt
// from the pin and the repository the session status reports, and check
// lists it.
func TestAutomationsInCheck(t *testing.T) {
	a := setupAutomationSources(t)
	s := setupStepsWith(t, crewOptions{role: store.RoleIndependent, steps: true, shortHome: true})
	body := s.claimBody()
	body["process"] = map[string]string{"repo": testProcessRepo, "commit": a.pin, "manifest": "process/manifest.json"}
	r, err := s.run(t, attemptsPath+"/claim", body)
	if err != nil || r.Outcome != "committed" {
		t.Fatalf("claim: %+v %v", r, err)
	}
	s.serve(t)
	c := &checker{o: CheckOptions{Home: s.cfg.Home}}
	c.checkAutomations(t.Context())
	rep := c.rep.Automations
	if rep == nil {
		t.Fatalf("no automations in the check; notices %v", c.rep.Notices)
	}
	if rep.Attempt != r.AttemptID || rep.Process == nil || rep.Process.Commit != a.pin || len(rep.Entries) != 4 {
		t.Fatalf("the check's automations: %+v", rep)
	}
	if e := entryNamed(t, rep.AutomationSet, "project-check"); e.Source != automation.SourceRepository ||
		e.Commit != git(t, a.project, "rev-parse", "main") || e.Point != automation.PointAfterPush {
		t.Errorf("project-check in the check: %+v", e)
	}
	if len(c.rep.Notices) != 0 {
		t.Errorf("notices: %v", c.rep.Notices)
	}
}
