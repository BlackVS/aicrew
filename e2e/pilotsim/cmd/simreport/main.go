// Command simreport keeps the pilot simulation's event log and writes its
// report (docs/E2E-PILOT-SIM.md, "The report"; design
// docs/DESIGN-PILOT-SIMULATION.md, section 7). scripts/pilot-sim.sh runs it
// on the host:
//
//	simreport event OUT TYPE [KEY=VALUE...]   appends one event to OUT/events.jsonl
//	simreport write OUT [SECRETS]             writes OUT/report.jsonl
//
// write reads the events and the step snapshots they name, and writes one
// JSON object per line, in b4's record shape (run, step, scenario, fault,
// observation, skip_verdict, summary) plus a store_diff record per step:
// the rows aicrewd's store and aimem's reads of the task gained, lost or
// changed since the step before. A value that holds one of the run's known
// secrets (SECRETS, one per line) is written with the secret replaced by
// its number. Every record is checked against the documented shape before
// the report is written.
package main

import (
	"bufio"
	"bytes"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	_ "modernc.org/sqlite"
)

const (
	eventsFile = "events.jsonl"
	reportFile = "report.jsonl"
	// maxRows bounds each table's gained, lost and changed lists in a store diff.
	maxRows = 100
	// maxValue bounds a text value in a store diff, in bytes.
	maxValue = 300
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "simreport:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) >= 3 && args[0] == "event" {
		return appendEvent(args[1], args[2], args[3:], time.Now())
	}
	if (len(args) == 2 || len(args) == 3) && args[0] == "write" {
		secrets := ""
		if len(args) == 3 {
			secrets = args[2]
		}
		return write(args[1], secrets)
	}
	return errors.New("usage: simreport event OUT TYPE [KEY=VALUE...] | simreport write OUT [SECRETS]")
}

// appendEvent writes one event line: its type, its time and its fields.
func appendEvent(out, typ string, kv []string, now time.Time) error {
	ev := map[string]string{"type": typ, "at": now.UTC().Format(time.RFC3339Nano)}
	for _, a := range kv {
		k, v, ok := strings.Cut(a, "=")
		if !ok || k == "" || k == "type" || k == "at" {
			return fmt.Errorf("event %s: %q is not KEY=VALUE", typ, a)
		}
		ev[k] = v
	}
	b, err := json.Marshal(ev)
	if err != nil {
		return err
	}
	f, err := os.OpenFile(filepath.Join(out, eventsFile), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	if _, err := f.Write(append(b, '\n')); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

type event map[string]string

func readEvents(out string) ([]event, error) {
	f, err := os.Open(filepath.Join(out, eventsFile))
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var evs []event
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64*1024), 1024*1024)
	for n := 1; sc.Scan(); n++ {
		var ev event
		if err := json.Unmarshal(sc.Bytes(), &ev); err != nil {
			return nil, fmt.Errorf("%s line %d: %v", eventsFile, n, err)
		}
		evs = append(evs, ev)
	}
	return evs, sc.Err()
}

type record = map[string]any

// scenario is an open scenario while the events are read.
type scenario struct {
	rec   record
	start time.Time
}

func at(ev event) time.Time {
	t, _ := time.Parse(time.RFC3339Nano, ev["at"])
	return t
}

func ms(d time.Duration) int64 { return d.Milliseconds() }

