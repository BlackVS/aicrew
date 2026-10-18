# aicrew-automation: {"host_api": 1}
# gates-green: the "gates" to "ready" transition of the state machine
# (docs/DESIGN-TURNS.md 4.2). The launcher runs it once per head when the
# local and the external review are READY_FOR_HUMAN_MERGE at the head, its
# CI passed and no external review is in flight (the launcher takes the
# point from the first observation without hands-reviewing; this script
# reads no label): the pull request leaves the draft and the WIP prefix, and
# the attempt's result is submitted. The worker learns of the submit through
# the attempt's lifecycle message; while this automation is in the effective
# set, aicrewd sends no "gates" wake, so the worker does not submit itself.

def main(event):
    a = attempt()
    if a.phase == "submitted":
        step("gates_green", "the attempt is submitted already; nothing to do")
        return None
    p = pr()
    ready()
    submit(p.url)
    return None
