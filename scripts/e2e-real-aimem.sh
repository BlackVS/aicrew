#!/usr/bin/env bash
# Runs the end-to-end harness against a real aimem (crew-execution b4;
# docs/E2E-REAL-AIMEM.md). It is not part of `go test ./...` or CI: it builds
# the pinned aimem from a local clone and runs every process on an isolated
# local hub.
#
# usage: scripts/e2e-real-aimem.sh -aimem-src PATH [-runs N] [-out DIR] [-keep]
#   -aimem-src  a local aimem clone holding the pinned commit (only read)
#   -runs       consecutive runs, each a fresh hub (default 1; the gate is 3)
#   -out        where the reports go (default .work/e2e)
#   -keep       keep each run's directory (logs, stores) for inspection
set -euo pipefail

src="" runs=1 out="" keep=""
while [ $# -gt 0 ]; do
  case "$1" in
    -aimem-src) src="$2"; shift 2 ;;
    -runs) runs="$2"; shift 2 ;;
    -out) out="$2"; shift 2 ;;
    -keep) keep=1; shift ;;
    *) sed -n '8,12p' "$0" >&2; exit 2 ;;
  esac
done
if [ -z "$src" ] || ! [ "$runs" -ge 1 ] 2>/dev/null; then
  sed -n '8,12p' "$0" >&2
  exit 2
fi

cd "$(git rev-parse --show-toplevel)"
out="${out:-.work/e2e}"
mkdir -p "$out"
src="$(cd "$src" && pwd)"

# A checkout on another file system (a Windows drive under WSL) belongs to
# another user; let this run's git read it without changing any config.
export GIT_CONFIG_COUNT=1 GIT_CONFIG_KEY_0=safe.directory GIT_CONFIG_VALUE_0='*'
export AICREW_E2E_AIMEM_SRC="$src"
[ -n "$keep" ] && export AICREW_E2E_KEEP=1

failed=0
for i in $(seq 1 "$runs"); do
  report="$(pwd)/$out/report-$i.jsonl"
  echo "run $i of $runs: report $report"
  if AICREW_E2E_REPORT="$report" AICREW_E2E_ARTIFACTS="$(pwd)/$out/run-$i" go test -tags realaimem -count=1 -timeout 30m -v -run TestRealAimem ./e2e/realaimem/ \
      > "$out/run-$i.log" 2>&1; then
    echo "run $i: PASS"
  else
    echo "run $i: FAIL (see $out/run-$i.log)"
    failed=1
  fi
  grep -E '"type":"(scenario|summary)"' "$report" || true
done
exit "$failed"
