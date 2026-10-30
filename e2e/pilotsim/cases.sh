# shellcheck shell=bash
# The pilot cases P1 to P5 (docs/DESIGN-PILOT-SIMULATION.md, section 6),
# sourced by scripts/pilot-sim.sh after its two cycles: each case is one
# fault on a cycle of its own, checked by e2e/pilotsim/cases.py at the
# points it names, and the cycle's own checks at its end. P3 leaves both
# members held past the run, so it comes last.
#
# A part of a case that the board tracks as a product gap is checked
# expected-to-fail: xfail waits for what the case asks for, records that it
# did not happen with the gap's task in OUT/expected-to-fail.txt, and fails
# the run when it does happen, since then the gap is closed and the case is
# to be updated.

# The cases' stub scenario: sim-hands reviews READY, each review longer
# than three of the launcher's turn gaps (seq 676).
CASE_SCENARIO="$SCENARIO_REPOS,\"hands\":{\"review_seconds\":50}"
XFAIL=$OUT/expected-to-fail.txt
rm -f "$XFAIL"

# member_scenario ROLE JSON: the member's scenario (aicrew-simclient), replaced
# by a rename, and only while the member is held and its turn has ended.
member_scenario() {
  printf '%s\n' "$2" | dc exec -T -u root "sim-$1" sh -c 'cat > /sim/scenario/member.json.new &&
    chmod 644 /sim/scenario/member.json.new && mv /sim/scenario/member.json.new /sim/scenario/member.json'
}
# fault CASE CMD...: the case's fault, left out when the run skips it.
fault() {
  local c=$1
  shift
  if [ "$SKIP" = "$c" ]; then
    step "case $c: its fault is left out ($*)"
    return 0
  fi
  "$@"
}
# skipped CASE POINT: a run that left out the case's fault ends here,
# green: the case's absence showed at POINT.
skipped() {
  collect
  step "pilot-sim: green: without its fault, case $1 failed at $2, where its absence should show (reports in $OUT)"
  exit 0
}
# reach CASE POINT SECONDS WHAT CMD...: waitfor, except that a run without
# the case's fault expects WHAT not to happen.
reach() {
  local c=$1 p=$2 limit=$3 what=$4 i
  shift 4
  for ((i = 0; i < limit; i += 2)); do
    if "$@" >/dev/null 2>&1; then
      [ "$SKIP" != "$c" ] || fail "AICREW_SIM_SKIP_FAULT=$c: $what happened without the case's fault"
      return 0
    fi
    sleep 2
  done
  [ "$SKIP" != "$c" ] || skipped "$c" "$p"
  fail "$what did not happen within ${limit}s"
}
# check_case CASE POINT: the case's checks at POINT on its snapshot; a run
# without the case's fault expects them to fail.
check_case() {
  local code=0
  take "$2"
  python3 "$SIM/cases.py" check "$1" "$2" "$D" || code=$?
  if [ "$SKIP" = "$1" ]; then
    [ "$code" = 1 ] || fail "AICREW_SIM_SKIP_FAULT=$1: the case's checks at $2 passed without its fault (snapshot $D)"
    skipped "$1" "$2"
  fi
  [ "$code" = 0 ] || fail "case $1: the checks at $2 (snapshot $D)"
}
# xfail TASK WHAT SECONDS CMD...: the gap TASK tracks: WHAT is expected not
# to happen within SECONDS.
xfail() {
  local task=$1 what=$2 limit=$3 i
  shift 3
  for ((i = 0; i < limit; i += 2)); do
    if "$@" >/dev/null 2>&1; then
      fail "case $CYCLE: expected to fail ($task), but $what happened: the gap is closed, so update the case"
    fi
    sleep 2
  done
  printf '%s\t%s\texpected to fail, and failed: %s\n' "$CYCLE" "$task" "$what" >> "$XFAIL"
  step "case $CYCLE: expected to fail ($task), and failed: $what"
}

