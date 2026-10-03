# Agent workspace convention

Status: convention (task crew-workspace). `aicrew-agent join` (1a81-4)
implements the home layout, `agent.json`, the managed files and the rerun
rules; `aicrew-agent check` (1a81-5a) the client wiring and the readiness
check (DEVELOPMENT, "Joining a team" and "Checking dependencies and
clients"). Repositories, worktrees and credential files are not implemented
yet, and no installer is defined. The member's aimem installation in the
home follows decision D-STORE (2026-10-02).
Updated 2026-10-02.

This document fixes where an aicrew agent keeps its configuration,
credentials, guidance, recovery data, logs, repositories and worktrees, and
what a repeated setup may change. It follows the onboarding-workspace section
of the reviewed AIForge design
([DESIGN-AIFORGE.md at f47dc5a](https://github.com/BlackVS/aimem/blob/f47dc5a46f30e462ac2ed42011da30d01df33e93/docs/DESIGN-AIFORGE.md))
and stays consistent with the proposed
[identity and session context contract](https://github.com/BlackVS/aimem/blob/857f4639398eb0f014ba458b61312d057eadd7b3/docs/DESIGN-AIFORGE-CONTEXT.md).
Where a question belongs to one of those contracts, this document names it as
deferred and does not answer it.

## Agent home

An agent starts in its own home directory, never inside a project repository.
One home serves one agent installation for one OS user and may hold work for
several projects and repositories. The operator chooses the home path;
onboarding proposes a default under a common root:

| Platform | Default home |
| --- | --- |
| Linux, macOS | `~/aicrew/agents/<agent>/` |
| Windows | `%USERPROFILE%\aicrew\agents\<agent>\` |

`<agent>` is a readable local label (lowercase letters, digits and `-`, at
most 32 characters). It is not an identity: the actor is the linked aimem
user ID, and renaming the label must not change who the agent is. The label
is also independent of the model and client, both of which may change.

```text
<agent-home>/
  AGENTS.md          managed entry file; points to docs/START.md
  CLAUDE.md          managed entry file for Claude Code; imports AGENTS.md
  agent.json         nonsecret configuration
  .mcp.json          Claude Code's project MCP file; one managed aimem entry
  .claude/settings.json  managed; points every Claude Code session here at aimem/
  aimem/             the member's aimem installation; owner-only, secret
  creds/             credential material only
  docs/              agent guidance and handoff
  state/             nonsecret recovery data; owner-only, holds the step socket
  logs/              redacted logs
  work/              temporary artifacts
  repos/             clean clones, one per repository key
  worktrees/         one isolated worktree per attempt
```

Client entry files exist only because clients read instructions from their
working directory. They stay short and point to `docs/`; they never repeat
repository instructions.

`agent.json` records nonsecret facts: the layout version, the agent label,
hub aliases with the stable hub IDs they locate, the configured clients,
credential reference names (never values), and the managed-file record used
for reruns (see below). The bootstrap writes these keys and keeps every
other key as it finds it:

```json
{
  "layout": 1,
  "label": "builder",
  "aicrew": {
    "url": "https://aicrew.example:8443",
    "tls_trust_mode": "ca_dns",
    "tls_trust_value": "aicrew.example",
    "aimem_hub": "main",
    "agent_id": "01a0...",
    "team_id": "01a0..."
  },
  "clients": ["claude"],
  "managed": {"AGENTS.md": "sha256:...", "CLAUDE.md": "sha256:...", "docs/START.md": "sha256:...", "docs/ROLES.md": "sha256:...",
              ".claude/settings.json": "sha256:...", ".mcp.json#mcpServers.aimem": "sha256:..."}
}
```

- `layout` is this layout's version. A home with another version is
  reported, never migrated.
- `aicrew` is the client's section (DEVELOPMENT, "Running aicrew-agent"). Its
  `agent_id` and `team_id` are written once the invitation is redeemed; a
  home that has them is linked, to one team. `aimem_hub` is the hub alias in
  the home's aimem installation (`aimem/`), whose `hub.json` holds the
  individual credential in aimem's own storage.
- `clients` are the clients the home is for (`claude`, `opencode`); the
  check wires and verifies each.
- `managed` holds each managed file's digest at its last managed write, and
  each client's managed MCP entry (`<file>#<parent>.<name>`) the same way.

Client wiring is project-level only: `.mcp.json` (Claude Code) and
`opencode.json` (OpenCode) in the home, each with one managed `aimem` MCP
entry, and Claude Code's `.claude/settings.json` with the home's aimem
variables (below). Nothing else of the onboarding's is written there: no
hook, and no user-level client configuration.

## The member's aimem installation

An aimem installation is its state root: `hub.json` with each hub's URL,
checkpoint token, individual task credential and CA path, and the team
sessions, project credentials, journals, memories, spools and local socket.
The member's whole installation lives in the home, as `aimem/`. Everything
that defines the member is then in one portable home, and the user's own
installation (`~/.local/state/aimem` by default) stays the human's. The
survey, the options and the decision (D-STORE) are recorded in
[proposals/STORAGE-MODEL.md](proposals/STORAGE-MODEL.md).

- **`aimem/` is owner-only and secret**, like `creds/`. `join` creates it
  and restricts it, and never reads or changes its contents: aimem owns
  them. It is never backed up or synced with the home, and it is never
  committed.
- **One home is one identity per hub**, and therefore one team membership.
- **Three carriers** name the installation, each with the same two
  absolute paths: `AIMEM_STATE_DIR=<home>/aimem` and
  `AIMEM_SOCKET=<home>/aimem/aimem.sock`.
  1. **`.claude/settings.json`** (managed). Its `env` reaches every Claude
     Code session started in the home, whether by `aicrew-agent run` or by
     hand, with its hooks, MCP servers and subprocesses.
  2. **The `.mcp.json` aimem entry's `env`** (managed).
  3. **The environment `join`, `check` and `run` give every process they
     start**: each aimem call, each client probe and the launched client.
     It replaces any inherited value, so the member sets nothing in its
     shell.
- **Mismatches.** `check` blocks when a carrier file disagrees with the
  home. It reports, without blocking:
  - `AIMEM_*` values in the environment or in `~/.config/aimem/env` that
    name another installation (that file must not set either variable);
  - an aimem MCP server in Claude Code's user file (`.claude.json`).
- **The socket.** The `AIMEM_SOCKET` path keeps a client in the home off
  the user's own aimem service. A home runs no service of its own, and a
  Unix socket's path is limited to about 104 bytes, so `check` reports a
  home whose socket path is longer.
- **A standalone client started in the home** is the member in personal
  mode. It has the same identity but no team session and no aicrew steps,
  and aimem refuses claims on the team's held tasks. Memory tools are
  unavailable there, because a home runs no local aimem service.
- **Identity never lives in a project checkout.** A repository file that
  could name a state root would let a cloned repository redirect hooks and
  tokens, so the home and its launcher make the binding.
- **OpenCode** has no per-session environment carrier. An OpenCode client
  started by hand in the home does not get the home's installation; one
  started by `run` does.

## Credentials

`creds/` holds credential material and nothing else: no scripts, notes, logs,
backups, source or configuration. Everything that is not a secret lives
elsewhere. The directory is owner-only: mode `0700` with `0600` files on
POSIX, and on Windows an ACL granting only the owning user (and SYSTEM), with
inheritance disabled. An implementation may keep the secret in an OS
credential store instead and leave only a nonsecret pointer; which backend,
file format or encryption is used is deferred to the onboarding and context
implementation reviews.

Each credential has a stable readable reference:

```text
<service>.<account>.<purpose>
```

Each part uses lowercase letters, digits and `-`. `service` is the system
that accepts the credential (`aimem`, `aicrew`, `github`), `account` is the
account or hub alias at that service, and `purpose` says what it is for.
Examples: `aimem.main-hub.agent` for the one individual aimem credential this
installation holds per hub, and `github.example-org.repo-write` for a scoped
forge token. The name does not change on rotation and never contains the
model, client, secret or expiry.

Rules:

- **No secret outside `creds/`**, the OS store, or the home's aimem
  installation (`aimem/`, aimem's own storage): not in `agent.json`,
  command arguments, environment files, repositories, worktrees, `state/`,
  `logs/` or chat. Logs and reports name the reference, never the value.
- **No implicit fallback.** A missing, expired or refused credential stops the
  operation and is reported with its reference name. An agent never retries
  with another credential, a personal credential for team work, or an
  administrative token.
- **Short-lived session secrets** issued under the context contract are still
  secrets and follow the same rules. How they are persisted and bound to a
  conversation is deferred to that contract's local-session slice.
- **Provider logins are exempt.** Model-provider authentication stays in each
  client's own supported storage. Onboarding does not read, copy, move or
  change it, and it never appears in `creds/`.
- **Rotation** replaces the material behind the same reference, then removes
  the old material once the new one is confirmed. Identity and links are
  unchanged; the rotation procedure itself belongs to the owning service.
- **Expiry** is tracked by the issuing service. A local expiry note, if kept,
  is nonsecret metadata outside `creds/`.
- **Backup** excludes `creds/` and `aimem/` from general backups and sync. Recovering a
  lost credential means reissue through the authorized flow, not restoring
  old material that may have been revoked.
- **Cleanup** of an agent home revokes its credentials at their services
  first, then deletes `creds/` and `aimem/`.

This layout does not isolate agents that run as the same OS user: any process
of that user can read the home, including another member's `aimem/` and its
credential. On Windows, owner-only means the one account's SID, so homes under
one account do not protect each other. Use separate OS users where isolation
is required.

## Repositories and worktrees

A repository key is the forge host, owner path and name as they appear in the
clone URL, and it maps directly to a directory:

```text
repos/<host>/<owner>/<name>/
repos/github.com/blackvs/aicrew/
repos/git.example.net/platform/aicrew/     # same short name, no collision
```

Each path segment may contain only letters, digits, `.`, `_` and `-`; any
other character rejects the key rather than being rewritten. Keys are compared
case-insensitively, so two keys that differ only by case are refused as a
collision on every platform. A clone under `repos/` is a clean source for
worktrees. Agents do not edit, build or check out branches in it.

Work happens in a worktree:

```text
worktrees/<name>-<task>-<n>/
worktrees/aicrew-5ec2a398-1/
```

The same layout on Windows, for an agent labelled `builder`:

```text
C:\Users\dev\aicrew\agents\builder\creds\
C:\Users\dev\aicrew\agents\builder\repos\github.com\blackvs\aicrew\
C:\Users\dev\aicrew\agents\builder\worktrees\aicrew-5ec2a398-1\
```

Nested repository paths are long, so keep the home root short on Windows;
deep trees can exceed the 260-character path limit unless long paths are
enabled for the OS and Git.

`<name>` is the repository name, `<task>` the last eight hex digits of the
task ID (its random part; the leading digits of time-ordered IDs repeat
across tasks), extended until unique, and `<n>` the attempt number. The attempt
identifier used by aicrew execution is defined by the execution contract;
this directory name is only a readable, unique local label. Every worktree is
created from an explicitly named base commit, recorded with its branch in
`state/`, never from whatever the shared clone happens to have checked out.
A worktree is removed after its attempt is finalized and its branch pushed or
abandoned by decision. Unpushed work is never deleted silently.

## Initial guidance

Onboarding writes `docs/START.md` (managed). It tells the agent:

1. Which instructions win: an explicit operator instruction first, then the
   repository's own `AGENTS.md`/`CLAUDE.md` for work in that repository, then
   the project process selected in aimem, then this generic home guidance.
   Home guidance never copies repository rules.
2. That `creds/` is credential-only and secrets never leave it.
3. That every change happens in a worktree created from an explicitly chosen
   base commit, one per attempt.
4. Where the handoff lives: `docs/HANDOFF.md` belongs to the agent and is
   never managed.
5. How to verify readiness (versions, client, MCP and skills) and when to
   stop and ask instead of improvising.
6. That `docs/ROLES.md` (managed) teaches its role.

`docs/ROLES.md` holds the guidance every member follows and one section per
role (coordinator, worker, independent), since a member's role can change
and a refresh does not ask aicrewd. It teaches:
- each transition as an `aicrew-agent step` command with an example body;
- the inbox rule: read at session start and after each step, act only on
  what the inbox or one's own step answers show, and acknowledge what was
  handled;
- the exit codes, following a refusal's next action, and never another
  credential;
- the instruction digest's definition (`docs/CREW-CONTRACT.md`, "Process
  pins");
- an interim rule for human-readable persisted text, which aimem task
  01a0d996-616b's canonical rule will replace.

Its operations are tested to be exactly the launcher's, and each example
body to decode into aicrewd's body for its route.

## Managed files and repeated setup

| Class | Files | Setup may |
| --- | --- | --- |
| Managed | `AGENTS.md`, `CLAUDE.md`, `docs/START.md`, `docs/ROLES.md`, `.claude/settings.json`, each client's `aimem` MCP entry, managed keys in `agent.json` | create; update only if unchanged since its last write |
| Agent-owned | `docs/HANDOFF.md`, other `docs/` notes | create once if missing; never change |
| Protected | `creds/`, `aimem/` (its contents), `repos/`, `worktrees/`, `state/`, `logs/`, provider logins, unknown files | never change, move or delete |

Reruns are non-destructive and repeatable:

- Setup shows its planned changes before applying them.
- A managed file is updated only when its content still matches the digest
  recorded at its last managed write. If someone edited it, setup reports a
  conflict and writes the proposed version beside it as `<file>.aicrew-new`
  instead of overwriting.
- Missing directories and files are created; nothing unknown is deleted.
- Credentials are only ever added through the credential flow, never replaced
  by a rerun.
- A layout-version change is reported with its migration steps; setup does
  not reorganize an existing home silently.
- The result is reported as ready, restart required, or blocked with
  instructions.

## Deferred to other contracts

| Question | Owner |
| --- | --- |
| Credential issuance, identity proof, session handles and verification | aimem context contract and its implementation slices |
| Credential file format, OS store backend, encryption | onboarding and context implementation reviews |
| Roles, grants and knowledge permissions | context contract and aimem knowledge access matrix |
| Session persistence and `state/` formats | context contract local-session slice; aicrew execution and onboarding tasks |
| Installing and upgrading dependencies (verified installers) | crew-onboarding (1a81-5b) |
| Attempt identifiers and reservation fencing | crew-contract and the aimem reservation contract |
