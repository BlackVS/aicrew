"""The cycle's checks after each step (docs/DESIGN-PILOT-SIMULATION.md, section 5).

    cycle.py check STEP DIR
    cycle.py state STORE_DIR TASK   (the attempt's STATE/PHASE and pull requests)

DIR is the step's snapshot, which scripts/pilot-sim.sh takes read-only:
  ids.json             the task's ID and the members' agent IDs
  store/aicrew.db      a copy of aicrewd's store (with its -wal file)
  aimem/task.json      the task as aimem's admin read returns it
  aimem/comments.json  its comments
  aimem/reservation.json its reservation, as the admin's recover status reads it
  homes/ROLE/          turns.jsonl, guard.jsonl and HANDOFF.md of each home

STEP is offer, accept, pull or submit. Prints each failed expectation and
exits 1 when there is one; the snapshot stays for the report.
"""
import json
import os
import sqlite3
import sys

STEPS = ["offer", "accept", "pull", "submit"]


def load(d, name, default=None):
    path = os.path.join(d, name)
    if not os.path.exists(path):
        return default
    with open(path, encoding="utf-8") as f:
        return f.read()


def check(step, d):
    errs = []
    expect = lambda ok, what: ok or errs.append(what)
    at = STEPS.index(step)
    ids = json.loads(load(d, "ids.json"))
    task = ids["task"]
    db = sqlite3.connect(os.path.join(d, "store", "aicrew.db"))
    db.row_factory = sqlite3.Row
    q = lambda sql, *a: db.execute(sql, a).fetchall()

    # aicrewd: the task's attempt, its steps, results and pull request.
    attempts = q("SELECT * FROM attempts WHERE task_id = ?", task)
    expect(len(attempts) == 1, "aicrewd: %d attempts of the task, not 1" % len(attempts))
    if len(attempts) != 1:
        return errs
    a = attempts[0]
    # The pull request's step may find the attempt submitted already: a fast
    # review lets the worker submit in the turn after it opened it.
    submitted = step == "submit" or (step == "pull" and a["phase"] == "submitted")
    want = [("offered", ""), ("running", "working"), ("running", "submitted" if submitted else "working"), ("running", "submitted")][at]
    expect((a["state"], a["phase"]) == want, "aicrewd: the attempt is %s/%s, not %s/%s" % ((a["state"], a["phase"]) + want))
    expect(a["worker_agent_id"] == ids["worker"], "aicrewd: the attempt's worker is %s" % a["worker_agent_id"])
    expect(a["branch"] == "work/" + task, "aicrewd: the attempt's branch is %s" % a["branch"])
    steps = q("SELECT operation, outcome, refusal FROM attempt_steps WHERE attempt_id = ? ORDER BY settled_at", a["id"])
    ops = [s["operation"] for s in steps]
    for s in steps:
        expect(s["outcome"] == "committed", "aicrewd: step %s settled %s %s" % (s["operation"], s["outcome"], s["refusal"]))
    want_ops = ["claim", "transfer", "transfer", "update"][at]
    expect(want_ops in ops, "aicrewd: no %s step among %s" % (want_ops, ops))
    expect("release" not in ops, "aicrewd: the reservation was released: %s" % ops)
    receipts = q("SELECT operation FROM receipts")
    expect(len(receipts) > 0, "aicrewd: no receipts")
    results = q("SELECT seq, result_ref FROM attempt_results WHERE attempt_id = ?", a["id"])
    pulls = q("SELECT number, url, head_commit, labels FROM attempt_pulls WHERE attempt_id = ?", a["id"])
    if submitted:
        expect(len(results) == 1, "aicrewd: %d results, not 1" % len(results))
        expect(len(pulls) == 1 and results and results[0]["result_ref"] == pulls[0]["url"],
               "aicrewd: the result %s is not the watched pull request %s" %
               ([r["result_ref"] for r in results], [p["url"] for p in pulls]))
        expect(a["blocked_reason"] == "", "aicrewd: the attempt was blocked: %s" % a["blocked_reason"])
    else:
        expect(not results, "aicrewd: a result before the submit: %s" % [r["result_ref"] for r in results])
    if at >= STEPS.index("pull"):
        expect(len(pulls) == 1, "aicrewd: %d watched pull requests, not 1" % len(pulls))

    # Messages: what each member was sent about this attempt or task. One a
    # turn read is acked in that turn; a later one waits for the next turn.
    for role in ("coordinator", "worker"):
        rows = q("""SELECT m.seq, m.kind, r.first_delivered_at, r.acknowledged_at FROM messages m
                    JOIN message_recipients r ON r.message_id = m.id
                    WHERE r.agent_id = ? AND (m.attempt_id = ? OR m.task_id = ?)""", ids[role], a["id"], task)
        if role == "coordinator" and step == "offer":
            expect(any(r["acknowledged_at"] for r in rows), "aicrewd: no acked message to the coordinator about the task")
        if role == "worker":
            # The worker is paused for the offer's checks: its offer waits.
            expect(rows and (step == "offer" or any(r["acknowledged_at"] for r in rows)),
                   "aicrewd: no %smessage to the worker about the attempt" % ("" if step == "offer" else "acked "))
        for r in rows:
            expect(not r["first_delivered_at"] or r["acknowledged_at"],
                   "aicrewd: the %s's message seq %d (%s) was delivered and not acked" % (role, r["seq"], r["kind"]))

    # aimem: the task, its triage comment and its reservation.
    t = json.loads(load(d, "aimem/task.json"))
    # The reservation holds the task while it is worked on; the submit moves
    # it to REVIEW with the pull request among its candidate refs.
    want_state = "REVIEW" if submitted else "READY"
    expect(t.get("state") == want_state, "aimem: the task is %s, not %s" % (t.get("state"), want_state))
    if submitted and pulls:
        refs = [r.get("ref") for r in t.get("candidate_refs") or []]
        expect(pulls[0]["url"] in refs, "aimem: the pull request is not among the candidate refs %s" % refs)
    comments = json.loads(load(d, "aimem/comments.json"))
    comments = comments.get("comments", comments) if isinstance(comments, dict) else comments
    expect(any("Triaged READY by the scripted coordinator" in c.get("body", "") for c in comments),
           "aimem: no triage comment")
    hold = json.loads(load(d, "aimem/reservation.json"))
    holder = "coordinator" if step == "offer" else "worker"
    expect(hold.get("state") == "held", "aimem: the reservation is %s, not held" % hold.get("state"))
    expect((hold.get("binding") or {}).get("role") == holder,
           "aimem: the reservation is bound to %s, not the %s" % ((hold.get("binding") or {}).get("role"), holder))

    # The homes: every turn finished, the guard silent, the handoff kept.
    for role in ("coordinator", "worker"):
        turns = [json.loads(l) for l in (load(d, "homes/%s/turns.jsonl" % role, "") or "").splitlines() if l.strip()]
        if role == "coordinator" or at >= STEPS.index("accept"):
            expect(turns, "%s: no turn recorded" % role)
        for tr in turns:
            expect(tr.get("outcome") == "finished", "%s: turn %s ended %s: %s" %
                   (role, tr.get("id"), tr.get("outcome"), tr.get("error", "")))
        guard = load(d, "homes/%s/guard.jsonl" % role, "") or ""
        expect(guard.strip() == "", "%s: the guard logged: %s" % (role, guard.strip()[:300]))
    handoff = load(d, "homes/worker/HANDOFF.md", "") or ""
    if at >= STEPS.index("accept"):
        expect("## CURRENT ATTEMPT" in handoff and a["id"] in handoff, "worker: no CURRENT ATTEMPT section for the attempt")
    if at >= STEPS.index("pull"):
        expect("Pull request #" in handoff, "worker: the handoff records no pull request")
    if submitted:
        expect("Submitted #" in handoff, "worker: the handoff records no submit")
    return errs


def state(store, task):
    """The task's attempt as STATE/PHASE PULLS, or none."""
    db = sqlite3.connect(os.path.join(store, "aicrew.db"))
    rows = db.execute("""SELECT a.state, a.phase, (SELECT COUNT(*) FROM attempt_pulls p WHERE p.attempt_id = a.id)
                         FROM attempts a WHERE a.task_id = ?""", (task,)).fetchall()
    return "%s/%s %d" % rows[0] if len(rows) == 1 else "none"


def main(argv):
    if len(argv) == 3 and argv[0] == "state":
        print(state(argv[1], argv[2]))
        return 0
    if len(argv) != 3 or argv[0] != "check" or argv[1] not in STEPS:
        print(__doc__, file=sys.stderr)
        return 2
    errs = check(argv[1], argv[2])
    for e in errs:
        print("cycle %s: %s" % (argv[1], e), file=sys.stderr)
    return 1 if errs else 0


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
