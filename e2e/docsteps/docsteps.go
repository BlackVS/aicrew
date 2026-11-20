// Package docsteps reads the command lines of docs/BRING-UP.md that the
// scenarios run (task 01a1f9f5-a83e). The document is the source: a runnable
// line carries a marker with a stable step id, and the pilot simulation
// runs the line it reads here, not a copy of it.
//
// The markers are HTML comments, which a rendered page does not show:
//
//   - `<!-- run: ID [ID...] [per=WORD] -->` on the line right before a fenced
//     block names the block's command lines in order, one id each (a line
//     continued with a trailing backslash is one line);
//   - “ `LINE` <!-- run: ID [per=WORD] --> “ marks an inline code span;
//   - “ `FLAGS` <!-- run-add: ID [ID...] --> “ marks flags the document
//     names as an addition to those steps (a private CA, no terminal);
//   - “ `FLAGS` <!-- run-alt: ID [ID...] --> “ marks the document's
//     alternative for flags the steps already have: each flag it names
//     replaces the step's flag of that name, with its value.
//
// per=WORD names the step's example value that the document says to vary per
// member ("another --label for the coordinator"): Line replaces it as a word.
// A placeholder is a word in capitals the document uses for a value
// (PROJECT, HOME, HUB_URL); Line fills every one or refuses the step.
package docsteps

import (
	"bufio"
	"bytes"
	"fmt"
	"os"
	"regexp"
	"sort"
	"strings"
)

// Step is one marked command line.
type Step struct {
	ID   string
	Line string // the command line, continuations joined
	Per  string // the example value varied per member, if any
	At   int    // the document's line number
	adds []string
	alts []string
}

// Command is a command line of aicrew, aicrew-agent or aimem in the
// document, marked or not.
type Command struct {
	Line string
	At   int
	ID   string // empty when unmarked
}

// Doc is the document's steps and command lines.
type Doc struct {
	Steps    map[string]*Step
	Commands []Command
}

var (
	runMarker  = regexp.MustCompile(`^<!-- run: ([^>]*?) -->$`)
	inlineMark = regexp.MustCompile("`([^`]+)` <!-- (run|run-add|run-alt): ([^>]*?) -->")
	spanRe     = regexp.MustCompile("`([^`]+)`")
	commandRe  = regexp.MustCompile(`^(aicrew|aicrew-agent|aimem) `)
	placeRe    = regexp.MustCompile(`[A-Z][A-Z0-9_]+`)
)

// Load reads the document at path.
func Load(path string) (*Doc, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return Parse(raw)
}

