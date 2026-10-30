"""The cycle's checks after each step (docs/DESIGN-PILOT-SIMULATION.md, section 5).

    cycle.py check STEP DIR [CASE]
    cycle.py state STORE_DIR TASK   (the attempt's STATE/PHASE and pull requests)
    cycle.py pull STORE_DIR TASK    (the number of the attempt's pull request)

DIR is the step's snapshot, which scripts/pilot-sim.sh takes read-only:
  ids.json               the task's ID and the members' agent IDs
  store/aicrew.db        a copy of aicrewd's store (with its -wal file)
  aimem/task.json        the task as aimem's admin read returns it
  aimem/comments.json    its comments
  aimem/history.json     its history (the changes, each with the task)
  aimem/reservation.json its reservation, as the admin's recover status reads it
  forge/pull.json        the attempt's pull request, from the step pull on
  forge/comments.json    its comments
  forge/status.json      the merge commit's combined status, from the step merge on
  verify.json            verify-delivery's answer for the pull request, at the step done
  homes/ROLE/            turns.jsonl, guard.jsonl, turns.json and HANDOFF.md of each home

STEP is one of STEPS. CASE names the pilot case (cases.py) whose cycle the
snapshot is, at the step done only: its own checks are in cases.py, and here
it drops the plain cycle's external RETURN and its rework, and for P4 allows
the block the case caused. Prints each failed expectation and exits 1 when there
is one; the snapshot stays for the report.
"""
import json
import os
import re
import sqlite3
import sys

STEPS = ["offer", "accept", "pull", "rework", "submit", "review", "merge", "done"]

# What each step expects, from its own row on: the attempt's state and
# phase, the step operation it must have, the results and reviews, the
# task's state in aimem and the reservation's holder.
EXPECT = {
    "offer":  dict(attempt=("offered", ""), op="claim", results=0, task="READY", holder="coordinator"),
    "accept": dict(attempt=("running", "working"), op="transfer", results=0, task="READY", holder="worker"),
    "pull":   dict(attempt=("running", "working"), op="transfer", results=0, task="READY", holder="worker"),
    "rework": dict(attempt=("running", "working"), op="transfer", results=0, task="READY", holder="worker"),
    "submit": dict(attempt=("running", "submitted"), op="update", results=1, task="REVIEW", holder="worker"),
    "review": dict(attempt=("running", "accepted"), op="update", results=1, task="REVIEW", holder="worker"),
    "merge":  dict(attempt=("running", "accepted"), op="update", results=1, task="REVIEW", holder="worker"),
    "done":   dict(attempt=("closed", "finalized"), op="finalize", results=1, task="DONE", holder=None),
}

# The operator's own calls in the audit: the runner's setup and its pauses.
# Any other operator operation, or a member's call of one, fails the check.
OPERATOR_OPS = {"control.nudge", "control.pause", "control.resume", "introspection.credential.issue", "invitation.issue",
                "team.create", "team.record_registration"}
REVIEW_HEAD = re.compile(r"^\[(oh-code-review \(high\) review|hands-bot review)\] reviewed at head ([0-9a-f]{40})$", re.M)
VERDICT = re.compile(r"^VERDICT\n(\S+)$", re.M)


def load(d, name, default=None):
    path = os.path.join(d, name)
    if not os.path.exists(path):
        return default
    with open(path, encoding="utf-8") as f:
        return f.read()


def loadj(d, name, default=None):
    raw = load(d, name)
    # The snapshot writes an empty file for one the home does not have.
    return default if raw is None or not raw.strip() else json.loads(raw)


def items(doc, key):
    return doc.get(key, []) if isinstance(doc, dict) else doc


def reviews(comments):
    """The pull request's reviews as (reviewer, head, verdict), oldest first."""
    out = []
    for c in comments:
        body = c.get("body", "")
        m, v = REVIEW_HEAD.search(body), VERDICT.search(body)
        if m:
            out.append(("local" if m.group(1).startswith("oh-") else "external", m.group(2), v.group(1) if v else ""))
    return out


