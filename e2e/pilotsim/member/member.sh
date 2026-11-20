#!/usr/bin/env bash
# A member's start: wait (bounded) for the files the scenario writes into
# /sim/in (the invitation code, the aimem token, the forge token and the
# hub's CA, each owner only, the document's command lines, then "ready"),
# run the versions, the preflight and join once with them, and run serve,
# restarted after a failure as the installed service restarts it.
#
# The command lines are docs/BRING-UP.md's own (steps 5 and 6), which the
# runner reads from the document with e2e/docsteps and fills in;
# doclines.sh runs them and records each id that ran in
# $HOME/docsteps.ran for the runner's check. join runs in /sim/in, where
# its input files have the document's names, with AICREW_NO_SERVICE=1,
# since the container has no systemd user
# manager (docs/E2E-PILOT-SIM.md, "Where the simulation differs from the
# document"). Nothing is typed: join reads the code with --code-file. The
# member reaches aicrewd and the hub through the fault proxies sim-px-crew
# and sim-px-aimem (e2e/pilotsim/compose.yaml), and runs aimem through
# aimem-timed, whose gates the fault cases set.
set -euo pipefail
: "${SIM_ROLE:?}" "${SIM_WAIT:=900}"
label=$SIM_ROLE
[ "$label" != indep ] || label=independent
home=$HOME/aicrew/agents/$label
in=/sim/in
for _ in $(seq "$SIM_WAIT"); do
  [ -f "$in/ready" ] && break
  sleep 1
done
[ -f "$in/ready" ] || { echo "member.sh: no inputs in $in after ${SIM_WAIT}s" >&2; exit 1; }
set -a
. /sim/tls/pins.env
set +a
if [ ! -f "$home/agent.json" ] || ! grep -q '"agent_id"' "$home/agent.json"; then
  # A member installed from the export (the upgrade scenario's previous
  # release) joins the closed-network way, with the export named, so that
  # its check's instructions name the export (docs/E2E-PILOT-SIM.md).
  base=
  [ ! -f /opt/aicrew-export/MANIFEST ] || base=/opt/aicrew-export
  cd "$in"
  if [ -f "$in/join.line" ]; then
    # The versions (step 5), the client's included, then the preflight a
    # person runs before join, with join's own values: it spends nothing,
    # and a member that fails it stops here; then join (step 6).
    AICREW_BASE=$base AICREW_NO_SERVICE=1 sh /usr/local/bin/doclines.sh "$in" "$HOME/docsteps.ran" \
      agent-version aimem-version client-version preflight join
    rm -f "$in"/*.line
  else
    # A release older than the document's form (the below-minimum run's
    # start, which has neither the preflight nor AICREW_NO_SERVICE) joins
    # in the simulation's own form; the runner writes no lines for it.
    AICREW_BASE=$base aicrew-agent join --label "$label" --url https://sim-px-crew:9443 \
      --tls-trust-mode spki_sha256 --tls-trust-value "$PIN_SIM_AICREWD" --aimem-hub main --client claude \
      --wake turns --no-service --code-file invite.code --aimem-command /usr/local/bin/aimem \
      --aimem-url https://sim-px-aimem:8440 --aimem-token-file member.token --aimem-ca-file hub-ca.pem \
      --cred "sim-gitea:3000=forge.token"
  fi
  rm -f invite.code member.token forge.token hub-ca.pem
  cd "$HOME"
fi
# serve runs as an installed member's service runs it (internal/service/
# render.go: Restart=on-failure, RestartSec=10, RestartPreventExitStatus=2):
# after a failure, such as a session refresh that met the hub stopped in
# case F6-unreachable, it starts again 10 seconds later; a clean stop ends
# the container. After exit 2 the service stays stopped and the host stays
# up, so check and an instruction still run in the home, as an operator
# runs them on a host whose service stopped. A stop signal goes to serve,
# or ends the wait.
pid=
trap '[ -z "$pid" ] || kill -TERM "$pid" 2>/dev/null; [ -z "$pid" ] || wait "$pid"; exit 0' TERM INT
while :; do
  aicrew-agent serve --home "$home" &
  pid=$!
  rc=0
  wait "$pid" || rc=$?
  pid=
  case $rc in
    0) exit 0 ;;
    2)
      echo "member.sh: serve exited 2; the service stays stopped, as the installed service's does, and the host stays up" >&2
      # A stop signal now ends only the wait.
      trap '[ -z "$pid" ] || kill "$pid" 2>/dev/null; exit 0' TERM INT
      while :; do
        sleep 3600 &
        pid=$!
        wait "$pid" || true
      done ;;
  esac
  echo "member.sh: serve exited $rc; it starts again in 10 s, as the installed service's does" >&2
  sleep 10
done
