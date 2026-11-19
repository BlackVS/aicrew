# One command brings a team up

Status: **design for review** (task `01a1f297-77a0`). Nothing here is
implemented. The owner reads it at the trial's review, and the questions
that are the owner's to decide are gathered in section 9.

**The request.** On 2026-11-19, during the first bring-up of a team on real
hosts, the owner asked for "a universal script or program, so that the
operator runs only it, once, and the wizard asks and does everything". In
the same hour he added three things:
- its only inputs are the accesses to the members' hosts and the tokens a
  person had to make;
- it is not only for the set-up, but is where the operator later upgrades,
  re-issues and watches the team;
- it has a headless mode that reads the same answers from a file.

His own summary of the day: "in substance I am only doing copy and paste;
a utility will handle that better, with fewer chances of mistakes".

**The inputs.**
- The task's comments seq 797 to 799, which record the owner's words above.
- The findings of the first bring-up in task `01a1f297-0ed5` and its
  comment seq 801 (findings 1 to 18 below are numbered as there).
- [BRING-UP.md](BRING-UP.md), the bring-up as the release v0.11.1
  documents it.
- [DESIGN-CONTROL-PLANE.md](DESIGN-CONTROL-PLANE.md), sections 2 (the
  runner), 4 (the operator surface) and 5 (security).
- [DESIGN-OPERATOR-SEAT.md](DESIGN-OPERATOR-SEAT.md).
- [proposals/PILOT-1-FOLLOWUPS.md](proposals/PILOT-1-FOLLOWUPS.md),
  sections 3.8 (forge deposits) and 3.9 (the aimem credential by
  enrollment).

**Sources.** A statement about what the product does today names where it
is read from: a file with its function, or a command's own usage text, at
aicrew v0.11.1 and aimem v0.11.4. A statement that has not been checked
says so.

## 1. What the first bring-up cost, and who it needed

The operator's side of the bring-up of 2026-11-19 had two members, one
hub, one aicrewd and one GitHub repository. The table lists its steps in
the order of BRING-UP.md, with where each ran and whether it needed a
person.

| # | Step | Where | Needs a person? |
| --- | --- | --- | --- |
| 1 | `aicrew team setup` (run twice: before and after the grant) | operator's workstation | no |
| 2 | `aimem identity team grant`, one line per project | the hub's host, as its service user | no |
| 3 | `aimem access user-add`, once per member | the hub's host | no |
| 4 | `aimem access token-issue-user`, once per member | the hub's host | no |
| 5 | a forge account per member, given write access | the forge's web pages | **yes**: an account is a legal identity on the forge |
| 6 | a forge token per member | the forge's web pages | **yes**: only the account's owner may issue it |
| 7 | `aicrew team pause` | operator's workstation | no (a choice, finding 5) |
| 8 | `aicrew invitation issue`, once per member | operator's workstation | no |
| 9 | carry three files to each member host | between machines | no |
| 10 | the three one-liners: aicrew, aimem, the client | each member host | no |
| 11 | the client's login | each member host | **yes**: the provider's login is interactive and belongs to the account holder |
| 12 | `aicrew-agent preflight` | each member host | no |
| 13 | `aicrew-agent join` | each member host | no |
| 14 | `aimem skills install` (refused, finding 14) | each member host | no |
| 15 | `aicrew-agent join --home` again | each member host | no |
| 16 | `loginctl enable-linger`, `service status` | each member host | only where the system's policy needs root |
| 17 | `aicrew-agent check`, `aicrew team show`, `aicrew status` | both places | no |
| 18 | delete the carried files | each member host and the workstation | no |

Providing the hosts themselves comes before all of this and is also a
person's.

Three kinds of step need a person: a forge account with its token, a
client's login, and a host. Everything else in the table is copying.
Eleven values moved by hand between four places:
- two aimem user IDs;
- the hub's ID and its alias in aicrew;
- aicrewd's URL and trust;
- the hub admin's bearer, cut out of the hub's environment file into a
  file (finding 3);
- two aimem tokens;
- two invitation codes;
- two forge tokens.

All three failures of the day came from that copying, not from a decision:
- a printed grant line that lacked two flags (finding 1);
- a shell without `XDG_RUNTIME_DIR` (finding 4);
- a value the document did not say where to find (findings 2 and 3).

