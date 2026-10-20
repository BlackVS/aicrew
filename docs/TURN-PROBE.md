# Turn probe

Measured on 2026-10-09 for task `01a11eed-dc03`, increment A2 of
[DESIGN-CONTROL-PLANE.md](DESIGN-CONTROL-PLANE.md). It measures what the
design assumes about one headless turn of each client, before any adapter
is built:
- start and resume;
- the end event;
- usage and cost;
- limits;
- authentication failure;
- the permission rules a turn runs under.

Measurement only:
- no product code;
- no aimem hub, credential or real repository;
- every run used a disposable directory under the system's temporary
  directory, dummy credential files, and each client's smallest model.

The section "The starting context of a turn" was added on 2026-10-20 for
task `01a157c9-5ed1`, increment 3a of [DESIGN-TURNS.md](DESIGN-TURNS.md).
It reads the pilot's records and ran no client.

## Setup

| | Version | Login | Model |
| --- | --- | --- | --- |
| Claude Code | 2.1.294 | the operator's subscription | Haiku (`--model haiku`) |
| Codex | 0.156.1 | ChatGPT login | the login's default, `model_reasoning_effort=low` |
| OpenCode | 1.18.32 | none; free models | `opencode/ling-3.1-flash-free`, `opencode/mimo-v2.6-flash-free` |

Everything ran on Windows 11 (`win32`).

Claude Code ran with `--setting-sources project,local --strict-mcp-config`,
so no user setting or MCP server took part. The project's
`.claude/settings.json` held the 22 managed deny rules of task
`01a1171d-c51c` unless a case says otherwise. Each client's environment
had no `CLAUDE*` or `ANTHROPIC*` variable unless a case set one.

**Works:** measured and repeatable in this run. **Fragile:** measured, but
with a failure mode the design must cover. **Not measured:** no conclusion.

## Results

### A turn, its end and its resume

| | Claude Code | Codex | OpenCode |
| --- | --- | --- | --- |
| Start | Works: `claude -p PROMPT --output-format stream-json --verbose --session-id UUID` | Works: `codex exec --json --skip-git-repo-check --sandbox workspace-write PROMPT` | Works: `opencode run --format json -m MODEL PROMPT` |
| The conversation's ID | the `--session-id` given, repeated in every event's `session_id` | `thread_id` in the first event, `thread.started` | `sessionID` in every event |
| Events | `system/init`, `system/thinking_tokens`, `assistant`, `rate_limit_event`, then `result` | `thread.started`, `turn.started`, `item.completed`, then `turn.completed` | `step_start`, `text`, then `step_finish` |
| Turn end | Works: one `result` event, then exit 0. A normal end is `subtype: success`, `is_error: false` and `stop_reason: end_turn`. | Works: `turn.completed`, then exit 0 | Works: `step_finish` with `reason: stop`, then exit 0 |
| Resume | Works: `--resume UUID`. The answer recalled the number given in the first turn, under the same `session_id`. | Works: `codex exec resume THREAD_ID --json PROMPT` recalled it | Works: `--session ID` recalled it, after one failed attempt (below) |
| Usage | Works: `result.usage` has input, output, cache-read and cache-write tokens. `result.modelUsage` has the same per model, with `costUSD` and `costBasis: list`. | Works: `turn.completed.usage` has input, cached-input, output and reasoning tokens | Works: `step_finish.part.tokens` has total, input, output, reasoning and cache read and write |
| Cost | Works: `result.total_cost_usd` (here $0.0025 for a one-word turn) | Not given: tokens only | Works: `step_finish.part.cost` (0 on a free model) |
| Standard input | Fragile: with stdin left open, the client warns that it waits for input. Close it. | Fragile: it prints "Reading additional input from stdin" and waits. Close it. | Not measured: it ran with stdin closed |

The `system/init` event also reports, among others:
- `apiKeySource` (`none`, `ANTHROPIC_API_KEY`, `apiKeyHelper`);
- `permissionMode`;
- `model`;
- `tools`;
- `claude_code_version`.

