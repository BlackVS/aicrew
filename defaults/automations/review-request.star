# aicrew-automation: {"host_api": 1}
# review-request: the external review is requested once per head
# (docs/DESIGN-TURNS.md 4.2, "self-review" to "external-review"). At
# after-push for a head that is not a base-only update, the stale
# hands-reviewed label goes and review-this is added. Nothing is done for a
# base-only update (the reviews stand by the patch identity), while
# hands-reviewing is present (the reviewer refuses a head that moved; the
# request follows its result), while review-this is present (the request
# is in flight), or when the external review at this head exists already.
# A label read that comes back empty right after an add is the reviewer
# switching labels, never a reason to add again: this script adds at most
# once, and never reads the labels back.

def main(event):
    if event.base_only:
        return None
    p = pr()
    if "hands-reviewing" in p.labels:
        step("review_request", "an external review is in progress; the request for this head follows its result")
        return None
    if "review-this" in p.labels:
        return None
    for r in p.reviews:
        if r.reviewer == "external" and r.head and (p.head.startswith(r.head) or r.head.startswith(p.head)):
            return None
    if "hands-reviewed" in p.labels:
        label(add=["review-this"], remove=["hands-reviewed"])
    else:
        label(add=["review-this"])
    return None
