package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/BlackVS/aicrew/internal/store"
)

var codeShape = regexp.MustCompile(`[0-9A-Z]{4}(-[0-9A-Z]{4}){6}`)

// invitationStore is a store with one team; the CLI opens it afterwards.
func invitationStore(t *testing.T) (path, teamID string) {
	t.Helper()
	path = filepath.Join(t.TempDir(), "aicrew.db")
	st, err := store.Open(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	op, err := store.OperatorCaller("op-test")
	if err != nil {
		t.Fatal(err)
	}
	tm, err := st.CreateTeam(context.Background(), op, "team", store.NewTeam{Name: "crew"})
	if err != nil {
		t.Fatal(err)
	}
	return path, tm.ID
}

func onTerminal(t *testing.T, yes bool) {
	t.Helper()
	old := isTerminal
	isTerminal = func(io.Writer) bool { return yes }
	t.Cleanup(func() { isTerminal = old })
}

func listInvitations(t *testing.T, storePath string, extra ...string) []invitationView {
	t.Helper()
	r := cli(t, append([]string{"invitation", "list", "-store", storePath}, extra...)...)
	var out []invitationView
	if r.code != 0 || json.Unmarshal([]byte(r.stdout), &out) != nil {
		t.Fatalf("list: %d %s %s", r.code, r.stdout, r.stderr)
	}
	return out
}

// redeemable reports whether code begins a redemption: it is the code of a
// live invitation in the store.
func redeemable(t *testing.T, storePath, code string) bool {
	t.Helper()
	st, err := store.Open(context.Background(), storePath)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	_, err = st.BeginRedemption(context.Background(), "probe-"+code[:4], store.NewSecret(code))
	return err == nil
}

// Issue prints the code once, only on a terminal, after the metadata; the
// code redeems; the listing shows no code; a join without -expect-user warns.
func TestInvitationIssueOnTerminal(t *testing.T) {
	storePath, team := invitationStore(t)
	onTerminal(t, true)
	r := cli(t, "invitation", "issue", "-store", storePath, "-team", team, "-role", "worker", "-hub", "hub-a", "-label", "builder")
	if r.code != 0 {
		t.Fatalf("issue: %d %s", r.code, r.stderr)
	}
	codes := codeShape.FindAllString(r.stdout, -1)
	if len(codes) != 1 || strings.Contains(r.stderr, codes[0]) {
		t.Fatalf("the code is not shown exactly once on stdout: %q / %q", r.stdout, r.stderr)
	}
	if !strings.Contains(r.stderr, "warning: no -expect-user") {
		t.Fatalf("no warning without -expect-user: %q", r.stderr)
	}
	var v invitationView
	if err := json.NewDecoder(strings.NewReader(r.stdout)).Decode(&v); err != nil || v.State != "issued" || v.TeamID != team ||
		v.Role != "worker" || v.Purpose != "join" || v.HubID != "hub-a" || v.Expired {
		t.Fatalf("metadata: %+v %v", v, err)
	}
	listed := listInvitations(t, storePath)
	raw, _ := json.Marshal(listed)
	if len(listed) != 1 || listed[0].ID != v.ID || strings.Contains(string(raw), codes[0]) || strings.Contains(string(raw), "digest") {
		t.Fatalf("listing: %s", raw)
	}
	if !redeemable(t, storePath, codes[0]) {
		t.Fatal("the printed code does not redeem")
	}
	// With -expect-user there is no warning.
	r = cli(t, "invitation", "issue", "-store", storePath, "-team", team, "-role", "worker", "-hub", "hub-a", "-label", "b2",
		"-expect-user", "user-9")
	if r.code != 0 || strings.Contains(r.stderr, "warning") {
		t.Fatalf("issue with -expect-user: %d %q", r.code, r.stderr)
	}
}

// Off a terminal the code is never printed: issue refuses before anything is
// issued, unless -code-file names a new private file, which receives it.
func TestInvitationIssueCodeFile(t *testing.T) {
	storePath, team := invitationStore(t)
	onTerminal(t, false)
	args := []string{"invitation", "issue", "-store", storePath, "-team", team, "-role", "worker", "-hub", "hub-a", "-label", "builder"}
	if r := cli(t, args...); r.code != 2 || !strings.Contains(r.stderr, "-code-file") || codeShape.MatchString(r.stdout+r.stderr) {
		t.Fatalf("issue off a terminal: %d %q %q", r.code, r.stdout, r.stderr)
	}
	if got := listInvitations(t, storePath); len(got) != 0 {
		t.Fatalf("a refused issue issued: %+v", got)
	}
	codeFile := filepath.Join(t.TempDir(), "invite.code")
	r := cli(t, append(args, "-code-file", codeFile)...)
	if r.code != 0 || codeShape.MatchString(r.stdout+r.stderr) {
		t.Fatalf("issue to a code file: %d %q %q", r.code, r.stdout, r.stderr)
	}
	b, err := os.ReadFile(codeFile)
	code := strings.TrimSpace(string(b))
	if err != nil || !codeShape.MatchString(code) {
		t.Fatalf("code file = %q, %v", b, err)
	}
	var v invitationView
	if err := json.Unmarshal([]byte(r.stdout), &v); err != nil || v.CodeFile != codeFile {
		t.Fatalf("metadata: %s (%v)", r.stdout, err)
	}
	if !redeemable(t, storePath, code) {
		t.Fatal("the written code does not redeem")
	}
	// An existing file is never reused, and nothing is issued.
	if r := cli(t, append(args, "-code-file", codeFile)...); r.code != 1 {
		t.Fatalf("issue into an existing file: %d", r.code)
	}
	if got := listInvitations(t, storePath); len(got) != 1 {
		t.Fatalf("an issue into an existing file issued: %d", len(got))
	}
	// A failed write revokes the invitation it issued: the code is lost.
	old := writeCode
	writeCode = func(*os.File, string) error { return errors.New("disk full") }
	t.Cleanup(func() { writeCode = old })
	lost := filepath.Join(t.TempDir(), "lost.code")
	if r := cli(t, append(args, "-code-file", lost)...); r.code != 1 || !strings.Contains(r.stderr, "was revoked") {
		t.Fatalf("a failed write: %d %q", r.code, r.stderr)
	}
	if _, err := os.Stat(lost); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("a failed write left its code file")
	}
	got := listInvitations(t, storePath)
	if len(got) != 2 || got[0].State != "revoked" {
		t.Fatalf("after a failed write: %+v", got)
	}
}

