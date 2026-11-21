package docsteps

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strings"
	"testing"
)

const docPath = "../../docs/BRING-UP.md"

// exempt are the marked steps no scenario runs, each with its reason. They
// are still parsed by TestLinesParse.
var exempt = map[string]string{
	"service-restart": "the simulation's member containers have no systemd user manager, so no service is " +
		"installed; the runner restarts serve as the installed service would (docs/E2E-PILOT-SIM.md)",
	"service-status": "the same: no systemd user manager in the simulation's member containers",
	"enable-linger":  "a systemd login setting of a real host, not an aicrew or aimem line",
	"architect-init-windows": "a Windows architect machine's PowerShell line; the simulation's containers are Linux " +
		"(tried by hand on Windows 11, docs/E2E-PILOT-SIM.md)",
	"architect-carry-windows": "the same: the Windows OpenSSH client's carrying, tried by hand",
	"session-leave": "named only in join's skills_conversation reason, which no scenario reaches: it needs the " +
		"hub to refuse a conversation for a session the home recorded earlier, a hub fault no harness injects",
}

// checkMarked: every command line of the document is a marked step or a
// repeat of one; a mention needs no marker (checkMentions reads its verbs).
func checkMarked(d *Doc) []error {
	var errs []error
	for _, c := range d.Commands {
		if c.ID == "" && !c.Mention {
			errs = append(errs, fmt.Errorf("line %d: %q has no run marker", c.At, c.Line))
		}
	}
	return errs
}

// checkMentions: a mention of aicrew or aicrew-agent names verbs the
// tool's usage names (aimem's usage is not in this tree).
func checkMentions(d *Doc, bins, home string) []error {
	var errs []error
	usage := map[string]string{}
	for _, c := range d.Commands {
		words := Split(c.Line)
		if !c.Mention || len(words) == 1 {
			continue
		}
		tool := path.Base(words[0])
		if tool == "aimem" {
			continue
		}
		// The usage of tool with args: the top level's, or a group's.
		usageOf := func(args []string) string {
			key := tool + " " + strings.Join(args, " ")
			if _, ok := usage[key]; !ok {
				cmd := exec.Command(filepath.Join(bins, tool+exe()), args...)
				cmd.Env = cleanEnv(home)
				cmd.Dir = home
				out, _ := cmd.CombinedOutput()
				usage[key] = string(out)
			}
			return usage[key]
		}
		if !verbsIn(usageOf(nil), tool, words[1:], usageOf, false) {
			errs = append(errs, fmt.Errorf("line %d: %q: no usage line of %s names %s", c.At, c.Line, tool,
				strings.Join(words[1:], " ")))
		}
	}
	return errs
}

// checkCovered: the scenarios' declared steps together are the marked steps
// less the exemptions, and no exempted step is declared.
func checkCovered(d *Doc, decl map[string][]string) []error {
	var errs []error
	ran := map[string]bool{}
	for name, ids := range decl {
		for _, id := range ids {
			if d.Steps[id] == nil {
				errs = append(errs, fmt.Errorf("scenario %s declares step %s, which the document does not mark", name, id))
			}
			if exempt[id] != "" {
				errs = append(errs, fmt.Errorf("scenario %s declares step %s, which is exempted", name, id))
			}
			ran[id] = true
		}
	}
	for id := range exempt {
		if d.Steps[id] == nil {
			errs = append(errs, fmt.Errorf("exempted step %s is not marked in the document", id))
		}
	}
	for id, s := range d.Steps {
		if !ran[id] && exempt[id] == "" {
			errs = append(errs, fmt.Errorf("line %d: step %s is run by no scenario and not exempted", s.At, id))
		}
	}
	return errs
}

// dummy fills a step's placeholders for the parse check.
func dummy(home string) map[string]string {
	return map[string]string{"AICREWD": "aicrewd.invalid", "PORT": "1", "HUB": "hub.invalid", "HOME": home,
		"HUB_URL": "https://hub.invalid:1", "FORGE_HOST": "forge.invalid", "PIN": "AAAA"}
}

