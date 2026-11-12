"""The pilot cases' and the fault cases' checks (docs/DESIGN-PILOT-SIMULATION.md, section 6).

    cases.py check CASE POINT DIR
    cases.py followup STORE_DIR TASK COORDINATOR   (P1: a message to the coordinator about an unacked one)
    cases.py hold STORE_DIR AGENT KIND             (the member's hold in aicrewd is KIND; "none" for no hold)
    cases.py has_op STORE_DIR TASK OP              (aicrewd audited OP, e.g. attempt.answer, on the task's attempt)
    cases.py blocked_report STORE_DIR TASK COORDINATOR  (P4: the coordinator has the stall's blocked report)
    cases.py lost STORE_DIR TASK OP                (F1: what a lost OP request leaves in aicrewd)
    cases.py logged CASE OUTCOME < LOG             (a fault proxy's log has a request of CASE with OUTCOME)
    cases.py recover_body VERB HOLD TASK           (F6: the admin's recovery body, from recover status and the task)
    cases.py cancel_body TASK                      (F6: the operator's edit of the task to CANCELLED)
    cases.py handles < FILES                       (F7: the aimem session handles in session files)
    cases.py scan SECRETS ROOT STORE_DIR SKIP...   (F7: the secrets in ROOT's files and the store's audit)

The direct steps of F2-settle, F3-launcher, F4 and F5 (faults.sh):

    cases.py answer ANSWER_FILE EXIT               (a step's fact: {"exit": EXIT, "answer": the answer or null})
    cases.py succeeded FACTS NAME                  (the step NAME succeeded; a reservation step committed)
    cases.py attempt_id FACTS NAME                 (the attempt the step NAME's answer names)
    cases.py attempt_of STORE_DIR TASK             (the task's latest attempt)
    cases.py pinned STORE_DIR                      (an attempt carries the process pin step_body copies)
    cases.py settled STORE_DIR TASK STATE          (the latest attempt is STATE, with no step pending)
    cases.py step_body offer|claim STORE_DIR TASK_FILE HUB WORKER  (a step's body at the task's revision)
    cases.py edit_body TASK_FILE TEXT              (F5: the admin's edit of the task's next action)
    cases.py open_attempts STORE_DIR TASK WORKER INDEP  (the open attempts, "ID STATE HOLDER" a line)
    cases.py calls < TIMING                        (aimem-timed's log as "CMD OP EXIT")
    cases.py pending_key TASK < STEPS              (the request key of a member's recorded step on TASK)
    cases.py lose FACTS                            (F4-lost: the race's loser answers as a lost step)
    cases.py held DIR                              (the snapshot's task is held in aimem)
    cases.py recovered DIR VERB                    (F6-unreachable: closed as recovered by VERB)

DIR is the snapshot scripts/pilot-sim.sh takes at the case's POINT, laid out
as cycle.py's (its docstring), with ids.json also naming "first_head" for
P5 and the forge's forge/commits.json (the pull request's commits),
forge/first-status.json and forge/head-status.json (the combined status of
the first and the final head), and status.json (`aicrew status --team`) for
P3, and for the fault cases proxy/NAME.jsonl (each fault proxy's request
log) and facts.json (what the runner recorded, such as container start
times). Prints each failed expectation and exits 1 when there is one.

A case's checks are what a run without the case's fault fails:
AICREW_SIM_SKIP_FAULT=CASE runs the case without it and expects them to
fail at the point the runner names.
"""
import datetime
import json
import os
import re
import sqlite3
import sys

from cycle import load, loadj, items, reviews


# What a follow-up about an unacknowledged message says.
FOLLOWUP = re.compile(r"acknowledg|\bunacked\b|\bnot acked\b", re.I)


def db_of(d):
    db = sqlite3.connect(os.path.join(d, "aicrew.db"))
    db.row_factory = sqlite3.Row
    return lambda sql, *a: db.execute(sql, a).fetchall()


def attempt_of(q, task):
    rows = q("SELECT * FROM attempts WHERE task_id = ?", task)
    return rows[0] if len(rows) == 1 else None


def attempts_of(q, task):
    return q("SELECT * FROM attempts WHERE task_id = ? ORDER BY created_at, id", task)


def latest(q, task):
    rows = attempts_of(q, task)
    return rows[-1] if rows else None


def messages_to(q, agent, attempt, task):
    return q("""SELECT m.seq, m.kind, m.text, r.first_delivered_at, r.acknowledged_at FROM messages m
                JOIN message_recipients r ON r.message_id = m.id
                WHERE r.agent_id = ? AND (m.attempt_id = ? OR m.task_id = ?) ORDER BY m.seq""", agent, attempt, task)


def ops_of(q, attempt):
    """The attempt's operations in aicrewd's audit (attempt.answer, ...); attempt_steps holds only
    the aimem reservation steps."""
    return [r["operation"] for r in q("SELECT operation FROM audit WHERE scope = ? AND operation LIKE 'attempt.%'",
                                      attempt)]


def p1_unacked(d, q, ids, a, expect):
    rows = messages_to(q, ids["worker"], a["id"], ids["task"])
    unacked = [r for r in rows if r["first_delivered_at"] and not r["acknowledged_at"]]
    expect(unacked, "aicrewd: every message to the worker about the task was acked: %s" %
           [(r["seq"], r["kind"]) for r in rows])


