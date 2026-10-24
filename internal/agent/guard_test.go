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

// The guard refuses each rule in its plain form, with options before the
// subcommand, with flags reordered or combined, chained, and inside the
// shells and wrappers that run another command line; it admits each safe
// neighbour.
func TestGuardRefusal(t *testing.T) {
	refused := map[string][]string{
		"bash": {
			"git reset --hard",
			"git reset --hard HEAD~1",
			"git -C /work/repo reset --hard origin/main",
			"git --no-pager -c core.pager=cat reset --hard",
			"git --git-dir=.git --work-tree=. reset --hard",
			"git clean -f",
			"git clean -fd",
			"git clean -xdf",
			"git clean --force -d",
			"git push --force",
			"git push -f origin main",
			"git push -uf origin main",
			"git push --force-with-lease origin HEAD",
			"git push --force-with-lease=main:abc origin main",
			"git push origin --delete feature",
			"git push origin -d feature",
			"git push origin :feature",
			"git push origin +main",
			"git push --no-verify",
			"git branch -D feature",
			"git branch -df feature",
			"git branch --delete --force feature",
			"git checkout -- .",
			"git checkout .",
			"git checkout -- '*'",
			"git checkout -f main",
			"git restore .",
			"git restore :/",
			"git restore --staged --worktree .",
			"git stash drop",
			"git stash clear",
			"git commit --amend",
			"git commit --amend --no-edit",
			"git commit --no-verify -m wip",
			"git commit -n -m wip",
			"git commit -nm wip",
			"git commit -anm wip",
			"git filter-branch --tree-filter x HEAD",
			"git filter-repo --path x",
			"git update-ref -d refs/heads/x",
			"rm -rf work",
			"rm -fr work",
			"rm -r -f work",
			"rm -R work",
			"rm --recursive work",
			"/bin/rm -rf work",
			"sudo rm -rf /",
			"env FOO=1 rm -rf work",
			"FOO=1 git reset --hard",
			"find . -name '*.go' -delete",
			"find . | xargs rm -rf",
			"go test ./... && git reset --hard",
			"go build; rm -rf work",
			"false || git clean -fd",
			"echo x | git stash drop",
			"(cd repo && git reset --hard)",
			"echo $(git reset --hard)",
			"echo `git reset --hard`",
			"bash -c 'git reset --hard'",
			"sh -c \"rm -rf work\"",
			"bash -lc 'cd x; git push --force'",
			"eval 'git reset --hard'",
			"git status\ngit reset --hard",
			"powershell -Command \"Remove-Item -Recurse work\"",
			"powershell.exe -NoProfile -ExecutionPolicy Bypass -Command \"git reset --hard\"",
			"pwsh -c 'git clean -fd'",
			"powershell \"git push --force\"",
			"powershell -EncodedCommand ZwBpAHQA",
			"cmd /c rd /s /q work",
			"cmd.exe /c \"rmdir /S /Q work\"",
			"cmd /c del /s *.tmp",
			"mkfs.ext4 /dev/sdb1",
		},
		"powershell": {
			"Remove-Item -Recurse -Force work",
			"Remove-Item work -recurse",
			"Remove-Item -Path work -Recurse:$true",
			"remove-item work -r",
			"rm -r work",
			"rm work -Recurse",
			"ri -rec work",
			"del work -Recurse",
			"rmdir work -Recurse",
			"Get-ChildItem work | Remove-Item -Recurse",
			"git reset --hard; git status",
			"& git reset --hard",
			"& { git clean -fd }",
			"Invoke-Expression 'git reset --hard'",
			"iex \"git push -f\"",
			"cmd /c rd /s /q work",
			"Format-Volume -DriveLetter D",
			"format D:",
			"Set-Location C:\\work\\repo; git reset --hard",
		},
	}
	for shell, lines := range refused {
		for _, line := range lines {
			if GuardRefusal(shell, line) == "" {
				t.Errorf("%s: %q admitted", shell, line)
			}
		}
	}
	admitted := map[string][]string{
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
			"git branch feature",
			"git checkout -b feature",
			"git checkout main",
			"git checkout -- file.go",
			"git restore file.go",
			"git restore --staged .",
			"git stash",
			"git stash pop",
			"git stash list",
			"git commit -m 'fix the -n flag'",
			"git commit -am 'msg'",
			"git commit -m 'git reset --hard was the old way'",
			"git rm -r --cached build",
			"git format-patch -1",
			"rm file.go",
			"rm -f file.go",
			"rmdir empty",
			"echo 'rm -rf /'",
			"grep -r reset --hard-coded .",
			"go test ./... -run TestX",
			"find . -name '*.go'",
			"bash script.sh",
			"powershell -File build.ps1",
			"cmd /c dir",
			"",
		},
		"powershell": {
			"Remove-Item file.go",
			"Remove-Item -Path file.go -Force",
			"Get-ChildItem -Recurse work",
			"rm file.go",
			"del file.go",
			"git push -u origin feature",
			"Rename-Item a b",
			"Set-Location C:\\work\\repo",
			"Write-Output 'git reset --hard'",
		},
	}
	for shell, lines := range admitted {
		for _, line := range lines {
			if what := GuardRefusal(shell, line); what != "" {
				t.Errorf("%s: %q refused: %s", shell, line, what)
			}
		}
	}
}

