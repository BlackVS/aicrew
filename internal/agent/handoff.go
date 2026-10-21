package agent

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"
)

// The member's session doc (docs/DESIGN-TURNS.md, section 3; increment
// 3c). A worker's docs/HANDOFF.md is the memory a fresh turn continues
// from. The agent owns the file; the launcher owns two of its sections:
//
//   - CURRENT ATTEMPT, which the launcher opens at the attempt's first turn
//     and the model keeps current (Done, Decisions, Next, Open questions);
//     the brief carries it, and a turn that leaves it unchanged is marked
//     `handoff not updated` in the ledger: two such turns in a row stall the
//     attempt.
//   - Archive, where the launcher moves the section of an attempt that is no
//     longer open, newest first, trimmed to its bound.
//
// Everything before the two sections is the agent's header. The home is a
// cache: after a turn the launcher pushes the doc to the attempt on aicrewd,
// redacted and bounded, and a home without the file takes the attempt's.
// The ledger, one line per turn of the attempt, is launcher data in
// state/attempts/<attempt>/ledger.jsonl.

const (
	// HandoffFile is the session doc, relative to the home.
	HandoffFile = "docs/HANDOFF.md"
	// maxHandoffSection is what the brief carries of the CURRENT ATTEMPT
	// section; a longer section is reported.
	maxHandoffSection = 8 << 10
	// maxArchiveAttempts and maxArchiveBytes bound the archive.
	maxArchiveAttempts = 10
	maxArchiveBytes    = 16 << 10
	// maxHandoffPush is aicrewd's bound on the doc (store.MaxHandoffBytes),
	// and maxHandoffEncoded what its JSON may take within the server's
	// 64 KiB body bound.
	maxHandoffPush    = 32 << 10
	maxHandoffEncoded = 60 << 10
	// maxLedgerLines bounds an attempt's ledger, and maxLedgerSummary each
	// line's summary.
	maxLedgerLines   = 50
	maxLedgerSummary = 300
	// keepAttemptDirs is how many attempts' directories under
	// state/attempts/ the launcher keeps, the open attempt's among them.
	keepAttemptDirs = 20

	sectionHeading = "## CURRENT ATTEMPT"
	archiveHeading = "## Archive"
)

// LedgerFile is an attempt's ledger, relative to the home, with the
// attempt's ID in place of the asterisk.
const LedgerFile = "state/attempts/*/ledger.jsonl"

func ledgerPath(home, attemptID string) string {
	return filepath.Join(home, filepath.FromSlash(strings.Replace(LedgerFile, "*", attemptID, 1)))
}

func handoffPath(home string) string { return filepath.Join(home, filepath.FromSlash(HandoffFile)) }

// sectionMark is the line the launcher opens the section with, which names
// its attempt: found wherever the model left it in the section.
var sectionMark = regexp.MustCompile("(?m)^Attempt `([^`]+)`.*(?:\n|$)")

// handoffDoc is the session doc split into the agent's header, the
// CURRENT ATTEMPT section's body and the archive's entries, newest first.
// Section is "" when the doc has no such section.
type handoffDoc struct {
	Header  string
	Section string
	Archive []string
	// HasSection and HasArchive say the headings were there.
	HasSection, HasArchive bool
}

// parseHandoff splits s at the two launcher headings, wherever they are.
func parseHandoff(s string) handoffDoc {
	var d handoffDoc
	lines := strings.SplitAfter(s, "\n")
	part := &d.Header
	var archive strings.Builder
	for _, ln := range lines {
		switch strings.TrimRight(ln, " \t\r\n") {
		case sectionHeading:
			d.HasSection, part = true, &d.Section
			continue
		case archiveHeading:
			d.HasArchive = true
			part = nil
			continue
		}
		if part == nil {
			archive.WriteString(ln)
		} else {
			*part += ln
		}
	}
	d.Header = strings.TrimRight(d.Header, "\n") + "\n"
	d.Section = strings.Trim(d.Section, "\n")
	d.Archive = splitArchive(archive.String())
	return d
}

