#!/usr/bin/env bash
# The pilot simulation (docs/E2E-PILOT-SIM.md; design
# docs/DESIGN-PILOT-SIMULATION.md). On Linux with Docker:
#
#   scripts/pilot-sim.sh [-keep] [-out DIR]
#
# It builds this tree's binaries, stages the image contexts, brings the
# compose stack up, does the operator's steps (the hub's project, process
# and peer; aicrewd's hub and team; Gitea's accounts, labels and
# repositories; each member's aimem user, token and invitation), hands each
# member and stub its input files, and runs the smoke check, the stubs'
# fixture pull requests and the cycle up to the submitted attempt, checking
# each step read-only (e2e/pilotsim/cycle.py). It brings the stack down and
# removes its volumes unless -keep is given. Exit 0: a healthy stack whose
# stubs serve the review protocol and CI that verify-delivery reads, and
# whose scripted members take a filed task to a submitted attempt; otherwise
# non-zero with the containers' logs.
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
  cp -r "$SIM/ca" "$SIM/hub" "$SIM/aicrewd" "$SIM/member" "$SIM/stub" "$STAGE/"
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
  build "$STAGE/stub/simstub" ./e2e/pilotsim/cmd/simstub
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
# The sim's process repository has no tag, so the commit is selected with
# --commit (aimem 0.10.17 takes a bare commit only with it).
hub aimem process select https://git.example.test/sim/process.git "$COMMIT" process/manifest.json --commit -p "$PROJECT" >/dev/null
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
step "Gitea: the operator, the members and the stubs, their tokens, the repositories and the labels"
for user in operator sim-coordinator sim-worker sim-hands sim-ci; do
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
# pilot is the team's repository; fixture holds the stubs' fixture pull
# requests. The external-review labels exist on both, as a real reviewer's
# site has them before anyone asks for a review.
for repo in pilot fixture; do
  forge POST /user/repos '{"name":"'$repo'","auto_init":true,"default_branch":"main"}' >/dev/null
  for user in sim-coordinator sim-worker sim-hands sim-ci; do
    forge PUT "/repos/operator/$repo/collaborators/$user" '{"permission":"write"}' >/dev/null
  done
  for label in review-this hands-reviewing hands-reviewed; do
    forge POST "/repos/operator/$repo/labels" '{"name":"'$label'","color":"#0e8a16"}' >/dev/null
  done
done

# ---- the stubs: their tokens, the scenario, their start ------------------------------
step "sim-hands and sim-ci: their tokens and the scenario"
# scenario JSON: the stubs' scenario file, replaced by a rename so a stub
# never reads half of it (DESIGN-PILOT-SIMULATION section 2.5). It carries
# the next generation, and returns once both stubs acknowledge it: a poll
# that read the old scenario has finished by then, so what the runner does
# next meets the new one.
GENERATION=0
acked() { [ "$(dc exec -T sim-hands cat "/sim/scenario/$1.ack" 2>/dev/null)" = "$GENERATION" ]; }
scenario() {
  GENERATION=$((GENERATION + 1))
  printf '{"generation":%d,%s\n' "$GENERATION" "${1#\{}" | dc exec -T sim-hands sh -c \
    'cat > /sim/scenario/scenario.json.new && mv /sim/scenario/scenario.json.new /sim/scenario/scenario.json'
  waitfor "sim-hands taking scenario $GENERATION" 120 acked hands
  waitfor "sim-ci taking scenario $GENERATION" 120 acked ci
}
SCENARIO_REPOS='"repos":["operator/pilot","operator/fixture"],"poll_seconds":1'
dc up -d sim-hands sim-ci >/dev/null
for stub in hands ci; do
  gitea admin user generate-access-token --username "sim-$stub" --token-name sim --scopes all --raw |
    into "sim-$stub" stub /sim/in/forge.token
done
scenario "{$SCENARIO_REPOS}"

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
# ---- the stubs' fixture pull requests ---------------------------------------------------
step "the stubs: fixture pull requests that verify-delivery confirms, refuses on a failed CI, and a moved head"
FIX=/repos/operator/fixture
REVIEW_THIS=$(forge GET "$FIX/labels" | python3 -c \
  'import json,sys; print(next(l["id"] for l in json.load(sys.stdin) if l["name"]=="review-this"))')
