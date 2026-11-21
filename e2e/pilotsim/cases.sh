# shellcheck shell=bash
# The pilot cases P1 to P5, the escalation case E1 and the fault cases F1 to
# F7 (faults.sh) (docs/DESIGN-PILOT-SIMULATION.md, section 6;
# docs/E2E-PILOT-SIM.md for E1), sourced by
# scripts/pilot-sim.sh after its two cycles: each case is one fault, or a
# sequence of them, on a cycle of its own, checked by e2e/pilotsim/cases.py
# at the points it names, and the cycle's own checks at its end. P3 leaves
# both members held past the run, so it comes last.
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
    ev fault case="$c" fault="$*" state=left_out
    return 0
  fi
  ev fault case="$c" fault="$*" state=armed
  "$@"
}
# skipped CASE POINT: a run that left out the case's fault ends here,
# green: the case's absence showed at POINT.
skipped() {
  ev skip_verdict case="$1" verdict=failed_as_expected point="$2"
  ev scenario_end name="$SCENARIO" result=FAIL reason="without its fault, case $1 failed at $2, as expected"
  SCENARIO=
  collect
  step "pilot-sim: green: without its fault, case $1 failed at $2, where its absence should show (reports in $OUT)"
  exit 0
}
# reach CASE POINT SECONDS WHAT CMD...: waitfor, except that a run without
# the case's fault expects WHAT not to happen. SECONDS is elapsed time, the
# predicate's own runs included.
reach() {
  local c=$1 p=$2 limit=$3 what=$4 end=$((SECONDS + $3))
  shift 4
  while :; do
    if "$@" >/dev/null 2>&1; then
      [ "$SKIP" != "$c" ] || fail "AICREW_SIM_SKIP_FAULT=$c: $what happened without the case's fault"
      return 0
    fi
    [ "$SECONDS" -lt "$end" ] || break
    sleep 2
  done
  [ "$SKIP" != "$c" ] || skipped "$c" "$p"
  fail "$what did not happen within ${limit}s"
}
# check_case CASE POINT: the case's checks at POINT on its snapshot, named
# by the subcase where the case has them (F1-claim-request), else by the
# point; a run without the case's fault expects them to fail.
check_case() {
  local code=0
  case $1 in
  *-*) take "$1" ;;
  *) take "$2" ;;
  esac
  python3 "$SIM/cases.py" check "$1" "$2" "$D" || code=$?
  if [ "$code" = 0 ]; then
    ev check snapshot="${D#"$OUT"/}" result=passed
  else
    ev check snapshot="${D#"$OUT"/}" result=failed
  fi
  if [ "$SKIP" = "$1" ]; then
    [ "$code" = 1 ] || fail "AICREW_SIM_SKIP_FAULT=$1: the case's checks at $2 passed without its fault (snapshot $D)"
    skipped "$1" "$2"
  fi
  [ "$code" = 0 ] || fail "case $1: the checks at $2 (snapshot $D)"
}
# xfail TASK WHAT SECONDS CMD...: the gap TASK tracks: WHAT is expected not
# to happen within SECONDS, elapsed time as reach counts it.
xfail() {
  local task=$1 what=$2 end=$((SECONDS + $3))
  shift 3
  while :; do
    if "$@" >/dev/null 2>&1; then
      fail "case $CYCLE: expected to fail ($task), but $what happened: the gap is closed, so update the case"
    fi
    [ "$SECONDS" -lt "$end" ] || break
    sleep 2
  done
  printf '%s\t%s\texpected to fail, and failed: %s\n' "$CYCLE" "$task" "$what" >> "$XFAIL"
  ev observation task="$task" what="expected to fail, and failed: $what"
  step "case $CYCLE: expected to fail ($task), and failed: $what"
}

