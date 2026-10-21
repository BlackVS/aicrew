package agent

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Managed Claude Code commands (ef73): `join` writes the team's recurring
// instructions into the home as slash commands, so a member never needs a
// typed prompt. Every role's commands are in every home, as every role's
// section is in ROLES.md: a member's role can change, and a refresh does not
// ask aicrewd. A role's command checks the member's role first. The steps a
// command teaches are rendered from the same data as ROLES.md (roles.go), so
// the two cannot disagree.

// crewCommand is one managed command.
type crewCommand struct {
	Name string // the command, without its slash; always crew-…
	Role string // the role it is for; "" for every member
	Desc string // the one-line description the client lists
	Hint string // the argument hint, if it takes one
	Body string // what to do, in Markdown
	Ops  []string
}

// crewCommands are the managed commands, in the order the client lists them.
var crewCommands = []crewCommand{
	{Name: "crew-start", Desc: "Start your aicrew session: read the home guidance and your inbox, then act within your role",
		Body: "Start your aicrew session.\n\n" +
			"1. Read `docs/START.md` and `docs/ROLES.md`.\n" +
			"2. Run `aicrew-agent session status`: it names your role and your team's projects.\n" +
			"3. If you are the coordinator, read `" + OpenAttemptsFile + "`, the team's open attempts at this " +
			"session's start, and reconcile them first, as `docs/ROLES.md` says under \"Reconcile first\".\n" +
			"4. Run `aicrew-agent inbox`.\n" +
			"5. Say in one short message what you will do; then act on what you found, within your role only.\n" +
			"6. If there is nothing to do, end your turn: the home's Stop hook wakes you when a message arrives.\n\n" +
			"Never run `aicrew-agent session start`, `session leave` or `run` here: the launcher holds this " +
			"home's session, and they are refused inside the client.",
		Ops: []string{"inbox"}},
	{Name: "crew-turn", Hint: "<brief>",
		Desc: "One headless turn of your aicrew session: read the brief and your notes, act within your role, update your handoff, end",
		Body: "The turn loop (`aicrew-agent serve`) started this turn, a fresh conversation, with the brief below: " +
			"your attempt's state on its first line, who and where you are, the frozen task, what started the turn " +
			"with the messages' content, the pull requests as last observed, a worker's handoff and the attempt's " +
			"ledger, and where to fetch more. Nobody answers a question during a turn, and nothing reaches you " +
			"until it ends.\n\n" +
			"$ARGUMENTS\n\n" +
			"1. Read `docs/HANDOFF.md`, your own notes, and `docs/START.md` and `docs/ROLES.md`.\n" +
			"2. Act on the brief within your role, as `docs/ROLES.md` says: it holds your status and the messages, " +
			"so read `aicrew-agent inbox` only for what it cut, and `aicrew-agent session status` only when it " +
			"says to. If you are the coordinator, read the board for the tasks the messages name. A worker with " +
			"a running attempt goes on with its work: the loop gives it another turn after this one, until it " +
			"submits or blocks.\n" +
			"3. Acknowledge what you handled.\n" +
			"4. Update `docs/HANDOFF.md` with what you did and what comes next: the next turn starts from it and " +
			"a new brief, with nothing of this conversation. A worker with an attempt keeps the four parts of its " +
			"CURRENT ATTEMPT section current (Done, Decisions, Next, Open questions); two turns in a row that " +
			"make no step and leave them unchanged stall the attempt. The section's heading and first line and " +
			"the Archive are the turn loop's.\n" +
			"5. End the turn. Never wait inside it: no sleep, no polling, no background watch. The next " +
			"message starts the next turn.\n\n" +
			"A turn runs without prompts, under an allow list: git, `aicrew-agent`, the project's build and test " +
			"commands, and edits in `worktrees/` and of `docs/HANDOFF.md`. Run each command alone, in one Bash " +
			"call, from the home: no `cd`, `&&`, `;`, pipe or redirection, since a compound command is judged as " +
			"a whole and denied. Use the Bash tool, not PowerShell. Read files with the Read tool and change them " +
			"with the Edit tool, never with the shell. When a call is denied, run what you need again alone as " +
			"these steps say; never work around a denial, and report one you cannot avoid in the handoff.\n\n" +
			"Never run `aicrew-agent session start`, `session leave`, `run` or `serve` here: the loop holds this " +
			"home's session.",
		Ops: []string{"inbox"}},
	{Name: "crew-inbox", Desc: "Read your aicrew inbox, act on it within your role, and acknowledge what you handled",
		Body: "Run `aicrew-agent inbox`. Act only on what it shows, within your role and your accepted attempt. " +
			"Acknowledge each message you handled with `aicrew-agent inbox --ack MESSAGE_ID,MESSAGE_ID`, the " +
			"messages' `id` from `aicrew-agent inbox --json` (acknowledging is not a `step`); an offer is handled " +
			"once you accepted or declined it, since the inbox shows only unacknowledged messages. An offer's " +
			"repository, branch, base commit and process pin are in `aicrew-agent inbox --json`.",
		Ops: []string{"inbox"}},
	{Name: "crew-triage", Role: "coordinator", Hint: "<task id>",
		Desc: "Coordinator: triage an aimem task between BACKLOG and READY",
		Body: "Triage task $ARGUMENTS of one of your team's projects (`aicrew-agent session status` lists them).\n\n" +
			"1. Read it with aimem's `get_task`. Assess it against its acceptance criteria, non-goals and " +
			"dependencies.\n" +
			"2. With aimem's `triage_task`, move it between BACKLOG and READY and set its `next_action`; record " +
			"your assessment with `add_task_comment`.\n" +
			"3. Never triage a task under a hold (aimem answers `task_held`: withdraw the offer or wait for its " +
			"release first), and never set DONE or CANCELLED this way."},
	{Name: "crew-offer", Role: "coordinator", Hint: "<task id> <worker agent id>",
		Desc: "Coordinator: offer a READY aimem task to a worker",
		Body: "Offer task $ARGUMENTS.\n\n" +
			"1. Read the task with aimem's `get_task`. If it is not READY, triage it first (`/crew-triage`).\n" +
			"2. Check that every dependency is DONE; the launcher refuses the offer otherwise.\n" +
			"3. Read the project's selected process (`aimem process show`) and compute its instruction digest " +
			"with `aicrew-agent digest` (`docs/ROLES.md`, \"The instruction digest\").\n" +
			"4. Name the project's repository as the hub binds it (`aimem project show`), or pass " +
			"`--repository REPO_URL` to fill it from the forge.\n" +
			"5. Send the offer with the step below, then tell the worker nothing else: the offer reaches its inbox.",
		Ops: []string{"offer"}},
	{Name: "crew-review", Role: "coordinator", Hint: "<attempt id>",
		Desc: "Coordinator: review a worker's submitted result against the task's frozen scope",
		Body: "Review the latest result submitted on attempt $ARGUMENTS.\n\n" +
			"1. Read the task's frozen scope: objective, acceptance criteria and non-goals.\n" +
			"2. Review the submitted pull request at level high with the `oh-code-review` skill, against that " +
			"scope. Read its head and labels with `aicrew-agent forge pr show --pr N`, post the review with " +
			"`aicrew-agent forge pr comment --pr N --body-file FILE`, and request an external review with " +
			"`aicrew-agent forge pr label add --pr N LABEL` where the project's process asks for one. " +
			pullRequestNever + "\n" +
			"3. Decide with the review step below: `accept`, or `rework` to send it back. Before you accept a pull " +
			"request that is still a draft, when the project's process marks it ready once its gates are green, mark " +
			"it with `aicrew-agent forge pr ready --pr N`: it needs no person.\n" +
			"4. Stop there. A person merges the pull request; only after that, record the delivery evidence " +
			"(`aicrew-agent forge pr show --pr N` names the merge commit).",
		Ops: []string{"review", "confirm-delivery"}},
	{Name: "crew-escalate", Role: "coordinator", Hint: "<task id>",
		Desc: "Coordinator: escalate a question you cannot settle to the architect",
		Body: "Escalate a question about task $ARGUMENTS that you cannot settle within your role.\n\n" +
			"1. Check that it is one to escalate (`docs/ROLES.md`, the coordinator's \"You do not talk to a human\"): " +
			"the floor (" + codeList(escalationFloor) + "), " + codeList(escalationAlways) + ", or a question you " +
			"cannot classify with confidence. Take the most restrictive category that fits.\n" +
			"2. Run `aicrew-agent escalations`. If an open escalation already asks this question, stop here: do " +
			"not raise it again, and carry on with other work while it is open.\n" +
			"3. Raise it with the step below: two to four options, each with its consequence, your recommendation, " +
			"the blocked member's agent ID if one waits, and the urgency.\n" +
			"4. Mirror it on the task with aimem's `add_task_comment`, a comment whose first line is " +
			"`[escalation.request ID]`, with the `id` the step answered. If aimem answers `task_held`, post it when " +
			"the hold ends and say so in `docs/HANDOFF.md`.\n" +
			"5. Carry on with other work. The answer arrives in your inbox as a message whose `escalation` " +
			"(`aicrew-agent inbox --json`) carries the decision; act on it then. Never decide it yourself meanwhile, " +
			"and never raise the same question again.",
		Ops: []string{"escalate"}},
	{Name: "crew-accept", Role: "worker", Hint: "<attempt id>",
		Desc: "Worker: accept an offer from your inbox after checking its pin",
		Body: "Accept the offer on attempt $ARGUMENTS.\n\n" +
			"1. Read the offer with `aicrew-agent inbox --json`: its repository, branch, base commit, process pin " +
			"and instruction digest.\n" +
			"2. Compute the digest yourself from the pin, with the offer's `process` fields: `aicrew-agent digest " +
			"--repository PROCESS_REPO --commit PROCESS_COMMIT --manifest PROCESS_MANIFEST` (`docs/ROLES.md`, " +
			"\"The instruction digest\"). It must equal the offer's. If it does not, if the command fails, or if " +
			"the pinned process's skills are not available to you, decline instead.\n" +
			"3. Accept with the step below, then start your worktree with `aicrew-agent clone`, from the offer's " +
			"base commit on its branch. If the clone fails, the attempt is already accepted and decline is " +
			"refused: block it with " + blockAfterAccept + ", the failure as the reason, then stop.\n" +
			"4. Only then acknowledge the offer's message with `aicrew-agent inbox --ack MESSAGE_ID`, the " +
			"message's `id` from `aicrew-agent inbox --json` (acknowledging is not a `step`): until you " +
			"acknowledge it, the inbox keeps showing it.",
		Ops: []string{"accept", "decline"}},
	{Name: "crew-submit", Role: "worker", Hint: "<attempt id>",
		Desc: "Worker: open your pull request and submit it for the coordinator's review",
		Body: "Submit the result of attempt $ARGUMENTS.\n\n" +
			"1. Push your branch, then open its pull request from your worktree with `aicrew-agent forge pr open " +
			"--title TITLE --body-file FILE`, with `--draft` while a gate is open if the project's process says " +
			"so. It prints the pull request's number and URL.\n" +
			"2. Take it through the gates the project's process requires: `aicrew-agent forge pr show --pr N` " +
			"reads it, `aicrew-agent forge pr label add|remove --pr N LABEL` labels it, `aicrew-agent forge " +
			"pr comment --pr N --body-file FILE` comments on it, and `aicrew-agent forge pr ready --pr N` marks a " +
			"draft ready when the process says its gates are green. The release's `gates-green` automation does " +
			"that and submits the attempt for you once both reviews and CI are green at the head, and the " +
			"attempt's lifecycle message tells you; mark ready and submit yourself only where the project's " +
			"process disables it (`aicrew-agent check` lists the effective set). " + pullRequestNever + "\n" +
			"3. Submit its URL with the work step below with `submit`; `block` with the reason, or `resume` " +
			"after a block, use the same step.",
		Ops: []string{"work"}},
	{Name: "crew-claim", Role: "independent", Hint: "<task id>",
		Desc: "Independent member: claim a READY task for yourself",
		Body: "Claim task $ARGUMENTS for yourself. It must be READY; only the coordinator triages, and a claim of " +
			"any other task is refused with `task_not_ready`. Read its dependencies first, then claim with the " +
			"step below, with the project's selected process and its digest.",
		Ops: []string{"claim"}},
	{Name: "crew-handoff", Desc: "Write your handoff in docs/HANDOFF.md",
		Body: "Update `docs/HANDOFF.md`, which is yours, so that a fresh session with no transcript can continue: " +
			"the goal; what was done, with the attempts, tasks, pull requests and commands that matter; decisions " +
			"and why; what was verified and how; open questions; the concrete next steps. Write complete " +
			"sentences (`docs/ROLES.md`, \"Writing that others read\"). Never write a secret into it."},
}

