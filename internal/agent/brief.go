package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// The turn brief (docs/DESIGN-TURNS.md, section 3, decision T1; increment
// 3b): every turn is a fresh conversation, and the launcher writes into its
// prompt what the turn starts from, so the model spends no tool calls
// finding its bearings. In order: the attempt's state as aicrewd's status
// gives it, on the first line; who and where the member is; the frozen
// task; what started the turn, with the messages' content; the pull
// requests as the forge watch last observed them; where to fetch more.
// Every text from outside the launcher is redacted of token shapes and
// bounded, and the whole brief is bounded.

const (
	// maxBriefBytes bounds the whole brief; maxBriefTaskBytes the frozen
	// task's text; maxBriefText one message's, review's or excerpt's text;
	// maxBriefMessages how many pending messages the brief carries in full.
	maxBriefBytes     = 24 << 10
	maxBriefTaskBytes = 8 << 10
	maxBriefText      = 1 << 10
	maxBriefMessages  = 10
)

// TaskSnapshotFile is where the launcher keeps the frozen text of an
// attempt's task, relative to the home, with the attempt's ID in place of
// the asterisk: launcher data, written at the attempt's first turn from
// the board and carried unchanged by every later turn of the attempt.
const TaskSnapshotFile = "state/attempts/*/task.json"

// taskSnapshot is TaskSnapshotFile: the task's text as the board held it
// when the launcher read it, with the board revision and the read time, so a
// reader can tell the snapshot from the live task.
type taskSnapshot struct {
	Task               TaskRef   `json:"task"`
	Revision           int64     `json:"revision"`
	ReadAt             time.Time `json:"read_at"`
	Title              string    `json:"title"`
	Objective          string    `json:"objective"`
	AcceptanceCriteria string    `json:"acceptance_criteria"`
	NonGoals           string    `json:"non_goals"`
	// Cut says the text was cut to maxBriefTaskBytes.
	Cut bool `json:"cut,omitempty"`
}

func taskSnapshotPath(home, attemptID string) string {
	return filepath.Join(home, filepath.FromSlash(strings.Replace(TaskSnapshotFile, "*", attemptID, 1)))
}

// inboxMessage is a delivered message as the brief carries it: the fields
// of aicrewd's inbox item the turn acts on (internal/store, InboxItem).
type inboxMessage struct {
	ID            string   `json:"id"`
	Seq           int64    `json:"seq"`
	Kind          string   `json:"kind"`
	SenderAgentID string   `json:"sender_agent_id"`
	AttemptID     string   `json:"attempt_id"`
	Task          *TaskRef `json:"task"`
	Text          string   `json:"text"`
	Deliveries    int64    `json:"deliveries"`
	Operator      *struct {
		Note string `json:"note"`
		By   string `json:"by"`
	} `json:"operator"`
	Escalation *struct {
		ID        string `json:"id"`
		Category  string `json:"category"`
		Question  string `json:"question"`
		Decision  string `json:"decision"`
		Rationale string `json:"rationale"`
	} `json:"escalation"`
	Board  *briefBoard `json:"board"`
	Forge  *briefPull  `json:"forge"`
	Action *struct {
		Subjects []actionSubject `json:"subjects"`
	} `json:"action"`
}

// briefPull is a pull request's facts as a message carries them.
type briefPull struct {
	Pull       int64    `json:"pull"`
	URL        string   `json:"url"`
	State      string   `json:"state"`
	HeadCommit string   `json:"head_commit"`
	Labels     []string `json:"labels"`
	Checks     string   `json:"checks"`
	Comments   int64    `json:"comments"`
	Changes    []string `json:"changes"`
}

type briefBoard struct {
	Revision int64  `json:"revision"`
	From     string `json:"from"`
	To       string `json:"to"`
}

