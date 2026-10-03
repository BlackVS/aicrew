# Proposal: what the first pilot changes in aicrew and aimem

Status: **proposal** (2026-10-03), under review; source: the hub document PROPOSAL-PILOT-1 at rev 6, copied unchanged below this line.

Status: proposal, 2026-10-03, for review through the usual gates as docs-only PRs in both repositories (aicrew `docs/proposals/`, aimem counterpart). Author: the coordinator session from the operator's decisions during the first pilot run (record: hub documents PILOT-RUN-1 and STORAGE-MODEL, pilot task 01a0d6d7-1b3d seq294 to seq343). No private names. Each section names the task that implements it; the tasks follow the merged text, not the other way round.

## 0. Why

The first pilot (2026-10-03) proved the trust chain and the attempt protocol end to end and stopped at the accept step on missing mechanisms, not on bugs in the mechanisms that exist. Every hand-off between members needed a human message, the worker had no official way to know which repository to clone and with which credential, the coordinator could not record its own triage, and the operator could not read the admin commands without an explanation. The product goal is a team that works without babysitting, onboarded by a handful of readable commands. This proposal turns the pilot's findings into design decisions.

## 1. Principles

1. **One owner per fact.** The aimem hub owns projects and everything that is a property of a project: its data, its process pin, its repository, and the access of teams to it. aicrewd owns teams, members, attempts and the members' capabilities. A fact is declared once, where its owner lives, and read by the other side.
2. **A member learns everything from its home and its inbox.** No instruction that matters arrives by chat: the team's hub, projects, repositories and credential requirements are in the home; the task, repository and base of an attempt are in the offer.
3. **Credentials are per member and per purpose.** Every member has its own identity on the hub and on each forge the team's projects use; material lives only in the member's home under `creds/` and in its aimem installation; declarations never carry values. What a token can do is decided by the forge; declarations state what is required so it can be checked before work, not at `git push`.
4. **Commands read as sentences.** Entity names are named flags; where an entity has an ID and a name, two explicit flags; names accepted wherever IDs are; one `--` style in both tools; one flag name for each recurring intent (read a secret, write a secret).
5. **No workaround ships as a procedure.** What the pilot did by hand (hand-copied tokens, typed start prompts, manual READY) is replaced by software or by an operator step that exists for a reason.
6. **No silent refusal.** Whatever is refused for a missing right or capability is refused by name, to the party that can act on it, with the command that fixes it.

## 2. Projects, repositories, teams

**2.1 A project has one repository, declared on the project in aimem.** `aimem project repo set --project <p> --kind github|gitea|gitlab --url <clone url> [--default-branch <b>] [--access read|write]` and `project repo clear`; `aimem project show --project <p>` prints repository, process pin and grants. `--access` defaults to `write` (branches and pull requests); `read` is for projects members only consult. The process repository stays a separate project property (`process select`, as today); its forge kind adds a read requirement. The credential kind a member needs follows from the repository's `--kind`; there is no separate credential declaration. Cross-repository work is tasks in each project with dependencies between them, as the AIForge work was run so far. Task 01a102f8-bb79 (aimem side).

**2.2 Team creation declares no projects.** `aicrew team create --name <team> --hub <alias>` creates the team in aicrewd and registers it on that hub (name and UUID, no grants) through aicrewd's peer credential. The hub alias is one aicrewd knows from its configuration. Task 01a102f8-bb84 (aicrew) and the register operation in 01a102f8-bb79 (aimem).

**2.3 Project access is the hub's grant, read by aicrewd.** `aimem identity team grant --peer <service> --team-name <team> --project <project>` is the only place a project is attached to a team; `revoke` detaches it. aicrewd keeps no project or repository list of its own: through its peer credential it reads the team's grants and each granted project's repository, process pin and access level (at offer time, at `team show`, in the reconciliation loop, and for the capability check below), and refuses an offer on a project the team is not granted, with a readable code. `-project` disappears from `aicrew team create`.

**2.4 `aicrew team show` and `aicrew team check` are the health check of the whole setup.** `team show --team <team>` prints the team whole: hub, registration state on the hub, granted projects with their repositories, process pins and access levels (read from the hub), members with roles, session state and verified capabilities. `team check --team <team>` walks the chain and reports each link green or red with the fixing command: hub reachable, peer registered and `peer check` passing, team registered, every grant's repository and process pin resolvable, every member's capabilities against the requirements, pending deposits (3.8). `aicrew doctor` does the same for the service itself (configuration, token files, TLS, the aimem block, `/healthz`).

## 3. Credentials, identities, capabilities

**3.1 Every member has its own forge identity and token.** The operator creates it on the forge (GitHub: an account or App per member; Gitea and GitLab: a bot user) and issues the token with the rights the granted projects' repositories need. aicrewd holds no forge authority and mints nothing (a broker through a GitHub App is out of scope and would be its own proposal).