// givesConnection says whether an aicrew line names its own connection
// (the architect's directory and its seat, which have no operator
// credential): parseLine's probe, which expects a line without one to stop
// for it, would then reach for the network or write the directory.
func givesConnection(words []string) bool {
	return slices.Contains(words, "--url") || slices.Contains(words, "-url")
}

// aicrewUsage checks such a line against its command's usage: the verbs
// name a command, and each flag is one it reads.
func aicrewUsage(bins, home, line string, words []string) error {
	var verbs, flags []string
	for _, w := range words[1:] {
		switch {
		case strings.HasPrefix(w, "-"):
			name, _, _ := strings.Cut(strings.TrimPrefix(w[1:], "-"), "=")
			flags = append(flags, name)
		case len(flags) == 0:
			verbs = append(verbs, w)
		}
	}
	cmd := exec.Command(filepath.Join(bins, "aicrew"+exe()), append(slices.Clone(verbs), "-h")...)
	cmd.Env = cleanEnv(home)
	cmd.Dir = home
	out, _ := cmd.CombinedOutput()
	head := "Usage of aicrew " + strings.Join(verbs, " ") + ":"
	if !strings.HasPrefix(string(out), head) {
		return fmt.Errorf("%q: aicrew names no command %s:\n%s", line, strings.Join(verbs, " "), out)
	}
	for _, f := range flags {
		if !regexp.MustCompile(`(?m)^  -` + regexp.QuoteMeta(f) + `(\s|$)`).Match(out) {
			return fmt.Errorf("%q: aicrew %s reads no flag --%s", line, strings.Join(verbs, " "), f)
		}
	}
	return nil
}

// parseLine checks one line against the product's own parser: an aicrew
// line that gives no connection runs with none configured and must stop at the
// connection, after its flags were taken; an aicrew-agent line's verbs and
// flags must be those its usage names.
func parseLine(bins, home, line string) error {
	words := Invocation(line)
	switch path.Base(words[0]) {
	case "aicrew":
		if givesConnection(words) {
			return aicrewUsage(bins, home, line, words)
		}
		cmd := exec.Command(filepath.Join(bins, "aicrew"+exe()), words[1:]...)
		cmd.Env = cleanEnv(home)
		cmd.Dir = home
		var errb bytes.Buffer
		cmd.Stderr = &errb
		err := cmd.Run()
		var ee *exec.ExitError
		if !errors.As(err, &ee) || ee.ExitCode() != 1 ||
			!strings.HasPrefix(errb.String(), "aicrew: name aicrewd and the operator credential") {
			return fmt.Errorf("%q does not parse: %v: %s", line, err, strings.TrimSpace(errb.String()))
		}
	case "aicrew-agent":
		var verbs, flags []string
		for _, w := range words[1:] {
			switch {
			case strings.HasPrefix(w, "-"):
				// Go's flag package reads -name and --name, no other
				// number of hyphens.
				name, _, _ := strings.Cut(strings.TrimPrefix(w[1:], "-"), "=")
				if name == "" || strings.HasPrefix(name, "-") {
					return fmt.Errorf("%q: %s is not a flag aicrew-agent reads", line, w)
				}
				flags = append(flags, name)
			case len(flags) == 0:
				verbs = append(verbs, w)
			}
		}
		cmd := exec.Command(filepath.Join(bins, "aicrew-agent"+exe()), append(slices.Clone(verbs), "-h")...)
		cmd.Env = cleanEnv(home)
		cmd.Dir = home
		out, _ := cmd.CombinedOutput()
		if !verbsIn(string(out), "aicrew-agent", verbs, nil, true) {
			return fmt.Errorf("%q: no usage line of aicrew-agent names %s:\n%s", line, strings.Join(verbs, " "), out)
		}
		for _, f := range flags {
			if !regexp.MustCompile(`(?m)(^|[\s\[(|,])--?` + regexp.QuoteMeta(f) + `($|[\s=\])|,])`).Match(out) {
				return fmt.Errorf("%q: aicrew-agent %s's usage has no flag --%s", line, strings.Join(verbs, " "), f)
			}
		}
	}
	return nil
}

