#!/usr/bin/env bash
# aicrew-agent installer for Linux and macOS: installs or upgrades the
# member's aicrew-agent, and the operator CLI aicrew beside it, from any
# directory:
#
#   curl -fsSL https://raw.githubusercontent.com/BlackVS/aicrew/v0.5.0/boot.sh | bash
#
# It installs the release this script was fetched from (RELEASE below):
# aicrew-agent and aicrew for this platform, each checked against that
# release's SHA256SUMS, as ~/.local/bin/aicrew-agent and ~/.local/bin/aicrew.
# A binary that already reports the release is left as it is; an older one
# is replaced, and both versions are printed. A checksum mismatch installs
# neither. It installs the binaries and nothing else:
# no agent home, no join, no client settings, no file outside the bin
# directory. After a first install, run `aicrew-agent join` (or rerun it
# for an existing home, so its Stop hook names this binary).
#
# Environment knobs:
#   AICREW_BIN_DIR=~/.local/bin     where aicrew-agent and aicrew go
#   AICREW_VERSION=vX.Y.Z           install another release than RELEASE
#   AICREW_REPO=owner/name          install from a fork
set -euo pipefail

# The release this script installs. Bumped together with the CHANGELOG
# when a release is cut (internal/installer checks they agree).
RELEASE=v0.5.0

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
# install_agent: install aicrew-agent and the operator CLI aicrew, both
# $TAG, from $DL_BASE into $BIN_DIR for $OS/$ARCH. A binary that already
# reports $TAG is left alone. Every download is checked against the
# release's SHA256SUMS in a temporary directory, all of them before any is
# installed: nothing in the bin directory changes unless every one passes.
sha256() { if command -v sha256sum >/dev/null 2>&1; then sha256sum "$1"; else shasum -a 256 "$1"; fi | awk '{print $1}'; }
install_agent() {
  local tmp rc=0 name todo=""
  for name in aicrew-agent aicrew; do
    if [ "$(installed_version "$name")" = "$TAG" ]; then
      echo "$name $TAG is current ($BIN_DIR/$name); nothing changed."
    else
      todo="$todo $name"
    fi
  done
  [ -n "$todo" ] || return 0
  tmp=$(mktemp -d)
  fetch_and_swap "$tmp" $todo || rc=$?
  rm -rf "$tmp"
  return "$rc"
}
# installed_version name: the release the installed binary reports, or "".
installed_version() {
  [ -x "$BIN_DIR/$1" ] || return 0
  "$BIN_DIR/$1" version 2>/dev/null | awk '{print $2}' || true
}
# fetch_and_swap tmp name...: every step checks its own result, since a
# function called in a || list runs without errexit.
fetch_and_swap() {
  local tmp=$1 name asset want got old new exe
  shift
  curl -fsSL "$DL_BASE/SHA256SUMS" -o "$tmp/SHA256SUMS" ||
    { echo "ERROR: cannot fetch the SHA256SUMS of $TAG; refusing an unverified binary. Nothing changed." >&2; return 1; }
  for name in "$@"; do
    asset="$name-$OS-$ARCH"
    echo "fetching $name $TAG ($asset)"
    curl -fsSL "$DL_BASE/$asset" -o "$tmp/$name" ||
      { echo "ERROR: cannot fetch $asset of $TAG. Nothing changed." >&2; return 1; }
    want=$(awk -v n="$asset" '$2==n || $2=="*"n {print $1}' "$tmp/SHA256SUMS")
    got=$(sha256 "$tmp/$name")
    if [ -z "$want" ] || [ "$want" != "$got" ]; then
      echo "ERROR: checksum mismatch for $asset (want ${want:-absent}, got $got); refusing it. Nothing changed." >&2
      return 1
    fi
    chmod 755 "$tmp/$name" || return 1
  done
  mkdir -p "$BIN_DIR" || { echo "ERROR: cannot create $BIN_DIR. Nothing changed." >&2; return 1; }
  for name in "$@"; do
    exe="$BIN_DIR/$name"
    old=$(installed_version "$name")
    new=$("$tmp/$name" version 2>/dev/null | awk '{print $2}') || new=""
    # Copy beside the target, then rename over it: atomic, and a running
    # binary keeps the file it started from.
    if ! { cp "$tmp/$name" "$exe.new.$$" && mv -f "$exe.new.$$" "$exe"; }; then
      rm -f "$exe.new.$$"
      echo "ERROR: cannot write $exe; the installed $name is unchanged." >&2
      return 1
    fi
    if [ -n "$old" ]; then
      echo "upgraded $name $old -> ${new:-$TAG} ($exe)"
    else
      echo "installed $name ${new:-$TAG} ($exe)"
    fi
  done
}
# END install-agent

install_agent
case ":$PATH:" in
  *":$BIN_DIR:"*) ;;
  *) echo "note: $BIN_DIR is not on PATH; add it to your shell profile." ;;
esac