followup() { store_copy "$CYC/peek" && python3 "$SIM/cases.py" followup "$CYC/peek" "$TASK" "$COORD_ID"; }
has_op() { store_copy "$CYC/peek" && python3 "$SIM/cases.py" has_op "$CYC/peek" "$TASK" "$1"; }
blocked_report() { store_copy "$CYC/peek" && python3 "$SIM/cases.py" blocked_report "$CYC/peek" "$TASK" "$COORD_ID"; }
pilot_reviewing() {
  forge GET "$PILOT/pulls/$PULL" |
    python3 -c 'import json,sys; sys.exit("hands-reviewing" not in [l["name"] for l in json.load(sys.stdin)["labels"]])'
}
# noted ROLE TEXT: the member's handoff has TEXT.
noted() { dc exec -T -u member "sim-$1" grep -qF "$2" "$(mhome "$1")/docs/HANDOFF.md"; }
# checked ROLE VERDICT PULL: the member's launcher logged a delivery check of
# PULL with VERDICT.
checked() {
  dc logs --no-color "sim-$1" 2>&1 | grep -F 'msg="delivery check"' | grep -F "pull=$3 " | grep -qF "verdict=$2"
}
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
  begin "case-$1" 1200
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
  # The blocked-wake subscription delivers the report on a later tick.
  waitfor "the coordinator's blocked report" 120 blocked_report
  check_case P4 blocked
  member resume coordinator
  waitfor "the coordinator's answer" 300 has_op attempt.answer
  waitfor "sim-coordinator's turn to end" 120 idle coordinator
  check_case P4 answered
  case_done P4
}

