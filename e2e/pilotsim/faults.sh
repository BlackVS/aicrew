# shellcheck shell=bash
# The fault cases F1, F2, F3, F6 and F7 (docs/DESIGN-PILOT-SIMULATION.md,
# section 6.1; docs/E2E-PILOT-SIM.md, "The fault cases"), sourced by
# cases.sh, whose helpers they use. They adapt the b4 harness's cases
# (e2e/realaimem) to the compose stack: a fault is a rule armed on one of
# the fault proxies (sim-px-aimem, sim-px-crew, sim-px-peer) or a container
# killed and started, and each case's part is a skip-the-fault case of its
# own, AICREW_SIM_SKIP_FAULT=PART.

# arm PROXY CASE ACTION METHOD PATH [FLAG...]: arms a rule on sim-px-PROXY;
# RULE is its id, or empty when the run leaves CASE's fault out.
RULE= RULE_PX=
arm() {
  local p=$1 c=$2
  shift 2
  RULE= RULE_PX=$p
  if [ "$SKIP" = "$c" ]; then
    step "case $c: its fault is left out (sim-px-$p: $*)"
    return 0
  fi
  RULE=$(px "$p" arm -case "$c" -action "$1" -method "$2" -path "$3" "${@:4}") || fail "case $c: arming sim-px-$p"
}
disarm() {
  [ -z "$RULE" ] || px "$RULE_PX" disarm "$RULE" >/dev/null || fail "disarming rule $RULE on sim-px-$RULE_PX"
  RULE=
}
fired() { [ -n "$RULE" ] && px "$RULE_PX" fired "$RULE" >/dev/null; }
# px_logged PROXY CASE OUTCOME: sim-px-PROXY logged a request of CASE with OUTCOME.
px_logged() { px "$1" log | python3 "$SIM/cases.py" logged "$2" "$3"; }
reservation_path() { printf '^/v1/projects/[^/]+/tasks/[^/]+/reservation/%s$' "$1"; }

# fact KEY VALUE: one fact of the case for its checks (CYC/facts.json; JSON
# values are kept as JSON).
fact() {
  python3 - "$CYC/facts.json" "$1" "$2" <<'PY'
import json, os, sys
path, key, value = sys.argv[1:]
facts = json.load(open(path)) if os.path.exists(path) else {}
try:
    facts[key] = json.loads(value)
except ValueError:
    facts[key] = value
json.dump(facts, open(path, "w"), indent=1)
PY
}
started() { docker inspect -f '{{.State.StartedAt}}' "$(dc ps -q "$1")"; }
healthy() { [ "$(docker inspect -f '{{.State.Health.Status}}' "$(dc ps -q "$1")")" = healthy ]; }
# restart SERVICE: killed outright, started again, and healthy.
restart() {
  step "killing $1 and starting it again"
  dc kill "$1" >/dev/null
  dc start "$1" >/dev/null
  waitfor "$1 healthy after its restart" 120 healthy "$1"
}
hold_status() {
  hub aimem reservation recover status --task "$TASK" --hub https://sim-aimem:8440 \
    --admin-token-file /home/aimem/admin.token --hub-ca-file /sim/tls/ca.pem
}

# ---- F1: lost aimem requests and replies ------------------------------------------

