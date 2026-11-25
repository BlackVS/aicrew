"""The upgrade's rerun of join, checked against the home's files.

    rerun.py REPORT BEFORE AFTER

REPORT is the rerun's `aicrew-agent join --json` report. BEFORE and AFTER
list the home's files as `sha256sum` prints them ("DIGEST  ./PATH"), taken
in the member's container just before and just after the rerun.

Each rule below restates the product's, and names the file and function it
comes from; when one of those changes, this file changes with it.

- The report's rows. The managed files are the top-level `changes`: the
  plan of managedfiles.Plan (internal/managedfiles/managedfiles.go), put
  into the report by Join (internal/agent/join.go, `Changes: plan`). The
  client wiring is `check.wiring`: checker.wire (internal/agent/check.go)
  appends planWiring's change (internal/agent/wiring.go), and Join reports
  the check as `check` (JoinReport.Check). A wiring row names
  "FILE (PARENT.NAME)" and is compared by its file.
- The actions. managedfiles.Plan gives create, update, unchanged, kept (a
  file that is not managed, the handoff), collision (a command not written)
  and conflict; planWiring gives create, update, unchanged and conflict. A
  conflict fails: the simulation's homes hold no edits of their own. Any
  other action, or none, fails.
- Each row against the files. A managed `create` is a file that was not
  there (managedfiles.Plan, os.ErrNotExist); a wiring `create` adds the
  entry, so its file is new or changed (planWiring). `update` is a file that
  was there and changed. unchanged, kept and collision leave the file as it
  was (managedfiles.Apply writes nothing for them).
- Nothing changes unreported. Every file whose digest differs, or that
  appeared or went, is named by a row with create or update. Two writers
  are not join's guidance and are left out: agent.json, join's own record
  (agentDoc.write in internal/agent/home.go, the `managed` digests), and
  aimem/, the aimem client's state. This rule does not read the report's
  rows to decide which files to look at.
- The status. restart_required exactly when a managed file was replaced, an
  `update` (managedfiles.Apply returns replaced only for update, and Join
  sets JoinRestartRequired on it), or the wiring changed, a wiring create or
  update (checker.wire returns that, the check of an existing home is then
  JoinRestartRequired, and Join carries it over). Otherwise ready: a created
  managed file alone needs no restart.
"""
import json
import sys

FILE_ACTIONS = ("create", "update", "unchanged", "kept", "collision")
WIRING_ACTIONS = ("create", "update", "unchanged")


def guidance(path):
    """Whether a home file is join's to report: not agent.json, not aimem/."""
    return path != "agent.json" and not path.startswith("aimem/")


def digests(text):
    """The listing's digests by path, without the leading "./"."""
    out = {}
    for line in text.splitlines():
        if not line.strip():
            continue
        digest, path = line.split(None, 1)
        path = path.lstrip("*")
        out[path[2:] if path.startswith("./") else path] = digest
    return out


def _row(errs, where, c, allowed, before, after, wiring):
    path, action = c.get("path", ""), c.get("action")
    name = path.split(" (", 1)[0]
    if action == "conflict":
        errs.append("%s %s: a conflict, in a home with no edits of its own" % (where, path))
        return None
    if action not in allowed:
        errs.append("%s %s: action %r is not one join reports there (%s)" % (where, path, action, ", ".join(allowed)))
        return None
    was, now = before.get(name), after.get(name)
    if action == "create":
        ok = now is not None and (was != now if wiring else was is None)
        why = "the entry's file is new or changed" if wiring else "the file was not there before"
    elif action == "update":
        ok = was is not None and now is not None and was != now
        why = "the file was there and changed"
    else:
        ok = was == now
        why = "the file did not change"
    if not ok:
        errs.append("%s %s: reported %s, but not so: %s" % (where, path, action, why))
    return name if action in ("create", "update") else None


def check(rep, before, after):
    """The failed expectations, and the lines that say what was compared."""
    errs, notes = [], []
    written, restart = set(), []
    for c in rep.get("changes") or []:
        name = _row(errs, "changes", c, FILE_ACTIONS, before, after, False)
        if name:
            written.add(name)
            if c.get("action") == "update":
                restart.append(c.get("path"))
    for c in (rep.get("check") or {}).get("wiring") or []:
        name = _row(errs, "check.wiring", c, WIRING_ACTIONS, before, after, True)
        if name:
            written.add(name)
            restart.append(c.get("path"))
    changed = sorted(p for p in set(before) | set(after) if before.get(p) != after.get(p) and guidance(p))
    for p in changed:
        if p not in written:
            errs.append("%s changed, but no row of the report writes it" % p)
    status = rep.get("status")
    want = "restart_required" if restart else "ready"
    if status != want:
        errs.append("status %s, where %s" % (status, "replaced or rewired: " + ", ".join(restart) if restart
                                              else "nothing was replaced or rewired: ready"))
    notes.append("the rerun's changes: " + (", ".join(changed) or "none (the home had this tree's files)"))
    notes.append("the rerun's status: %s (%s)" % (status, rep.get("instruction", "")))
    return errs, notes


def main(argv):
    if len(argv) != 3:
        print(__doc__, file=sys.stderr)
        return 2
    with open(argv[0], encoding="utf-8") as f:
        rep = json.load(f)
    with open(argv[1], encoding="utf-8") as f:
        before = digests(f.read())
    with open(argv[2], encoding="utf-8") as f:
        after = digests(f.read())
    errs, notes = check(rep, before, after)
    for n in notes:
        print("  " + n)
    for e in errs:
        print("rerun: " + e, file=sys.stderr)
    return 1 if errs else 0


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
