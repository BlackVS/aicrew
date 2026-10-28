#!/usr/bin/env bash
# The pilot simulation (docs/E2E-PILOT-SIM.md; design
# docs/DESIGN-PILOT-SIMULATION.md). On Linux with Docker:
#
#   scripts/pilot-sim.sh [-keep] [-out DIR]
#
# It builds this tree's binaries, stages the image contexts, brings the
# compose stack up, does the operator's steps (the hub's project, process
# and peer; aicrewd's hub and team; Gitea's accounts and repository; each
# member's aimem user, token and invitation), hands each member its input
# files, and runs the smoke check. It brings the stack down and removes its
# volumes unless -keep is given. Exit 0: a healthy stack; otherwise non-zero
# with the containers' logs.
#
# No secret is printed or passed as an argument: tokens and codes go from
# the container that makes them to the one that reads them through a pipe,
# into owner-only files.
set -Eeuo pipefail
# fail writes here, also from a check whose output waitfor discards.
exec 3>&2

usage() { echo "usage: scripts/pilot-sim.sh [-keep] [-out DIR]" >&2; exit 2; }
KEEP=
OUT=
while [ $# -gt 0 ]; do
  case "$1" in
    -keep) KEEP=1 ;;
    -out) [ $# -ge 2 ] || usage; OUT=$2; shift ;;
    *) usage ;;
  esac
  shift
done

ROOT=$(cd "$(dirname "$0")/.." && pwd)
OUT=${OUT:-$ROOT/.work/pilotsim}
mkdir -p "$OUT"
OUT=$(cd "$OUT" && pwd)
STAGE=$OUT/stage
COMPOSE=$ROOT/e2e/pilotsim/compose.yaml
LOGS=$OUT/logs
SIM=$ROOT/e2e/pilotsim
for tool in docker go python3; do
  command -v "$tool" >/dev/null || { echo "pilot-sim: $tool is required" >&2; exit 2; }
done

# The tested releases, from aicrew's supported set.
tested() {
  python3 -c 'import json,sys; r=json.load(open(sys.argv[1]))["components"][sys.argv[2]]["ranges"]; print(r[-1]["tested"])' \
    "$ROOT/internal/agent/supported.json" "$1"
}
PILOTSIM_AIMEM_VERSION=$(tested aimem)
PILOTSIM_AISKILLS_VERSION=$(tested ai-skills)
export PILOTSIM_STAGE=$STAGE PILOTSIM_AIMEM_VERSION PILOTSIM_AISKILLS_VERSION

dc() { docker compose -f "$COMPOSE" "$@"; }
step() { printf '==> %s\n' "$*"; }

# ---- staging -----------------------------------------------------------------
stage() {
  rm -rf "$STAGE"
  mkdir -p "$STAGE"
  cp -r "$SIM/ca" "$SIM/hub" "$SIM/aicrewd" "$SIM/member" "$STAGE/"
  mkdir -p "$STAGE/aicrewd/prebuilt" "$STAGE/member/prebuilt" "$STAGE/hub/aimem-release"
  local arch
  arch=$(docker version --format '{{.Server.Arch}}')
  build() { (cd "$ROOT" && GOOS=linux GOARCH=$arch CGO_ENABLED=0 go build -trimpath -o "$1" "$2"); }
  step "building this tree's binaries (linux/$arch)"
  build "$STAGE/aicrewd/prebuilt/aicrewd" ./cmd/aicrewd
  build "$STAGE/aicrewd/prebuilt/aicrew" ./cmd/aicrew
  build "$STAGE/member/prebuilt/aicrew-agent" ./cmd/aicrew-agent
  build "$STAGE/member/prebuilt/aicrew" ./cmd/aicrew
  build "$STAGE/member/claude" ./e2e/pilotsim/cmd/aicrew-simclient
  build "$STAGE/ca/simca" ./e2e/pilotsim/cmd/simca
  (cd "$STAGE/member/prebuilt" && sha256sum aicrew-agent aicrew > SHA256SUMS)
  cp "$ROOT/install-aicrewd.sh" "$STAGE/aicrewd/"
  cp "$ROOT/boot.sh" "$STAGE/member/"
  step "fetching aimem v$PILOTSIM_AIMEM_VERSION and its install-hub.sh (the installer checks the asset in sim-aimem)"
  local base="https://github.com/BlackVS/aimem/releases/download/v$PILOTSIM_AIMEM_VERSION"
  curl -fsSL "$base/aimem-linux-$arch" -o "$STAGE/hub/aimem-release/aimem-linux-$arch"
  curl -fsSL "$base/SHA256SUMS" -o "$STAGE/hub/aimem-release/SHA256SUMS"
  curl -fsSL "https://raw.githubusercontent.com/BlackVS/aimem/v$PILOTSIM_AIMEM_VERSION/install-hub.sh" \
    -o "$STAGE/hub/install-hub.sh"
}