// actionSubject is one subject of an action.needed message: what needs a
// decision, with its content.
type actionSubject struct {
	Kind      string       `json:"kind"`
	AttemptID string       `json:"attempt_id"`
	Task      *TaskRef     `json:"task"`
	Needs     []string     `json:"needs"`
	Pull      *briefPull   `json:"pull"`
	Review    *briefReview `json:"review"`
	Failed    []briefCheck `json:"failed"`
	Passed    []briefCheck `json:"passed"`
	Board     *briefBoard  `json:"board"`
	Blocked   *struct {
		Worker string `json:"worker_agent_id"`
		Reason string `json:"reason"`
	} `json:"blocked"`
	Conflict *struct {
		Base  string   `json:"base"`
		Files []string `json:"files"`
	} `json:"conflict"`
	PrePush *struct {
		Command string `json:"command"`
		Exit    int    `json:"exit"`
		Output  string `json:"output"`
	} `json:"pre_push"`
}

type briefReview struct {
	Author  string `json:"author"`
	URL     string `json:"url"`
	Head    string `json:"head"`
	AtHead  bool   `json:"at_head"`
	Verdict string `json:"verdict"`
	Blocker bool   `json:"blocker"`
	Body    string `json:"body"`
}

type briefCheck struct {
	Name       string `json:"name"`
	Conclusion string `json:"conclusion"`
	URL        string `json:"url"`
	Excerpt    string `json:"excerpt"`
	NoLog      string `json:"no_log"`
}

// briefInput is what a turn's brief is written from.
type briefInput struct {
	status       memberStatus
	attempt      *memberAttempt // the open attempt, nil when none
	trigger      string
	pending      []PendingMessage
	continuation bool
	// handoff is a worker's session doc as the turn starts (handoff.go),
	// nil for any other member or when it could not be read.
	handoff *handoffDoc
}

// brief writes the turn's brief, and returns the ids of the messages it
// carried.
func (l *TurnLoop) brief(ctx context.Context, in briefInput) (string, []string) {
	var b strings.Builder
	l.briefHead(&b, in)
	l.briefTask(ctx, &b, in.attempt)
	msgs := l.briefTrigger(ctx, &b, in)
	role := in.status.Session.Role
	kind := turnKindOf(role, in.attempt, msgs)
	if kind == kindCoordinator {
		l.briefTeam(ctx, &b)
	}
	l.briefPulls(&b)
	redact := func(s string) string { return tokenShapes.ReplaceAllString(RedactForgeTokens(l.Home, s), "[redacted]") }
	rest := redact(b.String())
	fetch := briefFetch(role, kind)
	// The handoff and the ledger take the room the other parts leave, so
	// they are what the bound cuts first.
	room := maxBriefBytes - len(rest) - len(fetch)
	var memory string
	if kind == kindCoordinator {
		memory = l.coordinatorHandoff(redact, room)
	} else {
		memory = l.briefMemory(in, room, redact)
	}
	// Every text from outside the launcher passes the redaction once more
	// as a whole, so a label, a check's name or URL, a review's author or
	// a command is covered as a body is (hands review of #189). The last
	// resort cuts the body, never the last part, which names the turn's
	// ROLES.md sections (hands review of #193).
	body, fetch := redact(rest+memory), redact(fetch)
	if len(body)+len(fetch) > maxBriefBytes {
		const note = "\n\n(The brief was cut at its bound; `aicrew-agent inbox` has the rest.)\n"
		body = cutText(body, max(maxBriefBytes-len(fetch)-len(note), 0)) + note
	}
	ids := make([]string, 0, len(msgs))
	for _, m := range msgs {
		ids = append(ids, m.ID)
	}
	return body + fetch, ids
}

