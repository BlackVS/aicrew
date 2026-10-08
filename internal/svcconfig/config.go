// Package svcconfig is aicrewd's configuration file: its shape, its reading
// and the checks that need no client of aimem, and its rewriting by the
// operator's commands (aicrewd config migrate, aicrew hub add). It links no
// store, so the operator's console client can use it. aicrewd checks the
// same file further when it builds its aimem clients (package server).
package svcconfig

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"path/filepath"
	"regexp"
	"strconv"
	"time"

	"github.com/BlackVS/aicrew/internal/hubteams"
	"github.com/BlackVS/aicrew/internal/tlstrust"
)

// Config is the service's configuration file. It names files and addresses
// only; it holds no secret itself.
type Config struct {
	StorePath       string   `json:"store_path"`
	ListenAddr      string   `json:"listen_addr"`
	TLSCertFile     string   `json:"tls_cert_file"`
	TLSKeyFile      string   `json:"tls_key_file"`
	ServiceID       string   `json:"service_id"`
	ShutdownTimeout Duration `json:"shutdown_timeout,omitempty"`
	// OperatorTokenFile is the private file holding the operator
	// credential, which authorizes the operator API (package optoken). It
	// is read on every operator call, so replacing the file rotates the
	// credential without a restart.
	OperatorTokenFile string `json:"operator_token_file"`
	// AimemHubs are the aimem hubs this service works with, one named block
	// each (docs/proposals/PILOT-1-FOLLOWUPS.md, 2.2). Without any, the
	// service refuses session entry and resume; everything else works.
	AimemHubs []AimemHub `json:"aimem_hubs,omitempty"`
	// Aimem is the single unnamed block before 0.3.0, read as one block
	// named "default" for one release and refused together with
	// aimem_hubs. It has no hub ID, so no team can name it as its hub.
	Aimem *AimemConfig `json:"aimem,omitempty"`
}

// AimemHub is one named aimem hub: its name, the hub's stable ID, how to
// reach and trust it, and the peer credentials aimem issued to this
// service for it.
type AimemHub struct {
	// Name is the alias a team names its hub by (team create --hub).
	Name string `json:"name"`
	// HubID is the hub's stable identity, as invitations and task
	// references name it.
	HubID string `json:"hub_id"`
	AimemConfig
	// TeamRegisterTokenFile and TeamReadTokenFile hold the team.register
	// and team.read peer credentials, each separate from the others.
	TeamRegisterTokenFile string `json:"team_register_token_file,omitempty"`
	TeamReadTokenFile     string `json:"team_read_token_file,omitempty"`
}

// LegacyHubName is the name the single block before 0.3.0 is read under.
const LegacyHubName = "default"

// Hubs are the configured hubs: aimem_hubs, or the legacy block as one hub
// named "default".
func (c Config) Hubs() []AimemHub {
	if len(c.AimemHubs) > 0 {
		return c.AimemHubs
	}
	if c.Aimem != nil {
		return []AimemHub{{Name: LegacyHubName, AimemConfig: *c.Aimem}}
	}
	return nil
}

// TeamsConfig is the hub's team.register and team.read client
// configuration, or false when it has neither credential.
func (h AimemHub) TeamsConfig(serviceID string) (hubteams.Config, bool) {
	if h.TeamRegisterTokenFile == "" && h.TeamReadTokenFile == "" {
		return hubteams.Config{}, false
	}
	return hubteams.Config{BaseURL: h.BaseURL, ServiceID: serviceID, TLSMode: h.TLSTrustMode, TLSValue: h.TLSTrustValue,
		RegisterTokenFile: h.TeamRegisterTokenFile, ReadTokenFile: h.TeamReadTokenFile}, true
}

// AimemConfig is how aicrew reaches aimem to redeem proof receipts
// (docs/CREW-CONTRACT.md, "Receipt redemption").
type AimemConfig struct {
	// BaseURL is aimem's https origin.
	BaseURL string `json:"base_url"`
	// TLSTrustMode is ca_dns or spki_sha256, and TLSTrustValue the host
	// name or the sha256- pin.
	TLSTrustMode  string `json:"tls_trust_mode"`
	TLSTrustValue string `json:"tls_trust_value"`
	// RedemptionTokenFile is the private file holding the redemption bearer
	// aimem issued to this service.
	RedemptionTokenFile string `json:"redemption_token_file"`
	// ReadTokenFile is the private file holding the reservation.read bearer
	// aimem issued to this service, a separate credential. Without it aicrew
	// has no read scope, and member-driven steps stay pending
	// (docs/CREW-CONTRACT.md, "Attempt steps").
	ReadTokenFile string `json:"read_token_file,omitempty"`
}

// checkOrigin is the offline check every aimem client of aicrewd makes of
// its hub: an https origin, and a TLS trust binding for that host.
func (a AimemConfig) checkOrigin() error {
	u, err := url.Parse(a.BaseURL)
	if err != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil ||
		u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || (u.Path != "" && u.Path != "/") {
		return errors.New("base_url must be an https origin without credentials, path, query or fragment")
	}
	trust := tlstrust.Binding{Mode: a.TLSTrustMode, Value: a.TLSTrustValue}
	if err := trust.Check(u.Hostname()); err != nil {
		return err
	}
	_, err = trust.ClientConfig(nil)
	return err
}

