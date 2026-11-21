# Your role in the team

Your role is in `aicrew-agent session status` (coordinator, worker or independent);
read the section for it, and the rules every member follows. Every
transition goes through your launcher with `aicrew-agent step`: never
claim, edit or release an aimem task directly (the coordinator's triage,
below, is the one exception), and never use another
credential or your personal context to get past a refusal. The full step
reference is in aicrew's DEVELOPMENT.md and CREW-CONTRACT.md.

## Every member

- **Read the inbox** at session start and after each step. Act only on what
  your inbox or your own step answers show: an attempt ID, task or offer from
  anywhere else is not yours to act on. Acknowledge what you handled with
  `aicrew-agent inbox --ack MESSAGE_ID,MESSAGE_ID`, the messages' `id` from
  `aicrew-agent inbox --json` (acknowledging is not a `step`); a message you
  did not acknowledge comes back, first.
- **Credentials and connections.** Never read `creds/`, and never put a credential into a command, URL, environment variable or git configuration. Never change git's global or system configuration, and never weaken TLS verification (`http.sslVerify`, `GIT_SSL_NO_VERIFY` or an unverified connection). When an access, a clone or a fetch is refused or fails, stop and report it; never work around it. Before you accept an offer, report it by declining the offer (an offer you cannot verify is declined). After you accepted it, decline is refused: block the attempt with `aicrew-agent step work --attempt ATTEMPT_ID --task TASK_ID` with the body `{"intent": "block", "detail": "REASON"}`, the failure as the reason.
- **Do not wait for a message yourself.** When you have nothing to do, end
  your turn: the home's Stop hook waits on your inbox and, when a message
  arrives, gives you a turn that names it. Read it with
  `aicrew-agent inbox` then. A turn that only asks you to reply
  "waiting" keeps you waiting: reply that one word and nothing else. In a
  home the turn loop runs (`aicrew-agent serve`, `/crew-turn`), there is
  no Stop hook: end the turn, and the next message starts the next one.
- **The launcher holds your session.** Under `aicrew-agent run`, act through
  `aicrew-agent inbox` and `aicrew-agent step`. `session start`,
  `session leave` and `run` are refused inside the client: a new proof
  would fence the launcher's session. To restart it, exit the client and
  `run` again.
- **A step's exit code:** 0 done; 3 refused, or settled as not committed;
  4 recorded but not settled (run `step recover` later); 1 failed (no
  launcher, or aicrewd, aimem or the channel failed). On a refusal, follow
  its `next_action` as written. `role_forbidden` or `attempt_forbidden` means
  the step is not yours: stop, and turn to your coordinator, never to a
  human. With a running attempt, report it with the `work` step's
  `block` and the refusal as its reason; the coordinator decides, and
  escalates what it cannot settle. Without one, aicrew has no message from
  a member to its coordinator yet: write the refusal in
  `docs/HANDOFF.md` and end your turn. A coordinator escalates instead
  (its section, below).
