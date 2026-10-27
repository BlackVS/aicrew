#!/usr/bin/env bash
# aicrew-agent installer for Linux and macOS: installs or upgrades the
# member's aicrew-agent, and the operator CLI aicrew beside it, from any
# directory:
#
#   curl -fsSL https://raw.githubusercontent.com/BlackVS/aicrew/v0.8.0/boot.sh | bash
#
# It installs the release this script was fetched from (RELEASE below):
# aicrew-agent and aicrew for this platform, each checked against that
# release's SHA256SUMS, as ~/.local/bin/aicrew-agent and ~/.local/bin/aicrew.
# A binary that already reports the release is left as it is; an older one
# is replaced, and both versions are printed. A checksum mismatch installs
# neither. It installs the binaries and nothing else:
# no agent home, no join, no client settings, no file outside the bin
# directory. After a first install, run `aicrew-agent join` (or rerun it
# for an existing home, so its Stop hook names this binary). A member home
# in turns mode runs as a service (aicrew-agent service): an upgrade stops
# every home's service before the swap and starts it again after.
#
# Environment knobs:
#   AICREW_BIN_DIR=~/.local/bin     where aicrew-agent and aicrew go
#   AICREW_VERSION=vX.Y.Z           install another release than RELEASE
#   AICREW_REPO=owner/name          install from a fork
#   AICREW_NO_SERVICE=1             leave the members' services alone
#   AICREW_PREBUILT_DIR=            a directory holding aicrew-agent and aicrew to
#                                   install instead of downloading; a SHA256SUMS
#                                   in it, when present, is checked
set -euo pipefail

# The release this script installs. Bumped together with the CHANGELOG
# when a release is cut (internal/installer checks they agree).
RELEASE=v0.8.0

