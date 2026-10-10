#!/usr/bin/env bash
# aicrewd hub installer: install or upgrade aicrew's service on a Debian or
# Ubuntu host, usually the aimem hub's own host. Run AS ROOT:
#
#   curl -fsSL https://raw.githubusercontent.com/BlackVS/aicrew/v0.5.0/install-aicrewd.sh | bash
#
# What it does: installs the release this script was fetched from (RELEASE
# below), aicrewd and aicrew, checked against that release's SHA256SUMS,
# under the service user (with systemd linger) in ~/aicrew/{bin,etc,lib},
# writes the systemd user unit aicrewd.service when there is none, starts
# it and waits until it answers health at that version.
#
# A fresh install also writes ~/aicrew/etc/aicrewd.json, with no hub yet,
# and has `aicrew operator-token new` write the operator credential to
# ~/aicrew/etc/operator.token; this script never reads it. It needs a TLS
# certificate: AICREW_TLS_CERT and AICREW_TLS_KEY naming existing files, or
# AICREW_DOMAIN to generate a self-signed one. With neither it refuses
# before changing anything. At the end it prints what remains to supply.
#
# Upgrades: re-run the one-liner of the newer release. The configuration is
# checked first: one that still has the single aimem block of aicrew 0.2.0
# is moved into aimem_hubs with `aicrewd config migrate`, which needs
# AICREW_CRED_DIR, the directory `aimem identity peer provision` wrote. The
# migration first runs on a scratch copy; if it would not complete, the
# script refuses before changing anything. Then it stops aicrewd, copies
# the store and the configuration beside themselves (<file>.backup-<UTC
# time>), keeps the previous binaries as aicrewd.prev and aicrew.prev, swaps
# the binaries, migrates, starts and waits for health at the new version.
# If the new release does not come up, the previous binaries, the
# configuration and the store copies are put back.
#
# This script never writes a credential or a secret value itself.
#
# Environment knobs:
#   AICREW_HUB_USER=sessiond        service account (created if missing)
#   AICREW_SERVICE_ID=aicrew-service  fresh install: the service's peer ID
#   AICREW_LISTEN=0.0.0.0:9443      fresh install: listen address
#   AICREW_TLS_CERT= AICREW_TLS_KEY=  fresh install: certificate and key files
#   AICREW_DOMAIN=aicrew.example.com  fresh install: generate a self-signed
#                                   certificate for this name instead
#   AICREW_CRED_DIR=                upgrade over a 0.2.0 configuration: the
#                                   directory aimem identity peer provision
#                                   wrote (the hub ID and the team credentials)
#   AICREW_REPO=owner/name          install from a fork
#   AICREW_VERSION=vX.Y.Z           install another release than RELEASE
#   AICREW_UPGRADE_WAIT=30          seconds to wait for health after a start
#   AICREW_PREBUILT_DIR=            a directory holding aicrewd and aicrew to
#                                   install instead of downloading
set -euo pipefail

# The release this script installs. Bumped together with the CHANGELOG
# when a release is cut (internal/installer checks they agree).
RELEASE=v0.5.0

[ "$(id -u)" = 0 ] || { echo "ERROR: run as root." >&2; exit 1; }
for t in curl runuser systemctl loginctl sha256sum; do
  command -v "$t" >/dev/null 2>&1 || { echo "ERROR: '$t' is required." >&2; exit 1; }
done

HUB_USER=${AICREW_HUB_USER:-sessiond}
REPO=${AICREW_REPO:-BlackVS/aicrew}
TAG=${AICREW_VERSION:-$RELEASE}
DL_BASE="https://github.com/$REPO/releases/download/$TAG"
ARCH=amd64
case "$(uname -m)" in aarch64|arm64) ARCH=arm64 ;; esac

