#!/usr/bin/env bash
# The pilot simulation (docs/E2E-PILOT-SIM.md; design
# docs/DESIGN-PILOT-SIMULATION.md). On Linux with Docker:
#
#   [AICREW_SIM_SKIP_FAULT=CASE] scripts/pilot-sim.sh [-keep] [-out DIR]
#
# It builds this tree's binaries, stages the image contexts, brings the
# compose stack up, does the operator's steps (the hub's project, process
# and peer; aicrewd's hub and team; Gitea's accounts, labels and
# repositories; each member's aimem user, token and invitation), hands each
# member and stub its input files, and runs the smoke check, the stubs'
# fixture pull requests, two cycles from a filed task to DONE, checking
# each step read-only (e2e/pilotsim/cycle.py), and the pilot cases P1 to P5
# (e2e/pilotsim/cases.py), each a fault on a cycle of its own. A case the
# board tracks as a gap is checked expected-to-fail, and its outcome is
# listed in OUT/expected-to-fail.txt. AICREW_SIM_SKIP_FAULT=CASE runs only
# that case, without its fault, and is green only when the case's checks
# fail where its absence should show. It brings the stack down and
# removes its volumes unless -keep is given. Exit 0: a healthy stack whose
# stubs serve the review protocol and CI that verify-delivery reads, and
# whose scripted members take a filed task to DONE with its delivery record; otherwise
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

SKIP=${AICREW_SIM_SKIP_FAULT:-}
case "$SKIP" in
  "" | P1 | P2 | P3 | P4 | P5) ;;
  *) echo "pilot-sim: AICREW_SIM_SKIP_FAULT=$SKIP is not one of P1 to P5" >&2; exit 2 ;;
esac

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

# ---- Gitea: the accounts, their tokens, the repository -------------------------------
step "Gitea: the operator, the members and the stubs, their tokens, the repositories, the labels and the process"
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
# process is the project's process repository, public as an operator
# publishes one: the hub imports its set from it, the members' automations
# read their points from it.
forge POST /user/repos '{"name":"process","auto_init":true,"default_branch":"main","private":false}' >/dev/null
for f in process/manifest.json process/HANDBOOK.md; do
  forge POST "/repos/operator/process/contents/$f"     '{"content":"'"$(base64 -w0 < "$SIM/hub/process/$f")"'","message":"add '$f'","branch":"main"}' >/dev/null
done
PROCESS_URL=https://sim-gitea:3000/operator/process.git
PROCESS_COMMIT=$(forge GET /repos/operator/process/branches/main | python3 -c 'import json,sys; print(json.load(sys.stdin)["commit"]["id"])')

# ---- the hub: project, process, repository, peer ---------------------------------
step "the hub: the project, its process and repository, and aicrewd's peer"
PROJECT=pilot
REPO_URL=https://sim-gitea:3000/operator/pilot.git
hub aimem tasks on -p "$PROJECT" >/dev/null
# The sim's process repository has no tag, so the commit is selected with
# --commit (aimem 0.10.17 takes a bare commit only with it).
hub aimem process select "$PROCESS_URL" "$PROCESS_COMMIT" process/manifest.json --commit -p "$PROJECT" >/dev/null
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

