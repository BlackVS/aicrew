package main

import (
	"strings"
	"testing"
)

// automation takes only run, and run refuses a request that is not the
// launcher's.
func TestAutomationCmd(t *testing.T) {
	var out, errb strings.Builder
	if code := automationCmd([]string{"replay"}, strings.NewReader(""), &out, &errb); code != exitUsage {
		t.Errorf("an unknown verb: %d", code)
	}
	errb.Reset()
	if code := automationCmd([]string{"run"}, strings.NewReader("{}"), &out, &errb); code != exitUsage ||
		!strings.Contains(errb.String(), "the launcher starts this process") || out.Len() != 0 {
		t.Errorf("a request that is not the launcher's: %d %q %q", code, out.String(), errb.String())
	}
}