// guideStepByOp is the step ROLES.md teaches for op.
func guideStepByOp(op string) (guideStep, bool) {
	for _, s := range commonSteps {
		if s.Op == op {
			return s, true
		}
	}
	for _, r := range guideRoles {
		for _, s := range r.Steps {
			if s.Op == op {
				return s, true
			}
		}
	}
	return guideStep{}, false
}

// commandMD renders c as a Claude Code command file.
func commandMD(c crewCommand) string {
	var b strings.Builder
	b.WriteString("---\ndescription: " + c.Desc + "\n")
	if c.Hint != "" {
		b.WriteString("argument-hint: " + c.Hint + "\n")
	}
	b.WriteString("---\n\n")
	if c.Role != "" {
		fmt.Fprintf(&b, "This command is for the %s role. First run `aicrew-agent session status` and read `role`; "+
			"if it is not %s, stop and say so.\n\n", c.Role, c.Role)
	}
	b.WriteString(c.Body + "\n\n" + credentialRule + "\n")
	if len(c.Ops) > 0 {
		b.WriteString("\nThe steps, as `docs/ROLES.md` teaches them:\n\n")
		for _, op := range c.Ops {
			s, _ := guideStepByOp(op)
			b.WriteString(stepLine(s))
		}
	}
	b.WriteString("\n" + strings.ReplaceAll(managedNote, "This file", "This command"))
	return b.String()
}

