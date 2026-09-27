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

## Session introspection (aimem → aicrew)

Aimem admits a team-mode request only after asking aicrew, online, whether
the request's aimem-scoped handle names a current aicrew session
(identity.v1 §3 in aimem's `DESIGN-AIFORGE-IDENTITY-WIRE.md`). Aicrew serves
that question at `POST /v1/crew/introspect` on its HTTPS service; the
registered endpoint is the full https URL of this route.

- **Handles.** Aicrew issues a handle (`acs1_` followed by 43 base64url
  characters, 256 random bits) to a session's own agent, at the session's
  current generation. It is bound to the agent's linked aimem hub, this
  service, the session and that generation, and lives at most 15 minutes.
  Issuing again for the same session and generation is the refresh: the
  handles it replaces stay valid for at most 60 more seconds, never past
  their own expiry. A handle is active only while its session is active at
  its generation, so a resume, rotation, role change, leave, removal or
  operator stop makes every handle of the session inactive at once, with no
  separate revocation. The client receives a first handle when it starts,
  resumes or re-proves a session. Aicrew stores only a handle's digest.
- **Credential.** Aimem authenticates with an introspection credential that
  aicrew issues (`aicrew_introspect_` followed by 256 random bits in hex).
  It is bound to one aimem hub, lives at most 366 days, and aicrew stores
  only its digest; the bearer exists once, in the file the operator's
  command creates privately for it. At most two are active per hub, so a
  rotation overlaps: issue a second, move aimem to it, revoke the first.
  Only the operator issues, lists or revokes one.
- **Request.** The body is `{version, hub_id, nonce, handle}` and at most
  4 KiB. The version is required as `1` both in `X-Aimem-Identity-Version`
  and in the body; otherwise aicrew answers `400 unsupported_version` and
  evaluates nothing. A missing, unknown, revoked or expired credential gets
  `401 peer_unauthenticated`, a credential bound to another hub
  `403 peer_forbidden`, and any other malformed request
  `400 invalid_request`. Refusals use the context contract's envelope and
  never echo the request.
- **Answer.** An active handle gets `200` with `{nonce, active: true,
  service_id, hub_id, identity: {user_id, token_id}, agent_id, team_id,
  role, session_id, generation, handle_expires_at}`. Every value comes from
  aicrew's own state: the session's role, the token the session is bound
  to (which must still be the agent's linked token), and the time the
  handle stops being active. Every other state, unknown, expired, replaced,
  ended, fenced by a generation change, another hub or service, or a
  malformed handle, gets `200` with `{nonce, active: false}` and no reason.
  Introspection reads one snapshot and changes nothing: it never ends a
  session, frees capacity or stands in for a stop or a leave.

## Receipt redemption (aicrew → aimem)

An identity proof completes only when aimem vouches for the receipt the
client obtained. Aicrew redeems it with aimem's
`POST /v1/identity/peers/{service_id}/redemptions` (identity.v1 §2), as the
store's verifier (`internal/verifier`).

- **Peer.** Aimem is named by its https origin and trusted only through the
  configured binding: a CA-issued certificate for the origin's DNS name, or
  a SHA-256 pin of its public key, which then replaces the chain check.
  Plain HTTP, trust on first use and disabled verification cannot be
  configured. Aicrew authenticates with the redemption bearer that aimem
  issued it, read on every call from a file only its owner can read.
- **Request.** The body is `{hub_id, challenge_id, receipt}`, with the
  version header and an `Idempotency-Key` of `k1_` and the unpadded
  base64url SHA-256 of the store's full request key; the raw key never
  leaves aicrew. Malformed IDs or a malformed receipt are refused before
  anything is sent.
- **One attempt.** At most 10 s, no retry, no redirect, no proxy and no
  connection reuse; a reply body over 16,384 bytes is refused unread. The
  store's same-key retry is the recovery: aimem answers an identical
  request with its recorded outcome.
- **Answer.** Only a `200` that echoes this request's key, service and
  challenge, names the requested hub and an active token, and carries
  well-formed IDs yields an identity; a replay is accepted like the
  original. Anything else is an error, never a partial identity.
- **Failures** carry a code and whether a same-key retry may succeed. Aimem's
  refusal codes keep identity.v1's table: `rate_limited`,
  `request_in_progress` and `identity_unavailable` are retryable; the rest
  are not. A transport failure, timeout, unreadable or unrecognized reply
  leaves the outcome unknown and is the retryable `identity_unavailable`. A
  `200` that does not answer the request is `invalid_reply`, not retried.
  No error carries the bearer or the receipt.

## Session tokens

Aicrew issues no persistent agent credential (`docs/ONBOARDING-CONTRACT.md`,
model point 1): an agent proves its aimem identity afresh whenever it enters
or resumes a team session, and receives a short-lived session token for that
session.

- **Entry.** The agent asks for an agent challenge, obtains an aimem receipt
  for it with its individual credential, and enters a team, or resumes its
  own active session, with the receipt. Aicrew redeems the receipt
  ("Receipt redemption") and requires the linked identity; a new aimem
  token ID for the same user is a rotation. Before asking aimem it checks
  that the entry could succeed (membership and no active session, or the
  agent's own active session). One transaction then consumes the challenge,
  applies the rotation, starts or resumes the session, and issues a token
  and a first aimem-scoped handle. A failed or unavailable verification
  changes nothing.
- **Token.** `ast1_` followed by 43 base64url characters (256 random bits),
  stored as a digest only. It lives at most 8 hours and is never refreshed:
  after that the agent resumes with a new proof. It is valid only while its
  session is active at the generation it was issued for, the session is
  bound to the agent's current aimem credential and the agent is still a
  member, so leave, stop, removal, resume, re-proof and rotation end it with
  no separate revocation. A handle issued under it never outlives it.
- **Authority.** The token authorizes its own session's handle refresh and
  leave, nothing else. Leave keeps its rules, `work_outstanding` included.
  A token is never a handle and never the introspection credential, and
  neither of those is ever a token; each has its own prefix and table. Aimem
  never receives the token.
- **Lost replies.** An entry retried with the same key and exactly the same
  input (challenge, team or session, service and receipt) before the
  challenge's deadline returns the recorded session with a fresh token and
  handle, and revokes those of the lost reply, in one transaction. It never
  starts a second session. The same key with any other input is an
  idempotency conflict; after the deadline, or once the session's generation
  has moved on, the retry is refused and the agent enters again with a new
  proof. A handle refresh or a leave is retried with the same key: a
  refresh replay returns the metadata without a handle, and a leave replay
  returns the recorded session although the leave ended the token.

## Client session API

`aicrewd` serves an agent's client these routes over its TLS listener. They
call only the store operations that authenticate by an aimem proof or a
session token; no route reaches an operation that trusts a caller it is
given, and a test enforces that.

| Route | Authentication | Purpose |
| --- | --- | --- |
| `POST /v1/crew/challenges`, JSON `{"agent_id"}` | none; rate-limited | Issue a challenge. Reply: `challenge_id`, `hub_id`, `service_id`, `expires_at`. |
| `POST /v1/crew/token`, form-encoded | the proof, or the session token | RFC 8693 exchange: entry, resume or handle refresh ("Standards mapping"). |
| `GET /v1/crew/session` | `Authorization: Bearer` session token | The token's session, hub, user and token ID and expiry. Changes nothing. |
| `POST /v1/crew/session/leave`, empty body or `{}` | `Authorization: Bearer` session token | Leave under the leave rules. |

- **Entry and resume.** The exchange names the proof type, this service as
  `audience`, the `challenge_id`, and either `team_id` (enter) or
  `session_id` (resume). The reply carries the session token
  (`access_token`), the session, and the first aimem-scoped handle as the
  extensions `aimem_handle` and `aimem_handle_expires_in`. A handle refresh
  names the session token, the session's aimem hub as `audience` and the
  handle type; a replay of a refresh key is refused as `refresh_replayed`,
  and the client refreshes again with a new key.
- **Retries.** Every route but the status read requires an
  `Idempotency-Key`. An entry whose identical request (same key) is still
  running is refused at once with the retryable `request_in_progress`,
  never answered after the original commits, which would revoke what the
  original returns. The client therefore retries only after abandoning the
  earlier attempt, and keeps the secrets from the latest request it sent.
- **Refusals** carry the context contract's envelope (`code`, `message`,
  `retryable`, `next_action`, `correlation_id`); on the token endpoint they
  also carry RFC 6749's `error` and `error_description`. There, a refused
  subject token or an exchange the policy will not honour
  (`challenge_invalid`, `proof_invalid`, `credential_inactive`,
  `identity_mismatch`, `identity_link_required`, `role_forbidden`,
  `context_stale`, `invalid_token`) is `400 invalid_request`, as RFC 8693
  §2.2.2 requires; `session_active` and `coordinator_active` are `409` with
  the same error. On the session routes an
  invalid token is `401` with `WWW-Authenticate: Bearer
  error="invalid_token"`, and `work_outstanding` and `idempotency_conflict`
  are `409`. Everywhere, `rate_limited` is `429` with `Retry-After`, and
  `request_in_progress` and `identity_unavailable` are the retryable `503`.
  No next action points to personal credentials, and no refusal or log line
  carries a receipt, token or handle.
- **Rate limits,** in memory per `aicrewd`: 10 challenges and 20 exchanges a
  minute per client address, and 6 handle refreshes a minute per session.
- **Disclosure.** The challenge route refuses an unknown and an unlinked
  agent ID alike, but a challenge it issues confirms that the ID is a
  linked agent. That is accepted: agent IDs are random, and the route is
  rate-limited.
- **Configuration.** Entry and resume need the `aimem` section of the
  service's configuration (the hub's origin, TLS trust and the redemption
  bearer file); without it they are refused as `aimem_unconfigured`, and
  the other routes work.

### Working in a team session: a fresh conversation

A team session belongs to one conversation, never to the machine. The
client starts it, and aimem binds it, like this:

1. `POST /v1/crew/challenges` for the agent.
2. `aimem identity proof --peer <service_id> --hub-id <hub_id> --challenge
   <challenge_id>` writes the receipt into a pipe.
3. The entry exchange returns the session token, kept in the client's
   memory only, and the first handle.
4. `aimem team-session open --service <service_id> --team <team_id>
   --session <session_id>` reads the handle from stdin; the client sets
   `AIMEM_TEAM_SESSION` for that agent process only.
5. Before the handle expires, a refresh exchange returns a new one for
   `aimem team-session refresh`, on stdin.
6. To finish, reconcile any open work through aicrew, then
   `POST /v1/crew/session/leave` and `aimem team-session close`.

Start a new conversation for team work rather than switching an existing
personal one. When the context fails, the conversation stops dependent work
and follows the refusal's next action: resume with a new proof, or ask the
operator. The session token dies after 8 hours, and a restarted client
resumes with a new proof. aimem's team access comes from the session's team
profile only, never from personal grants (aimem's context contract).

## Standards mapping

The identity flows follow OAuth 2.0 where a standard fits, so that a
standard authorization server can later issue aicrew's tokens without
changing what clients hold.

- **Session entry is an RFC 8693 token exchange.** The exchange endpoint
  (served by the client session API, form-encoded as the RFC requires) takes
  `grant_type=urn:ietf:params:oauth:grant-type:token-exchange`, the aimem
  proof receipt as `subject_token` of the aimem-proof-receipt type below,
  this aicrew service as `audience`, and
  `requested_token_type=urn:ietf:params:oauth:token-type:access_token`. The
  reply uses the RFC's `access_token`, `issued_token_type`,
  `token_type=Bearer` and `expires_in`. Handle refresh is a second exchange:
  the session token is the `subject_token` (the RFC's access-token type),
  the aimem hub is the `audience`, and the requested and issued type is the
  aimem-handle type below, with `token_type=N_A` because a handle is not an
  OAuth access token (RFC 8693 §2.2.1).
- **Token-type identifiers** are absolute https URIs in a namespace the
  project owns, as RFC 8693 allows any absolute URI. They identify; nothing
  fetches them. A move to a standard authorization server keeps them or
  maps them in its configuration. Each is defined under its own heading
  below, which is the URI's anchor.
- **Deliberate differences from RFC 8693.**
  - There is no client authentication. The agent is a public client; the
    single-use receipt, bound by aimem to this service and one challenge, is
    what authenticates it.
  - Entry carries an `Idempotency-Key`. OAuth has no idempotent retry, but
    a lost reply must not spend a second proof.
  - The team, the session to resume and the challenge travel as extension
    parameters, because an entry selects a session as well as a token.
- **Introspection follows RFC 7662's semantics.** The caller is the resource
  server authenticated by its own credential; `active` is the one field
  every answer has; an inactive answer carries no other field and no
  reason. identity.v1 fixes the deliberate differences: a JSON request, the
  presented value named `handle`, a `nonce` and version fields binding the
  answer to one call, and identity.v1's field names instead of `sub`, `aud`,
  `exp` and `client_id`. A later identity.v2 could add the RFC names beside
  them.
- **Opaque reference tokens.** No JWT or self-contained claim is issued: the
  holder of a token or handle learns nothing from it, and a resource server
  asks its issuer. A standard server can issue such tokens and answer
  introspection for them.
- **No OAuth library.** `ory/fosite` is a whole authorization-server
  framework with its own client, session and storage model, which would
  duplicate the store's receipts, generations and fencing; `go-jose` serves
  JWT, JWS and JWE, which opaque tokens do not need; `golang.org/x/oauth2`
  is a client library for a later client of a standard server. The standard
  library's random source, SHA-256 and constant-time comparison suffice,
  as for handles. The choice is revisited if aicrew adopts a standard
  authorization server.

### token-type-aimem-proof-receipt

`https://github.com/BlackVS/aicrew/blob/main/docs/CREW-CONTRACT.md#token-type-aimem-proof-receipt`
identifies an aimem proof receipt (`amr1_` and 43 base64url characters,
identity.v1) for an aicrew challenge: single-use, and valid at most 60 s.

### token-type-aimem-handle

`https://github.com/BlackVS/aicrew/blob/main/docs/CREW-CONTRACT.md#token-type-aimem-handle`
identifies an aimem-scoped session handle (`acs1_` and 43 base64url
characters; "Session introspection"), whose audience is one aimem hub.

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
`RECONCILING` and no further transition is sent. Only a committed receipt
for the pending request and the expected hold, or a refusal that is not
retryable, is a known outcome. A transport error, a timeout, a retryable
refusal and a receipt that does not match the request are all unknown: the
attempt keeps the worker's capacity and reconciles. A reservation
`coordination_proof` is an opaque reference that grants nothing in aicrew.
When the two stores disagree, aicrew conforms to aimem:

| Aicrew shows | Aimem shows | Resolution |
| --- | --- | --- |
| intent recorded, call not committed | no hold | close the intent; release local capacity |
| intent recorded, call committed | hold for this attempt | complete the local transition from the receipt |
| attempt active | no hold visible to the caller, or another holder's hold | stay open with the capacity; report; operator recovery. A status read is not evidence that this hold was released: the caller may only have lost sight of it |
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
| Operator recovery | any → `CLOSED` | Recovery release or finalize by an authorized principal in aimem; aicrew closes the attempt only once aimem can report this reservation closed (see Recovery) |

The reservation contract lets only the current holder mutate a held task,
and only the holder or an authorized recovery principal release it. Finalize
needs the current reservation ID and fence with an authenticated actor and
context, or the recovery path. Before transfer the holder is the
coordinator's offer; after it, the worker's attempt. So the coordinator sends
offer-stage releases, and the worker sends work mutations and stop releases.
This contract assumes the holding worker also sends the finalize; whether the
reviewing coordinator may, and which principal acts when the holder cannot,
are listed under open questions.

An offer can be accepted only while the team's coordinator generation is
the one it was made under; after a coordinator change, the current
coordinator releases or re-issues it. An offer is also bound to the
worker's session context when it was issued. If the worker had an active
session then, acceptance must come from that session at that generation, so
a resume, a credential rotation or a replacement session makes the offer
unacceptable, as "Sessions and generation fencing" requires for resume. If
the worker had no active session then, only the first session the worker
starts after the offer may accept it, and only at that session's first
generation. Aicrew numbers each member's sessions in a team in the order
they start and records on the offer the latest number at issue, so a
resume, a credential rotation or a replacement session never makes such an
offer acceptable again. An offer whose eligible session cannot be
established, such as one issued before this history was recorded, cannot
be accepted; the coordinator releases and re-issues it. A refused
acceptance changes nothing:
the hold and the worker's capacity stay until the coordinator's release is
confirmed. A worker's decline is recorded in
aicrew and releases nothing: the coordinator releases the offer's hold. An
expired offer cannot be accepted, and expiry releases nothing by itself.

The worker starts work only in `RUNNING`, that is, after aimem has confirmed
the transfer or claim. It works in a worktree created from the attempt's
recorded base commit (workspace convention). A submitted result is immutable;
rework produces a new result on the same attempt. An offer may expire; a
running attempt never does. Aicrew's accepted result is not merge evidence
and never marks a task `DONE` by itself; no merge, release or deployment
follows automatically.

**Results, review and finalize.** Submitted results form an append-only
history per attempt; none is ever changed. The team's current coordinator,
including a successor, reviews the latest result: accepting it records that
exact result and the reviewing session and generation; returning it for
rework clears the acceptance, so a later result needs its own review. No
member reviews its own result, even after becoming the coordinator. A
coordinator may finalize only from the session and generation that
recorded the acceptance, so a successor, or a coordinator after a resume,
reviews the result itself first; succession alone is not review. The
holder may finalize its own accepted result. Finalizing as `DONE` needs the
delivery evidence the project's process requires; for the current
development workflow, the reviewed head, the human merge and post-merge CI.
A trusted internal caller supplies both the requirement and the references;
arbitrary callers never do, and neither a reference nor aimem storing it
proves that the referenced check passed. There is no cancellation shortcut:
cancelling belongs to the stop and recovery flow.

**Stop.** The team's current coordinator, including a successor, or the
operator requests the stop of a running attempt in any work phase before
finalize, with a reason; the worker cannot request its own stop, even
after becoming the coordinator. The
request is local: it sends nothing to aimem, and the hold and the worker's
capacity stay. A requested stop cannot be withdrawn and voids a recorded
acceptance, returning the result to submitted; while it stands, no work
update, review or finalize is accepted. A step already in flight settles or
reconciles by the usual rules and never clears the stop; a finalize in
flight keeps its acceptance, and if aimem did not commit it the acceptance
is voided then. Only the worker confirms the stop, from its current session
with no step in flight. Nothing else confirms it: not elapsed time, a lost
connection, a coordinator change, or the worker's session being resumed or
replaced, so an unconfirmed stop stays requested indefinitely, with its
hold and capacity. After confirming, the worker, as holder, releases the
task to `READY`, or to `BLOCKED` with a blocker, through the ordinary step
ordering; the attempt closes and the capacity is freed only when aimem
commits the release. A lost reply is reconciled by the receipt for the same
key; a call that was not committed, or a final refusal, leaves the attempt
stopped with its hold and capacity.

**Recovery.** Aicrew sends no recovery mutation: the recovery principal
acts in aimem. Aicrew closes an attempt only on the known outcome of its
own request (a committed receipt, or a claim aimem refused or did not
commit); a status read never closes one. Until the reservation contract can report
that this exact reservation closed with its fence advanced (an aimem
follow-up for the recovery work), an attempt whose hold was released
outside aicrew stays open with the capacity for operator recovery.

**Independent claim.** Only a member with the independent role claims, from
its own active session at its current generation, and only a task in one of
the team's projects; a worker receives work by offer and a coordinator
offers it. A trusted reader supplies the task reference, its expected
revision and the project's selected process pin; the claimer supplies the
base commit and branch and the digest of the instructions it verified,
which must match the pin. Recording the claim takes the claimer's one
execution capacity, so a member busy in any team cannot claim, and a
member with a claim cannot be offered work anywhere. Aimem is then asked to
claim the task with an external holder that names this exact attempt,
under the claimer's own verified context. A committed claim runs the
attempt at once. A final refusal, such as a task another holder already
holds, or a call that was not committed, closes the attempt and frees the
capacity; any other outcome keeps both and reconciles by receipt. A claimed
attempt has no coordinator. From then on it follows the same rules as any
running attempt: the claimer works as holder, the team's current
coordinator reviews (a claimer never reviews its own result, and with no
coordinator a submitted result waits), the holder or the reviewing
coordinator finalizes, and a stop is requested by the coordinator or the
operator and confirmed by the claimer.

**Task content.** Aicrew's commands carry only the fields aicrew owns: the
step's intent and target state, a blocker, a result reference, a reason and
delivery references. The integration adapter reads aimem's current task,
keeps every other field, and sends the complete content with the expected
revision and the reservation fence. A revision conflict is refused, never
overwritten: the attempt keeps its phase, reconciles its revision from its
hold status while no request is pending, and sends a new request. A pending
request is never changed.

## Process pins

When an offer is created (or an independent claim is recorded), aicrew reads
the project's selected process from aimem and records it on the attempt: the
process repository, commit and manifest, plus a digest of the role
instructions the worker will receive. The worker receives and verifies that
pin before accepting; if the pinned assets or required skills are
unavailable, it declines with that reason instead of improvising. A running
attempt keeps its pin even if the project later selects a different process.
If the selection changes between offer and accept, acceptance is refused and
the offer is withdrawn and re-issued under the new pin; a recorded pin is
never updated. A matching instruction digest shows the worker has the
recorded instructions, not that it read or understood them. The pin comes
from a trusted reader of the project's selection, never from what a caller
sends. Every audit record carries the attempt's pin.

## Inbox, receipts and audit

Each team has one durable, ordered message log with a monotonic sequence.

- **Scope.** A message is either team-wide or scoped to one team project. A
  project-scoped message is readable only while that project remains in the
  team's project set; removing the project hides it from ordinary reads and
  keeps it for audit. Because every member of a team uses the same aimem
  profile, project scope within a team is not a per-member grant check;
  knowledge access itself stays in aimem.
- **Recipients** are fixed at send time. A team-wide message goes to every
  other active member; a directed message goes to one named active member.
  A message to its sender, or one with no recipient, is refused. A member
  who joins later does not receive earlier messages. A member who is removed
  receives nothing sent while it is out; if it is added again, it still has
  the messages addressed to it that it never acknowledged.
- **Current session.** Sending, reading and acknowledging all require the
  caller's own active session at its current generation. A stale
  generation, an ended session, a removed member or a changed role is
  refused with `context_stale`, and the refusal changes nothing.
- **Delivery is not receipt, and there is no client cursor.** A read returns
  the caller's oldest unacknowledged messages, in sequence order and in
  bounded pages, and records a delivery on each. Acknowledgement names
  explicit message IDs that were delivered to the caller; acknowledging an
  undelivered or foreign message is refused, and acknowledging twice is a
  no-op. Because every read starts at the oldest unacknowledged message, a
  message delivered but not acknowledged, even across a restart, is
  delivered again, and neither a page size nor the order of
  acknowledgements can skip it. A client that wants the next page
  acknowledges the current one.
- **Idempotent send.** A sender's idempotency key and input digest make a
  retried send return the original message, while the sender's session is
  still at the generation it sent from.
- **Text** is stored exactly as sent: 1 to 16 KiB of valid UTF-8 that is not
  blank. The store does not rewrite text or judge its style.
- **Lifecycle messages** that announce an offer, acceptance, submission,
  review, stop or recovery are written in the same local transaction as the
  transition they announce. The store provides this as an internal step;
  the transitions that use it arrive with crew-execution.
- **Questions never assign work.** Only the offer and claim transitions
  above create an attempt. Wake-up hints from client adapters are
  best-effort; the inbox stays authoritative.

Every aicrew mutation commits, in one local transaction, its state change,
receipt, audit record and any lifecycle message. Receipts are keyed by
(actor, operation, scope, key) with an input digest: an exact retry returns
the original result, and a changed input under the same key is a conflict.
A command that asks aimem before its transaction, such as an identity proof
or an invitation completion, holds its key while it runs: an identical retry
that arrives meanwhile waits and then returns the original's result, or is
evaluated anew if the original committed nothing. A wait that passes
aicrew's bound ends with the retryable `request_in_progress`; one the caller
cancels, or whose deadline passes, ends with the caller's own cancellation.
Neither changes anything.
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

These follow from combining the parent contracts. The first two are not
decided here; the third has since been decided:

1. Which principal releases an offer-stage hold when the worker declines or
   the offer expires while no coordinator session is active, or after the
   coordinator generation that created the offer was superseded. Until
   decided, such offers stay held and visible until an active coordinator or
   an authorized recovery principal releases them; capacity stays taken.
2. Whether finalize for an external hold may be sent by the reviewing
   coordinator's context, or only by the holding worker or recovery.
3. Decided (C4 decision 3; the reservation fixture's independent-worker
   claim): an independent worker's claim uses an external holder that names
   its aicrew attempt, not a standalone hold.

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
