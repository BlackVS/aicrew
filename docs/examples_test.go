package docs_test

// TestDocExamples checks the names the documentation's examples use against
// the code: every `aicrew`, `aicrew-agent` and `aicrewd` command word and
// flag, every aicrewd route and every automation builtin named as a call.
// docs/DEVELOPMENT.md ("Examples in the documentation") states the rule,
// the sources and the exclusions.

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"

	"go.starlark.net/starlark"

	"github.com/BlackVS/aicrew/internal/agent"
	"github.com/BlackVS/aicrew/internal/automation"
)

// skipMarker exempts the line it is on, for prose that names something on
// purpose, such as a command that does not exist.
const skipMarker = "<!-- doc-check: skip -->"

// ownRoutes are the route families aicrewd serves; any other /v1/ route
// the docs name is aimem's or another service's.
var ownRoutes = []string{"/v1/crew/", "/v1/admin/"}

// checkedDoc reports whether a Markdown file is an operational document the
// check covers. Designs, proposals and probe reports describe what is
// proposed or was observed; the archive, the roadmap and the changelog are
// records of the past, and the session state is a local file.
func checkedDoc(rel string) bool {
	base := filepath.Base(rel)
	switch {
	case strings.HasPrefix(rel, "archive/"), strings.HasPrefix(rel, "proposals/"):
		return false
	case base == "ROADMAP.md", base == "SESSION-STATE.md":
		return false
	case strings.HasPrefix(base, "DESIGN"), strings.HasSuffix(base, "-PROPOSAL.md"), strings.HasSuffix(base, "-PROBE.md"):
		return false
	}
	return strings.HasSuffix(base, ".md")
}

// binary is one command's current names: its top-level commands, the
// words its usage and dispatch use, and its flags.
type binary struct {
	top, words, flags map[string]bool
}

type vocabulary struct {
	bins       map[string]binary
	routes     []*regexp.Regexp
	routePaths []string
	builtins   map[string]bool
}

// doc is one document to check, by the name a problem reports.
type doc struct {
	name, body string
}

func TestDocExamples(t *testing.T) {
	v := loadVocabulary(t)
	var problems []string
	for _, d := range checkedDocs(t) {
		problems = append(problems, checkDoc(d, v)...)
	}
	if len(problems) > 0 {
		t.Errorf("%d stale name(s) in documentation examples:\n%s\n\nFix the example to the current name, or, for prose that names something on purpose, end the line with %s. See docs/DEVELOPMENT.md.",
			len(problems), strings.Join(problems, "\n"), skipMarker)
	}
}

