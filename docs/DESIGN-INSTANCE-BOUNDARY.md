# The public/instance boundary design

Status: accepted design (task `01a14de6-5968`; proposal v1 of 2026-10-18,
decided by the operator the same day with one amendment to D1 and four
questions answered). This document is the record of those decisions, under
task `01a15055-d00f`. Nothing in it is implemented unless an increment in
section 6 says so. Written 2026-10-18.

The principle it writes down: **agents receive everything that governs
their behaviour only through the hub, never directly from git and never
from the internet.** The public aiforge repositories on GitHub (aicrew,
aimem, aiskills) hold only what is universal. Everything that describes one
installation stays in that installation, and an installation may run in a
closed network with no internet access, installed and updated by copying
releases into it. Git remains the place where people author and review an
instance's process, skills and automations; the hub's pin is what makes a
version effective.

This document amends:
- [DESIGN-TURNS.md](DESIGN-TURNS.md): 9.2 (where the process manifest
  lives and how the launcher reads it), 13.1 (the precedence rule, now
  also for skills), 10.4 (the base update as a host function);
- [DESIGN-CONTROL-PLANE.md](DESIGN-CONTROL-PLANE.md): section 3 (the
  provider profile gains an endpoint facet; the browser-less login is the
  normal path). Its decision D16 (worker profiles on aicrewd, exportable
  for review) is the precedent this design follows for instance
  configuration.

Where this document and those two disagree, this one wins. Everything else
in them stands. [WORKSPACE.md](WORKSPACE.md)'s rule that nothing falls
back from a refused or missing credential applies unchanged to endpoints.

## 1. The case and the principle

A second hub in a partially isolated network could not use the GitHub
repository the primary hub's process was pinned to. The operator decided
to do nothing by hand there (no mirror, no manual selection) until the
product path works, and stated the principle above. Its consequences,
which the decisions below settle:
- the hub reads and stores the selected set at selection time, an admin's
  action on the hub host, with a digest; members obtain it from the hub
  under their project grant and cache it by digest;
- no repository URL reaches an agent's context: today's session bootstrap
  prints the process set's repository URL and commit, which becomes the
  set's name, commit and manifest only;
- a skills release is imported into the hub and served to members by
  aimem; no installer on a member machine reads GitHub;
- the closed-network transfer is one export from a machine with internet
  and one import on the hub; member machines need the hub only.

## 2. The decisions

### D1. The boundary rule

**Public and universal** (the aiforge repositories on GitHub): code and
releases; the installers; the skills releases; the template processes; the
default automations embedded in the aicrew release; the design documents.

**Instance-specific** (never public, never part of the public aiforge):
the hub's data (projects, grants, members, teams, boards); the selected
process and its stored set; custom automations; worker profiles
(DESIGN-CONTROL-PLANE D16); credentials; project repositories; and the
instance configuration repository (D3).

The rule:
- nothing instance-specific is ever committed to a public repository;
- nothing shipped in a release names an instance (a host, an endpoint, a
  hub name);
- each repository keeps a test that scans its shipped defaults and
  templates for URLs and dotted hostnames. aicrew has it for the default
  scripts since increment 2d-d; aiskills and aimem add the same for their
  templates.

**The amendment (operator):** the instance configuration repository also
holds the instance's custom skills and any modified copy of a public
template. A template process, a default automation or a public skill is
never edited in place in an instance; a change is a copy in the instance
repository. Two consequences:
- **The skills set is layered.** The effective set is the aiskills release
  plus the instance's skills; an instance entry of the same name overrides
  the public one, under the precedence rule DESIGN-TURNS 13.1 gives
  automations: instance above process above release. The process manifest
  pins each skill's name, version and source (`release` or `instance`); the
  hub serves the merged set; `aicrew-agent check` shows each skill's
  source, so an override is visible.
- **A template stays a template.** `aimem process init --from-template
  <name> <dir>` writes a copy for the instance repository; the public
  template is not edited.

### D2. The hub stores and serves every set

