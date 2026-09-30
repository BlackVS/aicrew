// Package aimemread is aicrew's production store.ReservationReader: the
// client of aimem's read-only reservation scope for this service
// (coordination.v1 §2, with C5c-w's closure evidence):
//
//	GET /v1/identity/peers/{service_id}/reservation-receipts/{p1}
//	GET /v1/identity/peers/{service_id}/reservations/{task_id}/receipts/{operation}/{k1}
//	GET /v1/identity/peers/{service_id}/reservations/{task_id}
//
// One call is one attempt: at most 10 s for connect, TLS and the reply, no
// retry, no redirect, no proxy and no connection reuse. aimem's TLS identity
// is verified against the configured binding, and the reservation.read bearer
// is read from its own private file on every call. An answer is accepted only
// in the scope's exact shapes; anything else, a refusal or a transport
// failure is an *Error, never "none", so a step stays pending rather than
// settle on a misread. No error carries the bearer.
package aimemread

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/BlackVS/aicrew/internal/privatefile"
	"github.com/BlackVS/aicrew/internal/store"
	"github.com/BlackVS/aicrew/internal/tlstrust"
)

const (
	// VersionHeader carries the reservation read scope's protocol version.
	VersionHeader = "X-Aimem-Reservation-Version"
	// Budget bounds one call: connect, TLS, request and the whole reply.
	Budget = 10 * time.Second
	// MaxReply is the largest reply body read, and the response header
	// limit; a longer body is refused without being parsed.
	MaxReply = 16384
	// maxCredential bounds a bearer file; a bearer is one line.
	maxCredential = 4096
)

// Codes this client reports besides aimem's own refusal codes.
const (
	// CodeUnavailable: aimem could not be reached, or its answer could not
	// be read. Nothing is known; a later read may succeed.
	CodeUnavailable = "read_unavailable"
	// CodeInvalidReply: aimem answered 200 with something that is not the
	// scope's answer to this request. It is not retried.
	CodeInvalidReply = "invalid_reply"
)

var (
	idShape         = regexp.MustCompile(`^[A-Za-z0-9._:-]{1,128}$`)
	digestShape     = regexp.MustCompile(`^(p1|k1)_[A-Za-z0-9_-]{43}$`)
	credentialShape = regexp.MustCompile(`^aimem_peer_[0-9a-f]{64}$`)
	fenceShape      = regexp.MustCompile(`^[1-9][0-9]{0,18}$`)

	// refusals are the read scope's refusal codes, with whether a later
	// read may succeed.
	refusals = map[string]bool{
		"invalid_request":      false,
		"unsupported_version":  false,
		"tls_required":         false,
		"peer_unauthenticated": false,
		"peer_unknown":         false,
		"peer_forbidden":       false,
		"credential_inactive":  false,
		"rate_limited":         true,
		"request_in_progress":  true,
	}

	// closedBy are the ways the scope reports a reservation closed.
	closedBy = map[string]bool{"holder_release": true, "holder_finalize": true,
		"recovery_release": true, "recovery_cancel": true}
)

// Error is a failed read. Code is a read-scope refusal code, CodeUnavailable
// or CodeInvalidReply; Reason is one fixed word naming what failed.
// RetryAfter is aimem's Retry-After on a refusal that may succeed later, or
// zero. Unwrap gives the caller's cancellation, for errors.Is, or why a
// credential file was refused.
type Error struct {
	Code       string
	Retryable  bool
	Reason     string
	RetryAfter time.Duration
	cause      error
}

func (e *Error) Error() string { return "aimem read scope: " + e.Code + " (" + e.Reason + ")" }
func (e *Error) Unwrap() error { return e.cause }

func unavailable(reason string) *Error {
	return &Error{Code: CodeUnavailable, Retryable: true, Reason: reason}
}

func invalidReply(reason string) *Error { return &Error{Code: CodeInvalidReply, Reason: reason} }

// Config names aimem and how to trust and authenticate to it.
type Config struct {
	// BaseURL is aimem's https origin, with no path.
	BaseURL string
	// ServiceID is this aicrew service's peer ID at aimem.
	ServiceID string
	// TLSMode and TLSValue are the TLS binding, as for the verifier.
	TLSMode  string
	TLSValue string
	// TokenFile is the private file holding the reservation.read bearer.
	TokenFile string
	// RedemptionTokenFile is the redemption bearer's file, which the read
	// credential must never be.
	RedemptionTokenFile string
}

// Client reads aimem's reservation scope. It implements
// store.ReservationReader.
type Client struct {
	cfg    Config
	origin string
	tls    *tls.Config
	budget time.Duration
}

var _ store.ReservationReader = (*Client)(nil)

// New checks cfg without any network call and returns its client.
func New(cfg Config) (*Client, error) {
	return newClient(cfg, nil)
}

