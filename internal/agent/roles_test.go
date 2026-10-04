package agent

import (
	"encoding/json"
	"sort"
	"strings"
	"testing"
)

// The guidance teaches exactly the launcher's operations: every step and
// local step, pending and recover, and the inbox's read and acknowledgement.
// A step added to the launcher, or renamed, fails here until the guidance
// teaches it (pilot G2).
func TestRoleGuidanceTeachesEveryOperation(t *testing.T) {
	want := map[string]bool{"pending": true, "recover": true, "inbox": true, "ack": true}
	for op := range stepOps {
		want[op] = true
	}
	for op := range localOps {
		want[op] = true
	}
	got := guideOps()
	var missing, extra []string
	for op := range want {
		if !got[op] {
			missing = append(missing, op)
		}
	}
	for op := range got {
		if !want[op] {
			extra = append(extra, op)
		}
	}
	sort.Strings(missing)
	sort.Strings(extra)
	if len(missing) > 0 || len(extra) > 0 {
		t.Fatalf("the guidance misses %v and teaches unknown %v", missing, extra)
	}
}

// The rendered guidance names every role, the inbox rule, the digest
// definition and the interim writing rule's canonical source, and each
// example body is JSON.
func TestRoleGuidanceContent(t *testing.T) {
	md := rolesMD()
	for _, s := range []string{"## Coordinator", "## Worker", "## Independent", "## Every member",
		"aicrew-agent session status", "aicrew-agent inbox", "inbox -ack", "at session start and after each step",
		"lowercase hex SHA-256 of the manifest's exact bytes at the pinned commit", "01a0d996-616b",
		"aicrew-agent step offer -body '{", "aicrew-agent step accept -attempt ATTEMPT_ID -task TASK_ID",
		"never use another\ncredential", "The launcher holds your session.", "are refused inside the client",
		managedNote} {
		if !strings.Contains(md, s) {
			t.Errorf("ROLES.md lacks %q", s)
		}
	}
	for op, bodies := range GuidanceBodies() {
		for _, b := range bodies {
			if !json.Valid([]byte(b)) || strings.Contains(b, "'") {
				t.Errorf("%s: the example body is not JSON a single-quoted shell argument can hold: %s", op, b)
			}
		}
	}
	if strings.Contains(md, "(below)") || strings.Contains(md, "<<'EOF'") {
		t.Error("ROLES.md points below for the digest, or shows a heredoc")
	}
}
