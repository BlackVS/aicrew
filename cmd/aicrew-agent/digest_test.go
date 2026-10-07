package main

import (
	"bytes"
	"context"
	"strings"
	"testing"
)

// The command takes the flags ROLES.md names, needs all three of the pin's
// fields, and never echoes a repository URL carrying a credential.
func TestDigestCommand(t *testing.T) {
	var out, errb bytes.Buffer
	noEnv := func(string) string { return "" }
	pin := []string{"--home", t.TempDir(), "--repository", "https://github.com/team/process",
		"--commit", strings.Repeat("a", 40), "--manifest", "processes/delivery.yaml"}
	if code := digest(context.Background(), pin, &out, &errb, noEnv); code != exitFailed ||
		!strings.Contains(errb.String(), "not an agent home") || out.Len() != 0 {
		t.Fatalf("a home with no agent.json: exit %d %q %q", code, out.String(), errb.String())
	}
	for _, drop := range []string{"--repository", "--commit", "--manifest"} {
		var args []string
		for i := 0; i < len(pin); i += 2 {
			if pin[i] != drop {
				args = append(args, pin[i], pin[i+1])
			}
		}
		if code := digest(context.Background(), args, &out, &errb, noEnv); code != exitUsage {
			t.Errorf("without %s: exit %d", drop, code)
		}
	}
	errb.Reset()
	args := []string{"--home", t.TempDir(), "--repository", "https://user:secret-in-url@github.com/team/process?x=1",
		"--commit", strings.Repeat("a", 40), "--manifest", "m.yaml"}
	if code := digest(context.Background(), args, &out, &errb, noEnv); code != exitFailed ||
		strings.Contains(out.String()+errb.String(), "secret-in-url") {
		t.Fatalf("exit %d: %q", code, errb.String())
	}
}
