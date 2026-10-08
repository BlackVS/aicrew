#!/usr/bin/env bash
# aicrew-agent installer for Linux and macOS: installs or upgrades the
# member's aicrew-agent, from any directory:
#
#   curl -fsSL https://raw.githubusercontent.com/BlackVS/aicrew/v0.4.0/boot.sh | bash
#
# It installs the release this script was fetched from (RELEASE below):
# aicrew-agent for this platform, checked against that release's
# SHA256SUMS, as ~/.local/bin/aicrew-agent. An installed aicrew-agent that
# already reports the release is left as it is; an older one is replaced,
# and both versions are printed. It installs the binary and nothing else:
# no agent home, no join, no client settings, no file outside the bin
# directory. After a first install, run `aicrew-agent join` (or rerun it
# for an existing home, so its Stop hook names this binary).
#
# Environment knobs:
#   AICREW_BIN_DIR=~/.local/bin     where aicrew-agent goes
#   AICREW_VERSION=vX.Y.Z           install another release than RELEASE
#   AICREW_REPO=owner/name          install from a fork
set -euo pipefail

# The release this script installs. Bumped together with the CHANGELOG
# when a release is cut (internal/installer checks they agree).
RELEASE=v0.4.0

command -v curl >/dev/null 2>&1 || { echo "ERROR: 'curl' is required." >&2; exit 1; }
REPO=${AICREW_REPO:-BlackVS/aicrew}
TAG=${AICREW_VERSION:-$RELEASE}
DL_BASE="https://github.com/$REPO/releases/download/$TAG"
BIN_DIR=${AICREW_BIN_DIR:-$HOME/.local/bin}
case "$(uname -s)" in
  Linux) OS=linux ;;
  Darwin) OS=darwin ;;
  *) echo "ERROR: $(uname -s) is not supported; on Windows use boot.ps1." >&2; exit 1 ;;
esac
case "$(uname -m)" in
  x86_64|amd64) ARCH=amd64 ;;
  aarch64|arm64) ARCH=arm64 ;;
  *) echo "ERROR: $(uname -m) is not a released architecture." >&2; exit 1 ;;
esac

# BEGIN install-agent
# install_agent: install aicrew-agent $TAG from $DL_BASE as
# $BIN_DIR/aicrew-agent for $OS/$ARCH. The download is checked against the
# release's SHA256SUMS in a temporary directory; nothing in the bin
# directory changes unless it passes.
sha256() { if command -v sha256sum >/dev/null 2>&1; then sha256sum "$1"; else shasum -a 256 "$1"; fi | awk '{print $1}'; }
install_agent() {
  local exe="$BIN_DIR/aicrew-agent" old="" tmp rc=0
  if [ -x "$exe" ]; then
    old=$("$exe" version 2>/dev/null | awk '{print $2}') || old=""
    if [ "$old" = "$TAG" ]; then
      echo "aicrew-agent $TAG is current ($exe); nothing changed."
      return 0
    fi
  fi
  tmp=$(mktemp -d)
  fetch_and_swap "$tmp" "$exe" "$old" || rc=$?
  rm -rf "$tmp"
  return "$rc"
}
# fetch_and_swap tmp exe old: every step checks its own result, since a
# function called in a || list runs without errexit.
fetch_and_swap() {
  local tmp=$1 exe=$2 old=$3 asset="aicrew-agent-$OS-$ARCH" want got new
  echo "fetching aicrew-agent $TAG ($asset)"
  curl -fsSL "$DL_BASE/SHA256SUMS" -o "$tmp/SHA256SUMS" ||
    { echo "ERROR: cannot fetch the SHA256SUMS of $TAG; refusing an unverified binary. Nothing changed." >&2; return 1; }
  curl -fsSL "$DL_BASE/$asset" -o "$tmp/aicrew-agent" ||
    { echo "ERROR: cannot fetch $asset of $TAG. Nothing changed." >&2; return 1; }
  want=$(awk -v n="$asset" '$2==n || $2=="*"n {print $1}' "$tmp/SHA256SUMS")
  got=$(sha256 "$tmp/aicrew-agent")
  if [ -z "$want" ] || [ "$want" != "$got" ]; then
    echo "ERROR: checksum mismatch for $asset (want ${want:-absent}, got $got); refusing it. Nothing changed." >&2
    return 1
  fi
  chmod 755 "$tmp/aicrew-agent" || return 1
  new=$("$tmp/aicrew-agent" version 2>/dev/null | awk '{print $2}') || new=""
  mkdir -p "$BIN_DIR" || { echo "ERROR: cannot create $BIN_DIR. Nothing changed." >&2; return 1; }
  # Copy beside the target, then rename over it: atomic, and a running
  # aicrew-agent keeps the file it started from.
  if ! { cp "$tmp/aicrew-agent" "$exe.new.$$" && mv -f "$exe.new.$$" "$exe"; }; then
    rm -f "$exe.new.$$"
    echo "ERROR: cannot write $exe; the installed aicrew-agent is unchanged." >&2
    return 1
  fi
  if [ -n "$old" ]; then
    echo "upgraded aicrew-agent $old -> ${new:-$TAG} ($exe)"
  else
    echo "installed aicrew-agent ${new:-$TAG} ($exe)"
  fi
}
# END install-agent

install_agent
case ":$PATH:" in
  *":$BIN_DIR:"*) ;;
  *) echo "note: $BIN_DIR is not on PATH; add it to your shell profile." ;;
esac
