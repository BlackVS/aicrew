package toolchain

import (
	"regexp"
	"slices"
	"strings"
	"testing"
)

// dev-go is the one list; every rule is a narrow Bash rule, a copy is the
// caller's to change, and an unknown name has no rules.
func TestLists(t *testing.T) {
	if !slices.Equal(Names(), []string{"dev-go"}) || !Valid("dev-go") || Valid("") || Valid("dev-cobol") || Rules("dev-cobol") != nil {
		t.Fatalf("names %q", Names())
	}
	narrow := regexp.MustCompile(`^Bash\((go (test|vet|build|mod tidy|mod download)|gofmt|bash (\./)?scripts/\*\.sh)( \*)?\)$`)
	rules := Rules("dev-go")
	for _, want := range []string{"Bash(go test *)", "Bash(go vet *)", "Bash(go build *)", "Bash(go mod tidy)",
		"Bash(go mod download *)", "Bash(gofmt *)", "Bash(bash scripts/*.sh *)"} {
		if !slices.Contains(rules, want) {
			t.Errorf("dev-go lacks %s", want)
		}
	}
	for _, r := range rules {
		if !narrow.MatchString(r) {
			t.Errorf("dev-go's rule %s is not a narrow toolchain rule", r)
		}
	}
	// Every list names its first tool and a test command its rules admit.
	for _, n := range Names() {
		w := strings.Fields(TestCommand(n))
		if len(w) < 2 || w[0] != Tool(n) || !slices.Contains(Rules(n), "Bash("+w[0]+" "+w[1]+" *)") {
			t.Errorf("toolchain %s: tool %q, test command %q", n, Tool(n), TestCommand(n))
		}
	}
	if Tool("dev-go") != "go" || TestCommand("dev-go") != "go test ./..." || Tool("dev-cobol") != "" || TestCommand("dev-cobol") != "" {
		t.Fatal("dev-go's tool or test command")
	}
	rules[0] = "Bash(*)"
	if Rules("dev-go")[0] == "Bash(*)" {
		t.Fatal("Rules shares the list")
	}
}
