package agent

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// testGuard is a guard in /work/repo whose repositories are on branch
// current, with main as the default branch; blind collects the commands it
// passes without a branch.
func testGuard(current string, blind *[]string) guardEnv {
	return guardEnv{root: "/work/repo", dir: "/work/repo", top: func(dir string) string {
		if dir == "/work/repo" || strings.HasPrefix(dir, "/work/repo/") {
			return "/work/repo"
		}
		return ""
	}, branches: func(string) (string, string) {
		if current == "" {
			return "", ""
		}
		return current, "main"
	}, blind: func(rule string, words []string, dir string) {
		if blind != nil {
			*blind = append(*blind, rule+" @ "+dir)
		}
	}}
}

// The guard refuses what reaches beyond the member's own work in every
// spelling: options before the subcommand, flags reordered or combined,
// chained, and inside the shells and wrappers that run another command
// line; it passes discarding work on the member's own branch and deleting
// inside its working directory.
func TestGuardRefusal(t *testing.T) {
	// Refused on any branch.
	always := map[string][]string{
		"bash": {
			"git push --force",
			"git push -f origin main",
			"git push -uf origin feature",
			"git push --force-with-lease origin HEAD",
			"git push --force-with-lease=main:abc origin main",
			"git push origin --delete feature",
			"git push origin -d feature",
			"git push origin :feature",
			"git push origin +feature",
			"git push --no-verify",
			"git -C /work/repo push --force",
			"git commit --no-verify -m wip",
			"git commit -n -m wip",
			"git commit -nm wip",
			"git commit -anm wip",
			"git branch -D main",
			"git branch -D master",
			"git branch --delete --force main",
			"git filter-branch --tree-filter x HEAD",
			"git filter-repo --path x",
			"git update-ref -d refs/heads/x",
			"rm -rf /",
			"rm -rf /etc",
			"rm -rf .",
			"rm -rf ./",
			"rm -rf ..",
			"rm -rf ../other",
			"rm -rf build/../..",
			"rm -rf *",
			"rm -rf .git",
			"rm -rf src/.git",
			"rm -rf ~/x",
			"rm -rf $HOME/x",
			"rm -rf \"$TARGET\"",
			"rm -fr /tmp/x",
			"rm -r -f /work/other",
			"rm -R /work",
			"rm --recursive /var",
			"/bin/rm -rf /",
			"sudo rm -rf /",
			"env FOO=1 rm -rf /",
			"env -u UNUSED git push --force",
			"env --unset UNUSED git push --force",
			"env -S 'git push --force'",
			"env -C /tmp rm -rf x",
			"sudo -u user git push --force",
			"sudo -u user -- git push -f",
			"sudo --user user git push --force",
			"sudo -D / rm -rf work",
			"timeout 10 git push --force",
			"timeout -s KILL 10 git push -f",
			"nice -n 5 git push --force",
			"xargs -I {} rm -rf {}",
			"rm -rf $(git rev-parse --show-toplevel)",
			"rm -rf build $(pwd)",
			"rm -rf x$(pwd)",
			"rm -rf `pwd`",
			"rm -rf build `pwd` dist",
			"rm $(pwd) -rf",
			"rm `pwd` -r",
			"rm build $(pwd) -rf",
			"rm \"$(pwd)\" -rf",
			"rm $(echo $(pwd)) -rf",
			"rm -rf .work/build-$(date +%s)",
			"find $(pwd) -delete",
			"find `pwd` -name '*.tmp' -delete",
			"echo $(git push --force)",
			"echo \"$(git push --force)\"",
			"echo $(echo $(git push --force))",
			"echo `git push --force` done",
			"echo $(a $(b $(c $(d $(e $(f $(g $(h $(i $(j))))))))))",
			"rm -rf",
			"cmd /c rd /s /q",
			"rm -rf build /etc",
			"rm -rf -- /etc",
			"find / -name '*.go' -delete",
			"find .. -delete",
			"find . -delete",
			"find -delete",
			"find . -name '*.tmp' -delete",
			"find /work/repo -type f -delete",
			"find . | xargs rm -rf",
			"cd .. && rm -rf repo",
			"cd $X; rm -rf build",
			"cd /; rm -rf work",
			"go test ./... && git push --force",
			"go build; rm -rf /etc",
			"false || git push -f",
			"(cd repo && git push --force)",
			"echo $(git push --force)",
			"echo `git push --force`",
			"bash -c 'git push --force'",
			"sh -c \"rm -rf /etc\"",
			"bash -lc 'cd x; git push --force'",
			"eval 'git push --force'",
			"git status\ngit push --force",
			"powershell -Command \"Remove-Item -Recurse C:\\Windows\"",
			"powershell.exe -NoProfile -ExecutionPolicy Bypass -Command \"git push --force\"",
			"pwsh -c 'git push -f'",
			"powershell \"git push --force\"",
			"powershell -EncodedCommand ZwBpAHQA",
			"cmd /c rd /s /q C:\\work",
			"cmd.exe /c \"rmdir /S /Q ..\\other\"",
			"cmd /c del /s C:\\*.tmp",
			"mkfs.ext4 /dev/sdb1",
		},
		"powershell": {
			"Remove-Item -Recurse -Force C:\\work",
			"Remove-Item ..\\other -recurse",
			"Remove-Item -Path C:\\Users -Recurse:$true",
			"remove-item $env:TEMP -r",
			"rm -r ~\\x",
			"rm C:\\work -Recurse",
			"ri -rec ..",
			"del . -Recurse",
			"rmdir C:\\x -Recurse",
			"Remove-Item -Recurse",
			"Get-ChildItem work | Remove-Item -Recurse",
			"Remove-Item -Recurse $(Get-Location)",
			"Remove-Item $(Get-Location) -Recurse",
			"Remove-Item $(Resolve-Path ../other) -Recurse -Force",
			"Write-Output \"$(git push --force)\"",
			"git push --force; git status",
			"& git push --force",
			"& { git push -f }",
			"Invoke-Expression 'git push --force'",
			"iex \"git push -f\"",
			"cmd /c rd /s /q C:\\work",
			"Format-Volume -DriveLetter D",
			"format D:",
			"Set-Location C:\\; Remove-Item -Recurse work",
		},
	}
	// Refused on main, the default branch; passed on the member's own.
	shared := map[string][]string{
		"bash": {
			"git reset --hard",
			"git reset --hard HEAD~1",
			"git -C /work/repo reset --hard origin/main",
			"git --no-pager -c core.pager=cat reset --hard",
			"git clean -f",
			"git clean -fd",
			"git clean -xdf",
			"git clean --force -d",
			"git checkout -- .",
			"git checkout .",
			"git checkout -- '*'",
			"git checkout -f feature",
			"git restore .",
			"git restore :/",
			"git restore --staged --worktree .",
			"git stash drop",
			"git stash clear",
			"git commit --amend",
			"git commit --amend --no-edit",
			"FOO=1 git reset --hard",
			"go test ./... && git reset --hard",
			"bash -c 'git reset --hard'",
			"cmd /c git clean -fd",
			"powershell -Command \"git reset --hard\"",
		},
		"powershell": {
			"git reset --hard; git status",
			"Set-Location C:\\work\\repo; git reset --hard",
		},
	}
	// Passed on any branch.
	passed := map[string][]string{
		"bash": {
			"git status",
			"git reset --soft HEAD~1",
			"git reset HEAD file.go",
			"git reset --mixed",
			"git clean -n",
			"git clean -fn",
			"git clean --dry-run -f",
			"git push",
			"git push -u origin feature",
			"git push origin HEAD:refs/heads/feature",
			"git branch -d feature",
			"git branch -D feature",
			"git branch feature",
			"git checkout -b feature",
			"git checkout main",
			"git checkout -- file.go",
			"git restore file.go",
			"git restore --staged .",
			"git stash",
			"git stash pop",
			"git commit -m 'fix the -n flag'",
			"git commit -am 'msg'",
			"git commit -m 'git push --force was the old way'",
			"git rm -r --cached build",
			"git format-patch -1",
			"rm file.go",
			"rm -f file.go",
			"rm -rf .work/build",
			"rm -rf build",
			"rm -r -f ./out dist",
			"rm -rf build/*",
			"rm -rf /work/repo/.work/x",
			"rm -rf -- -odd-name",
			"cd .work && rm -rf build",
			"cd /work/repo/.work; rm -rf x",
			"find build -name '*.tmp' -delete",
			"find build -delete",
			"env -C .work rm -rf build",
			"sudo -u user git status",
			"timeout 60 go test ./...",
			"rmdir empty",
			"rm -rf build && echo $(date)",
			"echo `rm -rf .work/build`",
			"git commit -m \"$(cat msg)\"",
			"echo $(a $(b))",
			"echo \"$(git log -1 --format='%H (x)')\" && rm -rf .work/build",
			"echo '$(git push --force)'",
			"echo \\$HOME",
			"echo 'rm -rf /'",
			"grep -r reset --hard-coded .",
			"go test ./... -run TestX",
			"bash script.sh",
			"powershell -File build.ps1",
			"cmd /c dir",
			"cmd /c rd /s /q .work\\build",
			"",
		},
		"powershell": {
			"Remove-Item file.go",
			"Remove-Item -Path file.go -Force",
			"Remove-Item -Recurse -Force .work\\build",
			"Remove-Item -Path .work\\build -Recurse",
			"Remove-Item -LiteralPath:build -Recurse",
			"Remove-Item build,dist -Recurse",
			"Remove-Item C:\\x -Recurse:$false",
			"Remove-Item -Recurse build -Filter *.tmp",
			"Get-ChildItem -Recurse work",
			"rm file.go",
			"del file.go",
			"git push -u origin feature",
			"Rename-Item a b",
			"Set-Location C:\\work\\repo",
			"Write-Output 'git push --force'",
		},
	}
	own, onMain := testGuard("feature", nil), testGuard("main", nil)
	for shell, lines := range always {
		for _, line := range lines {
			if r := own.refusal(shell, line); r.what == "" || r.next == "" {
				t.Errorf("%s: %q passed on the own branch", shell, line)
			}
		}
	}
	for shell, lines := range shared {
		for _, line := range lines {
			if r := onMain.refusal(shell, line); r.what == "" || !strings.Contains(r.what, "on main") {
				t.Errorf("%s: %q on main: %q", shell, line, r.what)
			}
			if r := own.refusal(shell, line); r.what != "" {
				t.Errorf("%s: %q refused on the own branch: %s", shell, line, r.what)
			}
		}
	}
	for shell, lines := range passed {
		for _, line := range lines {
			for _, g := range []guardEnv{own, onMain} {
				if r := g.refusal(shell, line); r.what != "" {
					t.Errorf("%s: %q refused: %s", shell, line, r.what)
				}
			}
		}
	}
}