// build turns the events into the report's records, in the order they happened.
func build(out string, evs []event) ([]record, error) {
	var recs []record
	var open *scenario
	var start, end time.Time
	var runRec record
	code, failure, skip := -1, "", ""
	passed, failed, steps := 0, 0, 0
	verdict := false
	snaps := map[string]record{} // the latest step record of each snapshot
	diffs := map[string]record{} // the first store diff of each snapshot
	var prev *snapshot           // the step before, for aicrewd's store
	prevTask := map[string]*snapshot{}

	closeScenario := func(t time.Time, result, reason string) {
		if open == nil {
			return
		}
		open.rec["result"] = result
		open.rec["duration_ms"] = ms(t.Sub(open.start))
		if reason != "" {
			open.rec["reason"] = reason
		}
		if result == "PASS" {
			passed++
		} else {
			failed++
		}
		open = nil
	}
	for _, ev := range evs {
		t := at(ev)
		switch ev["type"] {
		case "run":
			start = t
			skip = ev["skip"]
			runRec = record{"type": "run", "at": ev["at"], "go": runtime.Version()}
			for k, v := range ev {
				if k != "type" && k != "at" {
					runRec[k] = v
				}
			}
			if n, err := strconv.Atoi(ev["bound_minutes"]); err == nil {
				runRec["bound_minutes"] = n
			}
			recs = append(recs, runRec)
		case "scenario_start":
			closeScenario(t, "PASS", "")
			bound, _ := strconv.Atoi(ev["bound_seconds"])
			open = &scenario{start: t, rec: record{"type": "scenario", "at": ev["at"], "name": ev["name"],
				"bound_ms": int64(bound) * 1000}}
			recs = append(recs, open.rec)
		case "scenario_end":
			closeScenario(t, ev["result"], ev["reason"])
		case "step":
			steps++
			rec := record{"type": "step", "at": ev["at"], "scenario": ev["scenario"], "step": ev["step"],
				"snapshot": ev["snapshot"], "checks": "none"}
			_, again := snaps[ev["snapshot"]]
			snaps[ev["snapshot"]] = rec
			diff := record{"type": "store_diff", "at": ev["at"], "scenario": ev["scenario"], "step": ev["step"],
				"snapshot": ev["snapshot"]}
			// A directory taken again holds the later step only: neither
			// step's diff can be told.
			var cur *snapshot
			err := fmt.Errorf("the snapshot %s is an earlier step's directory, taken again", ev["snapshot"])
			if again {
				first := diffs[ev["snapshot"]]
				first["error"] = fmt.Sprintf("the snapshot %s was taken again by a later step", ev["snapshot"])
				first["aicrewd"], first["aimem"] = record{}, record{}
			} else {
				diffs[ev["snapshot"]] = diff
				cur, err = readSnapshot(out, ev["snapshot"])
			}
			if err != nil {
				// A snapshot the run left unfinished (it failed while taking
				// it) has no diff; the next step compares with the one before.
				diff["error"] = err.Error()
				diff["against"], diff["aicrewd"], diff["aimem_against"], diff["aimem"] = "", record{}, "", record{}
				recs = append(recs, rec, diff)
				continue
			}
			diff["against"], diff["aicrewd"] = "", shorten(tablesDiff(nil, cur.tables))
			if prev != nil {
				diff["against"], diff["aicrewd"] = prev.rel, shorten(tablesDiff(prev.tables, cur.tables))
			}
			before := prevTask[cur.task]
			diff["aimem_against"] = ""
			if before != nil {
				diff["aimem_against"] = before.rel
			}
			diff["aimem"] = aimemDiff(before, cur)
			recs = append(recs, rec, diff)
			prev = cur
			if cur.task != "" {
				prevTask[cur.task] = cur
			}
		case "check":
			if rec := snaps[ev["snapshot"]]; rec != nil {
				rec["checks"] = ev["result"]
			}
		case "fault", "observation":
			rec := record{"type": ev["type"]}
			for k, v := range ev {
				rec[k] = v
			}
			if ev["type"] == "observation" {
				rec["scenario"] = ""
				if open != nil {
					rec["scenario"] = open.rec["name"]
				}
			}
			recs = append(recs, rec)
		case "skip_verdict":
			verdict = true
			recs = append(recs, record{"type": "skip_verdict", "at": ev["at"], "case": ev["case"],
				"verdict": ev["verdict"], "point": ev["point"], "reasons": reasons(ev["reason"])})
		case "fail":
			if failure == "" {
				failure = ev["what"]
			}
		case "end":
			end = t
			code, _ = strconv.Atoi(ev["code"])
		}
	}
	if end.IsZero() && len(evs) > 0 {
		end = at(evs[len(evs)-1])
	}
	switch {
	case code == 0:
		closeScenario(end, "PASS", "")
	case failure != "":
		closeScenario(end, "FAIL", failure)
	case code > 0:
		closeScenario(end, "FAIL", fmt.Sprintf("the run exited %d", code))
	default:
		closeScenario(end, "FAIL", "the run ended without its end event")
	}
	if skip != "" && !verdict {
		why := failure
		if why == "" {
			why = "the run ended before the case's point"
		}
		recs = append(recs, record{"type": "skip_verdict", "at": end.UTC().Format(time.RFC3339Nano), "case": skip,
			"verdict": "not_as_expected", "point": "", "reasons": []string{why}})
	}
	sum := record{"type": "summary", "at": end.UTC().Format(time.RFC3339Nano), "scenarios_passed": passed,
		"scenarios_failed": failed, "steps": steps, "exit_code": code, "green": code == 0,
		"duration_ms": ms(end.Sub(start))}
	if runRec != nil {
		sum["bound_minutes"] = runRec["bound_minutes"]
	}
	if failure != "" {
		sum["failure"] = failure
	}
	return append(recs, sum), nil
}

