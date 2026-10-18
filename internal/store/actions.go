package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"slices"
	"sort"
	"strings"
	"time"
)

// The action.needed message (docs/DESIGN-TURNS.md, section 5, decision T3;
// increment 1c). A member is woken only for a transition that needs its
// decision, with the content the decision needs, so its turn needs no forge
// read to learn the status:
// - a pull request: a review with a BLOCKER at the head (its body), CI
//   failing on the change (each failed job and its log excerpt), both gates
//   green at the head (the review and the passed checks: mark it ready and
//   submit), or a merge, close or reopen;
// - a worker's blocked report, to the coordinators (the reason and the
//   attempt's pull request);
// - a board change the coordinators react to.
// A pull request change that only reports progress (a label, CI starting, a
// READY review while CI runs) is recorded and wakes no one. Subjects due
// together for the same recipients are one message, which lists each.
//
// A declared check that failed after a push has its field here, filled
// since increment 2d-b: the worker's launcher runs the checks declared at
// the after-push point and reports the first failure with its observations
// of the pull request, and the need holds while the failure stands at the
// head. A conflict in the update from the base has its field too; increment
// 2e's base update fills it. The gates wake is sent only when the
// launcher's effective set lacks the gates-green automation (increment
// 2d-d): with it, the launcher marks the pull request ready and submits
// the attempt itself, and the worker learns of the submit through the
// attempt's lifecycle message.

// KindActionNeeded is a message that wakes a member for a decision, with
// its content.
const KindActionNeeded MessageKind = "action.needed"

// What a pull request subject needs.
const (
	NeedReview   = "review"   // the external review at the head has a BLOCKER
	NeedCI       = "ci"       // CI failed on the change
	NeedGates    = "gates"    // both gates are green at the head
	NeedMerged   = "merged"   // the pull request was merged
	NeedClosed   = "closed"   // the pull request was closed without a merge
	NeedReopened = "reopened" // the pull request was reopened
	NeedPrePush  = "pre_push" // a declared check failed after the push (2d-b)
)

// ActionDetail is an action.needed message's subjects.
type ActionDetail struct {
	Subjects []ActionSubject `json:"subjects"`
}

// ActionSubject is one subject that needs a decision, with its content.
type ActionSubject struct {
	// Kind is pull, blocked, board or automation.
	Kind      string   `json:"kind"`
	AttemptID string   `json:"attempt_id,omitempty"`
	Task      *TaskRef `json:"task,omitempty"`
	// Needs is what a pull request needs (NeedReview and the rest).
	Needs  []string        `json:"needs,omitempty"`
	Pull   *ForgeDetail    `json:"pull,omitempty"`
	Review *PullReview     `json:"review,omitempty"`
	Failed []FailedCheck   `json:"failed,omitempty"`
	Passed []PassedCheck   `json:"passed,omitempty"`
	Board  *BoardDetail    `json:"board,omitempty"`
	Block  *BlockedSubject `json:"blocked,omitempty"`
	// Conflict is the base update's conflict, filled from increment 2e;
	// PrePush the declared check that failed after the push (NeedPrePush).
	Conflict *ConflictDetail `json:"conflict,omitempty"`
	PrePush  *PrePushFailure `json:"pre_push,omitempty"`
	// Runs are an automation subject's runs (automations.go): those that
	// failed, for the coordinators, or asked for a turn, for the worker.
	Runs []AutomationRunReport `json:"runs,omitempty"`
	// Text is the subject in words, as the message's text holds it.
	Text string `json:"text"`
}

// BlockedSubject is a worker's blocked report.
type BlockedSubject struct {
	Worker string `json:"worker_agent_id"`
	Reason string `json:"reason"`
}

// ConflictDetail is a conflict in the update of a branch from its base:
// the base, the conflicting files and their hunks, bounded.
type ConflictDetail struct {
	Base  string   `json:"base"`
	Files []string `json:"files"`
	Hunks string   `json:"hunks,omitempty"`
}

// PrePushFailure is a declared check that failed after a push (increment
// 2d-b): the command in words, its exit status and its output, bounded.
type PrePushFailure struct {
	Command string `json:"command"`
	Exit    int    `json:"exit"`
	Output  string `json:"output"`
	// Cut says the output was cut to the message's room.
	Cut bool `json:"cut,omitempty"`
	// Others names the other checks that failed at the same point, whose
	// output the home's automation log holds.
	Others []string `json:"others,omitempty"`
}

