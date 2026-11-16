// Command simupgrade serves the pilot simulation's upgrade scenario
// (docs/E2E-PILOT-SIM.md, "The upgrade scenario"; design
// docs/DESIGN-PILOT-SIMULATION.md, sections 3 and 9). scripts/pilot-sim.sh
// runs it on the host:
//
//	simupgrade previous REPO            prints the release the scenario upgrades from
//	simupgrade same BEFORE AFTER [-grown]  compares the first cycle's records
//
// previous is PILOTSIM_PREVIOUS when that is set (a manual run's override),
// and otherwise the newest vX.Y.Z tag merged into REPO's HEAD that does not
// sit on HEAD itself: on a release's own commit that is the release before
// it. No such tag is an error, never an empty answer.
//
// same reads the first cycle's records from two step snapshots that
// pilot-sim.sh's take wrote (BEFORE taken before the upgrade, AFTER after
// it), in each of the three places the upgrade must leave unchanged:
// aicrewd's store, aimem's reads of the task, and the member homes. It
// prints each difference and exits 1 when there is one. With -grown the
// AFTER snapshot is taken after a later cycle: the homes' turn and guard
// logs need only start with what BEFORE read, and the handoffs, which each
// attempt rewrites, are not compared.
package main

import (
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	_ "modernc.org/sqlite"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "simupgrade:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	switch {
	case len(args) == 2 && args[0] == "previous":
		tag, err := previous(args[1], os.Getenv("PILOTSIM_PREVIOUS"))
		if err != nil {
			return err
		}
		fmt.Println(tag)
		return nil
	case (len(args) == 3 || len(args) == 4 && args[3] == "-grown") && args[0] == "same":
		diffs, counts, err := same(args[1], args[2], len(args) == 4)
		if err != nil {
			return err
		}
		for _, d := range diffs {
			fmt.Println(d)
		}
		if len(diffs) > 0 {
			return fmt.Errorf("%d differences in the first cycle's records between %s and %s", len(diffs), args[1], args[2])
		}
		fmt.Printf("the first cycle's records read the same: %s\n", counts)
		return nil
	}
	return errors.New("usage: simupgrade previous REPO | simupgrade same BEFORE AFTER [-grown]")
}

var releaseTag = regexp.MustCompile(`^v[0-9]+\.[0-9]+\.[0-9]+$`)

// previous is the release the scenario upgrades from: the override when
// set, else the newest release tag merged into HEAD and not on HEAD.
func previous(repo, override string) (string, error) {
	if override != "" {
		if !releaseTag.MatchString(override) {
			return "", fmt.Errorf("PILOTSIM_PREVIOUS=%q is not a release tag vX.Y.Z", override)
		}
		return override, nil
	}
	out, err := exec.Command("git", "-C", repo, "tag", "--list", "v*", "--merged", "HEAD",
		"--no-contains", "HEAD", "--sort=-v:refname").Output()
	if err != nil {
		return "", fmt.Errorf("git tag in %s: %v", repo, err)
	}
	for t := range strings.FieldsSeq(string(out)) {
		if releaseTag.MatchString(t) {
			return t, nil
		}
	}
	return "", fmt.Errorf("%s has no release tag vX.Y.Z merged into HEAD before it; fetch the tags (a full clone) or set PILOTSIM_PREVIOUS", repo)
}

// item is one record a place read, and how a later read must match it.
type item struct {
	text string
	// grown says how the record compares when AFTER follows a later
	// cycle: "exact", "prefix" (an append-only log) or "skip".
	grown string
}

// scope is what the first cycle's records are, read from BEFORE: its
// task, its attempts, and the audit up to the upgrade.
type scope struct {
	task     string
	attempts []string
	auditMax int64
}

// places are the three the upgrade must leave unchanged. Each read returns
// its records by key.
var places = []struct {
	name string
	read func(dir string, s scope) (map[string]item, error)
}{
	{"aicrewd", readCrew},
	{"aimem", readAimem},
	{"homes", readHomes},
}

