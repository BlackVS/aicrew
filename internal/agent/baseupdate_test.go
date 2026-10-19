package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/BlackVS/aicrew/internal/automation"
	"github.com/BlackVS/aicrew/internal/forge"
	"github.com/BlackVS/aicrew/internal/store"
)

// baseUpdateEnv is a member's launcher with a claimed attempt whose
// worktree is a clone of a bare origin: the clone's origin names the
// forge's URL, which the launcher's git (automationGit) rewrites to the
// bare repository, so it reaches it as it would reach the forge. src is
// the test's own clone, for the commits that move the base and the branch.
type baseUpdateEnv struct {
	*automationEnv
	bare, src string
}

// tenLines is lines.txt at the base: the branch changes its line 8, and
// a base that changes line 5 changes the branch's patch identity without
// a conflict (the line is in the hunk's context).
const tenLines = "1\n2\n3\n4\n5\n6\n7\n8\n9\n10\n"

func setupBaseUpdate(t *testing.T) *baseUpdateEnv {
	t.Helper()
	s := setupStepsWith(t, crewOptions{role: store.RoleIndependent, steps: true, shortHome: true})
	id := s.claimed(t)
	provisionTestForge(t, s.cfg.Home, "git.example.test", automationForgeToken)
	srv := s.serve(t)

	dir := t.TempDir()
	bare, src := filepath.Join(dir, "project.git"), filepath.Join(dir, "src")
	git(t, dir, "init", "-q", "--bare", "-b", "main", bare)
	git(t, dir, "clone", "-q", bare, src)
	e := &baseUpdateEnv{bare: bare, src: src}
	e.write(t, "lines.txt", tenLines)
	git(t, src, "add", "lines.txt")
	git(t, src, "commit", "-q", "-m", "base")
	git(t, src, "push", "-q", "origin", "HEAD:refs/heads/main")
	git(t, src, "checkout", "-q", "-b", "work/task-1")
	e.write(t, "lines.txt", strings.Replace(tenLines, "8\n", "eight\n", 1))
	e.write(t, "work.txt", "work\n")
	git(t, src, "add", ".")
	git(t, src, "commit", "-q", "-m", "work")
	git(t, src, "push", "-q", "origin", "work/task-1")
	head := git(t, src, "rev-parse", "HEAD")
	// The base moves on: the pull request is behind it.
	e.moveBase(t, "moved.txt", "the base moved\n")

	wt := filepath.Join(s.cfg.Home, "worktrees", id)
	if err := os.MkdirAll(filepath.Dir(wt), 0o700); err != nil {
		t.Fatal(err)
	}
	git(t, filepath.Dir(wt), "clone", "-q", "-b", "work/task-1", bare, wt)
	git(t, wt, "config", "user.name", "member")
	git(t, wt, "config", "user.email", "member@example.test")
	git(t, wt, "fetch", "-q", "origin")
	git(t, wt, "remote", "set-url", "origin", testProjectRepo)
	oldGit := automationGit
	automationGit = func(ctx context.Context, dir string, env []string, args ...string) (string, error) {
		env = append(env, "GIT_CONFIG_COUNT=1", "GIT_CONFIG_KEY_0=url."+filepath.ToSlash(bare)+".insteadOf", "GIT_CONFIG_VALUE_0="+testProjectRepo)
		return execGit(ctx, dir, env, args...)
	}
	t.Cleanup(func() { automationGit = oldGit })

	f := &automationForge{token: automationForgeToken,
		pull: forge.Pull{Number: 7, URL: "https://git.example.test/crew/project-t/pulls/7", State: forge.PullOpen, Draft: true,
			Head: "work/task-1", HeadCommit: head, Base: "main", Mergeable: true}}
	old := newAutomationForge
	newAutomationForge = func() AutomationForge { return f }
	t.Cleanup(func() { newAutomationForge = old })
	e.automationEnv = &automationEnv{stepEnv: s, srv: srv, id: id, head: head, worktree: wt, forge: f}
	return e
}