lost() { store_copy "$CYC/peek" && python3 "$SIM/cases.py" lost "$CYC/peek" "$TASK" "$1"; }
# lost_effect OP: what a lost OP request leaves; an undecided update is the
# one the worker recorded with its request key.
lost_effect() {
  lost "$1" || return 1
  [ "$1" != update ] || noted worker "is pending, undecided: request key"
}
# lose_request CASE ROLE OP SECONDS WHAT: sim-px-aimem drops OP's request
# before the hub sees it; the member is paused as soon as it does, so it does
# not begin the step again under the rule, and aicrewd settles the step not
# committed.
lose_request() {
  local c=$1 role=$2 op=$3 limit=$4 what=$5
  arm aimem "$c" drop_request POST "$(reservation_path "$op")" -persist
  member resume "$role"
  if [ -n "$RULE" ]; then
    waitfor "$what reaching sim-px-aimem" "$limit" fired
    member pause "$role" "case $c: the lost $op"
  fi
  reach "$c" request 180 "the lost $op settled not committed" lost_effect "$op"
  waitfor "sim-$role's turn to end" 120 idle "$role"
  disarm
  check_case "$c" request
}
# lose_reply CASE ROLE OP SECONDS WHAT CMD...: sim-px-aimem drops OP's reply
# after the hub committed it; the nudged member begins the step again, and
# its step driver reconciles the lost reply by aimem's receipt. CMD is the
# state the step leads to.
lose_reply() {
  local c=$1 role=$2 op=$3 limit=$4 what=$5
  shift 5
  arm aimem "$c" drop_reply POST "$(reservation_path "$op")" -persist
  member resume "$role"
  member nudge "$role" "case $c: the $op again"
  waitfor "$what" "$limit" "$@"
  hold "$role" "case $c: the lost $op reply's checks"
  disarm
  check_case "$c" reply
}

case_f1() {
  case_start F1
  scenario "{$CASE_SCENARIO}"
  hold worker "case F1: the offer"
  hold coordinator "case F1: the lost claim"
  file_task
  lose_request F1-claim-request coordinator claim 300 "the coordinator's claim"
  lose_reply F1-claim-reply coordinator claim 300 "the coordinator's offer again" at 'offered/* 0'
  lose_request F1-transfer-request worker transfer 300 "the worker's accept"
  lose_reply F1-transfer-reply worker transfer 300 "the worker's accept again" at 'running/* *'
  # The worker opens its pull request and waits for its gates first.
  lose_request F1-update-request worker update 900 "the worker's submit"
  lose_reply F1-update-reply worker update 300 "the worker's superseding submit" at 'running/submitted 1'
  PULL=$(python3 "$SIM/cycle.py" pull "$CYC/peek" "$TASK")
  member resume worker
  member resume coordinator
  waitfor "the coordinator's review" 300 at 'running/accepted 1'
  hold coordinator "case F1: the operator's merge"
  merge
  waitfor "sim-ci's status on the merge commit of #$PULL" 120 pilot_ci_is "$MERGE" success
  lose_request F1-finalize-request coordinator finalize 300 "the coordinator's finalize"
  arm aimem F1-finalize-reply drop_reply POST "$(reservation_path finalize)" -persist
  member resume coordinator
  member nudge coordinator "case F1-finalize-reply: the finalize again"
  delivered
  disarm
  check_case F1-finalize-reply reply
  snapshot done F1
}

# ---- F2: a lost aicrewd reply ------------------------------------------------------

# F2-begin: sim-px-crew drops the reply to the offer's begin; the step fails
# for the coordinator, whose `step recover` replays the begin under its key
# and reaches the one attempt the lost reply began.
case_f2() {
  case_start F2
  scenario "{$CASE_SCENARIO}"
  hold worker "case F2: the offer"
  hold coordinator "case F2: the lost begin"
  file_task
  arm crew F2-begin drop_reply POST '^/v1/crew/attempts$'
  member resume coordinator
  waitfor "the coordinator's offer" 300 at 'offered/* 0'
  hold coordinator "case F2: the begin's checks"
  disarm
  check_case F2-begin begin
  case_done F2
}

# ---- F3: restarts --------------------------------------------------------------------

# F3-aicrewd: the hub committed the claim, and its answer is held at
# sim-px-aimem while aicrewd is killed and started; the step settles
# against the restarted aicrewd. F3-hub: the hub is killed and started
# between steps; its hold is intact, and the cycle's next steps work.
case_f3() {
  case_start F3
  scenario "{$CASE_SCENARIO}"
  hold worker "case F3: the offer"
  hold coordinator "case F3: aicrewd's restart"
  file_task
  fact aicrewd_before "$(started sim-aicrewd)"
  arm aimem F3-aicrewd delay_reply POST "$(reservation_path claim)" -delay 20s
  member resume coordinator
  if [ -n "$RULE" ]; then
    waitfor "the hub's answer to the claim, held at sim-px-aimem" 300 px_logged aimem F3-aicrewd delayed_reply
    restart sim-aicrewd
  fi
  fact aicrewd_after "$(started sim-aicrewd)"
  waitfor "the coordinator's offer" 300 at 'offered/* 0'
  hold coordinator "case F3: the offer's checks"
  disarm
  check_case F3-aicrewd restart

  fact hold_before "$(hold_status)"
  fact hub_before "$(started sim-aimem)"
  fault F3-hub restart sim-aimem
  fact hub_after "$(started sim-aimem)"
  check_case F3-hub restart
  case_done F3
}

