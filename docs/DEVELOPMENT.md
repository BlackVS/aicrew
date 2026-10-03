# Development

## Toolchain

Aicrew is written in Go. `go.mod` declares `go 1.25.0`, and CI builds and
tests with the latest Go 1.25 patch release. A newer local toolchain works;
keep `GOTOOLCHAIN=local` if you do not want Go to download another toolchain.

The store uses SQLite through `modernc.org/sqlite`, a pure-Go driver. With
CGO disabled the build needs no C compiler and produces a static binary on
Linux, Windows and macOS. Aimem makes the same choice, so both services share
one toolchain and storage engine. The store opens the database in WAL mode
with foreign keys on, full sync, a busy timeout, and write transactions that
take the lock at `BEGIN`, so concurrent commands serialize instead of failing.

One store process serves a database file. `Open` holds an exclusive
operating-system lock on `<database>.lock` (`flock` on Linux, macOS and the
BSDs, `LockFileEx` on Windows) and refuses a second opener with
`store_in_use`, in the same process or another. The lock ends with `Close`
or with the process, however it ends; the `.lock` file stays behind and is
harmless, so there is nothing to clean up after a crash. Symbolic links are
resolved first, so every spelling of a path shares the lock. Hard links to
the database and network file systems are not supported (SQLite's WAL mode
needs a local file system anyway).

## Dependencies

- Every module version is pinned in `go.mod`, and `go.sum` holds the
  checksum of each module in the graph. CI runs `go mod verify` and fails if
  `go mod tidy` would change either file.
- A new dependency or version bump must have been published for at least
  seven days, and its provenance must be checked, before it is merged.
- GitHub Actions are pinned to a commit SHA, with the release tag in a
  comment.

Current direct dependencies:

- `modernc.org/sqlite v1.58.0` (published 2026-09-01, the same version aimem
  uses).
- `golang.org/x/sys v0.47.0` (published 2026-06-30, Go project), for the
  store's file lock. It was already in the module graph through the SQLite
  driver at this version.

## Checks

Run these before every push; CI runs the same:

```sh
bash scripts/check-repo.sh     # repository hygiene
gofmt -l .                     # must print nothing
go vet ./...
GOOS=linux go vet -tags realaimem ./e2e/...   # the real-aimem harness compiles
go mod verify
go mod tidy -diff              # must print nothing
go test -count=1 ./...
CGO_ENABLED=0 go build ./...
bash scripts/release.sh build v0.0.0   # every release asset, sums, stamp check
bash scripts/release_test.sh           # the release script's refusals
```

CI check names: `repo-checks`, `go-lint`, `go-test (ubuntu-latest)`,
`go-test (windows-latest)`, `go-test (macos-latest)`, `go-build`,
`release-build`.

`go-test (ubuntu-latest)` also runs the agent, server and store packages
under the race detector:

```sh
CGO_ENABLED=1 go test -race -count=1 ./internal/agent/ ./internal/server/ ./internal/store/
```

It needs cgo (a C compiler). On a Windows host without one, run it in WSL.

The end-to-end runs against a real aimem are not part of these checks:
`scripts/e2e-real-aimem.sh` (`docs/E2E-REAL-AIMEM.md`).

## Layout

- `cmd/aicrewd`: the aicrew HTTPS service (below).
- `cmd/aicrew`: the operator's command line (below).
- `cmd/aicrew-agent`: an agent's client; it holds no store (below).
- `internal/server`: the service's configuration, TLS listener, request
  bounds, logging and routes, including aimem's session introspection.
- `internal/store`: the aicrew coordination store. It is internal and has
  no MCP surface; only `aicrewd` exposes anything over the network.
  See the package documentation for the rules it enforces.
- `internal/verifier`: the store's production verifier, which redeems
  aimem proof receipts (`docs/CREW-CONTRACT.md`, "Receipt redemption").
  `aicrewd` configures it from the `aimem` section below.
- `internal/agent`: the agent client's session engine: proof, entry or
  resume, aimem's binding, handle refresh and leave.
- `internal/tlstrust`: the TLS trust bindings (`ca_dns`, `spki_sha256`)
  shared by the verifier and the agent client.
- `internal/filelock`: a waiting, exclusive operating-system file lock.
- `internal/privatefile`: creates a file for a secret, exclusively and
  readable by its owner only, and checks that an existing secret file is
  private (mode bits on Unix, the effective DACL on Windows).
  `privatefiletest` weakens a file for tests only.

## Running aicrewd

`aicrewd` opens the store and serves HTTPS on one listener, over TLS it
terminates itself (TLS 1.2 or later). It has no plain-HTTP listener and does
not run behind a TLS-terminating proxy. Its routes are listed below.