followup() { store_copy "$CYC/peek" && python3 "$SIM/cases.py" followup "$CYC/peek" "$TASK" "$COORD_ID"; }
has_op() { store_copy "$CYC/peek" && python3 "$SIM/cases.py" has_op "$CYC/peek" "$TASK" "$1"; }
pilot_reviewing() {
  forge GET "$PILOT/pulls/$PULL" |
    python3 -c 'import json,sys; sys.exit("hands-reviewing" not in [l["name"] for l in json.load(sys.stdin)["labels"]])'
}
coordinator_noted() { dc exec -T -u member sim-coordinator grep -qF "$1" /home/member/agent/docs/HANDOFF.md; }
# nudged_hold ROLE KIND: the operator nudges the member, and its hold in
# aicrewd is KIND ("none" for no hold) a few seconds later.
nudged_hold() {
  local id=$WORKER_ID
  [ "$1" = coordinator ] && id=$COORD_ID
  member nudge "$1" "pilot case $CYCLE" && sleep 6 && store_copy "$CYC/peek" &&
    python3 "$SIM/cases.py" hold "$CYC/peek" "$id" "$2"
}

# case_start CASE: the case's own directory, with no task yet.
case_start() {
  CYCLE=$1 CYC=$OUT/case-$1 TASK= PULL= MERGE= FIRST=
  rm -rf "$CYC"
  mkdir -p "$CYC"
  step "case $1"
}
# offered: the case's task filed and offered, the worker held at the offer.
offered() {
  hold worker "case $CYCLE: the offer"
  file_task
  waitfor "the coordinator's triage and offer of task $TASK" 300 at 'offered/* 0'
}
# case_done CASE: the coordinator's review, the operator's merge, the
# delivery record, and the cycle's checks at the step done.
case_done() {
  member resume worker
  member resume coordinator
  waitfor "the coordinator's review" 600 at 'running/accepted 1'
  PULL=$(python3 "$SIM/cycle.py" pull "$CYC/peek" "$TASK")
  hold coordinator "case $1: the operator's merge"
  merge
  waitfor "sim-ci's status on the merge commit of #$PULL" 120 pilot_ci_is "$MERGE" success
  member resume coordinator
  delivered
  snapshot done "$1"
}

# P1: the worker never acks; the coordinator's follow-up has no bound yet.
case_p1() {
  case_start P1
  scenario "{$CASE_SCENARIO}"
  hold worker "case P1: its scenario"
  fault P1 member_scenario worker '{"never_ack":true}'
  offered
  member resume worker
  waitfor "the worker's accept" 300 at 'running/* *'
  hold worker "case P1: the unacked offer"
  check_case P1 unacked
  xfail 01a18ce5-334e-7000-abc8-b07f40704f12 "the coordinator's follow-up on the worker's unacked offer within 120s" 120 followup
  member_scenario worker '{}'
  case_done P1
}

# P2: the worker pushes while hands-reviewing is on; the stale review does
# not count and the request is renewed for the new head.
case_p2() {
  case_start P2
  scenario "{$CASE_SCENARIO}"
  hold worker "case P2: its scenario"
  fault P2 member_scenario worker '{"push_during_review":true}'
  offered
  hold coordinator "case P2: the submit checks"
  member resume worker
  waitfor "the worker's pull request" 300 at 'running/* 1'
  PULL=$(python3 "$SIM/cycle.py" pull "$CYC/peek" "$TASK")
  # A worker waiting on its gates gets no turn until the review lands: the
  # operator's nudge gives it one while sim-hands reviews.
  waitfor "sim-hands taking #$PULL" 120 pilot_reviewing
  member nudge worker "case P2: a turn during the review"
  waitfor "the worker's submit" 600 at 'running/submitted 1'
  hold worker "case P2: the submit checks"
  PULL=$(python3 "$SIM/cycle.py" pull "$CYC/peek" "$TASK")
  check_case P2 submit
  member_scenario worker '{}'
  case_done P2
}

# P3: a daily-budget hold on the coordinator and a login hold (401) on the
# worker reach aicrewd, `aicrew status` and, for the worker's, the
# coordinator; the launcher backs off. A hold's end once its cause is
# cleared waits for the operator's re-check, and an unserved model (404)
# reports no hold: both are gaps the board tracks.
case_p3() {
  case_start P3
  hold coordinator "case P3: its scenario"
  fault P3 member_scenario coordinator '{"cost_usd":60}'
  member resume coordinator
  reach P3 budget 300 "the coordinator's daily-budget hold" nudged_hold coordinator daily_budget
  hold worker "case P3: its scenario"
  fault P3 member_scenario worker '{"result":"login"}'
  member resume worker
  reach P3 login 300 "the worker's login hold" nudged_hold worker needs_login
  check_case P3 held
  hold worker "case P3: the login is back"
  member_scenario worker '{}'
  member resume worker
  xfail 01a18ce5-335f-7000-9cd6-f522f638f66a "the worker's login hold ends once the login is back, within 120s" 120 \
    nudged_hold worker none
  # A member's backoff leaves it one held outcome per run, and the login
  # hold took the worker's: the model variant ({"result":"model"}) is not
  # run here.
  printf '%s\t%s\t%s\n' P3 01a176ba-db7d-7000-ae0c-9b465d5b859b \
    'expected to fail, not run: an unserved model (404) reports no hold; reproducer: the member scenario {"result":"model"}' \
    >> "$XFAIL"
}