[ -n "${AICREW_PREBUILT_DIR:-}" ] || command -v curl >/dev/null 2>&1 ||
  { echo "ERROR: 'curl' is required." >&2; exit 1; }
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
# $TAG, from $DL_BASE into $BIN_DIR for $OS/$ARCH, or the ones in
# $AICREW_PREBUILT_DIR when it is set. A binary that already reports $TAG,
# or is the same file as the prebuilt one, is left alone. Every download is
# checked against the release's SHA256SUMS in a temporary directory, all of
# them before any is installed: nothing in the bin directory changes unless
# every one passes. Both prebuilt binaries are staged and checked the same
# way, against their directory's SHA256SUMS when it holds one, before either
# is judged current, so a mismatch for one never lets the other through.
sha256() { if command -v sha256sum >/dev/null 2>&1; then sha256sum "$1"; else shasum -a 256 "$1"; fi | awk '{print $1}'; }
install_agent() {
  local tmp rc=0 name todo=""
  tmp=$(mktemp -d)
  if [ -n "${AICREW_PREBUILT_DIR:-}" ]; then
    stage_prebuilt "$tmp" aicrew-agent aicrew || rc=$?
  fi
  if [ "$rc" = 0 ]; then
    for name in aicrew-agent aicrew; do
      if is_current "$name" "$tmp"; then
        echo "$name $(installed_version "$name") is current ($BIN_DIR/$name); nothing changed."
      else
        todo="$todo $name"
      fi
    done
    [ -z "$todo" ] || fetch_and_swap "$tmp" $todo || rc=$?
  fi
  rm -rf "$tmp"
  return "$rc"
}
# installed_version name: the release the installed binary reports, or "".
installed_version() {
  [ -x "$BIN_DIR/$1" ] || return 0
  "$BIN_DIR/$1" version 2>/dev/null | awk '{print $2}' || true
}
# is_current name tmp: whether the installed binary is the one to install:
# it reports $TAG, or it is the same file as the prebuilt one already staged
# and checked in tmp. A prebuilt binary is compared by its hash, never run.
is_current() {
  if [ -z "${AICREW_PREBUILT_DIR:-}" ]; then
    [ "$(installed_version "$1")" = "$TAG" ]
  else
    [ -f "$BIN_DIR/$1" ] && [ "$(sha256 "$BIN_DIR/$1")" = "$(sha256 "$2/$1")" ]
  fi
}
# fetch_and_swap tmp name...: every step checks its own result, since a
# function called in a || list runs without errexit. Prebuilt binaries are
# already staged in tmp by install_agent.
fetch_and_swap() {
  local tmp=$1 name old new exe
  shift
  if [ -n "${AICREW_PREBUILT_DIR:-}" ]; then
    for name in "$@"; do echo "installing the prebuilt $name from $AICREW_PREBUILT_DIR"; done
  else
    stage_release "$tmp" "$@" || return 1
  fi
  mkdir -p "$BIN_DIR" || { echo "ERROR: cannot create $BIN_DIR. Nothing changed." >&2; return 1; }
  case " $* " in *" aicrew-agent "*) svc_stop_all "$tmp/aicrew-agent" ;; esac
  for name in "$@"; do
    exe="$BIN_DIR/$name"
    old=$(installed_version "$name")
    new=$("$tmp/$name" version 2>/dev/null | awk '{print $2}') || new=""
    # Copy beside the target, then rename over it: atomic, and a running
    # binary keeps the file it started from.
    if ! { cp "$tmp/$name" "$exe.new.$$" && mv -f "$exe.new.$$" "$exe"; }; then
      rm -f "$exe.new.$$"
      echo "ERROR: cannot write $exe; the installed $name is unchanged." >&2
      svc_start_all
      return 1
    fi
    if [ -n "$old" ]; then
      echo "upgraded $name $old -> ${new:-$TAG} ($exe)"
    else
      echo "installed $name ${new:-$TAG} ($exe)"
    fi
  done
  svc_start_all
}
# stage_prebuilt tmp name...: copy each binary from $AICREW_PREBUILT_DIR
# into tmp and check the copy against the directory's SHA256SUMS, which
# may list it by its name or by its release asset name, when there is one.
stage_prebuilt() {
  local tmp=$1 dir=$AICREW_PREBUILT_DIR name want got
  shift
  for name in "$@"; do
    [ -f "$dir/$name" ] ||
      { echo "ERROR: $dir/$name is missing; AICREW_PREBUILT_DIR must hold aicrew-agent and aicrew. Nothing changed." >&2; return 1; }
    cp "$dir/$name" "$tmp/$name" || return 1
    if [ -f "$dir/SHA256SUMS" ]; then
      want=$(awk -v n="$name" -v a="$name-$OS-$ARCH" \
        '$2==n || $2=="*"n || $2==a || $2=="*"a {print $1; exit}' "$dir/SHA256SUMS")
      got=$(sha256 "$tmp/$name")
      if [ -z "$want" ] || [ "$want" != "$got" ]; then
        echo "ERROR: checksum mismatch for $dir/$name (want ${want:-absent}, got $got); refusing it. Nothing changed." >&2
        return 1
      fi
    fi
    chmod 755 "$tmp/$name" || return 1
  done
}
# stage_release tmp name...: download each binary's release asset of $TAG
# into tmp and check it against the release's SHA256SUMS.
stage_release() {
  local tmp=$1 name asset want got
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
}
# The members' services (aicrew-agent service): stop, swap, start. The new
# aicrew-agent, still in the temporary directory, stops every registered
# home's service before the swap; the installed one starts them again after
# it, rewriting each definition with its own path. A home with no service,
# or AICREW_NO_SERVICE=1, is left alone.
SVC_STOPPED=""
svc_stop_all() {
  [ "${AICREW_NO_SERVICE:-}" != 1 ] || return 0
  SVC_STOPPED=$("$1" service stop --all) ||
    echo "warning: not every member's service stopped; the swap goes on, and each is started again after it." >&2
  [ -z "$SVC_STOPPED" ] || printf 'stopped for the upgrade:\n%s\n' "$SVC_STOPPED"
}
svc_start_all() {
  [ -n "$SVC_STOPPED" ] || return 0
  SVC_STOPPED=""
  "$BIN_DIR/aicrew-agent" service start --all ||
    echo "warning: not every member's service started; see: aicrew-agent service status --all" >&2
}
# END install-agent

install_agent
case ":$PATH:" in
  *":$BIN_DIR:"*) ;;
  *) echo "note: $BIN_DIR is not on PATH; add it to your shell profile." ;;
esac