// newClient lets tests replace the system roots for ca_dns trust.
func newClient(cfg Config, roots *x509.CertPool) (*Client, error) {
	u, err := url.Parse(cfg.BaseURL)
	if err != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil ||
		u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || (u.Path != "" && u.Path != "/") {
		return nil, errors.New("read scope: the aimem URL must be an https origin without credentials, path, query or fragment")
	}
	if !validID(cfg.ServiceID) {
		return nil, errors.New("read scope: the service ID is not a valid identity.v1 ID")
	}
	if cfg.TokenFile == "" {
		return nil, errors.New("read scope: no read credential file is configured")
	}
	if cfg.RedemptionTokenFile != "" && samePath(cfg.TokenFile, cfg.RedemptionTokenFile) {
		return nil, errors.New("read scope: the read credential file is the redemption credential's; aimem issues a separate reservation.read credential")
	}
	trust := tlstrust.Binding{Mode: cfg.TLSMode, Value: cfg.TLSValue}
	if err := trust.Check(u.Hostname()); err != nil {
		return nil, fmt.Errorf("read scope: %w", err)
	}
	tc, err := trust.ClientConfig(roots)
	if err != nil {
		return nil, fmt.Errorf("read scope: %w", err)
	}
	return &Client{cfg: cfg, origin: strings.TrimSuffix(cfg.BaseURL, "/"), tls: tc, budget: Budget}, nil
}

// validID is an identity.v1 ID that is safe as one path segment: never "."
// or "..".
func validID(id string) bool { return idShape.MatchString(id) && id != "." && id != ".." }

// samePath reports whether a and b name the same file, by path or, when both
// exist, by identity.
func samePath(a, b string) bool {
	if filepath.Clean(a) == filepath.Clean(b) {
		return true
	}
	ia, errA := os.Stat(a)
	ib, errB := os.Stat(b)
	return errA == nil && errB == nil && os.SameFile(ia, ib)
}