// checkedDocs are README.md, the covered files under docs/ and the
// Markdown files the bootstrap writes into an agent home.
func checkedDocs(t *testing.T) []doc {
	read := func(p, name string) doc {
		b, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		return doc{name, string(b)}
	}
	docs := []doc{read("../README.md", "README.md")}
	err := filepath.WalkDir(".", func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if rel := filepath.ToSlash(p); !d.IsDir() && checkedDoc(rel) {
			docs = append(docs, read(p, "docs/"+rel))
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	home := agent.HomeDocs()
	for _, p := range slices.Sorted(func(yield func(string) bool) {
		for p := range home {
			if !yield(p) {
				return
			}
		}
	}) {
		docs = append(docs, doc{"home:" + p, home[p]})
	}
	return docs
}

func checkDoc(d doc, v vocabulary) []string {
	var out []string
	fenced := false
	// continued holds the physical lines of a fenced command whose earlier
	// lines ended in a shell (\) or PowerShell (`) continuation.
	var continued []physLine
	// open holds the physical lines of an inline code span that a line
	// break interrupted; a blank line or a fence ends the paragraph, and
	// with it the span, as Markdown reads it.
	var open []physLine
	for i, line := range strings.Split(d.body, "\n") {
		line = strings.TrimRight(line, "\r")
		if strings.HasPrefix(strings.TrimSpace(line), "```") {
			fenced = !fenced
			out = append(out, checkCommands(d.name, continued, v)...)
			continued, open = nil, nil
			continue
		}
		if strings.Contains(line, skipMarker) {
			continue
		}
		at := fmt.Sprintf("%s:%d", d.name, i+1)
		spans := []string{line}
		if fenced {
			body := strings.TrimRight(line, " \t")
			if strings.HasSuffix(body, `\`) || strings.HasSuffix(body, "`") {
				continued = append(continued, physLine{i + 1, body[:len(body)-1]})
			} else {
				out = append(out, checkCommands(d.name, append(continued, physLine{i + 1, line}), v)...)
				continued = nil
			}
		} else {
			spans = nil
			rest := line
			if open != nil {
				j := strings.IndexByte(rest, '`')
				switch {
				case j >= 0:
					spans = append(spans, rest[:j])
					out = append(out, checkSpan(d.name, append(open, physLine{i + 1, rest[:j]}), v)...)
					open, rest = nil, rest[j+1:]
				case strings.TrimSpace(rest) == "":
					open, rest = nil, ""
				default:
					spans = append(spans, rest)
					open, rest = append(open, physLine{i + 1, rest}), ""
				}
			}
			for _, m := range inlineCode.FindAllStringSubmatch(rest, -1) {
				spans = append(spans, m[1])
				out = append(out, checkSpan(d.name, []physLine{{i + 1, m[1]}}, v)...)
			}
			// A backtick left over once the pairs are taken opens a span
			// the next line continues.
			if tail := inlineCode.ReplaceAllString(rest, ""); strings.Contains(tail, "`") {
				s := tail[strings.IndexByte(tail, '`')+1:]
				spans = append(spans, s)
				open = []physLine{{i + 1, s}}
			}
		}
		for _, s := range spans {
			out = append(out, checkRoutes(at, s, v)...)
		}
	}
	return append(out, checkCommands(d.name, continued, v)...)
}

// checkSpan checks one inline code span, which may cover several physical
// lines: the invocations in it, and a builtin call it starts with.
func checkSpan(name string, lines []physLine, v vocabulary) []string {
	out := checkCommands(name, lines, v)
	if c := builtinCall.FindStringSubmatch(strings.TrimSpace(lines[0].text)); c != nil && !v.builtins[c[1]] {
		out = append(out, fmt.Sprintf("%s:%d: automation builtin %q (nearest: %s)", name, lines[0].n, c[1], nearest(c[1], keys(v.builtins))))
	}
	return out
}

var (
	inlineCode   = regexp.MustCompile("`([^`]+)`")
	builtinCall  = regexp.MustCompile(`^([a-z_][a-z0-9_]*)\(`)
	invocation   = regexp.MustCompile(`^(?:\S*[/\\])?(aicrew|aicrew-agent|aicrewd)(?:\.exe)?$`)
	commandWord  = regexp.MustCompile(`^[a-z][a-z0-9-]*$`)
	flagToken    = regexp.MustCompile(`^--?([a-z][a-z0-9-]*)`)
	placeholder  = regexp.MustCompile(`<[^>]+>`)
	routeMention = regexp.MustCompile(`(/v1/[A-Za-z0-9_{}<>.\-/]*)`)
)

// physLine is one physical line of a document, by its 1-based number.
type physLine struct {
	n    int
	text string
}

// word is a whitespace-separated token and the line it is on.
type word struct {
	s string
	n int
}

// checkCommands reads the shell commands of one logical line, which may
// span several physical lines, and checks every invocation of the three
// binaries in it. Each problem names the physical line of the offending
// word.
func checkCommands(name string, lines []physLine, v vocabulary) []string {
	var words []word
	for _, l := range lines {
		for f := range strings.FieldsSeq(l.text) {
			words = append(words, word{f, l.n})
		}
	}
	var out []string
	var seg []word
	flush := func() {
		out = append(out, checkInvocation(name, seg, v)...)
		seg = nil
	}
	for _, w := range words {
		switch {
		case w.s == "|" || w.s == "||" || w.s == "&&" || w.s == ";":
			flush()
		case strings.Contains(w.s, "$("):
			flush()
			if rest := w.s[strings.LastIndex(w.s, "$(")+2:]; rest != "" {
				seg = append(seg, word{rest, w.n})
			}
		case strings.HasSuffix(w.s, ";"):
			seg = append(seg, word{strings.TrimSuffix(w.s, ";"), w.n})
			flush()
		default:
			seg = append(seg, w)
		}
	}
	flush()
	return out
}

func checkInvocation(name string, seg []word, v vocabulary) []string {
	for len(seg) > 0 && (seg[0].s == "$" || seg[0].s == ">" || seg[0].s == "&") {
		seg = seg[1:]
	}
	if len(seg) < 2 {
		return nil
	}
	m := invocation.FindStringSubmatch(strings.Trim(seg[0].s, `"'`))
	if m == nil {
		return nil
	}
	prog, b := m[1], v.bins[m[1]]
	at := func(w word) string { return fmt.Sprintf("%s:%d", name, w.n) }
	var out []string
	rest := seg[1:]
	if commandWord.MatchString(seg[1].s) {
		cmd := seg[1]
		if !b.top[cmd.s] {
			return []string{fmt.Sprintf("%s: command \"%s %s\" (nearest: %s)", at(cmd), prog, cmd.s, nearest(cmd.s, keys(b.top)))}
		}
		if len(seg) > 2 && commandWord.MatchString(seg[2].s) && !b.words[seg[2].s] {
			out = append(out, fmt.Sprintf("%s: subcommand \"%s %s %s\" (nearest: %s)", at(seg[2]), prog, cmd.s, seg[2].s, nearest(seg[2].s, keys(b.words))))
		}
		rest = seg[2:]
	}
	for _, w := range rest {
		if fm := flagToken.FindStringSubmatch(w.s); fm != nil && !b.flags[fm[1]] {
			out = append(out, fmt.Sprintf("%s: flag %q of %s (nearest: --%s)", at(w), w.s, prog, nearest(fm[1], keys(b.flags))))
		}
	}
	return out
}

func checkRoutes(at, s string, v vocabulary) []string {
	var out []string
	for _, m := range routeMention.FindAllStringSubmatch(s, -1) {
		r := strings.TrimRight(m[1], ".,:")
		r = placeholder.ReplaceAllString(r, "{x}")
		own := slices.ContainsFunc(ownRoutes, func(p string) bool { return strings.HasPrefix(r+"/", p) })
		if !own || knownRoute(r, v) {
			continue
		}
		out = append(out, fmt.Sprintf("%s: route %q (nearest: %s)", at, m[1], nearest(r, v.routePaths)))
	}
	return out
}

// knownRoute accepts a full route, with {param} or <placeholder> standing
// for a segment, and a prefix that names a family of routes
// ("/v1/crew/attempts/{id}/").
func knownRoute(r string, v vocabulary) bool {
	for _, re := range v.routes {
		if re.MatchString(r) {
			return true
		}
	}
	prefix := strings.TrimSuffix(r, "/") + "/"
	for _, p := range v.routePaths {
		if strings.HasPrefix(p, prefix) {
			return true
		}
	}
	return false
}

// dispatchFuncs are, per binary, the functions whose comparisons of the
// first argument name its top-level commands.
var dispatchFuncs = map[string][]string{
	"aicrew":       {"run"},
	"aicrew-agent": {"main", "run"},
	"aicrewd":      {"configCommand"},
}

// stepSource holds the operations `aicrew-agent step OP` asks the launcher
// for; its word literals are aicrew-agent's words too.
const stepSource = "../internal/agent/stepserver.go"

// loadVocabulary reads the current names from the code: each binary's
// cmd/ source, the route table the server's test pins
// (internal/server/routes.txt), and the automation host API beside
// Starlark's universe.
func loadVocabulary(t *testing.T) vocabulary {
	v := vocabulary{bins: map[string]binary{}, builtins: map[string]bool{"main": true}}
	for prog, funcs := range dispatchFuncs {
		v.bins[prog] = loadBinary(t, prog, funcs)
	}

	b, err := os.ReadFile("../internal/server/routes.txt")
	if err != nil {
		t.Fatal(err)
	}
	param := regexp.MustCompile(`\\\{[^}]+\\\}`)
	for line := range strings.Lines(string(b)) {
		_, p, ok := strings.Cut(strings.TrimSpace(line), " ")
		if !ok || slices.Contains(v.routePaths, p) {
			continue
		}
		q := param.ReplaceAllString(regexp.QuoteMeta(p), `[^/]+`)
		v.routes = append(v.routes, regexp.MustCompile(`^`+q+`/?$`))
		v.routePaths = append(v.routePaths, p)
	}
	if len(v.routePaths) < 30 {
		t.Fatalf("read %d routes from internal/server/routes.txt; the check no longer reads the route table", len(v.routePaths))
	}

	for _, n := range automation.Builtins() {
		v.builtins[n] = true
	}
	for n := range starlark.Universe {
		v.builtins[n] = true
	}
	return v
}

func loadBinary(t *testing.T, prog string, funcs []string) binary {
	b := binary{top: map[string]bool{}, words: map[string]bool{}, flags: map[string]bool{}}
	fset := token.NewFileSet()
	sources, err := filepath.Glob("../cmd/" + prog + "/*.go")
	if err != nil {
		t.Fatal(err)
	}
	usageWords := regexp.MustCompile(regexp.QuoteMeta(prog) + `((?:\s+[a-z<\[][a-z0-9_|<>\[\]-]*)+)`)
	usageFlags := regexp.MustCompile(`(?:^|[^\w-])--?([a-z][a-z0-9-]*)`)
	wordList := regexp.MustCompile(`^[a-z][a-z0-9_-]*(?: [a-z][a-z0-9_-]*)*$`)
	if prog == "aicrew-agent" {
		sources = append(sources, stepSource)
	}
	for _, p := range sources {
		if strings.HasSuffix(p, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, p, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		ast.Inspect(file, func(n ast.Node) bool {
			switch n := n.(type) {
			case *ast.FuncDecl:
				if n.Recv == nil && slices.Contains(funcs, n.Name.Name) {
					addDispatch(n, b.top)
				}
			case *ast.CallExpr:
				addFlagSetName(n, b.flags)
			case *ast.BasicLit:
				if n.Kind != token.STRING {
					return true
				}
				s, err := strconv.Unquote(n.Value)
				if err != nil {
					return true
				}
				if wordList.MatchString(s) {
					for w := range strings.FieldsSeq(s) {
						b.words[w] = true
					}
				}
				for _, m := range usageWords.FindAllStringSubmatch(s, -1) {
					for _, w := range strings.FieldsFunc(m[1], func(r rune) bool { return strings.ContainsRune(" \t\n|<>[]", r) }) {
						b.words[w] = true
					}
				}
				for _, m := range usageFlags.FindAllStringSubmatch(s, -1) {
					b.flags[m[1]] = true
				}
			}
			return true
		})
	}
	if len(b.top) == 0 {
		t.Fatalf("found no top-level command of %s in %v; the check no longer reads the dispatch", prog, funcs)
	}
	return b
}

// addDispatch records the strings a dispatch function compares the first
// command-line argument with: args[0] == "team", os.Args[1] != "guard".
func addDispatch(fn *ast.FuncDecl, top map[string]bool) {
	ast.Inspect(fn.Body, func(n ast.Node) bool {
		bin, ok := n.(*ast.BinaryExpr)
		if !ok || (bin.Op != token.EQL && bin.Op != token.NEQ) {
			return true
		}
		for _, pair := range [][2]ast.Expr{{bin.X, bin.Y}, {bin.Y, bin.X}} {
			lit, ok := pair[1].(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING || !firstArg(pair[0]) {
				continue
			}
			s, _ := strconv.Unquote(lit.Value)
			top[s] = true
		}
		return true
	})
}

// firstArg reports whether e is args[0] or os.Args[1].
func firstArg(e ast.Expr) bool {
	ix, ok := e.(*ast.IndexExpr)
	if !ok {
		return false
	}
	lit, ok := ix.Index.(*ast.BasicLit)
	if !ok || lit.Kind != token.INT {
		return false
	}
	switch x := ix.X.(type) {
	case *ast.Ident:
		return x.Name == "args" && lit.Value == "0"
	case *ast.SelectorExpr:
		pkg, ok := x.X.(*ast.Ident)
		return ok && pkg.Name == "os" && x.Sel.Name == "Args" && lit.Value == "1"
	}
	return false
}

// addFlagSetName records the name of a flag registered on a FlagSet:
// fs.String("name", ...), fs.BoolVar(&x, "name", ...) and the like.
func addFlagSetName(call *ast.CallExpr, flags map[string]bool) {
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok {
		return
	}
	pos := 0
	switch sel.Sel.Name {
	case "String", "Bool", "Int", "Int64", "Uint", "Uint64", "Float64", "Duration", "Func", "BoolFunc":
	case "StringVar", "BoolVar", "IntVar", "Int64Var", "UintVar", "Uint64Var", "Float64Var", "DurationVar", "Var", "TextVar":
		pos = 1
	default:
		return
	}
	if len(call.Args) <= pos {
		return
	}
	if lit, ok := call.Args[pos].(*ast.BasicLit); ok && lit.Kind == token.STRING {
		s, _ := strconv.Unquote(lit.Value)
		flags[s] = true
	}
}

func keys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}

// nearest names the closest current name by edit distance.
func nearest(s string, names []string) string {
	best, bestD := "", -1
	for _, n := range names {
		if d := distance(s, n); bestD < 0 || d < bestD {
			best, bestD = n, d
		}
	}
	return best
}

func distance(a, b string) int {
	prev := make([]int, len(b)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(a); i++ {
		cur := make([]int, len(b)+1)
		cur[0] = i
		for j := 1; j <= len(b); j++ {
			cost := 1
			if a[i-1] == b[j-1] {
				cost = 0
			}
			cur[j] = min(prev[j]+1, cur[j-1]+1, prev[j-1]+cost)
		}
		prev = cur
	}
	return prev[len(b)]
}

// TestDocExamplesContinuedLines pins the reading of the examples: a stale
// flag on a continuation line is refused at that line, for the shell's \
// and PowerShell's `; an invocation inside a $(...) substitution is read
// too; and so are a stale route of aicrewd and a stale builtin call, while
// aimem's routes are left alone.
func TestDocExamplesContinuedLines(t *testing.T) {
	v := loadVocabulary(t)
	body := "```sh\n" +
		"aicrew team create --name crew \\\n" +
		"  --teem x\n" +
		"```\n" +
		"```powershell\n" +
		"aicrew-agent check `\n" +
		"  --hoem x\n" +
		"```\n" +
		"```sh\n" +
		"aicrew status \\\n" +
		"  --team crew\n" +
		"ID=$(aicrew-agent attempts --hoem .)\n" +
		"```\n" +
		"See `/v1/crew/attempt/{id}/accept`, `/v1/tasks/{id}` and `/v1/crew/attempts/<id>/accept`.\n" +
		"Then `update_bse()` and `update_base()`.\n" +
		"Install it with `aicrew-agent service start\n" +
		"--hoem HOME` and check it with `aicrew-agent check --home HOME`.\n" +
		"An unclosed `aicrew-agent check\n" +
		"\n" +
		"--hoem` after a blank line is prose again.\n"
	got := checkDoc(doc{"EXAMPLE.md", body}, v)
	var lines []string
	for _, g := range got {
		at, _, _ := strings.Cut(g, ": ")
		lines = append(lines, at[strings.LastIndex(at, ":")+1:])
	}
	if strings.Join(lines, ",") != "3,7,12,14,15,17" {
		t.Fatalf("want problems at lines 3, 7, 12, 14, 15 and 17, got:\n%s", strings.Join(got, "\n"))
	}
	for i, want := range []string{`flag "--teem"`, `flag "--hoem"`, `flag "--hoem"`, `route "/v1/crew/attempt/{id}/accept"`, `builtin "update_bse"`, `flag "--hoem"`} {
		if !strings.Contains(got[i], want) {
			t.Errorf("problem %d: want %s, got %s", i, want, got[i])
		}
	}
}
