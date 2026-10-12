package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/BlackVS/aicrew/internal/opapi"
	"github.com/BlackVS/aicrew/internal/store"
)

func memberCLI(t *testing.T, want int, args ...string) result {
	t.Helper()
	r := cli(t, append([]string{"member"}, args...)...)
	if r.code != want {
		t.Fatalf("aicrew member %v: exit %d, want %d\nstdout: %s\nstderr: %s", args, r.code, want, r.stdout, r.stderr)
	}
	return r
}

// team state, pause and resume, and member pause, resume and nudge: each
// prints the API's answer as JSON; a member is named by its label or its
// agent ID; a pause or resume that changes nothing says so.
func TestTeamAndMemberControl(t *testing.T) {
	s := serve(t)
	ctx := context.Background()
	tm := decodeTeam(t, teamCLI(t, 0, "create", "-name", "crew"))
	op := operator(t)
	agents := map[string]store.Agent{}
	for _, label := range []string{"lead", "builder"} {
		a, err := s.store.CreateAgent(ctx, op, "agent-"+label, store.NewAgent{Label: label})
		if err == nil {
			role := map[string]store.Role{"lead": store.RoleCoordinator, "builder": store.RoleWorker}[label]
			_, err = s.store.AddMember(ctx, op, "member-"+label, tm.ID, a.ID, role)
		}
		if err != nil {
			t.Fatal(err)
		}
		agents[label] = a
	}

	var st opapi.TeamState
	if err := json.Unmarshal([]byte(teamCLI(t, 0, "state", "-team-name", "crew").stdout), &st); err != nil {
		t.Fatal(err)
	}
	if st.TeamID != tm.ID || st.Paused != nil || len(st.Members) != 2 {
		t.Fatalf("state = %+v", st)
	}
	for _, m := range st.Members {
		// Neither member has entered a session.
		if m.State != opapi.StateOffline || m.Session != nil || m.Paused != nil {
			t.Fatalf("a member without a session: %+v", m)
		}
	}

	var res opapi.PauseResult
	if err := json.Unmarshal([]byte(teamCLI(t, 0, "pause", "-team", tm.ID, "-reason", "upgrade").stdout), &res); err != nil ||
		!res.Changed || res.Paused == nil || res.Paused.Reason != "upgrade" {
		t.Fatalf("team pause = %+v %v", res, err)
	}
	if r := teamCLI(t, 0, "pause", "-team", tm.ID); !strings.Contains(r.stderr, "the team was already paused; nothing changed") {
		t.Fatalf("pausing a paused team: %q", r.stderr)
	}
	_ = json.Unmarshal([]byte(teamCLI(t, 0, "state", "-team", tm.ID).stdout), &st)
	if st.Paused == nil || st.Members[0].State != opapi.StatePaused {
		t.Fatalf("state while paused = %+v", st)
	}
	teamCLI(t, 0, "resume", "-team-name", "crew")
	if r := teamCLI(t, 0, "resume", "-team-name", "crew"); !strings.Contains(r.stderr, "the team was not paused; nothing changed") {
		t.Fatalf("resuming a running team: %q", r.stderr)
	}

	res = opapi.PauseResult{}
	if err := json.Unmarshal([]byte(memberCLI(t, 0, "pause", "-team-name", "crew", "-agent", "builder", "-reason", "r").stdout), &res); err != nil ||
		!res.Changed || res.Paused == nil || res.Paused.AgentID != agents["builder"].ID {
		t.Fatalf("member pause by label = %+v %v", res, err)
	}
	res = opapi.PauseResult{}
	if err := json.Unmarshal([]byte(memberCLI(t, 0, "resume", "-team", tm.ID, "-agent", agents["builder"].ID).stdout), &res); err != nil ||
		!res.Changed || res.Paused != nil {
		t.Fatalf("member resume by agent ID = %+v %v", res, err)
	}

	var n opapi.Nudge
	if err := json.Unmarshal([]byte(memberCLI(t, 0, "nudge", "-team", tm.ID, "-agent", "lead", "-note", "the agent is upgraded").stdout), &n); err != nil ||
		n.MessageID == "" || n.AgentID != agents["lead"].ID {
		t.Fatalf("nudge = %+v %v", n, err)
	}
	note := filepath.Join(t.TempDir(), "note.txt")
	if err := os.WriteFile(note, []byte("restart with run\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	memberCLI(t, 0, "nudge", "-team", tm.ID, "-agent", "lead", "-note-file", note)
	_ = json.Unmarshal([]byte(teamCLI(t, 0, "state", "-team", tm.ID).stdout), &st)
	for _, m := range st.Members {
		want := map[string]int{agents["lead"].ID: 2, agents["builder"].ID: 0}[m.AgentID]
		if m.Unacknowledged != want {
			t.Fatalf("%s's unacknowledged count = %d, want %d", m.Label, m.Unacknowledged, want)
		}
	}
}

// The member commands refuse a missing team, member or note, both note
// flags, an unknown member and an ambiguous label.
func TestMemberControlRefusals(t *testing.T) {
	s := serve(t)
	ctx := context.Background()
	tm := decodeTeam(t, teamCLI(t, 0, "create", "-name", "crew"))
	op := operator(t)
	for _, id := range []string{"a1", "a2"} {
		a, err := s.store.CreateAgent(ctx, op, "agent-"+id, store.NewAgent{Label: "twin-" + id})
		if err == nil {
			_, err = s.store.AddMember(ctx, op, "member-"+id, tm.ID, a.ID, store.RoleWorker)
		}
		if err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.store.CreateAgent(ctx, op, "agent-x", store.NewAgent{Label: "outsider"}); err != nil {
		t.Fatal(err)
	}
	memberCLI(t, 2)
	memberCLI(t, 2, "pause", "-agent", "twin-a1")
	memberCLI(t, 2, "pause", "-team", tm.ID)
	memberCLI(t, 2, "nudge", "-team", tm.ID, "-agent", "twin-a1")
	memberCLI(t, 2, "nudge", "-team", tm.ID, "-agent", "twin-a1", "-note", "x", "-note-file", "-")
	memberCLI(t, 2, "resume", "-team", tm.ID, "-agent", "twin-a1", "-reason", "x")
	memberCLI(t, 2, "stop", "-team", tm.ID, "-agent", "twin-a1")
	if r := memberCLI(t, 1, "pause", "-team", tm.ID, "-agent", "outsider"); !strings.Contains(r.stderr, "aicrew team state lists them") {
		t.Fatalf("a non-member: %q", r.stderr)
	}
	teamCLI(t, 2, "pause")
	teamCLI(t, 2, "resume", "-team", tm.ID, "-reason", "x")
	teamCLI(t, 2, "state", "-team", tm.ID, "-reason", "x")
}

// A label two members share names neither.
func TestResolveMemberAmbiguousLabel(t *testing.T) {
	s := serve(t)
	ctx := context.Background()
	tm := decodeTeam(t, teamCLI(t, 0, "create", "-name", "crew"))
	op := operator(t)
	for _, id := range []string{"a1", "a2"} {
		a, err := s.store.CreateAgent(ctx, op, "agent-"+id, store.NewAgent{Label: "twin"})
		if err == nil {
			_, err = s.store.AddMember(ctx, op, "member-"+id, tm.ID, a.ID, store.RoleWorker)
		}
		if err != nil {
			t.Fatal(err)
		}
	}
	if r := memberCLI(t, 1, "pause", "-team", tm.ID, "-agent", "twin"); !strings.Contains(r.stderr, "2 members of team crew have label") {
		t.Fatalf("an ambiguous label: %q", r.stderr)
	}
}

// --note-file - reads the note from standard input.
func TestReadNote(t *testing.T) {
	if got, err := readNote("-", bytes.NewBufferString("from stdin\r\n")); err != nil || got != "from stdin" {
		t.Fatalf("stdin: %q %v", got, err)
	}
	if _, err := readNote("-", bytes.NewBufferString(strings.Repeat("n", maxNoteFile+1))); err == nil {
		t.Fatal("an oversized note was read")
	}
	if _, err := readNote(filepath.Join(t.TempDir(), "missing"), nil); err == nil {
		t.Fatal("a missing file was read")
	}
}
