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
- **A step begun before a resume** is the same agent's own step, and its
  resumed session recovers it under the same key. A token-driven begin's
  idempotency covers only its intent: the operation, the attempt or scope,
  and the request. It does not cover the acting session or generation. So
  the same agent's current session replays it, while the fenced
  generation's token replays nothing.

  A pending step gets a replacement proof for the current generation, and
  only if the resumed session still acts the step by the rule of the step's
  fact kind (Coordination facts). That is rechecked in the replacement's
  own transaction, where the write commits.
  - A pending independent claim is the claiming session's own, so its
    recorded generation moves with the resume.
  - A coordinator's resume moves the team's coordinator generation. So its
    pending offer, the worker's pending acceptance of that offer, and its
    own finalize as the accepting coordinator are refused. The coordinator
    withdraws and re-offers instead (D-ebb9-3).
  - The same key with another request is still `409
    idempotency_conflict`.

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

## Coordination facts (aimem → aicrew)

Before it commits a coordinated reservation step, aimem asks aicrew, online,
whether the step's coordination proof names a fact that is still true
(coordination.v1 in aimem's `DESIGN-AIFORGE-COORDINATION-WIRE.md`, as amended
by C5-w3 at aimem `0dd404a`; its fixtures are unchanged at `a9b9b6f`). Aicrew serves that question at
`POST /v1/crew/coordination`, on the same origin as introspection.

- **Proofs.**
  - Each step that needs a fact gets an `acp1_` proof (256 random bits). It
    is issued in the transaction that starts the step's intent, and it is
    bound to that one intent: the operation, the acting session and
    generation, the attempt and the request key. The six steps are the offer
    claim, the transfer on accept, the release of an offer never accepted,
    the independent claim, the finalize and the stop release.
  - A holder's `update` needs no proof.
  - A proof lives at most 15 minutes and ends when its step settles.
  - Aicrew stores only a proof's digest.
  - The proof travels only in the step's `coordination_proof` and in
    aimem's question. It never appears in an answer, a receipt, a log or a
    refusal.
- **Credential.**
  - Aimem uses its introspection credential. An introspection credential
    permits `crew.introspection`, `crew.coordination`, or both.
  - A credential without `crew.coordination` gets `401
    peer_unauthenticated` here, exactly like an unknown one.
  - A credential issued before coordination existed permits introspection
    only. Rotating it gives aimem one that permits both, which is the
    default for a new issue.
- **Request.**
  - The body is `{version, hub_id, nonce, proof}`, at most 4 KiB.
  - The version must be `1`, both in `X-Aimem-Coordination-Version` and in
    the body. Otherwise the answer is `400 unsupported_version`, and nothing
    is evaluated.
  - A credential bound to another hub gets `403 peer_forbidden`, and any
    other malformed request gets `400 invalid_request`. Refusals use the
    context contract's envelope.
- **Answer.**
  - An active proof gets `200` with `{nonce, active: true, service_id,
    hub_id, fact}`.
  - `fact` carries the kind, the operation and the task, the `k1_` digest of
    the step's request key, and the acting member (user, agent, team, role,
    session and generation). It also carries the references the kind
    requires: the offer and attempt references, the intended worker of an
    offer, the process pin, and the evidence digest. `expires_at` is
    truncated to the second.
  - Every value comes from one snapshot of aicrew's current state, never
    from the proof alone.
  - Every other state gets `200` with `{nonce, active: false}`, with no
    reason. That includes an unknown, expired, ended or superseded proof, a
    session that ended or moved to another generation, a changed role or
    membership, a moved coordinator generation, a withdrawn acceptance, and
    another hub.
  - A step reconciling after a lost reply still answers, because the member
    may retry the same key while the proof lives.