// splitArchive is the archive's entries, each from its "### Attempt"
// heading; text before the first entry is the launcher's own line.
func splitArchive(s string) []string {
	var out []string
	for i, e := range strings.Split("\n"+s, "\n### Attempt ") {
		if i == 0 {
			continue
		}
		out = append(out, "### Attempt "+strings.Trim(e, "\n"))
	}
	return out
}

// render is the doc in the launcher's order: the header, the section, the
// archive.
func (d handoffDoc) render() string {
	var b strings.Builder
	b.WriteString(strings.TrimRight(d.Header, "\n") + "\n")
	if d.HasSection {
		b.WriteString("\n" + sectionHeading + "\n\n" + d.Section + "\n")
	}
	if len(d.Archive) > 0 {
		fmt.Fprintf(&b, "\n%s\n\nClosed attempts, newest first: the launcher keeps the last %d, at most %d KiB.\n",
			archiveHeading, maxArchiveAttempts, maxArchiveBytes>>10)
		for _, e := range d.Archive {
			b.WriteString("\n" + e + "\n")
		}
	}
	return b.String()
}

// sectionAttempt is the attempt the section names, or "".
func (d handoffDoc) sectionAttempt() string {
	if m := sectionMark.FindStringSubmatch(d.Section); m != nil {
		return m[1]
	}
	return ""
}

// newSection is the CURRENT ATTEMPT section the launcher opens for an
// attempt.
func newSection(a *memberAttempt) string {
	return sectionMarkLine(a) + "\n\n### Done\n\n### Decisions\n\n### Next\n\n### Open questions"
}

// sectionMarkLine is the section's first line, which names its attempt.
func sectionMarkLine(a *memberAttempt) string {
	return fmt.Sprintf("Attempt `%s`, task `%s`. The launcher keeps this section's place and moves it to the "+
		"Archive when the attempt is no longer open. Keep its four parts current before every turn ends: the next "+
		"turn starts from them, and two turns in a row that leave them unchanged stall the attempt.", a.ID, a.Task.TaskID)
}

// archive moves the section into the archive as its newest entry, its
// headings one level down, and trims the archive to its bounds.
// id is the section's attempt, as the launcher recorded it.
func (d *handoffDoc) archive(now time.Time, id string) {
	if d.HasSection && strings.TrimSpace(d.Section) != "" {
		if id == "" {
			id = "(unnamed)"
		}
		body := d.Section
		if loc := sectionMark.FindStringIndex(body); loc != nil {
			body = body[:loc[0]] + body[loc[1]:]
		}
		body = strings.ReplaceAll("\n"+strings.Trim(body, "\n"), "\n### ", "\n#### ")
		entry := fmt.Sprintf("### Attempt %s (archived %s)\n%s", id, now.UTC().Format(time.RFC3339), body)
		d.Archive = append([]string{strings.TrimRight(entry, "\n")}, d.Archive...)
	}
	d.Section, d.HasSection = "", false
	d.trimArchive()
}

func (d *handoffDoc) trimArchive() {
	if len(d.Archive) > maxArchiveAttempts {
		d.Archive = d.Archive[:maxArchiveAttempts]
	}
	size := func() int {
		n := 0
		for _, e := range d.Archive {
			n += len(e) + 2
		}
		return n
	}
	for len(d.Archive) > 1 && size() > maxArchiveBytes {
		d.Archive = d.Archive[:len(d.Archive)-1]
	}
	if len(d.Archive) == 1 && size() > maxArchiveBytes {
		d.Archive[0] = cutText(d.Archive[0], maxArchiveBytes-2)
	}
}

// sectionHash is the CURRENT ATTEMPT section's fingerprint, or "" when
// the doc has none.
func sectionHash(d handoffDoc) string {
	if !d.HasSection {
		return ""
	}
	h := sha256.Sum256([]byte(d.Section))
	return hex.EncodeToString(h[:])
}

func docHash(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])
}