A runner can read the client's effective settings from it before the first
model call.

### Limits and budgets

- **Claude Code's limit state, on every turn.** Every turn carries a
  `rate_limit_event`, whether or not a limit is near. It has:
  - `rate_limit_info.status` (`allowed` in this run);
  - `resetsAt`, a Unix time;
  - `rateLimitType` (`five_hour`);
  - `unifiedWindows` with each window's `utilization` and `resetsAt`.

  So the reset time §3.4 needs comes with every turn, and a scheduler can
  hold turns before a window is exhausted rather than after. A `status`
  other than `allowed` was not provoked: exhausting a quota on purpose is
  not safe.
- **Claude Code's own budget.** `--max-budget-usd` ends the turn with:
  - `subtype: error_max_budget_usd`, `is_error: true`;
  - `terminal_reason: budget_exhausted`;
  - `errors: ["Reached maximum budget (...)"]`;
  - exit 1.

  It is checked after a model call. A turn with a $0.0001 budget still
  made its first call and spent $0.0024. Fragile: the budget may be
  overshot by one model call.
- **OpenCode, an upstream limit.** A free model's upstream answered `429`.
  The turn ended with one `error` event and exit 1, with
  `error.name: APIError`, `data.statusCode: 429` and
  `data.isRetryable: true`. The same resume with another free model then
  worked. CLIENT-WAKE-PROBE saw the same randomness on `serve`.
- **Codex.** No limit event was seen or provoked: not measured.

### Authentication failure

| Case | Result |
| --- | --- |
| Claude Code, a wrong key in `ANTHROPIC_API_KEY` | Works: it fails at once, exit 1. `result` has `is_error: true`, `api_error_status: 401`, `terminal_reason: api_error`, and the text "Invalid API key". |
| Claude Code, a wrong key from `apiKeyHelper` | Fragile: no `result` within 90 s. The client retries, emitting `system/api_retry` events with `error_status: 401`, `error: authentication_failed`, `attempt`, `max_retries: 10` and `retry_delay_ms`. A runner must end the turn at the first `authentication_failed` retry and report `needs_login`, not wait out ten retries. |
| Codex, OpenCode | Not measured: provoking it would mean logging this machine's clients out. |

### Permissions in a headless turn (Claude Code)

Each case asked the model to make one exact tool call. All ran with
`--permission-prompts none`.

| Case | Mode | Allow rule given as | Result |
| --- | --- | --- | --- |
| `git init -q repo1` | `dontAsk` | the project's `.claude/settings.json`: `Bash(git *)` | **Denied.** Also with `Bash(git init:*)`. |
| Write `worktrees/a.txt` | `dontAsk` | the project's settings: `Edit(/worktrees/**)`, `Edit(worktrees/**)`, `Edit(./worktrees/**)`, `Edit(**/worktrees/**)`, `Edit`, `Write` | **Denied** in every form |
| `git init -q repo1` | `dontAsk` | a `--settings FILE` holding `Bash(git *)` | Allowed; the repository was created |
| `git init -q repo1` | `dontAsk` | `--allowedTools "Bash(git *)"` | Allowed |
| Write `worktrees/a.txt` | `acceptEdits` | none | Allowed |
| `cat creds/github.token` | `dontAsk` | `--allowedTools "Bash(cat *)"` | **Denied by the project's deny rule.** A deny beats a command-line allow, and the dummy secret was not shown. |
| `mkdir approved1` | `dontAsk` | `--allowedTools "Bash(mkdir approved1)"` | Allowed: an exact rule admits its command |
| `mkdir other1` | `dontAsk` | `--allowedTools "Bash(mkdir approved1)"` | Denied: and only its command |
| `mkdir newdir` | `dontAsk` | none | Denied |
| `whoami` | `dontAsk`, and `default` | none | **Allowed.** Claude Code admits commands it classes as read-only without any rule. |
| `Get-ChildItem -Name` (PowerShell tool) | `dontAsk` | none | **Allowed,** the same read-only class |
| Read tool on a file in the directory | `dontAsk` | none | Allowed |
| `Get-Content creds/github.token` (PowerShell) | `dontAsk` | `--allowedTools "PowerShell(Get-*)"` | Denied by the deny rule |
| `ssh … host-a.invalid hostname` | `dontAsk` | `--allowedTools "Bash(ssh … host-a.invalid *)"` | Allowed. The connection then failed, as `.invalid` cannot resolve. |
| `ssh … host-b.invalid hostname` | `dontAsk` | the same host-a rule | Denied: a host allow list holds |
| `New-Item -ItemType Directory -Path ps1` (PowerShell) | `dontAsk` | `--allowedTools` with an exact, prefix (`New-Item:*`, `New-Item *`), lowercase, `*New-Item*` or alias (`mkdir *`) rule | **Denied** in every form, with or without the deny rules |
| `Set-Content -Path f5.txt -Value hi` (PowerShell) | `dontAsk` | `PowerShell(Set-Content *)` | **Denied** |
| `New-Item …` (PowerShell) | `dontAsk` | `PowerShell(*)`, or `PowerShell` alone | Allowed |
| The same cases | `default` | the project's settings | The same denials. The message says the call needs approval and the session has no approval surface. |

