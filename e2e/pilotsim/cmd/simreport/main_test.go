package main

import (
	"bufio"
	"database/sql"
	"encoding/json"
	"maps"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// fixture copies testdata/run, a red run's events and two step snapshots
// recorded on the lab host (cycle 1's offer and accept), into a temporary
// directory, and makes each snapshot's store from its recorded SQL.
func fixture(t *testing.T) string {
	t.Helper()
	out := t.TempDir()
	src := filepath.Join("testdata", "run")
	err := filepath.Walk(src, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(src, path)
		to := filepath.Join(out, rel)
		if info.IsDir() {
			return os.MkdirAll(to, 0o755)
		}
		if info.Name() != "store.sql" {
			return copyFile(path, to)
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		store := filepath.Join(filepath.Dir(to), "store")
		if err := os.MkdirAll(store, 0o755); err != nil {
			return err
		}
		db, err := sql.Open("sqlite", filepath.Join(store, "aicrew.db"))
		if err != nil {
			return err
		}
		defer db.Close()
		_, err = db.Exec(string(b))
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func readReport(t *testing.T, out string) []record {
	t.Helper()
	f, err := os.Open(filepath.Join(out, reportFile))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var recs []record
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64*1024), 4*1024*1024)
	for sc.Scan() {
		var r record
		if err := json.Unmarshal(sc.Bytes(), &r); err != nil {
			t.Fatalf("a report line is not JSON: %v", err)
		}
		if err := validate(r); err != nil {
			t.Errorf("a record does not have the documented shape: %v", err)
		}
		recs = append(recs, r)
	}
	if err := sc.Err(); err != nil {
		t.Fatal(err)
	}
	return recs
}

// path reads a nested field: path(r, "aicrewd", "attempts", "changed").
func path(v any, keys ...string) any {
	for _, k := range keys {
		m, ok := v.(map[string]any)
		if !ok {
			return nil
		}
		v = m[k]
	}
	return v
}

func TestWriteRecordedRun(t *testing.T) {
	out := fixture(t)
	if err := write(out, filepath.Join("testdata", "secrets")); err != nil {
		t.Fatal(err)
	}
	recs := readReport(t, out)
	var types []string
	for _, r := range recs {
		types = append(types, r["type"].(string))
	}
	want := "run scenario scenario step store_diff fault step store_diff observation summary"
	if got := strings.Join(types, " "); got != want {
		t.Fatalf("records:\n got %s\nwant %s", got, want)
	}

	setup, cycle := recs[1], recs[2]
	if setup["name"] != "setup" || setup["result"] != "PASS" || setup["duration_ms"] != float64(76500) {
		t.Errorf("setup: %v", setup)
	}
	if cycle["name"] != "cycle-1" || cycle["result"] != "FAIL" || cycle["bound_ms"] != float64(900000) ||
		!strings.HasPrefix(cycle["reason"].(string), "cycle 1: the checks after the accept step") {
		t.Errorf("cycle-1, the failing scenario: %v", cycle)
	}
	if recs[3]["checks"] != "passed" || recs[6]["checks"] != "failed" || recs[6]["step"] != "accept" {
		t.Errorf("the steps' checks: %v, %v", recs[3], recs[6])
	}

	// The first step's diff is against an empty store: the attempt is new.
	offer := recs[4]
	if offer["against"] != "" || path(offer, "aicrewd", "attempts", "gained") == nil {
		t.Errorf("offer's store diff: %v", offer["aicrewd"])
	}
	if path(offer, "aimem", "task", "state", "after") != "READY" {
		t.Errorf("offer's aimem diff: %v", offer["aimem"])
	}
	// The accept: the attempt runs, the worker's transfer is a new step, and
	// aimem's reservation moved to the worker with the next fence.
	accept := recs[7]
	if accept["against"] != "cycle-1/offer" || accept["aimem_against"] != "cycle-1/offer" {
		t.Errorf("accept compares with %v and %v", accept["against"], accept["aimem_against"])
	}
	changed, _ := path(accept, "aicrewd", "attempts", "changed").([]any)
	if len(changed) != 1 || path(changed[0], "fields", "state", "before") != "offered" ||
		path(changed[0], "fields", "state", "after") != "running" {
		t.Errorf("accept's attempts: %v", path(accept, "aicrewd", "attempts"))
	}
	gained, _ := path(accept, "aicrewd", "attempt_steps", "gained").([]any)
	if len(gained) != 1 || path(gained[0], "operation") != "transfer" {
		t.Errorf("accept's attempt_steps: %v", path(accept, "aicrewd", "attempt_steps"))
	}
	if path(accept, "aimem", "reservation", "fence", "before") != "1" ||
		path(accept, "aimem", "reservation", "fence", "after") != "2" {
		t.Errorf("accept's reservation: %v", path(accept, "aimem", "reservation"))
	}

	if obs := recs[8]; obs["scenario"] != "cycle-1" || obs["task"] == "" {
		t.Errorf("the observation: %v", obs)
	}
	sum := recs[9]
	if sum["green"] != false || sum["exit_code"] != float64(1) || sum["scenarios_passed"] != float64(1) ||
		sum["scenarios_failed"] != float64(1) || sum["steps"] != float64(2) || sum["bound_minutes"] != float64(90) ||
		sum["duration_ms"] != float64(136880) {
		t.Errorf("the summary: %v", sum)
	}

	// The known secret (the worker's session in the reservation, here) is
	// written by its number only; a line shorter than 8 is no secret.
	b, _ := os.ReadFile(filepath.Join(out, reportFile))
	if strings.Contains(string(b), "01a1a14b-11ea-7f1e-90a5-ef4b75970b92") || !strings.Contains(string(b), "[secret #1]") {
		t.Error("the known secret is in the report, or not replaced by its number")
	}
	if n, _ := sum["secrets_redacted"].(float64); n < 1 {
		t.Errorf("secrets_redacted is %v", sum["secrets_redacted"])
	}
}

// A run that left a case's fault out and ended before the case's point has
// no skip verdict of its own: the report writes not_as_expected.
func TestSkipRunWithoutVerdict(t *testing.T) {
	out := t.TempDir()
	now := time.Date(2026, 11, 3, 12, 0, 0, 0, time.UTC)
	for i, ev := range [][]string{
		{"run", "aicrew_commit=x", "aimem_version=0.10.21", "os=linux", "arch=amd64", "bound_minutes=90", "skip=F2-begin"},
		{"scenario_start", "name=case-F2", "bound_seconds=1200"},
		{"fail", "what=AICREW_SIM_SKIP_FAULT=F2-begin: the case's checks at begin passed without its fault"},
		{"end", "code=1"},
	} {
		if err := appendEvent(out, ev[0], ev[1:], now.Add(time.Duration(i)*time.Second)); err != nil {
			t.Fatal(err)
		}
	}
	if err := write(out, ""); err != nil {
		t.Fatal(err)
	}
	recs := readReport(t, out)
	v := recs[len(recs)-2]
	if v["type"] != "skip_verdict" || v["case"] != "F2-begin" || v["verdict"] != "not_as_expected" {
		t.Fatalf("the verdict: %v", v)
	}
	if recs[1]["result"] != "FAIL" {
		t.Errorf("the case's scenario: %v", recs[1])
	}
}

// A run that ended without its end event (killed from outside) fails its
// open scenario and is not green.
func TestRunWithoutEnd(t *testing.T) {
	out := t.TempDir()
	now := time.Date(2026, 11, 3, 12, 0, 0, 0, time.UTC)
	for _, ev := range [][]string{
		{"run", "aicrew_commit=x", "aimem_version=0.10.21", "os=linux", "arch=amd64", "bound_minutes=90", "skip="},
		{"scenario_start", "name=setup", "bound_seconds=1800"},
	} {
		if err := appendEvent(out, ev[0], ev[1:], now); err != nil {
			t.Fatal(err)
		}
	}
	if err := write(out, ""); err != nil {
		t.Fatal(err)
	}
	recs := readReport(t, out)
	if recs[1]["result"] != "FAIL" || recs[1]["reason"] != "the run ended without its end event" {
		t.Errorf("the open scenario: %v", recs[1])
	}
	if recs[2]["green"] != false {
		t.Errorf("the summary: %v", recs[2])
	}
}

// A snapshot the run left unfinished (here a store copy that is no
// database) gives its diff an error, and the report is still written.
func TestUnfinishedSnapshot(t *testing.T) {
	out := t.TempDir()
	store := filepath.Join(out, "cycle-1", "offer", "store")
	if err := os.MkdirAll(store, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(store, "aicrew.db"), []byte("half a copy"), 0o644); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 11, 3, 12, 0, 0, 0, time.UTC)
	for _, ev := range [][]string{
		{"run", "aicrew_commit=x", "aimem_version=0.10.21", "os=linux", "arch=amd64", "bound_minutes=90", "skip="},
		{"scenario_start", "name=cycle-1", "bound_seconds=900"},
		{"step", "scenario=cycle-1", "step=offer", "snapshot=cycle-1/offer"},
		{"fail", "what=no stable copy of aicrewd's store for the offer step"},
		{"end", "code=1"},
	} {
		if err := appendEvent(out, ev[0], ev[1:], now); err != nil {
			t.Fatal(err)
		}
	}
	if err := write(out, ""); err != nil {
		t.Fatal(err)
	}
	recs := readReport(t, out)
	if d := recs[3]; d["type"] != "store_diff" || d["error"] == nil {
		t.Errorf("the unfinished snapshot's diff: %v", d)
	}
}

func TestEventRefusesBadFields(t *testing.T) {
	out := t.TempDir()
	for _, kv := range []string{"novalue", "=x", "type=run", "at=now"} {
		if err := appendEvent(out, "fault", []string{kv}, time.Now()); err == nil {
			t.Errorf("event field %q accepted", kv)
		}
	}
}

func TestValidate(t *testing.T) {
	ok := record{"type": "scenario", "at": "2026-11-03T12:00:00Z", "name": "setup", "result": "PASS",
		"duration_ms": 1, "bound_ms": 1}
	if err := validate(ok); err != nil {
		t.Fatal(err)
	}
	for name, mod := range map[string]func(record){
		"no time":        func(r record) { delete(r, "at") },
		"no field":       func(r record) { delete(r, "bound_ms") },
		"unknown result": func(r record) { r["result"] = "passed" },
		"unknown type":   func(r record) { r["type"] = "scenarios" },
	} {
		r := maps.Clone(ok)
		mod(r)
		if validate(r) == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}
