package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/BlackVS/aicrew/internal/forge"
)

// A coordinator's delivery checks (task 01a1fa19-19a4). After a person
// merged an accepted result, aicrewd asks the team's coordinator to confirm
// the delivery. A turn cannot run the delivery check: its settings admit no
// such command, and any command that names the home's credentials is
// denied. So the coordinator's launcher runs the installed verify-delivery
// script itself, with the home's forge credential, and records the
// confirmation with the script's own evidence; no model call is needed to
// learn a fact the forge states.
//
// - At first sight of a new action.needed message the launcher reads the
//   inbox once (the read the brief makes) and looks at its subjects. A pull
//   subject that needs `merged`, for an attempt aicrewd lists as accepted,
//   starts that attempt's check. A message whose subjects are all such
//   subjects is deferred: it is kept from the turn gate, the count of
//   unacknowledged turns and the brief while the check runs. Any other
//   message, a mixed one included, goes to a turn as before.
// - The check reads the forge on its own schedule only (deliverySchedule),
//   never on every tick. Pending gives no turn; after a day it goes to a
//   turn as not confirmed. Not confirmed, or a fault of the check itself,
//   goes to a turn once, and the launcher keeps reading at the slowest step
//   until the day is over, and once more when a new message names the
//   attempt, so a cause repaired outside (a re-run CI job) is confirmed
//   without a turn.
// - Confirmed (exit code 0 and the verdict) takes the confirm-delivery step
//   with the script's evidence, then acknowledges the attempt's deferred
//   messages. A refused confirm goes to a turn once and is not repeated.

// The kinds of message the launcher defers: an action.needed message
// (KindActionNeeded) whose subjects are all pull subjects of an accepted
// attempt that need exactly NeedMerged.
const (
	deliveryMessageKind = "action.needed"
	deliverySubjectKind = "pull"
	deliveryNeed        = "merged"
)

const (
	// deliveryScript is the installed verify-delivery helper, under a
	// skill directory.
	deliveryScript = "verify-delivery/verify_delivery.py"
	// maxDeliveryOutput bounds what is read of the script's output, and
	// maxDeliveryStderr of its error stream.
	maxDeliveryOutput = 1 << 20
	maxDeliveryStderr = 4 << 10
	// deliveryDay is how long the launcher reads a check.
	deliveryDay = 24 * time.Hour
	// deliveryKeep is how long a finished check is kept, for a new
	// message's one more read.
	deliveryKeep = 7 * 24 * time.Hour
	// maxDeliveryReadFails is how many ticks in a row a new message may go
	// unread before it goes to a turn as an ordinary message.
	maxDeliveryReadFails = 5
	// maxDeliveryAckFails is how often the acknowledgement of a confirmed
	// delivery's messages is tried before they go to a turn.
	maxDeliveryAckFails = 10
)

// deliveryTimeout bounds one run of the script.
var deliveryTimeout = 2 * time.Minute

// deliverySchedule is the wait after each pending answer: 2, 5, 10, then
// every 15 minutes. The first read is at first sight.
var deliverySchedule = []time.Duration{2 * time.Minute, 5 * time.Minute, 10 * time.Minute, 15 * time.Minute}

// deliveryInterpreters are the names the interpreter is looked up by on
// PATH, in order.
var deliveryInterpreters = []string{"python3", "python"}

// The verdicts and outcomes a check records.
const (
	deliveryConfirmed    = "confirmed"
	deliveryNotConfirmed = "not_confirmed"
	deliveryPending      = "pending"
	deliveryFault        = "fault"
	deliveryRefused      = "refused"
	// deliveryStopped is a run the launcher's own stop ended: nothing of
	// it is recorded, and the check is read again after the restart.
	deliveryStopped = "stopped"
)

// deliveryCheck is one attempt's delivery check.
type deliveryCheck struct {
	Pull      string `json:"pull"`
	ResultSeq int64  `json:"result_seq"`
	// Worker is the attempt's worker agent, whose forge account is the
	// local review's author on Gitea and GitLab.
	Worker string `json:"worker,omitempty"`
	// Deferred are the attempt's messages kept from turns.
	Deferred []string  `json:"deferred,omitempty"`
	Since    time.Time `json:"since"`
	Next     time.Time `json:"next,omitzero"`
	Reads    int       `json:"reads,omitempty"`
	// Verdict and Reason are the last answer, Failed its failed checks.
	Verdict string   `json:"verdict,omitempty"`
	Reason  string   `json:"reason,omitempty"`
	Failed  []string `json:"failed,omitempty"`
	// Handed says the check went to a turn: its deferred messages are
	// released. Done says it reads no more, until a new message for the
	// attempt. Confirmed says the confirm step was recorded, and AckFails
	// how often the acknowledgement since failed.
	Handed    bool `json:"handed,omitempty"`
	Done      bool `json:"done,omitempty"`
	Confirmed bool `json:"confirmed,omitempty"`
	AckFails  int  `json:"ack_fails,omitempty"`
	// ToldAt is when a turn's brief carried the handed answer, which is
	// given once; later briefs only say it was handed over.
	ToldAt time.Time `json:"told_at,omitzero"`
	// Key and Body are the confirm-delivery call, recorded before it is
	// sent: a launcher stopped before it recorded the answer sends the same
	// call again, which aicrewd answers as a replay. Body is kept as its
	// bytes, which the state file's indentation does not touch.
	Key  string `json:"key,omitempty"`
	Body string `json:"body,omitempty"`
}