# ---- failure and teardown ----------------------------------------------------
collect() {
  mkdir -p "$LOGS"
  local s
  for s in $(dc ps -a --services 2>/dev/null || true); do
    dc logs --no-color "$s" > "$LOGS/$s.log" 2>&1 || true
  done
}
fail() {
  # In a command substitution, leave the report to the script itself.
  [ "$BASHPID" = "$$" ] || exit 1
  trap - ERR
  echo "pilot-sim: FAILED: $*" >&3
  collect
  dc ps -a >&3 2>&1 || true
  local s
  for s in $(dc ps -a --services 2>/dev/null || true); do
    echo "---- $s (last 40 lines; all in $LOGS/$s.log)" >&3
    tail -n 40 "$LOGS/$s.log" >&3 || true
  done
  exit 1
}
finish() {
  local code=$?
  if [ -z "$KEEP" ]; then
    dc down -v --remove-orphans >/dev/null 2>&1 || true
  else
    echo "pilot-sim: -keep: the stack is still up; inspect it with:" >&3
    echo "  PILOTSIM_STAGE=$STAGE PILOTSIM_AIMEM_VERSION=$PILOTSIM_AIMEM_VERSION" \
      "PILOTSIM_AISKILLS_VERSION=$PILOTSIM_AISKILLS_VERSION docker compose -f $COMPOSE ps" >&3
  fi
  exit "$code"
}

# waitfor WHAT SECONDS CMD...: runs CMD until it succeeds, at most SECONDS.
waitfor() {
  local what=$1 limit=$2 i
  shift 2
  for ((i = 0; i < limit; i += 2)); do
    if "$@" >/dev/null 2>&1; then return 0; fi
    sleep 2
  done
  fail "$what did not happen within ${limit}s"
}

# ---- the parties' commands -----------------------------------------------------
# The hub's admin, on the hub's host.
hub() { dc exec -T -u aimem -e HOME=/home/aimem -e PATH=/home/aimem/.local/bin:/usr/bin:/bin sim-aimem "$@"; }
identity() {
  hub aimem identity "$@" --hub https://sim-aimem:8440 --admin-token-file /home/aimem/admin.token \
    --hub-ca-file /sim/tls/ca.pem
}
# The operator, on aicrewd's host.
OPS_HOME=/home/sessiond
op() {
  dc exec -T -u sessiond -e HOME=$OPS_HOME -e PATH=$OPS_HOME/aicrew/bin:/usr/local/bin:/usr/bin:/bin \
    -e AICREW_URL=https://sim-aicrewd:9443 \
    -e AICREW_TLS_TRUST_MODE=spki_sha256 -e "AICREW_TLS_TRUST_VALUE=$PIN_SIM_AICREWD" \
    -e AICREW_OPERATOR_TOKEN_FILE=$OPS_HOME/aicrew/etc/operator.token sim-aicrewd "$@"
}
# into SERVICE USER PATH: standard input into an owner-only file there.
into() {
  dc exec -T -u "$2" "$1" sh -c 'umask 077 && cat > "$1"' sh "$3"
}
gitea() { dc exec -T -u git sim-gitea gitea --config /data/gitea/conf/app.ini "$@"; }
json_field() { python3 -c 'import json,sys; print(json.load(sys.stdin)[sys.argv[1]])' "$1"; }

