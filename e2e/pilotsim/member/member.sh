#!/usr/bin/env bash
# A member's start: wait (bounded) for the files the scenario writes into
# /sim/in (the invitation code, the aimem token, the forge token, each
# owner only, then "ready"), join once with them, and run serve, restarted
# after a failure as the installed service restarts it. Nothing is
# typed: join reads the code with --code-file. The member reaches aicrewd
# and the hub through the fault proxies sim-px-crew and sim-px-aimem
# (e2e/pilotsim/compose.yaml), and runs aimem through aimem-timed, whose
# gates the fault cases set.
set -euo pipefail
: "${SIM_ROLE:?}" "${SIM_WAIT:=900}"
home=$HOME/agent
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
  aicrew-agent join --label "$SIM_ROLE" --home "$home" --url https://sim-px-crew:9443 \
    --tls-trust-mode spki_sha256 --tls-trust-value "$PIN_SIM_AICREWD" --aimem-hub sim --client claude \
    --wake turns --no-service --code-file "$in/invite" --aimem-command /usr/local/bin/aimem-timed \
    --aimem-url https://sim-px-aimem:8440 --aimem-token-file "$in/aimem.token" --aimem-ca-file /sim/tls/ca.pem \
    --cred "sim-gitea:3000=$in/forge.token"
  rm -f "$in/invite" "$in/aimem.token" "$in/forge.token"
fi
# serve runs as an installed member's service runs it (internal/service/
# render.go: Restart=on-failure, RestartSec=10, RestartPreventExitStatus=2):
# after a failure, such as a session refresh that met the hub stopped in
# case F6-unreachable, it starts again 10 seconds later; a clean stop or
# exit 2 ends the container. A stop signal goes to serve.
pid=
trap '[ -z "$pid" ] || kill -TERM "$pid" 2>/dev/null; [ -z "$pid" ] || wait "$pid"; exit 0' TERM INT
while :; do
  aicrew-agent serve --home "$home" &
  pid=$!
  rc=0
  wait "$pid" || rc=$?
  pid=
  case $rc in 0 | 2) exit "$rc" ;; esac
  echo "member.sh: serve exited $rc; it starts again in 10 s, as the installed service's does" >&2
  sleep 10
done