# E1: the escalation case (docs/E2E-PILOT-SIM.md, "The escalation case").
# The worker stalls and the launcher blocks the attempt, as in P4; the
# coordinator raises an escalation on it instead of answering. The runner is
# the architect session: in sim-architect, the directory `aicrew architect
# init` wrote holds the architect credential the operator issued in
# sim-aicrewd, and `aicrew escalations wait` there tells it of the raise. It
# answers under that credential and mirrors the answer on the task, and the
# coordinator and the worker act on the answer's message.
#
# Between the raise and the answer the runner makes no operator call: it
# neither pauses, nudges nor snapshots, and reads only the wait's output.
# aicrewd's audit records commands, not reads, so that no list or show ran
# between the raise and the wait's return rests on the runner's own log of
# the architect's commands (CYC/architect.log), not on a record of aicrewd.
# The architect's machine is sim-architect, its user sessiond (the image's
# user; no aicrewd runs there), and its directory the document's
# ~/aicrew/architect. The operator's commands run in sim-aicrewd, as the
# service user (op) or as the operator's SSH login `ops` (the
# image's Debian already has a group named operator).
ARCH_DIR=/home/sessiond/aicrew/architect
ARCH_ENV=()
# arch CMD...: the architect session's command, in its directory, with the
# environment the directory's .claude/settings.json gives the session; each
# is logged with its start time.
arch() {
  printf '%s %s\n' "$(date -u +%Y-%m-%dT%H:%M:%S.%NZ)" "$*" >> "$CYC/architect.log"
  dc exec -T -u sessiond -w "$ARCH_DIR" -e PATH=/opt/aicrew-prebuilt:/usr/bin:/bin "${ARCH_ENV[@]}" sim-architect "$@"
}
# On the architect's machine, as its user, in its home, with aicrew on PATH
# as the one-liner leaves it; and as the operator's login on aicrewd's host.
archhost() {
  dc exec -T -u sessiond -w /home/sessiond -e HOME=/home/sessiond -e PATH=/opt/aicrew-prebuilt:/usr/bin:/bin sim-architect "$@"
}
oplogin() { dc exec -T -u ops -w /home/ops -e HOME=/home/ops -e USER=ops sim-aicrewd "$@"; }
# archdoc RUNNER ID [docstep options]: step ID's line run by RUNNER in its
# home, its output on stdout; the step is recorded as run.
archdoc() {
  local runner=$1 id=$2 line
  shift 2
  line=$(docline "$id" "$@") || fail "docs/BRING-UP.md: no line for step $id"
  "$runner" sh -c "$line" || return
  echo "$id" >> "$OUT/docsteps.ran"
}
# expect_out WHAT WANT GOT: a step's printed line is the document's.
expect_out() { [ "$3" = "$2" ] || fail "case E1: $1 printed '$3', not '$2'"; }
# known_secret RUNNER FILE: a credential carried without the runner is
# added to the run's known secrets, for the redaction check, by a pipe.
known_secret() { { "$1" cat "$2"; echo; } >> "$KNOWN_DIR/secrets"; }
# architect_ssh: the architect's machine logs in to aicrewd's host as
# `ops` with a key made for this run in sim-architect, which trusts
# sim-aicrewd's own host key, read from it rather than scanned. Both stay in
# the containers' filesystems, which the run's `down -v` removes.
architect_ssh() {
  archhost sh -c 'install -d -m 700 ~/.ssh && rm -f ~/.ssh/id_ed25519* && ssh-keygen -q -t ed25519 -N "" -f ~/.ssh/id_ed25519' ||
    fail "case E1: the architect machine's SSH key"
  archhost cat .ssh/id_ed25519.pub |
    oplogin sh -c 'install -d -m 700 ~/.ssh && cat > ~/.ssh/authorized_keys && chmod 600 ~/.ssh/authorized_keys' ||
    fail "case E1: the operator login's authorized key"
  dc exec -T sim-aicrewd cat /etc/ssh/ssh_host_ed25519_key.pub | awk '{print "sim-aicrewd", $1, $2}' |
    archhost sh -c 'cat > ~/.ssh/known_hosts' || fail "case E1: aicrewd host's key on the architect machine"
}
# architect_init: docs/BRING-UP.md step 8, both arrangements, by the
# document's lines: the directory on the architect's machine; on aicrewd's
# host the one-machine line into a directory there (the throwaway
# credential that the revoke then ends), and the two-machine carrying:
# issued to a file as the service user, handed to the login user, carried
# by scp, and gone from the host. The seat then answers with the carried
# credential.
architect_init() {
  local conn=(-set PROJECT="$PROJECT" -set AICREWD=sim-aicrewd -set PORT=9443 -alt -set "PIN=${PIN_SIM_AICREWD#sha256-}")
  archdoc archhost architect-init "${conn[@]}" > /dev/null || fail "case E1: aicrew architect init on the architect's machine"
  # One machine: the directory and its credential where the operator's are.
  archdoc op architect-init "${conn[@]}" > /dev/null || fail "case E1: aicrew architect init on aicrewd's host"
  archdoc op architect-credential-here > "$CYC/credential-here.json" || fail "case E1: the one-machine credential"
  known_secret op /home/sessiond/aicrew/architect/creds/aicrew.architect
  # Two machines, in the document's order.
  archdoc op architect-credential-file > "$CYC/credential-file.json" || fail "case E1: the credential issued to a file"
  known_secret op /home/sessiond/aicrew/etc/architect-planning.token
  expect_out "the issued file's rights" "600 sessiond" "$(archdoc op architect-file-rights)"
  archdoc op architect-credential-list > "$CYC/credential-list.json" || fail "case E1: aicrew architect credential list"
  grep -q '"planning"' "$CYC/credential-list.json" || fail "case E1: the credential list has no label planning ($CYC/credential-list.json)"
  archdoc oplogin architect-handover || fail "case E1: the handover to the operator's login"
  architect_ssh
  archdoc archhost architect-carry -set AICREWD_HOST=ops@sim-aicrewd || fail "case E1: the carrying by scp"
  expect_out "the carried file's rights" 600 "$(archdoc archhost architect-carried-rights)"
  expect_out "the host's copies" gone "$(archdoc oplogin architect-carry-gone)"
  oplogin sh -c 'test ! -e ~/architect-planning.token && sudo test ! -e ~sessiond/aicrew/etc/architect-planning.token' ||
    fail "case E1: a copy of the carried credential is left on aicrewd's host"
  expect_out "the operator credential's path" absent "$(archdoc archhost architect-no-operator-token)"
  # The whole filesystem, but the kernel's pseudo-filesystems: the search
  # must complete, and then find nothing.
  local found
  found=$(dc exec -T sim-architect find / \( -path /proc -o -path /sys -o -path /dev \) -prune -o \
    -name operator.token -print -quit) || fail "case E1: the search for an operator.token on the architect's machine"
  [ -z "$found" ] || fail "case E1: an operator.token is on the architect's machine: $found"
  # The throwaway one-machine credential is withdrawn; the carried one stays.
  ARCH_THROWAWAY=$(json_field id < "$CYC/credential-here.json") ||
    fail "case E1: the one-machine credential's id ($CYC/credential-here.json)"
  ARCH_CRED_ID=$(json_field id < "$CYC/credential-file.json") || fail "case E1: the carried credential's id ($CYC/credential-file.json)"
  archdoc op architect-credential-revoke -set ID="$ARCH_THROWAWAY" > /dev/null || fail "case E1: aicrew architect credential revoke"
  archdoc archhost architect-status "${conn[@]}" > "$CYC/architect-status.json" || fail "case E1: aicrew status from the seat"
  python3 "$SIM/smoke.py" status "$CYC/architect-status.json" coordinator worker independent ||
    fail "case E1: the seat's aicrew status ($CYC/architect-status.json)"
  mapfile -t ARCH_ENV < <(dc exec -T -u sessiond sim-architect cat "$ARCH_DIR/.claude/settings.json" |
    python3 -c 'import json,sys
for k, v in json.load(sys.stdin)["env"].items():
    print("-e"); print("%s=%s" % (k, v))')
  [ "${#ARCH_ENV[@]}" -gt 0 ] || fail "case E1: the architect directory's settings name no environment"
  # The seat's merges list, as /arch-merges reads it.
  local merges
  merges=$(docline architect-merges) || fail "docs/BRING-UP.md: no line for step architect-merges"
  arch sh -c "$merges" > "$CYC/architect-merges.out" || fail "case E1: aicrew merges from the seat"
  echo architect-merges >> "$OUT/docsteps.ran"
}
# architect_wait: the session's background wait. Its output, exit code and
# the time it returned go to CYC/wait.*.
architect_wait() {
  rm -f "$CYC"/wait.*
  (
    code=0
    arch aicrew escalations wait --interval 15 > "$CYC/wait.out" 2> "$CYC/wait.err" || code=$?
    date -u +%Y-%m-%dT%H:%M:%S.%NZ > "$CYC/wait.returned.new"
    echo "$code" > "$CYC/wait.code"
    mv "$CYC/wait.returned.new" "$CYC/wait.returned"
  ) &
}
claimed() { dc exec -T -u sessiond sim-architect test -s "$ARCH_DIR/state/escalations-seen.json"; }
wait_returned() { [ -s "$CYC/wait.returned" ]; }
answer_acked() { store_copy "$CYC/peek" && python3 "$SIM/cases.py" answer_acked "$CYC/peek" "$TASK" "$COORD_ID" "$WORKER_ID"; }
not_blocked() { ! at 'running/blocked *'; }

case_e1() {
  case_start E1
  scenario "{$CASE_SCENARIO}"
  architect_init
  hold coordinator "case E1: its scenario"
  member_scenario coordinator '{"escalate_blocked":true}'
  member resume coordinator
  hold worker "case E1: its scenario"
  fault E1 member_scenario worker '{"stall":true}'
  offered
  hold coordinator "case E1: the block's checks"
  member resume worker
  reach E1 blocked 300 "the launcher's block of the stalled attempt" at 'running/blocked *'
  hold worker "case E1: the block's checks"
  member_scenario worker '{}'
  waitfor "the coordinator's blocked report" 120 blocked_report
  check_case E1 blocked
  # The architect session's wait runs before the raise; the members are
  # resumed before it too, so no operator call falls between the raise
  # and the answer.
  architect_wait
  waitfor "the architect's wait to claim its seen set" 60 claimed
  member resume worker
  member resume coordinator
  waitfor "the architect's wait to return" 120 wait_returned
  local esc
  esc=$(sed -n 's/^escalation \([0-9a-f-]*\) .*/\1/p' "$CYC/wait.out" | head -n 1)
  [ -n "$esc" ] || fail "case E1: the wait returned no escalation (exit $(cat "$CYC/wait.code"); $CYC/wait.out, $CYC/wait.err)"
  arch aicrew escalations show --id "$esc" > "$CYC/show.out" || fail "case E1: aicrew escalations show"
  arch aicrew escalations answer --id "$esc" --decision "go on" \
    --rationale "The stall was the case's fault; nothing else blocks the task." > "$CYC/answer.out" ||
    fail "case E1: aicrew escalations answer"
  # The mirror of the answer on the task, as the architect session posts it
  # with the operator's aimem user (here the hub's admin, who files the task).
  store_copy "$CYC/peek"
  local full
  full=$(python3 "$SIM/cases.py" escalation_of "$CYC/peek" "$TASK") || fail "case E1: no escalation on task $TASK"
  hubapi POST "/v1/tasks/$TASK/comments" "{\"body\":\"[escalation.answer $full]\\n\\ngo on: the stall was the case's fault; nothing else blocks the task.\"}" > /dev/null
  waitfor "the members' acks of the answer" 300 answer_acked
  waitfor "the coordinator's answer to the worker" 300 has_op attempt.answer
  waitfor "the attempt to leave blocked" 300 not_blocked
  waitfor "sim-coordinator's turn to end" 120 idle coordinator
  fact wait "$(python3 "$SIM/cases.py" wait_fact "$CYC")"
  fact architect_credential "$ARCH_CRED_ID"
  fact architect_throwaway "$ARCH_THROWAWAY"
  fact architect_creds "$(dc exec -T -u sessiond sim-architect ls -A "$ARCH_DIR/creds" |
    python3 -c 'import json,sys; print(json.dumps(sys.stdin.read().split()))')"
  check_case E1 answered
  case_done E1
  # The cases after E1 have the plain coordinator.
  hold coordinator "case E1: its scenario's end"
  member_scenario coordinator '{}'
  member resume coordinator
}

