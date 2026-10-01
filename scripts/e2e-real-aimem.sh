#!/usr/bin/env bash
# Runs the end-to-end harness against a real aimem (crew-execution b4;
# docs/E2E-REAL-AIMEM.md). It is not part of `go test ./...` or CI: it builds
# the pinned aimem from a local clone and runs every process on an isolated
# local hub.
#
# usage: scripts/e2e-real-aimem.sh -aimem-src PATH [-runs N] [-out DIR] [-keep] [-skip-faults]
#   -aimem-src    a local aimem clone holding the pinned commit (only read)
#   -runs         consecutive runs, each a fresh hub (default 1; the gate is 3)
#   -out          where the reports go (default .work/e2e in the repository)
#   -keep         keep each run's directory (logs, stores) for inspection
#   -skip-faults  instead, run each fault scenario with one fault left out;
#                 every such run must fail (the skip-the-fault matrix)
set -euo pipefail

usage() { sed -n '7,14p' "$0" >&2; exit 2; }
src="" runs=1 out="" keep="" skip=""
while [ $# -gt 0 ]; do
  case "$1" in
    -aimem-src) [ $# -ge 2 ] || usage; src="$2"; shift 2 ;;
    -runs) [ $# -ge 2 ] || usage; runs="$2"; shift 2 ;;
    -out) [ $# -ge 2 ] || usage; out="$2"; shift 2 ;;
    -keep) keep=1; shift ;;
    -skip-faults) skip=1; shift ;;
    *) usage ;;
  esac
done
if [ -z "$src" ] || ! [ "$runs" -ge 1 ] 2>/dev/null; then
  usage
fi

# Paths are taken from where the script was invoked, absolute or relative,
# and made absolute once.
here="$(pwd)"
abs() { case "$1" in /*) printf '%s' "$1" ;; *) printf '%s/%s' "$here" "$1" ;; esac; }
src="$(cd "$(abs "$src")" && pwd)"
repo="$(git rev-parse --show-toplevel)"
out="${out:+$(abs "$out")}"
out="${out:-$repo/.work/e2e}"
mkdir -p "$out"
out="$(cd "$out" && pwd)"
cd "$repo"

# A checkout on another file system (a Windows drive under WSL) belongs to
# another user; let this run's git read it without changing any config.
export GIT_CONFIG_COUNT=1 GIT_CONFIG_KEY_0=safe.directory GIT_CONFIG_VALUE_0='*'
export AICREW_E2E_AIMEM_SRC="$src"
if [ -n "$keep" ]; then export AICREW_E2E_KEEP=1; fi

# run NAME FILTER: one test run, its report and artifacts named NAME.
run() {
  AICREW_E2E_REPORT="$out/report-$1.jsonl" AICREW_E2E_ARTIFACTS="$out/run-$1" \
    go test -tags realaimem -count=1 -timeout 40m -v -run "$2" ./e2e/realaimem/ > "$out/run-$1.log" 2>&1
}

if [ -n "$skip" ]; then
  # Each case leaves one fault out (AICREW_E2E_SKIP_FAULT) and runs only its
  # scenario. The test itself judges the run: it writes a skip_verdict of
  # failed_as_expected only when H passed and the scenario failed on an
  # assertion of that case. A case is accepted only on that record in a
  # fresh report; an absent report, a bootstrap failure or any other cause
  # is a rejected result.
  failed=0
  : > "$out/skip-matrix.txt"
  cases="F1-reply F1-request F2-begin F2-settle F5-stale F5-replay F5-resume F5-delay"
  cases="$cases F3-launcher F3-aicrewd F3-hub F4-second F4-race F4-busy F6-release F6-cancel F6-unreachable F7-leak"
  for c in $cases; do
    scenario="${c%%-*}"
    rm -f "$out/report-skip-$c.jsonl"
    AICREW_E2E_SKIP_FAULT="$c" run "skip-$c" "TestRealAimem/${scenario}_" || true
    if grep -q "\"type\":\"skip_verdict\".*\"verdict\":\"failed_as_expected\"" "$out/report-skip-$c.jsonl" 2>/dev/null; then
      line="$c: failed as expected (H passed; ${scenario} failed on its own assertion)"
    else
      line="$c: REJECTED ($(grep -o '"reasons":\[[^]]*\]' "$out/report-skip-$c.jsonl" 2>/dev/null || echo 'no verdict: no report, or the run did not get that far'))"
      failed=1
    fi
    echo "$line" | tee -a "$out/skip-matrix.txt"
  done
  exit "$failed"
fi

failed=0
for i in $(seq 1 "$runs"); do
  echo "run $i of $runs: report $out/report-$i.jsonl"
  if run "$i" TestRealAimem; then
    echo "run $i: PASS"
  else
    echo "run $i: FAIL (see $out/run-$i.log)"
    failed=1
  fi
  grep -E '"type":"(scenario|summary)"' "$out/report-$i.jsonl" || true
done
exit "$failed"
