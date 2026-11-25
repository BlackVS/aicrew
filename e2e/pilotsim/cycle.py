"""The cycle's checks after each step (docs/DESIGN-PILOT-SIMULATION.md, section 5).

    cycle.py check STEP DIR [CASE]
    cycle.py state STORE_DIR TASK   (the attempt's STATE/PHASE and pull requests)
    cycle.py pull STORE_DIR TASK    (the number of the attempt's pull request)
    cycle.py settled STORE_DIR TASK (exit 0 once every message to a member about the task is acked)

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
  homes/ROLE/            turns.jsonl, guard.jsonl, turns.json, HANDOFF.md and ROLES.md of each home
  homes/worker/check.json the worker's aicrew-agent check --json report

STEP is one of STEPS. CASE names the pilot case (cases.py) whose cycle the
snapshot is, at the step done only: its own checks are in cases.py, and here
it drops the plain cycle's external RETURN and its rework, allows the
operator's nudges, for P4 the block the case caused, and for F1 the attempts
and steps its lost requests left not committed. Prints each failed
expectation and exits 1 when there is one; the snapshot stays for the report.

PILOTSIM_OFFERED_NUDGES is the number of the operator's nudges the run made
outside a case: the below-minimum upgrade run's offered turn, one, before
the second cycle. The audit may hold that many control.nudge calls.

PILOTSIM_BUILD_COMMIT is the commit this tree's members are built from: a
worker home whose aicrew-agent is that build waits on its gates with the
body files in its worktree. Who submits is the home's release's: a worker
whose docs/ROLES.md has the `forge pr review` guidance, with gates-green in
its effective set, leaves the submit to its launcher, but in case F1.
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

# The operator's own calls in the audit: the runner's setup (its toolchain
# selection included) and its pauses,
# and in a case's cycle its nudges and case E1's issue of the architect
# credential. Any other operator operation, or a member's call of one,
# fails the check. The architect's answer is audited under its own caller
# kind.
OPERATOR_OPS = {"control.pause", "control.resume", "introspection.credential.issue", "invitation.issue",
                "team.create", "team.record_registration", "team.set_toolchain", "team.pause", "team.resume"}
# Case E1 issues the architect's credentials and revokes the throwaway one
# (docs/BRING-UP.md step 8) before its raise.
CASE_OPS = {"control.nudge", "architect.credential.issue", "architect.credential.revoke", "escalation.answer"}
# The cases whose cycle cycle.py checks at the step done.
CASES = ("P1", "P2", "P4", "E1", "O1", "P5", "F1", "F2", "F3")
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

    # aicrewd: the task's attempt, its steps, results and pull request. F1's
    # lost claim request closed an attempt before it, not committed.
    attempts = q("SELECT * FROM attempts WHERE task_id = ? ORDER BY created_at, id", task)
    earlier = attempts[:-1]
    expect(len(attempts) == 1 or (case == "F1" and all(e["state"] == "closed" for e in earlier)),
           "aicrewd: %d attempts of the task, not 1" % len(attempts))
    if not attempts:
        return errs
    a = attempts[-1]
    expect((a["state"], a["phase"]) == want["attempt"],
           "aicrewd: the attempt is %s/%s, not %s/%s" % ((a["state"], a["phase"]) + want["attempt"]))
    expect(a["worker_agent_id"] == ids["worker"], "aicrewd: the attempt's worker is %s" % a["worker_agent_id"])
    expect(a["branch"] == "work/" + task, "aicrewd: the attempt's branch is %s" % a["branch"])
    expect(a["blocked_reason"] == "", "aicrewd: the attempt was blocked: %s" % a["blocked_reason"])
    steps = q("SELECT operation, outcome, refusal FROM attempt_steps WHERE attempt_id = ? ORDER BY settled_at", a["id"])
    ops = [s["operation"] for s in steps]
    for s in steps:
        expect(s["outcome"] == "committed" or case == "F1",
               "aicrewd: step %s settled %s %s" % (s["operation"], s["outcome"], s["refusal"]))
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
    # No hold, and no escalation but case E1's one, answered by the
    # architect, and case O1's, answered by the operator: the other cycles
    # have no question to raise.
    holds = q("SELECT agent_id, kind FROM member_holds")
    expect(not holds, "aicrewd: member holds %s" % [tuple(h) for h in holds])
    escalations = q("SELECT id, answered_by FROM escalations WHERE task_id = ?", task)
    if case in ("E1", "O1"):
        answerer = "architect:" if case == "E1" else "operator"
        expect(len(escalations) == 1 and escalations[0]["answered_by"].startswith(answerer),
               "aicrewd: the escalations on the task are %s" % [tuple(e) for e in escalations])
        # The coordinator's mirror of the request, posted once the hold ended.
        if escalations and after("done"):
            bodies = [c.get("body", "") for c in items(loadj(d, "aimem/comments.json", []), "comments")]
            expect(any(b.startswith("[escalation.request %s]" % escalations[0]["id"]) for b in bodies),
                   "aimem: no [escalation.request %s] comment on the task" % escalations[0]["id"])
    else:
        expect(not escalations, "aicrewd: %d escalations on the task" % len(escalations))
    unanswered = q("SELECT id FROM escalations WHERE answered_at = ''")
    expect(not unanswered, "aicrewd: %d open escalations" % len(unanswered))
    # Each turn of a member reports its cost: the scripted client costs 0.
    for role in ("coordinator", "worker"):
        if role == "coordinator" or after("accept"):
            reports = q("SELECT cost_usd FROM turn_reports WHERE agent_id = ?", ids[role])
            expect(reports and all(r["cost_usd"] == 0 for r in reports),
                   "aicrewd: the %s's turn reports %s" % (role, [r["cost_usd"] for r in reports]))
    # The audit: the operator's calls are the runner's own, and a member
    # calls only member operations.
    allowed = OPERATOR_OPS | (CASE_OPS if case else set())
    offered = max(int(os.environ.get("PILOTSIM_OFFERED_NUDGES") or 0), 0)
    for r in q("SELECT caller_kind, caller_id, operation FROM audit"):
        if r["caller_kind"] == "operator" and r["operation"] == "control.nudge" and not case and offered > 0:
            offered -= 1
        elif r["caller_kind"] == "operator":
            expect(r["operation"] in allowed, "aicrewd: the audit has the operator call %s" % r["operation"])
        elif r["caller_id"] in (ids["coordinator"], ids["worker"]):
            expect(r["caller_kind"] == "agent" and r["operation"] not in OPERATOR_OPS | CASE_OPS,
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
    expect(not ({"BLOCKED", "CANCELLED"} if case not in ("P4", "E1", "O1") else {"CANCELLED"}) & set(states),
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
    for s, text in (("pull", "Pull request #"), ("rework", "Reworked #")):
        if after(s) and not (case and s == "rework"):
            expect(text in handoff, "worker: the handoff does not record %r" % text)
    if after("submit"):
        # Who submits comes from the run's facts, not the worker's notes:
        # where the worker home's release has the guidance that leaves the
        # submit to the launcher's gates-green (its docs/ROLES.md names
        # `forge pr review`, from v0.11.4 on), with gates-green in its
        # effective set, the worker posts its review and the launcher
        # submits (01a206f8-24d0, 01a2113d-fa1e), but in case F1
        # (submit_itself). A home of a release before that guidance, or
        # case F1, submits itself. Only the notes of this attempt's pull
        # request count: the handoff's archive keeps earlier attempts' notes.
        n = pulls[0]["number"] if pulls else 0
        launcher, why = launcher_submits(d, case)
        expect(why == "", "worker: who submits is not known: %s" % why)
        own = "Submitted #%d (" % n in handoff
        if launcher:
            expect(launcher_note(handoff, n), "worker: no note of its review of #%d leaving the submit to the "
                   "launcher" % n)
            expect(not own, "worker: it submitted itself where the launcher submits")
            expect(launcher_submitted(d, a["id"]), "worker's launcher: no committed submit of attempt %s among its "
                   "automation records" % a["id"])
        elif why == "":
            expect(own, "worker: the handoff does not record its own submit of #%d" % n)
            expect(not launcher_note(handoff, n), "worker: it left the submit of #%d to the launcher where it "
                   "submits itself" % n)
    if after("pull") and pulls:
        # A worker of this tree's build waits on its gates with the body
        # files the guidance has it post from in its worktree (01a20b66-0f78):
        # no continuation turn finds its pull request waiting on the external
        # review where the launcher's wait applies (the stand-in marks only
        # those). A home of an earlier release counts the files as
        # uncommitted work and does not wait.
        n = pulls[0]["number"]
        this, why = worker_this_build(d)
        expect(why == "", "worker: its build is not known: %s" % why)
        if this:
            expect(not continuation_wait(handoff, n), "worker: a continuation turn found #%d waiting on its gates: "
                   "the turn loop did not wait" % n)
    coordinator = load(d, "homes/coordinator/HANDOFF.md", "") or ""
    for s, text in (("review", "Accepted result 1 of attempt " + a["id"]),
                    ("done", "Attempt %s was finalized" % a["id"])):
        # A home of an earlier release (the upgrade scenario's first cycle)
        # has its coordinator finalize in a turn.
        if s == "done" and "Finalized attempt " + a["id"] in coordinator:
            continue
        if after(s):
            expect(text in coordinator, "coordinator: the handoff does not record %r" % text)
    return errs


# What the stand-in notes where the launcher submits (aicrew-simclient).
LAUNCHER_SUBMITS = "the launcher submits it, since gates-green is in the set"


def worker_report(d):
    """The worker's check report of the attempt's own time and the snapshot
    it was read from, or None and why there is none: the cycle's submit
    snapshot beside d, where the cycle took one (a re-check after an upgrade
    reads the first cycle's attempt, which ran on the earlier release), else
    d's."""
    src = os.path.join(os.path.dirname(os.path.abspath(d)), "submit")
    if not os.path.isfile(os.path.join(src, "homes", "worker", "check.json")):
        src = d
    try:
        rep = loadj(src, "homes/worker/check.json", None)
    except ValueError:
        rep = None
    if not isinstance(rep, dict) or not isinstance(rep.get("aicrew_agent"), dict):
        return None, "no readable check report of the worker in %s" % src
    return rep, src


def worker_this_build(d):
    """Whether the worker's home ran this tree's build (PILOTSIM_BUILD_COMMIT)
    at the attempt's time, and "" or why that is not known."""
    rep, src = worker_report(d)
    if rep is None:
        return False, src
    if not os.environ.get("PILOTSIM_BUILD_COMMIT", ""):
        return False, "PILOTSIM_BUILD_COMMIT is not set"
    commit = rep["aicrew_agent"].get("commit")
    if not commit:
        return False, "the worker's check report in %s names no build commit" % src
    return commit == os.environ["PILOTSIM_BUILD_COMMIT"], ""


# The wording of a home's docs/ROLES.md whose release has `forge pr review`
# and leaves the submit to the launcher's gates-green (#285, #286; v0.11.4
# on). The stand-in tests the same text (aicrew-simclient, reviewGuidance),
# and a test keeps the two equal.
REVIEW_GUIDANCE = "`review --pr N --body-file FILE [--head SHA]` for a review"


def launcher_submits(d, case):
    """Whether the launcher, not the worker, submits the attempt, from the
    run's facts, and "" or why that is not known. The facts are the home's
    own release, read as the stand-in reads them at the attempt's time: its
    docs/ROLES.md has the guidance that leaves the submit to the launcher
    (REVIEW_GUIDANCE), and its check report lists gates-green in its
    effective set, not disabled. Case F1 submits itself. No build commit or
    version is compared, so a release published later changes nothing: a
    home of a release before the guidance lacks its wording, and every
    later one has it."""
    rep, src = worker_report(d)
    if rep is None:
        return False, src
    if case == "F1":
        return False, ""
    roles = load(src, "homes/worker/ROLES.md")
    if not roles:
        return False, "no docs/ROLES.md of the worker in %s" % src
    if REVIEW_GUIDANCE not in roles:
        return False, ""
    if not isinstance(rep.get("automations"), dict):
        return False, "the worker's check report lists no automations (its launcher did not answer)"
    entries = rep["automations"].get("entries") or []
    return any(e.get("name") == "gates-green" and e.get("source") != "disabled" for e in entries), ""


def launcher_note(handoff, number):
    """Whether the worker's handoff notes, for pull request number, that the
    launcher submits it (aicrew-simclient: "Posted the review of #N at
    HEAD; " + LAUNCHER_SUBMITS)."""
    return re.search(r"(?m)^- \S+ Posted the review of #%d at [0-9a-f]+; %s\.$" % (number, re.escape(LAUNCHER_SUBMITS)),
                     handoff) is not None


# What the stand-in appends to its note on the external review in a
# continuation turn where the launcher's wait applies (aicrew-simclient,
# continuationWait, gateWaitHolds).
CONTINUATION_WAIT = " (a continuation turn)"


def continuation_wait(handoff, number):
    """Whether the worker's handoff notes, for pull request number, a
    continuation turn that found it waiting on the external review where the
    launcher's wait applies (aicrew-simclient, followPull)."""
    return re.search(r"(?m)^- \S+ Waiting on the external review of #%d .*%s\.$" %
                     (number, re.escape(CONTINUATION_WAIT)), handoff) is not None


def launcher_submitted(d, attempt):
    """Whether the worker's launcher's records hold gates-green's committed
    submit of attempt (internal/agent/automation.go, submitResult)."""
    for line in (load(d, "homes/worker/automations.jsonl", "") or "").splitlines():
        try:
            r = json.loads(line)
        except ValueError:
            continue
        t = r.get("text", "")
        if r.get("attempt") == attempt and r.get("kind") == "host_submit" and t.startswith("the result ") and \
                t.endswith(" is submitted"):
            return True
    return False


def state(store, task):
    """The task's latest attempt as STATE/PHASE PULLS, or none."""
    db = sqlite3.connect(os.path.join(store, "aicrew.db"))
    rows = db.execute("""SELECT a.state, a.phase, (SELECT COUNT(*) FROM attempt_pulls p WHERE p.attempt_id = a.id)
                         FROM attempts a WHERE a.task_id = ? ORDER BY a.created_at, a.id""", (task,)).fetchall()
    return "%s/%s %d" % rows[-1] if rows else "none"


def pull(store, task):
    """The number of the pull request of the task's latest attempt."""
    db = sqlite3.connect(os.path.join(store, "aicrew.db"))
    rows = db.execute("""SELECT p.number FROM attempt_pulls p JOIN attempts a ON a.id = p.attempt_id
                         WHERE a.task_id = ? ORDER BY a.created_at, a.id""", (task,)).fetchall()
    return rows[-1][0] if rows else None


def settled(store, task):
    """The messages to a member about the task, or one of its attempts, that are not acked yet: a
    message sent at a turn's end (the finalize's lifecycle) wakes the next turn, which acks it."""
    db = sqlite3.connect(os.path.join(store, "aicrew.db"))
    return db.execute("""SELECT m.seq FROM messages m JOIN message_recipients r ON r.message_id = m.id
                         WHERE r.acknowledged_at IS NULL
                           AND (m.task_id = ? OR m.attempt_id IN (SELECT id FROM attempts WHERE task_id = ?))
                         ORDER BY m.seq""", (task, task)).fetchall()


def main(argv):
    if len(argv) == 3 and argv[0] == "state":
        print(state(argv[1], argv[2]))
        return 0
    if len(argv) == 3 and argv[0] == "settled":
        left = settled(argv[1], argv[2])
        if left:
            print("not acked: seq %s" % ", ".join(str(r[0]) for r in left))
        return 1 if left else 0
    if len(argv) == 3 and argv[0] == "pull":
        n = pull(argv[1], argv[2])
        print(n or "")
        return 0 if n else 1
    if len(argv) not in (3, 4) or argv[0] != "check" or argv[1] not in STEPS or \
            (len(argv) == 4 and (argv[1] != "done" or argv[3] not in CASES)):
        print(__doc__, file=sys.stderr)
        return 2
    errs = check(argv[1], argv[2], argv[3] if len(argv) == 4 else "")
    for e in errs:
        print("cycle %s: %s" % (argv[1], e), file=sys.stderr)
    return 1 if errs else 0


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
