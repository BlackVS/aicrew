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
go mod verify
go mod tidy -diff              # must print nothing
go test -count=1 ./...
CGO_ENABLED=0 go build ./...
```

CI check names: `repo-checks`, `go-lint`, `go-test (ubuntu-latest)`,
`go-test (windows-latest)`, `go-test (macos-latest)`, `go-build`.

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
  "shutdown_timeout": "15s"
}
```

- `service_id` is the ID aimem registers this service under (identity.v1:
  1 to 128 characters from `[A-Za-z0-9._:-]`).
- `shutdown_timeout` is optional (default 15 s, at most 5 min).
- `aimem` is optional. Without it, session entry and resume are refused;
  with it, the service redeems agents' proofs with that aimem hub:

  ```json
  "aimem": {
    "base_url": "https://aimem.example:8443",
    "tls_trust_mode": "ca_dns",
    "tls_trust_value": "aimem.example",
    "redemption_token_file": "/etc/aicrew/aimem-redemption.token"
  }
  ```

  `base_url` is aimem's https origin. `tls_trust_mode` is `ca_dns` (the
  value is the origin's host) or `spki_sha256` (`sha256-` and the base64
  SHA-256 of aimem's public key). The token file holds the redemption
  bearer aimem issued to this service; it must be readable by the service's
  account only, and the service refuses to start otherwise. It is read on
  every redemption, so replacing it rotates the bearer without a restart.
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
`GET /v1/crew/session` and `POST /v1/crew/session/leave`
(`docs/CREW-CONTRACT.md`, "Client session API").

## Introspection credentials

Aimem authenticates its introspection calls with a credential that aicrew
issues for one aimem hub. The operator manages them with `aicrew`, which
opens the store file directly: only one process may hold a store, so stop
`aicrewd` first.

```sh
CGO_ENABLED=0 go build -o bin/aicrew ./cmd/aicrew
bin/aicrew introspection-credential issue  -store aicrew.db -hub HUB -secret-file introspection.secret
bin/aicrew introspection-credential list   -store aicrew.db
bin/aicrew introspection-credential rotate -store aicrew.db -hub HUB -secret-file introspection-2.secret
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
