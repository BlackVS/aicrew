// Package opclient is the operator's client of aicrewd's operator API
// (package opapi). It reaches aicrewd over TLS under an explicit trust
// binding and authenticates with the operator credential, read from its
// owner-only file for each client. It links no store: the service is the only
// process that opens one.
package opclient

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/BlackVS/aicrew/internal/opapi"
	"github.com/BlackVS/aicrew/internal/optoken"
	"github.com/BlackVS/aicrew/internal/tlstrust"
)

const (
	requestTimeout = 30 * time.Second
	maxReply       = 4 << 20
)

// Config is where the service is and how it is trusted.
type Config struct {
	// URL is aicrewd's https origin.
	URL   string
	Trust tlstrust.Binding
	// TokenFile is the operator credential's owner-only file.
	TokenFile string
}

// Client calls the operator API.
type Client struct {
	base  string
	token string
	http  *http.Client
}

// New checks the configuration and reads the operator credential.
func New(cfg Config) (*Client, error) {
	u, err := url.Parse(cfg.URL)
	if err != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil || strings.ContainsAny(cfg.URL, "?#") ||
		(u.Path != "" && u.Path != "/") {
		return nil, errors.New("the URL must be aicrewd's https origin, without credentials, path, query or fragment")
	}
	if err := cfg.Trust.Check(u.Hostname()); err != nil {
		return nil, err
	}
	tc, err := cfg.Trust.ClientConfig(nil)
	if err != nil {
		return nil, err
	}
	if cfg.TokenFile == "" {
		return nil, errors.New("the operator token file is required")
	}
	token, err := optoken.Read(cfg.TokenFile)
	if err != nil {
		return nil, err
	}
	return &Client{
		base:  strings.TrimSuffix(cfg.URL, "/"),
		token: token,
		http: &http.Client{
			Timeout: requestTimeout,
			Transport: &http.Transport{Proxy: nil, TLSClientConfig: tc, DisableKeepAlives: true,
				MaxResponseHeaderBytes: 64 << 10},
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
	}, nil
}

// Error is the service's refusal: its status, code and message.
type Error struct {
	Status        int
	Code, Message string
}

func (e *Error) Error() string {
	if e.Message == "" {
		return fmt.Sprintf("%s (HTTP %d)", e.Code, e.Status)
	}
	return fmt.Sprintf("%s: %s", e.Code, e.Message)
}

// Get reads path with the query and decodes the answer into out.
func (c *Client) Get(ctx context.Context, path string, query url.Values, out any) error {
	if len(query) > 0 {
		path += "?" + query.Encode()
	}
	return c.do(ctx, http.MethodGet, path, nil, out)
}

// Post sends body as JSON to path and decodes the answer into out.
func (c *Client) Post(ctx context.Context, path string, body, out any) error {
	raw, err := json.Marshal(body)
	if err != nil {
		return err
	}
	return c.do(ctx, http.MethodPost, path, raw, out)
}

func (c *Client) do(ctx context.Context, method, path string, body []byte, out any) error {
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, rd)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		// The transport's error names the address, never the token.
		return fmt.Errorf("aicrewd could not be reached: %w", err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxReply+1))
	if err != nil {
		return fmt.Errorf("read aicrewd's answer: %w", err)
	}
	if len(raw) > maxReply {
		return errors.New("aicrewd's answer is too large")
	}
	if resp.StatusCode >= 300 {
		var refusal opapi.Error
		if json.Unmarshal(raw, &refusal) != nil || refusal.Code == "" {
			refusal.Code = "unexpected_answer"
		}
		return &Error{Status: resp.StatusCode, Code: refusal.Code, Message: refusal.Message}
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("aicrewd's answer is not the expected JSON: %w", err)
	}
	return nil
}

// Code is err's refusal code, or "".
func Code(err error) string {
	var e *Error
	if errors.As(err, &e) {
		return e.Code
	}
	return ""
}