def check(step, d, case=""):
    errs = []
    expect = lambda ok, what: ok or errs.append(what)
    at = STEPS.index(step)
    after = lambda s: at >= STEPS.index(s)
    want = EXPECT[step]
    ids = loadj(d, "ids.json")
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
    expect((a["state"], a["phase"]) == want["attempt"],
           "aicrewd: the attempt is %s/%s, not %s/%s" % ((a["state"], a["phase"]) + want["attempt"]))
    expect(a["worker_agent_id"] == ids["worker"], "aicrewd: the attempt's worker is %s" % a["worker_agent_id"])
    expect(a["branch"] == "work/" + task, "aicrewd: the attempt's branch is %s" % a["branch"])
    if case != "P4":
        expect(a["blocked_reason"] == "", "aicrewd: the attempt was blocked: %s" % a["blocked_reason"])
    steps = q("SELECT operation, outcome, refusal FROM attempt_steps WHERE attempt_id = ? ORDER BY settled_at", a["id"])
    ops = [s["operation"] for s in steps]
    for s in steps:
        expect(s["outcome"] == "committed", "aicrewd: step %s settled %s %s" % (s["operation"], s["outcome"], s["refusal"]))
    expect(want["op"] in ops, "aicrewd: no %s step among %s" % (want["op"], ops))
    expect("release" not in ops, "aicrewd: the reservation was released: %s" % ops)
    expect(len(q("SELECT operation FROM receipts")) > 0, "aicrewd: no receipts")
    results = q("SELECT seq, result_ref FROM attempt_results WHERE attempt_id = ?", a["id"])
    pulls = q("SELECT number, url, head_commit, labels FROM attempt_pulls WHERE attempt_id = ?", a["id"])
    expect(len(results) == want["results"], "aicrewd: %d results, not %d" % (len(results), want["results"]))
    if after("pull"):
        expect(len(pulls) == 1, "aicrewd: %d watched pull requests, not 1" % len(pulls))
    if results and pulls:
        expect(results[-1]["result_ref"] == pulls[0]["url"], "aicrewd: the result %s is not the watched pull request %s" %
               (results[-1]["result_ref"], pulls[0]["url"]))
    if after("review"):
        expect(a["accepted_result"] == 1, "aicrewd: the accepted result is %d, not 1" % a["accepted_result"])
    if after("done"):
        expect(a["finalized_result"] == 1 and a["delivery_result"] == 1,
               "aicrewd: result finalized %d, delivered %d" % (a["finalized_result"], a["delivery_result"]))
    # No hold, no escalation: the cycle has no fault.
    holds = q("SELECT agent_id, kind FROM member_holds")
    expect(not holds, "aicrewd: member holds %s" % [tuple(h) for h in holds])
    escalations = q("SELECT id FROM escalations")
    expect(not escalations, "aicrewd: %d escalations" % len(escalations))
    # Each turn of a member reports its cost: the scripted client costs 0.
    for role in ("coordinator", "worker"):
        if role == "coordinator" or after("accept"):
            reports = q("SELECT cost_usd FROM turn_reports WHERE agent_id = ?", ids[role])
            expect(reports and all(r["cost_usd"] == 0 for r in reports),
                   "aicrewd: the %s's turn reports %s" % (role, [r["cost_usd"] for r in reports]))
    # The audit: the operator's calls are the runner's own, and a member
    # calls only member operations.
    for r in q("SELECT caller_kind, caller_id, operation FROM audit"):
        if r["caller_kind"] == "operator":
            expect(r["operation"] in OPERATOR_OPS, "aicrewd: the audit has the operator call %s" % r["operation"])
        elif r["caller_id"] in (ids["coordinator"], ids["worker"]):
            expect(r["caller_kind"] == "agent" and r["operation"] not in OPERATOR_OPS,
                   "aicrewd: a member called %s as %s" % (r["operation"], r["caller_kind"]))

    # Messages: what each member was sent about this attempt or task. One a
    # turn read is acked in that turn; a paused member's wait.
    for role in ("coordinator", "worker"):
        rows = q("""SELECT m.seq, m.kind, r.first_delivered_at, r.acknowledged_at FROM messages m
                    JOIN message_recipients r ON r.message_id = m.id
                    WHERE r.agent_id = ? AND (m.attempt_id = ? OR m.task_id = ?)""", ids[role], a["id"], task)
        expect(any(r["acknowledged_at"] for r in rows) or (role == "worker" and step == "offer"),
               "aicrewd: no acked message to the %s about the task" % role)
        for r in rows:
            expect(not r["first_delivered_at"] or r["acknowledged_at"],
                   "aicrewd: the %s's message seq %d (%s) was delivered and not acked" % (role, r["seq"], r["kind"]))

    # aimem: the task, its comments, its history and its reservation.
    t = loadj(d, "aimem/task.json")
    expect(t.get("state") == want["task"], "aimem: the task is %s, not %s" % (t.get("state"), want["task"]))
    if after("submit") and pulls:
        refs = [r.get("ref") for r in t.get("candidate_refs") or []]
        expect(pulls[0]["url"] in refs, "aimem: the pull request is not among the candidate refs %s" % refs)
    comments = items(loadj(d, "aimem/comments.json"), "comments")
    expect(any("Triaged READY by the scripted coordinator" in c.get("body", "") for c in comments),
           "aimem: no triage comment")
    states = [(h.get("task") or {}).get("state") for h in items(loadj(d, "aimem/history.json", []), "changes")]
    expect(states and None not in states, "aimem: the task's history has no states: %s" % states)
    expect(not ({"BLOCKED", "CANCELLED"} if case != "P4" else {"CANCELLED"}) & set(states),
           "aimem: the task's history went through %s" % states)
    hold = loadj(d, "aimem/reservation.json")
    if want["holder"]:
        expect(hold.get("state") == "held", "aimem: the reservation is %s, not held" % hold.get("state"))
        expect((hold.get("binding") or {}).get("role") == want["holder"],
               "aimem: the reservation is bound to %s, not the %s" % ((hold.get("binding") or {}).get("role"), want["holder"]))
    else:
        expect(hold.get("state") != "held", "aimem: the reservation is still held after the finalize")

    # The forge: the reviews at each head, the merge and its CI.
    if after("pull"):
        pr = loadj(d, "forge/pull.json", {})
        revs = reviews(loadj(d, "forge/comments.json", []))
        head = (pr.get("head") or {}).get("sha", "")
        external = [r for r in revs if r[0] == "external"]
        if after("rework") and not case:
            expect(external and external[0][2] == "RETURN_TO_IMPLEMENTATION" and external[0][1] != head,
                   "forge: no external RETURN of an older head before the rework: %s (head %s)" % (revs, head))
        if after("submit"):
            expect(("external", head, "READY_FOR_HUMAN_MERGE") in revs, "forge: no external READY at the head %s: %s" % (head, revs))
        if after("review"):
            expect(("local", head, "READY_FOR_HUMAN_MERGE") in revs, "forge: no local READY at the head %s: %s" % (head, revs))
        if after("merge"):
            expect(pr.get("merged") and (pr.get("merged_by") or {}).get("login") == "operator",
                   "forge: the pull request is not merged by operator: merged %s by %s" %
                   (pr.get("merged"), (pr.get("merged_by") or {}).get("login")))
            status = loadj(d, "forge/status.json", {})
            expect(status.get("state") == "success", "forge: the merge commit's CI is %s" % status.get("state"))

    # The delivery record: verify-delivery confirmed, and its evidence is the
    # attempt's terminal evidence and the task's evidence.
    if after("done"):
        v = loadj(d, "verify.json", {})
        evidence = [(e["kind"], e["ref"]) for e in v.get("evidence", [])]
        expect(v.get("verdict") == "confirmed", "verify-delivery: %s" % v.get("verdict"))
        expect(sorted(k for k, _ in evidence) == ["human_merge", "post_merge_ci", "reviewed_head"],
               "verify-delivery: the evidence kinds %s" % [k for k, _ in evidence])
        terminal = [(e["kind"], e["ref"]) for e in json.loads(a["terminal_evidence"] or "[]")]
        delivered = [(e["kind"], e["ref"]) for e in json.loads(a["delivery_evidence"] or "[]")]
        on_task = [(e.get("kind"), e.get("ref")) for e in t.get("evidence_refs") or []]
        for kind, ref in evidence:
            expect((kind, ref) in delivered, "aicrewd: the delivery evidence lacks %s %s" % (kind, ref))
            expect((kind, ref) in terminal, "aicrewd: the terminal evidence lacks %s %s" % (kind, ref))
            expect(ref in [r for _, r in on_task], "aimem: the task's evidence lacks %s %s" % (kind, ref))

    # The homes: every turn finished, none running, the guard silent, the
    # handoffs kept.
    for role in ("coordinator", "worker"):
        turns = [json.loads(l) for l in (load(d, "homes/%s/turns.jsonl" % role, "") or "").splitlines() if l.strip()]
        if role == "coordinator" or after("accept"):
            expect(turns, "%s: no turn recorded" % role)
        for tr in turns:
            expect(tr.get("outcome") == "finished", "%s: turn %s ended %s: %s" %
                   (role, tr.get("id"), tr.get("outcome"), tr.get("error", "")))
        st = loadj(d, "homes/%s/turns.json" % role, {}) or {}
        expect(not st.get("running"), "%s: a turn is running in the snapshot" % role)
        expect(not st.get("hold"), "%s: the home holds its turns: %s" % (role, st.get("hold")))
        guard = load(d, "homes/%s/guard.jsonl" % role, "") or ""
        expect(guard.strip() == "", "%s: the guard logged: %s" % (role, guard.strip()[:300]))
    handoff = load(d, "homes/worker/HANDOFF.md", "") or ""
    if after("accept") and not after("done"):
        expect("## CURRENT ATTEMPT" in handoff and a["id"] in handoff, "worker: no CURRENT ATTEMPT section for the attempt")
    for s, text in (("pull", "Pull request #"), ("rework", "Reworked #"), ("submit", "Submitted #")):
        if after(s) and not (case and s == "rework"):
            expect(text in handoff, "worker: the handoff does not record %r" % text)
    coordinator = load(d, "homes/coordinator/HANDOFF.md", "") or ""
    for s, text in (("review", "Accepted result 1 of attempt " + a["id"]),
                    ("done", "Finalized attempt " + a["id"])):
        if after(s):
            expect(text in coordinator, "coordinator: the handoff does not record %r" % text)
    return errs


