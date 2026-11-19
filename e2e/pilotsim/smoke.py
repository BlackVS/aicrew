"""The pilot simulation's smoke assertions on the reports scripts/pilot-sim.sh
keeps (docs/E2E-PILOT-SIM.md, "The smoke check").

  smoke.py check REPORT VERSION [previous]
                                 one member's `aicrew-agent check --json`;
                                 VERSION is the tested ai-skills release.
                                 `previous` is for the upgrade scenario's
                                 previous release, whose check knows only
                                 its own tested set: aimem and ai-skills may
                                 be `newer` there, nothing else is relaxed
  smoke.py status REPORT LABEL…  `aicrew status --team …`: every label listed

Exit 0 when the report holds, 1 with the reasons otherwise.
"""
import json
import sys

def check(report, skills_version, previous=False):
    d = json.load(open(report))
    errs = []
    states = ("supported", "newer") if previous else ("supported",)
    if d.get("status") != "ready":
        errs.append("status %r, not ready" % d.get("status"))
    comps = {c["name"]: c for c in d.get("components", [])}
    if comps.get("aimem", {}).get("state") not in states:
        errs.append("aimem: %r" % comps.get("aimem"))
    skills = comps.get("ai-skills", {})
    if skills.get("state") not in states or skills.get("found") != skills_version \
            or "installed in" not in skills.get("detail", ""):
        errs.append("ai-skills: %r" % skills)
    clients = d.get("clients", [])
    if len(clients) != 1:
        errs.append("clients: %r" % clients)
    for c in clients:
        if c.get("name") != "claude" or c.get("state") != "supported" or c.get("aimem_mcp") != "connected" \
                or c.get("missing_skills"):
            errs.append("client: %r" % c)
    model = d.get("turn_model") or {}
    if model.get("state") != "available":
        errs.append("turn model: %r" % model)
    if not any(f.get("state") == "verified" for f in d.get("forge") or []):
        errs.append("no verified forge credential: %r" % d.get("forge"))
    print("%s: aimem %s %s, ai-skills %s %s (%s)" % (report, comps.get("aimem", {}).get("state"),
          comps.get("aimem", {}).get("found"), skills.get("state"), skills.get("found"),
          "previous release: aimem and ai-skills may be newer" if previous else "strict: supported only"))
    return errs


def status(report, labels):
    teams = json.load(open(report))
    members = {m.get("label"): m for t in teams for m in t.get("members", [])}
    errs = []
    for label in labels:
        m = members.get(label)
        if m is None:
            errs.append("%s is not listed" % label)
        elif not (m.get("session") or {}).get("id"):
            errs.append("%s holds no session: %r" % (label, m))
    return errs


def main(argv):
    if argv[:1] == ["check"] and (len(argv) == 3 or (len(argv) == 4 and argv[3] == "previous")):
        errs = check(argv[1], argv[2], len(argv) == 4)
    elif len(argv) >= 3 and argv[0] == "status":
        errs = status(argv[1], argv[2:])
    else:
        print(__doc__, file=sys.stderr)
        return 2
    for e in errs:
        print("smoke:", e, file=sys.stderr)
    return 1 if errs else 0


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
