package automation

import (
	"errors"
	"testing"
)

// The matcher reads Claude Code's Bash rules as the turn's client does: "*"
// matches any text, "PREFIX:*" matches the prefix alone or followed by
// more, deny wins, and rules for other tools are ignored.
func TestAllowList(t *testing.T) {
	l := NewAllowList([]string{
		"Bash(go test)", "Bash(go test *)", "Bash(gofmt *)", "Bash(aimem process show*)", "Bash(npm run test:*)",
		"Read(./**)", "Edit(./worktrees/**)", "mcp__aimem__get_task",
	}, []string{"Bash(*config --global*)", "PowerShell(*creds/*)"})
	for _, c := range []struct {
		cmd    string
		args   []string
		admits bool
	}{
		{"go", []string{"test"}, true},
		{"go", []string{"test", "./..."}, true},
		{"go", []string{"testx"}, false},
		{"go", []string{"build"}, false},
		{"gofmt", []string{"-l", "."}, true},
		{"gofmt", nil, false},
		{"aimem", []string{"process", "show"}, true},
		{"npm", []string{"run", "test"}, true},
		{"npm", []string{"run", "test", "--watch"}, true},
		{"npm", []string{"run", "tests"}, false},
		{"go", []string{"test", "x", "config", "--global"}, false},
		{"go", []string{"test", "a b"}, true},
		{"cat", []string{"README.md"}, false},
	} {
		err := l.Admit(c.cmd, c.args)
		if c.admits != (err == nil) {
			t.Errorf("%s: %v", CommandLine(c.cmd, c.args), err)
		}
		if err != nil && !errors.Is(err, ErrRefused) {
			t.Errorf("%s: %v is not a refusal", CommandLine(c.cmd, c.args), err)
		}
	}
	// An argument that spans lines is matched by both rule forms: a legacy
	// deny still wins, and a legacy allow still admits (hands review of
	// #166).
	multi := []string{"commit", "-m", "subject\n\nbody"}
	legacyDeny := NewAllowList([]string{"Bash(git *)"}, []string{"Bash(git commit:*)"})
	if err := legacyDeny.Admit("git", multi); !errors.Is(err, ErrRefused) {
		t.Errorf("a legacy deny over a multi-line argument: %v", err)
	}
	if err := NewAllowList([]string{"Bash(git commit:*)"}, nil).Admit("git", multi); err != nil {
		t.Errorf("a legacy allow over a multi-line argument: %v", err)
	}
	if err := NewAllowList([]string{"Bash(git *)"}, []string{"Bash(*--no-verify*)"}).Admit("git",
		[]string{"commit", "-m", "a\nb", "--no-verify"}); !errors.Is(err, ErrRefused) {
		t.Errorf("a wildcard deny after a multi-line argument: %v", err)
	}
	if (AllowList{}).Admit("git", []string{"status"}) == nil {
		t.Error("an empty list admitted a command")
	}
	if got := CommandLine("git", []string{"commit", "-m", "it's done", "a;b"}); got != `git commit -m 'it'\''s done' 'a;b'` {
		t.Errorf("the command line: %s", got)
	}
}
