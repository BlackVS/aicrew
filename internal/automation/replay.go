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

// Facts are what a run's pr() and attempt() returned to the script, kept
// on the run's end record for a replay; a nil field is a call the script
// did not make.
type Facts struct {
	PR      *PR      `json:"pr,omitempty"`
	Attempt *Attempt `json:"attempt,omitempty"`
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
		h := &recordingHost{facts: rec.Facts, out: out}
		turn, err := Run(ctx, Script{Name: name, Source: source, Host: h, Allow: replayAllow}, *rec.Event)
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
	facts *Facts
	out   io.Writer
}

func (h *recordingHost) PR(context.Context) (PR, error) {
	if h.facts == nil || h.facts.PR == nil {
		fmt.Fprintln(h.out, "  pr(): the run recorded no pull request; an empty one")
		return PR{}, nil
	}
	return *h.facts.PR, nil
}

func (h *recordingHost) Attempt(context.Context) (Attempt, error) {
	if h.facts == nil || h.facts.Attempt == nil {
		fmt.Fprintln(h.out, "  attempt(): the run recorded no attempt; an empty one")
		return Attempt{}, nil
	}
	return *h.facts.Attempt, nil
}

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