// prePushEqual reports whether two failures are one.
func prePushEqual(a, b *PrePushFailure) bool {
	switch {
	case a == nil || b == nil:
		return a == b
	default:
		return a.Command == b.Command && a.Exit == b.Exit && a.Output == b.Output && slices.Equal(a.Others, b.Others)
	}
}

// PullReview is the external review at a pull request's head, as the
// worker's launcher read it: its header, its verdict, and its body, cut to
// a bound.
type PullReview struct {
	Author string `json:"author"`
	URL    string `json:"url"`
	Head   string `json:"head"`
	AtHead bool   `json:"at_head"`
	// Verdict is the review's verdict line, such as READY_FOR_HUMAN_MERGE;
	// Blocker says it returns the change to implementation.
	Verdict string `json:"verdict,omitempty"`
	Blocker bool   `json:"blocker"`
	Body    string `json:"body"`
	Cut     bool   `json:"cut,omitempty"`
}

// FailedCheck is a check that failed at the head, with its log excerpt or
// why there is none.
type FailedCheck struct {
	Name       string `json:"name"`
	Conclusion string `json:"conclusion,omitempty"`
	URL        string `json:"url,omitempty"`
	Excerpt    string `json:"excerpt,omitempty"`
	Cut        bool   `json:"cut,omitempty"`
	NoLog      string `json:"no_log,omitempty"`
}

// PassedCheck is a check that passed at the head.
type PassedCheck struct {
	Name string `json:"name"`
	URL  string `json:"url,omitempty"`
}

// verdictReady is the review verdict that lets a change through.
const verdictReady = "READY_FOR_HUMAN_MERGE"

// Bounds on the content an event and a message carry, as JSON encodes it.
const (
	maxReviewBody  = 5 << 10
	maxExcerpts    = 5 << 10
	maxFailed      = 10
	maxPassed      = 30
	maxObservedLen = 32 << 10 // a body or an excerpt as the launcher sends it
	maxCheckField  = 1 << 10  // a check's name, conclusion or note, a review's author or verdict
	maxPrePushOut  = 5 << 10  // a failed check's output, as the excerpts' budget
)

// schemaV35 adds the action.needed message kind and its detail, and the
// content facts of each attempt's pull request (the external review at the
// head, the checks), which the next observation is compared with. SQLite
// cannot change the kind's CHECK in place, so messages is rebuilt with every
// row, the foreign keys checked as for every migration.
var schemaV35 = []string{
	`CREATE TABLE messages_v35 (
		id                    TEXT PRIMARY KEY,
		team_id               TEXT NOT NULL REFERENCES teams (id),
		seq                   INTEGER NOT NULL CHECK (seq > 0),
		kind                  TEXT NOT NULL CHECK (kind IN ('message', 'lifecycle', 'action.needed')),
		sender_agent_id       TEXT NOT NULL DEFAULT '',
		sender_session_id     TEXT NOT NULL DEFAULT '',
		sender_generation     INTEGER NOT NULL DEFAULT 0,
		sender_model          TEXT NOT NULL DEFAULT '',
		sender_client         TEXT NOT NULL DEFAULT '',
		sender_client_version TEXT NOT NULL DEFAULT '',
		to_agent_id           TEXT NOT NULL DEFAULT '',
		project_hub_id        TEXT NOT NULL DEFAULT '',
		project_id            TEXT NOT NULL DEFAULT '',
		task_hub_id           TEXT NOT NULL DEFAULT '',
		task_project_id       TEXT NOT NULL DEFAULT '',
		task_id               TEXT NOT NULL DEFAULT '',
		text                  TEXT NOT NULL,
		created_at            TEXT NOT NULL,
		board                 TEXT NOT NULL DEFAULT '',
		operator              TEXT NOT NULL DEFAULT '',
		forge                 TEXT NOT NULL DEFAULT '',
		escalation            TEXT NOT NULL DEFAULT '',
		offer                 TEXT NOT NULL DEFAULT '',
		attempt_id            TEXT NOT NULL DEFAULT '',
		action                TEXT NOT NULL DEFAULT '',
		UNIQUE (team_id, seq)
	)`,
	`INSERT INTO messages_v35 (id, team_id, seq, kind, sender_agent_id, sender_session_id, sender_generation,
		sender_model, sender_client, sender_client_version, to_agent_id, project_hub_id, project_id,
		task_hub_id, task_project_id, task_id, text, created_at, board, operator, forge, escalation, offer, attempt_id)
	 SELECT id, team_id, seq, kind, sender_agent_id, sender_session_id, sender_generation,
		sender_model, sender_client, sender_client_version, to_agent_id, project_hub_id, project_id,
		task_hub_id, task_project_id, task_id, text, created_at, board, operator, forge, escalation, offer, attempt_id
	 FROM messages`,
	`DROP TABLE messages`,
	`ALTER TABLE messages_v35 RENAME TO messages`,
	`CREATE TABLE attempt_pull_facts (
		attempt_id TEXT PRIMARY KEY REFERENCES attempts (id),
		facts      TEXT NOT NULL
	)`,
}

