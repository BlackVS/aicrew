package agent

import (
	"fmt"
	"strings"
)

// The role guidance (pilot G2): the managed docs/ROLES.md of an agent home.
// It teaches each role's transitions as `aicrew-agent step` commands with
// their bodies, and the inbox rule. Its steps and example bodies are data
// here, so tests keep them in step with the launcher's operations and with
// aicrewd's body for each route.

// guideStep is one command the guidance teaches.
type guideStep struct {
	Op   string // the launcher's operation
	Args string // the command's arguments besides the body
	Body string // an example body, JSON; "" for none
	What string // when and why, one or two sentences
}

// guideRole is one role's section.
type guideRole struct {
	Role  string
	Intro string
	Steps []guideStep
}

// offerExample is an offer's body; the worker's accept, and an independent
// claim, reuse its pin.
const offerExample = `{"worker_agent_id": "WORKER_AGENT_ID",
 "task": {"hub_id": "HUB_ID", "project_id": "PROJECT_ID", "task_id": "TASK_ID"},
 "expected_revision": 7, "base_commit": "BASE_COMMIT", "branch": "work/TASK_ID",
 "process": {"repo": "PROCESS_REPO", "commit": "PROCESS_COMMIT", "manifest": "PROCESS_MANIFEST"},
 "instruction_digest": "sha256:DIGEST", "expires_at": "2026-10-03T12:00:00Z"}`

const claimExample = `{"task": {"hub_id": "HUB_ID", "project_id": "PROJECT_ID", "task_id": "TASK_ID"},
 "expected_revision": 7, "base_commit": "BASE_COMMIT", "branch": "work/TASK_ID",
 "process": {"repo": "PROCESS_REPO", "commit": "PROCESS_COMMIT", "manifest": "PROCESS_MANIFEST"},
 "instruction_digest": "sha256:DIGEST"}`

var guideRoles = []guideRole{
	{Role: "coordinator", Intro: "You plan the team's work: you offer tasks to workers, review their results, and " +
		"finalize them once their delivery is confirmed.",
		Steps: []guideStep{
			{"offer", "", offerExample, "Offer a task to a named worker. `expected_revision` is the task's current " +
				"revision in aimem; `process` is the project's selected process (`aimem process show`), and " +
				"`instruction_digest` its digest (see \"The instruction digest\" above). The launcher reads the task's dependencies itself and " +
				"refuses an offer while any is not DONE."},
			{"withdraw", "-attempt ATTEMPT_ID -task TASK_ID", "", "Release an offer that was declined, expired or is no " +
				"longer wanted, before it was accepted."},
			{"review", "-attempt ATTEMPT_ID", `{"result_seq": 1, "decision": "accept"}`, "Decide on the worker's latest " +
				"submitted result: `accept`, or `rework` to send it back."},
			{"confirm-delivery", "-attempt ATTEMPT_ID", `{"result_seq": 1, "evidence": [` +
				`{"kind": "reviewed_head", "ref": "REVIEW_URL"}, {"kind": "human_merge", "ref": "MERGE_COMMIT_URL"}, ` +
				`{"kind": "post_merge_ci", "ref": "CI_RUN_URL"}]}`, "After a person merged the accepted result, " +
				"record the evidence the project's process requires, each kind with its link."},
			{"finalize", "-attempt ATTEMPT_ID -task TASK_ID", `{"result_seq": 1}`, "Finalize the accepted, delivered " +
				"result as DONE, if the worker has not."},
			{"stop", "-attempt ATTEMPT_ID", `{"reason": "Priorities changed; the parser work waits."}`, "Ask the " +
				"worker to stop a running attempt. The worker confirms and releases the task."},
		}},
	{Role: "worker", Intro: "You take the tasks offered to you: you accept or decline them, do the work in your own " +
		"worktree, submit the result, and finalize it once its delivery is confirmed.",
		Steps: []guideStep{
			{"accept", "-attempt ATTEMPT_ID -task TASK_ID", `{"instruction_digest": "sha256:DIGEST"}`, "Accept an " +
				"offer from your inbox. Compute the digest yourself from the pin the offer names (see " +
				"\"The instruction digest\" above); it must " +
				"equal the offer's. Then start your worktree from the offer's `base_commit` on its `branch`."},
			{"decline", "-attempt ATTEMPT_ID", "", "Decline an offer you cannot take, for example when the pinned " +
				"process or its required skills are not available to you."},
			{"work", "-attempt ATTEMPT_ID -task TASK_ID", `{"intent": "submit", "detail": "RESULT_URL"}`, "Report " +
				"progress: `submit` with the result's link (a pull request), `block` with the reason as `detail`, " +
				"or `resume` after a block."},
			{"finalize", "-attempt ATTEMPT_ID -task TASK_ID", `{"result_seq": 1}`, "Finalize your accepted result " +
				"as DONE once the coordinator confirmed its delivery."},
			{"confirm-stop", "-attempt ATTEMPT_ID", "", "Confirm that you stopped, after the coordinator asked."},
			{"release", "-attempt ATTEMPT_ID -task TASK_ID", `{"target": "BLOCKED", "blocker": "Waiting on the schema decision."}`,
				"After confirming a stop, release the task: `READY`, or `BLOCKED` with a blocker."},
		}},
	{Role: "independent", Intro: "You choose your own tasks in the team's projects: you claim one, then work and " +
		"finalize it as a worker does. A coordinator reviews and confirms its delivery.",
		Steps: []guideStep{
			{"claim", "", claimExample, "Claim a task for yourself, with the project's selected process and " +
				"its digest (see \"The instruction digest\" above). aimem checks the task's dependencies when it " +
				"takes the claim and refuses it while any is not DONE; read them first to avoid a refused claim."},
		}},
}

