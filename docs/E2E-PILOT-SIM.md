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
cases P1 to P5 and the skip-the-fault matrix. **I4b-1** (task
`01a18ccf-d95c`) adds the fault proxies and b4's fault cases F1, F2-begin,
F3, F6 and F7 on the compose stack. **I5** (task `01a17d9b-3a78`) adds
the run's report, its bounds and the CI workflow. **I6** (task
`01a17d9b-5c3b`) adds the upgrade scenario ([below](#the-upgrade-scenario)).
b4's other faults come in I4b-2.

## Running it

On Linux with Docker, best on a host of its own (see "A dedicated host"
below). A Windows desktop works too, through WSL with Docker Desktop's
WSL integration switched on for the distribution, but there the run
competes with everything else on the desktop for memory.

```sh
[AICREW_SIM_SKIP_FAULT=CASE] [AICREW_SIM_IGNORE_MEMORY=1] [AICREW_SIM_BOUND_MINUTES=N]
[AICREW_SIM_HANG=SCENARIO[:SECONDS]] scripts/pilot-sim.sh [-keep] [-out DIR]
[PILOTSIM_PREVIOUS=vX.Y.Z] scripts/pilot-sim.sh -upgrade [-keep] [-out DIR]
```

- `-upgrade` runs the upgrade scenario instead of the cycles and the cases
  (see "The upgrade scenario"). `PILOTSIM_PREVIOUS` overrides the release
  it upgrades from, for a run by hand.

- `-out` is where the staged image contexts, the logs and the reports go
  (default `.work/pilotsim` in the repository).
- `-keep` leaves the stack up after the run and prints two `docker
  compose` commands, each with its environment: `ps`, to look inside, and
  `down -v --remove-orphans`, which removes the stack with its volumes.
  Remove a kept stack with that second command, never with `down` alone,
  which leaves the volumes. Without `-keep` the stack and its volumes are
  removed at the end, green or not, and the run fails if anything of the
  project is left (see "On a shared Docker host"). Every run starts by
  removing a stack an earlier `-keep` left.
- `AICREW_SIM_SKIP_FAULT=CASE` runs only the case CASE belongs to, without
  CASE's fault. CASE is one of `P1` to `P5` (see "The pilot cases") or a
  part of a fault case, such as `F1-claim-request` (see "The fault
  cases", which lists them).
- Before it builds anything, the script removes a leftover stack and
  checks the host's available memory (`MemAvailable` in `/proc/meminfo`)
  against the 4096 MiB a run needs. With less it exits 2, naming both
  amounts. `AICREW_SIM_IGNORE_MEMORY=1` runs it anyway, deliberately.
- `AICREW_SIM_BOUND_MINUTES=N` is the whole run's bound (90 minutes by
  default), and `AICREW_SIM_HANG=SCENARIO[:SECONDS]` hangs one scenario to
  test the bounds (see "The bounds").

It needs Docker with Compose v2, the Go that `go.mod` declares or later, Python 3, `ps`, and network
access: the images install aimem from its GitHub release, and the hub
imports the ai-skills release archive, as an operator's hosts do.

The script exits 0 when the stack is healthy, the stubs' fixtures pass,
two cycles reach DONE with their delivery records and the pilot and fault cases pass; with
`-upgrade`, when the setup on the previous release, both cycles, the
upgrade and the reads of the first cycle pass. Otherwise it exits non-zero
and prints the last lines of every container's log. Either way the full
logs are in `DIR/logs/` and the report in `DIR/report.jsonl` (see "The
report").

### A dedicated host

A host that runs nothing else keeps the simulation from competing with an
operator's desktop. On a desktop, the agent harness running the
simulation can be stopped for low host memory while the stack itself
stays small.

**What it needs.** A Linux host (a VM is fine), with at least 2 vCPU and
6 GB of memory; 6 vCPU and 16 GB leave ample room. About 10 GB of free
disk covers the images, the Go build cache and the reports. Full runs
measured, sampled every 5 seconds:

| Phase | Memory |
| --- | --- |
| idle host | about 0.6 GB used |
| the Go build of this tree's binaries (the run's peak) | about 1.3 GB used on the host; one `compile` process up to 335 MiB |
| the stack, during the cycles and the cases | Gitea up to 301 MiB, each member up to 64 MiB, aimem up to 56 MiB, aicrewd up to 30 MiB, each stub up to 15 MiB |

Every compose service has a memory limit of at least twice its peak:
Gitea 768 MiB, each of the three members 512 MiB, aimem and aicrewd
256 MiB each, the stubs and sim-ca 128 MiB each, and the four fault
proxies 64 MiB each, 3.4 GiB in all. The 4096 MiB the script asks for is
those limits plus the build beside them. A full run
takes about 61 minutes with the three members (60 min 57 s and 60 min 29 s
on a 6 vCPU host, 2026-11-12; the nightly runs on GitHub's hosted runner,
61 min 26 s and 61 min 24 s), well inside the run's 90-minute bound.

**Setup.** On the host, as an ordinary user in the `docker` group:
- Docker Engine with the Compose v2 plugin (and buildx, which Compose
  builds with);
- git, Python 3 and curl;
- Go, the version `go.mod` declares or later. A distribution's Go is
  enough when it is recent enough; `GOTOOLCHAIN=auto` fetches a newer one
  when `go.mod` asks for it;
- outbound HTTPS to GitHub (the aimem and ai-skills releases, Go modules)
  and to Docker Hub (the base images and Gitea).

**Running there.** The script runs on the host from a checkout there, so
the build, the stack and the checks all stay off the desktop.
`DOCKER_HOST=ssh://` would leave the Go build and the checks on the
desktop, so it is not the way to use such a host. Over ssh, with a key
made for this host only:

```sh
ssh -i <key> <user>@<host> 'cd aicrew && git fetch -q && git checkout -q <branch> && git pull -q --ff-only'
ssh -i <key> <user>@<host> 'cd aicrew && mkdir -p ~/runs && setsid -f sh -c "scripts/pilot-sim.sh -out ~/runs/out-<name>; echo exit=\$?" > ~/runs/pilotsim-<name>.log 2>&1 < /dev/null'
ssh -i <key> <user>@<host> 'tail -n 3 ~/runs/pilotsim-<name>.log'
scp -r -i <key> <user>@<host>:runs/out-<name> .
```

`setsid -f` starts the run in a session of its own and returns at once,
so the run goes on when the ssh session ends. A trailing `&` instead
keeps the ssh session open until the run ends. The last
line of the log is the exit code, and the reports, the snapshots and the
containers' logs are in `out-<name>`.

### On a shared Docker host

A host whose Docker serves other projects too is shared: what one project
leaves there, another cannot tell from its own. The simulation's project
name on such a host is `pilotsim` (the `name` of
`e2e/pilotsim/compose.yaml`). Every agent and person working for this
project there keeps to these rules, in the runner and in any command run
by hand:

- **a. One name.** Everything belongs to the compose project `pilotsim`:
  its containers carry the compose project label, and its named volumes
  and its network carry the `pilotsim_` prefix and that label.
- **b. A label on what compose does not make.** A container started
  outside compose gets `--label project=pilotsim`. A volume made by hand
  gets that label and the `pilotsim_` prefix.
- **c. No anonymous volumes.**
  - A one-off container runs with `--rm`.
  - A container removed by hand goes with `docker rm -v`.
  - A stack whose data is disposable goes down with `docker compose down -v`.
  - Where an image declares a volume, compose mounts a named volume (or a
    tmpfs) over that path. The forge image declares `/data`, which the
    stack mounts as `gitea-data`.
- **d. Judge only one's own.** Cleanup and checks read only the
  project's own resources, by its label or prefix. They never compare the
  host's whole list of containers, volumes or images with a fixed list.
- **e. Nothing host-wide.** No `docker system prune`, `docker volume
  prune`, `docker image prune -a` or `docker builder prune`, and no
  removal of anything that does not carry the project's name or label.
- **f. Others' leftovers stay.** A leftover that is not the project's is
  left alone and reported, with its name, creation time and size, to
  whoever coordinates the host.

The runner checks a and c on its own resources only:
- After the members start, every volume a container of the stack mounts
  must carry the project's label, so an anonymous volume fails the run,
  naming the container and the path.
- Without `-keep`, after the final `down -v`, nothing carrying the
  project's label may be left (containers, volumes, the network), and
  none of the volumes it recorded from the mounts.

## The report

Each run writes `DIR/report.jsonl`, one JSON object per line in b4's
record shape (docs/E2E-REAL-AIMEM.md, "The report"), each with its `type`
and its time `at`:

- `run`: the aicrew commit, the aimem and ai-skills versions, the OS, the
  architecture, Docker's version, the Go of the report tool, the run's
  bound in minutes, and the skipped fault or hung scenario if any.
- `scenario`: one per scenario (`setup`, `smoke`, `fixtures`, `cycle-1`,
  `cycle-2`, then `case-P2` and on in the order the cases run), with its
  `result` (`PASS` or `FAIL`), its duration, its bound, and for a failure
  the reason.
- `step`: one per step snapshot, with its scenario, its name, its
  directory under `DIR` and its checks (`passed`, `failed`, or `none` for a
  snapshot no check reads).
- `store_diff`: one per step, beside it. `aicrewd` holds each table of
  aicrewd's store that changed since the step before (`against` names it):
  the rows it `gained` and `lost`, and the `changed` ones by their key,
  with each changed field before and after. `aimem` holds the same for
  aimem's reads of the step's task, against the last step that read the
  same task (`aimem_against`): the task's and the reservation's fields,
  and the comments and history entries. A list longer than 100 rows is cut
  there, with the rest counted in `gained_more`, `lost_more` or
  `changed_more`, and a text value longer than 300 bytes is cut with its
  length.
- `fault`: each fault, `armed` or (under `AICREW_SIM_SKIP_FAULT`)
  `left_out`, with its case and what it does.
- `observation`: each part checked expected-to-fail, with the gap's task.
- `skip_verdict`: under `AICREW_SIM_SKIP_FAULT`, `failed_as_expected` with
  the point where the case's absence showed, or `not_as_expected` with the
  reason.
- `summary`: the scenarios passed and failed, the steps, the exit code,
  `green`, the run's duration and bound, the failure if any, and
  `secrets_redacted`.

A run is green when its exit code is 0: every scenario `PASS`, and under
`AICREW_SIM_SKIP_FAULT` the case's own scenario `FAIL` with the verdict
`failed_as_expected`. A red run's report ends at the failing scenario,
and the step before the failure carries its checks and its store diff.

The runner records events in `DIR/events.jsonl` as the run goes, and the
report tool, `e2e/pilotsim/cmd/simreport`, writes the report from them
and the step snapshots when the run ends, green or not. It checks every
record against this shape before it writes the report, and replaces any
of the run's known secrets (case F7's inventory) in a value with its
number. Its test runs it on a recorded red run: two step snapshots from
the lab host and the events of a scenario that failed at its second step.
Beside the report, the run copies every container's log to `DIR/logs/`
and each member home's `docs/`, `logs/` and `state/` to `DIR/homes/`; no
credential lives there.

## The bounds

Every wait has its own timeout. Each scenario has a bound: the setup 30
minutes (it builds the binaries and the images), the smoke check 15, the
fixtures 10, each cycle 15 and each case 20. The whole run has a bound of
its own, `AICREW_SIM_BOUND_MINUTES` (90 by default; it is to be set from
the first green nightly runs). A watchdog beside the runner checks both
every 2 seconds. At either bound it stops the runner's commands, a hung
`docker compose exec` included, and the run fails with the bound it
crossed; the report and the logs are written as on any failure, and the
stack is removed unless `-keep` is given.

`AICREW_SIM_HANG=SCENARIO[:SECONDS]` tests this: the scenario (any after
the setup) starts with a command in sim-aicrewd that never returns, and
SECONDS, if given, is its bound. With `AICREW_SIM_HANG=cycle-1:60`, the
run fails about a minute into the first cycle, at "scenario cycle-1's
bound of 60s", and no container or volume is left.

## In CI

`.github/workflows/pilot-sim.yml` runs the simulation nightly on main, by
hand (`workflow_dispatch`) on any branch, and on a pull request that
carries the label `pilot-sim`, the gate the e2e (real aimem) workflow
uses. It never runs on every push. The job's timeout is 120 minutes, a
backstop behind the run's own bound. It keeps `DIR` as the artifact
`pilot-sim` for 14 days: the report, the events, the step snapshots, the
logs and the homes, without the staged binaries.

A second job, `pilot-sim-upgrade`, runs `scripts/pilot-sim.sh -upgrade`
on a runner of its own, so the nightly run takes no longer. It runs under
the same label and has the same not-requested check of its own
(`pilot-sim-upgrade (not requested)`). Its checkout is the whole history
with the tags, which the choice of the previous release reads. Its run
bound is 30 minutes (`AICREW_SIM_BOUND_MINUTES`) and its timeout 45. Its
artifact is `pilot-sim-upgrade`. On a host of its own, run the two one
after the other, never together: each needs the memory above.

## What I1 builds

`e2e/pilotsim/compose.yaml` runs one network and these containers:

| Container | What it is | How it is installed |
| --- | --- | --- |
| `sim-ca` | a one-shot job: the run's throwaway CA, a certificate for each service, and their SPKI pins | `simca` (`e2e/pilotsim/cmd/simca`), on the CA code the b4 harness uses (`e2e/testca`) |
| `sim-aimem` | `aimem serve`, the hub | the tested aimem release (`internal/agent/supported.json`), installed on the first start by that release's `install-hub.sh` in its install-only mode (`AIMEM_NO_SERVICE=1`) from the staged asset (`AIMEM_PREBUILT_DIR`, checked against the release's `SHA256SUMS`); the container then runs the serve command the installer prints on its last line. The introspection credential's file is added to the env file by hand, as aimem's hub runbook says |
| `sim-aicrewd` | `aicrewd -config …` | `install-aicrewd.sh` with `AICREW_NO_SERVICE=1` and `AICREW_PREBUILT_DIR`, from this tree's binaries; the container runs the command it prints |
| `sim-gitea` | Gitea 1.27 with TLS from the run's CA | the pinned image, configured by its own CLI and API |
| `sim-hands`, `sim-ci` | the stub reviewer and the stub CI ([below](#the-stub-reviewer-and-the-stub-ci)) | `simstub` (`e2e/pilotsim/cmd/simstub`) in one image, `e2e/pilotsim/stub/` |
| `sim-px-aimem`, `sim-px-crew`, `sim-px-peer`, `sim-px-hub` | the fault proxies ([below](#the-fault-cases)) | `simproxy` (`e2e/pilotsim/cmd/simproxy`) in one image, `e2e/pilotsim/proxy/` |
| `sim-coordinator`, `sim-worker`, `sim-indep` | one member home each, `aicrew-agent serve`; `sim-indep` is an independent member, whose steps the fault cases drive ([below](#the-direct-steps)) | `boot.sh` with `AICREW_PREBUILT_DIR`; aimem's verifying boot script taken at the tested tag, as `aicrew-agent check` instructs; no ai-skills; then docs/BRING-UP.md's preflight and join (member.sh, [below](#where-the-simulation-differs-from-the-document)), which installs the skills from the hub inside the team's conversation, as on a person's hosts (BRING-UP.md, step 6). Only the upgrade scenario's previous release, which has no such path, installs ai-skills from its release archive checked against `SHA256SUMS`, as that release's homes had them |

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
- the members trust aicrewd by `spki_sha256`; they and the hub reach
  aicrewd through a fault proxy that presents aicrewd's own key;
- the members' aimem checks the hub's certificate against the run's CA
  and its name: it reaches the hub through `sim-px-aimem`, which presents
  the hub's certificate, issued with `sim-px-aimem` as a second name;
- Gitea's certificate chains to the run's CA, which every container trusts
  through `SSL_CERT_FILE`, and the members' git through `GIT_SSL_CAINFO`.

## The operator's steps

`scripts/pilot-sim.sh` plays the operator, each step with the product's own
command on the host it belongs to. Where [BRING-UP.md](BRING-UP.md) writes
the step, the runner runs the document's own line: each is marked there
with a step id (`<!-- run: ID -->`, which leaves the rendered page as it
was), and the runner reads it with `e2e/docsteps` and fills in its
placeholders. A scenario declares the ids it runs
(`e2e/docsteps/scenarios.json`), and a full run fails at its end when it
did not run one of them; `go test ./e2e/docsteps` checks that the
scenarios together run every marked line but the exemptions below, and
parses every `aicrew` and `aicrew-agent` line with this tree's binaries.
The simulation uses the document's names: the team `crew`, aicrewd's hub
alias and the members' `main`, the aimem users `crew-coordinator`,
`crew-worker` and `crew-independent`, and each member home at join's
default, `~/aicrew/agents/LABEL`.
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
   - `aicrew status`, `aicrew hub add`, then a restart of aicrewd;
   - `aicrew team setup`, the document's grant run by the hub's admin,
     then setup again;
   - `aicrew team toolchain --team-name crew --project pilot --toolchain dev-go`,
     the toolchain step of docs/BRING-UP.md, with its check: `aicrew team
     show` must name `dev-go` for the project. It runs before any member
     joins, so each worker's launcher records the selection when it starts;
   - `aicrew hub-credential issue` for the hub's introspection credential.
4. Each stub's token in its own `/sim/in`, then the scenario file.
5. `aicrew team pause` for the bring-up, then per member:
   - an aimem user and a user token, `aicrew invitation issue`;
   - the member's input files, among them the document's lines it runs:
     `aicrew-agent version`, `aimem version`, the preflight and join;
   - the simulation's CA in the member's system trust;
   then `aimem access list` and `aicrew invitation list`.
6. After the smoke check, each member's `aicrew-agent join --home HOME`
   on its linked home, `aicrew status`, and `aicrew team resume`.

Every input a person would give is a file written before the member
starts: the invitation code, the aimem token, the forge token and the
hub's CA, under the names the document gives them (`invite.code`,
`member.token`, `forge.token`, `hub-ca.pem`). Each is owner-only in the
member's `/sim/in`, and the script then writes `ready` there. The member
runs the document's lines there. The member joins with them and removes them, then runs serve. As
an installed member's service does, the container starts serve again
10 seconds after a failure, but not after a clean stop or exit 2: a
member whose session refresh meets the hub stopped (case F6-unreachable)
comes back once the hub does. A clean stop ends the container. After
exit 2 the container stays up with serve stopped, as a host stays up when
its service stops, so check and an instruction still run in the home; the
smoke check fails at once on member.sh's line for it.

No secret is printed or passed as an argument. A token or code goes from
the container that makes it to the one that reads it through a pipe.

### Where the simulation differs from the document

Each difference is in one place in the scripts, named in the table.

| Difference | Where | Why |
| --- | --- | --- |
| join and its rerun run with `AICREW_NO_SERVICE=1` | `member/member.sh`, `mrejoin` in `scripts/pilot-sim.sh` | the containers have no systemd user manager; member.sh restarts serve as the installed service would |
| join runs with `AICREW_BASE` naming the export, when the member was installed from it | `member/member.sh` | the upgrade scenario's previous release is installed from the aiforge export, as on a closed network |
| the simulation's CA is added to each member's system trust | the members' loop in `scripts/pilot-sim.sh` | the document's `ca_dns` trusts the system roots; a host on a private CA has it there |
| aimem-timed is `/usr/local/bin/aimem`, ahead of the real aimem on PATH | `member/Dockerfile`, `member/aimem-timed` | join in the document's form records no aimem command, and the fault cases' gates need the wrapper |
| sim-indep joins with the document's pinned alternative (`spki_sha256`) | `member_lines` in `scripts/pilot-sim.sh` | one scenario runs the alternative the document marks |
| the below-minimum run's first join is the simulation's own | `member/member.sh` | its start, aicrew v0.11.0, predates the document's form (no preflight); it runs no member line of the document |
| aicrew-agent check runs with `--json` | `mcheck` in `scripts/pilot-sim.sh` | the runner reads the report; the flag is document text (step 8) |

The marked lines no scenario runs, each with its reason (also in
`e2e/docsteps/docsteps_test.go`), are still parsed:
- `aicrew-agent service restart` and `service status`: no systemd user
  manager in the members' containers;
- `loginctl enable-linger`: a systemd login setting of a real host;
- `aicrew-agent session leave`: named only in join's
  `skills_conversation` reason, which needs the hub to refuse a
  conversation for a session the home recorded earlier, a hub fault no
  harness injects.

## The smoke check

- Each member's serve logs that the turn model is available: the model
  probe passed, and no turn has run.
- `aicrew-agent check --json` exits 0 in each home, and
  `e2e/pilotsim/smoke.py` reads its report. It requires:
  - aimem supported;
  - the client supported, with its aimem MCP server connected and no
    required skill missing;
  - the turn model available;
  - a verified forge credential;
  - ai-skills supported at the tested release, as the record its
    installer writes (`.ai-skills.json`) names it, and installed where the
    client reads it.

  The reports are kept as `DIR/check-<role>.json`. In the upgrade
  scenario this check runs on the previous release, whose `check` knows
  only its own tested set, so there aimem and ai-skills may also be
  `newer` (`smoke.py check … previous`); everything else is required as
  above, and the check after the upgrade requires `supported`. Each check
  prints its mode on its line in the log.
- `aicrew status` lists the three members, each holding a session
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
  carries. In each turn of its running attempt it first reads the turn's
  settings (the file the launcher passes with `--settings`) and fails the
  turn when their allow and deny rules would not admit `go test ./...`:
  "the turn's settings deny `go test ./...`: no toolchain is selected for
  the project (`aicrew team toolchain`, docs/BRING-UP.md)". It makes the
  worktree with `aicrew-agent clone` and, in that turn only, runs
  `go version` there in the turn's environment, which fails the turn when
  it does not run (the member image has Go from the distribution's
  package). This proves the selection, the settings it gives the worker's
  turn and the tool's presence on the worker's host; the simulation still
  runs no project tests. A run without the selection fails while it waits
  for the worker's pull request; the runner's failure output ends with
  each member's last turn errors, read from its `logs/turns.jsonl`, where
  that message stands. Then it commits one file, pushes with the home's own
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

The done step waits for more than the finalize. The finalize's lifecycle
message wakes the coordinator again, seconds after the finalizing turn has
ended, so the runner waits until the members have acked every message
about the task and their turns have ended, each wait bounded at 120
seconds.

The merge is the runner's, as the Gitea `operator` person account.
`e2e/pilotsim/cycle.py` checks each snapshot against its step's row:
- aicrewd's store (a copy): the attempt's state and phase, its steps (all
  committed, no `release`), receipts, results, the accepted, delivered and
  finalized result, the watched pull request, the messages about the task,
  each acked once a turn read it, the members' turn reports, no member
  hold, no escalation on the task (case E1's one, answered by the
  architect, excepted) and none open, and an audit in which the operator's
  calls are the script's own and the members' are member operations;
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

## The upgrade scenario

`scripts/pilot-sim.sh -upgrade` answers the question a release has to
answer before it is tagged: does it upgrade from the last one? It runs the
setup, the smoke check and the fixtures as above, but on the previous
release; then one cycle, the upgrade, a second cycle, and a last read of
the first cycle.

**The previous release** is the newest `vX.Y.Z` tag merged into HEAD that
does not sit on HEAD itself (`simupgrade previous`,
`e2e/pilotsim/cmd/simupgrade`). On a release's own commit, which is main
right after the release, that is the release before it. Nothing has to be
moved after a release. `PILOTSIM_PREVIOUS=vX.Y.Z` overrides it for a run
by hand. Without such a tag (a shallow clone, for one), or when one of
that release's assets cannot be downloaded, the run fails and names the
tag; it never skips.

**The previous release is not built.** The runner downloads its release
assets, as its installers would (`SHA256SUMS`, `install-aicrewd.sh`,
`boot.sh` and the three Linux binaries), and lays them out as an aiforge
export (`MANIFEST`, `aicrew/<tag>/`). Beside it the export holds the tested
aimem release (`aimem/<tag>/`: `SHA256SUMS`, `boot.sh`, `install.sh`,
`aimem-opencode.ts` and the Linux binary), as a real export holds all its
products. sim-aicrewd and each member install from it with `AICREW_BASE`,
by that release's own installers, which check every file against its
`SHA256SUMS`, and each member joins with `AICREW_BASE` too, the documented
join in a closed network: the home records the export as `export_base`,
and its check's instructions name the export. The settings are I1's.

**The upgrade** (scenario `upgrade`) uses this tree's installers, with the
command line of the first install (`AICREW_NO_SERVICE=1`,
`AICREW_PREBUILT_DIR`), run again over the installed release. That is the
documented upgrade: run the newer release's installer again.
`AICREW_PREBUILT_DIR` holds this tree's build, which stands for a release
that does not exist yet. Every member is paused first, and its turn has
ended:
1. Before the upgrade, the runner reads the first cycle (snapshot
   `cycle-1/upgrade-before`) and checks that sim-aicrewd and every member
   run the previous release's binaries, by their digests.
2. aicrewd is stopped, this tree's files are copied into its container,
   and it is started. At the start `aicrewd.sh` runs the installer, while
   aicrewd is stopped as the installer's install-only upgrade asks, and
   then starts aicrewd. The installer keeps a backup of the store and of
   the configuration and the old binaries as `.prev`; the runner checks
   that they are there and that aicrewd runs this tree's build.
3. For each member, this tree's files are copied into its running
   container, `boot.sh` runs as the member, and the container restarts.
   The runner checks the new `aicrew-agent`'s digest, waits for serve's
   model probe after the restart, and runs `aicrew-agent check`, the
   release notes' step after an upgrade, through the smoke check's
   checks. `join` does not run again: the home keeps its `agent.json`.
4. After the upgrade the runner reads the first cycle again
   (`cycle-1/upgrade-after`), and resumes the members.

**The first cycle reads unchanged.** Both snapshots go through the cycle's
checks of the step done, and `simupgrade same` compares them, failing on
any difference. Its output is `DIR/upgrade-same-*.txt`, and its last line
is an observation in the report. It compares three places:
- **aicrewd's store**: the task's attempts, everything kept per attempt
  (its steps, results, pull request and its facts, handoff, block and
  automation runs; the delivery and terminal evidence are the attempt's
  own columns), the messages about the task or its attempts with their
  recipients, and the whole audit up to the upgrade;
- **aimem**: the hub's reads of the task, its comments, its history and its
  reservation;
- **the homes**: each member's turn log, guard log and handoff.

After the second cycle (scenario `upgrade-kept`) the first cycle is read a
third time (`cycle-1/after-cycle-2`) and compared with the read before the
upgrade. There the homes' logs need only start with what they held, and
the handoffs, which each attempt rewrites, are left out. The tool's tests
change each place alone and require the difference to be found there, so
a comparison that leaves a place out fails them.

**Where the simulation differs from a host.** aicrewd is its container's
own process, so the runner stops and starts the container where an
operator stops and starts the service; with `AICREW_NO_SERVICE=1` boot.sh
does not restart a member's serve, so the runner restarts the member's
container where an installed member's service restarts. The copy of this
tree's files into the containers stands for the download of the new
release. The installers' command lines are the documented ones,
unchanged, and nothing in a store, a home or a unit is edited by hand.

**From aimem below the minimum** (`scripts/pilot-sim.sh -upgrade
-below-minimum`, CI job `pilot-sim-upgrade-below-minimum`). The same
scenario, from a home whose aimem is below this tree's minimum at the
moment aicrew is upgraded, which is what a team meets when a release
raises the minimum and its members' aimem stays behind. The start is a
pinned pair, `simupgrade below-minimum`: aicrew v0.11.0, the last release
whose aimem minimum (0.10.0) was below this tree's (0.11.0), with aimem
0.10.25, the release v0.11.0 was tested with. The members' image is built
with that aimem; the hub keeps the tested one, as a team's hub is upgraded
first. The pair stays a valid start for as long as the minimum only rises,
and is looked at again when the minimum is next raised; a test of the tool
fails if the pinned aimem is ever not below this tree's minimum. The run
differs from the upgrade above in three places:
1. After each member's restart on the new `aicrew-agent`, what serve does
   is recorded (`DIR/upgrade-below-serve-ROLE.txt`: the model probe's
   outcome as serve logged it, available, could not be verified, or not
   available with serve's exit 2, or no probe line within 120 s; and
   whether serve runs, read both from the process list and from serve's
   own log line that its turn loop runs), not required. A serve that
   exited leaves the member's host up, so check and the instruction below
   still run. `aicrew-agent check` must exit
   non-zero, `blocked` with the reason `aimem_below`, aimem `below`, and
   this exact instruction (`smoke.py below`):

   ```text
   aimem 0.11.0 or later is required. Install aimem v0.11.5 (the release this aicrew is tested with) with its verifying installer, from the aiforge export /opt/aicrew-export: `cd "$(mktemp -d)" && AIMEM_BASE='/opt/aicrew-export' AIMEM_VERSION=v0.11.5 bash '/opt/aicrew-export/aimem/v0.11.5/boot.sh'`; then rerun `aicrew-agent check`
   ```

2. After the first cycle reads unchanged (`upgrade-after`), the
   coordinator, still blocked, is resumed and offered a turn by the
   operator's nudge. What follows within two minutes is recorded from its
   turn log, serve's log (`DIR/upgrade-below-turn.txt`) and a snapshot of
   the store and the homes (`cycle-1/upgrade-offered`); then it is held
   again.
3. Each member runs the command between the instruction's backticks, as
   printed and nothing else, and its check must then pass the smoke
   check's strict assertions. A member whose serve is not running then
   (it exited 2 after the restart) has its service started again, as the
   operator starts a stopped service once check is green, and its serve
   must pass the model probe within 300 s. The second cycle and the
   comparisons follow as above.

**Observed** in the first green run (2026-11-20, CI run 44709262641, the
job 14 minutes 49 seconds):
- With aimem 0.10.25, the members set up on v0.11.0 against the hub's
  aimem 0.11.5, and the first cycle passed. That cycle is what ran the old
  client against the new hub.
- After the restart on the new `aicrew-agent`, serve ran in each member:
  its model probe ran and found the model available, its process was
  found, and its log
  said its turn loop runs. Check was blocked on `aimem_below` in all
  three, with the exact instruction above.
- Nudged by the operator while check was blocked, the coordinator started
  a turn: its inbox's lifecycle message, finished, one message acked,
  0 steps. With no steps, the turn exercised the old aimem client no
  further than the inbox.
- The command between the instruction's backticks, run as printed in each
  member, brought check to `supported` with aimem 0.11.5, offline from
  the export. The first cycle's records read the same, and the second
  cycle and the kept-records check passed.

The scenario `upgrade` is bounded to 15 minutes and `upgrade-kept` to 5;
the others keep their bounds above. A green run on a 6-vCPU host took 15
minutes: the setup 2.2, a cycle 5 to 6, the upgrade itself half a minute.

Today the previous release and this tree have the same store schema, so
the upgrade migrates nothing, and the scenario proves the upgrade's
mechanics and that the records stay readable. The first change of the
schema makes it run a real migration. aicrewd's rows are compared on the
columns they had before the upgrade: a migration's author sees each added
column named in a `note:` line of `DIR/upgrade-same-*.txt`, and the run
stays green; a column that is gone or renamed, or a changed value, fails
it.

## The pilot cases

After the two cycles, `e2e/pilotsim/cases.sh` runs the pilot cases of
[DESIGN-PILOT-SIMULATION.md](DESIGN-PILOT-SIMULATION.md), section 6.2, in
the order P2, P5, P4, the escalation case E1 (below), P1 and, after the fault cases below, P3. Each is one fault on a cycle of its own, set
through the stubs' scenario or the member's: `/sim/scenario/member.json`
in each member's container, which only the script writes, and only while
the member is paused and its turn has ended. The member's scenario
(`e2e/pilotsim/cmd/aicrew-simclient`) can leave the messages unacked
(`never_ack`), push once while `hands-reviewing` is on
(`push_during_review`), do nothing in a working attempt (`stall`), end
every turn with the provider's 401 or 404 (`result`: `login` or `model`),
report a client cost (`cost_usd`), and, for the coordinator, raise an
escalation on a blocked attempt instead of answering it, then give the
worker the architect's decision once the answer arrives
(`escalate_blocked`). In the cases sim-hands reviews READY
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
| E1 | the worker stalls after its accept, and the coordinator escalates the block | blocked | as P4's |
| E1 | | answered | the five steps of "The escalation case" below; the worker then resumes and the cycle reaches DONE |
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

Four parts of the cases are gaps the board tracks, and the script checks
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
should show: P2 and P1 at their first check, and P5, P4, E1 and P3 when the
first head's CI failure, the block or the hold never comes. A case that passes without its
fault fails the run. The snapshots stay in `DIR/case-<case>/<point>/`, or
`DIR/case-<case>/<subcase>/` for a subcase such as F1-claim-request: no
snapshot is taken twice, so the report's diff of each step is its own.

### The operator case

O1 runs [BRING-UP.md](BRING-UP.md)'s "The operator during a cycle" by
its own lines, in sim-aicrewd, where the operator's commands run
(arrangement 1). The worker stalls and the coordinator raises an
escalation on the blocked attempt, as in E1. The operator reads the
team's state (the worker's attempt `blocked`, with its reason), finds the
escalation with `aicrew escalations list --open`, reads it with `show`
and answers it; the list then no longer has it, and the team's state
shows the attempt out of `blocked`. The operator pauses the team, the
state shows the pause and its reason, and resumes it. The cycle then ends
as every case's does: the coordinator's review, the operator's merge and
the delivery record, with the cycle's checks at the step done (the one
escalation on the task answered by the operator). No mirror is posted on
the task: the document asks none of the operator.

### The escalation case

E1 is the check of the architect role's feature
([DESIGN-CONTROL-PLANE.md](DESIGN-CONTROL-PLANE.md), section 7.7): a
running team raises an escalation, the architect is told by the wait and
answers, and the team acts on the answer, under the turns model.

**The architect's directory.** The compose service `sim-architect` runs
aicrewd's image, which holds this tree's `aicrew`, idle, with no volume:
the directory lives in the container and goes with `down -v`. At the
case's start the runner writes it there with `aicrew architect init --url
https://sim-aicrewd:9443` and the trust pin of aicrewd's key. The operator
issues the architect credential where the operator credential lives, in
sim-aicrewd, with `aicrew architect credential issue --output -`, and the
runner pipes it into the directory's `creds/aicrew.architect`, owner-only.
The operator credential never enters sim-architect. The runner plays the
architect session: it runs the session's commands in the directory with the
environment the directory's `.claude/settings.json` gives a session.

**The run.** As in P4, the worker stalls and the launcher blocks the
attempt. Under the member scenario `escalate_blocked`, the coordinator
raises an escalation on the blocked attempt with `aicrew-agent escalate`,
naming the worker as blocked. The worker's reservation holds the task, and
aimem takes no comment from the coordinator on a held task, so the mirror
on the task waits until the hold ends, and the coordinator's handoff says
so (DESIGN-CONTROL-PLANE.md, section 7.7); it posts the mirror after the
finalize. Until then the request is not on the board, so an architect
without a connection to aicrewd, who reads the `[escalation.request `
comments, would see nothing of it before the delivery. The runner has
started `aicrew escalations wait --interval 15` in the directory before
the raise, and has resumed both members, so that it makes no operator call
between the raise and the answer: it neither pauses, nudges nor takes a
snapshot there. When the wait returns, the runner shows the escalation,
answers it with `aicrew escalations answer`, and mirrors the answer on the
task. The answer's message wakes both members; the coordinator gives the
worker the decision with its `answer` step, and the cycle goes on to DONE.

**The five steps** `cases.py` asserts at the point `answered`:

1. The escalation on the task: raised by the coordinator on the attempt,
   naming the worker as blocked, and the coordinator's handoff naming the
   mirror that waits for the hold (the task's `[escalation.request ID]`
   comment is checked at the step done, with the cycle's checks).
2. The wait exited 0 with that escalation's line, within 30 seconds (two
   intervals) of its `created_at`, and no `aicrew escalations list` or
   `show` ran between the raise and the wait's return.
3. `answered_by` names the case's architect credential (`architect:<ID>`,
   label `sim-e1`), the answer carries the decision, and the task has the
   `[escalation.answer ID]` comment.
4. The answer's message to the coordinator and to the worker, each
   acknowledged, each the trigger of a turn in the member's
   `logs/turns.jsonl`; the coordinator's `attempt.answer`, and the attempt
   no longer blocked.
5. No call of the operator credential in aicrewd's audit between the raise
   and the answer, and the directory's `creds/` holding
   `aicrew.architect` alone. That the members' only inputs are inbox
   messages is not asserted. It follows from headless turns, whose every
   trigger is an inbox message (DESIGN-CONTROL-PLANE.md 1.2, D1). The
   cycle's own check is that every turn finished and the guard logs stayed
   empty.

**What rests on the runner's log.** aicrewd's audit records commands, not
reads. That no list or show ran between the raise and the wait's return
is read from the runner's own log of the architect's commands
(`case-E1/architect.log`, in the snapshot's `facts.json`), not from a
record of aicrewd; the wait's return time is the runner's clock, which
the same host shares with aicrewd.

## The fault cases

After P1, `e2e/pilotsim/faults.sh` runs b4's fault cases on the stack, in
the order F1, F2, F3, F4, F5, F6, F7, before P3. They adapt the b4 harness's
(`e2e/realaimem`) cases to the containers: where b4 runs a proxy in the
test process and kills processes, the simulation arms a rule on a fault
proxy and kills containers.

### The fault proxies

`simproxy` (`e2e/pilotsim/cmd/simproxy`) stands on four of the TLS links
of [DESIGN-PILOT-SIMULATION.md](DESIGN-PILOT-SIMULATION.md), section 6.1:

| Proxy | The link | Its certificate |
| --- | --- | --- |
| `sim-px-aimem` | the members' aimem to the hub | the hub's, issued for `sim-aimem` and `sim-px-aimem`: the members name `sim-px-aimem` as their hub, and their aimem checks the name against the run's CA |
| `sim-px-crew` | the members to aicrewd | aicrewd's, its name among the leaf's; sim-indep pins it by key, the others trust it by `ca_dns` |
| `sim-px-peer` | the hub to aicrewd, its peer endpoint | aicrewd's, which the hub pins by key |
| `sim-px-hub` | aicrewd to the hub, its peer lookups | the hub's, also issued for `sim-px-hub`: aicrewd's hub binding names it, and aicrewd pins the hub by key |

Each terminates its clients' TLS with its target's own certificate and
key, opens its own TLS to the target trusting only the run's CA, and
forwards each HTTP/1.1 request unchanged. The runner arms a rule with
`simproxy arm` in the proxy's container, whose control API listens on the
container's loopback only. A rule fires on the next request that matches
its method and path, and is then removed, unless `-persist` keeps it until
`simproxy disarm`:
- `drop_request` closes the connection before forwarding, so the target
  never sees the request;
- `drop_reply` forwards the request, reads the whole answer, then closes
  the connection without a byte of it;
- `delay_reply` holds the answer for a time.

`simproxy fired` says whether a rule fired, and `simproxy log` prints the
request log: method, path, the target's status and what the proxy did,
never a header, query or body. Each snapshot keeps the four logs in
`proxy/`. `sim-px-crew` also keeps the coordination proofs it sees, owner
only, for F7.

### The scripted member's recovery

The scripted member recovers from what the cases fault as a model would,
on its next turn:
- a step aimem did not commit, or one still pending, ends the turn without
  acknowledging its messages, so the next turn reads them again and begins
  the step again;
- a submit aimem left undecided is recorded in the worker's handoff with
  its request key, and the next submit supersedes it;
- a step that failed on its way to aicrewd is finished by
  `aicrew-agent step recover`, which replays it under its recorded key.

The runner pauses a member as soon as its request is dropped, so it does
not begin the step again under the rule. Once aicrewd has settled the
step, the runner disarms the rule, arms the next one and nudges the
member.

### The direct steps

F2-settle, F3-launcher, F4 and F5 take steps themselves, as b4's harness
does, rather than through a member's turn. The runner runs `aicrew-agent
step` in a member's container, with the step's body on its standard input,
and keeps its answer and exit as facts of the case. Both scripted members
are held for these parts, so that no turn of theirs takes a step on the
cases' tasks. Each part files a task of its own, ends it as b4 does (the
offer withdrawn, or the running attempt stopped and released), and the
operator then cancels it on the board. A claim comes from `sim-indep`, the
third member: aicrewd gives it the role `independent`, the coordinator's
offers go to the worker only, and its scripted client takes no step of its
own.

Each member's home runs aimem through `aimem-timed`
(`e2e/pilotsim/member/aimem-timed`), b4's wrapper, which the image
installs as `aimem` ahead of the real one on PATH. It logs each aimem command's leading arguments and
exit, never its input. The runner sets its gates with files in the
member's gate directory: hold a command before or after it runs, or keep
its input, owner only, so that F5 can replay a coordination proof inside
the container. The proof never leaves it.

### The cases

Each part is a skip-the-fault case of its own. `cases.py` checks it at
its point:

| Case | Fault | Point | What it shows |
| --- | --- | --- | --- |
| F1-claim-request | `sim-px-aimem` drops the coordinator's claim before the hub | request | the hub saw no claim; aicrewd closed the attempt, not committed; nothing is held |
| F1-claim-reply | `sim-px-aimem` drops the claim's reply after the hub committed it | reply | the coordinator offered again; its driver read aimem's receipt after the lost reply and settled the claim committed; the coordinator holds the task |
| F1-transfer-request | the same for the worker's transfer | request | the transfer settled not committed; the attempt is still offered, and the coordinator holds the task |
| F1-transfer-reply | | reply | the worker accepted again; the receipt reconciled the transfer; the worker holds the task |
| F1-update-request | the same for the worker's submit | request | the update stays pending, undecided; the task is still READY; the worker's handoff records the request key |
| F1-update-reply | | reply | the worker's next submit superseded the pending one; the receipt reconciled it; the task is in REVIEW |
| F1-finalize-request | the same for the coordinator's finalize | request | the finalize settled not committed; the attempt is still accepted, the task in REVIEW |
| F1-finalize-reply | | reply | the coordinator delivered again; the receipt reconciled the finalize; the task is DONE |
| F2-begin | `sim-px-crew` drops the reply to the offer's begin | begin | the coordinator saw the step fail, and `step recover` replayed the begin to the one attempt the lost reply began |
| F2-settle | `sim-px-crew` drops the reply to the settle of `sim-indep`'s claim | settle | the claim saw its settle fail; `step recover` settled it, once: the attempt runs, with one committed claim and no step left recorded |
| F3-launcher | `sim-indep`'s container is killed after aimem committed its claim, before the settle | launcher | aicrewd's reconciler settled the claim within 45 seconds, and the attempt runs; the restarted launcher has no step recorded |
| F3-aicrewd | `sim-px-aimem` holds the hub's answer to the claim for 20 seconds while `sim-aicrewd` is killed and started | restart | aicrewd's container started again, and the claim settled committed after the start |
| F3-hub | `sim-aimem` is killed and started between steps | restart | the hub's container started again, with its hold as it was; the cycle's next steps then reach DONE |
| F4-second | `sim-indep` claims a task the coordinator offered | second | the claim is refused `task_busy` at its begin; the task has one attempt |
| F4-busy | the coordinator offers a second task to the worker, whose first offer is open | busy | the offer is refused `agent_busy` at its begin |
| F4-race | an offer and a claim of one task, begun at once | race | exactly one commits and the other is refused, at its begin or by aimem; one attempt is open, and the task is held |
| F4-lost | (the check itself) | race | the race's check never takes a step lost before its commit for a refusal |
| F5-stale | the coordinator's claim is held before it is sent while the hub's admin edits the task | stale | aimem refused the claim begun at the old revision (exit 3), and the offer begun again at the new revision holds the task; a plain edit of a held task is refused with 409 |
| F5-replay | the proof of a settled offer, replayed under a new key | replay | aimem refuses it |
| F5-resume | `sim-coordinator`'s container is killed under its offer, its claim held before it is sent | resume | the old generation's proof is refused; the new generation may not continue the offer; aicrewd settles it not committed within 45 seconds, and the worker is free again |
| F5-delay | `sim-px-peer` delays aicrewd's coordination answer by 3 seconds, past aimem's budget | delay | aimem refused the claim as retryable (exit 4), and the step's retry committed |
| F6-release | the hub's admin recovers the running attempt's hold (`aimem reservation recover release`) | recovered | aicrewd closed the attempt as recovered by `recovery_release`, with the closing fence; the task is READY (the operator then cancels it, so that no turn offers it again) |
| F6-cancel | the same with `recover cancel` | recovered | the same by `recovery_cancel`; the task is CANCELLED |
| F6-unreachable | after the same release, `sim-px-hub` drops aicrewd's reads of the hub's peers, and `sim-aimem` is stopped for 65 seconds, four of aicrewd's ticks | unreachable | nothing closed the attempt while the hub was unreachable; once the hub is back, aicrewd closes it as recovered |
| F7-leak | (the check itself) | scan | none of the run's secrets appears in the containers' logs, the run's reports and snapshots, or aicrewd's audit |

F3-aicrewd and F3-hub restart their service with `dc kill`, then `dc start`
(`e2e/pilotsim/restart.sh`). Compose was seen to return 0 from the start
without starting the service, after running the one-shot sim-ca again; the
cause is not known. So the helper reads whether the service's container
was started at all after the kill: whether Docker's start time for it
moved. When it did not, the run's output says that the start was skipped,
and the service is started once more. A container that was started and
then stopped is not started again: that is the product's, and the health
wait judges it. When that second
start does not start it either, the run fails, naming the skipped start
rather than the service's health. On a skipped start, the project's own
container events from the kill on are kept in
`DIR/restart-SERVICE-N.events`. What F3 proves is still judged by the
service's health after the start. `bash e2e/pilotsim/restart_test.sh`
tests the helper with stand-ins, and CI runs it.

F1, F2 and F3 end with the cycle's checks at the step done. For F1 these
accept the attempt its lost claim closed and the steps its lost requests
left not committed. F6 files a task for each verb and keeps its snapshots
in `DIR/case-F6/<verb>/`. F7 knows these secrets, at least 18 in all:
- the secrets the runner moved into the containers;
- the hub's admin token;
- aicrewd's operator token, and every peer credential the hub provisioned
  for aicrewd;
- the coordination proofs;
- the members' aimem session handles.

The runner keeps them in `DIR/.known`, owner only, and removes them when
the run ends. Without its fault, F7 plants every known secret in a
scanned log, and the scan must find each one there.

Without its fault, a request case fails where the lost request's effect
never comes; a reply case and F2-begin fail their checks, for want of the
dropped reply and the recovery; F3's parts fail for want of a restart, and
F6's when the attempt is never closed. Without their faults:
- F2-settle's claim sees no failed settle;
- F3-launcher's claim is settled by its launcher, not by the reconciler;
- F4-second and F4-race file a second task, so the claim or the race meets
  no open attempt and nothing is refused;
- F4-busy's first offer is withdrawn, so the worker is free;
- F4-lost makes the race's loser answer as a step lost before its commit,
  and the race's check must fail on it;
- F5-stale's claim is never held, so the task is not edited under it;
- F5-replay and F5-resume replay a live proof under its own step's key,
  which aimem accepts (b4's control);
- F5-delay's coordination answer comes in time;
- F6-unreachable's hub stays reachable, and the attempt closes during the
  65 seconds.

Each such run first runs the parts of its case before the skipped one,
with their faults.