def p2_submit(d, q, ids, a, expect):
    commits = loadj(d, "forge/commits.json", []) or []
    pr = loadj(d, "forge/pull.json", {})
    head = (pr.get("head") or {}).get("sha", "")
    revs = reviews(loadj(d, "forge/comments.json", []))
    expect(len(commits) == 2, "forge: the pull request has %d commits, not 2: no push during the review" % len(commits))
    # The forge lists the commits newest first: the older one is the first head.
    for first in [c["sha"] for c in commits if c["sha"] != head]:
        expect(not [r for r in revs if r[0] == "external" and r[1] == first],
               "forge: an external review of the first head %s counts: %s" % (first, revs))
    expect(("external", head, "READY_FOR_HUMAN_MERGE") in revs, "forge: no external READY at the head %s: %s" % (head, revs))
    expect("pushed while the external review ran" in (load(d, "homes/worker/HANDOFF.md", "") or ""),
           "worker: the handoff does not record the push during the review")
    expect((a["state"], a["phase"]) == ("running", "submitted"), "aicrewd: the attempt is %s/%s" % (a["state"], a["phase"]))


def p3_held(d, q, ids, a, expect):
    holds = {r["agent_id"]: r for r in q("SELECT * FROM member_holds")}
    c, w = holds.get(ids["coordinator"]), holds.get(ids["worker"])
    expect(c and c["kind"] == "daily_budget" and c["spent_usd"] >= c["limit_usd"] > 0,
           "aicrewd: the coordinator's hold is %s" % (dict(c) if c else None))
    expect(w and w["kind"] == "needs_login" and w["command"],
           "aicrewd: the worker's hold is %s" % (dict(w) if w else None))
    told = [r["text"] for r in q("""SELECT m.text FROM messages m JOIN message_recipients r ON r.message_id = m.id
                                    WHERE r.agent_id = ? AND m.text LIKE 'Member % is held: %'""", ids["coordinator"])]
    expect(any("login is missing or expired" in t for t in told),
           "aicrewd: the coordinator was not told of the worker's login hold: %s" % told)
    status = {m["agent_id"]: m for t in loadj(d, "status.json", []) for m in t.get("members", [])}
    for role, kind in (("coordinator", "daily_budget"), ("worker", "needs_login")):
        got = (status.get(ids[role], {}).get("hold") or {}).get("kind")
        expect(got == kind, "aicrew status: the %s's hold is %s, not %s" % (role, got, kind))
    for role in ("coordinator", "worker"):
        st = loadj(d, "homes/%s/turns.json" % role, {}) or {}
        expect(st.get("hold"), "%s: the launcher holds no turn" % role)


# The launcher's reason for blocking a stalled attempt.
STALLED = "The turn loop stalled this attempt"


def blocked_reports(q, coordinator, a, task):
    """The coordinator's blocked-wake messages about the attempt."""
    return [r["text"] for r in messages_to(q, coordinator, a["id"], task)
            if STALLED in (r["text"] or "") and "reported task" in r["text"]]


def p4_blocked(d, q, ids, a, expect):
    expect((a["state"], a["phase"]) == ("running", "blocked"), "aicrewd: the attempt is %s/%s" % (a["state"], a["phase"]))
    # A worker's blocked report is kept apart from the hub's grant block
    # (attempts.blocked_reason).
    block = q("SELECT reason FROM attempt_blocks WHERE attempt_id = ?", a["id"])
    expect(block and STALLED in block[0]["reason"], "aicrewd: the attempt's blocked report is %s" %
           [r["reason"] for r in block])
    expect(blocked_reports(q, ids["coordinator"], a, ids["task"]),
           "aicrewd: the coordinator has no blocked report: %s" %
           [r["text"] for r in messages_to(q, ids["coordinator"], a["id"], ids["task"])])
    t = loadj(d, "aimem/task.json", {})
    expect(t.get("state") == "BLOCKED", "aimem: the task is %s, not BLOCKED" % t.get("state"))
    expect(STALLED in (t.get("blocker") or ""), "aimem: the task's blocker is %r" % t.get("blocker"))


def p4_answered(d, q, ids, a, expect):
    expect("attempt.answer" in ops_of(q, a["id"]), "aicrewd: no attempt.answer among %s" % ops_of(q, a["id"]))
    expect("Answered the blocked report of attempt " + a["id"] in (load(d, "homes/coordinator/HANDOFF.md", "") or ""),
           "coordinator: the handoff does not record the answer")


def p5_submit(d, q, ids, a, expect):
    first = ids.get("first_head", "")
    pr = loadj(d, "forge/pull.json", {})
    head = (pr.get("head") or {}).get("sha", "")
    expect((loadj(d, "forge/first-status.json", {}) or {}).get("state") == "failure",
           "forge: the first head's CI is not a failure")
    expect(head and head != first, "forge: the head %s is not a second head after %s" % (head, first))
    expect((loadj(d, "forge/head-status.json", {}) or {}).get("state") == "success", "forge: the head's CI is not a success")
    revs = reviews(loadj(d, "forge/comments.json", []))
    expect(("external", head, "READY_FOR_HUMAN_MERGE") in revs, "forge: no external READY at the head %s: %s" % (head, revs))
    expect("the checks failed at " + first in (load(d, "homes/worker/HANDOFF.md", "") or ""),
           "worker: the handoff does not record the rework for the failed checks")
    pulls = q("SELECT head_commit FROM attempt_pulls WHERE attempt_id = ?", a["id"])
    expect(pulls and pulls[0]["head_commit"] == head, "aicrewd: the watched head is %s, not %s" %
           ([p["head_commit"] for p in pulls], head))
    expect((a["state"], a["phase"]) == ("running", "submitted"), "aicrewd: the attempt is %s/%s" % (a["state"], a["phase"]))


