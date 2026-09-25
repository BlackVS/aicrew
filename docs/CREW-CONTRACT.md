# Aicrew membership, identity link and execution-store contract

Status: proposed design for review (task crew-contract). Nothing here is
implemented; it fixes semantics for the implementation tasks listed at the
end. Endpoint shapes, wire encoding, storage schema, service authentication
and migration remain separate implementation reviews. Updated 2026-09-25.

Parent contracts, pinned:

- [AIForge split](https://github.com/BlackVS/aimem/blob/2b068c1b8f9f499fbae304c3ab6dab0be2dd509d/docs/DESIGN-AIFORGE.md)
- [Verified identity and session context](https://github.com/BlackVS/aimem/blob/2b068c1b8f9f499fbae304c3ab6dab0be2dd509d/docs/DESIGN-AIFORGE-CONTEXT.md)
  (the "context contract")
- [Task reservation and dependencies](https://github.com/BlackVS/aimem/blob/2b068c1b8f9f499fbae304c3ab6dab0be2dd509d/docs/DESIGN-AIFORGE-RESERVATIONS.md)
  (the "reservation contract")
- [Agent workspace convention](WORKSPACE.md)

If this document and a parent contract disagree, the parent wins and this
document is wrong.

## What aicrew stores and what it never stores

Aicrew's store is the authority for agents and their verified identity links,
teams, memberships, roles, sessions and generations, execution capacity,
offers and attempts, process pins recorded on attempts, the team inbox, audit
and its own request receipts.

Aicrew never stores or decides: task content, task state, dependencies or
history (aimem task authority); the task reservation (aimem, reservation
contract); resource grants or knowledge (aimem, context contract); any
credential secret (workspace convention). Where an aicrew record needs one of
these, it holds a stable reference and, at most, the last value it observed,
marked as observed. An observed value never authorizes anything.

## Identities and readable names

| Record | Stable key | Readable label |
| --- | --- | --- |
| Agent | aicrew agent ID, issued by aicrew | agent label (workspace convention) |
| Linked actor | (hub ID, aimem user ID), verified by the context contract's one-time proof | aimem display name, as observed |
| Team | (aicrew service ID, team ID) | team name |
| Team project | (hub ID, stable aimem project ID) | project name, as observed |
| Task reference | (hub ID, aimem task ID) | task title, as observed |
| Session | (aicrew service ID, session ID, generation), per the context contract | none |

- An agent has at most one linked actor. Linking follows the context
  contract's proof flow and binds by (hub ID, user ID), never by name, URL,
  repository or model. Re-linking an agent to a different user is an
  operator-authorized rebind that first reconciles active work.
- Labels may change and may repeat. Any lookup by label must resolve to
  exactly one stable key or be refused; a label never merges two records.
- Model, client, client version and declared capabilities are profile data
  on the agent. They are changeable, are copied into audit for each action,
  and never authorize anything.
- One agent corresponds to one agent home (one installation), which holds one
  individual aimem credential per hub.

## Teams, projects and roles

A team is linked by the operator to one aimem team access profile, as the
context contract describes. Its project set is an explicit list of team
projects on one hub. Adding a project to that list is a coordination fact:
it grants nothing. Whether the team may read or write a project is decided by
aimem from the profile's live grants on every request.

Membership is an (agent, team, role) record created or changed only by the
operator, or by redeeming an operator-issued invitation (crew-onboarding).
Team roles grant no aimem authority and no administration of aicrew.

| Role | May, within aicrew | May not |
| --- | --- | --- |
| Coordinator | offer work to a named worker, withdraw offers, review submitted results, request stop, send messages | edit membership or grants; widen any authority; mark work DONE without the project's delivery evidence |
| Worker | accept or decline offers addressed to it, report progress, block, submit results, confirm stop, send messages | claim tasks directly while its capacity is attached to an aicrew attempt; act on another worker's attempt |
| Independent worker | claim an eligible task for itself through the aimem reservation contract, then run it as its own attempt | offer work to others or manage members |

A team has at most one active coordinator session at a time. A team-wide
coordinator generation advances whenever a coordinator session starts,
resumes or ends. An offer carries the coordinator generation it was issued
under. An offer that has not been accepted when that generation is
superseded can no longer be accepted; releasing its hold follows open
question 1.

## Sessions and generation fencing

A session is one running team context for one member, as in the context
contract. A membership has at most one active session. The session's
generation advances on resume, credential rotation rebind, role change,
leave, membership removal and operator stop. Every aicrew mutation names the
session ID and generation; a stale generation is refused with `context_stale`
and changes nothing.

- **Liveness is informational.** Heartbeats and suspected disconnects are
  reported to observers, but never free capacity, withdraw accepted work or
  release an aimem reservation.
- **Leave** is refused with `work_outstanding` while the member has an offer,
  attempt or unresolved request. After reconciliation, leave ends the session
  and advances its generation.
- **Resume** revalidates identity and context online (context contract),
  then carries accepted work forward under the new generation. Pending offers
  to the resumed worker are not carried forward: they can no longer be
  accepted, and the coordinator withdraws and may re-issue them.
- **Receipt replay** rechecks current authorization and the current
  generation before returning an earlier result, so an old session cannot
  learn more than a current one.

## Execution capacity

Each agent has exactly one execution capacity across all its teams and
projects. The capacity is taken in aicrew's store when an offer is created,
or when an independent worker records its claim intent, and is released only
after the attempt reaches a terminal state and the aimem reservation has been
released or finalized. Capacity is keyed by agent, not by session, so a
resumed or rotated session keeps the same slot and two teams cannot both
reserve one agent. A uniqueness rule in the store enforces it; a busy agent
cannot receive an offer.

Aicrew tracks capacity only for aicrew attempts. A person or standalone agent
working outside aicrew is arbitrated by the aimem reservation alone.

## Attempts and the aimem reservation

An attempt is aicrew's execution record for one task and one worker. It holds
the task reference, worker agent, offering coordinator (if any), state,
explicit base commit and branch, process pin (below), result reference, and
the aimem reservation ID and fence last confirmed by a receipt. The attempt
is never a second task authority: aimem decides who holds the task.

### Ordering across the two stores

The two services share no transaction, so every step that touches the
reservation follows one rule:

1. **Record intent locally first.** Aicrew commits the intended transition,
   the capacity it needs and a stable idempotency key in one local
   transaction.
2. **Call aimem** with that key, the expected task revision and the current
   reservation ID and fence.
3. **Commit the confirmed result locally.** Aicrew records the aimem receipt,
   the new fence and the new attempt state, together with audit and any
   lifecycle message.

After a lost reply, aicrew queries the receipt with the same key and never
retries with a fresh key. While the receipt is unresolved, the attempt is
`RECONCILING` and no further transition is sent. When the two stores
disagree, aicrew conforms to aimem:

| Aicrew shows | Aimem shows | Resolution |
| --- | --- | --- |
| intent recorded, call not committed | no hold | close the intent; release local capacity |
| intent recorded, call committed | hold for this attempt | complete the local transition from the receipt |
| attempt active | hold released or finalized by recovery | close the attempt as recovered; release capacity |
| no attempt | hold naming an aicrew reference | operator recovery; never adopt or release it automatically |
| any | unreachable or unresolved | stay `RECONCILING`; report; send nothing |

### Lifecycle

| Step | Aicrew state | Aimem reservation operation |
| --- | --- | --- |
| Offer to a named worker | `OFFERING` → `OFFERED` | Claim with an external holder referencing the offer, under the coordinator's verified context |
| Accept | `ACCEPTING` → `RUNNING` | Transfer from offer to attempt, to the worker's verified context; fence advances |
| Decline, withdraw or offer expiry | → `CLOSED` | Release under the current fence; task returns to `READY` |
| Independent claim | `CLAIMING` → `RUNNING` | Claim with an external holder referencing the attempt, under the worker's own verified context |
| Block | `RUNNING` → `BLOCKED` | Fenced work mutation recording the blocker; hold kept |
| Submit result | `RUNNING` → `SUBMITTED` | Fenced work mutation to task `REVIEW`; result reference recorded |
| Review: return for rework | `SUBMITTED` → `RUNNING` | The worker, as holder, makes the fenced work mutation back to `IN_PROGRESS` when it resumes; the hold is unchanged |
| Review: accept result | `SUBMITTED` → `ACCEPTED` | none; acceptance is not delivery |
| Finalize after human merge | `ACCEPTED` → `FINALIZED` | Finalize `DONE` with reviewed delivery evidence |
| Stop | `STOP_REQUESTED` → `STOPPED` → `CLOSED` | Release to `READY`, or `BLOCKED` with a recorded blocker, after the worker confirms stop |
| Operator recovery | any → `CLOSED` | Recovery release or finalize by an authorized principal |

The reservation contract fixes that only the current holder, or an
authorized recovery principal, may mutate, release or finalize a held task.
Before transfer the holder is the coordinator's offer; after it, the worker's
attempt. So the coordinator sends offer-stage releases, and the worker sends
work mutations, stop releases and, for the first pilot, the finalize. Which
principal acts when that holder cannot, is listed under open questions.

The worker starts work only in `RUNNING`, that is, after aimem has confirmed
the transfer or claim. It works in a worktree created from the attempt's
recorded base commit (workspace convention). A submitted result is immutable;
rework produces a new result on the same attempt. An offer may expire; a
running attempt never does. Aicrew's accepted result is not merge evidence
and never marks a task `DONE` by itself; no merge, release or deployment
follows automatically.

## Process pins

When an offer is created (or an independent claim is recorded), aicrew reads
the project's selected process from aimem and records it on the attempt: the
process repository, commit and manifest, plus a digest of the role
instructions the worker will receive. The worker receives and verifies that
pin before accepting; if the pinned assets or required skills are
unavailable, it declines with that reason instead of improvising. A running
attempt keeps its pin even if the project later selects a different process.
If the selection changes between offer and accept, the offer is withdrawn and
re-issued under the new pin. Every audit record carries the attempt's pin.

## Inbox, receipts and audit

Each team has one durable, ordered message log with a monotonic sequence.

- **Scope.** A message is either team-wide or scoped to one team project. A
  project-scoped message is readable only while that project remains in the
  team's project set; removing the project hides it from ordinary reads and
  keeps it for audit. Because every member of a team uses the same aimem
  profile, project scope within a team is not a per-member grant check;
  knowledge access itself stays in aimem.
- **Recipients** are fixed at send time from the active memberships (and, for
  directed messages, the named recipient).
- **Delivery is not receipt.** A read returns unacknowledged messages after a
  cursor, in bounded pages, and records a delivery attempt. Acknowledgement
  names explicit message IDs that were delivered; acknowledging twice is a
  no-op. A client restarting from its cursor sees every unacknowledged
  message again.
- **Idempotent send.** A sender's idempotency key and input digest make a
  retried send return the original message.
- **Lifecycle messages** that announce an offer, acceptance, submission,
  review, stop or recovery are written in the same local transaction as the
  transition they announce.
- **Questions never assign work.** Only the offer and claim transitions
  above create an attempt. Wake-up hints from client adapters are
  best-effort; the inbox stays authoritative.

Every aicrew mutation commits, in one local transaction, its state change,
receipt, audit record and any lifecycle message. Receipts are keyed by
(actor, operation, scope, key) with an input digest: an exact retry returns
the original result, and a changed input under the same key is a conflict.
Audit records hold the linked actor IDs, agent, session and generation,
profile snapshot (model and client), process pin, task and attempt
references and receipt, and never a secret, session handle or proof.

## Cross-project references

- A team may span several projects on one hub. Multi-hub teams are out of
  scope for the first pilot.
- Task, dependency and evidence references carry the hub ID and stable
  aimem IDs, never a copied task.
- Dependencies are evaluated by aimem when the reservation is claimed
  (reservation contract). Aicrew may read them first to avoid a useless
  offer, but that read never makes a task eligible: aimem's refusal wins.
- A reference to another hub is display-only and can never satisfy a claim.
- Removing a project from the team does not release running work; it blocks
  new offers for that project and leaves existing attempts to be finished or
  stopped explicitly.

## Carried forward from the legacy team design

The legacy aimem team code and three superseded planning tasks are
reference, not architecture. This contract keeps their proven invariants:
session generations and a single active coordinator; one reserved attempt
per worker, now per agent across projects; receipt-keyed idempotency with an
input digest; delivery recorded separately from explicit acknowledgement;
lifecycle messages in the transition's transaction; liveness that never
frees work. It changes three known weaknesses: capacity is per agent rather
than per project-local session; process pins live on the hub-side attempt
rather than in checkout-local state; and receipt replay rechecks the current
generation. It drops the old model's project-local teams, hub-and-project
tokens and checkout-bound state.

Any later adapter permission feature (a supervisor answering a client's
permission prompts) is out of scope here. When designed, it must bind each
decision to session, generation, request, exact operation and scope and
policy revision; it may only allow, deny or escalate within delegation the
operator already gave; and it never approves a merge, release or production
action.

## Open questions for the aimem reservation API review

These follow from combining the parent contracts and are not decided here:

1. Which principal releases an offer-stage hold when the worker declines or
   the offer expires while no coordinator session is active, or after the
   coordinator generation that created the offer was superseded. Until
   decided, such offers stay held and visible until an active coordinator or
   an authorized recovery principal releases them; capacity stays taken.
2. Whether finalize for an external hold may be sent by the reviewing
   coordinator's context, or only by the holding worker or recovery.
3. Which holder mode an independent worker's claim uses: an external holder
   referencing its aicrew attempt (assumed here), or a standalone hold.

## Deferred

| Question | Owner |
| --- | --- |
| Wire API, transport, schema, migrations | each implementation task's own review |
| Proof, session handles, introspection, service credentials | aimem context contract implementation |
| Reservation API shape and fault tests | aimem reservation API task |
| Knowledge access for team contexts | aimem knowledge access matrix |
| Client wake-up adapters | crew-claude, crew-opencode |
| Backup, restore and store migration procedure | first store implementation task |

## Proposed implementation split

Each item is an existing aicrew task; its acceptance criteria are refined to
cite this contract only after it merges. Each needs its own READY assessment
and exact-head review, and each is expected to be M or smaller.

1. **crew-core** (`01a0d6d7-1a60`): the aicrew store with agents, linked
   actors (against a fake verifier until the aimem proof exists), teams,
   project sets, memberships, roles, sessions, generations, a single active
   coordinator, receipts and audit. Tests: stale generation, concurrent
   resume, label ambiguity, replay after generation change, operator-only
   membership changes.
2. **crew-messages** (`01a0d6d7-1acd`): inbox, cursors, delivery versus
   acknowledgement, idempotent send, project scope and lifecycle messages.
   Tests: restart recovery, duplicate send, acknowledgement of undelivered
   IDs, project removal.
3. **crew-execution** (`01a0d6d7-1aad`), proposed split in two:
   a. local attempt state machine, capacity and reconciliation table against
      a fake aimem reservation service;
   b. integration with the real aimem reservation API once it exists.
   Tests: two teams competing for one agent's capacity, lost reply at each
   reservation step, offer versus coordinator change, stop without
   disconnect release, recovery paths in the disagreement table.
4. **crew-context** (`01a0d6d7-1aa2`): session entry, resume and leave on the
   verified context, including `work_outstanding` refusal.
5. **crew-onboarding** (`01a0d6d7-1a81`): invitation redemption creating the
   membership and running the identity proof.
6. **crew-observer** (`01a0d6d7-1b18`): read-only roster, capacity, attempts
   and inbox delivery state.

Tests use isolated fixtures only. No live team, credential or project is
touched before the first pilot task.
