# Aicrew agent instructions

## Start here

Read `docs/START-HERE.md`, `docs/SESSION-STATE.md` if present, and the
pinned AIForge design linked from START-HERE at session start and after
compaction. Verify Git and live task evidence rather than trusting the
handoff. Use the aimem project `aicrew`; this is a standalone implementation
session, not a Pilot member.

The task board is authoritative for live task state. `docs/BACKLOG-MAP.md`
contains IDs and ownership, not a second status list. Read the selected process
and required skills. Do not join/resume a team or claim work on the aimem board.
Cross-project dependencies must be checked by ID; unknown is not DONE.

## Ownership and boundaries

The assigned implementation session owns aicrew changes. The separate aimem
session owns aimem prerequisites. The monitoring session reviews and watches
integration; it is not another implementation worker. Human merges remain
human merges. Parallel work across the two repositories is allowed; within
this repository use one work PR at a time, targeting `main`.

Aimem owns knowledge, tasks, actors and access profiles. Aicrew owns teams,
membership, roles, attempts and delivery. Use direct aimem interfaces, not a
duplicate knowledge API/backlog. Never bypass a prerequisite by embedding an
unreviewed authentication or task-ownership contract. Mocks may prove a draft
but do not establish the real integration contract.

Keep the existing LICENSE (PolyForm Noncommercial 1.0.0). Aimem uses the
same license and licensor; source copied from aimem must keep aimem's
`Required Notice:` line beside aicrew's in LICENSE. Check the terms of any
other source before importing it.

Go is the approved build language. No Go module exists yet, and no production
architecture is selected. `scripts/check-repo.sh` is the only check today;
run it before every push. CI runs the same script. Add Go build/vet/test to
CI in the PR that lands the first runnable code, not before.

## Files, credentials and handoff

STRICT: `.creds/` contains credentials only. Never put scripts, logs, notes,
binaries, source checkouts or backups there. Temporary/private artifacts go
in gitignored `.work/`; reusable reviewed scripts go in `scripts/`.
Never print secrets, embed them in config/commands, commit local bindings or
fall back from a refused ordinary credential to an admin/checkpoint token.
Provider authentication stays in client-supported storage.

The future product's agent-home layout is a design requirement, not something
to install over this development checkout. `repos/`, `worktrees/`, credential
naming and managed initial docs are covered by the workspace contract task.

`docs/SESSION-STATE.md` is a local, gitignored single-writer handoff, at most
50 lines: owner/date, objective, verified milestones, next, one-line pickup.
Read it after compaction. Take ownership explicitly when starting as the
assigned implementer; do not overwrite another active session's handoff.
Before stopping, verify first and then record commands/results, limitations
and exact next steps. Keep shared design decisions in reviewed docs and task
comments, not only this host-local file.

## Delivery

Assess READY criteria before claiming. Split L/XL work and reassess provisional
M tasks if scope grew. Commit only intentional files on a feature branch;
preserve operator/unrelated work. No release, deployment or token/membership
changes merely because a task was merged. Use commit/body files for multiline
Git metadata on Windows, with UTF-8 and no BOM.

Use medium pre-push review and high final-head review; docs-only changes also
get high pre-merge review. Request external review with plain `review-this`,
watch and read the result, and never push while `hands-reviewing` is present.
Check whether the external reviewer is installed on this new repository;
a label alone does not prove it is. Report any missing gate instead of
claiming a review. The detailed shared gates follow.

## Review gates

Use the installed oh-code-review skill and read a repository-specific
custom-codereview-guide if present. Pre-push: medium inline, no subagents,
against the final pending changes. Pre-merge: high, including docs-only PRs,
with verified findings, risk and verdict posted on the actual final head.
After source changes, high delta review and renewed external review are
required. Max/ultra only on explicit operator request.

Only concrete in-scope BLOCKER findings return a PR to implementation.
FOLLOW_UP findings become separate tasks, not scope expansion. Runtime
assumptions require a safe reproducer before production hardening. Never
claim CI, review or delivery without evidence for that head.

Use plain review-this; never select a reviewer model unless the operator
requests it. Watch the result and read the hands-bot comment at the named
head. hands-reviewed is not a verdict. Never push while hands-reviewing;
after a new source head, clear stale hands-reviewed and re-request review.
Do not hand-edit hands-reviewing or claim the reviewer runs merely because
a label exists. No automatic human merge, release or deployment.

PR text and reviews must omit private hosts, hub names, bindings, credential
names/paths and session URLs, even in quoted logs. At most one line of tool
attribution in reviews. Freeze scope/acceptance/non-goals and review that
scope. Write review bodies to UTF-8 files before posting.
