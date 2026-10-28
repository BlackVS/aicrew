#!/usr/bin/env bash
# sim-aimem's start. On the first start the pinned release's install-hub.sh
# installs the hub in its install-only mode (AIMEM_NO_SERVICE=1), from the
# staged release asset, which it checks against SHA256SUMS
# (AIMEM_PREBUILT_DIR); a later start finds it installed. The process
# repository is the b4 harness's (a local commit behind an https name).
# The installer's last line is the serve command, which this script execs.
set -euo pipefail
home=/home/aimem
aimem=$home/.local/bin/aimem

install -d -o aimem -g aimem -m 700 "$home/tls"
install -o aimem -g aimem -m 644 /sim/tls/sim-aimem.pem "$home/tls/hub.pem"
install -o aimem -g aimem -m 600 /sim/tls/sim-aimem.key "$home/tls/hub.key"

env_file=$home/.config/aimem/env
if [ ! -x "$aimem" ]; then
  AIMEM_NO_SERVICE=1 AIMEM_PREBUILT_DIR=/opt/aimem-release AIMEM_HUB_USER=aimem \
    AIMEM_HTTP_LISTEN=:8440 AIMEM_HUB_NAME=sim \
    AIMEM_TLS_CERT="$home/tls/hub.pem" AIMEM_TLS_KEY="$home/tls/hub.key" \
    bash /opt/aimem-install-hub.sh | tee /opt/aimem-install.log
  # The hub's introspection credential file is set by hand, as aimem's
  # PILOT-HUB-RUNBOOK says; aimem task 01a1829b-ca55-7000-8d6c-4653bad1c7fb
  # makes it an installer setting.
  grep -q '^AIMEM_INTROSPECTION_TOKEN_FILE=' "$env_file" ||
    echo "AIMEM_INTROSPECTION_TOKEN_FILE=$home/introspection.token" >> "$env_file"
  # The operator steps read the admin token the installer generated (it
  # prints none) from an owner-only copy of the env file's value.
  (umask 077 && sed -n 's/^AIMEM_HTTP_TOKEN=//p' "$env_file" > "$home/admin.token")
  chown aimem:aimem "$home/admin.token"
  [ -s "$home/admin.token" ] || { echo "ERROR: the installer's env file names no AIMEM_HTTP_TOKEN" >&2; exit 1; }
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

serve=$(tail -n 1 /opt/aimem-install.log)
case "$serve" in
  "runuser -u aimem -- "*" serve'") ;;
  *) echo "ERROR: the installer's last line is not a serve command: $serve" >&2; exit 1 ;;
esac
exec sh -c "$serve"