func same(before, after string, grown bool) ([]string, string, error) {
	s, err := scopeOf(before)
	if err != nil {
		return nil, "", err
	}
	var diffs, counts []string
	for _, p := range places {
		b, err := p.read(before, s)
		if err != nil {
			return nil, "", fmt.Errorf("%s, before: %v", p.name, err)
		}
		if !anyText(b) {
			return nil, "", fmt.Errorf("%s: nothing of the first cycle to compare in %s", p.name, before)
		}
		a, err := p.read(after, s)
		if err != nil {
			return nil, "", fmt.Errorf("%s, after: %v", p.name, err)
		}
		diffs = append(diffs, compare(p.name, b, a, grown)...)
		counts = append(counts, fmt.Sprintf("%s %d", p.name, len(b)))
	}
	return diffs, strings.Join(counts, ", "), nil
}

func compare(place string, before, after map[string]item, grown bool) []string {
	var out []string
	for _, k := range sortedKeys(before) {
		b := before[k]
		mode := "exact"
		if grown {
			mode = b.grown
		}
		a, ok := after[k]
		switch {
		case mode == "skip":
		case !ok:
			out = append(out, fmt.Sprintf("%s: %s is gone: %s", place, k, short(b.text)))
		case mode == "prefix" && !strings.HasPrefix(a.text, b.text):
			out = append(out, fmt.Sprintf("%s: %s no longer starts with what it held: %s", place, k, short(b.text)))
		case mode == "exact" && a.text != b.text:
			out = append(out, fmt.Sprintf("%s: %s changed: %s, now %s", place, k, short(b.text), short(a.text)))
		}
	}
	for _, k := range sortedKeys(after) {
		if _, ok := before[k]; !ok {
			out = append(out, fmt.Sprintf("%s: %s is new: %s", place, k, short(after[k].text)))
		}
	}
	return out
}

func anyText(m map[string]item) bool {
	for _, it := range m {
		if strings.TrimSpace(it.text) != "" {
			return true
		}
	}
	return false
}

func sortedKeys(m map[string]item) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func short(s string) string {
	if len(s) > 300 {
		return s[:300] + "..."
	}
	return s
}

func openStore(dir string) (*sql.DB, error) {
	path := filepath.Join(dir, "store", "aicrew.db")
	if _, err := os.Stat(path); err != nil {
		return nil, err
	}
	return sql.Open("sqlite", "file:"+filepath.ToSlash(path)+"?mode=ro")
}

func scopeOf(before string) (scope, error) {
	var ids struct {
		Task string `json:"task"`
	}
	raw, err := os.ReadFile(filepath.Join(before, "ids.json"))
	if err != nil {
		return scope{}, err
	}
	if err := json.Unmarshal(raw, &ids); err != nil || ids.Task == "" {
		return scope{}, fmt.Errorf("%s/ids.json names no task", before)
	}
	db, err := openStore(before)
	if err != nil {
		return scope{}, err
	}
	defer db.Close()
	s := scope{task: ids.Task}
	rows, err := db.Query("SELECT id FROM attempts WHERE task_id = ? ORDER BY id", ids.Task)
	if err != nil {
		return scope{}, err
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return scope{}, err
		}
		s.attempts = append(s.attempts, id)
	}
	if err := rows.Err(); err != nil {
		return scope{}, err
	}
	if len(s.attempts) == 0 {
		return scope{}, fmt.Errorf("aicrewd: the task %s has no attempt in %s", ids.Task, before)
	}
	if err := db.QueryRow("SELECT COALESCE(MAX(id), 0) FROM audit").Scan(&s.auditMax); err != nil {
		return scope{}, err
	}
	return s, nil
}