// prepareHandoff readies a worker's session doc before a turn. The
// section's attempt is the one the launcher recorded in the turn state
// (st.Handoff), or, in a home without that record, the one its first line
// names. A section of an attempt that is no longer open goes to the
// archive; on the open attempt a section that lost its first line gets it
// back. An open attempt without a section takes the attempt's from
// aicrewd, which only a home that held the section can have pushed: the
// whole doc over a missing file or join's untouched placeholder, else the
// section under the home's own header and archive. Without one there, the
// launcher opens a new section. It returns the doc as it now stands, and
// whether it could be read; when aicrewd cannot be read the turn runs
// without the doc, since a new section's push would replace the attempt's
// copy.
func (l *TurnLoop) prepareHandoff(ctx context.Context, st *turnState, attempt *memberAttempt) (handoffDoc, bool) {
	path := handoffPath(l.Home)
	local, err := os.ReadFile(path) // nil when there is no file
	switch {
	case errors.Is(err, os.ErrNotExist):
		local = nil
	case err != nil:
		l.Log.Warn("the handoff was not read", "error", err.Error())
		return handoffDoc{}, false
	}
	untouched := local == nil || string(local) == handoffMD
	d := parseHandoff(handoffMD)
	if local != nil {
		d = parseHandoff(string(local))
	}
	owner := st.Handoff
	if owner == "" {
		owner = d.sectionAttempt()
	}
	if d.HasSection && (attempt == nil || owner != attempt.ID) {
		d.archive(l.Now(), owner)
	}
	if attempt != nil && !d.HasSection {
		body, herr := l.Host.ReadHandoff(ctx, attempt.ID)
		if herr != nil {
			l.Log.Warn("the handoff was not read from aicrewd: this turn runs without it", "error", herr.Error())
			return handoffDoc{}, false
		}
		switch hub := parseHandoff(body); {
		case !hub.HasSection:
			d.Section, d.HasSection = newSection(attempt), true
		case untouched:
			d = hub
			l.Log.Info("the home had no handoff: it was taken from the attempt on aicrewd", "attempt", attempt.ID)
		default:
			d.Section, d.HasSection = hub.Section, true
			// A home without its own archive keeps the attempt's, such as
			// one joined with an older placeholder than handoffMD.
			if len(d.Archive) == 0 {
				d.Archive = hub.Archive
			}
			l.Log.Info("the home's handoff had no section for the attempt: it was taken from aicrewd",
				"attempt", attempt.ID)
		}
	}
	if attempt != nil {
		if named := d.sectionAttempt(); named != attempt.ID {
			if named != "" {
				l.Log.Warn("the handoff's section names another attempt than the open one: the launcher's line is put first",
					"attempt", attempt.ID, "named", named)
			}
			d.Section = sectionMarkLine(attempt) + "\n\n" + d.Section
		}
		st.Handoff = attempt.ID
	} else {
		st.Handoff = ""
	}
	if after := d.render(); local == nil || !bytes.Equal(local, []byte(after)) {
		werr := os.MkdirAll(filepath.Dir(path), 0o700)
		if werr == nil {
			werr = writeAtomic(path, []byte(after))
		}
		if werr != nil {
			l.Log.Warn("the handoff was not written", "error", werr.Error())
		}
	}
	if len(d.Section) > maxHandoffSection {
		l.Log.Warn("the handoff's CURRENT ATTEMPT section is over its bound: the brief carries its start",
			"bytes", len(d.Section), "bound", maxHandoffSection)
	}
	return d, true
}

// handoffAfterTurn records a worker's turn of an attempt: the ledger line
// and, for an attempt that was open before the turn (opened), whether the
// turn changed the CURRENT ATTEMPT section and the push to aicrewd. A
// finished turn that left the section unchanged is marked `handoff not
// updated`, and counts towards the stall when it made no step; a finished
// turn that changed the section or made a step resets the count. A turn
// that did not finish counts neither way, and the turn that opened the
// attempt, which had no section to keep, is only recorded. aicrewd refuses
// the push once a stop is requested or the attempt is no longer running:
// the home keeps its copy, and the refusal is logged.
func (l *TurnLoop) handoffAfterTurn(ctx context.Context, attemptID string, a *attemptTurns, rec TurnRecord,
	sectionBefore, headBefore, headAfter string, opened bool) {
	d, _, ok := readHandoff(l.Home)
	finished := rec.Outcome == TurnFinished
	unchanged := opened && finished && (!ok || sectionHash(d) == sectionBefore)
	switch {
	case unchanged && rec.Steps == 0:
		a.Unchanged++
	case opened && finished:
		a.Unchanged = 0
	}
	l.appendLedger(attemptID, ledgerEntry{Turn: rec.ID, Start: rec.StartedAt, Outcome: rec.Outcome, CostUSD: rec.CostUSD,
		HeadBefore: headBefore, HeadAfter: headAfter, Steps: rec.Steps, Summary: rec.Summary, HandoffNotUpdated: unchanged})
	if unchanged {
		l.Log.Warn("handoff not updated: the turn left the CURRENT ATTEMPT section of docs/HANDOFF.md unchanged",
			"turn", rec.ID, "attempt", attemptID, "in_a_row", a.Unchanged)
	}
	if opened {
		l.pushHandoff(ctx, attemptID, a)
	}
}

