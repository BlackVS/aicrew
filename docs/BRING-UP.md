# Bringing up a team on separate hosts

This page is for the operator who brings up a team's members, a
coordinator and a worker, each on a host of its own. It has two lists:
what each member's host needs, and the bring-up in order, from an empty
host to a member whose service runs. Each line names the command that
checks it and where in this repository the requirement comes from.

aicrewd, the aimem hub and the forge are taken as running: aicrewd is
installed with `install-aicrewd.sh` ([DEVELOPMENT.md](DEVELOPMENT.md),
"Installing and upgrading on a hub host"), and the hub is bound to it with
`aicrew hub add`. Member hosts are Linux. A Windows member is not covered
here.

The commands are the product's. Where a step has no product command, this
page says so. Such gaps are listed at the end, under "Gaps".

The steps were run on real hosts on 2026-11-19 and 2026-11-20: a hub with
aicrewd beside it, two member hosts and a GitHub repository, with aicrew
v0.11.1 and then v0.11.2. What that run showed is marked "seen" below;
what is read from code or from other documentation only is marked as
such. "How far this was tried" at the end lists both.

## Who does what

Only a person can:
- provide each host and its OS user;
- log the client in on each host;
- grant the team its projects on the hub, import the skills set there,
  and create each member's aimem user and issue its token there (the
  hub's admin);
- create the member's forge account and issue its token;
- issue each invitation with the operator credential.

