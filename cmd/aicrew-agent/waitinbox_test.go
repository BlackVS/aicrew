package main

import (
	"bytes"
	"context"
	"strings"
	"testing"
)

// Without a launcher serving the home, the hook prints nothing and exits 0,
// so Claude Code lets the session stop; a bad flag does the same, never a
// failure that would wedge the client.
func TestWaitInboxWithoutALauncher(t *testing.T) {
	home := t.TempDir()
	for _, args := range [][]string{{"--home", home}, {"--bogus"}, {}} {
		var out, errb bytes.Buffer
		code := waitInbox(context.Background(), args, strings.NewReader(`{"hook_event_name":"Stop","stop_hook_active":false}`),
			&out, &errb, func(string) string { return "" })
		if code != 0 || out.Len() != 0 {
			t.Errorf("%v: exit %d, stdout %q", args, code, out.String())
		}
	}
}