def p5_pending(d, q, ids, a, expect):
    expect((a["state"], a["phase"]) == ("running", "accepted"), "aicrewd: the attempt is %s/%s" % (a["state"], a["phase"]))
    expect(a["delivery_result"] == 0 and a["finalized_result"] == 0,
           "aicrewd: a pending delivery was recorded: delivered %s, finalized %s" % (a["delivery_result"], a["finalized_result"]))
    expect("attempt.confirm_delivery" not in ops_of(q, a["id"]), "aicrewd: an attempt.confirm_delivery on a pending CI")
    expect((loadj(d, "forge/status.json", {}) or {}).get("state") == "pending", "forge: the merge commit's CI is not pending")
    t = loadj(d, "aimem/task.json", {})
    expect(t.get("state") == "REVIEW", "aimem: the task is %s, not REVIEW" % t.get("state"))
    expect("verify-delivery: pending for" in (load(d, "homes/coordinator/HANDOFF.md", "") or ""),
           "coordinator: the handoff does not record verify-delivery's pending answer")


# ---- the fault cases ---------------------------------------------------------------

def proxy_log(d, name):
    return [json.loads(l) for l in (load(d, "proxy/%s.jsonl" % name, "") or "").splitlines() if l.strip()]


RECEIPT = re.compile(r"/reservation/receipts?(/|$)")


def op_path(op):
    return re.compile(r"^/v1/projects/[^/]+/tasks/[^/]+/reservation/%s$" % op)


def steps_of(q, attempt, op):
    return [r["outcome"] for r in
            q("SELECT outcome FROM attempt_steps WHERE attempt_id = ? AND operation = ? ORDER BY settled_at", attempt, op)]


def lost_in_store(q, task, op):
    """What a lost OP request leaves in aicrewd: the step settled not committed, or for an update kept
    pending, undecided."""
    a = latest(q, task)
    if a is None:
        return False
    if op == "claim":
        return any(r["state"] == "closed" and r["pending_key"] == "" for r in attempts_of(q, task))
    if op == "update":
        return a["state"] == "running" and a["pending_op"] == "update" and a["pending_key"] != ""
    want = {"transfer": "offered", "finalize": "running"}[op]
    return a["state"] == want and a["pending_key"] == "" and any(o != "committed" for o in steps_of(q, a["id"], op))


def holder_of(d):
    hold = loadj(d, "aimem/reservation.json", {}) or {}
    return hold.get("state"), (hold.get("binding") or {}).get("role")


def f1_request(case, op):
    def check(d, q, ids, a, expect):
        log = proxy_log(d, "aimem")
        drops = [e for e in log if e.get("case") == case and e["outcome"] == "dropped_request"]
        expect(drops and all(e["method"] == "POST" and op_path(op).match(e["path"]) for e in drops),
               "sim-px-aimem: no dropped %s request of %s: %s" % (op, case, drops))
        if drops:
            seen = [e for e in log if e["seq"] > drops[0]["seq"] and e["method"] == "POST" and op_path(op).match(e["path"])
                    and e["outcome"] != "dropped_request"]
            expect(not seen, "sim-px-aimem: the hub saw a %s request after the drop: %s" % (op, seen))
        expect(lost_in_store(q, ids["task"], op), "aicrewd: the lost %s is not settled as a lost request leaves it: %s" %
               (op, dict(a)))
        t = loadj(d, "aimem/task.json", {})
        state, holder = holder_of(d)
        handoff = load(d, "homes/worker/HANDOFF.md", "") or ""
        if op == "claim":
            expect(state != "held", "aimem: the task is held after the lost claim request")
        elif op == "transfer":
            expect((state, holder) == ("held", "coordinator"), "aimem: the hold is %s by %s, not the coordinator's" %
                   (state, holder))
        elif op == "update":
            expect(t.get("state") == "READY", "aimem: the task is %s after the lost update, not READY" % t.get("state"))
            expect("is pending, undecided: request key " + a["pending_key"] + "." in handoff,
                   "worker: the handoff does not record the pending submit's request key %s" % a["pending_key"])
        else:
            expect(t.get("state") == "REVIEW", "aimem: the task is %s after the lost finalize, not REVIEW" % t.get("state"))
            expect(a["phase"] == "accepted", "aicrewd: the attempt's phase is %s" % a["phase"])
    return check


# What the step leads to once its reply is reconciled: the attempt's state
# and phase (None for any), the task's state, the hold's holder.
AFTER_REPLY = {"claim": ("offered", None, "READY", "coordinator"), "transfer": ("running", None, "READY", "worker"),
               "update": ("running", "submitted", "REVIEW", "worker"), "finalize": ("closed", "finalized", "DONE", None)}


