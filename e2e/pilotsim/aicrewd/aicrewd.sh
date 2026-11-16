#!/usr/bin/env bash
# sim-aicrewd's start: the first start installs aicrewd with
# install-aicrewd.sh (AICREW_NO_SERVICE=1), with the run's certificate:
# from this tree's binaries (AICREW_PREBUILT_DIR), or, in the upgrade
# scenario, from the previous release's assets laid out as an aiforge
# export (AICREW_BASE), by that release's own installer. A start that finds
# a new release's files in /opt/aicrew-upgrade (scripts/pilot-sim.sh copies
# them into the stopped container, in place of a download) runs that
# release's installer over the installation first, while aicrewd is
# stopped, as the installer's install-only upgrade asks. Every start then
# runs the command the installer printed, as the service user. The
# operator's commands (aicrew hub add, team setup, invitation issue) are run
# into this container by scripts/pilot-sim.sh.
set -euo pipefail
home=/home/sessiond
# The installer's settings, the same at the first install and the upgrade.
settings=(AICREW_NO_SERVICE=1 AICREW_HUB_USER=sessiond AICREW_LISTEN=0.0.0.0:9443
  AICREW_TLS_CERT=/etc/sim-tls/aicrewd.pem AICREW_TLS_KEY=/etc/sim-tls/aicrewd.key)
if [ ! -x "$home/aicrew/bin/aicrewd" ]; then
  install -d -o sessiond -g sessiond -m 700 /etc/sim-tls
  install -o sessiond -g sessiond -m 644 /sim/tls/sim-aicrewd.pem /etc/sim-tls/aicrewd.pem
  install -o sessiond -g sessiond -m 600 /sim/tls/sim-aicrewd.key /etc/sim-tls/aicrewd.key
  if [ -f /opt/aicrew-export/MANIFEST ]; then
    tag=$(sed -n 's/^aicrew //p' /opt/aicrew-export/MANIFEST)
    env "${settings[@]}" AICREW_BASE=/opt/aicrew-export bash "/opt/aicrew-export/aicrew/$tag/install-aicrewd.sh"
  else
    env "${settings[@]}" AICREW_PREBUILT_DIR=/opt/aicrew-prebuilt bash /opt/install-aicrewd.sh
  fi
fi
if [ -f /opt/aicrew-upgrade/install-aicrewd.sh ]; then
  echo "aicrewd.sh: upgrading with the new release's install-aicrewd.sh"
  env "${settings[@]}" AICREW_PREBUILT_DIR=/opt/aicrew-upgrade/prebuilt bash /opt/aicrew-upgrade/install-aicrewd.sh
  mv /opt/aicrew-upgrade /opt/aicrew-upgrade.done
fi
exec runuser -u sessiond -- env HOME="$home" "$home/aicrew/bin/aicrewd" -config "$home/aicrew/etc/aicrewd.json"