# BEGIN download
# fetch_release dir: put the release's aicrewd and aicrew into dir, each
# checked against the release's SHA256SUMS; any failure removes them and
# fails. DL_BASE is the release's download URL and ARCH its architecture.
fetch_release() {
  local dir=$1 a want got
  curl -fsSL "$DL_BASE/SHA256SUMS" -o "$dir/SHA256SUMS" || {
    echo "ERROR: cannot fetch the SHA256SUMS of $DL_BASE; refusing unverified binaries." >&2
    return 1
  }
  for a in aicrewd aicrew; do
    curl -fsSL "$DL_BASE/$a-linux-$ARCH" -o "$dir/$a" || {
      echo "ERROR: cannot fetch $a-linux-$ARCH from $DL_BASE." >&2
      rm -f "$dir/aicrewd" "$dir/aicrew"
      return 1
    }
    want=$(awk -v n="$a-linux-$ARCH" '$2==n || $2=="*"n {print $1}' "$dir/SHA256SUMS")
    got=$(sha256sum "$dir/$a" | awk '{print $1}')
    if [ -z "$want" ] || [ "$want" != "$got" ]; then
      echo "ERROR: checksum mismatch for $a-linux-$ARCH (want ${want:-absent}, got $got); refusing it." >&2
      rm -f "$dir/aicrewd" "$dir/aicrew"
      return 1
    fi
    echo "checksum OK: $a-linux-$ARCH"
  done
  chmod 755 "$dir/aicrewd" "$dir/aicrew"
}
# END download

# BEGIN upgrade-transaction
# The caller defines svc_stop and svc_start (this installation's aicrewd
# service), crew_as (runs a command as the service's user), AICREWD_BIN and
# AICREW_BIN (the installed binaries), CONFIG (aicrewd's configuration) and
# AICREW_CRED_DIR when the configuration still has the 0.2.0 aimem block.

cfg_field() { # show-output name: one field of `aicrewd config show`
  printf '%s\n' "$1" | sed -n "s/^$2=//p" | head -n 1
}
health_version() { # listen-address: the version /healthz answers, "unknown"
  # for a healthy service that names none (releases before 0.4.0), empty
  # when it does not answer.
  local host=${1%:*} port=${1##*:} body
  case "$host" in ""|0.0.0.0|"[::]"|::) host=127.0.0.1 ;; esac
  body=$(curl -fsSk --max-time 2 "https://$host:$port/healthz" 2>/dev/null) || return 0
  case "$body" in *'"status":"ok"'*) ;; *) return 0 ;; esac
  case "$body" in
    *'"version":"'*) printf '%s\n' "$body" | sed -n 's/.*"version":"\([^"]*\)".*/\1/p' ;;
    *) echo unknown ;;
  esac
}
txn_wait() { # listen-address version: wait until health answers at that
  # version; "any" accepts any healthy answer.
  local i=0 n=$(( ${AICREW_UPGRADE_WAIT:-30} * 2 )) v
  while [ "$i" -lt "$n" ]; do
    v=$(health_version "$1")
    if [ -n "$v" ] && { [ "$2" = any ] || [ "$v" = "$2" ]; }; then return 0; fi
    sleep 0.5
    i=$((i + 1))
  done
  return 1
}
store_files() { # store: the store and the SQLite files beside it that exist
  local f
  for f in "$1" "$1-wal" "$1-shm"; do [ -e "$f" ] && printf '%s\n' "$f"; done
  return 0
}
refuse_legacy() { # reason service-id listen-address
  {
    echo "ERROR: $CONFIG still has the single aimem block of aicrew 0.2.0, which this release"
    echo "moves into aimem_hubs, and $1. Nothing was changed."
    echo "  1. On the aimem hub, provision this service into a directory:"
    echo "       aimem identity peer provision ${2:-SERVICE_ID} --endpoint https://THIS_HOST:${3##*:}/v1/crew/introspect \\"
    echo "         --peer-trust-dns --output-dir DIR --hub HUB_URL --admin-token-file FILE"
    echo "  2. Run this installer again with AICREW_CRED_DIR=DIR."
  } >&2
}

