package agent

import (
	"fmt"
	"strings"
	"time"
)

// TeamMember is one member of the team as aicrewd lists it, with its last
// capability report (GET /v1/crew/capabilities): the agent ID a coordinator
// names as an offer's worker_agent_id, and the repositories the member
// verified.
type TeamMember struct {
	AgentID      string       `json:"agent_id"`
	Label        string       `json:"label"`
	Role         string       `json:"role"`
	ReportedAt   *time.Time   `json:"reported_at,omitempty"`
	Capabilities []Capability `json:"capabilities"`
}

// TeamMembers is the members step's answer.
type TeamMembers struct {
	Members []TeamMember `json:"members"`
}

// Line is the member in one line: its agent ID, label and role, and the
// repositories it verified with their access, or that it reported none.
func (m TeamMember) Line() string {
	var repos []string
	for _, c := range m.Capabilities {
		for _, r := range c.Repositories {
			repos = append(repos, r.URL+" ("+r.Access+")")
		}
	}
	verified := "no capability report"
	switch {
	case m.ReportedAt != nil && len(repos) == 0:
		verified = "no repository verified at " + m.ReportedAt.UTC().Format(time.RFC3339)
	case m.ReportedAt != nil:
		verified = "verified at " + m.ReportedAt.UTC().Format(time.RFC3339) + ": " + strings.Join(repos, ", ")
	}
	return fmt.Sprintf("%s  %s (%s)  %s", m.AgentID, m.Label, m.Role, verified)
}
