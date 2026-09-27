// Package verifier is aicrew's production store.Verifier: the client of
// aimem's receipt redemption, identity.v1 §2
// (POST /v1/identity/peers/{service_id}/redemptions).
//
// One call is one attempt: at most 10 s for connect, TLS and the reply, no
// retry, no redirect, no proxy and no connection reuse. The store's same-key
// retry owns recovery; a redemption is idempotent at aimem for the encoded
// request key. aimem's TLS identity is verified against the configured
// binding, a CA-backed DNS name or a pinned SHA-256 of the public key, and the
// peer bearer is read from a private file on every call. A reply is accepted
// only when it answers this exact request with an active, complete identity.
// Every failure is an *Error carrying the contract's refusal code and whether
// a retry with the same key may succeed. No error carries the bearer or the
// receipt.
package verifier

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/BlackVS/aicrew/internal/privatefile"
	"github.com/BlackVS/aicrew/internal/store"
)

const (
	// VersionHeader carries the identity.v1 protocol version.
	VersionHeader = "X-Aimem-Identity-Version"
	// Budget bounds one call: connect, TLS, request and the whole reply.
	Budget = 10 * time.Second
	// MaxReply is the largest reply body read, and the response header
	// limit; a longer body is refused without being parsed.
	MaxReply = 16384
	// maxCredential bounds the bearer file; the bearer is one line.
	maxCredential = 4096
)

// Trust modes for aimem's TLS identity.
const (
	TrustCADNS = "ca_dns"
	TrustSPKI  = "spki_sha256"
)

// Codes this client reports besides aimem's own refusal codes.
const (
	// CodeUnavailable: aimem could not be reached or its answer could not be
	// read. The outcome is unknown; a retry with the same key is safe.
	CodeUnavailable = "identity_unavailable"
	// CodeInvalidReply: aimem answered 200 with something that does not
	// answer this request. It is not retried.
	CodeInvalidReply = "invalid_reply"
)

var (
	idShape      = regexp.MustCompile(`^[A-Za-z0-9._:-]{1,128}$`)
	receiptShape = regexp.MustCompile(`^amr1_[A-Za-z0-9_-]{43}$`)

	// refusals are identity.v1's refusal codes a redemption can receive,
	// with whether a retry with the same key may succeed.
	refusals = map[string]bool{
		"invalid_request":      false,
		"unsupported_version":  false,
		"tls_required":         false,
		"peer_unauthenticated": false,
		"peer_unknown":         false,
		"peer_forbidden":       false,
		"proof_invalid":        false,
		"credential_inactive":  false,
		"idempotency_conflict": false,
		"rate_limited":         true,
		"request_in_progress":  true,
		"identity_unavailable": true,
	}

	errPinMismatch = errors.New("aimem's certificate does not match the configured SPKI pin")
)

// Error is a failed redemption. Code is an identity.v1 refusal code,
// CodeUnavailable or CodeInvalidReply; Reason is one fixed word naming what
// failed. Unwrap gives the caller's cancellation or deadline, for errors.Is,
// or why the credential file was refused.
type Error struct {
	Code      string
	Retryable bool
	Reason    string
	cause     error
}

func (e *Error) Error() string { return "aimem redemption: " + e.Code + " (" + e.Reason + ")" }
func (e *Error) Unwrap() error { return e.cause }

func unavailable(reason string) *Error {
	return &Error{Code: CodeUnavailable, Retryable: true, Reason: reason}
}

func invalidReply(reason string) *Error { return &Error{Code: CodeInvalidReply, Reason: reason} }

// Config names aimem and how to trust and authenticate to it.
type Config struct {
	// BaseURL is aimem's https origin, such as https://hub.example:8443,
	// with no path.
	BaseURL string
	// ServiceID is this aicrew service's peer ID at aimem.
	ServiceID string
	// TLSMode is TrustCADNS or TrustSPKI. For TrustCADNS, TLSValue is the
	// DNS name (the BaseURL host) the CA-issued certificate must carry. For
	// TrustSPKI, it is "sha256-" and the standard base64 of the SHA-256 of
	// aimem's certificate public key (SubjectPublicKeyInfo).
	TLSMode  string
	TLSValue string
	// TokenFile is the private file holding the redemption bearer.
	TokenFile string
}