// Duration is a time.Duration written as a Go duration string, like "15s".
type Duration time.Duration

func (d *Duration) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return errors.New("a duration is a string such as \"15s\"")
	}
	v, err := time.ParseDuration(s)
	if err != nil {
		return errors.New("a duration is a string such as \"15s\"")
	}
	*d = Duration(v)
	return nil
}

const (
	// MaxConfigBytes is the largest configuration file aicrewd reads.
	MaxConfigBytes = 64 << 10
	// DefaultShutdownTimeout is shutdown_timeout when it is not given.
	DefaultShutdownTimeout = 15 * time.Second
	maxShutdownTimeout     = 5 * time.Minute
)

// serviceIDShape is the identity.v1 ID shape; aimem registers the service
// under this ID.
var serviceIDShape = regexp.MustCompile(`^[A-Za-z0-9._:-]{1,128}$`)

// LoadConfig reads and checks the configuration file at path. Unknown fields
// are refused. Errors name the field at fault, never a file's content. These
// are the checks that need no aimem client; aicrewd makes more.
func LoadConfig(path string) (Config, error) {
	raw, _, err := readConfigFile(path)
	if err != nil {
		return Config{}, err
	}
	return ParseConfig(raw)
}

// ParseConfig decodes and checks one configuration object.
func ParseConfig(raw []byte) (Config, error) {
	c, err := decodeConfig(raw)
	if err != nil {
		return Config{}, err
	}
	return c, c.validate()
}

func decodeConfig(raw []byte) (Config, error) {
	var c Config
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&c); err != nil {
		return Config{}, fmt.Errorf("config: %w", err)
	}
	if dec.Decode(&struct{}{}) != io.EOF {
		return Config{}, errors.New("config: one JSON object expected")
	}
	if c.ShutdownTimeout == 0 {
		c.ShutdownTimeout = Duration(DefaultShutdownTimeout)
	}
	return c, nil
}

func (c Config) validate() error {
	for _, f := range []struct{ name, value string }{
		{"store_path", c.StorePath}, {"listen_addr", c.ListenAddr},
		{"tls_cert_file", c.TLSCertFile}, {"tls_key_file", c.TLSKeyFile},
		{"operator_token_file", c.OperatorTokenFile},
	} {
		if f.value == "" {
			return fmt.Errorf("config: %s is required", f.name)
		}
	}
	// An empty host listens on every interface.
	_, port, err := net.SplitHostPort(c.ListenAddr)
	if err != nil {
		return errors.New("config: listen_addr must be host:port")
	}
	if n, err := strconv.Atoi(port); err != nil || n < 0 || n > 65535 {
		return errors.New("config: listen_addr must have a numeric port")
	}
	if !serviceIDShape.MatchString(c.ServiceID) {
		return errors.New("config: service_id must be 1 to 128 characters from [A-Za-z0-9._:-]")
	}
	if d := time.Duration(c.ShutdownTimeout); d <= 0 || d > maxShutdownTimeout {
		return fmt.Errorf("config: shutdown_timeout must be positive and at most %s", maxShutdownTimeout)
	}
	if c.Aimem != nil && len(c.AimemHubs) > 0 {
		return errors.New("config: aimem and aimem_hubs are given together; move the aimem block into aimem_hubs")
	}
	names, ids, readers := map[string]bool{}, map[string]bool{}, ""
	for i, h := range c.Hubs() {
		at := fmt.Sprintf("aimem_hubs[%d]", i)
		if c.Aimem != nil {
			at = "aimem"
		}
		if !hubNameShape.MatchString(h.Name) {
			return fmt.Errorf("config: %s.name must be 1 to 32 lowercase letters, digits or '-'", at)
		}
		if names[h.Name] {
			return fmt.Errorf("config: %s.name %q names two hubs", at, h.Name)
		}
		names[h.Name] = true
		if c.Aimem == nil {
			if !serviceIDShape.MatchString(h.HubID) || h.HubID == "." || h.HubID == ".." {
				return fmt.Errorf("config: %s.hub_id is required: the hub's stable ID", at)
			}
			if ids[h.HubID] {
				return fmt.Errorf("config: %s.hub_id %q is another hub's too", at, h.HubID)
			}
			ids[h.HubID] = true
		}
		if err := h.checkOrigin(); err != nil {
			return fmt.Errorf("config: %s: %w", at, err)
		}
		if h.RedemptionTokenFile == "" {
			return fmt.Errorf("config: %s.redemption_token_file is required", at)
		}
		if h.ReadTokenFile != "" {
			if readers != "" {
				// ReceiptByProof names no hub: one hub serves the read scope.
				return fmt.Errorf("config: %s.read_token_file: only one hub serves the reservation read scope (%s does)", at, readers)
			}
			readers = h.Name
			if filepath.Clean(h.ReadTokenFile) == filepath.Clean(h.RedemptionTokenFile) {
				return fmt.Errorf("config: %s.read_token_file is the redemption credential's file; aimem issues a separate reservation.read credential", at)
			}
		}
		if tc, ok := h.TeamsConfig(c.ServiceID); ok {
			if _, err := hubteams.New(tc); err != nil {
				return fmt.Errorf("config: %s: %w", at, err)
			}
		}
	}
	return nil
}

// hubNameShape is a hub's alias.
var hubNameShape = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,31}$`)