func (e *baseUpdateEnv) write(t *testing.T, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(e.src, name), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

// moveBase commits name with content on main and pushes it.
func (e *baseUpdateEnv) moveBase(t *testing.T, name, content string) string {
	t.Helper()
	git(t, e.src, "checkout", "-q", "main")
	e.write(t, name, content)
	git(t, e.src, "add", name)
	git(t, e.src, "commit", "-q", "-m", "base: "+name)
	git(t, e.src, "push", "-q", "origin", "main")
	return git(t, e.src, "rev-parse", "HEAD")
}

// remote is the bare origin's head of ref.
func (e *baseUpdateEnv) remote(t *testing.T, ref string) string {
	t.Helper()
	return git(t, e.bare, "rev-parse", ref)
}

// updateScript calls update_base() and records its answer as a step, so
// the test reads what the script saw; a conflict is returned as a turn.
const updateScript = `
def main(event):
    u = update_base()
    step("u", "|".join([str(u.updated), u.old_head, u.new_head, str(u.identity_changed), u.reason]))
    if u.conflict:
        return turn("fix", "conflict in " + ",".join(u.conflict.files), conflict = u.conflict)
    return None
`

// update_base() brings the branch up to date with a merge of the base in a
// worktree of its own, pushes without force when the patch identity is
// unchanged, posts the delta note and fast-forwards the attempt's clean
// worktree; a branch up to date is left alone; a push the origin refuses
// because the branch moved is reported, not retried; a merge that changes
// the patch identity is not pushed; a conflict is aborted and returned
// with its base, files and hunks, and the turn carries it. The temporary
// worktree is gone after each call, the attempt's worktree stays clean,
// and no credential reaches the script's process or the log.
func TestUpdateBase(t *testing.T) {
	e := setupBaseUpdate(t)
	ctx := context.Background()
	wt, old := e.worktree, e.head
	temp := wt + ".base-update"
	gone := func(when string) {
		t.Helper()
		if _, err := os.Stat(temp); err == nil {
			t.Errorf("%s: the temporary worktree is still there", when)
		}
		if wts := git(t, wt, "worktree", "list"); strings.Count(wts, "\n") != 0 {
			t.Errorf("%s: the worktrees:\n%s", when, wts)
		}
		if st := git(t, wt, "status", "--porcelain", "--untracked-files=no"); st != "" {
			t.Errorf("%s: the attempt's worktree is not clean:\n%s", when, st)
		}
	}
	answer := func(t *testing.T, run string) string {
		t.Helper()
		for _, r := range e.records(t, run) {
			if r.Kind == "u" {
				return r.Text
			}
		}
		t.Fatalf("run %s recorded no answer", run)
		return ""
	}

	// 1. A clean update: merged, pushed, the note posted, the attempt's
	// worktree fast-forwarded.
	identity := patchIdentity(ctx, wt, "main", old)
	if identity == "" {
		t.Fatal("no patch identity of the head")
	}
	// An untracked file in the attempt's worktree is no obstacle to its
	// fast-forward.
	if err := os.WriteFile(filepath.Join(wt, "scratch.txt"), []byte("untracked\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	res := e.run(t, updateScript, AutomationRun{Event: automation.Event{Kinds: []string{automation.PointAfterCI}}})
	if res.Failed != "" || res.Turn != nil {
		t.Fatalf("the run: %+v", res)
	}
	head := e.remote(t, "refs/heads/work/task-1")
	if head == old || answer(t, res.RunID) != "True|"+old+"|"+head+"|False|" {
		t.Fatalf("the answer %q, the origin's head %s (was %s)", answer(t, res.RunID), head, old)
	}
	if parents := git(t, e.bare, "rev-list", "--parents", "-n", "1", head); parents != head+" "+old+" "+e.remote(t, "refs/heads/main") {
		t.Errorf("the new head's parents: %q", parents)
	}
	if msg := git(t, e.bare, "log", "-1", "--format=%s", head); msg != "Merge main into work/task-1 (base-only update)" {
		t.Errorf("the merge's message: %q", msg)
	}
	if got := git(t, wt, "rev-parse", "HEAD"); got != head {
		t.Errorf("the attempt's worktree is at %s, not fast-forwarded to %s", got, head)
	}
	if got := patchIdentity(ctx, wt, "main", head); got != identity {
		t.Errorf("the identity changed: %s, was %s", got, identity)
	}
	note := fmt.Sprintf("Base-only update: %s -> %s, merged from main.\nPatch identity unchanged: %s\n", old, head, identity)
	if len(e.forge.comments) != 1 || !strings.HasPrefix(e.forge.comments[0], note) || !strings.Contains(e.forge.comments[0], "git patch-id --stable") {
		t.Errorf("the comments: %q", e.forge.comments)
	}
	if got := lastStep(e.records(t, res.RunID), stepHostUpdateBase); got != old+" -> "+head {
		t.Errorf("the step: %q", got)
	}
	if rec := e.baseUpdate(); rec != nil {
		t.Errorf("a run that ended left the update in progress: %+v", rec)
	}
	gone("after the update")
	// The script's process was given no credential, and the log holds none.
	raw, err := os.ReadFile(filepath.Join(filepath.Dir(StepSocket(e.cfg.Home)), childReport))
	if err != nil {
		t.Fatal(err)
	}
	var rep struct{ Env []string }
	if json.Unmarshal(raw, &rep) != nil || strings.Contains(string(raw), automationForgeToken) || len(rep.Env) > 1 {
		t.Errorf("the child's report: %s", raw)
	}
	if strings.Contains(e.logs.String(), automationForgeToken) {
		t.Error("the forge token reached the log")
	}

	// 2. Up to date: nothing is done.
	res = e.run(t, updateScript, AutomationRun{})
	if res.Failed != "" || answer(t, res.RunID) != "False|"+head+"||False|the branch is up to date with main" || len(e.forge.comments) != 1 {
		t.Fatalf("up to date: %+v, %q, %q", res, answer(t, res.RunID), e.forge.comments)
	}
	gone("up to date")

	// 3. The branch moves during the update: the push is refused, and
	// nothing else happens; the next run starts from the moved head.
	e.moveBase(t, "moved-2.txt", "the base moved again\n")
	git(t, e.src, "fetch", "-q", "origin")
	git(t, e.src, "checkout", "-q", "-B", "work/task-1", "origin/work/task-1")
	inner := automationGit
	automationGit = func(ctx context.Context, dir string, env []string, args ...string) (string, error) {
		if len(args) > 0 && args[0] == "push" {
			e.write(t, "more.txt", "more work\n")
			git(t, e.src, "add", "more.txt")
			git(t, e.src, "commit", "-q", "-m", "more work")
			git(t, e.src, "push", "-q", "origin", "work/task-1")
		}
		return inner(ctx, dir, env, args...)
	}
	res = e.run(t, updateScript, AutomationRun{})
	automationGit = inner
	moved := git(t, e.src, "rev-parse", "HEAD")
	if res.Failed != "" || !strings.HasPrefix(answer(t, res.RunID), "False|"+head+"||False|the push of work/task-1 was refused, the branch moved during the update:") ||
		e.remote(t, "refs/heads/work/task-1") != moved || len(e.forge.comments) != 1 {
		t.Fatalf("a refused push: %+v, %q, the origin's head %s", res, answer(t, res.RunID), e.remote(t, "refs/heads/work/task-1"))
	}
	if got := lastStep(e.records(t, res.RunID), stepHostUpdateBase); !strings.HasPrefix(got, "not updated: the push of work/task-1 was refused") {
		t.Errorf("the step: %q", got)
	}
	// The record of the refused push stays, for the next call to read the
	// origin's head against: a push the origin applied but did not
	// acknowledge is completed, any other is stale.
	if rec := e.baseUpdate(); rec == nil || rec.Old != head || rec.Pushed || rec.Commented {
		t.Errorf("the record after the refused push: %+v", rec)
	}
	gone("after the refused push")

	// 4. The merge changes the patch identity: the base changed a line in
	// the branch's hunk's context. Nothing is pushed; the attempt's
	// worktree, no longer at the branch's head, is left where it is.
	e.moveBase(t, "lines.txt", strings.Replace(tenLines, "5\n", "five\n", 1))
	res = e.run(t, updateScript, AutomationRun{})
	if res.Failed != "" || res.Turn != nil {
		t.Fatalf("a changed identity: %+v", res)
	}
	if a := answer(t, res.RunID); !strings.HasPrefix(a, "False|"+moved+"||True|merging main into work/task-1 changes the patch identity from ") ||
		!strings.HasSuffix(a, "; update the branch by hand") {
		t.Errorf("the answer: %q", a)
	}
	if e.remote(t, "refs/heads/work/task-1") != moved || git(t, wt, "rev-parse", "HEAD") != head || len(e.forge.comments) != 1 {
		t.Errorf("a changed identity pushed, moved the worktree or commented: %s %s %q", e.remote(t, "refs/heads/work/task-1"),
			git(t, wt, "rev-parse", "HEAD"), e.forge.comments)
	}
	if rec := e.baseUpdate(); rec != nil {
		t.Errorf("the stale record of the refused push was kept: %+v", rec)
	}
	gone("after the changed identity")

	// 5. A conflict: aborted, returned with its hunks, carried by the turn.
	e.moveBase(t, "work.txt", "base\n")
	res = e.run(t, updateScript, AutomationRun{})
	if res.Failed != "" || res.Turn == nil || res.Turn.Kind != "fix" || res.Turn.Content != "conflict in work.txt" {
		t.Fatalf("a conflict: %+v", res)
	}
	c := res.Turn.Conflict
	if c == nil || c.Base != "main" || !slices.Equal(c.Files, []string{"work.txt"}) || !strings.Contains(c.Hunks, "<<<<<<<") ||
		!strings.Contains(c.Hunks, "+work") && !strings.Contains(c.Hunks, " work") || !strings.Contains(c.Hunks, "base") {
		t.Errorf("the turn's conflict: %+v", c)
	}
	if a := answer(t, res.RunID); a != "False|"+moved+"||False|merging main into work/task-1 conflicts in work.txt" {
		t.Errorf("the answer: %q", a)
	}
	if e.remote(t, "refs/heads/work/task-1") != moved || git(t, wt, "rev-parse", "HEAD") != head || len(e.forge.comments) != 1 {
		t.Errorf("a conflict pushed, moved the worktree or commented")
	}
	gone("after the conflict")
	recs := e.records(t, res.RunID)
	if len(recs) == 0 || recs[len(recs)-1].Outcome != "turn" || recs[len(recs)-1].Turn != "fix" {
		t.Errorf("the records: %+v", recs)
	}
}

// baseUpdate is the base update the points keep in progress for the pull
// request, nil when none.
func (e *baseUpdateEnv) baseUpdate() *pointBaseUpdate {
	if p := readPoints(e.cfg.Home).Pulls[pullKey("git.example.test", "crew/project-t", 7)]; p != nil {
		return p.BaseUpdate
	}
	return nil
}

// An update cut between its push and its delta note (DESIGN-TURNS 12.4,
// increment 2e-2): the launcher records the update before the push and
// after each effect, so the next call, finding the origin's head at the
// recorded new head, posts the note, fast-forwards the attempt's
// worktree, records the step and clears the record, with no merge and no
// push; a call after that finds the branch up to date and repeats
// nothing. The cut is the run's context cancelled from the git seam right
// after the push, with the forge refusing the note on it; the restart is
// the step server closed and served again, the file being its only
// memory.
func TestUpdateBaseCompletesACutUpdate(t *testing.T) {
	e := setupBaseUpdate(t)
	wt, old := e.worktree, e.head
	identity := patchIdentity(context.Background(), wt, "main", old)
	if identity == "" {
		t.Fatal("no patch identity of the head")
	}
	answer := func(t *testing.T, run string) string {
		t.Helper()
		for _, r := range e.records(t, run) {
			if r.Kind == "u" {
				return r.Text
			}
		}
		return ""
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// The seam counts every push for the whole test, and cuts the run at
	// the first one.
	var pushes atomic.Int32
	inner := automationGit
	automationGit = func(ctx context.Context, dir string, env []string, args ...string) (string, error) {
		out, err := inner(ctx, dir, env, args...)
		if len(args) > 0 && args[0] == "push" && pushes.Add(1) == 1 {
			cancel()
		}
		return out, err
	}
	t.Cleanup(func() { automationGit = inner })
	res, err := e.srv.RunAutomation(ctx, AutomationRun{AttemptID: e.id, Source: []byte(updateScript), Name: "test.star", Pull: 7})
	if err != nil {
		t.Fatal(err)
	}
	// The exit: the server's close waits for the cut call's host side,
	// whose git and forge calls fail on the cancelled context, so the
	// record is read at rest.
	e.srv.Close()
	head := e.remote(t, "refs/heads/work/task-1")
	if head == old || pushes.Load() != 1 || len(e.forge.comments) != 0 {
		t.Fatalf("the cut run: %+v, the origin's head %s (was %s), %d pushes, the comments %q", res, head, old, pushes.Load(), e.forge.comments)
	}
	rec := e.baseUpdate()
	if rec == nil || rec.Old != old || rec.New != head || rec.Base != "main" || rec.Identity != identity || !rec.Pushed || rec.Commented {
		t.Fatalf("the record after the cut: %+v", rec)
	}

	// The restart, and the next call: completed, no merge, no push.
	e.srv = e.serve(t)
	res = e.run(t, updateScript, AutomationRun{})
	if res.Failed != "" || res.Turn != nil || answer(t, res.RunID) != "True|"+old+"|"+head+"|False|completed the update cut after its push" {
		t.Fatalf("the completing run: %+v, the answer %q", res, answer(t, res.RunID))
	}
	note := fmt.Sprintf("Base-only update: %s -> %s, merged from main.\nPatch identity unchanged: %s\n", old, head, identity)
	if len(e.forge.comments) != 1 || !strings.HasPrefix(e.forge.comments[0], note) {
		t.Errorf("the comments: %q", e.forge.comments)
	}
	if got := lastStep(e.records(t, res.RunID), stepHostUpdateBase); got != old+" -> "+head+"; completed the update cut after its push" {
		t.Errorf("the step: %q", got)
	}
	if pushes.Load() != 1 || e.remote(t, "refs/heads/work/task-1") != head {
		t.Errorf("the completion pushed: %d pushes, the origin's head %s", pushes.Load(), e.remote(t, "refs/heads/work/task-1"))
	}
	if got := git(t, wt, "rev-parse", "HEAD"); got != head {
		t.Errorf("the attempt's worktree is at %s, not fast-forwarded to %s", got, head)
	}
	if rec := e.baseUpdate(); rec != nil {
		t.Errorf("the completed update is still recorded: %+v", rec)
	}

	// Nothing to repeat.
	res = e.run(t, updateScript, AutomationRun{})
	if res.Failed != "" || answer(t, res.RunID) != "False|"+head+"||False|the branch is up to date with main" ||
		len(e.forge.comments) != 1 || pushes.Load() != 1 {
		t.Errorf("a call after the completion: %+v, %q, %d pushes, the comments %q", res, answer(t, res.RunID), pushes.Load(), e.forge.comments)
	}
	if _, err := os.Stat(wt + ".base-update"); err == nil {
		t.Error("the temporary worktree is still there")
	}
}

// patchIdentity is the stable patch id of a head against its merge base
// with the base branch as the worktree's origin has it: the same change
// on a newer base has the same identity, a different change another; an
// unknown head, a base or head of the wrong shape, a missing worktree and
// an empty diff have none.
func TestPatchIdentity(t *testing.T) {
	ctx := context.Background()
	wt := t.TempDir()
	write := func(name, content string) {
		if err := os.WriteFile(filepath.Join(wt, name), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	git(t, wt, "init", "-q", "-b", "main")
	write("a.txt", "a\n")
	git(t, wt, "add", "a.txt")
	git(t, wt, "commit", "-q", "-m", "base")
	base := git(t, wt, "rev-parse", "HEAD")
	git(t, wt, "checkout", "-q", "-b", "work")
	write("b.txt", "b\n")
	git(t, wt, "add", "b.txt")
	git(t, wt, "commit", "-q", "-m", "work")
	head := git(t, wt, "rev-parse", "HEAD")
	git(t, wt, "update-ref", "refs/remotes/origin/main", base)
	id := patchIdentity(ctx, wt, "main", head)
	if !baseShape.MatchString(id) {
		t.Fatalf("the identity: %q", id)
	}
	// The base moves and the change is rebased onto it: the same identity.
	git(t, wt, "checkout", "-q", "main")
	write("c.txt", "c\n")
	git(t, wt, "add", "c.txt")
	git(t, wt, "commit", "-q", "-m", "base moved")
	moved := git(t, wt, "rev-parse", "HEAD")
	git(t, wt, "update-ref", "refs/remotes/origin/main", moved)
	git(t, wt, "checkout", "-q", "work")
	git(t, wt, "rebase", "-q", "main")
	rebased := git(t, wt, "rev-parse", "HEAD")
	// Another change: another identity.
	write("b.txt", "bb\n")
	git(t, wt, "commit", "-q", "-a", "-m", "more")
	more := git(t, wt, "rev-parse", "HEAD")
	for _, c := range []struct {
		name, worktree, base, head, want string
	}{
		{"the rebased change", wt, "main", rebased, id},
		{"another change", wt, "main", more, "other"},
		{"the base itself", wt, "main", moved, ""},
		{"an unknown head", wt, "main", strings.Repeat("0", 40), ""},
		{"a head that is not a commit id", wt, "main", "HEAD", ""},
		{"no base", wt, "", rebased, ""},
		{"a base with a parent path", wt, "../main", rebased, ""},
		{"a missing worktree", filepath.Join(wt, "none"), "main", rebased, ""},
	} {
		got := patchIdentity(ctx, c.worktree, c.base, c.head)
		switch {
		case c.want == "other" && (got == "" || got == id):
			t.Errorf("%s: %q", c.name, got)
		case c.want != "other" && got != c.want:
			t.Errorf("%s: %q, want %q", c.name, got, c.want)
		}
	}
}