What this means:
- **Project allow rules.** Allow rules in a project's `.claude/settings.json`
  do not take effect in a headless turn, while deny rules from the same file
  do.
  - The likely cause is that this directory's workspace was never trusted,
    and the client applies a project's allow rules only after trust. That
    was not tested, because it would change the user's own client state.
  - Either way, allow rules work when the runner passes them per turn, with
    `--settings FILE` or `--allowedTools`.
- **Read-only commands.** `dontAsk` denies what it would otherwise ask about,
  not every unlisted command. The client admits commands it judges
  read-only, on its own. A read that must never happen, such as of a
  credential or a private key, has to be a deny rule.
- **PowerShell.** Its allow rules are all-or-nothing on this version. Only a
  blanket rule admits a mutating cmdlet, but the deny rules' patterns match
  PowerShell command text, as `01a1171d-c51c` found.
- **What holds as designed:**
  - Bash exact rules and host allow lists;
  - the deny rules above every allow.

### MCP tools and file reads in a turn (pilot finding #15)

The first turns on v0.5.6 showed a coordinator's turn denied aimem's
`get_task`: in `dontAsk` mode an MCP tool needs an allow rule, and the
per-turn list had none. Measured on 2026-10-13 with Claude Code 2.1.296 on
Windows, headless, `--permission-mode dontAsk --permission-prompts none`,
Haiku. The rules were passed with `--settings`, as the turn loop passes
them. A fake stdio MCP server named `aimem` answered `get_task`,
`triage_task` and `add_task_comment` and recorded each call, and a fake
`aimem` command stood on `PATH`. No hub was involved.

| Call | Settings | Result |
| --- | --- | --- |
| `mcp__aimem__get_task` | the v0.5.6 list (no MCP rule) | **Denied**, the finding reproduced |
| `mcp__aimem__get_task` | the member's `.claude/turn-settings.json` as `join` now writes it | Allowed; the server saw the call |
| `mcp__aimem__triage_task` | the member's file | **Denied**; the server saw nothing |
| `get_task`, `triage_task`, `add_task_comment` | the coordinator's `.claude/turn-settings-coordinator.json` | All three allowed |
| `aimem process show` (Bash) | the coordinator's file | Allowed |
| `aimem task create --title x` (Bash) | the coordinator's file | **Denied** |
| `aimem process show` | the member's file | **Denied** |
| Glob `worktrees/**/*.txt`, Grep `MARKER` in `worktrees` | the member's file, and also with no file rule | Allowed |
| Read `creds/secret.txt` | the coordinator's file | **Denied** by `Read(./creds/**)` |
| Grep `SECRET` in `creds` | the coordinator's file | **Denied**, by the same Read rule |
| `cat creds/secret.txt` (Bash) | the coordinator's file | **Denied** by `Bash(*creds/*)` |