def f1_reply(case, op):
    def check(d, q, ids, a, expect):
        log = proxy_log(d, "aimem")
        drops = [e for e in log if e.get("case") == case and e["outcome"] == "dropped_reply"]
        expect(drops and all(e["method"] == "POST" and op_path(op).match(e["path"]) and 200 <= e["status"] < 300
                             for e in drops),
               "sim-px-aimem: no %s reply the hub answered and the proxy dropped (%s): %s" % (op, case, drops))
        if drops:
            receipts = [e for e in log if e["seq"] > drops[0]["seq"] and e["method"] == "GET" and RECEIPT.search(e["path"])
                        and e["status"] == 200]
            expect(receipts, "sim-px-aimem: no receipt read after the dropped %s reply" % op)
        outcomes = steps_of(q, a["id"], op)
        expect("committed" in outcomes, "aicrewd: no committed %s step: %s" % (op, outcomes))
        expect(a["pending_key"] == "", "aicrewd: the attempt still has a pending step %s" % a["pending_op"])
        st, phase, task_state, holder = AFTER_REPLY[op]
        expect(a["state"] == st and phase in (None, a["phase"]),
               "aicrewd: the attempt is %s/%s, not %s/%s" % (a["state"], a["phase"], st, phase))
        t = loadj(d, "aimem/task.json", {})
        expect(t.get("state") == task_state, "aimem: the task is %s, not %s" % (t.get("state"), task_state))
        if holder:
            expect(holder_of(d) == ("held", holder), "aimem: the hold is %s by %s, not the %s's" % (holder_of(d) + (holder,)))
        if op == "update":
            expect("The submit superseded the pending one" in (load(d, "homes/worker/HANDOFF.md", "") or ""),
                   "worker: the handoff does not record the superseding submit")
    return check


def f2_begin(d, q, ids, a, expect):
    begins = [e for e in proxy_log(d, "crew") if e["method"] == "POST" and e["path"] == "/v1/crew/attempts"]
    drops = [e for e in begins if e.get("case") == "F2-begin" and e["outcome"] == "dropped_reply"]
    expect(len(drops) == 1 and 200 <= drops[0]["status"] < 300,
           "sim-px-crew: not one begin aicrewd answered and the proxy dropped: %s" % drops)
    if drops:
        expect([e for e in begins if e["seq"] > drops[0]["seq"] and e["outcome"] == "forwarded"],
               "sim-px-crew: no begin replayed after the dropped reply")
    rows = attempts_of(q, ids["task"])
    expect(len(rows) == 1 and rows[0]["state"] == "offered" and rows[0]["pending_key"] == "",
           "aicrewd: the replayed begin did not reach the one attempt: %s" % [(r["state"], r["pending_key"]) for r in rows])
    expect("Step offer failed (unavailable); step recover finished it: committed." in
           (load(d, "homes/coordinator/HANDOFF.md", "") or ""),
           "coordinator: the handoff does not record the failed offer and its recovery")
    expect(holder_of(d) == ("held", "coordinator"), "aimem: the hold is %s by %s" % holder_of(d))


def instant(s):
    """An RFC 3339 time, Docker's or aicrewd's, in UTC to the microsecond; None for anything else."""
    m = re.match(r"^(\d{4}-\d\d-\d\dT\d\d:\d\d:\d\d)(?:\.(\d+))?(Z|[+-]\d\d:\d\d)$", s or "")
    if not m:
        return None
    t = datetime.datetime.fromisoformat("%s.%s%s" % (m.group(1), (m.group(2) or "0")[:6].ljust(6, "0"),
                                                      "+00:00" if m.group(3) == "Z" else m.group(3)))
    return t.astimezone(datetime.timezone.utc)


def restarted(facts, name, expect):
    before, after = instant(facts.get(name + "_before")), instant(facts.get(name + "_after"))
    expect(before and after and after > before, "%s did not restart: started %s, then %s" %
           (name, facts.get(name + "_before"), facts.get(name + "_after")))
    return after


def f3_aicrewd(d, q, ids, a, expect):
    after = restarted(loadj(d, "facts.json", {}) or {}, "aicrewd", expect)
    delayed = [e for e in proxy_log(d, "aimem") if e.get("case") == "F3-aicrewd" and e["outcome"] == "delayed_reply"]
    expect(delayed and 200 <= delayed[0]["status"] < 300, "sim-px-aimem: the hub's claim answer was not held: %s" % delayed)
    claims = q("SELECT outcome, settled_at FROM attempt_steps WHERE attempt_id = ? AND operation = 'claim'", a["id"])
    settled = instant(claims[0]["settled_at"]) if len(claims) == 1 else None
    expect(claims and claims[0]["outcome"] == "committed" and after and settled and settled > after,
           "aicrewd: the claim did not settle committed against the restarted aicrewd: %s (restarted %s)" %
           ([tuple(c) for c in claims], after))
    expect((a["state"], a["pending_key"]) == ("offered", ""), "aicrewd: the attempt is %s, pending %r" %
           (a["state"], a["pending_key"]))


def f3_hub(d, q, ids, a, expect):
    facts = loadj(d, "facts.json", {}) or {}
    restarted(facts, "hub", expect)
    was, now = facts.get("hold_before") or {}, loadj(d, "aimem/reservation.json", {}) or {}
    expect(now.get("state") == "held", "aimem: the hold is %s after the hub's restart" % now.get("state"))
    # The hold itself, not when the status was read.
    kept = lambda h: {k: v for k, v in h.items() if not k.endswith("_at")}
    expect(kept(now) == kept(was), "aimem: the hold changed across the hub's restart: %s, then %s" % (was, now))


def f6_recovered(verb):
    def check(d, q, ids, a, expect):
        expect(a["state"] == "closed" and a["close_reason"] == "recovered" and a["recovered_by"] == "recovery_" + verb and
               a["recovered_fence"] != "",
               "aicrewd: the attempt is %s (%s), recovered by %r at fence %r" %
               (a["state"], a["close_reason"], a["recovered_by"], a["recovered_fence"]))
        t = loadj(d, "aimem/task.json", {})
        want = {"release": "READY", "cancel": "CANCELLED"}[verb]
        expect(t.get("state") == want, "aimem: the task is %s, not %s" % (t.get("state"), want))
    return check