// pullFacts is the content facts of an attempt's pull request, as last
// observed.
type pullContentFacts struct {
	Head   string        `json:"head"`
	Checks string        `json:"checks"`
	Review *PullReview   `json:"review,omitempty"`
	Failed []FailedCheck `json:"failed,omitempty"`
	Passed []PassedCheck `json:"passed,omitempty"`
	// PrePush is the declared check that failed at the head's after-push
	// point, as the last observation carried it; nil once an observation at
	// the head carries none (a run there passed) or the head moved.
	PrePush *PrePushFailure `json:"pre_push,omitempty"`
}

func readPullFacts(ctx context.Context, tx *sql.Tx, attemptID string) (*pullContentFacts, error) {
	var raw string
	err := tx.QueryRowContext(ctx, `SELECT facts FROM attempt_pull_facts WHERE attempt_id = ?`, attemptID).Scan(&raw)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read the pull request's facts: %w", err)
	}
	var f pullContentFacts
	if err := json.Unmarshal([]byte(raw), &f); err != nil {
		return nil, fmt.Errorf("read the pull request's facts: %w", err)
	}
	return &f, nil
}

func writePullFacts(ctx context.Context, tx *sql.Tx, attemptID string, f pullContentFacts) error {
	raw, err := json.Marshal(f)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx,
		`INSERT INTO attempt_pull_facts (attempt_id, facts) VALUES (?, ?)
		 ON CONFLICT (attempt_id) DO UPDATE SET facts = excluded.facts`, attemptID, string(raw))
	if err != nil {
		return fmt.Errorf("record the pull request's facts: %w", err)
	}
	return nil
}

// pullNeeds is what a pull request change needs a decision on: prev is the
// observation before it and pf the content facts before it (nil for none),
// cur the observation and cf its content facts.
func pullNeeds(prev PullObservation, pf *pullContentFacts, cur PullObservation, cf pullContentFacts) []string {
	var needs []string
	if cur.State != prev.State {
		switch cur.State {
		case "merged":
			needs = append(needs, NeedMerged)
		case "closed":
			needs = append(needs, NeedClosed)
		default:
			needs = append(needs, NeedReopened)
		}
	}
	if cur.State != "open" {
		return needs
	}
	if cur.Checks == "failure" && (prev.Checks != "failure" || prev.HeadCommit != cur.HeadCommit) {
		needs = append(needs, NeedCI)
	}
	// A declared check failed at the head: news unless the facts before,
	// read at this head, held the same failure.
	if cf.PrePush != nil && (pf == nil || pf.Head != cur.HeadCommit || !prePushEqual(pf.PrePush, cf.PrePush)) {
		needs = append(needs, NeedPrePush)
	}
	rv := cf.Review
	if rv == nil || !rv.AtHead || !sameCommit(rv.Head, cur.HeadCommit) {
		return needs
	}
	var pr *PullReview
	if pf != nil && pf.Review != nil && pf.Review.URL == rv.URL && sameCommit(pf.Review.Head, rv.Head) {
		pr = pf.Review
	}
	if rv.Blocker && pr == nil {
		needs = append(needs, NeedReview)
	}
	gates := func(r *PullReview, checks, head string) bool {
		return r != nil && !r.Blocker && r.Verdict == verdictReady && checks == "success" && sameCommit(r.Head, head)
	}
	if gates(rv, cur.Checks, cur.HeadCommit) && !(gates(pr, prev.Checks, prev.HeadCommit) && prev.HeadCommit == cur.HeadCommit) &&
		!cur.handles(AutomationGatesGreen) {
		needs = append(needs, NeedGates)
	}
	return needs
}

