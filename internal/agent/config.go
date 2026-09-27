// Package agent is the session engine of aicrew's agent client
// (cmd/aicrew-agent): it proves the agent's aimem identity, enters or resumes
// the agent's team session through aicrewd's client session API, binds aimem
// to it with aimem's team-session commands, keeps the aimem-scoped handle
// fresh and the session token alive, and leaves (docs/CREW-CONTRACT.md,
// "Client session API").
//
// The session token, the proof receipt and the handle live only in memory
// and in the stdin of the one aimem command that needs them: never in a
// command line, the environment, a file this package writes, or a log.
package agent

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"regexp"

	"github.com/BlackVS/aicrew/internal/tlstrust"
)

// Config is the agent's aicrew client configuration, from the "aicrew"
// section of the agent home's agent.json (docs/WORKSPACE.md). It holds no
// secret.
type Config struct {
	Home         string
	URL          string // aicrewd's https origin
	Trust        tlstrust.Binding
	AgentID      string
	TeamID       string
	AimemCommand string // the aimem executable; "aimem" by default
	AimemHub     string // aimem's hub name, if not the default
}

type aicrewSection struct {
	URL           string `json:"url"`
	TLSTrustMode  string `json:"tls_trust_mode"`
	TLSTrustValue string `json:"tls_trust_value"`
	AgentID       string `json:"agent_id"`
	TeamID        string `json:"team_id"`
	AimemCommand  string `json:"aimem_command,omitempty"`
	AimemHub      string `json:"aimem_hub,omitempty"`
}

const maxAgentJSON = 64 << 10

var idShape = regexp.MustCompile(`^[A-Za-z0-9._:-]{1,128}$`)

// LoadConfig reads <home>/agent.json. Other sections of the file are the
// onboarding's and are ignored; the aicrew section admits no unknown field.
func LoadConfig(home string) (Config, error) {
	f, err := os.Open(filepath.Join(home, "agent.json"))
	if err != nil {
		return Config{}, fmt.Errorf("agent.json: %w", err)
	}
	defer f.Close()
	raw, err := io.ReadAll(io.LimitReader(f, maxAgentJSON+1))
	if err != nil {
		return Config{}, fmt.Errorf("agent.json: %w", err)
	}
	if len(raw) > maxAgentJSON {
		return Config{}, errors.New("agent.json is larger than 64 KiB")
	}
	var doc struct {
		Aicrew json.RawMessage `json:"aicrew"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return Config{}, fmt.Errorf("agent.json: %w", err)
	}
	if len(doc.Aicrew) == 0 || string(doc.Aicrew) == "null" {
		return Config{}, errors.New("agent.json has no aicrew section")
	}
	var s aicrewSection
	dec := json.NewDecoder(bytes.NewReader(doc.Aicrew))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&s); err != nil {
		return Config{}, fmt.Errorf("agent.json aicrew: %w", err)
	}
	c := Config{Home: home, URL: s.URL, Trust: tlstrust.Binding{Mode: s.TLSTrustMode, Value: s.TLSTrustValue},
		AgentID: s.AgentID, TeamID: s.TeamID, AimemCommand: s.AimemCommand, AimemHub: s.AimemHub}
	if c.AimemCommand == "" {
		c.AimemCommand = "aimem"
	}
	return c, c.validate()
}

func (c Config) validate() error {
	u, err := url.Parse(c.URL)
	if err != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil ||
		u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || (u.Path != "" && u.Path != "/") {
		return errors.New("agent.json aicrew.url must be an https origin without credentials, path, query or fragment")
	}
	if err := c.Trust.Check(u.Hostname()); err != nil {
		return fmt.Errorf("agent.json aicrew: %w", err)
	}
	if !idShape.MatchString(c.AgentID) {
		return errors.New("agent.json aicrew.agent_id is not a valid ID")
	}
	if !idShape.MatchString(c.TeamID) {
		return errors.New("agent.json aicrew.team_id is not a valid ID")
	}
	if c.AimemHub != "" && !idShape.MatchString(c.AimemHub) {
		return errors.New("agent.json aicrew.aimem_hub is not a valid name")
	}
	return nil
}
