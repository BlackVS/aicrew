package agent

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// The lean start (docs/DESIGN-TURNS.md, section 3; increment 3d). A turn
// does not read START.md and ROLES.md in full: ROLES.md is written by
// section per turn kind, and the brief names the sections for the turn's
// kind. A coordinator's brief also carries the team's open attempts and its
// own docs/HANDOFF.md.

// The turn kinds.
const (
	kindCoordinator   = "coordinator"
	kindWorkerOffer   = "worker-offer"
	kindWorkerAttempt = "worker-attempt"
	kindWorkerReview  = "worker-review"
	kindIndependent   = "independent"
)

// The headings of ROLES.md's sections.
const (
	sectionEveryMember   = "Every member"
	sectionWriting       = "Writing that others read"
	sectionCoordinator   = "Coordinator"
	sectionWorkerOffer   = "Worker: an offer"
	sectionWorkerAttempt = "Worker: a running attempt"
	sectionWorkerReview  = "Worker: a review result"
	sectionIndependent   = "Independent"
)

// maxCoordinatorHandoff bounds what a coordinator's brief carries of its
// docs/HANDOFF.md, as maxHandoffSection bounds a worker's section, and
// maxBriefTeam its team's open attempts, which aicrewd lists without a
// limit.
const (
	maxCoordinatorHandoff = maxHandoffSection
	maxBriefTeam          = 8 << 10
)

// turnKindOf is the turn's kind: a coordinator's; a member's without a
// running attempt (an offer for a worker, a claim for an independent
// member); a turn of a running attempt on a review result, which is a
// review of its pull request in an action-needed subject or the
// coordinator's rework decision; or any other turn of a running attempt.
func turnKindOf(role string, attempt *memberAttempt, msgs []inboxMessage) string {
	switch {
	case role == "coordinator":
		return kindCoordinator
	case attempt == nil && role == "independent":
		return kindIndependent
	case attempt == nil:
		return kindWorkerOffer
	case attempt.Phase == "rework":
		return kindWorkerReview
	}
	for _, m := range msgs {
		if m.Action == nil {
			continue
		}
		for _, s := range m.Action.Subjects {
			if s.Review != nil && (s.AttemptID == "" || s.AttemptID == attempt.ID) {
				return kindWorkerReview
			}
		}
	}
	return kindWorkerAttempt
}

// kindSections are the ROLES.md sections a turn of kind reads, in the
// file's order; an independent member's own section comes with a worker's
// kinds while it has an attempt.
func kindSections(role, kind string) []string {
	out := []string{sectionEveryMember, sectionWriting}
	if role == "independent" {
		out = append(out, sectionIndependent)
	}
	switch kind {
	case kindCoordinator:
		out = append(out, sectionCoordinator)
	case kindWorkerOffer:
		out = append(out, sectionWorkerOffer)
	case kindWorkerAttempt:
		out = append(out, sectionWorkerAttempt)
	case kindWorkerReview:
		out = append(out, sectionWorkerAttempt, sectionWorkerReview)
	}
	return out
}

// briefFetch is the brief's last part, with the ROLES.md sections for the
// turn's kind.
func briefFetch(role, kind string) string {
	q := make([]string, 0, 4)
	for _, s := range kindSections(role, kind) {
		q = append(q, `"`+s+`"`)
	}
	return "\n## Where to fetch more\n\n" +
		"- `aicrew-agent inbox` (`--json` for every field): the messages in full; `aicrew-agent inbox --ack ID,ID` " +
		"acknowledges what you handled.\n" +
		"- `aicrew-agent session status`: your session and attempt as aicrewd holds them now.\n" +
		"- `aicrew-agent forge pr show --pr N`: a pull request with its comments and checks.\n" +
		"- aimem's `get_task`: the live task on the board.\n" +
		"- `docs/ROLES.md`, your role's rules: this is a " + kind + " turn, whose sections are " +
		strings.Join(q, ", ") + ". Read them when this brief does not settle what to do; never the whole file.\n"
}

// briefTeam is a coordinator's team state: the team's open attempts as
// aicrewd lists them now, at most maxBriefTeam bytes of them; a line
// counts the rest.
func (l *TurnLoop) briefTeam(ctx context.Context, b *strings.Builder) {
	b.WriteString("\n## The team\n\n")
	o, err := l.Host.OpenAttempts(ctx)
	switch {
	case err != nil:
		fmt.Fprintf(b, "The team's open attempts could not be read for the brief (%s): run `aicrew-agent attempts`.\n",
			redactBrief(l.Home, err.Error(), maxBriefText))
	case len(o.Attempts) == 0:
		b.WriteString("The team has no open attempts.\n")
	default:
		b.WriteString("The team's open attempts as aicrewd lists them now:\n\n")
		size := 0
		for i, a := range o.Attempts {
			line := redactBrief(l.Home, a.Line(), maxBriefText) + "\n"
			if size += len(line); size > maxBriefTeam {
				fmt.Fprintf(b, "\n%d more open attempt(s) were left out at the brief's bound: `aicrew-agent attempts` "+
					"lists them all.\n", len(o.Attempts)-i)
				return
			}
			b.WriteString(line)
		}
	}
}