// briefHead is the first line, the attempt's state as aicrewd's status
// gives it, and who and where the member is.
func (l *TurnLoop) briefHead(b *strings.Builder, in briefInput) {
	if a := in.status.Attempt; a != nil {
		fmt.Fprintf(b, "Attempt %s: %s/%s.\n\n", a.ID, orUnknown(a.State), orUnknown(a.Phase))
	} else {
		b.WriteString("No open attempt.\n\n")
	}
	s := in.status.Session
	fmt.Fprintf(b, "## Who and where\n\n- Role: %s; team %s; agent %s.\n- Home: %s.\n", orUnknown(s.Role), s.TeamID,
		s.AgentID, l.Home)
	a := in.attempt
	if a == nil {
		return
	}
	fmt.Fprintf(b, "- Attempt %s on task %s (project %s, hub %s).\n", a.ID, a.Task.TaskID, a.Task.ProjectID, a.Task.HubID)
	fmt.Fprintf(b, "- Branch %s from base commit %s; worktree `worktrees/%s`.\n", orUnknown(a.Branch),
		orUnknown(a.BaseCommit), a.ID)
	if r := a.Repository; r != nil {
		fmt.Fprintf(b, "- Repository %s (default branch %s).\n", r.URL, orUnknown(r.DefaultBranch))
	}
	if p := a.Process; p != nil {
		fmt.Fprintf(b, "- Process pin: %s at %s, manifest %s.\n", p.Repository, p.Commit, p.Manifest)
	}
}

// briefTask is the frozen task: the attempt's snapshot, taken from the
// board at the attempt's first turn and the same for every later turn.
func (l *TurnLoop) briefTask(ctx context.Context, b *strings.Builder, a *memberAttempt) {
	b.WriteString("\n## The task\n\n")
	if a == nil {
		b.WriteString("No task: no open attempt.\n")
		return
	}
	snap, fresh, err := l.frozenTask(ctx, a)
	if err != nil {
		fmt.Fprintf(b, "Task %s could not be read for the brief (%s): read it with aimem's `get_task`.\n",
			a.Task.TaskID, redactBrief(l.Home, err.Error(), maxBriefText))
		return
	}
	at := snap.ReadAt.UTC().Format(time.RFC3339)
	if fresh {
		fmt.Fprintf(b, "Task %s as the board holds it now (read at %s, revision %d; this first read is kept as the "+
			"attempt's snapshot, which every later turn carries unchanged).\n", a.Task.TaskID, at, snap.Revision)
	} else {
		fmt.Fprintf(b, "Task %s as the board held it at %s, revision %d: the attempt's snapshot, taken at its first "+
			"turn; the live task may have moved on.\n", a.Task.TaskID, at, snap.Revision)
	}
	fmt.Fprintf(b, "\n- Title: %s\n- Objective: %s\n- Acceptance criteria: %s\n- Non-goals: %s\n",
		snap.Title, snap.Objective, snap.AcceptanceCriteria, snap.NonGoals)
	if snap.Cut {
		b.WriteString("\n(The text was cut to the snapshot's bound; aimem's `get_task` has it in full.)\n")
	}
}

// frozenTask is the attempt's task snapshot: the local copy, or, when there
// is none, the board's text now, which becomes the copy. fresh says the
// board was read.
func (l *TurnLoop) frozenTask(ctx context.Context, a *memberAttempt) (snap taskSnapshot, fresh bool, err error) {
	if !attemptFileName(a.ID) {
		return snap, false, errors.New("the attempt's ID is not a file name")
	}
	path := taskSnapshotPath(l.Home, a.ID)
	if raw, rerr := os.ReadFile(path); rerr == nil && json.Unmarshal(raw, &snap) == nil && snap.Revision > 0 {
		return snap, false, nil
	}
	doc, err := l.Host.ReadTask(ctx, a.Task.TaskID)
	if err != nil {
		return snap, false, err
	}
	snap = taskSnapshot{Task: a.Task, Revision: doc.Revision, ReadAt: l.Now()}
	field := func(name string) string {
		var s string
		_ = json.Unmarshal(doc.Fields[name], &s)
		return redactBrief(l.Home, s, maxBriefTaskBytes)
	}
	snap.Title, snap.Objective = field("title"), field("objective")
	snap.AcceptanceCriteria, snap.NonGoals = field("acceptance_criteria"), field("non_goals")
	// The three texts share the bound, the later ones giving way to the
	// earlier.
	room := maxBriefTaskBytes - len(snap.Title)
	for _, p := range []*string{&snap.Objective, &snap.AcceptanceCriteria, &snap.NonGoals} {
		if len(*p) > room {
			*p, snap.Cut = cutText(*p, max(room, 0)), true
		}
		room -= len(*p)
	}
	raw, _ := json.MarshalIndent(snap, "", "  ")
	if err = l.attemptDir(a.ID); err == nil {
		err = writeAtomic(path, append(raw, '\n'))
	}
	if err != nil {
		l.Log.Warn("the task snapshot was not written: the next turn reads the board again", "error", err.Error())
	}
	return snap, true, nil
}