# O1: the operator during a cycle (docs/BRING-UP.md, "The operator during a
# cycle"), by the document's own lines in sim-aicrewd, where the operator's
# commands run (arrangement 1). The worker stalls and the coordinator raises
# an escalation on the blocked attempt, as in E1; the operator reads the
# team's state, finds the escalation, reads and answers it, and pauses and
# resumes the team; the cycle then ends with its pull request merged and
# delivered. No mirror is posted on the task: the document asks none of the
# operator.
# opjson ID FILE [docstep options]: step ID by op, its JSON into CYC/FILE.
opjson() {
  local id=$1 file=$2
  shift 2
  doc op /home/sessiond "$id" "$@" > "$CYC/$file" || fail "case O1: docs/BRING-UP.md's step $id"
}
# open_escalation: aicrew escalations list --open lists one for the task;
# its id goes to CYC/escalation.
open_escalation() {
  opjson escalations-list list.json
  python3 -c 'import json,sys
ids = [e["id"] for e in json.load(open(sys.argv[1])) or [] if e["task"]["task_id"] == sys.argv[2]]
assert len(ids) == 1
print(ids[0])' "$CYC/list.json" "$TASK" > "$CYC/escalation" 2>/dev/null
}
# team_state WHAT PY: aicrew team state; PY, a Python expression over the
# state `s` and the members by label `m`, holds.
team_state() {
  opjson team-state "state-$1.json"
  python3 -c 'import json,sys
s = json.load(open(sys.argv[1]))
m = {x["label"]: x for x in s["members"]}
sys.exit(0 if eval(sys.argv[2]) else 1)' "$CYC/state-$1.json" "$2"
}
attempt_unblocked() {
  team_state unblocked '(m["worker"].get("open_attempt") or {}).get("phase") not in (None, "blocked")'
}
case_o1() {
  case_start O1
  scenario "{$CASE_SCENARIO}"
  hold coordinator "case O1: its scenario"
  member_scenario coordinator '{"escalate_blocked":true}'
  member resume coordinator
  hold worker "case O1: its scenario"
  member_scenario worker '{"stall":true}'
  offered
  member resume worker
  waitfor "the launcher's block of the stalled attempt" 300 at 'running/blocked *'
  hold worker "case O1: the block"
  member_scenario worker '{}'
  member resume worker
  # The team's state shows the blocked attempt with its reason.
  team_state blocked 'm["worker"]["open_attempt"]["phase"] == "blocked" and m["worker"]["open_attempt"].get("block") and "coordinator" in m' ||
    fail "case O1: aicrew team state does not show the worker's attempt blocked ($CYC/state-blocked.json)"
  waitfor "the coordinator's escalation in aicrew escalations list --open" 300 open_escalation
  local esc
  esc=$(cat "$CYC/escalation")
  opjson escalations-show show.json -set "ESCALATION=$esc"
  python3 -c 'import json,sys; e = json.load(open(sys.argv[1])); assert e["id"] == sys.argv[2] and e["question"] and not e.get("answer")' \
    "$CYC/show.json" "$esc" || fail "case O1: aicrew escalations show ($CYC/show.json)"
  opjson escalations-answer answer.json -set "ESCALATION=$esc" -set "DECISION=go on" \
    -set "RATIONALE=The stall was the case's fault; nothing else blocks the task."
  python3 -c 'import json,sys; e = json.load(open(sys.argv[1])); assert e["id"] == sys.argv[2] and e["answer"]' \
    "$CYC/answer.json" "$esc" || fail "case O1: the answer printed no answer ($CYC/answer.json)"
  opjson escalations-list list-after.json
  python3 -c 'import json,sys; assert sys.argv[2] not in [e["id"] for e in json.load(open(sys.argv[1])) or []]' \
    "$CYC/list-after.json" "$esc" || fail "case O1: the answered escalation is still open ($CYC/list-after.json)"
  waitfor "the attempt to leave blocked in aicrew team state" 300 attempt_unblocked
  waitfor "sim-coordinator's turn to end" 120 idle coordinator
  # The pause holds the members' turns and shows in the team's state; the
  # resume lifts it.
  opjson team-pause pause.json -set "REASON=case O1: the operator looks"
  team_state paused 's.get("paused") and s["paused"].get("reason") == "case O1: the operator looks"' ||
    fail "case O1: aicrew team state does not show the pause ($CYC/state-paused.json)"
  opjson team-resume resume.json
  team_state resumed 'not s.get("paused")' || fail "case O1: the team is still paused ($CYC/state-resumed.json)"
  case_done O1
  hold coordinator "case O1: its scenario's end"
  member_scenario coordinator '{}'
  member resume coordinator
}