**3.2 `join` provisions every credential kind the team's granted projects require.** The material arrives either as deposits under the invitation (3.8, the normal path: the member needs only the invitation code) or as files on the command line (`--aimem-token-file <file>`, `--cred github=<file>`, each owner-only or `-` for a hidden prompt, the fallback for a machine the operator provisions by hand). `join` copies each kind to `<home>/creds/<kind>.token` and the aimem token into the home's aimem installation. Before provisioning anything, `join` fetches the team's requirements from aicrewd and prints them against what it has ("team aiforge requires github (write), gitea (read); available: github; missing: gitea"); a missing kind is a warning in the report and in `agent.json`, not a stop. `agent.json` lists the kinds the home holds and the forge identity each authenticates as, never a value. Task 01a102e1-354c.

**3.3 `check` verifies each kind against the declarations** by a read-only call to the forge ("who am I") and, per granted project, a read of its repository and, for `write`, the token's push and pull-request rights as the forge reports them. It prints a table: project, repository, forge, required access, verified or missing. Only the member's machine can verify a token, since only it holds the value; aicrewd learns the result through the member's authenticated session (3.6). A home missing a kind is not blocked by that alone (see 3.6); a home missing the aimem credential is.

**3.4 Use is confined.** Clones made from the offer's repository get a per-clone credential helper that reads the member's file, and the member's commit identity derived from its forge identity. The value never appears in a URL, a global git configuration, process arguments, logs or chat.

**3.5 The offer names the repository.** Offer and independent claim carry `repository: {kind, url, default_branch, base_commit, branch}`, taken from the task's project; the coordinator chooses nothing. Task 01a102e1-354c.

**3.6 Capabilities make refusals early.** At `join`, `check` and session start the agent reports its verified capabilities (credential kinds, repositories, access level) to aicrewd, which stores them per member. The coordinator's `/crew-offer` shows which workers can take a task of that project; aicrewd refuses an offer to a worker that lacks the required capability, with a readable code naming the missing kind, before it reaches the inbox; the worker's own check is the last guard at accept. A missing capability never blocks the home or the member's other work. Task 01a102e1-354c.

**3.7 A changed requirement is announced, not discovered.** When a project is granted to a team or a project's repository changes, aicrewd notices in its reconciliation loop (it rereads grants and repositories with the same cadence it reconciles steps) and, for every member whose capabilities fall short, puts a message in that member's inbox naming the project, the missing kind and the command (`aicrew-agent join --home <home> --cred <kind>=<file>`, or the deposit the operator should make), shows the gap in `aicrew team show`, and answers `aicrew team check --team <team>` with the full list of gaps so the operator sees right after a grant who needs which token. Until the member adds the material, only offers that need it are refused, each by name. Task 01a102e1-354c.

**3.8 Deposits: credentials travel with the invitation, once.** The operator attaches the member's credentials to the invitation instead of moving files between machines: `aicrew invitation issue --team <team> --role <role> --expect-user <user> --expires 72h --deposit aimem=<file> --deposit github=<file> --output <code file>`. aicrewd stores each deposit encrypted at rest, bound to that invitation and expiring with it. When `join` redeems the invitation over the authenticated TLS session, it receives the deposits once, writes them into the home (`creds/<kind>.token`, the aimem installation) and aicrewd deletes them; a deposit never leaves aicrewd a second time, is never listed with its value, and the delivery is audited. An expired or revoked invitation takes its deposits with it. aicrewd therefore holds a member's secrets only between `invitation issue` and `join`; long-term storage stays in the member's home. The member needs only the invitation code. Later additions (a new forge after a grant) use a deposit on a `link` invitation for that agent, redeemed by `join --home <home>`. Task 01a102e1-354c.

## 4. Members act without a human message

**4.1 Wake-up.** The launcher, which already holds the inbox, wakes the client when a message arrives; a member never ends a turn waiting. Task 01a0d6d7-1aed (raised to prerequisite).

**4.2 Initial instruction.** `run` hands the client a managed first instruction (`/crew-start`) so the first turn reads the home guidance and the inbox without a typed message; `run -- <client args>` still passes extra arguments; an operator flag disables it for debugging. Task 01a102cc-3a60.

**4.3 Managed slash commands.** `join` writes the role's commands into the home, namespaced `crew-` and checked against the client's built-ins and the user's own skills: `/crew-start`, `/crew-inbox`, `/crew-offer`, `/crew-review`, `/crew-accept`, `/crew-submit`, `/crew-handoff`; generated from the same source as ROLES.md. Task 01a102d2-ef73.

**4.4 The home names its team's projects and hub.** Written by `join` from the hub's grants and aicrewd's team record; shown in START.md and by `session status`. Task 01a102cc-3a4b.

