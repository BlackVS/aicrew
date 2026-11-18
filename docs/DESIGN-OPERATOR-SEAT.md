# The operator's seat after the architect role

Status: **design for review** (task `01a1eefc-02f3`). This is the design
that the goal's feature 8, "The operator has a seat in the product"
(`01a1e56f-d60f`), begins with. Nothing here is implemented. The owner reads
it at the trial's review. The questions that are the owner's to decide are
gathered in section 7.

**The feature as recorded.**
- **The outcome:** a question that needs a person reaches the board from
  the team, and is answered there when the operator is back. The team does
  not wait in a terminal.
- **The check:** a question raised by a member of a running team appears on
  the board for the operator, and the operator's answer reaches that member
  through the product.

**The inputs.**
- `docs/proposals/OPERATOR-SEAT.md`, with the owner's decisions of
  2026-09-29 (its section 9).
- `docs/DESIGN-CONTROL-PLANE.md`: decisions D10, D11, D14 and D18,
  section 7.5 (escalations) and section 7.7 (the architect is told).
- Case E1 of the pilot simulation, from pull request #267: its section in
  `docs/E2E-PILOT-SIM.md` and its green run on the lab host.
  That run's tree was commit 831b38d, and its report is quoted in #267.
- The older design task `01a0eba9-d5fe`, the approver role, with its
  "Case 1" in the comments.

**Sources.** Every statement below about what the product does today names
where it is read from. A file is named with its section, or with the
function or route that does it. A run is named by its record.

## 1. What feature 7 already delivers, and what is left

Feature 7, "The architect is a role in the product", is delivered through
three pull requests:
- #265, the design: `DESIGN-CONTROL-PLANE.md` section 7.7;
- #266, the wait and the kit;
- #267, the simulation's case E1.

E1's run walks the escalation path end to end. Its green run, on tree
831b38d, recorded these steps:

| Step | What the run recorded | Read from |
| --- | --- | --- |
| The raise | The coordinator raised an escalation on the blocked attempt at 13:17:49.205Z. The escalation names the worker as `blocked`, with urgency `now`. | the `escalations` row in the `answered` snapshot's store copy |
| The architect is told | `aicrew escalations wait` returned the escalation's ID 12.5 s after the raise. No list or show ran in between. | the runner's log of the architect's commands, `case-E1/architect.log` |
| The answer | It was answered at 13:18:03.212Z under the architect credential, and `answered_by` is `architect:<credential ID>`. | the store copy |
| The return | The coordinator and the worker each took a turn started from the answer's message, and each acknowledged it. The attempt then left `blocked`. | the homes' `turns.jsonl`, `message_recipients`, the coordinator's `attempt.answer` |
| No operator call | The audit holds no operator-credential call between the raise and the answer. | `audit` |
| The request on the board | The `[escalation.request ID]` comment reached the task only after the finalize. In an earlier run (tree 507b40d), the coordinator's handoff noted at 11:52:24Z that the mirror waited, and posted it at 11:56:16Z. | aimem's comments at the step `done`; the coordinator's handoff |
| The answer on the board | The runner posted the `[escalation.answer ID]` comment with the hub's admin token. It is on the task in the `answered` snapshot, in which the reservation is still `held`. | `scripts/pilot-sim.sh`, `hubapi`; `e2e/pilotsim/cases.sh`, `case_e1`; the snapshot's `aimem/comments.json` and `aimem/reservation.json` |

**Against the feature's check:**
- **"Appears on the board for the operator": partly delivered.** The
  operator is told in the architect session, through the wait. The board
  shows the question only after the hold on the task ends, which in E1 was
  after the answer (1.3).
- **"The operator's answer reaches that member through the product":
  delivered, through the architect.** The answer's message reaches both
  members, and their turns start from it. What is left is the answer given
  when no architect session is open (1.1), and the record of who decided it
  (1.4).

### 1.1 The operator answering without an architect session

