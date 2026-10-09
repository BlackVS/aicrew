# Control plane design

Status: proposed design for review (task `01a11eab-120d`, the first task of
the control-plane epic `01a11eaa-33e6`; section 7 answers the architect
role, task `01a11c96-4a15`; section 8 answers the operator's additions on
kinds of work, capabilities and worker profiles). Nothing here is
implemented. The operator and the monitoring session review it before any
code. Updated 2026-10-09.

aicrew becomes a control plane. aicrewd decides when each member of a crew
works, and a runner on the member's host makes it happen. No human starts a
member's client by hand again. The member's unit of work becomes a short,
event-driven, headless **turn**. It replaces today's long interactive session
held open by a Stop hook. The human keeps three things:
- the goal and the decisions, in the architect conversation (section 7);
- the operator surface, through the CLI and API (section 4);
- the merge.

Parent documents. Where this one disagrees with them, they win unless a
section here names the amendment:
- aicrew's own:
  - [CREW-CONTRACT.md](CREW-CONTRACT.md);
  - [WORKSPACE.md](WORKSPACE.md);
  - [ONBOARDING-CONTRACT.md](ONBOARDING-CONTRACT.md);
  - the measurements in [CLIENT-WAKE-PROBE.md](CLIENT-WAKE-PROBE.md);
  - the accepted escalation proposal,
    [proposals/OPERATOR-SEAT.md](proposals/OPERATOR-SEAT.md);
- aimem's AIForge contracts at their pinned commit (START-HERE).

## The shape in brief

```text
 human ── goal, decisions ──▶ architect session (interactive, the human's own)
                                  │ epics, tasks, READY, answers  (aimem board)
                                  ▼
 aimem hub ◀──── team mode ────── members' clients (coordinator, workers)
    │ board feed (new peer read)          ▲ one headless turn at a time
    ▼                                     │
 aicrewd ── store, scheduler, operator API ── runner (aicrew-agent, member host)
    ▲                                     pull over TLS, no inbound port
    └──── aicrew CLI (operator) ──── human merges on the forge
```

What does not change:
- aimem owns tasks, the board, reservations and knowledge. aicrew owns teams,
  membership, roles, attempts, the inbox, and now turns and runners.
- The inbox stays the authority for what a member is told. A wake-up is only
  a hint, and the inbox read is the only delivery.
- The client never holds the session token or a proof. The launcher (later
  the runner) holds them and drives steps over the home's step socket.
- No secret leaves the home. The merge stays a human click.

## 1. The member turn

### 1.1 What a turn is

A turn is one headless run of the member's client in its home. It starts
with a prompt from a fixed template, acts, and ends when the client exits.
A turn belongs to one member and runs on that member's host. It is recorded
with:
- what started it;
- its outcome;
- its usage and cost.

A member's conversation can span turns, through the client's own session
resume (1.7).

Below, "the runner" is whatever runs the member's turns: the home's own
loop, `aicrew-agent serve`, until increment B2, and the runner daemon of
section 2 from then on.

### 1.2 Triggers: every trigger is an inbox message

| Trigger | Where it comes from | Message kind (new kinds in bold) |
| --- | --- | --- |
| A team message or a lifecycle event (offer, acceptance, submission, review, stop, block) | aicrewd's store, as today | the existing kinds |
| A task of a granted project becomes READY, or an offered or claimed task changes state on the board (cancelled, blocked, moved back) | aimem's board feed (A0, below), read by aicrewd | **`board.changed`** (task 5570) |
| An escalation answer from the architect | the operator API, under the architect credential (7.5) | **`escalation.answer`** |
| A forge event on an attempt's PR (a check concluded, a review comment) | the member's turn loop (later its runner) polling the attempt's PR (D5) | **`forge.event`** |
| An operator action (nudge, resume after a pause, a note) | the operator API | **`operator.note`** |

**Decision D1: every trigger is a message.** The inbox already has the
properties a trigger needs. It is durable, ordered, per member and
idempotent on send, and it is acknowledged explicitly. So one rule starts
every turn: *the member has unacknowledged messages*. There is no second
queue to keep consistent with the inbox.

This also lets the board trigger (5570) ship before the turn model. Under
today's Stop hook a `board.changed` message wakes the coordinator like any
other message. Under turns, the same message starts a turn.

A message names the task, attempt or PR and the change, never free text
from the board. Its readable text is built by aicrewd from those fields.
Duplicates are harmless: one change gives one message. A change aicrewd
has already announced is not announced again after a restart, because the
feed's cursor is stored with the announcement in one transaction.

### 1.3 When a member gets a turn

A member gets a turn when all of these hold:
- the team and the member are not paused;
- no turn of the member is running;
- its runner is online;
- the member is not in `needs_login` or `limit_reached` (3.3, 3.4);
- and one of these:
  - **(a)** the member has unacknowledged messages; or
  - **(b) worker continuation:** the member holds an attempt in `Working`,
    its last turn ended without a block or a submit, and the attempt's
    continuation budget is not spent (1.8).

Messages that arrive while a turn runs are left for the next turn. A turn
starts at most once per 5 s per member, so a burst of messages coalesces
into one turn.

**Who decides.** In the first increments (A3), the home's own loop
(`aicrew-agent serve`) applies the rule, from the pending read it already
uses (`GET /v1/crew/inbox/pending`). Until the provider profile exists
(A5), its limits come from a `turns` object in the home's `agent.json`,
with the defaults of 3.1. From B2 on, aicrewd applies it and
commands the runner. aicrewd then holds the pause, the budgets and the
one-turn-per-member rule in one place.

### 1.4 Running a turn, per client

| | Claude Code (2.1.294) | Codex (0.156) | OpenCode (1.18) |
| --- | --- | --- | --- |
| Start | `claude -p PROMPT --output-format stream-json --verbose --session-id UUID` | `codex exec --json PROMPT` | `opencode run --format json PROMPT` |
| Continue a conversation | `--resume ID` | `codex exec resume ID PROMPT` | `--session ID` |
| Model | `--model` | `-m` | `-m provider/model` |
| Permissions | `--permission-mode dontAsk --permission-prompts none` (D2), with the home's managed allow and deny rules | `--sandbox workspace-write`, approvals never asked | the home's `opencode.json` `permission` rules |
| Turn end | the `result` event, then exit | the final JSONL event, then exit | the final JSON event, then exit |
| Usage and cost | the `result` event's usage and cost | its usage events (to measure) | its step events (to measure) |
| Measured | yes: CLIENT-WAKE-PROBE, and the deny-rule smoke of `01a1171d-c51c` | no | the `serve` API yes, `run` no |

- **Probe first.** Only Claude Code's headless mode was measured. The probe
  increment (A2) measures the rows marked "to measure" and "no" before any
  adapter is built. It covers the resume, permission, end, usage and limit
  behaviour of each client, as CLIENT-WAKE-PROBE did for the wake-up.
- **Claude Code is the first client.** The adapters for Codex and OpenCode
  follow the probe (A7).
- **Never passed:** the runner never passes `--dangerously-*` flags,
  `bypassPermissions`, Codex's `--dangerously-bypass-approvals-and-sandbox`,
  or a fallback model.
- **Decision D2: permissions in a turn.** Nobody is at the terminal to
  answer a prompt, so a turn must not prompt.
  - **Recommended:** Claude Code's `dontAsk` mode with prompts set to
    `none`. A tool call that neither an allow rule nor the mode permits is
    denied and listed in the result's `permission_denials`.
    - The home's managed settings gain a `permissions.allow` list for what
      the role needs. For a code profile, that is git in the worktree, the
      project's build and test commands, `aicrew-agent`, and editing in
      `worktrees/`. The allow list is part of the worker profile's tool
      policy (8.3), so each kind of work has its own.
    - The deny rules stay above it.
    - A denial is reported in the turn record. A denial the role needed is
      a guidance or allow-list change through a PR, never a widening at
      run time.
  - **Rejected:** `bypassPermissions` with the deny rules alone. It is a
    smaller first step, but it leaves no positive boundary in the
    unattended case.