// The guard answers Claude Code's PreToolUse protocol: a refused command
// gets the deny decision with a one-line reason naming the coordinator; an
// admitted one, another tool, and a malformed or empty payload get nothing,
// and every case exits 0.
func TestRunGuard(t *testing.T) {
	run := func(payload string) (string, int) {
		var out bytes.Buffer
		code := RunGuard(strings.NewReader(payload), &out)
		return out.String(), code
	}
	out, code := run(`{"hook_event_name":"PreToolUse","tool_name":"Bash","tool_input":{"command":"git -C . reset --hard HEAD"}}`)
	var d struct {
		HookSpecificOutput struct {
			HookEventName            string `json:"hookEventName"`
			PermissionDecision       string `json:"permissionDecision"`
			PermissionDecisionReason string `json:"permissionDecisionReason"`
		} `json:"hookSpecificOutput"`
	}
	if err := json.Unmarshal([]byte(out), &d); err != nil || code != 0 || d.HookSpecificOutput.HookEventName != "PreToolUse" ||
		d.HookSpecificOutput.PermissionDecision != "deny" ||
		!strings.Contains(d.HookSpecificOutput.PermissionDecisionReason, "git reset --hard") ||
		!strings.Contains(d.HookSpecificOutput.PermissionDecisionReason, "coordinator") ||
		strings.Contains(d.HookSpecificOutput.PermissionDecisionReason, "\n") {
		t.Fatalf("a refused command: %d %q", code, out)
	}
	if out, code := run(`{"tool_name":"PowerShell","tool_input":{"command":"Remove-Item -Recurse work"}}`); code != 0 ||
		!strings.Contains(out, `"deny"`) {
		t.Errorf("a refused PowerShell command: %d %q", code, out)
	}
	for _, p := range []string{
		`{"hook_event_name":"PreToolUse","tool_name":"Bash","tool_input":{"command":"git status"}}`,
		`{"hook_event_name":"PreToolUse","tool_name":"Write","tool_input":{"command":"git reset --hard"}}`,
		`{"hook_event_name":"PostToolUse","tool_name":"Bash","tool_input":{"command":"git reset --hard"}}`,
		`{"tool_name":"Bash","tool_input":{"command":42}}`,
		`not json`,
		``,
	} {
		if out, code := run(p); out != "" || code != 0 {
			t.Errorf("%q: %d %q", p, code, out)
		}
	}
}

// The managed settings install the guard as a PreToolUse hook for Bash and
// PowerShell in both wake modes, next to the Stop hook outside turns mode,
// and the hook's command, run on a payload, refuses a destructive command
// and admits a safe one. The settings are the same on a rewrite.
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
	writeAgentDoc(t, home, map[string]any{"wake": map[string]any{"mode": WakeTurns}})
	if pre, stop = read(); len(pre) != 1 || len(stop) != 0 {
		t.Fatalf("turns mode: %+v %+v", pre, stop)
	}
	// The command as Claude Code's shell runs it: the quoted binary, then
	// guard.
	words := splitCommands("bash", pre[0].Hooks[0].Command)
	if len(words) != 1 || len(words[0]) != 2 || words[0][1] != "guard" || filepath.Clean(words[0][0]) != filepath.Clean(exe) {
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
	if out := run("git -C . reset --hard HEAD"); !strings.Contains(out, `"permissionDecision":"deny"`) {
		t.Errorf("a destructive command: %q", out)
	}
	if out := run("git status"); out != "" {
		t.Errorf("a safe command: %q", out)
	}
}
