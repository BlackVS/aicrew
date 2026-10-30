# The pilot simulation

The pilot simulation runs the team cycle in containers, with no person or
agent typing into it. The design is [DESIGN-PILOT-SIMULATION.md](DESIGN-PILOT-SIMULATION.md).
This document says how to run it, and what each increment adds.

**I1** (task `01a17d9a-932b`) is the stack: every party installed and
joined the way the operator installs and joins it, and a smoke check.
**I3** (task `01a17d9a-bbff`) adds the stub reviewer and the stub CI, with
fixture pull requests that verify-delivery reads. **I2a** (task
`01a186ea-0457`) adds the scripted members' turns and the cycle up to the
submitted pull request. **I2b** (task `01a17d9a-f147`) takes the cycle to
DONE: the rework, the local review, the operator's merge and the delivery
record, twice in a row. **I4a** (task `01a17d9b-3a68`) adds the pilot
cases P1 to P5 and the skip-the-fault matrix. b4's faults, the report and
the CI workflow come in I4b to I6.

## Running it

On Linux with Docker (on a Windows host: WSL with Docker Desktop's WSL
integration switched on for the distribution):

```sh
[AICREW_SIM_SKIP_FAULT=CASE] scripts/pilot-sim.sh [-keep] [-out DIR]
```

- `-out` is where the staged image contexts, the logs and the reports go
  (default `.work/pilotsim` in the repository).
- `-keep` leaves the stack up after the run and prints the
  `docker compose` command, with its environment, to look inside. Without
  it the stack and its volumes are removed at the end, green or not. Every
  run starts by removing a stack an earlier `-keep` left.
- `AICREW_SIM_SKIP_FAULT=CASE`, CASE one of `P1` to `P5`, runs only that
  pilot case, without its fault (see "The pilot cases").

It needs Docker with Compose v2, the Go that `go.mod` declares or later, Python 3, and network
access: the images install aimem and ai-skills from their GitHub releases,
as an operator's host does.

The script exits 0 when the stack is healthy, the stubs' fixtures pass,
two cycles reach DONE with their delivery records and the pilot cases pass. Otherwise it exits non-zero
and prints the last lines of every container's log; the full logs are in
`DIR/logs/`.

## What I1 builds

`e2e/pilotsim/compose.yaml` runs one network and these containers:

