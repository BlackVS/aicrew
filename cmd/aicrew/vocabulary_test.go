package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/BlackVS/aicrew/internal/optoken"
)

// --output - writes the bearer alone to standard output for a pipe, with the
// metadata on stderr, and is refused on a terminal before anything is
// issued (merged proposal section 6.4).
func TestCredentialOutputPipe(t *testing.T) {
	s := serve(t)
	onTerminal(t, true)
	if r := cli(t, "hub-credential", "issue", "--hub", "hub-a", "--output", "-"); r.code != 1 ||
		!strings.Contains(r.stderr, "standard output is a terminal") || r.stdout != "" {
		t.Fatalf("issue to a terminal: %d %q %q", r.code, r.stdout, r.stderr)
	}
	if r := cli(t, "hub-credential", "list"); r.code != 0 || strings.TrimSpace(r.stdout) != "[]" {
		t.Fatalf("a refused issue issued: %s", r.stdout)
	}

	onTerminal(t, false)
	r := cli(t, "hub-credential", "issue", "--hub", "hub-a", "--output", "-")
	if r.code != 0 || strings.Count(r.stdout, "\n") != 1 {
		t.Fatalf("issue into a pipe: %d %q %q", r.code, r.stdout, r.stderr)
	}
	if hub, err := authenticate(t, s, r.stdout); err != nil || hub != "hub-a" {
		t.Fatalf("the piped bearer authenticates as %q, %v", hub, err)
	}
	var v credentialView
	if err := json.Unmarshal([]byte(r.stderr), &v); err != nil || v.HubID != "hub-a" || v.Output != "-" ||
		strings.Contains(r.stderr, strings.TrimSpace(r.stdout)) {
		t.Fatalf("metadata on stderr: %q (%v)", r.stderr, err)
	}
}

// operator-token new writes to --output, a file or a pipe, never to a
// terminal; -file, its name before 0.3.0, still works with a notice.
func TestOperatorTokenOutput(t *testing.T) {
	onTerminal(t, true)
	if r := cli(t, "operator-token", "new", "--output", "-"); r.code != 1 || r.stdout != "" {
		t.Fatalf("a token to a terminal: %d %q %q", r.code, r.stdout, r.stderr)
	}
	onTerminal(t, false)
	r := cli(t, "operator-token", "new", "--output", "-")
	if r.code != 0 || !optoken.Valid(strings.TrimSpace(r.stdout)) || !strings.Contains(r.stderr, "standard output") {
		t.Fatalf("a token into a pipe: %d %q %q", r.code, r.stdout, r.stderr)
	}
	path := filepath.Join(t.TempDir(), "op.token")
	r = cli(t, "operator-token", "new", "-file", path)
	if r.code != 0 || !strings.Contains(r.stderr, "-file is now --output") {
		t.Fatalf("a token with -file: %d %q", r.code, r.stderr)
	}
	if tok, err := optoken.Read(path); err != nil || !optoken.Valid(tok) {
		t.Fatalf("the token file: %v", err)
	}
	if r := cli(t, "operator-token", "new", "--output", path+"2", "-file", path+"3"); r.code != 2 {
		t.Fatalf("both names: %d", r.code)
	}
}

// -secret-file, --output's name before 0.3.0, still works with a notice.
func TestSecretFileNotice(t *testing.T) {
	serve(t)
	path := filepath.Join(t.TempDir(), "old.secret")
	r := cli(t, "hub-credential", "issue", "--hub", "hub-a", "-secret-file", path)
	if r.code != 0 || !strings.Contains(r.stderr, "-secret-file is now --output") {
		t.Fatalf("issue with -secret-file: %d %q", r.code, r.stderr)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatal(err)
	}
}

// --team-name names a team wherever --team takes its ID (merged proposal
// section 6.2): one or the other, never both; an unknown name fails before
// anything changes; invitations print the team's name beside its ID.
func TestTeamName(t *testing.T) {
	serve(t)
	created := decodeTeam(t, teamCLI(t, 0, "create", "--name", "crew"))
	if shown := decodeTeam(t, teamCLI(t, 0, "show", "--team-name", "crew")); shown.ID != created.ID {
		t.Fatalf("show by name: %+v", shown)
	}
	renamed := decodeTeam(t, teamCLI(t, 0, "rename", "--team-name", "crew", "--expect-revision", "1", "--name", "builders"))
	if renamed.ID != created.ID || renamed.Name != "builders" {
		t.Fatalf("rename by name: %+v", renamed)
	}
	if r := teamCLI(t, 1, "show", "--team-name", "crew"); !strings.Contains(r.stderr, `no team is named "crew"`) {
		t.Fatalf("an unknown name: %q", r.stderr)
	}
	if r := teamCLI(t, 2, "show", "--team", created.ID, "--team-name", "builders"); !strings.Contains(r.stderr, "not both") {
		t.Fatalf("both flags: %q", r.stderr)
	}

	code := filepath.Join(t.TempDir(), "invite.code")
	r := cli(t, "invitation", "issue", "--team-name", "builders", "--role", "worker", "--hub", "hub-a", "--label", "b",
		"--expect-user", "u-1", "--output", code)
	var v invitationView
	if r.code != 0 || json.Unmarshal([]byte(r.stdout), &v) != nil || v.TeamID != created.ID || v.TeamName != "builders" {
		t.Fatalf("issue by team name: %d %s %s", r.code, r.stdout, r.stderr)
	}
	r = cli(t, "invitation", "list", "--team-name", "builders")
	var listed []invitationView
	if r.code != 0 || json.Unmarshal([]byte(r.stdout), &listed) != nil || len(listed) != 1 || listed[0].TeamName != "builders" {
		t.Fatalf("list by team name: %d %s", r.code, r.stdout)
	}
	if r := cli(t, "invitation", "issue", "--team-name", "nobody", "--role", "worker", "--hub", "hub-a", "--label", "c",
		"--output", code+"2"); r.code != 1 {
		t.Fatalf("issue for an unknown team name: %d %q", r.code, r.stderr)
	}
	if _, err := os.Stat(code + "2"); err == nil {
		t.Fatal("an issue for an unknown team created its output file")
	}
}
