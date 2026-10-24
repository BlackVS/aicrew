package agent

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// The command guard (01a16922-d552): `aicrew-agent guard`, a Claude Code
// PreToolUse hook on the Bash and PowerShell tools of a member home. The
// idea is borrowed from Agent Bridge's command-guard.js (MIT; no code is
// ported): a rule in text is read and then argued with, and Claude Code's
// deny rules match a command's spelling, so a member that decides a reset
// is the simpler way back finds a spelling no rule lists. The guard judges
// each command by what it does: it splits the command line into its
// commands, looks inside the shells and wrappers that run another command
// line, and judges each whatever its options' order or form. The managed
// deny list stays as the second line.
//
// It is a safety catch against a member's mistake, not a sandbox: a
// script file, an alias or another tool still reaches what it refuses.
// It refuses only what reaches beyond the member's own work, since a false
// refusal stops a member that works without a person: a recursive delete
// outside the working directory, discarding work on main, master or the
// default branch, rewriting or deleting what others share, skipping hooks,
// and formatting a disk. Discarding work on the member's own branch and
// deleting inside its working directory pass. A command it passes because
// the branch could not be determined is recorded in logs/guard.jsonl.
//
// The hook protocol (Claude Code 2.1.310, verified on 2026-10-24): the
// payload on stdin names tool_name, tool_input.command and cwd; a refusal
// is hookSpecificOutput.permissionDecision "deny" on stdout with exit 0.
// The guard never blocks on its own failure: anything it cannot read, and
// a panic, answers nothing.

// maxGuardPayload bounds the payload the guard reads.
const maxGuardPayload = 1 << 20

// GuardLogFile is the home's record of the commands the guard passed
// without knowing the branch, relative to the home.
const GuardLogFile = "logs/guard.jsonl"

// guardGitTimeout bounds each git call the guard makes to read a branch.
const guardGitTimeout = 3 * time.Second