# U1: a member whose turns leave one message unacknowledged (task
# 01a1fa19). The independent member never acknowledges; the operator's
# note is its one message. Its turns wait 10 seconds, then 40 (member.sh's
# turns.unacked_wait), and after the third it is held: the team's state
# shows the hold with the message and the count, and the coordinator is
# told once. A second note gives exactly one more turn, no sooner than 40
# seconds after the last, with the hold still shown and not told again.
# The team's pause, as docs/BRING-UP.md writes it, clears the hold, and
# the resume lets the member, acknowledging again, go on.
U1_WAIT=10
# member_turns ROLE FROM: the member's turn records after its first FROM.
member_turns() {
  dc exec -T -u member "sim-$1" sh -c 'tail -n +"$(($2 + 1))" "$1/logs/turns.jsonl"' sh "$(mhome "$1")" "$2"
}
# u1_turns N: the member's turns since the case began are N.
u1_turns() {
  local mark
  mark=$(turn_mark indep) && [ "${mark#* }" -ge "$((U1_FROM + $1))" ] && [ "${mark%% *}" = idle ]
}
# u1_check N: the U1 checks with N turns on the messages (cases.py unacked).
u1_check() {
  member_turns indep "$U1_FROM" > "$CYC/turns-$1.jsonl" && store_copy "$CYC/peek" &&
    python3 "$SIM/cases.py" unacked "$CYC/turns-$1.jsonl" "$CYC/peek" "$COORD_ID" "$INDEP_ID" "$1" "$U1_WAIT" \
      > "$CYC/u1-$1.json"
}
case_u1() {
  case_start U1
  hold indep "case U1: its scenario"
  fault U1 member_scenario indep '{"never_ack":true}'
  U1_FROM=$(turn_mark indep)
  U1_FROM=${U1_FROM#* }
  member resume indep
  member nudge indep "case U1: a message the member leaves unacknowledged"
  reach U1 held 300 "the member's unacknowledged hold in aicrew team state" team_state held \
    '(m["independent"].get("hold") or {}).get("kind") == "unacknowledged" and m["independent"]["hold"]["turns"] == 3'
  # No fourth turn on the old message: the hold's next turn is a day on.
  sleep $((5 * U1_WAIT))
  u1_check 3 || fail "case U1: three turns, then the hold ($CYC/u1-3.json)"
  ev u1 turns=3 facts="$(cat "$CYC/u1-3.json")"
  member nudge indep "case U1: a second message to the held member"
  waitfor "the held member's one turn on the new message" 300 u1_turns 4
  sleep $((5 * U1_WAIT))
  u1_check 4 || fail "case U1: one more turn on the new message, the hold not told again ($CYC/u1-4.json)"
  ev u1 turns=4 facts="$(cat "$CYC/u1-4.json")"
  team_state held-new '(m["independent"].get("hold") or {}).get("kind") == "unacknowledged" and len(m["independent"]["hold"]["messages"]) == 2 and m["independent"]["unacknowledged"] == 2' ||
    fail "case U1: the team state does not show the held member with both messages ($CYC/state-held-new.json)"
  opjson team-pause pause.json -set "REASON=case U1: the operator looks at the held member"
  waitfor "the pause to clear the member's hold in aicrew team state" 120 team_state cleared \
    'not m["independent"].get("hold")'
  hold indep "case U1: its scenario's end"
  member_scenario indep '{}'
  opjson team-resume resume.json
  member resume indep
  waitfor "the member's acknowledgement after the resume" 120 team_state acked 'm["independent"]["unacknowledged"] == 0'
}

# P5: the CI fails on the first head and the rework's second head carries
# the gates; then the merge commit's CI stays pending, and the coordinator's
# launcher's delivery check answers pending, which is never recorded as
# delivered and gives no turn. Once the CI succeeds, the launcher's next read
# confirms the delivery with no wake (task 01a1fa19-19a4).
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
  waitfor "the coordinator's launcher's delivery check of the pending merge" 300 \
    checked coordinator pending "https://sim-gitea:3000/operator/pilot/pulls/$PULL"
  hold coordinator "case P5: the pending delivery's checks"
  check_case P5 pending
  # The merge commit's CI completes. Nothing wakes the coordinator for it:
  # its launcher reads again on the check's schedule, at most two minutes
  # after the pending read, and the paused member's reads wait for the
  # resume.
  scenario "{$SCENARIO_REPOS}"
  waitfor "sim-ci's success on the merge commit of #$PULL" 120 pilot_ci_is "$MERGE" success
  member resume coordinator
  delivered
  snapshot done P5
}

. "$SIM/faults.sh"

# A skipped fault runs only its case: F1-claim-request runs F1.
for c in P2 P5 P4 E1 O1 U1 P1 F1 F2 F3 F4 F5 F6 F7 P3; do
  [ -z "$SKIP" ] || [ "${SKIP%%-*}" = "$c" ] || continue
  "case_${c,,}"
done
[ -z "$SKIP" ] || fail "AICREW_SIM_SKIP_FAULT=$SKIP: the case passed without its fault"
scenario "{$SCENARIO_REPOS}"
ev scenario_end name="$SCENARIO" result=PASS
SCENARIO=

docsteps_verify pilot-sim
collect
step "pilot-sim: green: the three members checked, listed and serving; the stubs' fixtures confirmed, refused and released; two cycles from a filed task to DONE with the delivery record; the pilot cases P1 to P5, the escalation case E1, the operator case O1, the unacknowledged case U1 and the fault cases F1 to F7, expected-to-fail parts in $XFAIL (reports in $OUT)"
