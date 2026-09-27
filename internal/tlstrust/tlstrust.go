// Package tlstrust verifies a peer's TLS identity against a configured
// binding: a CA-issued certificate for a DNS name (ca_dns) or a pinned
// SHA-256 of the peer's public key (spki_sha256). Plain HTTP, trust on first
// use and disabled verification cannot be expressed. aicrew uses it towards
// aimem (internal/verifier) and an agent's client uses it towards aicrewd.
package tlstrust

import (
	"crypto/sha256"
	"crypto/subtle"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"errors"
	"strings"
)

// Trust modes.
const (
	CADNS = "ca_dns"
	SPKI  = "spki_sha256"
)

// ErrPinMismatch fails a handshake whose leaf key does not hash to the pin.
var ErrPinMismatch = errors.New("the peer's certificate does not match the configured SPKI pin")

// Binding is one peer's trust: for CADNS, Value is the DNS name (or IP) the
// CA-issued certificate must carry, and it must be the host of the peer's
// URL; for SPKI, Value is "sha256-" and the standard base64 of the SHA-256
// of the peer's SubjectPublicKeyInfo.
type Binding struct {
	Mode  string
	Value string
}

// Check validates b for a peer reached at host, without any network call.
func (b Binding) Check(host string) error {
	switch b.Mode {
	case CADNS:
		if b.Value == "" || b.Value != host {
			return errors.New("ca_dns trust must name the URL's host")
		}
		return nil
	case SPKI:
		_, err := decodePin(b.Value)
		return err
	}
	return errors.New("the TLS trust mode must be ca_dns or spki_sha256")
}

// ClientConfig is the TLS configuration of a client to the peer. roots
// replaces the system roots for CADNS; nil means the system roots.
func (b Binding) ClientConfig(roots *x509.CertPool) (*tls.Config, error) {
	cfg := &tls.Config{MinVersion: tls.VersionTLS12}
	switch b.Mode {
	case CADNS:
		if b.Value == "" {
			return nil, errors.New("ca_dns trust needs a host name")
		}
		cfg.RootCAs, cfg.ServerName = roots, b.Value
	case SPKI:
		pin, err := decodePin(b.Value)
		if err != nil {
			return nil, err
		}
		// The pin replaces the chain check, never drops it: the handshake
		// fails unless the leaf's public key hashes to the pin.
		cfg.InsecureSkipVerify = true
		cfg.VerifyConnection = func(cs tls.ConnectionState) error {
			if len(cs.PeerCertificates) == 0 {
				return ErrPinMismatch
			}
			sum := sha256.Sum256(cs.PeerCertificates[0].RawSubjectPublicKeyInfo)
			if subtle.ConstantTimeCompare(sum[:], pin) != 1 {
				return ErrPinMismatch
			}
			return nil
		}
	default:
		return nil, errors.New("the TLS trust mode must be ca_dns or spki_sha256")
	}
	return cfg, nil
}

func decodePin(v string) ([]byte, error) {
	b64, ok := strings.CutPrefix(v, "sha256-")
	pin, err := base64.StdEncoding.DecodeString(b64)
	if !ok || err != nil || len(pin) != sha256.Size {
		return nil, errors.New("spki_sha256 trust must be sha256- followed by a base64 SHA-256")
	}
	return pin, nil
}

// Untrusted reports whether err is a failure to verify the peer's identity.
func Untrusted(err error) bool {
	var verr *tls.CertificateVerificationError
	var herr x509.HostnameError
	var uerr x509.UnknownAuthorityError
	return errors.Is(err, ErrPinMismatch) || errors.As(err, &verr) || errors.As(err, &herr) || errors.As(err, &uerr)
}
