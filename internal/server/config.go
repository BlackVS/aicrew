package server

import (
	"fmt"

	"github.com/BlackVS/aicrew/internal/aimemread"
	"github.com/BlackVS/aicrew/internal/hubteams"
	"github.com/BlackVS/aicrew/internal/svcconfig"
	"github.com/BlackVS/aicrew/internal/verifier"
)

// The configuration file is package svcconfig's; these names keep the
// service's own.
type (
	Config      = svcconfig.Config
	AimemHub    = svcconfig.AimemHub
	AimemConfig = svcconfig.AimemConfig
	Duration    = svcconfig.Duration
)

// LegacyHubName is the name the single block before 0.3.0 is read under.
const LegacyHubName = svcconfig.LegacyHubName

// LoadConfig reads and checks the configuration file at path: svcconfig's
// checks, then each hub's aimem clients built offline, as the service
// builds them. Errors name the field at fault, never a file's content.
func LoadConfig(path string) (Config, error) {
	c, err := svcconfig.LoadConfig(path)
	if err != nil {
		return Config{}, err
	}
	return c, checkClients(c)
}

// ParseConfig decodes and checks one configuration object as LoadConfig
// does.
func ParseConfig(raw []byte) (Config, error) {
	c, err := svcconfig.ParseConfig(raw)
	if err != nil {
		return Config{}, err
	}
	return c, checkClients(c)
}

// checkClients builds each hub's clients without any network call.
func checkClients(c Config) error {
	for i, h := range c.Hubs() {
		at := fmt.Sprintf("aimem_hubs[%d]", i)
		if c.Aimem != nil {
			at = "aimem"
		}
		if _, err := verifier.New(verifierConfig(h.AimemConfig, c.ServiceID)); err != nil {
			return fmt.Errorf("config: %s: %w", at, err)
		}
		if h.ReadTokenFile != "" {
			if _, err := aimemread.New(readerConfig(h.AimemConfig, c.ServiceID)); err != nil {
				return fmt.Errorf("config: %s: %w", at, err)
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

func verifierConfig(a AimemConfig, serviceID string) verifier.Config {
	return verifier.Config{BaseURL: a.BaseURL, ServiceID: serviceID, TLSMode: a.TLSTrustMode,
		TLSValue: a.TLSTrustValue, TokenFile: a.RedemptionTokenFile}
}

func readerConfig(a AimemConfig, serviceID string) aimemread.Config {
	return aimemread.Config{BaseURL: a.BaseURL, ServiceID: serviceID, TLSMode: a.TLSTrustMode,
		TLSValue: a.TLSTrustValue, TokenFile: a.ReadTokenFile, RedemptionTokenFile: a.RedemptionTokenFile}
}
