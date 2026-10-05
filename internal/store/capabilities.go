package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Capability is what a member's home verified on one forge host
// (docs/proposals/PILOT-1-FOLLOWUPS.md, 3.6): the account its credential
// authenticates as there, and each required repository it verified with
// the access the forge reports. It is a planning fact (principle 7): it
// authorizes nothing, the forge enforces the token.
type Capability struct {
	Host         string                 `json:"host"`
	Kind         string                 `json:"kind"`
	Account      string                 `json:"account"`
	Repositories []RepositoryCapability `json:"repositories"`
}

// RepositoryCapability is one repository a member verified, with the
// access the forge reports for it.
type RepositoryCapability struct {
	URL    string `json:"url"`
	Access string `json:"access"`
}

// MemberCapabilities is a team member's last capability report.
type MemberCapabilities struct {
	AgentID      string       `json:"agent_id"`
	Label        string       `json:"label"`
	Role         Role         `json:"role"`
	ReportedAt   *time.Time   `json:"reported_at,omitempty"`
	Capabilities []Capability `json:"capabilities"`
}

const (
	maxCapabilityHosts = 32
	maxCapabilityRepos = 256
	maxCapabilityField = 255
)

func validateCapabilities(caps []Capability) error {
	if len(caps) > maxCapabilityHosts {
		return fmt.Errorf("%w: at most %d forge hosts", ErrInvalid, maxCapabilityHosts)
	}
	hosts := map[string]bool{}
	for _, c := range caps {
		if c.Host == "" || len(c.Host) > maxCapabilityField || strings.ContainsAny(c.Host, " /@\t\r\n") || hosts[c.Host] ||
			!RepositoryKinds[c.Kind] || c.Account == "" || len(c.Account) > maxCapabilityField ||
			len(c.Repositories) > maxCapabilityRepos {
			return fmt.Errorf("%w: a capability names one host once, its kind (github, gitea or gitlab), its account "+
				"and at most %d repositories", ErrInvalid, maxCapabilityRepos)
		}
		hosts[c.Host] = true
		for _, r := range c.Repositories {
			probe := AttemptRepository{Kind: c.Kind, URL: r.URL, Access: r.Access, DefaultBranch: "x"}
			if !probe.valid() {
				return fmt.Errorf("%w: a verified repository is an https clone URL with access read or write", ErrInvalid)
			}
		}
	}
	return nil
}

// ReportCapabilitiesWithToken records the token's agent's capabilities,
// replacing its earlier report. It returns when the report was recorded.
func (s *Store) ReportCapabilitiesWithToken(ctx context.Context, token string, caps []Capability) (time.Time, error) {
	if caps == nil {
		caps = []Capability{}
	}
	for i := range caps {
		if caps[i].Repositories == nil {
			caps[i].Repositories = []RepositoryCapability{}
		}
	}
	if err := validateCapabilities(caps); err != nil {
		return time.Time{}, err
	}
	report, err := json.Marshal(caps)
	if err != nil {
		return time.Time{}, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return time.Time{}, fmt.Errorf("begin: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck // no-op after commit
	now := s.now()
	b, err := tokenBinding(ctx, tx, token, now)
	if err != nil {
		return time.Time{}, err
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO agent_capabilities (agent_id, report, reported_at) VALUES (?, ?, ?)
		 ON CONFLICT (agent_id) DO UPDATE SET report = excluded.report, reported_at = excluded.reported_at`,
		b.AgentID, string(report), formatTime(now)); err != nil {
		return time.Time{}, fmt.Errorf("record capabilities: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return time.Time{}, fmt.Errorf("commit: %w", err)
	}
	return now, nil
}

// AgentCapabilities is an agent's last capability report, or none.
func (s *Store) AgentCapabilities(ctx context.Context, agentID string) ([]Capability, *time.Time, error) {
	var (
		caps []Capability
		at   *time.Time
	)
	err := s.snapshot(ctx, func(q querier) error {
		var err error
		caps, at, err = agentCapabilities(ctx, q, agentID)
		return err
	})
	return caps, at, err
}

func agentCapabilities(ctx context.Context, q querier, agentID string) ([]Capability, *time.Time, error) {
	var report, reported string
	err := q.QueryRowContext(ctx, `SELECT report, reported_at FROM agent_capabilities WHERE agent_id = ?`, agentID).
		Scan(&report, &reported)
	if errors.Is(err, sql.ErrNoRows) {
		return []Capability{}, nil, nil
	}
	if err != nil {
		return nil, nil, fmt.Errorf("read capabilities: %w", err)
	}
	var caps []Capability
	if err := json.Unmarshal([]byte(report), &caps); err != nil {
		return nil, nil, fmt.Errorf("read capabilities: %w", err)
	}
	at, err := parseTime(reported)
	if err != nil {
		return nil, nil, err
	}
	return caps, &at, nil
}

// TeamCapabilitiesWithToken lists the token's team's current members with
// their last capability reports, for the coordinator's planning.
func (s *Store) TeamCapabilitiesWithToken(ctx context.Context, token string) ([]MemberCapabilities, error) {
	var out []MemberCapabilities
	err := s.snapshot(ctx, func(q querier) error {
		b, err := tokenBinding(ctx, q, token, s.now())
		if err != nil {
			return err
		}
		members, err := listMembers(ctx, q, b.TeamID)
		if err != nil {
			return err
		}
		out = make([]MemberCapabilities, 0, len(members))
		for _, m := range members {
			a, err := getAgent(ctx, q, m.AgentID)
			if err != nil {
				return err
			}
			caps, at, err := agentCapabilities(ctx, q, m.AgentID)
			if err != nil {
				return err
			}
			out = append(out, MemberCapabilities{AgentID: m.AgentID, Label: a.Label, Role: m.Role, ReportedAt: at,
				Capabilities: caps})
		}
		return nil
	})
	return out, err
}

// RequirementsWithToken is what the token's team's hub requires of a
// member's home: each granted project with the repository the hub binds to
// it, from the team's grants snapshot.
func (s *Store) RequirementsWithToken(ctx context.Context, token string) ([]TeamGrant, error) {
	var out []TeamGrant
	err := s.snapshot(ctx, func(q querier) error {
		b, err := tokenBinding(ctx, q, token, s.now())
		if err != nil {
			return err
		}
		grants, err := teamGrants(ctx, q, b.TeamID)
		if err != nil {
			return err
		}
		out = []TeamGrant{}
		for _, g := range grants {
			if g.Repository != nil {
				out = append(out, g)
			}
		}
		return nil
	})
	return out, err
}

// accessCovers reports whether access satisfies required: write covers
// read.
func accessCovers(access, required string) bool {
	return access == required || (access == "write" && required == "read")
}

// CapabilityGap names what a worker's last report lacks for repository at
// access: the repository's host and the access, or "" when the report
// verified it.
func CapabilityGap(caps []Capability, repository AttemptRepository) string {
	for _, c := range caps {
		for _, r := range c.Repositories {
			if r.URL == repository.URL && accessCovers(r.Access, repository.Access) {
				return ""
			}
		}
	}
	return repository.Access
}