# ---- the direct steps' answers (F2-settle, F3-launcher, F4, F5) ---------------------------

def answer_of(facts, name):
    """The fact NAME the runner recorded for a step: (exit, answer), the answer {} when there was none."""
    f = facts.get(name) or {}
    return f.get("exit"), f.get("answer") or {}


def result_of(ans):
    return ans.get("result") if isinstance(ans.get("result"), dict) else {}


def committed(code, ans):
    r = result_of(ans)
    return code == 0 and ans.get("ok") is True and r.get("settled") is True and r.get("outcome") == "committed"


def refused_at_begin(ans, *codes):
    """aicrew's refusal at the step's begin, with one of CODES (process_faults_test.go, refusedAtBegin)."""
    return ans.get("status") == "refused" and not ans.get("ok") and (ans.get("error") or {}).get("code") in codes


# aimem's own refusals of a mutation that lost to another (b4's aimemConflictCodes). The driver's
# not_committed, made from a receipt after a lost reply, is none of them: it proves no commit, not a conflict.
AIMEM_CONFLICT = ("reservation_conflict", "revision_conflict", "stale_fence")


def refused_by_aimem(ans):
    r = result_of(ans)
    return (ans.get("status") in ("refused", "pending") and (r.get("report") or {}).get("outcome") == "refused" and
            (r.get("report") or {}).get("code") in AIMEM_CONFLICT and r.get("outcome") != "committed")


# A step lost before its commit, as the driver settles it.
LOST = {"ok": False, "status": "refused",
        "result": {"report": {"outcome": "refused", "code": "not_committed"}, "settled": True, "outcome": "not_committed"}}


def open_work(q, agent):
    return q("SELECT count(*) AS n FROM attempts WHERE worker_agent_id = ? AND state != 'closed'", agent)[0]["n"]


def f2_settle(d, q, ids, a, expect):
    facts = loadj(d, "facts.json", {}) or {}
    code, ans = answer_of(facts, "settle-claim")
    expect(code not in (None, 0) and not ans.get("ok"), "indep: the claim did not see its settle fail: exit %s, %s" % (code, ans))
    drops = [e for e in proxy_log(d, "crew") if e.get("case") == "F2-settle" and e["outcome"] == "dropped_reply"]
    expect(drops and all(e["method"] == "POST" and e["path"].endswith("/settle") and 200 <= e["status"] < 300
                         for e in drops),
           "sim-px-crew: no settle aicrewd answered and the proxy dropped: %s" % drops)
    claims = steps_of(q, a["id"], "claim") if a else []
    expect(a and a["state"] == "running" and a["pending_key"] == "" and claims == ["committed"],
           "aicrewd: the retried settle did not leave the claim running, settled once: %s, steps %s" %
           (dict(a) if a else None, claims))
    _, pending = answer_of(facts, "settle-pending")
    expect(pending.get("ok") and pending.get("result") is None, "indep: a step is left recorded: %s" % pending)


def f3_launcher(d, q, ids, a, expect):
    facts = loadj(d, "facts.json", {}) or {}
    restarted(facts, "launcher", expect)
    expect(a and a["state"] == "running" and a["pending_key"] == "",
           "aicrewd: the claim is not settled, running: %s" % (dict(a) if a else None))
    n = q("""SELECT count(*) AS n FROM audit WHERE operation = 'attempt.settle' AND caller_kind = 'reconciler'
             AND scope = ?""", a["id"] if a else "")[0]["n"]
    expect(n > 0, "aicrewd: the reconciler did not settle the claim (the launcher was down)")
    _, pending = answer_of(facts, "launcher-pending")
    expect(pending.get("ok") and pending.get("result") is None, "indep: the restarted launcher has a recorded step: %s" %
           pending)


def f4_second(d, q, ids, a, expect):
    code, ans = answer_of(loadj(d, "facts.json", {}) or {}, "second-claim")
    expect(code == 3 and refused_at_begin(ans, "task_busy"),
           "indep: the claim of the task with an open attempt was not refused task_busy at begin: exit %s, %s" % (code, ans))
    expect(len(attempts_of(q, ids["task"])) == 1, "aicrewd: the task has %d attempts, not 1" %
           len(attempts_of(q, ids["task"])))


def f4_busy(d, q, ids, a, expect):
    code, ans = answer_of(loadj(d, "facts.json", {}) or {}, "busy-offer")
    expect(code == 3 and refused_at_begin(ans, "agent_busy"),
           "coordinator: the offer to the busy worker was not refused agent_busy at begin: exit %s, %s" % (code, ans))


def f4_race(d, q, ids, a, expect):
    facts = loadj(d, "facts.json", {}) or {}
    steps = [answer_of(facts, "race-offer"), answer_of(facts, "race-claim")]
    wins = [s for s in steps if committed(*s)]
    losers = [s for s in steps if not committed(*s)]
    loser_refused = len(losers) == 1 and (refused_at_begin(losers[0][1], "task_busy", "agent_busy") or
                                          refused_by_aimem(losers[0][1]))
    expect(not refused_by_aimem(LOST) and not refused_at_begin(LOST, "task_busy", "agent_busy") and
           not refused_by_aimem({"status": "failed"}),
           "the race's check takes a lost step, or a failure, for a conflict refusal")
    still = [r for r in attempts_of(q, ids["task"]) if r["state"] != "closed"]
    expect(len(wins) == 1 and loser_refused and len(still) == 1 and holder_of(d)[0] == "held",
           "of the racing offer and claim, not exactly one committed and the other refused, one attempt open: %s, open %d" %
           (steps, len(still)))