// Parse reads the document's markers and command lines.
func Parse(raw []byte) (*Doc, error) {
	d := &Doc{Steps: map[string]*Step{}}
	var mark *pending
	var fence bool
	var block []string
	var blockAt []int
	var cont string
	var contAt int
	var addAlt []struct {
		kind, text string
		ids        []string
		at         int
	}
	sc := bufio.NewScanner(bytes.NewReader(raw))
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	n := 0
	for sc.Scan() {
		n++
		line := sc.Text()
		trim := strings.TrimSpace(line)
		if strings.HasPrefix(trim, "```") {
			if !fence {
				fence, block, blockAt, cont = true, nil, nil, ""
				continue
			}
			fence = false
			if cont != "" {
				return nil, fmt.Errorf("line %d: a continued line ends the block", n)
			}
			if err := d.closeBlock(block, blockAt, mark); err != nil {
				return nil, err
			}
			mark = nil
			continue
		}
		if fence {
			if cont == "" {
				contAt = n
			}
			if strings.HasSuffix(trim, `\`) {
				cont += strings.TrimSpace(strings.TrimSuffix(trim, `\`)) + " "
				continue
			}
			full := strings.TrimSpace(cont + trim)
			cont = ""
			if full == "" || strings.HasPrefix(full, "#") {
				continue
			}
			block, blockAt = append(block, full), append(blockAt, contAt)
			continue
		}
		if mark != nil && trim != "" {
			return nil, fmt.Errorf("line %d: the run marker of line %d is not right before a fenced block", n, mark.at)
		}
		if m := runMarker.FindStringSubmatch(trim); m != nil {
			ids, per := fields(m[1])
			mark = &pending{ids: ids, per: per, at: n}
			continue
		}
		marked := map[int]bool{}
		for _, m := range inlineMark.FindAllStringSubmatchIndex(line, -1) {
			text, kind := line[m[2]:m[3]], line[m[4]:m[5]]
			ids, per := fields(line[m[6]:m[7]])
			marked[m[2]] = true
			switch kind {
			case "run":
				if len(ids) != 1 {
					return nil, fmt.Errorf("line %d: an inline run marker names one id", n)
				}
				if err := d.add(&Step{ID: ids[0], Line: text, Per: per, At: n}); err != nil {
					return nil, err
				}
				d.Commands = append(d.Commands, Command{Line: text, At: n, ID: ids[0]})
			default:
				addAlt = append(addAlt, struct {
					kind, text string
					ids        []string
					at         int
				}{kind, text, ids, n})
			}
		}
		for _, m := range spanRe.FindAllStringSubmatchIndex(line, -1) {
			if marked[m[2]] {
				continue
			}
			if text := line[m[2]:m[3]]; isCommandLine(text) {
				d.Commands = append(d.Commands, Command{Line: text, At: n})
			}
		}
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	if fence {
		return nil, fmt.Errorf("the document ends inside a fenced block")
	}
	if mark != nil {
		return nil, fmt.Errorf("line %d: a run marker without its block", mark.at)
	}
	// A command line repeated as written elsewhere in the document is the
	// marked step's line: the marker is needed once.
	for i, c := range d.Commands {
		if c.ID != "" {
			continue
		}
		for id, s := range d.Steps {
			if s.Line == c.Line {
				d.Commands[i].ID = id
			}
		}
	}
	for _, a := range addAlt {
		for _, id := range a.ids {
			s := d.Steps[id]
			if s == nil {
				return nil, fmt.Errorf("line %d: %s names step %s, which is not marked", a.at, a.kind, id)
			}
			if a.kind == "run-add" {
				s.adds = append(s.adds, a.text)
			} else {
				s.alts = append(s.alts, a.text)
			}
		}
	}
	return d, nil
}

// closeBlock records a fenced block's command lines, under its marker's ids.
func (d *Doc) closeBlock(lines []string, at []int, mark *pending) error {
	if mark == nil {
		for i, l := range lines {
			if commandRe.MatchString(l) {
				d.Commands = append(d.Commands, Command{Line: l, At: at[i]})
			}
		}
		return nil
	}
	if len(mark.ids) != len(lines) {
		return fmt.Errorf("line %d: the run marker names %d ids for a block of %d lines", mark.at, len(mark.ids), len(lines))
	}
	for i, l := range lines {
		if err := d.add(&Step{ID: mark.ids[i], Line: l, Per: mark.per, At: at[i]}); err != nil {
			return err
		}
		if commandRe.MatchString(l) {
			d.Commands = append(d.Commands, Command{Line: l, At: at[i], ID: mark.ids[i]})
		}
	}
	return nil
}

// pending is a run marker waiting for its fenced block.
type pending struct {
	ids []string
	per string
	at  int
}

func (d *Doc) add(s *Step) error {
	if prev := d.Steps[s.ID]; prev != nil {
		return fmt.Errorf("line %d: step %s is already marked at line %d", s.At, s.ID, prev.At)
	}
	d.Steps[s.ID] = s
	return nil
}

// fields splits a marker's words into its ids and its per= value.
func fields(s string) (ids []string, per string) {
	for _, f := range strings.Fields(s) {
		if v, ok := strings.CutPrefix(f, "per="); ok {
			per = v
			continue
		}
		ids = append(ids, f)
	}
	return ids, per
}

// isCommandLine says whether an inline span is a command line, not a
// command's name: it starts with a command and has a flag or a placeholder.
func isCommandLine(s string) bool {
	if !commandRe.MatchString(s) {
		return false
	}
	for _, w := range strings.Fields(s)[1:] {
		if strings.HasPrefix(w, "-") || placeRe.MatchString(w) {
			return true
		}
	}
	return false
}

// Options are what a scenario gives a step's line.
type Options struct {
	Values map[string]string // a value for each placeholder
	Per    string            // the member's value for the step's per= word
	Adds   []string          // additions, each the text of a run-add marker of the step
	Alt    bool              // the step's run-alt flags in place of its own
}

// Line is step id's command line as a scenario runs it.
func (d *Doc) Line(id string, o Options) (string, error) {
	s := d.Steps[id]
	if s == nil {
		return "", fmt.Errorf("step %s is not marked in the document", id)
	}
	words := Split(s.Line)
	if o.Alt {
		if len(s.alts) == 0 {
			return "", fmt.Errorf("step %s has no run-alt marker", id)
		}
		for _, alt := range s.alts {
			words = replaceFlags(words, Split(alt))
		}
	}
	for _, a := range o.Adds {
		found := false
		for _, have := range s.adds {
			found = found || have == a
		}
		if !found {
			return "", fmt.Errorf("step %s: %q is not marked as its addition", id, a)
		}
		words = append(words, Split(a)...)
	}
	if o.Per != "" {
		if s.Per == "" {
			return "", fmt.Errorf("step %s names no per= word", id)
		}
		re := regexp.MustCompile(`\b` + regexp.QuoteMeta(s.Per) + `\b`)
		for i, w := range words {
			words[i] = re.ReplaceAllString(w, o.Per)
		}
	}
	var missing []string
	for i, w := range words {
		words[i] = fill(w, func(p string) string {
			v, ok := o.Values[p]
			if !ok {
				missing = append(missing, p)
				return p
			}
			return v
		})
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		return "", fmt.Errorf("step %s: no value for %s", id, strings.Join(unique(missing), ", "))
	}
	return Join(words), nil
}

// Placeholders are the capitals words of step id's line, its additions and
// alternatives included.
func (d *Doc) Placeholders(id string) []string {
	s := d.Steps[id]
	if s == nil {
		return nil
	}
	var out []string
	for _, t := range append(append([]string{s.Line}, s.adds...), s.alts...) {
		for _, w := range Split(t) {
			fill(w, func(p string) string { out = append(out, p); return p })
		}
	}
	sort.Strings(out)
	return unique(out)
}

// fill replaces each placeholder of w with f's value. A word in capitals
// after `$` is a shell variable ($USER), not a placeholder.
func fill(w string, f func(string) string) string {
	var b strings.Builder
	last := 0
	for _, m := range placeRe.FindAllStringIndex(w, -1) {
		if m[0] > 0 && (w[m[0]-1] == '$' || isWord(w[m[0]-1])) || m[1] < len(w) && isWord(w[m[1]]) {
			continue
		}
		b.WriteString(w[last:m[0]])
		b.WriteString(f(w[m[0]:m[1]]))
		last = m[1]
	}
	b.WriteString(w[last:])
	return b.String()
}

func isWord(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_'
}

// replaceFlags puts each flag of alt, with its value, in place of the
// line's flag of that name.
func replaceFlags(line, alt []string) []string {
	for i := 0; i+1 < len(alt); i += 2 {
		for j := 0; j+1 < len(line); j++ {
			if line[j] == alt[i] {
				line[j+1] = alt[i+1]
			}
		}
	}
	return line
}

func unique(s []string) []string {
	var out []string
	for i, v := range s {
		if i == 0 || v != s[i-1] {
			out = append(out, v)
		}
	}
	return out
}

// Split splits a command line into its words as sh does for the quoting
// the document uses: single and double quotes, no expansion.
func Split(s string) []string {
	var out []string
	var cur strings.Builder
	in, quote := false, rune(0)
	for _, r := range s {
		switch {
		case quote != 0 && r == quote:
			quote = 0
		case quote != 0:
			cur.WriteRune(r)
		case r == '\'' || r == '"':
			quote, in = r, true
		case r == ' ' || r == '\t':
			if in {
				out = append(out, cur.String())
				cur.Reset()
				in = false
			}
		default:
			cur.WriteRune(r)
			in = true
		}
	}
	if in {
		out = append(out, cur.String())
	}
	return out
}

// Join quotes the words for sh, in single quotes where a word needs any.
func Join(words []string) string {
	out := make([]string, len(words))
	for i, w := range words {
		if w != "" && strings.Trim(w, "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789-_./:=@,+%") == "" {
			out[i] = w
			continue
		}
		out[i] = "'" + strings.ReplaceAll(w, "'", `'\''`) + "'"
	}
	return strings.Join(out, " ")
}