# The coordinator's own fixture token, beside its home, for its local
# review comment and verify-delivery.
gitea admin user generate-access-token --username sim-coordinator --token-name fixture --scopes all --raw |
  into sim-coordinator member /home/member/fixture-forge.token
coord() { dc exec -T -u member -e GITEA_TOKEN_FILE=/home/member/fixture-forge.token sim-coordinator "$@"; }
# fixture_commit BRANCH NAME: a commit adding NAME on BRANCH (from main the first time).
fixture_commit() {
  local from=""
  forge GET "$FIX/branches/$1" >/dev/null 2>&1 || from=',"branch":"main","new_branch":"'$1'"'
  [ -n "$from" ] || from=',"branch":"'$1'"'
  forge POST "$FIX/contents/$2" '{"content":"'"$(printf '%s\n' "$2" | base64 -w0)"'","message":"add '$2'"'"$from"'}' >/dev/null
}
# fixture_pr BRANCH: opens the pull request with one commit and asks for a review; prints its number.
fixture_pr() {
  fixture_commit "$1" "$1.txt"
  local n
  n=$(forge POST "$FIX/pulls" '{"head":"'$1'","base":"main","title":"fixture '$1'"}' | json_field number)
  forge POST "$FIX/issues/$n/labels" '{"labels":['$REVIEW_THIS']}' >/dev/null
  echo "$n"
}
# pr_field N KEY[.KEY]: one field of pull request N.
pr_field() {
  forge GET "$FIX/pulls/$1" | python3 -c '
import json, sys
v = json.load(sys.stdin)
for k in sys.argv[1].split("."):
    v = v[k]
print(v)' "$2"
}
labels_of() { forge GET "$FIX/pulls/$1" | python3 -c 'import json,sys; print(" ".join(sorted(l["name"] for l in json.load(sys.stdin)["labels"])))'; }
# hands_comments N: the sim-hands review headers on pull request N, one per line.
hands_comments() {
  forge GET "$FIX/issues/$1/comments" | python3 -c '
import json, sys
for c in json.load(sys.stdin):
    if c["user"]["login"] == "sim-hands":
        print(c["body"].splitlines()[0])'
}
reviewed() { hands_comments "$1" | grep -qx "\[hands-bot review\] reviewed at head $2" && [ "$(labels_of "$1")" = hands-reviewed ]; }
ci_state() { forge GET "$FIX/commits/$1/status" | python3 -c '
import json, sys
s = [x for x in json.load(sys.stdin)["statuses"] if x["context"] == "sim-ci"]
print(s[0]["status"] if s else "none")'; }
ci_is() { [ "$(ci_state "$1")" = "$2" ]; }
# deliver N HEAD CI: the local review, both stubs' results, the operator's merge; prints the merge commit.
deliver() {
  waitfor "sim-hands's review of #$1" 120 reviewed "$1" "$2"
  waitfor "sim-ci's $3 status on #$1" 120 ci_is "$2" "$3"
  printf '[oh-code-review (high) review] reviewed at head %s\n\nVERDICT\nREADY_FOR_HUMAN_MERGE\n' "$2" |
    python3 -c 'import json,sys; print(json.dumps({"body": sys.stdin.read()}))' |
    coord sh -c 'curl -fsS --cacert /sim/tls/ca.pem -X POST -H "Authorization: token $(cat "$GITEA_TOKEN_FILE")" \
      -H "Content-Type: application/json" -d @- "https://sim-gitea:3000/api/v1/repos/operator/fixture/issues/$1/comments"' sh "$1" >/dev/null
  forge POST "$FIX/pulls/$1/merge" '{"Do":"merge"}' >/dev/null
  local merge
  merge=$(pr_field "$1" merge_commit_sha)
  waitfor "sim-ci's $3 status on the merge commit of #$1" 120 ci_is "$merge" "$3"
}
# verify N: verify-delivery, unchanged, as the coordinator runs it; writes $OUT/verify-N.json, returns its exit code.
verify() {
  coord sh -c 'python3 "$HOME/.claude/skills/verify-delivery/verify_delivery.py" \
    --pr "https://sim-gitea:3000/operator/fixture/pulls/$1" --api-base https://sim-gitea:3000/api/v1 \
    --review-author local=sim-coordinator --review-author external=sim-hands' sh "$1" > "$OUT/verify-$1.json"
}

# 1. Served by both stubs and merged by the operator: confirmed, with the three evidence kinds.
n=$(fixture_pr confirmed)
deliver "$n" "$(pr_field "$n" head.sha)" success
verify "$n" || fail "verify-delivery did not confirm fixture #$n ($OUT/verify-$n.json)"
python3 - "$OUT/verify-$n.json" <<'PY' || fail "fixture #$n: the delivery record ($OUT/verify-$n.json)"
import json, sys
d = json.load(open(sys.argv[1]))
kinds = sorted({e["kind"] for e in d.get("evidence", [])})
assert d["verdict"] == "confirmed", d["verdict"]
assert kinds == ["human_merge", "post_merge_ci", "reviewed_head"], kinds
PY

# 2. The scenario fails the CI: not_confirmed, on post_merge_ci.
scenario "{$SCENARIO_REPOS,\"ci\":{\"state\":\"failure\"}}"
n=$(fixture_pr failed-ci)
deliver "$n" "$(pr_field "$n" head.sha)" failure
code=0
verify "$n" || code=$?
[ "$code" = 3 ] || fail "verify-delivery exited $code, not 3 (not_confirmed), for fixture #$n ($OUT/verify-$n.json)"
python3 - "$OUT/verify-$n.json" <<'PY' || fail "fixture #$n: the refusal ($OUT/verify-$n.json)"
import json, sys
d = json.load(open(sys.argv[1]))
checks = {c["name"]: c["status"] for c in d["checks"]}
assert d["verdict"] == "not_confirmed" and "evidence" not in d, d["verdict"]
assert checks["post_merge_ci"] == "failed", checks
PY
scenario "{$SCENARIO_REPOS}"

# 3. A push while hands-reviewing is on: no review for either head, the labels released; a new request is served.
scenario "{$SCENARIO_REPOS,\"hands\":{\"review_seconds\":15}}"
n=$(fixture_pr moved)
first=$(pr_field "$n" head.sha)
reviewing() { [ "$(labels_of "$1")" = hands-reviewing ]; }
waitfor "sim-hands taking #$n" 60 reviewing "$n"
fixture_commit moved moved-2.txt
second=$(pr_field "$n" head.sha)
[ "$first" != "$second" ] || fail "fixture #$n: the push did not move the head"
released() { [ -z "$(labels_of "$1")" ]; }
waitfor "sim-hands releasing #$n" 60 released "$n"
[ -z "$(hands_comments "$n")" ] || fail "fixture #$n: sim-hands reviewed a moved head: $(hands_comments "$n")"
scenario "{$SCENARIO_REPOS}"
forge POST "$FIX/issues/$n/labels" '{"labels":['$REVIEW_THIS']}' >/dev/null
waitfor "sim-hands's review of #$n at its new head" 120 reviewed "$n" "$second"
[ "$(hands_comments "$n" | wc -l)" = 1 ] || fail "fixture #$n: more than one review: $(hands_comments "$n")"

# ---- the cycle: from a filed task to a submitted attempt ---------------------------------
step "the cycle: a filed task, triaged and offered by the scripted coordinator, accepted, opened and submitted by the scripted worker"
CYC=$OUT/cycle
rm -rf "$CYC"
mkdir -p "$CYC"
member_id() {
  python3 -c 'import json,sys; print(next(m["agent_id"] for t in json.load(open(sys.argv[1])) for m in t["members"] if m["label"] == sys.argv[2]))' \
    "$OUT/status.json" "$1"
}
WORKER_ID=$(member_id worker)
COORD_ID=$(member_id coordinator)
# hubapi METHOD PATH [JSON]: the hub's API as its admin, on the hub's host.
hubapi() {
  hub sh -c 'curl -fsS --cacert /sim/tls/ca.pem -X "$1" -H "Authorization: Bearer $(cat "$HOME/admin.token")" \
    -H "Content-Type: application/json" ${3:+-H "Idempotency-Key: sim-cycle-$1-$2"} ${3:+-d "$3"} "https://sim-aimem:8440$2"' sh "$@"
}
worker() { op aicrew member "$1" --team "$TEAM_ID" --agent "$WORKER_ID" ${2:+--reason "$2"} >/dev/null; }
# store_copy DIR: a copy of aicrewd's store in DIR. aicrewd keeps writing
# while the worker is paused, and tar exits 1 when a file changed as it read
# it: such a copy is taken again.
store_copy() {
  local i
  for i in 1 2 3 4 5; do
    rm -rf "$1" && mkdir -p "$1"
    if op tar -C "$OPS_HOME/aicrew/lib" -cf - . 2>/dev/null | tar -C "$1" -xf -; then return 0; fi
    sleep 1
  done
  return 1
}
# peek: the task's attempt as STATE/PHASE PULLS, from a copy of aicrewd's store.
peek() {
  store_copy "$CYC/peek" && python3 "$SIM/cycle.py" state "$CYC/peek" "$TASK"
}
at() { [[ "$(peek)" == $1 ]]; }
# snapshot STEP: the step's read-only copy of the three places (cycle.py), then its checks.
snapshot() {
  local d=$CYC/$1 role f
  mkdir -p "$d/aimem"
  printf '{"task":"%s","worker":"%s","coordinator":"%s"}\n' "$TASK" "$WORKER_ID" "$COORD_ID" > "$d/ids.json"
  store_copy "$d/store" || fail "no stable copy of aicrewd's store for the $1 step"
  hubapi GET "/v1/tasks/$TASK" > "$d/aimem/task.json"
  hubapi GET "/v1/tasks/$TASK/comments" > "$d/aimem/comments.json"
  hub aimem reservation recover status --task "$TASK" --hub https://sim-aimem:8440 \
    --admin-token-file /home/aimem/admin.token --hub-ca-file /sim/tls/ca.pem > "$d/aimem/reservation.json"
  for role in coordinator worker; do
    mkdir -p "$d/homes/$role"
    for f in logs/turns.jsonl logs/guard.jsonl docs/HANDOFF.md; do
      dc exec -T -u member "sim-$role" sh -c '[ ! -f "$1" ] || cat "$1"' sh "/home/member/agent/$f" \
        > "$d/homes/$role/${f##*/}"
    done
  done
  python3 "$SIM/cycle.py" check "$1" "$d" || fail "the cycle's checks after the $1 step (snapshot $d)"
}
# Each step's state is held for its checks by pausing the worker, as the
# operator can; the worker's turns resume after them.
worker pause "the cycle's offer checks"
TASK=$(hubapi POST "/v1/projects/$PROJECT/tasks" '{"title":"Pilot simulation cycle task","objective":"Add the file sim/TASK.md to the repository through a reviewed pull request.","acceptance_criteria":"The pull request adds one file, and the external review and CI pass at its head.","non_goals":"Everything else.","state":"READY","blocker":"","dependencies":[],"candidate_refs":[],"evidence_refs":[],"next_action":"Triage it.","archived":false}' |
  json_field id)
waitfor "the coordinator's triage and offer of task $TASK" 300 at 'offered/* 0'
snapshot offer
worker resume
waitfor "the worker's accept" 300 at 'running/working *'
worker pause "the cycle's accept checks"
snapshot accept
worker resume
# A fast review lets the worker submit in its next turn, which may start
# before the pause: the pull request's checks take either phase.
waitfor "the worker's pull request" 300 at 'running/@(working|submitted) 1'
worker pause "the cycle's pull request checks"
snapshot pull
worker resume
waitfor "the worker's submit" 300 at 'running/submitted 1'
snapshot submit

collect
step "pilot-sim: green: both members checked, listed and serving; the stubs' fixtures confirmed, refused and released; the cycle reached the submit (reports in $OUT)"