// guardPayload is the part of Claude Code's PreToolUse payload the guard
// reads.
type guardPayload struct {
	HookEventName string `json:"hook_event_name"`
	ToolName      string `json:"tool_name"`
	Cwd           string `json:"cwd"`
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

// guardBlind is one line of GuardLogFile.
type guardBlind struct {
	Time    string `json:"time"`
	Rule    string `json:"rule"`
	Command string `json:"command"`
	Dir     string `json:"dir"`
}

// refusal is what a command does that the guard refuses, and what the
// member can do instead; a zero refusal refuses nothing.
type refusal struct{ what, next string }

// guardEnv is what the guard knows besides the command line: the working
// directory the call starts in (root, the bound of a recursive delete), the
// current one (dir, "" once a cd cannot be followed), how to read a
// repository's branches, and where to record a command passed without
// knowing the branch.
type guardEnv struct {
	root, dir string
	// branches is the repository at dir's current branch and its default
	// branch (the remote's HEAD), each "" when it cannot be read.
	branches func(dir string) (current, def string)
	blind    func(rule string, words []string, dir string)
}

// RunGuard is `aicrew-agent guard --home HOME`: it reads a PreToolUse
// payload from in and writes the deny decision to out when the command is
// refused. home, when set, receives GuardLogFile. It always returns 0.
func RunGuard(in io.Reader, out io.Writer, home string) (code int) {
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
	// One call reads each repository's branches once, so a chain of git
	// commands stays within the hook's timeout.
	read := map[string][2]string{}
	branches := func(dir string) (string, string) {
		if b, ok := read[dir]; ok {
			return b[0], b[1]
		}
		current, def := gitBranches(dir)
		read[dir] = [2]string{current, def}
		return current, def
	}
	env := guardEnv{dir: p.Cwd, branches: branches, blind: func(rule string, words []string, dir string) {
		logGuardBlind(home, rule, words, dir)
	}}
	if env.dir == "" {
		env.dir, _ = os.Getwd()
	}
	env.root = env.dir
	r := env.refusal(shell, p.ToolInput.Command)
	if r.what == "" {
		return 0
	}
	var d guardDecision
	d.HookSpecificOutput.HookEventName = "PreToolUse"
	d.HookSpecificOutput.PermissionDecision = "deny"
	d.HookSpecificOutput.PermissionDecisionReason = "aicrew guard refused " + r.what + ". " + r.next +
		" If the task truly needs it, block the running attempt with this refusal as the reason, as docs/ROLES.md " +
		"says, so your coordinator decides; do not work around the guard."
	_ = json.NewEncoder(out).Encode(d)
	return 0
}

// gitBranches reads the current and the default branch of the repository
// at dir with git, each "" when it cannot be read.
func gitBranches(dir string) (current, def string) {
	read := func(args ...string) string {
		ctx, cancel := context.WithTimeout(context.Background(), guardGitTimeout)
		defer cancel()
		cmd := exec.CommandContext(ctx, "git", append([]string{"-C", dir}, args...)...)
		out, err := cmd.Output()
		if err != nil {
			return ""
		}
		return strings.TrimSpace(string(out))
	}
	current = read("symbolic-ref", "--quiet", "--short", "HEAD")
	if current == "" {
		return "", ""
	}
	if remote := read("symbolic-ref", "--quiet", "--short", "refs/remotes/origin/HEAD"); remote != "" {
		_, def, _ = strings.Cut(remote, "/")
	}
	return current, def
}

// maxGuardLogCommand bounds the command a GuardLogFile line records.
const maxGuardLogCommand = 200

// logGuardBlind appends a line to home's GuardLogFile; it records only the
// judged command, cut to maxGuardLogCommand, never the whole command line.
func logGuardBlind(home, rule string, words []string, dir string) {
	if home == "" {
		return
	}
	command := strings.Join(words, " ")
	if len(command) > maxGuardLogCommand {
		command = command[:maxGuardLogCommand]
	}
	line, err := json.Marshal(guardBlind{Time: time.Now().UTC().Format(time.RFC3339), Rule: rule, Command: command, Dir: dir})
	if err != nil {
		return
	}
	file := filepath.Join(home, filepath.FromSlash(GuardLogFile))
	if os.MkdirAll(filepath.Dir(file), 0o700) != nil {
		return
	}
	f, err := os.OpenFile(file, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	defer f.Close()
	_, _ = f.Write(append(line, '\n'))
}

// maxGuardDepth bounds how deep the guard looks into nested shells.
const maxGuardDepth = 8

func (e guardEnv) refusal(shell, line string) refusal {
	return e.refusalIn(shell, line, 0)
}

func (e guardEnv) refusalIn(shell, line string, depth int) refusal {
	if depth > maxGuardDepth {
		return refusal{"a command nested too deep to judge", "Run the command directly, without nesting shells."}
	}
	for _, words := range splitCommands(shell, line) {
		if dir, ok := changedDir(e.dir, words); ok {
			e.dir = dir
			continue
		}
		if r := e.judgeCommand(shell, words, depth); r.what != "" {
			return r
		}
	}
	return refusal{}
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

// changedDir is the working directory after words, when words change it
// (cd, Set-Location and their kin): "" when the new one cannot be told.
func changedDir(dir string, words []string) (string, bool) {
	if len(words) == 0 {
		return "", false
	}
	switch program(words[0]) {
	case "cd", "chdir", "pushd", "set-location", "sl", "push-location":
	default:
		return "", false
	}
	target := ""
	for i := 1; i < len(words); i++ {
		w := words[i]
		lw := strings.ToLower(w)
		if name, _, ok := strings.Cut(lw, ":"); ok && strings.HasPrefix(name, "-") {
			if isPrefixOf(name, "-path", 2) || isPrefixOf(name, "-literalpath", 2) {
				target = w[len(name)+1:]
			}
			continue
		}
		if isPrefixOf(lw, "-path", 2) || isPrefixOf(lw, "-literalpath", 2) {
			if i+1 < len(words) {
				target = words[i+1]
			}
			i++
			continue
		}
		if strings.HasPrefix(w, "-") && w != "-" {
			continue
		}
		target = w
		break
	}
	if target == "" || target == "-" || !literalPath(target) {
		return "", true
	}
	if abs, ok := absPath(target); ok {
		return abs, true
	}
	if dir == "" {
		return "", true
	}
	return path.Join(slashPath(dir), slashPath(target)), true
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

// The refusals' advice.
const (
	nextDeleteInside = "Delete only paths inside your working directory, named literally and relative to it (for " +
		"example .work/build), never the directory itself or .git."
	nextDeleteNamed = "Name the paths to delete literally, relative to your working directory, without variables, " +
		"~, a pipe or xargs."
	nextOwnBranch = "Work on your own task branch (git switch -c <branch>), where discarding your own work is " +
		"allowed; main, master and the default branch stay as the merges left them."
	nextNoForce = "Push without force: rebase or merge onto the remote branch, or push a new commit on top."
	nextRemote  = "Leave deleting and rewriting shared branches and refs to your coordinator."
	nextHooks   = "Run it with its hooks and fix what they report."
	nextHistory = "Leave rewriting history to your coordinator."
	nextFormat  = "Formatting a disk is never part of a task."
	nextEncoded = "Pass the command as plain text with -Command."
)

func (e guardEnv) judgeCommand(shell string, words []string, depth int) refusal {
	// Variable assignments, leaders and their options come before the
	// program. Under xargs the arguments come from stdin.
	piped := false
	for len(words) > 0 {
		w := words[0]
		if leaders[program(w)] {
			piped = piped || program(w) == "xargs"
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
		return refusal{}
	}
	prog, args := program(words[0]), words[1:]
	switch prog {
	case "bash", "sh", "zsh", "dash", "ksh":
		if inner, ok := shellScript(args); ok {
			return e.refusalIn("bash", inner, depth+1)
		}
	case "eval":
		return e.refusalIn(shell, strings.Join(args, " "), depth+1)
	case "powershell", "pwsh":
		if inner, r := powershellScript(args); r.what != "" {
			return r
		} else if inner != "" {
			return e.refusalIn("powershell", inner, depth+1)
		}
	case "invoke-expression", "iex":
		return e.refusalIn("powershell", strings.Join(args, " "), depth+1)
	case "cmd":
		for i, a := range args {
			if la := strings.ToLower(a); la == "/c" || la == "/k" || la == "/r" {
				return e.refusalIn("cmd", strings.Join(args[i+1:], " "), depth+1)
			}
		}
	case "git":
		return e.judgeGit(args)
	case "rm":
		if shell == "powershell" {
			return e.removeItem(prog, args, piped)
		}
		return e.judgeRm(args, piped)
	case "remove-item", "ri", "del", "erase", "rmdir", "rd":
		if r := e.removeItem(prog, args, piped); r.what != "" {
			return r
		}
		for _, a := range args {
			if la := strings.ToLower(a); strings.HasPrefix(la, "/s") || la == "/q/s" {
				var targets []string
				for _, t := range args {
					if !strings.HasPrefix(t, "/") {
						targets = append(targets, t)
					}
				}
				return e.deleteTargets(prog+" "+a, targets, piped, false)
			}
		}
	case "find":
		for _, a := range args {
			if a == "-delete" {
				var starts []string
				for _, s := range args {
					if strings.HasPrefix(s, "-") || s == "(" || s == "!" {
						break
					}
					starts = append(starts, s)
				}
				if len(starts) == 0 {
					starts = []string{"."}
				}
				return e.deleteTargets("find -delete", starts, piped, true)
			}
		}
	case "format", "format-volume", "clear-disk", "diskpart":
		return refusal{"formatting a disk (" + prog + ")", nextFormat}
	default:
		if strings.HasPrefix(prog, "mkfs") {
			return refusal{"formatting a disk (" + prog + ")", nextFormat}
		}
	}
	return refusal{}
}

// judgeRm judges a POSIX rm: a recursive one deletes only inside the
// working directory.
func (e guardEnv) judgeRm(args []string, piped bool) refusal {
	recursive := ""
	var targets []string
	options := true
	for _, a := range args {
		switch {
		case options && a == "--":
			options = false
		case options && (a == "--recursive" || (shortCluster(a) && strings.ContainsAny(a[1:], "rR"))):
			recursive = a
		case options && strings.HasPrefix(a, "-") && a != "-":
		default:
			targets = append(targets, a)
		}
	}
	if recursive == "" {
		return refusal{}
	}
	return e.deleteTargets("rm "+recursive, targets, piped, false)
}

// deleteTargets judges a recursive delete of targets: each must name a
// path inside the working directory (or, with sameOK, the directory
// itself). Targets from stdin cannot be judged; no target deletes nothing.
func (e guardEnv) deleteTargets(spelling string, targets []string, piped, sameOK bool) refusal {
	if piped {
		return refusal{"a recursive delete whose paths cannot be judged (" + spelling + ")", nextDeleteNamed}
	}
	for _, t := range targets {
		for _, one := range strings.Split(t, ",") {
			if one == "" {
				continue
			}
			if !literalPath(one) {
				return refusal{"a recursive delete whose paths cannot be judged (" + spelling + " " + one + ")", nextDeleteNamed}
			}
			if !e.inside(one, sameOK) {
				return refusal{"a recursive delete outside the working directory (" + spelling + " " + one + ")",
					nextDeleteInside}
			}
		}
	}
	return refusal{}
}

// literalPath reports whether p names a path without expanding a variable,
// a command or a home directory.
func literalPath(p string) bool {
	return !strings.ContainsAny(p, "$`%") && !strings.HasPrefix(p, "~")
}

// slashPath is p with forward slashes.
func slashPath(p string) string { return strings.ReplaceAll(p, `\`, "/") }

// absPath is p as a cleaned absolute path with forward slashes, when p is
// absolute: /x, //host/x, C:/x or C:x, and on Windows Git Bash's /c/x.
func absPath(p string) (string, bool) {
	p = slashPath(p)
	if runtime.GOOS == "windows" && len(p) >= 3 && p[0] == '/' && p[2] == '/' && isLetter(p[1]) {
		p = p[1:2] + ":" + p[2:]
	}
	switch {
	case len(p) >= 2 && p[1] == ':' && isLetter(p[0]):
		return p[:2] + path.Clean("/"+p[2:]), true
	case strings.HasPrefix(p, "//"):
		return "/" + path.Clean(p), true
	case strings.HasPrefix(p, "/"):
		return path.Clean(p), true
	}
	return "", false
}

func isLetter(c byte) bool { return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' }

// inside reports whether target, resolved from the current directory,
// names a path strictly inside the directory the call started in (or, with
// sameOK, that directory itself), not in .git, and not a glob over that
// directory's own entries.
func (e guardEnv) inside(target string, sameOK bool) bool {
	root, ok := absPath(e.root)
	if !ok {
		return false
	}
	abs, ok := absPath(target)
	if !ok {
		dir, known := absPath(e.dir)
		if !known {
			return false
		}
		abs, _ = absPath(dir + "/" + slashPath(target))
	}
	a, r := abs, strings.TrimSuffix(root, "/")
	if runtime.GOOS == "windows" {
		a, r = strings.ToLower(a), strings.ToLower(r)
	}
	rel := ""
	switch {
	case a == r:
		return sameOK
	case strings.HasPrefix(a, r+"/"):
		rel = abs[len(r)+1:]
	default:
		return false
	}
	parts := strings.Split(rel, "/")
	if strings.ContainsAny(parts[0], "*?[") {
		return false
	}
	for _, part := range parts {
		if strings.EqualFold(part, ".git") {
			return false
		}
	}
	return true
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
// encoded command cannot be, and is refused.
func powershellScript(args []string) (string, refusal) {
	for i := 0; i < len(args); i++ {
		la := strings.ToLower(args[i])
		switch {
		case isPrefixOf(la, "-encodedcommand", 3) || la == "-e" || la == "-ec":
			return "", refusal{"an encoded PowerShell command, which cannot be judged", nextEncoded}
		case isPrefixOf(la, "-command", 2):
			return strings.Join(args[i+1:], " "), refusal{}
		case isPrefixOf(la, "-file", 2):
			return "", refusal{}
		case !strings.HasPrefix(la, "-"):
			return strings.Join(args[i:], " "), refusal{}
		}
		for _, v := range powershellValued {
			if isPrefixOf(la, v, 3) {
				i++
				break
			}
		}
	}
	return "", refusal{}
}

// removeValued are Remove-Item's parameters, other than its paths, that
// take the next word as their value.
var removeValued = []string{"-filter", "-include", "-exclude", "-credential", "-stream"}

// removeItem judges PowerShell's Remove-Item (and its aliases): with
// -Recurse in any prefix PowerShell accepts, and not -Recurse:$false, it
// deletes only inside the working directory. Without a path it deletes
// what the pipeline gives it, which cannot be judged.
func (e guardEnv) removeItem(prog string, args []string, piped bool) refusal {
	recursive := ""
	var targets []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		la := strings.ToLower(a)
		if !strings.HasPrefix(la, "-") {
			targets = append(targets, a)
			continue
		}
		name, value, colon := strings.Cut(la, ":")
		switch {
		case isPrefixOf(name, "-recurse", 2):
			if !(colon && value == "$false") {
				recursive = a
			}
		case isPrefixOf(name, "-path", 2) || isPrefixOf(name, "-literalpath", 2):
			if colon {
				targets = append(targets, a[len(name)+1:])
			} else if i+1 < len(args) {
				targets = append(targets, args[i+1])
				i++
			}
		default:
			for _, v := range removeValued {
				if isPrefixOf(name, v, 3) && !colon {
					i++
					break
				}
			}
		}
	}
	if recursive == "" {
		return refusal{}
	}
	return e.deleteTargets(prog+" "+recursive, targets, piped || len(targets) == 0, false)
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

// judgeGit judges a git command after its global options. Discarding work
// is refused only on main, master or the default branch; pushing with
// force, deleting or rewriting shared refs and skipping hooks are refused
// on any branch.
func (e guardEnv) judgeGit(args []string) refusal {
	repo := e.dir
	for len(args) > 0 && strings.HasPrefix(args[0], "-") {
		name, _, _ := strings.Cut(args[0], "=")
		switch {
		case args[0] == "-C" && len(args) > 1:
			repo = joinDir(repo, args[1])
		case name == "--git-dir" || name == "--work-tree":
			repo = ""
		}
		if gitValued[args[0]] && len(args) > 1 {
			args = args[1:]
		}
		args = args[1:]
	}
	if len(args) == 0 {
		return refusal{}
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
	// discard is a command that discards work: refused on a shared branch.
	discard := ""
	switch sub {
	case "reset":
		if has("--hard") {
			discard = "git reset --hard"
		}
	case "clean":
		if (has("--force") || short("f")) && !has("--dry-run") && !short("n") {
			discard = "git clean --force"
		}
	case "checkout":
		if paths() || has("--force") || short("f") {
			discard = "git checkout discarding changes"
		}
	case "restore":
		staged := has("--staged") || short("S")
		worktree := has("--worktree") || short("W")
		if paths() && (!staged || worktree) {
			discard = "git restore of the whole tree"
		}
	case "stash":
		if len(opts) > 0 && (opts[0] == "drop" || opts[0] == "clear") {
			discard = "git stash " + opts[0]
		}
	case "commit":
		switch {
		case has("--no-verify") || noVerifyCluster(opts):
			return refusal{"git commit --no-verify", nextHooks}
		case has("--amend"):
			discard = "git commit --amend"
		}
	case "push":
		switch {
		case has("--force", "--force-with-lease", "--force-if-includes") || short("f"):
			return refusal{"git push --force", nextNoForce}
		case has("--delete", "--mirror", "--prune") || short("d"):
			return refusal{"git push --delete", nextRemote}
		case has("--no-verify"):
			return refusal{"git push --no-verify", nextHooks}
		}
		for _, o := range opts {
			if strings.HasPrefix(o, "+") || (strings.HasPrefix(o, ":") && len(o) > 1) {
				return refusal{"git push of a forced or deleting refspec (" + o + ")", nextNoForce}
			}
		}
	case "branch":
		if short("D") || ((has("--delete") || short("d")) && (has("--force") || short("f"))) {
			return e.branchDelete(repo, opts)
		}
	case "filter-branch", "filter-repo":
		return refusal{"git " + sub, nextHistory}
	case "update-ref":
		if has("-d", "--delete") {
			return refusal{"git update-ref -d", nextRemote}
		}
	}
	if discard == "" {
		return refusal{}
	}
	current, def := "", ""
	if repo != "" {
		current, def = e.branches(repo)
	}
	if current == "" {
		if e.blind != nil {
			e.blind(discard, append([]string{"git"}, args...), repo)
		}
		return refusal{}
	}
	if sharedBranch(current, def) {
		return refusal{discard + " on " + current, nextOwnBranch}
	}
	return refusal{}
}

// branchDelete judges a forced branch delete: refused for main, master or
// the default branch.
func (e guardEnv) branchDelete(repo string, opts []string) refusal {
	def := ""
	if repo != "" {
		_, def = e.branches(repo)
	}
	for _, o := range opts {
		if !strings.HasPrefix(o, "-") && sharedBranch(o, def) {
			return refusal{"git branch -D " + o, nextOwnBranch}
		}
	}
	return refusal{}
}

// sharedBranch reports whether branch is main, master or the default
// branch def.
func sharedBranch(branch, def string) bool {
	return branch == "main" || branch == "master" || (def != "" && branch == def)
}

// joinDir is dir after git's -C sub: sub itself when absolute, else joined
// to dir; "" when either cannot be told.
func joinDir(dir, sub string) string {
	if !literalPath(sub) {
		return ""
	}
	if abs, ok := absPath(sub); ok {
		return abs
	}
	if dir == "" {
		return ""
	}
	return path.Join(slashPath(dir), slashPath(sub))
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
