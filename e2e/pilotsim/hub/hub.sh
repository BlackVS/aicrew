#!/usr/bin/env bash
# sim-aimem's start: what install-hub.sh does on a systemd host, without
# the service. The release asset is checked against SHA256SUMS as the
# installer checks it; the admin token and the env file are written once,
# owner only; the process repository is the b4 harness's (a local commit
# behind an https name); then aimem serve runs as the hub's user.
set -euo pipefail
asset=aimem-linux-amd64
case "$(uname -m)" in aarch64|arm64) asset=aimem-linux-arm64 ;; esac
rel=/opt/aimem-release
home=/home/aimem

if [ ! -x /usr/local/bin/aimem ]; then
  want=$(awk -v a="$asset" '$2==a{print $1}' "$rel/SHA256SUMS")
  got=$(sha256sum "$rel/$asset" | awk '{print $1}')
  if [ -z "$want" ] || [ "$want" != "$got" ]; then
    echo "ERROR: checksum mismatch for $asset (want ${want:-absent}, got $got)." >&2
    exit 1
  fi
  echo "checksum OK: $asset"
  install -m 755 "$rel/$asset" /usr/local/bin/aimem
fi

install -d -o aimem -g aimem -m 700 "$home/tls" "$home/.config/aimem"
install -o aimem -g aimem -m 644 /sim/tls/sim-aimem.pem "$home/tls/hub.pem"
install -o aimem -g aimem -m 600 /sim/tls/sim-aimem.key "$home/tls/hub.key"
env_file=$home/.config/aimem/env
if [ ! -f "$env_file" ]; then
  umask 077
  token=$(head -c 24 /dev/urandom | od -An -tx1 | tr -d ' \n')
  printf '%s\n' "$token" > "$home/admin.token"
  {
    echo "AIMEM_HTTP_LISTEN=:8440"
    echo "AIMEM_HTTP_TOKEN=$token"
    echo "AIMEM_TLS_CERT=$home/tls/hub.pem"
    echo "AIMEM_TLS_KEY=$home/tls/hub.key"
    echo "AIMEM_HUB_NAME=sim"
    echo "AIMEM_INTROSPECTION_TOKEN_FILE=$home/introspection.token"
  } > "$env_file"
  unset token
  chown aimem:aimem "$home/admin.token" "$env_file"
  umask 022
fi

if [ ! -d "$home/process/.git" ]; then
  cp -r /opt/process "$home/process"
  chown -R aimem:aimem "$home/process"
  runuser -u aimem -- env HOME="$home" sh -c '
    cd "$HOME/process" &&
    git init -q &&
    git add process &&
    git -c user.name=sim -c user.email=sim@example.test -c commit.gpgsign=false commit -q -m process &&
    git config --global url."file://$HOME/process".insteadOf https://git.example.test/sim/process.git'
fi

set -a
. "$env_file"
set +a
exec runuser -u aimem -- env HOME="$home" /usr/local/bin/aimem serve
