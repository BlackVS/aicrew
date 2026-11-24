package main

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
)

// The below-minimum run shows its state only while the pinned aimem is below
// the minimum of this tree's supported set.
func TestBelowMinimumIsBelow(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "..", "..", "internal", "agent", "supported.json"))
	if err != nil {
		t.Fatal(err)
	}
	var set struct {
		Components map[string]struct {
			Ranges []struct{ Min string }
		}
	}
	if err := json.Unmarshal(raw, &set); err != nil {
		t.Fatal(err)
	}
	r := set.Components["aimem"].Ranges
	if len(r) == 0 {
		t.Fatal("supported.json has no aimem range")
	}
	if !versionBelow(belowMinimumAimem, r[0].Min) {
		t.Fatalf("aimem %s is not below this tree's minimum %s: the below-minimum run would show nothing; "+
			"pick a new pair (simupgrade, belowMinimumAicrew and belowMinimumAimem)", belowMinimumAimem, r[0].Min)
	}
	if !releaseTag.MatchString(belowMinimumAicrew) {
		t.Fatalf("%q is not a release tag", belowMinimumAicrew)
	}
}

// versionBelow compares two X.Y.Z versions.
func versionBelow(a, b string) bool {
	pa, pb := strings.Split(a, "."), strings.Split(b, ".")
	for i := 0; i < 3; i++ {
		var x, y int
		if _, err := fmt.Sscan(pa[i], &x); err != nil {
			return false
		}
		if _, err := fmt.Sscan(pb[i], &y); err != nil {
			return false
		}
		if x != y {
			return x < y
		}
	}
	return false
}

// snapshot writes a step snapshot of a first cycle in dir, as
// pilot-sim.sh's take writes one: ids.json, aicrewd's store with every
// table same reads, aimem's reads and the homes.
func snapshot(t *testing.T, dir string) {
	t.Helper()
	write(t, dir, "ids.json", `{"task":"T1","worker":"W","coordinator":"C","indep":"I","first_head":""}`)
	if err := os.MkdirAll(filepath.Join(dir, "store"), 0o755); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", filepath.Join(dir, "store", "aicrew.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, s := range []string{
		`CREATE TABLE attempts (id TEXT PRIMARY KEY, task_id TEXT, state TEXT, delivery_evidence TEXT)`,
		`CREATE TABLE attempt_steps (request_key TEXT PRIMARY KEY, attempt_id TEXT, operation TEXT)`,
		`CREATE TABLE attempt_results (attempt_id TEXT, seq INTEGER, result_ref TEXT)`,
		`CREATE TABLE attempt_pulls (attempt_id TEXT PRIMARY KEY, number INTEGER)`,
		`CREATE TABLE attempt_pull_facts (attempt_id TEXT PRIMARY KEY, facts TEXT)`,
		`CREATE TABLE attempt_handoffs (attempt_id TEXT PRIMARY KEY, body TEXT)`,
		`CREATE TABLE attempt_blocks (attempt_id TEXT PRIMARY KEY, reason TEXT)`,
		`CREATE TABLE attempt_automation_runs (attempt_id TEXT, run_id TEXT, payload BLOB)`,
		`CREATE TABLE messages (id TEXT PRIMARY KEY, task_id TEXT, attempt_id TEXT, seq INTEGER)`,
		`CREATE TABLE message_recipients (message_id TEXT, agent_id TEXT, acknowledged_at TEXT)`,
		`CREATE TABLE audit (id INTEGER PRIMARY KEY AUTOINCREMENT, operation TEXT)`,
		`INSERT INTO attempts VALUES ('A1', 'T1', 'closed', '[{"kind":"human_merge","ref":"pr"}]'), ('A9', 'T9', 'running', '')`,
		`INSERT INTO attempt_steps VALUES ('k1', 'A1', 'claim'), ('k2', 'A1', 'finalize'), ('k9', 'A9', 'claim')`,
		`INSERT INTO attempt_results VALUES ('A1', 1, 'pr')`,
		`INSERT INTO attempt_pulls VALUES ('A1', 7)`,
		`INSERT INTO attempt_automation_runs VALUES ('A1', 'r1', x'00ff')`,
		`INSERT INTO messages VALUES ('M1', 'T1', '', 1), ('M2', '', 'A1', 2), ('M9', 'T9', 'A9', 3)`,
		`INSERT INTO message_recipients VALUES ('M1', 'W', 't'), ('M2', 'C', 't'), ('M9', 'W', NULL)`,
		`INSERT INTO audit (operation) VALUES ('invitation.issue'), ('attempt.finalize')`,
	} {
		if _, err := db.Exec(s); err != nil {
			t.Fatalf("%s: %v", s, err)
		}
	}
	write(t, dir, "aimem/task.json", `{"id":"T1","state":"DONE","evidence_refs":[{"kind":"human_merge","ref":"pr"}]}`)
	write(t, dir, "aimem/comments.json", `{"comments":[{"body":"Triaged READY"}]}`)
	write(t, dir, "aimem/history.json", `{"changes":[{"task":{"state":"READY"}},{"task":{"state":"DONE"}}]}`)
	write(t, dir, "aimem/reservation.json", `{"state":"released"}`)
	for _, role := range roles {
		write(t, dir, "homes/"+role+"/turns.jsonl", `{"id":"`+role+`-1","outcome":"finished"}`+"\n")
		write(t, dir, "homes/"+role+"/guard.jsonl", "")
		write(t, dir, "homes/"+role+"/HANDOFF.md", "# "+role+"\nFinalized attempt A1\n")
	}
}

