package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/BlackVS/aicrew/internal/opapi"
	"github.com/BlackVS/aicrew/internal/optoken"
)

func esc(id, question string) opapi.Escalation {
	return opapi.Escalation{ID: id, TeamID: "team-1", Category: "scope", Urgency: "now", Question: question,
		Task: opapi.EscalationTask{ProjectID: "aicrew", TaskID: "task-" + id}}
}

// lister answers each read from reads in turn, repeating the last one.
func lister(reads ...func() ([]opapi.Escalation, error)) (func(context.Context) ([]opapi.Escalation, error), *int) {
	n := 0
	return func(context.Context) ([]opapi.Escalation, error) {
		i := min(n, len(reads)-1)
		n++
		return reads[i]()
	}, &n
}

func openList(es ...opapi.Escalation) func() ([]opapi.Escalation, error) {
	return func() ([]opapi.Escalation, error) { return es, nil }
}

func runWait(t *testing.T, list func(context.Context) ([]opapi.Escalation, error), seen string) (int, string, string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var out, errb bytes.Buffer
	code := waitEscalations(ctx, list, seen, time.Millisecond, &out, &errb)
	return code, out.String(), errb.String()
}

// A new escalation returns at once, a seen one never again, and the next
// new one returns while the seen one stays open; lines are oldest first.
func TestWaitTellsEachEscalationOnce(t *testing.T) {
	seen := filepath.Join(t.TempDir(), "state", "escalations-seen.json")
	a, b, c := esc("aaaaaaaa-0001-7000-8000-000000000001", "first?"), esc("bbbbbbbb-0002", "second?"), esc("cccccccc-0003", "third?")

	list, _ := lister(openList(b, a)) // newest first, as the route answers
	code, out, errs := runWait(t, list, seen)
	if code != 0 || out != "escalation aaaaaaaa-0001 team team-1 task aicrew/task-aaaaaaaa-0001-7000-8000-000000000001 scope now: first?\n"+
		"escalation bbbbbbbb-0002 team team-1 task aicrew/task-bbbbbbbb-0002 scope now: second?\n" {
		t.Fatalf("first wait: %d %q %q", code, out, errs)
	}
	if fi, err := os.Stat(seen); err != nil || fi.Mode().Perm()&0o077 != 0 && os.PathSeparator == '/' {
		t.Fatalf("the seen set: %v %v", fi, err)
	}

	// Both seen and still open: the wait keeps reading until c appears.
	list, reads := lister(openList(b, a), openList(b, a), openList(c, b, a))
	code, out, _ = runWait(t, list, seen)
	if code != 0 || *reads != 3 || !strings.HasPrefix(out, "escalation cccccccc-0003 ") || strings.Count(out, "\n") != 1 {
		t.Fatalf("second wait: %d after %d reads, %q", code, *reads, out)
	}
}

// An answered escalation leaves the seen set; the set never holds an ID
// the route no longer lists as open.
func TestWaitDropsAnsweredFromSeen(t *testing.T) {
	seen := filepath.Join(t.TempDir(), "seen.json")
	if err := writeSeen(seen, []string{"old-answered", "still-open"}); err != nil {
		t.Fatal(err)
	}
	list, _ := lister(openList(esc("new-one", "q"), esc("still-open", "q")))
	if code, out, _ := runWait(t, list, seen); code != 0 || !strings.Contains(out, "new-one") || strings.Contains(out, "still-open") {
		t.Fatalf("wait: %d %q", code, out)
	}
	got, err := readSeen(seen)
	if err != nil || strings.Join(got, ",") != "still-open,new-one" {
		t.Fatalf("seen set: %v %v", got, err)
	}
}

// A list at the route's bound may leave older open escalations out, so a
// seen ID absent from it stays in the set: it is not taken as answered and
// is not told again when it comes back into the list.
func TestWaitKeepsSeenBeyondTheBound(t *testing.T) {
	seen := filepath.Join(t.TempDir(), "seen.json")
	if err := writeSeen(seen, []string{"oldest-open"}); err != nil {
		t.Fatal(err)
	}
	full := make([]opapi.Escalation, waitListBound)
	for i := range full {
		full[i] = esc(fmt.Sprintf("newer-%03d", i), "q")
	}
	list, _ := lister(openList(full...))
	if code, out, _ := runWait(t, list, seen); code != 0 || strings.Count(out, "\n") != waitListBound || strings.Contains(out, "oldest-open") {
		t.Fatalf("wait: %d, %d lines", code, strings.Count(out, "\n"))
	}
	got, err := readSeen(seen)
	if err != nil || len(got) != waitListBound+1 || got[0] != "oldest-open" {
		t.Fatalf("seen set: %d entries, first %v, %v", len(got), got[:1], err)
	}

	// The newest are answered; the oldest is listed again and not retold.
	list, reads := lister(openList(esc("oldest-open", "q")), openList(esc("later", "q"), esc("oldest-open", "q")))
	if code, out, _ := runWait(t, list, seen); code != 0 || *reads != 2 || !strings.HasPrefix(out, "escalation later ") || strings.Count(out, "\n") != 1 {
		t.Fatalf("second wait: %d after %d reads, %q", code, *reads, out)
	}
	if got, _ := readSeen(seen); strings.Join(got, ",") != "oldest-open,later" {
		t.Fatalf("seen set after the answers: %v", got)
	}
}

