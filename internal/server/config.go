// Package server is aicrew's HTTPS service: the process that opens the store
// and answers requests over TLS it terminates itself. It has no plain-HTTP
// listener and no fallback.
package server

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"regexp"
	"strconv"
	"time"

	"github.com/BlackVS/aicrew/internal/aimemread"
	"github.com/BlackVS/aicrew/internal/verifier"
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
	// Aimem names the aimem hub that vouches for agents' proofs. Without it
	// the service refuses session entry and resume; everything else works.
	Aimem *AimemConfig `json:"aimem,omitempty"`
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

func (a AimemConfig) verifierConfig(serviceID string) verifier.Config {
	return verifier.Config{BaseURL: a.BaseURL, ServiceID: serviceID, TLSMode: a.TLSTrustMode,
		TLSValue: a.TLSTrustValue, TokenFile: a.RedemptionTokenFile}
}

func (a AimemConfig) readerConfig(serviceID string) aimemread.Config {
	return aimemread.Config{BaseURL: a.BaseURL, ServiceID: serviceID, TLSMode: a.TLSTrustMode,
		TLSValue: a.TLSTrustValue, TokenFile: a.ReadTokenFile, RedemptionTokenFile: a.RedemptionTokenFile}
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
	maxConfigBytes         = 64 << 10
	defaultShutdownTimeout = 15 * time.Second
	maxShutdownTimeout     = 5 * time.Minute
)

// serviceIDShape is the identity.v1 ID shape; aimem registers the service
// under this ID.
var serviceIDShape = regexp.MustCompile(`^[A-Za-z0-9._:-]{1,128}$`)

// LoadConfig reads and checks the configuration file at path. Unknown fields
// are refused. Errors name the field at fault, never a file's content.
func LoadConfig(path string) (Config, error) {
	f, err := os.Open(path)
	if err != nil {
		return Config{}, fmt.Errorf("open config: %w", err)
	}
	defer f.Close()
	raw, err := io.ReadAll(io.LimitReader(f, maxConfigBytes+1))
	if err != nil {
		return Config{}, fmt.Errorf("read config: %w", err)
	}
	if len(raw) > maxConfigBytes {
		return Config{}, fmt.Errorf("config is larger than %d bytes", maxConfigBytes)
	}
	return ParseConfig(raw)
}

// ParseConfig decodes and checks one configuration object.
func ParseConfig(raw []byte) (Config, error) {
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
		c.ShutdownTimeout = Duration(defaultShutdownTimeout)
	}
	return c, c.validate()
}

func (c Config) validate() error {
	for _, f := range []struct{ name, value string }{
		{"store_path", c.StorePath}, {"listen_addr", c.ListenAddr},
		{"tls_cert_file", c.TLSCertFile}, {"tls_key_file", c.TLSKeyFile},
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
	if c.Aimem != nil {
		if _, err := verifier.New(c.Aimem.verifierConfig(c.ServiceID)); err != nil {
			return fmt.Errorf("config: aimem: %w", err)
		}
		if c.Aimem.ReadTokenFile != "" {
			if _, err := aimemread.New(c.Aimem.readerConfig(c.ServiceID)); err != nil {
				return fmt.Errorf("config: aimem: %w", err)
			}
		}
	}
	return nil
}