func write(t *testing.T, dir, name, text string) {
	t.Helper()
	p := filepath.Join(dir, filepath.FromSlash(name))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
}

func exec1(t *testing.T, dir, stmt string) {
	t.Helper()
	db, err := sql.Open("sqlite", filepath.Join(dir, "store", "aicrew.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(stmt); err != nil {
		t.Fatalf("%s: %v", stmt, err)
	}
}

func pair(t *testing.T) (string, string) {
	before, after := filepath.Join(t.TempDir(), "before"), filepath.Join(t.TempDir(), "after")
	snapshot(t, before)
	snapshot(t, after)
	return before, after
}

func TestSameUnchanged(t *testing.T) {
	before, after := pair(t)
	// Records outside the first cycle may change: another task's attempt
	// and its messages, and the audit after the upgrade.
	exec1(t, after, `UPDATE attempts SET state = 'closed' WHERE id = 'A9'`)
	exec1(t, after, `INSERT INTO attempt_steps VALUES ('k10', 'A9', 'finalize')`)
	exec1(t, after, `UPDATE message_recipients SET acknowledged_at = 't' WHERE message_id = 'M9'`)
	exec1(t, after, `INSERT INTO audit (operation) VALUES ('control.resume')`)
	diffs, _, counts, err := same(before, after, false)
	if err != nil || len(diffs) > 0 {
		t.Fatalf("same: %v %v", diffs, err)
	}
	if counts != "aicrewd 42, aimem 4, homes 9" {
		t.Fatalf("counts %q", counts)
	}
}

// Each of the three places is compared: a change in any one of them alone
// is a difference named by that place. A place left out of the comparison
// fails here.
func TestSameEachPlace(t *testing.T) {
	mutations := map[string]func(t *testing.T, after string){
		"aicrewd": func(t *testing.T, after string) {
			exec1(t, after, `UPDATE attempts SET delivery_evidence = '[]' WHERE id = 'A1'`)
		},
		"aimem": func(t *testing.T, after string) {
			write(t, after, "aimem/task.json", `{"id":"T1","state":"DONE","evidence_refs":[]}`)
		},
		"homes": func(t *testing.T, after string) {
			write(t, after, "homes/worker/turns.jsonl", `{"id":"worker-1","outcome":"failed"}`+"\n")
		},
	}
	if len(places) != len(mutations) {
		t.Fatalf("%d places are compared, the test mutates %d", len(places), len(mutations))
	}
	for _, p := range places {
		mutate, ok := mutations[p.name]
		if !ok {
			t.Fatalf("no mutation of the place %s", p.name)
		}
		t.Run(p.name, func(t *testing.T) {
			before, after := pair(t)
			mutate(t, after)
			diffs, _, _, err := same(before, after, false)
			if err != nil {
				t.Fatal(err)
			}
			if len(diffs) == 0 {
				t.Fatalf("a change in %s was not found", p.name)
			}
			for _, d := range diffs {
				if !strings.HasPrefix(d, p.name+": ") {
					t.Fatalf("a change in %s was reported as %q", p.name, d)
				}
			}
		})
	}
}

func TestSameFindsEachKind(t *testing.T) {
	for name, c := range map[string]struct {
		mutate func(t *testing.T, after string)
		want   string
	}{
		"a step of the first attempt added": {func(t *testing.T, after string) {
			exec1(t, after, `INSERT INTO attempt_steps VALUES ('k3', 'A1', 'update')`)
		}, "aicrewd: attempt_steps "},
		"a message acked again": {func(t *testing.T, after string) {
			exec1(t, after, `UPDATE message_recipients SET acknowledged_at = 'later' WHERE message_id = 'M2'`)
		}, "aicrewd: message_recipients "},
		"an audit row before the upgrade lost": {func(t *testing.T, after string) {
			exec1(t, after, `DELETE FROM audit WHERE id = 1`)
		}, "aicrewd: audit "},
		"a blob changed": {func(t *testing.T, after string) {
			exec1(t, after, `UPDATE attempt_automation_runs SET payload = x'00fe'`)
		}, "aicrewd: attempt_automation_runs "},
		"the reservation held again": {func(t *testing.T, after string) {
			write(t, after, "aimem/reservation.json", `{"state":"held"}`)
		}, "aimem: reservation.json changed"},
		"a handoff rewritten": {func(t *testing.T, after string) {
			write(t, after, "homes/coordinator/HANDOFF.md", "# coordinator\n")
		}, "homes: coordinator/HANDOFF.md changed"},
		"a turn appended": {func(t *testing.T, after string) {
			write(t, after, "homes/indep/turns.jsonl", `{"id":"indep-1","outcome":"finished"}`+"\n"+`{"id":"indep-2"}`+"\n")
		}, "homes: indep/turns.jsonl changed"},
	} {
		t.Run(name, func(t *testing.T) {
			before, after := pair(t)
			c.mutate(t, after)
			diffs, _, _, err := same(before, after, false)
			if err != nil {
				t.Fatal(err)
			}
			if len(diffs) == 0 || !strings.Contains(strings.Join(diffs, "\n"), c.want) {
				t.Fatalf("want %q among %v", c.want, diffs)
			}
		})
	}
}

// After a later cycle the logs may grow and the handoffs change, and
// nothing else may.
func TestSameGrown(t *testing.T) {
	before, after := pair(t)
	write(t, after, "homes/worker/turns.jsonl", `{"id":"worker-1","outcome":"finished"}`+"\n"+`{"id":"worker-2"}`+"\n")
	write(t, after, "homes/worker/guard.jsonl", "a later line\n")
	write(t, after, "homes/worker/HANDOFF.md", "# worker\nthe second attempt\n")
	exec1(t, after, `INSERT INTO audit (operation) VALUES ('attempt.claim')`)
	if diffs, _, _, err := same(before, after, true); err != nil || len(diffs) > 0 {
		t.Fatalf("grown: %v %v", diffs, err)
	}
	write(t, after, "homes/worker/turns.jsonl", `{"id":"worker-2"}`+"\n")
	diffs, _, _, err := same(before, after, true)
	if err != nil || len(diffs) != 1 || !strings.HasPrefix(diffs[0], "homes: worker/turns.jsonl no longer starts with") {
		t.Fatalf("a rewritten log: %v %v", diffs, err)
	}
	exec1(t, after, `UPDATE attempts SET state = 'running' WHERE id = 'A1'`)
	if diffs, _, _, _ := same(before, after, true); len(diffs) < 2 {
		t.Fatalf("a changed attempt after a later cycle: %v", diffs)
	}
	// A record keyed by name is compared exactly after a later cycle too.
	before, after = pair(t)
	write(t, after, "aimem/task.json", `{"id":"T1","state":"READY"}`)
	diffs, _, _, err = same(before, after, true)
	if err != nil || len(diffs) != 1 || !strings.HasPrefix(diffs[0], "aimem: task.json changed") {
		t.Fatalf("a changed task after a later cycle: %v %v", diffs, err)
	}
}

// A store migration that adds columns changes no record: the rows are
// compared on the columns BEFORE had, and each added column is named. A
// column BEFORE had that is gone, dropped or renamed, is a difference.
func TestSameStoreMigration(t *testing.T) {
	before, after := pair(t)
	exec1(t, after, `ALTER TABLE attempts ADD COLUMN x TEXT NOT NULL DEFAULT ''`)
	exec1(t, after, `ALTER TABLE messages ADD COLUMN y INTEGER NOT NULL DEFAULT 0`)
	diffs, notes, _, err := same(before, after, false)
	if err != nil || len(diffs) > 0 {
		t.Fatalf("added columns: %v %v", diffs, err)
	}
	want := []string{"aicrewd: attempts gained the column x, which is not compared",
		"aicrewd: messages gained the column y, which is not compared"}
	if strings.Join(notes, "\n") != strings.Join(want, "\n") {
		t.Fatalf("notes %q, want %q", notes, want)
	}
	for name, stmt := range map[string]string{
		"dropped": `ALTER TABLE attempts DROP COLUMN delivery_evidence`,
		"renamed": `ALTER TABLE attempts RENAME COLUMN delivery_evidence TO evidence`,
	} {
		t.Run(name, func(t *testing.T) {
			before, after := pair(t)
			exec1(t, after, stmt)
			diffs, _, _, err := same(before, after, false)
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(strings.Join(diffs, "\n"), "aicrewd: attempts column delivery_evidence is gone") {
				t.Fatalf("a %s column: %v", name, diffs)
			}
		})
	}
}

func TestSameNeedsTheFirstCycle(t *testing.T) {
	before, after := pair(t)
	exec1(t, before, `DELETE FROM attempts WHERE task_id = 'T1'`)
	if _, _, _, err := same(before, after, false); err == nil || !strings.Contains(err.Error(), "has no attempt") {
		t.Fatalf("no attempt: %v", err)
	}
	before, after = pair(t)
	for _, role := range roles {
		write(t, before, "homes/"+role+"/turns.jsonl", "")
		write(t, before, "homes/"+role+"/HANDOFF.md", "")
	}
	if _, _, _, err := same(before, after, false); err == nil || !strings.Contains(err.Error(), "homes: nothing of the first cycle") {
		t.Fatalf("empty homes: %v", err)
	}
	before, after = pair(t)
	if err := os.Remove(filepath.Join(after, "aimem", "history.json")); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := same(before, after, false); err == nil || !strings.Contains(err.Error(), "aimem, after") {
		t.Fatalf("a read missing after: %v", err)
	}
}

// git runs git in dir with no configuration but the test's own.
func git(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-c", "user.name=t", "-c", "user.email=t@example.test",
		"-c", "commit.gpgSign=false", "-c", "tag.gpgSign=false"}, args...)...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

