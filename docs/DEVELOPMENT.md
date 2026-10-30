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
- `go.starlark.net v0.0.0-20261005163335-bcb1a1a55bf9` (commit `bcb1a1a`
  of github.com/google/starlark-go, published 2026-10-05; BSD-3-Clause, the
  Bazel Authors), the workflow scripts' interpreter ("Workflow scripts",
  below). The module publishes no tagged releases, so it is pinned by its
  pseudo-version: the newest commit that was seven days old when it was
  added. Only its interpreter packages are compiled in (`starlark`,
  `starlarkstruct`, `syntax`, `resolve`); its own module graph adds the
  `go-cmp` and `protobuf` checksums to `go.sum`, which no aicrew binary
  builds.

## Checks

Run these before every push; CI runs the same:

```sh
bash scripts/check-repo.sh     # repository hygiene
sh scripts/check-doc-examples.sh   # no stale names in the docs' examples
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
`go-test (windows-latest)`, `go-test (macos-latest)`, `go-race (agent)`,
`go-race (server)`, `go-race (store)`, `go-build`, and `release-build` on
each of the three platforms. A pull request shows 13 checks, these twelve
and the repository's secret scan; a commit on main shows the twelve. A
head is green when all of them pass, and the three `go-race` jobs are
required beside the three `go-test` jobs.

What runs where:
- **On main** (every push, and nightly at 03:41 UTC): every job runs in
  full.
- **On a pull request:** `repo-checks`, `go-lint`, `go-build`, every
  `release-build`, and `go-test` on Linux and macOS run in full.
  - A `go-race` job runs its package's tests only when the change touches
    that package or a package of this module its tests import.
  - `go-test (windows-latest)` runs only when the change touches
    `internal/agent`, `internal/service`, `scripts`, an installer
    (`boot.sh`, `boot.ps1`, `install-aicrewd.sh`) or a `_windows.go` file.
  - A change to `go.mod`, `go.sum`, `ci.yml` or the gate script runs both.
  - A gated job that has nothing to test still reports its check, as
    passed, with the test steps skipped.

`scripts/ci-gate.sh` makes that decision and prints the file that made it
in the job's log. A pull request's checkout is its merge commit, so
`HEAD^1..HEAD` is the change it makes.

The `go-race` jobs run the agent, server and store packages under the
race detector, on Linux, one package per job in parallel, so that the
`go-test` results do not wait for them and the race pass takes as long as
its slowest package. Locally, all three at once:

```sh
CGO_ENABLED=1 go test -race -count=1 ./internal/agent/ ./internal/server/ ./internal/store/
```

It needs cgo (a C compiler). On a Windows host without one, run it in WSL.

**A race binary sleeps for a second before it exits.** That is the race
detector's `atexit_sleep_ms`, 1000 by default. The agent tests re-run
their own binary as the fake `aimem`, `claude` and `opencode` and as
probes, several hundred processes in all, so the agent package's
`TestMain` sets `GORACE=atexit_sleep_ms=0` for the processes it starts.
The test process keeps the default. A `GORACE` that already sets the
option is kept. The agent package's race run measured:
- before: 864 s in CI and 786 s in WSL;
- after: 152 s in WSL.

The end-to-end runs against a real aimem are not part of these checks:
`scripts/e2e-real-aimem.sh` (`docs/E2E-REAL-AIMEM.md`). The
`e2e-real-aimem` workflow runs them nightly on main, by hand on any
branch, and on a pull request labelled `e2e-real-aimem`.

## Examples in the documentation

A reader copies an example as printed, so every name in it must exist in
the current code. `TestDocExamples` (`docs/examples_test.go`) reads the
code spans of the operational documents, the fenced blocks and the inline
code, and checks four kinds of name:

- **Commands.** In an `aicrew`, `aicrew-agent` or `aicrewd` invocation, a
  first word must be one the binary's dispatch compares its first argument
  with (`run` in `cmd/aicrew`; `main` and `run` in `cmd/aicrew-agent`;
  `configCommand` in `cmd/aicrewd`). A following word that looks like a
  subcommand must appear as a word in a string literal of that binary's
  `cmd/` source (usage text, a case label, a compared value) or, for
  `aicrew-agent step`, of `internal/agent/stepserver.go`.
- **Flags.** A `--flag` or `-f` in an invocation must be registered on a
  FlagSet in that binary's `cmd/` source or appear as a flag in its usage
  text. A fenced line that ends in `\` (sh) or a backtick (PowerShell)
  continues the invocation on the next line, and a failure names the
  physical line. An inline code span that wraps onto the next line of its
  paragraph is read whole, the same way.
- **Routes.** A `/v1/crew/...` or `/v1/admin/...` path must match a route
  of `internal/server/routes.txt`, which `TestRouteTableFile` pins to the
  routes the service registers (`AICREW_WRITE_ROUTES=1 go test -run
  '^TestRouteTableFile$' ./internal/server/` rewrites it). A `{param}` or
  `<placeholder>` stands for a segment, and a mention such as
  `/v1/crew/attempts/{id}/` passes when it is the prefix of a real route.
  Other `/v1/` paths are aimem's and are not checked here.
- **Automation builtins.** Inline code that starts with a call, such as
  `update_base()`, must name a builtin of the workflow scripts' host API
  (`automation.Builtins()`), of Starlark's universe, or `main`.

A failure prints file:line, the name and the nearest current name.

**What it reads.** `README.md`, every Markdown file under `docs/`, and the
Markdown files the bootstrap writes into an agent home
(`agent.HomeDocs()`: `AGENTS.md`, `CLAUDE.md`, `docs/START.md`,
`docs/ROLES.md`, `docs/HANDOFF.md` and the crew commands), reported as
`home:<path>:<line>`.

**What it skips.**
- Designs, proposals and probe reports (`DESIGN*.md`, `docs/proposals/`,
  `*-PROPOSAL.md`, `*-PROBE.md`): they describe proposed names and the
  corrections made to them. `docs/archive/`, `docs/ROADMAP.md` and
  `CHANGELOG.md` are records of the past, and `docs/SESSION-STATE.md` is a
  local file.
- A line ending in `<!-- doc-check: skip -->`, for prose that names
  something on purpose, such as a command that does not exist.

**Its limits.** The source text is the oracle, not the dispatch: a renamed
subcommand is caught only once its old word is gone from the binary's
source, and a flag is checked against the binary's flags as a whole, not
those of its own subcommand. Sample outputs, the order of steps and the
meaning of arguments are not checked; that is a reader's pass. Every fenced
block is read as commands, whatever its language tag, and a lone backtick
in prose opens a span that runs to the next line; the skip marker answers
both.

**Enforcement.** `scripts/check-doc-examples.sh` runs the test in CI's
`repo-checks` job; `go test ./...` runs it too. aimem has the same check
for its own documents.

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
- `internal/managedfiles`: the managed-file rerun rule (docs/WORKSPACE.md,
  "Managed files and repeated setup"), shared by `aicrew-agent join` and
  `aicrew architect init`.
- `internal/architect`: the architect directory that `aicrew architect
  init` writes (below).
- `internal/installer`: no code; the tests of the installers at the
  repository's root: `install-aicrewd.sh` for the hub, `boot.sh` and
  `boot.ps1` for a member's `aicrew-agent` (below).

## Running aicrewd

`aicrewd` opens the store and serves HTTPS on one listener, over TLS it
terminates itself (TLS 1.2 or later). It has no plain-HTTP listener and does
not run behind a TLS-terminating proxy. Its routes are listed below.

```sh
CGO_ENABLED=0 go build -o bin/aicrewd ./cmd/aicrewd
bin/aicrewd -config aicrewd.json
```

On a hub host, aicrewd is installed and upgraded with the release's
one-liner instead (`install-aicrewd.sh`, below).

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
- `board_read_token_file` is optional too. It holds the `board.read`
  credential (aimem 0.10.0 and later), its own file, checked like the
  others. With it, aicrewd reads the hub's board feed and wakes the
  coordinators on board changes ("Board wake", below). Without it, aicrewd
  logs a warning at start and the hub's board changes wake no one.
- The single `aimem` block of earlier releases, without `name` or
  `hub_id`, is still read as the hub `default` and logs a warning: it is
  kept until every member runs a release past it, then removed in a later
  minor release the CHANGELOG announces. A configuration with both forms is
  refused. Move it into
  `aimem_hubs` with `aicrewd config migrate` (below): a team cannot name
  that hub (`--hub`) because it has no hub ID.
- Keep the TLS key readable only by the service's account.

### Moving the aimem block: `aicrewd config migrate`

```sh
aicrewd config migrate -config aicrewd.json [-name NAME] [-hub-id ID] \
  [-team-register-token-file FILE] [-team-read-token-file FILE] [-service-id ID] \
  [-cred-dir DIR]