// sameCommit reports whether two commit names, either possibly abbreviated,
// name one commit.
func sameCommit(a, b string) bool {
	if a == "" || b == "" {
		return false
	}
	return strings.HasPrefix(a, b) || strings.HasPrefix(b, a)
}

// fitText is s, or its longest start that JSON encodes, with a trailing
// ellipsis, within max bytes, and whether it was cut.
func fitText(s string, max int) (string, bool) {
	encoded := func(s string) int { b, _ := json.Marshal(s); return len(b) }
	if encoded(s) <= max {
		return s, false
	}
	lo, hi := 0, len(s)
	for lo < hi {
		mid := (lo + hi + 1) / 2
		if encoded(strings.ToValidUTF8(s[:mid], "")+"…") <= max {
			lo = mid
		} else {
			hi = mid - 1
		}
	}
	return strings.ToValidUTF8(s[:lo], "") + "…", true
}

// boundContent cuts a review body and the failed jobs' excerpts to what an
// event and a message carry, as encoded: the excerpts share one budget.
func boundContent(f *pullContentFacts) {
	if f.Review != nil {
		var cut bool
		f.Review.Body, cut = fitText(f.Review.Body, maxReviewBody)
		f.Review.Cut = f.Review.Cut || cut
	}
	if len(f.Failed) > maxFailed {
		f.Failed = f.Failed[:maxFailed]
	}
	if len(f.Passed) > maxPassed {
		f.Passed = f.Passed[:maxPassed]
	}
	if f.PrePush != nil {
		p := *f.PrePush
		var cut bool
		p.Output, cut = fitText(p.Output, maxPrePushOut)
		p.Cut = p.Cut || cut
		if len(p.Others) > maxFailed {
			p.Others = p.Others[:maxFailed]
		}
		f.PrePush = &p
	}
	left := maxExcerpts
	for i := range f.Failed {
		if f.Failed[i].Excerpt == "" {
			continue
		}
		if left < 256 {
			f.Failed[i].Excerpt, f.Failed[i].Cut = "", true
			f.Failed[i].NoLog = "the message's room for excerpts is spent: `aicrew-agent forge pr show` prints it"
			continue
		}
		var cut bool
		f.Failed[i].Excerpt, cut = fitText(f.Failed[i].Excerpt, left)
		f.Failed[i].Cut = f.Failed[i].Cut || cut
		b, _ := json.Marshal(f.Failed[i].Excerpt)
		left -= len(b)
	}
}

// wakeOut is what one handler run asks to send: one subject, to its
// recipients. The runner merges the subjects of one team's runs that go to
// the same recipients into one message.
type wakeOut struct {
	recipients []string
	subject    ActionSubject
	project    *ProjectRef
	board      *BoardDetail
	forge      *ForgeDetail
}

// writeWakes writes the action.needed messages for one team's handler
// outputs: those with the same recipients become one message, as long as
// its text fits. It returns each output's message ID.
func writeWakes(ctx context.Context, tx *sql.Tx, teamID string, outs []*wakeOut, now time.Time) (map[*wakeOut]string, error) {
	ids := map[*wakeOut]string{}
	groups := map[string][]*wakeOut{}
	var order []string
	for _, o := range outs {
		if o == nil || len(o.recipients) == 0 {
			continue
		}
		r := slices.Clone(o.recipients)
		sort.Strings(r)
		key := strings.Join(slices.Compact(r), ",")
		if _, ok := groups[key]; !ok {
			order = append(order, key)
		}
		groups[key] = append(groups[key], o)
	}
	for _, key := range order {
		var batch []*wakeOut
		size := 0
		flush := func() error {
			if len(batch) == 0 {
				return nil
			}
			id, err := insertWake(ctx, tx, teamID, batch, now)
			if err != nil {
				return err
			}
			for _, o := range batch {
				ids[o] = id
			}
			batch, size = nil, 0
			return nil
		}
		for _, o := range groups[key] {
			n := len(o.subject.Text) + 2
			if len(batch) > 0 && size+n > maxMessageText {
				if err := flush(); err != nil {
					return nil, err
				}
			}
			batch, size = append(batch, o), size+n
		}
		if err := flush(); err != nil {
			return nil, err
		}
	}
	return ids, nil
}