- **Process pin.**
  - `offer`, `accepted_attempt` and `independent_claim`, the steps that
    start work, carry `process: {repo, commit, manifest}`. It is aicrew's
    recorded pin, copied verbatim with no normalization, because aimem
    compares it byte for byte with the project's current selection before
    it commits, and refuses a mismatch with `process_mismatch`. No other
    kind carries a pin.
  - Aicrew records a pin only in the hub selection's forms:
    - a Git URL of at most 512 bytes, starting with `https://`, `ssh://` or
      `git@`, with no whitespace, quote or leading `-`;
    - the full 40-character lowercase commit;
    - a clean relative manifest path of at most 256 bytes that stays inside
      the repository, and is not `..`.
  - A pin recorded in any other form answers inactive, never malformed.
- **Evidence digest (C5-w3).**
  - `accepted_for_finalization` carries `evidence_digest`, and no other
    kind does. It is `e1_` and the unpadded base64url SHA-256 of the
    finalize's `terminal_evidence`: for each reference in order, the 4-byte
    big-endian length of its UTF-8 bytes, then the bytes. Nothing is
    normalised.
  - It covers exactly the references the finalize sends aimem: the
    confirmed delivery's, in their confirmed order, or the one-shot path's,
    then the attempt's identity reference ("Confirmed delivery").
    aimem recomputes it in the committing transaction and refuses a
    mismatch with `evidence_mismatch`.
  - A finalize whose recorded evidence cannot be read answers inactive.


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
call only the store operations that authenticate by an aimem proof, a
session token or an invitation code; no route reaches an operation that
trusts a caller it is given, and a test enforces that.

