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

The joins added two more:
- the skills step, which a member cannot run with its own credential
  (finding 14);
- the hub administrator's import of the skills set, which the bring-up
  needs and which the document does not name (finding 19, comment seq 803
  on `01a1f297-0ed5`).

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
the work after the set-up (section 8, increment U4).

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

A member's host is reached through a **command prefix** the plan names,
with its kind (`ssh` or `exec`, below), for example `["ssh", "-o", "BatchMode=yes", "-o", "StrictHostKeyChecking=yes", "-o", "UpdateHostKeys=no", "crew-worker@host-b"]`.
- `aicrew up` runs each member-side command as that prefix plus the
  command, with the data on standard input.
- It never opens a connection itself, and holds no SSH key, password or
  host key.
- The operator's own SSH client applies its configuration, agent and
  `known_hosts`. A host key that SSH does not know fails the step: the
  prefix sets `StrictHostKeyChecking=yes`, which refuses a host whose key
  is not already known (OpenSSH's `ssh_config(5)`). `BatchMode=yes` alone
  is not enough, because an operator's configuration with
  `StrictHostKeyChecking=accept-new` would record a new key without asking.
  The prefix also sets `UpdateHostKeys=no`, so no run, a dry run included,
  writes to the operator's `known_hosts`. The operator verifies and adds a
  host's key by their own means, before the run.
- `aicrew up` refuses an SSH prefix (one whose program is `ssh`) that does
  not carry these three options, and names the missing one. The options
  are given on the command line, where they override the operator's
  configuration files.
- Windows 10 and 11 include an OpenSSH client, so the same prefix works from a Windows workstation.
- The prefix logs in as the member's OS user, the account the bring-up
  needs anyway (BRING-UP, "What a member's host needs"). It never uses
  root.

The same prefix lets a test reach a container: the pilot simulation names
`["docker", "exec", "-i", "pilotsim-worker"]` (increment U3).

**How a member-side command crosses the transport.** Every member-side
step is one SCRIPT, a fixed text of `aicrew up` run by `sh -c`. The two
kinds of prefix cross the boundary differently, so the plan names the
kind beside the prefix, and `aicrew up` builds the call for each:
- **`exec`** (for example `docker exec -i NAME`): the program receives
  its arguments as they are. `aicrew up` appends three arguments, `sh`,
  `-c` and SCRIPT, and nothing parses them again.
- **`ssh`**: OpenSSH joins every argument after the destination with
  spaces into one string, which the user's login shell on the host parses
  ([OpenSSH, ssh.c](https://github.com/openssh/openssh-portable/blob/V_9_9_P2/ssh.c#L1179-L1184)).
  Three separate arguments would lose SCRIPT's boundary: `sh -c` would get
  only its first word. So `aicrew up` passes exactly one remote-command
  argument, `sh -c ` followed by SCRIPT quoted as one word. The login
  shell removes that one level of quoting and hands `sh` the whole SCRIPT.
- **Quoting.** Inside SCRIPT every argument is quoted for a POSIX shell,
  in single quotes, with each `'` inside written as `'\''`; for `ssh`,
  SCRIPT itself is quoted again the same way.
- **The login shell.** The `ssh` form relies on a login shell that parses
  POSIX single quotes (sh, bash, dash, zsh). Before any other step,
  `aicrew up` runs a fixed probe through the prefix, a quoted string that
  holds a space, a `'` and a `$`, and requires it back unchanged. A login
  shell that changes it (csh or fish, for example) stops the run and is
  named.
- **The plan's values** are validated before they reach a script: labels
  and the team's name by join's own grammar, URLs as https URLs, host
  names as host names, the forge host as `join --cred` takes it. A value
  that fails stops the run before anything is created, and names the key.
- **Tests.** U1's tests run the same scripts through both kinds, with
  values that hold spaces, quotes and `$`, and require each command to
  receive its arguments exactly.

No value from the plan is ever pasted into a script unquoted. The scripts
are fixed texts of `aicrew up`, with the quoted values as their only
variable part.

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
| A member's forge token | a reference in the plan: an owner-only file, or a hidden prompt | from the start of the member's steps to its `join` | U1: a new owner-only file in an owner-only temporary directory on the member's host, given to `join --cred FORGE_HOST=forge.token` and removed by the same remote command. U2: join's standard input, with no file at all. |
| SSH keys and host keys | the operator's SSH client | never | never |
| The client's login | the person, on the member's host | never | never |

**What none of them is ever in:**
- the output the operator sees, or any output that is captured or kept: the
  terminal, a log, the journal. A private pipe between two processes of
  one step, which is how the enrollment record and the invitation code
  travel, is not output in this sense;
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
   - `stored` means only that a login is stored, not that it works: an
     expired Claude Code login passes, and for `--client opencode` the line
     is `no_check` (internal/agent/preflight.go; BRING-UP step 6). The
     evidence of a working account is the turn-model call of `check` after
     join (step 7). The preflight only stops the run early when the person
     has not logged in at all.
3. **The aimem credential, by enrollment.**
   - aimem v0.11.4 has both halves of PILOT-1-FOLLOWUPS 3.9: the admin's
     `aimem identity enroll issue --purpose new-user --output -` (it takes
     `--user-name`, `--hub` and `--admin-token-file`) and the client's
     `aimem enroll redeem --hub-name NAME --label LABEL`, which reads the
     record on standard input (cmd/aimem/identity_enroll.go and
     cmd/aimem/enroll_redeem.go).
   - `aicrew up` pipes the one into the other through the transport, with
     the home's `AIMEM_STATE_DIR` and `AIMEM_SOCKET` set. The hub creates
     the member's user at redemption, so no user ID is carried by a
     person.
   - **A clean installation needs no provisioning first.** On an
     installation without the hub's entry, redemption creates the entry
     with the record's hub URL and trust, stores the delivered credential
     and saves it (aimem v0.11.4, cmd/aimem/enroll_redeem.go, the delivery
     path). So the enrollment comes before join, and join is given no
     `--aimem-url`.
   - **Only into a home without a credential of its own making.**
     Redemption into an installation that already holds a working
     credential for the hub spends nothing and answers `already_enrolled`
     with that credential's user, whoever it is (the same file, before the
     publish step). A home the run did not make could hold another
     person's credential. So before it issues an enrollment, `aicrew up`
     reads the home: a home that is not linked but whose aimem
     installation already holds a credential for the hub stops the run,
     naming the home, unless that credential is the one this plan's own
     bundle delivered (the match below). The run never redeems a new
     bundle into such a home.
   - **The identity is pinned before the invitation, by a match.** The
     identity the member is linked to is the one the plan's bundle
     created, and it is established in one of two ways:
     - the redemption answers `enrolled` with the bundle ID the run issued
       (`enroll redeem --json` reports the outcome, the user ID, the token
       ID and the bundle ID);
     - or the bundle is `redeemed` on the hub, and the user ID and token ID
       that `aimem identity enroll list` names for it ("redeemed: user ...,
       token ...", cmd/aimem/identity_enroll.go) equal the user and token
       that the home's stored credential authenticates as: `aimem hub
       credential NAME --json` with the home's `AIMEM_STATE_DIR`, which
       reports `user_id` and `token_id` from the hub's live answer
       (cmd/aimem/hub_credential.go). This covers a redemption that saved
       the credential and then answered `already_enrolled` on a retry,
       since aimem saves the credential before its last identity check
       (cmd/aimem/enroll_redeem.go, the delivery path), and a rerun after
       a crash, when the record and its answer are gone.

     Anything else stops the run, a credential of an unrelated user
     included. The invitation is issued only after the match, with
     `--expect-user` set to the matched user ID. aicrewd then accepts a
     proof only from that user, so a credential that changed between the
     two steps cannot link the member to another identity.
4. **The join.** `aicrew-agent join --label L --url ... --tls-trust-mode
   ... --tls-trust-value ... --aimem-hub NAME --client claude --wake turns
   --code-file - --cred FORGE_HOST=forge.token`.
   - The code comes on standard input.
   - The aimem credential is already in the home, so join skips its own
     provisioning (`provision` returns at once when the installation holds
     an active credential: internal/agent/provision.go).
5. **The skills.** The owner decided on 2026-11-19 that a member gets
   them with no personal grant, by the product's own path:
   - `aicrew-agent join` installs them from the hub inside a team
     conversation it opens for that (aicrew task `01a1f3a6-77d3`);
   - `aimem skills install` acts as the team conversation when
     `AIMEM_TEAM_SESSION` is set, and never falls back to the personal
     credential (aimem task `01a1f3a5-e6ed`, expected in aimem v0.11.5).

   join opens that conversation itself, during the join. That matters
   because a join that ends `blocked` installs no service
   (cmd/aicrew-agent/join.go), so on a new home the service could never be
   the one to open it. That sequence is task `01a1f3a6-77d3`'s to settle.

   Both come before U1 (section 8). `aicrew up` runs nothing of its own
   for the skills and imports nothing: the join of step 4 installs them.
   The one thing it adds is the hub administrator's import of the skills
   set (finding 19), which runs only on the hub's host (`aimem skills
   import --from-archive FILE`, its usage at v0.11.4). `aicrew up` cannot
   run it from the workstation and does not try. When a member's join
   reports that the hub has no skills set imported (one of the failures
   aimem's install tells apart, comment seq 804 on `01a1f3a6-77d3`), the
   run stops with that step named for the hub's administrator, and resumes
   after it. Whether the dry run can see a missing set earlier, through a
   read the command already has, is a question for U1's assessment.
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
| 2 | The hub: its alias, its URL and its TLS trust (a CA file or a pin, as aimem's `--hub-ca-file` and `--hub-pin` take them) | the alias: the only hub aicrewd has. The URL and the trust: none | the grant, the enrollment, and the home's hub (finding 2). See below |
| 3 | The team's name and its projects | none | `team setup` |
| 4 | Start the team paused? | yes | finding 5: a coordinator that joins wakes on the board's READY tasks at once. The end table prints the resume command |
| 5 | The hub admin's bearer | a hidden prompt | the grant and the enrollments. Asked once, used, then gone (3.2) |
| 6 | Per member: its label, role, host access (the command prefix of 3.1) and client | first a coordinator, then workers; client `claude` | each member |
| 7 | Per member and forge host: the token's reference | a hidden prompt | `join --cred`. The forge hosts are read from each granted project's repository, which aicrewd's team grants carry (`opapi.TeamGrants`, as `team setup` prints them) |
| 8 | A closed network's export base | none | section 7 |
| 9 | Save the plan? | `crew-up.json`, without the secrets | a repeat, a review or a headless run |

**Where the hub's connection comes from.** The operator API gives the
hub's alias and ID with a team (`opapi.Team`, `hub_id`), but not the hub's
URL or trust: those are in aicrewd's own configuration
(`svcconfig.AimemConfig`), and no operator route returns them. So in U1
they are plan inputs, asked once. Before the admin's bearer is sent
anywhere, the run reaches the hub at that URL under that trust with a
request that carries no credential, and stops if the TLS check fails. The
admin's bearer then goes only to the hub verified this way. An
authenticated operator route that returns the binding's non-secret fields
(URL and trust) would remove the question; it is an aicrewd change, left
to U1's assessment, and until it exists the answer is the operator's.

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

**A redemption that was interrupted.** The hub may have redeemed a
bundle, issuing the member's user and credential, while the member's side
stopped before it saved the credential. aimem's own retry resumes that
delivery only with the original record on standard input, because the
record locates the bundle's pending keys in the home's installation (aimem
v0.11.4, cmd/aimem/enroll_redeem.go). Revoking a redeemed bundle does not
revoke the user and token it issued (`aimem identity enroll` usage). So
the run handles it in two cases:
- **Within one run.** `aicrew up` holds the record in memory until the
  identity is matched (section 4, step 3). On aimem's exits 4 (retryable)
  and 5 (outcome unknown) it sends the same record again, as aimem's usage
  says, up to a bound. A retry that answers `already_enrolled` goes to the
  match by the bundle's redeemed IDs, never straight to a stop. A run that
  ends without a match names the outcome; it never issues a second bundle
  for the member in the same run.
- **After a crash, when the record is gone.** The rerun reads the
  bundle's state on the hub (`aimem identity enroll list`, with the
  bundle ID from the journal), and the home's installation:
  - bundle `issued`: unspent. The rerun revokes it and issues a new one,
    as above.
  - bundle `redeemed`, and the home holds a credential: the rerun
    applies the match of section 4, step 3, by the bundle's redeemed user
    and token IDs. A match is delivered, and the run goes on. A credential
    of another user stops the run and names the home.
  - bundle `redeemed`, and the home holds no credential: spent, with the
    delivery lost. The rerun stops with exit code 5 and names the hub
    administrator's recovery for the user and token that bundle issued,
    which `enroll list` names: `aimem access token-revoke` and `aimem
    access user-set --state disabled`, on the hub's host. Only after that
    does a rerun issue a new bundle. A spent bundle is never reused, and
    the identity it created is never left live and unused.

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

**Before U1: the skills path.** Two tasks that already exist, both on the
goal's path, come first. They are not increments of this command:
- aimem `01a1f3a5-e6ed`: `aimem skills install` acts as the team
  conversation when `AIMEM_TEAM_SESSION` is set, and never falls back.
- aicrew `01a1f3a6-77d3`: join installs the skills inside a team
  conversation it opens. The simulation and the real-aimem harness go the
  documented way: no ai-skills from the archive in the member image, and
  `claude_skills` no longer a passing end of join.

`aicrew up` then calls what they deliver, through join.

| # | Increment | Size | Surfaces | Depends on |
| --- | --- | --- | --- | --- |
| U1 | **`aicrew up` brings a team up.** The questions, the plan and `--save`; `--dry-run`; the journal, the stops and the revocations; the transport; the operator-side steps (team setup, grant, pause, enrollment, invitations); the member-side steps with today's member commands and aimem's enrollment redemption; the end table | M (at its upper end; if U1's assessment finds L, the split is the member-side steps) | operator CLI; credential handling | none |
| U2 | **`join` reads every secret from one standard-input document:** the code, an enrollment record, the forge tokens. U1's temporary token file on the member's host goes away | S | join; credential handling | U1 |
| U3 | **The pilot simulation brings its team up with `aicrew up --plan` headless,** over the `docker exec` prefix, in place of its own member scripts (section 8.1) | M | test harness | U1 |
| U4 | **After the set-up:** a rerun converges to the plan. That covers upgrading the members' tools to the tested versions, replacing a member's forge token, and adding or removing a member. `aicrew up --status` prints section 5.4's table. It wraps `aicrew team pause\|resume` and `aicrew status`, and replaces nothing | M | operator CLI; credential handling | U1 |
| U5 | **With runners:** the member-side step enrols the host's runner, and aicrewd's `ensure-home` does the join | S | operator CLI | DESIGN-CONTROL-PLANE B1 to B3 |

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
| 14 | the skills | done by the join that `aicrew up` runs, once `01a1f3a5-e6ed` and `01a1f3a6-77d3` are delivered. The hub's skills set is the administrator's import on the hub's host; a missing set stops the run with that step named |
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

### 8.1 A rule of the design: the tests bring a team up the way a person does

The pilot simulation and the real-aimem harness bring their teams up
through the same commands a person is given, with no stand-in step. It is
the owner's rule of 2026-11-19 for the skills ("our test has to go the
same way that is offered to the user; if something on the way is wrong, it
is fixed, not bypassed"), and it applies to the command itself:
- Once `aicrew up` exists, the scenarios bring their teams up with
  `aicrew up --plan` and nothing beside it (U3). The transport's prefix is
  an input of the plan, as a host's SSH destination is for a person; it
  is not a step.
- A step that a scenario cannot take the way a person does is fixed in the
  product, or named in the scenario's document as not covered. It is never
  replaced by a step of the scenario's own.
- Until U3, the scenarios keep their own scripts, and each of those
  scripts runs the product's documented commands. Task `01a1f3a6-77d3`
  removes the one stand-in step they have today: ai-skills installed from
  the archive in the member image.

One stand-in stays, and it is not a step of the bring-up: the
simulation's client is a stand-in for Claude Code, since a test does not
call a model provider. The client's login, which only a person can do, is
therefore the one step of section 1 that no scenario takes. The
simulation's member image puts the stand-in on `PATH` as `claude`
(e2e/pilotsim/member/Dockerfile), so no scenario logs a real client in.

## 9. The questions that are the owner's

**Decided, 2026-11-19: a member's skills** (finding 14). A product fix and
no personal grant for a member, also not for the first team: aimem task
`01a1f3a5-e6ed` and aicrew task `01a1f3a6-77d3` (section 8). This was O3 in
the first draft of this document. The numbers of the other questions are
kept.

**Still open:**

- **O1, where it lives.** `aicrew up` in today's `aicrew` command
  (proposed), or a separate tool. The proposal needs no new binary,
  installer or release line.
- **O2, the transport.** The operator's own SSH client through a command
  prefix (proposed). The alternatives are an SSH library inside `aicrew`,
  or waiting for the runner. A library would make `aicrew` read keys and
  host keys itself. Waiting puts the whole bring-up behind the control
  plane's B1 to B3.
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
- Implement the skills path: that is aimem `01a1f3a5-e6ed` and aicrew
  `01a1f3a6-77d3`.
- Decide the size of U1 beyond its assessment.