### 1.5 What a turn reads first

The runner starts each turn with one fixed prompt: the managed command
`/crew-turn` and a summary of the trigger. The summary gives the message
kinds and ids, or "continuation", and never message text. The command
teaches the order:

1. `docs/HANDOFF.md`, the member's own notes.
2. `aicrew-agent session status`: role and projects.
3. `aicrew-agent inbox`: the messages that started the turn.
4. A coordinator also reads the board for the tasks those messages name.
5. Act within the role (ROLES.md).
6. Acknowledge what was handled.
7. Update the handoff.
8. End the turn.

A fresh conversation (1.7) first reads `docs/START.md` and `docs/ROLES.md`,
as `/crew-start` does today. A resumed one does not read them again.

### 1.6 How a turn ends

A turn ends when the client exits. The runner records the outcome:

| Outcome | When |
| --- | --- |
| `finished` | the client ended its turn normally (Claude Code: a `result` event that is not an error) |
| `failed` | a non-zero exit, an error result, or output that cannot be parsed |
| `needs_login` | the client reported missing or expired provider authentication (3.3) |
| `limit_reached` | the provider's usage or rate limit, or the profile's own budget (3.4) |
| `stopped` | ended by a stop command or by the turn's time limit (default 30 minutes) |
| `interrupted` | the runner restarted while the turn ran (2.6) |

**The turn record** holds:
- turn ID, agent, team, attempt and task (when there is one);
- the trigger summary, client, model and conversation ID;
- start and end times, the outcome, and its error code;
- usage, cost and permission denials;
- a short redacted summary (4.3).

It holds no secret and no transcript.

**Member states** follow from the records and the runner's reports:
- `working`: a turn is running;
- `waiting`: nothing to do;
- `needs_login`;
- `limit_reached`;
- `paused`;
- `offline`: the runner is not polling.

### 1.7 Conversations across turns

- **Decision D4.**
  - **A worker** has one conversation per attempt. The first turn of an
    attempt starts a fresh conversation, and every later turn of that
    attempt resumes it. The next attempt starts fresh. This follows
    CREW-CONTRACT's rule that team work starts in a fresh conversation, and
    it keeps one attempt's context out of the next.
  - **A coordinator** resumes its conversation until a rotation point, then
    starts fresh from its handoff. The rotation point is 50 turns, the
    client's reported context use above 70 %, or the start of a new day,
    whichever comes first.
- **Where the conversation ID is kept.** It is nonsecret, so it goes in the
  home's `state/turns.json` and in the turn record. The client keeps the
  transcript in its own storage.
- **The team session outlives turns.** The runner holds the member's team
  session as `aicrew-agent run` does today:
  - it refreshes the handle;
  - it resumes the session when the 8-hour token ends;
  - it serves the step socket.

  Each turn's client gets `AIMEM_TEAM_SESSION` and `AICREW_AGENT_HOME` in
  its own environment, so `aicrew-agent step` and `inbox` work unchanged.

### 1.8 A worker's attempt across turns

While the worker's attempt is `Working`, a turn that ends without a block
or a submit earns a continuation turn (1.3 b). Three limits bound this, each
in the provider profile (3.1):
- **Continuations:** at most 20 per attempt by default.
- **Budget:** a cost ceiling per attempt.
- **No progress:** after three consecutive turns with no step and no
  progress signal, the attempt is **stalled**.
  - The signal is the delivery kind's `progress` (8.1). For `code-pr` it
    is a new commit on the attempt's branch, which the runner reads from
    the worktree's HEAD. For `ops` it is a new entry in the command log or
    a recorded finding or plan.
  - Either way the runner reads a fact, and no model judges it.
  - A stall:
  - sends a lifecycle message to the coordinator;
  - stops continuation turns until a message arrives for the worker.

A submitted or blocked attempt gets no continuation turns. It waits for
messages: the coordinator's review, a forge event or an answer. The
coordinator decides what a stall means, as it decides any block.

### 1.9 Concurrency

- **One turn per member.** At most one turn of a member runs at a time. In
  aicrewd's store this is a uniqueness rule on running turns by agent, as
  execution capacity is today.
- **Slots per runner.** A runner runs at most its configured number of
  turns at once. The default is one. The operator raises it for a host with
  several members.
- **Across the team,** the coordinator and the workers run in parallel,
  each in its own home. No team-wide lock exists or is needed. The
  reservations and the store's steps already serialize what must be
  serialized.

### 1.10 Retiring the Stop hook

1. **Now:** the Stop hook and `wait-inbox` keep working. A `board.changed`
   message (A1) wakes an interactive member through them.
2. **With `aicrew-agent serve` (A3):** a home run by the turn loop has
   `wake.mode: "turns"` in `agent.json`. `join` then writes its managed
   settings without the Stop hook. The keep-alive and its idle "waiting"
   turns are gone for that home. `check` reports a home whose mode and
   settings disagree.
3. **Decision D3:** `aicrew-agent run` stays for a human-attended member, for
   example the pilot's watched coordinator, and keeps the Stop hook. When
   B2 has run unattended crews for a release, the operator decides whether
   the interactive mode stays as an option or goes.

## 2. The runner

### 2.1 What it is

The runner is `aicrew-agent runner`, a daemon that runs as one OS user on a
member's host and serves the agent homes of that user. For each home it
does what `aicrew-agent run` does for one client today:
- it holds the team session and refreshes the handle;
- it serves the step socket;
- it starts the client, now once per turn.

Isolation between members stays an OS account per member (WORKSPACE,
"Credentials"). So in the normal case the runner serves one member.
**Decision D8:** a runner serves several homes only when the operator
enrols it with `--shared-user`, accepting that those members are not
isolated from each other. `team state` shows that on every member of the
runner.

### 2.2 Enrolment

- **The operator invites the host:**
  `aicrew runner invite --label host-a --output code`. This is a new
  invitation purpose, `runner`, in the existing invitation store. It keeps
  the store's rules:
  - only a digest is kept;
  - the code is answered once and written only where `--output` names;
  - it expires (24 hours by default, at most 72);
  - it is redeemed once;
  - its routes are rate-limited;
  - it never appears in a log or a refusal.
- **The host enrols:**
  `aicrew-agent runner enrol --url URL --tls-trust-mode ... --tls-trust-value ...`.
  This runs as the member's OS user and reads the code at a hidden prompt
  or from standard input, never as an argument.
  - aicrewd answers with the runner's ID and a **runner credential**: a new
    bearer type, held only as a digest on aicrewd.
  - The runner writes the credential once to an owner-only file in its
    state directory: `~/aicrew/runner/creds/aicrew.<service>.runner`, or
    `%LOCALAPPDATA%\aicrew\runner\creds\...`. It never prints it.
- **No aimem identity.** A runner is not an aimem identity. It proves the
  host, and the members still prove themselves.
- **Binding a member to its runner.** `aicrew-agent runner adopt --home H`
  binds an existing home. The home's own session entry proves the agent,
  and the runner credential proves the host. aicrewd records the binding,
  one runner per agent. A member moves to another host only by the
  operator unbinding it first.
- **Revocation and rotation:**
  - `aicrew runner list|revoke|rotate` manage runners.
  - A revoked runner's next poll is refused.
  - Its members become `offline`. Their sessions and attempts are
    untouched, as liveness never frees capacity (CREW-CONTRACT).

### 2.3 The connection

The runner pulls. The host needs no inbound port, and aicrewd's existing
TLS listener and trust bindings carry everything.