func reasons(s string) []string {
	if s == "" {
		return []string{}
	}
	return []string{s}
}

// snapshot is one step's read-only copy, as scripts/pilot-sim.sh takes it.
type snapshot struct {
	rel    string
	task   string
	tables map[string]map[string]record
	aimem  map[string]any
}

func readSnapshot(out, rel string) (*snapshot, error) {
	dir := filepath.Join(out, rel)
	s := &snapshot{rel: rel, aimem: map[string]any{}}
	var ids struct {
		Task string `json:"task"`
	}
	if b, err := os.ReadFile(filepath.Join(dir, "ids.json")); err == nil {
		if err := json.Unmarshal(b, &ids); err != nil {
			return nil, fmt.Errorf("ids.json: %v", err)
		}
	}
	s.task = ids.Task
	for _, name := range []string{"task", "comments", "history", "reservation"} {
		b, err := os.ReadFile(filepath.Join(dir, "aimem", name+".json"))
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, err
		}
		var v any
		if err := json.Unmarshal(b, &v); err != nil {
			return nil, fmt.Errorf("aimem/%s.json: %v", name, err)
		}
		s.aimem[name] = v
	}
	tables, err := readStore(filepath.Join(dir, "store"))
	if err != nil {
		return nil, fmt.Errorf("aicrewd's store: %v", err)
	}
	s.tables = tables
	return s, nil
}

// readStore reads every table of the store copy in dir, each row keyed by
// its primary key (the rowid where the table has none). The copy is read
// from a private copy of its own, so the snapshot stays as it was taken.
func readStore(dir string) (map[string]map[string]record, error) {
	if _, err := os.Stat(filepath.Join(dir, "aicrew.db")); errors.Is(err, os.ErrNotExist) {
		return map[string]map[string]record{}, nil
	}
	tmp, err := os.MkdirTemp("", "simreport")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(tmp)
	for _, name := range []string{"aicrew.db", "aicrew.db-wal"} {
		if err := copyFile(filepath.Join(dir, name), filepath.Join(tmp, name)); err != nil && !errors.Is(err, os.ErrNotExist) {
			return nil, err
		}
	}
	db, err := sql.Open("sqlite", filepath.Join(tmp, "aicrew.db"))
	if err != nil {
		return nil, err
	}
	defer db.Close()
	names, err := strs(db, `SELECT name FROM sqlite_master WHERE type = 'table' AND name NOT LIKE 'sqlite_%' ORDER BY name`)
	if err != nil {
		return nil, err
	}
	tables := map[string]map[string]record{}
	for _, t := range names {
		rows, err := readTable(db, t)
		if err != nil {
			return nil, fmt.Errorf("%s: %v", t, err)
		}
		tables[t] = rows
	}
	return tables, nil
}

