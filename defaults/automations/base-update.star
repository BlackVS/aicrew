# aicrew-automation: {"host_api": 1}
# base-update: the "branch behind its base" rows of the state machine
# (docs/DESIGN-TURNS.md 4.2, worked in 10.4). At after-ci, a pull request
# behind its base is brought up to date by the launcher's update_base():
# it merges the base in a worktree of its own and pushes without force
# only when the patch identity is unchanged, so the reviews stand under
# the base-only rule and CI runs on the new head; the launcher posts the
# delta note. A conflict is the worker's: a fix turn carrying the base,
# the files and the hunks. A changed identity is the worker's too: a fix
# turn with the reason, nothing pushed. A push the origin refused (the
# branch moved during the update) needs nothing: the new head is an event
# of its own. Nothing is done while hands-reviewing is present: the
# reviewer refuses a head that moved, so the update follows its result.
# The script runs no git and holds no credential: the launcher does.

def main(event):
    p = pr()
    if not p.behind:
        return None
    if "hands-reviewing" in p.labels:
        step("base_update", "an external review is in progress; the update of this head follows its result")
        return None
    u = update_base()
    if u.conflict:
        return turn("fix", "Updating %s from %s conflicts in %s. Resolve the conflict and push the branch." % (p.branch, p.base, ", ".join(u.conflict.files)), conflict = u.conflict)
    if u.identity_changed:
        return turn("fix", "Updating %s from %s would change the patch identity, so the reviews would not stand: %s" % (p.branch, p.base, u.reason))
    return None