What this means:
- **An MCP tool needs its own rule** in `dontAsk` mode, by its full name
  (`mcp__aimem__get_task`). The client's read-only class does not cover
  MCP tools.
- **Read, Grep and Glob** run in the working directory without a rule, as
  above. The lists name them anyway (`Read(./**)`, `Grep(./**)`,
  `Glob(./**)`), so the intent is written down.
- **The deny rules hold over the wider list:** a `Read` deny also stops
  Grep, and the Bash patterns stop `cat`.
- On Windows Haiku sometimes chose the PowerShell tool for `aimem process
  show`. It was denied, since no narrow PowerShell rule admits anything.
  `/crew-turn` teaches the Bash tool.

### A worker's dev toolchain in a turn (pilot finding #18)

In the pilot a worker committed its change in the worktree and then
blocked, because its turn's settings denied `go test`. Measured on
2026-10-14 with Claude Code 2.1.299 on Windows, headless, `--permission-mode
dontAsk --permission-prompts none`, Haiku. The worker's settings were the
`state/turn-settings-worker.json` the turn loop writes when the team selects
`dev-go` for a project (`aicrew team toolchain`), passed with `--settings`.
The home held a small Go module in `worktrees/mod`, with a passing test and
`scripts/check.sh`, and a dummy `creds/secret.txt`. No hub was involved.

| Command | Settings | Result |
| --- | --- | --- |
| `cd worktrees/mod && go test ./...` | the common `.claude/turn-settings.json`, as v0.5.7 | **Denied**, the finding reproduced |
| `cd worktrees/mod && go test ./...` | the worker's file with `dev-go` | Allowed; the test passed |
| `cd worktrees/mod && go vet ./...`, `go build ./...`, `go mod tidy`, `gofmt -l .` | the worker's file | Allowed |
| `cd worktrees/mod && bash scripts/check.sh` | the worker's file | Allowed; the script ran |
| `go -C worktrees/mod test ./...` | the worker's file | **Denied**: `dev-go` admits the commands, not `go -C` |
| `cd worktrees/mod && go run .` | the worker's file | **Denied**: not in the list |
| `cat creds/secret.txt` | the worker's file | **Denied** by `Bash(*creds/*)` |
| `git config --global user.name probe` | the worker's file | **Denied** by `Bash(*config --global*)` |
| `cd worktrees/mod && go test ./... && cat ../../creds/secret.txt` | the worker's file | **Denied** as a whole by the deny rule; the model then ran `go test` alone, which was allowed. The dummy secret was not shown. |

What this means:
- **A compound command with `cd`** into the worktree is judged by its
  parts: `cd` inside the working directory is admitted, and the toolchain
  rule admits the rest. A worker runs its toolchain that way.
- **The deny rules hold over the toolchain**, also when a denied part is
  chained after an admitted one.

### An API key through `apiKeyHelper` (Claude Code)

**What was not measured, and why.** A tool call cannot run without a valid
key, and the probe had only the subscription login. So whether the key
reaches a Bash tool call's environment was not measured directly.

**The proxy.** A `SessionStart` hook, which the client starts as a child
process, as it starts a tool call, recorded which `ANTHROPIC*` variables
and which values containing the dummy key it saw:

| Key given as | `apiKeySource` in `init` | The hook saw |
| --- | --- | --- |
| `ANTHROPIC_API_KEY` in the client's environment | `ANTHROPIC_API_KEY` | `ANTHROPIC_API_KEY` |
| `apiKeyHelper` in a `--settings` file | `apiKeyHelper` | nothing |

The proxy supports D6: a key passed in the environment reaches the client's
children, and a helper's key does not. A direct check with a real API key
belongs to A5, which needs one anyway.

### The starting context of a turn (the pilot's records)

The "before" of the stateless turn (DESIGN-TURNS section 3): what a turn
starts from today, when every turn resumes the conversation. Measured from
the pilot's records only, with no new client run; the "after" is measured
by increment 3e on the pilot's resume.

