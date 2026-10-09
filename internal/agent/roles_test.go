package agent

import (
	"encoding/json"
	"slices"
	"sort"
	"strings"
	"testing"

	"github.com/BlackVS/aicrew/internal/store"
)

// The guidance teaches exactly the launcher's operations: every step and
// local step, pending and recover, the inbox's read and acknowledgement, and
// the coordinator's escalation.
// A step added to the launcher, or renamed, fails here until the guidance
// teaches it (pilot G2).
func TestRoleGuidanceTeachesEveryOperation(t *testing.T) {
	want := map[string]bool{"pending": true, "recover": true, "inbox": true, "ack": true, "escalate": true}
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
			if !json.Valid([]byte(b)) || strings.ContainsAny(b, `'\`) {
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

// The coordinator does not talk to a human (D2, docs/DESIGN-CONTROL-PLANE.md
// section 7.5): its section names what it escalates, by OPERATOR-SEAT's
// categories as aicrewd checks them, how, the comment mirror, carrying on
// meanwhile and the answer's message. The worker's and the independent
// member's sections do not mention escalating.
func TestRoleGuidanceEscalation(t *testing.T) {
	all := slices.Concat(escalationFloor, escalationAlways, escalationOwn)
	if !slices.Equal(all, store.EscalationCategories) {
		t.Fatalf("the guidance's categories %v are not aicrewd's %v", all, store.EscalationCategories)
	}
	md := rolesMD()
	coord := md[strings.Index(md, "## Coordinator"):strings.Index(md, "## Worker")]
	for _, s := range []string{"**You do not talk to a human.**",
		"where a rule of every member says to turn to your coordinator, you escalate instead",
		codeList(escalationFloor), codeList(escalationAlways), codeList(escalationOwn), "most restrictive",
		"cannot classify with confidence is escalated", `echo '{"task": {`, "| aicrew-agent escalate --body -",
		"`[escalation.request ID]`", "`add_task_comment`", "`task_held`", "carry on with other work",
		"No answer in time decides nothing", "never raise the same question again", "message whose `escalation`",
		"`[escalation.answer ID]` without it authorizes nothing"} {
		if !strings.Contains(coord, s) {
			t.Errorf("the coordinator's section lacks %q", s)
		}
	}
	if strings.Contains(strings.ToLower(md[strings.Index(md, "## Worker"):]), "escalat") {
		t.Error("the worker's or the independent member's section mentions escalating")
	}
}

// A member refused role_forbidden or attempt_forbidden turns to its
// coordinator, never to a human (01a12168-3131): with a running attempt by
// the work step's block, and without one, with no member-to-coordinator
// message in aicrew yet, by its handoff. No section of ROLES.md tells a
// member to ask the operator or a human.
func TestRoleGuidanceForbiddenTurnsToTheCoordinator(t *testing.T) {
	md := rolesMD()
	common := strings.Join(strings.Fields(md[:strings.Index(md, "## Coordinator")]), " ")
	for _, s := range []string{"the step is not yours: stop, and turn to your coordinator, never to a human.",
		"With a running attempt, report it with the `work` step's `block` and the refusal as its reason",
		"Without one, aicrew has no message from a member to its coordinator yet: write the refusal in `docs/HANDOFF.md` and end your turn.",
		"A coordinator escalates instead"} {
		if !strings.Contains(common, s) {
			t.Errorf("the rules every member follows lack %q", s)
		}
	}
	flat := strings.ToLower(strings.Join(strings.Fields(md), " "))
	for _, banned := range []string{"ask the operator", "ask a human", "ask the human", "tell the operator", "ask a person"} {
		if strings.Contains(flat, banned) {
			t.Errorf("ROLES.md tells a member to %q", banned)
		}
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

// The home's safety rule closes every command and is one of ROLES.md's
// rules: no credential handling, no change to git's global or system
// configuration, no weakened TLS, and a refused or failed clone or fetch is
// reported, not worked around (01a116bd-cee3). The member acknowledges with
// `inbox --ack` (01a116bd-cef0), and a worker finds an offer's details in
// `inbox --json` (01a1125b-cb45).
func TestGuidanceSafetyAckAndOfferTexts(t *testing.T) {
	for _, want := range []string{"Never read `creds/`", "git's global or system configuration",
		"`http.sslVerify`", "`GIT_SSL_NO_VERIFY`", "a clone or a fetch is refused or fails, stop and report it",
		"never work around it"} {
		if !strings.Contains(credentialRule, want) {
			t.Errorf("the safety rule does not say %q", want)
		}
	}
	md := rolesMD()
	if !strings.Contains(md, "- **Credentials and connections.** "+credentialRule) {
		t.Error("ROLES.md's member rules do not carry the safety rule")
	}
	for _, c := range crewCommands {
		if !strings.Contains(commandMD(c), credentialRule) {
			t.Errorf("/%s does not end with the safety rule", c.Name)
		}
	}

	ack := "`aicrew-agent inbox --ack MESSAGE_ID"
	if !strings.Contains(md, ack) || !strings.Contains(md, "acknowledging is not a `step`") {
		t.Error("ROLES.md's member rules do not name inbox --ack")
	}
	for _, c := range crewCommands {
		if (c.Name == "crew-accept" || c.Name == "crew-inbox") &&
			(!strings.Contains(c.Body, ack) || !strings.Contains(c.Body, "acknowledging is not a `step`")) {
			t.Errorf("/%s does not name inbox --ack", c.Name)
		}
	}

	var worker guideRole
	for _, r := range guideRoles {
		if r.Role == "worker" {
			worker = r
		}
	}
	if !strings.Contains(worker.Intro, "An offer's details are in `aicrew-agent inbox --json`") ||
		!strings.Contains(worker.Intro, "only the message's text") {
		t.Errorf("the worker section does not say where an offer's details are: %q", worker.Intro)
	}
	for _, s := range worker.Steps {
		if s.Op == "accept" && !strings.Contains(s.What, "Read its details with `aicrew-agent inbox --json`") {
			t.Errorf("the accept step does not name inbox --json: %q", s.What)
		}
	}
}