- **`POST /v1/runner/poll`.** A long poll of at most 30 s. It returns the
  runner's pending commands, oldest first.
- **`POST /v1/runner/events`.** Reports turn starts and ends, member
  states, command results and acknowledgements. Each event carries its own
  ID, so a retried report is recorded once.
- **Authentication:** both routes take `Authorization: Bearer` with the
  runner credential. They reach no member or operator operation, and those
  routes refuse the runner credential.
- **Delivery:** commands are rows in aicrewd's store. Each has an ID and,
  for a turn, a lease.
  - A command is delivered until it is acknowledged, so delivery is at
    least once.
  - The runner executes each command ID once.
- **Liveness:** a poll counts as the runner's heartbeat. After three missed
  polls (90 s) its members are `offline`.

### 2.4 Commands aicrewd sends

| Command | What the runner does |
| --- | --- |
| `ensure-home {label, team, role, profile, invitation_code, aimem_hub, url, trust}` | Runs `aicrew-agent join` for a new home with its worker profile (8.3), with the code on standard input. Each credential the profile lists must already be on the host (2.5); otherwise the result is `needs_credential`, naming each missing one by purpose. |
| `refresh-home {agent, profile}` | Reruns `join` on the home after an upgrade or a profile change (managed files, hooks, deny and allow rules, skills, MCP servers), with no code. |
| `run-turn {turn_id, agent, trigger_summary, profile, lease}` | Starts the turn (section 1) and reports its start and end. |
| `stop-turn {turn_id}` | Ends the turn's process tree and reports `stopped`. |
| `report {agent?}` | Runs `aicrew-agent check` and reports versions, the client's login state, capabilities and the home's readiness. |
| `fetch-log {turn_id}` | Sends that turn's redacted transcript, capped (4.3). |

**What aicrewd can never ask:**
- to upgrade the runner (the human runs the one-liner);
- to run a command of its choosing;
- to deposit, read or move a credential;
- to change the deny or allow rules;
- anything outside the homes bound to this runner.

### 2.5 Credentials on the host stay human or member steps

The runner never receives an aimem credential, a forge token or a provider
key from aicrewd. Each is put on the host by the human or the member's
operator:
- the aimem credential through `join`'s provisioning;
- a forge token or provider key through `join --cred`;
- a subscription login through the client's own login (3.3).

A home that lacks one reports `needs_credential` or `needs_login` with the
exact command. aicrewd is not a credential courier. The deposits of task
`01a105fa-339b` may later change that, under their own custody design.

### 2.6 Lost connection and restarts

- **The connection drops.**
  - A running turn runs to its end.
  - The runner keeps its events in a bounded, nonsecret journal in its
    state directory and sends them on reconnect.
  - No new turn starts without a command.
  - When a turn's lease runs out with no end report, aicrewd records it as
    `lost`. It never starts that member on another host, because the home
    lives on this one.
- **The host restarts.** The runner is installed as a user service:
  - `systemd --user` with lingering on Linux;
  - a launchd agent on macOS;
  - a scheduled task at start-up on Windows, under the member's account.

  On start it reads its journal and reports each turn that was running as
  `interrupted`. It resumes each member's session with a new proof, as
  `run` does. aicrewd then applies the turn rule again.
- **aicrewd restarts.** Commands and turns are store rows, so nothing is
  lost. Runners keep polling and are answered when the service is back.

## 3. The provider profile

The provider profile is one facet of the member's worker profile (8.3): the
client, the model, the auth mode and the limits. A member may override this
facet alone, because a subscription login belongs to its host.

### 3.1 What aicrewd keeps per member

```json
{"client": "claude", "model": "MODEL", "auth_mode": "subscription",
 "credential_ref": null,
 "limits": {"turn_timeout": "30m", "turn_budget_usd": 2, "attempt_budget_usd": 20,
            "daily_budget_usd": 50, "max_continuations": 20},
 "revision": 3}
```

- **`client`:** `claude`, `codex` or `opencode`. Mixed teams are allowed.
- **`auth_mode`:** `subscription` or `api_key`. With `api_key`,
  `credential_ref` names the home's credential (3.2).
- **Changes:** the operator sets it with
  `aicrew member profile set --agent ... [--model ...] ...`, under its
  revision.
- **Delivery:** it reaches the runner in every `run-turn`. The runner
  refuses a turn whose client the home has not wired, with a `report`
  naming what is missing.
- **It authorizes nothing.** As CREW-CONTRACT says of model and client, the
  profile is configuration. It is copied into each turn record and audit.

### 3.2 An API key

- **Deposit:** the key is a home credential, deposited with
  `aicrew-agent join --cred` under the usual naming, for example
  `anthropic-com.<account>.api`. It is never printed and never an argument.
- **Read only by the client.** The key must not be in the environment that
  the client's tool processes inherit, where a model's shell could print
  it.
- **Decision D6, recommended:** for Claude Code, the home's managed
  settings name an `apiKeyHelper`:
  - the helper is `aicrew-agent provider-key --home H`;
  - Claude Code runs it itself to get the key;
  - the deny rules gain `*provider-key*`, so the model's own Bash cannot
    call it. This is a text rule, with WORKSPACE's stated limit.
- **Verified first:** the probe (A2) checks that the key does not reach
  the tool processes' environment, before A5 builds on it.
- **Other clients:** Codex and OpenCode keep a key in their own credential
  stores (`codex login --with-api-key` from standard input,
  `opencode auth`). For them the runner feeds that store once from
  `creds/`, at `refresh-home`, through standard input. The key then stays
  in client-supported storage, as WORKSPACE allows for provider
  authentication.

### 3.3 A subscription login

The login is a one-time human step on the host, as the member's OS user,
in the home:
- Claude Code: `claude`, then `/login`;
- Codex: `codex login`;
- OpenCode: `opencode auth login`.

It stays in the client's own storage, which WORKSPACE exempts from
`creds/`.

**When it is missing or expired:**
- The runner sees the client's authentication error, at a turn or at a
  `report`.
- It reports `needs_login` with the exact command. No turn starts until a
  later `report` finds the login, or the operator resumes the member.
- It never retries in a loop, and never falls back to an API key or to
  another member's login.

### 3.4 Limits

- **Provider limits.** When the client reports a usage or rate limit, the
  turn ends as `limit_reached`, with the reset time when the client gives
  one.
  - The member gets no turn until that time.
  - With no reset time, it waits 1 hour, then 2, 4 and 8 hours between
    single probe turns.
  - Nothing falls back to another model, provider or credential (WORKSPACE:
    no implicit fallback).
- **The profile's own budgets.** A turn, attempt or daily budget that is
  spent is `limit_reached` with `kind: budget`:
  - **Turn budget:** passed to Claude Code as `--max-budget-usd`.
  - **Attempt and daily budgets:** applied by the scheduler before a turn
    starts.
  - **Lifting it:** the member waits until the operator raises the budget,
    or the day turns.
- **Who learns about it:** the coordinator gets a lifecycle message when a
  worker with an open attempt reaches a limit, so it can plan around it.

### 3.5 Token and cost accounting

**Per turn,** the turn record carries:
- input, output, cache-read and cache-write tokens;
- the client's own cost figure;
- the model.

Claude Code's `result` event gives these. The probe (A2) confirms the
fields for each client.

**Subscription cost:** under a subscription the figure is the list-price
equivalent. It is marked `notional`, since no money moves per turn.

**Reading it:** aicrewd aggregates per member, per task, per attempt and
per day, and the operator reads it through the API (4.2). It is a record
for planning, never a bill.

## 4. The operator surface

### 4.1 CLI and API only

- **One API.** Everything the operator does is a route of the operator API
  under `/v1/admin/`, with the operator credential, and an `aicrew`
  command over it.
