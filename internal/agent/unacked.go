package agent

import (
	"fmt"
	"slices"
	"strings"
	"time"
	"unicode"
)

// Turns without progress on unacknowledged messages (task 01a1fa19; the
// first team's coordinator ran 41 turns in twelve minutes on one message it
// could not act on). A turn whose only cause is the member's unacknowledged
// messages, which finishes or is cut by its own budget and leaves every one
// of them unacknowledged, is a turn without progress. The next such turn
// waits unackedWait after the first, four times that after the second, and
// after the third the member is held: one turn a day, or one turn no sooner
// than the longest wait after its last when a new message reaches it. The
// hold is reported to aicrewd, which shows it in the team state and tells
// the team's other coordinators once. Only an acknowledgement by the member,
// or a pause by the operator (then a resume), clears the count: a new message
// alone does not, so a coordinator writing to a held member cannot restart
// the three turns. The continuation turns of an attempt in work keep their
// own cadence and the stall rules.

const (
	// unackedTurns is how many turns without progress hold the member.
	unackedTurns = 3
	// defaultUnackedWait is the wait after the first turn without progress;
	// the next is unackedCeiling times it, which is also the least time
	// between two turns of a held member.
	defaultUnackedWait = 5 * time.Minute
	unackedCeiling     = 4
	// unackedRecheck is a held member's turn when no new message reaches it.
	unackedRecheck = 24 * time.Hour
	// maxUnackedOutcomes is how many earlier turns the brief names.
	maxUnackedOutcomes = 3
	// maxHoldMessages and maxHoldSummary are aicrewd's bounds on an
	// unacknowledged hold (store.MaxHoldMessages, store.MaxHoldSummary).
	maxHoldMessages = 10
	maxHoldSummary  = 1024
	// maxHoldTurns is aicrewd's bound on the turns a hold names
	// (store.MaxHoldTurns): the report names the smaller.
	maxHoldTurns = 10000
	// maxUnackedSummary bounds an earlier turn's summary in the brief.
	maxUnackedSummary = 300
)

// unackedState is the count of turns without progress, in the turn state.
type unackedState struct {
	// Messages are the ids the last such turn was admitted on, oldest
	// first, as the pending read lists them; Stuck the ids the count began
	// with, which only an acknowledgement (or their leaving the pending
	// read) or a pause clears.
	Messages []string `json:"messages"`
	Stuck    []string `json:"stuck"`
	// Served is the newest message's seq a turn was admitted on: a
	// pending message newer than it has not had its turn.
	Served int64 `json:"served"`
	Turns  int   `json:"turns"`
	// Since is when the first such turn ended: the hold's identity on
	// aicrewd, so a set that grew is one hold.
	Since     time.Time `json:"since"`
	LastEnded time.Time `json:"last_ended"`
	// Outcomes are the last turns' ends, for the brief.
	Outcomes []unackedTurn `json:"outcomes,omitempty"`
}

// unackedTurn is one turn without progress as the brief names it.
type unackedTurn struct {
	Turn    string    `json:"turn"`
	EndedAt time.Time `json:"ended_at"`
	Outcome string    `json:"outcome"`
	Summary string    `json:"summary,omitempty"`
}

func pendingIDs(p []PendingMessage) []string {
	ids := make([]string, len(p))
	for i, m := range p {
		ids[i] = m.ID
	}
	return ids
}

// unackedCountable reports whether a turn's only cause is the member's
// unacknowledged messages: messages are pending, it is no continuation, and
// the member has no attempt in work, whose turns keep their own cadence and
// the stall rules. An attempt the stall rules stalled no longer bounds its
// turns (its block may have been refused), so it is no exception.
func unackedCountable(st turnState, pending []PendingMessage, continuation bool, attempt *memberAttempt) bool {
	return len(pending) > 0 && !continuation && !boundedAttempt(st, attempt)
}

// boundedAttempt reports whether attempt is in work and not stalled: its
// turns are bounded by the continuations and the stall rules.
func boundedAttempt(st turnState, attempt *memberAttempt) bool {
	if !attempt.inWork() {
		return false
	}
	a := st.Attempts[attempt.ID]
	return a == nil || !a.Stalled
}

// unackedDue is when the member may next have a turn, the zero time when
// the count does not hold it back. A pending message newer than any a turn
// was admitted on gives a held member one turn.
func unackedDue(st turnState, pending []PendingMessage, wait time.Duration) time.Time {
	u := st.Unacked
	if u == nil || u.Turns == 0 {
		return time.Time{}
	}
	ceiling := unackedCeiling * wait
	switch {
	case u.Turns == 1:
		return u.LastEnded.Add(wait)
	case u.Turns < unackedTurns, maxSeq(pending) > u.Served:
		return u.LastEnded.Add(ceiling)
	}
	return u.LastEnded.Add(unackedRecheck)
}

// unackedHold is the hold the count reports, nil while it holds nothing.
func unackedHold(st turnState) *holdReport {
	u := st.Unacked
	if u == nil || u.Turns < unackedTurns {
		return nil
	}
	r := &holdReport{Kind: holdUnacknowledged, Messages: u.Messages, Turns: min(u.Turns, maxHoldTurns), StuckSince: u.Since,
		NextCheck: u.LastEnded.Add(unackedRecheck)}
	if len(r.Messages) > maxHoldMessages {
		r.Messages = r.Messages[:maxHoldMessages]
	}
	if n := len(u.Outcomes); n > 0 {
		r.Summary = cutText(u.Outcomes[n-1].Summary, maxHoldSummary)
	}
	return r
}