// briefTrigger is what started the turn: the pending messages with their
// content, or the continuation. It returns the messages it read.
//
// The read is the inbox's, so aicrewd counts each message delivered once
// more. A turn that then fails, is stopped, hits a limit or is interrupted
// never acts on them: they stay unacknowledged and reach the next turn, but
// their count says delivered. The turn record names them and marks such a
// turn unacted; that is a fact about the turn, not an audit of what the
// member did with each message, which the inbox shows by what is still
// unacknowledged.
func (l *TurnLoop) briefTrigger(ctx context.Context, b *strings.Builder, in briefInput) []inboxMessage {
	fmt.Fprintf(b, "\n## Why this turn\n\n%s.\n", in.trigger)
	if in.continuation {
		b.WriteString("No new message: go on with the attempt's work, and submit or block when it is done.\n")
		return nil
	}
	msgs, err := l.Host.Inbox(ctx, maxBriefMessages)
	if err != nil {
		fmt.Fprintf(b, "The messages could not be read for the brief (%s): run `aicrew-agent inbox`.\n",
			redactBrief(l.Home, err.Error(), maxBriefText))
		return nil
	}
	b.WriteString("\n")
	for _, m := range msgs {
		l.briefMessage(b, m)
	}
	if more := len(in.pending) - len(msgs); more > 0 {
		fmt.Fprintf(b, "\n%d more message(s) are pending: `aicrew-agent inbox`.\n", more)
	}
	return msgs
}

// briefMessage writes one message: its id to acknowledge, its kind, sender,
// attempt and task, its text, and the content its kind carries.
func (l *TurnLoop) briefMessage(b *strings.Builder, m inboxMessage) {
	fmt.Fprintf(b, "- Message %s (seq %d, %s", m.ID, m.Seq, m.Kind)
	if m.SenderAgentID != "" {
		fmt.Fprintf(b, ", from %s", m.SenderAgentID)
	}
	if m.AttemptID != "" {
		fmt.Fprintf(b, ", attempt %s", m.AttemptID)
	}
	if m.Task != nil {
		fmt.Fprintf(b, ", task %s", m.Task.TaskID)
	}
	if m.Deliveries > 1 {
		fmt.Fprintf(b, ", delivered %d times", m.Deliveries)
	}
	b.WriteString(")")
	if t := strings.TrimSpace(m.Text); t != "" {
		b.WriteString(": " + redactBrief(l.Home, t, maxBriefText))
	}
	b.WriteString("\n")
	if o := m.Operator; o != nil {
		fmt.Fprintf(b, "  - The operator's note (%s): %s\n", o.By, redactBrief(l.Home, o.Note, maxBriefText))
	}
	if e := m.Escalation; e != nil {
		fmt.Fprintf(b, "  - Escalation %s (%s) answered. Question: %s Decision: %s Rationale: %s\n", e.ID, e.Category,
			redactBrief(l.Home, e.Question, maxBriefText), redactBrief(l.Home, e.Decision, maxBriefText),
			redactBrief(l.Home, e.Rationale, maxBriefText))
	}
	if bd := m.Board; bd != nil {
		fmt.Fprintf(b, "  - Board: %s to %s, revision %d.\n", bd.From, bd.To, bd.Revision)
	}
	if m.Forge != nil {
		b.WriteString("  - " + pullLine(m.Forge) + "\n")
	}
	if m.Action != nil {
		for _, s := range m.Action.Subjects {
			l.briefSubject(b, s)
		}
	}
}

