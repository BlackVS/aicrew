# End-to-end runs against a real aimem

crew-execution b4 runs aicrew end to end against a real aimem on an
isolated local hub. It is a Go test package behind the `realaimem` build tag
(`e2e/realaimem`). It is not part of `go test ./...` or CI, which only vet
it. Every process is a real binary:

- `aimem serve` and the aimem CLI, built from the pinned aimem commit;
- this tree's `aicrewd`, `aicrew` and `aicrew-agent`.

The harness only provisions and observes. Every step goes through
`aicrew-agent step`, through the member's own aimem, aimem's coordination.v1
calls to aicrewd, and aicrewd's read scope.

## Running it

On Linux (the gate; under WSL on a Windows host):

```sh
scripts/e2e-real-aimem.sh -aimem-src ../aimem -runs 3
```

- `-aimem-src` is a local aimem clone that holds the pinned commit. It is
  only read: the commit is archived into the run directory and built there.
- `-runs` is the number of consecutive runs, each on a fresh hub. The
  evidence for a change is three green runs.
- `-out` sets where the reports and logs go (default `.work/e2e`), and
  `-keep` keeps each run directory.

It needs Go 1.25 or later, git, tar, and network access the first time, to
download both modules' dependencies. Windows is best effort and not wired
yet: the harness skips there.

## The pin

`aimemPin` in `e2e/realaimem/harness_test.go` is the aimem commit the
harness builds. It is master after every prerequisite (C5b, C6, C5-w3 and
`aimem hub credential`), the reservation fixture's corrected fences, and
`hub add --ca-file` with token files, because no aimem release carries them
yet.

The build stamps the commit's `git describe` as aimem's version, as aimem's
release build stamps a tag, so `aicrew-agent`'s dependency check reads it as
a development build. Moving the pin to the first release tag is a follow-up.

## What a run sets up

All of it lives in one temporary directory. Every process gets an
environment built from nothing but PATH: its own HOME, USERPROFILE, TMPDIR,
AIMEM_STATE_DIR and AIMEM_SOCKET. The harness refuses any other path.
Every listener is on 127.0.0.1.

1. **TLS.** A throwaway CA, and a leaf for the hub and for aicrewd, for
   127.0.0.1 and localhost. aicrewd pins the hub (`spki_sha256`), and the hub
   pins aicrewd (`--peer-trust-pin`).
2. **The hub, following aimem's pilot runbook:**
   - `tasks on` and `process select`;
   - aicrew registered as the identity peer, with the hub ID read back;
   - the identity.redeem and reservation.read credentials;
   - the team profile, with its grant.
3. **aicrew.**
   - The team, created through the store as its operator. There is no
     operator command for it yet; that is a follow-up.
   - The hub's outbound credential, issued with `aicrew
     introspection-credential issue`.
   - One invitation per member, issued with `aicrew invitation issue`.
4. **Members:** a coordinator, a worker and an independent member.
   - Each is an aimem user with an admin-issued `aimem_user_` token, and has
     its own aimem client state.
   - The member's aimem gets the hub with `aimem hub add … --token-file -
     --ca-file <the run's CA>` and `aimem hub task-token … --token-file -`.
     The token is on standard input, never an argument.
   - The member then runs the real `aicrew-agent join` at a pseudo-terminal,
     where the harness types the invitation code.
   - The client is a stand-in, not Claude Code, so join's dependency and
     client check may block on the client alone. The harness accepts only
     that, once the home is linked, and records it.
   - Each member's launcher (`aicrew-agent run`) then keeps the session
     open, with `/bin/sleep` as its client, and the scenarios drive the
     steps through its step channel.

## Scenarios (b4a)

- **H. The setup above.**
- **S1. An offer whose dependency is DONE:**
  - accepted, submitted, reviewed, its delivery confirmed, and finalized
    DONE;
  - aimem's receipts committed under aicrew's request keys;
  - the result reference noted with the attempt and the member;
  - the terminal evidence ending with the attempt's identity, under aimem's
    evidence-digest check;
  - the dependency evidence in aicrew's audit.
- **S2. An independent claim**, finalized by the reviewing coordinator. The
  identity names the worker.
- **S3. A stop** the worker confirms, released to BLOCKED with a blocker,
  and to READY.
- **S4. Offers never accepted:** withdrawn, declined, and expired.
- **S5. Dependencies.**
  - An open dependency is refused `dependencies_open`, and nothing reaches
    aimem.
  - Once the dependency is DONE, the offer commits.
  - A dependency reopened between the driver's read and the claim: the
    harness's timed aimem holds the claim on a gate. aimem then refuses it,
    and aicrewd settles the offer not committed.

Each scenario asserts both stores: aicrew's attempts, audit and capacity,
read-only; and aimem's tasks, holds and receipts, through admin reads.

## The report

`report-N.jsonl` holds one JSON object per line:

- `run`: the aicrew and aimem commits, the Go version and the OS.
- `step`: one `aicrew-agent step` with its exit and status, and the
  latency of its coordination round trip:
  - `begin_ms` and `settle_ms`, aicrewd's own route durations;
  - `aimem_calls`, each aimem command the member's client ran, timed around
    the CLI (a reservation call includes aimem's coordination call);
  - `coordination_fact_ms`, each coordination.v1 fact aimem asked aicrewd
    for meanwhile.
- `scenario`: PASS or FAIL, its assertions and its duration.
- `summary`: the counts, and the slowest coordination fact and aimem call
  against aimem's 5 s context-age bound.

Each run's process logs (hub, aicrewd and launchers) and the aimem call
timings are copied to `run-N/` beside the report.