```sh
CGO_ENABLED=0 go build -o bin/aicrewd ./cmd/aicrewd
bin/aicrewd -config aicrewd.json
```

The configuration file is JSON with no unknown fields. It names files and
an address; it holds no secret itself:

```json
{
  "store_path": "/var/lib/aicrew/aicrew.db",
  "listen_addr": "0.0.0.0:8443",
  "tls_cert_file": "/etc/aicrew/tls/cert.pem",
  "tls_key_file": "/etc/aicrew/tls/key.pem",
  "service_id": "aicrew-example",
  "operator_token_file": "/etc/aicrew/operator.token",
  "shutdown_timeout": "15s"
}
```

- `service_id` is the ID aimem registers this service under (identity.v1:
  1 to 128 characters from `[A-Za-z0-9._:-]`).
- `operator_token_file` is required. It holds the operator credential that
  authorizes the operator API (below). It must be readable by the service's
  account only, and hold one token alone on its line, as
  `aicrew operator-token new` writes it; the service refuses to start
  otherwise.
- `shutdown_timeout` is optional (default 15 s, at most 5 min).
- `aimem` is optional. Without it, session entry and resume are refused;
  with it, the service redeems agents' proofs with that aimem hub:

  ```json
  "aimem": {
    "base_url": "https://aimem.example:8443",
    "tls_trust_mode": "ca_dns",
    "tls_trust_value": "aimem.example",
    "redemption_token_file": "/etc/aicrew/aimem-redemption.token",
    "read_token_file": "/etc/aicrew/aimem-read.token"
  }
  ```

  `base_url` is aimem's https origin. `tls_trust_mode` is `ca_dns` (the
  value is the origin's host) or `spki_sha256` (`sha256-` and the base64
  SHA-256 of aimem's public key). The token file holds the redemption
  bearer aimem issued to this service; it must be readable by the service's
  account only, and the service refuses to start otherwise. It is read on
  every redemption, so replacing it rotates the bearer without a restart.
- `read_token_file` is optional. It holds the separate `reservation.read`
  credential aimem issued to this service (`aimem_peer_` and 64 lowercase
  hex), which reads aimem's reservation scope: the receipts and holds this
  service's proofs established (`docs/CREW-CONTRACT.md`, "Attempt steps").
  With it, a member's settle resolves from aimem's answers; without it,
  member-driven steps stay pending. The service refuses to start if the file
  is missing, readable by another account, not a peer credential, the
  redemption file, or holding the redemption credential. It is read on every
  call, like the redemption bearer. A refused or unreadable answer from
  aimem never counts as "nothing committed": the step stays pending.
- Keep the TLS key readable only by the service's account.

The service logs JSON lines to stderr: each request's method, matched route,
status and duration, never its headers, body, query or raw path. A request
body is capped at 64 KiB and headers at 16 KiB; the server also sets
read-header, read, write and idle timeouts. On SIGINT or SIGTERM it stops
accepting connections, lets requests in flight finish within
`shutdown_timeout`, closes the store and exits 0.

