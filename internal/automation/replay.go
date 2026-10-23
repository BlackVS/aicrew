package automation

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
)

// A replay (increment 2f, docs/DESIGN-TURNS.md 13.3): a script run against
// the events of a launcher's run log with a recording host. The launcher
// records, on each run's end record in logs/automations.jsonl, the event
// the run was for and the facts pr() and attempt() returned to the script;
// Replay runs the script's main(event) once per such record, the facts
// answering pr() and attempt() again, and every other host call logged
// as what the script would have done, performed by nothing.

// Facts are what a run's pr() and attempt() returned to the script, in the
// order it read them, kept on the run's end record for a replay; an empty
// list is a call the script did not make.
type Facts struct {
	PRs      []PR      `json:"prs,omitempty"`
	Attempts []Attempt `json:"attempts,omitempty"`
	// The reads past maxFactReads, counted and not kept.
	PRsDropped      int `json:"prs_dropped,omitempty"`
	AttemptsDropped int `json:"attempts_dropped,omitempty"`
	// PR and Attempt are the one answer each a run recorded before the
	// reads were kept in order (increment 2f): read for a replay, never
	// written, the replay being the run log's only reader.
	PR      *PR      `json:"pr,omitempty"`
	Attempt *Attempt `json:"attempt,omitempty"`
}

// maxFactReads bounds the reads of each kind a run's end record keeps: a
// replay reads a record as one line of at most maxReplayLine, and a pull
// request's answer carries its comments.
const maxFactReads = 16

// AddPR keeps a pr() answer after those before it.
func (f *Facts) AddPR(p PR) { f.PRs = keepRead(f.PRs, &f.PRsDropped, p) }

// AddAttempt keeps an attempt() answer after those before it.
func (f *Facts) AddAttempt(a Attempt) { f.Attempts = keepRead(f.Attempts, &f.AttemptsDropped, a) }

func keepRead[T any](reads []T, dropped *int, v T) []T {
	if len(reads) >= maxFactReads {
		*dropped++
		return reads
	}
	return append(reads, v)
}

// replayRecord is the part of a run's record a replay reads: a step record
// has no outcome and is skipped, and so is a run recorded before the
// launcher kept events.
type replayRecord struct {
	Run     string `json:"run"`
	Script  string `json:"script"`
	Outcome string `json:"outcome"`
	Event   *Event `json:"event"`
	Facts   *Facts `json:"facts"`
}

// maxReplayLine bounds one record of the log.
const maxReplayLine = 4 << 20

// replayAllow admits every command: nothing runs.
var replayAllow = NewAllowList([]string{"Bash(*)"}, nil)

// Replay runs the script's main(event) once per run record of log that
// carries an event, with a recording host, and writes what the script
// would have done for each event to out, then how the run ended: handled,
// a turn with its kind and content, or failed with the error. It returns
// the number of events replayed; a script that fails is reported per
// event, and only a log that cannot be read is an error.
func Replay(ctx context.Context, name string, source []byte, log io.Reader, out io.Writer) (int, error) {
	sc := bufio.NewScanner(log)
	sc.Buffer(make([]byte, 0, 64<<10), maxReplayLine)
	n, lineNo := 0, 0
	for sc.Scan() {
		lineNo++
		line := bytes.TrimSpace(sc.Bytes())
		if len(line) == 0 {
			continue
		}
		var rec replayRecord
		if err := json.Unmarshal(line, &rec); err != nil {
			return n, fmt.Errorf("line %d of the log is not a run record: %w", lineNo, err)
		}
		if rec.Outcome == "" || rec.Event == nil {
			continue
		}
		n++
		fmt.Fprintf(out, "event %d: %s %s (run %s of %s, recorded as %s)\n", n, strings.Join(rec.Event.Kinds, ","),
			strings.Join(rec.Event.IDs, ","), rec.Run, rec.Script, rec.Outcome)
		if rec.Event.BaseOnly {
			fmt.Fprintln(out, "  a base-only update")
		}
		h := newRecordingHost(rec.Facts, out)
		turn, err := Run(ctx, Script{Name: name, Source: source, Host: h, Allow: replayAllow}, *rec.Event)
		h.prs.unused(out)
		h.attempts.unused(out)
		switch {
		case err != nil:
			fmt.Fprintf(out, "  -> failed: %s\n", oneLine(err.Error()))
		case turn != nil:
			fmt.Fprintf(out, "  -> turn %s: %s\n", turn.Kind, oneLine(turn.Content))
		default:
			fmt.Fprintln(out, "  -> handled")
		}
	}
	return n, sc.Err()
}

