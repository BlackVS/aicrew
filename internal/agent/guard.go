package agent

import (
	"encoding/json"
	"io"
	"path"
	"strings"
)

// The command guard (01a16922-d552): `aicrew-agent guard`, a Claude Code
// PreToolUse hook on the Bash and PowerShell tools of a member home. The
// idea is borrowed from Agent Bridge's command-guard.js (MIT; no code is
// ported): a rule in text is read and then argued with, and Claude Code's
// deny rules match a command's spelling, so a member that decides a reset
// is the simpler way back finds a spelling no rule lists. The guard judges
// each command by what it does: it splits the command line into its
// commands, looks inside the shells and wrappers that run another command
// line, and refuses destructive git and recursive deletes whatever their
// options' order or form. The managed deny list stays as the second line.
//
// The hook protocol (Claude Code 2.1.310, verified on 2026-10-24): the
// payload on stdin names tool_name and tool_input.command; a refusal is
// hookSpecificOutput.permissionDecision "deny" on stdout with exit 0. The
// guard never blocks on its own failure: anything it cannot read, and a
// panic, answers nothing.

// maxGuardPayload bounds the payload the guard reads.
const maxGuardPayload = 1 << 20

// guardPayload is the part of Claude Code's PreToolUse payload the guard
// reads.
type guardPayload struct {
	HookEventName string `json:"hook_event_name"`
	ToolName      string `json:"tool_name"`
	ToolInput     struct {
		Command string `json:"command"`
	} `json:"tool_input"`
}

// guardDecision is the hook's answer to a refused call.
type guardDecision struct {
	HookSpecificOutput struct {
		HookEventName            string `json:"hookEventName"`
		PermissionDecision       string `json:"permissionDecision"`
		PermissionDecisionReason string `json:"permissionDecisionReason"`
	} `json:"hookSpecificOutput"`
}

// RunGuard is `aicrew-agent guard`: it reads a PreToolUse payload from in
// and writes the deny decision to out when the command is refused. It
// always returns 0.
func RunGuard(in io.Reader, out io.Writer) (code int) {
	defer func() {
		if recover() != nil {
			code = 0
		}
	}()
	raw, err := io.ReadAll(io.LimitReader(in, maxGuardPayload))
	if err != nil {
		return 0
	}
	var p guardPayload
	if json.Unmarshal(raw, &p) != nil || (p.HookEventName != "" && p.HookEventName != "PreToolUse") {
		return 0
	}
	shell := ""
	switch p.ToolName {
	case "Bash":
		shell = "bash"
	case "PowerShell":
		shell = "powershell"
	default:
		return 0
	}
	what := GuardRefusal(shell, p.ToolInput.Command)
	if what == "" {
		return 0
	}
	var d guardDecision
	d.HookSpecificOutput.HookEventName = "PreToolUse"
	d.HookSpecificOutput.PermissionDecision = "deny"
	d.HookSpecificOutput.PermissionDecisionReason = "aicrew guard: " + what + " is refused in a member home. If the " +
		"task needs it, turn to your coordinator as docs/ROLES.md says for a refusal (block the running attempt " +
		"with this refusal as the reason) instead of working around this guard."
	_ = json.NewEncoder(out).Encode(d)
	return 0
}

// maxGuardDepth bounds how deep the guard looks into nested shells.
const maxGuardDepth = 8

// GuardRefusal is what the command line does that the guard refuses, such
// as "git reset --hard", or "" when it refuses nothing. shell is the tool's
// shell: "bash" or "powershell".
func GuardRefusal(shell, line string) string {
	return refusalIn(shell, line, 0)
}

func refusalIn(shell, line string, depth int) string {
	if depth > maxGuardDepth {
		return "a command nested too deep to judge"
	}
	for _, words := range splitCommands(shell, line) {
		if what := judgeCommand(shell, words, depth); what != "" {
			return what
		}
	}
	return ""
}

// splitCommands splits a command line into its commands, each as its
// words: on ;, &, &&, |, ||, newlines, parentheses and standalone braces
// outside quotes, and, in bash, backquotes. Quotes are removed; a
// backslash (bash) or backquote (PowerShell) escapes the next character
// only before a quote, a space or itself, so Windows paths keep their
// backslashes.
func splitCommands(shell, line string) [][]string {
	var (
		cmds  [][]string
		words []string
		word  strings.Builder
		in    bool // a word is open, maybe empty ("")
		quote rune
	)
	escape := '\\'
	if shell == "powershell" {
		escape = '`'
	}
	endWord := func() {
		if in {
			words = append(words, word.String())
		}
		word.Reset()
		in = false
	}
	endCmd := func() {
		endWord()
		var kept []string
		for _, w := range words {
			if w == "{" || w == "}" {
				if len(kept) > 0 {
					cmds = append(cmds, kept)
				}
				kept = nil
				continue
			}
			kept = append(kept, w)
		}
		if len(kept) > 0 {
			cmds = append(cmds, kept)
		}
		words = nil
	}
	rs := []rune(line)
	for i := 0; i < len(rs); i++ {
		r := rs[i]
		switch {
		case quote != 0:
			switch {
			case r == quote:
				quote = 0
			case r == escape && quote == '"' && i+1 < len(rs) && strings.ContainsRune("\"\\`$", rs[i+1]):
				i++
				word.WriteRune(rs[i])
			default:
				word.WriteRune(r)
			}
		case r == '\'' || r == '"':
			quote, in = r, true
		case r == escape && i+1 < len(rs) && strings.ContainsRune("\"' \\`", rs[i+1]):
			i++
			word.WriteRune(rs[i])
			in = true
		case r == ';' || r == '&' || r == '|' || r == '\n' || r == '\r' || r == '(' || r == ')' ||
			(r == '`' && shell == "bash"):
			endCmd()
		case r == ' ' || r == '\t':
			endWord()
		case r == '$' && i+1 < len(rs) && rs[i+1] == '(':
			endCmd()
			i++
		default:
			word.WriteRune(r)
			in = true
		}
	}
	endCmd()
	return cmds
}