// readHandoff is the doc as the file holds it now.
func readHandoff(home string) (handoffDoc, string, bool) {
	raw, err := os.ReadFile(handoffPath(home))
	if err != nil {
		return handoffDoc{}, "", false
	}
	return parseHandoff(string(raw)), string(raw), true
}

// pushHandoff sends the doc to the attempt on aicrewd when it changed
// since the last push, redacted and within aicrewd's bounds; a refused
// push leaves the local copy as it is.
func (l *TurnLoop) pushHandoff(ctx context.Context, attemptID string, a *attemptTurns) {
	_, raw, ok := readHandoff(l.Home)
	if !ok || strings.TrimSpace(raw) == "" {
		return
	}
	doc := fitHandoffPush(tokenShapes.ReplaceAllString(RedactForgeTokens(l.Home, raw), "[redacted]"))
	h := docHash(doc)
	if h == a.Pushed {
		return
	}
	if err := l.Host.PushHandoff(ctx, attemptID, doc); err != nil {
		l.Log.Warn("the handoff was not pushed to aicrewd: the home keeps it", "attempt", attemptID, "error", err.Error())
		return
	}
	a.Pushed = h
}

// fitHandoffPush is doc within aicrewd's bounds: the archive's oldest
// entries go first, then the doc is cut, with a notice.
func fitHandoffPush(doc string) string {
	fits := func(s string) bool {
		enc, _ := json.Marshal(s)
		return len(s) <= maxHandoffPush && len(enc) <= maxHandoffEncoded
	}
	if fits(doc) {
		return doc
	}
	d := parseHandoff(doc)
	for len(d.Archive) > 0 && !fits(d.render()) {
		d.Archive = d.Archive[:len(d.Archive)-1]
	}
	if s := d.render(); fits(s) {
		return s
	}
	const notice = "\n\n(Cut at aicrewd's bound; the home holds the whole doc.)\n"
	s := d.render()
	lo, hi := 0, len(s)
	for lo < hi {
		mid := (lo + hi + 1) / 2
		if fits(cutText(s, mid) + notice) {
			lo = mid
		} else {
			hi = mid - 1
		}
	}
	return cutText(s, lo) + notice
}

// ledgerEntry is one line of an attempt's ledger.
type ledgerEntry struct {
	Turn       string    `json:"turn"`
	Start      time.Time `json:"start"`
	Outcome    string    `json:"outcome"`
	CostUSD    float64   `json:"cost_usd"`
	HeadBefore string    `json:"head_before,omitempty"`
	HeadAfter  string    `json:"head_after,omitempty"`
	Steps      int       `json:"steps"`
	Summary    string    `json:"summary,omitempty"`
	// HandoffNotUpdated marks a turn that left the CURRENT ATTEMPT section
	// unchanged.
	HandoffNotUpdated bool `json:"handoff_not_updated,omitempty"`
}

// firstSentence is the start of s up to its first sentence's end, on one
// line and bounded.
func firstSentence(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	for i := 0; i+1 < len(s); i++ {
		if (s[i] == '.' || s[i] == '!' || s[i] == '?') && s[i+1] == ' ' {
			s = s[:i+1]
			break
		}
	}
	return cutText(s, maxLedgerSummary)
}

