package docsteps

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// A member records a document step only after its line ran: a missing or
// empty line file, or a line that fails, stops the run with nothing
// recorded for that step (e2e/pilotsim/member/doclines.sh).
func TestMemberDocLines(t *testing.T) {
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("no sh on PATH")
	}
	script, err := filepath.Abs("../pilotsim/member/doclines.sh")
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		name    string
		lines   map[string]string // id -> its line file's content; absent: no file
		wantErr bool
		ran     string
		out     string
	}{
		{"both lines", map[string]string{"a": "echo a >> out", "b": "echo b >> out"}, false, "a\nb\n", "a\nb\n"},
		{"a missing line file", map[string]string{"b": "echo b >> out"}, true, "", ""},
		{"an empty line file", map[string]string{"a": "", "b": "echo b >> out"}, true, "", ""},
		{"a failing line", map[string]string{"a": "exit 3", "b": "echo b >> out"}, true, "", ""},
		{"the second one missing", map[string]string{"a": "echo a >> out"}, true, "a\n", "a\n"},
	} {
		t.Run(c.name, func(t *testing.T) {
			dir := t.TempDir()
			for id, l := range c.lines {
				if err := os.WriteFile(filepath.Join(dir, id+".line"), []byte(l), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			cmd := exec.Command(sh, filepath.ToSlash(script), ".", "ran", "a", "b")
			cmd.Dir = dir
			out, err := cmd.CombinedOutput()
			if (err != nil) != c.wantErr {
				t.Fatalf("doclines.sh: %v, want error %v\n%s", err, c.wantErr, out)
			}
			for file, want := range map[string]string{"ran": c.ran, "out": c.out} {
				got, _ := os.ReadFile(filepath.Join(dir, file))
				if string(got) != want {
					t.Errorf("%s: %q, want %q", file, got, want)
				}
			}
		})
	}
}
