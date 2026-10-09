// Package managedfiles writes a directory's managed files under the rerun
// rule of docs/WORKSPACE.md ("Managed files and repeated setup"): a missing
// file is created; a managed file is updated only while its content still
// matches the digest recorded at its last managed write, and otherwise the
// proposed version is written beside it as <file>.aicrew-new; an
// agent-owned file is created once and never changed; nothing else in the
// directory is touched. Agent homes (aicrew-agent join) and the architect
// directory (aicrew architect init) both use it.
package managedfiles

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
)

// File is a file the setup writes. A managed file follows the rerun rule;
// an agent-owned one is created once and never changed.
type File struct {
	Path    string // slash-separated, relative to the directory
	Content string
	Managed bool
	// Collision, when set, is what the file would shadow: it is never
	// written.
	Collision string
}

// Change is one planned or applied change to a file.
type Change struct {
	Path string `json:"path"`
	// Action is create, update, unchanged, conflict (the proposed version
	// is written beside the file as <path>.aicrew-new), kept (an
	// agent-owned file that exists) or collision (a managed file that
	// would shadow Detail; nothing is written).
	Action string `json:"action"`
	Detail string `json:"detail,omitempty"`
}

// Digest is the recorded form of a managed write's content.
func Digest(b []byte) string {
	sum := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// Plan decides each file's change against the recorded digests.
func Plan(dir string, files []File, recorded map[string]string) ([]Change, error) {
	var plan []Change
	for _, f := range files {
		if f.Collision != "" {
			plan = append(plan, Change{Path: f.Path, Action: "collision", Detail: f.Collision})
			continue
		}
		cur, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(f.Path)))
		switch {
		case errors.Is(err, os.ErrNotExist):
			plan = append(plan, Change{Path: f.Path, Action: "create"})
			continue
		case err != nil:
			return nil, err
		}
		switch {
		case !f.Managed:
			plan = append(plan, Change{Path: f.Path, Action: "kept"})
		case bytes.Equal(cur, []byte(f.Content)):
			plan = append(plan, Change{Path: f.Path, Action: "unchanged"})
		case recorded[f.Path] == Digest(cur):
			plan = append(plan, Change{Path: f.Path, Action: "update"})
		default:
			plan = append(plan, Change{Path: f.Path, Action: "conflict"})
		}
	}
	return plan, nil
}

// Apply carries out a plan and records the managed digests in rec. It
// returns whether a managed file's existing content was replaced.
func Apply(dir string, files []File, plan []Change, rec map[string]string) (bool, error) {
	byPath := map[string]File{}
	for _, f := range files {
		byPath[f.Path] = f
	}
	replaced := false
	for _, c := range plan {
		f := byPath[c.Path]
		path := filepath.Join(dir, filepath.FromSlash(f.Path))
		switch c.Action {
		case "create":
			if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
				return replaced, err
			}
			if err := createFile(path, f.Content); err != nil {
				return replaced, err
			}
		case "update":
			if err := WriteAtomic(path, []byte(f.Content)); err != nil {
				return replaced, err
			}
			replaced = true
		case "conflict":
			if err := WriteAtomic(path+".aicrew-new", []byte(f.Content)); err != nil {
				return replaced, err
			}
			continue // the recorded digest stays the last managed write's
		case "collision":
			continue // never written, never recorded
		}
		if f.Managed && c.Action != "kept" {
			rec[f.Path] = Digest([]byte(f.Content))
		}
	}
	return replaced, nil
}

// WriteAtomic replaces path's content through a temporary file in its
// directory and a rename.
func WriteAtomic(path string, data []byte) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+"-*")
	if err != nil {
		return err
	}
	_, werr := tmp.Write(data)
	cerr := tmp.Close()
	if werr != nil || cerr != nil {
		os.Remove(tmp.Name())
		return errors.Join(werr, cerr)
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		os.Remove(tmp.Name())
		return err
	}
	return nil
}

// createFile creates path, refusing to replace a file that appeared since
// the plan.
func createFile(path, content string) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return err
	}
	_, werr := f.WriteString(content)
	return errors.Join(werr, f.Close())
}