# ---- F6: recovery by the hub's admin ------------------------------------------------

# recover_hold VERB: the hub's admin recovers the task's hold (release to
# READY, cancel to CANCELLED) with an attestation, as aimem's runbook has it.
recover_hold() {
  hold_status > "$CYC/hold-$1.json"
  hubapi GET "/v1/tasks/$TASK" > "$CYC/task-$1.json"
  python3 "$SIM/cases.py" recover_body "$1" "$CYC/hold-$1.json" "$CYC/task-$1.json" |
    hub aimem reservation recover "$1" --task "$TASK" --key "sim-f6-$1-$TASK" --hub https://sim-aimem:8440 \
      --admin-token-file /home/aimem/admin.token --hub-ca-file /sim/tls/ca.pem >/dev/null
}
# cancel_task: the operator cancels the released task on the board, so that
# no later turn offers it again.
cancel_task() {
  hubapi GET "/v1/tasks/$TASK" > "$CYC/task-cancel.json"
  hubapi PUT "/v1/tasks/$TASK" "$(python3 "$SIM/cases.py" cancel_body "$CYC/task-cancel.json")" >/dev/null
}

case_f6() {
  case_start F6
  scenario "{$CASE_SCENARIO}"
  local verb
  for verb in release cancel; do
    hold worker "case F6-$verb: the offer"
    file_task
    waitfor "the coordinator's triage and offer of task $TASK" 300 at 'offered/* 0'
    hold coordinator "case F6-$verb: the recovery"
    member resume worker
    waitfor "the worker's accept" 300 at 'running/* *'
    hold worker "case F6-$verb: the recovery"
    fault "F6-$verb" recover_hold "$verb"
    reach "F6-$verb" recovered 120 "aicrewd closing the attempt as recovered" at 'closed/* *'
    check_case "F6-$verb" recovered
    [ "$verb" = cancel ] || cancel_task
    member resume coordinator
  done
  member resume worker
}

# ---- F7: secrets ------------------------------------------------------------------------

# F7-leak: none of the secrets the run made appears in the containers' logs,
# the run's reports or aicrewd's audit. The run knows the secrets the
# runner moved (into), the hub's admin token, the coordination proofs
# sim-px-crew saw and the members' aimem session handles. Without its fault
# the case plants one in a scanned log, and the scan must find it.
case_f7() {
  case_start F7
  local known=$KNOWN_DIR/secrets code=0
  { hub cat /home/aimem/admin.token; echo; } >> "$known"
  dc exec -T sim-px-crew sh -c 'cat /var/lib/simproxy/proofs 2>/dev/null || true' >> "$known"
  for role in coordinator worker; do
    dc exec -T -u member "sim-$role" sh -c 'cat /home/member/agent/aimem/aicrew-sessions/*.json 2>/dev/null || true' |
      python3 "$SIM/cases.py" handles >> "$known"
  done
  collect
  store_copy "$CYC/store" || fail "no stable copy of aicrewd's store for F7"
  if [ "$SKIP" = F7-leak ]; then
    step "case F7-leak: a secret is planted in a scanned log"
    { printf 'planted '; sed -n 1p "$known"; } > "$LOGS/planted.log"
  fi
  python3 "$SIM/cases.py" scan "$known" "$OUT" "$CYC/store" "$STAGE" "$KNOWN_DIR" || code=$?
  if [ "$SKIP" = F7-leak ]; then
    [ "$code" = 1 ] || fail "AICREW_SIM_SKIP_FAULT=F7-leak: the scan did not find the planted secret"
    skipped F7-leak scan
  fi
  [ "$code" = 0 ] || fail "case F7-leak: the secret scan"
}
