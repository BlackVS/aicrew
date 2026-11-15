#!/usr/bin/env bash
# aicrew's release steps. .github/workflows/release.yml runs them; CI runs
# `build` on every pull request and scripts/release_test.sh tests the rest.
#
#   release.sh resolve
#       From the event (GITHUB_EVENT_NAME, GITHUB_REF, GITHUB_REF_NAME,
#       INPUT_TAG, INPUT_DRY_RUN), append TAG and DRY_RUN to $GITHUB_ENV and
#       $GITHUB_OUTPUT. A pushed tag publishes. A manual run that publishes
#       must start from the tag it names (the attestation names the ref the
#       run started from); a dry run builds only and must start from main.
#   release.sh check vX.Y.Z [--dry-run]
#       Check out the commit to release. The tag must be vX.Y.Z on a commit
#       reachable from main, CHANGELOG.md must have its section, and the
#       release must not exist yet. This script never creates a tag: without
#       the tag only a dry run proceeds, on main's tip.
#   release.sh build vX.Y.Z
#       dist/: aicrewd, aicrew and aicrew-agent for every platform, stamped
#       with the tag, LICENSE, the installer scripts, and SHA256SUMS over
#       every other asset; then the stamped version is checked on the built
#       aicrew-agent.
#   release.sh notes vX.Y.Z [--dry-run]
#       release_notes.md: the tag's CHANGELOG section and the license notice.
#
# The workflow's publish job checks, attests and publishes dist/ without a
# checkout, so no step of this script runs with a token that writes.
set -euo pipefail
cmd=${1:-}; TAG=${2:-}; flag=${3:-}
fail() { echo "::error::$*" >&2; exit 1; }
semver='^v[0-9]+\.[0-9]+\.[0-9]+$'
dry=false; [ "$flag" = --dry-run ] && dry=true

# The release matrix (DEVELOPMENT, "Releasing").
binaries="aicrewd aicrew aicrew-agent"
platforms="linux/amd64 linux/arm64 darwin/amd64 darwin/arm64 windows/amd64"
# The installer scripts, published as assets under these names: an aiforge
# export copies them, and its script looks for exactly these three
# (internal/installer checks the list).
installers="install-aicrewd.sh boot.sh boot.ps1"

sha256() { if command -v sha256sum >/dev/null; then sha256sum -- "$@"; else shasum -a 256 -- "$@"; fi; }

# section prints CHANGELOG.md's "## [X.Y.Z]" section, without its heading.
section() {
  [ -f CHANGELOG.md ] || return 0
  awk -v ver="${TAG#v}" '
    index($0, "## [" ver "]") == 1 {on=1; next}
    on && /^## \[/ {exit}
    on {print}
  ' CHANGELOG.md
}

if [ "$cmd" = resolve ]; then
  out() { for f in "${GITHUB_ENV:-/dev/null}" "${GITHUB_OUTPUT:-/dev/null}"; do printf '%s\n' "$@" >> "$f"; done; }
  if [ "${GITHUB_EVENT_NAME:-}" = workflow_dispatch ]; then
    # The workflow's ref job refuses the same runs before any build; this
    # is the rule again where the tests can reach it.
    t=${INPUT_TAG:-}; [[ $t = v* ]] || t=v$t
    [[ $t =~ $semver ]] || fail "tag '${INPUT_TAG:-}' is not vMAJOR.MINOR.PATCH"
    d=false; [ "${INPUT_DRY_RUN:-}" = true ] && d=true
    if $d; then
      [ "${GITHUB_REF:-}" = refs/heads/main ] || fail "a dry run starts from main, not ${GITHUB_REF:-}"
    else
      [ "${GITHUB_REF:-}" = "refs/tags/$t" ] ||
        fail "a run that publishes starts from its tag refs/tags/$t, not ${GITHUB_REF:-}: gh workflow run release.yml --ref $t -f tag=$t -f dry_run=false"
    fi
    out "TAG=$t" "DRY_RUN=$d"
  else
    t=${GITHUB_REF_NAME:-}
    [[ $t =~ $semver ]] || fail "tag '$t' is not vMAJOR.MINOR.PATCH"
    out "TAG=$t" "DRY_RUN=false"
  fi
  exit 0
fi

[[ $TAG =~ $semver ]] || fail "tag '$TAG' is not vMAJOR.MINOR.PATCH"