**What exists.** The routes of `aicrew escalations` admit two kinds of
caller (`requireEscalationReader` in `internal/store/caller.go`): the
operator credential and an architect credential.
- **They are plain client commands.** `list`, `show`, `answer` and `wait`
  need no Claude Code session (DEVELOPMENT.md, "The architect, or the
  operator, reads and answers").
- **So two paths work today from a plain terminal:**
  - in the architect directory, where the managed settings name the
    architect credential;
  - anywhere the operator credential's file is named with `--token-file` or
    `AICREW_OPERATOR_TOKEN_FILE`.

**What is left.**
- **Nothing tells the operator outside a session.** The wait is the only
  thing that tells, and it runs where someone starts it (section 7.7, D23).
- **No document says the terminal path is a way to answer.** The kit's
  guidance (the architect directory's `docs/ARCHITECT.md`, written from
  `internal/architect/guidance.go`) speaks to the session.
- **No run has answered under the operator credential.** E1 answered under
  the architect credential.

### 1.2 A `now` escalation with nobody at the seat

**What exists.**
- **The record stays open.** Only an answer ends it (section 7.7, D24), and
  no timeout decides (section 7.5, step 5; OPERATOR-SEAT D6).
- **The coordinator carries on with other work** (DEVELOPMENT.md, "The
  coordinator's guidance").
- **The blocked worker's attempt stays `blocked`, holding its task.** The
  team state does not mark a blocked attempt as stalled
  (`LooksStalled` in `internal/store/control.go`).

**What is left.**
- **Urgency changes nothing.** `now` is a field of the record and a column
  of the wait's line. No surface treats it differently.
- **Nobody is told when no session runs a wait.** Section 7.7 (D23)
  declines notices outside the client, so the product promises no channel.

That is the gap this feature names: the team does not wait in a terminal,
but the question waits for someone to look.

### 1.3 The request on the board while the task is held

**What exists.**
- **aimem refuses the coordinator's comment on a held task.** In team mode
  a comment is a coordinator's triage write, and on a held task it is
  refused with `task_held`. The source is the aimem repository at its
  v0.11.2 release: `internal/server/tasks.go`, `addTaskComment`, which
  calls `AddTaskCommentUnheld`. The rule is aimem's
  `docs/DESIGN-AIFORGE-PILOT-1.md`, section 4, "Coordinator triage in team
  mode".
- **Personal mode is not refused.** The same route takes a personal-mode
  comment on a held task (`AddTaskComment`, with no hold check), which is
  how the architect's answer mirror can land during the hold.
- **The coordinator waits, as D14 says.** The request's mirror waits until
  the hold ends, and the coordinator's handoff says so. E1 showed exactly
  this (section 1, the table).
- **The board still shows the task in `BLOCKED`.** E1's task history passed
  through BLOCKED (`e2e/pilotsim/cycle.py`, the history check that E1 shares
  with P4).
- **The task's `blocker` field says why the attempt blocked, not what is
  asked.**
  - In E1's `blocked` snapshot (`aimem/task.json`), the blocker reads:
    "The turn loop stalled this attempt: 2 turns in a row made no step and
    left the CURRENT ATTEMPT section of docs/HANDOFF.md unchanged."
  - That is the launcher's stall text, which `e2e/pilotsim/cases.py` checks
    in P4 and E1.
  - It names neither the escalation, nor its question, nor its options.

  So the board shows that the task is blocked and why the worker stopped,
  but not the question put to the operator.

**What is left.**
- **The question itself is not on the board until the release.** For a
  blocked worker, that is after the answer.
- **The fallback without a connection shows nothing.** `/arch-escalations`
  falls back to reading `[escalation.request ` comments (DEVELOPMENT.md and
  `docs/E2E-PILOT-SIM.md`, "The escalation case"), so it shows nothing of
  such a request before the delivery.

### 1.4 Who the record names as the decider

**What exists.**
- **The record names a credential, not a person.**
  - `answered_by` is the caller's `String()` (`internal/store/escalations.go`,
    `AnswerEscalation`; `internal/store/caller.go`).
  - Under an architect credential it is `architect:<credential ID>`.
  - Under the operator credential it is `operator:aicrew-operator`, a fixed
    ID (`internal/server/admin.go`, `operatorCallerID`).
- **The architect session answers only on the operator's word.** That rule
  is guidance (section 7.3, "Limits"), not something the product checks.
  Nothing in the record tells an answer the operator gave in the
  conversation from one the session gave on its own.
- **D10 rules that the full operator credential never enters an AI
  session** (`DESIGN-CONTROL-PLANE.md` 5.1). The ops approvals of section
  8.4 rely on that custody: D18 has a human approve with the full operator
  credential.
- **D11 says a floor-category answer "records that the human decided it in
  the conversation".** The record has no field for that. Today it could be
  said only in the rationale's text.

**What is left.**
- **An answer through the architect session cannot be told apart.** It is
  `architect:<ID>` whether the person decided or the session did.
- **An answer through the operator credential is told apart, by custody.**
  It is `operator:aicrew-operator`, and that credential is meant to be held
  outside AI sessions. Custody is the only proof: there is no receipt and
  no second factor (OPERATOR-SEAT §4 and §9 D3, deferred by D11).
- **Custody is a practice, not something the product checks.**
  - The credential is a file read by whoever runs the command.
  - An agent session that runs as the same operating-system user as the
    file's owner can read that file and answer as the operator.
  - Nothing in the record would show it: the answer would read
    `operator:aicrew-operator` like the person's own.
  - D10 states the rule. The managed deny rules of the architect directory
    cover its own `creds/` (section 7.2), not a file elsewhere on the host.
    Keeping the file out of every AI session's reach is the operator's
    practice.

## 2. Where the operator answers what is left

### 2.1 The alternatives

**A. The board's page in aimem.** The operator reads the question on the
task page and answers there.
- **How the page works.** The page is aimem's `internal/server/tasks.html`.
  It calls only the hub's own routes with the token pasted into the
  browser, and its content security policy forbids cross-origin fetches
  (the page's header comment). So the page cannot call aicrewd.
- **How an answer could get from the page to aicrewd.** One of two ways:
  - **A1.** aimem keeps a typed answer record, written by the person's own
    aimem user, and aicrewd reads it through a new peer read scope.
  - **A2.** aimem forwards the answer to aicrewd with a credential of its
    own.
- **A comment is not enough.** An `[escalation.answer]` comment authorizes
  nothing (section 7.5, step 3; DEVELOPMENT.md). A comment's text proves
  only who wrote it on the board.

**B. The operator's own command line.** `aicrew escalations list`, `show`
and `answer` under the operator credential, in a terminal with no AI
session. The commands exist (1.1). The operator is told one of two ways:
- the board shows the request (1.3, with section 7's question 1);
- `aicrew escalations list --open` or `wait` is run when the operator is
  back.

**C. The architect session, as today.** E1's path. It needs a Claude Code
session open in the architect directory, and its answers are
`architect:<ID>`.

**D. Another surface.** Two are possible, and neither is proposed:
- **A terminal UI without a model.** This is OPERATOR-SEAT's D5 client. A
  terminal or web interface of aicrew's own is a non-goal of this task,
  and section 4.4 builds none until the API is stable.
- **A notice outside the product** (mail, chat). Section 7.7 (D23) declines
  it: the product promises only the client's own channel.

### 2.2 The comparison

| | A. Board page | B. Own command line | C. Architect session |
| --- | --- | --- | --- |
| What the operator does | opens the task in the browser and picks an option | reads the request on the board or in the list, then runs `aicrew escalations answer` | tells the session, which runs `answer` |
| What the member sees | the `escalation.answer` message, as today | the same | the same |
| New routes | A1: an aimem answer route, plus a peer read on aimem that aicrewd polls. A2: an aicrewd route for aimem's forward | none | none |
| Credentials | A1: a new aimem peer read scope for aicrewd. A2: a credential from aimem to aicrewd. Both need a browser-held authority the operator token may not be (section 4.4) | the operator credential, which exists | the architect credential, which exists |
| Wire changes | A1: an aimem record kind and a peer read. A2: an aimem-to-aicrewd call | none | none |
| Sensitive surfaces | aimem's access rules and routes; aicrewd's routes and authentication; a wire between them | none | none |
| `answered_by` | a new kind, for example `person:<aimem user>` | `operator:aicrew-operator` | `architect:<ID>` |
| What a run could assert | the answer's record on aimem, its read by aicrewd, then E1's return | the answer under the operator credential, no architect credential on this escalation, then E1's return | E1 as it stands |
| Size | L, split across aimem and aicrew | S (documents and a run), plus aimem's comment rule if question 1 is answered (a) or (b) | none |

### 2.3 The recommendation: B, with the request on the board

The operator reads the question where the team puts it, on the board, and
answers from their own command line under the operator credential. The
architect session (C) remains the other way, for an operator who is
working with it.

- **No new surface.** B needs no new route, credential or wire. Every
  command exists, and so does the custody that tells the operator's answer
  apart (1.4).
- **The same practice as an ops approval.** D18 already has a floor-category
  approval given by a human with the full operator credential.
- **Its limit: custody is a practice (1.4).** B tells the person's answer
  apart only as far as no agent session can read the operator credential's
  file. A session running as the same operating-system user as that file's
  owner can, and its answer would be recorded as the operator's. B is as
  strong as the operator's practice of keeping the file out of every AI
  session's reach, and no stronger.
- **The board is where the question shows.** The feature's outcome puts the
  question on the board. That needs the request mirrored during the hold,
  which is aimem's decision (section 7, question 1). Without it, B still
  answers, but the board shows the question only after the hold ends.
- **The browser write waits for its own design.** A (the board's page)
  needs an authentication design for a write that authorizes, and the
  operator token does not belong in a browser (section 4.4). That is the
  same ground as the deferred receipts (section 4).

**What would change it.**
- **A web authentication design for the board.** If the hub gains passkeys
  for the board's page (OPERATOR-SEAT §9 D3 points there), then A1 becomes
  the natural place, and B its fallback.
- **A widening of the operator credential.** If the owner wants the full
  operator credential kept for issuing credentials and invitations only, a
  narrower human-held credential (OPERATOR-SEAT §4, option C) replaces it
  in B. That is a new credential kind on aicrewd: a sensitive surface, and
  section 7's question 4.
- **Custody that cannot be kept by practice.** If agent sessions must run
  as the same operating-system user that holds the operator credential, B's
  custody fails as a proof. The answer then needs what an agent session
  cannot produce:
  - increment 4, with OPERATOR-SEAT's C bridge reading its token from the
    terminal and never from a file;
  - or the receipts with a second factor (section 4).
- **The board not taking the request during the hold.** If aimem keeps
  refusing (question 1 answered (c)), the restated check (section 6) asserts
  being told through `aicrew escalations list` instead of the board, and
  says so.

## 3. How the answer reaches the blocked member under B

Nothing in the return changes. Every step is a record that exists today:

1. **The request.** The coordinator raises the escalation with
   `aicrew-agent escalate`, naming the blocked worker (section 7.5,
   step 1).
   - Records: the escalation in aicrewd's store, and the coordinator's
     audited call.
2. **On the board.** The coordinator mirrors it as
   `[escalation.request ID]`.
   - If aimem takes a coordinator's comment on a held task (question 1),
     the mirror lands at once.
   - If not, it waits for the hold, as D14 says today.
   - Records: aimem's comment, under the coordinator's team-mode user.
3. **The operator answers** from their own terminal:
   `aicrew escalations answer --id ID --decision ... --rationale ...`, with
   the operator credential's file named by `--token-file`.
   - Records: the escalation's answer, with `answered_by` set to
     `operator:aicrew-operator`, and the audit's `operator` caller.
4. **The answer on the board.** The operator mirrors it as
   `[escalation.answer ID]` with their own aimem user, in personal mode,
   which a hold does not refuse (1.3).
   - Records: aimem's comment, under the person's user.
5. **The return.** In the answer's transaction, aicrewd writes the
   `escalation.answer` message to the coordinator and to the blocked member
   (`AnswerEscalation`; OPERATOR-SEAT §9 D4). Each member's launcher starts
   a turn from it, and the turn acknowledges it (CREW-CONTRACT.md, "What
   wakes the member").
   - The coordinator gives the worker the decision with its `answer` step,
     and the attempt leaves `blocked` (CREW-CONTRACT.md, "Answer").
   - Records: the messages and their recipients' acks, the turns'
     triggers, and the `attempt.answered` event.

Step 4 is by hand, as the architect's mirror is today. Folding the write
into `aicrew escalations answer` would need aicrew to write to aimem with
the person's credential, which it does not hold, so the write stays out.
Increment 2 has the command print the mirror's text, in the right form,
ready to paste. That is output only.

## 4. What stays deferred, and what a floor answer records meanwhile

**Deferred:**
- **The authority receipts** (OPERATOR-SEAT §4 and §9 D3): the
  second-factor unlock, the approvals ledger and the receipt bound to
  `{request_digest, decision, policy_revision}`. Deferred by D11, and still
  deferred here. Nothing in feature 8's check needs a proof stronger than
  custody of the operator credential. The receipts become necessary when
  any of these comes:
  - an answer from a browser (A);
  - a delegated merge (feature 9);
  - an answer that must be proven to someone other than the operator.
- **The policy record and its modes** (OPERATOR-SEAT §2). The coordinator
  applies the `default` mode from its guidance, since no team policy exists
  (DEVELOPMENT.md, "The coordinator's guidance").
- **Self-answers** (`escalation.self`, OPERATOR-SEAT §3) and the overturn
  with checkpoints (§9 D7). Neither exists in the store.
- **`answer_ref` on a `resume`** (OPERATOR-SEAT §3).
  - It does not exist: the work update has no such field.
  - The attempt leaves `blocked` by the coordinator's `answer` step instead
    (CREW-CONTRACT.md, "Answer").
  - E1 shows that path.

**What a floor answer records meanwhile.** A floor category is
architecture, wire, security, risk, merge or deploy. Until the receipts
exist:
- **Through B,** the answer records `answered_by: operator:aicrew-operator`.
  That says the operator's own credential gave it, which D10 keeps outside
  AI sessions, by the operator's practice (1.4).
- **Through C,** it records `architect:<ID>`. The rationale should then say
  that the person decided it in the conversation, as D11 asks. The record
  has no field for it, and this design adds none: a field the session
  fills proves nothing that the rationale does not.
- **In both,** the merge stays a human's click on the forge (D11), and an
  ops plan's approval stays D18's.

## 5. OPERATOR-SEAT's decisions, and the older task

| OPERATOR-SEAT | Now |
| --- | --- |
| §9 D1, manual mode holds | **Deferred** with the policy record (section 4). |
| §9 D2, the system of record | **Stands, as D14 shaped it:** aicrewd's store is the operational record; the task comment is the record of decision. The comment is written by the coordinator in team mode, and by the architect or the operator in personal mode. |
| §9 D3, the authority proof | **Deferred** (D11; section 4 here). |
| §9 D4, delivery to the coordinator and the blocked member | **Stands, and is built** (`AnswerEscalation`; E1). |
| §9 D5, the seat as an aicrew member role with a terminal client without a model | **Replaced.** D11 made the architect session the receiver. This design adds the operator's own command line (B). There is no member role `operator` and no client of aicrew's own. |
| §9 D6, no answer in time decides nothing | **Stands** (section 7.5, step 5). |
| §9 D7, overturn with checkpoints | **Deferred** with self-answers. |
| §9 D8, policy changes | **Deferred** with the policy record. |
| §9 D9, auto-merge | **Feature 9's,** not this feature's. |
| §9 D10, the categories | **Stands.** aicrewd accepts them, and a test keeps the kit's guidance equal to them (DEVELOPMENT.md). |
| §9 D11, the name | **Changed in meaning.** "Operator seat" now names where the operator answers (the board and their own command line), not a member role. |
| §9 D12, the verifier's identity | **Stands,** outside this feature. |
| §1, workers cannot address the seat | **Moot:** there is no seat member. Workers address only the coordinator, as the crew contract has it. |

**The older task `01a0eba9-d5fe`, the approver role.** Recommended: keep
it, narrowed to the part this feature does not need.
- **What it keeps:**
  - approvals of privileged actions: the "needs approval" list as instance
    configuration (its criterion 2);
  - the worker's check before such an action (its criterion 5);
  - the automation that performs an approved action with a custody
    credential (its criterion 6);
  - its Case 1, which is exactly such an action.
- **What is covered elsewhere:**
  - the escalation flow, the records, the delivery and the phasing (its
    criteria 1, 3, 7 and 9) are covered by C2, E1 and this design;
  - the answer classes (its criterion 4) are covered as far as section 4
    goes.
- **Who changes it.** The architect retitles it and amends its criteria
  after the owner's review (section 7, question 5). This task changes no
  other task.

## 6. The feature's check, restated as a run

**The restatement, in a sentence for the goal's log:** "Feature 8's check
is restated as a run of the pilot simulation. A member's escalation is on
the task's comments while the task is still held. The operator answers
from their own command line under the operator credential, with no
architect credential taking part in this escalation. The answer's message
starts a turn for each member it names, and the attempt leaves blocked.
Each step is asserted from the run's records. One thing changes from the
recorded check: the question is still read on the board, but it is
answered from the operator's own command line, not on the board. An answer
from the board's page needs an authentication design for a browser write
that authorizes, which the operator credential cannot give in a browser,
so it is deferred."

**The run** is the pilot simulation's running team on the lab host, as E1's
is. A case E2 asserts each step:

1. **The block and the raise.** The worker blocks its attempt. The
   coordinator raises an escalation naming the worker as `blocked`, with
   urgency `now`.
   - Read from: the escalation row.
2. **On the board during the hold.** `[escalation.request ID]` is among
   the task's comments in a snapshot in which the reservation still names
   the worker as its holder.
   - Read from: one snapshot's `aimem/comments.json` and
     `aimem/reservation.json`.
   - A reservation is its own record in aimem, not a revision of the task,
     so the task's history does not carry the release. The order is
     asserted by the one snapshot that holds both: the comment present,
     and the reservation `held` by the worker. E1's `answered` snapshot
     shows the shape, for the answer's comment.
   - This step needs question 1 answered (a), (b) or (d). Otherwise the
     comment first appears in a snapshot with no reservation, and the run
     asserts being told from step 3's `aicrew escalations list --open`
     instead.
3. **The operator answers.** In a container that holds the operator
   credential and no architect directory, the runner runs
   `aicrew escalations list --open`, `show` and `answer` under the
   operator credential.
   - The record's `answered_by` is `operator:aicrew-operator`.
   - The audit has the answer's call under the `operator` caller kind.
   - No `architect:` caller appears on this escalation's record, nor among
     the audit's rows between its raise and its answer. The run shares its
     stack and store with E1, which issues an architect credential, so the
     assertion holds whatever the cases' order is.
   - Read from: the escalation row and `audit`.
4. **The answer on the board.** The runner posts `[escalation.answer ID]`
   with an ordinary aimem user's token, not the hub admin token E1 used.
   - aimem's comment names that user as its actor.
   - Read from: aimem's comment.
5. **The return.** As E1's step 4: both members receive the
   `escalation.answer` message, each has a turn started from it, and each
   acknowledges it. The attempt leaves `blocked`.
   - Read from: the messages, the recipients' acks, the turns' triggers,
     and the attempt.
6. **Over the whole run,** the members' turns all finish and their guard
   logs stay empty.
   - Read from: the cycle's existing checks (`e2e/pilotsim/cycle.py`, the
     homes).
   - That the members' only inputs are inbox messages is not asserted by
     them. It follows from headless turns, whose every trigger is an inbox
     message (`DESIGN-CONTROL-PLANE.md` 1.2, D1).

**What rests on the runner's own log.** The runner plays the operator. Two
things are not in any record of aicrewd or aimem:
- **That the person read the board.** The run shows only that the comment
  was there at a time. A person reading the page is not something a run
  can assert.
- **That the terminal had no AI session.** That the operator credential
  stayed outside AI sessions is custody, a practice and not a record (1.4).
  The run can show only that no architect credential took part in this
  escalation.

### 6.1 The increments that follow

They are listed here and not filed. The architect files them after the
owner's review.

| # | Increment | Size | Surface | Depends on |
| --- | --- | --- | --- | --- |
| 1 | **aimem: a coordinator's comment on a held task.** Comments only: no state, field or ownership change. The rule is in aimem's `addTaskComment` and its `docs/DESIGN-AIFORGE-PILOT-1.md` section 4. Its tests show a held task's state, fence and holder unchanged after such a comment. | S to M | **Sensitive:** aimem's access rules (and aimem's project, not aicrew's) | question 1 |
| 2 | **aicrew: the operator's own command line as a documented way to answer.** DEVELOPMENT.md and ARCHITECT.md say how the operator answers outside a session, and what `answered_by` then records. `aicrew escalations answer` prints the answer's mirror ready to paste (output only, no new write). The coordinator's guidance drops "waits for the hold" when increment 1 lands. DEVELOPMENT.md's sentence that the keep-alive loop wakes members on the answer is replaced by the turns-model one that CREW-CONTRACT.md now has. | S | ordinary: documents and the kit's texts | question 2 |
| 3 | **The simulation's case E2:** the restated check above, as a cycle of its own. | S to M | ordinary: the simulation's scripts | 1 (or the variant without it), 2 |
| 4 | **A narrower human-held credential for answers,** only if the owner wants the full operator credential kept out of routine answers (question 4). A credential kind that reads and answers escalations, with `answered_by` set to `seat:<ID>`. | M | **Sensitive:** aicrewd's authentication and its credential store | question 4 |
| — | **Deferred, not listed as increments:** the board page's write (A), the receipts, the policy record, self-answers. | | | section 4 |

## 7. The questions that are the owner's

1. **Should aimem take a coordinator's comment on a held task?**
   - **(a) Yes, comments only.** A comment changes no state, field or
     owner. The request is on the board at once, and D14's "waits for the
     hold" ends.
   - **(b) Yes, but only for comments headed `[escalation.request `.**
     That is narrower, but it lets a comment's text steer an access rule.
   - **(c) No.** The request reaches the board at the hold's end, and the
     restated check asserts being told through the command line instead.
   - **(d) aicrewd posts the mirror itself,** with a credential of its own
     on aimem, outside team mode. A personal-mode comment is not refused
     under a hold (1.3), so aimem's hold rule is unchanged.
     - **Its cost:** aicrewd gains a write credential on aimem, which it
       does not hold today. That is sensitive on the other side: aimem's
       access rules must grant it, and aicrewd's custody of a credential
       that writes to the board is new.
     - **What it buys:** the mirror is written in the raise's own flow, not
       by the coordinator's turn.
   - **Recommended: (a).** It changes one rule on the side that owns it, and
     gives no new holder a write on the board.
     - The reservation contract already reserves comments for a policy of
       their own: aimem's `docs/DESIGN-AIFORGE-RESERVATIONS.md`, section
       "Claim, write, release and finalize", the row "Work mutation".
       There, comments and evidence additions that do not change task state
       "need a separate reviewed policy" and "must not become a state or
       ownership bypass".
     - A coordinator's comment that changes nothing else is that policy's
       smallest form.
     - (d) wins only if aimem would rather not change its hold rule than
       admit a new writer.
2. **Where does the operator answer?** The alternatives are in section 2:
   A (the board's page), B (their own command line), C (the architect
   session only) or D (another surface).
   - **Recommended: B, with C kept.** The reason is in 2.3.
3. **May the board's page carry a write that answers an escalation?**
   - **(a) Not now.** The page stays read and comment, and a browser
     answer waits for a web authentication design (passkeys, section 4.4).
   - **(b) Yes, as A1,** with the operator's aimem token in the browser
     authorizing the answer.
   - **Recommended: (a).** (b) would let a pasted token in browser storage
     authorize a floor-category answer. That is a weaker custody than the
     operator credential's, and it is on two sensitive surfaces at once.
4. **Which credential answers in B?**
   - **(a) The full operator credential,** as D18 does for ops approvals.
   - **(b) A new human-held answer credential** (increment 4), so that the
     full operator credential is used only to issue credentials and
     invitations.
   - **Recommended: (a) for this feature.** It needs nothing new, and D18
     already rests on the same custody. Choose (b) if the operator does
     not want the full credential used for routine answers.
   - **The limit of both:** a credential in a file is only as safe as the
     practice that keeps agent sessions from reading it (1.4). (b) helps
     against that only if its token is read from the terminal and never
     stored, as OPERATOR-SEAT's C bridge has it; otherwise only the
     receipts do (question 6).
5. **What becomes of task `01a0eba9-d5fe`?**
   - **(a) Kept, narrowed** to approvals of privileged actions, as in
     section 5.
   - **(b) Closed as covered,** with Case 1 filed anew when an ops or
     server action needs it.
   - **Recommended: (a).** Case 1 is not covered by anything built, and
     section 8.4's ops approvals (D18) cover only ops plans. A server
     access grant from a code task, Case 1's shape, is neither.
6. **Do the receipts stay deferred?**
   - **(a) Yes,** until a browser answer, a delegated merge or a third-party
     proof needs them (section 4).
   - **(b) No:** build OPERATOR-SEAT's C bridge (the approval token read
     from the TTY) with this feature.
   - **Recommended: (a).** Feature 8's check needs no proof beyond the
     operator credential's custody. A receipt without a consumer that
     checks it would be built ahead of its need.
7. **Is a notice for `now` escalations wanted outside the product's
   client?** That means a mail or a chat message when nobody is at the
   seat.
   - **(a) No,** as D23 decided.
   - **(b) Yes,** as a design of its own.
   - **Recommended: (a) until a run** shows `now` questions waiting long
     enough to matter. The board (with question 1) and the command line
     cover "when the operator is back".
