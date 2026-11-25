"""The upgrade's rerun of join, checked against the home's files.

    rerun.py REPORT BEFORE AFTER

REPORT is the rerun's `aicrew-agent join --json` report. BEFORE and AFTER
list the home's files as `sha256sum` prints them ("DIGEST  ./PATH"), taken
in the member's container just before and just after the rerun.

What the rerun should report is derived from the files, not from which
release the home had: for each change the report lists, `update` (or
`create`, for a file that was not there) where the file before the rerun
differs from what this tree's join wrote, and `unchanged` where it does
not; `restart_required` exactly when at least one differs, else `ready`.
A conflict fails: the simulation's homes hold no edits of their own. So a
previous release whose managed files equal this tree's (no guidance change
since it) passes with nothing updated, and an older one must be updated.

A change's path names a file, or a client entry in one as
"FILE (PARENT.NAME)"; an entry is compared by its file.
"""
import json
import sys

WRITES = ("create", "update")


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


def check(rep, before, after):
    """The failed expectations, and the lines that say what was compared."""
    errs, notes = [], []
    differs = []
    for c in rep.get("changes") or []:
        path, action = c.get("path", ""), c.get("action", "")
        name = path.split(" (", 1)[0]
        if name not in after:
            errs.append("%s: not in the home after the rerun" % path)
            continue
        if name not in before:
            want = "create"
        elif before[name] != after[name]:
            want = "update"
        else:
            want = "unchanged"
        if want != "unchanged":
            differs.append(path)
        if action == "conflict":
            errs.append("%s: a conflict, in a home with no edits of its own" % path)
        elif action in WRITES + ("unchanged",) and action != want:
            errs.append("%s: reported %s, but the file %s" % (
                path, action, {"create": "was not there before", "update": "changed",
                               "unchanged": "did not change"}[want]))
        elif action not in WRITES + ("unchanged",) and want != "unchanged":
            errs.append("%s: reported %s, but the file changed" % (path, action))
    status = rep.get("status")
    want_status = "restart_required" if differs else "ready"
    if status != want_status:
        errs.append("status %s, where %s" % (status, "files changed: " + ", ".join(differs) if differs
                                              else "no file changed: ready"))
    notes.append("the rerun's changes: " + (", ".join(differs) or "none (the home had this tree's files)"))
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
