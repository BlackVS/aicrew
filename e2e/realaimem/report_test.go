//go:build realaimem

package realaimem

import (
	"encoding/json"
	"os"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

// The run's report: JSON lines, one object per line, each with a "type".
//
//   - "run": the aicrew and aimem commits, the Go version, the OS.
//   - "step": one `aicrew-agent step`, with the measured latency of its
//     coordination round trip: begin (aicrewd's route), each aimem call the
//     member's client made (the timed aimem), each coordination.v1 fact
//     aimem asked aicrewd for meanwhile, and settle (aicrewd's route). The
//     route durations are aicrewd's own (`duration_ms`, whole
//     milliseconds); the aimem calls are timed around the CLI, which
//     includes aimem's coordination call.
//   - "scenario": PASS or FAIL, its assertions and its duration.
//   - "summary": the counts, and the slowest coordination fact and aimem
//     call of the run, against aimem's 5 s context-age bound.
type report struct {
	t    *testing.T
	path string
	mu   sync.Mutex
	f    *os.File

	maxFactMs, maxAimemMs int64
	pass, fail            int
	results               []*scenario
}

func newReport(t *testing.T, defaultPath string) *report {
	path := os.Getenv("AICREW_E2E_REPORT")
	if path == "" {
		path = defaultPath
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		t.Fatalf("the report: %v", err)
	}
	t.Logf("report: %s", path)
	return &report{t: t, path: path, f: f}
}

func (r *report) write(v map[string]any) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.f == nil {
		return
	}
	v["at"] = time.Now().UTC().Format(time.RFC3339Nano)
	b, _ := json.Marshal(v)
	r.f.Write(append(b, '\n'))
}

func (r *report) run(h *harness) {
	r.write(map[string]any{"type": "run", "aicrew_commit": h.aicrewV, "aimem_commit": h.aimemV,
		"go": runtime.Version(), "os": runtime.GOOS, "arch": runtime.GOARCH})
}

func (r *report) close() {
	r.mu.Lock()
	f := r.f
	r.mu.Unlock()
	if f == nil {
		return
	}
	r.write(map[string]any{"type": "summary", "scenarios_passed": r.pass, "scenarios_failed": r.fail,
		"max_coordination_fact_ms": r.maxFactMs, "max_aimem_call_ms": r.maxAimemMs, "context_age_bound_ms": 5000})
	r.mu.Lock()
	r.f.Close()
	r.f = nil
	r.mu.Unlock()
}

// scenario is one scenario's record.
type scenario struct {
	r          *report
	t          *testing.T
	name       string
	start      time.Time
	assertions []map[string]any
	finished   bool
	failed     bool
}

func (r *report) scenario(t *testing.T, name string) *scenario {
	sc := &scenario{r: r, t: t, name: name, start: time.Now()}
	t.Cleanup(sc.finish)
	return sc
}

// check records an assertion and fails the scenario when it does not hold.
func (sc *scenario) check(what string, ok bool, detail ...any) {
	sc.t.Helper()
	sc.checkCase("", what, ok, detail...)
}

// checkCase records an assertion that belongs to a skip-the-fault case: the
// one the case's run must fail when its fault is left out.
func (sc *scenario) checkCase(c, what string, ok bool, detail ...any) {
	sc.t.Helper()
	a := map[string]any{"what": what, "ok": ok}
	if c != "" {
		a["case"] = c
	}
	sc.assertions = append(sc.assertions, a)
	if !ok {
		sc.t.Errorf("%s: %s %v", sc.name, what, detail)
	}
}

// require records an assertion that the rest of the scenario depends on.
func (sc *scenario) require(what string, ok bool, detail ...any) {
	sc.t.Helper()
	sc.check(what, ok, detail...)
	if !ok {
		sc.t.FailNow()
	}
}

func (sc *scenario) finish() {
	if sc.finished {
		return
	}
	sc.finished = true
	result := "PASS"
	if sc.t.Failed() {
		result = "FAIL"
		sc.r.fail++
	} else {
		sc.r.pass++
	}
	sc.r.write(map[string]any{"type": "scenario", "name": sc.name, "result": result, "assertions": sc.assertions,
		"duration_ms": time.Since(sc.start).Milliseconds()})
	sc.r.mu.Lock()
	sc.r.results = append(sc.r.results, sc)
	sc.r.mu.Unlock()
	sc.failed = result == "FAIL"
}