The joins added a fourth: the skills step that a member cannot run with its
own credential (finding 14).

## 2. The command

**Proposal:** `aicrew up`, a command of today's `aicrew` operator
command-line tool.
- It runs on the operator's workstation, where the operator credential
  already is.
- It reaches each member's host through a shell access the operator
  already has (section 3).
- It does every step of the table above except 5, 6, 11 and the hosts.

It has one description of the team, the **plan**, given in one of two ways:

| Form | Use |
| --- | --- |
| `aicrew up` | asks the questions of section 5, then shows the plan and runs it after a yes. It can write the plan to a file first (`--save FILE`). |
| `aicrew up --plan FILE` | reads the same answers from the file; with `--dry-run` it prints what would be done and changes nothing. |

A rerun with the same plan **converges**: every step first reads whether
its result is already there, and acts only if it is not. That is also what
makes a stopped run resumable (section 6). The same rerun later carries
the work after the set-up (section 8, increment U5).

**Why not a page of the operator's seat, or aicrewd doing it.**
- aicrewd holds no shell access to any host and mints no credential
  (DESIGN-CONTROL-PLANE 2.4 and 2.5, "aicrewd is not a credential
  courier"). Giving it SSH would make it the one place from which every
  member host can be entered. The design keeps that power where it is
  today: with the operator, on the operator's machine, for the length of
  a run.
- The operator's seat (DESIGN-OPERATOR-SEAT) is where a person answers a
  member's question. A bring-up is the operator's own action and has
  nothing to answer.

**How it relates to the control plane.** The control plane's runner does
the member-side half of this command from aicrewd's side: `ensure-home`
runs `join` with the code, and `report` runs `check` (DESIGN-CONTROL-PLANE
2.4). `aicrew up` is that work done from the operator's machine before a
runner exists, through the operator's own access to the host.
- When runners exist (increments B1 to B3 there), the member-side step of
  `aicrew up` becomes enrolling the host's runner. aicrewd's
  `ensure-home` then does the join.
- The questions, the plan and the operator-side steps stay as they are.

So this is not a wizard beside the control plane. It is the control
plane's first operator command, with the transport the runner later
replaces.

## 3. Where it runs, and every credential it touches

### 3.1 The transport to a member's host

A member's host is reached through a **command prefix** the plan names, for
example `["ssh", "-o", "BatchMode=yes", "crew-worker@host-b"]`.
- `aicrew up` runs each member-side command as that prefix plus the
  command, with the data on standard input.
- It never opens a connection itself, and holds no SSH key, password or
  host key.
- The operator's own SSH client applies its configuration, agent and
  `known_hosts`. A host key that SSH does not know fails the step, because
  `BatchMode=yes` lets SSH ask nothing. The operator adds the key by their
  own means.
- Windows 10 and 11 include an OpenSSH client, so the same prefix works from a Windows workstation.
- The prefix logs in as the member's OS user, the account the bring-up
  needs anyway (BRING-UP, "What a member's host needs"). It never uses
  root.

The same prefix lets a test reach a container: the pilot simulation names
`["docker", "exec", "-i", "pilotsim-worker"]` (increment U3).

Data travels over standard input, never in the remote command line, which
any process on the host can read. That removes finding 7's carriage return
as well, because no shell pipe between two commands touches the bytes.

### 3.2 The credentials

| Credential | Where it comes from | How long the command holds it | How it reaches its use |
| --- | --- | --- | --- |
| The operator credential of aicrewd | its owner-only file, as for every `aicrew` command (`AICREW_OPERATOR_TOKEN_FILE` or `--token-file`) | the run | read by `aicrew` itself, as today |
| The hub admin's bearer | a reference in the plan: an owner-only file, or a hidden prompt | the grant and enrollment steps only | passed to the local `aimem` as `--admin-token-file`, which aimem's `identity team grant` and `identity enroll issue` take. A prompted value is written to a new owner-only file in an owner-only temporary directory and removed when the step ends, since aimem's usage names this credential as a file. |
| A member's aimem enrollment record (replaces the aimem token, section 4) | the standard output of `aimem identity enroll issue --output -`, read through a pipe | from issue to redemption, in memory | the standard input of `aimem enroll redeem` on the member's host |
| A member's invitation code | aicrewd's answer to the invitation request, which `aicrew up` makes itself | from issue to redemption, in memory | the standard input of `aicrew-agent join --code-file -` |
| A member's forge token | a reference in the plan: an owner-only file, or a hidden prompt | from the start of the member's steps to its `join` | U1: a new owner-only file in an owner-only temporary directory on the member's host, given to `join --cred HOST=FILE` and removed by the same remote command. U2: join's standard input, with no file at all. |
| SSH keys and host keys | the operator's SSH client | never | never |
| The client's login | the person, on the member's host | never | never |

**What none of them is ever in:**
- standard output or a log;
- the plan file or the run's journal;
- an argument of any process, local or remote;
- an environment variable.

Each one reaches only the step that uses it. Every refusal or error that
names one names it by its purpose, never by its content, as join's errors
do today (`readToken` in internal/agent/provision.go).

**What `aicrew up` adds to the operator's machine:** a non-secret journal
per plan (section 6). Nothing else is kept.

**What it may never do:**
- copy the hub admin's bearer anywhere but the owner-only file aimem
  reads;
- pass any credential to aicrewd except what aicrewd already takes, which
  is the operator credential;
- keep an invitation code or an enrollment record across runs. A stopped
  run revokes them instead and issues new ones (section 6).

## 4. What a member's host receives

**Today** (BRING-UP steps 2 to 6): three files carried by the person, an
invitation code, an aimem token and a forge token, and a `join` line of
four lines with nine flags.

**With `aicrew up`:** nothing that the person carries. Over the transport,
in order:
1. **The tools.** The three one-liners of BRING-UP step 5, with the
   versions the release tests (`internal/agent/supported.json`). In a
   closed network the plan names the export base, and the one-liners run
   with `AICREW_BASE` and `AIMEM_BASE` (section 7).
2. **The preflight.** `aicrew-agent preflight` with aicrewd's URL and
   trust, which `aicrew up` knows from its own connection.
   - A `login (claude): none` stops the run here, before any invitation
     or enrollment is issued for this member (section 6).
   - This preflight is the client check of PILOT-1-FOLLOWUPS section 7,
     "an active account with the model provider".
3. **The aimem credential, by enrollment.**
   - aimem v0.11.4 has both halves of PILOT-1-FOLLOWUPS 3.9: the admin's
     `aimem identity enroll issue --purpose new-user --output -` (it takes
     `--user-name`, `--hub` and `--admin-token-file`) and the client's
     `aimem enroll redeem --hub-name NAME --label LABEL`, which reads the
     record on standard input (cmd/aimem/identity_enroll.go and
     cmd/aimem/enroll_redeem.go).
   - `aicrew up` pipes the one into the other through the transport, with
     the home's `AIMEM_STATE_DIR` and `AIMEM_SOCKET` set. The hub creates
     the member's user at redemption, so no user ID is carried and the
     invitation needs no `--expect-user`.
   - **Not yet checked:** whether `enroll redeem` writes the hub entry of
     an installation that has none, or needs `join`'s provisioning first.
     U1's assessment decides the order of the two steps by trying it.
4. **The join.** `aicrew-agent join --label L --url ... --tls-trust-mode
   ... --tls-trust-value ... --aimem-hub NAME --client claude --wake turns
   --code-file - --cred HOST=FILE`.
   - The code comes on standard input.
   - The aimem credential is already in the home, so join skips its own
     provisioning (`provision` returns at once when the installation holds
     an active credential: internal/agent/provision.go).
5. **The skills,** by the path the owner chooses (section 9, O3).
6. **The service.** Lingering on, if the user may set it (otherwise the
   run stops and names the root command, section 6), then `aicrew-agent
   service status`.
7. **The check.** `aicrew-agent check --home HOME --json`, read by `aicrew
   up` for the end table (section 5.3).

So the member's side becomes no line at all for the person: the only thing
done by hand on a member's host is the client's login.

**PILOT-1-FOLLOWUPS 3.8 and 3.9, what exists and what this changes.**
- **The envelope** (3.9, one code that carries both the enrollment record
  and the invitation code) does not exist in aicrew: `aicrew invitation
  issue` has no `--bundle` (its usage at v0.11.1). With a transport, the
  envelope is not needed: `aicrew up` keeps the two codes in memory and
  delivers each to its own command. The envelope stays the right form for
  a host the operator cannot reach, where a person types one code. It is
  a later increment, not this design's.
- **Forge deposits** (3.8, the token held encrypted by aicrewd until the
  member collects it) do not exist either. With a transport, the token
  goes from the operator's machine straight to the member's join, and
  aicrewd never holds it. That is stronger than custody on aicrewd: no
  deposit key, no pending deposits to audit, and nothing on aicrewd to
  steal. The design proposes that deposits wait for the runner, the case
  where the operator has no shell on the host (section 9, O6).

## 5. The questions, the plan and what it prints

### 5.1 The questions, in order

Each question shows its default; Enter accepts it. A default that comes
from what the product already knows is read, never guessed.

| # | Question | Default | Why the run needs it |
| --- | --- | --- | --- |
| 1 | aicrewd's URL, trust and operator credential | the `AICREW_*` variables, if set | every operator step. The answer is checked at once with `aicrew status` |
| 2 | The hub | the only hub aicrewd has, by its alias | the grant, the enrollment, and the home's hub (finding 2). The hub's ID and URL are read from aicrewd's hub binding |
| 3 | The team's name and its projects | none | `team setup` |
| 4 | Start the team paused? | yes | finding 5: a coordinator that joins wakes on the board's READY tasks at once. The end table prints the resume command |
| 5 | The hub admin's bearer | a hidden prompt | the grant and the enrollments. Asked once, used, then gone (3.2) |
| 6 | Per member: its label, role, host access (the command prefix of 3.1) and client | first a coordinator, then workers; client `claude` | each member |
| 7 | Per member and forge host: the token's reference | a hidden prompt | `join --cred`. The forge hosts are read from each granted project's repository, which aicrewd's team grants carry (`opapi.TeamGrants`, as `team setup` prints them) |
| 8 | A closed network's export base | none | section 7 |
| 9 | Save the plan? | `crew-up.json`, without the secrets | a repeat, a review or a headless run |

### 5.2 The plan file

A JSON document, the format aicrew already reads for `agent.json` and
`aicrewd.json`, so no new dependency comes with it.
- It names hosts, users, roles, the team, its projects and, for each
  secret, a reference: `{"file": "PATH"}` (owner-only, checked as `join`
  checks `--code-file`) or `{"prompt": true}`.
- It never holds a value. A secret's place takes only one of the two
  reference objects; `aicrew up --plan` refuses anything else there and
  names the key.
- An example belongs in DEVELOPMENT with the first increment, so the
  docs' example check covers it.

### 5.3 The dry run

`aicrew up --plan FILE --dry-run` reads everything it may read (aicrewd,
the hub through aicrewd, each host's versions and preflight) and changes
nothing. It prints one line per step:

```text
team crew          exists, granted aicrew; paused       nothing to do
member coordinator host-a  aicrew v0.11.1, aimem v0.11.4, claude 2.1.286   login stored
                   would issue: enrollment (user crew-coordinator), invitation (role coordinator, 24h)
                   would join with: github.com token from file ./coordinator.github (account checked at run)
member worker      host-b  aicrew missing                   would install aicrew v0.11.1
                   login: cannot tell before the client is installed
```

A dry run spends no invitation, enrollment or model call. Each line names
the credential that step would use, by purpose, and which rights it gives:
the role, the projects and the forge account.

### 5.4 What it prints at the end

The check that the bring-up reached its outcome is the table of BRING-UP
step 8, read from `aicrew-agent check --json` on each host and from
aicrewd:

```text
team crew  paused  projects: aicrew (granted)
label        host    versions               linked  forge                  turn model  service  check
coordinator  host-a  0.11.1 / 0.11.4 / ok   yes     github.com verified    available   running  ready
worker       host-b  0.11.1 / 0.11.4 / ok   yes     github.com verified    available   running  ready
to start work: aicrew team resume --team-name crew
```

It exits 0 only when every member's check is `ready`.

## 6. A run that stops, and a run that resumes

**The journal.** `aicrew up` writes a non-secret journal per plan, beside
the plan as `crew-up.json.journal`. For each step it records its name,
outcome and time, and the IDs it created: the team, each agent, each
invitation, each enrollment bundle. The journal is for the reader and for
the revocations below. Whether a step is done is decided by reading the
system, never by the journal: a rerun reads aicrewd, the hub and each host
again.

**Where a run stops for a person.** It stops with exit code 5, the step
named, and the exact command for the person:
- **No client login:** "on host-b, as crew-worker, run `claude` and log
  in". In a headless run, the same stop and the same exit code.
- **Lingering refused by the system's policy:** the `loginctl
  enable-linger crew-worker` line a root user runs.
- **A forge token that the forge refuses, or that lacks the access a
  project needs.** The pre-check is the read-only "who am I" and
  repository read that `join` and `check` already make. U1 makes them
  from the operator's machine, before any invitation is issued for that
  member (section 9, O4).

**What a stop leaves behind.**
- An invitation or an enrollment issued in the run and not redeemed is
  revoked before the run exits: `aicrew invitation revoke` and `aimem
  identity enroll revoke --bundle-id`. A stop never leaves a live
  one-time code.
- A crashed run cannot revoke them. Its rerun finds them in the journal,
  revokes them, and issues new ones. In the worst case they expire by
  their own lifetime, 24 hours by default.

**Resume** is rerunning the same plan. The steps that are done read as
done, and the first step that is not runs.

## 7. Both forges and a closed network

- **GitHub and Gitea** differ only in the token the person makes (step 6
  of section 1). For each forge host, the plan names the host as `join
  --cred` names it: `github.com`, or the Gitea host with its port.
  - The rights each token needs are BRING-UP's "The forge token's
    rights".
  - The account findings 8 to 11 belong to the pre-check of section 6:
    a fine-grained GitHub token on a repository the account only
    collaborates on, and a coordinator token that cannot write. The
    pre-check names the right a token lacks where the forge shows it
    (BRING-UP, "What `check` reads of the token"). Where it cannot tell,
    which is a fine-grained GitHub token, the end table says so.
- **A closed network.**
  - The plan's `export_base` is an aiforge export that the member hosts
    can read: a directory on each host, or an https server. The tool
    installs then run with `AICREW_BASE` and `AIMEM_BASE`, as their
    installers document (DEVELOPMENT, "Installing in a closed network").
  - With a directory, `aicrew up` can copy the export to each host over
    the transport first: tar through standard input, then each file
    checked against the export's `SHA256SUMS` on the host.
  - The client's npm package comes from the network's own registry
    mirror, which the plan names. That is the one input a closed network
    adds.

## 8. Increments

Each increment is one task, no larger than M. The surfaces are those of
AGENTS.md "Delivery"; "credential handling" marks an increment that moves
a secret, which takes the sensitive gates.

| # | Increment | Size | Surfaces | Depends on |
| --- | --- | --- | --- | --- |
| U1 | **`aicrew up` brings a team up.** The questions, the plan and `--save`; `--dry-run`; the journal, the stops and the revocations; the transport; the operator-side steps (team setup, grant, pause, enrollment, invitations); the member-side steps with today's member commands and aimem's enrollment redemption; the end table | M (at its upper end; if U1's assessment finds L, the split is the member-side steps) | operator CLI; credential handling | none |
| U2 | **`join` reads every secret from one standard-input document:** the code, an enrollment record, the forge tokens. U1's temporary token file on the member's host goes away | S | join; credential handling | U1 |
| U3 | **The pilot simulation brings its team up with `aicrew up --plan` headless,** over the `docker exec` prefix, in place of its own member scripts. The documented path becomes the tested one, which the rule of a scenario per documented path asks for and finding 14 showed missing | M | test harness | U1, and O3's skills path |
| U4 | **The skills by the path of O3:** either a read grant the bring-up gives each member's user, or `aimem skills install` inside a team session (an aimem change: processctx reads the team session's credential, as `aimem reservation` does; and an aicrew change: serve installs the missing skills when its session opens, before the first turn) | S (grant) or M across both repositories (team session) | aimem authentication for the second; join and serve | U1 |
| U5 | **After the set-up:** a rerun converges to the plan. That covers upgrading the members' tools to the tested versions, replacing a member's forge token, and adding or removing a member. `aicrew up --status` prints section 5.4's table. It wraps `aicrew team pause\|resume` and `aicrew status`, and replaces nothing | M | operator CLI; credential handling | U1 |
| U6 | **With runners:** the member-side step enrols the host's runner, and aicrewd's `ensure-home` does the join | S | operator CLI | DESIGN-CONTROL-PLANE B1 to B3 |

**What U1 removes from the operator's list of 2026-11-19,** step by step
(the numbers of section 1's table):

| # | Step | After U1 |
| --- | --- | --- |
| 1 | `team setup`, twice | done by `aicrew up` |
| 2 | the grant on the hub's host, with its missing flags and the `XDG_RUNTIME_DIR` shell | done by `aicrew up` from the workstation, through aimem's `--hub` and `--admin-token-file`. No shell on the hub's host is needed |
| 3, 4 | user and token per member on the hub's host | replaced by the enrollment: the hub creates the user at redemption |
| 5, 6 | forge accounts and tokens | still the person's; given once, as a file or at a prompt |
| 7 | pause | question 4, default yes |
| 8 | invitations | done by `aicrew up`. The codes are never shown |
| 9 | carrying three files to each host | gone |
| 10 | the one-liners on each host | done by `aicrew up` |
| 11 | the client's login | still the person's. The run stops before spending anything, and resumes |
| 12, 13, 15 | preflight, join, join again | done by `aicrew up` |
| 14 | the skills | done by `aicrew up` once O3 is decided and U4 is delivered. Until then the run stops at this step and names it |
| 16 | lingering and the service | done by `aicrew up`, unless the system's policy needs root; then a stop names the command |
| 17 | the checks | the end table |
| 18 | deleting the carried files | nothing was carried |

Of the eleven values carried by hand, nine are no longer seen by a person:
the user IDs, the hub's ID and alias, aicrewd's URL and trust, the aimem
tokens and the invitation codes. The hub admin's bearer and the two forge
tokens are given once, by reference or at a prompt, and never moved by
hand.

**How U1 is tested before U3.**
- Unit tests run the steps against a fake transport, a stand-in aicrewd
  and a stand-in aimem.
- One end-to-end run uses the real-aimem harness's hub and aicrewd, with
  one member reached by the local shell as its prefix.

## 9. The questions that are the owner's

- **O1, where it lives.** `aicrew up` in today's `aicrew` command
  (proposed), or a separate tool. The proposal needs no new binary,
  installer or release line.
- **O2, the transport.** The operator's own SSH client through a command
  prefix (proposed). The alternatives are an SSH library inside `aicrew`,
  or waiting for the runner. A library would make `aicrew` read keys and
  host keys itself. Waiting puts the whole bring-up behind the control
  plane's B1 to B3.
- **O3, a member's skills** (finding 14). Two choices:
  - (a) Each member's aimem user gets a read grant on the team's
    projects. It works with the released versions, but contradicts
    aimem's pilot runbook, where a team member needs no personal grant.
    It also needs an admin route that works from the workstation: `aimem
    access grant add` runs only on the hub's host (its usage at v0.11.4).
  - (b) The skills are installed inside a team session, as U4 describes:
    no personal grant, but a change in two repositories.

  The design proposes (b), and (a) as the first team's interim if the
  owner wants the team running before U4.
- **O4, the forge token's pre-check from the operator's machine.**
  Proposed: the token is used once, read-only, on the operator's machine
  before the invitation is spent. This adds one place that uses the token
  briefly. The alternative is to find out only at join on the member's
  host, after the invitation is redeemed.
- **O5, start paused.** Proposed default: yes (finding 5).
- **O6, forge deposits through aicrewd** (PILOT-1-FOLLOWUPS 3.8).
  Proposed: not for hosts the operator reaches. Deposits wait for hosts
  reached only through a runner.

## 10. What this design does not do

- Provision hosts, or create forge accounts or tokens for a person (the
  task's non-goals).
- Give aicrewd any access to a host or any credential beyond those it
  holds today.
- Replace BRING-UP.md. The document stays the description of what the
  command does, and of the way by hand where the command cannot reach a
  host.
- Decide the skills path (O3) or the size of U1 beyond its assessment.