// program is a command word's program name: its base name, lower case,
// without a Windows executable extension.
func program(w string) string {
	w = strings.ToLower(path.Base(strings.ReplaceAll(w, `\`, "/")))
	for _, ext := range []string{".exe", ".cmd", ".bat", ".com"} {
		w = strings.TrimSuffix(w, ext)
	}
	return w
}

// leaders are the words that run the command after them: their own
// options are skipped.
var leaders = map[string]bool{"sudo": true, "env": true, "command": true, "exec": true, "nohup": true, "time": true,
	"nice": true, "xargs": true, "builtin": true, "doas": true}

func judgeCommand(shell string, words []string, depth int) string {
	// Variable assignments, leaders and their options come before the
	// program.
	for len(words) > 0 {
		w := words[0]
		if leaders[program(w)] {
			words = words[1:]
			for len(words) > 0 && strings.HasPrefix(words[0], "-") {
				words = words[1:]
			}
			continue
		}
		if !strings.Contains(w, "=") || strings.HasPrefix(w, "-") || strings.HasPrefix(w, "/") {
			break
		}
		words = words[1:]
	}
	if len(words) == 0 {
		return ""
	}
	prog, args := program(words[0]), words[1:]
	switch prog {
	case "bash", "sh", "zsh", "dash", "ksh":
		if inner, ok := shellScript(args); ok {
			return refusalIn("bash", inner, depth+1)
		}
	case "eval":
		return refusalIn(shell, strings.Join(args, " "), depth+1)
	case "powershell", "pwsh":
		if inner, what := powershellScript(args); what != "" {
			return what
		} else if inner != "" {
			return refusalIn("powershell", inner, depth+1)
		}
	case "invoke-expression", "iex":
		return refusalIn("powershell", strings.Join(args, " "), depth+1)
	case "cmd":
		for i, a := range args {
			if la := strings.ToLower(a); la == "/c" || la == "/k" || la == "/r" {
				return refusalIn("cmd", strings.Join(args[i+1:], " "), depth+1)
			}
		}
	case "git":
		return judgeGit(args)
	case "rm":
		if shell == "powershell" {
			return removeItem(prog, args)
		}
		for _, a := range args {
			if a == "--recursive" || (shortCluster(a) && strings.ContainsAny(a[1:], "rR")) {
				return "a recursive delete (rm " + a + ")"
			}
		}
	case "remove-item", "ri", "del", "erase", "rmdir", "rd":
		if what := removeItem(prog, args); what != "" {
			return what
		}
		for _, a := range args {
			if la := strings.ToLower(a); strings.HasPrefix(la, "/s") || la == "/q/s" {
				return "a recursive delete (" + prog + " " + a + ")"
			}
		}
	case "find":
		for _, a := range args {
			if a == "-delete" {
				return "a recursive delete (find -delete)"
			}
		}
	case "format", "format-volume", "clear-disk", "diskpart":
		return "formatting a disk (" + prog + ")"
	default:
		if strings.HasPrefix(prog, "mkfs") {
			return "formatting a disk (" + prog + ")"
		}
	}
	return ""
}

// shellScript is the script a POSIX shell runs with -c, from its
// arguments.
func shellScript(args []string) (string, bool) {
	for i, a := range args {
		if !strings.HasPrefix(a, "-") || a == "--" {
			return "", false
		}
		if !strings.HasPrefix(a, "--") && strings.Contains(a, "c") && i+1 < len(args) {
			return args[i+1], true
		}
	}
	return "", false
}

// powershellValued are powershell.exe's and pwsh's options that take the
// next word as their value.
var powershellValued = []string{"-executionpolicy", "-windowstyle", "-outputformat", "-inputformat",
	"-configurationname", "-workingdirectory", "-version", "-psconsolefile", "-settingsfile", "-custompipename"}

// powershellScript is the command line powershell.exe or pwsh runs, from its
// arguments: after -Command (in any prefix PowerShell accepts), or from the
// first word that is not an option. A script file is not judged; an
// encoded command cannot be, and is refused (what).
func powershellScript(args []string) (inner, what string) {
	for i := 0; i < len(args); i++ {
		la := strings.ToLower(args[i])
		switch {
		case isPrefixOf(la, "-encodedcommand", 3) || la == "-e" || la == "-ec":
			return "", "an encoded PowerShell command, which cannot be judged"
		case isPrefixOf(la, "-command", 2):
			return strings.Join(args[i+1:], " "), ""
		case isPrefixOf(la, "-file", 2):
			return "", ""
		case !strings.HasPrefix(la, "-"):
			return strings.Join(args[i:], " "), ""
		}
		for _, v := range powershellValued {
			if isPrefixOf(la, v, 3) {
				i++
				break
			}
		}
	}
	return "", ""
}

// removeItem judges PowerShell's Remove-Item (and its aliases): -Recurse in
// any prefix PowerShell accepts.
func removeItem(prog string, args []string) string {
	for _, a := range args {
		la := strings.ToLower(a)
		name, _, _ := strings.Cut(la, ":")
		if isPrefixOf(name, "-recurse", 2) {
			return "a recursive delete (" + prog + " " + a + ")"
		}
	}
	return ""
}

// isPrefixOf reports whether w is a prefix of full at least min long.
func isPrefixOf(w, full string, min int) bool {
	return len(w) >= min && strings.HasPrefix(full, w)
}

// shortCluster reports whether a is a cluster of short options, such as
// -rf.
func shortCluster(a string) bool {
	if len(a) < 2 || a[0] != '-' || a[1] == '-' {
		return false
	}
	for _, c := range a[1:] {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z') {
			return false
		}
	}
	return true
}

// gitValued are git's global options that take the next word as their
// value.
var gitValued = map[string]bool{"-C": true, "-c": true, "--git-dir": true, "--work-tree": true, "--namespace": true,
	"--super-prefix": true, "--config-env": true, "--exec-path": true, "--list-cmds": true}

// judgeGit judges a git command after its global options.
func judgeGit(args []string) string {
	for len(args) > 0 && strings.HasPrefix(args[0], "-") {
		if gitValued[args[0]] && len(args) > 1 {
			args = args[1:]
		}
		args = args[1:]
	}
	if len(args) == 0 {
		return ""
	}
	sub, opts := args[0], args[1:]
	has := func(names ...string) bool {
		for _, o := range opts {
			name, _, _ := strings.Cut(o, "=")
			for _, n := range names {
				if name == n {
					return true
				}
			}
		}
		return false
	}
	short := func(letters string) bool {
		for _, o := range opts {
			if shortCluster(o) && strings.ContainsAny(o[1:], letters) {
				return true
			}
		}
		return false
	}
	paths := func() bool {
		for _, o := range opts {
			switch o {
			case ".", "./", "*", ":/", ":/*", ":(top)":
				return true
			}
		}
		return false
	}
	what := ""
	switch sub {
	case "reset":
		if has("--hard") {
			what = "git reset --hard"
		}
	case "clean":
		if (has("--force") || short("f")) && !has("--dry-run") && !short("n") {
			what = "git clean --force"
		}
	case "push":
		switch {
		case has("--force", "--force-with-lease", "--force-if-includes") || short("f"):
			what = "git push --force"
		case has("--delete", "--mirror", "--prune") || short("d"):
			what = "git push --delete"
		case has("--no-verify"):
			what = "git push --no-verify"
		default:
			for _, o := range opts {
				if strings.HasPrefix(o, "+") || (strings.HasPrefix(o, ":") && len(o) > 1) {
					what = "git push of a forced or deleting refspec (" + o + ")"
				}
			}
		}
	case "branch":
		if short("D") || ((has("--delete") || short("d")) && (has("--force") || short("f"))) {
			what = "git branch -D"
		}
	case "checkout":
		if paths() || has("--force") || short("f") {
			what = "git checkout discarding changes"
		}
	case "restore":
		staged := has("--staged") || short("S")
		worktree := has("--worktree") || short("W")
		if paths() && (!staged || worktree) {
			what = "git restore of the whole tree"
		}
	case "stash":
		if len(opts) > 0 && (opts[0] == "drop" || opts[0] == "clear") {
			what = "git stash " + opts[0]
		}
	case "commit":
		switch {
		case has("--amend"):
			what = "git commit --amend"
		case has("--no-verify") || noVerifyCluster(opts):
			what = "git commit --no-verify"
		}
	case "filter-branch", "filter-repo":
		what = "git " + sub
	case "update-ref":
		if has("-d", "--delete") {
			what = "git update-ref -d"
		}
	}
	return what
}

// noVerifyCluster reports a commit's -n (--no-verify) among its short
// options, alone or in a cluster (-an, -nm): a letter that takes a value
// ends the cluster, the rest of it being that value.
func noVerifyCluster(opts []string) bool {
	for _, o := range opts {
		if !shortCluster(o) {
			continue
		}
		for _, c := range o[1:] {
			if c == 'n' {
				return true
			}
			if strings.ContainsRune("mFcCtu", c) {
				break
			}
		}
	}
	return false
}