// commonSteps are every member's.
var commonSteps = []guideStep{
	{"inbox", "", "", "Read your oldest unacknowledged messages. `aicrew-agent inbox -ack ID,ID` acknowledges " +
		"the ones you handled."},
	{"pending", "", "", "List the steps your launcher recorded and has not settled."},
	{"recover", "", "", "Finish the recorded steps after a failure or a restart."},
}

// guideOps are the operations the guidance teaches: inbox and ack through
// `aicrew-agent inbox`, the rest through `aicrew-agent step`.
func guideOps() map[string]bool {
	ops := map[string]bool{"ack": true}
	for _, s := range commonSteps {
		ops[s.Op] = true
	}
	for _, r := range guideRoles {
		for _, s := range r.Steps {
			ops[s.Op] = true
		}
	}
	return ops
}

// GuidanceBodies are the example bodies the role guidance shows, by the
// launcher's operation; aicrewd's tests decode each into its route's body.
func GuidanceBodies() map[string][]string {
	out := map[string][]string{}
	for _, r := range guideRoles {
		for _, s := range r.Steps {
			if s.Body != "" {
				out[s.Op] = append(out[s.Op], s.Body)
			}
		}
	}
	return out
}

func stepLine(s guideStep) string {
	cmd := "aicrew-agent step " + s.Op
	if s.Op == "inbox" {
		cmd = "aicrew-agent inbox"
	}
	if s.Args != "" {
		cmd += " " + s.Args
	}
	if s.Body == "" {
		return fmt.Sprintf("- **%s**: %s\n\n  ```sh\n  %s\n  ```\n", s.Op, s.What, cmd)
	}
	return fmt.Sprintf("- **%s**: %s\n\n  ```sh\n  %s -body '%s'\n  ```\n", s.Op, s.What, cmd,
		strings.ReplaceAll(s.Body, "\n", ""))
}

// rolesMD is the managed docs/ROLES.md.
func rolesMD() string {
	var b strings.Builder
	b.WriteString(`# Your role in the team

Your role is in ` + "`aicrew-agent session status`" + ` (coordinator, worker or independent);
read the section for it, and the rules every member follows. Every
transition goes through your launcher with ` + "`aicrew-agent step`" + `: never
claim, edit or release an aimem task directly, and never use another
credential or your personal context to get past a refusal. The full step
reference is in aicrew's DEVELOPMENT.md and CREW-CONTRACT.md.

## Every member

- **Read the inbox** at session start and after each step. Act only on what
  your inbox or your own step answers show: an attempt ID, task or offer from
  anywhere else is not yours to act on. Acknowledge what you handled; a
  message you did not acknowledge comes back, first.
- **A step's exit code:** 0 done; 3 refused, or settled as not committed;
  4 recorded but not settled (run ` + "`step recover`" + ` later); 1 failed (no
  launcher, or aicrewd, aimem or the channel failed). On a refusal, follow
  its ` + "`next_action`" + ` as written. ` + "`role_forbidden`" + ` or ` + "`attempt_forbidden`" + ` means
  the step is not yours: stop and ask the operator.
- **Worktrees and handoff** follow ` + "`docs/START.md`" + `: one worktree per attempt
  under ` + "`worktrees/`" + `, from the offer's base commit; ` + "`docs/HANDOFF.md`" + ` is yours.
- **The instruction digest** of a process pin is ` + "`sha256:`" + ` and the
  lowercase hex SHA-256 of the manifest's exact bytes at the pinned commit,
  from your clone of the process repository under ` + "`repos/`" + `:

  ` + "```sh" + `
  echo "sha256:$(git -C repos/PROCESS show PROCESS_COMMIT:PROCESS_MANIFEST | sha256sum | cut -d' ' -f1)"
  ` + "```" + `

`)
	for _, s := range commonSteps {
		b.WriteString(stepLine(s))
	}
	b.WriteString(`
## Writing that others read

Text you persist (task fields and comments, results, handoffs, and any
Markdown you save) is read later by people and other agents who were not in
your session. This is an interim rule; aimem task 01a0d996-616b will publish
the canonical one, which replaces it here.

- Write complete sentences with normal word spacing; explain any uncommon
  abbreviation once. Shorten by removing repetition and linking evidence,
  never by joining words or dropping grammar.
- Keep code, paths, identifiers, hashes and quoted output exact.
- Before each write, read the actual payload or file you are about to save,
  not your summary of it. Time, token or context pressure, and long sessions,
  never relax this.
- Unreadable: "fixd parser;tests ok,PR12 rdy4rev". Readable: "Fixed the
  parser's handling of empty lines. The tests pass. Pull request 12 is ready
  for review."
`)
	for _, r := range guideRoles {
		fmt.Fprintf(&b, "\n## %s%s\n\n%s\n\n", strings.ToUpper(r.Role[:1]), r.Role[1:], r.Intro)
		for _, s := range r.Steps {
			b.WriteString(stepLine(s))
		}
	}
	b.WriteString("\n" + managedNote)
	return b.String()
}