def f5_stale(d, q, ids, a, expect):
    facts = loadj(d, "facts.json", {}) or {}
    expect(facts.get("held_edit") == 409, "aimem: a plain edit of a held task answered %s, not 409" % facts.get("held_edit"))
    expect(facts.get("stale_edit") == 200, "aimem: the edit of the unheld task mid-step answered %s, not 200" %
           facts.get("stale_edit"))
    calls = facts.get("stale_calls") or []
    expect("reservation claim 3" in calls, "aimem did not refuse the claim begun at the old revision: %s" % calls)


def f5_replay(d, q, ids, a, expect):
    r = (loadj(d, "facts.json", {}) or {}).get("replay") or {}
    expect(r.get("exit") == 3 and r.get("code"), "aimem did not refuse the settled step's proof replayed under a new key: %s" % r)


def f5_resume(d, q, ids, a, expect):
    facts = loadj(d, "facts.json", {}) or {}
    restarted(facts, "launcher", expect)
    r = facts.get("resume_replay") or {}
    expect(r.get("exit") == 3 and r.get("code"), "aimem did not refuse the old generation's proof: %s" % r)
    _, rec = answer_of(facts, "resume-recover")
    expect(not rec.get("ok"), "coordinator: the new generation continued the old generation's offer: %s" % rec)
    expect(holder_of(d)[0] != "held", "aimem holds the task for the old generation's offer")
    expect(a and a["state"] == "closed" and a["close_reason"] == "claim not_committed",
           "aicrewd: the never-sent offer is not settled not committed: %s" % (dict(a) if a else None))
    expect(open_work(q, ids["worker"]) == 0, "aicrewd: the worker's capacity is not free after the resumed offer")


def f5_delay(d, q, ids, a, expect):
    facts = loadj(d, "facts.json", {}) or {}
    delayed = [e for e in proxy_log(d, "peer") if e.get("case") == "F5-delay" and e["outcome"] == "delayed_reply"]
    expect(delayed, "sim-px-peer: aicrewd's coordination answer was not delayed")
    calls = facts.get("delay_calls") or []
    expect("reservation claim 4" in calls and "reservation claim 0" in calls,
           "aimem did not refuse the slow claim as retryable, then commit its retry: %s" % calls)


def f6_unreachable(d, q, ids, a, expect):
    drops = [e for e in proxy_log(d, "hub") if e.get("case") == "F6-unreachable" and e["outcome"] == "dropped_request"]
    expect(drops, "sim-px-hub: no read of the hub's peers was dropped")
    expect(a and a["state"] != "closed", "aicrewd closed the attempt while the hub was unreachable: %s" %
           (dict(a) if a else None))


CHECKS = {
    ("P1", "unacked"): p1_unacked,
    ("P2", "submit"): p2_submit,
    ("P3", "held"): p3_held,
    ("P4", "blocked"): p4_blocked,
    ("P4", "answered"): p4_answered,
    ("P5", "submit"): p5_submit,
    ("P5", "pending"): p5_pending,
    ("F2-begin", "begin"): f2_begin,
    ("F3-aicrewd", "restart"): f3_aicrewd,
    ("F3-hub", "restart"): f3_hub,
    ("F6-release", "recovered"): f6_recovered("release"),
    ("F6-cancel", "recovered"): f6_recovered("cancel"),
    ("F2-settle", "settle"): f2_settle,
    ("F3-launcher", "launcher"): f3_launcher,
    ("F4-second", "second"): f4_second,
    ("F4-busy", "busy"): f4_busy,
    ("F4-race", "race"): f4_race,
    ("F4-lost", "race"): f4_race,
    ("F5-stale", "stale"): f5_stale,
    ("F5-replay", "replay"): f5_replay,
    ("F5-resume", "resume"): f5_resume,
    ("F5-delay", "delay"): f5_delay,
    ("F6-unreachable", "unreachable"): f6_unreachable,
}
# The cases whose checks read the steps' answers: their task may have no attempt (F4-busy's offer is
# refused at its begin).
NO_ATTEMPT_NEEDED = ("F2-settle", "F3-launcher", "F4-", "F5-")
for _op in ("claim", "transfer", "update", "finalize"):
    CHECKS[("F1-%s-request" % _op, "request")] = f1_request("F1-%s-request" % _op, _op)
    CHECKS[("F1-%s-reply" % _op, "reply")] = f1_reply("F1-%s-reply" % _op, _op)


# ---- F6's bodies and F7's scan -------------------------------------------------------

CONTENT = ["title", "objective", "acceptance_criteria", "non_goals", "blocker", "dependencies", "candidate_refs",
           "evidence_refs", "next_action", "archived"]


def recover_body(verb, hold, task):
    content = {f: task.get(f) for f in CONTENT}
    content["state"] = {"release": "READY", "cancel": "CANCELLED"}[verb]
    h = hold["hold"] if isinstance(hold.get("hold"), dict) else hold
    return {"reservation_id": h["reservation_id"], "fence": h["fence"], "expected_revision": task["revision"],
            "content": content, "reason": "pilot simulation recovery " + verb,
            "evidence": {"kind": "attestation", "attestation_id": "sim-f6-%s-%s" % (verb, task["id"]),
                         "statement": "The pilot simulation recovers this hold to test aicrew's reconciliation."}}