// credentialRule closes every command and is one of ROLES.md's rules: smoke
// runs showed a member with broad tool access reach for a credential file
// when a clone was refused (ef73), and turn off TLS verification in git's
// global configuration when one failed (6006). A failure before an offer is
// accepted declines it; one after accepting blocks the attempt, since an
// accepted attempt is RUNNING and decline is refused there (1171d).
const credentialRule = "Never read `creds/`, and never put a credential into a command, URL, environment " +
	"variable or git configuration. Never change git's global or system configuration, and never weaken " +
	"TLS verification (`http.sslVerify`, `GIT_SSL_NO_VERIFY` or an unverified connection). When an access, " +
	"a clone or a fetch is refused or fails, stop and report it; never work around it. Before you accept " +
	"an offer, report it by declining the offer (an offer you cannot verify is declined). After you " +
	"accepted it, decline is refused: block the attempt with " + blockAfterAccept + ", the failure as the " +
	"reason."

// blockAfterAccept is the step, in the form the steps are taught, that
// reports an accepted attempt which cannot proceed.
const blockAfterAccept = "`aicrew-agent step work --attempt ATTEMPT_ID --task TASK_ID` with the body " +
	"`{\"intent\": \"block\", \"detail\": \"REASON\"}`"

// pullRequestNever is what every pull request instruction says about other
// forge clients (pilot finding 2026-10-11).
const pullRequestNever = "Never use `gh`, `tea`, `glab` or another forge client, or a forge's API with a " +
	"credential: their account is not yours. When a `forge pr` command is refused or fails, stop and report it."

