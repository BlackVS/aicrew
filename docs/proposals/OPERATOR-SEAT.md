# Operator seat: a human member role that receives escalations

Board task 01a0eba9-d5fe (aicrew), with the operator's refinement in comment seq189.
**Status: proposal for review, 2026-09-29.** Decisions D1 to D11 (§8) are open; nothing
here is implemented or authorized by this document.
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
   (existing inbox) and, when the attempt cannot continue, records a work update `block`
   with a blocker (existing `RUNNING → BLOCKED`, hold kept). Workers address only the
   coordinator; aicrew refuses a worker's message to a seat member (`role_forbidden`).
2. **Coordinator decides class and category.** For every question it applies the team's
   escalation policy (§2): answer it, or escalate it. Answering within the frozen scope
   (objective, acceptance criteria, non-goals, recorded decisions on the task and its
   design docs) is the default. The coordinator never guesses at intent: an answer it
   cannot ground in a recorded decision is an escalation, however small.
3. **Coordinator to seat.** An escalation is one *request record* (§3) sent as a directed
   message to the seat and mirrored on the task. While it is open the task is `BLOCKED`
   with the blocker `awaiting operator: <request id>`; the worker's attempt stays blocked
   with its hold and capacity, as today.
4. **Seat answers.** The human produces one *answer record* (§3). A clarification informs;
   an authority answer grants (§4). Aicrew delivers the answer to the coordinator **and**
   to the blocked member, both with delivery and explicit ack, and mirrors it on the task.
5. **Back to work.** The coordinator applies the answer (rework, new offer, stop, or
   simply "continue") and the holder sends the ordinary work update that leaves
   `BLOCKED`. Leaving `BLOCKED` is always a member step with the answer's id as its
   reason; the seat never mutates the task or the reservation.

Simple questions never reach the seat: everything the coordinator can answer from the
record is answered by the coordinator and recorded as a *self-answer* (§3) for later
review. The seat can overturn a self-answer at any time (§3, D7).

**Permission prompts from the launcher.** The probe showed the launcher can see and
answer a client's permission prompt (Claude `can_use_tool`, OpenCode `permission.asked`).
Those prompts are one *input* to this flow, not a separate one: the launcher's adapter
classifies a prompt into a category of §2 (most are `environment` and are decided by
policy without any model), and only a prompt the policy marks `ask` becomes an
escalation request from the coordinator. A launcher never asks the seat directly.

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
escalating: `question`, `category`, `answer`, `grounds` (the recorded decision it relied
on), `policy_revision` and the mode or rule that allowed it. Self-answers are delivered
to the seat as a low-priority digest (one message per attempt milestone in `default` and
`auto`, one per question in `manual`) so the human can review them in the seat's inbox
and on the task.

**Overturn.** The seat answers a self-answer with `supersedes` set. The coordinator must
act on it before any further step on the affected attempt: request rework or a stop. An
overturned self-answer stays in the record; nothing is edited.

**Leaving BLOCKED.** The answer does not change task state. The holder's next work
update (`resume`, or `submit`) names the answer id in its reason, and aicrew refuses a
resume on a `BLOCKED` attempt whose open request has no answer. That keeps the reservation
rules unchanged: only the holder mutates, only over its own connection.

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
| A. WebAuthn in the aimem console | The seat registers a passkey or hardware key with aimem; the console shows the open request; the human confirms; the assertion's challenge is the request digest; aimem verifies and issues the receipt | Strong: needs user presence on the authenticator; the bearer in an agent home cannot sign | New: WebAuthn registration and verification in the hub (a Go library exists), one console page, approvals ledger, read scope. Needs hub TLS, which team mode already requires |
| B. Hardware-key CLI | `aimem approve <request>` on the seat's machine performs a FIDO2 assertion with a key touch, no browser | Strong, same reason | CTAP client code per OS; harder on Windows without admin rights; the machine that runs agents is often the same machine |
| C. Human-only approval token | The hub admin issues the seat a distinct credential type (`aimem_approve_`, user-scoped, operation `approve` only). The human enters it at a hidden prompt per approval, or keeps it on another device; it is never written into an agent home, `hub.json` or a session file | Medium: strong against an agent that only has the installation credential; weak if the token is ever stored on the agent host | Small: one credential type in the access store, one route, the approvals ledger and read scope |
| D. Forge approval | A GitHub approving review or the merge click counts as the authority answer | Covers only `merge`; the forge account is usually logged in on the agent host | None, but it cannot cover the other five floor categories |

