#!/usr/bin/env bash
# Tests of e2e/pilotsim/restart.sh, the fault cases' restart of a killed
# service, with stand-ins for the runner's dc, docker, step, fail, waitfor
# and healthy. CI runs it; it needs only bash.
set -euo pipefail
here=$(cd "$(dirname "$0")" && pwd)
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
pass=0
failures=0

# One case: PLAN is what each `dc start` does to the service, in order
# (up: started and running; skip: compose returns 0 and starts nothing;
# crash: started, then stopped at once, as a product that dies on start;
# error: compose exits 1 and starts nothing). Each case runs in a bash of its
# own under the runner's error handling (errexit, pipefail and an ERR trap
# that fails), which a subshell tested with || would switch off.
cat > "$work/case.sh" <<'CASE'
set -Eeuo pipefail
OUT=$DIR DOCKER_PROJECT=pilotsim
STATE=$DIR/state PLAN=$DIR/plan CALLS=$DIR/calls STARTS=$DIR/starts
echo running > "$STATE"; echo 0 > "$STARTS"
tr ' ' '\n' <<<"$PLANW" > "$PLAN"
dc() {
  echo "dc $*" >> "$CALLS"
  case $1 in
  kill) echo stopped > "$STATE" ;;
  start)
    local next
    next=$(head -n1 "$PLAN"); sed -i '1d' "$PLAN"
    case $next in
    up) echo running > "$STATE"; echo $(($(cat "$STARTS") + 1)) > "$STARTS" ;;
    crash) echo stopped > "$STATE"; echo $(($(cat "$STARTS") + 1)) > "$STARTS" ;;
    error) return 1 ;;
    esac
    return 0 ;;
  ps) echo "c-$3" ;;
  esac
}
docker() {
  echo "docker $*" >> "$CALLS"
  case $1 in
  inspect) echo "2026-11-18T00:00:0$(cat "$STARTS")Z" ;;
  events) echo "1 kill pilotsim-sim-aimem-1"; echo "2 start pilotsim-sim-ca-1"; echo "3 die pilotsim-sim-ca-1" ;;
  esac
}
step() { printf '==> %s\n' "$*"; }
fail() { echo "FAILED: $*"; exit 1; }
healthy() { [ "$(cat "$STATE")" = running ]; }
waitfor() { local what=$1; shift 2; "$@" || fail "$what did not happen"; echo "waited: $what"; }
trap 'fail "line $LINENO exited $?"' ERR
# shellcheck source=e2e/pilotsim/restart.sh
. "$HERE/restart.sh"
restart sim-aimem
CASE
# Prints the helper's output, then its exit status as the last line.
run_case() {
  local name=$1 rc=0
  mkdir -p "$work/$name"
  DIR=$work/$name PLANW=$2 HERE=$here bash "$work/case.sh" > "$work/$name/out" 2>&1 || rc=$?
  echo "exit=$rc" >> "$work/$name/out"
}

check() {
  local name=$1 what=$2
  shift 2
  if "$@"; then
    pass=$((pass + 1))
  else
    failures=$((failures + 1))
    echo "FAIL: $name: $what"
    sed 's/^/  | /' "$work/$name/out"
  fi
}
has() { grep -qF -- "$2" "$work/$1/out"; }
lacks() { ! grep -qF -- "$2" "$work/$1/out"; }

# The service starts on the first start: no retry line, no events kept.
run_case normal "up"
check normal "healthy wait reached" has normal "waited: sim-aimem healthy after its restart"
check normal "exit 0" has normal "exit=0"
check normal "no retry line" lacks normal "was skipped"
check normal "no events file" test ! -e "$work/normal/restart-sim-aimem-1.events"
check normal "one start" test "$(grep -c '^dc start' "$work/normal/calls")" = 1

# The first start returns 0 and starts nothing; the second starts it.
run_case retry "skip up"
check retry "the retry line" has retry "pilot-sim: the start of sim-aimem was skipped (compose returned 0 and did not start it); starting it again"
check retry "the second start named" has retry "the second start of sim-aimem started it"
check retry "two starts" test "$(grep -c '^dc start' "$work/retry/calls")" = 2
check retry "healthy wait still reached" has retry "waited: sim-aimem healthy after its restart"
check retry "exit 0" has retry "exit=0"
check retry "events kept" grep -q "start pilotsim-sim-ca-1" "$work/retry/restart-sim-aimem-1.events"
check retry "events filtered to the project" grep -q -- "--filter label=com.docker.compose.project=pilotsim --filter type=container" "$work/retry/calls"

# Neither start brings it up: the named failure, before the health wait.
run_case never "skip skip"
check never "the named failure" has never "FAILED: the start of sim-aimem was skipped twice; compose did not start it"
check never "the events file named" has never "restart-sim-aimem-1.events"
check never "events kept" test -s "$work/never/restart-sim-aimem-1.events"
check never "no health wait" lacks never "waited:"
check never "exit 1" has never "exit=1"

# The first start is skipped and the second exits non-zero: the events are
# kept before the named failure, not lost to the ERR trap.
run_case error "skip error"
check error "the retry line" has error "was skipped (compose returned 0 and did not start it); starting it again"
check error "the named failure" has error "FAILED: the start of sim-aimem was skipped, and the second start exited 1"
check error "not the ERR trap" lacks error "FAILED: line"
check error "events kept" test -s "$work/error/restart-sim-aimem-1.events"
check error "exit 1" has error "exit=1"

# The service is started and dies at once (a fault of the product): no
# retry, and the health wait judges it, as before.
run_case crash "crash up"
check crash "no retry line" lacks crash "was skipped"
check crash "one start" test "$(grep -c '^dc start' "$work/crash/calls")" = 1
check crash "the health wait's failure" has crash "FAILED: sim-aimem healthy after its restart did not happen"
check crash "no events file" test ! -e "$work/crash/restart-sim-aimem-1.events"

echo "restart_test: $pass passed, $failures failed"
[ "$failures" = 0 ]