def cancel_body(task):
    body = {f: task.get(f) for f in CONTENT}
    body.update(state="CANCELLED", expected_revision=task["revision"])
    return body


HANDLE = re.compile(r'"handle"\s*:\s*"([^"]+)"')
STORE_FILE = re.compile(r"\.db(-wal|-shm)?$")


def scan(secrets_file, root, store, skip):
    """The known secrets, the scanned sources, and each secret found in each source, by number and source:
    never the secret itself."""
    with open(secrets_file, encoding="utf-8", errors="replace") as f:
        secrets = sorted({l.strip() for l in f if len(l.strip()) >= 8})
    skip = {os.path.abspath(s) for s in skip}
    sources = {}
    for dirpath, dirs, files in os.walk(root):
        dirs[:] = [x for x in dirs if os.path.abspath(os.path.join(dirpath, x)) not in skip]
        for name in files:
            if STORE_FILE.search(name):
                continue  # the store's copies: their audit is read below
            path = os.path.join(dirpath, name)
            with open(path, "rb") as f:
                sources[os.path.relpath(path, root)] = f.read().decode("utf-8", "replace")
    db = sqlite3.connect(os.path.join(store, "aicrew.db"))
    sources["aicrewd audit"] = "\n".join("%s\n%s" % r for r in db.execute("SELECT input, result FROM audit"))
    leaks = ["secret #%d in %s" % (i, src) for src, text in sorted(sources.items())
             for i, s in enumerate(secrets) if s in text]
    return secrets, sources, leaks


# ---- the runner's helpers for the direct steps ------------------------------------------

def step_body(kind, store, task_file, hub, worker):
    """An offer to the worker, or a claim, of the task at its current revision. The process pin is the
    one the cycles' offers carried (aicrewd's store); the step's --repository fills the rest."""
    q = db_of(store)
    pin = q("""SELECT process_repository, process_commit, process_manifest, instruction_digest FROM attempts
               WHERE process_commit != '' ORDER BY created_at DESC LIMIT 1""")
    if not pin:
        raise SystemExit("cases.py: aicrewd's store has no attempt with a process pin")
    with open(task_file) as f:
        t = json.load(f)
    body = {"task": {"hub_id": hub, "project_id": t["project"], "task_id": t["id"]}, "expected_revision": t["revision"],
            "repository": {"access": "write", "branch": "work/" + t["id"]},
            "process": {"repo": pin[0]["process_repository"], "commit": pin[0]["process_commit"],
                        "manifest": pin[0]["process_manifest"]},
            "instruction_digest": pin[0]["instruction_digest"]}
    if kind == "offer":
        body["worker_agent_id"] = worker
        expires = datetime.datetime.now(datetime.timezone.utc) + datetime.timedelta(hours=2)
        body["expires_at"] = expires.strftime("%Y-%m-%dT%H:%M:%SZ")
    return body


def edit_body(task, text):
    body = {f: task.get(f) for f in CONTENT}
    body.update(state=task["state"], expected_revision=task["revision"], next_action=text)
    return body


def open_attempts(store, task, worker, indep):
    """The task's open attempts as "ID STATE HOLDER", HOLDER the role whose capacity it holds."""
    role = {worker: "worker", indep: "indep"}
    return ["%s %s %s" % (r["id"], r["state"], role.get(r["worker_agent_id"], "coordinator"))
            for r in attempts_of(db_of(store), task) if r["state"] != "closed"]


def json_objects(text):
    """The JSON objects in TEXT, one after another (files concatenated)."""
    dec, i, out = json.JSONDecoder(), 0, []
    while True:
        while i < len(text) and text[i].isspace():
            i += 1
        if i >= len(text):
            return out
        try:
            obj, i = dec.raw_decode(text, i)
        except ValueError:
            return out
        out.append(obj)


def lose(facts_file):
    """F4-lost's control: the race's loser answers as a step lost before its commit."""
    with open(facts_file) as f:
        facts = json.load(f)
    for name in ("race-offer", "race-claim"):
        if not committed(*answer_of(facts, name)):
            facts[name] = {"exit": 3, "answer": LOST}
            break
    with open(facts_file, "w") as f:
        json.dump(facts, f, indent=1)


def check(case, point, d):
    errs = []
    expect = lambda ok, what: ok or errs.append(what)
    ids = loadj(d, "ids.json")
    q = db_of(os.path.join(d, "store"))
    # A fault case reads the task's latest attempt: F1's lost claim closed one before it.
    a = latest(q, ids.get("task", "")) if case.startswith("F") else attempt_of(q, ids.get("task", ""))
    if a is None and case != "P3" and not case.startswith(NO_ATTEMPT_NEEDED):
        return ["aicrewd: the task %s has no single attempt" % ids.get("task")]
    CHECKS[(case, point)](d, q, ids, a, expect)
    return errs