// A branch switched to earlier in the call is the branch a later command
// discards work on, in both directions; git checkout of a name that may be
// a path leaves the branch as it was.
func TestGuardBranchTransitions(t *testing.T) {
	cases := []struct {
		current, line string
		refused       bool
	}{
		{"feature", "git switch main && git reset --hard HEAD~1", true},
		{"feature", "git checkout main; git clean -fd", true},
		{"feature", "git checkout master && git reset --hard", true},
		{"feature", "git -C /work/repo switch main && git -C . reset --hard", true},
		{"feature", "cd sub && git switch main && cd .. && git reset --hard HEAD~1", true},
		{"feature", "git -C sub switch main && git reset --hard", true},
		{"main", "cd sub && git switch -c task && cd .. && git reset --hard", false},
		{"feature", "git -C /work/other switch main && git reset --hard", false},
		{"main", "git switch -c task && git reset --hard", false},
		{"main", "git checkout -b task && git reset --hard", false},
		{"main", "git switch feature && git commit --amend", false},
		{"main", "git switch -c task main && git clean -fd", false},
		{"main", "git checkout file.go && git reset --hard", true},
		{"main", "git checkout -- main && git reset --hard", true},
		{"main", "git switch --detach HEAD~1 && git reset --hard", false},
	}
	for _, c := range cases {
		if r := testGuard(c.current, nil).refusal("bash", c.line); (r.what != "") != c.refused {
			t.Errorf("on %s: %q: refused %q, want refused %v", c.current, c.line, r.what, c.refused)
		}
	}
	var blind []string
	if r := testGuard("main", &blind).refusal("bash", "git switch - && git reset --hard"); r.what != "" || len(blind) != 1 {
		t.Errorf("an unknown switch: %q %q", r.what, blind)
	}
}