def state(store, task):
    """The task's attempt as STATE/PHASE PULLS, or none."""
    db = sqlite3.connect(os.path.join(store, "aicrew.db"))
    rows = db.execute("""SELECT a.state, a.phase, (SELECT COUNT(*) FROM attempt_pulls p WHERE p.attempt_id = a.id)
                         FROM attempts a WHERE a.task_id = ?""", (task,)).fetchall()
    return "%s/%s %d" % rows[0] if len(rows) == 1 else "none"


def pull(store, task):
    db = sqlite3.connect(os.path.join(store, "aicrew.db"))
    rows = db.execute("""SELECT p.number FROM attempt_pulls p JOIN attempts a ON a.id = p.attempt_id
                         WHERE a.task_id = ?""", (task,)).fetchall()
    return rows[0][0] if len(rows) == 1 else None


def main(argv):
    if len(argv) == 3 and argv[0] == "state":
        print(state(argv[1], argv[2]))
        return 0
    if len(argv) == 3 and argv[0] == "pull":
        n = pull(argv[1], argv[2])
        print(n or "")
        return 0 if n else 1
    if len(argv) not in (3, 4) or argv[0] != "check" or argv[1] not in STEPS or \
            (len(argv) == 4 and (argv[1] != "done" or argv[3] not in ("P1", "P2", "P4", "P5"))):
        print(__doc__, file=sys.stderr)
        return 2
    errs = check(argv[1], argv[2], argv[3] if len(argv) == 4 else "")
    for e in errs:
        print("cycle %s: %s" % (argv[1], e), file=sys.stderr)
    return 1 if errs else 0


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
