package automation

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
)

// The allow list a script's run() is checked against: the turn's own
// (docs/DESIGN-TURNS.md, 9.1 and 10.3). A command outside it fails the
// script; the list is never widened for a script. The rules are Claude
// Code's permission rules for the Bash tool, as the managed turn settings
// write them: "Bash(PATTERN)", where "*" matches any text, and the older
// "Bash(PREFIX:*)", which matches the prefix alone or followed by a space
// and more. A deny rule wins over every allow rule. Rules for other tools
// are not about commands and are ignored.

// ErrRefused is a command the allow list does not admit.
var ErrRefused = errors.New("the command is outside the turn's allow list")

// AllowList is a turn's Bash allow and deny rules.
type AllowList struct {
	allow, deny []*regexp.Regexp
}

// NewAllowList compiles the Bash rules of allow and deny.
func NewAllowList(allow, deny []string) AllowList {
	var l AllowList
	for _, r := range allow {
		if re := bashRule(r); re != nil {
			l.allow = append(l.allow, re)
		}
	}
	for _, r := range deny {
		if re := bashRule(r); re != nil {
			l.deny = append(l.deny, re)
		}
	}
	return l
}

// commandName is a command a script may name: a bare program name, never a
// path or a word with a space, so the rule it matches is the one a reader
// sees.
var commandName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._+-]{0,63}$`)

// Admit returns nil when the list admits cmd with args, and ErrRefused
// naming the command line otherwise.
func (l AllowList) Admit(cmd string, args []string) error {
	if !commandName.MatchString(cmd) {
		return fmt.Errorf("%w: %q is not a bare command name", ErrRefused, cmd)
	}
	line := CommandLine(cmd, args)
	for _, re := range l.deny {
		if re.MatchString(line) {
			return fmt.Errorf("%w: %s is denied", ErrRefused, line)
		}
	}
	for _, re := range l.allow {
		if re.MatchString(line) {
			return nil
		}
	}
	return fmt.Errorf("%w: %s", ErrRefused, line)
}

// CommandLine is cmd with args as a shell would show it: an argument with a
// space, a quote or a shell character is single-quoted. The command runs
// with no shell; the line is what the rules are matched against.
func CommandLine(cmd string, args []string) string {
	parts := []string{cmd}
	for _, a := range args {
		parts = append(parts, quote(a))
	}
	return strings.Join(parts, " ")
}

func quote(a string) string {
	if a != "" && !strings.ContainsAny(a, " \t\r\n'\"\\$`;&|<>(){}*?[]#~") {
		return a
	}
	return "'" + strings.ReplaceAll(a, "'", `'\''`) + "'"
}

// bashRule compiles "Bash(PATTERN)" into a matcher of a whole command line,
// or nil for another tool's rule.
func bashRule(rule string) *regexp.Regexp {
	p, ok := strings.CutPrefix(rule, "Bash(")
	if !ok || !strings.HasSuffix(p, ")") {
		return nil
	}
	p = strings.TrimSuffix(p, ")")
	if prefix, ok := strings.CutSuffix(p, ":*"); ok {
		// (?s): an argument may span lines, as a commit message does, and
		// the rule must still match it (hands review of #166).
		return regexp.MustCompile(`(?s)^` + regexp.QuoteMeta(prefix) + `(\s.*)?$`)
	}
	var b strings.Builder
	b.WriteString(`(?s)^`)
	for i, part := range strings.Split(p, "*") {
		if i > 0 {
			b.WriteString(`.*`)
		}
		b.WriteString(regexp.QuoteMeta(part))
	}
	b.WriteString(`$`)
	return regexp.MustCompile(b.String())
}
