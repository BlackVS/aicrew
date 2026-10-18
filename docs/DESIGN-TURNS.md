# Turn economics design

Status: proposed design for review (task `01a13f80-1a20`). Nothing here is
implemented. The operator and the monitoring session review it on its pull
request before it is accepted, and the increments (section 14) are filed only
after that. Written 2026-10-15.

The model writes and decides; aicrewd drives the workflow. This document
amends [DESIGN-CONTROL-PLANE.md](DESIGN-CONTROL-PLANE.md):
- sections 1.5 (what a turn reads first), 1.7 (conversations across turns)
  and 1.8 (a worker's attempt across turns);
- sections 3.4 (limits) and 3.5 (token and cost accounting);
- the A1 board wake and the A6 forge events increments, which become
  sources of one event model (section 12).

Where the two disagree, this one wins once it is accepted. Everything else in
the control-plane design stands.

## 1. Governing principles

These are the operator's, verbatim. Every decision below names the
principle it serves.

1. If something can be automated, it must be automated. Using agents for mechanical work is a luxury.
2. Agents do what automates badly or with great difficulty.
3. The agent's tier matches the task's level.
4. Agents do every task in a separate session, so the context does not bloat.
5. The context at the start of a task is complete for the task but not bloated, and the agent can fetch more when it needs it, for example from the hub.
6. One task for an agent is one board task smaller than L, that is XS, S or M; L and XL are split before any agent takes them. A task that fits one fresh session is the unit the budget, the handoff and the review are sized for.

## 2. Evidence: the pilot cycle on #154

**The task.** Task `01a135fe-18e5` (P2/XS) asked for the start hint to name
`serve` for a home in turns mode. The change was about 40 lines, delivered
as pull request #154. The worker ran one attempt, `01a13896-c8b8`, with one
conversation across all 11 turns. The coordinator ran 13 turns in three
conversations.

**Sources.**
- Each home's `logs/turns.jsonl` and the per-turn transcripts in
  `logs/turns/`. Only the homes' logs were read; the homes were not changed.
- The hub's lifecycle messages, as named in each turn's trigger.

The times below are UTC. The prices are Claude Opus 5.5 at list, as Claude
Code reports them (`costBasis: list`): $4 per million input tokens, $20 per
million output tokens, $0.20 per million cache-read tokens and $8 per
million cache-write tokens (1-hour cache).

### 2.1 The headline figure was a counting error

The turn log's `cost_usd` values for the worker sum to **$46.34**. That is
not what the turns cost.

- **What the field holds.** On a resumed conversation, Claude Code's
  `result.total_cost_usd` is the running total of the whole conversation,
  not the cost of the turn.
- **What the launcher does with it.**
  - It copies the field into the turn record (`internal/agent/turnclaude.go`).
  - It then adds it to the attempt's spend and the day's spend after every
    turn (`st.spend` in `internal/agent/serve.go`).
  - So each turn counts every earlier turn of the conversation again.
- **The proof.** The differences between consecutive `cost_usd` values match
  each turn's own token counts at the list prices above, with an error under
  half a cent on every turn. The raw values do not fit any set of prices.

The real figures are:

| Member | Turns | Sum of logged `cost_usd` | Real cost |
| --- | --- | --- | --- |
| Worker | 11 | $46.34 | **$7.66** |
| Coordinator | 13 | $15.43 | **$4.98** |
| Both | 24 | $61.77 | **$12.64** |

The consequences:
- **Both attempt-budget holds were spurious.** The second came after the
  operator raised the attempt budget to $40. At the time of writing
  (12:20Z), the worker was held until 12:44Z as "attempt budget spent":
  the launcher had counted $46.34, while the worker had really spent
  $7.66.
- **The per-turn cap worked.** `turn_budget_usd` (default $2) is applied by
  Claude Code per invocation. No turn cost more than $2; the largest, turn
  9, cost $1.97 and was stopped by the cap, as its summary says. The turns
  that looked like $3 to $7 were running totals. This answers the
  `turn_budget_usd` question of `01a13ef0-157d`.

The defect is filed as `01a13f81-7df9` and is increment 0 in section 14.
The rest of this section uses the real per-turn figures.

### 2.2 The worker's turns

| # | Start | Min | Resumed | Trigger | Output | Cache read | Cache write | Real cost | What it did |
| --- | --- | --- | --- | --- | --- | --- | --- | --- | --- |
| 1 | 10-14 04:06 | 2.6 | no | offer | 10,605 | 1,773,356 | 78,960 | 1.20 | accepted, implemented, committed; blocked: `go test` denied |
| 2 | 10-15 05:22 | 8.9 | yes | escalation answer | 7,588 | 1,725,749 | 108,077 | 1.36 | ran the checks, pushed, opened the draft PR, local review, `review-this` |
| 3 | 05:31 | 0.3 | yes | continuation | 810 | 438,405 | 2,840 | 0.13 | nothing: the review had not started |
| 4 | 05:32 | 0.3 | yes | continuation | 635 | 335,810 | 2,142 | 0.10 | nothing |
| 5 | 05:32 | 0.4 | yes | continuation | 1,269 | 457,786 | 2,925 | 0.14 | blocked: "the review never started" |
| 6 | 05:34 | 2.2 | yes | forge event | 3,892 | 1,812,533 | 9,311 | 0.51 | blocked: CI failed, and it could not see which check |
| 7 | 05:36 | 0.7 | yes | forge event | 2,236 | 898,016 | 5,964 | 0.27 | blocked: the review was posted, and it could not read it |
| 8 | 07:01 | 0.5 | yes | forge event | 1,768 | 535,531 | 135,641 | 1.23 | blocked: CI green, and the verdict was still unreadable |
| 9 | 10:33 | 7.7 | yes | operator notes | 7,093 | 2,830,694 | 157,730 | 1.97 | rebased on main, aligned with #155, pushed; stopped by the $2 cap |
| 10 | 10:40 | 3.5 | yes | continuation | 4,938 | 2,127,944 | 11,447 | 0.62 | checks, delta review, labels at the new head |
| 11 | 10:44 | 0.2 | yes | continuation | 554 | 511,322 | 2,480 | 0.13 | nothing: waiting for the review |

### 2.3 The coordinator's turns

| # | Start | Resumed | Cache write | Real cost | What it did |
| --- | --- | --- | --- | --- | --- |
| 1 | 10-13 16:00 | no | 60,249 | 0.62 | could not read the READY task (fixed in 0.5.7) |
| 2 | 16:01 | yes | 3,283 | 0.11 | the same; acknowledged |
| 3 | 10-14 04:05 | no | 66,153 | 0.70 | read the task, offered it |
| 4 | 04:06 | yes | 2,904 | 0.08 | acknowledged the acceptance |
| 5 | 04:08 | yes | 8,400 | 0.28 | escalated the worker's `go test` block |
| 6 | 10-15 05:22 | no | 60,815 | 0.58 | acknowledged the escalation's answer |
| 7 | 05:23 | yes | 2,825 | 0.08 | acknowledged the resume |
| 8 | 05:32 | yes | 6,887 | 0.24 | escalated "is hands running?" |
| 9 | 05:35 | yes | 3,955 | 0.13 | acknowledged; its escalation was now moot |
| 10 | 05:36 | yes | 8,373 | 0.29 | escalated "which CI check failed?" |
| 11 | 05:36 | yes | 6,281 | 0.20 | escalated "what does the review say?" |
| 12 | 07:01 | yes | 92,996 | 0.82 | nothing new; one escalation now moot |
| 13 | 10:33 | yes | 96,688 | 0.85 | acknowledged the resume |

### 2.4 What the numbers show

1. **About three fifths of the spend went to turns that wrote nothing.**
   - The worker's four writing turns (1, 2, 9 and 10) cost $5.15.
   - Its three waiting turns cost $0.36, and its four blind blocks cost
     $2.15. In those blocks the launcher already held the facts the model
     lacked.
   - The coordinator's whole $4.98 went on reading, acknowledging and
     relaying. Its turns made one offer, wrote no code and reviewed
     nothing.
   - Counting both members, $5.15 of $12.64 was writing (41 %).
2. **A hold rewrites the conversation.**
   - Claude Code caches a conversation for one hour. After any gap longer
     than that, the next resumed turn writes the whole conversation to the
     cache again.
   - That happened four times: worker turns 8 and 9, coordinator turns 12
     and 13. The rewrites came to 483,055 tokens, $3.86, or 31 % of the
     cycle.
3. **A resumed turn pays for its size several times over.** Every model
   call in a turn reads the whole conversation from the cache.
   - At 130 to 180 thousand tokens, even a turn that did nothing read 0.3
     to 0.5 million tokens.
   - The writing turns read 1.7 to 2.8 million.
   - Cache reads are cheap at $0.20 per million, but they grow with the
     conversation, not with the task.
4. **A fresh start is not free either.** The fresh conversations (worker 1,
   coordinator 1, 3 and 6) each wrote 60 to 79 thousand tokens to the cache
   before doing anything: $0.48 to $0.63 per start. This is the client's own
   prompt plus START.md, ROLES.md, the handoff, the session status and the
   inbox.
   - Stateless turns at today's starting context would cost about $0.55
     each before any work.
   - Under one dollar per routine turn therefore needs a lean starting
     context (principle 5) as much as it needs fresh sessions (principle 4).
5. **Every wait cost a turn, and every turn waited for a human.**
   - Waiting turns 3, 4 and 11 checked whether the external review had
     started, a fact the launcher already polls.
   - Blocks 5 to 8 asked the coordinator for facts it could not read
     either.
   - The coordinator relayed them to the architect, and the operator
     answered by hand.
   - The cycle took 30 hours of wall time and needed the operator four
     times: the escalation answer, the budget raise and two nudges.

## 3. Stateless turns (decision T1)

**Decision.** Every turn starts from a fresh context, with no conversation
resume between turns. The memory is:
- the attempt's handoff;
- the attempt's state on the hub;
- the repository: the branch, its commits and the pull request.

This amends 1.7 (D4) and 1.8.

**Why.**
- Principle 4: no context bloat across turns.
- Section 2.4 points 2 and 3: a resumed turn pays for the whole
  conversation, and after a hold it pays to rewrite it.
- With stateless turns, a turn's cost depends on its task, not on how many
  turns came before it.

**What the turn starts with.** The launcher writes a **turn brief** into the
prompt itself, so the model does not spend tool calls to find its bearings.
The brief holds:
- **Who and where:** role, team, the attempt ID and its phase, the
  repository, the branch, the worktree path and the base commit.
- **The task:** title, objective, acceptance criteria and non-goals, as the
  offer froze them, and the process pin.
- **Why this turn:** the trigger. For an "action needed" message (section 5)
  that is its full content: review bodies, check facts and the coordinator's
  question.
- **The pull request:** number, head, labels, draft state, and the review
  verdicts and CI conclusion at the head, from the launcher's last read.
- **The handoff:** the attempt handoff (below), in full.
- **The ledger:** one line per earlier turn of the attempt (below).
- **Where to fetch more:**
  - `aicrew-agent inbox`;
  - `forge pr show` with comments and checks (01a13ef0-1547);
  - the board;
  - ROLES.md, by section.

START.md and ROLES.md are no longer read in full at every start. The brief
names the role's rules that apply to this turn kind (section 6), and the
model reads more when it needs to (principle 5).

**The attempt handoff.** This is `state/attempts/<attempt>/HANDOFF.md`, one
file per attempt. It is distinct from the member's own `docs/HANDOFF.md`.
- **The model writes** these sections, and the turn's last action updates
  them:
  - **Done:** what was done, by commit.
  - **Decisions:** decisions and their reasons, including any the reviews
    settled.
  - **Next:** the next step, concrete enough for a fresh session to take it.
  - **Open questions:** those still open, and who owns each.
- **The launcher appends** one ledger line after each turn: turn ID, start,
  outcome, real cost, head commit before and after, the steps taken, and the
  first sentence of the summary.
  - The ledger is the only part of the file the launcher writes.
  - A turn that ends without updating the model's sections gets a ledger
    line marked `handoff not updated`. Two such turns in a row count as a
    stall (1.8).
- **The file is bounded:** at most 8 KiB for the model's part and 50 ledger
  lines.

**The coordinator** starts every turn fresh in the same way. Its brief
carries the team state, the action-needed content and its own
`docs/HANDOFF.md`. The rotation point of D4 goes away.

**The target.** A routine turn costs under one dollar at list prices.
- A fresh start at about 25 thousand tokens of new context (the brief and
  the role rules for the turn kind) costs about $0.20 to write.
- Writing turns of the size of turns 1 and 2 stay near $1.20, because
  writing code is the work. Splitting them is the job of principle 6, not of
  the turn loop.
- Increment 3 measures the starting context first, in TURN-PROBE, before
  any code changes.

## 4. The harness performs the mechanical steps (decision T2)

**Decision.** The steps that follow from facts are performed by the
launcher, or by the runner after B2, with the member's own credentials. No
model turn is involved. aicrewd owns the policy and the attempt's state, the
launcher reads the forge and acts, and the model is woken only for
judgement.

**Why.**
- Principles 1 and 2.
- Section 2.4 point 5: seven of the worker's eleven turns, and most of the
  coordinator's, were about facts.

### 4.1 The facts

The launcher keeps, per attempt, what it reads from the forge and the
worktree:
- the head commit, and whether the branch is behind its base;
- the patch identity against the merge base;
- the draft state and the `WIP:` prefix;
- the labels;
- the local review at the head: a comment by the member's forge identity,
  headed `[oh-code-review (high) review] reviewed at head <sha>`, and its
  verdict;
- the external review at the head: the `[hands-bot review]` comment naming
  the head, and its verdict and findings;
- the CI conclusion per check at the head, and for a failed job its name
  and a bounded excerpt of its log;
- whether the pull request is mergeable, and whether it is merged, with the
  merge commit.

These are A6's observations, extended with the comment bodies and the
check runs (`01a13ef0-1547`, and the check-run pages of `01a1301d-55e3`).

### 4.2 The state machine

The pull request of a `code-pr` attempt moves through these states. A
transition is either the **harness's** (no model turn) or the **model's**,
and then it is an action needed (section 5).

| State | Leaves when | To | Who |
| --- | --- | --- | --- |
| `writing` | a commit is pushed and the draft PR is open with the `WIP:` prefix | `checking` | model |
| `checking` | the checks the process declares at `after push` pass at the head (section 9) | `self-review` | harness |
| `checking` | a check fails | `writing`, with the failure as action needed | harness |
| `self-review` | the local review at the head is posted with READY_FOR_HUMAN_MERGE | `external-review` (adds `review-this`) | model writes it, harness moves on |
| `self-review` | the local review finds a BLOCKER | `writing` | model |
| `external-review` | `hands-reviewed` lands with the verdict at the head | `gates` | harness |
| `external-review` | the review fails to run (`hands-review-failed`, or the `could not run` comment) | `external-review`, with `review-this` added once more | harness |
| `external-review` | no pickup within 30 minutes, or a second failure | unchanged; a status line for the operator (`aicrew status`) and one message to the coordinator | harness |
| `gates` | the external verdict is READY and CI is green at the head | `ready`: drop `WIP:`, mark ready, submit the attempt | harness |
| `gates` | the external review has a BLOCKER | `writing`, with the review as action needed | harness |
| `gates` | CI failed because of the runner: cancelled, lost runner, or setup timeout | `gates`, after re-running the failed jobs once | harness |
| `gates` | CI failed on the change | `writing`, with the job and its excerpt as action needed | harness |
| any open state | the branch is behind its base | merge the base into the branch; if clean and the patch identity is unchanged, push and post the delta note: the reviews stand | harness |
| any open state | that update conflicts, or changes the patch identity | `writing`, with the conflict as action needed | harness |
| any state after `writing` | a new head that is not a base-only update | `checking`; clear `hands-reviewed`, restore `WIP:` and draft | harness |
| `ready` | merged | `delivered`: verify the merge tree equals the reviewed head, wait for post-merge CI, record the delivery evidence | harness |
| `ready` | the coordinator or the operator asks for a change | `writing`, with the request as action needed | harness |

The rules the harness follows are the review gates the members follow today
(the global PR lifecycle and this repository's AGENTS.md): WIP and draft
until both reviews and CI are green, plain `review-this`, no push while
`hands-reviewing` is present, and the patch-identity rule for a base-only
update. They do not change. They move from the model's instructions into
scripts that can be tested.

**Where it runs.** A transition marked "harness" is one of the default
automations of 13.1: a Starlark handler (12.3), run by the core on an event.
- Until B2, it runs in the launcher, with the member's forge credential, as
  A3 did for the turn loop, and it reports each transition to aicrewd as a
  fact on the attempt.
- After B2, it runs in the runner, and aicrewd routes the events.
- The facts, the bus and the sandbox are core; the rules in the table are
  behaviour (section 13).

### 4.3 Today's steps, one by one

| Step in the #154 cycle | Turn | Moves out of the model | Principle |
| --- | --- | --- | --- |
| Run `go test`, `go vet`, `gofmt` before the push | 2, 10 | yes: the process declares them at `after push`, the repository holds them, and the harness runs them (section 9) | 1 |
| Push, open the draft with `WIP:` | 2 | no: the first push of new work ends the writing turn, and the model names the PR | 2 |
| Post the local high review | 2, 10 | no: a review is judgement | 2 |
| Add `review-this` after the local review | 2 | yes | 1 |
| Wait for the review to start | 3, 4, 11 | yes: no turn while a fact is pending | 1 |
| Report that the review did not start | 5 | yes: the 30-minute rule above, to the coordinator and the operator, not as a block | 1 |
| Find which CI check failed | 6 | yes: the job and its excerpt go in the action-needed message, or the harness re-runs a runner failure | 1 |
| Read the external review | 7, 8 | yes: the verdict is a fact; a BLOCKER's body goes in the message | 1 |
| Update the branch from main, post the delta note, re-request the reviewer | 9, 10 | yes when clean with the same patch identity (worked example in 10.4); no on a conflict. The worker re-derived this procedure four times on 10-15, which is the case section 11 describes | 1, 2 |
| Align with #155 after the rebase | 9 | no: a semantic change is judgement, and the reviewers and CI catch a drift, as for a human pull request | 2 |
| Clear `hands-reviewed`, re-add `review-this` after a new head | 10 | yes | 1 |
| Delta review at the new head | 10 | yes after a base-only update (the reviews stand); no after an edit | 1, 2 |
| Drop `WIP:`, mark ready, submit | (never reached) | yes | 1 |
| Acknowledge a lifecycle message that needs no action | coordinator 4, 6, 7, 9, 13 | yes: messages of a kind the role takes no action on are acknowledged by the launcher | 1 |
| Offer the READY task | coordinator 3 | yes: the offer policy (section 8) | 1 |
| Relay the worker's block to the architect | coordinator 5, 8, 10, 11 | yes for missing facts (gone with section 5); no for a real decision | 1, 2 |

## 5. One "action needed" message, with the content (decision T3)

**Decision.**
- aicrewd aggregates the pull request's state from the launcher's facts. It
  wakes the worker only when a transition in 4.2 is the model's.
- The wake is one message of a new kind, `action.needed`, and it carries
  everything the decision needs:
  - **A review with a BLOCKER:** the review's body and its findings, with
    the head it names.
  - **CI failed on the change:** each failed job's name, its conclusion and
    a log excerpt of at most 8 KiB, cut to the first error and its context.
    Where the forge offers no logs, the message says so.
  - **A failed check before the push:** the command and its output, bounded
    the same way.
  - **A conflict in the update from the base:** the base, the conflicting
    files and the conflict hunks, bounded.
  - **A question or a change request** from the coordinator or the
    operator: the text.
- No member reads the forge to learn the status. `forge pr show` gains the
  comments and the check runs (`01a13ef0-1547`), so a model that needs more
  can fetch it (principle 5), but the message is enough for the decision.
- Messages that need no judgement wake no model:
  - `forge.event`s that only report progress;
  - an acceptance;
  - a resume;
  - an answer the attempt already reflects.

  They update the attempt's facts, and the launcher acknowledges them.

**Why.**
- Principles 1, 2 and 5.
- Section 2.4 point 5: blocks 5 to 8, and the coordinator's relays, existed
  only because the facts were not in the message.

**What it absorbs:**
- `01a13ef0-1547`: forge pr show returns the content.
- `01a1301d-55e3`: poll within the wait, the true watch state, check-run
  pages.
- `01a13ef0-158a`: the block reason and time on the attempt become two of
  its facts.

## 6. Model tiers per turn kind (decision T4)

**Decision.** The launcher picks the model for each turn from the turn's
kind and the member's role, using the rule below. The kind follows from the
trigger, so no model chooses it.

| Role | Turn kind | Trigger | Tier |
| --- | --- | --- | --- |
| worker | implement | an accepted offer (the harness accepts after the digest check) | strong |
| worker | fix | `action.needed`: a BLOCKER, a failed CI on the change, a failed check, a conflict in the update from the base | strong |
| worker | self-review | the state machine's `self-review` | strong |
| worker | answer | `action.needed`: a question or change request | strong |
| coordinator | review | a submitted attempt, when the team's process asks the coordinator to review | strong |
| coordinator | decide | a block that needs a decision, or an escalation to draft | strong |
| coordinator | triage | a board change that needs reading, such as a task to size or split (principle 6) | small |
| any | report | a report or summary the operator asked for | small |
| any | inbox | a message of a kind not listed above | small |

- **Strong and small** are two model names in the member's provider profile
  (A5), `models.strong` and `models.small`. Until A5, they go in the home's
  turn settings, where the `model` field exists today.
- **No fallback.** A small turn that finds it needs the strong model ends
  with the outcome `needs_strong` and a one-line reason. The next turn runs
  strong, from the same brief. Nothing falls back to another provider or
  credential (WORKSPACE).
- **Prices.** The rule does not depend on them. Increment 4 records each
  tier's measured cost per turn kind in TURN-PROBE.

**Why.** Principle 3. Turns that write or judge code run on the strong
model. Reading and routing, which are most of the coordinator's turns in
2.3, run on the small one.

## 7. Cost as a first-class figure (decision T5)

**Decision.**
- **The real cost per turn** is recorded, after increment 0. aicrewd adds it
  up per attempt, per task, per member and per day (3.5, A4).
- **Where it shows:**
  - `aicrew status` and `aicrew team state` show each open attempt's spend
    against its budget.
  - At delivery, the harness writes the attempt's cost, its turn count and
    its model mix into the task's evidence.
- **The budget follows the task's size.** The operator sets one attempt
  budget per size, as a team setting, and it is the admission threshold
  (DESIGN-CONTROL-PLANE 3.4, `01a11f23-2af8`). These defaults are
  provisional until increments 1 to 3 have run on real tasks:

  | Size | Attempt budget |
  | --- | --- |
  | XS | $4 |
  | S | $8 |
  | M | $16 |

  L and XL get no budget, because no agent takes them (principle 6, section
  8).
- **A spent budget is announced.** A spent attempt or daily budget, or a
  provider limit, becomes:
  - one lifecycle message to the coordinator;
  - a line in `aicrew status` for the operator, with the amount, the limit
    and the next check.

  It is not repeated on every poll. This absorbs `01a13ef0-1572`, which
  carries the "a worker's limit reaches the coordinator" half of
  `01a13098-79d5`.

**Why.**
- Principles 1 and 6: a budget sized to a task that fits one fresh session
  is a real control.
- The holds of 2.1 were invisible until the operator looked, and they were
  wrong.

## 8. The coordinator under the same principles (decision T6)

**Decision.** Offering, accepting and delivering follow fixed policy and
run in aicrewd and the harness. The coordinator's model turns are for
judgement only: reviews, decisions on a block, and escalations.

- **Offer policy (aicrewd):** a READY task with no open attempt is offered
  to a free worker of the team when:
  - its size is XS, S or M;
  - its project is granted to the team;
  - the worker has the capability the task requires (E3).

  The offer carries the process pin, as the coordinator's offer does today.
  A task sized L or XL is never offered. It is sent to the coordinator as a
  `triage` turn to propose a split, which the architect or the operator
  accepts on the board (principle 6).
- **Acceptance (harness):** the worker's launcher verifies the instruction
  digest and accepts. A mismatch is an action needed for the coordinator.
- **Delivery (harness):** the `ready → delivered` transition of 4.2,
  including the evidence on the task.
- **What remains for the coordinator's model:**
  - the review of a submitted attempt, when the team's process asks for it;
  - a block that needs a decision, with the facts attached by aicrewd, not
    fetched;
  - an escalation to the architect, when the decision is not the team's.

**Relation to `01a13ef0-155e`.** That task asked the coordinator to read the
pull request with its own credential and relay the facts. Under T3 the
facts arrive in the message, so there is nothing to read or relay. What
remains of 155e is that a worker's block needing a decision wakes the
coordinator's `decide` turn, with the facts. Increment 1 absorbs it, as a
default subscription with that turn (13.2).

**Why.**
- Principles 1 and 2.
- In 2.3, the coordinator's 13 turns made one offer and 12 reads,
  acknowledgements and relays.

## 9. Where automations live (decision T7)

The operator's question was: when a step moves out of the model's turn,
where does the code that performs it live? There are three layers, each with
its own language and its own review path.

| Layer | What it holds | Language | How it changes |
| --- | --- | --- | --- |
| 1. The harness | aicrewd, the launcher (the runner after B2), the installers | Go | a pull request on aicrew, then a release |
| 2. The process | which command or script runs at which point, per project | the process manifest (data), and workflow scripts in Starlark (10) | a pull request on aiskills, independent of a harness release |
| 3. The repository | the commands themselves, and the project's own workflow scripts | bash under `scripts/`; Starlark for workflow scripts | a pull request on the repository, under its own gates |

### 9.1 The harness

The harness executes no foreign native code. What it runs from the other
layers is a declared command under the allow list, or a reviewed script in
its sandbox. It is the only layer that is Go, and it offers outward a fixed set of **extension points** on the state machine
of 4.2:
- `after push`: a new head, before the self-review;
- `after CI`: CI concluded at the head, whatever the conclusion;
- `ci failed`: the change failed CI, before the action-needed message;
- `both gates green`: the reviews and CI are green at the head, and no
  external review is in flight;
- `before submit`: the last point before the harness submits;
- `after merge`: before the delivery is recorded.

At a point, the harness does one of two things.
- **It runs a declared command.** The command runs under the member's own
  account, in the attempt's worktree, within the turn's allow list. A
  command outside the list is refused, never widened. The run is recorded
  as a step of the attempt, with the command, its exit status and a bounded
  excerpt of its output. That step is the fact the state machine moves on,
  and a failure is the content of an action-needed message (section 5).
- **It runs a declared workflow script** in the embedded interpreter of
  section 10, with the host API of 10.3 and nothing more.

The points, their order and what a failure means are product: they are
reviewed and released as Go.

### 9.2 The process

The process says when and what. The process manifest that the offer pins
from the aiskills repository (`processes/<name>/manifest.json`) declares,
per extension point, the commands or scripts to run. For example:
- `after push: scripts/check-repo.sh`;
- `after CI: automations/base-update.star`.

The manifest is data, not code:
- a change is a reviewed pull request on aiskills;
- it reaches a team through the pin, independent of a harness release;
- a team on an older pin keeps its older declarations.

A workflow script the process ships lives beside the manifest, under the
same pin. The manifest's schema for the points is an aiskills change, filed
there as an issue when increment 2 starts.

### 9.3 The repository

The repository holds its commands in its own scripting, under its own
gates.
- **Commands are bash under `scripts/`,** as today. The `dev-go` toolchain
  list already admits them, and Git Bash provides bash on every Windows
  home.
- **There are no PowerShell-only, Python or Node automations.** Each would
  add a runtime to every member machine, or bind the project to one
  platform.
- **A small Go tool under `tools/`,** built with `go build`, is the
  exception, for a step bash cannot do portably.
- **A project's own workflow scripts** live under `automations/` as
  Starlark (section 10).

### 9.4 What follows

- **Nothing runs without a reviewed pull request.** The harness changes by
  its release, the process by its pin, and the commands and scripts by the
  repository's merged history.
- **A workflow script is loaded from the base, never from the head.** The
  harness loads a script from the pinned process or from the base branch's
  merged commit, never from the attempt's own head. A worker therefore
  cannot change the script that acts on its own pull request.
- **The checks are different.** A check declared at `after push` runs the
  head's code, as CI does, because testing the change is its purpose.
- **Nothing of layers 2 and 3 executes inside aicrewd as native code.** A
  workflow script runs in the interpreter, under its limits (10.3).
- **How 4.3's checks fit the layers.** The `go test`, `go vet` and `gofmt`
  of 4.3 are layer 3 commands, declared at `after push` by layer 2 and run
  by layer 1. Today's per-team toolchain list (finding #18) stays the allow
  list that permits them, and the manifest decides which run at which
  point.

**Principles.** 1, because the mechanical steps run without a model. 2,
because the harness stays small and generic while each project declares its
own steps.

## 10. The language of workflow automations (decision T8)

The operator's preference, evaluated here as a proposal rather than
settled, is that repository commands stay bash and workflow automations at
the extension points are Starlark. The decision is to **adopt it, with one
correction about memory limits (10.3).**

### 10.1 Why not shell for workflow steps

A workflow step reads the pull request's state, decides, acts and records.

- **In shell, that is a script with the world in reach.** It has the
  network, every file of the account, every credential the account can
  read, and every program on the path. What it may touch is whatever the
  allow list happens to admit.
- **Its results are hard to bound and record.** Its state comes back as
  text the script parses.
- **It does not test well.** A shell workflow can be tested only against a
  real forge or a heavy fake.

An embedded language inverts this. The script sees only what the host
hands it. Every action goes through a host function, which checks the
allow list, bounds the output and records the step. The same script runs in
a test against a fake host.

### 10.2 Starlark, Lua and Tengo

| | Starlark (`go.starlark.net`) | Lua (`gopher-lua`) | Tengo |
| --- | --- | --- | --- |
| Access to files, network, the system | none built in; only what the host passes | the `os` and `io` libraries exist and must be left unopened, per state | none unless the host imports a module |
| Determinism | by design: ordered dicts, no clock or randomness, values frozen after load | table iteration order unspecified; clock in `os` | map iteration order follows Go's, so it is not stable |
| Time limit | an execution-step limit and a cancel per thread | context cancellation | context cancellation |
| Memory limit | none built in (10.3) | none built in | a cap on allocations per run |
| Who reads and writes it | Python-like: agents write it well, and reviewers read it as Python | Lua 5.1; less familiar to both | Go-like, but its own language |
| Maturity | Bazel, Buck2, Tilt, Copybara; Google maintains the Go implementation | widely used, one main Go implementation | smaller user base, mostly one maintainer |

**Starlark wins on the three points that matter here.**
- **It is hermetic by construction,** where Lua is hermetic only by
  configuration: a forgotten `OpenLibs` hands a script the file system.
- **It is deterministic,** so the same facts give the same actions, and a
  script that misbehaved once can be replayed in a test.
- **Agents write it fluently and people review it easily,** because it reads
  as a small subset of Python.

**What Starlark lacks:**
- a memory limit, which Tengo has;
- exceptions: `fail()` ends the script;
- unbounded loops: there is no `while` and no recursion unless enabled, and
  the host leaves both off.

The missing loops are a feature for workflow steps. The memory limit is
covered by the host (10.3).

### 10.3 The host API and its limits

A script defines `main(event)`, and the core calls it at the declared
point or for a subscribed event (12.3). `event` holds the kinds and IDs of
the events the run covers. `main` returns `None` when the event is handled,
or a `turn(...)` value when judgement is needed (12.3). Anything worth
recording on the way goes through `step`. The host gives the script these values and
functions, and nothing else.

| Name | What it does |
| --- | --- |
| `pr()` | The pull request's state from the launcher's facts (4.1): `number`, `url`, `head`, `base`, `branch`, `behind`, `draft`, `labels`, `mergeable`; `reviews`, local and external, each with its head, its verdict, its findings and its body (bounded to 16 KiB); `checks`, each with its name and conclusion, for a failed job its log excerpt (bounded to 8 KiB, as in section 5), and `runner_failure` when the failure is the runner's, not the change's. |
| `attempt()` | The attempt's state: `id`, `phase`, `task` (`id`, `size`), `base_commit`, `spent_usd`, `budget_usd`. |
| `run(cmd, args)` | Runs one command, with an argument list and no shell. It runs in the attempt's worktree, under the member's account, within the turn's allow list. It returns `exit` and `output`, the output bounded to 64 KiB. A command outside the allow list fails the script. |
| `step(kind, text)` | Records a step of the attempt. A kind the state machine knows, such as `update_conflict`, moves it, for example to an action needed. |
| `comment(body)` | Posts a comment on the pull request, with the member's forge credential, bounded to 16 KiB. |
| `label(add, remove)` | Adds and removes labels on the pull request, with the member's forge credential: at most 8 names per list, the removals first (increment 2d-c, decision Q1: a structured API, not parsed CLI output). |
| `ready()` | Drops the pull request's draft flag and its `WIP:` prefix. |
| `submit(url)` | Submits the attempt with `url` as its result. It is the holder's own work step, sent by the launcher once the script ended without failing and after `before-submit` ran, so aicrewd's fences apply unchanged; a run at `before-submit` itself cannot submit. |
| `rerun()` | Re-runs the failed jobs of the head where the forge has a call for it, once per head (decision Q4), and returns `reran`, `jobs` and `reason`. |
| `turn(kind, content)` | A value, not an action: returned from `main`, it asks for a model turn of that kind with that content (12.3). It has no side effect. |

There is no file access, no network, no clock and no environment. Values
are frozen between calls.

**The limits:**
- **Time.** At most 1 million execution steps, and a wall-clock cancel at 2
  minutes. A `run` counts against the wall clock, with its own timeout
  inside it.
- **Memory.** `go.starlark.net` has no memory cap, and a single step such as
  a string repetition can allocate a lot. The script therefore runs in a
  child process of the launcher, `aicrew-agent automation run`, under an OS
  memory limit: a job object on Windows, as the turn's client runs today,
  and an rlimit on Linux and macOS. The host API reaches the parent through
  the same step socket the turns use. This corrects the preference's "runs
  under memory limits": the language does not provide them, so the process
  does.
- **Output.** Every host result is bounded, so a script's view of the world
  stays small.
- **A failed script** (a `fail()`, a limit, or a refused command) is
  recorded as a step and becomes an action needed for the coordinator. It
  is never retried in a loop.

### 10.4 Worked example: the base update

This is today's "branch behind main, patch identity unchanged" case as a
script at `after CI`. It updates the branch by merging the base, so no
force-push is needed. The review rule's patch identity is taken against
the merge base, so a clean merge of the base leaves it unchanged. The
repository's `scripts/patch-identity.sh BASE` prints that identity
(`git diff $(git merge-base BASE HEAD) HEAD | git patch-id --stable`),
which keeps the pipe in a reviewed layer 3 command.

```python
# automations/base-update.star: at "after CI".
# A pull request behind its base is brought up to date when the update
# leaves the patch identity unchanged; the reviews then stand (the
# base-only rule). Anything else goes to the worker as an action needed.

NOTE = """Base-only update: {old} -> {new}, merged from {base}.
Patch identity unchanged: {id}
(git diff $(git merge-base {base} HEAD) HEAD | git patch-id --stable)
The reviews at {old} stand; CI runs on {new}."""

def identity(base):
    r = run("bash", ["scripts/patch-identity.sh", base])
    if r.exit != 0:
        fail("patch identity: " + r.output)
    return r.output.strip()

def main(event):
    p = pr()
    if not p.behind or "hands-reviewing" in p.labels:
        return None  # up to date, or the reviewer is reading: a later event retries
    base = "origin/" + p.base
    before = identity(base)
    if run("git", ["fetch", "origin", p.base]).exit != 0:
        fail("fetch " + p.base)
    merge = run("git", ["merge", "--no-edit", base])
    if merge.exit != 0:
        run("git", ["merge", "--abort"])
        step("update_conflict", "merging %s into %s conflicts" % (p.base, p.branch))
        return turn("fix", "Updating %s from %s conflicts:\n%s" % (p.branch, p.base, merge.output))
    after = identity(base)
    if after != before:
        run("git", ["reset", "--keep", p.head])
        step("update_changed_patch", "identity %s became %s" % (before, after))
        return turn("fix", "Updating from %s changes the patch identity; update the branch by hand." % p.base)
    new = run("git", ["rev-parse", "HEAD"]).output.strip()
    if run("git", ["push", "origin", "HEAD:" + p.branch]).exit != 0:
        run("git", ["reset", "--keep", p.head])
        step("update_push_refused", "the branch moved during the update")
        return None  # the new head is an event of its own
    comment(NOTE.format(old = p.head, new = new, base = base, id = after))
    step("base_update", "%s -> %s, identity %s" % (p.head, new, after))
    return None
```

The script is about 40 lines, and a reviewer reads it in a minute.
- **What it calls:** the host calls `pr`, `run`, `step` and `comment`. It
  returns `None` when the update is done or not needed, and `turn("fix", ...)`
  with the conflict when the worker must decide.
- **What `run` needs:** `bash scripts/...` and five `git` subcommands, all of
  which the allow list must admit, and the reviewer sees each one.
- **How it is tested:** with a fake host in Go. The tests cover the behind
  and up-to-date cases, a conflict, a changed identity, a refused push and
  `hands-reviewing`, with no forge and no git.

**Principles.** 1 and 2: the script performs a step that needs no judgement,
and a person read every line of it before it could run.

## 11. From a repetition to an automation (decision T9)

**Decision.** An agent that notices it re-derives the same procedure does
not automate it in its turn. It files a task "automate X" with the
evidence, and the next turn goes on with the procedure by hand.

**The example.** On 10-15 the worker re-derived "update the branch, post the
delta note, re-request the reviewer" four times. Section 4.2 now makes that
a harness transition, and 10.4 shows it as a script. The rule is how the
next such procedure gets found without an operator reading the logs.

**What the task carries:**
- where the repetition happened: the turns, attempts or tasks, by ID;
- how often it happened;
- the level it belongs to, from the three below, and why.

**The three levels:**

| Level | When | Where it goes |
| --- | --- | --- |
| A skill | a way of acting that still needs judgement | ai-skills: a skill, or a change to one, through an issue on the aiskills repository |
| A repository tool | deterministic, for one project | the repository: a bash script, a `tools/` program or an `automations/` script (9.3), declared at its point in the process manifest (9.2) |
| A harness feature | it recurs across projects, which makes it product | aicrew: a task on the board, then a release (9.1) |

**Why the turn does not automate it.**
- The turn's job is its task. An automation written inside it would be
  unreviewed code outside the task's scope.
- An automation is a task of its own, sized and reviewed like any other
  (principle 6).

**Who files it, and how.**
- A worker or the coordinator files the task with the board tools the role
  already has.
- The coordinator triages it like any other task. A harness feature goes to
  the architect, because it is product.
- The guidance goes in ROLES.md and `/crew-turn`. The turn brief's ledger
  (section 3) is where an agent sees the same steps across its turns.

**Relation to oh-learn-from-code-review.** That ai-skills path does the same
for repeated review findings: a finding the reviews keep making becomes a
standing rule instead of being rediscovered. This rule extends the idea from
review findings to procedures. The two meet when a repeated finding is
deterministic enough to check: it then becomes a repository tool at a
declared point, not a review rule.

**Principles.** 1 and 2. Principle 6 applies to the automation's own task.

## 12. The event model (decision T10)

**Decision.** aicrewd gets one event model in place of today's two
hard-wired wake paths: the board wake for the coordinator (A1) and the
forge events for the worker (A6). Both become sources of the same bus.

**Why.**
- Principle 1: what happens on an event becomes data and scripts, not a
  model turn.
- Section 2: the four blocks of 2.2 (worker turns 5 to 8) cost the worker
  four turns and the coordinator five (its turns 8 to 12). The delivery
  rules below make that impossible.

### 12.1 Sources

| Source | Events | Who reads it |
| --- | --- | --- |
| The hub board | a task's state changed, a comment was added, a task was added to an epic | aicrewd, through the peer board feed (A0, A1) |
| The forge | a pull request's head, review comments, labels, draft state, CI conclusions, merge | the launcher (the runner after B2), with the member's own credential; aicrewd reads no forge (A6) |
| Hub documents | a document changed | aicrewd, through the peer feed when aimem offers one |
| Timers | a deadline passed, such as the 30-minute review pickup of 4.2, or a hold's end | aicrewd |
| aicrewd itself | a budget was spent, a member's state changed, an attempt changed phase, an escalation was answered | aicrewd |

An event names its source, its subject, its kind, its time and a key that
is unique within the source. The subject is a task, a pull request, an
attempt or a member. The payload is bounded to 16 KiB. A pull request's
review bodies and CI excerpts are not in the event: they are facts the
handler reads with `pr()` (10.3).

### 12.2 Subscriptions

A subscription says: this member, or this role, wants events of these
kinds about these subjects.

**Default subscriptions:**
- **A member** gets its own tasks, attempts and pull requests.
- **A coordinator** gets READY tasks of the team's projects, and every
  worker's blocked reports and spent budgets.
- **An architect** gets escalations and pull requests awaiting the merge.

**Explicit subscriptions** can be added, for example "this agent watches
task X". They are data, in the project's process or the team's settings
(section 13).

### 12.3 Handlers, cheap first

A subscription names its handler. There are two kinds, and the cheap one
always runs first.
1. **An automation.** This is a Starlark script in the sandbox of 10.3, or
   a repository command under the allow list (9.3). It reads the current
   facts, acts, records steps, and returns one of two things:
   - `None`: the event is handled, and no model is needed;
   - `turn(kind, content)`: judgement is needed. `turn` is a pure
     constructor in the host API, with no side effect. The kind is one of
     section 6's turn kinds, and the content goes into the action-needed
     message (section 5).
2. **A model turn,** only when the automation returns `turn(...)`. A
   subscription with no automation, such as an escalation answer to a
   blocked worker, goes straight to a turn of its declared kind.

**Where a handler runs.** A handler that needs the member's credential or
worktree (`run`, `comment`) runs in that member's launcher, or its runner
after B2. aicrewd routes the event to it. A handler that only reads facts
and returns can run in aicrewd, in the same sandbox.

### 12.4 Delivery, deduplication, coalescing and replay

- **The journal.** aicrewd writes every event to its store, keyed by
  source and key, in the same transaction as the source's cursor. An event
  seen twice, from a feed replay or a second poll, is the same row:
  **deduplication** is the key.
- **Matching.** Each event is matched to its subscriptions and queued per
  pair of subscriber and subject. Events about one subject are handled in
  order.
- **Coalescing.** The handlers are level-triggered: a handler reads the
  subject's current facts, not the event that woke it.
  - Events for the same subscriber and subject that arrive within 30
    seconds of each other are merged into one handler run, which sees
    their kinds and IDs.
  - So are events that arrive while a handler or a turn for that subject
    is running.
  - Four forge events in two minutes become one run, and at most one turn.
- **At most one turn per member at a time,** as today (1.9). Turns needed
  for several subjects are merged into one action-needed message, which
  lists each subject with its content.
- **Delivery is at least once.** An event stays pending until a handler
  run records that it handled it. The record holds the run's outcome, its
  steps, and the commit of the script and subscription that handled it.
  - **Replay:** after a crash or a lost connection, the pending events are
    run again, from the journal.
  - **Idempotent handlers:** a handler reads the current state, so a
    replay does not repeat an effect the state already shows. The base
    update of 10.4 does not merge again into a branch that is up to date.
  - **A crash between two effects is not covered by reading the state
    alone.** If the push succeeded and the run stopped before its comment
    and step, the replay sees an up-to-date branch and returns. The delta
    note is then missing. Increment 2 specifies the recovery before it
    ships `base-update`, with a fake-host test that crashes between the
    push and the comment. The likely shape: the host records each effect of
    a run, and a replay of the same run completes the missing ones.
- **Failure.** A handler that fails three times in a row parks its events
  and sends one action-needed message to the coordinator, naming the
  handler, the subject and the last error. The events are not dropped, and
  a fixed handler replays them.

## 13. The core and the behaviour (decision T11)

**Decision.** The core is what aicrew releases as Go. Everything that
answers "what to do on this event" lives outside it, as data and scripts,
and changes by a reviewed pull request without a harness release.

| The core (Go, released) | The behaviour (data and Starlark, reviewed, no release needed) |
| --- | --- |
| the bus, the journal and delivery (12.4) | subscriptions (12.2) |
| the sources (12.1) | handlers: automations and turn kinds (12.3) |
| the extension points (9.1) | the commands at each point (9.2) |
| the sandbox and the host API (10.3) | the gate rules of 4.2 as scripts |
| the turn loop, the brief and the attempt handoff (section 3) | the role rules a turn kind reads |
| accounting and budgets as admission thresholds (section 7) | the budget amounts and the announcement's recipients |

**Reloading.** aicrewd and the launcher reload the behaviour without a
restart:
- when a team's process pin changes;
- when the base branch's commit changes the repository's `automations/`
  or its subscription file;
- when a new release ships new defaults.

The handler run records which commit of the script it ran (12.4), so a
reload never leaves it unclear which behaviour handled an event.

### 13.1 The default automations

The product ships a default set of automations with each release. They are
Starlark scripts and subscription data, not core code:
- **`base-update`:** the base-only update from main (10.4);
- **`gates-green`:** drop `WIP:`, mark ready and submit when both reviews
  and CI are green at the head;
- **`review-request`:** clear `hands-reviewed` and re-add `review-this`
  after a new head;
- **`ci-rerun`:** re-run the failed jobs once after a runner failure;
- **`offer`:** the coordinator's offer on a READY task (section 8).

**Where they live.**
- **The source:** the aicrew repository, under `defaults/automations/`
  (the scripts) and `defaults/automations/subscriptions.json` (the data).
  The path is kept apart from `automations/`, which holds aicrew's own
  project automations like any other repository's (9.3).
- **Reviews and tests:** they are reviewed with the product, by the same
  gates as any aicrew pull request. Their tests run in aicrew's CI against
  the fake host.
- **The release:** the release embeds them in `aicrewd` and `aicrew-agent`
  (Go's `embed`), so a member home needs no extra download, and the
  defaults are exactly those of the binary's version.

They are not in the aiskills processes. A default is part of what an
aicrew version does, and it moves with that version. A process is a
project's choice, and it moves with the project's pin.

**Precedence.** One rule, by name. The process manifest may list
`automations`, each with a name and a source:
- a script beside the manifest in the pinned process;
- a script in the repository's `automations/` at the base branch's commit;
- `disabled`.

Resolution:
1. **The process names it:** the process's entry wins. That entry may
   replace a default (same name), disable it, or add a new name. Its
   subscriptions replace the default's for that name.
2. **The process does not name it:** the release default applies.

A project's override is therefore one manifest entry and, when it is a
script, one file, each through the project's own reviewed pull request.
It survives every harness release, because the release changes only the
defaults it does not override. `aicrew-agent check` lists the effective
set, with each entry's source and commit.

### 13.2 The test: today's six tasks

The question is which of the six tasks filed from the #154 cycle would be
a script instead of core code under this split.

| Task | Becomes |
| --- | --- |
| `01a13ef0-1547`, forge pr show returns the content | **core:** the forge source reads comments, check runs and failed-job excerpts, and `pr()` exposes them. Which excerpt goes into which message is behaviour (a script). |
| `01a13ef0-155e`, the coordinator acts on a blocked report | **script:** a default subscription (the coordinator to blocked reports) and a handler that attaches the facts, then returns `turn("decide", ...)` only when the block needs a decision. |
| `01a13ef0-1572`, a spent budget is announced | **mostly script:** the core emits `budget.spent` (it owns the accounting) and shows it in `aicrew status`. Who is told, and how, is a default subscription and handler. |
| `01a13ef0-157d`, the cost of a resumed turn | **core:** stateless turns, the brief and the attempt handoff are the turn loop. |
| `01a13ef0-158a`, blocked_reason and blocked_since | **core:** two facts on the attempt, in aicrewd's store and `aicrew team state`. |
| `01a13ef0-1594`, the race pass as its own CI job | **neither:** the repository's CI workflow, under the repository's gates. |

So two become scripts, one is mostly a script, two are core, and one
belongs to the repository. The core tasks are the ones that add a fact or a
source. The script tasks are the ones that decide what to do with them.
That is the line T11 draws.

**Principles.** 1 and 2, and the operator's "no workarounds": changing a
behaviour is a reviewed pull request that takes effect without a release
and without touching a member's home.

### 13.3 Later: behaviour as an API of the hub

This comes later, not in the first increments. Subscriptions and
automations become a first-class API of the hub (aicrewd). The API has two
clients on the same operations:
- the console, for a person;
- MCP tools and the CLI, for an agent that adapts or debugs a particular
  hub instance.

**The operations:**
- **list** and **read:** the effective set, as `aicrew-agent check` shows
  it (13.1), with each entry's source and commit;
- **validate:** check a script or a subscription against the host API, its
  names and their arguments, and against the schema;
- **dry-run:** run a script against a recorded event log, with the facts as
  they were (below);
- **apply:** make a change on this hub instance.

Every applied change is versioned with its author and can be reverted. How
an instance-level change ranks against the process and the release
defaults, and whether an agent's apply needs the operator's approval, are
decided when this API is designed. The proposal is that the instance ranks
above the process, and that an agent's apply waits for the operator, as an
escalation answer does.

**What it changes now: two things.**
- **The files have a schema.** The subscription file, the manifest's
  `automations` entries (13.1) and a script's header have a published JSON
  Schema, versioned with the host API. The schema covers each entry's name,
  source, point, subscription and turn kind; the header names the
  host-API version the script was written for. An editor and the validate
  operation read and write the same files; nothing is held only in a
  database.
- **aicrewd can replay a recorded event log against a script.** The
  journal of 12.4 records each event, and each handler run records the
  facts it read with `pr()` and `attempt()`.
  - `aicrew automation replay --script FILE --events LOG` runs a script
    against those records with a recording host. `run`, `step` and
    `comment` are logged, not performed, and the output lists what the
    script would have done for each event.
  - This is the fake host the tests use (10.4), made a command. Increment 2
    ships it.

## 14. The increments, ordered by savings

The estimates are for one XS task like #154. Each is computed from the real
per-turn figures in section 2 under the stated assumptions. They are
estimates until each increment's own measurement replaces them. No
increment is larger than M (principle 6).

| # | Increment | Size | Decisions | Absorbs | Both members, per XS task | Operator touches |
| --- | --- | --- | --- | --- | --- | --- |
| today | as measured | | | | $12.64, 24 turns | 4 |
| 0 | **The per-turn cost** is the turn's own. No spurious holds. | XS | none (a defect) | `01a13f81-7df9`; the `turn_budget_usd` question of `01a13ef0-157d` | $12.64 | 2 |
| 1 | **The event core:** the bus, the journal, the sources (board, forge with comments, checks and excerpts, aicrewd, timers), subscriptions as data, and delivery with deduplication, coalescing and replay. The handler kind is a turn, with `action.needed` and its content. The default subscriptions route only events that need action. The subscription file has its schema (13.3). | M | T3, T10 | `01a13ef0-1547`, `01a1301d-55e3`, `01a13ef0-158a`, `01a13ef0-155e`; A1's board wake and A6's forge events become sources | about $7 | 1 |
| 2 | **The sandbox and the default automations:** Starlark with the host API of 10.3 in a limited child process, the extension points of 9.1, the default automations `base-update`, `gates-green`, `review-request` and `ci-rerun`, precedence and reload (13.1), the schema of the `automations` entries and script headers, and `aicrew automation replay` (13.3). The process manifest schema follows as an issue on aiskills. | M | T2, T7, T8, T11 | `01a135fe-18d1` (a conflict is a fact) | about $6.50 | 1 |
| 3 | **Stateless turns:** the turn brief, the attempt handoff and its ledger, no resume, the lean starting context measured in TURN-PROBE, and the repetition rule of 11 in ROLES.md and `/crew-turn`. | M | T1, T9 | `01a13ef0-157d` | about $4.50 | 1 |
| 4 | **Model tiers:** the rule of section 6, `needs_strong`, and the two model names in the turn settings. | S | T4 | none; it relates to `01a13613-709c` (model availability) | about $3.50 | 1 |
| 5 | **The coordinator's policy:** the `offer` default automation, the harness's acceptance after the digest check, and delivery after the merge. | S | T6 | `01a12e9c-0c95` | about $3 | 1 |
| 6 | **Cost as a figure:** status and the delivery evidence, budgets per size, and the `budget.spent` event with its default handler. | S | T5 | `01a13ef0-1572`, half of `01a13098-79d5`, `01a11f23-2af8`; the cost half of A4 (`01a11eee-3cba`) | about $3 | 1 |

How the estimates are made:
- **0:** the same turns. The two holds and the budget raise go away, which
  leaves the escalation answer and one nudge.
- **1:** no waiting turns and no blind blocks.
  - **The worker:** while a fact is pending, an attempt gets no
    continuation turn, and a failure arrives with its content. The worker
    keeps turns 1, 2, 9 and 10, about $5.15, and drops $2.51.
  - **The coordinator:** it gets no turn for an acceptance or a resume, and
    no relay turns. It keeps the offer ($0.70) and the toolchain
    escalation ($0.28), and gains one review turn, estimated at $1.
  - **Total:** about $7.
- **2:** the default automations run the checks, the labels and the base
  update, so turns 2, 9 and 10 lose about a third of their calls. The
  worker's share falls to about $4.50. Total: about $6.50.
- **3:** every turn starts at about 25 thousand tokens instead of 130 to
  180 thousand, and no hold rewrites a conversation ($3.86 in 2.4).
  - The worker's four writing turns come to about $3. Turn 1 stays as
    measured; turns 2, 9 and 10 lose their rewrite of the conversation and
    their reads of it.
  - The coordinator's offer and review come to about $1.50.
  - Total: about $4.50.
- **4:** the coordinator's remaining reading turns and any `inbox` turns
  run on the small model. The saving depends on its price, which TURN-PROBE
  records.
- **5 and 6:** they add little saving per task. They remove the
  coordinator's offer turn and the operator's manual budget work, and make
  the cost visible.

0 is an XS fix that ships first: the budgets are wrong until it does.
1 and 2 come next, in that order, because 2's automations are handlers on
1's bus. 3 is independent of both and can ship in parallel with them as a
separate line of work, once the operator agrees.

## 15. Non-goals

- The hub API for behaviour (13.3) is later. Only its file schema and the
  replay are in the increments.
- No product code in this document's pull request.
- No change to the pilot homes and no release.
- The review gates themselves do not change: what makes a pull request
  ready, and who merges, stay as they are. Section 4 moves their mechanical
  part from the model's instructions into tested scripts.
- No automation that runs as foreign code inside aicrewd, and no Python,
  Node or PowerShell-only automation on member machines (sections 9 and 10).
- No other delivery kind. The state machine is `code-pr`'s. The `ops` kind
  (DESIGN-CONTROL-PLANE 8.4) gets its own when it is built, under the same
  principles.

## 16. Decisions for the operator

1. **Accept T1 to T11** as written, or amend them.
2. **The provisional budgets per size** (section 7), or other numbers.
3. **T8, Starlark** for the workflow scripts (10.2), and the host API of
   10.3 as the complete list.
4. **Where the default automations live** (13.1): in the aicrew
   repository, embedded in the release, with a process overriding them by
   name. The alternative is a default process in aiskills, which would
   make the product's defaults depend on a pin rather than on its version.
5. **Whether a coordinator review stays in the team's process** for every
   attempt. T6 keeps it as a strong turn. Dropping it, and relying on the
   worker's local review and the external review, would save about $1 per
   task.
