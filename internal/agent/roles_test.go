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
		"aicrew-agent session status", "aicrew-agent inbox", "inbox --ack", "at session start and after each step",
		"lowercase hex SHA-256 of the manifest's exact bytes at the pinned commit", "01a0d996-616b",
		"aicrew-agent step offer --body '{", "aicrew-agent step accept --attempt ATTEMPT_ID --task TASK_ID",
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

// Triage is the coordinator's (PILOT-1 §5): the coordinator's section names
// the tools, the READY rule and both refusals; the other roles are told the
// coordinator triages.
func TestRoleGuidanceTriage(t *testing.T) {
	md := rolesMD()
	coord := md[strings.Index(md, "## Coordinator"):strings.Index(md, "## Worker")]
	for _, s := range []string{"**Triage is yours alone.**", "`triage_task`", "`add_task_comment`", "BACKLOG",
		"`task_not_ready`", "`task_held`", "never set DONE or CANCELLED"} {
		if !strings.Contains(coord, s) {
			t.Errorf("the coordinator's section lacks %q", s)
		}
	}
	others := md[strings.Index(md, "## Worker"):]
	if strings.Contains(others, "triage_task") || !strings.Contains(others, "You do not triage tasks") ||
		!strings.Contains(others, "only the coordinator triages") {
		t.Error("the worker and independent sections do not leave triage to the coordinator")
	}
	if !strings.Contains(md, "the coordinator's triage,\nbelow, is the one exception") {
		t.Error("the direct-write rule does not name triage as its exception")
	}
}

// The first instruction names only the home's guidance files and
// `aicrew-agent inbox`: no ID, URL, handle or secret, and no other command.
func TestFirstInstructionNamesOnlyTheHome(t *testing.T) {
	s := FirstInstruction()
	for _, want := range []string{"docs/START.md", "docs/ROLES.md", "`aicrew-agent inbox`", "within your role"} {
		if !strings.Contains(s, want) {
			t.Errorf("the first instruction lacks %q", want)
		}
	}
	for _, banned := range []string{"http", "aicrew-agent step", "session start", "token", "creds", "01a"} {
		if strings.Contains(strings.ToLower(s), banned) {
			t.Errorf("the first instruction names %q: %s", banned, s)
		}
	}
	if strings.Count(s, "`") != 2 {
		t.Errorf("the first instruction names a command besides aicrew-agent inbox: %s", s)
	}
}

// START.md tells a member where its team's projects are (3a4b): the
// coordinator lists their tasks in team mode, the worker takes its project
// and task from the offer.
func TestStartNamesTheTeamsProjects(t *testing.T) {
	for _, s := range []string{"8. **Your team's projects.**", "`aicrew-agent session status`", "your team's projects",
		"A coordinator lists a\n   project's tasks with aimem's task tools in team mode",
		"A worker takes the project, the task and the\n   repository from the offer"} {
		if !strings.Contains(startMD, s) {
			t.Errorf("START.md lacks %q", s)
		}
	}
}
