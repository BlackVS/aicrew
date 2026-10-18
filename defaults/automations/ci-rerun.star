# aicrew-automation: {"host_api": 1}
# ci-rerun: the "gates" row of the state machine for a CI failure that is
# the runner's, not the change's (docs/DESIGN-TURNS.md 4.2): the failed jobs
# are re-run once per head, a count rerun() keeps. A failure of the change,
# and a second runner failure at the head, reach the worker through the CI
# need as before.

def main(event):
    p = pr()
    runner = [c.name for c in p.checks if c.runner_failure]
    if not runner:
        return None
    r = rerun()
    if r.reran:
        step("ci_rerun", "runner failure of %s: re-ran %s" % (", ".join(runner), ", ".join(r.jobs)))
    else:
        step("ci_rerun", "runner failure of %s: not re-run: %s" % (", ".join(runner), r.reason))
    return None
