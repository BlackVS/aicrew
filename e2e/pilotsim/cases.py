"""The pilot cases' checks (docs/DESIGN-PILOT-SIMULATION.md, section 6).

    cases.py check CASE POINT DIR
    cases.py followup STORE_DIR TASK COORDINATOR   (P1: a message to the coordinator about an unacked one)
    cases.py hold STORE_DIR AGENT KIND             (the member's hold in aicrewd is KIND; "none" for no hold)
    cases.py has_op STORE_DIR TASK OP              (the task's attempt has a committed OP step)

DIR is the snapshot scripts/pilot-sim.sh takes at the case's POINT, laid out
as cycle.py's (its docstring), with ids.json also naming "first_head" for
P5 and the forge's forge/commits.json (the pull request's commits),
forge/first-status.json and forge/head-status.json (the combined status of
the first and the final head), and status.json (`aicrew status --team`) for
P3. Prints each failed expectation and exits 1 when there is one.

A case's checks are what a run without the case's fault fails:
AICREW_SIM_SKIP_FAULT=CASE runs the case without it and expects them to
fail at the point the runner names.
"""
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


def messages_to(q, agent, attempt, task):
    return q("""SELECT m.seq, m.kind, m.text, r.first_delivered_at, r.acknowledged_at FROM messages m
                JOIN message_recipients r ON r.message_id = m.id
                WHERE r.agent_id = ? AND (m.attempt_id = ? OR m.task_id = ?) ORDER BY m.seq""", agent, attempt, task)


def ops_of(q, attempt):
    return [r["operation"] for r in q("SELECT operation FROM attempt_steps WHERE attempt_id = ? AND outcome = 'committed'",
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


def p4_blocked(d, q, ids, a, expect):
    expect((a["state"], a["phase"]) == ("running", "blocked"), "aicrewd: the attempt is %s/%s" % (a["state"], a["phase"]))
    expect("The turn loop stalled this attempt" in a["blocked_reason"],
           "aicrewd: the attempt's blocked reason is %r" % a["blocked_reason"])
    told = [r["text"] for r in messages_to(q, ids["coordinator"], a["id"], ids["task"])]
    expect(any("reported task" in t and "blocked" in t for t in told),
           "aicrewd: the coordinator has no blocked report: %s" % told)
    t = loadj(d, "aimem/task.json", {})
    expect(t.get("state") == "BLOCKED", "aimem: the task is %s, not BLOCKED" % t.get("state"))


def p4_answered(d, q, ids, a, expect):
    expect("answer" in ops_of(q, a["id"]), "aicrewd: no answer step among %s" % ops_of(q, a["id"]))
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
    expect("confirm-delivery" not in ops_of(q, a["id"]), "aicrewd: a confirm-delivery step on a pending CI")
    expect((loadj(d, "forge/status.json", {}) or {}).get("state") == "pending", "forge: the merge commit's CI is not pending")
    t = loadj(d, "aimem/task.json", {})
    expect(t.get("state") == "REVIEW", "aimem: the task is %s, not REVIEW" % t.get("state"))
    expect("verify-delivery: pending for" in (load(d, "homes/coordinator/HANDOFF.md", "") or ""),
           "coordinator: the handoff does not record verify-delivery's pending answer")


CHECKS = {
    ("P1", "unacked"): p1_unacked,
    ("P2", "submit"): p2_submit,
    ("P3", "held"): p3_held,
    ("P4", "blocked"): p4_blocked,
    ("P4", "answered"): p4_answered,
    ("P5", "submit"): p5_submit,
    ("P5", "pending"): p5_pending,
}


def check(case, point, d):
    errs = []
    expect = lambda ok, what: ok or errs.append(what)
    ids = loadj(d, "ids.json")
    q = db_of(os.path.join(d, "store"))
    a = attempt_of(q, ids.get("task", ""))
    if a is None and case != "P3":
        return ["aicrewd: the task %s has no single attempt" % ids.get("task")]
    CHECKS[(case, point)](d, q, ids, a, expect)
    return errs


def main(argv):
    if len(argv) == 4 and argv[0] == "followup":
        q = db_of(argv[1])
        a = attempt_of(q, argv[2])
        told = messages_to(q, argv[3], a["id"] if a else "", argv[2])
        return 0 if any(FOLLOWUP.search(r["text"] or "") for r in told) else 1
    if len(argv) == 4 and argv[0] == "hold":
        rows = db_of(argv[1])("SELECT kind FROM member_holds WHERE agent_id = ?", argv[2])
        return 0 if (rows[0]["kind"] if rows else "none") == argv[3] else 1
    if len(argv) == 4 and argv[0] == "has_op":
        q = db_of(argv[1])
        a = attempt_of(q, argv[2])
        return 0 if a and argv[3] in ops_of(q, a["id"]) else 1
    if len(argv) != 4 or argv[0] != "check" or (argv[1], argv[2]) not in CHECKS:
        print(__doc__, file=sys.stderr)
        return 2
    errs = check(argv[1], argv[2], argv[3])
    for e in errs:
        print("case %s at %s: %s" % (argv[1], argv[2], e), file=sys.stderr)
    return 1 if errs else 0


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