A **set** is a versioned bundle: a manifest, its files, and a digest over
them. There are three kinds: the **process set** (handbook, checklists,
templates, the manifest's automation entries and their scripts), the
**skills set** (a skills release, merged with the instance's skills under
D1), and later the **instance automations set**.

One rule for all three:
- the hub **imports** the content when an admin selects it on the hub
  host, stores it bounded by size with its digest, and records the
  selection (repository, commit, manifest, digest) in one compare-and-swap;
- the hub **serves** it to members under their project grant; members
  cache by digest and never fetch git for a set;
- **no repository URL reaches an agent's context.** The session bootstrap
  names the set by its name, commit and manifest; the URL is shown by
  `aimem process show --full` to the admin only;
- **the admin's machine is the only one that reads git or an archive;**
  the hub itself runs no git;
- the hub keeps the sets of the **last five selections**; selecting one of
  them again (with `--expect`) needs no re-import (Q2);
- **import is a CLI action of an admin on the hub host, only.** The API
  lists and reads sets; it never imports (Q3).

Two import sources give the same stored result:
- **a git reference:** URL, full commit, manifest path, fetched by the
  admin's command on the hub host;
- **an archive file:** the aiskills release archive, or an export (D4),
  verified against its `SHA256SUMS`.

The archive form is what makes the smallest closed-network install work
with no forge at all, and it is supported from the start (Q1).

### D3. The instance configuration repository

Where an instance wants git to track and review its own process,
automations, profile exports and custom skills, it keeps a repository on
its own forge, for example `https://forge.example.com/ops/aiforge-instance.git`.
- **The product ships no git server** and does not care which forge it is.
  A bare repository on a host is a devops task; the product documents the
  minimum it needs (an https or ssh clone, read access for the admin's
  import command, and pull requests plus CI where the gates are wanted).
- **The hub pins that repository by commit** as it pins any process, and
  the pin is an import (D2): the forge needs to be reachable from the
  admin's command on the hub host only, not from member machines.
- **It starts as a copy** of a template process from aiskills, written by
  `aimem process init --from-template <name> <dir>`, or the admin imports
  the template archive directly when no repository exists.
- **A change is a pull request** on that repository under the instance's
  own gates; then the admin moves the pin, which imports the new commit.
  A template update is a deliberate pull request, never an automatic pull.
- **Worker profiles** keep D16's export and import; the export files may
  live in the same repository.
- **The repository source of automations stays.** A project's own
  `automations/` at the base branch (DESIGN-TURNS 9.3) is read by the
  launcher from the project's repository, which the member can reach by
  definition; it is the project's layer, beside the instance's.

### D4. Closed-network installation and update

- **`aiforge-export`** on a connected machine (a script in aiskills,
  released and installed with the skills) writes one directory: the release assets of aimem, aicrewd and
  aicrew-agent for the chosen platforms with their `SHA256SUMS`, the
  aiskills release archive with its sums, the template processes, and the
  one-liner installers. The directory is copied into the closed network.
- **Every installer** (aimem's `boot.sh` and `install-hub.sh`, aicrew's
  `boot.sh` and `install-aicrewd.sh`, aiskills' `boot.sh`) accepts a local
  base directory or an internal URL instead of github.com, through
  `AIMEM_BASE`, `AICREW_BASE` and `AI_SKILLS_BASE` (a file path such as
  `/media/aiforge-export` or `https://files.example.com/aiforge`), verifies
  the same `SHA256SUMS`, and prints no GitHub URL in its output.
- **The imports take archives:** `aimem process select --from-archive FILE`
  and `aimem skills import --from-archive FILE` beside the git form.
- **Members install skills from the hub:** `aicrew-agent check` and the
  agent instructions tell a member to run `aimem skills install`, never to
  download from a GitHub URL.
- **Agent client updates** (Claude Code, Codex, OpenCode) stay the
  operator's job, as decided before; `check` reports the client version
  against the profile's supported range, as today.

### D5. The provider endpoint, the browser-less login, the reviewer site

- **The endpoint facet.** The provider profile (DESIGN-CONTROL-PLANE,
  section 3) gains an `endpoint` with three shapes:
  - `vendor` (the default): direct egress to the provider on an
    allow-list, under a subscription or an API key;
  - `gateway`: the base URL of a local LiteLLM-style gateway, in `api_key`
    mode only, since a subscription login does not pass through a gateway;
    the key is the gateway's. The gateway is also one place for the token
    and cost accounting of 3.5;
  - `local`: OpenCode with local models (Ollama, vLLM): a base URL and the
    model names.

  The runner passes the base URL to the client from the profile, through
  the client's own environment or configuration for a base URL. The model
  never sees or sets it. There is no fallback between endpoints
  (WORKSPACE's rule).
- **Subscription login without a browser is the normal path.** The human
  step on the host (DESIGN-CONTROL-PLANE 2.5 and 3.3) uses the client's
  paste-the-URL-and-code flow (Claude Code) or device-code flow (Codex);
  `aicrew member login` wraps it and prints no secret.
- **The external reviewer site** (OpenHands, the `hands` site) is instance
  infrastructure, not a release. Its model endpoint is its own
  configuration (OpenHands takes a base URL and a model), so a closed
  network points it at the gateway or at a local model. The review
  protocol, the labels, does not change. The in-network model is
  documented as an instance requirement, not shipped (Q4).

### D6. What changes in the existing pieces

- **aimem:** task `01a0cf44-c0f4` (the hub-served process bundle, designed
  as portable-team-context increment 1d) is the default path, raised on
  2026-10-18: import at select, store, the read route, the client's cache
  by digest, the bootstrap without a URL, `project show`. Task
  `01a1500b-30eb` (tasks-on names the process gap) is merged. The skills
  set is its own task, `01a1505d-e494` (portable teams 1e).
- **aicrew:** the launcher's `process` source of automations
  (`internal/agent/automationsources.go`) reads the manifest and the
  scripts beside it from the hub-served process set instead of a git clone
  of the process repository. The `repository` source (`automations/` at
  the base branch) stays a git read of the project's own repository. The
  agent instructions for skills (`internal/agent/instructions.go`) point at
  the hub.
- **Increment 2e** (the base-update default, task `01a14478-8366`) is
  re-based on this design: a script never fetches and never holds a
  credential; `update_base()` becomes a host function performed by the
  launcher with the member's credential, as 2d-c's transitions are. 2f
  (`aicrew automation replay`, task `01a14478-8378`) is unchanged.
- **The control plane's A5** (the provider profile, task `01a11eee-d416`)
  gains the endpoint facet (D5).

## 3. The questions, answered

| | Question | Decision |
| --- | --- | --- |
| Q1 | The smallest install with no forge at all, through archive import only | Supported from the start |
| Q2 | Rollback | The hub keeps the sets of the last 5 selections; `aimem process select --expect` back to a kept one needs no re-import |
| Q3 | Who imports | An admin, by CLI, on the hub host only; the API lists and reads, never imports |
| Q4 | The reviewer site's in-network model | Documented as an instance requirement, not shipped |

## 4. Reading the designs together

The one-line pointers in the other documents say where this one changes
them; the substance is here.
- **DESIGN-TURNS 9.2.** The process manifest and the scripts beside it
  live in the instance configuration repository (D3), seeded from an
  aiskills template; a change is a pull request there, not on aiskills.
  The launcher reads them from the hub-served process set (D2), never from
  a git clone.
- **DESIGN-TURNS 13.1.** The precedence by name, process above release,
  gains the instance level above the process once the instance automations
  set exists (13.3 proposed it; D1 decides it), and the same rule orders the
  skills set.
- **DESIGN-TURNS 10.4.** The worked example's fetch, merge, identity and
  push become the host function `update_base()`; the script keeps the
  decision and the turn (D6, 2e).
- **DESIGN-TURNS 13.3.** The hub API's `apply` is an admin's action; the
  import of a set is the same (Q3).
- **DESIGN-CONTROL-PLANE 3.** The provider profile gains the endpoint
  facet; the browser-less login is the normal path (D5).
- **DESIGN-CONTROL-PLANE 2.5 and 5.1.** Unchanged: the runner never
  receives a credential, and the endpoint facet carries a base URL and a
  mode, never a key.

## 5. What the launcher and the hub print

Three texts change under D2 and D4, each so that an agent never sees a
repository URL or a GitHub download instruction:
- the session bootstrap: `Process set: <name> @ <commit> (<manifest>)`,
  with the digest; today it prints the repository URL;
- `aicrew-agent check`: the effective set with each entry's source and
  commit, each skill's source (`release` or `instance`), and the set's
  digest; a missing skill is installed with `aimem skills install`;
- the installers: the configured base and the verified sums, no GitHub
  URL.

## 6. The increments

In the order of the proposal, as filed on the board on 2026-10-18. Sizes
are confirmed at each task's READY assessment.

| # | Repository | Task | Size | Increment | Depends on |
| --- | --- | --- | --- | --- | --- |
| 1 | aimem | `01a0cf44-c0f4` | M | The hub-served process bundle as the default path: git and archive import, store, the read route, the client's cache by digest, the bootstrap text without a URL. | none |
| 2 | aimem | `01a1505d-e494` | S-M | The hub-served skills set: a skills release imported into the hub, `aimem skills install` from the hub, the manifest pins each skill's name, version and source; instance skills override by name; the bootstrap and `aimem skills list` show each skill's source. | 1 |
| 3 | aicrew | `01a15056-bc89` | M | The launcher reads the process set (manifest and scripts) from the hub, not from a git clone; repository automations unchanged; `check` reports the set's digest. | 1, this document |
| 4 | aicrew | `01a15056-bcb3` | S | The aicrew installers accept a local base; `check` and the agent instructions name the hub for skills; no GitHub URL in any printed instruction. | 2, this document |
| 5 | aimem | `01a15057-43ed` | S | The aimem installers accept a local base. | 1 |
| 6 | aiskills | `01a15057-454c` | S | The release archive carries the template processes; the `aiforge-export` script; the no-hostname test for templates. Task `01a14629-3f5f` (templates) carries the copy rule of D1. | this document |
| 7 | aicrew | `01a15056-bcc7` | M | The provider endpoint facet and the browser-less login step (extends A5, `01a11eee-d416`, and the runner). | this document |
| 8 | aicrew | `01a15055-d00f` | S | This document. The design task `01a14de6-5968` closes when it merges. | none |
| 9 | aicrew | `01a14478-8366` | M | 2e re-based: the base update through the `update_base()` host function; then 2f, `01a14478-8378`, unchanged. | this document |

What can start at once: 1 needs nothing; 2 and 5 follow 1; 3, 4, 6, 7 and
9 wait for this document to merge.

## 7. Non-goals

- No product code in this document's pull request.
- No git server inside the hub, and no choice of forge for the instance
  configuration repository.
- No fully offline design for the hub's own storage beyond what exists.
- No decision on worker profiles, which stay on aicrewd under D16.
- No change to increments 2d-a to 2d-d, which do not depend on the source
  of the automations.
- The installer and `check` changes are the filed increments 4, 5 and 6,
  not this document.
- No change to the external review protocol; the reviewer site's model is
  a deployment requirement.
- No in-product update of the agent clients.

## 8. Decisions, in one table

All six were accepted by the operator on 2026-10-18; D1 as amended.

| | Decision | As decided |
| --- | --- | --- |
| D1 | The boundary rule | Public aiforge holds only universal parts; the instance repository holds the instance's process, custom automations, custom skills and every modified copy of a public template; nothing shipped names an instance; each repository tests its shipped templates for URLs and hostnames |
| D2 | Sets | The hub imports a set at selection by an admin on the hub host, from a git reference or an archive, stores it with a digest under one compare-and-swap, serves it under the project grant, members cache by digest; no repository URL in an agent's context; the last five selections kept; import by CLI only |
| D3 | The instance configuration repository | On the instance's own forge, pinned and imported by the hub, reachable from the hub host only; started from an aiskills template by `aimem process init --from-template`; the skills set layered instance above process above release |
| D4 | Closed-network installation | `aiforge-export`, installers with a local base, `--from-archive` imports, no GitHub URL in any instruction; client updates stay the operator's job |
| D5 | Endpoint, login, reviewer | The endpoint facet (`vendor`, `gateway`, `local`); browser-less login as the normal path; the reviewer site as instance infrastructure with an in-network model |
| D6 | Existing pieces | aimem 1d as the default path; the launcher reads the process set from the hub; 2e's `update_base()` host function; A5's endpoint facet |