// verbWord is a usage word of verbs: alternatives in lower case.
var verbWord = regexp.MustCompile(`^[a-z][a-z0-9-]*(\|[a-z][a-z0-9-]*)*$`)

// placeholderWord is a usage word that takes one positional word.
var placeholderWord = regexp.MustCompile(`^[A-Z][A-Z0-9_]*(\|[A-Z][A-Z0-9_]*)*$`)

// verbsIn says whether a usage line of tool names the words, in order:
// each word is one of the alternatives of the usage word at its place, or
// the value of a placeholder there (NAME). A flag or an optional part takes
// no positional word. A line that sends to the group's own usage with
// "..." is read on in group's text, the usage of tool with the group's
// name; group nil reads no further. complete asks for the whole verb
// sequence of a runnable line: no verb of the usage line may be left over.
func verbsIn(usage, tool string, words []string, group func(args []string) string, complete bool) bool {
	re := regexp.MustCompile(`(?m)^\s*(?:usage:\s+)?` + regexp.QuoteMeta(tool) + `\s+(.*)$`)
	for _, m := range re.FindAllStringSubmatch(usage, -1) {
		if lineNames(strings.Fields(m[1]), tool, words, group, complete) {
			return true
		}
	}
	return false
}

// lineNames is verbsIn for one usage line's words.
func lineNames(usage []string, tool string, words []string, group func(args []string) string, complete bool) bool {
	for i, w := range words {
		switch {
		case i >= len(usage):
			return false
		case usage[i] == "..." && i > 0 && group != nil:
			return verbsIn(group(words[:1]), tool, words, nil, complete)
		case verbWord.MatchString(usage[i]):
			if !slices.Contains(strings.Split(usage[i], "|"), w) {
				return false
			}
		case i > 0 && placeholderWord.MatchString(usage[i]):
		default:
			return false
		}
	}
	return !complete || len(usage) <= len(words) || !verbWord.MatchString(usage[len(words)])
}

func cleanEnv(home string) []string {
	var env []string
	for _, kv := range os.Environ() {
		if strings.HasPrefix(kv, "AICREW") || strings.HasPrefix(kv, "AIMEM") || strings.HasPrefix(kv, "HOME=") ||
			strings.HasPrefix(kv, "USERPROFILE=") {
			continue
		}
		env = append(env, kv)
	}
	return append(env, "HOME="+home, "USERPROFILE="+home)
}

func exe() string {
	if runtime.GOOS == "windows" {
		return ".exe"
	}
	return ""
}

// buildCLIs builds aicrew and aicrew-agent from this tree, once per run.
func buildCLIs(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	cmd := exec.Command("go", "build", "-o", dir+string(filepath.Separator), "../../cmd/aicrew", "../../cmd/aicrew-agent")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("go build: %v\n%s", err, out)
	}
	return dir
}

// checkParse parses every marked aicrew and aicrew-agent line, exempted
// ones included, with its additions and its alternative.
func checkParse(d *Doc, bins, home string) []error {
	var errs []error
	for id, s := range d.Steps {
		if inv := Invocation(s.Line); inv == nil || path.Base(inv[0]) == "aimem" {
			continue
		}
		variants := []Options{{Values: dummy(home)}}
		if len(s.adds) > 0 {
			variants = append(variants, Options{Values: dummy(home), Adds: s.adds})
		}
		if len(s.alts) > 0 {
			variants = append(variants, Options{Values: dummy(home), Alt: true})
		}
		for _, o := range variants {
			for p := range strings.FieldsSeq(strings.Join(d.Placeholders(id), " ")) {
				if _, ok := o.Values[p]; !ok {
					o.Values[p] = "x-" + strings.ToLower(p)
				}
			}
			line, err := d.Line(id, o)
			if err == nil {
				err = parseLine(bins, home, line)
			}
			if err != nil {
				errs = append(errs, fmt.Errorf("step %s (line %d): %w", id, s.At, err))
			}
		}
	}
	return errs
}