// pullRequestRule is ROLES.md's rule on pull requests, which every member
// follows.
const pullRequestRule = "Open, read, label, comment on and mark ready pull requests with `aicrew-agent forge pr`, " +
	"which uses your home's own forge credential for the repository's host: `open --title TITLE --body-file FILE " +
	"[--draft]` in your worktree after pushing your branch, `show --pr N`, `label add|remove --pr N LABEL`, " +
	"`comment --pr N --body-file FILE`, and `ready --pr N`, which drops the draft flag and a `WIP:` or `Draft:` " +
	"title prefix once the gates the project's process requires are green (the merge stays a person's). Each prints the pull request's number and URL, and whether your " +
	"launcher watches it (`watch:`): a change of a watched pull request that needs your decision (the external " +
	"review at the head asks for changes, CI fails, both gates turn green, a merge or a close) arrives in your inbox " +
	"as an `action.needed` message that carries what you need: the review's body, each failed job with its log " +
	"excerpt, or the passed checks. It wakes you; progress alone (a label, CI starting) does not. Never wait for a review or CI with a background watch, a sleep loop " +
	"or a polling script: its end never reaches you. After `label add --pr N review-this`, end your turn. " + pullRequestNever

func commandPath(name string) string { return ".claude/commands/" + name + ".md" }

