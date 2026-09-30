package agent

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The hidden prompt refuses anything that is not a terminal, so a code can
// never come from a pipe or a file.
func TestReadHiddenRefusesNonTerminals(t *testing.T) {
	f, err := os.Create(filepath.Join(t.TempDir(), "code"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	f.WriteString("ABCD-EFGH\n")
	f.Seek(0, io.SeekStart)
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	defer w.Close()
	w.WriteString("ABCD-EFGH\n")
	for name, in := range map[string]*os.File{"file": f, "pipe": r} {
		if IsTerminal(in) {
			t.Fatalf("%s: reported as a terminal", name)
		}
		var out strings.Builder
		line, err := ReadHidden(in, &out, "Invitation code: ")
		if !errors.Is(err, ErrNotTerminal) || line != "" || out.Len() != 0 {
			t.Fatalf("%s: %q, %v, prompt %q", name, line, err, out.String())
		}
	}
}

func TestReadLine(t *testing.T) {
	for in, want := range map[string]string{
		"abc\n":      "abc",
		"abc\r\n":    "abc",
		"abc\ndef\n": "abc",
		"abc":        "abc",
	} {
		got, err := readLine(strings.NewReader(in))
		if err != nil || got != want {
			t.Fatalf("%q: %q, %v", in, got, err)
		}
	}
	if _, err := readLine(strings.NewReader("")); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("empty input: %v", err)
	}
	if _, err := readLine(strings.NewReader(strings.Repeat("x", maxPromptLine+1) + "\n")); err == nil {
		t.Fatal("an overlong line was read")
	}
	if got, err := readLine(strings.NewReader(strings.Repeat("x", maxPromptLine) + "\n")); err != nil || len(got) != maxPromptLine {
		t.Fatalf("a line at the bound: %d, %v", len(got), err)
	}
	// Nothing past the line is consumed.
	r := strings.NewReader("one\ntwo\n")
	readLine(r)
	if rest, _ := io.ReadAll(r); string(rest) != "two\n" {
		t.Fatalf("left %q", rest)
	}
}
