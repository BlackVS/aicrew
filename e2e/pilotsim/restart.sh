# shellcheck shell=bash
# The fault cases' restart of a killed service (docs/E2E-PILOT-SIM.md,
# "The fault cases"), sourced by faults.sh and by restart_test.sh. It uses
# the runner's dc, step, fail, waitfor and healthy, and OUT and
# DOCKER_PROJECT.
#
# `dc start` was seen to return 0 without starting the service: compose ran
# the one-shot sim-ca again and printed nothing for the service itself. The
# cause is not known. So the helper reads whether the service's container
# was started at all after the kill (its start time moved), says so when it
# was not, starts it once more, and keeps the project's own container events
# from the kill on, for the next reading of the cause. A container that was
# started and then stopped is not retried: what became of it is the
# product's, judged by the service's health after this, as before.

RESTARTS=0
# started_at SERVICE: the start time Docker records for the service's
# container, empty when there is no container.
started_at() {
  local id
  id=$(dc ps -aq "$1") && [ -n "$id" ] && docker inspect -f '{{.State.StartedAt}}' "$id" 2>/dev/null
}
# restart_events SERVICE N SINCE: the project's container events from SINCE
# (the kill) to now, in OUT/restart-SERVICE-N.events.
restart_events() {
  local file="$OUT/restart-$1-$2.events"
  docker events --since "$3" --until "$(date -u +%s.%N)" \
    --filter "label=com.docker.compose.project=$DOCKER_PROJECT" --filter type=container \
    --format '{{.TimeNano}} {{.Action}} {{.Actor.Attributes.name}}' > "$file" 2>&1 ||
    echo "pilot-sim: the events for $file could not be read" >> "$file"
  echo "$file"
}
# restart SERVICE: killed outright, started again, and healthy.
restart() {
  local svc=$1 since before file
  RESTARTS=$((RESTARTS + 1))
  step "killing $svc and starting it again"
  before=$(started_at "$svc" || true)
  since=$(date -u +%s.%N)
  dc kill "$svc" >/dev/null
  dc start "$svc" >/dev/null
  if [ "$(started_at "$svc" || true)" = "$before" ]; then
    echo "pilot-sim: the start of $svc was skipped (compose returned 0 and did not start it); starting it again"
    dc start "$svc" >/dev/null
    file=$(restart_events "$svc" "$RESTARTS" "$since")
    [ "$(started_at "$svc" || true)" != "$before" ] ||
      fail "the start of $svc was skipped twice; compose did not start it (the project's events from the kill are in $file)"
    echo "pilot-sim: the second start of $svc started it; the project's events from the kill are in $file"
  fi
  waitfor "$svc healthy after its restart" 120 healthy "$svc"
}