// briefSubject writes one subject of an action.needed message.
func (l *TurnLoop) briefSubject(b *strings.Builder, s actionSubject) {
	fmt.Fprintf(b, "  - Subject %s", s.Kind)
	if s.AttemptID != "" {
		fmt.Fprintf(b, ", attempt %s", s.AttemptID)
	}
	if s.Task != nil {
		fmt.Fprintf(b, ", task %s", s.Task.TaskID)
	}
	if len(s.Needs) > 0 {
		fmt.Fprintf(b, "; needs: %s", strings.Join(s.Needs, ", "))
	}
	b.WriteString(".\n")
	if s.Pull != nil {
		b.WriteString("    - " + pullLine(s.Pull) + "\n")
	}
	if r := s.Review; r != nil {
		b.WriteString("    - " + l.reviewLine(r.Author, r.URL, r.Head, r.AtHead, r.Verdict, r.Blocker, r.Body) + "\n")
	}
	for _, c := range s.Failed {
		fmt.Fprintf(b, "    - Failed check %s (%s) %s", c.Name, orUnknown(c.Conclusion), c.URL)
		switch {
		case c.Excerpt != "":
			b.WriteString(":\n      " + indented(redactBrief(l.Home, c.Excerpt, maxBriefText)))
		case c.NoLog != "":
			b.WriteString(": no log (" + c.NoLog + ")")
		}
		b.WriteString("\n")
	}
	if len(s.Passed) > 0 {
		names := make([]string, 0, len(s.Passed))
		for _, c := range s.Passed {
			names = append(names, c.Name)
		}
		fmt.Fprintf(b, "    - Passed checks: %s.\n", strings.Join(names, ", "))
	}
	if bl := s.Blocked; bl != nil {
		fmt.Fprintf(b, "    - Blocked by worker %s: %s\n", bl.Worker, redactBrief(l.Home, bl.Reason, maxBriefText))
	}
	if c := s.Conflict; c != nil {
		fmt.Fprintf(b, "    - Conflict with base %s in: %s.\n", c.Base, strings.Join(c.Files, ", "))
	}
	if p := s.PrePush; p != nil {
		fmt.Fprintf(b, "    - Pre-push check failed (exit %d): %s\n      %s\n", p.Exit, p.Command,
			indented(redactBrief(l.Home, p.Output, maxBriefText)))
	}
	if bd := s.Board; bd != nil {
		fmt.Fprintf(b, "    - Board: %s to %s, revision %d.\n", bd.From, bd.To, bd.Revision)
	}
}

// briefPulls is the pull requests as the forge watch last observed them.
func (l *TurnLoop) briefPulls(b *strings.Builder) {
	b.WriteString("\n## Pull requests\n\n")
	obs := l.Host.Observed()
	if len(obs) == 0 {
		b.WriteString("None observed yet by the launcher's forge watch.\n")
		return
	}
	for _, o := range obs {
		b.WriteString("- " + pullLine(&briefPull{Pull: o.Number, URL: o.URL, State: o.State, HeadCommit: o.HeadCommit,
			Labels: o.Labels, Checks: o.Checks, Comments: o.Comments}) + "\n")
		if r := o.Review; r != nil {
			b.WriteString("  - " + l.reviewLine(r.Author, r.URL, r.Head, r.AtHead, r.Verdict, r.Blocker, "") + "\n")
		}
		for _, f := range o.Failed {
			fmt.Fprintf(b, "  - Failed check %s (%s) %s\n", f.Name, orUnknown(f.Conclusion), f.URL)
		}
		if p := o.PrePush; p != nil {
			fmt.Fprintf(b, "  - Pre-push check failed at this head (exit %d): %s\n", p.Exit, p.Command)
		}
	}
}

