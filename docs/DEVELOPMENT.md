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
- `internal/server`: the service's configuration, TLS listener, request
  bounds, logging and routes, including aimem's session introspection.
- `internal/store`: the aicrew coordination store. It is internal and has
  no MCP surface; only `aicrewd` exposes anything over the network.
  See the package documentation for the rules it enforces.
- `internal/verifier`: the store's production verifier, which redeems
  aimem proof receipts (`docs/CREW-CONTRACT.md`, "Receipt redemption").
  It is not wired into `aicrewd` yet; the first route that completes a
  proof configures it with aimem's https origin, this service's peer ID,
  the TLS trust binding (`ca_dns` with the origin's host, or `spki_sha256`
  with `sha256-` and the base64 SHA-256 of aimem's public key) and the
  redemption bearer file.
- `internal/privatefile`: creates a file for a secret, exclusively and
  readable by its owner only, and checks that an existing secret file is
  private (mode bits on Unix, the effective DACL on Windows).
  `privatefiletest` weakens a file for tests only.

## Running aicrewd

`aicrewd` opens the store and serves HTTPS on one listener, over TLS it
terminates itself (TLS 1.2 or later). It has no plain-HTTP listener and does
not run behind a TLS-terminating proxy. It currently answers only
`GET /healthz`.

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
URL, the service ID and the TLS trust binding of this certificate.

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
