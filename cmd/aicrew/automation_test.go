package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// aicrew automation replay runs a script against a member's run log with
// the recording host: the actions are printed per event and none is
// performed (the directory holds nothing new, and no hub is reached); a
// script without the header, a missing file and a bad log line are
// failures, and anything but replay with both flags is a usage error.
func TestAutomationReplay(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join(dir, "s.star")
	log := filepath.Join(dir, "automations.jsonl")
	os.WriteFile(script, []byte(`# aicrew-automation: {"host_api": 1}
def main(event):
    p = pr()
    if "forge.changed" in event.kinds:
        fail("not handled")
    step("seen", p.head)
    label(add = ["review-this"])
    return None
`), 0o600)
	os.WriteFile(log, []byte(`{"at":"2026-10-19T12:00:00Z","run":"r1","attempt":"a1","script":"review-request.star","kind":"review_request","text":"x"}
{"at":"2026-10-19T12:00:01Z","run":"r1","attempt":"a1","script":"review-request.star","outcome":"handled","events":["pull#4@0123456"],"kinds":["after-push"],"point":"after-push","event":{"kinds":["after-push"],"ids":["pull#4@0123456"]},"facts":{"pr":{"number":4,"head":"0123456","labels":["hands-reviewed"]}}}
{"at":"2026-10-19T12:00:02Z","run":"r2","attempt":"a1","script":"review-request.star","outcome":"handled","kinds":["forge.changed"],"event":{"kinds":["forge.changed"],"ids":["pull#4@0123456"],"change":{"labels_added":["review-this"]}}}
`), 0o600)
	r := cli(t, "automation", "replay", "--script", script, "--events", log)
	if r.code != 0 {
		t.Fatalf("%d: %s%s", r.code, r.stdout, r.stderr)
	}
	for _, w := range []string{
		"replaying s.star against " + log + ": nothing is performed\n",
		"event 1: after-push pull#4@0123456 (run r1 of review-request.star, recorded as handled)\n  step seen: 0123456\n  label: add [review-this], remove []\n  -> handled\n",
		"event 2: forge.changed pull#4@0123456 (run r2 of review-request.star, recorded as handled)\n  pr(): the run recorded no pull request; an empty one\n  -> failed: the script failed: fail: not handled\n",
		"2 event(s) replayed\n",
	} {
		if !strings.Contains(r.stdout, w) {
			t.Errorf("the output lacks %q:\n%s", w, r.stdout)
		}
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 2 {
		t.Errorf("the replay wrote files: %v", entries)
	}

	if r := cli(t, "automation", "replay", "--script", script, "--events", filepath.Join(dir, "none.jsonl")); r.code != 1 {
		t.Errorf("a missing log: %d %s", r.code, r.stderr)
	}
	bad := filepath.Join(dir, "bad.star")
	os.WriteFile(bad, []byte("def main(event):\n    return None\n"), 0o600)
	if r := cli(t, "automation", "replay", "--script", bad, "--events", log); r.code != 1 || !strings.Contains(r.stderr, "bad.star") {
		t.Errorf("a script without the header: %d %s", r.code, r.stderr)
	}
	os.Remove(bad)
	for _, args := range [][]string{{"automation"}, {"automation", "replay"}, {"automation", "replay", "--script", script}, {"automation", "run"}} {
		if r := cli(t, args...); r.code != 2 || !strings.Contains(r.stderr, "aicrew automation replay --script FILE --events LOG") {
			t.Errorf("%v: %d %s", args, r.code, r.stderr)
		}
	}
}