The architect can do the rest, once the operator gives it a shell as the
member's OS user on each host: the installs, `join` with the inputs the
operator placed, `check`, and the service. The architect's own credential
cannot issue invitations. It reads escalations, the merges list and the
team states only ([DEVELOPMENT.md](DEVELOPMENT.md), "The architect
directory").

## What a member's host needs

The coordinator's host and the worker's host need the same things, except
the project's build tools, which only the worker's host needs. The role is
set by the invitation, not by the host.

| Need | Check | Source |
| --- | --- | --- |
| Linux on amd64 or arm64 | `uname -sm` | `boot.sh` refuses any other system or architecture |
| One OS user per member, not root, with a home directory | `id` | `e2e/pilotsim/member/Dockerfile` (`useradd -m`); `boot.sh` installs into `~/.local/bin` |
| `curl`, `ca-certificates`, `git`, `jq`, `python3` | `command -v curl git jq python3` | the member image's package list; `boot.sh` needs curl, aimem's installer needs jq, and git is used for each worktree |
| On a worker's host, the project's build tools: for aicrew, Go at go.mod's `go` version or later, and git | `go version` and `git --version`, before join; after join, the worker's `aicrew-agent check --home HOME` <!-- run: check --> has no `toolchain_tool_missing` block | the toolchain the operator selects in step 1 runs them in the worker's turns, and `check` looks for its first tool on its PATH. The worker's service runs with the PATH of the shell that installed or last restarted it, so install the tools before join (step 5). Seen on the first team's worker host on 2026-11-20: Go built aicrew only with `CGO_ENABLED=0`, since the host had a C compiler without the C library's headers; and three of aicrew's tests of file modes failed under the host's umask, on the unchanged base too |
| Node.js with npm, for the client | `npm --version` | `aicrew-agent check`'s instruction for a missing client is `npm install -g @anthropic-ai/claude-code@<tested>` |
| A systemd user manager, with lingering on | `systemctl --user status` and `loginctl show-user "$USER" --property=Linger` | `aicrew-agent service` (a systemd user unit), and the note it prints when lingering is off |
| About 0.5 GB of disk for the tools | `df -h ~` | the try below: 34 MB for aicrew and aimem, 456 MB for Claude Code; worktrees come on top |
| 512 MB of memory for the member, plus the client's own; on a worker's host, 2 GiB is recommended for the project's tests | `free -m` | `e2e/pilotsim/compose.yaml` (`mem_limit: 512m` per member, with a stand-in client). Seen on 2026-11-20: the first team's worker host had 1 GiB, and its `go test ./...` of aicrew ran with swap in use |
| Reaches aicrewd | `aicrew-agent preflight` with join's `--url` and trust, before join (step 6) | join's own client to aicrewd, on its unauthenticated `/healthz` route |
| Reaches the aimem hub with the member's credential | `aicrew-agent check --home HOME`, after join: no `credential_missing` or `credential_refused` block, and no notice that the hub could not confirm the home's credential | `check` asks the hub about the credential in the home's own aimem installation, never the OS user's |
| Reaches the forge | `aicrew-agent check --home HOME`, its forge table | `check`'s read-only "who am I" per credential |
| The client, logged in | before join, the preflight's `login (claude):` line (step 6) shows a login is stored; after join, `aicrew-agent check --home HOME`, its `turn model:` line, shows it works | `claude auth status`; `check` in a `turns` home (one minimal model call, about $0.002) |

Two notes on these:
- **A container is not enough by itself.** A container usually has no
  systemd user manager. In one, `aicrew-agent service install` cannot run
  the member, so something else must restart `aicrew-agent serve` after a
  failure ([DEVELOPMENT.md](DEVELOPMENT.md), "Starting a member"). There
  is no product command for that, so a virtual machine is the simpler
  host.
- **The client's login.** No product command checks that the login is
  valid without a model call. Claude Code's `claude auth status` reads the
  stored login only: in 2.1.286 it reported a made-up login, an expired
  one and one on a host without a network all as logged in. So the
  preflight says only that a login is stored. OpenCode was not observed,
  and the preflight makes no claim about its login. The turn-model call in
  `check` is the check that shows the login works, and `unknown` there
  means a refused login, a limit or another failure.

## Before step 1: the operator's two connections

The operator's steps run two kinds of command, and each needs its
connection set up first.

**Where the operator's `aicrew` commands run.** The operator is a person
at their own machine. Their `aicrew` commands run in one of two places, and
the difference is where the operator credential lies:
1. **On aicrewd's host, reached by SSH.** This is what `install-aicrewd.sh`
   leaves: it installs `aicrew` beside aicrewd in the service user's
   `~/aicrew/bin` (the user is `sessiond` unless `AICREW_HUB_USER` named
   another) and writes the operator credential to that user's
   `~/aicrew/etc/operator.token`, owner-only. The operator logs in to the
   host by SSH, becomes the service user (`sudo -iu sessiond`), and runs
   the commands there. The credential never leaves that host. An AI
   session on the operator's machine reaches it only through a shell on
   that host as that user, so no such session needs one: an architect
   session gets its own, narrower credential (DEVELOPMENT.md, "The
   architect directory").
2. **On the operator's own machine.** The operator copies
   `operator.token` to their machine, owner-only, and installs `aicrew`
   there with the member one-liner (step 5). The commands are the same.
   The credential then lies on that machine too, where every process of
   their OS user, an AI session they start included, can read it.

The rest of this page is the same for both. `aicrew` reaches aicrewd
through four values, as flags on each command or, once, as variables of
the shell:

```sh
export AICREW_URL=https://AICREWD:PORT
export AICREW_TLS_TRUST_MODE=ca_dns            # or spki_sha256
export AICREW_TLS_TRUST_VALUE=AICREWD          # the host name, or sha256- and the pin
export AICREW_OPERATOR_TOKEN_FILE=operator.token
```

On aicrewd's host, `AICREW_OPERATOR_TOKEN_FILE` is
`~/aicrew/etc/operator.token`. The flags are `--url`,
`--tls-trust-mode`, `--tls-trust-value` and `--token-file`. The commands
of this page show neither: they assume the variables.
Check: `aicrew status` <!-- run: status --> answers.

**aimem's admin commands run on the hub's host, as the hub's service
user.** They reach the hub in two ways:
- `aimem access` and `aimem skills` (steps 1 and 2) go to the hub's local
  service over its socket;
- `aimem identity` (step 1's grant) goes to the hub's HTTPS listener,
  named by `--hub`, with the hub admin's bearer.

So a socket error concerns the first kind, and a refused grant the
second's URL or bearer.
- **The socket.** aimem finds it under `XDG_RUNTIME_DIR`. A shell opened
  with `su - USER` has no `XDG_RUNTIME_DIR`, and `aimem access` and
  `aimem skills` then stop with "is the local aimem service running?"
  although the service runs (seen on 2026-11-19). Set it first:

  ```sh
  export XDG_RUNTIME_DIR=/run/user/$(id -u)
  ```

  Where aimem looks is read from aimem's code (`AIMEM_SOCKET`, then the
  state root when `AIMEM_STATE_DIR` is set, then `XDG_RUNTIME_DIR`); the
  line above was not tried.
- **The hub admin's bearer.** `aimem identity` takes it as a file only
  its owner can read. On the hub's host it is the value of
  `AIMEM_HTTP_TOKEN` in the service user's `~/.config/aimem/env`. Write it
  to a new file for step 1's grants, and remove the file after them:

  ```sh
  ADMIN_TOKEN_FILE=$(mktemp)
  sed -n 's/^AIMEM_HTTP_TOKEN=//p' ~/.config/aimem/env > "$ADMIN_TOKEN_FILE"
  ```

  `mktemp` creates a new file that only its owner can read, so the bearer
  never lands in an existing file with wider rights. The `sed` line is the
  one aimem's own hub installer reads the value with. The bearer is the
  hub's admin credential: it stays on the hub's host.

## The bring-up, in order

For each member, the operator's steps come first and the member host's
after. Each step ends with its check. Throughout, `crew` is the team's
name, `main` the hub's alias in aicrew, and `PROJECT` the project the
team works on.

### 1. The team on its hub (operator and the hub's admin, once)

<!-- run: team-setup -->
```sh
aicrew team setup crew --hub main --project PROJECT
```

A new team has no project yet. `team setup` creates or registers it, then
prints `not granted yet; on the hub's host, as its admin, run:` followed by
one line per project, and exits 3:

<!-- run: hub-grant -->
```text
  aimem identity team grant --peer SERVICE --team-name crew --project PROJECT --hub HUB_URL --admin-token-file ADMIN_TOKEN_FILE
```

The hub's admin runs each line on the hub's host, as "Before step 1"
says, with two values filled in. aicrewd does not tell `aicrew` the hub's
address, so the line cannot:
- `HUB_URL` is the hub's TLS listener, `https://HOST:PORT`: the
  `--base-url` that `aicrew hub add` was given.
- `ADMIN_TOKEN_FILE` is the file with the hub admin's bearer ("Before
  step 1"): `"$ADMIN_TOKEN_FILE"` in that shell. After the grants, remove
  it: `rm -f "$ADMIN_TOKEN_FILE"`.
- A hub on a private CA also takes `--hub-ca-file FILE` <!-- run-add: hub-grant --> or
  `--hub-pin sha256-PIN`.

Without `--hub` the command stops with "--hub must be the hub's TLS
listener" (seen on 2026-11-19, when the printed line had neither flag).
The operator then runs `aicrew team setup` again with the same
arguments.

Check: the rerun exits 0 and prints
`every project is granted to team crew`. A `granted:` project followed by
`has no repository on the hub` or `has no selected process on the hub`
still needs that done on the hub first; rerun after it until neither line
appears.

**The toolchain of each project.** A worker's turn may run the project's
build and test commands only when the operator has selected a toolchain
for the project. Without one, its turns deny `go test` and the like, and
the worker cannot test its change. For aicrew, and any project built with
Go:

<!-- run: team-toolchain -->
```sh
aicrew team toolchain --team-name crew --project PROJECT --toolchain dev-go
```

`dev-go` admits Go's test, vet, build and module commands, `gofmt` and
the repository's `scripts/*.sh` (`internal/toolchain`). A worker's launcher
records the team's projects when it starts, so select the toolchain before
the worker joins. A selection made later takes effect when the worker's
service restarts: `aicrew-agent service restart --home HOME` <!-- run: service-restart --> on the
worker's host. The restarted service records the team's projects after
its model probe and its session's start, and only then writes
`msg="the turn loop runs"` to `HOME/logs/serve.log`. A check run before
that line may still print what it printed before the restart: run it
again once the line is there. First see that the warning names the
project just selected: another project of the team keeps its own warning
until it has a selection. A warning for that project that persists
after the line was not a check run too early: the service could not
record the team, and `HOME/logs/serve.log` then holds `msg="the team's
projects were not recorded"` with the cause in its `error=` field.
Restart the service again once that cause is gone.

Check: `aicrew team show --team-name crew` <!-- run: team-show --> names `dev-go` for the project
under `toolchains`. After step 6, the worker's `aicrew-agent check --home HOME`
says `ready` with no warning that names the project just selected. A team
with several projects keeps a warning for each project without a
selection. Such a warning names the operator's command with the team's ID,
`--team ID`: it names the same team as `--team-name crew`. A worker's check
reports the toolchain from aicrew 0.11.3 on, the release step 5 installs.

The hub also needs the skills set that the project's process pins: the
members get their skills from the hub in step 6. The hub's admin imports
the ai-skills release on the hub's host, from its release archive and the
SHA256SUMS beside it:

<!-- run: skills-import -->
```sh
aimem skills import --from-archive aiskills-1.43.0.tar.gz
```

The archive and SHA256SUMS are the assets of that ai-skills release, or
the same files from an aiforge export in a closed network. Check:
`aimem skills list --hub` <!-- run: skills-list --> on the hub's host names the release set.

**A new team works as soon as a member joins.** A coordinator that joins
wakes on the READY tasks of the team's projects. To bring the team up
first and start its work later, pause it now:

<!-- run: team-pause-bring-up -->
```sh
aicrew team pause --team-name crew --reason "bring-up"
```

and resume it in step 8. A pause holds the members' turns, not their
joins: the first team's members joined it paused on 2026-11-19, and ended
`ready` in it on 2026-11-20 (seen).

### 2. The member's aimem user and token (operator, on the hub)

On the hub's host, with aimem's admin commands ("Before step 1"):

<!-- run: user-add member-token per=worker -->
```sh
aimem access user-add --user-name crew-worker
aimem access token-issue-user --user-name crew-worker --label crew-worker --expires +90d --output member.token
```

`--expires` takes a time (`2027-03-01T00:00:00Z`) or a span from now
(`+90d`). The member's aimem user gets no grant of its own: the hub serves it under
the team's grant (step 6, "The skills").

Check: `user-add` prints the user's ID, which step 4's `--expect-user`
needs. `aimem access list` <!-- run: access-list --> shows the users and their live tokens; there is
no separate user list. The token file is owner-only.

**Carrying a file to the member's host.** Steps 2 to 4 each make a file
the member's host needs: this token, the forge token and the invitation
code. Copy each as it is, for example with `scp` to the member's user, and
make it owner-only there (`chmod 600 FILE`). A text pipe can change the
bytes: in PowerShell, piping one command's output into another adds a
carriage return, and the token is then refused (seen on 2026-11-19). The
aimem and forge tokens can also be typed at join's hidden prompts instead
(step 6).

### 3. The member's forge account and token (operator, on the forge)

Each member has its own forge account and token
([proposals/PILOT-1-FOLLOWUPS.md](proposals/PILOT-1-FOLLOWUPS.md), 3.1). The
account needs push access to the team's repositories, since `check` reads
that permission for each granted project.
- **GitHub:** a user account for the member. Add it as a collaborator
  with write access to each repository, and issue a **classic** personal
  access token for it with the `repo` scope. A fine-grained token cannot
  reach a repository its account only collaborates on: it works only on
  repositories of the account that owns the token, or of an organization
  that admits fine-grained tokens (GitHub's documentation on personal
  access tokens; met by the first pilot on 2026-10-11). The first team's
  members used classic tokens: join reported them `verified` on
  2026-11-19, and `check` on 2026-11-20.
- **Gitea:** a user for the member, given write access to each repository
  (as a collaborator, or through a team with write). Issue an access token
  for it, under the user's settings, Applications.

The token needs the rights listed under "The forge token's rights" below,
and it must write, for the coordinator too: its review comment, the
external-review label and marking a pull request ready go through
`aicrew-agent forge pr` with its own token.

**A token that cannot write is not refused at join.** join asks the forge
only who the token is. `check` then reads what it may do ("What `check`
reads of the token"): on Gitea and with a classic GitHub token it names a
missing write right, and with a fine-grained GitHub token it says
`not_checked`. So check `check`'s project table after join, not only join's
report.

**One forge account for two members** works: aicrew does not compare the
members' forge accounts. On a free GitHub plan a single owner may have only
one machine account, and the first team's two members shared one. Each
member still has its own token, and the automations of a pull request run
with the token of the member whose attempt it belongs to.

Check: none until step 6, where `join --cred` asks the forge who the token
authenticates as before writing it, and then `check`, which reads the
token's own rights where the forge shows them ("What `check` reads of the
token" below).

### 4. The invitation (operator)

<!-- run: invitation-issue per=worker -->
```sh
aicrew invitation issue --team-name crew --role worker --hub HUB_ID --label worker --expect-user USER_ID --output invite.code
```

Use `--role coordinator` and another `--label` for the coordinator. The
code is written only to `--output` and is never an argument later. Give it
privately to whoever runs `join`. It lives 24 hours by default and at most
72 (`--expires 72h`); a bring-up spread over days issues it last, or again.

Check: `aicrew invitation list --team-name crew` <!-- run: invitation-list --> shows its state.

### 5. The tools (member host, as the member's user)

```sh
curl -fsSL https://raw.githubusercontent.com/BlackVS/aicrew/v0.11.3/boot.sh | bash
cd "$(mktemp -d)" && curl -fsSL https://raw.githubusercontent.com/BlackVS/aimem/v0.11.5/boot.sh | AIMEM_VERSION=v0.11.5 bash
npm install -g @anthropic-ai/claude-code@2.1.286
```

- aimem's tag is the release this aicrew is tested with, from
  `internal/agent/supported.json`. `aicrew-agent check` prints the same
  line when aimem is missing.
- aimem's boot script wires the directory it runs in as a project, so it
  runs in an empty one.
- npm needs a prefix the user may write to, for example
  `npm config set prefix ~/.npm-global` with its `bin` on `PATH`.
- Claude Code 2.1.286 declares Node.js 22 or later. Debian 13 ships
  Node.js 20.19: npm warns at the install, and the client works. Ubuntu
  26.04 ships Node.js 22, without the warning (both seen on 2026-11-19).
- aimem's boot script also adds two hooks to the user's own
  `~/.claude/settings.json`, `Stop` and `StopFailure`, each running
  `aimem submit-claude`, which records a finished turn in aimem. join and
  `check` therefore print, on a host set up as written: "this home runs
  under an OS user whose own Claude Code configuration reaches every turn
  of the member: hooks in ~/.claude/settings.json". On a member's host this
  notice is expected, and these two hooks are the reason. In a member's
  turn they run with the home's own aimem: the home's managed
  `.claude/settings.json` gives every session started there, its hooks
  included, the home's `AIMEM_STATE_DIR` and `AIMEM_SOCKET` (read from the
  code, `internal/agent/homeaimem.go`). Anything else the notice names
  (other skills, permissions, a `CLAUDE.md`) is the user's own and reaches
  the member's turns too.

Then the person logs the client in: `claude`, and its login flow.

Check: `aicrew-agent version` <!-- run: agent-version -->, `aimem version` <!-- run: aimem-version --> and `claude --version` <!-- run: client-version --> print
v0.11.3, v0.11.5 and 2.1.286.

### 6. Join (member host)

First the preflight, with the same `--url` and trust values join takes:

<!-- run: preflight -->
```sh
aicrew-agent preflight --url https://AICREWD:PORT --tls-trust-mode ca_dns --tls-trust-value AICREWD --client claude
```

It spends nothing: no invitation, no session and no model call. It
sends no credential. It calls aicrewd's `/healthz` with join's own client
and trust, and runs `claude auth status`. Its `aicrewd:` line is one of:
- **`reachable`**: aicrewd answers, with its version.
- **`name`**: the host name does not resolve on this host.
- **`refused`**: nothing answers at the address, or it times out.
- **`certificate`**: the certificate does not match the CA and name
  (`ca_dns`) or the pinned key (`spki_sha256`).
- **`clock`**: the certificate is not valid at this host's time. Check
  the clock first. Only `ca_dns` checks the dates: under `spki_sha256`
  the pin replaces the chain check, so a pinned aicrewd never gives this
  answer.
- **`wrong_address`**: something answers, but not aicrewd's health route.
- **`interrupted`**: an answer began (HTTP 200) but could not be read:
  the connection broke or timed out during it. Check the network between
  the hosts, any proxy, and aicrewd's log.

Its `login (claude):` line is `stored`, `none` or `not_found`. `stored`
means a login is stored, not that it is valid: only the turn-model call
in `check`, after join, shows that. The login is read without the
shell's `CLAUDE*` and `ANTHROPIC_*` variables (except
`CLAUDE_CONFIG_DIR`), since the member's service has none. Each of
`ANTHROPIC_API_KEY`, `CLAUDE_CODE_OAUTH_TOKEN` and the Bedrock or Vertex
switch makes `claude auth status` report a login on its own. For
`--client opencode` the line is `no_check`.

Check: the preflight exits 0. It exits 1 with the line that failed.

Then join, on a terminal, so the code is read at a hidden prompt:

<!-- run: join per=worker -->
```sh
aicrew-agent join --label worker --url https://AICREWD:PORT \
  --tls-trust-mode ca_dns --tls-trust-value AICREWD --aimem-hub main --client claude --wake turns \
  --aimem-url https://HUB --aimem-token-file member.token \
  --cred FORGE_HOST=forge.token
```

- **The forge host.** `FORGE_HOST` is `github.com`, or the Gitea host with
  its port. With `--cred FORGE_HOST=-` the token is typed at a hidden
  prompt instead of read from a file.
- **The hub's CA.** A hub on a private CA adds
  `--aimem-ca-file hub-ca.pem` <!-- run-add: join -->. Pin aicrewd as its trust mode requires
  ([DEVELOPMENT.md](DEVELOPMENT.md), "Joining a team").
- **A pinned aicrewd.** With aicrewd's key pinned instead of its CA and
  name, the preflight and join both take
  `--tls-trust-mode spki_sha256 --tls-trust-value sha256-PIN` <!-- run-alt: preflight join -->
  in place of their two trust flags.
- **Without a terminal.** A run with no terminal gives the code with
  `--code-file PATH` <!-- run-add: join -->, an owner-only file. Otherwise it stops with
  `no_terminal` and says so.
- **The service.** In `turns` mode, join installs and starts the member's
  service itself when the home is ready.

- **The skills.** From aicrew 0.11.2 on, when the client does not see
  the skills the team's process pins, join installs them from the hub
  itself. It opens a conversation in the team as the member, has the
  home's aimem install the skills of the team's first granted project as
  that conversation, closes it, and checks again. The member's aimem user
  needs no grant of its own: the hub reads under the team's grant. This
  needs aimem 0.11.5 or later in the home. A paused team is no obstacle,
  since a pause holds the member's turns, not its conversation.

When join cannot install the skills, it ends `blocked` with one of these
reasons, the step that fixes it, and the rerun,
`aicrew-agent join --home HOME` <!-- run: join-rerun -->:
- `skills_not_on_hub`: the hub holds no set with every pinned skill. The
  hub's admin imports it (step 1); aimem's own lines name each skill and
  its import command.
- `skills_grant`: the team's grant does not cover the project. Step 1's
  `aicrew team setup` prints the grant.
- `skills_no_process`: the project selects no process on the hub.
- `skills_aimem_below`: the home's aimem predates the team path. The line
  installs the release that has it.
- `skills_conversation`: the conversation could not be opened, or the
  hub did not accept it. For a session the home recorded earlier, the
  line names `aicrew-agent session leave --home HOME` <!-- run: session-leave --> first.
- `skills_no_project` and `skills_install_failed`: the team has no
  project yet, or aimem failed otherwise, with its own text.

On a worker's host, join also ends `blocked` with the reason
`toolchain_tool_missing` when a toolchain is selected for the team's
project (step 1) and its tool, `go` for `dev-go`, is not on the PATH.
Join then installs no service. Install the tool (the host table above),
then rerun join from a shell that finds it:
`aicrew-agent join --home HOME`. The rerun installs the service, which runs with that shell's PATH.

Check: join's report is `ready`, and its `skills:` lines name what it
installed. Its forge table shows the host `provisioned`, and `aicrew
invitation list` shows step 4's invitation redeemed. On 2026-11-20 both
members' joins with v0.11.2 ended `ready` in about eleven seconds, with
the skills installed from the hub (seen).

After `ready`, delete the input files from the member's host:
- **The aimem token** (`--aimem-token-file`): join gave it to the home's
  own aimem installation, which keeps it.
- **The forge token** (`--cred`): join wrote it to the home's credential
  store.
- **The invitation code** (`--code-file`): it is spent.
- **The hub's CA file** (`--aimem-ca-file`), if one was given: join copied
  it into the home.

Two lines of join's and `check`'s output are not faults:
- **"Claude Code lists the project MCP server aimem as not yet approved
  for an interactive start in this home. This home is in turns mode ...
  Nothing is to be accepted".** This is the notice of a home joined with
  `--wake turns`, as above. Only an interactive start asks to trust the
  folder and to approve the server, and a turns home has none:
  `aicrew-agent run` refuses to start there. Its service runs each turn
  in print mode, which asks neither, and the init probe in print mode
  reports aimem's server `connected` while the approval is pending. This
  is read from the code and from measurements of October 2026
  (`docs/TURN-PROBE.md`, "MCP tools and file reads in a turn"). The
  members' first turns on v0.11.2 were seen on 2026-11-20: the
  coordinator offered a task and the worker accepted it and committed its
  change. A home joined with
  `--wake stop-hook` gets the other form of the notice, which names
  `aicrew-agent run`: there the person who starts the client accepts
  both.
- **"claude mcp list: ..., aimem pending approval for an interactive
  start".** The same approval, as `claude mcp list` reports it. The line
  before it, "claude init probe: ..., aimem MCP server connected", is how
  a turn sees the server.

### 7. The service (member host)

<!-- run: enable-linger service-status -->
```sh
loginctl enable-linger "$USER"
aicrew-agent service status --home HOME
```

Whether a user may turn lingering on for itself depends on the system's
policy; where it may not, a person with root does. Without it, the member
stops at the last logout.

Check: `service status` says `running`.

### 8. The team (operator)

If the team was paused in step 1, resume it when every member is
`ready`: `aicrew team resume --team-name crew` <!-- run: team-resume -->.

Check: `aicrew team show --team-name crew` lists the coordinator and the
worker. `aicrew-agent check --home HOME` on each host has no `blocked`
line, and on the worker's host no warning for a project whose toolchain
was selected. Its `turn model:` line
says `available`, and its project table says `verified` for each
repository; with `--json` <!-- run-add: check --> it prints the same
report as JSON, for a script. `aicrew status` shows both members.

This is feature 5's check.

### A team already up without a toolchain

A team brought up before this document named the toolchain step has no
toolchain selected. Its worker's first task ends where the first team's
did on 2026-11-20: the turn denies the project's tests, the worker blocks
the attempt, and the coordinator escalates. Nothing in the worker's home
is edited by hand. In this order:
1. On the worker's host, `aicrew-agent check --home HOME` prints
   `warning: project PROJECT has no toolchain selected` under its status
   line, with the operator's command. That command names the team by its
   ID, `--team ID`, which is the same team as `--team-name crew` in step
   2 below. A worker's check reports the
   toolchain from aicrew-agent 0.11.3 on; an older one prints no such
   line: upgrade the member to v0.11.3 first (step 5's one-liner). The
   upgrade restarts its service, which records the team again: run check
   once `msg="the turn loop runs"` is in
   `HOME/logs/serve.log` after the upgrade, since a check run earlier may
   read the record from before it and print no line.
2. The operator selects the toolchain, as in step 1:
   `aicrew team toolchain --team-name crew --project PROJECT --toolchain dev-go`.
   Check: `aicrew team show --team-name crew` names it.
3. On the worker's host, the service restarts, so that its launcher
   records the selection: `aicrew-agent service restart --home HOME`.
   Wait until `HOME/logs/serve.log` has `msg="the turn loop runs"` from
   after the restart: the record is written before that line.
4. `aicrew-agent check --home HOME` again. A check that still prints the
   warning was run before the record: run it again after that line. See
   first that the warning names the project just selected: another
   project keeps its own until it has a selection. A warning for that
   project that persists after the line was not a check run too early:
   `HOME/logs/serve.log` then holds `msg="the team's projects were not
   recorded"` with the cause in its `error=` field; restart the service
   again once that cause is gone. A `blocked` status with the reason
   `toolchain_tool_missing` says the host lacks the toolchain's tool.
5. If so, install it (for aicrew, Go at go.mod's `go` version or later),
   then restart the service from a shell that finds it, as the check's
   instruction says, and wait for the line again. A worker whose join
   ended blocked has no service yet: there, rerun join from that shell
   (`aicrew-agent join --home HOME`), which installs it.
6. `aicrew-agent check --home HOME` says `ready`, with no warning that
   names the project just selected.
7. Only then is the open escalation answered, so that the worker's
   resumed attempt runs its tests under the new selection.

## The operator during a cycle

Once the team is resumed, the coordinator offers a task, the worker works
on it and opens its pull request, and the reviews and CI decide when a
person merges it. The operator watches and answers with these commands,
on the host where the operator's commands run ("Before step 1"). Each
prints JSON.

**The team's state.**

<!-- run: team-state -->
```sh
aicrew team state --team-name crew
```

It lists each member under `members`:
- `state` is `working` (a turn runs), `waiting` (the member waits for
  its next message), `paused`, `offline` (aicrewd has not heard from it
  lately), `needs_login` or `limit_reached`. A member's `hold`, when
  present, says why it takes no turn and until when, with the command
  that logs a client in.
- `unacknowledged` counts the messages in the member's inbox that it
  has not acknowledged yet, delivered or not. A member acknowledges a
  message once a turn has handled it, so the count says only that these
  messages are not handled yet. While a turn works on them, `state` is
  `working`. A turn can also end without acknowledging them, for
  example when a step it needs is refused. Then the next turn on the
  same messages waits 5 minutes, the one after 20, and after three such
  turns the member is held: `state` shows `waiting`, and its `hold` has
  `kind` `unacknowledged` with the `messages`, the number of `turns`,
  the last turn's `summary` and the `next_check`. A held member gets one
  turn a day on those messages, and one turn on a new message no sooner
  than 20 minutes after its last. The team's coordinators get a message
  about a worker's hold. The operator learns of a coordinator's hold
  only from this command. The hold ends when the member acknowledges
  one of the messages, or when the operator pauses the team and then
  resumes it (below), after the hold has left the team's state. On
  2026-11-20, before this hold existed, a coordinator's turns ended and
  started again every few seconds on one message for twelve minutes, its
  `state` alternating between `working` and `waiting` with the count at
  one.
- `open_attempt` is the attempt the member works on: its `phase` is
  `working`, `blocked` (the worker stopped and said why), `submitted`
  (the coordinator reviews it), `rework`, `accepted` or `finalized`. A
  blocked attempt carries `block` with the worker's reason and since
  when.
- `stalled` is true for an attempt that has not changed for longer than
  aicrewd's stall window and waits for no merge and no answer.

Check: the command exits 0 and names the coordinator and the worker.
`paused` at the top says the team is paused, with its reason.

**An escalation.** A question the coordinator cannot settle within its
role becomes an escalation, and the team's work on that task waits for
the answer. Find the open ones:

<!-- run: escalations-list -->
```sh
aicrew escalations list --open
```

Each has an `id`, the `task`, the coordinator's `question`, its
`options` and `recommendation`, and what is `blocked` until the answer.
Read one whole by its ID, or by a prefix of at least eight characters
that starts no other escalation's ID:

<!-- run: escalations-show -->
```sh
aicrew escalations show --id ESCALATION
```

Answer it once:

<!-- run: escalations-answer -->
```sh
aicrew escalations answer --id ESCALATION --decision "DECISION" --rationale "RATIONALE"
```

A good decision is one the coordinator can act on without asking again:
it names one of the options, or what to do instead, in a sentence. The
rationale says why, with the facts it rests on: the coordinator reads both.
Check: the answer prints the escalation with its
`answer`, and `aicrew escalations list --open` no longer lists it. The
coordinator and the worker act on it in their next turns: the attempt
leaves `blocked` in the team's state.

**Pause and resume.** A pause holds every member's turns, not their
sessions, for example while the operator looks at something by hand:

<!-- run: team-pause -->
```sh
aicrew team pause --team-name crew --reason "REASON"
```

and `aicrew team resume --team-name crew` lets them go on. Check: the
team's state shows `paused` with the reason, and no longer after the
resume.

**On the forge.** The worker's pull request carries the worker's review,
the external reviewer's and the coordinator's, each naming the head it
read, and CI's checks. When all three are `READY_FOR_HUMAN_MERGE` at the
same head and CI is green, the pull request is marked ready, and a person
merges it: neither the members nor the operator's commands merge. Until
task 01a1fa07-5db2 is done, the readiness check does not read the
worker's and the coordinator's reviews, whose headers are not yet in the
form it reads, so the pull request waits for the person's own look at
the three reviews.

## The forge token's rights

A member's own forge token is used by `join` and `check`, by git through
`aicrew-agent git-credential`, by `aicrew-agent forge pr` (the commands a
member's client runs) and by the default automations every member's
launcher runs (a CI re-run, a base update, labels, comments and marking a
pull request ready). The lists below are what those calls need, on each
repository of the team's projects.

### Gitea

| Scope | What needs it |
| --- | --- |
| `read:user` | `join --cred` and `check` ask the forge who the token authenticates as. Without it `join` refuses the token, naming the scope |
| `write:repository` | git's clone and push; `forge pr open`; `forge pr ready` (it edits the title); reading the repository, its branches, the pull request and its reviews and statuses; an automation's base update, which pushes |
| `write:issue` | `forge pr label` and `forge pr comment`, which go through the issue endpoints; `forge pr show` reads the comments there |
| `read:organization` | only when a label the team uses is defined at the organization and not in the repository: `forge pr label add` reads the organization's labels only for a name the repository does not define |

Tried on 2026-11-17 on the pilot simulation's Gitea (1.27), with tokens of
the simulation's worker account on its repository, which a user owns. Each
token was given to `join --cred`, then `check`, a push, and `forge pr`
open, label add, comment, ready, label remove and show were run:
- **All four scopes:** every step passed.
- **Without `read:user`:** `join` refused the token ("the forge rejected
  the token") and kept the credential it had.
- **`read:repository` instead of `write:repository`:** the push and
  `forge pr open` failed. `check` still said `verified`.
- **`read:issue` instead of `write:issue`:** label add and comment failed,
  naming `write:issue` as the scope required.
- **No issue scope:** label add, comment and show failed.
- **Without `read:organization`:** label add failed; everything else
  passed. The label lookup has changed since: a label the repository
  defines no longer reads the organization's labels.

Gitea has no re-run of a job: its checks are commit statuses, and the
CI re-run automation reports that nothing was re-run.

### GitHub

This list is read from the code and from GitHub's documentation of the
permissions each REST endpoint needs. A fine-grained token was not tried.
A classic token with `repo` was: the first team's members joined with one,
join reported it `verified` on 2026-11-19, and `check` on 2026-11-20.

A fine-grained personal access token of the member's account, with access
to the team's repositories:

| Permission | What needs it |
| --- | --- |
| Metadata: read | reading the repository and its default branch, including the account's push permission `check` reports (GitHub adds it to every token) |
| Contents: read and write | git's clone and push; reading a branch's head; an automation's base update, which pushes |
| Pull requests: read and write | `forge pr open`, `show` and `ready` (marking a draft ready and editing the title); labels and comments (see the next row) |
| Issues: read and write (an alternative) | `forge pr label` and `forge pr comment` use the issue and label endpoints ([labels](https://docs.github.com/en/rest/issues/labels#add-labels-to-an-issue), [comments](https://docs.github.com/en/rest/issues/comments#create-an-issue-comment)). Each endpoint's page documents that a fine-grained token needs at least one of two permission sets, Issues or Pull requests (write to add, read to list), and [the permissions list](https://docs.github.com/en/rest/authentication/permissions-required-for-fine-grained-personal-access-tokens) names both for each of them. So Pull requests: read and write, which the row above needs anyway, should be enough; giving Issues as well does no harm. Not tried |
| Commit statuses: read | the head's CI state, in `forge pr show` and the watch of a pull request |
| Actions: read and write | read: a failed job's log in `forge pr show`. Write: the CI re-run automation, which is in every member's home by default and re-runs the failed jobs of a runner failure once per head, with the member's own token |

Two more, from GitHub's documentation and not from the code:
- **Changes to workflow files.** A push that changes a file under
  `.github/workflows` needs Workflows: write. A member whose work touches
  the CI needs it.
- **Check runs.** The CI state also reads the head's check runs
  ([the endpoint](https://docs.github.com/en/rest/checks/runs#list-check-runs-for-a-git-reference)).
  GitHub's documentation names a permission for it only for GitHub App
  tokens (Checks: read), and says that a classic token needs `repo` on a
  private repository. The fine-grained token's list does not name the
  endpoint, so its case is unverified: on a public repository nothing more
  should be needed; on a private one it is not known.

The account's own `GET /user` needs no permission.

A classic personal access token covers the same with the `repo` scope
(`public_repo` on a public repository), and `workflow` for workflow files.
GitHub recommends the fine-grained one, but a member whose account only
collaborates on the repository needs the classic one (step 3).

### What `check` reads of the token

`check` reads the account's access to each repository, which the forge
reports for the account, and, for a project that needs write access, what
the token itself may do. Its project table shows them apart: `access` is
the account's, `token` is the token's (`verified`, `insufficient` or
`not_checked`). A row is `insufficient` when either falls short, with the
missing right named. When the forge's answer names a right as missing from
the token, the home reports that repository to aicrewd as `read`, so no
offer needing a write reaches the member. A `not_checked` token keeps the
account's access.
- **Gitea.** A token cannot list its own scopes (the token list needs the
  account's password). `check` sends two requests, each aimed at number 0,
  which Gitea gives to no issue or pull request (it numbers them from 1),
  with no body, so a request that did reach a record would carry nothing
  to change.

  | Scope | Request | Answer with the scope | Answer without it |
  | --- | --- | --- | --- |
  | `write:repository` | `PATCH /repos/OWNER/REPO/pulls/0` | 404 `not found` | 403 naming `write:repository` |
  | `write:issue` | `POST /repos/OWNER/REPO/issues/0/labels` | 422 `[]: Empty Content-Type` | 403 naming `write:issue` |

  These are the answers of Gitea 1.27.3. Only the answer in the third
  column counts as the scope being there, and only a refusal naming the
  scope counts as missing. Any other answer, for example from another
  version that words or orders its checks differently, is `not_checked`
  with the answer, and `check` adds a notice; it is never read as a
  verdict either way.
- **GitHub, classic token.** GitHub sends the token's scopes in the
  `X-OAuth-Scopes` header of each answer
  ([scopes](https://docs.github.com/en/apps/oauth-apps/building-oauth-apps/scopes-for-oauth-apps)).
  `check` reads it from the repository's answer it already asks for: `repo`,
  or `public_repo` on a repository the forge reports as public, is
  `verified`; a header naming other scopes only is `insufficient`. A header
  that is absent or names no scope is `not_checked`.
- **GitHub, fine-grained token.** No answer names its permissions, so
  `check` says they are not checked and points to the list above.
- **GitLab.** Not read: `not_checked`.

The Gitea part was tried. Of the GitHub part, a classic token with `repo`
was seen `verified` on real hosts (step 3); the rest (a fine-grained token,
a classic token without a scope) is read from GitHub's documentation and
tested against a stand-in server only.

## How far this was tried

**On real hosts, 2026-11-19 and 2026-11-20.** A hub with aicrewd beside
it, two member hosts (one Debian 13, one Ubuntu 26.04, to try two
systems), one GitHub repository owned by a personal account, and one
machine account for both members:
- **The operator's steps 1 to 4** were run by the owner. The findings of that run are corrected above: the
  grant line, the two connections, the hub admin's bearer, the paused
  team, the invitation's lifetime, the user name and the carrying of
  files.
- **Steps 5 and 6** on both member hosts, with the product's commands
  only, run as the member's user without a terminal (`--code-file`):
  - with v0.11.1 and aimem v0.11.4, join ended `blocked` on the skills,
    which aicrew v0.11.2 fixed;
  - with v0.11.2 and aimem v0.11.5, join ended `ready` on both hosts, the
    skills installed from the hub, and each member's service started and
    held its own session thirty seconds later. `check` passed with the
    forge credential and the repository verified.
- **The first turns, 2026-11-20.** The coordinator offered the first task
  and the worker accepted it, made the change and committed it. Its turns
  were then denied `go test`: the team had no toolchain selected and the
  worker's host had no Go. The worker blocked the attempt and the
  coordinator escalated. That is why step 1 now selects the toolchain,
  the host table names the build tools, and "A team already up without a
  toolchain" says what such a team does.
- **Not seen yet:** `aicrew team show` of step 8.

On 2026-11-16, step 5 and the start of step 6 were run as an unprivileged
user in a scratch `debian:bookworm-slim` container on the lab simulation
host:
- **The packages.** Those of the table installed, with Debian's `nodejs`
  and `npm`.
- **The one-liners.** The aicrew one-liner installed `aicrew-agent` and
  `aicrew` v0.11.0, each checked against `SHA256SUMS`. aimem's boot script
  at v0.10.25 installed aimem v0.10.25. npm installed Claude Code 2.1.286.
- **Join.** Without a terminal or an invitation, `join` stopped with
  `no_terminal` and the instruction above. Nothing further was possible
  without a real invitation, aimem token, forge token and login.

The steps after that (steps 2 to 4 on the operator's side, then join with
`--code-file`, `check` and `serve`) are what the pilot simulation runs for
each member on every run, with its own aimem, Gitea and aicrewd
(`scripts/pilot-sim.sh`, "the members"; `e2e/pilotsim/member/member.sh`).
It joins with `--no-service` and supervises `serve` itself, since its
containers have no systemd. So step 7 and the client's real login were not
tried.

## Gaps

These are findings for the architect, not fixed here:
1. **The client's login has no check without a model call.** The
   preflight shows only that Claude Code has a login stored. The
   turn-model call is the only product check that the login works.
2. **`check` and a token's missing rights.** Closed for Gitea's two write
   scopes and for a classic GitHub token ("What `check` reads of the
   token"). Left: a fine-grained GitHub token's permissions, which no
   answer names, so `check` says they are not checked; a missing right
   there still shows at the first call that needs it.
3. **The first install warns about services on a host without systemd.**
   On a fresh host without `systemctl`, the aicrew one-liner prints
   "no supported service manager: systemctl not found" and "not every
   member's service stopped", although no service exists. DEVELOPMENT
   says a machine with no service is left alone.
4. **A container host has no product supervisor.** Without a systemd user
   manager, the member's restart after a failure is left to the host.
5. **Reaching aicrewd before join.** Answered by `aicrew-agent preflight`
   (step 6).
6. **Adding a label on Gitea needed `read:organization` on every
   repository.** Closed: the lookup reads the organization's labels only
   for a name the repository does not define. Left: a label defined only
   at the organization still needs the scope.
7. **On GitHub the CI re-run needs Actions: write in every member's
   token.** The re-run automation is in every member's home by default and
   runs with the member's own token.
