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
			"3. Run `aicrew-agent inbox`.\n" +
			"4. Say in one short message what you will do; then act on what you found, within your role only.\n" +
			"5. If there is nothing to do, end your turn: the home's Stop hook wakes you when a message arrives.\n\n" +
			"Never run `aicrew-agent session start`, `session leave` or `run` here: the launcher holds this " +
			"home's session, and they are refused inside the client.",
		Ops: []string{"inbox"}},
	{Name: "crew-inbox", Desc: "Read your aicrew inbox, act on it within your role, and acknowledge what you handled",
		Body: "Run `aicrew-agent inbox`. Act only on what it shows, within your role and your accepted attempt. " +
			"Acknowledge each message you handled; an offer is handled once you accepted or declined it, since the " +
			"inbox shows only unacknowledged messages. An offer's repository, branch, base commit and process pin " +
			"are in `aicrew-agent inbox --json`.",
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
			"scope, and post the review on the pull request.\n" +
			"3. Decide with the review step below: `accept`, or `rework` to send it back.\n" +
			"4. Stop there. A person merges the pull request; only after that, record the delivery evidence.",
		Ops: []string{"review", "confirm-delivery"}},
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
			"base commit on its branch.\n" +
			"4. Only then acknowledge the offer's message: until you acknowledge it, `aicrew-agent inbox --json` " +
			"keeps showing it.",
		Ops: []string{"accept", "decline"}},
	{Name: "crew-submit", Role: "worker", Hint: "<attempt id> <pull request url>",
		Desc: "Worker: submit your result for the coordinator's review",
		Body: "Submit the result of attempt $ARGUMENTS: the pull request link, after its gates passed as the " +
			"project's process requires. Use the work step below with `submit`; `block` with the reason, or " +
			"`resume` after a block, use the same step.",
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

// credentialRule closes every command: the ef73 smoke run showed a member
// with broad tool access reach for a credential file when a clone was
// refused.
const credentialRule = "Never read `creds/`, and never put a credential into a command, URL, environment " +
	"variable or git configuration. When an access is refused, stop and report it (an offer you cannot " +
	"verify is declined); never work around it."

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
		out = append(out, homeFile{path: commandPath(c.Name), content: commandMD(c), managed: true,
			collision: commandCollision(c.Name)})
	}
	return out
}

// HasCommand reports whether home holds the managed command name.
func HasCommand(home, name string) bool {
	_, err := os.Stat(filepath.Join(home, filepath.FromSlash(commandPath(name))))
	return err == nil
}
