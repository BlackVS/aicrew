# aicrew-automation: {"host_api": 1}
# base-update: the "branch behind its base" rows of the state machine
# (docs/DESIGN-TURNS.md 4.2, worked in 10.4). A pull request behind its
# base is brought up to date by the launcher's update_base(): it fetches,
# merges the base in a worktree of its own and pushes without force only
# when the patch identity is unchanged, so the reviews stand under the
# base-only rule and CI runs on the new head; the launcher posts the delta
# note. A conflict is the worker's: a fix turn carrying the base, the
# files and the hunks. A changed identity is the worker's too: a fix turn
# with the reason, nothing pushed. A push the origin refused (the branch
# moved during the update) needs nothing: the new head is an event of its
# own. Nothing is done while hands-reviewing is present: the reviewer
# refuses a head that moved, so the update follows its result.
#
# When: at after-ci, and on forge.changed when the change removed
# hands-reviewing, the label is not back and every check at the head has
# concluded (with a check still running, the coming after-ci does it);
# any other change is answered before the pull request is read.
# update_base() is called without reading pr().behind first: its own
# answer is the fresh reading of the base (one fetch, no worktree when
# the branch is up to date), while pr().behind is the base as the
# launcher last fetched it. A call that reports an updated branch with a
# reason completed an update a launcher's exit cut after its push, or
# could not post its note: it is followed by one call more, so a base
# that moved on in between is merged; a branch up to date costs that
# call nothing more than the fetch. The script runs no git and holds no
# credential: the launcher does.

def main(event):
    if "forge.changed" in event.kinds and "hands-reviewing" not in event.change.labels_removed:
        return None
    p = pr()
    if "forge.changed" in event.kinds:
        for c in p.checks:
            if c.conclusion != "success" and c.conclusion != "failure":
                step("base_update", "the external review ended while %s is still running; the update of this head follows CI" % c.name)
                return None
    if "hands-reviewing" in p.labels:
        step("base_update", "an external review is in progress; the update of this head follows its result")
        return None
    u = update_base()
    if u.updated and u.reason:
        u = update_base()
    if u.conflict:
        return turn("fix", "Updating %s from %s conflicts in %s. Resolve the conflict and push the branch." % (p.branch, p.base, ", ".join(u.conflict.files)), conflict = u.conflict)
    if u.identity_changed:
        return turn("fix", "Updating %s from %s would change the patch identity, so the reviews would not stand: %s" % (p.branch, p.base, u.reason))
    return None
