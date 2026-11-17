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

## Who does what

Only a person can:
- provide each host and its OS user;
- log the client in on each host;
- grant the team its projects on the hub, and create each member's aimem
  user and issue its token there (the hub's admin);
- create the member's forge account and issue its token;
- issue each invitation with the operator credential.

The architect can do the rest, once the operator gives it a shell as the
member's OS user on each host: the installs, `join` with the inputs the
operator placed, `check`, and the service. The architect's own credential
cannot issue invitations. It reads escalations, the merges list and the
team states only ([DEVELOPMENT.md](DEVELOPMENT.md), "The architect
directory").

## What a member's host needs

The coordinator's host and the worker's host need the same things. The
role is set by the invitation, not by the host.

| Need | Check | Source |
| --- | --- | --- |
| Linux on amd64 or arm64 | `uname -sm` | `boot.sh` refuses any other system or architecture |
| One OS user per member, not root, with a home directory | `id` | `e2e/pilotsim/member/Dockerfile` (`useradd -m`); `boot.sh` installs into `~/.local/bin` |
| `curl`, `ca-certificates`, `git`, `jq`, `python3` | `command -v curl git jq python3` | the member image's package list; `boot.sh` needs curl, aimem's installer needs jq, and git is used for each worktree |
| Node.js with npm, for the client | `npm --version` | `aicrew-agent check`'s instruction for a missing client is `npm install -g @anthropic-ai/claude-code@<tested>` |
| A systemd user manager, with lingering on | `systemctl --user status` and `loginctl show-user "$USER" --property=Linger` | `aicrew-agent service` (a systemd user unit), and the note it prints when lingering is off |
| About 0.5 GB of disk for the tools | `df -h ~` | the try below: 34 MB for aicrew and aimem, 456 MB for Claude Code; worktrees come on top |
| 512 MB of memory for the member, plus the client's own | `free -m` | `e2e/pilotsim/compose.yaml` (`mem_limit: 512m` per member, with a stand-in client) |
| Reaches aicrewd | `aicrew-agent preflight` with join's `--url` and trust, before join (step 6) | join's own client to aicrewd, on its unauthenticated `/healthz` route |
| Reaches the aimem hub with the member's credential | `aicrew-agent check --home HOME`, after join: no `credential_missing` or `credential_refused` block, and no notice that the hub could not confirm the home's credential | `check` asks the hub about the credential in the home's own aimem installation, never the OS user's |
| Reaches the forge | `aicrew-agent check --home HOME`, its forge table | `check`'s read-only "who am I" per credential |
| The client, logged in | before join, `aicrew-agent preflight --client claude` shows a login is stored; after join, `aicrew-agent check --home HOME`, its `turn model:` line, shows it works | `claude auth status`; `check` in a `turns` home (one minimal model call, about $0.002) |

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

## The bring-up, in order

For each member, the operator's steps come first and the member host's
after. Each step ends with its check. Throughout, `crew` is the team's
name, `main` the hub's alias in aicrew, and `PROJECT` the project the
team works on.

### 1. The team on its hub (operator and the hub's admin, once)

```sh
aicrew team setup crew --hub main --project PROJECT
```

A new team has no project yet. `team setup` creates or registers it, then
prints `not granted yet; on the hub, as its admin, run:` followed by one
`aimem identity team grant ...` line per project, and exits 3. The hub's
admin runs each printed line on the hub. The operator then runs
`aicrew team setup` again with the same arguments.

Check: the rerun exits 0 and prints
`every project is granted to team crew`. A `granted:` project followed by
`has no repository on the hub` or `has no selected process on the hub`
still needs that done on the hub first; rerun after it until neither line
appears.

### 2. The member's aimem user and token (operator, on the hub)

On the hub's host, with aimem's admin commands:

```sh
aimem access user-add --user-name crew-worker
aimem access token-issue-user --user-id USER_ID --label crew-worker --expires 2027-01-01T00:00:00Z --output member.token
```

Check: `user-add` prints the user's ID, which step 4 pins. The token file
is owner-only. Carry it to the member's host privately, or type it at the
hidden prompt in step 6.

### 3. The member's forge account and token (operator, on the forge)

Each member has its own forge account and token
([proposals/PILOT-1-FOLLOWUPS.md](proposals/PILOT-1-FOLLOWUPS.md), 3.1). The
account needs push access to the team's repositories, since `check` reads
that permission for each granted project.
- **GitHub:** a user account for the member. Add it as a collaborator
  with write access to each repository, and issue a personal access token
  for it.
- **Gitea:** a user for the member, given write access to each repository
  (as a collaborator, or through a team with write). Issue an access token
  for it, under the user's settings, Applications.

The token needs the rights listed under "The forge token's rights" below.

Check: none until step 6, where `join --cred` asks the forge who the token
authenticates as before writing it. A missing right shows only later, at
the first call that needs it (see the gaps).

### 4. The invitation (operator)

```sh
aicrew invitation issue --team-name crew --role worker --hub HUB_ID --label worker --expect-user USER_ID --output invite.code
```

Use `--role coordinator` and another `--label` for the coordinator. The
code is written only to `--output`, lives 24 hours by default, and is
never an argument later. Give it privately to whoever runs `join`.

Check: `aicrew invitation list --team-name crew` shows its state.

### 5. The tools (member host, as the member's user)

```sh
curl -fsSL https://raw.githubusercontent.com/BlackVS/aicrew/v0.11.0/boot.sh | bash
cd "$(mktemp -d)" && curl -fsSL https://raw.githubusercontent.com/BlackVS/aimem/v0.10.25/boot.sh | AIMEM_VERSION=v0.10.25 bash
npm install -g @anthropic-ai/claude-code@2.1.286
```

- aimem's tag is the release this aicrew is tested with, from
  `internal/agent/supported.json`. `aicrew-agent check` prints the same
  line when aimem is missing.
- aimem's boot script wires the directory it runs in as a project, so it
  runs in an empty one.
- npm needs a prefix the user may write to, for example
  `npm config set prefix ~/.npm-global` with its `bin` on `PATH`.

Then the person logs the client in: `claude`, and its login flow.

Check: `aicrew-agent version`, `aimem version` and `claude --version` print
v0.11.0, v0.10.25 and 2.1.286.

### 6. Join (member host)

First the preflight, with the same `--url` and trust values join takes:

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
  `--aimem-ca-file hub-ca.pem`. Pin aicrewd as its trust mode requires
  ([DEVELOPMENT.md](DEVELOPMENT.md), "Joining a team").
- **Without a terminal.** A run with no terminal gives the code with
  `--code-file PATH`, an owner-only file. Otherwise it stops with
  `no_terminal` and says so.
- **The service.** In `turns` mode, join installs and starts the member's
  service itself when the home is ready.

The first join may end `blocked` because the skills are missing. Its
`to do:` line is `aimem skills install -p PROJECT --tool claude`, to run
in the home with the home's `AIMEM_STATE_DIR` and `AIMEM_SOCKET`, which
the line names. Run it as printed, then run `aicrew-agent join --home
HOME` again.

Check: join's report is `ready`. Its forge table shows the host
`provisioned`, and `aicrew invitation list` shows step 4's invitation
redeemed.

### 7. The service (member host)

```sh
loginctl enable-linger "$USER"
aicrew-agent service status --home HOME
```

Whether a user may turn lingering on for itself depends on the system's
policy; where it may not, a person with root does. Without it, the member
stops at the last logout.

Check: `service status` says `running`.

### 8. The team (operator)

Check: `aicrew team show --team-name crew` lists the coordinator and the
worker. `aicrew-agent check --home HOME` on each host has no `blocked`
line. Its `turn model:` line says `available`, and its project table says
`verified` for each repository. `aicrew status` shows both members.

This is feature 5's check.

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
| `read:user` | `join --cred` and `check` ask the forge who the token authenticates as |
| `write:repository` | git's clone and push; `forge pr open`; `forge pr ready` (it edits the title); reading the repository, its branches, the pull request and its reviews and statuses; an automation's base update, which pushes |
| `write:issue` | `forge pr label` and `forge pr comment`, which go through the issue endpoints; `forge pr show` reads the comments there |
| `read:organization` | `forge pr label add`: it looks the label up among the owner's organization labels as well as the repository's. Gitea refuses that lookup to a token without this scope even when the owner is a user, so every repository needs it |

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
  passed.

Gitea has no re-run of a job: its checks are commit statuses, and the
CI re-run automation reports that nothing was re-run.

### GitHub

This list is read from the code and from GitHub's documentation of the
permissions each REST endpoint needs. It was not tried with a token.

A fine-grained personal access token of the member's account, with access
to the team's repositories:

| Permission | What needs it |
| --- | --- |
| Metadata: read | reading the repository and its default branch, including the account's push permission `check` reports (GitHub adds it to every token) |
| Contents: read and write | git's clone and push; reading a branch's head; an automation's base update, which pushes |
| Pull requests: read and write | `forge pr open`, `show` and `ready` (marking a draft ready and editing the title); labels and comments (see the next row) |
| Issues: read and write | `forge pr label` and `forge pr comment` use the issue endpoints. GitHub lists them under both Issues and Pull requests and does not say whether one is enough, so give both |
| Commit statuses: read | the head's CI state, in `forge pr show` and the watch of a pull request |
| Actions: read and write | read: a failed job's log in `forge pr show`. Write: the CI re-run automation, which is in every member's home by default and re-runs the failed jobs of a runner failure once per head, with the member's own token |

Two more, from GitHub's documentation and not from the code:
- **Changes to workflow files.** A push that changes a file under
  `.github/workflows` needs Workflows: write. A member whose work touches
  the CI needs it.
- **Check runs.** The CI state also reads the head's check runs. GitHub's
  documentation names no fine-grained permission for listing them. On a
  public repository they need none; on a private one this is not known.

The account's own `GET /user` needs no permission.

A classic personal access token covers the same with the `repo` scope,
and `workflow` for workflow files. GitHub recommends the fine-grained one.

## How far this was tried

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
2. **`check` does not see a token's missing rights.** It reads the
   account's permission on each repository. Gitea reports that for the
   account, not the token, so a token with `read:repository` only is
   `verified` (GitHub was not tried). A missing right shows at the first push or `forge pr`
   call that needs it, as a refusal naming the scope on Gitea.
3. **The first install warns about services on a host without systemd.**
   On a fresh host without `systemctl`, the aicrew one-liner prints
   "no supported service manager: systemctl not found" and "not every
   member's service stopped", although no service exists. DEVELOPMENT
   says a machine with no service is left alone.
4. **A container host has no product supervisor.** Without a systemd user
   manager, the member's restart after a failure is left to the host.
5. **Reaching aicrewd before join.** Answered by `aicrew-agent preflight`
   (step 6).
6. **Adding a label on Gitea needs `read:organization` on every
   repository.** The label lookup also reads the owner's organization
   labels. For a user's repository that read is meant to find nothing and
   be skipped, but Gitea refuses it to a token without the scope, and the
   refusal ends the label's addition.
7. **On GitHub the CI re-run needs Actions: write in every member's
   token.** The re-run automation is in every member's home by default and
   runs with the member's own token.