// worktreeTop finds the worktree holding a directory: the nearest one with
// a .git entry, a directory or a linked worktree's file.
func TestWorktreeTop(t *testing.T) {
	root := t.TempDir()
	repo := filepath.Join(root, "repo")
	linked := filepath.Join(root, "linked")
	for _, d := range []string{filepath.Join(repo, ".git"), filepath.Join(repo, "sub", "deep"), linked} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(linked, ".git"), []byte("gitdir: x"), 0o600); err != nil {
		t.Fatal(err)
	}
	for dir, want := range map[string]string{
		repo: repo, filepath.Join(repo, "sub", "deep"): repo, linked: linked, filepath.Join(linked, "x"): linked,
	} {
		if got := worktreeTop(dir); got != filepath.ToSlash(want) {
			t.Errorf("%s: %q, want %q", dir, got, want)
		}
	}
}

// A command that discards work where the branch cannot be read passes,
// and is recorded; one the guard refuses on any branch is still refused.
func TestGuardBlind(t *testing.T) {
	var blind []string
	g := testGuard("", &blind)
	if r := g.refusal("bash", "git -C sub reset --hard && git --git-dir=x clean -fd"); r.what != "" {
		t.Fatalf("refused: %s", r.what)
	}
	if len(blind) != 2 || blind[0] != "git reset --hard @ /work/repo/sub" || blind[1] != "git clean --force @ " {
		t.Fatalf("recorded: %q", blind)
	}
	if r := g.refusal("bash", "git push --force"); r.what == "" {
		t.Fatal("a forced push passed")
	}
}

