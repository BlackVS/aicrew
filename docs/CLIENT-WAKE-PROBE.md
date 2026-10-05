# Client observation and wake-up probe

Measured on 2026-10-05 for task `01a0eb7c-b10e` (the feasibility probe the
operator made a prerequisite of the wake-up adapter, `01a0d6d7-1aed`). It
answers one question: can the `aicrew-agent` launcher observe and drive the
members' clients reliably enough to wake a member when its inbox changes,
without a human message? Measurement only: no product code, no aimem hub,
credential, team session or real repository; every run used a disposable
scratch directory. It builds on aimem's earlier probes
(`docs/AGENT-CAPABILITY-PROBE.md` and `docs/CLAUDE-CHANNEL-PROBE.md`, the
latter on Claude Code 2.1.281).

## Setup

| | Windows 11 | Linux (Ubuntu 24.04 under WSL 2) |
| --- | --- | --- |
| Claude Code | 2.1.289 | 2.1.284, auto-updated to 2.1.289 during the runs; the channel and final PTY runs report 2.1.289 |
| OpenCode | 1.18.32 (`opencode serve`, V2 API) | not installed: not measured |
| Model | Claude Haiku 4.5 (the operator's subscription login); OpenCode's free `ling-3.1-flash-free` | Claude Haiku 4.5 |
| Terminal | ConPTY, driven by a Go program (`CreatePseudoConsole`) | PTY, driven by Python's `pty` |

The fixtures were small programs in a local scratch area, not in this
repository:
- a stream-json driver;
- a hook command that logs every event, waits for a simulated inbox file at
  `Stop` and can answer `PermissionRequest`;
- a minimal MCP stdio server declaring the `claude/channel` capability that
  pushes `notifications/claude/channel` when an event file appears;
- PTY and ConPTY drivers that type into the TUI;
- an OpenCode driver on the V2 HTTP API and its event stream.

The section "Reproduce" gives the essential shapes. The Claude settings
were project-only (`--setting-sources project,local --strict-mcp-config`),
so no user hook or plugin took part.

## Results

**Works**: measured, repeatable in this run. **Fragile**: measured, but with
a failure mode the design must cover. **Not measured**: no conclusion.

### Claude Code

| State or action | Headless `-p`, stream-json in and out | Hooks (interactive and headless) | Channels (interactive, development flag) | TUI under PTY / ConPTY |
| --- | --- | --- | --- | --- |
| (a) turn finished / idle | Works: a `result` event per turn; the process stays alive between turns while stdin is open | Works: `Stop` per turn end (with `stop_hook_active` on a chained stop) | n/a | Fragile: only by screen text |
| (b) waiting for input | Works: after `result` the process waits on stdin | Works: `Notification` "Claude is waiting for your input" about 60 s after a turn ends (Windows and Linux) | n/a | Fragile: only by screen text |
| (c) permission requested: detect | Works with `--permission-prompt-tool stdio`: a `control_request` `can_use_tool` with the tool and input | Works: `PermissionRequest` (interactive), with `PreToolUse` before it | Not measured (permission relay not declared) | Works by screen text ("Do you want to create ...") |
| (c) permission: answer | Works: a `control_response` allow (file written) or deny (refused, reported in `permission_denials`) | Works: `PermissionRequest` answering `{"behavior":"allow"}` wrote the file, `deny` did not; the dialog may flash before the answer lands | Not measured | Works by keystroke (Enter on "1. Yes"): fragile |
| (d) tool started / finished | Works: `tool_use` and `tool_result` content | Works: `PreToolUse` and `PostToolUse` | n/a | Fragile |
| (e) crash / exit | Works: process exit after stdin closes (exit 0) | n/a | Fragile: a crashed channel server is not restarted (aimem probe, 2.1.281) | Works: process exit; two Ctrl-C exit 0 |
| Wake an idle session | Works: a user message written to stdin 8 s after the last turn started the next turn in about 20 ms, the reply about 1 s later (Windows) | See "The Stop hook wait" below: works only while a hook still holds the turn's end | Works: an event to the idle session got its reply in 1.2 s (Windows) and 1.9 s (Linux) | Works by typing a prompt: about as fast, but the launcher must own the terminal |
| Event while busy | Messages queue on stdin | n/a | Fragile: shown in the transcript during a 20 s tool call, but the model never acted on it (2.1.289; on 2.1.281 it was handled at the next model request) | n/a |
| Duplicates | n/a | n/a | Delivered twice and answered twice: no deduplication | n/a |
| Human attach / takeover | Impossible: there is no terminal UI | n/a (hooks run inside the human's session) | n/a (inside the human's session) | Not measured: needs the launcher to relay its pseudo terminal to the human |
| Consent and limits | None | Hooks are project settings; a hook runs within its `timeout` (killed beyond it) | `--dangerously-load-development-channels`, a start-up warning answered each run, first-party authentication; not in print mode (aimem probe) | First start in a new folder asks for workspace trust ("No, exit" is the default) |

**The TUI under a pseudo terminal.** The interactive client was driven
through a PTY on Linux and a ConPTY on Windows, each starting at 120×40.

| | Linux PTY | Windows ConPTY |
| --- | --- | --- |
| Rendering | Works: the full TUI with its dialogs, input box and transcript | Works: the same, once the child inherits no redirected standard handles (see "Reproduce") |
| Resize | Works: 120×40 to 90×30 while idle (`TIOCSWINSZ` and `SIGWINCH`). The TUI redrew within 3 s: its horizontal rule went from 120 to 90 characters, and the next prompt was answered. | Works: `ResizePseudoConsole` to 90×30. The rule went from 120 to 90 characters, and the next prompt and a permission dialog worked. |
| Paste | Works: a two-line bracketed paste (`ESC[200~` ... `ESC[201~`), then Enter, arrived as one message and was answered | Works: the same paste, answered |
| Ctrl-C | Works: two Ctrl-C exit with status 0 | Works: two Ctrl-C exit with code 0 |
| Input injection | Works: typed text and Enter, the arrow keys in dialogs | Works: the same |
| State recognition | Fragile: only by screen text, with whitespace lost to cursor movement; the hooks give the same states structurally | Fragile: the same |
| Human takeover | Not measured: the probe drove the terminal itself; a relay of the pseudo terminal to a human's terminal was not built | Not measured: the same |

**The Stop hook wait.** A `Stop` hook may refuse the stop with
`{"decision":"block","reason":"..."}`, and Claude continues with the reason as
its next instruction. A hook that waits for the inbox therefore keeps a
member from ending its turn while nothing has arrived:
- **Headless, Windows:** the hook waited, a message arrived 5.6 s into the
  wait, and the answer followed about 2 s later. With a 900 s hook timeout,
  it woke the session after about 3 and 7 minutes and then waited a full
  500 s before letting the session stop. The run lasted 15 minutes, with no
  model call while it waited.
- **Interactive, Linux PTY and Windows ConPTY:** a message written while the
  hook waited was answered 1.7 s (Linux) and 1.5 s (Windows) later. The TUI
  labels the block's reason "Stop hook error", which is cosmetic.
- **The limit:** once the hook lets the session stop, the interactive session
  is idle and no hook fires again until the human or something else starts a
  turn. A message written then waited until the next turn ended. A hook
  killed by its own timeout lets the session stop the same way.

### OpenCode (Windows, 1.18.32, `opencode serve` V2 API)

| State or action | Server API and event stream (`/api/event`) |
| --- | --- |
| (a) turn finished / idle | Works: `session.next.step.ended` with a `finish` other than a tool call; there is no separate idle event; `POST /api/session/{id}/wait` waits for the session |
| (b) waiting for input | Works: the same turn end; the session then takes the next prompt |
| (c) permission requested: detect | Works: `permission.v2.asked` with the request id, action and resources |
| (c) permission: answer | Works: `POST /api/session/{id}/permission/{requestID}/reply` with `once` wrote the file (`always` and `reject` exist) |
| (d) tool started / finished | Works: `session.next.tool.called`, `tool.success`, `tool.failed` |
| (e) crash / exit | Works: the event stream ended with a connection reset 0.5 s after the server was killed |
| Wake an idle session | Works: `POST /api/session/{id}/prompt` 8 s after the last turn ended; turn end about 2 s later |
| Event while busy | Fragile: a `delivery: "steer"` prompt was admitted during a running command, but after `POST .../interrupt` no turn-end event followed within 60 s: after an interrupt the host has to ask (`/wait`, the session list) instead of waiting for an event |
| Human attach / takeover | Not measured: the TUI is a client of the same server (`opencode attach`), documented, not exercised here |
| Model availability | Fragile: free models fail at random (`session.next.step.failed`, an upstream 400 or 403); the host has to treat a failed step as a turn end and retry |

The OpenCode TUI under a PTY or ConPTY was not measured: its server API
already gives every state structurally.

## Recommendation

**Claude Code, an interactive member (the pilot's case, a human watching):**
keep the TUI in the human's terminal and wake through hooks, with channels as
the opt-in idle path.

1. **The first increment, for 1aed:** a `Stop` hook in the home's managed
   settings runs an `aicrew-agent` command that asks the launcher, over the
   step channel, to wait for the member's inbox:
   - **On new, unacknowledged messages** it blocks the stop with a reason
     that names what arrived and the next action (read the inbox,
     acknowledge, act within the accepted attempt). The inbox stays
     authoritative; the reason is only a hint.
   - **Within its own timeout** (set long; 500 s waits held under a 900 s
     timeout) it lets the session stop.
   - **No model cost** while it waits, and the same behaviour on Windows
     and Linux.
2. **The idle gap is the remaining problem:** a session the hook has let
   stop can't be woken by a hook. The options, smallest first:
   - **(a) Channels:** an MCP server in the home pushes a hint to the idle
     session (measured: about 1.2 to 1.9 s). It needs the development flag
     and its start-up warning, and first-party authentication. Duplicates
     arrive twice and a hint during a busy turn may be ignored. So it is
     opt-in, and the hint is re-sent with bounded backoff while messages
     stay unacknowledged.
   - **(b) The launcher owns the client's terminal:** it runs the TUI in a
     PTY or ConPTY it relays to the human, and types a prompt when the
     `Notification` "waiting for your input" hook reports the member idle.
     Driving the client's pseudo terminal was measured to work on both
     platforms (injection, resize, paste, Ctrl-C). The relay to the human's
     terminal was not built, though. It makes the launcher a terminal proxy
     that types into a human's input line. That is a later supervisor task,
     not 1aed.
3. **Permissions:** detection works through `PermissionRequest` and
   answering is possible, but no auto-approval policy is proposed here (a
   non-goal). The launcher may report a pending permission (the hook fires)
   so that a coordinator learns a member is blocked.

**Claude Code, an unattended member (no human terminal):** headless
`-p --input-format stream-json --output-format stream-json`, driven by the
launcher, gives every state, wakes in about 20 ms by writing the next user
message, and answers permission requests through the stdio control protocol.
There is no human takeover: use it only for members nobody watches.

**OpenCode:** the launcher runs `opencode serve` and drives the member
through its V2 API and event stream. Every state is structured, permission
replies are programmatic, and a prompt wakes an idle session. The human
attaches with `opencode attach`. Not yet measured here: the interrupt's
missing turn-end event, and attach.

**Reject:**
- recognizing states from screen text (the hooks and the event streams
  give them structurally);
- typing permission answers by keystroke;
- relying on a channel or a hook as the source of truth: the aicrew inbox
  stays authoritative, and every hint is idempotent and may be lost or
  duplicated.

## Not measured

- OpenCode on Linux; OpenCode's TUI and `opencode attach`; Codex (deferred).
- A human taking over a pseudo terminal the launcher owns: the relay was not built.
- A `Stop` hook timeout beyond 900 s, and any upper bound Claude Code
  enforces on it.
- A channel event while a permission prompt is pending; the permission relay
  (`claude/channel/permission`).
- Sleep and resume, network loss, and very long idle periods.
- macOS.

## Reproduce

Every run used a disposable directory and the model's smallest tier.

- **Headless stream-json:**
  - Start `claude -p --input-format stream-json --output-format stream-json
    --verbose --model haiku --setting-sources project,local
    --strict-mcp-config --permission-mode default`, and write one JSON user
    message per line: `{"type":"user","message":{"role":"user","content":
    [{"type":"text","text":"..."}]}}`.
  - With `--permission-prompt-tool stdio`, a
    `{"type":"control_request","request_id":...,"request":{"subtype":
    "can_use_tool",...}}` line is answered with `{"type":"control_response",
    "response":{"subtype":"success","request_id":...,"response":{"behavior":
    "allow","updatedInput":...}}}`, or with `"behavior":"deny"` and a
    `message`.
- **Hooks:** a project `.claude/settings.json` names one command for
  `Stop`, `Notification`, `PermissionRequest`, `PreToolUse` and
  `PostToolUse`, with a `timeout` on `Stop`.
  - At `Stop`, the command reads its JSON input, waits for an inbox file,
    and prints `{"decision":"block","reason":"A new message arrived in your
    inbox: ..."}` or nothing.
  - At `PermissionRequest`, it may print `{"hookSpecificOutput":
    {"hookEventName":"PermissionRequest","decision":{"behavior":"allow"}}}`.
- **Channels:** an MCP stdio server answers `initialize` with the
  capabilities `{"experimental":{"claude/channel":{}},"tools":{}}`, and pushes
  `{"jsonrpc":"2.0","method":"notifications/claude/channel","params":
  {"content":"...","meta":{"message_id":"m1"}}}`. Claude starts with
  `--mcp-config` naming it and `--dangerously-load-development-channels
  server:<name>`. (On 2.1.289 the client first sends `server/discover`.)
- **Resize and paste:**
  - On Linux, set the PTY's size with `TIOCSWINSZ` and send `SIGWINCH`; on Windows, call `ResizePseudoConsole`.
  - The TUI's horizontal rule (`─`, U+2500, repeated) shows the width it drew at.
  - A paste is the bracketed form `ESC[200~` text `ESC[201~`, then Enter.
- **ConPTY on Windows:**
  - The child must not inherit the parent's redirected standard handles
    (`STARTF_USESTDHANDLES` with empty handles). Otherwise Claude sees no
    terminal and falls back to print mode.
  - The parent session's `CLAUDE*` environment variables must not reach the
    probe's client.
- **OpenCode:**
  - `opencode serve`, with a project `opencode.json` setting
    `"permission":{"edit":"ask","bash":"ask"}`.
  - `GET /api/event` (server-sent events), `POST /api/session` with a model
    and a location, then `POST /api/session/{id}/prompt` with
    `{"prompt":{"text":"..."}}`, and
    `POST /api/session/{id}/permission/{requestID}/reply` with
    `{"reply":"once"}`.
