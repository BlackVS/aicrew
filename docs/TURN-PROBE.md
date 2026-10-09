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

## Not measured

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
