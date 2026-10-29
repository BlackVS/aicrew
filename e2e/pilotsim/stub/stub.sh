#!/bin/sh
# A stub's start: wait (bounded) for its own forge token in /sim/in and the
# scenario file the runner writes into /sim/scenario (the token first, the
# scenario last, by a rename), then run the stub in the mode SIM_STUB names.
set -eu
: "${SIM_STUB:?}" "${SIM_WAIT:=900}"
i=0
while [ ! -f /sim/in/forge.token ] || [ ! -f /sim/scenario/scenario.json ]; do
  i=$((i + 1))
  [ "$i" -le "$SIM_WAIT" ] || { echo "stub.sh: no token or scenario after ${SIM_WAIT}s" >&2; exit 1; }
  sleep 1
done
exec simstub "$SIM_STUB" -scenario /sim/scenario/scenario.json -token-file /sim/in/forge.token \
  -api https://sim-gitea:3000/api/v1
