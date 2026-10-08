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
- `internal/server`: the service's TLS listener, request bounds, logging
  and routes, including aimem's session introspection.
- `internal/svcconfig`: `aicrewd.json`: its shape, the checks that need no
  aimem client, and its rewriting by `aicrewd config migrate` and
  `aicrew hub add`. It links no store, so `aicrew` can use it; `aicrewd`
  adds its client checks on top.
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
- `aimem_hubs` is optional. Without it, session entry and resume are
  refused; with it, the service redeems agents' proofs with those aimem
  hubs, each a named block:

  ```json
  "aimem_hubs": [{
    "name": "main",
    "hub_id": "HUB_ID",
    "base_url": "https://aimem.example:8443",
    "tls_trust_mode": "ca_dns",
    "tls_trust_value": "aimem.example",
    "redemption_token_file": "/etc/aicrew/aimem-redemption.token",
    "read_token_file": "/etc/aicrew/aimem-read.token",
    "team_register_token_file": "/etc/aicrew/aimem-team-register.token"
  }]
  ```

  `name` is the hub's alias in aicrew, which `aicrew team create --hub`
  takes: 1 to 32 lowercase letters, digits or `-`, unique. `hub_id` is the
  ID the hub reports (`aimem identity peer list`), unique; a proof is
  redeemed with the hub its challenge names. `base_url` is aimem's https
  origin. `tls_trust_mode` is `ca_dns` (the
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
  aimem never counts as "nothing committed": the step stays pending. One
  block at most has a `read_token_file`, and that hub's read scope settles
  and reconciles only its own tasks: a step on another hub stays pending.
- `team_register_token_file` and `team_read_token_file` are optional. They
  hold the `team.register` and `team.read` credentials aimem issued to this
  service (`aimem identity cred issue … --operation team.register`), each its
  own file, checked like the read credential. With `team.register`, aicrewd
  registers each team created on that hub under the team's name (below).
  With `team.read`, aicrewd reads the team's grants at every offer and
  claim, and refreshes every team's grants snapshot once a minute; without
  it, offers and claims of that hub's teams are refused `hub_unavailable`.
- The single `aimem` block of earlier releases, without `name` or
  `hub_id`, is still read for this release, as the hub `default`, and logs
  a warning; a configuration with both forms is refused. Move it into
  `aimem_hubs` with `aicrewd config migrate` (below): a team cannot name
  that hub (`--hub`) because it has no hub ID.
- Keep the TLS key readable only by the service's account.

### Moving the aimem block: `aicrewd config migrate`

```sh
aicrewd config migrate -config aicrewd.json [-name NAME] [-hub-id ID] \
  [-team-register-token-file FILE] [-team-read-token-file FILE] [-service-id ID]
```

It rewrites a configuration's single `aimem` block as one `aimem_hubs`
entry, in the block's place:
- the entry's `name` is `-name`, or `default`, the name the block is read
  under today;
- the block's five fields move as they are: `base_url`, `tls_trust_mode`,
  `tls_trust_value`, `redemption_token_file` and `read_token_file`;
- `-hub-id` and the two team credential files are added when given, the
  files as absolute paths;
- `-service-id` replaces `service_id`.

Every other field keeps its value and its place. The file is replaced
atomically, with its mode (and, on Unix, its owner and group). The previous
file is kept beside it as `aicrewd.json.<UTC time>.bak`. A symbolic link is
followed: the file it names is migrated, and the link stays.

The input must be a configuration aicrewd accepts. The result is checked,
with its size, the same way before anything is written. A refusal writes
nothing. An `aimem_hubs` member beside the block, even an empty one, is
refused, and so is a name given twice. Names are matched regardless of
case, as aicrewd reads them. Before writing, migrate also checks that
aicrewd would read the result as the original with the block moved.

A file already in the `aimem_hubs` form, or with no hub, is left as it is:
a second run changes nothing. Flags are refused on such a file.

Each run names the fields still to supply: a hub's `hub_id`, and its
`team_register_token_file` and `team_read_token_file`. `aicrew hub add`
(below) sets all three from the directory `aimem identity peer provision`
wrote.

It also reminds the operator that `service_id` must be the peer ID the hub
lists for this service, which migrate cannot check.

Exit status:
- `0`: the file is complete, and aicrewd can be restarted with it;
- `3`: a hub still has no `hub_id`. aicrewd refuses the file until it is
  set, so do not restart it yet;
- `1`: refused, and nothing was written;
- `2`: a usage error.

### Binding a hub: `aicrew hub add`

```sh
aicrew hub add NAME --config aicrewd.json --base-url URL \
  --tls-trust-mode ca_dns|spki_sha256 --tls-trust-value VALUE --cred-dir DIR
```

It runs on the machine that runs aicrewd, and edits `aicrewd.json`
directly; it does not call aicrewd.

`DIR` is the directory `aimem identity peer provision` wrote on the hub's
side, then carried to this machine. Its files have fixed names:
- `aimem-hub-id`: the hub's ID, one UUID in lowercase on one line. A
  missing, empty, multi-line or other file is refused by name.
- `aimem-redeem.token`, `aimem-read.token`, `aimem-team-register.token`
  and `aimem-team-read.token`: the four peer credentials. Each must be
  readable by its owner only and hold one peer credential alone on one
  line. Two files holding the same credential are refused. A credential is
  never printed.

Before writing anything, it checks the hub live. It reads this service's
teams with the team.read credential, under the given trust binding and the
file's `service_id`. A refusal is reported with the hub's code and what to
do about it, for example `peer_forbidden` (check `service_id` against
`aimem identity peer list`) or `hub_unavailable` (check the URL, the trust
binding and the network).

Then it writes the `aimem_hubs` entry for `NAME`:
- every path is absolute;
- an entry of that name is replaced in place, and any other name is
  appended;
- only one hub may serve the reservation read scope. When another hub
  already has a `read_token_file`, the entry gets none, and the command
  says so.

The file is checked, written and kept as `aicrewd config migrate` does: a
read-back check, an atomic replace with the file's mode, and the previous
file kept as `aicrewd.json.<UTC time>.bak`. A legacy `aimem` block is
refused; run `aicrewd config migrate` first. Restart aicrewd to apply the
change. If another hub still lacks its `hub_id`, the command names that
hub and exits 3, as migrate does: do not restart aicrewd yet.

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

`aicrewd` serves the operator's administration on its HTTPS listener
(`listen_addr`), beside the agents' routes, so the
service keeps running while the operator manages teams, invitations and
hub credentials. The routes, under `/v1/admin/`, and their JSON are defined
in `internal/opapi`:

| Route | Operation |
| --- | --- |
| `GET /v1/admin/hub-credentials?hub=HUB` | list credentials (metadata) |
| `POST /v1/admin/hub-credentials` | issue: `{"hub_id", "operations"}`; the answer carries the bearer, once |
| `POST /v1/admin/hub-credentials/rotate` | `{"hub_id", "operations"}`: needs exactly one active credential for the hub (otherwise `409 rotate_needs_one_active`) and issues a second, answered once with `replaces` naming the first, which stays active until it is revoked |
| `POST /v1/admin/hub-credentials/revoke` | revoke: `{"id"}` |
| `GET /v1/admin/teams` | list teams |
| `POST /v1/admin/teams` | create: `{"name", "hub"}`; registers the team on its hub |
| `GET /v1/admin/team?id=TEAM` | show a team and its members |
| `POST /v1/admin/team/rename` | `{"team_id", "expected_revision", "name"}` |
| `POST /v1/admin/team/register` | `{"id", "hub"}`: register the team on its hub again; `hub` names the hub of a team that names none |
| `POST /v1/admin/team/grants` | `{"id"}`: read the team's grants from its hub now and record them; answers the team and this service's `service_id`, or `hub_unavailable` with the hub's code |
| `GET /v1/admin/invitations?team=TEAM` | list invitations (metadata, with each team's ID and name) |
| `POST /v1/admin/invitations` | issue: `{"purpose", "team_id", "role", "hub_id", "label", "agent_id", "expected_user_id", "ttl"}`; the answer carries the code, once |
| `POST /v1/admin/invitations/revoke` | revoke: `{"id"}` |

The credential routes are also served under their names before 0.3.0,
`/v1/admin/introspection-credentials` (and `/rotate`, `/revoke`), for one
release; they are removed in 0.4.0.

**The operator credential.**
- **Required on every route.** Every route requires
  `Authorization: Bearer <operator token>`. A missing or wrong bearer, a
  member's session token and aimem's introspection bearer are all refused
  with `401 unauthorized`, and the operator routes never consult the member
  session API.
- **Creating it.** `aicrew operator-token new --output PATH` writes a new
  token (`aop_` and 64 lowercase hex) to a new owner-only file, or with
  `--output -` to standard output for a pipe ("Secrets the client writes",
  below). It opens no store and calls no service, and the token never
  reaches a terminal. Name that file as
  `operator_token_file`, and keep the operator's own copy owner-only.
- **Rotation.** The service reads the file on every operator call, so
  replacing it rotates the credential without a restart: write a new token
  with `aicrew operator-token new --output NEW` beside the old file, then
  rename `NEW` over `operator_token_file`, and replace the operator's own
  copy (`AICREW_OPERATOR_TOKEN_FILE`) on each machine `aicrew` runs from: a
  client with the old copy is refused `401`. A file that is missing,
  readable by another account or malformed fails closed: every operator
  call answers `503 operator_unavailable` until it is fixed.
- **Failed attempts.** Failed authentications are limited to 10 a minute per
  client address. Every attempt takes one from the address's budget before
  its bearer is compared, in one step, and gives it back once it has
  authenticated, or when the service cannot read its own token file
  (`503`). Concurrent attempts therefore compare no more bearers than the
  budget holds, and an address with none left is refused with
  `429 rate_limited` and a `Retry-After` before any comparison. A token is
  held only while a call authenticates, so successful calls are refused
  only when more of them authenticate at the same moment than the address
  has budget left.

**Secrets and errors.**
- **Once-only secrets.** A credential's bearer and an invitation's code exist
  only in the answer that issued them; the store keeps digests, and a list
  never carries either.
- **No client keys.** Every write is its own command: the service takes no
  client idempotency key, because a replayed issue could not answer its
  secret again.
- **Error codes.** Every refusal is `{"code", "message"}`; the message
  never carries a secret.
  - `400 invalid_request`: a validation failure, with the store's message.
  - `404 not_found`: no such team, invitation or credential.
  - `409 team_exists`: team names are unique.
  - `409 revision_conflict`: a stale `expected_revision`.
  - `409 credential_limit`: the hub already has two active credentials.
  - `409 rotate_needs_one_active`: rotate found other than one.
  - `409 invitation_final`: the invitation is already redeemed or revoked.
  - `500 internal_error`: no message.
- **Bodies** are `application/json` of at most 16 KiB, decoded strictly
  (unknown fields are refused). Otherwise `415` or `413`, both
  `invalid_request`.
- **The log.** Each operator action is logged as `operator` with its action
  (`team.create`, `invitation.issue`, ...), its outcome and the ID it
  touched, besides the request line; never a body, a bearer or a code.

### The console client: `aicrew`

`aicrew` is the operator's client of this API, the console alternative to a
web console; it never opens the store and links no store code. It runs from
any machine that reaches `aicrewd` over TLS, while the service keeps
running. Every administrative command takes the connection, as flags or,
once per shell, as environment variables:

| Flag | Variable | Value |
| --- | --- | --- |
| `--url` | `AICREW_URL` | `aicrewd`'s https origin |
| `--tls-trust-mode` | `AICREW_TLS_TRUST_MODE` | `ca_dns` or `spki_sha256`, as for `aicrew-agent join` |
| `--tls-trust-value` | `AICREW_TLS_TRUST_VALUE` | the host name, or `sha256-` and the pin |
| `--token-file` | `AICREW_OPERATOR_TOKEN_FILE` | the operator credential's owner-only file |

```sh
CGO_ENABLED=0 go build -o bin/aicrew ./cmd/aicrew
export AICREW_URL=https://aicrew.example:8443 AICREW_TLS_TRUST_MODE=ca_dns AICREW_TLS_TRUST_VALUE=aicrew.example
export AICREW_OPERATOR_TOKEN_FILE="$HOME/.config/aicrew/operator.token"
```

A refusal prints the service's code and message and exits 1; a usage error
exits 2. `aicrew hub add`, `aicrew operator-token new` and `aicrew version`
take no connection.

Both tools read a flag as `--name` or `-name` (Go's flag package); the
documentation writes `--`.

**Secrets the client writes.** A command that reveals a secret (a hub
credential's bearer, an invitation's code, an operator token) writes it only
where `--output` names, and never to a terminal:

- `--output PATH`: a new file, which must not exist, created readable by
  its owner only (mode 0600, or an owner-only protected DACL on Windows).
- `--output -`: standard output, for a pipe. It is refused when standard
  output is a terminal. Standard output then carries the secret alone and
  the command's metadata goes to stderr.

The output is checked before anything is issued, so a refused output issues
nothing; if the secret cannot be written, what was just issued is revoked.

**Names before 0.3.0.** These keep working for one release, each with a
one-line notice, and are removed in 0.4.0: the command
`introspection-credential` (now `hub-credential`), its API routes (above),
and the flags `-secret-file`, `-code-file` and `-file` (now `--output`).

## Hub credentials

Aimem authenticates its introspection and coordination-fact calls with a
credential that aicrew issues for one aimem hub: the hub credential. The
operator manages them with `aicrew` (the connection as above), while
`aicrewd` runs.

```sh
bin/aicrew hub-credential issue  --hub HUB --output hub.secret
bin/aicrew hub-credential list
bin/aicrew hub-credential rotate --hub HUB --output hub-2.secret
bin/aicrew hub-credential issue  --hub HUB --output intro-only.secret --operations introspection
bin/aicrew hub-credential revoke --id ID
```

- The bearer is written only where `--output` names ("Secrets the client
  writes", above). The command prints the credential's metadata, never the
  bearer. If the bearer cannot be written, the credential just issued is
  revoked.
- Hand the file to aimem's operator through a private channel; aimem reads
  it from `AIMEM_INTROSPECTION_TOKEN_FILE`. Delete aicrew's copy afterwards.
- A hub has at most two active credentials. To rotate: `rotate` issues the
  second, aimem moves to it, then `revoke` the first.
- `--operations` names what a new credential permits: `introspection`,
  `coordination`, or both, which is the default. `list` shows each
  credential's operations.
- A credential issued before coordination facts existed permits
  introspection only. To enable coordination for that hub, `rotate` it:
  the new credential permits both, aimem moves to it, then `revoke` the old
  one.

## Reconciliation

With a hub's `read_token_file` configured, aicrewd also runs its
reconciliation loop: every 15 s it settles the steps members left pending
and closes as recovered the attempts whose reservation aimem closed outside
aicrew, reading at most 30 times a minute (`docs/CREW-CONTRACT.md`,
"Reconciliation by aicrewd"). It logs each recovered closure. Without the
read credential the loop does not run.

## Teams

A team has a name and a hub. Its projects are the grants its hub's
operator gives the team's profile there (`aimem identity team grant
--team-name crew --project PROJECT`): aicrew keeps no project list of its
own. The operator manages teams with `aicrew`, while `aicrewd` runs. Each
command prints the team as JSON; `list` adds each team's member count and
`show` its current members.

```sh
bin/aicrew team setup    crew --hub main --project PROJECT [--project PROJECT ...]
bin/aicrew team create   --name crew --hub main
bin/aicrew team register --team-name crew [--hub main]
bin/aicrew team list
bin/aicrew team show     --team-name crew
bin/aicrew team rename   --team-name crew --expect-revision N --name crew-2
```

- `setup` puts a team on its hub with its projects, in one command, and can
  be run again:
  - it creates the team on `--hub` when no team has the name, or registers
    an existing one there unless it is already registered. A team on
    another hub is refused, since a team keeps its hub;
  - a refused registration is reported with the hub's code and what to do,
    for example `peer_forbidden` (check `service_id` in `aicrewd.json`
    against `aimem identity peer list`) or `team_name_taken`. It exits 1;
  - it then reads the team's grants from the hub live
    (`POST /v1/admin/team/grants`, not the minute's snapshot). If the hub no
    longer knows a team aicrewd holds as registered, it registers the team
    again and reads once more. A disabled profile grants nothing, so it is
    reported as the problem, with exit 1 and no grant command;
  - each `--project` the hub grants is listed, with a note when the hub
    binds it no repository or process. For each one it does not grant yet,
    it prints the exact command the hub's admin runs,
    `aimem identity team grant --peer SERVICE_ID --team-name TEAM --project PROJECT`,
    and exits 3;
  - run again once the grants exist, it registers nothing again, reports
    every project granted and exits 0.
- Wherever a command takes `--team`, the team's ID, it takes `--team-name`
  instead: one or the other, never both. A name that no team has fails
  before anything changes. Team names are unique.

- `create` and `rename` refuse a name another team already has
  (`team_exists`).
- `--hub` names the team's hub by its alias in `aimem_hubs`; a team keeps
  its hub. When that hub's block has `team_register_token_file`, `create`
  registers the team there under its name (aimem's team.register), and
  `rename` registers the new name. The outcome is the team's
  `registration`: `registered`, or the hub's refusal (`team_name_taken`:
  another team of this service holds the name on the hub; `profile_disabled`)
  or `hub_unavailable`, each with what to do in `detail`, also printed on
  stderr. The team exists whatever the hub answers. `register` tries again
  and exits 1 unless the team is registered.
- `show` and `list` print the team's `grants` as aicrewd last read them
  from the hub (`team.read`): each granted project with its repository and
  process pin, the `grants_state` the hub answered (`enabled`, `disabled`,
  `not_registered`) and `grants_read_at`. An offer or a claim never uses
  this copy: each reads the hub live, and is refused `project_not_granted`
  when the hub does not grant the task's project, or `hub_unavailable` when
  the hub does not answer.
- `register --hub ALIAS` names the hub of a team created before teams named
  one, then registers it; a team keeps its hub.
- `rename` applies only to the revision `show` or `list` printed, and
  refuses a team that changed since (`revision_conflict`).
- `team projects` and `--project` of earlier releases are removed; the
  service refuses a request that still carries `projects`.
- Members join through invitations (below); `aicrew team` does not change
  membership.

## Invitations

An invitation lets one agent join a team, or link or rebind an agent record
(`docs/ONBOARDING-CONTRACT.md`). The operator manages them with `aicrew`,
while `aicrewd` runs.

```sh
bin/aicrew invitation issue  --team-name crew --role worker --hub HUB_ID --label builder --expect-user AIMEM_USER_ID --output invite.code
bin/aicrew invitation issue  --team TEAM --role worker --hub HUB_ID --purpose link --agent AGENT --output invite.code
bin/aicrew invitation issue  --team TEAM --role worker --hub HUB_ID --purpose rebind --agent AGENT --expect-user AIMEM_USER_ID --output invite.code
bin/aicrew invitation list   [--team TEAM | --team-name NAME]
bin/aicrew invitation revoke --id INVITATION
```

- The code is generated by `aicrewd`, answered once and never kept: the
  store holds only its digest.
  `issue` writes it only where `--output` names ("Secrets the client
  writes", above), never to a terminal, and refuses without `--output`
  before issuing anything. If it cannot be written, the invitation just
  issued is revoked. The invitation's metadata names its team by ID and
  name.
- The code is never an argument. Give it privately to the person running
  the agent, who enters it at the client's hidden prompt.
- `--purpose` is `join` (the default; names the new agent's `--label`),
  `link` or `rebind` (each names the `--agent`).
  - `--expect-user` pins the aimem user the proof must name. It is required
    for `rebind`. For `join` and `link` it is optional, and `issue` warns
    without it: anyone holding the code and an aimem credential for the hub
    could redeem it.
- `--expires` sets the lifetime: 24 hours by default, at most 72.
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
bin/aicrew-agent run --client claude --home ~/aicrew/agents/builder
bin/aicrew-agent session start  --home ~/aicrew/agents/builder
bin/aicrew-agent session status --home ~/aicrew/agents/builder
bin/aicrew-agent session leave  --home ~/aicrew/agents/builder
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

- `run --client claude|opencode` does what `session start` does, then
  starts the client (from `PATH`, or `client_command` in the `aicrew`
  section) as its child in the agent home, with `AIMEM_TEAM_SESSION` set in
  that child's environment only; arguments after `--` go to the client.
- **The first turn.** For Claude Code, `run` passes a first instruction as
  the client's initial prompt, ahead of the arguments after `--`. The
  member's first turn then needs no typed message: it reads
  `docs/START.md` and `docs/ROLES.md`, runs `aicrew-agent inbox`, says in
  one short message what it will do, and acts within its role.
  - The instruction names only those files and `aicrew-agent inbox`: no
    secret, handle or ID.
  - After that turn, the Stop hook ("Waking a member") wakes it on its
    inbox.
  - `--no-start` turns the instruction off. Use it for debugging, when you
    pass the client your own prompt after `--` (for example `-- -p "..."`),
    or when `client_command` is not Claude Code itself (the real-aimem
    harness runs `/bin/sleep` as a stand-in client).
  - OpenCode starts without one: its first argument is a project directory.
- When the client exits, it leaves and closes aimem's binding, exiting with
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
- `session status` shows the recorded session, the member's role, aimem's
  binding and the team's projects as the launcher last read them
  (`state/team.json`, WORKSPACE.md "Initial guidance"). It shows no secret
  and does not call `aicrewd`. Inside a client that `run` started, `--home`
  defaults to the launcher's home.
- `session leave` proves afresh and resumes the recorded session, which
  fences any client still holding it, then leaves. It never enters the
  team: if the recorded session has already ended, it only closes aimem's
  binding of it and clears the record.
- Inside a client that `run` started (both `AIMEM_TEAM_SESSION` and
  `AICREW_AGENT_HOME` are set), `session start`, `session leave` and `run`
  refuse with exit 2 before reading the home, and name `aicrew-agent inbox`
  as the next action. The launcher holds that session, and a new proof would
  resume it under a new generation and fence the launcher's token, cutting
  the client off from its inbox and steps. `session status`, `inbox`, `step`
  and `check` work there. To restart the session, exit the client and `run`
  again from a terminal.
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
bin/aicrew-agent join --label builder --url https://aicrew.example:8443 \
  --tls-trust-mode ca_dns --tls-trust-value aicrew.example --aimem-hub main --client claude
bin/aicrew-agent join --home ~/aicrew/agents/builder    # rerun: refresh and check
```

- **The first `join` provisions the home's aimem installation**
  (`docs/WORKSPACE.md`, "The member's aimem installation"):

  ```sh
  bin/aicrew-agent join --label builder --url https://aicrew.example:8443 \
    --tls-trust-mode ca_dns --tls-trust-value aicrew.example --aimem-hub main --client claude \
    --aimem-url https://aimem.example --aimem-token-file member.token [--aimem-ca-file hub-ca.pem]
  ```

  - **The token.** `--aimem-token-file` names an owner-only file holding the
    member's user-scoped token (`aimem_user_...`), or `-` to type it at a
    hidden prompt. A token given as the flag's value is refused: a token is
    never an argument.
  - **The CA.** `--aimem-ca-file`, for a hub on a private CA, is copied to the
    home's `creds/aimem.<hub>.ca.pem` (owner-only), so nothing in the
    installation points outside the home.
  - **What join runs.** It runs `aimem hub add` and `aimem hub task-token`
    against the home's installation, with the token on aimem's standard input
    only, then checks the credential as below.
  - **Reruns.** A home whose credential is already active is not provisioned
    again. A failed step stops the run with aimem's message; rerunning with
    the same flags completes it.
  - **On a linked home** the flags are refused.

  The manual equivalent, for an operator who provisions a home before `join`,
  runs the same two commands with the home's two variables set for them
  only:

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
- `--home` defaults to `~/aicrew/agents/<label>` (`%USERPROFILE%\aicrew\agents\<label>`
  on Windows). `--aimem-hub` is aimem's name for the hub whose identity the
  invitation names; `--aimem-command` overrides the `aimem` executable.
  `--client claude|opencode` (or both, comma-separated) names the clients
  the home is for: required on the first run, recorded in `agent.json`
  after. `--json` prints the report as JSON.
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
- **The member's own forge credentials** (`docs/proposals/PILOT-1-FOLLOWUPS.md`,
  3.1 to 3.4): each `--cred HOST=PATH` gives the member's token for one forge
  host, from an owner-only file, or with `--cred HOST=-` from standard input
  (a hidden prompt on a terminal; at most one `-`). It is taken on the first
  join and on a linked home's rerun:

  ```sh
  bin/aicrew-agent join --home ~/aicrew/agents/builder --cred github.com=github.token
  ```

  - **Verified first.** `join` asks the forge who the token authenticates as
    (a read-only call) before writing anything. GitHub, Gitea and GitLab are
    known: github.com is GitHub and gitlab.com is GitLab; another host is
    told apart by public endpoints that need no token (Gitea's
    `/api/v1/version`, then GitHub Enterprise's `/api/v3/meta`), otherwise
    GitLab. A token is never sent to another host, and a redirect to one is
    refused.
  - **Where it goes.** The token is written owner-only to
    `creds/<service>.<account>.<purpose>`, the WORKSPACE credential
    reference: the host with `.` and `:` written `-`
    (`gitea.example.org:3000` is `gitea-example-org-3000`), the account the
    forge reported, encoded the same way, and the purpose `repo-write`. Two
    hosts are two files, and two hosts whose encoded names collide are
    refused by name.
  - **What `agent.json` records.** Its `forge` section names, per host, the
    dialect, account, purpose, file and the commit name and address the
    forge reports. It never holds a token; the file is found through this
    record, never by parsing its name.
  - **What the report says.** Each host is `provisioned`, `unchanged`,
    `unreachable` (not written; rerun when the host answers) or `refused`
    (the forge rejected the token, or the name collides; not written). The
    other hosts proceed either way. A rotated token rewrites the same file; a
    token for another account moves to a new file and removes the old one.
  - A `--cred` value that names no file is refused without being quoted: it
    takes a file or `-`, never a token.
- It writes no other secret of its own. `creds/` is created owner-only and
  holds the forge credentials and the hub's CA copy when `--aimem-ca-file`
  is given. `aimem/` is created owner-only, or restricted if the operator's
  provisioning created it; the individual aimem credential stays there, in
  aimem's own storage.
- It writes the managed `.claude/settings.json` (`env`: the home's
  `AIMEM_STATE_DIR` and `AIMEM_SOCKET`, absolute), under the same digest
  rule as the guidance files.

### Checking dependencies and clients: `aicrew-agent check`

```sh
bin/aicrew-agent check --home ~/aicrew/agents/builder [--client claude|opencode] [--json]
bin/aicrew-agent version [--json]
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
- **Forge credentials.** Every credential `agent.json` records is checked
  with the forge's read-only "who am I" and listed as a table: host,
  account, purpose and state, which is `verified`, `missing` (the file is
  gone or unreadable), `refused` (revoked, expired, or now another account)
  or `unreachable`. A credential that is not verified is a notice with the
  `join --cred` command that fixes it, never a blocker: it narrows the work
  the member can take on that host, while the home stays usable. Only the
  aimem credential blocks.
- **The team's projects (capabilities).** When the home's launcher runs,
  the check asks it to verify the team's requirements and report them to
  aicrewd: only the launcher holds the session. For each project the team's
  hub grants, the launcher reads the repository's permissions on its forge
  with the home's credential for that host (push for `write`), and the
  check prints a table: project, repository, host, account, required access
  and state.
  - The state is `verified`, `insufficient` (less access than required),
    `missing` (no credential for the host), `refused` (the forge rejected
    the token or shows no such repository) or `unreachable`.
  - The verified repositories, with the access the forge reports, go to
    aicrewd as the member's capabilities.
  - The launcher also reports them when its session starts, so the
    capabilities of a `join` arrive with the session that follows it.
  - A requirement that is not verified is a notice, never a blocker: aicrewd
    refuses only the offers that need it (`capability_missing`).
  - Without a running launcher, the check says so and prints no project
    rows.

`version` reports the build: a release build stamps
`github.com/BlackVS/aicrew/internal/version.Override` with `-ldflags -X`;
a source build reports `dev` and its commit.

### The member's commands

`join` writes the team's recurring instructions into the home as Claude
Code commands, so a member needs no typed prompt. They live in
`.claude/commands/crew-*.md` and are managed files, under the same rerun
rule as the guidance.

| Command | Role | What it does |
| --- | --- | --- |
| `/crew-start` | every member | reads START.md, ROLES.md, `session status` and the inbox, then acts within the role |
| `/crew-inbox` | every member | reads the inbox, acts on it, acknowledges what was handled |
| `/crew-triage <task>` | coordinator | moves a task between BACKLOG and READY with aimem's `triage_task` |
| `/crew-offer <task> <worker>` | coordinator | checks READY, dependencies and the pin, then offers |
| `/crew-review <attempt>` | coordinator | reviews a submission against the frozen scope at level high, then the review step; stops before the human merge |
| `/crew-accept <attempt>` | worker | checks the offer's pin and digest, accepts, clones |
| `/crew-submit <attempt> <url>` | worker | submits the result |
| `/crew-claim <task>` | independent | claims a READY task |
| `/crew-handoff` | every member | writes `docs/HANDOFF.md` |

- **Every role's commands are in every home**, as every role's section is
  in ROLES.md: a member's role can change, and a rerun does not ask
  aicrewd. A role's command checks `session status` first and stops in
  another role.
- **One source.** The steps a command teaches are rendered from the same
  data as ROLES.md, and a test keeps them equal.
- **Collisions.** Every name has the `crew-` prefix. `join` writes no
  command that would shadow a Claude Code built-in, or a command or skill
  of the same name of the member's own (`commands/<name>.md` or
  `skills/<name>/` under `CLAUDE_CONFIG_DIR`, else `~/.claude`). It reports
  the collision as `collision` in its plan, with an instruction.
- **The first turn.** `run` starts Claude Code with `/crew-start` when the
  home holds it, and with the instruction's text in a home joined before
  the commands existed.
- **The safety rule** closes every command and is one of ROLES.md's member
  rules, from one constant. It forbids:
  - reading `creds/`;
  - putting a credential into a command, URL, environment variable or git
    configuration;
  - changing git's global or system configuration;
  - weakening TLS verification.

  A refused or failed access, clone or fetch is reported, never worked
  around. Smoke runs with broad tool access showed a member doing both
  things the rule forbids, until the rule named them.

### Waking a member: the Stop hook

A Claude Code member acts only when it has a turn. The home's managed
`.claude/settings.json`, which `join` writes, installs a Stop hook. When the
member's turn ends, the hook waits on its inbox, so a message that arrives
during the wait gives the member its next turn (task `01a0d6d7-1aed`;
`docs/CLIENT-WAKE-PROBE.md` measures the mechanism). This is not unattended
operation: see "The idle gap" below, and the pilot criterion of two members
handing off with no human message is still open.



```json
"hooks": {"Stop": [{"hooks": [{"type": "command",
  "command": "\"/path/to/aicrew-agent\" wait-inbox --home \"/path/to/home\"", "timeout": 660}]}]}
```

- **When a turn ends**, Claude Code runs `aicrew-agent wait-inbox`, which
  asks the home's launcher to wait on the member's inbox. The launcher polls
  `GET /v1/crew/inbox/pending` every 3 s as its session; that route records
  no delivery.
- **A message waiting, or arriving during the wait,** blocks the stop. The
  member gets a turn naming how many messages arrived, their kinds and ids,
  and the next action: read them with `aicrew-agent inbox`, acknowledge,
  and act within its role. The hint carries no message text. The inbox read
  stays the only delivery, so a lost or repeated hint costs one extra read.
- **Keep-alive.** When a wait ends with nothing pending, the hook blocks
  once more with a turn that asks only for the word "waiting", so the next
  turn end waits again. That costs one small model call per wait (10 minutes
  by default). After `idle_hours` without a message in the same client
  session, the hook lets the session stop, and the member is idle until
  something else starts a turn. A new client session starts a new idle
  period.
- **Never a wedge.** With no launcher (a client started outside
  `aicrew-agent run`), a closed session, or anything else failing, the hook
  prints nothing and exits 0, and Claude Code lets the session stop. The
  hook's timeout is a minute past the wait, so the hook always ends first.
- **Settings**, in `agent.json`'s top-level `wake` object:
  - `wait_seconds` (default 600, at most 1800);
  - `keep_alive` (default `true`);
  - `idle_hours` (default 8).

  A rerun of `join` rewrites the hook's timeout from them. `check` gives a
  notice when the settings do not hold the hook as `join` writes it.
- **The idle gap.** Once the hook has let the session stop, it cannot wake
  it. Waking an idle session needs a channel or the launcher owning the
  terminal; both are later increments.

### Driving steps: `aicrew-agent step`

The launcher drives every attempt step for its client, which never holds the
session token or a coordination proof. The client (the model, or a script in
its conversation) asks the launcher for one step at a time:

```sh
aicrew-agent step claim   --body - < claim.json
aicrew-agent step work    --attempt A1 --task T1 --body '{"intent":"submit","detail":"https://forge.example/pr/12"}'
aicrew-agent step release --attempt A1 --task T1 --body '{"target":"BLOCKED","blocker":"waiting on design"}'
aicrew-agent step confirm-stop --attempt A1
aicrew-agent step pending
aicrew-agent step recover
aicrew-agent step offer --repository https://github.com/team/app --body - < offer.json
```

`--repository CLONE_URL`, on `offer` and `claim` only, fills the body's
`repository` object: the `url` (when the body names none), the forge's
`kind` (from the home's credential for that host, when the body names
none), the `default_branch`, and that branch's head as the `base_commit`,
read through the forge with the home's own credential for that host
(`join --cred`). It prints what it resolved on stderr. What the body names
is kept: a given `base_commit` is never replaced, and the `branch` and the
`access` are always the body's. aicrewd compares `kind`, `url` and
`access` with the repository the hub binds to the project
(`repository_mismatch`). A host the home holds no credential for is refused
with the `join --cred` command that fixes it.

- **Operations.** The reservation steps `offer`, `claim`, `accept`,
  `withdraw`, `work`, `release` and `finalize` take the begin route's body
  (`docs/CREW-CONTRACT.md`, "Attempt steps"). `offer` and `claim` create
  their attempt and name the task in the body. The others need `--attempt`
  and the aimem task's `--task`. The local steps `decline`, `review`, `stop`,
  `confirm-stop` and `confirm-delivery` need only `--attempt` and their body.
  `pending` lists the recorded steps, and `recover` finishes them.
- **Finding the launcher.** `--home` defaults to `AICREW_AGENT_HOME`, which
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

### Cloning with the member's credential: `aicrew-agent clone`

```sh
aicrew-agent clone --repository https://github.com/team/app --attempt A1   --base 0123456789abcdef0123456789abcdef01234567 --branch attempt/A1
```

It works with the member's own credential for the repository's host, which
`join --cred` provisioned (`docs/proposals/PILOT-1-FOLLOWUPS.md`, 3.4):

- **The clone** is `repos/<service>/<owner>/<name>` (the host encoded as in
  the credential reference), made once and reused by later attempts. A
  directory there that clones another repository is refused.
- **The credential helper.** The clone's own git configuration resets any
  inherited credential helper and names `aicrew-agent git-credential --home
  <home>`, which git runs when it needs a credential for the host. The
  helper answers on git's pipe only, with the account and the token read
  from `creds/` through `agent.json`; on a terminal it refuses. The token
  never appears in a URL, a git configuration, a process argument or the
  output.
- **The commit identity** of the clone is the member's account on that host,
  with the address the forge reported: commits and pushes from the attempt's
  worktree are the member's.
- **The worktree** is `worktrees/<attempt>`, a new branch at the base commit,
  after a fetch. An existing worktree for the attempt, a base the repository
  lacks, or a branch git refuses stops the command.
- Only https clone URLs are taken: the member's credential is an https
  token. Until the attempt carries its repository (354c-1b), the repository,
  base and branch are given as flags.

### An offer's instruction digest: `aicrew-agent digest`

```sh
aicrew-agent digest --repository https://github.com/team/process --commit 0123456789abcdef0123456789abcdef01234567 --manifest processes/delivery.yaml
```

It prints the instruction digest of a process pin: `sha256:` and the
lowercase hex SHA-256 of the manifest's exact bytes at the pinned commit.
A worker computes it to verify an offer before accepting, and a coordinator
to make one.

- **The clone** is the process repository's own clone under `repos/`, made
  and reused as `aicrew-agent clone` makes one, with no checkout. The
  command fetches it, and fetches the pinned commit by name when no branch
  holds it.
- **The credential helper** is the same per-clone helper. It answers with
  the home's credential for the host when `join --cred` provisioned one,
  and with nothing otherwise, so a public process repository needs no
  credential. The worker never handles a token to verify an offer, and the
  token never appears in a URL, a configuration, an argument or the output.
- **Refusals.** An ssh pin (the home's credential is an https token), a
  commit that is not a full commit ID, a manifest path that is not a clean
  relative path, and a directory with no `agent.json` are refused before
  anything is fetched. A refused fetch, a commit the repository lacks and a
  manifest it lacks stop the command with that reason and no digest. The
  worker then declines the offer.
- `--json` adds the clone, the commit and the manifest.

### Reading the inbox: `aicrew-agent inbox`

Offers, acceptances, submissions, stops and other lifecycle messages reach
a member's team inbox, each naming its attempt; a worker accepts an offer by
the attempt ID its inbox shows, and reads the offer's base commit, branch,
process pin, instruction digest and expiry from the offer's message. The
agent home's managed `docs/ROLES.md` teaches each role these steps. The client reads it through the launcher,
which holds the session, like a step:

```sh
aicrew-agent inbox                 # the oldest unacknowledged messages, 20 at most
aicrew-agent inbox --limit 50 --json
aicrew-agent inbox --ack ID,ID      # acknowledge what was handled
```

- A message stays in every later read until it is acknowledged, so a
  crash between reading and acting loses nothing; the next page comes once
  the current one is acknowledged.
- A page holds at most `--limit` messages and at most 128 KiB of JSON, and
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
`aicrew-agent version` (`--json` for the build as JSON).

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
