# Agent workspace convention

Status: convention for review (task crew-workspace). Docs only: nothing here
is implemented, and no installer, protocol or runtime schema is defined.
Updated 2026-09-25.

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
  creds/             credential material only
  docs/              agent guidance and handoff
  state/             nonsecret recovery data
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
for reruns (see below). Its exact fields and format belong to the onboarding
implementation (crew-onboarding) and are not fixed here.

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

- **No secret outside `creds/`** or the OS store: not in `agent.json`,
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
- **Backup** excludes `creds/` from general backups and sync. Recovering a
  lost credential means reissue through the authorized flow, not restoring
  old material that may have been revoked.
- **Cleanup** of an agent home revokes its credentials at their services
  first, then deletes `creds/`.

This layout does not isolate agents that run as the same OS user: any process
of that user can read the home. Use separate OS users where isolation is
required.

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

## Managed files and repeated setup

| Class | Files | Setup may |
| --- | --- | --- |
| Managed | `AGENTS.md`, `CLAUDE.md`, `docs/START.md`, managed keys in `agent.json` | create; update only if unchanged since its last write |
| Agent-owned | `docs/HANDOFF.md`, other `docs/` notes | create once if missing; never change |
| Protected | `creds/`, `repos/`, `worktrees/`, `state/`, `logs/`, provider logins, unknown files | never change, move or delete |

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
| `agent.json` fields, installer behavior, version sets | crew-onboarding |
| Attempt identifiers and reservation fencing | crew-contract and the aimem reservation contract |