```

It rewrites a configuration's single `aimem` block as one `aimem_hubs`
entry, in the block's place:
- the entry's `name` is `-name`, or `default`, the name the block is read
  under today;
- the block's five fields move as they are: `base_url`, `tls_trust_mode`,
  `tls_trust_value`, `redemption_token_file` and `read_token_file`;
- `-hub-id` and the two team credential files are added when given, the
  files as absolute paths;
- `-cred-dir DIR` takes the hub ID, the two team credential files and the
  `board.read` credential (`board_read_token_file`) from the directory
  `aimem identity peer provision` wrote, checked as `aicrew hub add` checks
  them. It replaces `-hub-id` and the two file flags, and cannot be given
  with them. The installer passes it;
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

`aicrewd config show -config PATH` reads a configuration as migrate does (a
hub may still lack its `hub_id`). It prints `store_path`, `listen_addr`,
`service_id` and `legacy_aimem_block` (`yes` or `no`), one `name=value` per
line, so that the installer parses no JSON.

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
- `aimem-redeem.token`, `aimem-read.token`, `aimem-team-register.token`,
  `aimem-team-read.token` and `aimem-board-read.token`: the five peer
  credentials. Each must be readable by its owner only and hold one peer
  credential alone on one line. Two files holding the same credential are
  refused. A credential is never printed.
- A directory provisioned before aimem 0.10.0 has no
  `aimem-board-read.token`, and is refused with what to do: run
  `aimem identity peer provision` again with the same arguments. That
  rerun issues only the missing `board.read` credential and keeps every
  other file. Then run `aicrew hub add` again with the directory: it
  updates the entry in place, and aicrewd reads the board after a restart.

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

### Installing and upgrading on a hub host: `install-aicrewd.sh`

On a Debian or Ubuntu host, usually the aimem hub's own, aicrewd is
installed and upgraded with the release's one-liner, run as root:

```sh
curl -fsSL https://raw.githubusercontent.com/BlackVS/aicrew/v0.9.0/install-aicrewd.sh | bash
```

It replaces the manual steps: downloading, checking the sums, copying the
binaries, editing `aicrewd.json` and restarting. The script installs the
release it pins (`RELEASE=`), which is the release of the tag it is fetched
from. It downloads `aicrewd` and `aicrew` for the host's architecture and
refuses either one unless its SHA-256 is listed in the release's
`SHA256SUMS`.

Everything goes under the service user (`AICREW_HUB_USER`, default
`sessiond`, the aimem hub's user), with systemd linger:
- `~/aicrew/bin`: `aicrewd` and `aicrew`;
- `~/aicrew/etc`: `aicrewd.json` and `operator.token`;
- `~/aicrew/lib`: the store, `aicrew.db`;
- `~/.config/systemd/user/aicrewd.service`: written only when there is no
  unit yet.

**A fresh install** needs a TLS certificate, given by either:
- `AICREW_TLS_CERT` and `AICREW_TLS_KEY`: the absolute paths of existing
  files the service user can read;
- `AICREW_DOMAIN`: a host name, for which the script generates a
  self-signed certificate into `~/aicrew/etc/tls`, as aimem's installer
  does for `AIMEM_DOMAIN`.

With neither, the install is refused before anything changes.

The script then:
1. has `aicrew operator-token new` write the operator credential to
   `~/aicrew/etc/operator.token`. This happens on a fresh install only. The
   script never reads the file; it is the `--token-file` of the operator's
   `aicrew` commands on this host;
2. writes `aicrewd.json` with `service_id` (`AICREW_SERVICE_ID`, default
   `aicrew-service`), `listen_addr` (`AICREW_LISTEN`, default
   `0.0.0.0:9443`), the certificate, the store and the operator token, and
   no hub;
3. starts aicrewd and waits until `/healthz` answers at the release;
4. prints what remains: provision the service on the aimem hub
   (`aimem identity peer provision`), bind the hub with `aicrew hub add`,
   and restart.

**An upgrade** is the newer release's one-liner, run again on the host:
1. Before anything changes, the new `aicrewd config show` reads the
   configuration.
   - A configuration that still has the single `aimem` block of 0.2.0 must
     be moved into `aimem_hubs`. That needs `AICREW_CRED_DIR`, the directory
     `aimem identity peer provision` wrote, from which
     `aicrewd config migrate -cred-dir` takes the hub ID and the two team
     credential files, checked as `aicrew hub add` checks them.
   - The migration runs first on a scratch copy. If `AICREW_CRED_DIR` is
     unset, fails a check, or the copy would not migrate completely, the
     script refuses with nothing changed. It prints the two steps:
     provision on the hub, then rerun with the directory.
   - A configuration already in the `aimem_hubs` form needs no directory.
2. It stops aicrewd and copies the configuration and the store (with its
   `-wal` and `-shm` files) beside themselves as `<file>.backup-<UTC time>`.
   The previous binaries are kept as `aicrewd.prev` and `aicrew.prev`.
3. It swaps the binaries, migrates the configuration if needed, starts the
   service, and waits for `/healthz` at the new version
   (`AICREW_UPGRADE_WAIT` seconds, default 30).
4. If the new release does not come up, the rollback puts back the previous
   binaries, the configuration copy and the store copy. The store the
   failed run left is kept as `<store>.failed-<UTC time>`.

Other knobs:
- `AICREW_VERSION`: install a release other than the pinned one;
- `AICREW_REPO`: install from a fork;
- `AICREW_PREBUILT_DIR`: a directory holding `aicrewd` and `aicrew` to
  install instead of downloading;
- `AICREW_NO_SERVICE=1`: install-only mode, for a host or container
  without systemd, where a supervisor of the operator's choice runs
  aicrewd. `systemctl` and `loginctl` are neither needed nor called.
  - A fresh install does everything above except linger, the unit, the
    start and the health wait. It ends by printing the command to run as
    the service user, `~/aicrew/bin/aicrewd -config ~/aicrew/etc/aicrewd.json`
    (with the full paths).
  - An upgrade refuses, with nothing changed, while aicrewd still answers
    `/healthz`, because the script cannot stop it, and copying a store in
    use is not safe. Stop it, run the upgrade, then start the printed
    command. The copies, the `.prev` binaries, the swap and the migration
    are as above, but nothing is started and there is no health wait.
  - A failed swap or migration still puts the previous binaries, the
    configuration copy and the store copy back, and says to start the
    previous release again.

The script never writes a credential or a secret value itself.
`internal/installer` checks that `RELEASE` and every documented one-liner
name the newest CHANGELOG release, and runs the upgrade transaction in bash
against real builds, in both modes. It also runs the whole script in
install-only mode, with stubs for the account commands and for systemd. The release check refuses a tag that any installer
does not pin.

The service logs JSON lines to stderr: each request's method, matched route,
status and duration, never its headers, body, query or raw path. A request
body is capped at 64 KiB and headers at 16 KiB; the server also sets
read-header, read, write and idle timeouts. On SIGINT or SIGTERM it stops
accepting connections, lets requests in flight finish within
`shutdown_timeout`, closes the store and exits 0.

`GET /healthz` answers `{"status":"ok","version":"vX.Y.Z"}`, the running
release, which the installer waits for. Besides it, aicrewd serves aimem's
session introspection,
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
| `GET /v1/admin/escalations?team=TEAM&open=1` | list escalations, newest first and at most 200; the operator or an architect credential |
| `GET /v1/admin/escalation?id=ID` | show one; the operator or an architect credential |
| `POST /v1/admin/escalations/answer` | `{"id", "decision", "rationale"}`, once (`409 escalation_answered`); the operator or an architect credential |
| `GET /v1/admin/architect-credentials` | list architect credentials (metadata) |
| `POST /v1/admin/architect-credentials` | issue: `{"label"}`; the answer carries the bearer, once |
| `POST /v1/admin/architect-credentials/revoke` | revoke: `{"id"}` |
| `GET /v1/admin/team/state?id=TEAM` | the team's pause and each current member's state ("Team state, pause and nudge", below) |
| `GET /v1/admin/team-states?team=TEAM` | the same state for every team, or for one: a list ("The team's state from the architect seat", below); the operator or an architect credential |
| `GET /v1/admin/attempt/handoff?id=ATTEMPT` | an attempt's handoff as its worker last pushed it, in any state of the attempt ("An attempt's handoff", below); the operator or an architect credential |
| `POST /v1/admin/team/pause` | `{"team_id", "reason"}`: pause the team |
| `POST /v1/admin/team/resume` | `{"team_id"}`: resume the team |
| `POST /v1/admin/member/pause` | `{"team_id", "agent_id", "reason"}`: pause one member |
| `POST /v1/admin/member/resume` | `{"team_id", "agent_id"}`: resume one member |
| `POST /v1/admin/member/nudge` | `{"team_id", "agent_id", "note"}`: the operator's note to one member; answers the message's ID |

The credential routes are also served under their names before 0.3.0,
`/v1/admin/introspection-credentials` (and `/rotate`, `/revoke`). They are
kept until every member runs a release past them, then removed in a later
minor release the CHANGELOG announces.

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

**Names before 0.3.0.** These keep working, each with a one-line notice,
and are kept until every member runs a release past them, then removed in a
later minor release the CHANGELOG announces: the command
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

### Board wake

With a hub's `board_read_token_file` configured, and a team on that hub,
aicrewd reads the hub's board feed (aimem's `board.read`) at the same
15-second tick. A tick reads at most 4 pages of 500 changes, so a backlog
is read over several ticks and the reads stay far below the credential's
60 a minute. Each page is recorded in one transaction with the
`board.changed` events it journals and the cursor to read on from
(`docs/CREW-CONTRACT.md`, "Board changes"). The event core ("The event
core", below) then delivers them:
- **What wakes a coordinator:** a task of a project the team is granted
  that becomes READY, or that changes state on the board while the team has
  an attempt for it. The coordinator receives an `action.needed` message
  with a `board` subject, and the Stop hook wakes it as for any message.
- **What is skipped:** a change one of aicrew's own steps made. Each
  committed step keeps the task revision it made.
- **History:** a project's changes from before aicrewd first saw it
  granted are read and announced to no one. That covers aicrewd's first
  read, which starts at the board's first change, and a project granted
  later.
- **A restored hub:** a hub that answers `cursor_ahead` or
  `invalid_cursor` has the cursor dropped. The feed is read again from its
  start, as history.
- **A hub that asks to wait:** it is not read again before the time it
  named.
- **A coordinator's session start:** every page also records each task's
  latest state, history included. When a coordinator session starts or
  resumes, aicrewd writes one lifecycle message for each granted project
  with READY tasks that have no open attempt of the team, listing them (pilot
  finding #17). A coordinator that acknowledged an announcement in a turn
  that could not act, and then restarted, is woken by it. A restored hub's
  states are dropped with its cursor and read again from its feed. Schema
  31 drops the stored cursors once, so that an upgraded aicrewd reads every
  feed again from its start, as history, to fill the states.

### Forge events

A member waiting for a review, for CI or for a merge is woken by a
`forge.event` message, never by a background script it started: only a
message ends the Stop hook's wait (`docs/CREW-CONTRACT.md`, "Forge
events"). aicrewd reads no forge; the member's launcher does, with the
home's own forge credential:
- **The baseline:** every `aicrew-agent forge pr` command reports the pull
  request it acted on to aicrewd (`POST /v1/crew/pulls`) through the
  launcher, and prints `watch:` with whether it is watched. aicrewd matches
  it to the member's running attempt by repository and branch, and records
  it.
- **The watch:** while the member waits for its inbox, the launcher reads
  each watched pull request (the home's `state/pulls.json`) at the start of
  the wait and every 60 seconds, and reports it. A read costs one to three
  forge calls: the pull request, and on GitHub and Gitea the head commit's
  status, and on GitHub its check runs, every page of them. A read ends with
  the wait, so a slow forge cannot hold a short wait past its end.
- **The content:** when CI concluded or the comments changed since the
  launcher last reported the pull request, or the last check still
  running under a failure concluded, it also reads the content (the
  newest external review at the head with its verdict, the failed checks
  with their excerpts, the passed checks) and reports it. An observation
  also carries the declared check that failed at the head's `after-push`
  point (`pre_push`: the command, its exit and its output, with the names
  of the other checks that failed there; "The extension points", below)
  for as long as the launcher keeps it for the head. The launcher cuts an
  observation to aicrewd's request bound before sending it (the passed
  checks beyond 30, then the review's body, the excerpts and the check's
  output halved in turn, each cut marked; the message shows no more
  anyway); the route takes the server's 64 KiB, not the step routes'
  4 KiB.
- **The message:** aicrewd compares a report with the last one and
  journals one `forge.changed` event for a change, in the same transaction.
  A change that needs a decision (the review at the head asks for changes,
  CI failed, a declared check failed at the head, both gates green, a
  merge, close or reopen) is delivered by the event core ("The event
  core", below) as an `action.needed` message with that content, to the
  worker, and to the coordinators while the result is submitted or when
  the pull request was merged or closed. Progress alone wakes no one. A
  repeated report journals nothing. The failed check's need (`pre_push`)
  is the observation's: it is news at a head whose facts held no such
  failure, holds at delivery while the last observation at the head still
  carries it, and clears at a new head or when an observation at the head
  carries none (a run there passed). A worker that pushes head after head
  with the same command failing is heard by the coordinators too: the facts
  count the consecutive heads whose last observation carried a failure of
  one command (a run that passes at a failing head ends the count, and so
  does a head that never fails; a head's first observation, before its
  checks ran, does not), and from the third head on the `pre_push` wake
  goes to the team's coordinators with the worker, its text naming the
  count (the `check-streak` condition of `forge-wake`).
- **The end of a watch:** a merged or closed pull request, or one that is no
  running attempt's (`no_attempt`), is no longer watched. A command on a
  merged or closed pull request reports it and prints `watch: not watched:
  it is merged` (or `closed`).

### The event core

The board wake and the forge events are two sources of one event model
(`docs/DESIGN-TURNS.md`, section 12; increment 1b). What happens on an
event is data; the bus is core.
- **The journal** (store schema 33). A source writes each event in the
  transaction that moves its own cursor: the board page with its cursor, a
  pull request observation with the attempt's last observation. An event
  names its source, subject, kind, time and a key unique within the
  source, with a payload of at most 16 KiB. A board page read again is the
  same events, and queues nothing twice.
- **The subscriptions** are data: `internal/events/subscriptions.json`,
  embedded in the release, with its JSON Schema beside it
  (`internal/events/subscriptions.schema.json`). A subscription names its
  event kinds, its recipients by role, each with the conditions under
  which it is woken, and its handler:
  - `board-wake`: `board.changed` to the team's coordinators, when the
    change is not one of aicrew's own and the task became READY or the
    team has an attempt for it;
  - `forge-wake`: `forge.changed` to the worker always, and to the
    coordinators when the change bears on a submit or the result is
    submitted, or when the same declared check failed at the bound of
    heads in a row;
  - `blocked-wake`: `attempt.changed` to the coordinators when the change
    is a worker's blocked report (increment 1d, below);
  - `automation-wake`: `automation.run` to the coordinators when the run
    failed, and to the worker when it asks for a turn (increment 2d-a,
    "Workflow scripts");
  - `review-pickup-wake`: `timer.due` to the coordinators, for the review
    pickup below;
  - `answer-wake`: `attempt.answered` to the attempt's worker, with the
    coordinator's answer to its blocked report.

  These reproduce the routing from before the event core. The kinds, roles,
  conditions and handlers a file may name are this release's; a test
  checks the file against the schema, and the schema's names against the
  code.
- **Delivery.** The same transaction queues one delivery per matching
  subscription and team, keyed by subscription, team and subject (a task,
  or an attempt's pull request). Every 5 seconds aicrewd runs each queue
  whose events have been quiet for 30 seconds, or whose oldest has waited 2
  minutes. A run hands the queue's pending events, in order, to the
  handler, and records what it handled, in the transaction of the message
  it writes. So a burst of events about one subject wakes once, and a crash
  replays a run whole or not at all: pending events wait in the journal for
  the next start.
- **The handler** is `wake`: one `action.needed` subject per run (store
  schema 35 adds the kind), to the recipients the run's events name, read
  as they are at the run. A team's queues due at the same tick run in one
  transaction, and the subjects for the same recipients are one message.
  A pull subject carries every change of the run with the content its
  needs call for, a board subject the last change with each transition in
  its text, a blocked subject the reason with the attempt's pull request.
- **aicrewd's own sources** (increment 1d), journaled in the transaction
  of the change:
  - `attempt.changed`: an attempt's phase change (a blocked report, a
    resume, a submit, a review's decision, the finalize). A blocked
    report's reason, at most 4 KiB of it, is in the event; the attempt
    keeps the whole of it until it leaves the blocked phase.
  - `member.changed`: a member's session started, resumed, or ended (left,
    stopped or ended).
  - `attempt.answered`: the team's current coordinator answered a
    worker's blocked report (`aicrew-agent step answer`, CREW-CONTRACT.md,
    "Answer"), keyed by the request's idempotency key. Its payload is the
    attempt, the task, the worker, the coordinator and the answer's text,
    at most 12 KiB as JSON encodes it.
  - `timer.due`: a deadline stored in aicrewd (schema 34) passed. The
    delivery loop journals it once, on its first tick after the deadline,
    and a restart keeps it. The review pickup is its first user.
  - `budget.spent`: a member's attempt or daily budget is spent, as its
    launcher reported the hold (CREW-CONTRACT.md, "A member's hold"), once
    per hold. Its payload is the member and the hold.

  The default `blocked-wake` subscription wakes the team's coordinators on
  a blocked report. Its message names the reason and the attempt's last
  observed pull request (state, head, labels, CI conclusion, comment
  count), so the coordinator can answer or escalate without reading the
  forge. The report's broadcast to the team then leaves the coordinators
  out, so they are woken once for it. The coordinator answers a question
  that is its own with `aicrew-agent step answer`, and the default
  `answer-wake` subscription wakes the worker with one `action.needed`
  message holding the answer, as long as the attempt is open (a message
  to a worker whose attempt has since moved on says so). No default
  subscription takes the
  other attempt changes, member changes or spent budgets yet; a member's
  hold reaches the coordinators as aicrewd's own lifecycle message.
- **The review pickup** (DESIGN-TURNS.md 4.2, `external-review`). A
  worker whose pull request waits on its gates gets no turns and is never
  stalled, so a reviewer that never picks the request up would leave it
  waiting unseen. An observation of the attempt's pull request waits for
  a pickup while the pull request is open, carries `review-this`, carries
  no `hands-reviewing`, and the external review at its head has not
  landed. The wait begins at the first such observation, and begins again
  at a new head or a new pull request, or when the request is added again
  after an observation without it; aicrewd keeps it in the pull request's
  facts, so a restarted launcher's observations continue it. Each wait
  sets the attempt's one timer (`review-pickup/ATTEMPT`) 30 minutes on,
  which moves the deadline of a wait before it. When the timer is due, the
  `review-pickup-wake` subscription's handler reads the attempt again: if
  it is running and the same wait still holds, the team's coordinators
  (not the worker) get one `action.needed` message with a `review_pickup`
  subject naming the pull request, the attempt, the task, the head and the
  wait, otherwise no one. A pickup, a failed run (hands swaps the request
  for `hands-review-failed`), a review at the head, a new head and a merge
  or close each end the wait. The worker's own wait is unchanged, and the
  team state shows the wait as `review_wait`.
- **Failures.** A handler that fails three times in a row parks the
  queue's events and sends the team's coordinators one message naming the
  subscription, the subject and the last error. Parked events are kept;
  they are replayed by a release whose subscription file differs.
- **Upgrade and downgrade.** Schema 33 adds the `events`,
  `event_deliveries` and `event_runs` tables; nothing earlier is journaled.
  Schema 34 adds `attempt_blocks` and `timers`. Schema 35 rebuilds
  `messages` to admit the `action.needed` kind (SQLite cannot change a
  CHECK in place), every row kept, and adds `attempt_pull_facts`. Schema
  36 adds `attempt_automation_runs`, one row per automation run reported
  at an extension point. Schema 37 adds `attempt_handoffs`, each running
  attempt's handoff as its worker's launcher pushed it (CREW-CONTRACT.md,
  "The attempt's handoff"); every attempt is kept, with none. Schema 38
  adds `turn_reports`, each turn's cost as its member's launcher reported
  it (CREW-CONTRACT.md, "The turns' costs"); no turn is reported yet.
  Schema 39 adds `member_holds`, each member's current hold as its
  launcher reported it, and `member_hold_announcements`, the holds told to
  the coordinators (CREW-CONTRACT.md, "A member's hold"); no member is
  held yet.
  An older aicrewd refuses a schema-33 store, as for every schema change.
  Going back means restoring the store copy the upgrade made
  (`<store>.backup-<UTC time>`), which loses what was written since: the
  board is read again from the copy's older cursor, and the launchers
  report their pull requests again, so their changes are announced again
  rather than lost.

### Workflow scripts

A step that needs no judgement becomes a workflow script
(`docs/DESIGN-TURNS.md`, section 10): Starlark, run by
`internal/automation` (increment 2a) with the host API of 10.3 and
nothing more.
- **The dialect:** no `while`, no recursion, no reassigned globals, no
  `load`; `print` reaches nothing. The universe holds no file, network,
  clock or environment, and every global is frozen after the script
  loads.
- **The host API:** `pr()`, `attempt()`, `run(cmd, args)`,
  `step(kind, text)`, `comment(body)`, the transitions of 4.2 (increment
  2d-c) `label(add=[...], remove=[...])`, `ready()`, `submit(url)` and
  `rerun()`, the base update (increment 2e) `update_base()`, and
  `turn(kind, content, conflict=None)` as a value `main(event)` returns.
  Any other name fails the script at load, which is how a script written
  for a newer release fails on an older one: the host API version stays
  1, since the additions break no script. The results are bounded: a
  review's body and a comment at 16 KiB, a failed job's excerpt at 8 KiB,
  a command's output at 64 KiB, a `label()` call at 8 names of 64 bytes,
  a `submit()` reference at 1 KiB. `pr()` names the pull request's `url`,
  and each check's `runner_failure`: the forge's own word says the runner
  failed, not the change (`forge.RunnerFailure`: GitHub's `cancelled`,
  `timed_out`, `startup_failure` and `stale`; GitLab's
  `runner_system_failure`, `stuck_or_timeout_failure` and
  `scheduler_failure`; Gitea's `cancelled`, which its commit statuses do
  not carry, a cancelled run reading as a failure there).
- **Text, not bytes:** every host result is a Starlark string. A
  command's output is its standard output and standard error together;
  each run of bytes that is not UTF-8 becomes one U+FFFD, and the text is
  then cut on a rune boundary, keeping its start (`automation.Bound`).
- **The limits:** 1 million execution steps and 2 minutes of wall clock,
  `run()` calls included, and a memory limit of 512 MiB on the script's
  process (below).
- **The allow list:** `run(cmd, args)` runs a bare command name with an
  argument list and no shell, only when the worker turn's own list admits
  it (`agent.TurnAllowList`: the managed `Bash(...)` rules and the team's
  toolchain, under the turn's deny rules). The list is never widened for
  a script.

**The script's process** (increment 2b). The launcher runs a script with
`StepServer.RunAutomation`, in a child process, `aicrew-agent automation
run`, since the interpreter has no memory limit of its own:
- **No credential reaches it.** Its environment is empty (Windows adds
  `SYSTEMROOT`), its arguments are the command's name, and its working
  directory is the home's private state directory. Its standard input
  carries the script, the event, the allow list, the limits and a token
  that names this one run.
- **The memory limit** is set by the child before any part of the script
  runs; it refuses to run the script when it cannot set it:
  - **Windows:** a job object of the child's own with a process memory
    limit (committed memory). It nests inside the launcher's kill-on-close
    job.
  - **Linux:** `RLIMIT_DATA`, the data segment the process holds at its
    start plus the limit. `RLIMIT_AS` would not do: the Go runtime
    reserves address space it never uses and fails at once under it. The
    data limit holds every private writable mapping, a thread's stack and
    the C library's thread state with the heap, so a child near the limit
    whose runtime needs another OS thread dies there first: with the
    runtime's `pthread_create failed` line, or with a signal where the C
    library's thread start touched what it did not get. The launcher reads
    those as the limit acting, as it reads the runtime's `out of memory`:
    the script is interpreted and memory-safe and the child runs nothing
    else, so nothing but the limit ends it that way. The line is kept in
    the failure's text.
  - **macOS and the rest:** `RLIMIT_DATA` where the platform has it, and a
    watchdog that ends the process when its Go runtime holds more than
    the limit, since macOS does not enforce the data limit for the
    runtime's mappings.
- **The host** comes back over the home's step socket as `automation`
  calls carrying the run's token, valid only while the run lives, and
  served beside the steps, never under their lock. `pr()` reads the
  attempt's pull request with the home's forge credential (`behind` from
  the worktree's last fetch of the base, the one at `after-push` for the
  patch identity, so a base that moved since reads as up to date: the
  fresh reading is `update_base()`'s own answer, at the cost of one
  fetch, which is why the base-update default calls it without reading
  `behind`); `attempt()` the session status,
  with the attempt's turn cost and budget; `run()` executes in the
  attempt's worktree with the client's environment, after the launcher
  checks the allow list again; `step()` and the run's outcome go to
  `logs/automations.jsonl`, redacted, the run's end record carrying the
  event it was for (its kinds and IDs, `base_only`, and on
  `forge.changed` the change, its texts redacted as `pr()`'s are, before
  the script's process sees it) and the facts `pr()` and `attempt()`
  returned to the script, every read in the order the script made it
  (`prs` and `attempts`, at most 16 reads each, since a replay reads a
  record as one line of at most 4 MiB and a pull request's answer carries
  its comments; the reads past that are counted in `prs_dropped` and
  `attempts_dropped`, absent when none were), so a replay runs a script
  against the log (increment 2f, below). The single `pr` and `attempt`
  of a record written before the reads were kept in order are read and
  never written: the replay is the log's only reader, and it still reads
  them; `comment()` posts on the attempt's
  pull request. The transitions act with the home's credential too, and
  each is recorded as a step of the run (`host_label`, `host_ready`,
  `host_submit`, `host_rerun`, `host_update_base`): `label()` removes, then adds, labels the
  repository defines; `ready()` lifts the draft and its prefix
  (`forge.ReadyPull`); `submit(url)` asks for the submit, which the
  launcher sends once the script ended without failing: the
  before-submit point first, then the holder's own work step with `url`
  as the result reference, so the attempt is submitted as the member
  would submit it, outside the script's wall clock; a submit that fails
  fails the run (a run at `before-submit` itself cannot submit: the step
  that runs it does); `rerun()` re-runs the failed jobs at the head once
  per head, the head claimed among the pull request's re-run heads in
  `state/points.json` (kept as long as the pull request's entry is) before
  the forge is asked, so a second call at the head, in the same run or a
  later one, or after the branch came back to the head, re-runs nothing
  and says so. The forge's call is `forge.RerunFailed`: GitHub
  re-runs the failed jobs of each workflow run a failed job belongs to
  (the credential needs write access to Actions; without it the forge
  refuses and the script fails), GitLab retries each failed job, and
  Gitea offers none through its
  commit statuses (its Actions API gained a job re-run only in Gitea
  1.26, and a commit status names no job), so there `rerun()` re-runs
  nothing, leaves the head unclaimed and the CI failure reaches the
  worker as today.
- **`update_base()`** (increment 2e, the "branch behind its base" rows of
  4.2 and the worked example of 10.4) brings the pull request's branch up
  to date with its base, with the launcher's git and the clone's
  credential helper, so the script runs no network git and holds no
  credential: the launcher fetches the base and the branch, and in a
  temporary detached worktree of the attempt's clone at the branch's
  head as the origin has it (a turn's uncommitted work in the attempt's
  worktree is never touched) merges the base with no edit, computes the
  patch identity before and after (`patchIdentityAt`, around
  `git patch-id --stable` against the merge base), pushes the merge
  without force when the identity is unchanged, and posts the delta note
  of 10.4 (the old and the new head, the identity, the command) as a
  comment. The result is a struct: `updated`, `old_head`, `new_head`,
  `identity_changed`, `conflict` (None, or `base`, `files` and `hunks`,
  bounded to 50 files and 16 KiB) and `reason`. A conflict aborts the
  merge and is a result, as is a changed identity (nothing pushed), a
  push the origin refused because the branch moved (nothing else done;
  the new head is an event of its own) and a branch up to date; the
  forge or git failing is an error that fails the script. The attempt's
  worktree is fast-forwarded to the new head when it is clean and at the
  old head. The outcome is one `host_update_base` step; the temporary
  worktree is removed whatever happened. The pushed head reaches the
  launcher as an observation like any other: `after-push` fires with
  `base_only` true (the identities match), so `review-request` leaves the
  reviews standing. `turn(kind, content, conflict=u.conflict)` carries
  the conflict to the worker: the run report's `conflict` field, and the
  action subject's (CREW-CONTRACT, "Event delivery"). A run cut between
  its push and its delta note is completed at the next call (increment
  2e-2; `docs/DESIGN-TURNS.md` 12.4): the launcher records the update in
  progress on the pull request's entry in `state/points.json` (the old and
  the new head, the base, the identity, and whether the push and the note
  are done) before the push and after each effect, and clears it after the
  step once the note is posted; a later `update_base()` whose fetch finds
  the origin's head at the recorded new head posts the note unless it was
  posted, fast-forwards the attempt's worktree, records the step and
  clears the record, with no merge and no push, and the script gets
  `updated` true with the recorded heads and the reason "completed the
  update cut after its push"; at any other head the record is stale and
  goes. A push the origin refused leaves the record for the same reading,
  since a push the origin applied but did not acknowledge is one.
- **A failed run** (`fail()`, a limit, a refused command, a host error,
  the process ending) is a result and an `automation_failed` step. The
  launcher never retries it.

**Which automations run** (increment 2c, `docs/DESIGN-TURNS.md` 13.1). An
automation is an entry: a name, where its script comes from, and when it
runs, at an extension point (`after-push`, `after-ci`, `ci-failed`,
`gates-green`, `before-submit`, `after-merge`), for the event kinds it
subscribes to, or both. The entries come from three sources:
- **The release's defaults,** embedded from `defaults/automations/`:
  `subscriptions.json` holds their entries with the source `release`, and
  the scripts sit beside it. Since increment 2d-d they are the transitions
  of `docs/DESIGN-TURNS.md` 4.2 the launcher performs itself:
  `review-request` at `after-push` (clear `hands-reviewed`, add
  `review-this` once; nothing for a base-only update, while
  `hands-reviewing` or `review-this` is present, or when the external
  review at the head exists) and on `forge.changed` (task 05cf: a change
  that carries the external reviewer's failure, its could-not-run comment
  read as the notice `review-failed` or `hands-review-failed` added, gets
  `review-this` once more with the failure label removed, unless
  `hands-reviewing` or `review-this` is present; a second failure at the
  same head gets a step and no request, as the 4.2 row says; the script
  acts on the change, never on the absence of a label, so an empty label
  read adds nothing), `ci-rerun` at `ci-failed` (`rerun()` once a
  check failed on the runner) and `gates-green` at `gates-green`
  (`ready()`, then `submit(pr().url)`, unless the attempt is submitted
  already; the script reads no label, the point's edge does) and
  `base-update` at `after-ci` and on `forge.changed` (task 2e and its
  scheduling follow-up: `update_base()` unless `hands-reviewing` is
  present, without reading `pr().behind` first, since the call's own
  up-to-date answer is the fresh reading of the base at the cost of one
  fetch; on `forge.changed` only when the change removed
  `hands-reviewing`, the label is not back and every check at the head
  has concluded, so an update the review held is retried when the
  review ends, or when the change carries `checks_settled` with a check
  at the head failed: the last check that still ran when the first one
  failed has concluded, so the update `after-ci` could not make while a
  check ran is made then (task 01a1540d-38c3; a settle into success is
  `after-ci`'s own, and a re-run after a failure settles again and runs
  the update once more, at the cost of one fetch when the branch is up
  to date); `after-ci` keeps its meaning for every subscriber; a
  call that reports an updated branch with a reason, the completion of
  a cut update or a note not posted, is followed by one call more, so a
  base that moved on in between is merged; a conflict is a `fix` turn
  carrying it, a changed identity a `fix` turn with the reason, and a
  refused push, a clean update and a branch up to date ask for nothing:
  the reviews stand under the base-only rule).
  Their tests run against the fake host
  (`internal/automation/defaults_test.go`), and one asserts the scripts
  name no host, endpoint or instance: a default is universal release
  content.
- **The process manifest at the attempt's pin:** its `automations` list.
  A `process` script sits beside the manifest at the pinned commit, named
  from the repository's root as the manifest names its other files. A
  `repository` script is under the repository's `automations/` at the
  base branch's merged commit, which the launcher fetches; the attempt's
  head is never read, so a worker cannot change the script that acts on
  its own pull request.
- **Precedence by name:** the manifest's entry replaces the release default
  of the same name, or turns it off with the source `"disabled"`; a name
  the defaults lack is added; a default the manifest does not name
  applies.

A script's first line is its header, `# aicrew-automation: {"host_api":
1}`, naming the host API it was written for. The entries and the header
have published JSON Schemas, `internal/automation/automations.schema.json`
and `header.schema.json`, versioned with the host API; the tests validate
the defaults and examples against them (`internal/schematest`). The
launcher keeps the resolved set (`StepServer.Automations`) and reads it
again only when the pin changes or the base branch's commit changes
`automations/`, without a restart. A script it cannot read, or whose
header it refuses, stays in the set with its problem. `aicrew-agent check`
lists the open attempt's effective set, each entry with its name, when it
runs, its source and its commit; without an attempt, the release's
defaults.

**The extension points** (increment 2d-a, `docs/DESIGN-TURNS.md` 9.1 and
4.1). The launcher dispatches the points of its attempt's pull request
from what its forge poll observes, once per edge: `after-push` for a head
it had not seen, `after-ci` for a head and a CI conclusion, `ci-failed`
for a head whose CI failed, `gates-green` for a head whose local and
external reviews are both `READY_FOR_HUMAN_MERGE` while its CI passed and
no external review is in flight (`hands-reviewing` absent: a run then would
consume the head's one edge without the transition, so the first
observation without the label takes it), `after-merge` for the merge, and
`before-submit` in the `work` step that
carries the submit, before it is sent. The fired edges are the home's
`state/points.json`, so a restarted launcher does not fire a point it
already fired, and an edge once fired stays fired however the
observations flap (CI failed, passed and failed again at one head fires
`after-ci` twice, not three times); an edge is recorded after its run
ends, so a run cut short by a restart runs again at the next observation
(at least once, 12.4). A point that could not be prepared, because the
session status or the effective set could not be read before any entry
started, records no edge either: nothing ran, so it is not a failed run;
the next accepted observation fires it again, and the error is logged
once per edge, not at every poll.
A merged pull request leaves the watch list, so
the file keeps it until its `after-merge` ran, and the next poll runs the
point first.

**The forge event** (task 05cf, `docs/DESIGN-TURNS.md` 12.2). Beside the
points, the launcher runs the script entries whose `events` name
`forge.changed` once per accepted observation that changed the pull
request against the state it keeps per pull request in
`state/points.json`: the labels, the checks conclusion, whether a check
was still running, the comment count and the time of the newest comment
its last content read saw (no comment body is kept). A change is the head
moved, the state or the checks conclusion changed, the last running check
concluded at the same head, labels added or removed, or the comment count
risen;
the first observation of a pull request sets the state and is no change,
and the first content read sets the comment mark and names no comment as
new, so history is never acted on (a pull request observed without a
comment has its mark already, so each comment that then arrives is new,
and the ten the event carries are chosen with the external reviewer's
notice first, so the bound never drops it). The event's `change` (`event.change`
in the script; `None` at a point, so host API 1 stands) carries
`head_moved`, `state` and `checks` (the new value when it changed),
`labels_added`, `labels_removed`, `comments` (the new ones since the mark,
oldest first, each with `author`, `kind`, `body` and `notice`, at most ten
of 4 KiB), `review_failures`, the count of observations at this head,
this one included, that carried the external reviewer's failure: a new
comment the forge layer read as `review-failed` (`[hands-bot review] could
not run`) or `hands-review-failed` added, and `checks_settled`: the last
check still running at the head concluded since the poll before. Under a
failure the conclusion stays `failure` while another check runs and when
it ends, so `checks` does not show that end and no `after-ci` edge comes;
a re-run that runs and concludes again settles again. Whether a check
runs is read from each check run and commit status (the combined state
hides a pending status under a failure; GitLab's pipeline reports it
running until it ends) by the launcher's poll only: a member's own
report carries none and changes nothing of it at the same head, while
its report of a new head forgets a check running at the old one, so the
new head's first poll settles nothing. It stays with the
launcher: the observation aicrewd receives is unchanged, since aicrewd
decodes it strictly and would refuse a field it does not know. The entries run after the
observation's points, under the same run lock, and each run is reported
with `events` in place of a point. A declared command never runs for an
event. Unlike a point, the change has no edge: the kept state moves on
with the observation, so a dispatch the launcher's exit cuts, or one it
could not prepare, is logged and not repeated; a follow-up brings these
runs under the at-least-once rule of 12.4.

**The gates wake** (increment 2d-d). Each observation the launcher
reports names the automations of its attempt's effective set that run
(`automations`: every entry but a disabled one, read from the release and
the process manifest and kept per pin, so a poll reads nothing anew).
aicrewd adds the `gates` need, the worker's cue to mark the pull request
ready and submit, only when `gates-green` is not among them: with the
default in the set the launcher performs the transition, and the worker
learns of the submit through the attempt's lifecycle message. A process
that disables the default gets the wake as before, and so does a launcher
whose release or process manifest cannot be read, which names no
automation. A set that fails later, at a repository-sourced script's base
read, still names its automations: the point then records no edge and
fires again at the next observation, so the transition the names promise
is performed once the base reads.
The event a script gets at a point names the point as its kind and the
pull request with its head as its ID, and carries `base_only`: at
`after-push`, whether the new head's patch identity (`git diff
<merge-base> <head> | git patch-id --stable`, computed in the attempt's
worktree) equals the previous head's. A manifest entry may declare a
**command** instead of a script, `"source": {"command": ["git", "status",
"--short"]}`: 1 to 32 arguments, at a point only, with no events or turn,
and never in the release defaults. The launcher runs it without a shell in
the attempt's worktree, under the turn's allow list and within the
wall-clock limit, with the client's environment only: a command runs code
from the attempt's head, so it receives no credential through its
environment or through git. Its environment holds no forge credential,
session token or other token (a test reads the environment a command
received); git's credential helpers are reset for its process (an empty
`credential.helper` through `GIT_CONFIG_*`), so `git credential fill` and
any network git at a point fail by design, and forge actions are the host
API's; the declared command itself is never the helper command
`aicrew-agent git-credential`; and its arguments cannot name
`credential.helper` (also scoped to a URL), `core.askPass` or
`GIT_ASKPASS`, as a `-c` or `--config-env` setting, a bare `key=value`
word or key, or a `GIT_CONFIG_*` word carrying one, which would re-add a
helper after the reset: such a command is refused as a failed run naming
the setting. The code it runs still has the member's OS-user access, as a
worker turn's code does (`docs/DESIGN-TURNS.md` 9.1: a command runs under
the member's own account; 5.3 for the host). Its
output, bounded, and its exit status are recorded in
`logs/automations.jsonl`. A command the launcher's own exit cut is no run
of the command: nothing is recorded or reported, and the point fires again
at the next launcher's observation (12.4). A refused command, one that did
not start or one that passed its wall-clock limit is a failed run at every
point, and so is
a nonzero exit, except at `after-push`, where it is the check's verdict on
the head (increment 2d-b, `docs/DESIGN-TURNS.md` 9.1 and 5): the run is
handled, with its exit recorded, and the first check that failed (the
command, its exit, up to 8 KiB of its output, and the names of the other
checks that failed there) is kept in `state/points.json` for the head and
sent as `pre_push` with the pull request's observations, cut to what the
observation takes (the command within 1 KiB with the cut marked, forty
names, the output without NUL; the manifest admits more), once right after
the run and with every poll at the head, so that aicrewd wakes the worker
with it and the need holds across polls and restarts; a head where the
checks pass clears it. The coordinators are not woken for a failing check:
it is the worker's change to fix. Every run is
reported to aicrewd, the report cut to what the route takes as encoded,
the steps' texts first and the command's words last, a cut marked
(`POST /v1/crew/automations`; store schema 36 keeps
one row per run, so a report sent twice is one run) and journaled as
`automation.run`. The default `automation-wake` subscription wakes the
team's coordinators on a failed run and the worker on a turn, each role in
a message of its own holding only its runs, so a queue with both does not
hand the worker a failure only a coordinator can act on; a handled run
wakes no one. A failed run is never retried (10.3): the message names
the entry, the point, the pull request and the error, with the run's
steps, and the coordinator fixes or disables the entry. `aicrew-agent
check` shows a command entry with the source `command`, the manifest's
commit and the command's words.

### Replaying a run log: `aicrew automation replay`

`aicrew automation replay --script FILE --events LOG` (increment 2f,
`docs/DESIGN-TURNS.md` 13.3) runs a workflow script against a member's
run log, `logs/automations.jsonl` under its home, with a recording host:
it is the fake host of the automation tests made a command. Each run
recorded with its event is replayed, in the log's order, with the
script's `main(event)` under the interpreter's own limits and no child
process: `pr()` and `attempt()` answer the facts the run recorded, the
Nth read with the run's Nth read, so a script that reads the pull
request, labels it and reads it again does what the run did. A read past
the recorded ones answers the last again, and the output says so with
the count the run recorded (or kept, when it dropped reads past its
bound): a replay exists to explore a script that reads differently from
the run, so it goes on and says where the reads diverged. Recorded reads
the replay did not make are listed after the script ends. A run that
recorded none answers an empty value, and the output says so; a record
written before the reads were kept in order answers its one answer to
every read, as it did then;
`run`, `step`, `comment`, `label`, `ready`, `submit`, `rerun` and
`update_base` are printed as what the script would have done and perform
nothing (`run` answers exit 0 with no output, and every command is
admitted, since none runs; `rerun` and `update_base` answer the reason
"not performed in a replay"). The output lists, per event, its kinds and
IDs, the run and the script that recorded it and how that run ended, the
actions in order, and how the replay ended: handled, a turn with its kind
and content, or failed with the error (`fail()`, a limit, a script that
does not load). A script that fails is reported per event and the replay
goes on; a line of the log that is not a record is the command's error.
The script needs the header of 13.3; the command reads no hub and no
forge, and a step record or a run recorded before the launcher kept
events is skipped.

## Teams

A team has a name and a hub. Its projects are the grants its hub's
operator gives the team's profile there (`aimem identity team grant
--peer SERVICE_ID --team-name crew --project PROJECT`): aicrew keeps no project list of its
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
bin/aicrew team toolchain --team-name crew --project PROJECT --toolchain dev-go
bin/aicrew team toolchain --team-name crew --project PROJECT --clear
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
- `toolchain` selects the development toolchain a worker's headless turn
  runs for one of the team's projects (pilot finding #18), or clears it
  with `--clear`. The name is one of aicrew's named lists (today only
  `dev-go`; "Wake modes" below lists its rules); aicrew and aicrewd both
  refuse any other. aicrewd keeps one selection per team and project
  (`POST /v1/admin/team/toolchain`, store schema 32) and prints the team
  with its `toolchains`. A project the hub does not grant as last read is
  accepted, with a note on stderr, and applies once it is granted. Members
  read the selection with their projects (`GET /v1/crew/projects` carries
  `toolchain`) at their next session start, so a member's launcher is
  restarted after a change. The coordinator's turn ignores it.
- Members join through invitations (below); `aicrew team` does not change
  membership.

### Team state, pause and nudge

The operator's control of a running team (`docs/DESIGN-CONTROL-PLANE.md`,
section 4.2 and decision D12; increment C1):

```sh
bin/aicrew team state    --team-name crew
bin/aicrew team pause    --team-name crew [--reason TEXT]
bin/aicrew team resume   --team-name crew
bin/aicrew member pause  --team-name crew --agent builder [--reason TEXT]
bin/aicrew member resume --team-name crew --agent builder
bin/aicrew member nudge  --team-name crew --agent builder --note TEXT
bin/aicrew member nudge  --team-name crew --agent builder --note-file PATH|-
```

**Team state.** `state` prints the team's pause and, for each current
member:
- its label, role, and the client and model its agent reported;
- its active session: ID, generation, start, and when aicrewd last heard
  from it (`last_seen_at`) and last saw its wake read the pending list
  (`last_waited_at`);
- its own pause;
- its newest open attempt: ID, state, phase, task, and when it last
  changed (`updated_at`). While the worker has reported it blocked,
  `block` gives the reason and when the block began; `hub_block` is the
  hub's own, when the team's project is no longer granted. The two are
  kept apart (schema 34), and a block reported before schema 34 shows no
  `block` until the next report. While its pull request asks for the
  external review and the reviewer has not picked it up, `review_wait`
  gives the pull request, its head, since when the request has waited
  there, and `overdue: true` once that is 30 minutes or more (the review
  pickup, "The event core");
- its unacknowledged messages;
- its `state`, derived as follows until the runner reports it (B2):
  - `paused`: the team or the member is paused;
  - `offline`: no active session, or aicrewd has heard nothing from it for
    30 minutes;
  - `waiting`: its wake read the pending list within the last 15 seconds,
    which the Stop hook's `wait-inbox` does every 3 seconds while it waits;
  - `working`: otherwise.

  What aicrewd heard is kept in memory, so after a restart a session counts
  from its start or resume until its next call. `needs_login` and
  `limit_reached` arrive with the provider profile (A5), the current turn
  with the turn reports (A4), and the runner and host with B2.

**Pause and resume.**
- **What a pause does.** A pause is a scheduler state, not a message that a
  model has to obey. While the team or the member is paused, the member's
  pending read (`GET /v1/crew/inbox/pending`) lists nothing and names the
  pause: `{"pending": [], "paused": {...}}`. So the Stop hook's `wait-inbox`
  starts no turn on a new message, and its keep-alive turn tells the member
  that the operator paused it. aicrewd decides this, so it holds for an agent
  of any release.
- **Where it shows.** The session status (`GET /v1/crew/session`) names the
  pause in force, for the turn loop (A3) to read before a turn.
- **What it leaves alone.** A pause never touches attempts, reservations or
  sessions. A turn already running ends normally and may still read its
  inbox.
- **Resuming.** Messages that arrived during the pause stay in the inbox,
  and the next wake after the resume delivers them.
- **The team and a member.** The team's pause holds every member. A
  member's pause holds that member alone and outlasts a resume of the team.
- **Repeating a call.** Pausing what is paused, or resuming what is not,
  changes nothing: `changed` is false and stderr says so.

**Nudge.**
- `nudge` writes one lifecycle message to the member alone. Its `text` is
  "A note from the operator: " followed by the note, and its `operator`
  carries the note and who sent it. Notes are at most 4 KiB.
- The message wakes the member like any other, once it is not paused. The
  member's rules (`docs/ROLES.md`) say to act on it within the member's role
  and acknowledge it, and that it assigns no task.
- Use it to tell a member something the board does not carry, for example
  that its upgraded agent is installed and it should restart with `run`.

**Naming a member and the team.**
- `--agent` names the member by its agent ID, or by its label when exactly
  one current member has it. The team is named by `--team` or `--team-name`.
- Every route is the operator credential's alone. An unknown team or a
  non-member is `404 not_found`.
- A team route naming a member, a member route naming none, or a resume with
  a reason is `400 invalid_request`.

### Pull requests waiting for the merge: `aicrew merges`

```sh
bin/aicrew merges [--team TEAM]
```

The merge is a human click on the forge, and nothing else told the operator
that a pull request waits for it (`docs/DESIGN-CONTROL-PLANE.md`, section
4.2; increment C3). `aicrew merges` (`GET /v1/admin/merges[?team=ID]`) lists
the attempts whose latest result the team's coordinator reviewed and
accepted, and whose delivery is not confirmed yet, oldest first:
- **Each entry** names the team, the attempt, the task, the worker (agent
  ID and label), the accepted result's sequence and reference (the pull
  request), when it was submitted, and the branch.
- **`pull`** is the pull request as the worker's launcher last observed it
  ("Forge events"): its number, URL, state, head commit, labels, CI
  conclusion and comment count, and when it was seen. It is absent when the
  worker never reported one.
- **What is not listed:** a result only submitted, one sent back for
  rework, and one whose delivery is confirmed or finalized.

aicrewd reads only its own records: no forge call, no merge, no change to
the pull request. The operator credential reads it, and so does an
architect credential (`/arch-merges`), which still reaches no operator
write. A team is named by its ID, since an architect credential cannot
resolve a name through the team routes; without `--team` the list covers
every team.

### The team's state from the architect seat: `aicrew status`

```sh
bin/aicrew status [--team TEAM]
```

The operator sees the crew's members, who work in their own sessions,
through the architect seat. `aicrew status`
(`GET /v1/admin/team-states[?team=ID]`) lists each team's state, as
`aicrew team state` prints it ("Team state, pause and nudge", above), for
every team (at most 100, by name) or for the one named:
- **What it adds.** Each open attempt's `updated_at` and
  `handoff_updated_at` (when its worker last pushed its handoff, absent
  when it has none), and `stalled: true` on a member that looks stalled.
- **Stalled.** A member is stalled when it is working or waiting, has an
  open attempt, and that attempt has not changed for over an hour (twice
  the working window). A paused or offline member is never stalled, since
  its state already says why it does nothing. Nor is one whose attempt
  waits on someone else through a channel of its own: an accepted result
  waits for the human merge (`aicrew merges`), a blocked one for an
  escalation's answer, and a finalized one is done.
- **Who reads it.** The operator credential reads it, and so does an
  architect credential (`/arch-status`). The architect credential still
  reaches no pause, resume or nudge, and not `GET /v1/admin/team/state`.
- **Naming the team.** A team is named by its ID, as for the merges.

### An attempt's handoff: `aicrew attempts handoff`

```sh
bin/aicrew attempts handoff --id ATTEMPT [--json]
```

The worker's launcher pushes its session doc to its attempt after each turn
(CREW-CONTRACT, "The attempt's handoff"), so the doc says what the worker
recorded: its decisions, what it verified, and its next step. The operator
reads it with `aicrew attempts handoff` (`GET /v1/admin/attempt/handoff?id=ID`),
for instance before deciding on a stalled or blocked attempt; the team
state's `handoff_updated_at` says whether there is one.
- **Who reads it.** The operator credential, and an architect credential.
- **Any state.** The doc is kept after the attempt closes, and reads the
  same.
- **The print.** The header names the attempt, its team and the push time;
  the doc follows. The text is the worker's, redacted and bounded by its
  launcher only, so the plain print removes terminal escapes and control
  characters other than newline and tab. `--json` prints aicrewd's answer
  as it is.
- **None.** An attempt without a handoff prints that it has none.

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

## The architect directory: `aicrew architect init`

The architect is the operator's own interactive Claude Code session, which
plans with the operator and writes epics and tasks to the aimem board
(`docs/DESIGN-CONTROL-PLANE.md`, section 7). It is not an agent home:
- no aicrew membership or team session;
- no forge credential, and no Stop hook;
- the control plane does not run it.

`aicrew architect init` writes its directory. It calls no service.

```sh
bin/aicrew architect init --dir ~/aicrew/architect --project PROJECT [--project PROJECT ...] [--hub HUB] \
    [--url URL --tls-trust-mode MODE --tls-trust-value VALUE]
