package agent

import (
	"bytes"
	"context"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// The client session API's token types (docs/CREW-CONTRACT.md, "Standards
// mapping").
const (
	grantTokenExchange = "urn:ietf:params:oauth:grant-type:token-exchange"
	accessTokenType    = "urn:ietf:params:oauth:token-type:access_token"
	proofTokenType     = "https://github.com/BlackVS/aicrew/blob/main/docs/CREW-CONTRACT.md#token-type-aimem-proof-receipt"
	handleTokenType    = "https://github.com/BlackVS/aicrew/blob/main/docs/CREW-CONTRACT.md#token-type-aimem-handle"

	// requestTimeout bounds one call to aicrewd. When it passes, the
	// connection is closed: the attempt is abandoned before any retry.
	requestTimeout = 30 * time.Second
	maxReply       = 64 << 10
)

// Refusal is aicrewd's refusal envelope. Its texts are aicrewd's fixed ones
// and never carry a secret.
type Refusal struct {
	Status     int
	Code       string
	Message    string
	NextAction string
	Retryable  bool
	RetryAfter time.Duration
}

func (r *Refusal) Error() string {
	return fmt.Sprintf("aicrew refused (%s): %s Next: %s", r.Code, r.Message, r.NextAction)
}

// TransportError is a call whose outcome is unknown: aicrewd was not
// reached, or its answer was not read. A retry with the same key is safe.
type TransportError struct{ Err error }

func (e *TransportError) Error() string { return "aicrew unreachable: " + e.Err.Error() }
func (e *TransportError) Unwrap() error { return e.Err }

// Retryable reports whether a retry may succeed: an unknown outcome, or a
// refusal aicrewd marks retryable.
func Retryable(err error) bool {
	var r *Refusal
	var t *TransportError
	return errors.As(err, &t) || (errors.As(err, &r) && r.Retryable)
}

// codeOf is the refusal code of err, or "".
func codeOf(err error) string {
	var r *Refusal
	if errors.As(err, &r) {
		return r.Code
	}
	return ""
}

// Challenge is an identity-proof challenge.
type Challenge struct {
	ID        string
	HubID     string
	ServiceID string
	ExpiresAt time.Time
}

// Session is the session a reply describes.
type Session struct {
	ID         string `json:"id"`
	TeamID     string `json:"team_id"`
	AgentID    string `json:"agent_id"`
	Role       string `json:"role"`
	State      string `json:"state"`
	Generation string `json:"generation"`
}

// Entry is what an entry or resume returns. Token and Handle are secrets.
type Entry struct {
	Session       Session
	Token         string
	TokenExpires  time.Time
	Handle        string
	HandleExpires time.Time
}

// Handle is a refreshed aimem-scoped handle; Value is a secret.
type Handle struct {
	Value   string
	Expires time.Time
}

// CrewAPI is the client session API as the engine uses it.
type CrewAPI interface {
	Challenge(ctx context.Context, key, agentID string) (Challenge, error)
	Enter(ctx context.Context, key, receipt, audience, challengeID, teamID, sessionID string) (Entry, error)
	Refresh(ctx context.Context, key, token, hubID string) (Handle, error)
	Leave(ctx context.Context, key, token string) (Session, error)
}

// Crew calls aicrewd over TLS verified against the configured binding.
type Crew struct {
	base   string
	client *http.Client
	now    func() time.Time
}

// NewCrew builds the client of cfg's aicrewd. roots replaces the system
// roots for ca_dns trust; nil means the system roots.
func NewCrew(cfg Config, roots *x509.CertPool) (*Crew, error) {
	tc, err := cfg.Trust.ClientConfig(roots)
	if err != nil {
		return nil, err
	}
	return &Crew{
		base: strings.TrimSuffix(cfg.URL, "/"),
		client: &http.Client{
			Timeout: requestTimeout,
			Transport: &http.Transport{Proxy: nil, TLSClientConfig: tc, DisableKeepAlives: true,
				MaxResponseHeaderBytes: maxReply},
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
		now: time.Now,
	}, nil
}

func (c *Crew) do(ctx context.Context, method, path, contentType, key, bearer string, body []byte, out any) error {
	ctx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, bytes.NewReader(body))
	if err != nil {
		return err
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	if key != "" {
		req.Header.Set("Idempotency-Key", key)
	}
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	req.Header.Set("Accept", "application/json")
	resp, err := c.client.Do(req)
	if err != nil {
		return &TransportError{Err: errors.New(scrub(err))}
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxReply+1))
	if err != nil || len(raw) > maxReply {
		return &TransportError{Err: errors.New("the reply could not be read")}
	}
	if resp.StatusCode != http.StatusOK {
		r := &Refusal{Status: resp.StatusCode}
		var env struct {
			Code       string `json:"code"`
			Message    string `json:"message"`
			NextAction string `json:"next_action"`
			Retryable  bool   `json:"retryable"`
		}
		if json.Unmarshal(raw, &env) != nil || env.Code == "" {
			// No envelope: a proxy or a server error; the outcome is unknown.
			return &TransportError{Err: fmt.Errorf("aicrewd answered %d without a refusal", resp.StatusCode)}
		}
		r.Code, r.Message, r.NextAction, r.Retryable = env.Code, env.Message, env.NextAction, env.Retryable
		if s, err := strconv.Atoi(resp.Header.Get("Retry-After")); err == nil && s > 0 {
			r.RetryAfter = time.Duration(s) * time.Second
		}
		return r
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return &TransportError{Err: errors.New("the reply is not the expected JSON")}
	}
	return nil
}

// scrub keeps a transport error's kind without the request line; Go's URL
// errors quote the URL, which carries no secret here, but nothing else is
// needed.
func scrub(err error) string {
	var uerr *url.Error
	if errors.As(err, &uerr) {
		return uerr.Op + ": " + uerr.Err.Error()
	}
	return err.Error()
}

func (c *Crew) Challenge(ctx context.Context, key, agentID string) (Challenge, error) {
	body, _ := json.Marshal(map[string]string{"agent_id": agentID})
	var out struct {
		ChallengeID string `json:"challenge_id"`
		HubID       string `json:"hub_id"`
		ServiceID   string `json:"service_id"`
		ExpiresAt   string `json:"expires_at"`
	}
	if err := c.do(ctx, http.MethodPost, "/v1/crew/challenges", "application/json", key, "", body, &out); err != nil {
		return Challenge{}, err
	}
	exp, err := time.Parse(time.RFC3339, out.ExpiresAt)
	if err != nil || out.ChallengeID == "" || out.HubID == "" || out.ServiceID == "" {
		return Challenge{}, &TransportError{Err: errors.New("the challenge reply is incomplete")}
	}
	return Challenge{ID: out.ChallengeID, HubID: out.HubID, ServiceID: out.ServiceID, ExpiresAt: exp}, nil
}

func (c *Crew) Enter(ctx context.Context, key, receipt, audience, challengeID, teamID, sessionID string) (Entry, error) {
	form := url.Values{"grant_type": {grantTokenExchange}, "subject_token": {receipt},
		"subject_token_type": {proofTokenType}, "audience": {audience},
		"requested_token_type": {accessTokenType}, "challenge_id": {challengeID}}
	if sessionID != "" {
		form.Set("session_id", sessionID)
	} else {
		form.Set("team_id", teamID)
	}
	var out struct {
		AccessToken          string  `json:"access_token"`
		TokenType            string  `json:"token_type"`
		ExpiresIn            int64   `json:"expires_in"`
		Session              Session `json:"session"`
		AimemHandle          string  `json:"aimem_handle"`
		AimemHandleExpiresIn int64   `json:"aimem_handle_expires_in"`
	}
	start := c.now()
	if err := c.do(ctx, http.MethodPost, "/v1/crew/token", "application/x-www-form-urlencoded", key, "",
		[]byte(form.Encode()), &out); err != nil {
		return Entry{}, err
	}
	if out.AccessToken == "" || out.AimemHandle == "" || out.Session.ID == "" || out.TokenType != "Bearer" {
		return Entry{}, &TransportError{Err: errors.New("the entry reply is incomplete")}
	}
	// Expiries count from when the request was sent, so they are never
	// later than aicrewd's.
	return Entry{Session: out.Session, Token: out.AccessToken,
		TokenExpires: start.Add(time.Duration(out.ExpiresIn) * time.Second),
		Handle:       out.AimemHandle, HandleExpires: start.Add(time.Duration(out.AimemHandleExpiresIn) * time.Second)}, nil
}

func (c *Crew) Refresh(ctx context.Context, key, token, hubID string) (Handle, error) {
	form := url.Values{"grant_type": {grantTokenExchange}, "subject_token": {token},
		"subject_token_type": {accessTokenType}, "audience": {hubID}, "requested_token_type": {handleTokenType}}
	var out struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int64  `json:"expires_in"`
	}
	start := c.now()
	if err := c.do(ctx, http.MethodPost, "/v1/crew/token", "application/x-www-form-urlencoded", key, "",
		[]byte(form.Encode()), &out); err != nil {
		return Handle{}, err
	}
	if out.AccessToken == "" {
		return Handle{}, &TransportError{Err: errors.New("the refresh reply is incomplete")}
	}
	return Handle{Value: out.AccessToken, Expires: start.Add(time.Duration(out.ExpiresIn) * time.Second)}, nil
}

func (c *Crew) Leave(ctx context.Context, key, token string) (Session, error) {
	var out Session
	err := c.do(ctx, http.MethodPost, "/v1/crew/session/leave", "application/json", key, token, []byte("{}"), &out)
	return out, err
}