// Failed reads are retried; five in a row end the wait with the error, and
// a success between them resets the count.
func TestWaitEndsAfterFiveFailures(t *testing.T) {
	seen := filepath.Join(t.TempDir(), "seen.json")
	fail := func() ([]opapi.Escalation, error) { return nil, errors.New("connection refused") }

	list, reads := lister(fail, fail, fail, fail, fail)
	code, out, errs := runWait(t, list, seen)
	if code != 1 || *reads != 5 || out != "" || !strings.Contains(errs, "could not be read 5 times in a row: connection refused") {
		t.Fatalf("five failures: %d after %d reads, %q %q", code, *reads, out, errs)
	}

	list, reads = lister(fail, fail, fail, fail, openList(), fail, fail, fail, fail, openList(esc("late", "q")))
	if code, out, _ := runWait(t, list, seen); code != 0 || *reads != 10 || !strings.Contains(out, "late") {
		t.Fatalf("reset by a success: %d after %d reads, %q", code, *reads, out)
	}
}

// The coordinator's text is printed on one line, without terminal escapes.
func TestWaitLineIsClean(t *testing.T) {
	line := escalationLine(esc("dddddddd-0004", "merge\x1b[31m now?\nyes\tno"))
	if line != "escalation dddddddd-0004 team team-1 task aicrew/task-dddddddd-0004 scope now: merge now? yes no" {
		t.Fatalf("line: %q", line)
	}
	// Every field the coordinator gave is cleaned, not only the question.
	e := esc("eeeeeeee-0005", "q")
	e.Task.TaskID = "t-1\x1b]0;title\x07\nnext"
	if line := escalationLine(e); strings.ContainsAny(line, "\x1b\x07\n") {
		t.Fatalf("line: %q", line)
	}
}

// A seen set that is not JSON is refused with how to recover, never
// overwritten.
func TestWaitRefusesABrokenSeenSet(t *testing.T) {
	seen := filepath.Join(t.TempDir(), "seen.json")
	os.WriteFile(seen, []byte("{not json"), 0o600)
	list, _ := lister(openList(esc("x", "q")))
	if code, out, errs := runWait(t, list, seen); code != 1 || out != "" || !strings.Contains(errs, "remove it to be told of every open escalation again") {
		t.Fatalf("broken set: %d %q %q", code, out, errs)
	}
	if b, _ := os.ReadFile(seen); string(b) != "{not json" {
		t.Fatalf("the broken set was changed: %q", b)
	}
}

// Through aicrewd: the flags are checked, and with a refused credential
// the wait ends after five reads instead of waiting for ever.
func TestEscalationsWaitCommand(t *testing.T) {
	serve(t)
	waitUnit = time.Millisecond
	t.Cleanup(func() { waitUnit = time.Second })
	call := func(args ...string) (int, string, string) {
		var out, errb bytes.Buffer
		code := run(context.Background(), args, &out, &errb)
		return code, out.String(), errb.String()
	}
	for _, args := range [][]string{{"escalations", "wait", "--interval", "14"}, {"escalations", "wait", "extra"},
		{"escalations", "wait", "--open"}} {
		if code, _, _ := call(args...); code != 2 {
			t.Fatalf("%v: exit %d, want 2", args, code)
		}
	}
	seen := filepath.Join(t.TempDir(), "seen.json")
	bad := filepath.Join(t.TempDir(), "bad.token")
	tok, err := optoken.Generate()
	if err != nil {
		t.Fatal(err)
	}
	if err := optoken.Write(bad, tok); err != nil { // owner-only, and not the service's
		t.Fatal(err)
	}
	t.Setenv("AICREW_OPERATOR_TOKEN_FILE", bad)
	code, out, errs := call("escalations", "wait", "--interval", "15", "--seen", seen)
	if code != 1 || out != "" || !strings.Contains(errs, "5 times in a row") {
		t.Fatalf("refused credential: %d %q %q", code, out, errs)
	}
	if _, err := os.Stat(seen); err == nil {
		t.Fatal("a failed wait wrote the seen set")
	}
}