# ---- the cycle: from a filed task to its delivery record ---------------------------------
step "the cycle, twice: a filed task carried to DONE by the scripted members, with a rework, the operator's merge and the delivery record"
member_id() {
  python3 -c 'import json,sys; print(next(m["agent_id"] for t in json.load(open(sys.argv[1])) for m in t["members"] if m["label"] == sys.argv[2]))' \
    "$OUT/status.json" "$1"
}
WORKER_ID=$(member_id worker)
COORD_ID=$(member_id coordinator)
# hubapi METHOD PATH [JSON]: the hub's API as its admin, on the hub's host;
# a write's idempotency key names the cycle.
hubapi() {
  hub sh -c 'curl -fsS --cacert /sim/tls/ca.pem -X "$1" -H "Authorization: Bearer $(cat "$HOME/admin.token")" \
    -H "Content-Type: application/json" ${3:+-H "Idempotency-Key: sim-cycle-$4-$1-$2"} ${3:+-d "$3"} "https://sim-aimem:8440$2"' \
    sh "$1" "$2" "${3:-}" "${CYCLE:-0}"
}
# member pause|resume ROLE [REASON]: the operator's hold on a member's
# turns; member nudge ROLE NOTE: the operator's wake of a member.
member() {
  local id=$WORKER_ID
  [ "$2" = coordinator ] && id=$COORD_ID
  if [ "$1" = nudge ]; then
    op aicrew member nudge --team "$TEAM_ID" --agent "$id" --note "$3" >/dev/null
  else
    op aicrew member "$1" --team "$TEAM_ID" --agent "$id" ${3:+--reason "$3"} >/dev/null
  fi
}
# turn_mark ROLE: "running N" or "idle N", N the turns the home recorded.
turn_mark() {
  dc exec -T -u member "sim-$1" python3 -c '
import json, os
h = "/home/member/agent"
try:
    st = json.load(open(h + "/state/turns.json"))
except FileNotFoundError:
    st = {}
log = h + "/logs/turns.jsonl"
n = sum(1 for _ in open(log)) if os.path.exists(log) else 0
print("running" if st.get("running") else "idle", n)'
}
# idle ROLE: the member runs no turn, and recorded none, over two seconds:
# a pause stops the next turn, not the current one (seq 678).
idle() {
  local a b
  a=$(turn_mark "$1") && sleep 2 && b=$(turn_mark "$1") && [ "$a" = "$b" ] && [ "${a%% *}" = idle ]
}
# hold ROLE REASON: pauses the member and waits until its turn has ended.
hold() {
  member pause "$1" "$2"
  waitfor "sim-$1's turn to end" 300 idle "$1"
}
# store_copy DIR: a copy of aicrewd's store in DIR. aicrewd keeps writing
# while a member is paused, and tar exits 1 when a file changed as it read
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
PILOT=/repos/operator/pilot
# pilot_field KEY[.KEY]: one field of the cycle's pull request.
pilot_field() {
  forge GET "$PILOT/pulls/$PULL" | python3 -c '
import json, sys
v = json.load(sys.stdin)
for k in sys.argv[1].split("."):
    v = v[k]
print(v)' "$1"
}
# reworked: sim-hands returned an older head, and the worker pushed a new one.
# The head is read before the pipe: a docker exec inside the pipe's command
# substitution would read the pipe, which is the comments' JSON.
reworked() {
  local head
  head=$(pilot_field head.sha) || return 1
  forge GET "$PILOT/issues/$PULL/comments" | python3 -c '
import json, re, sys
for c in json.load(sys.stdin):
    m = re.match(r"\[hands-bot review\] reviewed at head (\S+)", c["body"])
    if m and "\nRETURN_TO_IMPLEMENTATION" in c["body"] and m.group(1) != sys.argv[1]:
        sys.exit(0)
sys.exit(1)' "$head"
}
pilot_ci_is() { [ "$(forge GET "$PILOT/commits/$1/status" | json_field state)" = "$2" ]; }
# take STEP: the step's read-only copy of the four places (cycle.py) in
# D=CYC/STEP.
take() {
  local d=$CYC/$1 role f
  D=$d
  rm -rf "$d"
  mkdir -p "$d/aimem" "$d/forge"
  printf '{"task":"%s","worker":"%s","coordinator":"%s","first_head":"%s"}\n' "$TASK" "$WORKER_ID" "$COORD_ID" \
    "${FIRST:-}" > "$d/ids.json"
  store_copy "$d/store" || fail "no stable copy of aicrewd's store for the $1 step"
  op aicrew status --team "$TEAM_ID" > "$d/status.json" || fail "aicrew status for the $1 step"
  [ -n "$TASK" ] || { take_homes "$d"; return 0; }
  hubapi GET "/v1/tasks/$TASK" > "$d/aimem/task.json"
  hubapi GET "/v1/tasks/$TASK/comments" > "$d/aimem/comments.json"
  hubapi GET "/v1/tasks/$TASK/history" > "$d/aimem/history.json"
  hub aimem reservation recover status --task "$TASK" --hub https://sim-aimem:8440 \
    --admin-token-file /home/aimem/admin.token --hub-ca-file /sim/tls/ca.pem > "$d/aimem/reservation.json"
  if [ -n "${PULL:-}" ]; then
    forge GET "$PILOT/pulls/$PULL" > "$d/forge/pull.json"
    forge GET "$PILOT/issues/$PULL/comments" > "$d/forge/comments.json"
    forge GET "$PILOT/pulls/$PULL/commits" > "$d/forge/commits.json"
    forge GET "$PILOT/commits/$(pilot_field head.sha)/status" > "$d/forge/head-status.json"
    [ -z "${FIRST:-}" ] || forge GET "$PILOT/commits/$FIRST/status" > "$d/forge/first-status.json"
    [ -z "${MERGE:-}" ] || forge GET "$PILOT/commits/$MERGE/status" > "$d/forge/status.json"
  fi
  [ ! -f "$CYC/verify.json" ] || cp "$CYC/verify.json" "$d/verify.json"
  take_homes "$d"
}
# take_homes DIR: each home's turn records, guard log, turn state and handoff.
take_homes() {
  local d=$1 role f
  for role in coordinator worker; do
    mkdir -p "$d/homes/$role"
    for f in logs/turns.jsonl logs/guard.jsonl docs/HANDOFF.md state/turns.json; do
      dc exec -T -u member "sim-$role" sh -c '[ ! -f "$1" ] || cat "$1"' sh "/home/member/agent/$f" \
        > "$d/homes/$role/${f##*/}"
    done
  done
}
# snapshot STEP [CASE]: take the step, then its cycle checks (for a case's
# cycle, at the step done).
snapshot() {
  take "$1"
  python3 "$SIM/cycle.py" check "$1" "$D" ${2:+"$2"} || fail "cycle $CYCLE: the checks after the $1 step (snapshot $D)"
}
# file_task: the cycle's task, READY on the board as the operator files it.
file_task() {
  TASK=$(hubapi POST "/v1/projects/$PROJECT/tasks" '{"title":"Pilot simulation cycle task '"$CYCLE"'","objective":"Add the file sim/TASK.md to the repository through a reviewed pull request.","acceptance_criteria":"The pull request adds one file, and the external review and CI pass at its head.","non_goals":"Everything else.","state":"READY","blocker":"","dependencies":[],"candidate_refs":[],"evidence_refs":[],"next_action":"Triage it.","archived":false}' |
    json_field id)
}
# merge: the operator merges the pull request as a person once its gates
# are green; MERGE is its merge commit.
merge() {
  forge POST "$PILOT/pulls/$PULL/merge" '{"Do":"merge"}' >/dev/null
  MERGE=$(pilot_field merge_commit_sha)
}
# delivered: the members are done with the finalized attempt, and the
# runner's own verify-delivery, as the coordinator ran it, is the record the
# attempt and the task must carry.
delivered() {
  waitfor "the coordinator's delivery and finalize" 300 at 'closed/finalized *'
  waitfor "sim-coordinator's turn to end" 120 idle coordinator
  waitfor "sim-worker's turn to end" 120 idle worker
  coord sh -c 'python3 "$HOME/.claude/skills/verify-delivery/verify_delivery.py" \
    --pr "https://sim-gitea:3000/operator/pilot/pulls/$1" --api-base https://sim-gitea:3000/api/v1 \
    --review-author local=sim-coordinator --review-author external=sim-hands' sh "$PULL" > "$CYC/verify.json" ||
    fail "cycle $CYCLE: verify-delivery did not confirm #$PULL ($CYC/verify.json)"
}
# cycle N: one task from the board to DONE (docs/E2E-PILOT-SIM.md, "The cycle").
# Each step's state is held for its checks by pausing a member, as the
# operator can, until its turn has ended; its turns resume after them.
cycle() {
  CYCLE=$1 CYC=$OUT/cycle-$1 PULL= MERGE= FIRST=
  rm -rf "$CYC"
  mkdir -p "$CYC"
  # sim-hands returns a pull request's first review, and every review takes
  # longer than three of the launcher's turn gaps: a waiting worker is
  # neither stalled nor blocked (seq 676).
  scenario "{$SCENARIO_REPOS,\"hands\":{\"first_verdict\":\"RETURN_TO_IMPLEMENTATION\",\"review_seconds\":50}}"
  hold worker "the cycle's offer checks"
  file_task
  waitfor "the coordinator's triage and offer of task $TASK" 300 at 'offered/* 0'
  snapshot offer
  member resume worker
  waitfor "the worker's accept" 300 at 'running/working *'
  hold worker "the cycle's accept checks"
  snapshot accept
  member resume worker
  waitfor "the worker's pull request" 300 at 'running/working 1'
  hold worker "the cycle's pull request checks"
  PULL=$(python3 "$SIM/cycle.py" pull "$CYC/peek" "$TASK")
  snapshot pull
  # The coordinator is held from here to the submit's checks.
  hold coordinator "the cycle's submit checks"
  member resume worker
  waitfor "sim-hands's return and the worker's rework of #$PULL" 300 reworked
  hold worker "the cycle's rework checks"
  snapshot rework
  member resume worker
  waitfor "the worker's submit" 300 at 'running/submitted 1'
  hold worker "the cycle's submit checks"
  snapshot submit
  member resume worker
  member resume coordinator
  waitfor "the coordinator's review" 300 at 'running/accepted 1'
  hold coordinator "the operator's merge"
  snapshot review
  # The operator merges as a person once the gates are green, and sim-ci
  # reports on the merge commit before the coordinator's next turn.
  merge
  waitfor "sim-ci's status on the merge commit of #$PULL" 120 pilot_ci_is "$MERGE" success
  snapshot merge
  member resume coordinator
  delivered
  snapshot done
}
if [ -z "$SKIP" ]; then
  cycle 1
  cycle 2
fi

# ---- the pilot cases (DESIGN-PILOT-SIMULATION section 6) --------------------------------
. "$SIM/cases.sh"