// deliveryResult is one run of the check.
type deliveryResult struct {
	Verdict  string
	Reason   string
	Failed   []string
	Evidence []StepEvidence
}

// deliveryRun is what one run of the check is given.
type deliveryRun struct {
	Pull     string
	External string   // the external review's author, "" on GitHub
	Local    []string // the local review's authors, none on GitHub
}

// heldMessages are the messages the turn gate does not see this tick.
type heldMessages map[string]bool

// deliveries applies the coordinator's delivery checks: it looks at the new
// messages, runs the checks that are due, and returns the pending messages
// a turn may have, the messages it held back, and the brief's lines.
func (l *TurnLoop) deliveries(ctx context.Context, st *turnState, pending []PendingMessage) ([]PendingMessage, heldMessages,
	[]string, []string) {
	if st.Deliveries == nil {
		st.Deliveries = map[string]*deliveryCheck{}
	}
	now := l.Now()
	var open *OpenAttempts
	held := l.lookAtNew(ctx, st, pending, &open, now)
	l.startEarly(ctx, st, &open, now)
	// gone are the messages acknowledged this tick: the pending list read
	// before still names them.
	gone := heldMessages{}
	for _, id := range sortedChecks(st.Deliveries) {
		c := st.Deliveries[id]
		switch {
		case c.Confirmed:
			l.ackDelivered(ctx, st, id, c, gone)
		case c.Body != "" && !now.Before(c.Next):
			l.confirm(ctx, st, id, c, gone, now)
		case !c.Done && !now.Before(c.Next):
			l.runCheck(ctx, st, id, c, &open, now, gone)
		}
	}
	for id, c := range st.Deliveries {
		if (c.Done || c.Handed) && now.Sub(c.Since) > deliveryKeep {
			delete(st.Deliveries, id)
		}
	}
	// A check still reading holds its messages and gives each turn one
	// line; a handed answer is given to one turn, which it wakes.
	var lines, handed []string
	for _, id := range sortedChecks(st.Deliveries) {
		c := st.Deliveries[id]
		switch {
		case !c.Handed:
			for _, m := range c.Deferred {
				held[m] = true
			}
			lines = append(lines, deliveryLine(id, c))
		case c.ToldAt.IsZero():
			lines = append(lines, deliveryLine(id, c))
			handed = append(handed, id)
		default:
			lines = append(lines, fmt.Sprintf("The launcher's delivery check of attempt %s (%s) is not confirmed; "+
				"it was handed to a turn at %s. Nothing is asked of this turn for it.", id, c.Pull,
				c.ToldAt.UTC().Format(time.RFC3339)))
		}
	}
	for _, e := range sortedEarly(st.DeliveryEarly) {
		lines = append(lines, fmt.Sprintf("Attempt %s's pull request (%s) was merged before its result was "+
			"accepted: the launcher's delivery check starts once the result is accepted; do not run it.", e,
			st.DeliveryEarly[e]))
	}
	lines = append(lines, st.DeliveryNotes...)
	if len(st.DeliveryReleased) > 0 || len(st.DeliveryNotes) > 0 {
		handed = append(handed, "")
	}
	if len(st.DeliveryReleased) > 0 {
		lines = append(lines, releasedLines(st.DeliveryReleased)...)
	}
	var out []PendingMessage
	for _, p := range pending {
		if !held[p.ID] && !gone[p.ID] {
			out = append(out, p)
		}
	}
	// The messages acknowledged this tick leave the turns' view too: the
	// count of unacknowledged turns drops them as it drops held ones.
	for id := range gone {
		held[id] = true
	}
	return out, held, lines, handed
}

// toldDeliveries records the handed answers, and the released messages' and
// notes' lines ("" in ids), as given to a turn. The caller decides when: a
// turn that ran with the lines in its brief, or one that ran on the
// launcher's own wake whatever its brief held, so those lines never start
// a turn twice.
func toldDeliveries(st *turnState, ids []string, now time.Time) {
	for _, id := range ids {
		if c := st.Deliveries[id]; c != nil {
			c.ToldAt = now
		}
		if id == "" {
			st.DeliveryReleased, st.DeliveryNotes = nil, nil
		}
	}
}