| Container | What it is | How it is installed |
| --- | --- | --- |
| `sim-ca` | a one-shot job: the run's throwaway CA, a certificate for each service, and their SPKI pins | `simca` (`e2e/pilotsim/cmd/simca`), on the CA code the b4 harness uses (`e2e/testca`) |
| `sim-aimem` | `aimem serve`, the hub | the tested aimem release (`internal/agent/supported.json`), installed on the first start by that release's `install-hub.sh` in its install-only mode (`AIMEM_NO_SERVICE=1`) from the staged asset (`AIMEM_PREBUILT_DIR`, checked against the release's `SHA256SUMS`); the container then runs the serve command the installer prints on its last line. The introspection credential's file is added to the env file by hand, as aimem's hub runbook says |
| `sim-aicrewd` | `aicrewd -config …` | `install-aicrewd.sh` with `AICREW_NO_SERVICE=1` and `AICREW_PREBUILT_DIR`, from this tree's binaries; the container runs the command it prints |
| `sim-gitea` | Gitea 1.27 with TLS from the run's CA | the pinned image, configured by its own CLI and API |
| `sim-hands`, `sim-ci` | the stub reviewer and the stub CI ([below](#the-stub-reviewer-and-the-stub-ci)) | `simstub` (`e2e/pilotsim/cmd/simstub`) in one image, `e2e/pilotsim/stub/` |
| `sim-coordinator`, `sim-worker` | one member home each, `aicrew-agent serve` | `boot.sh` with `AICREW_PREBUILT_DIR`; aimem's verifying boot script and the ai-skills release archive checked against `SHA256SUMS`, both as `aicrew-agent check` instructs; then `aicrew-agent join … --wake turns --no-service --code-file …` |

The member's client is `aicrew-simclient` (`e2e/pilotsim/cmd/aicrew-simclient`),
installed as `claude` on the image's `PATH`, where a member finds its
client. It answers what `aicrew-agent check`, serve's model probe and the
turn loop ask:
- `--version` gives the tested Claude Code release;
- `mcp list` and the init event show what the home wires. The client finds
  it the way Claude Code does, and checks the aimem server with a real MCP
  initialize;
- the model probe gets one finished turn, with no model call;
- a turn runs the role's script ([the cycle](#the-cycle)).

Anything else exits 2 ("not simulated").

TLS is real everywhere:
- aicrewd and the hub pin each other's SPKI;
- the members trust aicrewd by `spki_sha256`;
- Gitea's certificate chains to the run's CA, which every container trusts
  through `SSL_CERT_FILE`, and the members' git through `GIT_SSL_CAINFO`.

## The operator's steps

`scripts/pilot-sim.sh` plays the operator, each step with the product's own
command on the host it belongs to:
1. In Gitea:
   - the `operator` account, one account per member and one per stub
     (`sim-hands`, `sim-ci`), each with its own token;
   - the repositories `pilot` (the team's) and `fixture` (the stubs'
     fixtures), with the members and the stubs as collaborators and the
     labels `review-this`, `hands-reviewing` and `hands-reviewed`, as a
     real reviewer's site has them;
   - the public repository `process`, the project's process set
     (`e2e/pilotsim/hub/process/`), from which the hub imports the set and
     the members' automations read their points.
2. On the hub: `aimem tasks on`, `aimem process select` (the `process`
   repository's commit), `aimem project repo set`, and
   `aimem identity peer provision`. The peer's credential directory is then
   copied to aicrewd's host.
3. On aicrewd's host:
   - `aicrew hub add`, then a restart of aicrewd;
   - `aicrew team setup`, with the grant it prints run by the hub's admin,
     then setup again;
   - `aicrew hub-credential issue` for the hub's introspection credential.
4. Each stub's token in its own `/sim/in`, then the scenario file.
5. Per member:
   - an aimem user and a user token, `aicrew invitation issue`;
   - the member's input files.

Every input a person would give is a file written before the member
starts: the invitation code, the aimem token and the forge token. Each is
owner-only in the member's `/sim/in`, and the script then writes `ready`
there. The member joins with them and removes them, then runs serve.

No secret is printed or passed as an argument. A token or code goes from
the container that makes it to the one that reads it through a pipe.

## The smoke check

- Each member's serve logs that the turn model is available: the model
  probe passed, and no turn has run.
- `aicrew-agent check --json` exits 0 in both homes, and
  `e2e/pilotsim/smoke.py` reads its report. It requires:
  - aimem supported;
  - the client supported, with its aimem MCP server connected and no
    required skill missing;
  - the turn model available;
  - a verified forge credential;
  - ai-skills supported at the tested release, as the record its
    installer writes (`.ai-skills.json`) names it, and installed where the
    client reads it.

  The reports are kept as `DIR/check-<role>.json`.
- `aicrew status` lists both members, each holding a session
  (`DIR/status.json`).
- `verify_delivery.py --help` runs from the coordinator's skill directory.

## The stub reviewer and the stub CI

Both are `simstub`, in `sim-hands` (`simstub hands`) and `sim-ci`
(`simstub ci`). Each reads only its own Gitea token and the scenario file,
`/sim/scenario/scenario.json`, which they share. A stub reads the file
again on every poll, so the runner changes a verdict or a CI state by
replacing it:

```json
{
  "repos": ["operator/pilot", "operator/fixture"],
  "poll_seconds": 1,
  "hands": {"verdict": "READY_FOR_HUMAN_MERGE", "first_verdict": "", "review_seconds": 0},
  "ci": {"state": "success", "context": "sim-ci"}
}
```

Every field but `repos` has the default shown. `hands.verdict` may be
`RETURN_TO_IMPLEMENTATION`. `hands.first_verdict`, when set, is the verdict
of a pull request's first review, one with no `[hands-bot review]` comment
yet; later reviews take `hands.verdict`. `ci.state` may be any Gitea commit
state.

The runner also writes a `generation`, one higher each time. A stub writes
the generation it loads to `/sim/scenario/<mode>.ack` before the poll that
uses it, and the runner waits for both acks before its next step. A stub
polls one at a time, so by then no poll that read the old scenario is
still running. Without that wait, a CI poll that started before the change
could still post the old state on a head the runner creates after it.

`sim-hands` follows the external-review label protocol:
1. On an open pull request labelled `review-this`, it puts
   `hands-reviewing` on, then takes `review-this` off, so the pull request
   always carries one of the protocol's labels.
2. After `review_seconds` it reads the head again.
   - If the head moved, it posts nothing and takes `hands-reviewing` off,
     so the pull request can be requested again.
   - Otherwise it posts one comment headed
     `[hands-bot review] reviewed at head <full SHA>`, in the oh-code-review
     layout, with a `VERDICT` line followed by the verdict. Then it puts
     `hands-reviewed` on and takes `hands-reviewing` off.

`sim-ci` posts one commit status under its context on every branch head
that has none: the pull requests' heads and, after a merge, the merge
commit.

## The stubs' fixtures

After the smoke check, the script opens pull requests on `fixture` as the
operator and checks them:
1. **Confirmed.** Both stubs serve the pull request, the coordinator's
   account posts a local review headed `[oh-code-review (high) review]`,
   and `operator` merges it. verify-delivery runs unchanged from the
   coordinator's skill directory, with `--api-base` and
   `--review-author local=sim-coordinator --review-author external=sim-hands`.
   It must return `confirmed` with the evidence kinds `reviewed_head`,
   `human_merge` and `post_merge_ci`.
2. **A failed CI.** The scenario sets `ci.state` to `failure`, and the same
   steps must give `not_confirmed`, with `post_merge_ci` failed.
3. **A moved head.** With `review_seconds` at 15, a commit is pushed while
   `hands-reviewing` is on. Neither head gets a review, and the labels come
   off. A new `review-this` is then served at the new head, with one review.

The verify-delivery results are kept as `DIR/verify-<number>.json`.

## The cycle

After the fixtures, the script runs the cycle twice on the same stack: each
time one task from the board to DONE, with no typed input. Each turn of a
member is the client's turn mode (`e2e/pilotsim/cmd/aicrew-simclient/turn.go`).
It reads the role and the attempt from the brief on standard input, and the
messages from `aicrew-agent inbox --json`. It acts only through the
product's surfaces: aicrew-agent's steps and its `clone` and `forge pr`
commands, git in the worktree, the home's aimem MCP server, and the
installed verify-delivery helper. It acks the messages it read, records
what it did in `docs/HANDOFF.md`, and ends with one stream-json `result`
line, an error result when its script fails. It never sleeps: a turn with
nothing to do ends, and the launcher's next message starts the next one.

- **The coordinator**:
  - reads each task a board change or a READY list names. It skips a task
    with an open attempt (`aicrew-agent attempts`). It triages a BACKLOG or
    READY task to READY, with its next action and a comment, through
    `triage_task` and `add_task_comment`. Then it offers the task to the
    first worker that `aicrew-agent members` shows with the project's
    repository verified at write access, with
    `aicrew-agent step offer --repository URL --process`. The URL comes
    from `project_repository`; `--process` fills the process pin and its
    instruction digest from the hub's selection.
  - on a submit announcement, records the result and its number in its
    handoff, posts the local review at the pull request's full head SHA
    (`forge pr comment`, headed `[oh-code-review (high) review]`), and
    accepts the result with the `review` step. No member surface names a
    result's number yet (task `01a188f9-4d9a`), so the coordinator counts
    the attempt's submit announcements: the nth is result n.
  - in every turn, for each accepted attempt whose pull request is merged,
    runs verify-delivery from its skill directory with its home's own forge
    credential, `--api-base` and
    `--review-author local=<its account> --review-author external=sim-hands`.
    On `confirmed` it confirms the delivery with the evidence returned
    (`confirm-delivery`) and finalizes (`finalize`); otherwise it records
    the verdict and waits for a later turn.
  - answers a worker's blocked report with the `answer` step.
- **The worker** accepts an offer with the instruction digest the offer
  carries. In its running attempt it makes the worktree with
  `aicrew-agent clone`, commits one file, pushes with the home's own
  credential, opens the pull request with `aicrew-agent forge pr open`, and
  adds `review-this`. Later turns read the pull request with
  `forge pr show` and wait while `review-this` or `hands-reviewing` is on.
  A `RETURN_TO_IMPLEMENTATION` from sim-hands at the head, or failed
  checks once the review is done, gets a new commit, the stale
  `hands-reviewed` taken off and a new `review-this`. Once sim-hands's
  review at the head is READY and the checks passed, the worker submits it
  through `work` (intent `submit`). In the `rework` phase it resumes
  through `work` first, and in the `blocked` phase it waits for the
  coordinator's answer and then resumes the same way.

The script gives the members no input of its own. `--process` needs an
aimem with `aimem process pin` that reads under the team session, 0.10.18
or later.

The scenario makes sim-hands return each pull request's first review and
take 50 seconds over every review, more than three of the launcher's turn
gaps: a worker waiting on its gates gets no continuation turn and is never
stalled or blocked (docs/DESIGN-CONTROL-PLANE.md, 1.8).

The operator files the task READY through the hub's API: a coordinator is
woken by a change to READY, not by a new BACKLOG task. The script holds
each step's state with `aicrew member pause`, waits until the paused
member's turn has ended (`state/turns.json` names no running turn, and
`logs/turns.jsonl` gains none over two seconds), takes a read-only
snapshot, checks it, and resumes the member. The steps:

| Step | Held | What it shows |
| --- | --- | --- |
| offer | worker | the attempt offered, the reservation held for the coordinator |
| accept | worker | the attempt running, the reservation transferred to the worker |
| pull | worker | the watched pull request; the coordinator is held from here |
| rework | worker | sim-hands returned an older head, and the worker pushed a new one |
| submit | worker, coordinator | one result; the task in REVIEW with the pull request among its candidate refs |
| review | coordinator | the local review READY at the head, the result accepted |
| merge | coordinator | merged by `operator`, sim-ci's success on the merge commit |
| done | none | the attempt closed and finalized, the task DONE, the reservation ended |

The merge is the runner's, as the Gitea `operator` person account.
`e2e/pilotsim/cycle.py` checks each snapshot against its step's row:
- aicrewd's store (a copy): the attempt's state and phase, its steps (all
  committed, no `release`), receipts, results, the accepted, delivered and
  finalized result, the watched pull request, the messages about the task,
  each acked once a turn read it, the members' turn reports, no member
  hold and no escalation, and an audit in which the operator's calls are
  the script's own and the members' are member operations;
- aimem: the task's state, the triage comment, a history that never went
  through BLOCKED or CANCELLED, and the reservation's holder;
- the forge: sim-hands's RETURN of the older head and its READY at the
  head, the coordinator's READY at the head, the merge and its CI;
- both homes: every turn in `logs/turns.jsonl` finished, none running and
  no hold in `state/turns.json`, `logs/guard.jsonl` empty, and the
  handoffs recording the pull request, the rework, the submit, the
  acceptance and the finalize.

The last check is the delivery record. The script runs verify-delivery
itself, as the coordinator ran it, and its `confirmed` evidence
(`reviewed_head`, `human_merge`, `post_merge_ci`) must be the attempt's
delivery and terminal evidence and among the task's evidence refs.

The snapshots stay in `DIR/cycle-<n>/<step>/`.

## The pilot cases

After the two cycles, `e2e/pilotsim/cases.sh` runs the pilot cases of
[DESIGN-PILOT-SIMULATION.md](DESIGN-PILOT-SIMULATION.md), section 6.2, in
the order P2, P5, P4, P1, P3. Each is one fault on a cycle of its own, set
through the stubs' scenario or the member's: `/sim/scenario/member.json`
in each member's container, which only the script writes, and only while
the member is paused and its turn has ended. The member's scenario
(`e2e/pilotsim/cmd/aicrew-simclient`) can leave the messages unacked
(`never_ack`), push once while `hands-reviewing` is on
(`push_during_review`), do nothing in a working attempt (`stall`), end
every turn with the provider's 401 or 404 (`result`: `login` or `model`),
and report a client cost (`cost_usd`). In the cases sim-hands reviews READY
the first time: the plain cycles already run the rework after a
`RETURN_TO_IMPLEMENTATION`.

`e2e/pilotsim/cases.py` checks each case at the points below, on a snapshot
laid out as the cycle's, and each case that reaches DONE ends with the
cycle's checks at the step done, without the plain cycle's external RETURN.

| Case | Fault | Point | What it shows |
| --- | --- | --- | --- |
| P2 | the worker pushes while `hands-reviewing` is on; a worker waiting on its gates gets no turn, so the operator's nudge gives it one during the review | submit | two commits; no review of the first head counts; sim-hands's READY at the second head after the renewed request |
| P5 | sim-ci fails the first head | submit | the worker reworked for the failed checks; READY and CI success at the second head, the head aicrewd watches |
| P5 | sim-ci leaves the merge commit pending | pending | verify-delivery answered pending, and the attempt has no delivery, no finalize and no `confirm-delivery` step; the task stays in REVIEW |
| P4 | the worker stalls after its accept | blocked | the launcher's stall rule blocked the attempt; the coordinator's blocked report; the task BLOCKED in aimem |
| P4 | | answered | the coordinator's `answer` step and its handoff; the worker then resumes and the cycle reaches DONE |
| P1 | the worker never acks | unacked | the offer delivered to the worker and not acked |
| P3 | the coordinator's turn costs 60 USD; the worker's ends with a 401 | held | the coordinator's `daily_budget` hold and the worker's `needs_login` hold in aicrewd, in `aicrew status` and in both launchers' turn state; the coordinator told of the worker's hold |

A hold's message goes to the team's coordinators other than the held
member, so only the worker's hold can reach a coordinator here. For the
coordinator's own daily-budget hold, "reaches the coordinators" does not
apply in a team with one coordinator: there is no other coordinator to
tell. That hold is checked in aicrewd's record, in `aicrew status` and in
the launcher's backoff. P3 comes
last: both members stay held after it, the coordinator until the next day
and the worker for its backoff.

Three parts of the cases are gaps the board tracks, and the script checks
them expected-to-fail: it waits for what the case asks for, and records in
`DIR/expected-to-fail.txt` that it did not happen. When it does happen, the
gap is closed and the run fails until the case is updated.

| Case | Expected to fail | Task |
| --- | --- | --- |
| P1 | the coordinator's follow-up on the unacked offer within 120 seconds; no bound exists for an unacked message | `01a18ce5-334e` |
| P3 | the login hold ending within 120 seconds once the login is back; the launcher re-checks only after its backoff | `01a18ce5-335f` |
| P3 | an unserved model (404) reported as a hold; the launcher backs off and reports nothing. Not run: a member's backoff leaves one held outcome per run. Its reproducer is the member scenario `{"result":"model"}` | `01a176ba-db7d` |
| P5 | the delivery once the merge commit's CI succeeds, with no wake: the pull watch reads only the head's checks. The operator's nudge then wakes the coordinator, and the cycle ends with its delivery record | `01a18ce5-336d` |

`AICREW_SIM_SKIP_FAULT=CASE` runs the stack and that case only, without its
fault. The run is green only when the case then fails where its absence
should show: P2, P1 and P5's first part at their first check, P4 and P3
when the block or the hold never comes. A case that passes without its
fault fails the run. The snapshots stay in `DIR/case-<case>/<point>/`.