// OpenAttempts reads the team's open attempts from aicrewd, as the
// launcher's session.
func (s *StepServer) OpenAttempts(ctx context.Context) (OpenAttempts, error) {
	return ReadOpenAttempts(ctx, s.local, s.driver.Session.stepToken())
}

// The coordinator's handoff in its brief: the heading, and the line that
// names how much of the file a cut left out.
const (
	coordinatorHandoffHead = "\n## The handoff\n\nYour `docs/HANDOFF.md` as this turn starts; keep it current before the turn ends.\n\n"
	coordinatorHandoffCut  = "\n\n(The last %d bytes of `docs/HANDOFF.md` were cut at the brief's bound; the file in the home " +
		"has them.)\n"
)

// coordinatorHandoff is what a coordinator's brief carries of its
// docs/HANDOFF.md within room bytes: the file's start, at most
// maxCoordinatorHandoff bytes once redacted, with a line naming what was
// cut. A coordinator has no attempt, so the launcher keeps no section of its
// file; the file in the home stays whole.
func (l *TurnLoop) coordinatorHandoff(redact func(string) string, room int) string {
	raw, err := os.ReadFile(filepath.Join(l.Home, filepath.FromSlash(HandoffFile)))
	if err != nil {
		if !os.IsNotExist(err) {
			l.Log.Warn("the handoff was not read for the brief", "error", err.Error())
		}
		return ""
	}
	doc := redact(string(raw))
	head, cutLine := coordinatorHandoffHead, coordinatorHandoffCut
	// The cut line's number takes at most 10 digits.
	bound := min(maxCoordinatorHandoff, room-len(head)-len(cutLine)-10)
	if len(doc) <= bound {
		return head + doc + "\n"
	}
	if bound <= 0 {
		l.Log.Warn("the brief had no room for the coordinator's handoff", "bytes", len(doc))
		return head + "(No room was left for `docs/HANDOFF.md` in this brief: read it in the home.)\n"
	}
	cut := cutText(doc, bound)
	l.Log.Warn("the brief cut the coordinator's handoff at its bound", "bytes", len(doc), "bound", bound)
	return head + cut + fmt.Sprintf(cutLine, len(doc)-len(strings.TrimSuffix(cut, "…")))
}

// reviewResultRule is the worker's section for a turn on a review result.
const reviewResultRule = "A turn on a review result: an `action.needed` message whose subject carries a review of " +
	"your pull request (its author, the head it read, its verdict, whether it names a BLOCKER, and its body), or " +
	"the coordinator's rework decision, which puts your attempt in the `rework` phase. A review binds to the head " +
	"it read: act on one at your pull request's current head, and read the full comment with " +
	"`aicrew-agent forge pr show --pr N` when the brief cut its body. As the project's process says, fix each " +
	"BLOCKER in the attempt's worktree; a FOLLOW_UP is not this attempt's work, so name it under Open questions " +
	"in your handoff and in the pull request's body. Record in the CURRENT ATTEMPT section's Decisions how you " +
	"settled each finding. Push, re-request the review as the pull-request rule under \"" + sectionEveryMember +
	"\" says, and end the turn. After a rework decision, `resume` the attempt with the `work` step first, and " +
	"`submit` again once the pull request is ready (the steps are under \"" + sectionWorkerAttempt + "\")."

// repetitionRule is DESIGN-TURNS section 11 (T9): a procedure re-derived
// becomes a proposal, never an automation written inside the turn.
const repetitionRule = "When you notice that you work out again a procedure you or the team worked out before " +
	"(your brief's ledger shows your earlier turns), do not automate it in your turn: the turn's job is its task. " +
	"Propose an \"automate X\" task with where it happened (the turns, attempts or tasks, by ID), how often, and " +
	"the level it belongs to and why: a skill (a way of acting that still needs judgement), a repository tool " +
	"(deterministic, for one project) or a harness feature (it recurs across projects). A worker writes the " +
	"proposal under Open questions in its handoff and in an \"Automate\" part of its pull request's body; the " +
	"coordinator posts it, its own as well, with aimem's `add_task_comment` on the task where it happened, " +
	"headed `[automate]`; the architect files the task. Then go on with the procedure by hand."