# preflight_upgrade new-aicrewd: refuse, before anything changes, an upgrade
# the new release could not finish. A 0.2.0 configuration must migrate
# completely from AICREW_CRED_DIR: the migration runs on a scratch copy.
preflight_upgrade() {
  local new=$1 show scratch out rc=0
  show=$(crew_as "$new" config show -config "$CONFIG") || {
    echo "ERROR: aicrewd $TAG does not accept $CONFIG (above); nothing was changed." >&2
    return 1
  }
  [ "$(cfg_field "$show" legacy_aimem_block)" = yes ] || return 0
  if [ -z "${AICREW_CRED_DIR:-}" ]; then
    refuse_legacy "AICREW_CRED_DIR is not set" "$(cfg_field "$show" service_id)" "$(cfg_field "$show" listen_addr)"
    return 1
  fi
  scratch=$(crew_as mktemp -d)
  crew_as cp -p "$CONFIG" "$scratch/aicrewd.json"
  out=$(crew_as "$new" config migrate -config "$scratch/aicrewd.json" -cred-dir "$AICREW_CRED_DIR" 2>&1) || rc=$?
  crew_as rm -rf "$scratch"
  if [ "$rc" != 0 ]; then
    printf '%s\n' "$out" >&2
    refuse_legacy "the migration from $AICREW_CRED_DIR would not complete (above)" \
      "$(cfg_field "$show" service_id)" "$(cfg_field "$show" listen_addr)"
    return 1
  fi
}

# upgrade_txn staged-dir: replace the installed release with the aicrewd and
# aicrew in staged-dir, after preflight_upgrade passed.
upgrade_txn() {
  local staged=$1 old_v new_v show store listen legacy ts f
  old_v=$(crew_as "$AICREWD_BIN" -version 2>/dev/null | awk '{print $2}') || true
  new_v=$(crew_as "$staged/aicrewd" -version 2>/dev/null | awk '{print $2}') || true
  show=$(crew_as "$staged/aicrewd" config show -config "$CONFIG") || return 1
  store=$(cfg_field "$show" store_path)
  listen=$(cfg_field "$show" listen_addr)
  legacy=$(cfg_field "$show" legacy_aimem_block)
  [ -n "$store" ] && [ -n "$listen" ] || { echo "ERROR: cannot read the store and listen address of $CONFIG; nothing was changed." >&2; return 1; }
  ts=$(date -u +%Y%m%dT%H%M%SZ)
  printf '==> upgrading aicrewd %s -> %s (store %s)\n' "${old_v:-unknown}" "${new_v:-unknown}" "$store"
  svc_stop
  if ! crew_as cp -p "$CONFIG" "$CONFIG.backup-$ts"; then
    svc_start
    echo "ERROR: could not copy $CONFIG; nothing was changed." >&2
    return 1
  fi
  while IFS= read -r f; do
    if ! crew_as cp -p "$f" "$store.backup-$ts${f#"$store"}"; then
      svc_start
      echo "ERROR: could not copy $f; nothing else was changed (the copies made so far end in .backup-$ts)." >&2
      return 1
    fi
  done < <(store_files "$store")
  printf '==> copies: %s.backup-%s, %s.backup-%s\n' "$CONFIG" "$ts" "$store" "$ts"
  if ! { crew_as cp -p "$AICREWD_BIN" "$AICREWD_BIN.prev" && crew_as cp -p "$AICREW_BIN" "$AICREW_BIN.prev"; }; then
    svc_start
    echo "ERROR: could not keep the previous binaries as .prev; the release in place is unchanged." >&2
    return 1
  fi
  if ! { crew_as cp "$staged/aicrewd" "$AICREWD_BIN.new" && crew_as cp "$staged/aicrew" "$AICREW_BIN.new" &&
         crew_as chmod 755 "$AICREWD_BIN.new" "$AICREW_BIN.new" &&
         crew_as mv "$AICREWD_BIN.new" "$AICREWD_BIN" && crew_as mv "$AICREW_BIN.new" "$AICREW_BIN"; }; then
    echo "ERROR: could not swap the binaries; rolling back." >&2
    rollback "$ts" "$store" "$listen" "$old_v"
    return 1
  fi
  if [ "$legacy" = yes ]; then
    if ! crew_as "$AICREWD_BIN" config migrate -config "$CONFIG" -cred-dir "$AICREW_CRED_DIR"; then
      echo "ERROR: aicrewd config migrate did not complete; rolling back." >&2
      rollback "$ts" "$store" "$listen" "$old_v"
      return 1
    fi
  fi
  svc_start
  if txn_wait "$listen" "$new_v"; then
    printf '==> upgraded aicrewd %s -> %s; the service answers health at %s\n' "${old_v:-unknown}" "$new_v" "$new_v"
    printf '==> the previous binaries are %s.prev and %s.prev; the copies stay at %s.backup-%s and %s.backup-%s (remove them once satisfied)\n' \
      "$AICREWD_BIN" "$AICREW_BIN" "$CONFIG" "$ts" "$store" "$ts"
    return 0
  fi
  echo "ERROR: aicrewd ${new_v:-(new)} did not answer health at its version within ${AICREW_UPGRADE_WAIT:-30}s; rolling back." >&2
  rollback "$ts" "$store" "$listen" "$old_v"
  return 1
}