Besides `GET /healthz`, it serves aimem's session introspection,
`POST /v1/crew/introspect` (`docs/CREW-CONTRACT.md`, "Session
introspection"). Aimem registers this service with the route's full https
URL, the service ID and the TLS trust binding of this certificate. It also
serves agents' clients: `POST /v1/crew/challenges`, `POST /v1/crew/token`,
`GET /v1/crew/session`, `POST /v1/crew/session/leave`, the attempt step
routes, and the member inbox, `GET /v1/crew/inbox` and
`POST /v1/crew/inbox/ack` (`docs/CREW-CONTRACT.md`, "Client session API").

## The operator API

`aicrewd` serves the operator's administration on its own listener, so the
service keeps running while the operator manages teams, invitations and
introspection credentials. The routes, under `/v1/admin/`, and their JSON
are defined in `internal/opapi`:

| Route | Operation |
| --- | --- |
| `GET /v1/admin/introspection-credentials?hub=HUB` | list credentials (metadata) |
| `POST /v1/admin/introspection-credentials` | issue: `{"hub_id", "operations"}`; the answer carries the bearer, once |
| `POST /v1/admin/introspection-credentials/rotate` | issue a replacement for the hub's one active credential; the answer names it in `replaces` |
| `POST /v1/admin/introspection-credentials/revoke` | revoke: `{"id"}` |
| `GET /v1/admin/teams` | list teams |
| `POST /v1/admin/teams` | create: `{"name", "projects"}` |
| `GET /v1/admin/team?id=TEAM` | show a team and its members |
| `POST /v1/admin/team/projects` | `{"team_id", "expected_revision", "projects"}` |
| `POST /v1/admin/team/rename` | `{"team_id", "expected_revision", "name"}` |
| `GET /v1/admin/invitations?team=TEAM` | list invitations (metadata) |
| `POST /v1/admin/invitations` | issue: `{"purpose", "team_id", "role", "hub_id", "label", "agent_id", "expected_user_id", "ttl"}`; the answer carries the code, once |
| `POST /v1/admin/invitations/revoke` | revoke: `{"id"}` |

**The operator credential.**
- **Required on every route.** Every route requires
  `Authorization: Bearer <operator token>`. A missing or wrong bearer, a
  member's session token and aimem's introspection bearer are all refused
  with `401 unauthorized`, and the operator routes never consult the member
  session API.
- **Creating it.** `aicrew operator-token new -file PATH` writes a new token
  (`aop_` and 64 lowercase hex) to a new owner-only file. It opens no store
  and calls no service, and the token is never printed. Name that file as
  `operator_token_file`, and keep the operator's own copy owner-only.
- **Rotation.** The service reads the file on every operator call, so
  replacing it rotates the credential without a restart. A file that
  becomes unreadable fails closed (`503 operator_unavailable`).
- **Failed attempts.** Failed authentications are limited to 10 a minute per
  client address. Once that budget is spent, the address is refused with
  `429` before its bearer is compared.

**Secrets and errors.**
- **Once-only secrets.** A credential's bearer and an invitation's code exist
  only in the answer that issued them; the store keeps digests, and a list
  never carries either.
- **No client keys.** Every write is its own command: the service takes no
  client idempotency key, because a replayed issue could not answer its
  secret again.
- **Error codes.** Team names are unique (`409 team_exists`), a stale
  `expected_revision` is `409 revision_conflict`, and validation failures
  are `400 invalid_request` with the store's message.
- **The log.** Each operator action is logged as `operator` with its action
  (`team.create`, `invitation.issue`, ...), its outcome and the ID it
  touched, besides the request line; never a body, a bearer or a code.

`aicrew` still administers through the store file, below, until its
client moves onto this API.

## Introspection credentials

Aimem authenticates its introspection and coordination-fact calls with a
credential that aicrew issues for one aimem hub. The operator manages them with `aicrew`, which
opens the store file directly: only one process may hold a store, so stop
`aicrewd` first.

```sh
CGO_ENABLED=0 go build -o bin/aicrew ./cmd/aicrew
bin/aicrew introspection-credential issue  -store aicrew.db -hub HUB -secret-file introspection.secret
bin/aicrew introspection-credential list   -store aicrew.db
bin/aicrew introspection-credential rotate -store aicrew.db -hub HUB -secret-file introspection-2.secret
bin/aicrew introspection-credential issue  -store aicrew.db -hub HUB -secret-file intro-only.secret -operations introspection
bin/aicrew introspection-credential revoke -store aicrew.db -id ID
```

- The bearer is written only to the `-secret-file`, which must not exist;
  it is created readable by its owner only (mode 0600, or an owner-only
  protected DACL on Windows). The command prints the credential's metadata,
  never the bearer. If the file cannot be written, the credential just
  issued is revoked.
- Hand the file to aimem's operator through a private channel; aimem reads
  it from `AIMEM_INTROSPECTION_TOKEN_FILE`. Delete aicrew's copy afterwards.
- A hub has at most two active credentials. To rotate: `rotate` issues the
  second, aimem moves to it, then `revoke` the first.
- `-operations` names what a new credential permits: `introspection`,
  `coordination`, or both, which is the default. `list` shows each
  credential's operations.
- A credential issued before coordination facts existed permits
  introspection only. To enable coordination for that hub, `rotate` it:
  the new credential permits both, aimem moves to it, then `revoke` the old
  one.

## Reconciliation

With `aimem.read_token_file` configured, aicrewd also runs its
reconciliation loop: every 15 s it settles the steps members left pending
and closes as recovered the attempts whose reservation aimem closed outside
aicrew, reading at most 30 times a minute (`docs/CREW-CONTRACT.md`,
"Reconciliation by aicrewd"). It logs each recovered closure. Without the
read credential the loop does not run.

## Teams

A team has a name and its intended projects (`HUB_ID/PROJECT_ID`; the list
grants no aimem access, and aicrewd refuses an offer outside it). The
operator manages teams with `aicrew`, which opens the store file directly:
stop `aicrewd` first. Each command prints the team as JSON; `list` adds each
team's member count and `show` its current members.

```sh
bin/aicrew team create   -store aicrew.db -name crew -project HUB_ID/PROJECT_ID
bin/aicrew team list     -store aicrew.db
bin/aicrew team show     -store aicrew.db -team TEAM
bin/aicrew team projects -store aicrew.db -team TEAM -expect-revision N -project HUB_ID/PROJECT_ID
bin/aicrew team rename   -store aicrew.db -team TEAM -expect-revision N -name crew-2
```

- `create` and `rename` refuse a name another team already has
  (`team_exists`).
- `projects` replaces the whole set; with no `-project` it clears it.
  `projects` and `rename` apply only to the revision `show` or `list`
  printed, and refuse a team that changed since (`revision_conflict`).
- Members join through invitations (below); `aicrew team` does not change
  membership.

## Invitations

An invitation lets one agent join a team, or link or rebind an agent record
(`docs/ONBOARDING-CONTRACT.md`). The operator manages them with `aicrew`,
which opens the store file directly, like the credentials above: stop
`aicrewd` first.

```sh
bin/aicrew invitation issue  -store aicrew.db -team TEAM -role worker -hub HUB_ID -label builder -expect-user AIMEM_USER_ID
bin/aicrew invitation issue  -store aicrew.db -team TEAM -role worker -hub HUB_ID -purpose link -agent AGENT -code-file invite.code
bin/aicrew invitation issue  -store aicrew.db -team TEAM -role worker -hub HUB_ID -purpose rebind -agent AGENT -expect-user AIMEM_USER_ID
bin/aicrew invitation list   -store aicrew.db [-team TEAM]
bin/aicrew invitation revoke -store aicrew.db -id INVITATION
```

- The code is shown once and never kept: the store holds only its digest.
  `issue` prints it only to a terminal, after the invitation's metadata. Off
  a terminal it refuses, before issuing anything, unless `-code-file` names
  a new file. That file must not exist, and is created readable by its owner
  only. If it cannot be written, the invitation just issued is revoked.
- The code is never an argument. Give it privately to the person running
  the agent, who enters it at the client's hidden prompt.
- `-purpose` is `join` (the default; names the new agent's `-label`),
  `link` or `rebind` (each names the `-agent`).
  - `-expect-user` pins the aimem user the proof must name. It is required
    for `rebind`. For `join` and `link` it is optional, and `issue` warns
    without it: anyone holding the code and an aimem credential for the hub
    could redeem it.
- `-expires` sets the lifetime: 24 hours by default, at most 72.
- `list` shows metadata only (state, attempts, expiry), never a code.
  `revoke` ends an invitation that is not yet redeemed. Undoing a redeemed
  one means removing the membership.

## Running aicrew-agent

`aicrew-agent` is an agent's client. It opens no store and needs no operator
authority: it proves the agent's aimem identity and keeps the agent's team
session through `aicrewd`'s client session API (`docs/CREW-CONTRACT.md`,
"Client session API").

```sh
CGO_ENABLED=0 go build -o bin/aicrew-agent ./cmd/aicrew-agent
bin/aicrew-agent run -client claude -home ~/aicrew/agents/builder
bin/aicrew-agent session start  -home ~/aicrew/agents/builder
bin/aicrew-agent session status -home ~/aicrew/agents/builder
bin/aicrew-agent session leave  -home ~/aicrew/agents/builder
bin/aicrew-agent step pending   # from the client run started ("Driving steps")
bin/aicrew-agent inbox          # likewise ("Reading the inbox")
```

It reads the `aicrew` section of the agent home's `agent.json`, which holds
no secret; other sections belong to onboarding:

```json
"aicrew": {
  "url": "https://aicrew.example:8443",
  "tls_trust_mode": "ca_dns",
  "tls_trust_value": "aicrew.example",
  "agent_id": "01a0...",
  "team_id": "01a0...",
  "aimem_command": "aimem",
  "aimem_hub": "main",
  "client_command": "claude"
}
```

- `run -client claude|opencode` does what `session start` does, then
  starts the client (from `PATH`, or `client_command` in the `aicrew`
  section) as its child in the agent home, with `AIMEM_TEAM_SESSION` set in
  that child's environment only; arguments after `--` go to the client.
  When the client exits, it leaves and closes aimem's binding, exiting with
  the client's code (128 plus the signal if a signal ended it), or 3 if
  open work kept the session. Before the client starts, Ctrl-C or SIGTERM
  stops the startup, including its retries, and leaves any session it
  entered; the client then never starts, and one that races the client's
  start stops it at once. Once the client runs, the launcher
  does not exit on Ctrl-C, which the terminal delivers to the client;
  SIGTERM is forwarded to the client, which is killed if still running
  10 s later. A killed launcher takes the client with it on Linux
  (parent-death signal) and Windows (job object); on macOS the client keeps
  running without team access once its handle expires, within 15 minutes,
  and the next `run` resumes the session.
- `session start` enters the team, or resumes the session a previous run
  recorded, binds aimem to it with `aimem team-session open` (or `refresh`
  when aimem already holds the session's file), prints
  `AIMEM_TEAM_SESSION=<path>`, and keeps the session until interrupted: it
  refreshes the handle when a third of its life remains (never later than
  90 s before it expires) and resumes with a new proof 10 minutes before
  the session token's 8-hour ceiling. On SIGINT or SIGTERM it leaves, then
  runs `aimem team-session close`; a second interrupt stops it at once, and
  the next `start` resumes the session.
- `session status` shows the recorded session and aimem's binding, without
  secrets and without calling `aicrewd`.
- `session leave` proves afresh and resumes the recorded session, which
  fences any client still holding it, then leaves. It never enters the
  team: if the recorded session has already ended, it only closes aimem's
  binding of it and clears the record.
- A proof that aicrewd refuses while its challenge is still valid (the
  receipt lives only 60 s) is renewed for the same challenge, at most three
  times, under a new request key.
- Exit codes: 0 done, 1 failed (the refusal's next action is logged),
  2 usage, 3 the session was kept because the member has open work
  (reconcile it through aicrew, then leave again).
- The session token, the proof receipt and the handle stay in memory and
  reach aimem only on stdin or through a pipe. The agent home's
  `state/aicrew-session.json` records only the session, team, service, hub
  and aimem file path, so that a restarted client resumes.
- aimem's lifecycle commands for one session never overlap, across every
  `aicrew-agent` process of the agent home: each holds an exclusive lock on
  a file under `state/locks/` named for the session, so a close issued
  while a refresh runs, here or in another process, waits for it.
- The session is recorded before aimem is asked to bind it, so a binding
  that fails still leaves a session the next `start` resumes.

### Joining a team: `aicrew-agent join`

The client bootstrap (`docs/ONBOARDING-CONTRACT.md`, "The client's view")
redeems an invitation and prepares the agent home (`docs/WORKSPACE.md`):

```sh
bin/aicrew-agent join -label builder -url https://aicrew.example:8443 \
  -tls-trust-mode ca_dns -tls-trust-value aicrew.example -aimem-hub main -client claude
bin/aicrew-agent join -home ~/aicrew/agents/builder    # rerun: refresh and check
```

- **The operator provisions the home's aimem installation once, before
  `join`** (`docs/WORKSPACE.md`, "The member's aimem installation"). With
  the home's two variables set for these two commands only, it gives that
  installation the hub and the member's individual credential:

  ```sh
  export AIMEM_STATE_DIR=~/aicrew/agents/builder/aimem   # absolute paths
  export AIMEM_SOCKET=~/aicrew/agents/builder/aimem/aimem.sock
  aimem hub add main https://aimem.example --token-file - [--ca-file ca.pem]
  aimem hub task-token main --token-file -
  ```

  In PowerShell, set them with
  `$env:AIMEM_STATE_DIR = "$HOME\aicrew\agents\builder\aimem"` and
  `$env:AIMEM_SOCKET = "$env:AIMEM_STATE_DIR\aimem.sock"`. Either way, use
  a shell you close afterwards, so that the operator's own aimem is not
  pointed at the member's installation. A `join` on a home not provisioned
  yet stops with these exact paths.

  Each command reads the member's user-scoped token on standard input,
  never from an argument. The member sets nothing: `join`, `check` and `run` give every aimem process they start
  these two variables, replacing inherited values. `join` writes the same
  two variables into the home's `.claude/settings.json` and `.mcp.json`
  for clients started there by hand. `CLAUDE_CONFIG_DIR` stays an optional
  override of the member's Claude Code directory.
- `-home` defaults to `~/aicrew/agents/<label>` (`%USERPROFILE%\aicrew\agents\<label>`
  on Windows). `-aimem-hub` is aimem's name for the hub whose identity the
  invitation names; `-aimem-command` overrides the `aimem` executable.
  `-client claude|opencode` (or both, comma-separated) names the clients
  the home is for: required on the first run, recorded in `agent.json`
  after. `-json` prints the report as JSON.
- The invitation code is read only at a hidden prompt on a terminal, never
  from an argument, a pipe, a file or the environment; off a terminal the
  command refuses. A code with a typing error (its checksum) is caught
  before it costs an invitation attempt, and asked for again: at most three
  prompts in all.
- Before the prompt it checks the individual aimem credential in the home's
  installation with `aimem hub credential <hub> --json`. A missing, refused
  or unconfirmed credential, or a hub that installation does not know, stops
  the run with the provisioning instruction above. An answer that is not a
  credential status stops it too. Only an aimem without that command (it answers with
  its usage) is left to the identity proof, and a proof that names the
  missing credential gives the same instruction.
- It then begins the redemption, proves the identity with
  `aimem identity proof`, and completes it, recovering on its own: a lost
  reply or a retryable refusal is retried with the same key, a refused
  receipt gets a new one for the same challenge, and an expired challenge a
  new begin. It stops on `invitation_invalid`, `identity_already_linked`,
  `role_conflict`, `identity_mismatch`, `work_outstanding`,
  `credential_inactive` and `aimem_unconfigured`, each with what to do.
- `state/aicrew-join.json` keeps the begin and completion keys and the
  challenge, never the code or a receipt, so a rerun with the same code
  after a crash or a lost reply resumes with the same keys. It is removed
  once the home is linked.
- A linked home (its `agent.json` names the agent and team) is only
  refreshed: no prompt, no identity proof and no `aicrewd` call. One home
  serves one team, so options naming another server, trust or aimem hub
  are refused.
- Every run ends with the dependency and client check below, and its
  report includes the check's.
- The report is `ready`, `restart_required` (a managed guidance file or the
  client wiring changed, so restart any client open in the home) or
  `blocked` with its instructions; exit 0 for the first two, 1 when blocked
  or failed, 2 on usage. A blocked check leaves the home linked: rerun
  after following its instructions. It opens no session: it prints the
  `session start` command.
- It writes no secret. `creds/` is created empty and owner-only. `aimem/`
  is created owner-only, or restricted if the operator's provisioning created
  it; the individual aimem credential stays there, in aimem's own storage.
- It writes the managed `.claude/settings.json` (`env`: the home's
  `AIMEM_STATE_DIR` and `AIMEM_SOCKET`, absolute), under the same digest
  rule as the guidance files.

### Checking dependencies and clients: `aicrew-agent check`

```sh
bin/aicrew-agent check -home ~/aicrew/agents/builder [-client claude|opencode] [-json]
bin/aicrew-agent version [-json]
```

The check (`join` runs it at its end) installs nothing. It runs every aimem
call and client probe with the home's aimem installation, as `run` does. A
member who shares an OS account sets nothing for aimem. `CLAUDE_CONFIG_DIR`
is optional: when set, the check probes Claude Code with it and reads the
member's user-level skills under it. It reports:

- **Versions against the supported set**, which is embedded in the build
  (`internal/agent/supported.json`):
  - aimem from `aimem version`;
  - ai-skills from its installer's `.ai-skills.json` beside the skills the
    clients read. The ai-skills installer does not write it yet
    ([aiskills#24](https://github.com/BlackVS/aiskills/issues/24)); until a
    release does, the ai-skills version is "unknown";
  - each selected client from `--version`.

  Older than the minimum gives `blocked`. Newer than tested gives a notice.
  A source build's or an unrecorded version is "unknown": a notice that
  neither blocks nor counts as supported.
- **The home's aimem installation.**
  - **The credential.** The home's installation must hold an individual
    credential for `agent.json`'s `aimem_hub`. A missing or refused one, or
    an unknown hub, blocks with the provisioning instruction; an
    unreachable hub gives a notice.
  - **The carriers.** `.claude/settings.json` and the `.mcp.json` aimem
    entry must name the home's installation; a carrier that does not blocks
    (rerun `join`, merging any `.aicrew-new`). This check applies when
    Claude Code is selected.
  - **Notices, without blocking:**
    - `AIMEM_*` variables in the environment or in `~/.config/aimem/env`
      that name another installation, reported by name only;
    - an aimem MCP server at user scope, or at local scope for the home, in
      Claude Code's `.claude.json`;
    - a socket path over the Unix limit.
- **The client wiring.** One MCP entry per selected client, inside the
  home only: `.mcp.json` `mcpServers.aimem` for Claude Code, and
  `opencode.json` `mcp.aimem` for OpenCode, both running
  `<aimem_command> mcp`. Claude Code's entry carries the home's
  `AIMEM_STATE_DIR` and `AIMEM_SOCKET` in its `env`.
  - The entry is managed like the guidance files: added when missing,
    updated only while it matches its recorded digest, and otherwise left
    alone with the proposed file written as `<file>.aicrew-new`.
  - Other keys in those files are kept.
  - No hook is installed, and no user-level client configuration is
    written.
- **What the client sees.** Each selected client is asked whether aimem's
  MCP server runs in the home and whether the required skills are
  visible. There is no model call: the model endpoint is a local port that
  closes every connection, and the key is a dummy.
  - Claude Code: the print-mode init event and `claude mcp list`.
  - OpenCode 1.x: `mcp list` and `debug skill`.
  - OpenCode 2.x: its own `serve`, read through `/api/mcp` and `/api/skill`.

  Each step is bounded (90 s). Print mode never shows Claude Code's
  workspace-trust dialog. A project server Claude Code has not yet approved
  for interactive use is reported as a notice: the first interactive start
  in the home asks to trust the folder and to approve it.
- **The instructions of a blocked report** are exact and pinned:
  - aimem's verifying boot script with `AIMEM_VERSION`;
  - the ai-skills release archive checked against its `SHA256SUMS` and
    installed with `install.sh --user -t claude -s <skills>` (OpenCode reads
    the same directory), because the ai-skills one-line boot does not
    verify its download yet
    ([aiskills#25](https://github.com/BlackVS/aiskills/issues/25));
  - the client's npm package at the tested version.

  OpenCode 1.x refusing data that OpenCode 2 wrote is reported, not
  resolved.

`version` reports the build: a release build stamps
`github.com/BlackVS/aicrew/internal/version.Override` with `-ldflags -X`;
a source build reports `dev` and its commit.

### Driving steps: `aicrew-agent step`

The launcher drives every attempt step for its client, which never holds the
session token or a coordination proof. The client (the model, or a script in
its conversation) asks the launcher for one step at a time:

```sh
aicrew-agent step claim   -body - < claim.json
aicrew-agent step work    -attempt A1 -task T1 -body '{"intent":"submit","detail":"https://forge.example/pr/12"}'
aicrew-agent step release -attempt A1 -task T1 -body '{"target":"BLOCKED","blocker":"waiting on design"}'
aicrew-agent step confirm-stop -attempt A1
aicrew-agent step pending
aicrew-agent step recover
```

- **Operations.** The reservation steps `offer`, `claim`, `accept`,
  `withdraw`, `work`, `release` and `finalize` take the begin route's body
  (`docs/CREW-CONTRACT.md`, "Attempt steps"). `offer` and `claim` create
  their attempt and name the task in the body. The others need `-attempt`
  and the aimem task's `-task`. The local steps `decline`, `review`, `stop`,
  `confirm-stop` and `confirm-delivery` need only `-attempt` and their body.
  `pending` lists the recorded steps, and `recover` finishes them.
- **Finding the launcher.** `-home` defaults to `AICREW_AGENT_HOME`, which
  `run` sets in its client's environment only, to the agent home's absolute
  path. The launcher listens on the Unix socket `state/step.sock` of that
  home. The socket exists only while `run` runs. Without it, `step` exits 1
  and says that no launcher serves the home. A client of another agent home
  reaches only that home's launcher.
- **Privacy.** The socket is reachable only through `state/`, which the
  launcher makes private before listening and then verifies:
  - on Unix, mode 0700, with the socket at 0600;
  - on Windows, a protected DACL for the current user, SYSTEM and
    Administrators, which the socket inherits. The socket's own inherited
    DACL is what refuses other accounts, since an account allowed to
    bypass traverse checking reaches a file by its path whatever its
    directory allows.

  An existing `state/` is restricted the same way. If the launcher cannot
  make it private, or cannot listen (a Unix socket's path is limited to
  about 104 bytes), it logs a warning and runs the client without the
  channel. Nothing secret crosses the socket: an answer carries the step's
  outcome, never the token or a proof.
- **What the launcher does.** For a reservation step, the launcher:
  1. begins the step under an `Idempotency-Key`, which it records first;
  2. records the step;
  3. composes aimem's body from the begin's values, with the task's
     complete content read through `aimem mcp` `get_task`;
  4. sends it with `aimem reservation OP --task T --key K`, with the body,
     and any proof, on stdin only;
  5. settles the step.

  If the task's revision moved since the begin, nothing is sent, and the
  step is refused as `stale_revision`. Steps run one at a time.
- **Pending records.** Each step in flight is recorded in
  `state/steps/<begin key>.json` (0600, replaced atomically). The record
  holds the phase, the request, the attempt, the step's nonsecret values
  and the proof's SHA-256, never the proof or the token. A settled step's
  record is removed. A pending settlement keeps it.
- **Recovery.** `run` finishes the recorded steps in the background as soon
  as it serves the channel, and `step recover` does so on request. Each
  step is finished with its recorded begin key and aimem key:
  - a step aimem has not answered begins again under the same key, for a
    replacement proof, then is sent;
  - a step aimem answered is settled;
  - a first begin that aicrewd refuses (not retryably) keeps no record, but
    a refusal during recovery keeps it, because a lost begin may have
    committed.
- **Answers.** `step` prints one JSON answer, whose fields are `ok`,
  `status`, `result`, and `error` with `code`, `message`, `retryable` and
  `next_action`. Its exit codes:
  - 0: `done`, committed or answered;
  - 3: `refused`, by aicrewd or aimem, or settled as not committed;
  - 4: `pending`, recorded but not settled; run `step recover` later;
  - 1: `failed`, meaning no launcher, or aicrewd, aimem or the channel
    failed;
  - 2: usage.

### Reading the inbox: `aicrew-agent inbox`

Offers, acceptances, submissions, stops and other lifecycle messages reach
a member's team inbox, each naming its attempt; a worker accepts an offer by
the attempt ID its inbox shows, and reads the offer's base commit, branch,
process pin, instruction digest and expiry from the offer's message. The
agent home's managed `docs/ROLES.md` teaches each role these steps. The client reads it through the launcher,
which holds the session, like a step:

```sh
aicrew-agent inbox                 # the oldest unacknowledged messages, 20 at most
aicrew-agent inbox -limit 50 -json
aicrew-agent inbox -ack ID,ID      # acknowledge what was handled
```

- A message stays in every later read until it is acknowledged, so a
  crash between reading and acting loses nothing; the next page comes once
  the current one is acknowledged.
- A page holds at most `-limit` messages and at most 128 KiB of JSON, and
  always at least one message; only the messages returned are recorded as
  delivered.
- Only messages the inbox delivered can be acknowledged
  (`message_not_delivered` otherwise); acknowledging twice reports them as
  `already`.
- Exit codes and the no-launcher failure are `step`'s.
- aicrewd serves it as `GET /v1/crew/inbox` and `POST /v1/crew/inbox/ack`
  (`docs/CREW-CONTRACT.md`, "Client session API").

## Releasing

A release is a `vX.Y.Z` tag on a commit of main. Cutting it is the
operator's decision. `.github/workflows/release.yml` builds and publishes it;
`scripts/release.sh` holds the steps, and CI runs its build and its tests on
every pull request (`release-build`), publishing nothing.

**The CHANGELOG rule.**
- Ordinary pull requests do not edit `CHANGELOG.md`. Parallel pull requests
  would collide on it, and resolving that collision turns a base-only update
  into an edited one, which costs a new review.
- One release-preparation pull request writes the version's section,
  `## [X.Y.Z] - YYYY-MM-DD`, from the titles of the pull requests merged since
  the last tag. That section is the release's notes.
- The workflow refuses a tag whose section is missing.

**Cutting a release:**

1. Merge the release-preparation pull request (the CHANGELOG section).
2. Tag main and push the tag:

   ```sh
   git fetch origin && git tag -a vX.Y.Z origin/main -m "aicrew X.Y.Z" && git push origin vX.Y.Z
   ```

3. The workflow checks the release, refusing it when:
   - the tag is not `vX.Y.Z` (no pre-release or build suffix);
   - its commit is not on main;
   - the section is missing;
   - the release already exists.

   It then runs the tests, builds and checks that the built `aicrew-agent`
   reports exactly the tag. A separate job, the only one allowed to write,
   publishes with the `gh` CLI. A release is marked latest only when no
   higher tag exists.
4. Running the workflow by hand from main republishes an **existing** tag (after
   a failed run). It never creates a tag. With `dry_run` (the default) it
   checks, builds, stamps and prints the sums of a tag that need not exist yet,
   main's tip standing in, and publishes nothing. Use it before the first real
   tag.

   Its sums are not the release's, by design. Go stamps the main module's
   version from the VCS tag (`go version -m` shows
   `mod github.com/BlackVS/aicrew v0.1.0` in a released binary), and a dry run
   builds before the tag exists. So every binary differs while `LICENSE`
   matches (v0.1.0: dry run 37019427980 and release run 37019971021, same
   commit and Go version, all 15 binary digests different). A dry run proves
   the build, the checks and the asset list, not the digests. The published
   sums are reproduced by rebuilding from the tag with the same Go version and
   the same flags (`-trimpath`, `-s -w`, `CGO_ENABLED=0`, an unmodified tree).

**What is published:**
- `aicrewd`, `aicrew` and `aicrew-agent` for linux/amd64, linux/arm64,
  darwin/amd64, darwin/arm64 and windows/amd64, as single binaries named
  `<binary>-<os>-<arch>` (`.exe` on Windows);
- `LICENSE`;
- `SHA256SUMS` over every other asset.

The notes end with the license URL and its `Required Notice:` lines. Each
binary is stamped with the tag through
`-ldflags "-X github.com/BlackVS/aicrew/internal/version.Override=vX.Y.Z"`,
and reports it with `aicrewd -version`, `aicrew version` and
`aicrew-agent version` (`-json` for the build as JSON).

**Verifying a download.** In the directory holding the downloaded assets and
`SHA256SUMS`:

- Linux:

  ```sh
  sha256sum --ignore-missing -c SHA256SUMS
  ```

- macOS:

  ```sh
  shasum -a 256 --ignore-missing -c SHA256SUMS
  ```

- Windows (PowerShell), for each downloaded file, here `aicrew-agent-windows-amd64.exe`:

  ```powershell
  $h = (Get-FileHash aicrew-agent-windows-amd64.exe -Algorithm SHA256).Hash.ToLower()
  if (-not (Select-String -Path SHA256SUMS -Pattern "^$h\s+aicrew-agent-windows-amd64.exe$" -Quiet)) { throw 'checksum mismatch' }
  ```

Then the binary's version must be the release's.
