package managedfiles

import (
	"os"
	"path/filepath"
	"testing"
)

// One pass over every action: create (with its parent made), unchanged,
// update, conflict, kept and collision, and the digests Apply records.
func TestPlanAndApply(t *testing.T) {
	dir := t.TempDir()
	write := func(rel, s string) {
		os.MkdirAll(filepath.Dir(filepath.Join(dir, rel)), 0o755)
		os.WriteFile(filepath.Join(dir, rel), []byte(s), 0o644)
	}
	write("same.md", "v1\n")
	write("old.md", "v0\n")
	write("edited.md", "mine\n")
	write("owned.md", "notes\n")
	files := []File{
		{Path: "sub/new.md", Content: "new\n", Managed: true},
		{Path: "same.md", Content: "v1\n", Managed: true},
		{Path: "old.md", Content: "v1\n", Managed: true},
		{Path: "edited.md", Content: "v1\n", Managed: true},
		{Path: "owned.md", Content: "template\n"},
		{Path: "shadow.md", Content: "x\n", Managed: true, Collision: "your own shadow.md"},
	}
	rec := map[string]string{"old.md": Digest([]byte("v0\n")), "edited.md": Digest([]byte("v0\n"))}
	plan, err := Plan(dir, files, rec)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"create", "unchanged", "update", "conflict", "kept", "collision"}
	for i, c := range plan {
		if c.Action != want[i] || c.Path != files[i].Path {
			t.Fatalf("plan[%d] = %+v, want %s %s", i, c, files[i].Path, want[i])
		}
	}
	replaced, err := Apply(dir, files, plan, rec)
	if err != nil || !replaced {
		t.Fatalf("apply: %v %v", replaced, err)
	}
	for rel, w := range map[string]string{"sub/new.md": "new\n", "old.md": "v1\n", "edited.md": "mine\n",
		"edited.md.aicrew-new": "v1\n", "owned.md": "notes\n"} {
		if b, _ := os.ReadFile(filepath.Join(dir, rel)); string(b) != w {
			t.Errorf("%s = %q, want %q", rel, b, w)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "shadow.md")); !os.IsNotExist(err) {
		t.Error("a collision was written")
	}
	for p, d := range map[string]string{"sub/new.md": Digest([]byte("new\n")), "same.md": Digest([]byte("v1\n")),
		"old.md": Digest([]byte("v1\n")), "edited.md": Digest([]byte("v0\n"))} {
		if rec[p] != d {
			t.Errorf("record %s = %s, want %s", p, rec[p], d)
		}
	}
	if _, ok := rec["owned.md"]; ok {
		t.Error("an agent-owned file was recorded")
	}
	if _, ok := rec["shadow.md"]; ok {
		t.Error("a collision was recorded")
	}
}