// verdict writes the skip-the-fault verdict of case c: failed_as_expected
// only when H passed, and c's scenario failed on an assertion tagged with
// c. Anything else (H failing, the scenario passing, or failing for
// another reason) is not_as_expected. An absent record, as when the run
// never got this far, is a rejected result too.
func (r *report) verdict(c string) {
	scenarioName, _, _ := strings.Cut(c, "-")
	var hPassed, ran, failed, caseFailed bool
	var reasons []string
	for _, sc := range r.results {
		switch sc.name {
		case "H":
			hPassed = !sc.failed
		case scenarioName:
			ran, failed = true, sc.failed
			for _, a := range sc.assertions {
				if a["case"] == c && a["ok"] == false {
					caseFailed = true
				}
			}
		}
	}
	if !hPassed {
		reasons = append(reasons, "H did not pass")
	}
	if !ran {
		reasons = append(reasons, scenarioName+" did not run")
	}
	if ran && !failed {
		reasons = append(reasons, scenarioName+" passed without its fault")
	}
	if failed && !caseFailed {
		reasons = append(reasons, scenarioName+" failed, but not on an assertion of "+c)
	}
	v := "failed_as_expected"
	if len(reasons) > 0 {
		v = "not_as_expected"
	}
	r.write(map[string]any{"type": "skip_verdict", "case": c, "verdict": v, "reasons": reasons})
}

// window marks the logs' ends before a step, so its entries can be told
// from those before.
type window struct {
	aicrewd, timing int
	start           time.Time
}

func (h *harness) mark() window {
	return window{aicrewd: len(lines(h.aicrewdLog)), timing: len(lines(h.timingLog)), start: time.Now()}
}

// latency attributes the logs' entries since w to the step, and reports
// them.
func (h *harness) latency(sc *scenario, w window, mem, op string, exit int, answer StepAnswerLite) {
	rec := map[string]any{"type": "step", "scenario": sc.name, "member": mem, "op": op, "exit": exit,
		"status": answer.Status, "total_ms": time.Since(w.start).Milliseconds()}
	var begin, settle, facts []int64
	for _, l := range tail(lines(h.aicrewdLog), w.aicrewd) {
		var e struct {
			Msg      string `json:"msg"`
			Route    string `json:"route"`
			Status   int    `json:"status"`
			Duration int64  `json:"duration_ms"`
		}
		if json.Unmarshal([]byte(l), &e) != nil || e.Msg != "request" {
			continue
		}
		switch {
		case strings.HasSuffix(e.Route, "/coordination"):
			facts = append(facts, e.Duration)
		case strings.HasSuffix(e.Route, "/settle"):
			settle = append(settle, e.Duration)
		case strings.HasPrefix(e.Route, "POST /v1/crew/attempts"):
			begin = append(begin, e.Duration)
		}
	}
	var calls []map[string]any
	for _, l := range tail(lines(h.timingLog), w.timing) {
		var e struct {
			Cmd   string `json:"cmd"`
			Op    string `json:"op"`
			Start int64  `json:"start_ns"`
			End   int64  `json:"end_ns"`
			Exit  int    `json:"exit"`
		}
		if json.Unmarshal([]byte(l), &e) != nil {
			continue
		}
		ms := (e.End - e.Start) / int64(time.Millisecond)
		calls = append(calls, map[string]any{"cmd": e.Cmd, "op": e.Op, "ms": ms, "exit": e.Exit})
		if e.Cmd == "reservation" && ms > h.report.maxAimemMs {
			h.report.maxAimemMs = ms
		}
	}
	for _, f := range facts {
		if f > h.report.maxFactMs {
			h.report.maxFactMs = f
		}
	}
	rec["begin_ms"], rec["settle_ms"], rec["coordination_fact_ms"], rec["aimem_calls"] = begin, settle, facts, calls
	h.report.write(rec)
}

func tail(all []string, from int) []string {
	if from >= len(all) {
		return nil
	}
	return all[from:]
}
