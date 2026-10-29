#!/usr/bin/env bash
# Decides whether a slow CI job runs its tests for this event, and writes
# run=true or run=false to $GITHUB_OUTPUT (docs/DEVELOPMENT.md, "Checks").
#
#   ci-gate.sh race PKG   go test -race on ./internal/PKG/
#   ci-gate.sh windows    go test on Windows
#
# Everything but a pull request runs: a push to main, the nightly run and a
# manual one. A pull request's checkout is its merge commit, whose first
# parent is the base, so HEAD^1..HEAD is the change the pull request makes
# (the checkout needs fetch-depth 2). The race job runs when the change
# touches a directory of a package the tested package or its tests import
# from this module; the Windows job when it touches internal/agent,
# internal/service, scripts, an installer or a _windows.go file. go.mod,
# go.sum and this workflow run both.
set -euo pipefail

kind=${1:?usage: ci-gate.sh race PKG | windows}

decide() {
  echo "ci-gate: $kind ${2:-}: run=$1 ($3)"
  if [ -n "${GITHUB_OUTPUT:-}" ]; then echo "run=$1" >>"$GITHUB_OUTPUT"; fi
  exit 0
}

if [ "${GITHUB_EVENT_NAME:-}" != pull_request ]; then
  decide true "" "event ${GITHUB_EVENT_NAME:-local}"
fi

changed=$(git diff --no-renames --name-only HEAD^1 HEAD)

while IFS= read -r f; do
  case $f in
    go.mod | go.sum | .github/workflows/ci.yml | scripts/ci-gate.sh)
      decide true "" "$f changed"
      ;;
  esac
done <<<"$changed"

case $kind in
  race)
    pkg=${2:?usage: ci-gate.sh race PKG}
    root=$(go list -m -f '{{.Dir}}')
    mod=$(go list -m)
    dirs=$(go list -deps -test -f '{{if .Module}}{{if eq .Module.Path "'"$mod"'"}}{{.Dir}}{{end}}{{end}}' "./internal/$pkg/" |
      sed -e "s|^$root/||" -e 's|\\|/|g' | sort -u)
    while IFS= read -r f; do
      while IFS= read -r d; do
        [ -n "$d" ] || continue
        case $f in
          "$d"/*) decide true "$pkg" "$f is under $d" ;;
        esac
      done <<<"$dirs"
    done <<<"$changed"
    decide false "$pkg" "no change under its packages"
    ;;
  windows)
    while IFS= read -r f; do
      case $f in
        internal/agent/* | internal/service/* | scripts/* | boot.sh | boot.ps1 | install-aicrewd.sh | *_windows.go)
          decide true "" "$f changed"
          ;;
      esac
    done <<<"$changed"
    decide false "" "no change to the Windows-relevant paths"
    ;;
  *)
    echo "ci-gate.sh: unknown kind $kind" >&2
    exit 2
    ;;
esac