def main(argv):
    if len(argv) == 3 and argv[0] == "answer":
        try:
            with open(argv[1]) as f:
                ans = json.load(f)
        except (OSError, ValueError):
            ans = None
        print(json.dumps({"exit": int(argv[2]), "answer": ans}))
        return 0
    if len(argv) == 3 and argv[0] in ("succeeded", "attempt_id"):
        with open(argv[1]) as f:
            code, ans = answer_of(json.load(f), argv[2])
        if argv[0] == "attempt_id":
            print(result_of(ans).get("attempt_id", ""))
            return 0 if result_of(ans).get("attempt_id") else 1
        settles = "settled" in result_of(ans)
        return 0 if code == 0 and ans.get("ok") and (not settles or committed(code, ans)) else 1
    if len(argv) == 3 and argv[0] == "attempt_of":
        a = latest(db_of(argv[1]), argv[2])
        print(a["id"] if a else "")
        return 0 if a else 1
    if len(argv) == 4 and argv[0] == "settled":
        a = latest(db_of(argv[1]), argv[2])
        return 0 if a and a["state"] == argv[3] and a["pending_key"] == "" else 1
    if len(argv) == 6 and argv[0] == "step_body":
        print(json.dumps(step_body(*argv[1:])))
        return 0
    if len(argv) == 3 and argv[0] == "edit_body":
        with open(argv[1]) as t:
            print(json.dumps(edit_body(json.load(t), argv[2])))
        return 0
    if len(argv) == 5 and argv[0] == "open_attempts":
        for line in open_attempts(*argv[1:]):
            print(line)
        return 0
    if len(argv) == 1 and argv[0] == "calls":
        print(json.dumps(["%s %s %s" % (e.get("cmd"), e.get("op"), e.get("exit")) for e in json_objects(sys.stdin.read())]))
        return 0
    if len(argv) == 2 and argv[0] == "pending_key":
        for rec in json_objects(sys.stdin.read()):
            if (rec.get("request") or {}).get("task_id") == argv[1] and rec.get("step"):
                print(rec["step"].get("request_key", ""))
                return 0
        print("cases.py: no recorded step on task %s" % argv[1], file=sys.stderr)
        return 1
    if len(argv) == 2 and argv[0] == "lose":
        lose(argv[1])
        return 0
    if len(argv) == 2 and argv[0] == "pinned":
        return 0 if db_of(argv[1])("SELECT id FROM attempts WHERE process_commit != '' LIMIT 1") else 1
    if len(argv) == 2 and argv[0] == "held":
        return 0 if holder_of(argv[1])[0] == "held" else 1
    if len(argv) == 3 and argv[0] == "recovered":
        errs = []
        q = db_of(os.path.join(argv[1], "store"))
        f6_recovered(argv[2])(argv[1], q, loadj(argv[1], "ids.json"), latest(q, loadj(argv[1], "ids.json")["task"]),
                              lambda ok, what: ok or errs.append(what))
        for e in errs:
            print("cases.py recovered:", e, file=sys.stderr)
        return 1 if errs else 0
    if len(argv) == 4 and argv[0] == "followup":
        q = db_of(argv[1])
        a = attempt_of(q, argv[2])
        told = messages_to(q, argv[3], a["id"] if a else "", argv[2])
        return 0 if any(FOLLOWUP.search(r["text"] or "") for r in told) else 1
    if len(argv) == 4 and argv[0] == "hold":
        rows = db_of(argv[1])("SELECT kind FROM member_holds WHERE agent_id = ?", argv[2])
        return 0 if (rows[0]["kind"] if rows else "none") == argv[3] else 1
    if len(argv) == 4 and argv[0] == "blocked_report":
        q = db_of(argv[1])
        a = attempt_of(q, argv[2])
        return 0 if a and blocked_reports(q, argv[3], a, argv[2]) else 1
    if len(argv) == 4 and argv[0] == "has_op":
        q = db_of(argv[1])
        a = attempt_of(q, argv[2])
        return 0 if a and argv[3] in ops_of(q, a["id"]) else 1
    if len(argv) == 4 and argv[0] == "lost":
        return 0 if lost_in_store(db_of(argv[1]), argv[2], argv[3]) else 1
    if len(argv) == 3 and argv[0] == "logged":
        log = [json.loads(l) for l in sys.stdin if l.strip()]
        return 0 if any(e.get("case") == argv[1] and e["outcome"] == argv[2] for e in log) else 1
    if len(argv) == 4 and argv[0] == "recover_body":
        with open(argv[2]) as h, open(argv[3]) as t:
            print(json.dumps(recover_body(argv[1], json.load(h), json.load(t))))
        return 0
    if len(argv) == 2 and argv[0] == "cancel_body":
        with open(argv[1]) as t:
            print(json.dumps(cancel_body(json.load(t))))
        return 0
    if len(argv) == 1 and argv[0] == "handles":
        for h in HANDLE.findall(sys.stdin.read()):
            print(h)
        return 0
    if len(argv) >= 4 and argv[0] == "scan":
        secrets, sources, leaks = scan(argv[1], argv[2], argv[3], argv[4:])
        print("F7: %d secrets known, %d sources scanned" % (len(secrets), len(sources)))
        if len(secrets) < 18:
            print("F7: the run knows %d secrets, fewer than the 18 it made at least" % len(secrets), file=sys.stderr)
            return 1
        for leak in leaks:
            print("F7: " + leak, file=sys.stderr)
        return 1 if leaks else 0
    if len(argv) != 4 or argv[0] != "check" or (argv[1], argv[2]) not in CHECKS:
        print(__doc__, file=sys.stderr)
        return 2
    errs = check(argv[1], argv[2], argv[3])
    for e in errs:
        print("case %s at %s: %s" % (argv[1], argv[2], e), file=sys.stderr)
    return 1 if errs else 0


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
