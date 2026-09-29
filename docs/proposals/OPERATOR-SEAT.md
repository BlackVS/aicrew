# Operator seat: a human member role that receives escalations

Status: **proposal** (aicrew PR #47, `docs/proposals/OPERATOR-SEAT.md`), amended after
review on 2026-09-29: token outside AI sessions (§4, §7), merge path allowlist (§6),
decision-bound confirmation (§4), floor-first classification (§1, §2), verifier identity
(§6, D12), `resume` with `answer_ref` (§3). Board task 01a0eba9-d5fe, refinement seq189.
**Accepted by the operator on 2026-09-29 with the amendments in §9**, which take precedence
over any earlier section they contradict. Nothing here is implemented yet.
Sources: aimem `DESIGN-AIFORGE*.md` on master plus `DESIGN-AIFORGE-KNOWLEDGE.md` (aimem PR #151, since merged),
aicrew `CREW-CONTRACT.md` and `ONBOARDING-CONTRACT.md` at `main`, `internal/store/confirm.go`,
ai-skills `verify-delivery` at v1.25.0, and the launcher observation probe (task 01a0eb7c).

**Vocabulary.** The *operator seat* (working name; alternative *approver*) is a team member
role held by a human, with its own aicrew session and its own aimem identity, separate from
the coordinator's. It is not the automated supervisor host of `ai-agent-supervisor-vision.md`,
and not the hub admin, although the same person usually holds all three.

## 1. Escalation flow

```
worker ──question/blocker──▶ coordinator ──escalation request──▶ operator seat
worker ◀──answer (relayed)── coordinator ◀──answer (durable)──── operator seat
```

1. **Worker to coordinator.** A worker that cannot proceed sends a directed team message
   and, when the attempt cannot continue, a work update `block` with a blocker (existing
   `RUNNING → BLOCKED`, hold kept). Workers address only the coordinator; aicrew refuses a
   worker's message to a seat member (`role_forbidden`).
2. **Coordinator decides.** It files the question under one category (§2, floor first)
   and applies the policy: answer within the frozen scope (objective, acceptance criteria,
   non-goals, recorded decisions) or escalate. It never guesses at intent: an answer it
   cannot ground in a recorded decision is an escalation, however small.
3. **Coordinator to seat.** One *request record* (§3), a directed message to the seat
   mirrored on the task. While open, the task is `BLOCKED` with blocker
   `awaiting operator: <request id>`; the attempt keeps its hold and capacity.
4. **Seat answers.** One *answer record* (§3): a clarification informs, an authority answer
   grants (§4). Aicrew delivers it to the coordinator **and** the blocked member, each with
   delivery and explicit ack, and mirrors it on the task.
5. **Back to work.** The coordinator applies the answer (continue, rework, new offer or
   stop); the holder sends the `resume` update that leaves `BLOCKED` (§3). The seat never
   mutates the task or the reservation.

Simple questions never reach the seat: everything the coordinator can answer from the
record is answered by the coordinator and recorded as a *self-answer* (§3) for later
review. The seat can overturn a self-answer at any time (§3, D7).

**Permission prompts from the launcher.** The probe showed the launcher can see and
answer a client's permission prompt (Claude `can_use_tool`, OpenCode `permission.asked`).
Those prompts are one *input* to this flow, not a separate one: the launcher's adapter
classifies a prompt into a category of §2 (most are `environment` and are decided by
policy without any model), and only a prompt the policy marks `ask` becomes an
escalation request from the coordinator. A launcher never asks the seat directly.
Classification follows the floor-first rule of §2: a prompt that would deploy, publish,
or widen a permission (a new allow rule, a broader token, a trust dialog, `--dangerously-*`
flags, settings or hook edits) is `deploy` or `security` and always `ask`, whatever the
working directory; a prompt the adapter cannot classify is `ask`.

## 2. Escalation policy: modes, rules and the floor

The policy is one record per team, versioned, held by aicrew: `{mode, rules[], revision,
changed_by, changed_at}`. Every self-answer and every escalation names the revision it
was decided under.

**Categories.** Each question is filed under exactly one category. The list is the
acceptance criterion's, made concrete:

| Category | Examples | Class of answer |
| --- | --- | --- |
| `architecture` | design-authority docs, new components, service boundaries | authority |
| `wire` | wire or contract change, amendment of a frozen wire, new version | authority |
| `security` | trust boundary, credential handling, permission widening, secret storage | authority |
| `risk` | risk acceptance, waiving or downgrading a review finding, skipping a gate | authority |
| `merge` | any merge outside the auto-merge policy (§6) | authority |
| `deploy` | production deploy, release tag, installer change roll-out | authority |
| `scope` | acceptance-criteria or non-goal change, splitting or dropping a deliverable | authority |
| `cross_repo` | a change in one repo that needs a decision in another | authority or clarification |
| `intent` | what the operator meant, priority between two valid readings | clarification |
| `process` | which review level, reviewer profile, test approach within the rules | clarification |
| `implementation` | naming, file layout, library within the allowed set, ordering of steps | clarification |
| `environment` | a client permission prompt: shell command, file write, network, in the worktree | clarification |
| `retry` | re-run a flaky check, retry after a transient failure (never "until green") | clarification |

**Modes.** A mode is the default disposition of every category.

| Category | `manual` | `default` | `auto` |
| --- | --- | --- | --- |
| floor: `architecture wire security risk merge deploy` | ask | ask | ask |
| `scope`, `cross_repo` | ask | ask | ask |
| `intent` | ask | ask | allow (answer from the record; escalate if none) |
| `process`, `implementation`, `retry` | ask | allow | allow |
| `environment` | ask | allow inside the worktree, deny outside | allow inside the worktree, deny outside |

`ask` means the coordinator escalates. `allow` means the coordinator answers, records the
self-answer and continues. `deny` means the coordinator refuses the worker's request and
records that too. **Rules** are per-category overrides `{category, disposition}` on top
of the mode, so the operator can set `retry: ask` in `default` without moving to `manual`.

**The floor.** The six floor categories are `ask` in every mode, and a rule cannot lower
them. Their answers are authority answers (§4). The floor is a constant of the design,
not a row of the policy record, so no policy edit can remove it.

**Classification: the floor wins.** A question or prompt that fits several categories is
filed under the most restrictive one, in this order: floor categories first, then `scope`
and `cross_repo`, then the rest. A classification the coordinator (or the launcher
adapter) cannot make with confidence is `ask`. Deployment and permission-widening prompts
are `deploy` or `security` in every working directory. Decision-table cases:

| Case | Fits | Filed as | Disposition |
| --- | --- | --- | --- |
| Rename a helper inside the worktree | `implementation` | `implementation` | per mode |
| Rename a field of a frozen wire message | `implementation`, `wire` | `wire` | ask, authority |
| Edit `AGENTS.md`, `CLAUDE.md`, a skill file or a fixture | `implementation`, `architecture` | `architecture` | ask, authority |
| Re-run a flaky check on the head | `retry` | `retry` | per mode; never a retry loop |
| Skip a failing check to merge today | `retry`, `risk` | `risk` | ask, authority |
| Shell command `go test ./...` in the worktree, mode `default` or `auto`, no rule | `environment` | `environment` | allow, self-answer recorded |
| The same command, mode `manual` | `environment` | `environment` | ask (manual makes every category ask) |
| The same command, mode `default` with rule `environment: ask` | `environment` | `environment` | ask (the rule tightens the mode) |
| The same command, mode `auto` with rule `environment: deny` | `environment` | `environment` | deny, recorded; the worker is told why |
| Shell command that writes outside the worktree, mode `default` or `auto`, no rule | `environment` | `environment` | deny (the modes table's "deny outside") |
| The same command, any mode, rule `environment: ask` | `environment` | `environment` | ask; a rule can raise deny to ask, never to allow outside the worktree |
| `git push`, a release tag, `boot.sh` or an installer run, anywhere | `environment`, `deploy` | `deploy` | ask, authority |
| Add an allow rule, edit settings or hooks, accept a trust dialog, a `--dangerously-*` flag | `environment`, `security` | `security` | ask, authority |
| A prompt the adapter cannot parse | none | unclassified | ask |

**Changing the policy is a floor action.** A mode change, or a rule that loosens a
disposition (`ask → allow`, `deny → allow`, `deny → ask`), is an authority answer to a
request of category `security` that the seat itself raises. A rule that tightens
(`allow → ask`, `ask → deny`) needs only the seat's session (D8). Every change carries the
new revision; requests already open keep the revision they were filed under.

## 3. Records

Three record kinds, one schema. Each is durable in two places: aicrew's team message log
(delivery and ack, in the transition's transaction) and an aimem task comment (immutable,
sequenced, visible to every reader of the task). The aicrew message is the operational
copy; the task comment is the record of decision. Both carry the same `id`.

**Request** (`escalation.request`), written by the coordinator:

| Field | Content |
| --- | --- |
| `id`, `task`, `attempt`, `policy_revision` | stable ids; the attempt is optional for team-level questions |
| `category`, `class` | from §2; `class` is `clarification` or `authority` |
| `context` | one paragraph and links: task, design doc sections, PR, review comment, the worker's message |
| `question` | one sentence a human can answer without opening the links |
| `options` | two to four, each one line, with the consequence of each |
| `recommendation` | one option and why, in two sentences at most |
| `blocked` | who waits: member, attempt, and what cannot proceed |
| `urgency` | `now` (a running attempt is idle), `today`, `next_session` |

**Answer** (`escalation.answer`), written by the seat:

| Field | Content |
| --- | --- |
| `request`, `decision` | the request id; the chosen option, or free text for `intent` |
| `rationale` | why, and any constraint the decision adds to the frozen scope |
| `decider`, `decided_at` | the seat member's aimem user id and time |
| `class`, `proof` | `clarification` has no proof; `authority` carries the aimem approval receipt id (§4) |
| `supersedes` | set when this answer overturns a self-answer or an earlier answer |

**Self-answer** (`escalation.self`), written by the coordinator when it answers without
escalating: `question`, `category`, `answer`, `grounds` (the recorded decision relied on),
`policy_revision` and the mode or rule that allowed it. Self-answers reach the seat as a
low-priority digest (per attempt milestone in `default` and `auto`, per question in
`manual`) for review in the inbox and on the task.

**Overturn.** The seat answers a self-answer with `supersedes` set. The coordinator must
request rework or a stop on the affected attempt before any other step. Nothing is edited.

**Leaving BLOCKED.** The answer does not change task state. The holder leaves `BLOCKED`
with a work update of intent `resume` (allowed from `Blocked` and `Rework`; `submit` is
allowed only from `Working`, so a result comes in a later `submit`). Today `WorkUpdate`
has `session_id`, `generation`, `intent` and `detail`, and `resume` ignores `detail`. The
increment adds one optional field, `answer_ref`, the answer record's id: required on a
`resume` from `Blocked` when the attempt has an open escalation request, refused
(`answer_missing`) when absent or when the request has no answer, and ignored otherwise.
The reservation rules are unchanged: only the holder mutates, over its own connection.

## 4. Two answer classes and the human-only credential

A **clarification** informs. It may be produced from any session the seat member holds,
including a Claude Code or OpenCode session the human types into.

An **authority answer** grants: it changes what the team may do. It must carry a proof
that no agent session can produce, even one running under the human's own aimem identity
with the human's own installation credential. aimem is the identity root, so the proof is
an aimem-verified **approval receipt** bound to the request: `{receipt_id, request_id,
request_digest, decision, user_id, method, issued_at}`, stored in an aimem approvals ledger
and readable by aicrew through a peer read scope like the reservation read scope. The
coordinator and aicrew accept an authority answer only after reading the receipt from
aimem; the text of the message proves nothing.

Options for how the human produces the receipt:

| Option | How | Strength against an agent holding the human's credential | Cost |
| --- | --- | --- | --- |
| A. WebAuthn in the aimem console | The seat registers a passkey or hardware key with aimem; the console shows the open request with its question, the fixed decision and the policy revision; the human confirms that displayed decision; the assertion's challenge is derived from the request digest, the decision and the policy revision ("What the receipt binds"), never the request digest alone; aimem verifies and issues the receipt | Strong: needs user presence on the authenticator; the bearer in an agent home cannot sign | New: WebAuthn registration and verification in the hub (a Go library exists), one console page, approvals ledger, read scope. Needs hub TLS, which team mode already requires |
| B. Hardware-key CLI | `aimem approve <request>` on the seat's machine performs a FIDO2 assertion with a key touch, no browser | Strong, same reason | CTAP client code per OS; harder on Windows without admin rights; the machine that runs agents is often the same machine |
| C. Human-only approval token | The hub admin issues the seat a distinct credential type (`aimem_approve_`, user-scoped, operation `approve` only). The human types it only into a separate non-AI command, `aimem approve <request>`, which reads it from the TTY (never argv, env, a file or stdin from a pipe) and sends it to the hub. It never enters an AI session, an agent home, `hub.json` or a session file | Medium: strong against an agent that only has the installation credential and against a model transcript; weak if the token is ever stored on the agent host | Small: one credential type in the access store, one route, the approvals ledger and read scope |
| D. Forge approval | A GitHub approving review or the merge click counts as the authority answer | Covers only `merge`; the forge account is usually logged in on the agent host | None, but it cannot cover the other five floor categories |

**Recommendation.** Target A. Bridge with C for the first increment after the pilot,
under three rules: the token is issued for the seat's user only, it authorizes nothing
but `approve`, and only `aimem approve` ever reads it, from the TTY, in memory only. The
approvals ledger, the receipt format and the read scope are the same for A and C, so the
bridge is not thrown away. D stays a consumer's evidence (verify-delivery's `human_merge`),
never a substitute for the receipt.

**What the receipt binds.** The ceremony fixes the decision before the human confirms:
the seat selects an option, the hub records the pending confirmation `{request_digest,
decision, policy_revision}` server-side and derives the challenge from exactly those three
values. The confirmation (a WebAuthn assertion under A, the token-bearing `aimem approve`
call under C) is accepted only if it answers that challenge, and the receipt repeats the
three values. So an approval cannot be replayed for another request, another decision on
the same request, or the same request under a later policy revision. A receipt is
single-use per request, and a request whose policy revision has changed is answered
again. Denials are receipts too, so a "no" is as durable as a "yes".

Test vectors for the ledger: (1) the same request confirmed twice is one receipt; (2) an
edited request record with the original confirmation is refused; (3) **the decision is
changed while the original confirmation is kept and presented for the new decision:
refused, nothing recorded**; (4) the policy revision moves between selection and
confirmation: refused; (5) a receipt read through the peer scope names the confirmed
decision and never the credential or assertion; (6) **the skill selects an option that
differs from the human's intent: `aimem approve` displays the fixed decision before any
confirmation, the human sees the mismatch and declines, and nothing is recorded**; a
variant where the human confirms without reading is out of scope for the software.

**Where the skill stops: what you see is what you approve.** A seat skill in an AI
session may render the inbox, draft the answer, select the option and print the exact
`aimem approve <request>` command. It never asks for, reads or receives the token or the
authenticator. `aimem approve <request>` then runs in the human's own terminal, outside
any model session: it fetches the pending confirmation from the hub and displays the
request's question, the fixed decision and the policy revision as the hub holds them. The
human confirms **that displayed decision** explicitly, by typing the decision back or
answering `y/N` to it, and only then enters the token. The skill's own description of what
it selected is never what the human approves; the hub's display is.

## 5. Reuse and what is new

| Need | Reuse | Genuinely new |
| --- | --- | --- |
| Human identity | The human's existing aimem user, its user-scoped token, the proof flow, one aicrew agent per aimem user | An aicrew member role `operator`; the seat joins like any member, by invitation |
| Seat session | aicrew sessions, generations, the 8-hour session token, resume | Role rules: the seat may receive escalations, answer, request a stop, tighten rules; it never offers, accepts, reviews or confirms delivery |
| Transport | The team message log: directed messages, delivery separate from ack, idempotent send, lifecycle messages | Message kinds `escalation.request/answer/self` with the structured body of §3, and the routing rule (workers cannot address the seat) |
| Blocking | Task `BLOCKED` plus `blocker`; aicrew `RUNNING → BLOCKED` with the hold kept; the resume update | The blocker naming a request id, and the refusal of a resume with an unanswered request |
| Record of decision | aimem task comments (immutable, sequenced); task history | A team-mode comment write in aimem for these records only (today team mode serves reads only; the reservation contract already reserves "comments and evidence additions" for a separate policy) |
| Authority proof | aimem access store, admin CLI, hub TLS, peer credentials with one operation, the read-scope pattern | The approvals ledger, the receipt, the `approve` operation, WebAuthn (target) or the approval credential (bridge), and a peer read scope for aicrew |
| Policy | aicrew audit and receipts; the coordinator's process (ai-skills) | The per-team policy record, the category list, and a coordinator skill `escalate-or-answer` that files requests and self-answers |
| Delivery evidence | `confirm-delivery` and its one predicate (`deliveryConfirmer`); verify-delivery v1.25.0 | Nothing for the pilot; the verifier role changes only the predicate (§6) |

Nothing is added to the coordination wire; the reservation is untouched. The comment write
and the approvals ledger are aimem PRs; role, policy and message kinds are aicrew increments.

## 6. Consumers

**Policy-based auto-merge.** A merge is either inside the merge policy, and then a
delegated merger may perform it, or outside it, and then it is a `merge` escalation to the
seat. A first policy that can stand without a human:

- every changed path matches an **explicit allowlist** kept in the repository's policy
  file, for example `README.md`, `CHANGELOG.md`, `docs/*.md` except the patterns below,
  and `docs/img/**`; anything not listed is outside the policy. The allowlist never
  contains design, contract or wire documents (`docs/DESIGN*.md`, `docs/*CONTRACT*.md`,
  `docs/*-WIRE*.md`, `docs/proposals/**`), fixtures (`docs/fixtures/**`, `testdata/**`),
  agent instructions (`AGENTS.md`, `CLAUDE.md`, `.claude/**`, `.agents/**`), skill files
  (`skills/**`, `SKILL.md`), workflows, installers or `openapi.json`: a change to any of
  those is `architecture` (or `wire`) and escalates, however small the diff;
- the PR title has no `WIP:` and the branch is up to date with the base;
- verify-delivery's `reviewed_head` check passes at the current head for both required
  reviews, with a trusted author list, and the review comments contain no `BLOCKER`;
- CI on the head is green (a `pending` or `known flaky` result is not green);
- the author is a team member acting in a team session, and the policy revision the
  evaluator applied is recorded on the PR.

Everything else escalates. The merge click stays human until the operator gives an
authority answer "delegate merges under policy P revision N in repository R to merger M";
that delegation is itself a `merge`-category receipt, revocable by another. The evaluator
should ship as advisory first: it posts "auto-mergeable under P rN" or the escalation
request, and the human still clicks (D9).

**The verifier role.** `deliveryConfirmer` is one predicate: today the current coordinator
who is not the worker. A verifier role changes it to "a member with role `verifier`, who
is neither the worker nor the reviewing coordinator". The seat is not the verifier: the
seat decides, the verifier checks. One person cannot hold both through two memberships:
the onboarding contract refuses a second role for an agent already active in the team
(`role_conflict`) and links one aimem user to exactly one aicrew agent. So either the
verifier is a **separate identity** (its own aimem user and agent, for example a
dedicated verifier installation the same person operates), or the crew contract takes a
new decision: a membership may carry a role set, with the predicates above evaluated per
role. This design assumes the separate identity (D12). Until the role exists the
coordinator confirms, as now.

## 7. Phasing

**Pilot (now).** The human stays the approver through a relay session and pasted
prompts. In text only: escalations and self-answers use the §3 shapes, as task comments
written by the human's relay session in personal mode, and the blocker names the request
id. That gives the first increment its record and its category statistics.

**Increment 1 (smallest after the pilot).**
- aicrew: role `operator`; the policy record with mode `default` and the floor; message
  kinds `escalation.request/answer/self` with routing and the resume refusal; delivery to
  the seat's inbox through an ordinary member session.
- aimem: the approvals ledger, the `approve` operation and the C-bridge credential; one
  route to issue a receipt for a request digest; the peer read scope for receipts.
- ai-skills: `escalate-or-answer` for the coordinator, and a seat skill that renders the
  inbox, drafts the answer and prints the `aimem approve <request>` command. The skill
  never receives the approval token: the human runs that command in a plain terminal,
  where the token is read from the TTY and sent to the hub.
- aicrew: the `answer_ref` field on `WorkUpdate` and the `answer_missing` refusal (§3).
- Acceptance: one worker question answered by the coordinator and visible to the seat as
  a self-answer; one `wire` question escalated, answered with a receipt, delivered to
  both members, the task leaving `BLOCKED` on the holder's resume; a forged answer text
  without a receipt refused by the coordinator skill and by aicrew.

**Can wait.** The team-mode comment write (until then the seat mirrors records to the
task in personal mode); WebAuthn in the console; the auto-merge evaluator beyond advisory;
the verifier role; per-category urgency routing and notifications; `manual` mode
mirroring in real time.

## 8. Decisions for the operator

Each has options and a recommendation; the design above assumes the recommendation.

- **D1 Manual mode.** (a) *Hold*: every question escalates and the coordinator waits.
  (b) *Mirror*: the coordinator answers non-floor questions and each self-answer reaches
  the seat at once. Recommendation: (a), the Claude Code meaning; (b) is `default` plus a
  real-time digest, expressible by a rule later.
- **D2 System of record first.** (a) aicrew message log plus a task comment the seat
  writes in personal mode; (b) wait for the team-mode comment write. Recommendation: (a);
  the comment becomes coordinator-written, same schema, when the write lands.
- **D3 Authority proof.** Options A to D in §4. Recommendation: A as the target, C as the
  bridge, D as evidence only. Reject B for the pilot hosts.
- **D4 Answer delivery.** (a) to the coordinator only, who relays; (b) to the coordinator
  and the blocked member. Recommendation: (b); the worker sees the decision unfiltered,
  and the coordinator still owns the next step.
- **D5 What the seat is.** (a) an aicrew member role with a session, joined by invitation,
  answering through its inbox; (b) an out-of-band operator principal on `aicrewd`.
  Recommendation: (a); the authority proof does not depend on the session either way.
- **D6 No answer in time.** (a) the task stays `BLOCKED` and urgency only changes
  notification; (b) a timeout falls back to the coordinator's recommendation for
  clarifications. Recommendation: (a); a timeout never decides.
- **D7 Overturn effect.** (a) the coordinator must request rework or a stop on the
  affected attempt before any other step; (b) the overturn is advisory. Recommendation: (a).
- **D8 Policy changes.** (a) mode change and any loosening rule need a receipt;
  tightening needs the seat's session; (b) every change needs a receipt. Recommendation:
  (a); tightening is safe and should be cheap.
- **D9 Auto-merge start.** (a) advisory evaluator, human clicks; (b) delegated bot merge
  for the allowlist from the start. Recommendation: (a) for two months of records, then
  (b) by an explicit delegation receipt per repository.
- **D10 Category list.** Accept the thirteen categories of §2, or name changes. The floor
  is fixed; `scope` and `cross_repo` are `ask` in every mode but their answers may be
  clarifications when no authority is granted.
- **D11 Name.** `operator seat` or `approver` as the role's name in aicrew. Recommendation:
  role id `operator`, displayed as "operator seat"; `approver` describes only the
  authority-answer function.
- **D12 Verifier identity.** (a) the verifier is a separate aimem user and aicrew agent,
  even when the same person operates it; (b) amend the crew and onboarding contracts so a
  membership may carry a role set, and evaluate `deliveryConfirmer` and the seat rules per
  role. Recommendation: (a); it needs no contract change and keeps "one user, one agent".
  Choose (b) only if running a second identity per person proves impractical in the pilot.

## 9. Operator decisions (2026-09-29)

The operator decided D1 to D12. Where a decision below differs from §1 to §8, this section
wins; the first implementation increment brings the earlier sections in line.

- **D1 Manual mode: (a) hold.** Every question escalates and the coordinator waits.
- **D2 System of record: (a).** The aicrew message log is the operational record now; the
  task comment is written by the seat's own client in personal mode until the team-mode
  comment write exists, then by the coordinator with the same schema.
- **D3 Authority proof: amended.** The seat's client is **not an AI session** (see D5), so
  the prompt-injection risk that motivated per-decision confirmation does not arise inside
  it. Instead:
  - the seat session is **unlocked once** at start with a second factor no agent holds, and
    lives for a bounded time (for example 8 hours); its credential is held only in the
    client's memory, never in an agent home, a config file or `hub.json`;
  - within an unlocked session **every answer counts as the operator's**, authority answers
    included, with no further confirmation, except switching a team's policy to `auto`,
    which needs a fresh second-factor confirmation (D8);
  - the second factor is **not tied to hardware keys**: any WebAuthn/passkey authenticator
    (Windows Hello, Touch ID or Face ID, a phone passkey, a security key) and TOTP from an
    authenticator app as a fallback. The confirmation always happens outside any AI
    session, in the seat client or the console;
  - the receipt still binds `{request_digest, decision, policy_revision}` (§4) and records
    the unlocked seat session that produced it; forge approval stays evidence only.
- **D4 Answer delivery: (b), made general.** An answer always reaches the coordinator, plus
  whoever the request's `blocked` field names. A worker's question relayed by the
  coordinator therefore reaches both; the coordinator's own question reaches the
  coordinator, and the holder too when it concerns an attempt in progress.
- **D5 What the seat is: (a), with a non-AI client.** An aicrew member with role `operator`,
  its own session, joined by invitation. The first client is a **terminal UI without a
  model**: it lists open requests, shows question, options and recommendation, and sends
  the operator's choice or text. It offers a **chat with the coordinator** per request, so
  the operator can ask about context or options; the chat is attached to the decision
  record, and the decision is still entered by the operator in the client. The "seat skill"
  of §4 and §7 is dropped. A web or mobile client is a later direction that needs its own
  security design (passkeys make it feasible).
- **D6 No answer in time: (a).** The task stays `BLOCKED`; a timeout never decides, and the
  coordinator may offer other work meanwhile.
- **D7 Overturn effect: (a), with checkpoints.** Before a member acts on any answer or
  self-answer, it commits its work in progress and pushes its branch, and the record
  carries `checkpoint {repo, branch, commit}`. An overturn obliges the coordinator, before
  any other step on the attempt, to either restart from the checkpoint on a new branch
  (the later work stays on the old branch; nothing is deleted or force-pushed) or fix
  forward, and to record which and why. A blocked request takes its checkpoint when the
  attempt blocks.
- **D8 Policy changes: (a).** Tightening needs only the seat session; loosening needs the
  seat session, and switching to `auto` additionally needs a fresh second-factor
  confirmation.
- **D9 Auto-merge: (a).** Advisory first; a delegated bot merge only by an explicit
  per-repository decision with a fresh second-factor confirmation, after records show the
  advisory verdicts matching the operator's; the explicit path allowlist of §6 applies.
- **D10 Categories: accepted, with one amendment.** Splitting a task while every acceptance
  criterion of the parent maps to a child is `process` and belongs to the coordinator
  (recorded as a self-answer). `scope` covers only changing the objective, acceptance
  criteria or non-goals, adding new scope, or deferring part of it; a split that drops or
  defers a criterion is a scope change.
- **D11 Name: accepted.** Role id `operator`, displayed as "operator seat".
- **D12 Verifier identity: (a).** The verifier is a separate aimem user and aicrew agent
  running the `verify-delivery` skill, relieving the coordinator of delivery checks; it is
  never the attempt's worker nor its reviewing coordinator.