# P4: the worker stalls; the launcher's stall rule blocks the attempt, the
# coordinator sees the block and answers, and the worker resumes.
case_p4() {
  case_start P4
  scenario "{$CASE_SCENARIO}"
  hold worker "case P4: its scenario"
  fault P4 member_scenario worker '{"stall":true}'
  offered
  hold coordinator "case P4: the block's checks"
  member resume worker
  reach P4 blocked 300 "the launcher's block of the stalled attempt" at 'running/blocked *'
  hold worker "case P4: the block's checks"
  member_scenario worker '{}'
  check_case P4 blocked
  member resume coordinator
  waitfor "the coordinator's answer" 300 has_op answer
  waitfor "sim-coordinator's turn to end" 120 idle coordinator
  check_case P4 answered
  case_done P4
}

# P5: the CI fails on the first head and the rework's second head carries
# the gates; then the merge commit's CI stays pending, and verify-delivery's
# pending answer is never recorded as delivered.
case_p5() {
  case_start P5
  scenario "{$CASE_SCENARIO}"
  fault P5 scenario "{$CASE_SCENARIO,\"ci\":{\"state\":\"failure\"}}"
  offered
  hold coordinator "case P5: the submit checks"
  member resume worker
  waitfor "the worker's pull request" 300 at 'running/* 1'
  hold worker "case P5: the first head's CI"
  PULL=$(python3 "$SIM/cycle.py" pull "$CYC/peek" "$TASK")
  FIRST=$(pilot_field head.sha)
  reach P5 first-head 120 "sim-ci's failure on the first head" pilot_ci_is "$FIRST" failure
  scenario "{$CASE_SCENARIO}"
  member resume worker
  waitfor "the worker's submit at a second head" 600 at 'running/submitted 1'
  hold worker "case P5: the submit checks"
  check_case P5 submit
  member resume worker
  member resume coordinator
  waitfor "the coordinator's review" 300 at 'running/accepted 1'
  hold coordinator "case P5: the operator's merge"
  scenario "{$SCENARIO_REPOS,\"ci\":{\"state\":\"pending\"}}"
  merge
  waitfor "sim-ci's pending status on the merge commit of #$PULL" 120 pilot_ci_is "$MERGE" pending
  member resume coordinator
  waitfor "the coordinator's verify-delivery of the pending merge" 300 \
    coordinator_noted "verify-delivery: pending for https://sim-gitea:3000/operator/pilot/pulls/$PULL"
  hold coordinator "case P5: the pending delivery's checks"
  check_case P5 pending
  # The merge commit's CI completes. The pull watch reads only the head's
  # checks, so nothing wakes the coordinator for it.
  scenario "{$SCENARIO_REPOS}"
  waitfor "sim-ci's success on the merge commit of #$PULL" 120 pilot_ci_is "$MERGE" success
  member resume coordinator
  xfail 01a18ce5-336d-7000-9bef-5146088e9805 \
    "the coordinator's delivery once the merge commit's CI succeeds, with no wake from the operator, within 120s" 120 \
    at 'closed/finalized *'
  member nudge coordinator "case P5: the merge commit's CI completed"
  delivered
  snapshot done P5
}

for c in P2 P5 P4 P1 P3; do
  [ -z "$SKIP" ] || [ "$SKIP" = "$c" ] || continue
  "case_${c,,}"
done
[ -z "$SKIP" ] || fail "AICREW_SIM_SKIP_FAULT=$SKIP: the case passed without its fault"
scenario "{$SCENARIO_REPOS}"

collect
step "pilot-sim: green: both members checked, listed and serving; the stubs' fixtures confirmed, refused and released; two cycles from a filed task to DONE with the delivery record; the pilot cases P1 to P5, expected-to-fail parts in $XFAIL (reports in $OUT)"