// turnRan says whether a turn ran on its brief: finished, cut by its own
// budget, or stopped by its time limit (as the count of unacknowledged
// turns reads it). Any other outcome leaves a handoff to the next turn.
func turnRan(outcome, limitKind string, stopping bool) bool {
	return outcome == TurnFinished || (outcome == TurnStopped && !stopping) ||
		(outcome == TurnLimitReached && limitKind == "budget")
}

func nilIfEmpty[M ~map[K]V, K comparable, V any](m M) M {
	if len(m) == 0 {
		return nil
	}
	return m
}

func sortedEarly(m map[string]string) []string {
	ids := make([]string, 0, len(m))
	for id := range m {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

func sortedChecks(m map[string]*deliveryCheck) []string {
	ids := make([]string, 0, len(m))
	for id := range m {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

// lookAtNew classifies the messages newer than the last one looked at. An
// action.needed message is read once from the inbox; while that read
// fails, the message is held for up to maxDeliveryReadFails ticks, then goes
// to a turn as an ordinary message.
func (l *TurnLoop) lookAtNew(ctx context.Context, st *turnState, pending []PendingMessage, open **OpenAttempts,
	now time.Time) heldMessages {
	held := heldMessages{}
	var fresh []PendingMessage
	for _, p := range pending {
		if p.Seq > st.DeliverySeen || slices.Contains(st.DeliveryUnread, p.ID) {
			fresh = append(fresh, p)
		}
	}
	// Every message looked at now is classified at most once: an unread
	// one is kept by its id, and the others are past the mark.
	for _, p := range fresh {
		st.DeliverySeen = max(st.DeliverySeen, p.Seq)
	}
	st.DeliveryUnread = nil
	if len(fresh) == 0 {
		st.DeliveryReadFails = 0
		return held
	}
	var toRead []PendingMessage
	for _, p := range fresh {
		if p.Kind == deliveryMessageKind {
			toRead = append(toRead, p)
			continue
		}
		// A note about an attempt is never deferred; it is a new message
		// for the attempt's check.
		l.newMessageFor(st, p.AttemptID, now)
	}
	unread := toRead
	if len(toRead) > 0 {
		items, err := l.Host.Inbox(ctx, maxInboxRead)
		if err == nil {
			*open, err = l.openAttempts(ctx, *open)
		}
		if err == nil {
			byID := map[string]inboxMessage{}
			for _, m := range items {
				byID[m.ID] = m
			}
			unread = nil
			for _, p := range toRead {
				m, ok := byID[p.ID]
				if !ok {
					unread = append(unread, p)
					continue
				}
				l.classify(st, m, *open, now)
			}
		} else {
			l.Log.Warn("the inbox was not read for the delivery checks", "error", err.Error())
		}
	}
	if len(unread) > 0 {
		st.DeliveryReadFails++
		if st.DeliveryReadFails < maxDeliveryReadFails {
			for _, p := range unread {
				held[p.ID] = true
				st.DeliveryUnread = append(st.DeliveryUnread, p.ID)
			}
			return held
		}
		l.Log.Warn("new messages were not read for the delivery checks: they go to a turn as they are",
			"messages", len(unread), "ticks", st.DeliveryReadFails)
		for _, p := range unread {
			st.DeliveryReleased = append(st.DeliveryReleased, p.ID)
		}
	}
	st.DeliveryReadFails = 0
	return held
}

// maxInboxRead is the inbox page the launcher reads for its checks.
const maxInboxRead = 100

// openAttempts reads the team's open attempts once per tick.
func (l *TurnLoop) openAttempts(ctx context.Context, have *OpenAttempts) (*OpenAttempts, error) {
	if have != nil {
		return have, nil
	}
	o, err := l.Host.OpenAttempts(ctx)
	if err != nil {
		return nil, err
	}
	return &o, nil
}

// classify starts the check of each accepted attempt a message says was
// merged, and defers the message when that is all it says.
func (l *TurnLoop) classify(st *turnState, m inboxMessage, open *OpenAttempts, now time.Time) {
	if m.Action == nil || len(m.Action.Subjects) == 0 {
		l.newMessageFor(st, m.AttemptID, now)
		return
	}
	pure := true
	var merged []string
	for _, s := range m.Action.Subjects {
		a := acceptedAttempt(open, s.AttemptID)
		// Only the merge of the accepted result's own pull request starts
		// its check: a notice of another pull goes to a turn as it is.
		isMerged := s.Kind == deliverySubjectKind && slices.Contains(s.Needs, deliveryNeed) && a != nil &&
			s.Pull != nil && samePull(s.Pull.URL, a.LatestResult)
		if !isMerged {
			pure = false
			l.newMessageFor(st, s.AttemptID, now)
			if s.Kind == deliverySubjectKind && slices.Contains(s.Needs, deliveryNeed) && s.Pull != nil && a != nil {
				ref := a.LatestResult.Kind + " " + a.LatestResult.Ref
				st.DeliveryNotes = append(st.DeliveryNotes, fmt.Sprintf("%s%s (%s) was merged, but attempt %s's "+
					"accepted result is %s, not that pull request: the launcher starts no delivery check for it. If the "+
					"merge is the result's delivery, escalate it.", "The pull request ", s.Pull.URL, m.ID, s.AttemptID, ref))
			}
			if s.Kind == deliverySubjectKind && slices.Contains(s.Needs, deliveryNeed) && s.Pull != nil &&
				s.Pull.URL != "" && a == nil && openAttempt(open, s.AttemptID) && st.Deliveries[s.AttemptID] == nil {
				// Merged before the result was accepted: the check starts once
				// aicrewd lists the attempt accepted with that pull request.
				if st.DeliveryEarly == nil {
					st.DeliveryEarly = map[string]string{}
				}
				st.DeliveryEarly[s.AttemptID] = s.Pull.URL
				l.Log.Info("a pull request was merged before its result was accepted", "attempt", s.AttemptID)
			}
			continue
		}
		if len(s.Needs) != 1 {
			pure = false
		}
		merged = append(merged, s.AttemptID)
		c := st.Deliveries[s.AttemptID]
		if c != nil && !c.Confirmed && c.Body == "" && !c.binds(a) {
			l.Log.Info("the delivery check starts again: the attempt's accepted result changed", "attempt", s.AttemptID)
			c = nil
		}
		if c == nil {
			c = &deliveryCheck{Pull: a.LatestResult.Ref, ResultSeq: a.LatestResult.ResultSeq, Worker: a.WorkerAgentID,
				Since: now, Next: now}
			st.Deliveries[s.AttemptID] = c
			l.Log.Info("the delivery check of an accepted attempt starts", "attempt", s.AttemptID, "pull", c.Pull)
		} else {
			l.newMessageFor(st, s.AttemptID, now)
		}
	}
	if !pure {
		return
	}
	for _, id := range merged {
		c := st.Deliveries[id]
		if !slices.Contains(c.Deferred, m.ID) {
			c.Deferred = append(c.Deferred, m.ID)
		}
	}
}

// openAttempt says whether the attempt is among the open ones.
func openAttempt(open *OpenAttempts, id string) bool {
	return open != nil && slices.ContainsFunc(open.Attempts, func(a OpenAttempt) bool { return a.ID == id })
}

// startEarly starts the checks of the pull requests merged before their
// result was accepted, once aicrewd lists the attempt accepted with that
// pull request as its result, and forgets those whose attempt left the open
// list. It reads the open attempts, not the forge, at most every
// deliveryEarlyEvery.
func (l *TurnLoop) startEarly(ctx context.Context, st *turnState, open **OpenAttempts, now time.Time) {
	if len(st.DeliveryEarly) == 0 || now.Before(st.DeliveryEarlyNext) {
		return
	}
	st.DeliveryEarlyNext = now.Add(deliveryEarlyEvery)
	o, err := l.openAttempts(ctx, *open)
	if err != nil {
		l.Log.Warn("the open attempts were not read for an early merge", "error", err.Error())
		return
	}
	*open = o
	for id, pull := range st.DeliveryEarly {
		switch a := acceptedAttempt(o, id); {
		case !openAttempt(o, id):
			delete(st.DeliveryEarly, id)
		case a != nil && samePull(pull, a.LatestResult):
			delete(st.DeliveryEarly, id)
			if st.Deliveries[id] == nil {
				st.Deliveries[id] = &deliveryCheck{Pull: a.LatestResult.Ref, ResultSeq: a.LatestResult.ResultSeq,
					Worker: a.WorkerAgentID, Since: now, Next: now}
				l.Log.Info("the delivery check of an accepted attempt starts", "attempt", id, "pull", pull)
			}
		case a != nil:
			// Accepted with another pull request: that merge is not its result.
			delete(st.DeliveryEarly, id)
		}
	}
}

// deliveryEarlyEvery is how often the open attempts are read for a pull
// request merged before its result was accepted.
const deliveryEarlyEvery = 30 * time.Second

// samePull says whether url is the pull request of the accepted result r.
func samePull(url string, r *LatestResult) bool {
	norm := func(s string) string { return strings.ToLower(strings.TrimRight(s, "/")) }
	return r != nil && r.Kind == resultPullRequest && url != "" && norm(url) == norm(r.Ref)
}

// resultPullRequest is the kind of a result whose reference is the
// attempt's watched pull request.
const resultPullRequest = "pull_request"

// binds says whether the check is the attempt's accepted result.
func (c *deliveryCheck) binds(a *OpenAttempt) bool {
	return a != nil && a.LatestResult != nil && a.LatestResult.ResultSeq == c.ResultSeq && samePull(c.Pull, a.LatestResult)
}

// acceptedAttempt is the open attempt id when it is accepted with a result,
// nil otherwise.
func acceptedAttempt(open *OpenAttempts, id string) *OpenAttempt {
	if open == nil || id == "" {
		return nil
	}
	for i := range open.Attempts {
		a := &open.Attempts[i]
		if a.ID == id && a.Phase == "accepted" && a.LatestResult != nil {
			return a
		}
	}
	return nil
}

// newMessageFor gives a finished or waiting check of the attempt one more
// read now: a new message may follow a repair made outside.
func (l *TurnLoop) newMessageFor(st *turnState, attemptID string, now time.Time) {
	c := st.Deliveries[attemptID]
	if attemptID == "" || c == nil || c.Confirmed || c.Verdict == deliveryRefused {
		return
	}
	c.Done, c.Next = false, now
}

// runCheck runs one check that is due and applies its answer.
func (l *TurnLoop) runCheck(ctx context.Context, st *turnState, id string, c *deliveryCheck, open **OpenAttempts,
	now time.Time, gone heldMessages) {
	o, err := l.openAttempts(ctx, *open)
	if err != nil {
		l.Log.Warn("the open attempts were not read for a delivery check", "attempt", id, "error", err.Error())
		return
	}
	*open = o
	if a := acceptedAttempt(o, id); a == nil || !c.binds(a) {
		// Finalized, sent back, stopped or accepted with another result:
		// nothing of this check is left to confirm, and its messages go to
		// a turn as they are.
		l.Log.Info("the delivery check ends: the attempt's accepted result is not the check's", "attempt", id)
		delete(st.Deliveries, id)
		return
	}
	run := deliveryRun{Pull: c.Pull}
	r := l.prepareRun(ctx, c, &run)
	if r.Verdict == "" {
		r = l.verify(ctx, run)
	}
	if r.Verdict == deliveryStopped || ctx.Err() != nil {
		return
	}
	c.Reads++
	c.Verdict, c.Reason, c.Failed = r.Verdict, r.Reason, r.Failed
	l.Log.Info("delivery check", "attempt", id, "pull", c.Pull, "verdict", r.Verdict, "reason", r.Reason)
	past := now.Sub(c.Since) >= deliveryDay
	switch r.Verdict {
	case deliveryConfirmed:
		// The call is recorded before it is sent: a launcher stopped at any
		// point after sends the same call again, never a second confirm.
		body, _ := json.Marshal(map[string]any{"result_seq": c.ResultSeq, "evidence": r.Evidence})
		c.Body = string(body)
		c.Key = fmt.Sprintf("aicrew-agent-delivery-%s-%d", id, c.ResultSeq)
		if err := writeTurnState(l.Home, *st); err != nil {
			l.Log.Warn("record the turn state: the confirm waits for the next tick", "error", err.Error())
			return
		}
		l.confirm(ctx, st, id, c, gone, now)
	case deliveryPending:
		if past {
			c.Verdict, c.Reason = deliveryNotConfirmed, "the delivery check answered pending for a day"
			c.Handed, c.Done = true, true
			return
		}
		c.Next = now.Add(deliverySchedule[min(c.Reads-1, len(deliverySchedule)-1)])
	default:
		c.Handed = true
		c.Done = past
		c.Next = now.Add(deliverySchedule[len(deliverySchedule)-1])
	}
}

// confirm sends the recorded confirm-delivery call and, once aicrewd
// recorded it, acknowledges the deferred messages.
func (l *TurnLoop) confirm(ctx context.Context, st *turnState, id string, c *deliveryCheck, gone heldMessages,
	now time.Time) {
	ans := l.Host.Do(ctx, StepCall{Version: 1, Op: "confirm-delivery", AttemptID: id, Body: json.RawMessage(c.Body),
		Key: c.Key})
	if !ans.OK && (ans.Status == StepFailed || ans.Error != nil && ans.Error.Retryable) {
		// Not answered, or to be tried again: the same call, with the same
		// key, goes again on the schedule, which aicrewd answers as a
		// replay if the first one committed.
		l.Log.Warn("the confirm-delivery step was not recorded: it is sent again", "attempt", id, "error", stepRefusal(ans))
		c.AckFails++ // counts the tries of the call, too
		if now.Sub(c.Since) >= deliveryDay {
			c.Body = ""
			c.Verdict, c.Reason = deliveryRefused, "aicrewd did not record the confirm-delivery step for a day: "+
				stepRefusal(ans)
			c.Handed, c.Done = true, true
			return
		}
		c.Next = now.Add(deliverySchedule[min(c.AckFails-1, len(deliverySchedule)-1)])
		return
	}
	if !ans.OK && ans.Error != nil && ans.Error.Code == "idempotency_conflict" {
		// aicrewd keeps a receipt only for a committed step, and the key
		// names this attempt and result: the call committed before the
		// launcher recorded its answer. A resumed session's generation is
		// part of the step's input, so the same call reads as another.
		l.Log.Info("the confirm-delivery step had committed before the restart", "attempt", id)
		ans = StepAnswer{OK: true, Status: StepDone}
	}
	c.Body, c.AckFails = "", 0
	if !ans.OK {
		c.Verdict, c.Reason = deliveryRefused, "aicrewd refused the confirm-delivery step: "+stepRefusal(ans)
		c.Handed, c.Done = true, true
		return
	}
	c.Confirmed = true
	// The confirmation is recorded before the acknowledgement; without
	// the record the acknowledgement waits for the next tick.
	if err := writeTurnState(l.Home, *st); err != nil {
		l.Log.Warn("record the turn state: the acknowledgement waits for the next tick", "error", err.Error())
		return
	}
	l.ackDelivered(ctx, st, id, c, gone)
}

// prepareRun fills the review authors a Gitea or GitLab check needs, or
// answers not confirmed with the reason one is missing.
func (l *TurnLoop) prepareRun(ctx context.Context, c *deliveryCheck, run *deliveryRun) deliveryResult {
	host, err := pullHost(c.Pull)
	if err != nil {
		return deliveryResult{Verdict: deliveryFault, Reason: err.Error()}
	}
	doc, _, _ := readAgentDoc(l.Home)
	e, ok := doc.forgeEntries()[host]
	if !ok || e.File == "" {
		return deliveryResult{Verdict: deliveryFault, Reason: "the home has no forge credential for " + host}
	}
	if forge.Kind(e.Kind) == forge.GitHub {
		return deliveryResult{}
	}
	if e.ExternalReviewer == "" {
		return deliveryResult{Verdict: deliveryNotConfirmed, Reason: fmt.Sprintf("the external review's author on %s "+
			"is not configured: agent.json's forge.%s.external_reviewer is empty (join --reviewer %s=ACCOUNT sets it)",
			host, host, host)}
	}
	run.External = e.ExternalReviewer
	// The local review is the worker's or the coordinator's own: either
	// author counts.
	account, err := l.memberAccount(ctx, c.Worker, host)
	if err != nil {
		l.Log.Warn("the worker's forge account was not read for a delivery check", "attempt", c.Pull,
			"error", err.Error())
	}
	for _, a := range []string{account, e.Account} {
		if a != "" && !slices.Contains(run.Local, a) {
			run.Local = append(run.Local, a)
		}
	}
	if len(run.Local) == 0 {
		return deliveryResult{Verdict: deliveryNotConfirmed, Reason: fmt.Sprintf("no local review's author is known "+
			"on %s: neither worker %s's account nor this home's", host, c.Worker)}
	}
	return deliveryResult{}
}

// memberAccount is a member's forge account on host, from the team's
// capability reports.
func (l *TurnLoop) memberAccount(ctx context.Context, agentID, host string) (string, error) {
	ans := l.Host.Do(ctx, StepCall{Version: 1, Op: "members"})
	if !ans.OK {
		return "", errors.New("the team's members were not read: " + stepRefusal(ans))
	}
	var team struct {
		Members []struct {
			AgentID      string `json:"agent_id"`
			Capabilities []struct {
				Host    string `json:"host"`
				Account string `json:"account"`
			} `json:"capabilities"`
		} `json:"members"`
	}
	if err := json.Unmarshal(ans.Result, &team); err != nil {
		return "", fmt.Errorf("the team's members are not the expected JSON: %w", err)
	}
	for _, m := range team.Members {
		if m.AgentID != agentID {
			continue
		}
		for _, c := range m.Capabilities {
			if c.Host == host {
				return c.Account, nil
			}
		}
	}
	return "", nil
}

func stepRefusal(ans StepAnswer) string {
	if ans.Error != nil {
		return ans.Error.Code + ": " + ans.Error.Message
	}
	return "no answer"
}

// ackDelivered acknowledges a confirmed delivery's deferred messages,
// possibly none, and ends the check.
func (l *TurnLoop) ackDelivered(ctx context.Context, st *turnState, id string, c *deliveryCheck, gone heldMessages) {
	if len(c.Deferred) > 0 {
		body, _ := json.Marshal(map[string][]string{"ids": c.Deferred})
		if ans := l.Host.Do(ctx, StepCall{Version: 1, Op: "ack", Body: body}); !ans.OK {
			c.AckFails++
			l.Log.Warn("the confirmed delivery's messages were not acknowledged", "attempt", id, "error", stepRefusal(ans))
			if c.AckFails >= maxDeliveryAckFails {
				c.Confirmed, c.Handed, c.Done = false, true, true
				c.Verdict = deliveryConfirmed
				c.Reason = "the launcher confirmed the delivery, but did not acknowledge its messages: acknowledge them"
			}
			return
		}
	}
	for _, m := range c.Deferred {
		gone[m] = true
	}
	l.Log.Info("the delivery is confirmed", "attempt", id, "pull", c.Pull)
	delete(st.Deliveries, id)
}

// deliveryLine is the brief's line on one check.
func deliveryLine(id string, c *deliveryCheck) string {
	if !c.Handed {
		return fmt.Sprintf("The delivery check of attempt %s (%s) is the launcher's and is pending (%d read(s) since %s): "+
			"do not run it; the launcher confirms the delivery and acknowledges its messages.", id, c.Pull, c.Reads,
			c.Since.UTC().Format(time.RFC3339))
	}
	// The action and the reason come first: the brief bounds each line, and
	// a cut takes only the failed checks' tail.
	var b strings.Builder
	fmt.Fprintf(&b, "The launcher's delivery check of attempt %s (%s) is not confirmed (%s). Escalate with this "+
		"reason, and acknowledge the attempt's messages.", id, c.Pull, c.Verdict)
	if !c.Done {
		b.WriteString(" The launcher reads it again until a day has passed, and confirms it if it passes then.")
	}
	b.WriteString(" Reason: " + c.Reason + ".")
	for _, f := range c.Failed {
		b.WriteString(" Failed: " + f + ".")
	}
	return b.String()
}

// releasedLines name the messages released to turns unread: the action
// first, then the ids in lines the brief's line bound keeps whole.
func releasedLines(ids []string) []string {
	lines := []string{"The launcher could not read the message(s) below for its delivery checks. If one says that " +
		"an accepted result's pull request was merged, escalate it with that message: the launcher did not start " +
		"its check."}
	const prefix = "Not read by the launcher: "
	line := ""
	for _, id := range ids {
		if line != "" && len(prefix)+len(line)+2+len(id) > maxBriefText-16 {
			lines = append(lines, prefix+line+".")
			line = ""
		}
		if line != "" {
			line += ", "
		}
		line += id
	}
	return append(lines, prefix+line+".")
}

// pullHost is a pull request URL's host.
func pullHost(pull string) (string, error) {
	rest, ok := strings.CutPrefix(pull, "https://")
	host, _, found := strings.Cut(rest, "/")
	if !ok || !found || host == "" {
		return "", fmt.Errorf("the pull request %q is not an https URL", pull)
	}
	return forge.NormalizeHost(host)
}

// deliveryTokenVar is the environment variable the script reads the
// token's file from, by forge kind.
func deliveryTokenVar(k forge.Kind) string {
	switch k {
	case forge.GitHub:
		return "GITHUB_TOKEN_FILE"
	case forge.GitLab:
		return "GITLAB_TOKEN_FILE"
	}
	return "GITEA_TOKEN_FILE"
}

// verify runs the installed script once: a fixed argument list and no
// shell, the interpreter in isolated mode, the home's state directory as
// its working directory, an environment of PATH and the token file's
// variable (with SSL_CERT_FILE when the launcher has one, and SYSTEMROOT on
// Windows, which the interpreter needs there), a timeout and a bound on its
// output. The token's value is never in an argument or a log line.
func (l *TurnLoop) verify(ctx context.Context, run deliveryRun) deliveryResult {
	fault := func(f string, a ...any) deliveryResult {
		return deliveryResult{Verdict: deliveryFault, Reason: fmt.Sprintf(f, a...)}
	}
	script := ""
	for _, d := range skillDirs("claude", l.Home) {
		p := filepath.Join(d, filepath.FromSlash(deliveryScript))
		if fi, err := os.Stat(p); err == nil && fi.Mode().IsRegular() {
			script = p
			break
		}
	}
	if script == "" {
		return fault("the verify-delivery skill's script %s is not installed", deliveryScript)
	}
	names := l.Interpreters
	if len(names) == 0 {
		names = deliveryInterpreters
	}
	interp := ""
	for _, n := range names {
		if p, err := exec.LookPath(n); err == nil {
			interp = p
			break
		}
	}
	if interp == "" {
		return fault("no interpreter for the delivery check: none of %s is on PATH", strings.Join(names, ", "))
	}
	host, err := pullHost(run.Pull)
	if err != nil {
		return fault("%s", err.Error())
	}
	doc, _, _ := readAgentDoc(l.Home)
	e := doc.forgeEntries()[host]
	if e.File == "" {
		return fault("the home has no forge credential for %s", host)
	}
	kind := forge.Kind(e.Kind)
	args := []string{"-I", script, "--pr", run.Pull}
	switch kind {
	case forge.Gitea:
		args = append(args, "--forge", "gitea", "--api-base", "https://"+host+"/api/v1")
	case forge.GitLab:
		args = append(args, "--forge", "gitlab")
	}
	for _, a := range run.Local {
		args = append(args, "--review-author", "local="+a)
	}
	if run.External != "" {
		args = append(args, "--review-author", "external="+run.External)
	}
	env := []string{"PATH=" + os.Getenv("PATH"),
		deliveryTokenVar(kind) + "=" + filepath.Join(l.Home, "creds", e.File)}
	if v := os.Getenv("SSL_CERT_FILE"); v != "" {
		env = append(env, "SSL_CERT_FILE="+v)
	}
	if runtime.GOOS == "windows" {
		env = append(env, "SYSTEMROOT="+os.Getenv("SYSTEMROOT"))
	}
	dir := filepath.Join(l.Home, "state")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fault("the check's working directory: %v", err)
	}
	cctx, cancel := context.WithTimeout(ctx, deliveryTimeout)
	defer cancel()
	cmd := exec.CommandContext(cctx, interp, args...)
	cmd.Dir, cmd.Env = dir, env
	cmd.WaitDelay = 5 * time.Second
	var stderr bytes.Buffer
	cmd.Stderr = &limitedWriter{w: &stderr, n: maxDeliveryStderr}
	out, err := cmd.StdoutPipe()
	if err != nil {
		return fault("the check's output: %v", err)
	}
	if err := cmd.Start(); err != nil {
		return fault("the check did not start: %v", err)
	}
	raw, rerr := io.ReadAll(io.LimitReader(out, maxDeliveryOutput+1))
	_, _ = io.Copy(io.Discard, out)
	werr := cmd.Wait()
	switch {
	case cctx.Err() != nil && ctx.Err() == nil:
		return fault("the check did not end within %s", deliveryTimeout)
	case ctx.Err() != nil:
		return deliveryResult{Verdict: deliveryStopped}
	case rerr != nil:
		return fault("the check's output was not read: %v", rerr)
	case len(raw) > maxDeliveryOutput:
		return fault("the check's output is over %d bytes", maxDeliveryOutput)
	}
	code := 0
	var exitErr *exec.ExitError
	if errors.As(werr, &exitErr) {
		code = exitErr.ExitCode()
	} else if werr != nil {
		return fault("the check failed: %v", werr)
	}
	return parseDelivery(raw, code, strings.TrimSpace(stderr.String()))
}

// parseDelivery reads the script's answer. Only exit code 0 with the
// verdict confirmed and its evidence confirms.
func parseDelivery(raw []byte, code int, stderr string) deliveryResult {
	var v struct {
		Verdict string `json:"verdict"`
		Checks  []struct {
			Name   string `json:"name"`
			Status string `json:"status"`
			Detail string `json:"detail"`
			Reason string `json:"reason"`
		} `json:"checks"`
		Evidence []StepEvidence `json:"evidence"`
		Error    *struct {
			Kind    string `json:"kind"`
			Message string `json:"message"`
		} `json:"error"`
	}
	jerr := json.Unmarshal(raw, &v)
	tail := func() string {
		if stderr == "" {
			return ""
		}
		return ": " + cutText(stderr, 300)
	}
	if code == 2 {
		return deliveryResult{Verdict: deliveryFault, Reason: "the check refused its invocation (exit code 2)" + tail()}
	}
	if jerr != nil || v.Verdict == "" {
		return deliveryResult{Verdict: deliveryFault, Reason: fmt.Sprintf("the check exited %d without a verdict that parses%s",
			code, tail())}
	}
	var failed []string
	for _, c := range v.Checks {
		if c.Status == "failed" || c.Status == "pending" {
			f := c.Name + " " + c.Status
			if d := first(c.Detail, c.Reason); d != "" {
				f += " (" + cutText(d, 200) + ")"
			}
			failed = append(failed, f)
		}
	}
	switch {
	case code == 0 && v.Verdict == deliveryConfirmed && len(v.Evidence) > 0:
		return deliveryResult{Verdict: deliveryConfirmed, Evidence: v.Evidence}
	case code == 4 && v.Verdict == deliveryPending:
		return deliveryResult{Verdict: deliveryPending, Reason: "CI on the merge commit or the forge is not done", Failed: failed}
	case code == 3 && v.Verdict == "not_confirmed":
		return deliveryResult{Verdict: deliveryNotConfirmed, Reason: "the check did not confirm the delivery", Failed: failed}
	}
	return deliveryResult{Verdict: deliveryFault, Reason: fmt.Sprintf("the check exited %d with the verdict %q", code,
		cutText(v.Verdict, 40))}
}

func first(s ...string) string {
	for _, v := range s {
		if v != "" {
			return v
		}
	}
	return ""
}

// deliveryState is what a tick's delivery checks may change of the turn
// state, so that an idle tick writes nothing.
func deliveryState(st turnState) []byte {
	raw, _ := json.Marshal(struct {
		D  map[string]*deliveryCheck
		S  int64
		F  int
		U  []string
		R  []string
		N  []string
		E  map[string]string
		EN time.Time
		UN *unackedState
	}{nilIfEmpty(st.Deliveries), st.DeliverySeen, st.DeliveryReadFails, st.DeliveryUnread, st.DeliveryReleased,
		st.DeliveryNotes, nilIfEmpty(st.DeliveryEarly), st.DeliveryEarlyNext, st.Unacked})
	return raw
}