// Client redeems receipts with one aimem hub. It implements store.Verifier.
type Client struct {
	cfg      Config
	endpoint string
	tls      *tls.Config
	budget   time.Duration
}

var _ store.Verifier = (*Client)(nil)

// New checks cfg without any network call and returns its client. The bearer
// file is read on each call, so a replaced file takes effect on the next one.
func New(cfg Config) (*Client, error) {
	return newClient(cfg, nil)
}

// newClient lets tests replace the system roots for ca_dns trust.
func newClient(cfg Config, roots *x509.CertPool) (*Client, error) {
	u, err := url.Parse(cfg.BaseURL)
	if err != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil ||
		u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || (u.Path != "" && u.Path != "/") {
		return nil, errors.New("verifier: the aimem URL must be an https origin without credentials, path, query or fragment")
	}
	if !idShape.MatchString(cfg.ServiceID) || cfg.ServiceID == "." || cfg.ServiceID == ".." {
		return nil, errors.New("verifier: the service ID is not a valid identity.v1 ID")
	}
	if cfg.TokenFile == "" {
		return nil, errors.New("verifier: no redemption credential file is configured")
	}
	tc := &tls.Config{MinVersion: tls.VersionTLS12}
	switch cfg.TLSMode {
	case TrustCADNS:
		if cfg.TLSValue != u.Hostname() {
			return nil, errors.New("verifier: ca_dns trust must name the aimem URL's host")
		}
		tc.RootCAs, tc.ServerName = roots, cfg.TLSValue
	case TrustSPKI:
		pin, err := decodePin(cfg.TLSValue)
		if err != nil {
			return nil, err
		}
		// The pin replaces the chain check, never drops it: the handshake
		// fails unless the leaf's public key hashes to the pin.
		tc.InsecureSkipVerify = true
		tc.VerifyConnection = func(cs tls.ConnectionState) error {
			if len(cs.PeerCertificates) == 0 {
				return errPinMismatch
			}
			sum := sha256.Sum256(cs.PeerCertificates[0].RawSubjectPublicKeyInfo)
			if subtle.ConstantTimeCompare(sum[:], pin) != 1 {
				return errPinMismatch
			}
			return nil
		}
	default:
		return nil, errors.New("verifier: the TLS trust mode must be ca_dns or spki_sha256")
	}
	origin := strings.TrimSuffix(cfg.BaseURL, "/")
	return &Client{
		cfg:      cfg,
		endpoint: origin + "/v1/identity/peers/" + cfg.ServiceID + "/redemptions",
		tls:      tc,
		budget:   Budget,
	}, nil
}

func decodePin(v string) ([]byte, error) {
	b64, ok := strings.CutPrefix(v, "sha256-")
	pin, err := base64.StdEncoding.DecodeString(b64)
	if !ok || err != nil || len(pin) != sha256.Size {
		return nil, errors.New("verifier: spki_sha256 trust must be sha256- followed by a base64 SHA-256")
	}
	return pin, nil
}

// IdempotencyKey encodes a request key for the Idempotency-Key header: "k1_"
// and the unpadded base64url SHA-256 of its UTF-8 bytes. The raw key never
// leaves aicrew.
func IdempotencyKey(requestKey string) string {
	sum := sha256.Sum256([]byte(requestKey))
	return "k1_" + base64.RawURLEncoding.EncodeToString(sum[:])
}