// holdSummary is a turn's summary as an unacknowledged hold carries it:
// redacted, on one line with no control character, at most maxHoldSummary
// bytes.
func holdSummary(home, s string) string {
	s = tokenShapes.ReplaceAllString(RedactForgeTokens(home, s), "[redacted]")
	s = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return ' '
		}
		return r
	}, strings.ToValidUTF8(s, ""))
	return cutText(strings.Join(strings.Fields(s), " "), maxHoldSummary)
}

// clearUnacked ends the count and the hold it reports.
func (l *TurnLoop) clearUnacked(st *turnState, why string) {
	if st.Unacked == nil {
		return
	}
	l.Log.Info("the turns without progress are cleared: "+why, "turns", st.Unacked.Turns)
	st.Unacked = nil
	if st.HoldReport != nil && st.HoldReport.Kind == holdUnacknowledged {
		setHold(st, nil)
	}
	if err := writeTurnState(l.Home, *st); err != nil {
		l.Log.Warn("record the turn state", "error", err.Error())
	}
}

// noteGone clears the count once a message it began with is no longer
// pending: the member acknowledged it, or it left the pending read for
// another reason, such as a project grant withdrawn. The read lists the
// oldest messages first, so a newer one never pushes them out.
func (l *TurnLoop) noteGone(st *turnState, pending []PendingMessage) {
	if st.Unacked != nil && anyMissing(st.Unacked.Stuck, pendingIDs(pending)) {
		l.clearUnacked(st, "a message the count began with is no longer pending")
	}
}

// countUnacked counts a turn on admitted, the pending messages it was
// admitted with, that ended and left the stuck messages (those the count
// began with, or admitted when it begins) pending; after is the pending read
// after it, when read says it succeeded (a turn after a failed read counts
// as one that acknowledged nothing). A turn that ends finished, cut by its own budget or
// stopped by its time limit counts; one stopped because the loop is
// stopping, or failed, does not, and a failed turn keeps its own backoff and
// leaves its new messages new.
func (l *TurnLoop) countUnacked(st *turnState, rec TurnRecord, admitted, after []PendingMessage, read, stopping bool) {
	u := st.Unacked
	stuck := pendingIDs(admitted)
	if u != nil {
		stuck = u.Stuck
	}
	if read && anyMissing(stuck, pendingIDs(after)) {
		l.clearUnacked(st, "the turn acknowledged a message the count began with")
		return
	}
	served := maxSeq(admitted)
	if u != nil {
		u.LastEnded = rec.EndedAt
		if rec.Outcome != TurnFailed {
			u.Served = max(u.Served, served)
		}
	}
	counted := rec.Outcome == TurnFinished || (rec.Outcome == TurnStopped && !stopping) ||
		(rec.Outcome == TurnLimitReached && rec.LimitKind == "budget")
	if !counted {
		return
	}
	if u == nil {
		u = &unackedState{Stuck: stuck, Since: rec.EndedAt, LastEnded: rec.EndedAt, Served: served}
		st.Unacked = u
	}
	u.Turns++
	u.Messages = pendingIDs(admitted)
	u.Outcomes = append(u.Outcomes, unackedTurn{Turn: rec.ID, EndedAt: rec.EndedAt, Outcome: rec.Outcome,
		Summary: holdSummary(l.Home, rec.Summary)})
	if n := len(u.Outcomes) - maxUnackedOutcomes; n > 0 {
		u.Outcomes = u.Outcomes[n:]
	}
	if u.Turns == unackedTurns {
		l.Log.Warn("no turn on the unacknowledged messages but one a day, or one per new message: turns in a row "+
			"left them unacknowledged", "turns", u.Turns, "messages", strings.Join(u.Messages, ","))
		return
	}
	l.Log.Info("the turn left its messages unacknowledged", "turns", u.Turns, "messages", strings.Join(u.Messages, ","))
}

// maxSeq is the newest message's seq in p, 0 for none.
func maxSeq(p []PendingMessage) int64 {
	var n int64
	for _, m := range p {
		n = max(n, m.Seq)
	}
	return n
}

// anyMissing reports whether one of ids is not in from.
func anyMissing(ids, from []string) bool {
	for _, id := range ids {
		if !slices.Contains(from, id) {
			return true
		}
	}
	return false
}

// briefUnacked tells a turn on messages earlier turns left unacknowledged
// which turn it is and how the earlier ones ended, from the launcher's count.
func briefUnacked(b *strings.Builder, u *unackedState) {
	if u == nil || u.Turns == 0 {
		return
	}
	fmt.Fprintf(b, "\nThis is turn %d on message(s) %s: the last %d turn(s) ended and left them unacknowledged. "+
		"The launcher waits longer before each such turn and, after %d, gives the member one such turn a day; "+
		"it tells the operator in the team's state. Acknowledge a message you have handled. If you cannot act on "+
		"one, say why in your summary.\n", u.Turns+1, strings.Join(u.Messages, ", "), u.Turns, unackedTurns)
	for _, o := range u.Outcomes {
		fmt.Fprintf(b, "- Turn %s ended %s, %s: %s\n", o.Turn, o.EndedAt.UTC().Format(time.RFC3339), o.Outcome,
			orNone(cutText(o.Summary, maxUnackedSummary)))
	}
}

func orNone(s string) string {
	if s == "" {
		return "no summary"
	}
	return s
}
