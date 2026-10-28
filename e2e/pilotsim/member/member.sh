#!/usr/bin/env bash
# A member's start: wait (bounded) for the files the scenario writes into
# /sim/in (the invitation code, the aimem token, the forge token, each
# owner only, then "ready"), join once with them, and run serve. Nothing is
# typed: join reads the code with --code-file.
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
  aicrew-agent join --label "$SIM_ROLE" --home "$home" --url https://sim-aicrewd:9443 \
    --tls-trust-mode spki_sha256 --tls-trust-value "$PIN_SIM_AICREWD" --aimem-hub sim --client claude \
    --wake turns --no-service --code-file "$in/invite" \
    --aimem-url https://sim-aimem:8440 --aimem-token-file "$in/aimem.token" --aimem-ca-file /sim/tls/ca.pem \
    --cred "sim-gitea:3000=$in/forge.token"
  rm -f "$in/invite" "$in/aimem.token" "$in/forge.token"
fi
exec aicrew-agent serve --home "$home"