// insertWake writes one action.needed message carrying batch's subjects.
// A message with one subject also names its task, attempt and details, as
// the messages before it did.
func insertWake(ctx context.Context, tx *sql.Tx, teamID string, batch []*wakeOut, now time.Time) (string, error) {
	recipients := slices.Compact(slices.Sorted(slices.Values(batch[0].recipients)))
	m := Message{TeamID: teamID, Kind: KindActionNeeded, Action: &ActionDetail{}}
	var texts []string
	for _, o := range batch {
		m.Action.Subjects = append(m.Action.Subjects, o.subject)
		texts = append(texts, o.subject.Text)
	}
	if len(batch) == 1 {
		o := batch[0]
		m.Task, m.AttemptID, m.Project = o.subject.Task, o.subject.AttemptID, o.project
	}
	// The board and forge details, as the messages before action.needed
	// carried them, when the message holds one subject of that kind.
	var boards, forges int
	for _, o := range batch {
		if o.board != nil {
			boards++
			m.Board = o.board
		}
		if o.forge != nil {
			forges++
			m.Forge = o.forge
		}
	}
	if boards > 1 {
		m.Board = nil
	}
	if forges > 1 {
		m.Forge = nil
	}
	if len(recipients) == 1 {
		m.To = recipients[0]
	}
	m.Text = boundedText(strings.Join(texts, "\n\n"))
	written, err := insertMessage(ctx, tx, m, recipients, now)
	return written.ID, err
}

// encodeAction is a message's action detail as stored, "" for none.
func encodeAction(d *ActionDetail) (string, error) {
	if d == nil {
		return "", nil
	}
	b, err := json.Marshal(d)
	if err != nil {
		return "", fmt.Errorf("encode the action: %w", err)
	}
	return string(b), nil
}

// holdingNeeds is the needs that still hold at the observation o with the
// content cf: a review at its head that asks for changes, CI failing, a
// declared check failing at its head, both gates green at its head, and the
// state it is in. A run that queued a gates-green wake and then a blocking
// review keeps the review alone.
func holdingNeeds(needs []string, o PullObservation, cf pullContentFacts) []string {
	rv := cf.Review
	atHead := rv != nil && sameCommit(rv.Head, o.HeadCommit)
	var out []string
	for _, n := range needs {
		var holds bool
		switch n {
		case NeedReview:
			holds = o.State == "open" && atHead && rv.Blocker
		case NeedCI:
			holds = o.State == "open" && o.Checks == "failure"
		case NeedPrePush:
			holds = o.State == "open" && cf.PrePush != nil
		case NeedGates:
			holds = o.State == "open" && atHead && !rv.Blocker && rv.Verdict == verdictReady && o.Checks == "success" &&
				!o.handles(AutomationGatesGreen)
		case NeedMerged:
			holds = o.State == "merged"
		case NeedClosed:
			holds = o.State == "closed"
		case NeedReopened:
			holds = o.State == "open"
		}
		if holds {
			out = append(out, n)
		}
	}
	return out
}

// eventBudget is the room a forge event's payload may take, below the
// journal's bound.
const eventBudget = maxEventPayload - 512

// fitForgeEvent cuts ev's content until its payload fits eventBudget,
// whatever the metadata the launcher reported: passed checks first, then
// the failed jobs' excerpts, the failed jobs, the review's body, the
// changes in words and the labels. What it leaves out it names in
// ev.Omitted, so the message points to `forge pr show`; the needs stay.
func fitForgeEvent(ev *forgeEvent) error {
	size := func() int { b, _ := json.Marshal(ev); return len(b) }
	if size() <= eventBudget {
		return nil
	}
	var omitted []string
	note := func(n int, what string) {
		if n > 0 {
			omitted = append(omitted, fmt.Sprintf("%d %s", n, what))
		}
	}
	dropped := 0
	for len(ev.Passed) > 0 && size() > eventBudget {
		keep := len(ev.Passed) / 2
		dropped += len(ev.Passed) - keep
		ev.Passed = ev.Passed[:keep]
	}
	note(dropped, "passed check(s)")
	if size() > eventBudget {
		n := 0
		for i := range ev.Failed {
			if ev.Failed[i].Excerpt != "" {
				ev.Failed[i].Excerpt, ev.Failed[i].Cut, n = "", true, n+1
			}
		}
		note(n, "log excerpt(s)")
	}
	if ev.PrePush != nil && size() > eventBudget {
		p := *ev.PrePush
		p.Output, _ = fitText(p.Output, 1<<10)
		p.Cut = true
		ev.PrePush = &p
		omitted = append(omitted, "most of the failed check's output")
	}
	dropped = 0
	for len(ev.Failed) > 0 && size() > eventBudget {
		keep := len(ev.Failed) / 2
		dropped += len(ev.Failed) - keep
		ev.Failed = ev.Failed[:keep]
	}
	note(dropped, "failed check(s)")
	if ev.Review != nil && size() > eventBudget {
		ev.Review.Body, _ = fitText(ev.Review.Body, 1<<10)
		ev.Review.Cut = true
		omitted = append(omitted, "most of the review's body")
	}
	if len(ev.Changes) > 10 && size() > eventBudget {
		n := len(ev.Changes) - 10
		ev.Changes = append(ev.Changes[:10:10], fmt.Sprintf("and %d more change(s)", n))
	}
	if len(ev.Observation.Labels) > 10 && size() > eventBudget {
		note(len(ev.Observation.Labels)-10, "label(s)")
		ev.Observation.Labels = ev.Observation.Labels[:10]
	}
	ev.Omitted = strings.Join(omitted, ", ")
	if n := size(); n > eventBudget {
		return fmt.Errorf("%w: the pull request's event is %d bytes even with its content cut", ErrInvalid, n)
	}
	return nil
}