trap finish EXIT
trap 'fail "line $LINENO exited $?"' ERR
# A stack an earlier -keep run left up would refuse this run's accounts.
dc down -v --remove-orphans >/dev/null 2>&1 || true
stage
step "building the images"
dc build --quiet || fail "the images did not build"
step "starting sim-ca, sim-aimem, sim-aicrewd and sim-gitea"
dc up -d --wait --wait-timeout 300 sim-aimem sim-aicrewd sim-gitea || fail "the services did not become healthy"
set -a
eval "$(hub cat /sim/tls/pins.env | grep -E '^PIN_[A-Z_]+=sha256-[A-Za-z0-9+/]+=*$')"
set +a
[ -n "${PIN_SIM_AIMEM:-}" ] && [ -n "${PIN_SIM_AICREWD:-}" ] || fail "sim-ca wrote no pins"

# ---- the hub: project, process, repository, peer ---------------------------------
step "the hub: the project, its process and repository, and aicrewd's peer"
PROJECT=pilot
REPO_URL=https://sim-gitea:3000/operator/pilot.git
hub aimem tasks on -p "$PROJECT" >/dev/null
COMMIT=$(hub git -C /home/aimem/process rev-parse HEAD)
hub aimem process select https://git.example.test/sim/process.git "$COMMIT" process/manifest.json -p "$PROJECT" >/dev/null
hub aimem project repo set --project "$PROJECT" --kind gitea --url "$REPO_URL" --access write >/dev/null
identity peer provision aicrew-service --endpoint https://sim-aicrewd:9443/v1/crew/introspect \
  --peer-trust-pin "$PIN_SIM_AICREWD" --output-dir /home/aimem/peer --expires 30d >/dev/null
# The peer's credentials move to aicrewd's host as the operator moves them.
hub tar -C /home/aimem -cf - peer |
  dc exec -T -u root sim-aicrewd sh -c 'umask 077 && tar -C /home/sessiond -xf - && chown -R sessiond:sessiond /home/sessiond/peer'
hub rm -rf /home/aimem/peer
HUB_ID=$(op cat $OPS_HOME/peer/aimem-hub-id)

# ---- aicrewd: the hub, the team, the introspection credential ----------------------
step "aicrewd: aicrew hub add, team setup and the hub's introspection credential"
op aicrew hub add sim --config $OPS_HOME/aicrew/etc/aicrewd.json --base-url https://sim-aimem:8440 \
  --tls-trust-mode spki_sha256 --tls-trust-value "$PIN_SIM_AIMEM" --cred-dir $OPS_HOME/peer >/dev/null
dc restart sim-aicrewd >/dev/null
waitfor "aicrewd's restart" 60 op curl -fs --cacert /sim/tls/ca.pem https://sim-aicrewd:9443/healthz
TEAM=sim
setup_out=$(op aicrew team setup "$TEAM" --hub sim --project "$PROJECT" || true)
grant=$(printf '%s\n' "$setup_out" | sed -n 's/^[[:space:]]*aimem identity team grant //p')
[ -n "$grant" ] || fail "aicrew team setup printed no grant: $setup_out"
# shellcheck disable=SC2086 # the grant's words, as printed
identity team grant $grant >/dev/null
op aicrew team setup "$TEAM" --hub sim --project "$PROJECT" >/dev/null || fail "aicrew team setup, second run"
TEAM_ID=$(op aicrew team show -team-name "$TEAM" | json_field id)
op aicrew hub-credential issue -hub "$HUB_ID" --output $OPS_HOME/introspection.token >/dev/null
op cat $OPS_HOME/introspection.token | into sim-aimem aimem /home/aimem/introspection.token
op rm -f $OPS_HOME/introspection.token

