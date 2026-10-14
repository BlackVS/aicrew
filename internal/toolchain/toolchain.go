// Package toolchain holds the named development toolchains a worker's turn
// may run (pilot finding #18). The operator selects one per team and
// project on aicrewd (`aicrew team toolchain`); a worker's turn admits the
// selected lists' rules besides the common ones. aicrewd and aicrew-agent
// both validate a name against this table, so a list is defined once, in
// the product, and never hand-edited in a home.
package toolchain

import (
	"slices"
	"sort"
)

// lists are the toolchains by name: Claude Code allow rules for the Bash
// tool, run in the turn's worktree. Each command is admitted alone and
// with arguments. The home's deny rules apply above them.
var lists = map[string][]string{
	// dev-go: Go's test, vet, build and module commands, gofmt, and the
	// repository's own scripts.
	"dev-go": {
		"Bash(go test)", "Bash(go test *)",
		"Bash(go vet)", "Bash(go vet *)",
		"Bash(go build)", "Bash(go build *)",
		"Bash(go mod tidy)", "Bash(go mod tidy *)",
		"Bash(go mod download)", "Bash(go mod download *)",
		"Bash(gofmt *)",
		"Bash(bash scripts/*.sh)", "Bash(bash scripts/*.sh *)",
		"Bash(bash ./scripts/*.sh)", "Bash(bash ./scripts/*.sh *)",
	},
}

// Valid reports whether name is a known toolchain.
func Valid(name string) bool {
	_, ok := lists[name]
	return ok
}

// Rules is the named toolchain's allow rules, or nil for an unknown name.
func Rules(name string) []string { return slices.Clone(lists[name]) }

// Names are the known toolchains, sorted.
func Names() []string {
	names := make([]string, 0, len(lists))
	for n := range lists {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}