cd ~/aicrew/architect && claude
```

**The managed files.** A rerun follows the agent home's rule: a managed file
is updated only while it is unchanged since its last managed write, and
otherwise the new version is written beside it as `<file>.aicrew-new`. The
command then exits 1 and names the merge to do.
- `AGENTS.md` and `CLAUDE.md` point to `docs/ARCHITECT.md`.
- `docs/ARCHITECT.md` is the guidance:
  - planning with the operator, from goal to epic to tasks;
  - how a task is written so that a worker can take it, including the kind
    of work and the capability it needs;
  - what READY means. A task moves to READY only on the operator's word.
  - how escalations are read and answered;
  - the limits.

  It lists the projects, and the session names the project on every task
  call.
- The commands:
  - `/arch-plan` plans a goal into an epic and tasks;
  - `/arch-task` writes one task;
  - `/arch-ready` checks a task against READY, and moves it on the
    operator's word;
  - `/arch-escalations` lists the coordinator's open escalations with
    `aicrew escalations list --open`, and answers on the operator's word
    (below). Without a connection, it reads them from the tasks' comments
    headed `[escalation.request ID]`.
  - `/arch-merges` says in words which pull requests wait for the
    operator's merge, with `aicrew merges` ("Pull requests waiting for the
    merge", below). It never merges and never changes a pull request.
  - `/arch-status` says in words what each member is doing, with
    `aicrew status` ("The team's state from the architect seat", above),
    and points out a member that looks stalled. It never pauses, resumes
    or nudges.
- `.claude/settings.json` denies the session the directory's `creds/`,
  through the Read and Edit tools and in Bash or PowerShell command text,
  with the limit `docs/WORKSPACE.md` states. `creds/` is created
  owner-only, for the architect credential (`creds/aicrew.architect`,
  below).
- `.mcp.json` has one `aimem` entry, which names no `AIMEM_*` variable. So
  the session uses the user's own aimem installation, in personal mode, as
  the human's own aimem user.

**Written once, never changed:**
- `docs/NOTES.md`, the session's own notes;
- `.aimem.json`, aimem's binding of the directory to the first project and,
  with `--hub`, to that hub. If your aimem installation needs more for this
  directory, such as a task credential's name, add it there as aimem
  documents it.

**The record.** `architect.json` records the projects, the hub and the
managed files' digests. A directory recorded with another layout is
refused, never migrated.

**The escalation commands.** With `--url`, `--tls-trust-mode` and
`--tls-trust-value`, init writes them into the managed settings' `env`,
with `AICREW_OPERATOR_TOKEN_FILE` naming `creds/aicrew.architect`. The
session's `aicrew escalations` commands then connect with no flags, and
never name the credential's path in a command, where the deny rules would
stop them. Put the credential there with the operator credential:

```sh
bin/aicrew architect credential issue --label planning --output ~/aicrew/architect/creds/aicrew.architect
bin/aicrew architect credential list
bin/aicrew architect credential revoke --id ID
```

An architect credential reads and answers escalations and reads the
merges list (`aicrew merges`) and the team states (`aicrew status`), and
nothing else, so the full operator token
never enters an AI session. It lives 90 days.

**First start.** The first `claude` in the directory asks to trust it and
to approve its `aimem` MCP server, and each aimem tool asks once for
permission: the operator answers, as for any session.

## Running aicrew-agent

`aicrew-agent` is an agent's client. It opens no store and needs no operator
authority: it proves the agent's aimem identity and keeps the agent's team
session through `aicrewd`'s client session API (`docs/CREW-CONTRACT.md`,
"Client session API").

### Starting a member

After `join` reports the home `ready`, the member is started with one
command, from a terminal, as the member's user:

```sh
aicrew-agent run --client claude --home HOME
```

`--client` is the client the home was joined for (`claude` or `opencode`).
The launcher enters the team session, starts the client in the home and
keeps the session while the client runs ("Running aicrew-agent" below). It
is the command `join` prints and the home's `docs/START.md` names.
`aicrew-agent session start` is not a way to start a member: it holds the
session with no client, and says so.

A home in `turns` mode (`join --wake turns`, "The turn loop" below) runs
without a terminal, as a service of the member's account:

```sh
aicrew-agent service install --home HOME
```

`join` runs it itself for a ready home in `turns` mode, so the member is
already running when `join` ends, and says so; `join --no-service` or
`AICREW_NO_SERVICE=1` leaves it out, and `join` then prints this command.

### The member's service: `aicrew-agent service`

A member started in a terminal lives as long as the window. Its service
outlives the window, and starts again at the next login and after a
failure:

```sh
aicrew-agent service install|uninstall|start|stop|restart|status (--home HOME | --all) [--json]
```

- **One service per home**, named `aicrew-member-<label>-<8 hex digits of
  the home path's hash>`. It runs `aicrew-agent serve --home HOME --log
  HOME/logs/serve.log` under the member's own account, with no elevation:
  - **Linux:** a systemd user unit,
    `~/.config/systemd/user/<name>.service`, enabled for the user's login.
    It restarts the serve after a failure, but not after a refusal (exit
    2). The unit carries the `PATH` `install` ran with, on which the serve
    finds `claude` and `aimem`; rerun `install` after it changes. **The user
    manager stops at the last logout** unless lingering is on;
    `install` notes when it is off. To keep the member running without a
    login, as the hub's service does: `loginctl enable-linger USER`.
  - **macOS:** a launchd agent,
    `~/Library/LaunchAgents/io.github.blackvs.aicrew.<name>.plist`, loaded
    in the user's login session (`gui/UID`), with the same `PATH`. Without
    a login session (an ssh login with nobody at the console) it is
    written, and loads at the next login.
  - **Windows:** a logon task, `\aicrew\<name>`, registered from its
    definition in `%APPDATA%\aicrew\tasks\<name>.xml`. It runs
    `conhost.exe --headless` around the serve, so no window opens, with
    the member's interactive token and no elevation, and never twice at
    once.
- **The verbs.** `install` writes and registers the service and starts it;
  rerun, it rewrites the definition from the current binary and `PATH`,
  which a running service takes at its next start (`restart` applies it
  now). `start` does the same for an installed service. `stop` stops it until
  the next login or `start`; `restart` is both. `uninstall` stops and
  removes it. `status` says `running` (with the serve's process ID),
  `stopped`, `not installed`, or `started by hand`. `--all` acts on every
  home with a service, as listed in the user's registry
  (`<user config directory>/aicrew/services.json`).
- **One serve per home.** `serve` holds `state/serve.lock` for its whole
  life and records its process ID in `state/serve.pid`. A second `serve`
  of the same home, from a terminal or a service, is refused (exit 2), so
  two never fence each other's session.
- **A serve started by hand** is reported, never duplicated: `install` and
  `start` refuse while one runs and the service does not, and say to stop
  it (Ctrl-C in its terminal) and then run `aicrew-agent service start
  --home HOME`. `stop` leaves it running.
- **Stopping.** systemd and launchd send the serve SIGTERM, which stops a
  running turn, records it, and leaves the session. Ending a Windows task
  ends only its `conhost`, so `stop` then ends the serve's own process, by
  the ID the serve recorded while it holds the lock, never by name. That
  ends the process without leaving the session; the next start resumes it
  with a new proof, and records a turn it found running as `interrupted`.
- **`check`** reports the home's service: `service: NAME: STATE (MANAGER)`,
  with a `to do:` line when a home in `turns` mode has none, a stopped one,
  or a serve started by hand. A `stop-hook` home uses none.
- **Upgrades.** The member one-liner stops every registered home's service
  before it swaps the binaries, and starts them again after
  ("Installing and upgrading" below).

### Installing and upgrading: `boot.sh` and `boot.ps1`

On a member's machine, `aicrew-agent` and the operator CLI `aicrew` are
installed and upgraded with the release's one-liner, from any directory,
as the member's own user. The operator's machine uses the same one-liner:
`aicrew architect init`, `aicrew architect credential` and
`aicrew escalations` run there.

- Linux and macOS:

  ```sh
  curl -fsSL https://raw.githubusercontent.com/BlackVS/aicrew/v0.9.0/boot.sh | bash
  ```

- Windows, in a PowerShell window that is already open:

  ```powershell
  irm https://raw.githubusercontent.com/BlackVS/aicrew/v0.9.0/boot.ps1 | iex
  ```

  From cmd, or from the Run dialog (Win+R):

  ```bat
  powershell -NoProfile -ExecutionPolicy Bypass -Command "irm https://raw.githubusercontent.com/BlackVS/aicrew/v0.9.0/boot.ps1 | iex"
  ```

  The second form starts a new `powershell.exe`. Typed into a PowerShell
  window that is already open, a host policy may refuse it
  (`Program 'powershell.exe' failed to run: Access is denied`), so use
  the first form there.

It replaces copying the binary by hand. Each script installs the release it
pins (`RELEASE=` or `$release`), which is the release of the tag it is
fetched from:
- it downloads `aicrew-agent` and `aicrew` for the platform, and refuses
  them unless each one's SHA-256 is listed in the release's `SHA256SUMS`.
  Both are checked before either is installed, so a mismatch installs
  neither;
- it installs them as `~/.local/bin/aicrew-agent` and `~/.local/bin/aicrew`
  on Linux and macOS, or as `aicrew-agent.exe` and `aicrew.exe` in
  `%LOCALAPPDATA%\aicrew\bin` on Windows, where the directory is added
  to the user's `PATH` when it is missing;
- a binary that already reports the release is left alone, and the
  script says it is current. An older one is replaced, and both versions
  are printed;
- on Windows a running binary (a launcher or a Stop hook) keeps its file:
  the old file is renamed aside to `<name>.exe.old-<UTC time>`, and a
  later run removes it.

`aicrew-agent check` reports the `aicrew` beside it (`operator_cli` in
`--json`): the same release, another release, or none, with a notice for
the last two. A member does not need `aicrew`, so it never blocks.

The one-liner installs the binaries and nothing else. It creates no agent
home, does not join, touches no client settings, and writes no file outside
the bin directory, whatever directory it runs from. `join` writes the
home's Stop hook with the absolute path of the `aicrew-agent` that ran it.
So after a first install, or after moving from a copied binary, run
`aicrew-agent join --home HOME` again for each home, so its hook names the
installed binary. Upgrades keep the path, and the hooks keep working.

**The members' services: stop, swap, start.** When the one-liner replaces
`aicrew-agent`, the new binary, still in the temporary directory after its
checksum passed, runs `aicrew-agent service stop --all` before the swap, and
the installed one runs `aicrew-agent service start --all` after it, which
also rewrites each definition with the installed path. The script prints
the services it stopped. A machine with no service, a release that is
already current, or `AICREW_NO_SERVICE=1` leaves them alone. A swap that
fails starts them again on the binary that stayed.

Knobs:
- `AICREW_BIN_DIR`: another bin directory, for example to upgrade a binary
  where it already is;
- `AICREW_VERSION`: another release than the pinned one;
- `AICREW_REPO`: a fork;
- `AICREW_NO_SERVICE=1`: leave the members' services alone;
- `AICREW_PREBUILT_DIR` (`boot.sh` only): a directory holding
  `aicrew-agent` and `aicrew` to install instead of downloading. Nothing is
  downloaded, and curl is not needed. A `SHA256SUMS` in the directory, when
  present, is checked as a release's is, by the binary's name or its
  release asset name; without one the binaries are installed as they are.
  An installed binary that is the same file (by hash) is current; a
  prebuilt binary is never run before its checksum is checked.

`internal/installer` checks both pins and the documented one-liners against
the CHANGELOG, and runs both scripts' install step against a local release:
a fresh install of both binaries, the same release, a refused hash for
either binary (installing neither), and an upgrade while the old agent is
running. The release check refuses a tag either script does
not pin.

For development, build it from source:

```sh
CGO_ENABLED=0 go build -o bin/aicrew-agent ./cmd/aicrew-agent
bin/aicrew-agent run --client claude --home ~/aicrew/agents/builder
bin/aicrew-agent session start  --home ~/aicrew/agents/builder   # the session only, no client
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
- **The client's PATH.** `run`, each turn of `serve`, and every `check`
  probe put the directory of the running `aicrew-agent` first on the
  client's `PATH`, ahead of the inherited entries, which stay as they were.
  The guidance has a member run `aicrew-agent` by name, and the user's
  `PATH` need not hold the installed one, or may hold another. A `PATH`
  that already starts with that directory is left alone. On Windows the
  variable keeps its name (`Path` or `PATH`). The user's shell and profile
  do not change.
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
- The invitation code is read at a hidden prompt on a terminal, never from
  an argument or the environment. A headless install (a container, a
  service account) gives it with `--code-file PATH`, a file readable by its
  owner only, checked as the token files are, or `--code-file -`, one line
  on standard input when that is not a terminal (the prompt when it is).
  Off a terminal and without `--code-file`, the command stops with
  `no_terminal`, naming both.
  - `--code-file` links a new home: on a linked home it is a usage error. A
    value that is not a file is refused without being quoted, since it may
    be the code itself.
  - Off a terminal, `--code-file -` and a `--cred HOST=-` would both read
    standard input, so the pair is refused. `--aimem-token-file -`, a hidden
    prompt, is refused there too.
  - A code with a typing error (its checksum) is caught before it costs an
    invitation attempt. At the prompt it is asked for again, at most three
    prompts in all. A code from a file or a pipe is refused at once
    (`code_malformed`), leaving the home as three failed prompts do: fix the
    code, then rerun.
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
  member's start command ("Starting a member").
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
- It writes the managed `.claude/settings.json`, under the same digest
  rule as the guidance files:
  - `env`: the home's `AIMEM_STATE_DIR` and `AIMEM_SOCKET`, absolute;
  - the Stop hook;
  - the command guard, a PreToolUse hook running `aicrew-agent guard --home H`
    before every Bash and PowerShell call, unless the home's `agent.json`
    turns it off (WORKSPACE.md, "The command guard");
  - the `permissions.deny` list, the guard's second line (WORKSPACE.md,
    "Deny rules").

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
    clients read. The ai-skills installers write it from 1.28.0 on
    ([aiskills#24](https://github.com/BlackVS/aiskills/issues/24)); an
    installation without it, from an older release, is "unknown";
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
      that name another installation, reported by name only. aimem's
      installer-only knobs are not reported: aimem itself never reads them,
      so a member process that inherits one acts on nothing. The list is
      `aimemInstallerKnobs` in `internal/agent/check_installation.go`:
      `AIMEM_BIN_DIR`, `AIMEM_CLAUDE_SETTINGS`, `AIMEM_CODEX_HOME`,
      `AIMEM_NO_SYSTEMD`, `AIMEM_OC_PLUGIN_DIR`, `AIMEM_PREBUILT`,
      `AIMEM_REINSTALL`, `AIMEM_REPO`, `AIMEM_TARGET_VERSION`,
      `AIMEM_UNIT_DIR`, `AIMEM_UPGRADE_WAIT`, `AIMEM_USER_ONLY` and
      `AIMEM_VERSION`, as aimem v0.10.0's installers read them. Its hub
      variables (`AIMEM_HUB_URL`, `AIMEM_HUB_TOKEN`, `AIMEM_GROUPS`) are
      still reported;
    - an aimem MCP server at user scope, or at local scope for the home, in
      Claude Code's `.claude.json`;
    - a socket path over the Unix limit;
    - a `.claude/settings.json` that lacks a managed deny rule.
- **The client wiring.** One MCP entry per selected client, inside the
  home only: `.mcp.json` `mcpServers.aimem` for Claude Code, and
  `opencode.json` `mcp.aimem` for OpenCode, both running
  `<aimem_command> mcp`. Claude Code's entry carries the home's
  `AIMEM_STATE_DIR` and `AIMEM_SOCKET` in its `env`.
  - The entry is managed like the guidance files: added when missing,
    updated only while it matches its recorded digest, and otherwise left
    alone with the proposed file written as `<file>.aicrew-new`.
  - Other keys in those files are kept.
  - Claude Code's `.claude/settings.json` carries two hooks: the Stop hook
    that wakes the member (except in turns mode) and the PreToolUse command
    guard before every Bash and PowerShell call. No user-level client
    configuration is written.
- **What the client sees.** Each selected client is asked whether aimem's
  MCP server runs in the home and whether the required skills are
  visible. There is no model call: the model endpoint is a local port that
  closes every connection, and the key is a dummy.
  - Claude Code: the print-mode init event and `claude mcp list`. The run
    is ended as soon as its init event is read: against the closed model
    endpoint it would retry its request and never end by itself. It gets
    a SIGTERM, so it closes its MCP servers (a kill on Windows, where the
    servers end with their closed input), and is killed 2 s later if it
    has not exited.
  - OpenCode 1.x: `mcp list` and `debug skill`.
  - OpenCode 2.x: its own `serve`, read through `/api/mcp` and `/api/skill`.

  Each step is bounded (90 s). The check first says that it starts the
  client, which can take a minute or two on a cold start, then prints one
  line per step when it ends: its elapsed time and what it found, or that
  it stopped at its ceiling (the human output only, never `--json`). Print
  mode never shows Claude Code's
  workspace-trust dialog. A project server Claude Code has not yet approved
  for interactive use is reported as a notice: the first interactive start
  in the home asks to trust the folder and to approve it.
- **The turn model,** in a `turns` home whose client is Claude Code. The
  home's model (`turns.model`, an alias such as `haiku` or a full ID, or
  the client's default when empty) resolves through the member's own Claude
  Code configuration and its provider or gateway, so only a call shows
  that it is served. After discovery, check makes one minimal model call:
  `claude -p "Reply with OK."` with the same text as its system prompt, no
  tool (`--tools ""`), no MCP server (`--strict-mcp-config`), one turn, no
  session kept, `--max-budget-usd 0.05` and its own 60 s ceiling. Unlike
  discovery, it runs in the member's own environment, since it is a real
  call; a served model costs about $0.002 and answers in about two seconds.
  The report's `turn_model` (`turn model:` in the human output) names the
  configured model, the model the client resolved it to and the state:
  - `available`;
  - `unavailable`: Claude Code answered that its provider does not serve the
    model (its own `model_not_found` message and a 404 result, at no cost).
    The check blocks with `turn_model_unavailable` and the instruction to
    set `turns.model` to a model the provider serves;
  - `unknown`: a refused login, a limit, the ceiling or another failure, as
    a notice; the first turn reports it in its own way.
- **The instructions of a blocked report** are exact and pinned:
  - aimem's verifying boot script with `AIMEM_VERSION`;
  - the ai-skills release archive checked against its `SHA256SUMS` and
    installed with `bash ./src/install.sh --user -t claude -s <skills>`
    (OpenCode reads the same directory), because the ai-skills one-line
    boot does not verify its download yet
    ([aiskills#25](https://github.com/BlackVS/aiskills/issues/25)). The
    installer runs through `bash` because an archive may store it without
    its execute bit, as 1.33.13 does;
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
| `/crew-review <attempt>` | coordinator | reviews a submission against the frozen scope at level high, posts and labels with `aicrew-agent forge pr`, then the review step; stops before the human merge |
| `/crew-escalate <task>` | coordinator | checks the question is one to escalate, raises it with `aicrew-agent escalate`, mirrors it as an `[escalation.request ID]` task comment, then carries on |
| `/crew-accept <attempt>` | worker | checks the offer's pin and digest, accepts, clones |
| `/crew-submit <attempt>` | worker | opens the pull request with `aicrew-agent forge pr open`, takes it through the gates, submits its URL |
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
- **Claude Code's block cap.** Claude Code ends a turn whose stop a Stop
  hook has blocked 9 times in a row, which ended the keep-alive in the
  pilot. `run` therefore sets `CLAUDE_CODE_STOP_HOOK_BLOCK_CAP=100000` in
  its client's environment, unless that holds a larger number already. The
  keep-alive stays bounded by `idle_hours`. This is interim: a home under
  the turn loop (`serve`) has no Stop hook.
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

### Wake modes: the Stop hook or the turn loop

A home is woken in one of two ways, recorded in `agent.json` as
`wake.mode` (`docs/DESIGN-CONTROL-PLANE.md`, 1.10, D3):
- **`stop-hook`** (the default): an interactive client under
  `aicrew-agent run`, woken by the Stop hook above.
- **`turns`**: the turn loop, `aicrew-agent serve`, starts each headless
  turn when a message waits, so the home has no Stop hook and no
  keep-alive turns.

`aicrew-agent join --home H --wake turns` (or `--wake stop-hook`) records
the mode, keeping the `wake` object's other settings, and then rewrites the
managed files from it:
- **`.claude/settings.json`** holds the Stop hook only in `stop-hook` mode.
  The command guard (unless turned off) and the deny rules are there in
  both modes.
- **`.claude/turn-settings.json`** (managed, in both modes) is the settings
  a headless turn runs under. The turn loop passes it with `--settings`,
  because a home's own allow rules do not apply to a headless turn
  (`docs/TURN-PROBE.md`). A coordinator's turn gets
  **`.claude/turn-settings-coordinator.json`** instead, chosen by the
  session's role. In a home not rejoined since the upgrade that file is
  missing, and the coordinator's turn runs with the common file, which
  admits less, while the loop logs that `join` should be rerun. Both files
  hold the deny rules and an allow list:
  - the base list: `Read(./**)`, `Grep(./**)` and `Glob(./**)` in the home
    and its worktrees, `Edit(./worktrees/**)`, `Edit(./docs/HANDOFF.md)`,
    `Bash(git *)` and `aicrew-agent` (by name and by the path of the
    `aicrew-agent` that wrote the file);
  - aimem's read tools, since in `dontAsk` mode an MCP tool without a rule
    is denied (pilot finding #15): `get_task`, `list_tasks`,
    `get_task_comment`, `list_task_comments`, `get_task_history`,
    `get_epic`, `list_epics`, `list_docs`, `read_doc`, `get_design_doc`,
    `process_context`, `project_repository` and `writing_rule`, each as
    `mcp__aimem__<tool>`;
  - in the coordinator's file only, its own aimem work: `add_task_comment`
    (an escalation's mirror), `triage_task`, and the reads `/crew-offer`
    names, `Bash(aimem process show*)` and `Bash(aimem project show*)`.
    A worker's turn is denied triage;
  - the home's additions, `agent.json`'s `turns.allow`, for the project's
    build and test commands, for example
    `{"turns": {"allow": ["Bash(go test *)", "Bash(go build *)"]}}`. An
    addition must be a Bash, Edit, Read or WebFetch rule with a pattern that
    names something. A whole tool (`Bash`, `Bash(*)`, `Edit(/**)`) or a
    PowerShell rule is left out, since PowerShell's allow rules admit
    nothing narrower than the whole tool.
  - **A worker's toolchain.** When the operator selected a toolchain for
    any of the team's projects (`aicrew team toolchain`), a worker's turn
    runs under **`state/turn-settings-worker.json`** instead: the common
    file's rules and the selected lists', in that order, with the same deny
    rules. The turn loop writes it before each of the worker's turns from
    `state/team.json`, which the launcher records at the session's start
    (WORKSPACE, "Initial guidance"). It is not a managed file. A worker
    whose team selects none, or only names this `aicrew-agent` does not
    know (logged, asking for an upgrade), runs with the common file, as
    before. The named lists are aicrew's own (`internal/toolchain`), never
    edited in a home, so another project can carry another list later.
    `dev-go` admits, run in the worktree as `cd worktrees/NAME && COMMAND`,
    the one compound form the managed `/crew-turn` command allows (the
    command guard passes it too):
    `go test`, `go vet`, `go build`, `go mod tidy`, `go mod download`,
    `gofmt`, and the repository's scripts, `bash scripts/*.sh` (or
    `./scripts/`), each alone or with arguments. `go -C DIR` and `go run`
    are not admitted. The coordinator's file never carries a toolchain.
    These commands run the repository's code; the deny rules still stop
    the texts they name (`docs/TURN-PROBE.md`), and isolation stays the
    OS account (WORKSPACE, "Its limit").

  Its path rules are anchored at the home with `./`, the turn's working
  directory: a file passed with `--settings` anchors a leading `/` at its
  own directory, `.claude/`, unlike the home's `.claude/settings.json`,
  which anchors it at the home (measured with Claude Code 2.1.296). A
  `turns.allow` rule written with `/` is rewritten to `./`, and so are the
  copied deny rules. The commands the client judges read-only run without a
  rule, and a deny rule beats every allow.
- **`/crew-turn`**, a managed command, is the prompt each turn starts with.
  Its argument is the turn brief the launcher writes ("The turn loop"
  below), and it teaches the turn's order: the handoff, the work within the
  role from the brief and the ROLES.md sections it names (no managed doc is
  read in full), the acknowledgement, the handoff, and the end of the turn,
  without ever waiting inside it. It also carries the repetition rule: a
  procedure worked out again is proposed, never automated in the turn.

`check` notes a Stop hook left in a `turns` home, and each `turns.allow`
addition it leaves out.

### The turn loop: `aicrew-agent serve`

```sh
aicrew-agent join  --home H --wake turns
aicrew-agent serve --home H [--log FILE]
```

`join` installs the home's service, which runs `serve` ("The member's
service" above); `serve` in a terminal is for a home without one. One serve
runs per home: a second is refused while the first holds
`state/serve.lock`. `--log` appends the log to a file as well, as the
service does to `logs/serve.log`.

`serve` runs a home in `turns` mode without a terminal
(`docs/DESIGN-CONTROL-PLANE.md`, 1.3 to 1.10, increment A3). It holds the
member's team session as `run` does: the proof, the entry or resume, the
handle refresh and the step socket. Instead of one interactive client, it
runs one headless Claude Code turn at a time. An interrupt or SIGTERM ends
the loop: a running turn is stopped and recorded, and the session is left
as `run` leaves it. `serve` refuses a home in `stop-hook` mode, and `run`
refuses one in `turns` mode, since its client would have no Stop hook to
wake it.

Before it takes the session, `serve` probes the turn model with check's
call ("Checking dependencies and clients", the turn model). A model the
provider does not serve refuses the start with the instruction to set
`turns.model`, and `serve` exits 2, so a service does not restart into it.
A served model is logged with the model it resolved to; a probe that shows
neither is logged as a warning, and the loop starts.

**When a turn starts.** Every 3 s the loop reads the member's pending
messages and its session status. A turn starts when all of these hold:
- the team and the member are not paused;
- no limit holds the member (below);
- one of:
  - **a message:** the member has unacknowledged messages;
  - **a continuation:** the member is a worker whose attempt is still in
    work (`running`, phase `working` or `rework`) after its last turn, which
    therefore ended without a block or a submit. The session status names
    the member's open attempt for this.

At most one turn starts per 5 s, so a burst of messages gives one turn.
Messages that arrive during a turn wait for the next one.

**The turn.**
- **The command:** `claude -p --output-format stream-json --verbose
  --permission-mode dontAsk --permission-prompts none --settings
  .claude/turn-settings.json` (the coordinator's file for a coordinator),
  with `--session-id`, `--model` and `--max-budget-usd`. The prompt,
  `/crew-turn` followed by the brief, goes on standard input, which is then
  closed (the brief runs to many lines, beyond a command line's length), and
  the turn runs in the home with the environment `run` gives its client.
  No dangerous flag, permission bypass or fallback model is ever passed.
- **Conversations:** every turn is a fresh conversation
  (`docs/DESIGN-TURNS.md`, section 3, decision T1, which amends D4). Nothing
  of one turn's conversation reaches the next but the handoff, the brief
  and the repository, so a hold between turns rewrites no conversation's
  cache, and a turn's cost depends on its work, not on the turns before it.
- **The turn brief** is what the launcher writes into the prompt, in this
  order, so the model spends no tool calls finding its bearings:
  1. the first line: the member's attempt as aicrewd's session status gives
     it (`Attempt ID: state/phase.`), so a turn on a cancelled or stopped
     attempt knows at once, or `No open attempt.`;
  2. who and where: role, team, agent, home, the attempt's task, branch,
     base commit, worktree, repository and process pin;
  3. the frozen task: title, objective, acceptance criteria and non-goals
     from the attempt's **task snapshot**, `state/attempts/<attempt>/task.json`,
     which the launcher writes from the board at the attempt's first turn
     (through the member's aimem connection) and carries unchanged by every
     later turn; the snapshot holds the board revision and the read time,
     and the brief's task header prints them and says whether this is the
     first read or the snapshot, so a reader can tell it from the live task.
     The board is read again only when the snapshot is missing. A cancel or
     a stop reaches the turn through the first line, not through the board.
     The snapshot is bounded to 8 KiB and is launcher data, kept after the
     attempt;
  4. why this turn: the trigger (the messages' kinds and ids, or the
     continued attempt), then the pending messages read from the inbox,
     which counts as a delivery (below): each with its id to acknowledge, kind,
     sender, attempt, task and text, and the content its kind carries: an
     `action.needed` message's subjects with their needs, the pull request's
     facts, the review's verdict and body, the failed checks' excerpts, the
     passed checks, a block, a conflict, a pre-push failure; a forge
     subject's pull request; an answered escalation; the operator's note;
     a board change. At most 10 messages; the rest are counted and left to
     `aicrew-agent inbox`;
  5. a coordinator's team state: the team's open attempts as aicrewd lists
     them now (`GET /v1/crew/attempts`), each with its next action and,
     for a running one, when its handoff was last pushed or that none was, at
     most 8 KiB of them, with a line counting those left out and naming
     `aicrew-agent attempts`;
  6. the pull requests as the launcher's forge watch last observed them:
     state, head, labels, checks, comments, the review at the head, the
     failed checks; or that none was observed yet;
  7. a worker's handoff: the CURRENT ATTEMPT section of its session doc
     (below), at most 8 KiB, and the attempt's ledger (below), oldest
     first; a coordinator's handoff: its whole `docs/HANDOFF.md`, at most
     8 KiB of it, the start kept and the end cut with a line naming how
     many bytes were cut (a coordinator has no attempt, so the launcher
     keeps no section of its file, which stays whole in the home);
  8. where to fetch more: the inbox, the session status, `forge pr show`,
     the board, and the ROLES.md sections for the turn's kind.

  **The turn's kind** decides the ROLES.md sections the brief names, so a
  turn reads only those, never the whole file (the lean start, increment
  3d): `coordinator`; `worker-offer`, a worker without a running attempt;
  `worker-review`, a running attempt whose messages carry a review of its
  pull request in an `action.needed` subject, or whose phase is `rework`
  after the coordinator's decision; `worker-attempt`, any other turn of a
  running attempt, a continuation among them; `independent`, an
  independent member without an attempt, which takes the worker's kinds
  with one. Every kind names "Every member" and "Writing that others read";
  an independent member's kinds also name its own section.

  Every text from outside the launcher is redacted of the home's forge
  tokens and the known token shapes; a message's or a review's text is cut
  at 1 KiB and the whole brief at 24 KiB, which says so. The handoff and
  the ledger take the room the other parts leave, so the bound cuts them
  first: the ledger's oldest lines, then the section, each cut logged. The
  last resort cuts the body before the last part, which always stays
  whole, since it names the turn's ROLES.md sections.
- **The trigger** in the turn's record names the messages' kinds and ids,
  or the continued attempt, never a message's text.
- **The brief's messages and the delivery count.** The brief's inbox read
  is the member's own read, so aicrewd counts each message it carries
  delivered once more. A message's delivery count therefore means "handed
  to a turn's brief or read by the member", not "acted on": a turn that
  fails, is stopped, hits a limit or is interrupted after the brief never
  acts on its messages, which stay unacknowledged and reach the next turn.
  The turn's record is the place to see what a turn was given:
  `brief_messages` names the ids the brief carried (a continuation carries
  none), and `brief_messages_unacted` is true when the turn's outcome is
  not `finished`. The running turn names them before the brief is handed
  over, so a turn found running at a restart names them too. The record
  states a fact about the turn, not an audit of each message: a message a
  finished turn left unacknowledged is still pending in the inbox.
- **Outcomes:**
  - `finished`: a normal `result` event;
  - `failed`: an error result, or an exit without a result;
  - `needs_login`: a refused key, or the client's first authentication
    retry, which ends the turn at once;
  - `limit_reached`: the turn's budget, or the provider's limit;
  - `model_unavailable`: the provider does not serve the turn's model
    (Claude Code's `model_not_found`, or a 404 result). The error names
    `turns.model`, and the member is held as for `needs_login`: an hour,
    doubling up to 8 hours. It stays in the launcher: a login or a
    provider limit reaches aicrewd as the member's hold, this does not;
  - `stopped`: the turn's time limit, or the loop's stop;
  - `interrupted`: a turn the loop found running when it started again.

**Limits,** from `agent.json`'s `turns` object until the provider profile
(A5) exists, with the design's defaults:

```json
{"turns": {"turn_timeout": "30m", "turn_budget_usd": 2, "attempt_budget_usd": 20,
           "daily_budget_usd": 50, "max_continuations": 20, "model": "",
           "allow": ["Bash(go test *)"]}}
```

- A spent daily budget gives no turn until the next UTC day. A spent
  attempt budget gives no turn while that attempt is open, and is checked
  again each hour, for an operator who raised it.
- **A hold from the old counting.** Before 0.5.9, the loop added each
  resumed turn's running total, so an attempt's and a day's spend in
  `state/turns.json` could be several times the real cost. Upgrading does
  not rewrite those sums. The real cost is in the turn records: the
  difference between consecutive `cost_usd` values of one conversation.
  To clear such a hold, use the knobs, not an edit of the state:
  - an attempt hold: raise `attempt_budget_usd` above the recorded sum.
    The hold is checked again within the hour, and the next attempt
    starts at zero.
  - a daily hold: it clears at the next UTC day.
- `max_continuations` bounds one attempt's continuation turns. A message
  still gets its turn.
- A provider limit holds the member until the reset time the client
  reported. Without one, and after a login failure, the hold is 1 hour,
  then 2, 4 and 8. A failed turn holds it for 1 minute, doubling up to 30.
  A finished turn clears the hold.
- **The stall:** three turns of an attempt in a row with no step and no new
  commit in `worktrees/<attempt>` stall it. The loop then blocks the
  attempt through the `work` step. The block's message tells the
  coordinator, which decides what the stall means, as it decides any block.
  The attempt gets no continuation turns until a message arrives.
- **The handoff's stall:** two finished turns of an attempt in a row that
  make no step and leave the CURRENT ATTEMPT section of `docs/HANDOFF.md`
  unchanged stall it the same way, even when they commit. A step or a
  change of the section resets the count.

**The session doc** (`docs/DESIGN-TURNS.md`, section 3, increment 3c). A
worker's `docs/HANDOFF.md` is the memory a fresh turn continues from; the
home is a cache, and the member's canonical copy is on its attempt in
aicrewd.
- **Its parts.** Everything before the two launcher sections is the
  agent's header. Before a worker's turn the launcher opens a
  `## CURRENT ATTEMPT` section for the open attempt, with a first line
  that names the attempt and the task and the four parts Done, Decisions,
  Next and Open questions, which the model keeps current. A section the
  model rewrote without that first line is still the open attempt's: the
  launcher records the section's attempt in `state/turns.json` before the
  turn runs (so a launcher stopped during it keeps the record), puts a
  missing line back, and logs a line that names another attempt. A
  section of an
  attempt that is no longer open moves to the `## Archive`, newest first,
  its headings a level down; the archive keeps the last 10 attempts, at
  most 16 KiB. A section over 8 KiB is logged and cut in the brief.
- **The ledger**, `state/attempts/<attempt>/ledger.jsonl`, is launcher
  data: one JSON line per turn of the attempt with the turn ID, start,
  outcome, cost, head before and after, steps, the summary's first
  sentence (redacted) and the `handoff not updated` mark, at most 50 lines.
- **The sync.** After a worker's turn of an open attempt the launcher
  pushes the doc to the attempt's handoff route (CREW-CONTRACT.md, "The
  attempt's handoff") when it changed since the last push: redacted, and
  cut to aicrewd's 32 KiB doc bound and, JSON-encoded, within the server's
  64 KiB body bound, the archive's oldest entries first. A refused push (a
  stop requested, the attempt no longer running) leaves the home's copy as
  it is and is logged. A home whose doc has no section for the open
  attempt (a new home, whose doc is join's placeholder, or none) reads the
  attempt's copy before the turn and takes it: the whole doc over the
  placeholder or a missing file, only its section under a header of the
  home's own, with the attempt's archive when the home's doc has none (an
  older join's placeholder is such a header). When that read fails, the turn runs without the doc and
  neither counts towards the stall nor pushes, so a new section never
  replaces the attempt's copy.
- **Retention.** A new attempt's directory under `state/attempts/` (its
  task snapshot and ledger) removes those of other attempts beyond the
  newest 20, by modification time; the open attempt's is never removed.

**Records.**
- **Each turn** is one line of `logs/turns.jsonl`: its trigger, client,
  model, conversation, times, outcome, usage, cost and cost basis, the names
  of the tools denied, the steps it ran, and the tail of its result text.
  The text and the error are redacted of the home's forge tokens and of
  aicrew's, aimem's, forge and provider token shapes. Records hold no
  secret and no transcript.
- **What a record adds for cost** (`docs/DESIGN-TURNS.md`, increment 0):
  - **The worker type:** `role` and `client`.
  - **The model** the turn used.
  - **Tokens by kind:** input, output, cache read and cache write, with
    the cache write split into `cache_write_5m_tokens` and
    `cache_write_1h_tokens`.
  - **The wall time:** `duration_ms`.
- **A turn's cost is its own, derived from its tokens.** Claude Code's
  `total_cost_usd` is the conversation's total, which is the turn's own
  since every turn is a fresh conversation.
  - **Price list:** when the turn used one model that the price list in
    `internal/agent/pricing.go` prices, `cost_usd` is the turn's tokens at
    those prices. The record then has `cost_source: price_list` and the
    list's version in `price_list`.
  - **The client's total:** otherwise, `cost_usd` is the client's total
    for the turn's conversation, and the record has
    `cost_source: client_total`. In records of releases that resumed
    conversations (before increment 3b of DESIGN-TURNS) it is the
    difference of the running total since the conversation's previous
    turn.
  - **Unpriced:** records of those releases carry `cost_source: unpriced`
    with a cost of 0 for a turn that resumed a conversation whose previous
    total was not kept. No turn is unpriced now.
  - **The client's figure** is kept, never summed, as `client_total_usd`.
  - **The sums:** the attempt's and the day's spend add up the turns' own
    costs.
  - **Older records:** records written before this version carry the
    running total in `cost_usd`, without a `cost_source`. They are not
    rewritten.
- **The raw stream** of each turn is kept in `logs/turns/<turn id>.jsonl`,
  owner-only. It is the home's own log of the turn and never leaves the
  host (D7).
- **The loop's state** is `state/turns.json`, nonsecret: each attempt's
  turns, cost and progress, the cost per day, and any hold. The costs are
  a cache of the totals aicrewd keeps: each turn's cost is reported to
  aicrewd after the turn, a report that does not reach it is kept (the
  last 50) and sent again before the next turn, and a home with no figure
  for the day or the attempt reads aicrewd's totals before the budgets are
  checked (CREW-CONTRACT.md, "The turns' costs"). The attempt's
  task snapshot is `state/attempts/<attempt>/task.json` ("The turn brief"
  above).

**While idle,** the loop also reads the watched pull requests every 60 s
("Forge events").

**The smoke** with real Claude Code is
`go test -tags realclaude -run TestRealClaudeTurns ./internal/agent`, with
a logged-in `claude` on PATH. It uses a disposable home and aicrewd in the
test. A message gives one turn that reads the inbox and acknowledges it; a
second gives another turn, a fresh conversation; idle ticks give no turn. `AICREW_SMOKE_KEEP=DIR` keeps the turns' logs.

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
aicrew-agent step offer --repository https://github.com/team/app --process --body - < offer.json
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

`--process`, on `offer` only, fills the body's `process` pin (`repo`, the
full `commit`, `manifest`) and its `instruction_digest` from the hub's
current process selection for the body's `task.project_id`. It runs
`aimem process pin -p PROJECT --json` from the home's root with the home's
aimem installation, so the coordinator needs no clone of the process
repository and no route to it: the hub checks its stored set against the
selection and hashes the manifest. The client's team session
(`AIMEM_TEAM_SESSION`) is kept, so a member reads under its team's grant;
without one the home's own credential reads. It needs aimem 0.10.18 or
later, the first that reads a pin under a team session. It prints the pin
it filled on stderr. A body that names both `process` and
`instruction_digest` is kept as it is; one that names only one of them is
refused, since the hub's digest need not be its pin's. A project with no
selection, a selection without a stored set, or a refused credential ends
the step with aimem's reason, and nothing is sent.

- **Operations.** The reservation steps `offer`, `claim`, `accept`,
  `withdraw`, `work`, `release` and `finalize` take the begin route's body
  (`docs/CREW-CONTRACT.md`, "Attempt steps"). `offer` and `claim` create
  their attempt and name the task in the body. The others need `--attempt`
  and the aimem task's `--task`. The local steps `decline`, `review`,
  `answer`, `stop`, `confirm-stop` and `confirm-delivery` need only
  `--attempt` and their body.
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
  token. The repository, base and branch are given as flags, and the member
  takes them from its offer: the attempt carries its repository, base commit
  and branch (01a105fa-337e). `clone` reads nothing from the attempt
  itself.

### Pull requests with the member's credential: `aicrew-agent forge pr`

```sh
aicrew-agent forge pr open --title "Fix the parser" --body-file pr.md --draft
aicrew-agent forge pr show --pr 12
aicrew-agent forge pr label add --pr 12 review-this
aicrew-agent forge pr label remove --pr 12 hands-reviewed
aicrew-agent forge pr comment --pr 12 --body-file review.md
aicrew-agent forge pr ready --pr 12
```

A member pushes its branch with the clone's credential helper. It opens,
reads, labels, comments on and marks ready the pull request with the same
credential, through these commands. Before them, no product command used that
credential for a pull request, and a worker that had pushed its branch
correctly blocked: `gh` holds another account, and handling the token by
hand is forbidden. ROLES.md, `/crew-submit` and `/crew-review` use these
commands and say never to use `gh`, `tea`, `glab` or a forge's API with a
credential.

- **The repository** is the current directory's `origin`, so the commands
  run in the attempt's worktree. `--repository URL` names another. The
  credential is the home's own for the repository's host, read through
  `agent.json` as the credential helper reads it.
- **The token** is read in the command's own process. It never appears in
  an argument, the output or an error. As for every forge call, a redirect
  to another host is refused.
- **A credential is never posted.** A body file in the home's `creds/` is
  refused unread, whether named directly or through a link. Every input
  that holds the host's token is refused as soon as the token is read,
  before an error could quote it or a request send it. That covers the
  repository's path, the title, body, head and base, the labels, and the
  comment. Everything the command prints passes through a filter that
  replaces each of the home's recorded forge tokens. This covers an input
  quoted before any check, such as a body file's name or a host without a
  credential.
- **Each command prints** the pull request's number and URL first. Then it
  prints the title, the state (open, closed or merged, and draft), the head
  branch and commit, the base, the merge commit once merged, the labels,
  a new comment's URL, and `watch:`, whether the launcher now watches the
  pull request for forge events ("Forge events", above) or why not.
  `--json` prints the same as one object.
- **`show` prints the content** as well (`docs/DESIGN-TURNS.md`, 4.1), so a
  member never reads the forge another way to learn why it was woken:
  - **the checks** of the head commit: each GitHub check run (every page)
    and commit status, Gitea commit status or GitLab job of the head
    pipeline, with its state and the forge's own conclusion;
  - **a failed job's log excerpt**, at most 8 KiB, from ten lines before the
    first line that reads as a failure (`--- FAIL`, `FAIL`, `panic:`,
    `error:`), else the runner's `##[error]`, else the log's end, with
    timestamps and terminal escapes removed. GitHub Actions jobs and GitLab
    jobs offer their logs; GitHub's redirect to a signed address is
    followed without the token, which never leaves the forge's host. A
    commit status, Gitea's included, another app's check run, a refused log
    and failed jobs past the fifth say why there is no excerpt;
  - **the comments**, oldest first, with their author, time and body (at
    most 32 KiB each): conversation comments, submitted reviews with their
    state, and inline comments with their file and line, at most ten pages
    of each list. GitLab's system notes and Gitea's review requests are
    left out. A body whose first line is a review header
    (`[oh-code-review (LEVEL) review] reviewed at head SHA` or
    `[hands-bot review] reviewed at head SHA`) is marked as the local or the
    external review, and whether it names the head.

  Every body and excerpt is printed indented under its heading.
- **`open`.** The head is the checkout's branch unless `--head` names one,
  and the base is the repository's default branch unless `--base` names
  one. `--draft` sets GitHub's draft flag. Gitea and GitLab take no draft
  flag on create, so there it prefixes the title with `WIP: ` (Gitea) or
  `Draft: ` (GitLab), unless the title already has one. A forge's refusal,
  such as a pull request that already exists for the head, is reported
  with the forge's own message.
- **`label`** adds or removes labels the repository, or on Gitea and GitLab
  its organization or group, already defines. It never creates a label:
  GitHub and GitLab would create an unknown one, so each is read first, and
  an unknown label stops the command before anything changes. Removing a
  label the pull request does not carry changes nothing.
- **`comment`** posts the file's text, or standard input's for `-`. A body
  or comment is UTF-8 of at most 256 KiB.
- **`ready`** marks a draft ready once the gates are green. On GitHub it
  drops the draft flag through GraphQL's `markPullRequestReadyForReview`,
  since the REST API cannot. In every dialect it drops the draft prefixes
  of the title, in any case: `WIP:`, `[WIP]`, `Draft:`, `[Draft]` and
  `(Draft)`. That is how Gitea and GitLab mark a draft, and how a process
  may mark one on GitHub. A pull request that is ready already is not
  written. A closed or merged one is refused, and so is a title that is
  nothing but a draft prefix. The pilot's coordinator had to answer that
  marking ready "needs a person"; it needs none, while the merge stays a
  person's.
- **Not here:** merging, which stays a person's, pull requests across forks,
  and creating labels.

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

### Escalations: `aicrew-agent escalate` and `aicrew escalations`

The coordinator raises a question it cannot settle within its role, through
the launcher, as it takes a step (`docs/CREW-CONTRACT.md`, "Escalations"):

```sh
aicrew-agent escalate --body - < escalation.json
```

The body is the request's JSON: `task`, `category`, `question`, `context`,
two to four `options` with their consequences, `recommendation`, `blocked`
and `urgency`. The exit codes are `step`'s.

Before raising a question, the coordinator lists the open (unanswered)
escalations through the launcher, and never raises one that an open
escalation already asks:

```sh
aicrew-agent escalations [--json]   # GET /v1/crew/escalations, every page
```

- **The coordinator** sees every open escalation of its team, whoever
  raised it.
- **Any other member** sees only those it raised itself.
- **Paging:** the route answers 20 to a page, with `next` naming the
  following page's `after`. The command follows every page.
- **What a page holds:** each item names the escalation, its task,
  category, urgency and question. Its context, options and answer are left
  out, so even a page of the largest questions stays within the launcher's
  256 KiB reply bound.

ROLES.md's "How to escalate" and `/crew-escalate` both start with this
check.

The architect, or the operator, reads and answers:

```sh
bin/aicrew escalations list [--team TEAM] [--open]
bin/aicrew escalations show --id ID
bin/aicrew escalations answer --id ID --decision TEXT --rationale TEXT
```

- **Who can run them:** they take the operator credential or an architect
  credential in the token file. A team is named by its ID, because
  resolving a name reads the team routes, which an architect credential
  does not reach.
- **The ID:** `show` and `answer` take the full ID that `list` prints, or a
  prefix of it, such as the 13-character short form used for tasks and
  attempts. The prefix must be at least 8 characters and start exactly
  one escalation's ID. An ambiguous prefix is refused, and the refusal
  lists the IDs it starts, at most 10.
- **Once only:** an answer is recorded once.
- **Delivery:** the answer reaches the coordinator's inbox, and the blocked
  member's, as a lifecycle message carrying the escalation. The keep-alive
  loop then wakes them as for any message.
- **The task comment:** mirroring the request and the answer on the task,
  as comments headed `[escalation.request ID]` and `[escalation.answer ID]`,
  is the members' and the architect's guidance. A comment authorizes
  nothing.
- **The coordinator's guidance:** the coordinator's section of the managed
  `docs/ROLES.md` and `/crew-escalate` say the following:
  - it does not talk to a human;
  - it always escalates OPERATOR-SEAT's floor (architecture, wire,
    security, risk, merge, deploy), scope, cross_repo, intent, and
    anything it cannot classify;
  - it settles process, implementation, environment (inside the worktree
    only) and retry itself, within the project's process. That is
    OPERATOR-SEAT's `default` mode, since no team policy exists yet;
  - it mirrors each request on the task;
  - it carries on with other work while a request is open, and acts on the
    answer's message;
  - when its handoff claims a pause, it raises one `intent` escalation ("am
    I paused?") and meanwhile proceeds with triage. Every member's rules
    say that a pause written in a handoff has no authority: only the team's
    state on aicrewd pauses (D12, C1).

  A test keeps those categories equal to the ones aicrewd accepts.

### A coordinator's open attempts: `aicrew-agent attempts`

When a coordinator's session starts, under `run` or `session start`, the
launcher reads the team's open attempts from aicrewd (`GET
/v1/crew/attempts`). It records them in the home's
`state/open-attempts.md` before the client starts. `session start` also
prints them on stderr. The first instruction, `/crew-start`, START.md and
the coordinator's section of ROLES.md ("Reconcile first") send the
coordinator to that file before anything else.

Each attempt has its task, worker and state, and a next action:
- **An offer that can no longer be accepted:** withdraw it, then offer the
  task again. That is an offer that is stale (made under an earlier
  coordinator session), expired or declined.
- **A live offer:** wait for the worker.
- **A running attempt:** follow it.
- **An attempt with a pending step:** recover the step.

A failed read is written to the file too, so the file never shows an
earlier session's list as this one's. The current list is read through
the launcher:

```sh
aicrew-agent attempts           # the team's open attempts, one line each
aicrew-agent attempts --json
```

Only the team's coordinator may read it. A worker's start records nothing.

### The team's members: `aicrew-agent members`

An offer names its worker by agent ID (`worker_agent_id`). The coordinator
reads the IDs through the launcher, which lists the team's members from
aicrewd (`GET /v1/crew/capabilities`) with the repositories each member's
last capability report verified:

```sh
aicrew-agent members            # one line each: agent ID, label (role), verified repositories
aicrew-agent members --json
```

The offer step in ROLES.md, `/crew-offer` and the `capability_missing`
refusal of an offer name the command. Any member of the team may read it.

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
  the last tag. That section is the release's notes. Its upgrade steps
  show the Windows member one-liner in both forms, as "Installing and
  upgrading" does: the bare `irm ... | iex` for a PowerShell window that is
  already open, and the `powershell -NoProfile ...` form for cmd or the
  Run dialog.
- The workflow refuses a tag whose section is missing.
- The release-preparation pull request carries the label
  `e2e-real-aimem`, and its `real-aimem` check (workflow
  `e2e (real aimem)`) must be green at its head
  (`docs/E2E-REAL-AIMEM.md`, "In CI"). The label starts the run, and a
  push reruns it.

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