// pullText is a pull request subject in words, with its content.
func pullText(o PullObservation, needs []string, cf pullContentFacts, omitted string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "action.needed: pull request #%d (%s) at %s:", o.Number, o.URL, short(o.HeadCommit))
	for _, n := range needs {
		switch n {
		case NeedReview:
			r := cf.Review
			fmt.Fprintf(&b, "\n- The external review at the head asks for changes (verdict %s): %s", firstNonEmpty(r.Verdict, "BLOCKER"), r.URL)
			indentInto(&b, r.Body)
			if r.Cut {
				b.WriteString("\n  | [cut: `aicrew-agent forge pr show` prints it whole]")
			}
		case NeedCI:
			b.WriteString("\n- CI failed on the change:")
			for _, f := range cf.Failed {
				fmt.Fprintf(&b, "\n  - %s: %s %s", f.Name, firstNonEmpty(f.Conclusion, "failure"), f.URL)
				switch {
				case f.Excerpt != "":
					indentInto(&b, f.Excerpt)
				case f.NoLog != "":
					fmt.Fprintf(&b, "\n    no log excerpt: %s", f.NoLog)
				}
			}
			if len(cf.Failed) == 0 {
				b.WriteString(" `aicrew-agent forge pr show` names the failed jobs.")
			}
		case NeedPrePush:
			p := cf.PrePush
			fmt.Fprintf(&b, "\n- A declared check failed after the push: `%s` (exit %d). Fix the change and push again; the checks run at the next head.",
				p.Command, p.Exit)
			if strings.TrimSpace(p.Output) == "" {
				b.WriteString("\n  | [no output]")
			} else {
				indentInto(&b, p.Output)
			}
			if p.Cut {
				b.WriteString("\n  | [cut: the home's logs/automations.jsonl holds it whole]")
			}
			if len(p.Others) > 0 {
				fmt.Fprintf(&b, "\n  Other checks failed at this point too: %s; the home's logs/automations.jsonl holds their output.",
					strings.Join(p.Others, ", "))
			}
		case NeedGates:
			names := make([]string, 0, len(cf.Passed))
			for _, p := range cf.Passed {
				names = append(names, p.Name)
			}
			fmt.Fprintf(&b, "\n- Both gates are green at the head: the external review is %s (%s) and CI passed (%s). "+
				"Mark the pull request ready with `aicrew-agent forge pr ready --pr %d` and submit the result.",
				verdictReady, cf.Review.URL, strings.Join(names, ", "), o.Number)
		case NeedMerged:
			b.WriteString("\n- The pull request was merged.")
		case NeedClosed:
			b.WriteString("\n- The pull request was closed without a merge.")
		case NeedReopened:
			b.WriteString("\n- The pull request was reopened.")
		}
	}
	if omitted != "" {
		fmt.Fprintf(&b, "\n- Left out for room: %s. `aicrew-agent forge pr show --pr %d` prints them.", omitted, o.Number)
	}
	return b.String()
}

func indentInto(b *strings.Builder, text string) {
	for line := range strings.SplitSeq(strings.TrimRight(text, "\n"), "\n") {
		b.WriteString("\n  | ")
		b.WriteString(line)
	}
}

func firstNonEmpty(s ...string) string {
	for _, v := range s {
		if v != "" {
			return v
		}
	}
	return ""
}