func copyFile(from, to string) error {
	in, err := os.Open(from)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.Create(to)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

func strs(db *sql.DB, q string, args ...any) ([]string, error) {
	rows, err := db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

func quote(name string) string { return `"` + strings.ReplaceAll(name, `"`, `""`) + `"` }

func readTable(db *sql.DB, table string) (map[string]record, error) {
	info, err := db.Query(`SELECT name, pk FROM pragma_table_info(?)`, table)
	if err != nil {
		return nil, err
	}
	type col struct {
		name string
		pk   int
	}
	var pks []col
	for info.Next() {
		var c col
		if err := info.Scan(&c.name, &c.pk); err != nil {
			info.Close()
			return nil, err
		}
		if c.pk > 0 {
			pks = append(pks, c)
		}
	}
	info.Close()
	sort.Slice(pks, func(i, j int) bool { return pks[i].pk < pks[j].pk })
	q := "SELECT * FROM " + quote(table)
	byRowid := len(pks) == 0
	if byRowid {
		q = "SELECT rowid AS __rowid, * FROM " + quote(table)
	}
	rows, err := db.Query(q)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	cols, err := rows.Columns()
	if err != nil {
		return nil, err
	}
	out := map[string]record{}
	for rows.Next() {
		vals := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			return nil, err
		}
		row := record{}
		for i, c := range cols {
			row[c] = column(vals[i])
		}
		var key []any
		if byRowid {
			key = []any{row["__rowid"]}
			delete(row, "__rowid")
		} else {
			for _, c := range pks {
				key = append(key, row[c.name])
			}
		}
		k, _ := json.Marshal(key)
		out[string(k)] = row
	}
	return out, rows.Err()
}

// column is a column's value, whole, for the comparison: a blob that is
// UTF-8 as text, any other blob as its bytes.
func column(v any) any {
	if x, ok := v.([]byte); ok && utf8.Valid(x) {
		return string(x)
	}
	return v
}

// shorten is a diff as the report writes it, once compared: text cut at
// maxValue bytes, a blob that is not UTF-8 by its length only.
func shorten(r record) record {
	return shortenValue(r).(record)
}

func shortenValue(v any) any {
	switch x := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(x))
		for k, e := range x {
			out[k] = shortenValue(e)
		}
		return out
	case []any:
		out := make([]any, len(x))
		for i, e := range x {
			out[i] = shortenValue(e)
		}
		return out
	case []byte:
		return fmt.Sprintf("<%d bytes>", len(x))
	case string:
		return cut(x)
	}
	return v
}

func cut(s string) string {
	if len(s) <= maxValue {
		return s
	}
	i := maxValue
	for i > 0 && !utf8.RuneStart(s[i]) {
		i--
	}
	return fmt.Sprintf("%s…(%d bytes)", s[:i], len(s))
}