func report(t *testing.T, errs []error) {
	t.Helper()
	for _, e := range errs {
		t.Error(e)
	}
}

// The document's command lines are the scenarios' (task 01a1f9f5-a83e):
// every one is marked, the scenarios together run every marked step not
// exempted, and every aicrew and aicrew-agent line parses.
func TestDocumentSteps(t *testing.T) {
	d, err := Load(docPath)
	if err != nil {
		t.Fatal(err)
	}
	decl, err := LoadScenarios("scenarios.json")
	if err != nil {
		t.Fatal(err)
	}
	report(t, checkMarked(d))
	report(t, checkCovered(d, decl))
	bins, home := buildCLIs(t), t.TempDir()
	report(t, checkParse(d, bins, home))
	report(t, checkMentions(d, bins, home))
}

// A deliberately altered document fails the checks: a renamed verb, a
// renamed flag, an unmarked new line and a marker removed.
func TestDocumentStepsAltered(t *testing.T) {
	raw, err := os.ReadFile(docPath)
	if err != nil {
		t.Fatal(err)
	}
	decl, err := LoadScenarios("scenarios.json")
	if err != nil {
		t.Fatal(err)
	}
	bins, home := buildCLIs(t), t.TempDir()
	for _, c := range []struct{ name, old, new, want string }{
		{"a renamed operator verb", "aicrew team state --team-name crew\n", "aicrew team status --team-name crew\n", "step team-state"},
		{"a renamed operator flag", "--toolchain dev-go\n```", "--tool-chain dev-go\n```", "step team-toolchain"},
		{"a renamed member flag", "--client claude --wake turns", "--client claude --wake-mode turns", "step join"},
		{"a member verb that does not exist", "`aicrew-agent join --home HOME` <!-- run: join-rerun -->",
			"`aicrew-agent rejoin --home HOME` <!-- run: join-rerun -->", "step join-rerun"},
		{"an unmarked new line", "## The forge token's rights", "`aicrew team list --team-name crew`\n\n## The forge token's rights", "has no run marker"},
		{"a marker removed", "<!-- run: escalations-show -->\n", "", "has no run marker"},
		{"a mention of a verb that does not exist", "## The forge token's rights",
			"Then `aicrew team lists`.\n\n## The forge token's rights", "no usage line of aicrew names team lists"},
		{"a mention of a member verb that does not exist", "## The forge token's rights",
			"Then `aicrew-agent forge prs`.\n\n## The forge token's rights", "no usage line of aicrew-agent names forge prs"},
		{"an unmarked line after sudo", "## The forge token's rights",
			"Then `sudo -iu USER aicrew team list --team-name crew`.\n\n## The forge token's rights", "has no run marker"},
		{"an unmarked block line after an assignment", "## The forge token's rights",
			"```sh\nAICREW_X=1 aicrew team nosuch --flag v\n```\n\n## The forge token's rights", "has no run marker"},
		{"a marked line after sudo with a verb that does not exist", "## The forge token's rights",
			"<!-- run: extra -->\n```sh\nsudo -iu USER aicrew team nosuch --team-name crew\n```\n\n## The forge token's rights",
			"step extra (line"},
		{"an unmarked block line in a command substitution", "## The forge token's rights",
			"```sh\nstate=$(aicrew team state --team-name crew)\n```\n\n## The forge token's rights", "has no run marker"},
		{"an unmarked block line in a double-quoted command substitution", "## The forge token's rights",
			"```sh\nstate=\"$(aicrew team state --team-name crew)\"\n```\n\n## The forge token's rights", "has no run marker"},
		{"an unmarked block line in double-quoted backquotes", "## The forge token's rights",
			"```sh\nstate=\"`aicrew team state --team-name crew`\"\n```\n\n## The forge token's rights", "has no run marker"},
		{"a mention with a word where the usage takes only flags", "## The forge token's rights",
			"Then `aicrew status list`.\n\n## The forge token's rights", "no usage line of aicrew names status list"},
		{"a member line without its verb", "`aicrew-agent session leave --home HOME` <!-- run: session-leave -->",
			"`aicrew-agent session --home HOME` <!-- run: session-leave -->", "no usage line of aicrew-agent names session"},
		{"a mention of a third verb that does not exist", "## The forge token's rights",
			"Then `aicrew-agent forge pr nosuch`.\n\n## The forge token's rights", "no usage line of aicrew-agent names forge pr nosuch"},
		{"three hyphens on an exempted member line", "aicrew-agent service status --home HOME",
			"aicrew-agent service status ---home HOME", "---home is not a flag aicrew-agent reads"},
		{"a shortened member flag", "--tls-trust-mode ca_dns --tls-trust-value AICREWD --client claude",
			"--tls-trust ca_dns --tls-trust-value AICREWD --client claude", "usage has no flag --tls-trust"},
		{"an alternative with a flag the step lacks", "sha256-PIN` <!-- run-alt:", "sha256-PIN --typo value` <!-- run-alt:", "the step has no flag --typo"},
	} {
		t.Run(c.name, func(t *testing.T) {
			if !bytes.Contains(raw, []byte(c.old)) {
				t.Fatalf("the document has no %q", c.old)
			}
			d, err := Parse(bytes.Replace(raw, []byte(c.old), []byte(c.new), 1))
			if err != nil {
				t.Fatal(err)
			}
			errs := append(append(append(checkMarked(d), checkCovered(d, decl)...), checkParse(d, bins, home)...), checkMentions(d, bins, home)...)
			for _, e := range errs {
				if strings.Contains(e.Error(), c.want) {
					return
				}
			}
			t.Errorf("no failure names %q: %v", c.want, errs)
		})
	}
}