func TestPrevious(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("no git")
	}
	empty := filepath.Join(t.TempDir(), "gitconfig")
	write(t, filepath.Dir(empty), "gitconfig", "")
	t.Setenv("GIT_CONFIG_GLOBAL", empty)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	repo := t.TempDir()
	git(t, repo, "init", "-q", "-b", "main")
	commit := func(m string) { git(t, repo, "commit", "-q", "--allow-empty", "-m", m) }

	// The forge: a tag's SHA256SUMS answers 200 when it is in published,
	// the status in answers when it is there, and 404 otherwise. mu guards
	// the three, which the server's handler and the test both use.
	var mu sync.Mutex
	published := map[string]bool{"v0.9.0": true, "v0.10.0": true, "v0.11.0": true, "v0.12.0": true}
	answers := map[string]int{}
	var asked []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		tag, ok := strings.CutSuffix(strings.TrimPrefix(r.URL.Path, "/"), "/SHA256SUMS")
		asked = append(asked, tag)
		switch {
		case !ok:
			http.NotFound(w, r)
		case answers[tag] != 0:
			w.WriteHeader(answers[tag])
		case published[tag]:
			_, _ = io.WriteString(w, "sums\n")
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	var log strings.Builder
	prev := func(override string) (string, error) {
		log.Reset()
		mu.Lock()
		asked = nil
		mu.Unlock()
		tag, err := previous(repo, override, publishedAt(srv.URL), &log)
		mu.Lock()
		defer mu.Unlock()
		return tag, err
	}

	commit("one")
	if _, err := prev(""); err == nil || !strings.Contains(err.Error(), "no release tag") {
		t.Fatalf("no tag: %v", err)
	}
	git(t, repo, "tag", "v0.9.0")
	commit("two")
	git(t, repo, "tag", "v0.10.0")
	git(t, repo, "tag", "v0.10.1-rc1")
	// A release on a branch HEAD does not contain.
	git(t, repo, "switch", "-q", "-c", "side")
	commit("side")
	git(t, repo, "tag", "v0.12.0")
	git(t, repo, "switch", "-q", "main")
	commit("three")
	// HEAD is a release's own commit: the release before it.
	git(t, repo, "tag", "v0.11.0")
	for _, c := range []struct{ override, want string }{
		{"", "v0.10.0"},
		{"v0.8.0", "v0.8.0"},
	} {
		got, err := prev(c.override)
		if err != nil || got != c.want {
			t.Fatalf("previous(%q) = %q, %v; want %s", c.override, got, err, c.want)
		}
	}
	commit("four")
	if got, err := prev(""); err != nil || got != "v0.11.0" || log.Len() != 0 || len(asked) != 1 {
		t.Fatalf("after the release: %q, %v, log %q, asked %v", got, err, log.String(), asked)
	}

	// A tag whose release run failed, between two published releases: the
	// older published one, and the tag passed over is named.
	git(t, repo, "tag", "v0.11.1")
	commit("five")
	got, err := prev("")
	if err != nil || got != "v0.11.0" || !slices.Equal(asked, []string{"v0.11.1", "v0.11.0"}) {
		t.Fatalf("over an unpublished tag: %q, %v, asked %v", got, err, asked)
	}
	if want := "passed over v0.11.1: no published release (its SHA256SUMS answered 404)"; !strings.Contains(log.String(), want) {
		t.Fatalf("log %q, want %q", log.String(), want)
	}
	// Any other answer is an error, not a reason to start from an older
	// release.
	mu.Lock()
	answers["v0.11.1"] = http.StatusServiceUnavailable
	mu.Unlock()
	if got, err := prev(""); err == nil || got != "" || !strings.Contains(err.Error(), "v0.11.1") || !strings.Contains(err.Error(), "503") {
		t.Fatalf("a forge that answers 503: %q, %v", got, err)
	}
	mu.Lock()
	delete(answers, "v0.11.1")
	mu.Unlock()
	// No published release before HEAD at all: an error naming each tag.
	mu.Lock()
	clear(published)
	mu.Unlock()
	if _, err := prev(""); err == nil || !strings.Contains(err.Error(), "no published release before HEAD") ||
		!strings.Contains(err.Error(), "v0.11.1, v0.11.0, v0.10.0, v0.9.0") {
		t.Fatalf("nothing published: %v", err)
	}
	// The forge cannot be reached: an error, never an older tag.
	srv.Close()
	if got, err := prev(""); err == nil || got != "" || !strings.Contains(err.Error(), "could not be read") {
		t.Fatalf("an unreachable forge: %q, %v", got, err)
	}

	for _, bad := range []string{"0.11.0", "v0.11", "v0.11.0-rc1", "main"} {
		if _, err := prev(bad); err == nil {
			t.Fatalf("override %q accepted", bad)
		}
	}
}