// The guard answers Claude Code's PreToolUse protocol: a refused command
// gets the deny decision with a one-line reason that names what was
// refused, what to do instead and the coordinator; a passed one, another
// tool, and a malformed or empty payload get nothing, and every case exits
// 0. A command passed without a branch is recorded in the home's log.
func TestRunGuard(t *testing.T) {
	home := t.TempDir()
	notRepo := t.TempDir()
	run := func(payload string) (string, int) {
		var out bytes.Buffer
		code := RunGuard(strings.NewReader(payload), &out, home)
		return out.String(), code
	}
	out, code := run(`{"hook_event_name":"PreToolUse","tool_name":"Bash","tool_input":{"command":"git -C . push --force"}}`)
	var d struct {
		HookSpecificOutput struct {
			HookEventName            string `json:"hookEventName"`
			PermissionDecision       string `json:"permissionDecision"`
			PermissionDecisionReason string `json:"permissionDecisionReason"`
		} `json:"hookSpecificOutput"`
	}
	reason := &d.HookSpecificOutput.PermissionDecisionReason
	if err := json.Unmarshal([]byte(out), &d); err != nil || code != 0 || d.HookSpecificOutput.HookEventName != "PreToolUse" ||
		d.HookSpecificOutput.PermissionDecision != "deny" || !strings.Contains(*reason, "git push --force") ||
		!strings.Contains(*reason, nextNoForce) || !strings.Contains(*reason, "coordinator") || strings.Contains(*reason, "\n") {
		t.Fatalf("a refused command: %d %q", code, out)
	}
	if out, code := run(`{"tool_name":"PowerShell","tool_input":{"command":"Remove-Item -Recurse C:\\Windows"}}`); code != 0 ||
		!strings.Contains(out, `"deny"`) {
		t.Errorf("a refused PowerShell command: %d %q", code, out)
	}
	blind, _ := json.Marshal(map[string]any{"tool_name": "Bash", "cwd": notRepo,
		"tool_input": map[string]string{"command": "git reset --hard"}})
	for _, p := range []string{
		`{"hook_event_name":"PreToolUse","tool_name":"Bash","tool_input":{"command":"git status"}}`,
		`{"hook_event_name":"PreToolUse","tool_name":"Write","tool_input":{"command":"git push --force"}}`,
		`{"hook_event_name":"PostToolUse","tool_name":"Bash","tool_input":{"command":"git push --force"}}`,
		`{"tool_name":"Bash","tool_input":{"command":42}}`,
		`not json`,
		``,
		string(blind),
	} {
		if out, code := run(p); out != "" || code != 0 {
			t.Errorf("%q: %d %q", p, code, out)
		}
	}
	raw, err := os.ReadFile(filepath.Join(home, filepath.FromSlash(GuardLogFile)))
	var line guardBlind
	if err != nil || json.Unmarshal(raw, &line) != nil || line.Rule != "git reset --hard" ||
		line.Command != "git reset --hard" || line.Time == "" {
		t.Fatalf("the blind log: %v %q", err, raw)
	}
}