// Invocation finds the tool's command wherever the shell runs it, and
// keeps a quoted value whole for the parse.
func TestInvocation(t *testing.T) {
	for line, want := range map[string]string{
		`aicrew team pause --team-name crew --reason "a (b) c"`:              `aicrew team pause --team-name crew --reason 'a (b) c'`,
		`sudo -iu sessiond aicrew status`:                                    `aicrew status`,
		`state=$(aicrew team state --team-name crew)`:                        `aicrew team state --team-name crew`,
		`state="$(aicrew team state --team-name crew)"`:                      `aicrew team state --team-name crew`,
		"state=\"`aicrew team state --team-name crew`\"":                     `aicrew team state --team-name crew`,
		`uid="$(aimem access user-add --user-name crew-worker | jq -r .id)"`: `aimem access user-add --user-name crew-worker`,
		`echo 'aicrew is a name here' | wc -w`:                               ``,
		`loginctl enable-linger "$USER"`:                                     ``,
	} {
		if got := Join(Invocation(line)); got != want {
			t.Errorf("%s: %q, want %q", line, got, want)
		}
	}
}

// Line fills the placeholders, varies the per= word, adds only marked
// additions and puts the marked alternative in place of the flags.
func TestLine(t *testing.T) {
	d, err := Parse([]byte("<!-- run: join per=worker -->\n```sh\naicrew-agent join --label worker --url https://AICREWD:PORT \\\n" +
		"  --tls-trust-mode ca_dns --tls-trust-value AICREWD --cred FORGE_HOST=forge.token\n```\n\n" +
		"No terminal: `--code-file PATH` <!-- run-add: join -->. A pin: `--tls-trust-mode spki_sha256 --tls-trust-value sha256-PIN` <!-- run-alt: join -->.\n" +
		"Then `aicrew team pause --team-name crew --reason \"a REASON\"` <!-- run: pause -->, and `loginctl enable-linger \"$USER\"`.\n"))
	if err != nil {
		t.Fatal(err)
	}
	v := map[string]string{"AICREWD": "a.example", "PORT": "9443", "FORGE_HOST": "gitea:3000", "PATH": "/in/code", "PIN": "Zm9v"}
	for _, c := range []struct {
		name string
		id   string
		o    Options
		want string
	}{
		{"plain", "join", Options{Values: v},
			"aicrew-agent join --label worker --url https://a.example:9443 --tls-trust-mode ca_dns --tls-trust-value a.example --cred gitea:3000=forge.token"},
		{"per member", "join", Options{Values: v, Per: "coordinator"},
			"aicrew-agent join --label coordinator --url https://a.example:9443 --tls-trust-mode ca_dns --tls-trust-value a.example --cred gitea:3000=forge.token"},
		{"an addition", "join", Options{Values: v, Adds: []string{"--code-file PATH"}},
			"aicrew-agent join --label worker --url https://a.example:9443 --tls-trust-mode ca_dns --tls-trust-value a.example --cred gitea:3000=forge.token --code-file /in/code"},
		{"the alternative", "join", Options{Values: v, Alt: true},
			"aicrew-agent join --label worker --url https://a.example:9443 --tls-trust-mode spki_sha256 --tls-trust-value sha256-Zm9v --cred gitea:3000=forge.token"},
		{"quoted", "pause", Options{Values: map[string]string{"REASON": "look"}},
			"aicrew team pause --team-name crew --reason 'a look'"},
	} {
		got, err := d.Line(c.id, c.o)
		if err != nil || got != c.want {
			t.Errorf("%s: %q, %v", c.name, got, err)
		}
	}
	for name, o := range map[string]Options{
		"a missing value":         {Values: map[string]string{"AICREWD": "a"}},
		"an addition not marked":  {Values: v, Adds: []string{"--no-service"}},
		"a per word on no marker": {Values: map[string]string{"REASON": "x"}, Per: "y"},
	} {
		id := "join"
		if strings.Contains(name, "per word") {
			id = "pause"
		}
		if _, err := d.Line(id, o); err == nil {
			t.Errorf("%s: no error", name)
		}
	}
	if len(d.Commands) != 2 {
		t.Errorf("commands: %+v", d.Commands)
	}
}