// recordingHost answers pr() and attempt() from the recorded facts and
// logs every other call to out without performing it.
type recordingHost struct {
	prs      replayReads[PR]
	attempts replayReads[Attempt]
	out      io.Writer
}

func newRecordingHost(f *Facts, out io.Writer) *recordingHost {
	h := &recordingHost{out: out,
		prs:      replayReads[PR]{call: "pr()", what: "pull request"},
		attempts: replayReads[Attempt]{call: "attempt()", what: "attempt"}}
	if f == nil {
		return h
	}
	h.prs.recorded, h.prs.dropped = f.PRs, f.PRsDropped
	h.attempts.recorded, h.attempts.dropped = f.Attempts, f.AttemptsDropped
	// A record from before the reads were kept in order answers its one
	// answer to every read, as it did then.
	if f.PRs == nil && f.PR != nil {
		h.prs.recorded, h.prs.single = []PR{*f.PR}, true
	}
	if f.Attempts == nil && f.Attempt != nil {
		h.attempts.recorded, h.attempts.single = []Attempt{*f.Attempt}, true
	}
	return h
}

// replayReads answers one kind of read in the order the run recorded it.
// A read past the recorded ones answers the last again and says so, since
// a replay exists to explore a script that reads differently from the run.
type replayReads[T any] struct {
	call, what string
	recorded   []T
	dropped    int
	single     bool
	n          int
}

func (r *replayReads[T]) next(out io.Writer) T {
	r.n++
	switch {
	case len(r.recorded) == 0:
		fmt.Fprintf(out, "  %s: the run recorded no %s; an empty one\n", r.call, r.what)
		var zero T
		return zero
	case r.n <= len(r.recorded):
		return r.recorded[r.n-1]
	case r.single:
		return r.recorded[0]
	case r.dropped > 0:
		fmt.Fprintf(out, "  %s: read %d, the run kept %d of %d; the last again\n", r.call, r.n, len(r.recorded), len(r.recorded)+r.dropped)
	default:
		fmt.Fprintf(out, "  %s: read %d, the run recorded %d; the last again\n", r.call, r.n, len(r.recorded))
	}
	return r.recorded[len(r.recorded)-1]
}

// unused says how many recorded reads the replayed script did not make.
func (r *replayReads[T]) unused(out io.Writer) {
	if left := len(r.recorded) + r.dropped - r.n; !r.single && left > 0 {
		fmt.Fprintf(out, "  %s: %d of the run's %d reads not made\n", r.call, left, len(r.recorded)+r.dropped)
	}
}

func (h *recordingHost) PR(context.Context) (PR, error) { return h.prs.next(h.out), nil }

func (h *recordingHost) Attempt(context.Context) (Attempt, error) { return h.attempts.next(h.out), nil }

func (h *recordingHost) Run(_ context.Context, cmd string, args []string) (RunResult, error) {
	fmt.Fprintf(h.out, "  run: %s\n", CommandLine(cmd, args))
	return RunResult{}, nil
}

func (h *recordingHost) Step(_ context.Context, kind, text string) error {
	fmt.Fprintf(h.out, "  step %s: %s\n", kind, oneLine(text))
	return nil
}

func (h *recordingHost) Comment(_ context.Context, body string) error {
	fmt.Fprintf(h.out, "  comment: %s\n", oneLine(body))
	return nil
}

func (h *recordingHost) Label(_ context.Context, add, remove []string) error {
	fmt.Fprintf(h.out, "  label: add %v, remove %v\n", add, remove)
	return nil
}

func (h *recordingHost) Ready(context.Context) error {
	fmt.Fprintln(h.out, "  ready")
	return nil
}

func (h *recordingHost) Submit(_ context.Context, url string) error {
	fmt.Fprintf(h.out, "  submit: %s\n", url)
	return nil
}

func (h *recordingHost) Rerun(context.Context) (RerunResult, error) {
	fmt.Fprintln(h.out, "  rerun: not performed in a replay")
	return RerunResult{Reason: "not performed in a replay"}, nil
}

func (h *recordingHost) UpdateBase(context.Context) (UpdateBaseResult, error) {
	fmt.Fprintln(h.out, "  update_base: not performed in a replay")
	return UpdateBaseResult{Reason: "not performed in a replay"}, nil
}

// oneLine is s on one line, bounded, for the replay's output.
func oneLine(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > 300 {
		s = s[:300] + "…"
	}
	return s
}
