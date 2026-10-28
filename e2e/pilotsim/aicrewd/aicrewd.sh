#!/usr/bin/env bash
# sim-aicrewd's start: the first start installs aicrewd with
# install-aicrewd.sh (AICREW_NO_SERVICE=1, AICREW_PREBUILT_DIR), with the
# run's certificate; every start then runs the command the installer
# printed, as the service user. The operator's commands (aicrew hub add,
# team setup, invitation issue) are run into this container by
# scripts/pilot-sim.sh.
set -euo pipefail
home=/home/sessiond
if [ ! -x "$home/aicrew/bin/aicrewd" ]; then
  install -d -o sessiond -g sessiond -m 700 /etc/sim-tls
  install -o sessiond -g sessiond -m 644 /sim/tls/sim-aicrewd.pem /etc/sim-tls/aicrewd.pem
  install -o sessiond -g sessiond -m 600 /sim/tls/sim-aicrewd.key /etc/sim-tls/aicrewd.key
  AICREW_NO_SERVICE=1 AICREW_PREBUILT_DIR=/opt/aicrew-prebuilt AICREW_HUB_USER=sessiond \
    AICREW_LISTEN=0.0.0.0:9443 AICREW_TLS_CERT=/etc/sim-tls/aicrewd.pem AICREW_TLS_KEY=/etc/sim-tls/aicrewd.key \
    bash /opt/install-aicrewd.sh
fi
exec runuser -u sessiond -- env HOME="$home" "$home/aicrew/bin/aicrewd" -config "$home/aicrew/etc/aicrewd.json"