// readCredential returns the bearer in path, which must be a private file
// holding one line of visible ASCII. Errors never quote the content.
func readCredential(path string) (string, error) {
	if err := privatefile.Check(path); err != nil {
		return "", err
	}
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	raw, err := io.ReadAll(io.LimitReader(f, maxCredential+1))
	if err != nil {
		return "", err
	}
	if len(raw) > maxCredential {
		return "", fmt.Errorf("%s is larger than a credential", path)
	}
	s := strings.TrimSuffix(strings.TrimSuffix(string(raw), "\n"), "\r")
	if s == "" {
		return "", fmt.Errorf("%s is empty", path)
	}
	for i := 0; i < len(s); i++ {
		if s[i] < 0x21 || s[i] > 0x7e {
			return "", fmt.Errorf("%s must hold the credential alone on one line", path)
		}
	}
	return s, nil
}

type request struct {
	HubID       string `json:"hub_id"`
	ChallengeID string `json:"challenge_id"`
	Receipt     string `json:"receipt"`
}

// Redeem redeems one receipt and returns the identity aimem vouches for, or an
// *Error. ctx is the caller's: its cancellation ends the call at once, and a
// deadline longer than Budget is shortened to it.
func (c *Client) Redeem(ctx context.Context, req store.RedeemRequest) (store.VerifiedIdentity, error) {
	switch {
	case !idShape.MatchString(req.ChallengeID), !idShape.MatchString(req.HubID):
		return store.VerifiedIdentity{}, &Error{Code: "invalid_request", Reason: "request_ids"}
	case req.RequestKey == "" || !utf8.ValidString(req.RequestKey):
		return store.VerifiedIdentity{}, &Error{Code: "invalid_request", Reason: "request_key"}
	case !receiptShape.MatchString(req.Receipt.Reveal()):
		return store.VerifiedIdentity{}, &Error{Code: "proof_invalid", Reason: "receipt_shape"}
	}
	bearer, err := readCredential(c.cfg.TokenFile)
	if err != nil {
		// The cause names the file and the fix, never its content.
		e := unavailable("credential_file")
		e.cause = err
		return store.VerifiedIdentity{}, e
	}
	body, err := json.Marshal(request{HubID: req.HubID, ChallengeID: req.ChallengeID, Receipt: req.Receipt.Reveal()})
	if err != nil {
		return store.VerifiedIdentity{}, unavailable("request")
	}
	key := IdempotencyKey(req.RequestKey)

	callCtx, cancel := context.WithTimeout(ctx, c.budget)
	defer cancel()
	hreq, err := http.NewRequestWithContext(callCtx, http.MethodPost, c.endpoint, bytes.NewReader(body))
	if err != nil {
		return store.VerifiedIdentity{}, unavailable("request")
	}
	hreq.Header.Set("Authorization", "Bearer "+bearer)
	hreq.Header.Set("Idempotency-Key", key)
	hreq.Header.Set(VersionHeader, "1")
	hreq.Header.Set("Content-Type", "application/json")
	hreq.Header.Set("Accept", "application/json")
	resp, err := c.httpClient().Do(hreq)
	if err != nil {
		return store.VerifiedIdentity{}, transportFailure(ctx, callCtx, err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, MaxReply+1))
	if err != nil {
		return store.VerifiedIdentity{}, transportFailure(ctx, callCtx, err)
	}
	if len(data) > MaxReply {
		return store.VerifiedIdentity{}, unavailable("oversize")
	}
	if resp.StatusCode != http.StatusOK {
		return store.VerifiedIdentity{}, refusal(resp.StatusCode, data)
	}
	return c.verify(req, key, data)
}