# rollback ts store listen old-version: put the previous binaries, the
# configuration copy and the store copy back, and start the service again.
rollback() {
  local ts=$1 store=$2 listen=$3 old_v=$4 f ok=1
  svc_stop
  [ ! -e "$AICREWD_BIN.prev" ] || crew_as cp -p "$AICREWD_BIN.prev" "$AICREWD_BIN" || ok=""
  [ ! -e "$AICREW_BIN.prev" ] || crew_as cp -p "$AICREW_BIN.prev" "$AICREW_BIN" || ok=""
  crew_as cp -p "$CONFIG.backup-$ts" "$CONFIG" || ok=""
  while IFS= read -r f; do
    crew_as mv "$f" "$f.failed-$ts" || ok=""
  done < <(store_files "$store")
  while IFS= read -r f; do
    crew_as cp -p "$f" "$store${f#"$store.backup-$ts"}" || ok=""
  done < <(store_files "$store.backup-$ts")
  if [ -z "$ok" ]; then
    echo "ERROR: the rollback could not restore every file: put $AICREWD_BIN.prev, $CONFIG.backup-$ts and $store.backup-$ts back by hand before starting the service." >&2
    return 1
  fi
  svc_start
  if txn_wait "$listen" any; then
    echo "ROLLED BACK: aicrewd ${old_v:-unknown} is running again on the configuration and store copied before the upgrade ($CONFIG.backup-$ts, $store.backup-$ts; the store the failed upgrade left ends in .failed-$ts)." >&2
  else
    echo "ROLLED BACK: the previous binaries, configuration and store are restored, but the service does not answer health; check it (journalctl --user -u aicrewd as the service user)." >&2
  fi
}
# END upgrade-transaction

# --- preflight: refuse before any change ------------------------------------
HOME_DIR=""
if id "$HUB_USER" >/dev/null 2>&1; then HOME_DIR=$(getent passwd "$HUB_USER" | cut -d: -f6); fi
ROOT_DIR=${HOME_DIR:-/nonexistent}/aicrew
AICREWD_BIN="$ROOT_DIR/bin/aicrewd"
AICREW_BIN="$ROOT_DIR/bin/aicrew"
CONFIG="$ROOT_DIR/etc/aicrewd.json"
UPGRADE=""
[ -n "$HOME_DIR" ] && [ -e "$AICREWD_BIN" ] && UPGRADE=1