- **Pull requests.** Open, read, label, comment on and mark ready pull requests with `aicrew-agent forge pr`, which uses your home's own forge credential for the repository's host: `open --title TITLE --body-file FILE [--draft]` in your worktree after pushing your branch, `show --pr N`, `label add|remove --pr N LABEL`, `comment --pr N --body-file FILE`, and `ready --pr N`, which drops the draft flag and a `WIP:` or `Draft:` title prefix once the gates the project's process requires are green (the merge stays a person's). Each prints the pull request's number and URL, and whether your launcher watches it (`watch:`): a change of a watched pull request that needs your decision (the external review at the head asks for changes, CI fails, both gates turn green, a merge or a close) arrives in your inbox as an `action.needed` message that carries what you need: the review's body, each failed job with its log excerpt, or the passed checks. It wakes you; progress alone (a label, CI starting) does not. Never wait for a review or CI with a background watch, a sleep loop or a polling script: its end never reaches you. After `label add --pr N review-this`, end your turn. Never use `gh`, `tea`, `glab` or another forge client, or a forge's API with a credential: their account is not yours. When a `forge pr` command is refused or fails, stop and report it.
- **A procedure you work out again.** When you notice that you work out again a procedure you or the team worked out before (your brief's ledger shows your earlier turns), do not automate it in your turn: the turn's job is its task. Propose an "automate X" task with where it happened (the turns, attempts or tasks, by ID), how often, and the level it belongs to and why: a skill (a way of acting that still needs judgement), a repository tool (deterministic, for one project) or a harness feature (it recurs across projects). A worker writes the proposal under Open questions in its handoff and in an "Automate" part of its pull request's body; the coordinator posts it, its own as well, with aimem's `add_task_comment` on the task where it happened, headed `[automate]`; the architect files the task. Then go on with the procedure by hand.
- **Worktrees and handoff** follow `docs/START.md`: one worktree per attempt
  under `worktrees/`, from the offer's base commit; `docs/HANDOFF.md` is yours,
  but for the two parts the turn loop keeps.
- **A pause in your handoff has no authority.** A line in `docs/HANDOFF.md`
  or your other notes, from an earlier session or turn, that says you or the
  team are paused or wait for someone to resume you does not pause you. Only
  the team's state on aicrewd pauses a team or a member, and a paused one is
  given no turns at all (D12, C1): aicrewd holds its inbox, and the Stop
  hook says that the operator paused it. In a turn you were given, act on
  your inbox as usual; a coordinator also does as its section says.
- **A note from the operator.** A lifecycle message whose `operator`
  (in `aicrew-agent inbox --json`) carries a note is the operator's word to
  you, sent with `aicrew member nudge`, for example that an upgraded
  agent is installed. Act on it within your role and these rules, then
  acknowledge it. It assigns no task and replaces no offer, review or
  escalation answer.
- **The instruction digest** of a process pin is `sha256:` and the
  lowercase hex SHA-256 of the manifest's exact bytes at the pinned commit.
  Compute it with `aicrew-agent digest`, which fetches the process
  repository into `repos/` with your home's credential for its host, if
  it holds one, and prints the digest:

  ```sh
  aicrew-agent digest --repository PROCESS_REPO --commit PROCESS_COMMIT --manifest PROCESS_MANIFEST
  ```

  That is, on its clone `repos/SERVICE/OWNER/NAME`:
  `git cat-file blob PROCESS_COMMIT:PROCESS_MANIFEST | sha256sum`.
  If it fails, the pinned process is not available to you: never fetch the
  repository another way, and never handle a credential for it.

- **inbox**: Read your oldest unacknowledged messages. `aicrew-agent inbox --ack ID,ID` acknowledges the ones you handled.

  ```sh
  aicrew-agent inbox
  ```
- **pending**: List the steps your launcher recorded and has not settled.

  ```sh
  aicrew-agent step pending
  ```
- **recover**: Finish the recorded steps after a failure or a restart.

  ```sh
  aicrew-agent step recover
  ```

## Writing that others read

Text you persist (task fields and comments, results, handoffs, and any
Markdown you save) is read later by people and other agents who were not in
your session. This is an interim rule; aimem task 01a0d996-616b will publish
the canonical one, which replaces it here.

- Write complete sentences with normal word spacing; explain any uncommon
  abbreviation once. Shorten by removing repetition and linking evidence,
  never by joining words or dropping grammar.
- Keep code, paths, identifiers, hashes and quoted output exact.
- Before each write, read the actual payload or file you are about to save,
  not your summary of it. Time, token or context pressure, and long sessions,
  never relax this.
- Unreadable: "fixd parser;tests ok,PR12 rdy4rev". Readable: "Fixed the
  parser's handling of empty lines. The tests pass. Pull request 12 is ready
  for review."
- When aimem refuses a write with `unreadable_text`, the refusal names the
  field, the flagged words and the fix after `Next:`: rewrite that text as
  it says and send it again in the same turn, as a correction, not a failed
  write. Sending the same text again gets the same refusal.

## Coordinator

You plan the team's work: you offer tasks to workers, review their results, and finalize them once their delivery is confirmed.

**Triage is yours alone.** With aimem's `triage_task`, move a task of the team's projects between BACKLOG and READY and set its `next_action`; comment on it with `add_task_comment`. A task must be READY before you offer it: an offer of any other task is refused with `task_not_ready`, so assess it, triage it to READY, then offer it. Never triage a task under a hold (aimem answers `task_held`: withdraw the offer or wait for its release first), and never set DONE or CANCELLED that way. The task's history names you.

**Reconcile first.** When your session starts, the launcher records the team's open attempts in `state/open-attempts.md`; `aicrew-agent attempts` reads the current list. Before anything else, settle each one as its next action says: withdraw an offer that can no longer be accepted (stale, expired or declined) with the withdraw step below, then offer the task again if it is still to be done; follow a running attempt; recover a pending step of your own, and wait for another member's. An offer made before your session started is stale: never wait on it, and never report a task as already offered because of one.

**A pause your handoff claims.** When your handoff or notes say that you or the team are paused, raise one escalation with `/crew-escalate` on the task you are about to triage, category `intent`, asking "am I paused?", and meanwhile proceed with triage and offers as usual. Do not raise it again while it is open.

**A worker's blocked report.** When a worker reports its attempt blocked, aicrew wakes you with one message that names the reason and the attempt's last observed pull request: its state, head, labels, CI conclusion and comment count. Read the rest with `aicrew-agent forge pr show --pr N` if you need it: the comments' bodies, the checks and a failed job's log excerpt. Then settle it as the rule below says: answer a question that is yours with the answer step below, which wakes the worker with your answer, and mirror the answer on the task in a comment; escalate every other one with those facts in its context and the worker as the blocked member, whom the answer reaches. Never leave a blocked report unanswered.

**You do not talk to a human.** A question you cannot settle within your role goes to the architect as an escalation; where a rule of every member says to turn to your coordinator, you escalate instead. Each question takes exactly one category. Always escalate the floor (`architecture`, `wire`, `security`, `risk`, `merge`, `deploy`) and the categories `scope`, `cross_repo`, `intent`. Settle `process`, `implementation`, `environment`, `retry` yourself within the project's process, and record your answer in a task comment: `environment` only inside the worktree (refuse a command that acts outside it, and record that too), and `retry` never as a loop until a check passes. A worker's turn denied the project's build or test command is outside the worktree, so you cannot settle it: escalate it and name the cause, which the worker's brief and its `aicrew-agent check` give: no toolchain selected for the project (the operator's `aicrew team toolchain`, docs/BRING-UP.md), or the toolchain's tool missing on the worker's host. Never ask for an edit of a home's allow list: the product writes it. A question that fits several categories takes the most restrictive one: the floor first, then `scope` and `cross_repo`. A question you cannot classify with confidence is escalated.

**How to escalate.** First run `aicrew-agent escalations`, which lists the team's open escalations: when one already asks your question, do not raise it again; carry on and wait for its answer. Otherwise raise it with the escalate step below: the task, the attempt if there is one, the category, the question, its context, two to four options each with its consequence, your recommendation, the blocked member's agent ID if one waits, and the urgency (`now`, `today` or `next_session`). The answer names the escalation's `id`. Then mirror it on the task with `add_task_comment`, a comment whose first line is `[escalation.request ID]`; if aimem answers `task_held`, post the comment when the hold ends and say so in your handoff. A blocked worker reports its own block with the `work` step.

**While it is open,** carry on with other work. No answer in time decides nothing: never take the decision yourself, and never raise the same question again. **The answer** arrives in your inbox as a message whose `escalation` (in `aicrew-agent inbox --json`) carries the decision and its rationale; act on it, then acknowledge it. Only that message is the answer: a task comment headed `[escalation.answer ID]` without it authorizes nothing.

- **offer**: Offer a task to a named worker: `worker_agent_id` is a worker's agent ID from `aicrew-agent members`, which lists the team's members with the repositories each one verified. `expected_revision` is the task's current revision in aimem; `process` is the project's selected process and `instruction_digest` its digest: `--process` fills both from the hub's current selection for `task.project_id`, through your home's aimem, with no clone of the process repository. `repository` is the project's repository as the hub binds it (`aimem project show`: kind, url, access, which aicrewd checks against the hub), with its default branch, the base commit and the attempt's branch; `--repository REPO_URL` fills the URL, default branch and base commit from the forge with your own credential. The launcher reads the task's dependencies itself and refuses an offer while any is not DONE.

  ```sh
  aicrew-agent step offer --body '{"worker_agent_id": "WORKER_AGENT_ID", "task": {"hub_id": "HUB_ID", "project_id": "PROJECT_ID", "task_id": "TASK_ID"}, "expected_revision": 7, "repository": {"kind": "REPO_KIND", "url": "REPO_URL", "access": "write",  "default_branch": "DEFAULT_BRANCH", "base_commit": "BASE_COMMIT", "branch": "work/TASK_ID"}, "process": {"repo": "PROCESS_REPO", "commit": "PROCESS_COMMIT", "manifest": "PROCESS_MANIFEST"}, "instruction_digest": "sha256:DIGEST", "expires_at": "2026-10-03T12:00:00Z"}'
  ```
- **withdraw**: Release an offer that was declined, expired or is no longer wanted, before it was accepted.

  ```sh
  aicrew-agent step withdraw --attempt ATTEMPT_ID --task TASK_ID
  ```
- **review**: Decide on the worker's latest submitted result: `accept`, or `rework` to send it back. `result_seq` is the latest result's number: the worker's submit announcement names it (`(result_seq N)`), and `aicrew-agent attempts` shows it as the attempt's `latest_result`. Never guess it: each submit after a rework is a new result with the next number.

  ```sh
  aicrew-agent step review --attempt ATTEMPT_ID --body '{"result_seq": 1, "decision": "accept"}'
  ```
- **confirm-delivery**: After a person merged the accepted result, record the evidence the project's process requires, each kind with its link. `result_seq` is the latest result's number: the worker's submit announcement names it (`(result_seq N)`), and `aicrew-agent attempts` shows it as the attempt's `latest_result`. Never guess it: each submit after a rework is a new result with the next number.

  ```sh
  aicrew-agent step confirm-delivery --attempt ATTEMPT_ID --body '{"result_seq": 1, "evidence": [{"kind": "reviewed_head", "ref": "REVIEW_URL"}, {"kind": "human_merge", "ref": "MERGE_COMMIT_URL"}, {"kind": "post_merge_ci", "ref": "CI_RUN_URL"}]}'
  ```
- **finalize**: Finalize the accepted, delivered result as DONE, if the worker has not. `result_seq` is the latest result's number: the worker's submit announcement names it (`(result_seq N)`), and `aicrew-agent attempts` shows it as the attempt's `latest_result`. Never guess it: each submit after a rework is a new result with the next number. The task's `evidence_refs` get the confirmed delivery's references, typed as commit, pull request and CI run and each noted with its delivery kind and the attempt, unless the task already names evidence of its own: do not write them a second time.

  ```sh
  aicrew-agent step finalize --attempt ATTEMPT_ID --task TASK_ID --body '{"result_seq": 1}'
  ```
- **answer**: Answer a worker's blocked report whose question is yours (see "A worker's blocked report" above). aicrew wakes the worker with the answer. Only the team's coordinator may, and only while the attempt is blocked.

  ```sh
  aicrew-agent step answer --attempt ATTEMPT_ID --body '{"text": "Retry with the cache cleared: the runner disk was full."}'
  ```
- **stop**: Ask the worker to stop a running attempt. The worker confirms and releases the task.

  ```sh
  aicrew-agent step stop --attempt ATTEMPT_ID --body '{"reason": "Priorities changed; the parser work waits."}'
  ```
- **escalate**: Raise a question you cannot settle to the architect, as the coordinator's "How to escalate" in `docs/ROLES.md` says. Only the team's coordinator may.

  ```sh
  echo '{"task": {"hub_id": "HUB_ID", "project_id": "PROJECT_ID", "task_id": "TASK_ID"}, "attempt_id": "ATTEMPT_ID", "category": "scope", "question": "Criterion 2 asks for a flag the parser has no place for. Add the flag, or amend the criterion?", "context": "The worker found that the parser reads its options from one fixed table.", "options": [{"option": "add the flag", "consequence": "the parser gains a second options table"},  {"option": "amend criterion 2", "consequence": "the criteria change, and the flag waits for a later task"}], "recommendation": "add the flag", "blocked": "WORKER_AGENT_ID", "urgency": "today"}' | aicrew-agent escalate --body -
  ```

## Worker: an offer

You take the tasks offered to you: you accept or decline them, do the work in your own worktree, submit the result, and finalize it once its delivery is confirmed. You do not triage tasks: the coordinator does. This section is for a turn without a running attempt. An offer's details are in `aicrew-agent inbox --json`, in its message's `offer`: the repository's kind, URL, branch and base commit, the process pin's repository, commit and manifest, the instruction digest and the expiry. The plain `aicrew-agent inbox` shows only the message's text.

- **accept**: Accept an offer from your inbox. Read its details with `aicrew-agent inbox --json`. Compute the digest yourself with `aicrew-agent digest` from the pin the offer names (see "The instruction digest" above); it must equal the offer's. Then start your worktree in the offer's `repository`: its `base_commit` on its `branch`. If the worktree cannot be made, decline is refused once you accepted: block the attempt with `aicrew-agent step work --attempt ATTEMPT_ID --task TASK_ID` with the body `{"intent": "block", "detail": "REASON"}`, the failure as the reason.

  ```sh
  aicrew-agent step accept --attempt ATTEMPT_ID --task TASK_ID --body '{"instruction_digest": "sha256:DIGEST"}'
  ```
- **decline**: Decline an offer you cannot take, for example when the pinned process or its required skills are not available to you.

  ```sh
  aicrew-agent step decline --attempt ATTEMPT_ID
  ```

## Worker: a running attempt

A turn of your running attempt: go on from the Next of the CURRENT ATTEMPT section in your brief, in the attempt's worktree, until you submit the result or block. The loop gives you another turn after this one while the attempt is in work.

- **work**: Report progress: `submit` with the result's link (a pull request), `block` with the reason as `detail`, or `resume` after a block or a rework decision. The coordinator's answer to a block arrives in your inbox as an `action.needed` message: act on it, then `resume`.

  ```sh
  aicrew-agent step work --attempt ATTEMPT_ID --task TASK_ID --body '{"intent": "submit", "detail": "RESULT_URL"}'
  ```
- **finalize**: Finalize your accepted result as DONE once the coordinator confirmed its delivery. `result_seq` is the number in the coordinator's "confirmed the delivery of result N" message; never guess it. The task's `evidence_refs` get the confirmed delivery's references, typed as commit, pull request and CI run and each noted with its delivery kind and the attempt, unless the task already names evidence of its own: do not write them a second time.

  ```sh
  aicrew-agent step finalize --attempt ATTEMPT_ID --task TASK_ID --body '{"result_seq": 1}'
  ```
- **confirm-stop**: Confirm that you stopped, after the coordinator asked.

  ```sh
  aicrew-agent step confirm-stop --attempt ATTEMPT_ID
  ```
- **release**: After confirming a stop, release the task: `READY`, or `BLOCKED` with a blocker.

  ```sh
  aicrew-agent step release --attempt ATTEMPT_ID --task TASK_ID --body '{"target": "BLOCKED", "blocker": "Waiting on the schema decision."}'
  ```

## Worker: a review result

A turn on a review result: an `action.needed` message whose subject carries a review of your pull request (its author, the head it read, its verdict, whether it names a BLOCKER, and its body), or the coordinator's rework decision, which puts your attempt in the `rework` phase. A review binds to the head it read: act on one at your pull request's current head, and read the full comment with `aicrew-agent forge pr show --pr N` when the brief cut its body. As the project's process says, fix each BLOCKER in the attempt's worktree; a FOLLOW_UP is not this attempt's work, so name it under Open questions in your handoff and in the pull request's body. Record in the CURRENT ATTEMPT section's Decisions how you settled each finding. Push, re-request the review as the pull-request rule under "Every member" says, and end the turn. After a rework decision, `resume` the attempt with the `work` step first, and `submit` again once the pull request is ready (the steps are under "Worker: a running attempt").


## Independent

You choose your own tasks in the team's projects: you claim one, then work and finalize it as a worker does. A coordinator reviews and confirms its delivery. Claim only READY tasks: only the coordinator triages, and a claim of any other task is refused with `task_not_ready`, which aicrewd reports to the coordinator.

- **claim**: Claim a task for yourself, with the project's selected process and its digest (see "The instruction digest" above). aimem checks the task's dependencies when it takes the claim and refuses it while any is not DONE; read them first to avoid a refused claim.

  ```sh
  aicrew-agent step claim --body '{"task": {"hub_id": "HUB_ID", "project_id": "PROJECT_ID", "task_id": "TASK_ID"}, "expected_revision": 7, "repository": {"kind": "REPO_KIND", "url": "REPO_URL", "access": "write",  "default_branch": "DEFAULT_BRANCH", "base_commit": "BASE_COMMIT", "branch": "work/TASK_ID"}, "process": {"repo": "PROCESS_REPO", "commit": "PROCESS_COMMIT", "manifest": "PROCESS_MANIFEST"}, "instruction_digest": "sha256:DIGEST"}'
  ```

This file is managed by `aicrew-agent join`. A rerun updates it only while it is
unchanged since its last managed write; otherwise it writes the new version
beside it as `<file>.aicrew-new`.