# ---- Gitea: the accounts, their tokens, the repository -------------------------------
step "Gitea: the operator and the members, their tokens and the repository"
for user in operator sim-coordinator sim-worker; do
  admin=()
  [ "$user" = operator ] && admin=(--admin)
  gitea admin user create --username "$user" --email "$user@example.test" --random-password \
    --must-change-password=false "${admin[@]}" >/dev/null
done
gitea admin user generate-access-token --username operator --token-name sim --scopes all --raw |
  into sim-aicrewd sessiond $OPS_HOME/forge-operator.token
forge() { # METHOD PATH [JSON]: the operator's Gitea API call, from aicrewd's host
  op sh -c 'curl -fsS --cacert /sim/tls/ca.pem -X "$1" -H "Authorization: token $(cat "$HOME/forge-operator.token")" \
    -H "Content-Type: application/json" ${3:+-d "$3"} "https://sim-gitea:3000/api/v1$2"' sh "$@"
}
forge POST /user/repos '{"name":"pilot","auto_init":true,"default_branch":"main"}' >/dev/null
for user in sim-coordinator sim-worker; do
  forge PUT "/repos/operator/pilot/collaborators/$user" '{"permission":"write"}' >/dev/null
done

# ---- the members ---------------------------------------------------------------------
step "the members: aimem users and tokens, invitations, and their input files"
dc up -d sim-coordinator sim-worker >/dev/null
for role in coordinator worker; do
  svc=sim-$role
  uid=$(hub aimem access user-add --user-name "sim-$role" | json_field id)
  expiry=$(date -u -d '+30 days' +%Y-%m-%dT%H:%M:%SZ)
  hub aimem access token-issue-user --user-id "$uid" --label "sim-$role" --expires "$expiry" \
    --output "/home/aimem/member-$role.token" >/dev/null
  hub cat "/home/aimem/member-$role.token" | into "$svc" member /sim/in/aimem.token
  hub rm -f "/home/aimem/member-$role.token"
  gitea admin user generate-access-token --username "sim-$role" --token-name sim --scopes all --raw |
    into "$svc" member /sim/in/forge.token
  op aicrew invitation issue -team "$TEAM_ID" -role "$role" -hub "$HUB_ID" -label "$role" -expect-user "$uid" \
    --output "$OPS_HOME/invite-$role" >/dev/null
  op cat "$OPS_HOME/invite-$role" | into "$svc" member /sim/in/invite
  op rm -f "$OPS_HOME/invite-$role"
  dc exec -T -u member "$svc" touch /sim/in/ready
done

# ---- the smoke check -------------------------------------------------------------------
step "the smoke check"
# served SERVICE: its serve passed the model probe; a member that exited
# (a failed join) fails the run at once.
served() {
  if dc ps -a --status exited --services 2>/dev/null | grep -qx "$1"; then
    fail "$1 exited before serving"
  fi
  dc logs --no-color "$1" 2>&1 | grep -q 'the turn model is available'
}
for role in coordinator worker; do
  waitfor "sim-$role's serve and its model probe" 600 served "sim-$role"
  dc exec -T -u member "sim-$role" aicrew-agent check --home /home/member/agent --json > "$OUT/check-$role.json" ||
    fail "aicrew-agent check is not green in sim-$role (report: $OUT/check-$role.json)"
  python3 "$SIM/smoke.py" check "$OUT/check-$role.json" "$PILOTSIM_AISKILLS_VERSION" || fail "sim-$role's check report ($OUT/check-$role.json)"
done
op aicrew status --team "$TEAM_ID" > "$OUT/status.json" || fail "aicrew status"
python3 "$SIM/smoke.py" status "$OUT/status.json" coordinator worker || fail "aicrew status ($OUT/status.json)"
dc exec -T -u member sim-coordinator sh -c \
  'python3 "$HOME/.claude/skills/verify-delivery/verify_delivery.py" --help >/dev/null' ||
  fail "verify_delivery.py --help does not run in sim-coordinator"
collect
step "pilot-sim: green: both members checked, listed and serving (reports in $OUT)"