**Recommendation.** Target A. Bridge with C for the first increment after the pilot,
under three rules: the token is issued for the seat's user only, it authorizes nothing
but `approve`, and the seat client refuses to store it (hidden prompt, memory only). The
approvals ledger, the receipt format and the read scope are the same for A and C, so the
bridge is not thrown away. D stays a consumer's evidence (verify-delivery's `human_merge`),
never a substitute for the receipt.

**What the receipt binds.** The request digest covers the request record as filed, so an
edited question cannot reuse an approval. A receipt is single-use per request, and a
request whose policy revision has since changed is answered again. Denials are receipts
too, so a "no" is as durable as a "yes".

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

Nothing is added to the coordination wire: escalations never carry a proof reference, and
the reservation is untouched. The team-mode comment write and the approvals ledger are
aimem feature PRs; the role, policy and message kinds are aicrew increments.

## 6. Consumers

**Policy-based auto-merge.** A merge is either inside the merge policy, and then a
delegated merger may perform it, or outside it, and then it is a `merge` escalation to the
seat. A first policy that can stand without a human:

- the diff touches only `docs/**`, `*.md`, `CHANGELOG.md` and nothing else (no code,
  workflows, installers, fixtures or `openapi.json`), and no path a frozen wire names;
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
seat decides, the verifier checks, and one human may hold both roles only through two
memberships. Until the role exists the coordinator confirms, as now.

## 7. Phasing

**Pilot (now).** The human stays the approver through a relay session and pasted
prompts. Two things change in text only: the coordinator files every escalation in the
§3 request shape and every self-answer in the self shape, as task comments written by the
human's relay session in personal mode; and the blocker text names the request id. This
gives the pilot the record and the category statistics the first increment needs.

**Increment 1 (smallest after the pilot).**
- aicrew: role `operator`; the policy record with mode `default` and the floor; message
  kinds `escalation.request/answer/self` with routing and the resume refusal; delivery to
  the seat's inbox through an ordinary member session.
- aimem: the approvals ledger, the `approve` operation and the C-bridge credential; one
  route to issue a receipt for a request digest; the peer read scope for receipts.
- ai-skills: `escalate-or-answer` for the coordinator, and a seat skill that renders the
  inbox, drafts the answer, and asks for the approval token at a hidden prompt.
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
  (b) *Mirror*: the coordinator answers non-floor questions, and each self-answer is
  delivered to the seat at once. Recommendation: (a). It is the Claude Code meaning of
  manual, and (b) is `default` plus a real-time digest, which a rule can express later.
- **D2 System of record first.** (a) aicrew message log as the operational record and
  the task comment written by the seat in personal mode; (b) wait for the team-mode
  comment write before the first increment. Recommendation: (a); the task comment
  becomes coordinator-written when the write lands, with the same schema.
- **D3 Authority proof.** Options A to D in §4. Recommendation: A as the target, C as the
  bridge, D as evidence only. Reject B for the pilot hosts.
- **D4 Answer delivery.** (a) to the coordinator only, who relays; (b) to the coordinator
  and the blocked member. Recommendation: (b); the worker sees the decision unfiltered,
  and the coordinator still owns the next step.
- **D5 What the seat is.** (a) an aicrew member role with a session, joined by
  invitation, answering through its inbox; (b) an out-of-band operator principal on
  `aicrewd` with no session. Recommendation: (a); it reuses identity, sessions and the
  inbox, and the authority proof does not depend on the session either way.
- **D6 No answer in time.** (a) the task stays `BLOCKED` indefinitely and urgency only
  changes notification; (b) a timeout falls back to the coordinator's recommendation for
  clarifications. Recommendation: (a); a timeout never decides, and the coordinator may
  offer other work meanwhile.
- **D7 Overturn effect.** (a) the coordinator must request rework or a stop on the
  affected attempt before any other step; (b) the overturn is advisory. Recommendation: (a).
- **D8 Policy changes.** (a) mode change and any loosening rule need a receipt;
  tightening needs the seat's session; (b) every change needs a receipt. Recommendation:
  (a); tightening is safe and should be cheap.
- **D9 Auto-merge start.** (a) advisory evaluator, human clicks; (b) delegated bot merge
  for docs-only from the start. Recommendation: (a) for the first two months of records,
  then (b) by an explicit delegation receipt per repository.
- **D10 Category list.** Accept the thirteen categories of §2, or name changes. The floor
  is fixed; `scope` and `cross_repo` are `ask` in every mode but their answers may be
  clarifications when no authority is granted.
- **D11 Name.** `operator seat` or `approver` as the role's name in aicrew. Recommendation:
  role id `operator`, displayed as "operator seat"; `approver` describes only the
  authority-answer function.