// crewTables are the first cycle's records in aicrewd's store: the task's
// attempts with everything kept per attempt (its steps, results, pull
// request, handoff, block and automation runs; the terminal and delivery
// evidence are the attempt's own columns), the messages about them with
// their recipients, and the whole audit up to the upgrade, which is
// append-only.
var crewTables = []struct{ table, where string }{
	{"attempts", "task_id = :task"},
	{"attempt_steps", "attempt_id IN (:attempts)"},
	{"attempt_results", "attempt_id IN (:attempts)"},
	{"attempt_pulls", "attempt_id IN (:attempts)"},
	{"attempt_pull_facts", "attempt_id IN (:attempts)"},
	{"attempt_handoffs", "attempt_id IN (:attempts)"},
	{"attempt_blocks", "attempt_id IN (:attempts)"},
	{"attempt_automation_runs", "attempt_id IN (:attempts)"},
	{"messages", "task_id = :task OR attempt_id IN (:attempts)"},
	{"message_recipients", "message_id IN (SELECT id FROM messages WHERE task_id = :task OR attempt_id IN (:attempts))"},
	{"audit", "id <= :audit"},
}

func readCrew(dir string, s scope) (map[string]item, error) {
	db, err := openStore(dir)
	if err != nil {
		return nil, err
	}
	defer db.Close()
	quoted := make([]string, len(s.attempts))
	for i, a := range s.attempts {
		quoted[i] = "'" + strings.ReplaceAll(a, "'", "''") + "'"
	}
	out := map[string]item{}
	for _, t := range crewTables {
		where := strings.NewReplacer(":attempts", strings.Join(quoted, ","), ":task", "?", ":audit", "?").Replace(t.where)
		var args []any
		for _, m := range regexp.MustCompile(`:task|:audit`).FindAllString(t.where, -1) {
			if m == ":task" {
				args = append(args, s.task)
			} else {
				args = append(args, s.auditMax)
			}
		}
		rows, err := db.Query("SELECT * FROM "+t.table+" WHERE "+where, args...)
		if err != nil {
			return nil, fmt.Errorf("%s: %v", t.table, err)
		}
		recs, err := rowTexts(rows)
		if err != nil {
			return nil, fmt.Errorf("%s: %v", t.table, err)
		}
		// A row is its own key: compared as a set, a changed row reads as
		// one gone and one new.
		for _, r := range recs {
			out[t.table+" "+r] = item{text: r, grown: "exact"}
		}
	}
	return out, nil
}

// rowTexts is each row as canonical JSON, keyed by column name.
func rowTexts(rows *sql.Rows) ([]string, error) {
	defer rows.Close()
	cols, err := rows.Columns()
	if err != nil {
		return nil, err
	}
	var out []string
	for rows.Next() {
		vals := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			return nil, err
		}
		m := map[string]any{}
		for i, c := range cols {
			if b, ok := vals[i].([]byte); ok {
				vals[i] = "x'" + hex.EncodeToString(b) + "'"
			}
			m[c] = vals[i]
		}
		b, err := json.Marshal(m) // map keys marshal sorted
		if err != nil {
			return nil, err
		}
		out = append(out, string(b))
	}
	return out, rows.Err()
}

// aimemReads are the hub's reads of the task the snapshot keeps.
var aimemReads = []string{"task.json", "comments.json", "history.json", "reservation.json"}

func readAimem(dir string, _ scope) (map[string]item, error) {
	out := map[string]item{}
	for _, f := range aimemReads {
		raw, err := os.ReadFile(filepath.Join(dir, "aimem", f))
		if err != nil {
			return nil, err
		}
		var v any
		if err := json.Unmarshal(raw, &v); err != nil {
			return nil, fmt.Errorf("%s: %v", f, err)
		}
		b, err := json.Marshal(v)
		if err != nil {
			return nil, err
		}
		out[f] = item{text: string(b), grown: "exact"}
	}
	return out, nil
}

// homeFiles are what the snapshot keeps of each home, with how each
// compares after a later cycle.
var homeFiles = []struct{ name, grown string }{
	{"turns.jsonl", "prefix"},
	{"guard.jsonl", "prefix"},
	{"HANDOFF.md", "skip"},
}

var roles = []string{"coordinator", "worker", "indep"}

func readHomes(dir string, _ scope) (map[string]item, error) {
	out := map[string]item{}
	for _, role := range roles {
		for _, f := range homeFiles {
			raw, err := os.ReadFile(filepath.Join(dir, "homes", role, f.name))
			if err != nil {
				return nil, err
			}
			out[role+"/"+f.name] = item{text: string(raw), grown: f.grown}
		}
	}
	return out, nil
}