SERVICE_ID=${AICREW_SERVICE_ID:-aicrew-service}
LISTEN=${AICREW_LISTEN:-0.0.0.0:9443}
if [ -z "$UPGRADE" ] && [ ! -e "$CONFIG" ]; then
  printf '%s' "$SERVICE_ID" | grep -Eqx '[A-Za-z0-9._:-]{1,128}' && [ "$SERVICE_ID" != . ] && [ "$SERVICE_ID" != .. ] ||
    { echo "ERROR: AICREW_SERVICE_ID must be 1 to 128 characters from [A-Za-z0-9._:-]; nothing was changed." >&2; exit 1; }
  printf '%s' "$LISTEN" | grep -Eqx '[][A-Za-z0-9.:-]*:[0-9]{1,5}' ||
    { echo "ERROR: AICREW_LISTEN must be host:port; nothing was changed." >&2; exit 1; }
  if [ -n "${AICREW_TLS_CERT:-}${AICREW_TLS_KEY:-}" ]; then
    for f in "${AICREW_TLS_CERT:-}" "${AICREW_TLS_KEY:-}"; do
      case "$f" in /*) ;; *) echo "ERROR: AICREW_TLS_CERT and AICREW_TLS_KEY must both name a file by its absolute path; nothing was changed." >&2; exit 1 ;; esac
      case "$f" in *'"'*|*\\*) echo "ERROR: $f: a path with a quote or a backslash is refused; nothing was changed." >&2; exit 1 ;; esac
      [ -f "$f" ] || { echo "ERROR: $f does not exist; nothing was changed." >&2; exit 1; }
      [ -z "$HOME_DIR" ] || runuser -u "$HUB_USER" -- test -r "$f" ||
        { echo "ERROR: $HUB_USER cannot read $f; nothing was changed." >&2; exit 1; }
    done
  elif [ -n "${AICREW_DOMAIN:-}" ]; then
    printf '%s' "$AICREW_DOMAIN" | grep -Eqx '[A-Za-z0-9.-]{1,253}' ||
      { echo "ERROR: AICREW_DOMAIN must be a host name; nothing was changed." >&2; exit 1; }
    command -v openssl >/dev/null 2>&1 || { echo "ERROR: 'openssl' is required for AICREW_DOMAIN." >&2; exit 1; }
  else
    {
      echo "ERROR: a fresh install needs a TLS certificate; nothing was changed. Either"
      echo "  AICREW_TLS_CERT=/path/cert.pem AICREW_TLS_KEY=/path/key.pem (existing files the service user can read), or"
      echo "  AICREW_DOMAIN=aicrew.example.com to generate a self-signed certificate."
    } >&2
    exit 1
  fi
fi

STAGED=$(mktemp -d)
trap 'rm -rf "$STAGED"' EXIT
chmod 755 "$STAGED"
if [ -n "${AICREW_PREBUILT_DIR:-}" ]; then
  install -m 755 "$AICREW_PREBUILT_DIR/aicrewd" "$STAGED/aicrewd"
  install -m 755 "$AICREW_PREBUILT_DIR/aicrew" "$STAGED/aicrew"
  echo "installing the prebuilt binaries from $AICREW_PREBUILT_DIR"
else
  echo "installing aicrewd and aicrew $TAG (linux-$ARCH)"
  fetch_release "$STAGED"
fi

as_user() { runuser -u "$HUB_USER" -- "$@"; }
sysuser() { runuser -u "$HUB_USER" -- env "XDG_RUNTIME_DIR=/run/user/$(id -u "$HUB_USER")" systemctl --user "$@"; }
crew_as() { runuser -u "$HUB_USER" -- env "HOME=$HOME_DIR" "$@"; }
svc_stop() { sysuser stop aicrewd.service 2>/dev/null || true; }
# A release that failed to start leaves the unit failed, and past its start
# limit systemd refuses to start it again until the failure is reset.
svc_start() { sysuser reset-failed aicrewd.service 2>/dev/null || true; sysuser restart aicrewd.service || true; }

if [ -n "$UPGRADE" ]; then
  preflight_upgrade "$STAGED/aicrewd" || exit 1
  sysuser daemon-reload
  upgrade_txn "$STAGED" || exit 1
  exit 0
fi

# --- fresh install ----------------------------------------------------------
if [ -z "$HOME_DIR" ]; then
  useradd -m -s /bin/bash "$HUB_USER"
  echo "created user $HUB_USER"
  HOME_DIR=$(getent passwd "$HUB_USER" | cut -d: -f6)
  ROOT_DIR=$HOME_DIR/aicrew
  AICREWD_BIN="$ROOT_DIR/bin/aicrewd"
  AICREW_BIN="$ROOT_DIR/bin/aicrew"
  CONFIG="$ROOT_DIR/etc/aicrewd.json"
fi
loginctl enable-linger "$HUB_USER"
# Linger starts the user's systemd instance; systemctl --user needs its bus.
for _ in $(seq 1 20); do [ -S "/run/user/$(id -u "$HUB_USER")/bus" ] && break; sleep 0.5; done
as_user mkdir -p "$ROOT_DIR/bin" "$ROOT_DIR/etc" "$ROOT_DIR/lib" "$HOME_DIR/.config/systemd/user"
as_user chmod 700 "$ROOT_DIR/etc" "$ROOT_DIR/lib"
as_user cp "$STAGED/aicrewd" "$AICREWD_BIN"
as_user cp "$STAGED/aicrew" "$AICREW_BIN"
as_user chmod 755 "$AICREWD_BIN" "$AICREW_BIN"

if [ -e "$CONFIG" ]; then
  echo "kept existing $CONFIG"
else
  if [ -n "${AICREW_TLS_CERT:-}" ]; then
    as_user test -r "$AICREW_TLS_CERT" && as_user test -r "$AICREW_TLS_KEY" ||
      { echo "ERROR: $HUB_USER cannot read $AICREW_TLS_CERT or $AICREW_TLS_KEY." >&2; exit 1; }
    CERT=$AICREW_TLS_CERT KEY=$AICREW_TLS_KEY
  else
    CERT=$ROOT_DIR/etc/tls/cert.pem KEY=$ROOT_DIR/etc/tls/key.pem
    if [ ! -f "$CERT" ]; then
      as_user mkdir -p "$ROOT_DIR/etc/tls"
      as_user openssl req -x509 -newkey ec -pkeyopt ec_paramgen_curve:prime256v1 \
        -keyout "$KEY" -out "$CERT" -days 3650 -nodes -subj "/CN=$AICREW_DOMAIN" \
        -addext "subjectAltName=DNS:$AICREW_DOMAIN" 2>/dev/null
      as_user chmod 600 "$KEY"
      echo "generated a self-signed certificate for $AICREW_DOMAIN in $ROOT_DIR/etc/tls (replace it with a real one when enrolled)"
    fi
  fi
  if [ -e "$ROOT_DIR/etc/operator.token" ]; then
    echo "kept existing $ROOT_DIR/etc/operator.token"
  else
    as_user "$AICREW_BIN" operator-token new --output "$ROOT_DIR/etc/operator.token"
  fi
  as_user sh -c 'umask 077 && cat > "$1"' sh "$CONFIG" <<EOF
{
  "store_path": "$ROOT_DIR/lib/aicrew.db",
  "listen_addr": "$LISTEN",
  "tls_cert_file": "$CERT",
  "tls_key_file": "$KEY",
  "service_id": "$SERVICE_ID",
  "operator_token_file": "$ROOT_DIR/etc/operator.token"
}
EOF
  crew_as "$AICREWD_BIN" config show -config "$CONFIG" >/dev/null
  echo "wrote $CONFIG"
fi

UNIT=$HOME_DIR/.config/systemd/user/aicrewd.service
if [ -e "$UNIT" ]; then
  echo "kept existing $UNIT"
else
  as_user sh -c 'cat > "$1"' sh "$UNIT" <<'EOF'
[Unit]
Description=aicrewd - aicrew service

[Service]
ExecStart=%h/aicrew/bin/aicrewd -config %h/aicrew/etc/aicrewd.json
Restart=on-failure
RestartSec=2
UMask=0077

[Install]
WantedBy=default.target
EOF
  echo "wrote $UNIT"
fi
sysuser daemon-reload
sysuser enable aicrewd.service >/dev/null 2>&1 || true
svc_start

SHOW=$(crew_as "$AICREWD_BIN" config show -config "$CONFIG")
LISTEN=$(cfg_field "$SHOW" listen_addr)
SERVICE_ID=$(cfg_field "$SHOW" service_id)
WANT=$(crew_as "$AICREWD_BIN" -version | awk '{print $2}')
if txn_wait "$LISTEN" "$WANT"; then
  echo "aicrewd is up: $WANT on $LISTEN"
else
  echo "WARNING: aicrewd did not answer health at $WANT; inspect: journalctl --user -u aicrewd (as $HUB_USER)" >&2
  exit 1
fi
cat <<EOF

Still to supply (this installer writes no credential):
  1. On the aimem hub, provision this service ($SERVICE_ID) into a directory DIR:
       aimem identity peer provision $SERVICE_ID --endpoint https://THIS_HOST:${LISTEN##*:}/v1/crew/introspect \\
         --peer-trust-dns --output-dir DIR --hub HUB_URL --admin-token-file FILE
  2. As $HUB_USER, bind the hub (aimem_hubs in $CONFIG):
       $AICREW_BIN hub add NAME --config $CONFIG --base-url HUB_URL \\
         --tls-trust-mode ca_dns --tls-trust-value HUB_HOST --cred-dir DIR
  3. Restart aicrewd: systemctl --user restart aicrewd
The operator credential is $ROOT_DIR/etc/operator.token (aicrew --token-file); it was never shown.
EOF