**The records.** The pilot's worker and coordinator homes, members at the
0.5.7 to 0.5.9 releases of the pilot's second cycle (the record carries no
version; the turn loop resumed the conversation in all three), Claude Code
with the model of DESIGN-TURNS section 2 (Claude Opus 5.5, list prices): the
worker's `logs/turns.jsonl` (12 turns in one conversation, 2026-10-14 and
15) and the coordinator's (15 turns in three conversations, 2026-10-13 to
15), read only, and the client's own transcript of each conversation, which
carries the usage of every model call. A turn record holds the client's
totals for the turn (the cache reads are summed over all its calls), so the
starting context is read from the transcript: it is the context of the
turn's first model call, input plus cache-written plus cache-read tokens.
The calls of a turn are those between the record's start and end; their
cache reads and writes sum to the record's own on all 27 turns. The turn's
own cost is the difference between the record's `cost_usd` and the previous
record's in the same conversation, since these records predate the fix of
`01a13f81-7df9`. Times are UTC; the prices are the list prices of
DESIGN-TURNS section 2.

| | Worker | Coordinator |
| --- | --- | --- |
| Turns (conversations) | 12 (1) | 15 (3) |
| Fresh turns | 1 | 3 |
| Context at the first model call of a fresh turn | 34,374 | 34,255 to 34,371 |
| Context after the start reads (`HANDOFF.md`, `START.md`, `ROLES.md`, `session status`, `inbox`), before any work | 55,109 | 59,242 |
| Resumed turns: context at the first call, median (min to max) | 126,930 (94,871 to 173,108) | 74,065 (61,825 to 101,227) |
| Written to the cache at the first call of a warm resumed turn | 1,285 to 1,690 | 1,260 to 1,782 |
| Model calls per turn, median (min to max) | 6 (3 to 25) | 6 (3 to 16) |
| Cache reads per turn, median (min to max) | 0.72 M (0.34 to 2.83 M) | 0.36 M (0.20 to 1.67 M) |
| Cost of a resumed turn, median (min to max) | $0.51 ($0.10 to $1.97) | $0.26 ($0.08 to $1.37) |
| Cost of a fresh turn | $1.20 (it also wrote code) | $0.58 to $0.70 |
| All turns | $9.22 | $7.23 |

**The hold.** The client's cache lives one hour. A resumed turn that starts
more than an hour after the previous one writes its whole context to the
cache again at the first call, at $8 per million:

| Turn (UTC) | Gap before it | Written at the first call | The turn's cost | Of it, the rewrite |
| --- | --- | --- | --- | --- |
| Worker 2026-10-15 05:22 | 1 d 1 h 14 | 94,869 | $1.36 | $0.76 |
| Worker 2026-10-15 07:01 | 1 h 24 | 132,779 | $1.23 | $1.06 |
| Worker 2026-10-15 10:33 | 3 h 32 | 137,172 | $1.97 | $1.10 |
| Worker 2026-10-15 12:44 | 2 h 00 | 173,106 | $1.56 | $1.38 |
| Coordinator 2026-10-15 07:01 | 1 h 24 | 90,792 | $0.82 | $0.73 |
| Coordinator 2026-10-15 10:33 | 3 h 32 | 94,445 | $0.85 | $0.76 |
| Coordinator 2026-10-15 12:45 | 2 h 12 | 98,000 | $0.88 | $0.78 |
| Coordinator 2026-10-15 13:57 | 1 h 12 | 87,061 (14,164 read) | $1.37 | $0.70 |

Eight turns, 908,224 tokens, $7.27: 44 % of the $16.45 the two members
spent. Every other resumed turn wrote under two thousand tokens at its
first call.

**What the numbers say.**
- **The client's floor is about 34 thousand tokens.** The first model call
  of every fresh turn, in both roles, held 34 thousand tokens, with only
  the `/crew-turn` prompt as input and before any read: the client's own
  system prompt and tool schemas, the home's two instruction files (half a
  kilobyte together) and the command's text. It is paid at every fresh
  start, and it is not in the design's 25 thousand.
