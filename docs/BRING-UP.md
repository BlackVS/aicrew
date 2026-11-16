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
| Reaches aicrewd | `curl -sS -o /dev/null -w '%{http_code}\n' --cacert CA https://AICREWD:PORT/healthz` | the `/healthz` check `e2e/pilotsim/compose.yaml` runs against aicrewd |
| Reaches the aimem hub | `aimem hub credential HUB --json` in the home, after join | `join` and `check` |
| Reaches the forge | `aicrew-agent check --home HOME`, its forge table | `check`'s read-only "who am I" per credential |
| The client, logged in | `aicrew-agent check --home HOME`, its `turn model:` line | `check` in a `turns` home (one minimal model call, about $0.002) |

Two notes on these:
- **A container is not enough by itself.** A container usually has no
  systemd user manager. In one, `aicrew-agent service install` cannot run
  the member, so something else must restart `aicrew-agent serve` after a
  failure ([DEVELOPMENT.md](DEVELOPMENT.md), "Starting a member"). There
  is no product command for that, so a virtual machine is the simpler
  host.
- **The client's login.** No product command checks the login without a
  model call. `check`'s client discovery deliberately uses a dummy key and
  a closed model endpoint. The turn-model call is the check that shows the
  login works, and `unknown` there means a refused login, a limit or
  another failure.

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

Check: none until step 6, where `join --cred` asks the forge who the token
authenticates as before writing it. The token rights a member needs are
not written down in this repository. See "Gaps".

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

On a terminal, so the code is read at a hidden prompt:

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
   turn-model call is the only product check of the login.
2. **The forge token's rights are not documented.** Neither forge's
   token scopes or permissions for a member are written down in the
   repository. `check` reports `insufficient` only after the fact.
3. **The first install warns about services on a host without systemd.**
   On a fresh host without `systemctl`, the aicrew one-liner prints
   "no supported service manager: systemctl not found" and "not every
   member's service stopped", although no service exists. DEVELOPMENT
   says a machine with no service is left alone.
4. **A container host has no product supervisor.** Without a systemd user
   manager, the member's restart after a failure is left to the host.
5. **Reaching aicrewd is checked only by join itself.** No product command
   checks it before join.
