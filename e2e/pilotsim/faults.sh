# shellcheck shell=bash
# The fault cases F1 to F7 (docs/DESIGN-PILOT-SIMULATION.md,
# section 6.1; docs/E2E-PILOT-SIM.md, "The fault cases"), sourced by
# cases.sh, whose helpers they use. They adapt the b4 harness's cases
# (e2e/realaimem) to the compose stack: a fault is a rule armed on one of
# the fault proxies (sim-px-aimem, sim-px-crew, sim-px-peer, sim-px-hub) or a container
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
    ev fault case="$c" fault="sim-px-$p $*" state=left_out
    return 0
  fi
  RULE=$(px "$p" arm -case "$c" -action "$1" -method "$2" -path "$3" "${@:4}") || fail "case $c: arming sim-px-$p"
  ev fault case="$c" fault="sim-px-$p $*" state=armed
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

# ---- direct steps and aimem's gates ------------------------------------------------
# The cases F2-settle, F3-launcher, F4 and F5 drive the members' steps
# themselves, as b4's harness does, with both scripted members held so that
# no turn of theirs takes a step on the cases' tasks.

MHOME=/home/member/agent
# mstep_run ROLE OP NAME BODY [FLAG...]: `aicrew-agent step OP` in
# sim-ROLE, BODY (JSON, or empty for none) on its standard input; its
# answer in CYC/NAME.json and its exit in CYC/NAME.exit. Two can run at
# once (F4-race): record makes the facts afterwards.
mstep_run() {
  local role=$1 op=$2 name=$3 body=$4 code=0
  shift 4
  if [ -n "$body" ]; then
    printf '%s\n' "$body" | dc exec -T -u member "sim-$role" aicrew-agent step "$op" --home $MHOME --body - "$@" \
      > "$CYC/$name.json" 2> "$CYC/$name.err" || code=$?
  else
    dc exec -T -u member "sim-$role" aicrew-agent step "$op" --home $MHOME "$@" < /dev/null \
      > "$CYC/$name.json" 2> "$CYC/$name.err" || code=$?
  fi
  echo "$code" > "$CYC/$name.exit"
}
# record NAME: the fact NAME, {"exit": N, "answer": the step's answer or null}.
record() { fact "$1" "$(python3 "$SIM/cases.py" answer "$CYC/$1.json" "$(cat "$CYC/$1.exit")")"; }
# mstep ROLE OP NAME BODY [FLAG...]: mstep_run, then record.
mstep() {
  mstep_run "$@"
  record "$3"
}
# must ROLE OP NAME BODY [FLAG...]: mstep, and the step must succeed (a
# reservation step must commit).
must() {
  mstep "$@"
  python3 "$SIM/cases.py" succeeded "$CYC/facts.json" "$3" ||
    fail "case $CYCLE: sim-$1's $2 ($3) did not succeed: $(cat "$CYC/$3.json" "$CYC/$3.err" 2>/dev/null | head -c 600)"
}
# attempt_id NAME: the attempt the fact NAME's answer names.
attempt_id() { python3 "$SIM/cases.py" attempt_id "$CYC/facts.json" "$1"; }
# attempt_of_task: the ID of TASK's latest attempt, from aicrewd's store.
attempt_of_task() { store_copy "$CYC/peek" && python3 "$SIM/cases.py" attempt_of "$CYC/peek" "$TASK"; }
# settled_as STATE: TASK's latest attempt is STATE with no step pending.
settled_as() { store_copy "$CYC/peek" && python3 "$SIM/cases.py" settled "$CYC/peek" "$TASK" "$1"; }
# channel_up ROLE: sim-ROLE's launcher answers a step.
channel_up() { dc exec -T -u member "sim-$1" aicrew-agent step pending --home $MHOME < /dev/null > /dev/null; }
# step_body offer|claim: the body of an offer to the worker, or of a claim,
# of TASK at its current revision; the process pin is the one the cycles'
# offers carried, from aicrewd's store, and --repository fills the rest.
step_body() {
  hubapi GET "/v1/tasks/$TASK" > "$CYC/task-now.json"
  store_copy "$CYC/peek" &&
    python3 "$SIM/cases.py" step_body "$1" "$CYC/peek" "$CYC/task-now.json" "$HUB_ID" "$WORKER_ID"
}
# new_task NAME: TASK, a READY task of its own on the board, filed under NAME.
new_task() {
  CYCLE=$1 file_task
  step "case $CYCLE: task $TASK ($1)"
}
# close_task NAME: TASK's open attempts ended as b4 ends them (the
# coordinator withdraws an offer; it stops a running attempt, whose holder
# confirms and releases the task to READY), then the operator cancels the
# task on the board, so that no later turn takes it.
close_task() {
  local id state holder n=0
  store_copy "$CYC/peek" || fail "no stable copy of aicrewd's store"
  while read -r id state holder; do
    n=$((n + 1))
    case $state in
    offered) must coordinator withdraw "$1-$n-withdraw" '{}' --attempt "$id" --task "$TASK" ;;
    running)
      must coordinator stop "$1-$n-stop" '{"reason":"case done"}' --attempt "$id"
      must "$holder" confirm-stop "$1-$n-confirm" '{}' --attempt "$id"
      must "$holder" release "$1-$n-release" '{"target":"READY"}' --attempt "$id" --task "$TASK"
      ;;
    esac
  done < <(python3 "$SIM/cases.py" open_attempts "$CYC/peek" "$TASK" "$WORKER_ID" "$INDEP_ID")
  cancel_task
}
# gate ROLE FILE: a file in sim-ROLE's aimem-timed gate directory
# (e2e/pilotsim/member/aimem-timed); gated ROLE FILE: it is there.
gate() { dc exec -T -u member "sim-$1" sh -c 'mkdir -p /home/member/gate && : > "/home/member/gate/$1"' sh "$2"; }
gated() { dc exec -T -u member "sim-$1" test -e "/home/member/gate/$2"; }
# calls_mark ROLE: the lines of sim-ROLE's aimem timing log so far;
# calls_since ROLE MARK: the commands it logged since, as "CMD OP EXIT".
calls_mark() { dc exec -T -u member "sim-$1" sh -c 'cat /home/member/gate/timing.jsonl 2>/dev/null | wc -l'; }
calls_since() {
  dc exec -T -u member "sim-$1" sh -c 'tail -n +"$(($1 + 1))" /home/member/gate/timing.jsonl 2>/dev/null || true' sh "$2" |
    python3 "$SIM/cases.py" calls
}
# replay_claim ROLE KEY: the body aimem-timed captured from sim-ROLE's last
# reservation claim, sent again as the member's own `aimem reservation
# claim` under KEY (a new one when empty) at TASK's current revision: a
# replay of a proof, not a step. The proof stays in the container, and is
# removed there; the answer is {"exit": N, "code": aimem's refusal code}.
replay_claim() {
  local rev
  rev=$(hubapi GET "/v1/tasks/$TASK" | json_field revision)
  dc exec -T -u member "sim-$1" sh -c '
cd /home/member/agent || exit 2
f=/home/member/gate/captured-reservation-claim
sf=$(python3 -c "import json; print(json.load(open(\"state/aicrew-session.json\"))[\"aimem_session_file\"])")
key=$3
[ -n "$key" ] || key=sim-replay-$(date +%s%N)
python3 -c "import json, sys; b = json.load(open(sys.argv[1])); b[\"expected_revision\"] = int(sys.argv[2]); print(json.dumps(b))" \
  "$f" "$2" > "$f.replay"
code=0
AIMEM_STATE_DIR=/home/member/agent/aimem AIMEM_SOCKET=/home/member/agent/aimem/aimem.sock AIMEM_TEAM_SESSION=$sf \
  aimem reservation claim --task "$1" --key "$key" < "$f.replay" > "$f.out" 2>/dev/null || code=$?
rm -f "$f" "$f.replay"
python3 - "$code" "$f.out" <<"PY"
import json, sys
try:
    code = json.load(open(sys.argv[2])).get("code", "")
except (OSError, ValueError, AttributeError):
    code = ""
print(json.dumps({"exit": int(sys.argv[1]), "code": code}))
PY
rm -f "$f.out"' sh "$TASK" "$rev" "$2"
}
# pending_key ROLE: the request key of sim-ROLE's recorded step on TASK.
pending_key() {
  dc exec -T -u member "sim-$1" sh -c 'cat /home/member/agent/state/steps/step-*.json 2>/dev/null' |
    python3 "$SIM/cases.py" pending_key "$TASK"
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

  # F2-settle: sim-px-crew drops the reply to the settle of the
  # independent member's claim; its `step recover` settles it, once.
  local main=$TASK
  new_task F2-settle
  arm crew F2-settle drop_reply POST '^/v1/crew/attempts/[^/]+/settle$'
  mstep indep claim settle-claim "$(step_body claim)" --repository "$REPO_URL"
  mstep indep recover settle-recover ""
  mstep indep pending settle-pending ""
  disarm
  check_case F2-settle settle
  close_task settle
  TASK=$main
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

  # F3-launcher: aimem committed the independent member's claim, and its
  # launcher is killed (its container, before the settle): aicrewd's
  # reconciler settles the claim alone, within 45 s. The restarted
  # launcher's `step recover` leaves no recorded step.
  local pid
  new_task F3-launcher
  fault F3-launcher gate indep hold-after-reservation-claim
  mstep_run indep claim launcher-claim "$(step_body claim)" --repository "$REPO_URL" &
  pid=$!
  if [ "$SKIP" != F3-launcher ]; then
    waitfor "sim-indep's claim, committed and held before its settle" 300 gated indep paused-after-reservation-claim
    fact launcher_before "$(started sim-indep)"
    step "killing sim-indep, and its launcher with it, before the settle"
    dc kill sim-indep >/dev/null
  fi
  wait "$pid" || true
  record launcher-claim
  waitfor "the claim settled, the attempt running" 45 settled_as running
  if [ "$SKIP" != F3-launcher ]; then
    dc start sim-indep >/dev/null
    fact launcher_after "$(started sim-indep)"
    waitfor "sim-indep's step channel after its restart" 120 channel_up indep
    dc exec -T -u member sim-indep sh -c 'rm -f /home/member/gate/paused-after-*'
  fi
  mstep indep recover launcher-recover ""
  mstep indep pending launcher-pending ""
  check_case F3-launcher launcher
  close_task launcher

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

# ---- F4: competing steps --------------------------------------------------------------

# faulted CASE WHAT: the event of CASE's fault, WHAT; false when the run
# leaves it out.
faulted() {
  if [ "$SKIP" = "$1" ]; then
    step "case $1: its fault is left out ($2)"
    ev fault case="$1" fault="$2" state=left_out
    return 1
  fi
  ev fault case="$1" fault="$2" state=armed
}

# F4-second: a claim of a task with an open attempt is refused task_busy at
# its begin. F4-busy: an offer to a busy worker is refused agent_busy at its
# begin. F4-race: of an offer and a claim racing for one task, exactly one
# commits, the other is refused, and one attempt is open. F4-lost: the
# race's check takes a step lost before its commit (not_committed) for no
# conflict refusal; without its fault the loser's answer is made a lost
# step's, and the check must fail on it.
case_f4() {
  case_start F4
  hold worker "case F4: competing steps"
  hold coordinator "case F4: competing steps"
  local first offered race other pa pb

  new_task F4-second
  first=$TASK
  must coordinator offer second-offer "$(step_body offer)" --repository "$REPO_URL" --process
  offered=$(attempt_id second-offer)
  faulted F4-second "the independent member claims the offered task" || new_task F4-second-control
  mstep indep claim second-claim "$(step_body claim)" --repository "$REPO_URL"
  TASK=$first
  check_case F4-second second

  new_task F4-busy
  faulted F4-busy "the coordinator offers a second task to the worker, whose offer is open" ||
    must coordinator withdraw busy-free '{}' --attempt "$offered" --task "$first"
  mstep coordinator offer busy-offer "$(step_body offer)" --repository "$REPO_URL" --process
  check_case F4-busy busy
  close_task busy
  TASK=$first
  close_task second

  new_task F4-race
  race=$TASK other=$TASK
  if ! faulted F4-race "an offer and a claim of one task, at once"; then
    new_task F4-race-control
    other=$TASK
  fi
  TASK=$race
  pa=$(step_body offer)
  TASK=$other
  pb=$(step_body claim)
  mstep_run coordinator offer race-offer "$pa" --repository "$REPO_URL" --process &
  pa=$!
  mstep_run indep claim race-claim "$pb" --repository "$REPO_URL" &
  pb=$!
  wait "$pa" "$pb" || true
  record race-offer
  record race-claim
  TASK=$race
  if [ "$SKIP" = F4-lost ]; then
    step "case F4-lost: the race's loser answers as a step lost before its commit"
    ev fault case=F4-lost fault="the loser's answer made a lost step's" state=left_out
    python3 "$SIM/cases.py" lose "$CYC/facts.json"
    check_case F4-lost race
  fi
  check_case F4-race race
  close_task race
  if [ "$other" != "$race" ]; then
    TASK=$other
    close_task race-control
  fi
  member resume worker
  member resume coordinator
}

# ---- F5: stale steps --------------------------------------------------------------------

# edit_status TEXT: the hub's admin edits TASK's next action to TEXT, at its
# current revision; the hub's HTTP status.
edit_status() {
  hubapi GET "/v1/tasks/$TASK" > "$CYC/task-edit.json"
  hub sh -c 'curl -sS -o /dev/null -w "%{http_code}" --cacert /sim/tls/ca.pem -X PUT \
    -H "Authorization: Bearer $(cat "$HOME/admin.token")" -H "Content-Type: application/json" \
    -H "Idempotency-Key: sim-edit-$2-$(date +%s%N)" -d "$1" "https://sim-aimem:8440/v1/tasks/$2"' \
    sh "$(python3 "$SIM/cases.py" edit_body "$CYC/task-edit.json" "$1")" "$TASK"
}
# active_proof_control FACT: b4's control for F5-replay and F5-resume: the
# independent member's live proof, its claim held before it is sent,
# replayed under its own step's key, which aimem accepts; the answer is the
# fact FACT, which the case's check must refuse.
active_proof_control() {
  local pid
  new_task "F5-control-$1"
  gate indep capture-reservation-claim
  gate indep hold-reservation-claim
  mstep_run indep claim "$1-claim" "$(step_body claim)" --repository "$REPO_URL" &
  pid=$!
  waitfor "sim-indep's claim, held before it is sent" 300 gated indep paused-reservation-claim
  fact "$1" "$(replay_claim indep "$(pending_key indep)")"
  dc exec -T -u member sim-indep sh -c 'rm -f /home/member/gate/paused-reservation-claim'
  gate indep go-reservation-claim
  wait "$pid" || true
  record "$1-claim"
}

# F5-stale: the coordinator's claim held before it is sent while the hub's
# admin edits the still unheld task: aimem refuses the claim begun at the
# old revision, aicrewd settles the offer not committed, and the offer begun
# again at the new revision commits. (A held task cannot go stale under its
# holder: aimem refuses a plain edit of it.) F5-replay: a proof replayed
# after its step settled, under a new key, is refused. F5-resume: the
# coordinator's launcher dies under its offer and resumes under a new
# generation: the old generation's proof is refused, the resumed member may
# not continue the offer, and aicrewd settles it not committed within 45 s.
# F5-delay: aicrewd's coordination answer delayed past aimem's 2 s budget:
# aimem refuses the claim as retryable, and the step's retry commits.
case_f5() {
  case_start F5
  hold worker "case F5: stale steps"
  hold coordinator "case F5: stale steps"
  local pid mark

  new_task F5-held
  must coordinator offer held-offer "$(step_body offer)" --repository "$REPO_URL" --process
  fact held_edit "$(edit_status "edited while held")"
  close_task held

  new_task F5-stale
  fault F5-stale gate coordinator hold-reservation-claim
  mark=$(calls_mark coordinator)
  mstep_run coordinator offer stale-offer "$(step_body offer)" --repository "$REPO_URL" --process &
  pid=$!
  if [ "$SKIP" != F5-stale ]; then
    waitfor "the coordinator's claim, held before it is sent" 300 gated coordinator paused-reservation-claim
    fact stale_edit "$(edit_status "edited mid-step")"
    dc exec -T -u member sim-coordinator sh -c 'rm -f /home/member/gate/paused-reservation-claim'
    gate coordinator go-reservation-claim
  fi
  wait "$pid" || true
  record stale-offer
  fact stale_calls "$(calls_since coordinator "$mark")"
  check_case F5-stale stale
  waitfor "aicrewd settling the stale offer not committed" 120 settled_as closed
  must coordinator offer stale-again "$(step_body offer)" --repository "$REPO_URL" --process
  take F5-stale-again
  python3 "$SIM/cases.py" held "$D" || fail "case F5: the offer begun again at the new revision does not hold the task"
  close_task stale

  new_task F5-replay
  gate coordinator capture-reservation-claim
  must coordinator offer replay-offer "$(step_body offer)" --repository "$REPO_URL" --process
  must coordinator withdraw replay-withdraw '{}' --attempt "$(attempt_id replay-offer)" --task "$TASK"
  if faulted F5-replay "the settled offer's proof, replayed under a new key"; then
    fact replay "$(replay_claim coordinator "")"
  else
    dc exec -T -u member sim-coordinator sh -c 'rm -f /home/member/gate/captured-reservation-claim'
    active_proof_control replay
  fi
  check_case F5-replay replay
  close_task replay

  new_task F5-resume
  gate coordinator capture-reservation-claim
  fault F5-resume gate coordinator hold-reservation-claim
  mstep_run coordinator offer resume-offer "$(step_body offer)" --repository "$REPO_URL" --process &
  pid=$!
  if [ "$SKIP" != F5-resume ]; then
    waitfor "the coordinator's claim, held before it is sent" 300 gated coordinator paused-reservation-claim
    fact launcher_before "$(started sim-coordinator)"
    step "killing sim-coordinator, and its launcher with it, under the offer"
    dc kill sim-coordinator >/dev/null
    wait "$pid" || true
    dc start sim-coordinator >/dev/null
    fact launcher_after "$(started sim-coordinator)"
    waitfor "sim-coordinator's step channel after its restart" 120 channel_up coordinator
    dc exec -T -u member sim-coordinator sh -c 'rm -f /home/member/gate/paused-reservation-claim'
    fact resume_replay "$(replay_claim coordinator "")"
    mstep coordinator recover resume-recover ""
    waitfor "aicrewd settling the resumed coordinator's never-sent offer not committed" 45 settled_as closed
  else
    wait "$pid" || true
    dc exec -T -u member sim-coordinator sh -c 'rm -f /home/member/gate/captured-reservation-claim'
    active_proof_control resume_replay
  fi
  record resume-offer
  check_case F5-resume resume
  close_task resume

  new_task F5-delay
  arm peer F5-delay delay_reply POST '^/v1/crew/coordination$' -delay 3s
  mark=$(calls_mark indep)
  must indep claim delay-claim "$(step_body claim)" --repository "$REPO_URL"
  disarm
  fact delay_calls "$(calls_since indep "$mark")"
  check_case F5-delay delay
  close_task delay
  member resume worker
  member resume coordinator
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
    # Each verb files its own task (hubapi's idempotency key names CYCLE)
    # and keeps its own snapshots.
    CYCLE=F6-$verb CYC=$OUT/case-F6/$verb
    mkdir -p "$CYC"
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

  # F6-unreachable: sim-px-hub drops aicrewd's reads of the hub's peers and
  # the hub is stopped after the admin's recovery: nothing closes while the
  # hub is unreachable for aicrewd (four of its ticks); once the hub is back
  # and reachable, the attempt is closed as recovered.
  CYCLE=F6-unreachable CYC=$OUT/case-F6/unreachable
  mkdir -p "$CYC"
  hold worker "case F6-unreachable: the offer"
  file_task
  waitfor "the coordinator's triage and offer of task $TASK" 300 at 'offered/* 0'
  hold coordinator "case F6-unreachable: the recovery"
  member resume worker
  waitfor "the worker's accept" 300 at 'running/* *'
  hold worker "case F6-unreachable: the recovery"
  arm hub F6-unreachable drop_request GET '^/v1/identity/peers/' -persist
  recover_hold release
  if [ -n "$RULE" ]; then
    step "stopping sim-aimem"
    dc stop sim-aimem >/dev/null
  fi
  step "case F6-unreachable: four of aicrewd's ticks (65 s)"
  sleep 65
  if [ -n "$RULE" ]; then
    dc start sim-aimem >/dev/null
    waitfor "sim-aimem healthy after its restart" 120 healthy sim-aimem
  fi
  check_case F6-unreachable unreachable
  disarm
  waitfor "aicrewd closing the attempt as recovered, the hub back" 60 at 'closed/* *'
  take F6-unreachable-back
  python3 "$SIM/cases.py" recovered "$D" release || fail "case F6-unreachable: the attempt is not closed as recovered"
  cancel_task
  member resume coordinator
  member resume worker
}

# ---- F7: secrets ------------------------------------------------------------------------

# F7-leak: none of the secrets the run made appears in the containers' logs,
# the run's reports or aicrewd's audit. The run knows the secrets the
# runner moved (into), the hub's admin token, aicrewd's operator token and
# peer credentials, the coordination proofs sim-px-crew saw and the members'
# aimem session handles. Without its fault the case plants every one in a
# scanned log, and the scan must find each.
case_f7() {
  case_start F7
  local known=$KNOWN_DIR/secrets code=0 f
  { hub cat /home/aimem/admin.token; echo; } >> "$known"
  # aicrewd's credentials, as b4 knows them: the operator's token, and every
  # peer credential the hub provisioned (moved to aicrewd's host by archive,
  # not through into), at least the five b4 names.
  for f in aicrew/etc/operator.token $(op sh -c 'cd "$1/peer" && ls *.token' sh "$OPS_HOME"); do
    case $f in aicrew/*) ;; *) f=peer/$f ;; esac
    op sh -c 'test -s "$1" && cat "$1"' sh "$OPS_HOME/$f" >> "$known" || fail "case F7: no $OPS_HOME/$f to know"
    echo >> "$known"
  done
  for f in redeem read team-register team-read board-read; do
    op test -s "$OPS_HOME/peer/aimem-$f.token" || fail "case F7: no peer credential aimem-$f.token to know"
  done
  dc exec -T sim-px-crew sh -c 'cat /var/lib/simproxy/proofs 2>/dev/null || true' >> "$known"
  for role in coordinator worker indep; do
    dc exec -T -u member "sim-$role" sh -c 'cat /home/member/agent/aimem/aicrew-sessions/*.json 2>/dev/null || true' |
      python3 "$SIM/cases.py" handles >> "$known"
  done
  collect
  store_copy "$CYC/store" || fail "no stable copy of aicrewd's store for F7"
  if [ "$SKIP" = F7-leak ]; then
    step "case F7-leak: every known secret is planted in a scanned log"
    sed 's/^/planted /' "$known" > "$LOGS/planted.log"
  fi
  python3 "$SIM/cases.py" scan "$known" "$OUT" "$CYC/store" "$STAGE" "$KNOWN_DIR" \
    > "$KNOWN_DIR/scan.out" 2> "$KNOWN_DIR/scan.err" || code=$?
  cat "$KNOWN_DIR/scan.out"
  if [ "$SKIP" = F7-leak ]; then
    # Each secret, of every kind the run knows, is found where it was planted.
    local n found
    n=$(sed -n 's/^F7: \([0-9]*\) secrets known.*/\1/p' "$KNOWN_DIR/scan.out")
    found=$(grep -c ' in logs/planted\.log$' "$KNOWN_DIR/scan.err")
    [ "$code" = 1 ] && [ -n "$n" ] && [ "$found" = "$n" ] ||
      fail "AICREW_SIM_SKIP_FAULT=F7-leak: the scan found $found of the ${n:-?} planted secrets"
    skipped F7-leak scan
  fi
  cat "$KNOWN_DIR/scan.err" >&2
  [ "$code" = 0 ] || fail "case F7-leak: the secret scan"
}