// httpClient is one call's client: no proxy (the proxy environment is
// ignored), no redirect, no connection reuse.
func (c *Client) httpClient() *http.Client {
	return &http.Client{
		Transport: &http.Transport{
			Proxy: nil, TLSClientConfig: c.tls, DisableKeepAlives: true,
			ForceAttemptHTTP2: false, MaxResponseHeaderBytes: MaxReply,
		},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
}

// transportFailure classifies a failed exchange: the caller's cancellation,
// the call budget, aimem's TLS identity, or any other transport error. The
// outcome at aimem is unknown in every case, so each is retryable.
func transportFailure(caller, call context.Context, err error) *Error {
	var verr *tls.CertificateVerificationError
	var herr x509.HostnameError
	var uerr x509.UnknownAuthorityError
	switch {
	case caller.Err() != nil:
		e := unavailable("cancelled")
		e.cause = caller.Err()
		return e
	case call.Err() != nil:
		return unavailable("timeout")
	case errors.Is(err, errPinMismatch), errors.As(err, &verr), errors.As(err, &herr), errors.As(err, &uerr):
		return unavailable("tls_untrusted")
	}
	return unavailable("transport")
}

// refusal maps a non-200 reply. A refusal envelope with a known code keeps
// that code, and the contract's table decides retryability. Anything else,
// including a redirect, leaves the outcome unknown.
func refusal(status int, data []byte) *Error {
	if status >= 300 && status < 400 {
		return unavailable("redirect")
	}
	var env struct {
		Code string `json:"code"`
	}
	if json.Unmarshal(data, &env) != nil || env.Code == "" {
		return unavailable("status")
	}
	retryable, known := refusals[env.Code]
	if !known {
		return unavailable("unknown_code")
	}
	return &Error{Code: env.Code, Retryable: retryable, Reason: "refused"}
}

// reply holds every field of a redemption result. Pointers tell a missing
// (or null) field from an empty one.
type reply struct {
	RedemptionID  *string `json:"redemption_id"`
	RequestKey    *string `json:"request_key"`
	Replayed      *bool   `json:"replayed"`
	PeerServiceID *string `json:"peer_service_id"`
	ChallengeID   *string `json:"challenge_id"`
	Identity      *struct {
		HubID   *string `json:"hub_id"`
		UserID  *string `json:"user_id"`
		TokenID *string `json:"token_id"`
	} `json:"identity"`
	TokenState *string `json:"token_state"`
	RedeemedAt *string `json:"redeemed_at"`
}

func validID(p *string) bool { return p != nil && idShape.MatchString(*p) }

// verify decodes a 200 reply strictly and checks that it answers this
// request: the echoed key, peer and challenge, the requested hub, an active
// token and well-formed IDs. A replay is accepted like the original.
func (c *Client) verify(req store.RedeemRequest, key string, data []byte) (store.VerifiedIdentity, error) {
	var r reply
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&r); err != nil || dec.Decode(&struct{}{}) != io.EOF {
		return store.VerifiedIdentity{}, invalidReply("malformed")
	}
	switch {
	case r.RequestKey == nil || *r.RequestKey != key:
		return store.VerifiedIdentity{}, invalidReply("request_key")
	case r.PeerServiceID == nil || *r.PeerServiceID != c.cfg.ServiceID:
		return store.VerifiedIdentity{}, invalidReply("peer_service_id")
	case r.ChallengeID == nil || *r.ChallengeID != req.ChallengeID:
		return store.VerifiedIdentity{}, invalidReply("challenge_id")
	case r.Identity == nil || !validID(r.Identity.HubID) || !validID(r.Identity.UserID) || !validID(r.Identity.TokenID):
		return store.VerifiedIdentity{}, invalidReply("identity")
	case *r.Identity.HubID != req.HubID:
		return store.VerifiedIdentity{}, invalidReply("hub_id")
	case r.TokenState == nil || *r.TokenState != "active":
		return store.VerifiedIdentity{}, invalidReply("token_state")
	case !validID(r.RedemptionID) || r.Replayed == nil || r.RedeemedAt == nil:
		return store.VerifiedIdentity{}, invalidReply("malformed")
	}
	if _, err := time.Parse(time.RFC3339, *r.RedeemedAt); err != nil {
		return store.VerifiedIdentity{}, invalidReply("redeemed_at")
	}
	return store.VerifiedIdentity{HubID: *r.Identity.HubID, UserID: *r.Identity.UserID, TokenID: *r.Identity.TokenID}, nil
}