// The purposes' bindings are enforced before anything is issued.
func TestInvitationIssueBindings(t *testing.T) {
	storePath, team := invitationStore(t)
	onTerminal(t, true)
	base := []string{"invitation", "issue", "-store", storePath, "-team", team, "-role", "worker", "-hub", "hub-a"}
	// Each is the command's own usage refusal (exit 2), made before the
	// store is opened; the message names what is missing.
	for name, c := range map[string]struct {
		extra []string
		says  string
	}{
		"join without a label":       {nil, "-label"},
		"join naming an agent":       {[]string{"-label", "x", "-agent", "a1"}, "no -agent"},
		"link without an agent":      {[]string{"-purpose", "link"}, "-agent"},
		"rebind without an agent":    {[]string{"-purpose", "rebind", "-expect-user", "u"}, "-agent"},
		"rebind without expect-user": {[]string{"-purpose", "rebind", "-agent", "a1"}, "requires -expect-user"},
		"an unknown purpose":         {[]string{"-purpose", "adopt", "-label", "x"}, "usage"},
		"a list flag on issue":       {[]string{"-label", "x", "-id", "inv"}, "usage"},
		"a code as an argument":      {[]string{"-label", "x", "ABCD-EFGH"}, "usage"},
	} {
		if r := cli(t, append(append([]string{}, base...), c.extra...)...); r.code != 2 || !strings.Contains(r.stderr, c.says) {
			t.Errorf("%s: %d %q", name, r.code, r.stderr)
		}
	}
	// An invalid role reaches the store, which refuses it.
	if r := cli(t, append(append([]string{}, base...), "-label", "x", "-role", "admin")...); r.code != 1 {
		t.Errorf("an invalid role: %d %q", r.code, r.stderr)
	}
	if got := listInvitations(t, storePath); len(got) != 0 {
		t.Fatalf("a refused issue issued: %+v", got)
	}
	if r := cli(t, "invitation", "issue", "-store", storePath, "-team", "no-such-team", "-role", "worker", "-hub", "hub-a", "-label", "x"); r.code != 1 {
		t.Fatalf("an unknown team: %d %q", r.code, r.stderr)
	}
}

// Revoke makes an invitation final; the listing filters by team.
func TestInvitationRevokeAndList(t *testing.T) {
	storePath, team := invitationStore(t)
	onTerminal(t, true)
	r := cli(t, "invitation", "issue", "-store", storePath, "-team", team, "-role", "worker", "-hub", "hub-a", "-label", "builder")
	var v invitationView
	if r.code != 0 || json.NewDecoder(strings.NewReader(r.stdout)).Decode(&v) != nil {
		t.Fatalf("issue: %d %s", r.code, r.stderr)
	}
	code := codeShape.FindString(r.stdout)
	r = cli(t, "invitation", "revoke", "-store", storePath, "-id", v.ID)
	var revoked invitationView
	if r.code != 0 || json.Unmarshal([]byte(r.stdout), &revoked) != nil || revoked.State != "revoked" {
		t.Fatalf("revoke: %d %s %s", r.code, r.stdout, r.stderr)
	}
	if redeemable(t, storePath, code) {
		t.Fatal("a revoked invitation still redeems")
	}
	if r := cli(t, "invitation", "revoke", "-store", storePath, "-id", v.ID); r.code != 1 {
		t.Fatalf("revoking a revoked invitation: %d", r.code)
	}
	if got := listInvitations(t, storePath, "-team", "other-team"); len(got) != 0 {
		t.Fatalf("filter by team: %+v", got)
	}
	if got := listInvitations(t, storePath, "-team", team); len(got) != 1 || got[0].State != "revoked" {
		t.Fatalf("list by team: %+v", got)
	}
}
