# Storage model: agent state on a host (decision D-STORE)

Status: **record of decision D-STORE** (2026-10-02), delivered by aicrew #76 and aimem #169; the text below is the hub document STORAGE-MODEL, rev 1, unchanged.

Status: decided by the operator on 2026-10-02 (variant A). Written by the coordinator from a read-only survey of aimem v0.7.4 (tag 79f713f) and aicrew main at ea60ff7 (after #75). Record of decision: aicrew pilot task 01a0d6d7-1b3d, comment seq317. This document carries no private host, hub or credential names and may be copied into the aicrew repository (docs/proposals/) as is.

## 1. Question

One host runs the human operator's own tooling, standalone agents (a coding client in a repository, using the human's identity), and members of aicrew teams (a coordinator and workers, each a distinct hub user). Several hubs may exist, and any agent may be bound to any of them. The pilot needs two team members on one Windows account. What is stored at which level, and how is a member isolated without a second OS account?

## 2. What the code does today

### aimem client (v0.7.4)

- An **installation** is a state root: `AIMEM_STATE_DIR`, else `$XDG_STATE_HOME/aimem`, else `~/.local/state/aimem` (cmd/aimem/main.go stateRoot; the MCP server has the same copy in internal/mcp/mcp.go). The design doc says "one persistent individual agent secret per installation".
- Under the root: `hub.json` (mode 0600, plaintext JSON) with every named hub's URL, checkpoint token, individual `task_token` and the absolute path of a CA file; `aicrew-sessions/` (team session handles, short-lived secrets); `task-credentials/` (project-scoped tokens, DPAPI on Windows); `team-sessions/` (legacy, keyed by checkout path); `projects/<id>/journal.db` (journal and memories, including the `user` and `group-*` scopes); `spool/` (undelivered checkpoints); `docsync/`, `process/`, `sync/`, `curate/` cursors and caches; `adapter.log`; `providers.json` (may hold API keys); `aimem.sock` when no other socket location applies.
- **Socket:** `AIMEM_SOCKET`, else `$XDG_RUNTIME_DIR/aimem.sock`, else `<root>/aimem.sock` (internal/server/server.go). With `XDG_RUNTIME_DIR` set (normal on Linux) the socket is per OS user, not per root: a second installation's hooks and memory tools write into the first installation's journal, and a second `serve` fails. On Windows the socket is under the root.
- **`~/.config/aimem/env`** is read by every command before the root is resolved and sets any `AIMEM_*` variable the process lacks. The process environment wins.
- **MCP server:** `aimem mcp` from the client's `.mcp.json`, no env, no hub name; project from the cwd (`.aimem.json` pin or git origin). Personal mode: memory and journal through the socket, tasks straight to the hub with the root's `task_token` or the checkout credential. Team mode (`AIMEM_TEAM_SESSION=<file>`): session file plus the same root's `task_token`; no socket, no checkout credential; hooks skip capture.
- **Hub credentials:** `hub add --token-file` copies the token into hub.json; `--ca-file` records only the absolute path; `hub task-token` stores the `aimem_user_` token in the same hub.json. Several hubs coexist by alias; a repository's `.aimem.json` names its hub by alias, resolved against the opening installation's hub.json.
- **Per OS user regardless of root:** the systemd unit and the Windows scheduled tasks (`aimem-serve`, `aimem-sync`, default root; a reinstall kills every serve of the user), user-level client hooks in `~/.claude/settings.json`, skill directories under `~/.claude`, `~/.codex`, `~/.agents`, `~/.config/opencode`. `CLAUDE_CONFIG_DIR` is never consulted by aimem.

### aicrew agent (main at ea60ff7)

- The **home** holds membership, not identity: `agent.json` (layout, label, hub alias, agent and team IDs, trust pin), `state/` (session record, join state, pending steps, locks, the step socket), the managed `AGENTS.md`, `CLAUDE.md`, `docs/START.md`, `docs/ROLES.md`, and `.mcp.json` with `aimem mcp` and no env. `creds/`, `logs/`, `repos/`, `worktrees/` are created empty. No secret is written to the home; session tokens travel on stdin only.
- `join` does not create an identity. It reads one from aimem (`aimem hub credential <alias>`, `aimem identity proof --hub-name <alias>`) and tells the member to run `aimem hub task-token` when none exists. Whichever aimem installation the environment names is the member's identity.
- Processes inherit the environment unchanged except for one or two replaced variables: aimem lifecycle calls get the full environment; reservation and task reads get `AIMEM_TEAM_SESSION`; the client under `run` gets `AIMEM_TEAM_SESSION` and `AICREW_AGENT_HOME`. `AIMEM_STATE_DIR`, `AIMEM_SOCKET` and `CLAUDE_CONFIG_DIR` are never set, recorded or checked. #75 made `check` keep `CLAUDE_CONFIG_DIR` in its probes and added a line telling members on a shared account to set two variables in their shell.
- The step socket is `<home>/state/step.sock` on every OS, protected by an owner-only DACL on Windows. Owner means the process user's SID: two homes under one account can reach each other's socket, `state/` and `creds/`. WORKSPACE.md states this limit.
- The real-aimem e2e harness isolates each member by `HOME`/`USERPROFILE`, `TMPDIR`, `AIMEM_STATE_DIR` and `AIMEM_SOCKET`, and refuses `XDG_*` variables: the only working recipe in the codebase.

### Consequence