// briefMemory is a worker's handoff and ledger within room bytes: the
// CURRENT ATTEMPT section, at most maxHandoffSection, and the attempt's
// ledger. When they do not fit, the ledger's oldest lines go first, then
// the section is cut; each cut is logged.
func (l *TurnLoop) briefMemory(in briefInput, room int, redact func(string) string) string {
	if in.handoff == nil || in.attempt == nil || !in.handoff.HasSection {
		return ""
	}
	section := redact(in.handoff.Section)
	if len(section) > maxHandoffSection {
		section = cutText(section, maxHandoffSection)
	}
	var lines []string
	for _, e := range readLedger(l.Home, in.attempt.ID) {
		lines = append(lines, redact(ledgerLine(e))+"\n")
	}
	const sectionHead = "\n## The handoff\n\nThe CURRENT ATTEMPT section of `docs/HANDOFF.md` as this turn starts: " +
		"continue from its Next, and keep its four parts current before the turn ends.\n\n"
	ledgerHead := fmt.Sprintf("\n\n## The ledger\n\nThe attempt's earlier turns, oldest first (`%s`):\n\n",
		filepath.ToSlash(strings.Replace(LedgerFile, "*", in.attempt.ID, 1)))
	const ledgerCut = "(Older turns were left out at the brief's bound; the ledger has them.)\n"
	size := func() int {
		n := len(sectionHead) + len(section) + len(ledgerHead) + len(ledgerCut)
		for _, ln := range lines {
			n += len(ln)
		}
		return n
	}
	dropped := 0
	for len(lines) > 0 && size() > room {
		lines, dropped = lines[1:], dropped+1
	}
	if dropped > 0 {
		l.Log.Warn("the brief left out the ledger's oldest turns at its bound", "lines", dropped)
	}
	if over := size() - room; over > 0 {
		l.Log.Warn("the brief cut the handoff's CURRENT ATTEMPT section at its bound", "bytes", len(section),
			"cut_to", max(len(section)-over, 0))
		section = cutText(section, max(len(section)-over, 0))
	}
	var b strings.Builder
	b.WriteString(sectionHead + section + ledgerHead)
	switch {
	case dropped > 0:
		b.WriteString(ledgerCut)
	case len(lines) == 0:
		b.WriteString("None yet.\n")
	}
	for _, ln := range lines {
		b.WriteString(ln)
	}
	return b.String()
}

func pullLine(p *briefPull) string {
	s := fmt.Sprintf("Pull request #%d %s: %s, head %s, labels [%s], checks %s, %d comment(s)", p.Pull, p.URL,
		orUnknown(p.State), orUnknown(p.HeadCommit), strings.Join(p.Labels, ", "), orUnknown(p.Checks), p.Comments)
	if len(p.Changes) > 0 {
		s += "; changed: " + strings.Join(p.Changes, ", ")
	}
	return s + "."
}

func (l *TurnLoop) reviewLine(author, url, head string, atHead bool, verdict string, blocker bool, body string) string {
	where := "at an older head " + head
	if atHead {
		where = "at the head"
	}
	s := fmt.Sprintf("Review by %s %s (%s): verdict %s", author, where, url, orUnknown(verdict))
	if blocker {
		s += ", a blocker"
	}
	if body != "" {
		s += ":\n      " + indented(redactBrief(l.Home, body, maxBriefText))
	}
	return s
}

// indented continues a multi-line text under a list item.
func indented(s string) string { return strings.ReplaceAll(s, "\n", "\n      ") }

// redactBrief is s without the home's forge tokens and the known token
// shapes, at most n bytes (its head).
func redactBrief(home, s string, n int) string {
	return cutText(tokenShapes.ReplaceAllString(RedactForgeTokens(home, s), "[redacted]"), n)
}

// cutText is s cut to at most n bytes on a rune boundary, ending in an
// ellipsis when it was cut; "" when n cannot hold the ellipsis.
func cutText(s string, n int) string {
	if len(s) <= n {
		return s
	}
	const ellipsis = "…"
	if n < len(ellipsis) {
		return ""
	}
	return strings.TrimRight(strings.ToValidUTF8(s[:n-len(ellipsis)], ""), " \t\n") + ellipsis
}
