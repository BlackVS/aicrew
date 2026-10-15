package service

import (
	"strings"
	"testing"

	"golang.org/x/sys/windows"
)

// Windows reads each quoted argument back as it was.
func TestWinQuoteRoundTrip(t *testing.T) {
	args := []string{`C:\a b\c`, `C:\a b\`, `a"b`, `a\"b`, `x\\"y`, "plain", `C:\x\`, `tab	in`}
	var words []string
	for _, a := range args {
		words = append(words, winQuote(a))
	}
	got, err := windows.DecomposeCommandLine("prog " + strings.Join(words, " "))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(got[1:], "\n") != strings.Join(args, "\n") {
		t.Fatalf("read back %q, want %q", got[1:], args)
	}
}