Two homes under one OS account without a separate state root are one hub actor with one `task_token`, shared sessions and shared journals. The pilot's bypass test ("the worker cannot claim a held task directly") is void in that configuration. A per-root override alone leaves the socket (Linux), the user-wide env file, and the user-level client hooks outside the home.

## 3. Levels and rules

| Level | Holds | Rule |
| --- | --- | --- |
| Hub | tasks, documents, knowledge, identities, reservations | everything shared between agents lives here, never in host files |
| OS user (`~/.local/state/aimem`, `~/.claude`, `~/.config/aimem/env`, `~/.gitconfig`) | the human's own aimem installation with all their hubs and tokens, their memories, the client login, skills, global rules | belongs to the human and to standalone agents acting as the human; nothing of a team member lives here; the env file must not set `AIMEM_STATE_DIR` or `AIMEM_SOCKET` once more than one installation exists |
| Agent home (`<home>/`) | the member's aimem installation (`<home>/aimem/`: hub.json with the member's token and hubs, sessions, spool), `agent.json`, `state/`, `creds/` (CA material), managed docs, `.mcp.json`, `.claude/settings.json` | everything that defines the member; portable and self-contained; one home = one identity per hub = one team membership |
| Project checkout (`repos/`, `worktrees/<attempt>`) | `.aimem.json` (hub alias, groups, pin), `.mcp.json`, hooks, `docs/SESSION-STATE.md` | nonsecret project configuration only; never identity: the hub alias resolves through the opening installation's hub.json, so one checkout works from any installation; `task_credential: local` is not used in homes |

Identity lives only in an installation, never in a project folder. A repository file able to name a state root would let a cloned repository redirect the human's hooks and tokens to a planted installation, so the binding of a home to its installation is made by the launcher and by the home's own settings, never by a file in a checkout.

A standalone agent started inside a home is the member in personal mode: the same identity, no team session, no aicrew steps, and aimem refuses claims on held tasks. Memory tools are unavailable there because a home runs no local service. The one thing that must never happen is an identity split: the MCP server as the member while the user-level hooks, inheriting the client's environment, capture the session into the human's journal. This is why the carrier of the two variables must cover the whole client session, not only the MCP server.

## 4. Options considered

- **A. The member's aimem installation inside the home; aicrew-agent addresses it.** Self-contained home, matches aimem's own notion of an installation, no aimem change on the critical path. Chosen.
- **B. Identity kept in the aicrew home, aimem stays per user.** Needs aimem changes (a token file for team sessions, sessions outside the root) and splits one identity across two tools. Rejected.
- **C. One OS account per home.** The only real isolation (distinct SIDs), rejected by the operator as overhead for a worker; it remains available to anyone who needs that isolation, with no code change.

Standalone agents and the operator are untouched by A: they keep the user-level installation. A standalone agent that one day needs its own identity gets the same recipe (a directory as root plus the two variables) without aicrew.

## 5. Decision and implementation

**PR1 (aicrew, Opus, S, pilot prerequisite).**
- `join` creates `<home>/aimem/` (owner-only) and writes a managed `<home>/.claude/settings.json` whose `env` sets `AIMEM_STATE_DIR=<home>/aimem` and `AIMEM_SOCKET=<home>/aimem/aimem.sock` (absolute). Claude Code applies project settings `env` to the whole session started in that directory, including hooks, MCP servers and subprocesses, whoever started it.
- `.mcp.json#mcpServers.aimem.env` carries the same two variables; `join`, `check` and `run` set them for every aimem process and the launched client, replacing inherited values. A test proves no aimem launch from a home sees the user's root.
- `check` reports: the three carriers disagreeing; foreign `AIMEM_*` values in the environment or in `~/.config/aimem/env`; a user-scope aimem MCP server in `~/.claude.json`. Its verdict uses the home's installation.
- Docs: WORKSPACE.md adds `aimem/` and the one-SID limit; DEVELOPMENT.md drops the #75 shell-variables line, keeps `CLAUDE_CONFIG_DIR` as an optional override, and records the operator's one-time provisioning of a home (`aimem hub add`, `hub task-token`, run with the home's variables). The member sets nothing.
- Non-goals: `join` driving `hub add` and `task-token` (PR2); OpenCode carriers; aimem changes; migration beyond a `join` rerun.

**PR2 (aicrew, after the pilot, together with 1a81-6 operator bundle).** `join` performs `hub add` and `task-token` into the home's installation from a token file and a CA file copied into `creds/`, so no path in hub.json points outside the home.

**aimem (Sol, XS).** With `AIMEM_STATE_DIR` set explicitly, the default socket lives under that root rather than `XDG_RUNTIME_DIR`; `AIMEM_SOCKET` still wins. DESIGN-AIFORGE-CONTEXT gains a paragraph on several installations per OS user and the env-file hazard.

**Limits recorded for the pilot.** One Windows SID: homes do not protect each other, and either member's processes can reach the other's step socket and files. OpenCode has no per-session env carrier, so the model is proven on Claude Code only. A home runs no aimem service, so personal-mode memory tools are unavailable in it.

## 6. Follow-ups outside this decision

- Several team memberships per home (today one `agent_id`/`team_id` in `agent.json`).
- An OpenCode carrier for the two variables.
- The e2e harness recipe and the product recipe converge on the same variables; the harness could reuse the home's settings once PR1 lands.
- `task-credentials/` and `team-sessions/` keyed by checkout path: harmless once each home has its own root, but worth noting in aimem's docs.
