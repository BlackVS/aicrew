# Start aicrew development

Assigned session: **Opus 5.5**, standalone. Another session (GPT-6 Sol)
implements aimem prerequisites. The review/monitor session does not implement
either backlog. Do not join the old Pilot team or launch workers.

1. Read AGENTS.md, this file, the local SESSION-STATE.md, BACKLOG-MAP.md and
   the AIForge design with its context and reservation contracts, pinned to
   their reviewed aimem commit:
   <https://github.com/BlackVS/aimem/tree/2b068c1b8f9f499fbae304c3ab6dab0be2dd509d/docs>
   (`DESIGN-AIFORGE.md`, `DESIGN-AIFORGE-CONTEXT.md`,
   `DESIGN-AIFORGE-RESERVATIONS.md`). Aicrew's own contracts are
   `docs/WORKSPACE.md`, `docs/CREW-CONTRACT.md` and
   `docs/ONBOARDING-CONTRACT.md`. Endpoint shapes, wire
   encoding, storage schemas and migration are still open implementation
   reviews; do not infer them. Aimem owns its documents; do not keep copies
   here. Move the pin only to a later reviewed aimem commit.
2. Check Git branch/status/remotes and preserve the operator's LICENSE and
   any uncommitted operator files. Start a feature branch; do not push to main.
3. Run `aimem version`, `aimem task-token show-source` and `aimem process show`
   from this checkout. Confirm project aicrew and complete instructions.
   Verify oh-code-review is actually available to this client.
4. Start/restart the client here so its aimem MCP uses this working directory.
   Verify task tools and read the aicrew board. A configured server is not
   proof that the client has loaded it. Respect normal trust/MCP prompts.
5. Read repository-baseline task 01a0d6d7-1a34-7000-b831-a3f8b823de3c and its
   aimem dependency 01a0d6d7-1936-7000-b31f-b70a5f7e11c7. Preparation can be
   inspected now; do not claim the dependency is satisfied until evidence says
   so. Pick only eligible work, following the process, not list order.

The repository was created by the operator. The baseline task records the
verified visibility, license, CI check (`repo-checks`) and external-review
integration; recheck them rather than assume. Preserve the existing README
and LICENSE unless a scoped reviewed change is needed.

## Coordination with aimem implementation

Keep production code changes in this repository. Request a contract decision
through the owning task/comment with exact requirements and evidence; do not
silently implement a competing endpoint. Dependency IDs can reference aimem
tasks on the same hub. Do not try an admin credential when access is refused.

First pilot stays one coordinator, one worker and one project, including
restart, denial and retry evidence. Independent-worker/multi-project pilots
follow. Web management and AI-skills GitHub publication are later work.

The selected shared process is a transitional reuse of the existing process
set. Keep its review/human-merge rules; its descriptions of old aimem team
commands do not authorize joining a team or define aicrew's new API. Verify
compatibility and propose an aicrew-specific process before runtime changes.
