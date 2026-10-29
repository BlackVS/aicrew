# End-to-end runs against a real aimem

crew-execution b4 runs aicrew end to end against a real aimem on an
isolated local hub. It is a Go test package behind the `realaimem` build tag
(`e2e/realaimem`). It is not part of `go test ./...` or the pull-request
CI, which only vet it: the `e2e-real-aimem` workflow runs it ("In CI",
below), and it runs by hand. Every process is a real binary:

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
- `-out` sets where the reports and logs go, absolute or relative to where
  the script runs (default `.work/e2e` in the repository). `-keep` keeps
  each run directory.
- `-skip-faults` runs the skip-the-fault matrix instead (see "Faults").

It needs Go 1.26 or later (aimem v0.10.15 builds with the Go 1.26
toolchain), git, tar, and network access the first time, to download both
modules' dependencies. Windows is best effort and not wired yet: the
harness skips there.

## The pin

`aimemPin` in `e2e/realaimem/harness_test.go` is the aimem commit the
harness builds: the v0.10.15 release (2c6e933), which
`internal/agent/supported.json` names as the tested aimem; the minimum stays
0.10.0, the first release with every prerequisite:
- C5b, C6, C5-w3 and `aimem hub credential`;
- the reservation fixture's corrected fences;
- `hub add --ca-file` with token files;
- team registration and team read (aimem #175), with which aicrewd names
  its own team profiles;
- aimem's own `task_not_ready` refusal (#179), which the coordinator's
  triage answers;
- `aimem identity peer provision`, which registers the peer and writes its
  credentials and the hub's ID (`aimem-hub-id`) into one directory,
  the directory aicrew's operator commands read;
- the control plane's prerequisites (aimem 0.10.0): the board feed
  (`board.read`, whose credential peer provision writes as a fifth file,
  `aimem-board-read.token`), a task's required capability, and team
  members' `report-*` documents. aicrew reads none of them yet.

The build stamps the commit's `git describe` as aimem's version, as aimem's
release build stamps the tag. At the pin that is `v0.10.15`, which
`aicrew-agent`'s dependency check reads as the tested release. The
source clone must hold the commit and the tag (`git fetch --tags`).

## In CI

`.github/workflows/e2e-real-aimem.yml` runs the harness once a night on
main, by hand (`workflow_dispatch`) on any branch, and on a pull request
that carries the label `e2e-real-aimem`. It checks out aimem with its
history and tags beside this repository and runs
`scripts/e2e-real-aimem.sh -aimem-src ../aimem`, with a 45-minute bound; a
run takes about fifteen minutes. The report and the logs are kept as the
run's `e2e-real-aimem` artifact for 14 days. It stays out of the ordinary
pull-request run: it builds aimem and runs for much longer than the
pull-request jobs.

On a pull request, adding the label starts a run, and every later push
while the label is on runs it again. The run shows on the pull request as
the check `real-aimem` of the workflow `e2e (real aimem)`. Adding the label
needs only the right to label a pull request, not to start workflows.
Another label added to a labelled pull request starts no second run.
Any pull request event that does not run the harness, on a pull request
without the label or for another label, shows as a skipped check named
`real-aimem (not requested)`. It never takes the name `real-aimem`, so it
cannot stand in for a real run at the same head.

A release-preparation pull request carries the label and needs the check
green at its head (DEVELOPMENT.md, "Releasing").

## Status at the pin

Every scenario passes at the pin on aicrew commit 9096a07: H, S1–S5, G1,
G2, T1, E1, B1, C1 and F1–F7, in three consecutive runs under WSL with
Go 1.26.4 on 2026-10-26, about 13.5 minutes each. The skip-the-fault
matrix at that commit fails all 19 cases as expected (task
01a17770-ff4a).

## What a run sets up

All of it lives in one temporary directory. Every process gets an
environment built from nothing but PATH: its own HOME, USERPROFILE, TMPDIR,
AIMEM_STATE_DIR and AIMEM_SOCKET. The harness refuses any other path.
Every listener is on 127.0.0.1.

1. **TLS.** A throwaway CA, and a leaf for the hub and for aicrewd, for
   127.0.0.1 and localhost. aicrewd pins the hub (`spki_sha256`), and the hub
   pins aicrewd (`--peer-trust-pin`). A leaf of the same CA serves the
   run's test forge (`forge_test.go`): a Gitea-dialect API on 127.0.0.1 that
   answers who a token is, the project repository's permissions for it and
   its default branch's head, and holds no Git data. The members read it
   under the run's CA (`SSL_CERT_FILE`).
2. **The hub, following aimem's pilot runbook:**
   - `tasks on` and `process select`. The process is a set aimem accepts
     (a version 1 manifest and its handbook), committed to a repository
     the run creates. The selection records
     `https://git.example.test/e2e/process.git`, which offers name; the
     hub host's own `.gitconfig` (in its isolated `HOME`) maps that URL to
     the local repository with `url.<file URL>.insteadOf`, so aimem reads
     the set through git without the network;
   - aicrew provisioned as the identity peer with
     `aimem identity peer provision --output-dir`: the identity.redeem,
     reservation.read, team.register and team.read credentials and the hub's
     ID in one directory. The run fails unless the hub ID there is the one
     the peer list shows.
3. **aicrew.**
   - aicrewd's configuration is built with the operator commands, from
     the 0.2.0 shape an upgraded hub has:
     - it starts with the single `aimem` block;
     - `aicrewd config migrate -cred-dir` moves it into `aimem_hubs`, with
       the hub's ID and the team credential files from the provisioned
       directory, and must report nothing still to supply;
     - `aicrew hub add --cred-dir` then binds the hub again in place, after
       its live team read against the real hub.

     The run checks the result: one `aimem_hubs` entry, the hub's ID, and
     all four files from the directory. aicrewd then starts with its
     operator credential, pointing at the hub through the fault proxy. The
     operator administers it with `aicrew` through the operator API while it
     runs.
   - The team, by `aicrew team setup --hub --project`:
     - the first run registers the team on the hub (team.register) and must
       exit 3, printing exactly one grant command, by the name aicrewd
       registered;
     - the hub's admin runs that command as printed;
     - the second run must find the team already registered and the project
       granted, read live (team.read), and exit 0.

     The team's only project is what aicrewd reads at every offer and
     claim.
   - The hub's outbound credential, issued with `aicrew hub-credential
     issue`.
   - One invitation per member, issued with `aicrew invitation issue`.
4. **Members:** a coordinator, a worker and an independent member.
   - Each is an aimem user with an admin-issued `aimem_user_` token. Its
     aimem client state is the installation in its agent home, `<home>/aimem`,
     which `aicrew-agent` gives every aimem process it starts there
     (`docs/WORKSPACE.md`, "The member's aimem installation").
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

These run with every member's traffic going through the fault proxies, with
no fault armed.


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
- **G1. Grants.** The hub's operator revokes the team's grant
  (`aimem identity team revoke --team-name`).
  - The next claim is refused `project_not_granted` by aicrewd's live
    team read.
  - The next offer is refused before it begins: the coordinator's client
    already cannot read the task through aimem.
  - Nothing reaches aimem's reservation.
  - Granted again, the offer commits.
- **G2. Capabilities.** Each member joined with its own forge token
  (`join --cred`), and its launcher reported, as its session started, that
  its home verified write access to the project's repository; the run
  waits for the three reports before S1.
  - The forge withdraws the worker's push right; the worker's check
    (`aicrew-agent step capabilities`, through its launcher) reports read
    access.
  - The next offer to the worker is refused `capability_missing`, naming
    the forge's host and `write`, and nothing begins.
  - Restored and checked again, the offer commits.
- **T1. A worker's toolchain (pilot finding #18).** `aicrew team toolchain`
  refuses a name it does not know, and selects `dev-go` for the team's
  project.
  - The worker's launcher restarts; its session start records the
    project's toolchain in the home's `state/team.json`.
  - Cleared with `--clear`, the next start records none.
  - The e2e runs no headless client, so the worker's per-turn settings
    file is checked by the agent tests and the real probe in
    `docs/TURN-PROBE.md`.
- **B1. Board wake (A1).** aicrewd reads the hub's board feed with the
  `board.read` credential `aicrew hub add` bound from the provision
  directory.
  - A task created in BACKLOG wakes no one.
  - Moved to READY by the hub's admin, the task reaches the coordinator's
    inbox as an `action.needed` message whose subject is the board change.
    It arrives within a board read and the event queue's quiet and tick
    (`store.EventQuiet`, `server.EventTick`), and the checks that nothing
    is announced wait that long too. The Stop hook's `wait-inbox` blocks on
    it, and the message names the task and the change.
  - The coordinator offers the task, and the worker accepts. aicrew's own
    offer and accept are not announced.
  - aicrewd restarts, and nothing is announced again.
  - The hub's admin cancels the task while the worker holds it
    (`reservation recover cancel`). The coordinator gets exactly one
    announcement naming the change, and aicrewd closes the attempt as
    recovered.
- **C1. Team state, pause and nudge (C1).** The operator's control of the
  team, with `aicrew team` and `aicrew member`. The members' wake waits 8 s.
  - `aicrew team state` lists the coordinator and the worker with their
    roles and sessions, and neither is paused.
  - The operator pauses the team and nudges the worker. The worker's
    Stop hook starts no turn on the new message: its keep-alive turn says
    the team is paused. The team state shows both members paused and the
    worker's note unacknowledged.
  - After `aicrew team resume`, the worker's Stop hook blocks on the note,
    and the inbox delivers it with its `operator` note.
  - With only the worker paused (`aicrew member pause`), the coordinator's
    Stop hook wakes on its note and the worker's does not. After
    `aicrew member resume`, it does.
  - Both members' sessions keep their IDs and generations throughout.

Each scenario asserts both stores: aicrew's attempts, audit and capacity,
read-only; and aimem's tasks, holds and receipts, through admin reads.

## Faults (b4b-1)

**The fault proxy.** An HTTP-aware, re-encrypting proxy (`proxy_test.go`)
stands in three places:
- between the members' aimem and the hub;
- between the members' clients and aicrewd;
- between the hub and aicrewd's introspection and coordination routes.

It terminates its client's TLS with its target's own run key, so the
members' CA trust and both SPKI pins hold, and opens its own TLS to the
target. The hub therefore still terminates TLS itself. It never presents a
key or trusts a CA from outside the run.

A fault is armed for the next request matching a method and a path, and
fires once:
- drop the reply after the target answered;
- drop the request before forwarding it;
- delay the reply.

F1's faults instead hold for the whole step. aimem's CLI reads the task
over the same connection before it sends a reservation mutation, and Go's
HTTP client replays a request that carries an `Idempotency-Key` once when a
reused connection closes before any answer. A fault that fired once would
be replayed past, and the step would commit with no loss to reconcile.

The timed aimem adds **gates**: it can pause a named command until the
harness releases it, and keep a command's standard input in an owner-only
run file, which is never copied to the artifacts.

- **F1. Lost aimem replies.** For each of claim, transfer, update, finalize
  and release:
  - **The reply is lost after the hub committed:** aimem's CLI exits 5, the
    driver reads the receipt with the same key, and the step settles
    committed.
  - **The request is lost:** the step settles not committed. A lost update
    request cannot be decided while the hold has not moved, so the holder
    supersedes it.
  - The finalize's identity is asserted in the terminal evidence aimem
    persisted in its reservation event. No hub route reads these events, so
    the harness reads the hub's project store, read-only.
- **F2. Lost aicrewd replies.**
  - A lost begin is replayed under its Idempotency-Key, and reaches the one
    attempt the lost reply began.
  - A lost settle is retried and settles once.
- **F5. Stale steps.**
  - **A held task cannot be edited by the admin (409).** The staleness
    tested is therefore the real one: an offer begun at the task's revision,
    its claim paused while the admin edits the unheld task. aimem refuses
    the claim, aicrew settles the offer not committed, and the offer begun
    again commits.
  - **A proof replayed after its step settled** is refused.
  - **After a member resumes to a new generation,** its old proof is refused,
    and the new generation may not continue the old generation's offer.
    The resume ended that offer's proof, so aicrewd settles the offer not
    committed within 45 s and the worker's capacity is free.
  - **aicrewd's coordination answer delayed past aimem's 2 s call budget** is
    refused retryable, and the driver's retry commits.

## Faults (b4b-2)

The timed aimem also pauses **after** a command returns (`hold-after-…`).
aicrewd reaches the hub through the fault proxy too, so its reads can be
held back.

- **F3. Restarts.**
  - **The member's launcher is killed after aimem committed its claim and
    before the settle.** aicrewd's reconciler settles the claim alone,
    within 45 s; the audit names the reconciler as the settle's caller. The
    restarted launcher has no recorded step left.
  - **aicrewd is killed outright between begin and send.** The step commits
    and settles against the restarted aicrewd.
  - **The hub is killed outright between steps.** It restarts with its hold
    intact, and the next step releases the hold.
- **F4. Competing steps.**
  - A claim of a task with an open attempt is refused `task_busy` at begin.
  - An offer to a busy worker is refused `agent_busy` at begin.
  - An offer and a claim racing for one task: exactly one commits, and one
    attempt is open. The loser's answer must be a conflict refusal:
    aicrew's at begin (`task_busy`, `agent_busy`), or aimem's at the claim
    (`reservation_conflict`, `revision_conflict`, `stale_fence`). A
    failure, a non-answer, or a step lost before commit and settled as
    `not_committed` is none of them. The scenario checks that its
    predicate rejects the last, and the F4-lost case shows the race's own
    assertion failing on it.
- **F6. Recovery through the read scope.**
  - aimem's `recover release`, and `recover cancel`, on a running attempt:
    aicrewd closes it as recovered, with `closed_by` and the closing fence.
  - A held hold is not closed across 4 ticks.
  - **Nothing closes while the hub is unreachable for aicrewd.** aicrewd's
    reads are held back by the proxy from just before the recovery, then
    the hub is stopped. Once the hub is back, the attempt is closed as
    recovered.
- **F7. Secrets.** None of the run's secrets appears in any process log,
  the aimem call timings, the report, any command's output, or aicrew's
  audit table. The secrets are the admin bearer, user tokens, aicrew's
  credentials, invitation codes, captured proofs and aimem session handles.
  Each captured proof is registered when it is read, before a later
  capture replaces it, and F7 checks that the scan finds the earliest. The
  one command whose job is to print a secret, aimem's token issue, is left
  out of the scan.

F5 runs after F3, F4 and F6, and F7 runs last. F5's crashed coordinator
once held the worker's capacity until its offer's proof expired; since a
resume ends the session's old proofs (01a0f758-c827), F5 asserts that
aicrewd settles that offer within 45 s and frees the worker, and its place
is kept only to avoid churn.

## The skip-the-fault matrix

`AICREW_E2E_SKIP_FAULT=<case>` leaves one fault out. Every assertion that
the fault exists for is tagged with its case.

At the end of such a run, the test itself writes a `skip_verdict` record.
It reports `failed_as_expected` only when H passed, and the case's scenario
failed on an assertion tagged with that case.

`scripts/e2e-real-aimem.sh -skip-faults` runs every case on its scenario,
and accepts a case only on that record in a fresh report. An absent report,
a bootstrap failure, or a failure for another reason is rejected.

Each case's control reaches the same assertion as its fault and fails it,
rather than failing by construction:
- the F5 replay and resume cases replay a live proof under its own step's
  key, which aimem accepts;
- the F4 cases aim the competing step at a free task, or free the worker;
- F4-lost replaces the race loser's real answer with a step lost before
  commit (the driver's synthesized `not_committed`), which the race's
  assertion must refuse;
- F7-leak plants a secret in a scanned log.

The cases are:
- F1-reply, F1-request;
- F2-begin, F2-settle;
- F3-launcher, F3-aicrewd, F3-hub;
- F4-second, F4-race, F4-lost, F4-busy;
- F5-stale, F5-replay, F5-resume, F5-delay;
- F6-release, F6-cancel, F6-unreachable;
- F7-leak.

Every injected or skipped fault is recorded in the report.

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
- `fault`: each fault armed, injected or skipped (its case, proxy or gate,
  action and path).
- `observation`: behaviour the run observed and reported, which no
  assertion encodes.
- `skip_verdict`: under `AICREW_E2E_SKIP_FAULT`, whether the run failed as
  expected, and if not, why.
- `summary`: the counts, and the slowest coordination fact and aimem call
  against aimem's 5 s context-age bound.

Each run's process logs (hub, aicrewd and launchers) and the aimem call
timings are copied to `run-N/` beside the report.
