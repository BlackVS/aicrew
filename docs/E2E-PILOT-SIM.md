# The pilot simulation

The pilot simulation runs the team cycle in containers, with no person or
agent typing into it. The design is [DESIGN-PILOT-SIMULATION.md](DESIGN-PILOT-SIMULATION.md).
This document says how to run it, and what each increment adds.

**I1** (task `01a17d9a-932b`) is the stack: every party installed and
joined the way the operator installs and joins it, and a smoke check. The
cycle, the scripted roles, the stub reviewer and CI, the faults, the report
and the CI workflow come in I2 to I6.

## Running it

On Linux with Docker (on a Windows host: WSL with Docker Desktop's WSL
integration switched on for the distribution):

```sh
scripts/pilot-sim.sh [-keep] [-out DIR]
```

- `-out` is where the staged image contexts, the logs and the reports go
  (default `.work/pilotsim` in the repository).
- `-keep` leaves the stack up after the run, for a look inside with
  `docker compose -f e2e/pilotsim/compose.yaml …`. Without it the stack and
  its volumes are removed at the end, green or not.

It needs Docker with Compose v2, Go 1.26 or later, Python 3, and network
access: the images install aimem and ai-skills from their GitHub releases,
as an operator's host does.

The script exits 0 when the stack is healthy. Otherwise it exits non-zero
and prints the last lines of every container's log; the full logs are in
`DIR/logs/`.

## What I1 builds

`e2e/pilotsim/compose.yaml` runs one network and these containers:

| Container | What it is | How it is installed |
| --- | --- | --- |
| `sim-ca` | a one-shot job: the run's throwaway CA, a certificate for each service, and their SPKI pins | `simca` (`e2e/pilotsim/cmd/simca`), on the CA code the b4 harness uses (`e2e/testca`) |
| `sim-aimem` | `aimem serve`, the hub | the tested aimem release (`internal/agent/supported.json`), its asset checked against the release's `SHA256SUMS` as `install-hub.sh` checks it, then the installer's env file and `aimem serve` under the container. `install-hub.sh` itself needs systemd; its service-less mode is aimem task `01a18163-87ac` |
| `sim-aicrewd` | `aicrewd -config …` | `install-aicrewd.sh` with `AICREW_NO_SERVICE=1` and `AICREW_PREBUILT_DIR`, from this tree's binaries; the container runs the command it prints |
| `sim-gitea` | Gitea 1.27 with TLS from the run's CA | the pinned image, configured by its own CLI and API |
| `sim-coordinator`, `sim-worker` | one member home each, `aicrew-agent serve` | `boot.sh` with `AICREW_PREBUILT_DIR`; aimem's verifying boot script and the ai-skills release archive checked against `SHA256SUMS`, both as `aicrew-agent check` instructs; then `aicrew-agent join … --wake turns --no-service --code-file …` |

The member's client is `aicrew-simclient` (`e2e/pilotsim/cmd/aicrew-simclient`),
installed as `claude` on the image's `PATH`, where a member finds its
client. It answers only what `aicrew-agent check` and serve's model probe
ask:
- `--version` gives the tested Claude Code release;
- `mcp list` and the init event show what the home wires. The client finds
  it the way Claude Code does, and checks the aimem server with a real MCP
  initialize;
- the model probe gets one finished turn, with no model call.

Anything else exits 2 ("not simulated").

TLS is real everywhere:
- aicrewd and the hub pin each other's SPKI;
- the members trust aicrewd by `spki_sha256`;
- Gitea's certificate chains to the run's CA, which every container trusts
  through `SSL_CERT_FILE`.

## The operator's steps

`scripts/pilot-sim.sh` plays the operator, each step with the product's own
command on the host it belongs to:
1. On the hub: `aimem tasks on`, `aimem process select` (the b4 harness's
   process repository: a local commit behind an https name),
   `aimem project repo set`, and `aimem identity peer provision`. The
   peer's credential directory is then copied to aicrewd's host.
2. On aicrewd's host:
   - `aicrew hub add`, then a restart of aicrewd;
   - `aicrew team setup`, with the grant it prints run by the hub's admin,
     then setup again;
   - `aicrew hub-credential issue` for the hub's introspection credential.
3. In Gitea: the `operator` account and one account per member, each with
   its own token, and the repository with both members as collaborators.
4. Per member:
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
  - ai-skills installed where the client reads it.

  The reports are kept as `DIR/check-<role>.json`. ai-skills 1.26.1, the
  tested release, records no version (aiskills#24), so check reports it
  `unknown`, and the smoke check accepts that until task `01a18173-b005`
  moves the tested release to one that records it.
- `aicrew status` lists both members, each holding a session
  (`DIR/status.json`).
- `verify_delivery.py --help` runs from the coordinator's skill directory.