| Route | Authentication | Purpose |
| --- | --- | --- |
| `POST /v1/crew/challenges`, JSON `{"agent_id"}` | none; rate-limited | Issue a challenge. Reply: `challenge_id`, `hub_id`, `service_id`, `expires_at`. |
| `POST /v1/crew/token`, form-encoded | the proof, or the session token | RFC 8693 exchange: entry, resume or handle refresh ("Standards mapping"). |
| `POST /v1/crew/invitations/begin`, JSON `{"code"}` | the invitation code; rate-limited | Begin redeeming an invitation. Reply: `challenge_id`, `hub_id`, `service_id`, `expires_at`. |
| `POST /v1/crew/invitations/complete`, JSON `{"code", "challenge_id", "receipt"}` | the invitation code and an aimem receipt; rate-limited | Complete it. Reply: `agent_id`, `team_id`, `role`, `hub_id`, `user_id`, `created`, `rebound`, `rotated`; no secret. |
| `GET /v1/crew/session` | `Authorization: Bearer` session token | The token's session, hub, user and token ID and expiry. Changes nothing. |
| `POST /v1/crew/session/leave`, empty body or `{}` | `Authorization: Bearer` session token | Leave under the leave rules. |
| `POST /v1/crew/attempts` | `Authorization: Bearer` session token | Begin an offer ("Attempt steps"). |
| `POST /v1/crew/attempts/{id}/accept`, `/decline`, `/withdraw` | `Authorization: Bearer` session token | Begin an acceptance or a withdrawal, or record a decline. |
| `POST /v1/crew/attempts/claim` | `Authorization: Bearer` session token | Begin an independent claim. |
| `POST /v1/crew/attempts/{id}/stop`, `/confirm-stop`, `/release` | `Authorization: Bearer` session token | Request or confirm a stop, or begin the stopped attempt's release. |
| `POST /v1/crew/attempts/{id}/review`, `/confirm-delivery`, `/finalize` | `Authorization: Bearer` session token | Review the latest result, confirm its delivery, or begin finalizing it as `DONE`. |
| `POST /v1/crew/attempts/{id}/work` | `Authorization: Bearer` session token | Begin, or supersede, the holder's work update: block, submit or resume. |
| `POST /v1/crew/attempts/{id}/settle` | `Authorization: Bearer` session token | Settle a step through aimem's read scope. |
| `GET /v1/crew/inbox?limit=N` | `Authorization: Bearer` session token | Deliver the member's oldest unacknowledged messages, at most N (1 to 100, default 20) and at most 128 KiB of JSON (always at least one), and record the delivery of those returned ("Inbox, receipts and audit"). Reply: `messages`. |
| `POST /v1/crew/inbox/ack`, JSON `{"ids"}`, with `Idempotency-Key` | `Authorization: Bearer` session token | Acknowledge messages delivered to the member. Reply: `acknowledged`, `already`. A message never delivered to it is `409 message_not_delivered`. |

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
  also carry RFC 6749's `error` and `error_description`. That includes the
  refusals made before a route's handler runs: a method the path does not
  serve is `405 method_not_allowed` with `Allow`, and a declared body over
  the service's limit is `413 request_too_large`, both with `error:
  invalid_request` on the token endpoint. There, a refused
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
  An IPv4 client is one address; an IPv6 client is its /64, so cycling
  addresses inside one allocation gains nothing. Each limiter tracks at
  most 4,096 clients. To admit a new one into a full table it drops only
  clients whose budget has refilled completely, which loses nothing; while
  every tracked client is still spending, a new client is refused as
  `rate_limited` until one has refilled. A flood of new addresses therefore
  never restores anyone's budget: at worst it keeps new clients out for one
  minute.
- **Disclosure.** The challenge route refuses an unknown and an unlinked
  agent ID alike, but a challenge it issues confirms that the ID is a
  linked agent. That is accepted: agent IDs are random, and the route is
  rate-limited.
- **Configuration.** Entry, resume and invitation completion need the
  `aimem` section of the service's configuration (the hub's origin, TLS
  trust and the redemption bearer file); without it they are refused as
  `aimem_unconfigured`, and the other routes work.
- **Invitation redemption** ([onboarding contract](ONBOARDING-CONTRACT.md),
  "Redemption"). The code and the aimem receipt travel only in the JSON body:
  never in the path, the query, a header, a log line, an audit record or a
  refusal. Each route takes its key in the `Idempotency-Key` header, as the
  other routes do: the begin's key is the redemption key, the completion's
  the completion key, with the contract's retry rules. The refusals are the
  envelope's:
  - `invitation_invalid` (`403`) is the one non-disclosing answer for an
    unknown, malformed, expired, revoked, redeemed or locked invitation;
    next action: ask the operator for a new invitation;
  - `challenge_invalid` (`400`): begin again with a new key;
  - `proof_invalid`, `credential_inactive` and `identity_mismatch` as at
    entry;
  - `identity_already_linked` (`409`, one aimem user links to one agent) and
    `role_conflict` (`409`, role changes are operator operations): ask the
    operator;
  - `work_outstanding` (`409`) for a rebind while the agent has open work;
  - `request_in_progress` and `identity_unavailable`, the retryable `503`.

  **Limits:** 10 begins and 20 completions a minute per client address,
  with the limiter above. **Refusal counter:** each refusal on these two
  routes adds to an in-memory total per code, and the log line of every
  refusal carries its code's running total. A line holds only the route, the
  code and the count: never an invitation code, key, digest or address.

### Attempt steps

Under D4(a) aicrewd never mutates a reservation. The acting member's own
aimem connection sends each mutation, and aicrewd confirms the outcome
through aimem's read-only reservation scope. So every reservation step is
two calls, begin and settle: the offer family, the independent claim, the
work updates, the stop release and finalize.

The session token alone names the agent, its session and its generation.
The store authenticates the token again inside every command, replays
included, so an ended or resumed session's token begins nothing. Every
begin needs an `Idempotency-Key`; settle does not.

- **Begin.**
  - `POST /v1/crew/attempts`: the coordinator offers a task to a named
    worker. The body carries:
    - `worker_agent_id`;
    - `task`, as `hub_id`, `project_id` and `task_id`;
    - `expected_revision`, `base_commit` and `branch`;
    - `process`, as `repo`, `commit` and `manifest`;
    - `instruction_digest` and `expires_at`;
    - `dependency_evidence?`: `[{task_id, state, revision}]`, the
      coordinator's client's read of the task's dependencies ("Cross-project
      references"), at most 64, distinct, each `DONE` with a positive
      revision. Anything else is `400 invalid_request`. aicrewd records it
      in the offer's audit and verifies nothing with it.

    The pin and the digest are the coordinator's ("Process pins").
  - `/{id}/accept`: the offer's worker accepts. The body carries the
    `instruction_digest` the worker verified, which must equal the offer's.
  - `/{id}/withdraw`: the team's current coordinator releases an offer that
    was never accepted, whether withdrawn, declined or expired. The body is
    empty or `{}`.
  - `POST /v1/crew/attempts/claim`: an independent member claims a task in
    one of its team's projects for itself. The body carries `task`,
    `expected_revision`, `base_commit`, `branch`, `process` and
    `instruction_digest`, all the claimer's ("Process pins"). The claim's
    fact carries the pin. `Location` names the new attempt.
  - `/{id}/release`: the holder of a stopped attempt, after its own
    confirmation, releases the task. The body is `{target, blocker?}`, with
    the target `READY`, or `BLOCKED` with a blocker. The step carries the
    `stopped` fact.
  - `/{id}/finalize`, with `{result_seq}`: the holder, or the coordinator
    from the session and generation that recorded the acceptance, finalizes
    the accepted result as `DONE`. The body names the result only. The
    finalize needs the confirmed delivery of that result ("Confirmed
    delivery"), and its terminal evidence is that record's, never the
    finalizer's. The step carries the `accepted_for_finalization` fact.
  - `/{id}/work`, with `{intent, detail?}`: the holder's work update.
    - `intent` is `block` (with the blocker as `detail`), `submit` (with
      the result reference) or `resume`.
    - The member's client adds a submitted result's reference to the
      task's `candidate_refs` with the note `aicrew attempt <id> by member
      <agent>`, naming the attempt and the submitting member. A resend of
      the same reference with the same note adds nothing.
    - It is a fenced update of the holder's own hold, with no coordination
      fact, so it has no proof. Like every aimem mutation it advances the
      hold's fence by exactly one: aicrew confirms a committed update only
      at the next fence and records it, so the holder's later steps send it,
      and a delayed update under the old fence is refused.
  - A begin records the intent and the capacity it needs, as "Ordering
    across the two stores" requires. It answers `200` with coordination.v1's
    begin response: `{operation, request_key, expected_revision,
    reservation_id?, fence?, holder?, coordination_proof}`.
    - For an offer or a claim, `Location` names the new attempt.
    - Some steps also carry the values the member sends aimem with them
      (D-b1b-2). No other step carries them:
      - a stopped attempt's release carries `target_state`, `reason` and
        `blocker?`;
      - a finalize carries `target_state: DONE`, `reason` and
        `terminal_evidence` (the confirmed delivery's references, then the
        attempt's identity reference);
      - a work update carries `intent`, `target_state`, and `blocker` or
        `result_ref`.
    - A work update's answer has no `coordination_proof`.
    - The member sends exactly that request to aimem.
    - The proof appears only in this answer.
  - **Retried begin.** A retry with the same key and the same input, while
    the step is pending, gets a replacement proof for the same step and
    request key. The earlier proof ends at once, so aimem refuses it if it
    is still on its way. A work update has no proof, so its retry answers
    the same step. A retry after the step settled is refused: `409
    step_settled` if it committed, and `409 attempt_state` if it did not.
    - **After a resume.** The same holds for a retry from the same agent's
      resumed session. The input is the step's intent, without the acting
      session and generation.
    - **The replacement proof** names the current generation. It is issued
      only while that session still acts the step by its fact kind's rule,
      checked in the replacement's transaction: otherwise `403
      attempt_forbidden`, and nothing is issued ("Sessions and generation
      fencing").
  - **Superseding a stuck update** (D-b1b-4 (a)). aimem may refuse an update
    without moving anything, for example for authorization or validation.
    Then no evidence ever settles it (see Settle below). The holder may give
    the pending update a new request key: `/{id}/work` with `{intent,
    detail?, supersedes}`.
    - `supersedes` names the pending update's key, and the intent and
      detail repeat the update's own.
    - The update is unchanged: the same expected revision and fence. So
      aimem's revision check lets at most one of its keys commit.
    - Every key is an alias of the one step: settling under any of them
      settles it, and each keeps the step's outcome.
    - A step supersedes at most 8 keys; the next supersede is refused with
      `409 supersede_limit`. Settling the step then takes at most one hold
      read and 9 receipt reads.
- **Decline.** `/{id}/decline`, with an empty body or `{}`: the offer's
  worker declines.
  - It is local: there is no step and no proof. The coordinator then
    withdraws the offer.
  - The reply is the attempt: `id`, `state`, `declined`, `stop`,
    `close_reason`, and `process_verified_receipt` once aimem verified its
    pin.
- **Stop.** Local steps, with no step and no proof, each answering with the
  attempt:
  - `/{id}/stop`, with `{reason}`: the team's current coordinator requests
    the stop of a running attempt. The worker never requests its own stop.
  - `/{id}/confirm-stop`, with an empty body or `{}`: the worker confirms,
    from its current session, with no step in flight.
- **Review.** `/{id}/review`, with `{result_seq, decision}`: the team's
  current coordinator accepts the latest submitted result, or returns it for
  rework. It is local, and no member reviews its own result.
- **Confirmed delivery.** `/{id}/confirm-delivery`, with `{result_seq,
  evidence}`: a team member confirms that the accepted result was delivered.
  It is local, and it is audited.
  - `evidence` is a list of `{kind, ref}`: the `reviewed_head`,
    `human_merge` and `post_merge_ci` of the current development process,
    with at most 15 references.
  - A finalize carries those references and then one more, which aicrew
    adds: `{kind: text, ref: "aicrew attempt <id> by member <agent>"}`,
    naming the attempt and its worker. aimem keeps it with the task's
    terminal evidence, and the evidence digest covers it, so a finalize
    carries at most 16. The one-shot path's evidence has the same cap and
    the same last reference. A finalize refuses a confirmation recorded with
    more references as `delivery_unconfirmed`, and the delivery is confirmed
    again.
  - Each reference is of a shape aimem accepts as terminal evidence, so a
    confirmed set is never refused for it at finalize: valid UTF-8, not
    blank, at most 256 bytes, with no control character but tab, newline
    and carriage return, and no bidirectional override. The one-shot path's
    evidence follows the same rules.
  - aimem also refuses secret-shaped text, which aicrew does not mirror: a
    confirmation whose reference looks like a secret is refused by aimem at
    finalize, and is then confirmed again.
  - Aicrew queries no forge. The confirming member gathers the evidence
    (for example with the verify-delivery skill), and aicrew records it,
    bound to the accepted result and to the confirming agent, session,
    generation and time.
  - Today the confirming member is the team's current coordinator, who is
    not the attempt's worker. That rule is one predicate, so a dedicated
    verifier role would change only it.
  - A confirmation of the same result replaces an earlier one while no
    finalize is in flight. Rework or a stop voids it with the acceptance.
  - Evidence that no member confirmed unlocks nothing: a finalize refused
    for it is `409 delivery_unconfirmed`.
- **Settle.** `/{id}/settle`, with `{request_key, outcome, code?}`.
  - Any member of the attempt's team may settle, so another member can
    settle for one whose client went offline.
  - `outcome` is the member's report, and it is only a hint. It is
    `committed`, `refused` (with aimem's refusal `code`) or `unknown`.
  - **A committed receipt** in the read scope, under any proof issued for
    the step (a replaced one included), applies the transition. The answer
    is `200`, with `settled: true`, `outcome: committed` and the attempt.
  - **A refused or unknown report** voids the step at once: its proofs end,
    so aimem refuses any late use of them.
  - **Not committed.** The step settles as not committed (`200`, `outcome:
    not_committed`) only when the read scope still shows no receipt, for
    lookups started at least 10 s after all the step's proofs ended or
    expired.
  - **Pending.** Until then, the answer is `202`, with `settled: false` and
    `Retry-After`, and the client settles again. That includes a
    `committed` report the read scope does not show yet.
  - Settling a settled step reports its outcome again.
  - **A work update** has no proof, so it follows coordination.v1's
    proofless rule instead:
    - A committed receipt by key, under any of the step's keys, applies it.
    - Nothing is voided, and no report changes anything.
    - A `none` becomes final (`not_committed`) only when the read scope's
      hold status shows the hold's fence or task revision past the
      request's, and the receipts are read after that observation.
    - Until then the step stays pending, however long. Superseding it is
      the way out.
  - Without a configured read scope (`read_token_file`), every settle
    answers `202`: no report is ever trusted.
- **Refusals** use the envelope above. The step routes add these codes to
  the session API's:
  - `403 attempt_forbidden`: not the session's team, or not its role for
    this step;
  - `409 task_busy`: this service already has an open attempt on the task,
    in any team;
  - `409` for `agent_busy`, `attempt_state`, `offer_expired`,
    `offer_declined`, `offer_stale`, `instruction_mismatch`,
    `delivery_unconfirmed`, `supersede_limit` and `step_settled`;
  - `404 step_unknown`: no step of the attempt has that request key;
  - the retryable `503 outcome_unknown`, with `Retry-After`: the read scope
    did not answer, or showed a receipt that is not the step's.
- **Paths.** An attempt ID is 1 to 128 characters from `[A-Za-z0-9._:-]`,
  starting with a letter or digit. Any other path is `404`. The log names
  the route template, never the ID.

### Reconciliation by aicrewd

With a read scope configured, aicrewd settles the steps members left
pending, and closes attempts whose reservation aimem closed outside aicrew,
with no member online (crew-execution b3b). It acts only on the read scope's
answers, as its own reconciler caller.

- **The reconciler's authority.** No route ever acts as the reconciler, and
  its audit records name it. It may settle a pending step and close an
  attempt as recovered, and nothing else: it never begins a step, reviews,
  confirms a delivery or writes to aimem. Every other command refuses it.
- **Settling.** The reconciler settles by the rules of Settle above, but
  with no member's report, so it voids nothing, and a member still sending
  the step is never cut off.
  - It settles committed only on a committed receipt for that exact step:
    by one of its proofs, or an update's request key. So a finalize is
    recorded as finalized only on its own receipt, never because the task
    is `DONE` in aimem, which other routes such as an admin recovery also
    reach.
  - It settles not committed only when the scope's rules make a `none`
    final: every proof of the step ended or expired at least 10 s earlier,
    or, for an update, the hold's fence or revision was observed past the
    request's.
- **The recovered closure.** An open attempt holding a reservation, with
  no step pending, closes as `recovered` only when the hold status answers
  `closed` for exactly its reservation, with a `closing_fence` greater than
  the fence aicrew confirmed. It records aimem's `closed_by`, the closing
  fence and the time; the attempt's capacity is free again. `none`, `held`,
  another reservation's closure or an unchanged fence leave it open. The
  rule is checked again inside the closing transaction.
- **Nothing on time alone.** A read that fails, is refused or is malformed
  settles and closes nothing, however long it lasts.
- **Progress across windows.** A step's settle reads one lookup per proof,
  or per key of an update. A lookup whose `none` is final is recorded as the
  step's scan progress and not read again: a proof's, when read at least
  10 s after the proof ended or expired; an update key's, when read after
  the hold's fence or revision was seen past the request. So a step with
  more lookups than a read window finishes across windows and restarts,
  whatever its kind. Proofs are read newest first, since only the newest can
  still be live.
- **Bounded work per step.** An update step reads at most its hold and 9
  keys (the supersede cap). A proof-backed step reads its proofs that are
  not yet final: in practice the newest, and those replaced in the last
  10 s.
- **Pacing.** A round runs every 15 s and takes every candidate, a pending
  step or a hold, least recently checked first (pending steps first among
  equals). A candidate whose scan the budget interrupts counts as checked:
  it goes to the back of the line and resumes from its recorded progress,
  so no candidate holds the head of the queue across windows. A candidate
  the budget refuses before its first read was not checked, and keeps its
  place: rounds that run with the budget spent move nothing behind the
  scans that spent it. Its reads
  stay within 30 in any rolling minute, half of the read credential's 60,
  the rest being left to members' settles. When aimem answers
  `rate_limited` or `request_in_progress`, the loop pauses for aimem's
  `Retry-After`, and at least one round.

### Working in a team session: a fresh conversation

A team session belongs to one conversation, never to the machine. The
client (`aicrew-agent run`, or `aicrew-agent session start`;
`docs/DEVELOPMENT.md`) starts it, and aimem binds it, like this:

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

`aicrew-agent run` does all of this around one agent client (Claude Code
or OpenCode), which it starts as its child in the agent home with
`AIMEM_TEAM_SESSION` set in that child's environment only; the client's
`aimem mcp` inherits it (observed for both clients). When the client exits,
the launcher leaves, keeping the session instead if the member still has
open work. Ctrl-C and SIGTERM stop the client on every platform. If the
launcher itself is killed, Linux and Windows stop the client with it. macOS
cannot: a client that outlives a killed launcher keeps running, but its
handle is no longer refreshed, so aimem refuses its team tools within the
handle's lifetime of at most 15 minutes, and the next run resumes the
session under a new generation, which ends the old handle at once.

The client drives attempt steps through its launcher, not with the session
token: `aicrew-agent step OP` asks the launcher of `AICREW_AGENT_HOME`
over a Unix socket in the agent home's private `state/` directory. The
launcher begins, sends through aimem's member CLI, and settles each
reservation step, keeping a nonsecret record of each step in flight until
it settles (`docs/DEVELOPMENT.md`, "Driving steps"). Neither the token nor
a proof crosses the socket.

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

For every step, step 2 is the acting member's ("Attempt steps"):
- the begin route returns the request;
- the member's own aimem connection sends it;
- settle learns the outcome from aimem's read scope, never from the
  member's report.

The store keeps its one-shot operations, which call aimem through a
mutating port, as internal test paths. No route reaches them, and they are
deleted in crew-execution b3 (D-b1b-6).

After a lost reply, aicrew queries the receipt with the same key and never
retries with a fresh key. While the receipt is unresolved, the attempt is
`RECONCILING` and no further transition is sent. Only a committed receipt
for the pending request and the expected hold, or a refusal that is not
retryable, is a known outcome. A transport error, a timeout, a retryable
refusal and a receipt that does not match the request are all unknown: the
attempt keeps the worker's capacity and reconciles. A reservation's
`coordination_proof` grants nothing by itself: aimem asks aicrew about it
before committing ("Coordination facts").
When the two stores disagree, aicrew conforms to aimem. For a member-driven
step, "aimem shows" means what the read scope shows for the step's proofs,
and "not committed" is known only once the read scope still shows nothing
10 s after those proofs ended. For a work update, "not committed" is known
only once the hold's fence or revision is seen past the request's, and its
keys still show nothing when read after that.

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
| Offer to a named worker | `OFFERING` → `OFFERED` | Claim with an external holder referencing the offer, under the coordinator's verified context. The coordinator's client sends it. |
| Accept | `ACCEPTING` → `RUNNING` | Transfer from offer to attempt, to the worker's verified context; the fence advances. The worker's client sends it. |
| Decline, withdraw or offer expiry | → `CLOSED` | Release under the current fence; the task returns to `READY`. The coordinator's client sends it. |
| Independent claim | `CLAIMING` → `RUNNING` | Claim with an external holder referencing the attempt, under the worker's own verified context. The claimer's client sends it. |
| Block | `RUNNING` → `BLOCKED` | Fenced work mutation recording the blocker; hold kept, its fence advanced by one. The holder's client sends it. |
| Submit result | `RUNNING` → `SUBMITTED` | Fenced work mutation to task `REVIEW`; result reference recorded; hold kept, its fence advanced by one. The holder's client sends it. |
| Review: return for rework | `SUBMITTED` → `RUNNING` | The worker, as holder, makes the fenced work mutation back to `IN_PROGRESS` when it resumes; the hold is kept, its fence advanced by one |
| Review: accept result | `SUBMITTED` → `ACCEPTED` | none; acceptance is not delivery |
| Finalize after human merge | `ACCEPTED` → `FINALIZED` | Finalize `DONE` with the confirmed delivery evidence as terminal evidence. The finalizer's client sends it. |
| Stop | `STOP_REQUESTED` → `STOPPED` → `CLOSED` | Release to `READY`, or `BLOCKED` with a recorded blocker, after the worker confirms the stop. The holder's client sends it, under the `stopped` fact. |
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
A team member confirms that evidence for the accepted result ("Confirmed
delivery" under "Attempt steps"), and the finalize's terminal evidence is
that confirmed record, never what the finalizer sends. Aicrew queries no
forge. Neither a reference, nor aicrew recording it, nor aimem storing it
proves that the referenced check passed: the confirming member vouches for
it. There is no cancellation shortcut:
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
offers it. The claimer supplies the task reference, its expected revision,
the base commit and branch, the project's selected process pin, and the
digest of the instructions it verified, which must match the pin. aimem
compares the expected revision and the pin with its own state when it
commits, so neither is trusted before then ("Process pins"). Recording the claim takes the claimer's one
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

When an offer is created, or an independent claim is recorded, aicrew
records the process pin on the attempt: the process repository, commit and
manifest, plus a digest of the role instructions the worker will receive. The worker receives and verifies that
pin before accepting; if the pinned assets or required skills are
unavailable, it declines with that reason instead of improvising. A running
attempt keeps its pin even if the project later selects a different process.
If the selection changes between offer and accept, acceptance is refused and
the offer is withdrawn and re-issued under the new pin; a recorded pin is
never updated. A matching instruction digest shows the worker has the
recorded instructions, not that it read or understood them. On the offer
route, the coordinator supplies the pin and the digest (D-b1(a), D-b1a-3):
- When aimem commits, it compares the pin in the offer's and the
  acceptance's coordination facts with the project's current selection, and
  refuses a different one as `process_mismatch`. So a stale pin never takes
  or moves a hold.
- The digest is not authoritative: a wrong one can only make acceptance
  fail.

**The instruction digest** is `sha256:` followed by the lowercase hex
SHA-256 of the process manifest's exact bytes at the pinned commit
(`git show <commit>:<manifest>`), with nothing normalized. The coordinator
computes it for an offer, and the worker computes it again from its own clone
before accepting; equal digests show both read the same manifest. The offer's
announcement carries the pin and the digest with the base commit, branch and
expiry, so the worker reads them from its inbox ("Inbox, receipts and
audit").

The independent claim works the same way: the claimer supplies the pin and
the digest, and the claim's fact carries the pin for aimem to compare.

A pin supplied by a member is unverified input until aimem commits a claim
under it (D-b1b-1). The attempt then records that committed claim receipt as
`process_verified_receipt`: the offer's claim for an offered attempt, and the
independent claim for a claimed one. An attempt whose claim was refused or
never committed keeps no verified receipt. So does an attempt recorded
before schema v16.

Every audit record carries the attempt's pin. The steps that start work
carry the pin in their coordination fact, so aimem confirms it is still the
project's selection when the hold is taken ("Coordination facts").

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
  transition they announce. Each names the attempt it announces
  (`attempt_id`), so a worker learns an offer's attempt ID from its own
  inbox, with nothing relayed; a member's own message names none. The
  offer's announcement also carries the offer (`offer`: `base_commit`,
  `branch`, `process {repo, commit, manifest}`, `instruction_digest` and
  `expires_at`), from which the worker verifies the pin and starts its
  worktree; no other message carries one.
- **Reading over the session API.** A member reads its inbox with
  `GET /v1/crew/inbox` and acknowledges with `POST /v1/crew/inbox/ack`, as
  its session token's session and generation ("Client session API"); its
  client does so through the launcher with `aicrew-agent inbox`.
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
  (reservation contract). Before an offer, the coordinator's client reads
  them over its own aimem connection, to avoid a useless offer:
  - a dependency it cannot read counts as open: unknown is not DONE;
  - an open dependency refuses the offer locally with `dependencies_open`,
    before anything is recorded or begun;
  - otherwise the offer's begin carries the evidence of that read (each
    dependency's ID, state and revision), which aicrewd records in the
    offer's audit. A recovered offer resends the evidence it began with.

  That read never makes a task eligible: aimem's refusal at the claim wins.
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
