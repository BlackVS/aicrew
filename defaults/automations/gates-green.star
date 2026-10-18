# aicrew-automation: {"host_api": 1}
# gates-green: the "gates" to "ready" transition of the state machine
# (docs/DESIGN-TURNS.md 4.2). The launcher runs it once per head when the
# local and the external review are READY_FOR_HUMAN_MERGE at the head and
# its CI passed: the pull request leaves the draft and the WIP prefix, and
# the attempt's result is submitted. The worker learns of the submit through
# the attempt's lifecycle message; while this automation is in the effective
# set, aicrewd sends no "gates" wake, so the worker does not submit itself.

def main(event):
    a = attempt()
    if a.phase == "submitted":
        step("gates_green", "the attempt is submitted already; nothing to do")
        return None
    p = pr()
    if "hands-reviewing" in p.labels:
        step("gates_green", "an external review is in progress; nothing to do until its result")
        return None
    ready()
    submit(p.url)
    return None