// The document's skills import names the ai-skills release this tree is
// tested with, which the simulation imports.
func TestSkillsImportVersion(t *testing.T) {
	d, err := Load(docPath)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile("../../internal/agent/supported.json")
	if err != nil {
		t.Fatal(err)
	}
	var sup struct {
		Components map[string]struct {
			Ranges []struct{ Tested string } `json:"ranges"`
		} `json:"components"`
	}
	if err := json.Unmarshal(raw, &sup); err != nil {
		t.Fatal(err)
	}
	r := sup.Components["ai-skills"].Ranges
	want := "aiskills-" + r[len(r)-1].Tested + ".tar.gz"
	if s := d.Steps["skills-import"]; s == nil || !strings.HasSuffix(s.Line, " "+want) {
		t.Errorf("docs/BRING-UP.md's skills import does not name %s", want)
	}
}

// Join keeps the shell's own words of a document line as a person's shell
// reads them, and quotes every other word that is not plain.
func TestJoinShellWords(t *testing.T) {
	for _, c := range []struct {
		in   []string
		want string
	}{
		{[]string{"sudo", "install", "-o", "$USER", "~sessiond/aicrew/etc/a.token", "~/a.token", "&&", "sudo", "rm", "~sessiond/x"},
			`sudo install -o "$USER" ~sessiond/aicrew/etc/a.token ~/a.token && sudo rm ~sessiond/x`},
		{[]string{"test", "!", "-e", "~", "&&", "echo", "gone"}, `test '!' -e ~ && echo gone`},
		{[]string{"echo", "a b", "$user", "~/x y", "~root;rm", "&&&", "$(id)"},
			`echo 'a b' '$user' '~/x y' '~root;rm' '&&&' '$(id)'`},
	} {
		if got := Join(c.in); got != c.want {
			t.Errorf("Join(%q) = %s, want %s", c.in, got, c.want)
		}
	}
}