- **Definitions:** the JSON is defined in `internal/opapi` and documented
  in DEVELOPMENT, as today.
- **No UI** is built until the API is stable (4.4).

### 4.2 What the operator reads and does

| Need | Command (route) |
| --- | --- |
| Team state | `aicrew team state --team-name T`: each member's role, runner and host, state (1.6), current turn, open attempt, unacknowledged messages, profile summary and pause; the team's pause |
| Turns | `aicrew turns list [--agent A] [--attempt X] [--since D]`; `aicrew turns show ID`: the record and its summary |
| A turn's log | `aicrew turns log ID --output FILE`: fetched from the runner (`fetch-log`), redacted, not kept on aicrewd |
| Costs | `aicrew costs --team-name T --by member,task,day [--since D]` |
| Escalations | `aicrew escalations list [--open]`, `show ID`, and `answer ID` (the architect, 7.5): the coordinator's requests and their answers |
| PRs awaiting merge | `aicrew merges --team-name T`: attempts the coordinator reviewed READY, with the PR, its head and the verify state. The merge itself stays on the forge, by a human. |
| Control | `aicrew team pause\|resume`; `aicrew member pause\|resume\|nudge`; `aicrew turns stop ID`; `aicrew member profile set` |
| Runners | `aicrew runner invite\|list\|revoke\|rotate\|unbind`; `aicrew runner set --caps ...` (the host's capabilities, 8.2) |
| Worker profiles | `aicrew profile list\|show\|set`, export and import (8.3); `aicrew member profile set --agent A --profile P` |
| Ops approvals | `aicrew approvals list [--open]`, `show ID`, `approve\|reject ID` (8.4); with the full operator credential only (D18) |

Pause is a scheduler state. A paused team or member gets no new turns.
Running turns end normally, unless `turns stop` ends them. Pause never
touches attempts, reservations or sessions.

### 4.3 Turn logs and secrets

- **The full transcript stays on the host,** in the client's own storage
  and the home's `logs/`.
- **What aicrewd receives** with each turn end is a summary of at most
  8 KiB:
  - the tail of the result text;
  - the names of the tools used;
  - the permission denials;
  - the outcome.
- **Redaction.** Before anything leaves the host, both the summary and a
  fetched log are redacted of known token shapes: aicrew's operator,
  session and runner tokens, aimem's tokens, forge tokens and provider
  keys. Redaction is best-effort.
- **Decision D7:** transcripts are never stored centrally. The exception
  is an ops member's command log, which is stored on aicrewd (D17, 8.4).

### 4.4 What "stable API" means

The TUI, then a web UI, are built only on an API that meets all of these:
- every route and its JSON are documented, decoded strictly, and covered by
  a contract test on the `opapi` types;
- within `/v1`, changes are additive only: new routes, and new optional
  fields;
- a removal is announced in a release's notes and served with a notice for
  at least one minor release, as the 0.5.0 removals are;
- lists share one shape for paging and filtering;
- one full release has passed in which the CLI was the only client and no
  route changed incompatibly.

A web UI also needs its own authentication design. The operator token does
not belong in a browser, and OPERATOR-SEAT §9 D3 already points at
passkeys.

## 5. Security

### 5.1 Credentials and who holds them

| Credential | Held by | Reaches |
| --- | --- | --- |
| Operator token | the operator's machine; aicrewd keeps its file | the operator API |
| Runner credential (new) | the runner's state directory, owner-only | `/v1/runner/*` for its own bound members |
| Architect credential (new) | the architect directory's owner-only credential file | the operator API's reads and escalation answers, nothing else (D10); never an ops approval (D18) |
| Ops identities (SSH, PowerShell remoting) | the ops member's home `creds/`, on the operator-prepared host | the hosts the profile names, limited there by sudoers or JEA (D19) |
| Member session token | the runner's memory | the member's session API, as today |
| Member aimem credential | the home's `aimem/` | the member's team-mode and personal-mode aimem access |
| Forge token, provider key | the home's `creds/` | the forge; the provider |
| Subscription login | the client's own storage | the provider |
| Invitation code (member or runner) | one answer, then the person or the `ensure-home` body | one redemption |

### 5.2 What a runner can and cannot do

**It can:**
- poll for and run the commands of section 2.4 for the members bound to it;
- report their turns and states;
- hold their team sessions as their launcher;
- see each turn's output on its own host.

**It cannot:**
- act for a member not bound to it (aicrewd refuses its events and polls
  for one);
- reach the operator API, another team or another runner's members;
- receive a credential from aicrewd, apart from a one-time invitation code
  inside `ensure-home`;
- change a member's profile, pause, budgets or rules;
- review, confirm delivery or merge, except as its members' own clients,
  within their roles.

### 5.3 A compromised host

It reaches what its OS users hold:
- the homes' aimem credentials, forge tokens and provider keys or logins;
- the runner credential;
- the session tokens in the runner's memory.

With them it can act as those members, within their team roles, until each
is revoked. It cannot reach:
- other hosts' members;
- aicrewd's operator authority;
- aimem's administration;
- another member's credentials on another OS account.

**Recovery is revocation:**
1. `aicrew runner revoke`;
2. the members' aimem credentials at the hub;
3. their forge tokens and provider keys at their services;
4. then the memberships, if needed.

Members that share one OS user share one fate (D8).

### 5.4 A compromised aicrewd

It can:
- command turns on the runners;
- write messages members will read, as it can today;
- record an escalation answer the architect never gave.

The protocol gives it no way to get a member's secret: no command returns
one. A fetched log is redacted, but a secret the model itself wrote into
the transcript could still leak through redaction's limits. The runner
builds every prompt from its fixed template and never runs a command of
aicrewd's choosing, so aicrewd's reach is what a message can persuade a
member to do. That is the same as today.

### 5.5 What carries over, and what gains weight

- **The deny rules apply in headless turns.** The c51c smoke ran Claude
  Code headless, and the home's project settings applied. D2 adds an allow
  list beneath them.
- **OS isolation** stays one account per member, with a runner per
  account.
- **No `--dangerously-*` flag** is ever passed. No permission is widened at
  run time.
- **Untrusted input:** repository content, board text and messages are
  untrusted input to a model, as today.

Without a human watching each session, these boundaries carry more weight.
So the floor stays human: merge, release, deployment, and any change to
rules or credentials.

### 5.6 Kinds of work that reach live systems

Section 8's ops work adds its own boundaries:
- **Placement** on operator-prepared hosts only. The crew never
  provisions network access or credentials, and a host's capabilities are
  the operator's word, never the host's own claim (8.2).
- **Its own OS account or VM** for each ops member, and credentials scoped
  to the hosts its profile names.
- **A stricter policy per kind.** Commands are sorted into read-only,
  mutating and forbidden classes. Mutating commands run only as approved
  exact commands in that apply turn. The target hosts enforce the same
  limits themselves, with sudoers or JEA (D19).
- **A human approval** of every plan before any mutating command, bound to
  the plan's digest, given with the full operator credential and never
  from an AI session (D18).
- **A full command log** of every ops turn on aicrewd (D17).

A compromised ops host reaches the hosts its identities serve, within what
those hosts allow the identity. That is why the host-side limits of D19,
not the client's text rules, are the boundary.

## 6. What we are not, and the increments

### 6.1 What we are not

**Idea sources only.** hands (the external reviewer) and OpenChamber are
not models to copy. We adopt their ideas where they fit:
- both auth modes and provider profiles;
- per-session goals and states;
- pairing a host with a one-time code;
- cost accounting per run;
- forge events as triggers;
- one place to observe the crew.

**What we do not build:**
- a chat-centred, multi-device workspace;
- a runner bound to a single engine;
- scheduled prompts;
- multiple runs fused into one result;
- declared multi-step pipelines.

**Agile, not a declared pipeline.** Their orchestration declares a
waterfall up front. Ours is a team with roles and a board on aimem shared
by humans and agents:
- work moves in short task, PR, review and merge cycles;
- the plan lives on the board and changes in the architect conversation;
- the human acts at the merge.

### 6.2 Non-goals of this document

- Implementing any increment.
- Any UI.
- VM lifecycle (the epic's last, optional line).
- Delivery kinds other than `code-pr` and `ops`, beyond naming the contract
  they plug into (8.1).
- Changing aimem. The aimem prerequisite below is named for the aimem
  implementation session to take up; aicrew does not file it on the aimem
  board.

### 6.3 The increments

Each increment is one task and one PR line. They are filed in the epic
once this document is reviewed READY.

| # | Increment | Depends on | aicrewd wire change |
| --- | --- | --- | --- |
| A0 | **aimem prerequisite, owned by the aimem session:** a peer read `board.read` for the peer's teams' granted projects. It is a cursor feed of task state changes (task, from, to, revision, time), read once per tick for all of the peer's teams, under its own single-operation credential like `team.read`. | aimem | none in aicrew |
| A0b | **aimem prerequisite, owned by the aimem session:** a typed field on a task for the capability it requires (8.2, D15). Until then, a project's process names one capability for its work. | aimem | none in aicrew |
| A0c | **aimem prerequisite, owned by the aimem session:** a team-mode write of a hub document, for reports (8.4). Until then, the report is the submit's text, mirrored by the coordinator as a comment. | aimem | none in aicrew |
| A1 | **01a11c8d-5570, board wake:** aicrewd reads the feed each reconcile tick and writes `board.changed` messages to the coordinator, with the cursor in the same transaction. It works under today's Stop hook. | A0 | new message kind |
| A2 | **Turn probe** (a document, like CLIENT-WAKE-PROBE): measures, for each client, a headless turn with resume, `dontAsk` with an allow list, the end and limit events, and usage and cost. It also confirms `apiKeyHelper` isolation and the ops policy shapes: PowerShell cmdlet rules, an SSH host allow list, and per-turn exact-command allow rules (8.4). | none | none |
| A3 | **`aicrew-agent serve --home H`:** the local turn loop for one home and Claude Code. Its triggers are pending messages and worker continuation; it applies the limits and stall rule, writes turn records in the home's `logs/`, and sets `wake.mode: "turns"` (no Stop hook). | A2 | none |
| A4 | **Turn reports:** a member-session route that records turns and accounting on aicrewd, with `aicrew turns` and `aicrew costs`. | A3 | yes |
| A5 | **The provider profile** on aicrewd, API keys through `apiKeyHelper`, and `needs_login` and `limit_reached` handling. | A4 | yes |
| A6 | **Forge events:** the turn loop (the runner from B2 on) polls the PRs of its members' attempts (check conclusions, review comments) with the member's own forge credential and reports them; aicrewd sends `forge.event` to the attempt's worker and coordinator. | A4 | yes |
| A7 | **The Codex and OpenCode turn adapters,** one PR each. | A2, A3 | none |
| B1 | **Runner enrolment:** the `runner` invitation purpose, the runner credential, `poll` and `events`, and `aicrew runner invite\|list\|revoke\|rotate`. | A4 | yes |
| B2 | **The scheduler moves to aicrewd:** `adopt`, `run-turn`, `stop-turn` and `report`, member states and `offline`, leases, and the journal across a lost connection. | B1, A5 | yes |
| B3 | **`ensure-home` and `refresh-home`,** and installing the runner as a user service from the one-liners. | B2 | yes |
| C1 | **`aicrew team state`; pause, resume and nudge** of a team or a member. Until B2, the turn loop reads the pause from the member's session status before each turn. | A4 (B2 for runner fields) | yes |
| C2 | **01a11c8d-5598, escalations:** `aicrew-agent escalate`, the escalation records, the architect credential, `aicrew escalations list\|show\|answer`, the `escalation.answer` message, and the comment mirrors (7.5, D10, D14). | D1 | yes |
| C3 | **`aicrew merges`:** PRs awaiting the human merge. | A4 | yes |
| C4 | **`aicrew turns log`,** through `fetch-log`. | B2 | yes |
| E1 | **Delivery kinds:** the CREW-CONTRACT amendment and the code that make an offer name `delivery {kind, ...}`, with `code-pr` carrying today's repository fields and checks unchanged (8.1). | A4 | yes |
| E2 | **Worker profiles:** the record on aicrewd and `aicrew profile`. `join` generates the home's managed files from a profile, and `check` verifies the home against it. The provider profile (A5) becomes its facet. | A5 | yes |
| E3 | **Capabilities and placement:** `{kind, scope}` capabilities; host capabilities set by the operator on the runner; matching at offer (`capability_missing`). | E2, B2 | yes |
| E4 | **The ops interactive mode:** the `plan` and `plan-review` steps, plan records, `aicrew approvals`, per-turn exact allow rules for the apply, the ops command log on aicrewd, and the Linux command classes. | E1, E3, A6 | yes |
| E5 | **Windows ops:** the PowerShell command classes, and the JEA guidance for the operator's hosts. | E4 | none |
| E6 | **The ops declarative mode:** a configuration-repository PR as the deliverable, with the apply after the merge as an approved step. | E4 | yes |
| D1 | **The architect kit,** `aicrew architect init` (7.2). It is 4a15's first increment. | none | none |
| D2 | **ROLES.md:** the coordinator does not talk to a human, and escalates through C2's path. | C2 | none |

**Where the existing tasks sit:**
- 5570 is A1, and ships as an inbox message under the Stop hook, before
  turns exist.
- 5598 is C2.
- 4a15's first increment is D1, which needs no change to aicrewd's wire.

**What can start at once:** A2 and D1 can start as soon as this document is
merged, and C2 right after D1. A1 waits on aimem's A0. The E line follows
the turns and the runner, because a kind of work that reaches live systems
needs the turn records, the profile and the placement first.

## 7. The architect role

### 7.1 What the architect session is

The architect is the operator's own interactive Claude Code session, on a
strong model, in an **architect directory**. It is not a member home:
- no aicrew membership or team session;
- no forge credential;
- no Stop hook, and no runner;
- the control plane does not run it.

The operator explains the goal, hands over drafts and data, and plans the
work with it.

The architect works on the aimem board in **personal mode**, as the human's
own aimem user. It:
- creates the epic and the tasks;
- writes their acceptance criteria, size, dependencies and order;
- moves them to READY on the operator's word;
- answers the coordinator's escalations.

It never claims, implements, reviews or merges.

### 7.2 How the operator starts it

`aicrew architect init --dir DIR --project P [--project P2 ...]` (D1) writes
the directory's managed files, under the same rerun rules as a home's:
- `AGENTS.md` and `CLAUDE.md`, pointing to `docs/ARCHITECT.md`;
- `docs/ARCHITECT.md`, the guidance below;
- a `.mcp.json` aimem entry for the **user's own** aimem installation, with
  no `AIMEM_STATE_DIR` override;
- the commands `/arch-plan`, `/arch-task`, `/arch-ready` and
  `/arch-escalations`.

It also writes managed `.claude/settings.json` deny rules like a home's,
covering the directory's `creds/`.

From C2 on, the operator issues the architect credential (D10) with
`aicrew architect credential issue --output DIR/creds/aicrew.<service>.architect`.
The managed settings' `env` names that file as
`AICREW_OPERATOR_TOKEN_FILE`. The architect's `aicrew` commands then
never name the path, and the deny rules on `creds/` still hold.

The operator then runs `claude` in that directory, as they would start any
session.

### 7.3 The guidance it carries

- **Planning:** goal, then epic, then tasks. Each task is one PR line, of
  size S or M. An L or XL task is split before it is written. Dependencies
  are explicit by ID, and cross-project ones are checked rather than
  assumed.
- **How a task is written so a worker can take it:**
  - the objective, as what done looks like;
  - acceptance criteria a reviewer can verify;
  - explicit non-goals;
  - dependencies, size, priority and the next action;
  - the kind of work and the capability it needs, where its project's
    process does not already fix them (8.2);
  - for ops work, the acceptance criteria as checks a read-only command can
    run;
  - the evidence that motivated it.

  This is the shape the tasks on this board have.
- **READY means:**
  - the criteria are testable;
  - every dependency is DONE, or ordered before the task;
  - the size is at most M;
  - no decision is open.

  The architect sets READY only when the operator says so in the
  conversation. The coordinator keeps its own triage between BACKLOG and
  READY (aimem's team-mode triage).
- **Limits:** no secrets in the conversation or on the board. Never act on
  a member's terminal or home. The floor (merge, release, deploy, rules and
  credentials) is the human's to decide, never the architect's.

### 7.4 Board rights

The architect uses the human's own aimem user, which already creates epics
and tasks and sets their state in the projects it may write. The team's
profile needs **no new right**, and aimem needs no change.

**Decision D9:** the architect writes as the human, since the human is in
the conversation. The alternative is a separate aimem user for the
architect. It gives distinct audit, but needs its own credential and
grants. Recommended: the human's user, revisited if the audit needs to
tell them apart.

### 7.5 Escalations: from the coordinator to the architect and back

This is task 5598's shape, which C2 implements.

**Where the record lives.** aicrewd's store holds the escalation, and the
aimem task carries a mirror of it. That is OPERATOR-SEAT's D2(a): aicrew is
the operational record, and the task comment is the record of decision.
A task comment alone cannot be the record, for two reasons:
- **Held tasks.** aimem refuses every coordinator write, comments
  included, on a task under reservation (`task_held`, aimem's
  DESIGN-AIFORGE-PILOT-1 §4). That is exactly the task a blocked attempt is
  about.
- **Authority.** A comment proves only who wrote it on the board. An answer
  must come from the architect alone, which a credential checks more
  simply than a comment's author.

The flow:

1. **The request.** The coordinator runs `aicrew-agent escalate`
   through its launcher, like a step. aicrewd refuses the request from any
   role but coordinator.
   - The request carries the task and, optionally, the attempt.
   - It also carries the fields of OPERATOR-SEAT §3: category, question,
     two to four options with their consequences, recommendation, who is
     blocked, and urgency.
   - aicrewd records it and answers its ID.
   - The coordinator mirrors it as a task comment headed
     `[escalation.request ID]`. If the task is held, the comment waits
     until the hold ends, and the coordinator says so in its handoff.
   - The coordinator then carries on with other work. A blocked worker's
     attempt uses the existing `block` step.
2. **Where the architect looks.** The architect's `/arch-escalations` runs
   `aicrew escalations list --open`.
   - **Decision D10:** the architect uses a separate **architect
     credential** on aicrewd's operator API. It reads team state, turns,
     costs, escalations and merges, answers escalations, and does nothing
     else. The full operator token, which issues invitations and
     credentials, never enters an AI session.
   - Until C2, the architect reads the task's comments through aimem
     directly.
3. **The answer.**
   - The architect runs
     `aicrew escalations answer ID --decision ... --rationale ...` with that
     credential, using §3's answer fields. aicrewd records it against the
     request.
   - The architect also writes the answer on the task, in personal mode,
     headed `[escalation.answer ID]`, as the board's record of the
     decision.
   - An `[escalation.answer]` comment without the recorded answer authorizes
     nothing.
4. **The answer returns.** In the answer's own transaction, aicrewd writes
   an `escalation.answer` message to the coordinator. It also writes it to
   the member the request names as blocked (OPERATOR-SEAT §9 D4). The
   message starts their turns.
5. **No answer in time** decides nothing. The request stays open, and the
   coordinator offers other work meanwhile (OPERATOR-SEAT D6).

**Decision D11: how this relates to OPERATOR-SEAT.**
- **What changes:** the architect replaces the seat's non-AI client as the
  receiver of escalations.
- **What is kept:** OPERATOR-SEAT's categories, its floor and its record
  fields.
- **What waits:** its authority receipts (second-factor proof that no
  agent session can produce) are deferred.
- **Until the receipts exist,** an answer in a floor category records that
  the human decided it in the conversation. The merge stays a human click
  on the forge in every case.

**Decision D12: pause and resume.**
- 5598's second criterion makes pause and resume messages that the
  coordinator honours. Under the control plane they are the scheduler's
  state instead (4.2, C1). A paused team gets no turns at all, which does
  not depend on a model obeying a message.
- Recommended: amend 5598's criterion 2 to point at C1.

### 7.6 The first increment

D1, the architect kit, needs no change to aicrewd's wire. It makes today's
practice reproducible: the monitoring session has acted as architect by
hand since 2026-10-05. D2 (ROLES.md) follows with C2, once the escalation
path exists to be named.

## 8. Kinds of work, capabilities and worker profiles

The operator's additions of 2026-10-09 (task comments seq 424 to 429). A
crew is not only a development team. It may set up a server, parse logs or
write a research note. This section keeps the core the same for every kind
of work, and fixes the one non-code kind the operator named: ops.

### 8.1 One core, many delivery kinds

These are the same for any work:
- roles and the board;
- offers and attempts;
- submit, review and confirmed delivery;
- the inbox, turns and the runner.

What differs is the **delivery kind**: what a task's deliverable is, how it
is verified, where the human gate sits, and what counts as progress. The
project's pinned process defines its kinds, as it defines the development
process today. A project whose process names no kind is `code-pr`, as
every project is today. A kind is a named contract:

| Field | What it says | Code via a pull request (the first kind) |
| --- | --- | --- |
| `kind` | the name | `code-pr` |
| `target` | where the work happens | a repository the hub binds to the project |
| `deliverable` | what `submit` names as `result_ref` | the PR URL |
| `verification` | the checks before review | CI on the head; the review at level high |
| `human_gate` | the human step before delivery | the merge |
| `evidence` | the confirmed delivery's `{kind, ref}` list | `reviewed_head`, `human_merge`, `post_merge_ci` |
| `progress` | the signal the stall rule (1.8) reads | a new commit on the attempt's branch |
| `needs` | the capability and credentials a worker must have | `code: forge HOST`, write access to the repository |

Most of the attempt already fits. `result_ref` and the delivery evidence
are `{kind, ref}` values today (CREW-CONTRACT, "Attempt steps"). The parts
that assume code are these:

| Forge-specific today | Where | How it becomes the `code-pr` kind |
| --- | --- | --- |
| An offer must name a `repository`, which must equal the hub's binding, or it is refused as `repository_mismatch`. A project with no repository cannot be offered at all. | aicrewd, `granted` in the offer and claim routes; CREW-CONTRACT, "The attempt's repository" | The offer names a `delivery {kind, ...}`. `code-pr` carries today's six repository fields under the same check. Another kind carries its own target. The crew-contract amendment is increment E1. |
| A worker's capabilities are forge hosts and repositories with their access, and an offer is refused `capability_missing` without one. | `aicrew-agent check`'s report; `POST /v1/crew/capabilities` | A capability becomes `{kind, scope}` (8.2). Today's report is the `code` kind's capabilities. |
| `aicrew-agent clone`, `repos/`, `worktrees/`, and the forge credential (`join --cred`) | the member's home | These are the `code-pr` kind's workspace and credential. Another kind declares its own in the worker profile (8.3). |
| `/crew-review`'s code review, and the delivery evidence | ROLES.md, the managed commands | These are the `code-pr` kind's `verification` and `evidence`, read from the process instead of being written into ROLES.md. |
| The deny rules' git patterns | the managed settings (`01a1171d-c51c`) | The rules on credential files and TLS weakening apply to every profile. The rules on git's global and system configuration are the code profiles' policy (8.3). An ops profile adds its own classes (8.4). |
| The stall rule's commit check; forge events | 1.8; 1.2 | These are the `code-pr` kind's `progress` and triggers. |

The process repository and its pin (`aicrew-agent digest`) are not forge
delivery. They are how any project's process is fixed, and they stay as
they are.

**Not designed here:** delivery kinds beyond `code-pr` and `ops`, such as a
hub document, a dataset, a report or a research note. Each plugs into the
contract above when a project's process needs it.

### 8.2 Capabilities and placement

- **What a task needs.**
  - A task requires a capability such as `ops: network-x` or
    `code: forge github.com`.
  - **Decision D15:** the requirement comes from the project's pinned
    process, which names the kind and capability of the project's work. A
    task narrows it only through a typed task field.
  - aimem tasks have no such field today, so that field is an aimem
    prerequisite (A0b), named here for the aimem session. Until it exists,
    a project's work has one capability, and a team that needs two kinds
    of work uses two projects. That is the natural shape anyway: an ops
    network is its own project.
- **What a worker has.** A member's capabilities are those its worker
  profile declares (8.3), limited to those its runner's host provides.
  - **Host capabilities are the operator's word.** The operator records
    them on the runner at enrolment or later (`aicrew runner set --caps
    ops:network-x`), after preparing the host.
  - **A host does not assert its own capabilities.** A compromised host
    must not gain work by claiming a network.
- **Matching.**
  - The coordinator sees each member's profile and capabilities, and never
    a host (`GET /v1/crew/capabilities` grows to carry them). It offers a
    task only to a member whose capabilities cover the task's.
  - aicrewd refuses any other offer as `capability_missing`, as it does
    today for a forge repository.
- **Placement.**
  - The control plane runs a member only on its bound runner (2.2). An ops
    member's runner is on a host the operator prepared with the network
    identity (VPN or ZTNA) and the credentials for that network.
  - The crew never provisions network access, a VPN profile or a
    credential. A host that lacks one reports `needs_credential` (2.5).
- **Isolation.** An ops member runs in its own OS account, or its own VM,
  never sharing an account with a code member (D8's `--shared-user` is
  refused for an ops profile). Its credentials are scoped to its kind: an
  SSH identity for the hosts it serves, no forge write token unless its
  declarative mode needs one.

### 8.3 Worker profiles

A **worker profile** is a named, versioned record of what a member is made
of. Examples:

| Profile | Capability | Credentials it needs | Skills and MCP | Policy |
| --- | --- | --- | --- | --- |
| `dev-go` | `code: forge github.com` | a forge write token | oh-code-review and the Go skills; the aimem MCP | the code deny rules (c51c) and the code allow list (D2) |
| `ops-mail` | `ops: network-x` | an SSH identity for host M | the ops skills; the aimem MCP | the ops command classes for Linux (8.4) |
| `ops-win` | `ops: network-y` | a PowerShell remoting identity for the allowed hosts | the ops skills; the aimem MCP | the ops command classes for Windows (8.4) |
| `research` | `research` | none | the hub documents and the aimem MCP | no shell beyond reading files |

- **Fields:**
  - name and revision;
  - capabilities;
  - the delivery kinds it takes;
  - the credentials it needs, by purpose, never by value;
  - skills: from the ai-skills set at a pinned version, plus the project's
    own;
  - MCP servers and client plugins;
  - the tool policy: deny rules, allow rules and, for ops, the command
    classes;
  - the provider facet: section 3's provider profile, now one part of the
    worker profile.
- **Per member.** A member names one profile. It may override only the
  provider facet (model, auth mode, credential reference), because a
  subscription login is a fact of its host.
- **Where it lives (decision D16):** on aicrewd, edited by the operator
  through the API and CLI (`aicrew profile list|show|set`), under its
  revision.
  - A profile's policy is security configuration, which belongs with the
    operator's authority and aicrewd's audit. A hub document belongs to
    the team's knowledge.
  - A profile can be exported to a file for review in a PR, and imported
    from one.
- **Generated, applied and checked.**
  - `join`, and the runner's `refresh-home`, generate the home's managed
    files from the profile:
    - `.claude/settings.json`, with its env, hooks and permissions;
    - `.mcp.json`;
    - the installed skills and plugins;
    - the managed commands of the profile's kinds.
  - `check` verifies the home against the profile, including the supported
    client versions per profile.
  - `ensure-home`'s `needs_credential` names the credentials the profile
    lists, so a profile with no forge token never asks for one.
- **Rollout.** A changed profile reaches each home that uses it at the
  home's next refresh: the runner's `refresh-home` from B3 on, and before
  that the human's `join` rerun. `team state` shows each home's profile
  revision against the current one.

### 8.4 The ops delivery kind

Ops work changes live systems, so its human gate comes before the change,
not after it. The kind has two modes. A task's mode comes from the process,
or from the task where the process allows both.

**Declarative mode.** The change is code in a configuration repository:
OpenTofu, Ansible, Nix, or a compose file, whatever the process pins.
- The deliverable is a pull request, delivered as `code-pr` with its
  review and human merge.
- The apply runs after the merge, by the pipeline or by the worker in a
  second, approved step, as below.
- It fits new servers built from scratch. It is available only where the
  project's process names a working toolchain for it.

**Interactive mode (the default).** The worker works on the live host in
supervised phases. It is the default for one-off work (an incident, a few
mailboxes, one setting) and always for Windows hosts, unless the process
names a working declarative toolchain for them.

1. **Diagnose.** The worker runs read-only commands only, with no
   approval, and writes a **finding**.
2. **Plan.** The worker writes a **plan record**:
   - the target hosts;
   - each mutating command exactly as it will run;
   - its expected effect;
   - its rollback, or `irreversible`;
   - the pre-state it captures first, such as a copy of a configuration
     file;
   - the checks that verify the result.
3. **Review.** The coordinator reviews the plan against the task's frozen
   scope, as it reviews code.
   - A plan is not a result. Today an accepted result can only go on to
     delivery and finalize, so the plan is recorded and reviewed by its
     own steps: `plan` by the worker, `plan-review` by the coordinator.
   - The attempt stays `Working` throughout. These steps are the ops
     kind's amendment to the crew contract (E4).
4. **Approve.** The human approves the plan through the operator API. The
   approval binds the plan's digest, so an edited plan needs a new
   approval.
5. **Apply.** The worker runs exactly the approved commands, in order.
   - A command that fails, or an effect that differs from the plan, stops
     the apply.
   - The worker reports, and runs the rollback only if the approval
     included it.
6. **Verify.** Read-only commands check the task's acceptance criteria.
7. **Report.** A report names what was done, the evidence, and the turn
   log's references. It is the `deliverable`, and the confirmed delivery's
   evidence points at it.

**Command classes.** A profile's ops policy sorts commands into three
classes, as patterns per platform. Read-only does not mean harmless: reading
a private key or a password file changes nothing and leaks everything, so
the forbidden class names those reads too.

| Class | Approval | Linux example | Windows example |
| --- | --- | --- | --- |
| read-only | none | `systemctl status`, `journalctl`, reading files under `/etc/postfix`, `dig`, `ss -tlnp` | `Get-*`, `Test-*`, `Measure-*`, `Resolve-DnsName` |
| mutating | the approved plan | `systemctl restart`, `postconf -e`, package installs, writing configuration | `Set-*`, `New-*`, `Remove-*`, `Restart-*`, `Install-*` |
| forbidden | never | changing users or sudoers, flushing the firewall, `rm -rf` on system paths, reading `/etc/shadow` or private keys | changing local administrators, disabling Defender or the firewall, `Enter-PSSession`, exporting certificates with their private keys |

- **How a class is enforced.** Each class maps onto Claude Code's
  permission rules, under D2's `dontAsk` mode:
  - read-only patterns are allow rules;
  - forbidden patterns are deny rules;
  - mutating patterns are neither, so they are denied by default.
  - For an apply turn, the runner adds an allow rule for each approved
    command, exactly as written, to that turn alone. It passes them as the
    turn's own settings, never as a change to the home's.
  - Remote targets are allow-listed hosts: `ssh HOST` for the hosts in the
    profile, and `Invoke-Command -ComputerName` with the same list.
- **The rules match text.** As WORKSPACE says of the deny rules, they stop
  a mistake through the tools they name, not a determined bypass.
- **Decision D19: the host enforces too.** The operator prepares the
  target hosts so that the ops identity can do only its class of work:
  - a sudoers command list on Linux;
  - a Just Enough Administration endpoint on Windows, whose role
    capabilities list the allowed cmdlets.

  The client's classes are the first line, and the host's own limits are
  the boundary.
- **The A2 probe** measures these policy shapes before an ops profile is
  built: PowerShell cmdlet rules, an SSH host allow list, and per-turn
  exact-command allow rules.

**The approval step.**
- **Commands:** `aicrew approvals list [--open]`,
  `aicrew approvals show ID`, and `aicrew approvals approve|reject ID
  [--with-rollback] --reason ...`.
- **Records:** an approval names the plan's digest, the approver, the
  time, whether the rollback was included, and the turn that may apply it.
- **Who approves (decision D18):** a human, with the full operator
  credential, never the architect credential (D10). A change to a live
  system is the floor, like a merge. OPERATOR-SEAT's second-factor receipts
  will bind it more tightly when they exist.
- **The rollback expectation.** Every mutating command has a rollback, or
  the plan marks it `irreversible`, which the approval must acknowledge.
  The worker captures the pre-state before the apply.

**Audit (decision D17).** Every turn of an ops member keeps a full command
log: each command, its output, and its class. Unlike D7's code turns, that
log goes to aicrewd and is kept, redacted, owner-readable only, so an ops
action is reviewable like a PR. The operator reads it with
`aicrew turns log`. Command output can hold secrets that redaction misses,
which is why the log stays on aicrewd and is never mirrored to the board.

**Where the report lives.**
- Team mode cannot write hub documents today; a coordinator can only
  comment on tasks. So the report is the worker's submit: its text is kept
  by aicrewd, and the coordinator mirrors a summary on the task as a
  comment.
- A team-mode document write, for the report as a hub document, is an
  aimem prerequisite (A0c), named here for the aimem session.

### 8.5 Walk-through: setting up the mail server on host M

1. **The board.** The architect writes the task "Set up the mail server on
   host M" in the ops project for network X, whose process names the
   `ops` kind, the interactive mode and the capability `ops: network-x`.
   - The acceptance criteria are checks:
     - mail is accepted on port 25 with STARTTLS;
     - a test message reaches a mailbox;
     - the DKIM and SPF records resolve;
     - a report exists.
   - The operator says READY, and the architect sets it.
2. **The trigger.** The task's move to READY reaches the coordinator as a
   `board.changed` message (A1), and its turn starts (section 1).
3. **The offer.**
   - The coordinator's capability read shows one member with the
     `ops-mail` profile and `ops: network-x`. That member's runner is on
     the host the operator prepared with the ZTNA identity for network X
     and an SSH identity limited to host M.
   - The coordinator offers it the task. An offer to a `dev-go` member
     would be refused `capability_missing`.
4. **Diagnose.** The worker's turns run read-only commands over SSH on host
   M: the installed packages, the listening ports, the DNS records. Its
   finding: Postfix is not installed, and port 25 is free.
5. **Plan.**
   - The plan lists the exact commands:
     - install Postfix and OpenDKIM;
     - write `main.cf` with STARTTLS and the certificate paths;
     - generate the DKIM key;
     - restart the services.
   - It also gives the DNS records to publish. The DNS zone is outside
     host M, so that step is an escalation (7.5) unless the profile
     covers it.
   - Each command has a rollback: remove the packages, restore the saved
     `main.cf`.
   - The worker records the plan with the `plan` step.
6. **Review and approval.** The coordinator's `plan-review` returns the
   plan once, because one rollback was missing. The worker records the
   amended plan, and the coordinator accepts it. The human runs `aicrew
   approvals approve ID --with-rollback`.
7. **Apply.** The approval message starts the worker's apply turn, which
   has exact allow rules for the approved commands only. Each command's
   output goes to the ops command log.
8. **Verify.** Read-only checks:
   - an SMTP session shows STARTTLS;
   - a test message is delivered to a mailbox;
   - `dig` resolves the DKIM and SPF records.
9. **Report and delivery.**
   - The worker submits the report as its result. The coordinator reviews
     it and confirms delivery with the approval, the verification output
     and the log's references as evidence.
   - The task becomes DONE through the usual finalize. The coordinator
     mirrors the report on the task.

A small change, such as adding two mailboxes, has the same shape with a
one-line plan. An incident ("the mail server stopped delivering") starts
at the diagnosis, and its finding may end the task without any mutating
step.

## 9. Decisions for the operator

Each has a recommendation, and the design above assumes it.

| | Decision | Recommendation |
| --- | --- | --- |
| D1 | How a turn is triggered | Every trigger is an inbox message (1.2) |
| D2 | Permissions in a headless turn | `dontAsk`, prompts `none`, a managed allow list beneath the deny rules; never `bypassPermissions` |
| D3 | The interactive mode (`run` with the Stop hook) | Keep it for human-attended members until B2 has run a release; decide then |
| D4 | Conversation lifetime | Per attempt for a worker; for a coordinator, rotate at 50 turns, 70 % context or a new day |
| D5 | Forge events | The runner polls its attempts' PRs with the member's own credential; no inbound webhooks to aicrewd |
| D6 | Where an API key lives | `creds/`, given to Claude Code through `apiKeyHelper`; for Codex and OpenCode, fed into their own stores |
| D7 | Turn transcripts | They stay on the host; aicrewd keeps a redacted summary and fetches logs on demand |
| D8 | Several homes per runner | Only with `--shared-user`, accepted explicitly and shown in team state |
| D9 | The architect's identity on aimem | The human's own aimem user |
| D10 | How the architect reads and answers escalations | A separate architect credential (reads and escalation answers only); the full operator token never enters an AI session |
| D11 | The architect and OPERATOR-SEAT | The architect receives escalations; the seat's records and categories are kept; receipts deferred |
| D12 | Pause and resume | A scheduler state (C1); amend 5598's criterion 2 |
| D13 | The board trigger's source | Ask the aimem session for the `board.read` cursor feed of task state changes (A0) |
| D14 | The escalation record | aicrewd's store, mirrored as task comments; a coordinator's comment on a held task waits for the hold to end, unless aimem later allows comments under a hold |
| D15 | Where a task's required capability comes from | The project's pinned process; a task narrows it through a typed field once aimem has one (A0b) |
| D16 | Where worker profiles live | On aicrewd, edited by the operator, exportable for review; not a hub document |
| D17 | Ops audit | A full, redacted command log of every ops turn, kept on aicrewd (an exception to D7) |
| D18 | Who approves an ops plan | A human with the full operator credential, never the architect credential; bound to the plan's digest |
| D19 | Host-side enforcement for ops | The operator prepares sudoers command lists or JEA endpoints; the client's command classes are the first line, not the boundary |