case "$cmd" in
check)
  git fetch -q --force origin '+refs/heads/main:refs/remotes/origin/main' '+refs/tags/*:refs/tags/*'
  if git rev-parse -q --verify "refs/tags/$TAG" >/dev/null; then
    commit=$(git rev-parse "$TAG^{commit}")
    git merge-base --is-ancestor "$commit" origin/main ||
      fail "$TAG points at $(git rev-parse --short "$commit"), which is not on main: merge first, then tag"
  elif $dry; then
    commit=$(git rev-parse origin/main)
    echo "::notice::tag $TAG does not exist: a dry run of main's tip $(git rev-parse --short "$commit")"
  else
    fail "tag $TAG does not exist: push the tag to release; this workflow never creates one"
  fi
  git checkout -q --detach "$commit"
  if ! section | grep -q '[^[:space:]]'; then
    $dry || fail "CHANGELOG.md has no [${TAG#v}] section at $(git rev-parse --short HEAD): the release-preparation PR writes it"
    echo "::warning::CHANGELOG.md has no [${TAG#v}] section (a dry run goes on)"
  fi
  # The one-liners fetch the installers from this tag, and each installs
  # the release it pins: a tag one does not pin would install another
  # release. boot.ps1 is checked out with CRLF (.gitattributes).
  for f in $installers; do
    [ -f "$f" ] || continue
    case "$f" in
      *.ps1) tr -d '\r' < "$f" | grep -qx "\$release = '$TAG'" && continue ;;
      *) grep -qx "RELEASE=$TAG" "$f" && continue ;;
    esac
    $dry || fail "$f at $(git rev-parse --short HEAD) does not pin $TAG: the release-preparation PR bumps its pin"
    echo "::warning::$f does not pin $TAG (a dry run goes on)"
  done
  if ! $dry && gh release view "$TAG" >/dev/null 2>&1; then
    fail "release $TAG already exists"
  fi
  echo "release $TAG: commit $(git rev-parse --short HEAD) checked (dry run: $dry)"
  ;;
build)
  rm -rf dist; mkdir dist
  for p in $platforms; do
    os=${p%/*}; arch=${p#*/}; ext=""; [ "$os" = windows ] && ext=.exe
    for b in $binaries; do
      GOOS=$os GOARCH=$arch CGO_ENABLED=0 go build -trimpath \
        -ldflags "-s -w -X github.com/BlackVS/aicrew/internal/version.Override=$TAG" \
        -o "dist/$b-$os-$arch$ext" "./cmd/$b"
    done
  done
  cp LICENSE dist/LICENSE
  # The installers as they are checked out at the tag: boot.ps1 with CRLF
  # (.gitattributes), the shell scripts with LF.
  for f in $installers; do cp "$f" "dist/$f"; done
  # One format on every host: "HASH  NAME" (a binary-mode "*NAME" from some
  # sha256sum builds is written as text mode; the bytes hashed are the same).
  (cd dist && sha256 $(ls | grep -vx SHA256SUMS) | sed 's/ \*/  /' > SHA256SUMS)
  # The stamp is checked on a shipped binary when this host's platform is
  # in the matrix, else on a host build with the same flags.
  hos=$(go env GOHOSTOS); harch=$(go env GOHOSTARCH); ext=""; [ "$hos" = windows ] && ext=.exe
  agent="dist/aicrew-agent-$hos-$harch$ext"
  if [ ! -f "$agent" ]; then
    agent="$(mktemp -d)/aicrew-agent$ext"
    CGO_ENABLED=0 go build -trimpath -ldflags "-X github.com/BlackVS/aicrew/internal/version.Override=$TAG" -o "$agent" ./cmd/aicrew-agent
  fi
  got=$("$agent" version -json | grep -o '"version":"[^"]*"' | cut -d'"' -f4)
  [ "$got" = "$TAG" ] || fail "the built aicrew-agent reports version '$got', not $TAG"
  echo "stamped version checked: $got"
  echo "SHA256SUMS:"; cat dist/SHA256SUMS
  ;;
notes)
  body=$(section)
  if ! printf '%s' "$body" | grep -q '[^[:space:]]'; then
    $dry || fail "CHANGELOG.md has no [${TAG#v}] section"
    body="(dry run: no CHANGELOG section for ${TAG#v})"
  fi
  {
    printf '%s\n\n---\n\n' "$body"
    echo "Verify a download against SHA256SUMS before running it; DEVELOPMENT.md, \"Releasing\", has the command for each platform."
    echo
    echo "SHA256SUMS checks that a download arrived intact. To check that this repository's release workflow built it: \`gh attestation verify <file> --repo BlackVS/aicrew\`."
    echo
    echo "Licensed under the PolyForm Noncommercial License 1.0.0: https://polyformproject.org/licenses/noncommercial/1.0.0"
    echo
    grep '^Required Notice:' LICENSE
  } > release_notes.md
  echo "release notes written ($(wc -l < release_notes.md) lines)"
  ;;
*)
  fail "usage: release.sh resolve | check|build|notes vX.Y.Z [--dry-run]"
  ;;
esac