// claudeBuiltins are Claude Code's own commands, which a managed command must
// never shadow. Claude Code has no command that lists them, so the list is
// pinned here; the crew- prefix keeps every managed name off it.
var claudeBuiltins = map[string]bool{
	"add-dir": true, "agents": true, "bug": true, "clear": true, "compact": true, "config": true, "context": true,
	"cost": true, "doctor": true, "exit": true, "export": true, "help": true, "hooks": true, "ide": true,
	"init": true, "install-github-app": true, "login": true, "logout": true, "mcp": true, "memory": true,
	"model": true, "output-style": true, "permissions": true, "plugin": true, "pr-comments": true,
	"release-notes": true, "resume": true, "review": true, "rewind": true, "sandbox": true,
	"security-review": true, "skills": true, "status": true, "statusline": true, "terminal-setup": true,
	"todos": true, "upgrade": true, "usage": true, "vim": true,
}

// commandCollision names what a managed command would shadow: a built-in, or
// a command or skill of the same name at the member's user scope (under
// CLAUDE_CONFIG_DIR, else ~/.claude). "" when it shadows nothing.
func commandCollision(name string) string {
	if claudeBuiltins[name] || !strings.HasPrefix(name, "crew-") {
		return "a Claude Code built-in command"
	}
	uh, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	dir := claudeUserDir(uh)
	for _, p := range []string{filepath.Join(dir, "commands", name+".md"), filepath.Join(dir, "skills", name)} {
		if _, err := os.Stat(p); err == nil {
			return "your own " + p
		}
	}
	return ""
}

// commandFiles are the managed command files, each with what it collides
// with, if anything.
func commandFiles() []homeFile {
	out := make([]homeFile, 0, len(crewCommands))
	for _, c := range crewCommands {
		out = append(out, homeFile{Path: commandPath(c.Name), Content: commandMD(c), Managed: true,
			Collision: commandCollision(c.Name)})
	}
	return out
}

// HasCommand reports whether home holds the managed command name.
func HasCommand(home, name string) bool {
	_, err := os.Stat(filepath.Join(home, filepath.FromSlash(commandPath(name))))
	return err == nil
}
