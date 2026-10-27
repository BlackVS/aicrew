# Pilot simulation design

Status: design for review (task `01a16a06-f242`). Nothing here is
implemented. The increments in section 9 are filed on the aicrew board from
this document. Written 2026-10-27.

The pilot is the team cycle run for real: a coordinator and a worker in
their own homes take a task from the board to a merged pull request and a
recorded delivery. It ran two cycles and was paused on 2026-10-15, and
neither cycle stumbled on the protocol. Both stumbled on process gaps: a channel a
message promised that did not exist, a nudge typed into a member's
terminal, a home file edited by hand, a turn that paid for the whole
conversation. Before the pilot resumes, the operator wants a test that
runs the same cycle in containers on its own. A command and a CI job start
it, it passes or fails by itself, and no person or agent types anything
into it.

This document designs that test. It extends three things, each in one
place:
- [DESIGN-AIFORGE](https://github.com/BlackVS/aimem/blob/2b068c1b8f9f499fbae304c3ab6dab0be2dd509d/docs/DESIGN-AIFORGE.md)
  splits the work between aimem and aicrew. The simulation checks that
  split from both stores after every step (section 5).
- [E2E-REAL-AIMEM.md](E2E-REAL-AIMEM.md) is the b4 harness, built under
  task `01a16942-c715`. It proves the wire protocol between real binaries
  on one host. The simulation reuses its CA, its pins, its fault proxies
  and its matrix (section 6), and adds the parts b4 leaves out:
  - the turn loop with a client that acts;
  - the installers and `join` as an operator runs them;
  - a real forge;
  - one container per party.

## 1. What it proves, and what it does not

The simulation proves that the released product, installed the way the
operator installs it, carries a task from the board to a recorded delivery
while every member runs only what the product gives it. Every step is
checked in aicrewd's store, in aimem's store and in the member homes.

It does not prove that a model does the right thing. A deterministic
program replaces the model, so a failure always points at aicrewd, the
launcher, an installer, aimem or a document's promise, never at a model's
judgement.

The rule that keeps it honest: **nothing in the simulation may do what the
product cannot.** It does not type through a pty, patch an installer,
hand-write a home file, write a store, or call an operator route a member
could not call. Where the product lacks a way, the gap is a product task
and a prerequisite (section 8), not a workaround in the test.

## 2. The scripted member

### 2.1 Where it sits

A member home names its client in `agent.json` as
`aicrew.client_command` (`internal/agent/config.go`). `serve`, `run` and
`check` run that command in place of `claude`. The scripted member is a
small Go binary, `aicrew-simclient`, built from this repository under
`e2e/pilotsim/`. It is installed in each member container, and the
scenario puts its path in `client_command`. aicrewd, the launcher and the
hub are not changed and do not know about it.

The launcher runs the client in three ways, and the scripted member
answers each the way the real client does:
- **Dependency check** (`check`, `join`): `--version`, a `-p hi` that
  prints a stream-json `system` init event, and `mcp list`, which names the
  home's aimem server.
- **Model probe** before the first turn (`modelprobe.go`): `-p "Reply
  with OK."` with `--max-turns 1`. It prints one `result` line, `success`
  by default, or the outcome the scenario sets (`model_unavailable` as an
  `api_error_status` of 404).
- **A turn** (`turnclaude.go`): `-p --output-format stream-json …` in the
  home, with `/crew-turn <brief>` on standard input. It acts, then prints
  the stream-json events the launcher reads and ends with one `result`
  line.

### 2.2 What it reads

- The brief on standard input (`brief.go`): the attempt's state, the role,
  the team, the attempt, the task, the branch, the base commit, the
  worktree, the frozen task, the trigger with its inbox messages, the open
  attempts for a coordinator, the pull requests as last observed, and the
  handoff section.
- Its environment as the launcher sets it: `AICREW_AGENT_HOME`,
  `AIMEM_TEAM_SESSION`, the home's aimem variables, and `aicrew-agent`
  first on `PATH`.
- Its own home, read-only except `docs/HANDOFF.md` and the worktree:
  `state/attempts/<id>/task.json`, `state/pulls.json`, the
  credential the home holds for the forge.
- The scenario file, `/sim/scenario.json`, mounted read-only: the role's
  script and the faults this member plays.

### 2.3 What it does

It acts only through the product's surfaces, the ones a model's tool calls
reach:
- `aicrew-agent step <op>` for every attempt step:
  - for a worker: accept, decline, work (its `submit` intent), release (to
    BLOCKED) and confirm-stop;
  - for a coordinator: offer, withdraw, review, confirm-delivery, finalize
    and stop.
- `aicrew-agent inbox --ack` for the messages a turn read, and
  `aicrew-agent escalate` for a coordinator's escalation.
- The home's aimem command, for the task reads and the coordinator's
  `triage_task` and `add_task_comment`.
- git in its worktree, pushing with the home's own forge credential.
- The forge API with the same credential: open a pull request, add
  `review-this`, read the labels and comments.
- An edit of `docs/HANDOFF.md`, so the turn is not a stall by the
  launcher's rules (`turns.go`).

The role scripts follow ROLES.md as code:
- **Worker:** accept the offer, commit to the attempt branch, open the
  pull request, add `review-this`, wait while `hands-reviewing` is
  present, and answer a rework with a new commit and a new `review-this`.
  Then submit through `work`, and ack each message it is sent.
- **Coordinator:** triage a READY task, offer it, follow up on a silent
  worker, and post the local review in the oh-code-review format at the
  full head SHA. After the merge, run verify-delivery (section 4) and
  confirm the delivery with the evidence it returns, then finalize.
  verify-delivery is the ai-skills skill's Python helper, and the script
  runs it at its installed path, `verify-delivery/verify_delivery.py` in
  the skill directory the home's client reads (section 3), as a model's
  tool call would.
  Escalate when the scenario says the worker blocked.

Each scenario chooses the member's behaviour for the faults the pilot hit:
- a turn that fails;
- a budget hold, a login hold (`api_error_status` 401) or an unserved
  model;
- a member that never acks a message;
- a worker that pushes while `hands-reviewing` is present;
- a worker that stalls.

The test asserts what aicrewd and the launcher do with each.

### 2.4 What it never does

- It never writes aicrewd's or aimem's store, nor a file the bootstrap
  manages in the home (`agent.json`, `.claude/`, `state/` other than
  through `step`).
- It never calls an aicrewd operator route, nor aimem's admin calls.
- It never reads another member's home or credential.
- It never sleeps for an outcome it could read: it reads labels,
  comments and steps, and ends the turn when it has nothing to do, so
  the launcher's own loop drives the next turn.

### 2.5 The stub reviewer and the stub CI

The external reviewer is a container of its own, `sim-hands`, that speaks
the real protocol on the forge:
- It polls for `review-this`, swaps it for `hands-reviewing`, and posts a
  comment in the oh-code-review format: the header
  `[hands-bot review] reviewed at head <full 40-character SHA>`, then a
  `VERDICT` line followed by `READY_FOR_HUMAN_MERGE` (or
  `RETURN_TO_IMPLEMENTATION` when the scenario asks). The coordinator's
  local review uses the same format with the header
  `[oh-code-review (high) review]`, so verify-delivery's default `local`
  and `external` patterns match both.
- It then sets `hands-reviewed`.
- It refuses to post for a head that moved while it held
  `hands-reviewing`, as hands does.

`sim-ci` posts a commit status for every pushed head and for the merge
commit, `success` unless the scenario asks for a failure.

Because both speak the real formats, the launcher's gate logic
(`dispatch.go`, the `review-request` and `gates-green` automations) and
verify-delivery run unchanged.

## 3. The topology

`e2e/pilotsim/compose.yaml` starts one network and these containers. Each
is installed the way the operator installs it:

| Container | What it runs | How it is installed |
| --- | --- | --- |
| `sim-ca` | a one-shot job: a throwaway CA, the server certificates and the SPKI pins, written to a shared volume | the harness's CA code |
| `sim-aimem` | `aimem serve`, the hub | aimem built from the pinned commit, as b4 does, because `install-hub.sh` takes no local base yet (aimem task `01a15057-43ed`; the row switches to the installer when that lands); `tasks on`, `process select`, `identity peer provision` |
| `sim-aicrewd` | `aicrewd -config …`, the command the installer prints | `install-aicrewd.sh` in install-only mode (`AICREW_NO_SERVICE=1`) with `AICREW_PREBUILT_DIR` (prerequisite 3), then `aicrew hub add`, `aicrew team setup`, `aicrew invitation issue` per member |
| `sim-gitea` | Gitea with TLS from the CA, an `operator` user, member users with their own tokens, one repository | the Gitea image pinned at 1.27 or later (verify-delivery reads a review of an older head through its compare diff, which earlier versions do not serve), configured by its own CLI |
| `sim-hands` | the stub reviewer (section 2.5) | built from `e2e/pilotsim/` |
| `sim-ci` | the stub CI status (section 2.5) | built from `e2e/pilotsim/` |
| `sim-coordinator`, `sim-worker` | one member home each, `aicrew-agent serve` under the container's init | an image with git and Python 3; `boot.sh` with `AICREW_PREBUILT_DIR` (prerequisite 2); ai-skills by its own installer, at the version the selected process set requires, into the skill directory the client reads (`aicrew-agent check` judges it by its `.ai-skills.json`, and the coordinator runs verify-delivery from it); then `aicrew-agent join --home … --wake turns --no-service --code-file /sim/invite-<role> …` (prerequisite 1), then `client_command` set to `aicrew-simclient` |
| `sim-runner` | the scenarios, the checks and the report | the runner (section 7) |

TLS is real everywhere:
- aicrewd and the hub pin each other's SPKI.
- Members trust aicrewd by `spki_sha256`.
- Gitea's certificate chains to the throwaway CA, which the members, the
  stubs and the runner trust through `SSL_CERT_FILE`.

Every input a person would give is a file the scenario writes before the
member starts:
- the invitation code, with `--code-file`;
- the aimem token and the forge token, as owner-only files;
- no login, because the scripted client has none.

The upgrade scenario installs the previous release of aicrewd and the
members with the same installers and runs one cycle. It then upgrades
aicrewd and each member through the installers, the way an operator
upgrades, and runs a second cycle, which must see the first cycle's
records.

## 4. The cycle

One scenario runs one task from the board to DONE. The runner files the
task through aimem as the operator would, and every later step is the
product's:

1. The task is READY on the board, and the coordinator's turn triages
   and offers it.
2. The worker's turn accepts the offer. The launcher clones and creates
   the worktree.
3. The worker commits, opens the pull request on Gitea and adds
   `review-this`.
4. `sim-hands` reviews. `sim-ci` reports. The coordinator's turn posts
   the local review.
5. The gates are green, and the `gates-green` automation marks the pull
   request ready.
6. The runner merges as Gitea's `operator` user, a person account, as the
   operator does. `sim-ci` reports on the merge commit.
7. The coordinator's turn runs verify-delivery against Gitea, with
   `--api-base` and `--review-author` naming the coordinator's and
   `sim-hands`'s accounts. It confirms the delivery with the evidence
   verify-delivery returns, and finalizes.
8. The task is DONE in aimem, and the attempt is finalized in aicrewd with
   its terminal evidence.

The last assertion of the cycle is that delivery record: verify-delivery
returned `confirmed`, and its three evidence entries (the reviewed head,
the person's merge and the post-merge CI) are the ones on the attempt and
the task. A run with a scenario fault also asserts the fault's outcome.
For example: a push during `hands-reviewing` leaves no review for the
moved head and a renewed request, and a budget hold appears in
`aicrew status` and in the coordinators' notices.

## 5. The checks after each step

After every step of section 4, the runner reads the three places and
compares them with the step's expected state. A check reads; it never
repairs.

- **aicrewd's store** (read through the operator CLI and, for what it does
  not print, the store file read-only):
  - the attempt's state, phase, branch and base commit;
  - `attempt_steps` and `attempt_results`;
  - `audit` and `receipts`;
  - `messages` and their acks;
  - `attempt_pulls` and `attempt_pull_facts`;
  - `turn_reports` (turns and costs);
  - `member_holds`;
  - `escalations`;
  - the automation runs.
- **aimem's store** (through admin reads, as b4 does):
  - the task's state and fields;
  - the reservation and its receipts, under aicrew's request keys;
  - the task's comments;
  - the journal entries the step should have written, and none it should
    not.
- **The member homes** (read-only, from the runner's mount):
  - `logs/turns.jsonl` (one record per turn, with its outcome);
  - `logs/guard.jsonl` (empty unless the scenario expects a line);
  - `docs/HANDOFF.md`;
  - `state/turns.json`.

Each check is a small table of expected values per step. The report keeps
the diff between the store before and after the step (section 7), so a
failure shows what changed, not only what was missing.

## 6. Faults

### 6.1 From b4

b4's three re-encrypting proxies sit on the same boundaries here:
- the members' aimem to the hub;
- the members to aicrewd;
- the hub to aicrewd.

Its fault cases F1 to F7 run on the new stack where a step of the cycle
crosses the same boundary:
- lost aimem and aicrewd replies;
- restarts of the launcher, aicrewd and the hub;
- competing and stale steps;
- recovery through the read scope;
- the secret-leak checks.

In the compose stack a restart is a container restart, not a process
signal.

### 6.2 From the pilots

New cases, each a scenario setting of the scripted member or the stubs:
- **P1:** a message the worker never acks. The coordinator's follow-up
  and the escalation must happen within their bounds.
- **P2:** a push while `hands-reviewing` is present. The stale review must
  not count, and the request must be renewed.
- **P3:** a budget hold, a login hold and an unserved model, one each.
  The hold reaches the coordinators and `aicrew status`; the member backs
  off; clearing the cause clears the hold.
- **P4:** a stalled worker. The launcher's stall rule blocks the attempt,
  and the coordinator sees the block.
- **P5:** a failing review (`RETURN_TO_IMPLEMENTATION`) and a failing CI
  status. The rework loop runs to a second head, and the gates follow it.

### 6.3 The matrix

The skip-the-fault matrix carries over: `AICREW_SIM_SKIP_FAULT=<case>`
leaves one fault out. The case is accepted only when the run fails where
the fault's absence should show. Each pilot case joins the matrix with its
own expected failure.

## 7. The runner

- **One command:** `scripts/pilot-sim.sh`, on Linux with Docker. It
  builds this tree's binaries and the pinned aimem, builds the images,
  starts compose, runs the scenarios in order, stops compose and removes
  the volumes unless `-keep` is given. Locally and in CI it is the same
  command with the same arguments.
- **Bounds:**
  - every wait has its own timeout;
  - every scenario has a bound;
  - the whole run has a hard bound (60 minutes to start with, set from
    the first green runs), after which the runner stops compose and fails
    the run.
  - No assertion waits on a sleep without a bound.
- **Deterministic:**
  - fixed names for users, teams, repositories and tasks;
  - every random choice seeded from the scenario;
  - the launcher's timers set only where `agent.json` documents a
    setting: `turns.turn_timeout` (1 minute at the least, 30 minutes by
    default). `wake.wait_seconds` bounds only the Stop-hook wait of an
    interactive client, which `serve` in turns mode does not use. The
    rest are constants and run at the
    production pace: the forge poll (60 seconds), the inbox poll
    (3 seconds) and the gap between two turn starts (5 seconds). A cycle
    waits on the forge poll a handful of times, a few minutes in all,
    which fits inside the run's bound; nothing patches them.
- **Report:** `report.jsonl` in b4's record shape (`run`, `step`,
  `scenario`, `fault`, `observation`, `skip_verdict`, `summary`), plus a
  `store_diff` record per step, with the rows each store gained, lost or
  changed. The containers' logs and the member homes are copied beside it.
  A run is green when every scenario's record says `passed` and the summary
  says so.
- **CI:** a workflow, `pilot-sim.yml`, runs nightly on main and by hand
  (`workflow_dispatch`) on any branch. It does not run on every push,
  because the stack is too heavy for that. Its artifact keeps the report
  and the logs for 14 days. Once the simulation has been green for a week,
  it becomes a requirement of the release-preparation pull request, beside
  b4's run.

## 8. Prerequisites: product gaps, filed as tasks

The simulation needs three things the product does not have yet. Each is a
product task, because a headless install needs it outside the test too.
Increment I1 depends on all three. Until they land, the simulation does not
type through a pty, patch an installer or hand-write a home.

1. `01a17d8e-4255`: `join --code-file PATH|-` reads the invitation code
   from a file or standard input, as `--aimem-token-file` does. Today join
   reads it only at a terminal prompt and stops with `no_terminal`
   otherwise.
2. `01a17d8e-4267`: `boot.sh` installs from a local directory of prebuilt
   binaries (`AICREW_PREBUILT_DIR`), as `install-aicrewd.sh` does.
3. `01a17d8e-427b`: `install-aicrewd.sh` has an install-only mode that
   needs neither systemd nor linger, so a plain container can install
   aicrewd. Its supervisor then runs the `aicrewd -config` command the
   installer prints.

## 9. Increments

Each is under L and filed on the aicrew board; the dependencies below set
the order of work.
- **I1 (M), the compose topology, the installers and join** (`01a17d9a-932b`).
  - What it builds: `sim-ca`, `sim-aimem`, `sim-aicrewd`, `sim-gitea` and
    the member containers, installed and joined as section 3 says.
  - Its check: every member's `aicrew-agent check` is green, and `serve`
    starts and passes the model probe with `aicrew-simclient` answering
    only the dependency check and the probe.
  - It depends on this design and on the three prerequisites.
- **I2 (M), the scripted member and the full cycle** (`01a17d9a-f147`).
  - What it builds: `aicrew-simclient`'s worker and coordinator scripts,
    and the cycle of section 4 from a filed task to the delivery record.
  - Its checks: the per-step checks of section 5.
  - Depends on I1 and I3.
- **I3 (S), the stub reviewer and the stub CI on Gitea** (`01a17d9a-bbff`).
  - What it builds: `sim-hands` and `sim-ci`, speaking the real labels,
    the review format with the full head SHA, and commit statuses.
  - Its tests: verify-delivery runs against a fixture pull request on
    `sim-gitea` and returns `confirmed`.
  - Depends on I1.
- **I4 (S), the faults and the matrix** (`01a17d9b-3a68`).
  - What it builds: b4's proxies and cases on the new stack (section 6.1),
    the pilot cases P1 to P5, and the skip-the-fault matrix.
  - Depends on I2.
- **I5 (S), the runner's report, bound and CI workflow** (`01a17d9b-3a78`).
  - What it builds: the JSONL report with the per-step store diff, the
    run's hard bound, `pilot-sim.yml` nightly and by hand, and the
    artifact.
  - Depends on I2.
- **I6 (S), the upgrade scenario** (`01a17d9b-5c3b`).
  - What it builds: the previous release installed by the installers, a
    cycle, an upgrade through the installers, and a second cycle that sees
    the first one's records.
  - Depends on I2 and I5.

## 10. What stays out

- **A real model in the gating path.** At most a later, optional,
  non-gating scenario behind an API key may run a real client. No
  increment here builds it.
- **The operator's own hubs** and any subscription client.
- **Any change to the b4 harness or to aicrewd** to fit the simulation. A
  gap found on the way is a separate task, as section 8's are.
- **Restarting the pilot.** That stays the operator's decision, after the
  simulation is green.