- **The start reads add 19 to 25 thousand.** `HANDOFF.md` (the worker's was
  33 KB by the pilot's end), `ROLES.md` (18 KB) and `START.md` (3 KB), then
  `session status` and `inbox`, brought a fresh turn to 55 to 59 thousand
  tokens before any work. Section 2.4 point 4 counted 60 to 79 thousand
  for the fresh turns from their totals; per call, the start is 55 to 59
  thousand and the rest was those turns' work.
- **A resumed turn starts where the last one ended**, plus 1.3 to 1.8
  thousand tokens for the new prompt and the previous answer. The worker's
  second day started at 95 to 173 thousand; the coordinator's turns at 62
  to 101 thousand. Section 2.4's "130 to 180 thousand" is the worker's
  second day; the coordinator sat lower.
- **Every call reads the whole context.** At a median of six calls, a
  worker turn read 0.72 million tokens ($0.14) and the twenty-call turn
  2.83 million ($0.57), for the conversation's size, not the task's.
- **The hold's rewrite is 44 % of the spend**, as the table shows. Section
  2.4 point 2 counted four such turns ($3.86, their whole cache writes)
  from the records as they stood at 10:44 UTC on 2026-10-15: the worker's
  first turn of that day, after the night, was a rewrite too, and three
  more followed that day.

**The target, restated against the floor.** DESIGN-TURNS section 3 sets a
fresh start at about 25 thousand tokens of new context, the brief and the
role rules for the turn kind, at about $0.20 to write. Those 25 thousand
sit on top of the client's floor: at today's floor a fresh start is about
59 thousand tokens, written once at $0.47 ($0.27 the floor, $0.20 the
brief), and every call of the turn then reads about 59 thousand ($0.012)
instead of 62 to 173 thousand. Increment 3e records the "after" in this
table's shape: the context at the first call, the calls, the reads and the
cost of a routine turn. What the floor consists of is not measured here
(below).

## Where the design must change

| Finding | Section to amend | Proposed change |
| --- | --- | --- |
| The home's project allow rules are ignored in a headless turn | §1.4 D2; A3 | The runner passes the allow list per turn, with `--settings FILE` (the profile's policy) or `--allowedTools`. The deny rules stay in the home's managed settings, where they apply, and may also be passed per turn. |
| Read-only commands run without any allow rule | §1.4 D2; §8.4 command classes | "Unlisted is denied" holds only for commands the client does not judge read-only. The forbidden class must be deny rules, as §8.4 already says of secret reads. The read-only class needs no allow rule. |
| PowerShell allow rules admit nothing narrower than the whole tool | §8.4, the apply turn on Windows; E5 | Exact per-turn allows work for Bash, not PowerShell. On Windows, either the runner runs the approved commands itself and gives the model their output, or the apply turn allows `PowerShell` under the deny rules, with JEA on the host as the boundary (D19). Recommended: the runner runs approved commands itself, on both platforms, which also removes the text match from the apply. |
| A wrong `apiKeyHelper` key retries for minutes | §3.3; A5 | The runner ends a turn at the first `system/api_retry` with `authentication_failed` and reports `needs_login`. |
| `--max-budget-usd` overshoots by one model call | §3.4 | The turn budget is a soft ceiling. The attempt and daily budgets the scheduler applies before a turn are the hard ones. |
| Every turn reports the provider's windows and reset time | §3.4; A5 | `limit_reached` takes its reset time from `rate_limit_event`. The scheduler may hold turns when a window's utilization nears its end. |
| Codex reports tokens but no cost | §3.5; A7a | Codex's cost is computed from a price table or left empty. Its tokens are recorded either way. |
| Claude Code and Codex wait on an open stdin | §1.4; A3, A7 | The runner starts each turn with stdin closed. |
| A fresh turn's first model call holds about 34 thousand tokens before any read, in both roles | DESIGN-TURNS §3, the target | The 25 thousand tokens of the target are the brief and the role rules on top of the client's floor: a fresh start writes about 59 thousand at today's floor ($0.47), not 25. The floor's composition is measured by increment 3b when it sizes the brief, with one bare client run; the per-turn tool set is the lever if the tool schemas are its bulk. |
| The hold's rewrite was eight turns, 908 thousand tokens and $7.27, 44 % of the pilot's spend | DESIGN-TURNS §2.4 point 2 | 2.4 counted four turns and $3.86 from the records of 2026-10-15 up to 10:44 UTC; the overnight gap before the worker's first turn of that day and the day's later turns raise it. The fresh turn (increment 3b) removes it, and its test is a hold longer than an hour followed by a turn that writes no conversation. |

