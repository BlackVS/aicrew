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
# (up: it runs afterwards; skip: compose returns 0 and it stays stopped).
# Prints the helper's output and its exit status as the last line.
run_case() {
  local name=$1 plan=$2
  mkdir -p "$work/$name"
  (
    set +e
    OUT=$work/$name DOCKER_PROJECT=pilotsim
    STATE=$work/$name/state PLAN=$work/$name/plan CALLS=$work/$name/calls
    echo running > "$STATE"
    tr ' ' '\n' <<<"$plan" > "$PLAN"
    dc() {
      echo "dc $*" >> "$CALLS"
      case $1 in
      kill) echo stopped > "$STATE" ;;
      start)
        local next
        next=$(head -n1 "$PLAN"); sed -i '1d' "$PLAN"
        [ "$next" = up ] && echo running > "$STATE"
        return 0 ;;
      ps) echo "c-$3" ;;
      esac
    }
    docker() {
      echo "docker $*" >> "$CALLS"
      case $1 in
      inspect) [ "$(cat "$STATE")" = running ] && echo true || echo false ;;
      events) echo "1 kill pilotsim-sim-aimem-1"; echo "2 start pilotsim-sim-ca-1"; echo "3 die pilotsim-sim-ca-1" ;;
      esac
    }
    step() { printf '==> %s\n' "$*"; }
    fail() { echo "FAILED: $*"; exit 1; }
    healthy() { [ "$(cat "$STATE")" = running ]; }
    waitfor() { local what=$1; shift 2; "$@" || fail "$what did not happen"; echo "waited: $what"; }
    # shellcheck source=e2e/pilotsim/restart.sh
    . "$here/restart.sh"
    restart sim-aimem
    echo "exit=$?"
  ) > "$work/$name/out" 2>&1 || echo "exit=$?" >> "$work/$name/out"
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

echo "restart_test: $pass passed, $failures failed"
[ "$failures" = 0 ]
