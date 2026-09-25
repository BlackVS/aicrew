# Onboarding and identity-link authorization contract

Status: proposed design for review (task crew-onboarding design). Nothing
here is implemented. It fixes who may start and complete identity linking,
how invitations are scoped and redeemed, and how retries, expiry, revocation
and recovery behave. Wire encoding, code formats, the hub CLI and the client
bootstrap remain implementation reviews. Updated 2026-09-25.

Parent contracts, pinned:

- [AIForge split](https://github.com/BlackVS/aimem/blob/2b068c1b8f9f499fbae304c3ab6dab0be2dd509d/docs/DESIGN-AIFORGE.md)
- [Verified identity and session context](https://github.com/BlackVS/aimem/blob/2b068c1b8f9f499fbae304c3ab6dab0be2dd509d/docs/DESIGN-AIFORGE-CONTEXT.md)
  (the "context contract")
- [Crew contract](CREW-CONTRACT.md) and [workspace convention](WORKSPACE.md)

If this document and a parent contract disagree, the parent wins. Where this
design needs something the aimem contracts do not provide, it says so under
"Cross-project decisions" instead of assuming it.

## The model in brief

1. **No aicrew agent credential.** The context contract makes the individual
   aimem credential the only persistent agent secret: one per installation,
   issued and verified by aimem and never seen by aicrew. Aicrew therefore
   issues no persistent credential of its own. It recognizes an agent by a
   fresh identity proof and then issues only the short-lived session handle
   the context contract allows. "Before the agent has an aicrew credential"
   is simply the normal state.
2. **Authority to join comes from an invitation.** An operator issues a
   single-use invitation scoped to one team, role and hub. Whoever holds the
   invitation may start linking for it; completion also needs a valid aimem
   receipt, which only the holder of an aimem credential can obtain. No
   operator ever handles a receipt.
3. **Authority to re-prove comes from the identity itself.** An agent that is
   already linked proves again with its own aimem credential. That covers
   every session start and credential rotation. It needs no invitation.
4. **A different identity needs the operator.** Linking an existing agent to
   a different aimem user takes an operator-issued rebind invitation, and
   the agent's work must be reconciled first.

Challenges carry no authority (context contract). Anyone may ask for one;
what a completed proof may change is fixed by what the challenge was bound
to: an invitation or an existing agent record.

## Who may do what

| Action | Who | Authority |
| --- | --- | --- |
| Issue or revoke an invitation | operator | trusted operator caller |
| Start redeeming an invitation (get a challenge) | invitation holder | the invitation code |
| Obtain an aimem receipt | the client | its individual aimem credential, directly with aimem |
| Complete redemption | invitation holder | invitation code plus a receipt that aimem verifies |
| Re-prove a linked agent (session start, rotation) | the agent's client | a receipt for the already-linked aimem user |
| Rebind an agent to a different aimem user | invitation holder | operator-issued rebind invitation plus a receipt |
| Remove a membership or stop a session | operator | trusted operator caller (crew-core) |

The operator never receives, relays or submits a receipt, and never sees an
agent's aimem credential.

## Invitations

An invitation records:

- **Scope:** one team, one role, and the target hub (by stable hub ID).
- **Purpose:** `join` (a new or already-linked agent joins the team),
  `link` (an existing unlinked agent record is linked), or `rebind` (an
  existing agent record moves to a different aimem user).
- **Bindings:** a specific agent record (required for `link` and `rebind`),
  an expected aimem user ID the proof must match (required for `rebind`,
  optional otherwise), and an intended agent label for a new agent.
- **Lifecycle:** the issuing operator, an expiry (default 24 hours, at most
  72), a state (`issued`, `redeemed`, `revoked`, `expired`, `locked`) and an
  attempt counter.

The invitation code is a bearer capability:

- It has at least 128 bits of randomness, in a form a person can type, with
  a checksum. The exact format belongs to implementation review.
- It is shown once to the operator, and aicrew stores only a digest.
- The person at the client enters it at a hidden prompt. It never appears in
  command arguments, environment files, logs, Git, chat or task comments.
- It is single use: completion moves the invitation to `redeemed`.

Pinning the expected aimem user is recommended for `join` and `link`, and
required for `rebind`, which deliberately moves an existing agent to a new
identity. Without a pin, a stolen code can be redeemed by whoever holds it
and an aimem credential for the target hub. With a pin, a stolen code is
useless to anyone else. Every completion is audited, and observers show the
resulting link either way.

## Redemption

```text
operator        aicrew                   client                 aimem
   | issue(scope) -> code shown once        |                      |
   |-- code, privately -------------------->|                      |
   |               |<-- begin(code, key) ---|                      |
   |               |-- challenge (<=5 min) ->|                      |
   |               |                        |-- challenge + own ---->|
   |               |                        |   aimem credential     |
   |               |                        |<-- receipt (<=1 min) --|
   |               |<-- complete(code, challenge, receipt, key) ----|
   |               |-- redeem receipt (service to service) -------->|
   |               |<-- verified hub, user, token ------------------|
   |               | one local transaction: link, agent,            |
   |               | membership, invitation redeemed, audit         |
   |               |-- result (agent ID, membership; no secret) --->|
```

1. **Begin.** The client presents the code and a redemption key it
   generated. If the invitation is `issued` and unexpired, aicrew creates a
   challenge bound to its service ID, this invitation and the target hub,
   with a deadline of at most 5 minutes. A new challenge supersedes any
   earlier outstanding one for the invitation. Each begin counts as an
   attempt; after 5 the invitation becomes `locked`. A retry of a begin with
   the same redemption key is not a new attempt: it returns the recorded
   challenge while that challenge and the invitation are still usable, and
   is refused otherwise (see "Retries and lost replies").
2. **Proof.** The client sends the challenge to aimem with its own
   individual credential and receives a single-use receipt (context
   contract, steps 2 and 3).
3. **Complete.** The client presents the code, the challenge ID, the receipt
   and a completion key. Aicrew first checks that the invitation is still
   `issued` and that the challenge is its current, unexpired one. It then
   redeems the receipt with aimem and applies the binding rules below in one
   local transaction.
4. **Session.** Completion returns no secret. The client then starts a
   session through the normal proof (crew-context), which is when aicrew
   issues the first session handle.

### Binding rules at completion

| Check | Refused with |
| --- | --- |
| Verified hub differs from the invitation's hub | `identity_mismatch` |
| Invitation pins a user and the verified user differs | `identity_mismatch` |
| `join`: verified user already linked to agent X | not refused: X gains the membership, and no new agent is created |
| `join`: verified user not linked | new agent created with the intended label, then linked and given the membership |
| `link`: bound agent unlinked | link set, membership created |
| `link`: bound agent already linked to the verified user | membership created; link unchanged |
| `link`: bound agent linked to a different user | `identity_mismatch` (needs `rebind`) |
| `link` or `rebind`: verified user already linked to another agent | `identity_already_linked` (one user, one agent) |
| `rebind`: bound agent has outstanding work | `work_outstanding` |
| `rebind`: allowed | link replaced, old link kept in audit, every session of the agent ended and its generation advanced |
| Agent already an active member of the team with a different role | `role_conflict` (role changes are operator operations) |

One aimem user links to at most one aicrew agent, and an agent links to at
most one user. That prevents duplicate identities: a second invitation
redeemed by an already-linked user adds a membership to the same agent. The
link, the new agent (if any), the membership, the invitation's `redeemed`
state, the audit record and the receipt all commit in one transaction, or
none of them do.

## Retries and lost replies

| Lost or failed | Recovery |
| --- | --- |
| Begin reply | Retry with the same redemption key. If the recorded challenge is still pending and unexpired and the invitation is still usable, the same challenge comes back and no attempt is counted. If the challenge has expired or been superseded, the retry is refused as `challenge_invalid`; the client recovers by beginning again with a **new** redemption key, which counts one attempt and succeeds only while the invitation is valid and under its attempt limit. |
| Aimem receipt reply | Ask aimem for another receipt for the same challenge (context contract). |
| Aimem's reply to aicrew's redemption call | Aicrew retries the redemption with the same request key and gets the same result (context contract) before it commits or refuses anything. |
| Complete reply | Retry with the same completion key. The recorded result comes back without a second effect. |
| Complete retried with a new key after redemption | Refused as `invitation_invalid`. The client continues by starting a session through proof, since it is now linked. |
| Aimem unreachable, or it refuses the receipt | Nothing is created or changed. The invitation stays `issued` and the client may begin again until the attempt limit. |
| Client state lost before completion | Rerun the bootstrap with the same code. The new begin supersedes the old challenge. |

Unavailable or failed verification never creates or changes a link.

**Client recovery is automatic.** The future client bootstrap handles a
`challenge_invalid` refusal of a begin retry on its own: it generates a new
redemption key and begins again, without operator intervention. It stops and
reports only on `invitation_invalid` (the invitation is expired, revoked,
redeemed, locked or unknown), which needs a new invitation from the
operator. A redemption key identifies one begin and its retry receipt never
changes, so a new challenge always comes from a new key. This recovery rule
was accepted with the operator's merge of the redemption implementation
(aicrew PR #9).

## Expiry and revocation

- An invitation expires at its deadline. Begin and complete both refuse it
  afterwards, and a challenge never outlives its invitation.
- Challenges last at most 5 minutes (aicrew) and receipts at most 1 minute
  (aimem). Expired ones need a new begin.
- The operator may revoke an invitation at any time before it is redeemed.
  Revocation and completion serialize in the aicrew store, so exactly one
  wins. If revocation wins, a receipt aimem already consumed simply goes
  unused.
- Revoking a redeemed invitation changes nothing. To undo its effect, the
  operator removes the membership, which ends the session (crew-core).
- Public refusals use one non-disclosing `invitation_invalid` for unknown,
  expired, revoked, redeemed and locked invitations. The operator audit
  keeps the exact reason.

## Re-proof, rotation and recovery for linked agents

- **Session start.** The client asks for a challenge bound to its existing
  agent record and completes it with a receipt for the linked user. Aicrew
  starts the session only if the verified hub and user match the link.
- **Credential rotation.** A re-proof that returns the same hub and user but
  a new token ID is a rotation. Aicrew rebinds the agent's session to the
  new token ID and advances its generation, so old handles are fenced; work
  is reconciled, never released silently (context contract, rotation row).
  No operator action is needed.
- **New installation for the same agent.** Once aimem has issued that
  installation its individual credential, re-proof is a rotation, as above.
- **Lost aimem credential.** Recovery is aimem's authorized reissue. Aicrew
  offers no fallback: neither a session handle nor an invitation replaces
  the credential (context contract).
- **Suspected misuse.** The operator revokes unredeemed invitations, removes
  memberships and stops sessions. The aimem side revokes the credential.

## The client's view

1. The operator issues an invitation and passes the code to the person
   running the agent, privately.
2. On the client, that person runs the bootstrap command and enters the code
   at a hidden prompt.
3. The bootstrap checks the individual aimem credential (see decision D1),
   redeems the invitation, prepares the agent home (workspace convention),
   and reports ready, restart required, or blocked with instructions.

There is no hub-to-client SSH, no credential file to copy, and no receipt
passing through the operator.

## Cross-project decisions

These need aimem decisions or confirmation. None of them changes the merged
context contract.

- **D1: credential for a new installation.** The architecture says
  onboarding "links the existing aimem identity or creates one through an
  authorized flow", and that the client "receives its agent credential".
  The context contract defines verification of an individual credential but
  no flow that issues one to a fresh installation, or creates a new aimem
  identity, from an onboarding step. For one-step onboarding, aimem would
  need an authorized enrollment capability that the client redeems directly
  with aimem. It could be delivered with, or alongside, the aicrew
  invitation, and the credential would land in client-supported protected
  storage. **Until aimem decides**, the bootstrap requires an individual
  aimem credential already installed through aimem's own supported flow,
  and otherwise reports blocked with instructions. Owner: aimem (context
  implementation or a new enrollment task).
- **D2: one aicrew agent per aimem user.** This is aicrew policy, recorded
  so aimem knows that two installations of the same aimem user are one
  aicrew agent (a rotation, not a second agent). Separate agents need
  separate aimem users. It needs no aimem change.
- **D3: invitation-bound challenges.** Already allowed: step 1 of the
  context contract binds a challenge to "a pending invitation or existing
  agent record". No change.
- **D4: re-proof at session start.** This uses the context contract's proof
  and session-handle rules unchanged. Handle refresh without a new proof
  follows that contract's implementation review.

## Deferred

| Question | Owner |
| --- | --- |
| Wire API, code format, exact attempt and expiry limits | implementation reviews below |
| Aimem enrollment for new installations or identities (D1) | aimem |
| Session handle issuance, refresh and storage | crew-context with the context contract implementation |
| Hub CLI and client bootstrap surfaces | crew-onboarding implementation |

## Implementation tasks and crew-core 2b readiness

**crew-core 2b** (verified identity link) becomes READY when this contract is
merged. Its scope:

- challenge records bound to an invitation or an agent record;
- a verifier interface that redeems a receipt, with fake verifiers only in
  tests;
- the binding rules and the one-user-one-agent rule;
- re-proof for linked agents and rotation;
- refusal codes as above;
- no invitation store: 2b takes an invitation's already-validated scope as
  input.

**crew-onboarding** splits into:

1. **Invitations:** an internal store for issue, revoke, expire, lock and
   code digests, with operator policy. It depends only on crew-core.
2. **Redemption:** begin and complete, composing invitations with 2b in one
   transaction, plus the retry table. Depends on 1 and 2b.
3. **Client bootstrap:** hidden entry, credential check (D1 interim), agent
   home, readiness report. Depends on 2 and on its own surface review.

**crew-context** adds session start by re-proof and session handles, after
2b and the aimem context implementation (19b4).