// appendLedger adds e to the attempt's ledger, keeping its last lines.
func (l *TurnLoop) appendLedger(attemptID string, e ledgerEntry) {
	if !attemptFileName(attemptID) {
		return
	}
	e.Summary = firstSentence(tokenShapes.ReplaceAllString(RedactForgeTokens(l.Home, e.Summary), "[redacted]"))
	lines := readLedger(l.Home, attemptID)
	lines = append(lines, e)
	if len(lines) > maxLedgerLines {
		lines = lines[len(lines)-maxLedgerLines:]
	}
	var b bytes.Buffer
	for _, x := range lines {
		raw, _ := json.Marshal(x)
		b.Write(append(raw, '\n'))
	}
	path := ledgerPath(l.Home, attemptID)
	err := l.attemptDir(attemptID)
	if err == nil {
		err = writeAtomic(path, b.Bytes())
	}
	if err != nil {
		l.Log.Warn("the ledger was not written", "attempt", attemptID, "error", err.Error())
	}
}

// readLedger is the attempt's ledger, oldest first; a line that does not
// decode is skipped.
func readLedger(home, attemptID string) []ledgerEntry {
	f, err := os.Open(ledgerPath(home, attemptID))
	if err != nil {
		return nil
	}
	defer f.Close()
	var out []ledgerEntry
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 4096), 64<<10)
	for sc.Scan() {
		var e ledgerEntry
		if json.Unmarshal(sc.Bytes(), &e) == nil {
			out = append(out, e)
		}
	}
	return out
}

// ledgerLine is a ledger entry as the brief carries it.
func ledgerLine(e ledgerEntry) string {
	var b strings.Builder
	fmt.Fprintf(&b, "- %s turn %s: %s, %d step(s), $%.2f", e.Start.UTC().Format(time.RFC3339), shortID(e.Turn),
		orUnknown(e.Outcome), e.Steps, e.CostUSD)
	switch {
	case e.HeadAfter != "" && e.HeadAfter != e.HeadBefore:
		fmt.Fprintf(&b, ", head %s to %s", shortID(orUnknown(e.HeadBefore)), shortID(e.HeadAfter))
	case e.HeadAfter != "":
		fmt.Fprintf(&b, ", head %s unchanged", shortID(e.HeadAfter))
	}
	if e.HandoffNotUpdated {
		b.WriteString("; handoff not updated")
	}
	if e.Summary != "" {
		b.WriteString(". " + e.Summary)
	}
	return b.String()
}

func shortID(s string) string {
	if len(s) > 12 {
		return s[:12]
	}
	return s
}

// attemptFileName says id can name a directory under state/attempts/.
func attemptFileName(id string) bool {
	return id != "" && id != "." && id != ".." && !strings.ContainsAny(id, `/\`)
}

// attemptDir makes the attempt's directory under state/attempts/. When it
// is new, the directories of the other attempts beyond the newest
// keepAttemptDirs are removed: launcher data of attempts no longer open
// (the open attempt is the one whose directory this makes).
func (l *TurnLoop) attemptDir(attemptID string) error {
	dir := filepath.Dir(ledgerPath(l.Home, attemptID))
	if _, err := os.Stat(dir); err == nil {
		return nil
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	l.pruneAttemptDirs(attemptID)
	return nil
}

// pruneAttemptDirs keeps keep's directory and the newest others, up to
// keepAttemptDirs in all, by modification time.
func (l *TurnLoop) pruneAttemptDirs(keep string) {
	root := filepath.Dir(filepath.Dir(ledgerPath(l.Home, keep)))
	entries, err := os.ReadDir(root)
	if err != nil {
		return
	}
	type dir struct {
		name string
		mod  time.Time
	}
	var others []dir
	for _, e := range entries {
		if !e.IsDir() || e.Name() == keep {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		others = append(others, dir{e.Name(), info.ModTime()})
	}
	slices.SortFunc(others, func(a, b dir) int { return b.mod.Compare(a.mod) })
	for _, d := range others[min(len(others), keepAttemptDirs-1):] {
		if err := os.RemoveAll(filepath.Join(root, d.name)); err != nil {
			l.Log.Warn("an old attempt's launcher data was not removed", "attempt", d.name, "error", err.Error())
		}
	}
}