**4.5 Session rule.** Under `run` the launcher holds the session; `session start`, `session leave` and `run` refuse to execute from inside a launched client (the launcher's environment variables are present) and name `aicrew-agent inbox` as the next action; START.md step 5 says so. Task 01a102d0-0d7b.

## 5. Coordinator triage in team mode

A coordinated claim requires the task READY; a team session has no task write. Two options, decision by the operator:

- **(a)** the coordinator role gets task triage authority in team mode through the team profile (state BACKLOG↔READY, priority and size, next_action, comments), with aimem recording the member as the actor;
- **(b)** the offer carries the READY assessment and aimem accepts a coordinated claim on a BACKLOG task when the claim contains the offering coordinator's assessment, moving the task to READY as part of the claim.

In both, aimem answers a claim on a not-READY task with its own code (`task_not_ready`) instead of `reservation_conflict`. Task 01a102d9-440b; the aimem side is filed once the option is chosen. Also: `update_task` replacing the whole field set is a trap for triage; a partial update or a dedicated state transition is part of the aimem increment.

## 6. Readable commands

**6.1 aimem admin commands** (`aimem identity …`, `aimem access …`, `aimem project …`): entity names are named flags (`--peer`, `--team-name`/`--team-id`, `--project`, `--user-name`/`--user-id`, `--operation`); a positional argument only where a command has exactly one entity (`aimem identity peer check <service>` stays); the old positional form kept one release with a notice; team profiles carry `team_name`; output prints names beside IDs; `--help` shows a full example. Task 01a102f8-bb79.

**6.2 aicrew commands** accept names wherever IDs are accepted (`--team pilot`, `--expect-user pilot-worker`), print both, and the documentation uses `--` for both tools.

**6.3 Names.** `service_id` names the deployment's service: `aicrew-service` for this deployment (renamed before the resume). `aicrew introspection-credential` becomes `aicrew hub-credential` (the credential this service issues to a hub), old name kept one release. Task 01a102ee-a2cd.

**6.4 One flag per secret intent, in both tools.** Writing a secret to a new owner-only file is always `--output <file>` (today `-code-file`, `-secret-file`, `-file` in aicrew and `--secret-file` in aimem); the file must not exist, is created readable by its owner only, and the value never goes to the terminal. Reading a secret is always `--token-file <file|->` (`-` for a hidden prompt or stdin); attaching one to an invitation is `--deposit <kind>=<file>`. Old names kept one release with a notice. Covered by 01a102f8-bb79 (aimem) and 01a102ee-a2cd extended to the aicrew client (aicrew).

## 7. Target onboarding, once this ships

Once per project (owner of the data): `aimem project repo set --project <p> --kind github --url <url>` and `aimem process select … --project <p>`. Once per team: `aicrew team create --name <team> --hub <alias>`; `aimem identity team grant --peer <service> --team-name <team> --project <p>` per project; `aicrew team check --team <team>` (for this deployment: a team `aiforge` granted `aicrew`, `aimem` and `aiskills`, all on GitHub, so one GitHub token per member covers them). Per member: a hub user and token, a forge identity and token, then one command that carries both: `aicrew invitation issue --team <team> --role <role> --expect-user <user> --expires 72h --deposit aimem=<f> --deposit github=<f> --output <code file>`; the code goes to the member privately, nothing else does. Member, once: `aicrew-agent join --label <l> --url <aicrewd> --hub <alias> --client claude` with the code at the hidden prompt; `join` receives the deposits, provisions the home and ends `ready` with the capability table. Work: `aicrew-agent run -client claude -home <home>`; the client starts with `/crew-start`; the human merges pull requests and watches `aicrew team show`. What a machine then needs before the code: the installed binaries (aimem, aicrew-agent, the client) and an active account with the model provider for the client; everything else arrives at join.

## 8. Out of scope

Token minting or brokering by aicrewd; a shared project token; several repositories per project and multi-repository offers; several teams per home; OpenCode carriers and commands (1af8); event-driven delivery beyond the launcher's wake-up; a web console (a future client of the operator API); deleting hub profiles when a team is removed.

Later, its own proposal: toolchain requirements (forge CLIs such as gh or tea, LSP servers, MCP servers, skills) declared at the organization level (the process manifest in aiskills, which is the company's development process), at the project level (language, forge), at the role level (coordinator and worker), and verified against the host by `check` with verified installers, as aimem and ai-skills are today. Also later: delivering the model provider's credential to the client from the home.

## 9. Order of delivery

aimem: 2.1 project repository property and `project show`, the team register operation, 6.1 and 6.4 (S together); the `task_not_ready` code and the option-5 increment (S) after the operator's choice. aicrew next release: 6.3 rename, 6.4 flag names and 4.5 session rule (XS each); 2.2 to 2.4 and 3 together (team registration, reading grants and repositories from the hub, deposits, credential provisioning, capabilities, requirement announcements, health check, offer field; M to L, split as the assessment proposes); 4.1 to 4.4 as one group (M); then 5. Before the pilot resumes: rename the service to `aicrew-service`; re-select the process pins of `aicrew`, `aimem` and `aiskills` to the aiskills repository's GitHub URL (the process repository moved from the private Gitea); set each project's repository; `join` rerun per home; `run`; re-offer the pilot task.
