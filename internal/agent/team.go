package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// The team's projects in the home (3a4b). A member's session reads them from
// aicrewd (GET /v1/crew/projects: every project the team's hub grants, with
// or without a repository) at each session start; the launcher records that
// answer here so the member finds its team's projects in its
// home, through `aicrew-agent session status`, without asking the operator.
// It holds no secret; aicrewd stays the authority, and the record is only as
// fresh as its ReadAt.

// TeamRecord is the team's projects as the launcher last read them.
type TeamRecord struct {
	Version int       `json:"version"`
	ReadAt  time.Time `json:"read_at"`
	// HubAlias is the home's name for the team's hub (agent.json's
	// aimem_hub), beside each project's hub ID.
	HubAlias string        `json:"hub_alias,omitempty"`
	Projects []TeamProject `json:"projects"`
}

// TeamProject is one project granted to the team, with the repository the
// hub binds to it, or none.
type TeamProject struct {
	HubID      string `json:"hub_id"`
	ProjectID  string `json:"project_id"`
	Repository *struct {
		Kind   string `json:"kind"`
		URL    string `json:"url"`
		Access string `json:"access"`
	} `json:"repository"`
}

const projectsPath = "/v1/crew/projects"

// RecordTeam reads the team's projects as the session of token and records
// them in home, with the home's hub alias.
func RecordTeam(ctx context.Context, home, hubAlias string, crew SessionReader, token string) error {
	raw, err := crew.Read(ctx, token, projectsPath)
	if err != nil {
		return err
	}
	var answer struct {
		Projects []TeamProject `json:"projects"`
	}
	if err := json.Unmarshal(raw, &answer); err != nil {
		return fmt.Errorf("the team's projects: %w", err)
	}
	return SaveTeam(home, TeamRecord{ReadAt: time.Now().UTC(), HubAlias: hubAlias, Projects: answer.Projects})
}

func teamPath(home string) string { return filepath.Join(home, "state", "team.json") }

// SaveTeam replaces the home's record of its team's projects.
func SaveTeam(home string, r TeamRecord) error {
	r.Version = 1
	if r.Projects == nil {
		r.Projects = []TeamProject{}
	}
	b, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(teamPath(home)), 0o700); err != nil {
		return err
	}
	return writeAtomic(teamPath(home), append(b, '\n'))
}

// LoadTeam reads the record; ok is false when there is none yet.
func LoadTeam(home string) (TeamRecord, bool, error) {
	raw, err := os.ReadFile(teamPath(home))
	if errors.Is(err, os.ErrNotExist) {
		return TeamRecord{}, false, nil
	}
	if err != nil {
		return TeamRecord{}, false, err
	}
	var r TeamRecord
	if err := json.Unmarshal(raw, &r); err != nil || r.Version != 1 {
		return TeamRecord{}, false, fmt.Errorf("%s is not a team record", teamPath(home))
	}
	return r, true, nil
}
