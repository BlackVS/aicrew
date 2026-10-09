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
	// maxInboxReply bounds an inbox page's reply: aicrewd keeps a page's
	// JSON within 128 KiB (one message may reach about 100 KiB).
	maxInboxReply = 256 << 10
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
	_, _, err := c.exchange(ctx, method, path, contentType, key, bearer, body, out, http.StatusOK)
	return err
}

// exchange sends one request and decodes a reply with one of the accepted
// statuses into out, returning the status and the reply's headers. Any other
// status is aicrewd's refusal, or a transport error when it carries no
// envelope.
func (c *Crew) exchange(ctx context.Context, method, path, contentType, key, bearer string, body []byte, out any,
	accepted ...int) (int, http.Header, error) {
	return c.exchangeUpTo(ctx, maxReply, method, path, contentType, key, bearer, body, out, accepted...)
}

// exchangeUpTo is exchange with a reply of at most limit bytes.
func (c *Crew) exchangeUpTo(ctx context.Context, limit int, method, path, contentType, key, bearer string, body []byte,
	out any, accepted ...int) (int, http.Header, error) {
	ctx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, bytes.NewReader(body))
	if err != nil {
		return 0, nil, err
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
		return 0, nil, &TransportError{Err: errors.New(scrub(err))}
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, int64(limit)+1))
	if err != nil || len(raw) > limit {
		return 0, nil, &TransportError{Err: errors.New("the reply could not be read")}
	}
	ok := false
	for _, s := range accepted {
		ok = ok || resp.StatusCode == s
	}
	if !ok {
		r := &Refusal{Status: resp.StatusCode}
		var env struct {
			Code       string `json:"code"`
			Message    string `json:"message"`
			NextAction string `json:"next_action"`
			Retryable  bool   `json:"retryable"`
		}
		if json.Unmarshal(raw, &env) != nil || env.Code == "" {
			// No envelope: a proxy or a server error; the outcome is unknown.
			return 0, nil, &TransportError{Err: fmt.Errorf("aicrewd answered %d without a refusal", resp.StatusCode)}
		}
		r.Code, r.Message, r.NextAction, r.Retryable = env.Code, env.Message, env.NextAction, env.Retryable
		if s, err := strconv.Atoi(resp.Header.Get("Retry-After")); err == nil && s > 0 {
			r.RetryAfter = time.Duration(s) * time.Second
		}
		return 0, nil, r
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return 0, nil, &TransportError{Err: errors.New("the reply is not the expected JSON")}
	}
	return resp.StatusCode, resp.Header, nil
}

// attemptsPath is the step routes' root (docs/CREW-CONTRACT.md, "Attempt
// steps").
const attemptsPath = "/v1/crew/attempts"

// BeginStep sends one begin route. The attempt is the one the reply's
// Location names.
func (c *Crew) BeginStep(ctx context.Context, key, token, path string, body []byte) (Step, string, error) {
	var st Step
	_, h, err := c.exchange(ctx, http.MethodPost, path, "application/json", key, token, body, &st, http.StatusOK)
	if err != nil {
		return Step{}, "", err
	}
	id, ok := strings.CutPrefix(h.Get("Location"), attemptsPath+"/")
	if !ok || id == "" || strings.Contains(id, "/") || st.RequestKey == "" {
		return Step{}, "", &TransportError{Err: errors.New("the begin reply named no attempt or step")}
	}
	return st, id, nil
}

// SettleStep reports a step. A pending step (202) is a Settlement with
// RetryAfter, not an error.
func (c *Crew) SettleStep(ctx context.Context, token, attemptID, requestKey string, report StepReport) (Settlement, error) {
	body, _ := json.Marshal(struct {
		RequestKey string `json:"request_key"`
		StepReport
	}{requestKey, report})
	var reply struct {
		Settled bool   `json:"settled"`
		Outcome string `json:"outcome"`
	}
	status, h, err := c.exchange(ctx, http.MethodPost, attemptsPath+"/"+url.PathEscape(attemptID)+"/settle",
		"application/json", "", token, body, &reply, http.StatusOK, http.StatusAccepted)
	if err != nil {
		return Settlement{}, err
	}
	set := Settlement{Settled: status == http.StatusOK && reply.Settled, Outcome: reply.Outcome}
	if s, err := strconv.Atoi(h.Get("Retry-After")); err == nil && s > 0 {
		set.RetryAfter = time.Duration(s) * time.Second
	}
	return set, nil
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

// LocalStep sends a local step (decline, review, stop, confirm-stop,
// confirm-delivery) and returns aicrewd's answer, the attempt.
func (c *Crew) LocalStep(ctx context.Context, key, token, path string, body []byte) (json.RawMessage, error) {
	var out json.RawMessage
	if err := c.do(ctx, http.MethodPost, path, "application/json", key, token, body, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// Read is one session-API read as the token's session: the requirements
// or the team's capabilities.
func (c *Crew) Read(ctx context.Context, token, path string) (json.RawMessage, error) {
	var out json.RawMessage
	if _, _, err := c.exchangeUpTo(ctx, maxInboxReply, http.MethodGet, path, "", "", token, nil, &out, http.StatusOK); err != nil {
		return nil, err
	}
	return out, nil
}

// inboxPath is aicrewd's member inbox (pilot G1).
const inboxPath = "/v1/crew/inbox"

// escalationsPath is where the team's coordinator raises an escalation.
const escalationsPath = "/v1/crew/escalations"

// Inbox reads a page of the member's oldest unacknowledged messages, which
// aicrewd records as delivered.
func (c *Crew) Inbox(ctx context.Context, token string, limit int) (json.RawMessage, error) {
	var out json.RawMessage
	if _, _, err := c.exchangeUpTo(ctx, maxInboxReply, http.MethodGet, inboxPath+"?limit="+strconv.Itoa(limit), "", "",
		token, nil, &out, http.StatusOK); err != nil {
		return nil, err
	}
	return out, nil
}
