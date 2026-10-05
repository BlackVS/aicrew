package store

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// A member's report replaces its earlier one and is read back per agent and
// for the team; a report that is not one is refused and keeps the last.
func TestCapabilityReports(t *testing.T) {
	ctx := context.Background()
	e := newClaimStopEnv(t)
	s := e.s
	wb, err := s.AuthenticateSessionToken(ctx, e.workerTok)
	if err != nil {
		t.Fatal(err)
	}
	if caps, at, err := s.AgentCapabilities(ctx, wb.AgentID); err != nil || len(caps) != 0 || at != nil {
		t.Fatalf("before any report: %v %v %v", caps, at, err)
	}
	report := []Capability{{Host: "git.example.test", Kind: "gitea", Account: "builder",
		Repositories: []RepositoryCapability{{URL: testRepository.URL, Access: "write"}}}}
	if _, err := s.ReportCapabilitiesWithToken(ctx, e.workerTok, report); err != nil {
		t.Fatal(err)
	}
	caps, at, err := s.AgentCapabilities(ctx, wb.AgentID)
	if err != nil || at == nil || len(caps) != 1 || caps[0].Repositories[0].Access != "write" {
		t.Fatalf("after the report: %+v %v %v", caps, at, err)
	}
	for name, bad := range map[string][]Capability{
		"kind":    {{Host: "git.example.test", Kind: "svn", Account: "b"}},
		"account": {{Host: "git.example.test", Kind: "gitea"}},
		"host":    {{Host: "git example", Kind: "gitea", Account: "b"}},
		"twice":   {{Host: "h", Kind: "gitea", Account: "b"}, {Host: "h", Kind: "gitea", Account: "b"}},
		"url":     {{Host: "h", Kind: "gitea", Account: "b", Repositories: []RepositoryCapability{{URL: "http://h/r.git", Access: "write"}}}},
		"access":  {{Host: "h", Kind: "gitea", Account: "b", Repositories: []RepositoryCapability{{URL: "https://h/r.git", Access: "admin"}}}},
	} {
		if _, err := s.ReportCapabilitiesWithToken(ctx, e.workerTok, bad); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: %v", name, err)
		}
	}
	if _, err := s.ReportCapabilitiesWithToken(ctx, "not-a-token", report); !errors.Is(err, ErrTokenInvalid) {
		t.Fatalf("a bad token: %v", err)
	}
	if _, err := s.ReportCapabilitiesWithToken(ctx, e.workerTok, nil); err != nil {
		t.Fatal(err)
	}
	if caps, _, _ := s.AgentCapabilities(ctx, wb.AgentID); len(caps) != 0 {
		t.Fatalf("an empty report kept %+v", caps)
	}
	members, err := s.TeamCapabilitiesWithToken(ctx, e.leadTok)
	if err != nil || len(members) < 2 {
		t.Fatalf("team = %+v, %v", members, err)
	}
	for _, m := range members {
		if m.AgentID == wb.AgentID && (m.ReportedAt == nil || m.Capabilities == nil) {
			t.Fatalf("the worker's row = %+v", m)
		}
	}
}

// A report covers a repository at the access it verified, write covering
// read; the gap names the access it lacks.
func TestCapabilityGap(t *testing.T) {
	caps := []Capability{{Host: "git.example.test", Kind: "gitea", Account: "b",
		Repositories: []RepositoryCapability{{URL: "https://git.example.test/crew/a.git", Access: "read"},
			{URL: "https://git.example.test/crew/b.git", Access: "write"}}}}
	for _, c := range []struct {
		url, access, want string
	}{
		{"https://git.example.test/crew/a.git", "read", ""},
		{"https://git.example.test/crew/a.git", "write", "write"},
		{"https://git.example.test/crew/b.git", "read", ""},
		{"https://git.example.test/crew/c.git", "read", "read"},
	} {
		if got := CapabilityGap(caps, AttemptRepository{URL: c.url, Access: c.access}); got != c.want {
			t.Errorf("%s at %s: %q, want %q", c.url[strings.LastIndex(c.url, "/")+1:], c.access, got, c.want)
		}
	}
}
