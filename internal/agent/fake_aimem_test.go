package agent

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The test binary doubles as a fake aimem: run with AICREW_FAKE_AIMEM_ROOT
// set and aimem's arguments, it plays aimem's client commands against files
// under that root and records every call in calls.log.
func TestMain(m *testing.M) {
	if root := os.Getenv("AICREW_FAKE_AIMEM_ROOT"); root != "" && len(os.Args) > 1 &&
		(os.Args[1] == "identity" || os.Args[1] == "team-session") {
		os.Exit(fakeAimem(root, os.Args[1:]))
	}
	os.Exit(m.Run())
}

// fakeReceipt is the receipt the fake aimem issues for a challenge, which
// the test verifier accepts.
func fakeReceipt(challengeID string) string {
	sum := sha256.Sum256([]byte("receipt:" + challengeID))
	return "amr1_" + base64.RawURLEncoding.EncodeToString(sum[:])
}

type fakeCall struct {
	Args      []string  `json:"args"`
	StdinPipe bool      `json:"stdin_handle,omitempty"`
	Event     string    `json:"event"`
	At        time.Time `json:"at"`
}

func record(root string, c fakeCall) {
	c.At = time.Now()
	f, err := os.OpenFile(filepath.Join(root, "calls.log"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	defer f.Close()
	b, _ := json.Marshal(c)
	f.Write(append(b, '\n'))
}

func sessionFile(root, id string) string { return filepath.Join(root, "aicrew-sessions", id+".json") }

func flagValue(args []string, name string) string {
	for i := 0; i+1 < len(args); i++ {
		if args[i] == name {
			return args[i+1]
		}
	}
	return ""
}

func fakeAimem(root string, args []string) int {
	fail := func(msg string) int {
		fmt.Fprintln(os.Stderr, msg)
		return 1
	}
	readHandle := func() (string, bool) {
		b, _ := io.ReadAll(io.LimitReader(os.Stdin, 4096))
		h := strings.TrimSpace(string(b))
		return h, strings.HasPrefix(h, "acs1_") && len(h) == 48
	}
	switch strings.Join(args[:2], " ") {
	case "identity proof":
		record(root, fakeCall{Args: args, Event: "proof"})
		fi, err := os.Stdout.Stat()
		if err != nil || fi.Mode()&os.ModeNamedPipe == 0 {
			return fail("stdout is not a pipe")
		}
		fmt.Println(fakeReceipt(flagValue(args, "--challenge")))
		return 0
	case "team-session open":
		h, ok := readHandle()
		record(root, fakeCall{Args: args, StdinPipe: ok, Event: "open"})
		if !ok {
			return fail("stdin holds no handle")
		}
		if _, err := os.Stat(filepath.Join(root, "fail-open")); err == nil {
			return fail("the hub cannot be reached")
		}
		id := flagValue(args, "--session")
		path := sessionFile(root, id)
		if _, err := os.Stat(path); err == nil {
			return fail("team session " + id + " is already open")
		}
		os.MkdirAll(filepath.Dir(path), 0o700)
		b, _ := json.Marshal(map[string]string{"session": id, "handle": h, "service": flagValue(args, "--service"),
			"team": flagValue(args, "--team")})
		if err := os.WriteFile(path, b, 0o600); err != nil {
			return fail(err.Error())
		}
		fmt.Println(path)
		return 0
	case "team-session refresh":
		h, ok := readHandle()
		record(root, fakeCall{Args: args, StdinPipe: ok, Event: "refresh-start"})
		if !ok {
			return fail("stdin holds no handle")
		}
		path := sessionFile(root, args[2])
		raw, err := os.ReadFile(path)
		if err != nil {
			return fail("not open here")
		}
		var f map[string]string
		json.Unmarshal(raw, &f)
		f["handle"] = h
		b, _ := json.Marshal(f)
		os.WriteFile(path, b, 0o600)
		record(root, fakeCall{Args: args, Event: "refresh-end"})
		return 0
	case "team-session close":
		record(root, fakeCall{Args: args, Event: "close"})
		if _, err := os.Stat(filepath.Join(root, "fail-close")); err == nil {
			return fail("the hub could not confirm it has ended")
		}
		os.Remove(sessionFile(root, args[2]))
		return 0
	case "team-session status":
		record(root, fakeCall{Args: args, Event: "status"})
		path := sessionFile(root, args[2])
		if _, err := os.Stat(path); err != nil {
			return fail("not open here")
		}
		b, _ := json.Marshal(map[string]string{"path": path})
		fmt.Println(string(b))
		return 0
	}
	return fail("unknown command")
}

// fakeCalls reads the fake aimem's call log.
func fakeCalls(t *testing.T, root string) []fakeCall {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(root, "calls.log"))
	if err != nil {
		return nil
	}
	var calls []fakeCall
	for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		var c fakeCall
		if json.Unmarshal([]byte(line), &c) == nil {
			calls = append(calls, c)
		}
	}
	return calls
}

func jsonUnmarshal(b []byte, v any) error { return json.Unmarshal(b, v) }