// tablesDiff is each table's gained, lost and changed rows, for the tables
// that changed.
func tablesDiff(before, after map[string]map[string]record) record {
	out := record{}
	names := map[string]bool{}
	for t := range before {
		names[t] = true
	}
	for t := range after {
		names[t] = true
	}
	for t := range names {
		if d := rowsDiff(before[t], after[t]); d != nil {
			out[t] = d
		}
	}
	return out
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func rowsDiff(before, after map[string]record) record {
	var gained, lost, changed []any
	for _, k := range sortedKeys(after) {
		old, ok := before[k]
		if !ok {
			gained = append(gained, after[k])
			continue
		}
		if c := fieldsDiff(old, after[k]); len(c) > 0 {
			changed = append(changed, record{"key": json.RawMessage(k), "fields": c})
		}
	}
	for _, k := range sortedKeys(before) {
		if _, ok := after[k]; !ok {
			lost = append(lost, before[k])
		}
	}
	if len(gained)+len(lost)+len(changed) == 0 {
		return nil
	}
	d := record{}
	for name, list := range map[string][]any{"gained": gained, "lost": lost, "changed": changed} {
		if len(list) == 0 {
			continue
		}
		if len(list) > maxRows {
			d[name+"_more"] = len(list) - maxRows
			list = list[:maxRows]
		}
		d[name] = list
	}
	return d
}

// fieldsDiff is each field whose value differs, with its value before and after.
func fieldsDiff(before, after map[string]any) record {
	out := record{}
	for k, v := range after {
		if old, ok := before[k]; !ok || !same(old, v) {
			out[k] = record{"before": before[k], "after": v}
		}
	}
	for k, v := range before {
		if _, ok := after[k]; !ok {
			out[k] = record{"before": v, "after": nil}
		}
	}
	return out
}

func same(a, b any) bool {
	x, _ := json.Marshal(a)
	y, _ := json.Marshal(b)
	return bytes.Equal(x, y)
}

// aimemDiff compares aimem's reads of the task with those of the last step
// that read the same task: the task's and the reservation's fields, and the
// comments and history entries gained, lost or changed.
func aimemDiff(before, after *snapshot) record {
	out := record{}
	if after.task == "" {
		return out
	}
	var b map[string]any
	if before != nil {
		b = before.aimem
	}
	for _, name := range []string{"task", "reservation"} {
		x, _ := b[name].(map[string]any)
		y, _ := after.aimem[name].(map[string]any)
		delete(x, "links")
		delete(y, "links")
		if d := fieldsDiff(x, y); len(d) > 0 {
			out[name] = d
		}
	}
	for name, list := range map[string]string{"comments": "comments", "history": "changes"} {
		key := func(e map[string]any) any { return e["id"] }
		if name == "history" {
			key = func(e map[string]any) any {
				t, _ := e["task"].(map[string]any)
				return t["revision"]
			}
		}
		if d := rowsDiff(entries(b[name], list, key), entries(after.aimem[name], list, key)); d != nil {
			out[name] = d
		}
	}
	return out
}

func entries(doc any, list string, key func(map[string]any) any) map[string]record {
	out := map[string]record{}
	m, _ := doc.(map[string]any)
	items, _ := m[list].([]any)
	for i, it := range items {
		e, ok := it.(map[string]any)
		if !ok {
			continue
		}
		k := key(e)
		if k == nil {
			k = i
		}
		kb, _ := json.Marshal([]any{k})
		out[string(kb)] = e
	}
	return out
}

// The documented shape: each type's required fields, and the values a
// field may take where they are fixed.
var shape = map[string][]string{
	"run":          {"aicrew_commit", "aimem_version", "os", "arch", "go", "bound_minutes", "skip"},
	"scenario":     {"name", "result", "duration_ms", "bound_ms"},
	"step":         {"scenario", "step", "snapshot", "checks"},
	"store_diff":   {"scenario", "step", "snapshot", "against", "aicrewd", "aimem_against", "aimem"},
	"fault":        {"case", "fault", "state"},
	"observation":  {"scenario", "task", "what"},
	"skip_verdict": {"case", "verdict", "point", "reasons"},
	"summary":      {"scenarios_passed", "scenarios_failed", "steps", "exit_code", "green", "duration_ms", "bound_minutes"},
}

var allowed = map[string]map[string][]string{
	"scenario":     {"result": {"PASS", "FAIL"}},
	"step":         {"checks": {"passed", "failed", "none"}},
	"fault":        {"state": {"armed", "left_out"}},
	"skip_verdict": {"verdict": {"failed_as_expected", "not_as_expected"}},
}

// validate checks one record against the documented shape.
func validate(r record) error {
	typ, _ := r["type"].(string)
	fields, ok := shape[typ]
	if !ok {
		return fmt.Errorf("unknown record type %q", typ)
	}
	if _, err := time.Parse(time.RFC3339Nano, fmt.Sprint(r["at"])); err != nil {
		return fmt.Errorf("%s: no time: %v", typ, err)
	}
	for _, f := range fields {
		if _, ok := r[f]; !ok {
			return fmt.Errorf("%s: no %s", typ, f)
		}
	}
	for f, values := range allowed[typ] {
		v := fmt.Sprint(r[f])
		found := false
		for _, a := range values {
			found = found || v == a
		}
		if !found {
			return fmt.Errorf("%s: %s is %q, not one of %v", typ, f, v, values)
		}
	}
	return nil
}

func readSecrets(path string) ([]string, error) {
	if path == "" {
		return nil, nil
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	var out []string
	for l := range strings.SplitSeq(string(b), "\n") {
		if l = strings.TrimSpace(l); len(l) >= 8 && !seen[l] {
			seen[l] = true
			out = append(out, l)
		}
	}
	sort.Strings(out)
	return out, nil
}

// encode is a record's line with each known secret replaced by its number.
func encode(r record, secrets []string) ([]byte, int, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(r); err != nil {
		return nil, 0, err
	}
	line, n := buf.Bytes(), 0
	for i, s := range secrets {
		q, _ := json.Marshal(s)
		q = q[1 : len(q)-1]
		if c := bytes.Count(line, q); c > 0 {
			n += c
			line = bytes.ReplaceAll(line, q, fmt.Appendf(nil, "[secret #%d]", i+1))
		}
	}
	return line, n, nil
}

func write(out, secretsFile string) error {
	evs, err := readEvents(out)
	if err != nil {
		return err
	}
	recs, err := build(out, evs)
	if err != nil {
		return err
	}
	secrets, err := readSecrets(secretsFile)
	if err != nil {
		return err
	}
	var buf bytes.Buffer
	redacted := 0
	for _, r := range recs {
		if err := validate(r); err != nil {
			return err
		}
		if r["type"] == "summary" {
			r["secrets_redacted"] = redacted
		}
		line, n, err := encode(r, secrets)
		if err != nil {
			return err
		}
		redacted += n
		buf.Write(line)
	}
	return os.WriteFile(filepath.Join(out, reportFile), buf.Bytes(), 0o644)
}