// CheckCredential reads the read bearer as a call would, and refuses a
// missing, readable-by-others or malformed file, one that is the redemption
// credential's file, or one holding the redemption credential's secret. Its
// errors name the file and the fix, never the content.
func (c *Client) CheckCredential() error {
	bearer, err := readCredential(c.cfg.TokenFile)
	if err != nil {
		return err
	}
	if !credentialShape.MatchString(bearer) {
		return fmt.Errorf("%s does not hold a peer credential (aimem_peer_ and 64 lowercase hex)", c.cfg.TokenFile)
	}
	if c.cfg.RedemptionTokenFile == "" {
		return nil
	}
	if samePath(c.cfg.TokenFile, c.cfg.RedemptionTokenFile) {
		return fmt.Errorf("%s is the redemption credential's file; use the separate reservation.read credential", c.cfg.TokenFile)
	}
	if other, err := readCredential(c.cfg.RedemptionTokenFile); err == nil && other == bearer {
		return fmt.Errorf("%s holds the redemption credential; use the separate reservation.read credential", c.cfg.TokenFile)
	}
	return nil
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

// ReceiptByProof returns the transition committed under a proof's p1_
// digest, or state "none".
func (c *Client) ReceiptByProof(ctx context.Context, proofDigest string) (store.ScopeReceiptLookup, error) {
	if !digestShape.MatchString(proofDigest) || !strings.HasPrefix(proofDigest, "p1_") {
		return store.ScopeReceiptLookup{}, &Error{Code: "invalid_request", Reason: "proof_digest"}
	}
	var out store.ScopeReceiptLookup
	if err := c.get(ctx, "reservation-receipts/"+proofDigest, &out); err != nil {
		return store.ScopeReceiptLookup{}, err
	}
	return out, checkLookup(out, "", "")
}

// ReceiptByKey returns the transition committed under a request key's k1_
// digest for op on task, or state "none".
func (c *Client) ReceiptByKey(ctx context.Context, task store.TaskRef, op store.ReservationOp, keyDigest string) (store.ScopeReceiptLookup, error) {
	if !digestShape.MatchString(keyDigest) || !strings.HasPrefix(keyDigest, "k1_") {
		return store.ScopeReceiptLookup{}, &Error{Code: "invalid_request", Reason: "key_digest"}
	}
	if !validID(task.TaskID) || !validID(string(op)) {
		return store.ScopeReceiptLookup{}, &Error{Code: "invalid_request", Reason: "task_or_operation"}
	}
	var out store.ScopeReceiptLookup
	path := "reservations/" + url.PathEscape(task.TaskID) + "/receipts/" + url.PathEscape(string(op)) + "/" + keyDigest
	if err := c.get(ctx, path, &out); err != nil {
		return store.ScopeReceiptLookup{}, err
	}
	if err := checkLookup(out, task.TaskID, keyDigest); err != nil {
		return store.ScopeReceiptLookup{}, err
	}
	if out.Receipt != nil && out.Receipt.Operation != string(op) {
		return store.ScopeReceiptLookup{}, invalidReply("receipt_operation")
	}
	return out, nil
}

// HoldStatus returns task's hold under this service's proof ("held"), this
// service's most recent reservation on it if it is no longer active
// ("closed"), or "none".
func (c *Client) HoldStatus(ctx context.Context, task store.TaskRef) (store.ScopeHold, error) {
	if !validID(task.TaskID) {
		return store.ScopeHold{}, &Error{Code: "invalid_request", Reason: "task"}
	}
	var out store.ScopeHold
	if err := c.get(ctx, "reservations/"+url.PathEscape(task.TaskID), &out); err != nil {
		return store.ScopeHold{}, err
	}
	return out, checkHold(out)
}

// checkLookup accepts a receipt answer only in the scope's shape: "none"
// alone, or "committed" with a complete receipt for this task and key when
// they are known.
func checkLookup(l store.ScopeReceiptLookup, taskID, keyDigest string) error {
	switch l.State {
	case store.ScopeNone:
		if l.Receipt != nil {
			return invalidReply("none_with_receipt")
		}
		return nil
	case store.ScopeCommitted:
	default:
		return invalidReply("receipt_state")
	}
	r := l.Receipt
	switch {
	case r == nil:
		return invalidReply("receipt_missing")
	case r.ID == "" || r.Operation == "" || !idShape.MatchString(r.TaskID) || r.ReservationID == "" ||
		!fenceShape.MatchString(r.Fence) || r.TaskRevision <= 0 || r.CommittedAt == "" ||
		!digestShape.MatchString(r.RequestKeyDigest) || !strings.HasPrefix(r.RequestKeyDigest, "k1_"):
		return invalidReply("receipt_fields")
	case taskID != "" && r.TaskID != taskID:
		return invalidReply("receipt_task")
	case keyDigest != "" && r.RequestKeyDigest != keyDigest:
		return invalidReply("receipt_key")
	}
	if _, err := time.Parse(time.RFC3339, r.CommittedAt); err != nil {
		return invalidReply("receipt_time")
	}
	return nil
}

// checkHold accepts a hold answer only in the scope's shape.
func checkHold(h store.ScopeHold) error {
	switch h.State {
	case store.ScopeNone:
		if h != (store.ScopeHold{State: store.ScopeNone}) {
			return invalidReply("none_with_fields")
		}
	case store.ScopeHeld:
		if h.ReservationID == "" || !fenceShape.MatchString(h.Fence) || h.TaskRevision <= 0 ||
			h.ClosingFence != "" || h.ClosedBy != "" || h.ClosedAt != "" {
			return invalidReply("held_fields")
		}
	case store.ScopeClosed:
		if h.ReservationID == "" || !fenceShape.MatchString(h.ClosingFence) || !closedBy[h.ClosedBy] ||
			h.TaskRevision <= 0 || h.Fence != "" || h.HolderMode != "" || h.OwnWorkRef != "" {
			return invalidReply("closed_fields")
		}
		if _, err := time.Parse(time.RFC3339, h.ClosedAt); err != nil {
			return invalidReply("closed_time")
		}
	default:
		return invalidReply("hold_state")
	}
	return nil
}

// get reads one scope path under this service and decodes its answer into
// out, strictly.
func (c *Client) get(ctx context.Context, path string, out any) error {
	bearer, err := readCredential(c.cfg.TokenFile)
	if err != nil {
		e := unavailable("credential_file")
		e.cause = err
		return e
	}
	callCtx, cancel := context.WithTimeout(ctx, c.budget)
	defer cancel()
	endpoint := c.origin + "/v1/identity/peers/" + url.PathEscape(c.cfg.ServiceID) + "/" + path
	hreq, err := http.NewRequestWithContext(callCtx, http.MethodGet, endpoint, nil)
	if err != nil {
		return unavailable("request")
	}
	hreq.Header.Set("Authorization", "Bearer "+bearer)
	hreq.Header.Set(VersionHeader, "1")
	hreq.Header.Set("Accept", "application/json")
	resp, err := c.httpClient().Do(hreq)
	if err != nil {
		return transportFailure(ctx, callCtx, err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, MaxReply+1))
	if err != nil {
		return transportFailure(ctx, callCtx, err)
	}
	if len(data) > MaxReply {
		return unavailable("oversize")
	}
	if resp.StatusCode != http.StatusOK {
		return refusal(resp, data)
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(out); err != nil {
		return invalidReply("shape")
	}
	if dec.Decode(&struct{}{}) != io.EOF {
		return invalidReply("trailing_data")
	}
	return nil
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
// the call budget, aimem's TLS identity, or any other transport error.
func transportFailure(caller, call context.Context, err error) *Error {
	switch {
	case caller.Err() != nil:
		e := unavailable("cancelled")
		e.cause = caller.Err()
		return e
	case call.Err() != nil:
		return unavailable("timeout")
	case tlstrust.Untrusted(err):
		return unavailable("tls_untrusted")
	}
	return unavailable("transport")
}

// refusal maps a non-200 reply. A refusal envelope with a known code keeps
// that code, and the scope's table decides whether a later read may
// succeed; a retryable one carries aimem's Retry-After. Anything else,
// including a redirect, is unavailable.
func refusal(resp *http.Response, data []byte) *Error {
	if resp.StatusCode >= 300 && resp.StatusCode < 400 {
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
	e := &Error{Code: env.Code, Retryable: retryable, Reason: "refused"}
	if retryable {
		if s, err := strconv.Atoi(resp.Header.Get("Retry-After")); err == nil && s > 0 && s <= 3600 {
			e.RetryAfter = time.Duration(s) * time.Second
		}
	}
	return e
}