## Not measured

- What the 34 thousand tokens of a fresh turn's first model call consist
  of: the client's system prompt, the built-in tool schemas, the list of
  deferred tools (the pilot's turns loaded an MCP tool's schema on demand
  through `ToolSearch` before calling it, so the MCP schemas are likely not
  in the floor), the home's instruction files and the command's text. One
  bare run with an empty prompt, with and without the MCP configuration,
  separates them; increment 3b does it when it sizes the brief.
- A provider's limit in its rejected state, for any client (not provoked).
- Codex and OpenCode authentication failure; Codex's limit events; Codex's
  sandbox and OpenCode's `permission` rules under a headless turn.
- A real API key's absence from a Bash tool call (A5).
- Whether trusting the workspace makes a project's allow rules apply.
- Linux and macOS. Everything ran on Windows. The PowerShell findings are
  Windows ones. The others are expected to hold elsewhere, but were not
  measured there.

## Reproduce

Every run used a fresh directory under the system's temporary directory,
and dummy credential files holding `DUMMYSECRET4242`.

**Claude Code:**
- **A turn:**
  `claude -p PROMPT --output-format stream-json --verbose --model haiku
  --setting-sources project,local --strict-mcp-config --session-id UUID`,
  with stdin closed.
- **Resume:** the same with `--resume UUID`.
- **Permissions:** add `--permission-mode dontAsk --permission-prompts none`,
  and an allow rule in the project's settings, a `--settings FILE`, or
  `--allowedTools RULE`.
  - Ask for exactly one tool call: "Call the Bash tool once with exactly
    this command string and nothing added: …". Otherwise the model may add
    `; echo $?`, and a compound command is judged as a whole.
  - Read `result.permission_denials` and the tool results.
- **Budget:** `--max-budget-usd 0.0001`.
- **Authentication:**
  - a dummy `ANTHROPIC_API_KEY` in the client's environment; or
  - `{"apiKeyHelper": "python helper.py"}` in a `--settings` file, where
    the helper prints a dummy key.

  Add a `SessionStart` hook that records the `ANTHROPIC*` variable names it
  sees.

**Codex:**
- `codex exec --json --skip-git-repo-check --sandbox workspace-write -c
  model_reasoning_effort=low PROMPT`, then
  `codex exec resume THREAD_ID --json PROMPT`, with stdin closed.

**OpenCode:**
- `opencode run --format json -m opencode/ling-3.1-flash-free PROMPT`, then
  the same with `--session ID`.

**The pilot records (the starting context):** per turn of a home's
`logs/turns.jsonl`, take `conversation`, `started_at`, `ended_at`, `usage`
and `cost_usd`. In the client's transcript of that conversation (Claude Code
writes one JSON-lines file per session under its projects directory), take
every `assistant` entry that has `message.usage`, one per `message.id` (a
call with several content blocks is logged as several entries with the same
id), and keep those whose `timestamp` lies between the turn's start and
end. The starting context is the first kept call's `input_tokens` plus
`cache_creation_input_tokens` plus `cache_read_input_tokens`. The
cross-check: the kept calls' cache reads and writes sum to the record's
`usage.cache_read_tokens` and `cache_write_tokens`. The turn's own cost is
`cost_usd` minus the previous record's in the same conversation; a gap is
the time from the previous turn's end to the turn's start.