// The guard reads the branch from the real repository at the payload's
// working directory: discarding work is refused on main and passed on the
// member's own branch.
func TestRunGuardReadsTheBranch(t *testing.T) {
	repo := t.TempDir()
	git := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", repo}, args...)...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
	git("init", "-q", "-b", "main")
	if current, _ := gitBranches(repo); current != "main" {
		t.Fatalf("the branch: %q", current)
	}
	payload, _ := json.Marshal(map[string]any{"tool_name": "Bash", "cwd": repo,
		"tool_input": map[string]string{"command": "git reset --hard"}})
	home := t.TempDir()
	var out bytes.Buffer
	RunGuard(bytes.NewReader(payload), &out, home)
	if !strings.Contains(out.String(), "git reset --hard on main") {
		t.Fatalf("on main: %q", out.String())
	}
	git("switch", "-q", "-c", "feature")
	out.Reset()
	RunGuard(bytes.NewReader(payload), &out, home)
	if out.String() != "" {
		t.Fatalf("on the own branch: %q", out.String())
	}
	if _, err := os.Stat(filepath.Join(home, filepath.FromSlash(GuardLogFile))); !os.IsNotExist(err) {
		t.Fatalf("a known branch was recorded as blind: %v", err)
	}
}

// The managed settings install the guard for the home as a PreToolUse hook
// for Bash and PowerShell in both wake modes, next to the Stop hook outside
// turns mode, and leave it out when agent.json turns it off; the hook's
// command, run on a payload, refuses a destructive command and passes a
// safe one. The settings are the same on a rewrite.
func TestGuardHookRuns(t *testing.T) {
	home := t.TempDir()
	old := selfExecutable
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	selfExecutable = func() string { return exe }
	t.Cleanup(func() { selfExecutable = old })
	type hook struct {
		Matcher string `json:"matcher"`
		Hooks   []struct {
			Type    string `json:"type"`
			Command string `json:"command"`
			Timeout int    `json:"timeout"`
		} `json:"hooks"`
	}
	read := func() (pre, stop []hook) {
		t.Helper()
		var doc struct {
			Hooks struct {
				PreToolUse []hook `json:"PreToolUse"`
				Stop       []hook `json:"Stop"`
			} `json:"hooks"`
		}
		if err := json.Unmarshal([]byte(claudeSettings(home)), &doc); err != nil {
			t.Fatal(err)
		}
		if claudeSettings(home) != claudeSettings(home) {
			t.Fatal("the settings differ on a rewrite")
		}
		return doc.Hooks.PreToolUse, doc.Hooks.Stop
	}
	pre, stop := read()
	if len(pre) != 1 || len(stop) != 1 || pre[0].Matcher != "Bash|PowerShell" || len(pre[0].Hooks) != 1 ||
		pre[0].Hooks[0].Type != "command" || pre[0].Hooks[0].Timeout != 10 {
		t.Fatalf("stop-hook mode: %+v %+v", pre, stop)
	}
	writeAgentDoc(t, home, map[string]any{"guard": map[string]any{"off": true}})
	if pre, stop := read(); len(pre) != 0 || len(stop) != 1 {
		t.Fatalf("the guard off: %+v %+v", pre, stop)
	}
	writeAgentDoc(t, home, map[string]any{"wake": map[string]any{"mode": WakeTurns}})
	if pre, stop = read(); len(pre) != 1 || len(stop) != 0 {
		t.Fatalf("turns mode: %+v %+v", pre, stop)
	}
	// The command as Claude Code's shell runs it: the quoted binary, guard
	// and the home.
	words := splitCommands("bash", pre[0].Hooks[0].Command)
	if len(words) != 1 || len(words[0]) != 4 || words[0][1] != "guard" || words[0][2] != "--home" ||
		filepath.Clean(words[0][0]) != filepath.Clean(exe) || filepath.Clean(words[0][3]) != filepath.Clean(home) {
		t.Fatalf("the hook runs %q", pre[0].Hooks[0].Command)
	}
	run := func(command string) string {
		t.Helper()
		payload, _ := json.Marshal(map[string]any{"hook_event_name": "PreToolUse", "tool_name": "Bash",
			"tool_input": map[string]string{"command": command}})
		cmd := exec.Command(words[0][0], words[0][1:]...)
		cmd.Stdin = bytes.NewReader(payload)
		out, err := cmd.Output()
		if err != nil {
			t.Fatalf("the hook on %q: %v", command, err)
		}
		return string(out)
	}
	if out := run("git -C . push --force"); !strings.Contains(out, `"permissionDecision":"deny"`) {
		t.Errorf("a destructive command: %q", out)
	}
	if out := run("git status"); out != "" {
		t.Errorf("a safe command: %q", out)
	}
}
