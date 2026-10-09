// Package hubteams is aicrewd's client of the hub's team operations and
// board feed for this peer (aimem DESIGN-AIFORGE-IDENTITY-WIRE, "Team
// registration and read" and "Board feed"; docs/proposals/PILOT-1-FOLLOWUPS.md,
// sections 2.2 and 2.3; docs/DESIGN-CONTROL-PLANE.md, A1):
//
//	PUT /v1/identity/peers/{service_id}/team-registrations/{team_id}  team.register
//	GET /v1/identity/peers/{service_id}/team-reads[/{team_id}]         team.read
//	GET /v1/identity/peers/{service_id}/board-changes                  board.read
//
// Each operation has its own peer credential, read from its own private file
// on every call. The transport is the read scope's (package aimemread): one
// attempt within a bounded budget, the hub's TLS identity verified against
// the configured binding, no proxy, no redirect, no connection reuse and a
// bounded reply. A refusal keeps the hub's code; anything else is
// CodeUnavailable. No error carries a bearer.
package hubteams

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
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/BlackVS/aicrew/internal/privatefile"
	"github.com/BlackVS/aicrew/internal/tlstrust"
)

const (
	// VersionHeader carries identity.v1's version, which both operations
	// require.
	VersionHeader = "X-Aimem-Identity-Version"
	// Budget bounds one call: connect, TLS, request and the whole reply.
	Budget = 10 * time.Second
	// MaxReply bounds a reply body: a read of every team of the peer.
	MaxReply = 1 << 20
	// maxCredential bounds a bearer file.
	maxCredential = 4096
)

// CodeUnavailable is a hub that could not be reached, or whose answer is
// not the operation's answer. Nothing is known; a later call may succeed.
const CodeUnavailable = "hub_unavailable"

var (
	idShape         = regexp.MustCompile(`^[A-Za-z0-9._:-]{1,128}$`)
	credentialShape = regexp.MustCompile(`^aimem_peer_[0-9a-f]{64}$`)

	// refusals are the operations' refusal codes, with whether a later
	// call may succeed.
	refusals = map[string]bool{
		"invalid_request":      false,
		"unsupported_version":  false,
		"tls_required":         false,
		"peer_unauthenticated": false,
		"peer_unknown":         false,
		"peer_forbidden":       false,
		"team_name_taken":      false,
		"profile_disabled":     false,
		"not_found":            false,
		"invalid_cursor":       false,
		"cursor_ahead":         false,
		"rate_limited":         true,
		"request_in_progress":  true,
		"identity_unavailable": true,
	}
)

// Error is a failed call. Code is the hub's refusal code or
// CodeUnavailable; Reason is one fixed word naming what failed.
type Error struct {
	Code       string
	Retryable  bool
	Reason     string
	RetryAfter time.Duration
	cause      error
}

func (e *Error) Error() string { return "hub team operation: " + e.Code + " (" + e.Reason + ")" }
func (e *Error) Unwrap() error { return e.cause }

func unavailable(reason string) *Error {
	return &Error{Code: CodeUnavailable, Retryable: true, Reason: reason}
}

// Code is err's code, or "" when err is not an *Error.
func Code(err error) string {
	var e *Error
	if errors.As(err, &e) {
		return e.Code
	}
	return ""
}

// Config names the hub, how to trust it, and the credential files. A client
// without one of the files cannot do that operation.
type Config struct {
	BaseURL           string
	ServiceID         string
	TLSMode, TLSValue string
	RegisterTokenFile string // the team.register credential
	ReadTokenFile     string // the team.read credential
	BoardTokenFile    string // the board.read credential
}

// Client calls the hub's team operations for one peer.
type Client struct {
	cfg    Config
	origin string
	tls    *tls.Config
	budget time.Duration
}

// New checks cfg without any network call.
func New(cfg Config) (*Client, error) { return newClient(cfg, nil) }

func newClient(cfg Config, roots *x509.CertPool) (*Client, error) {
	u, err := url.Parse(cfg.BaseURL)
	if err != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil ||
		u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || (u.Path != "" && u.Path != "/") {
		return nil, errors.New("hub teams: the aimem URL must be an https origin without credentials, path, query or fragment")
	}
	if !validID(cfg.ServiceID) {
		return nil, errors.New("hub teams: the service ID is not a valid identity.v1 ID")
	}
	if cfg.RegisterTokenFile == "" && cfg.ReadTokenFile == "" && cfg.BoardTokenFile == "" {
		return nil, errors.New("hub teams: no team.register, team.read or board.read credential file is configured")
	}
	files := map[string]bool{}
	for _, f := range []string{cfg.RegisterTokenFile, cfg.ReadTokenFile, cfg.BoardTokenFile} {
		if f != "" && files[f] {
			return nil, errors.New("hub teams: team.register, team.read and board.read are separate credentials; name a file for each")
		}
		files[f] = true
	}
	trust := tlstrust.Binding{Mode: cfg.TLSMode, Value: cfg.TLSValue}
	if err := trust.Check(u.Hostname()); err != nil {
		return nil, fmt.Errorf("hub teams: %w", err)
	}
	tc, err := trust.ClientConfig(roots)
	if err != nil {
		return nil, fmt.Errorf("hub teams: %w", err)
	}
	return &Client{cfg: cfg, origin: strings.TrimSuffix(cfg.BaseURL, "/"), tls: tc, budget: Budget}, nil
}

func validID(id string) bool { return idShape.MatchString(id) && id != "." && id != ".." }

// CanRegister, CanRead and CanReadBoard report which operations the client
// has a credential for.
func (c *Client) CanRegister() bool  { return c.cfg.RegisterTokenFile != "" }
func (c *Client) CanRead() bool      { return c.cfg.ReadTokenFile != "" }
func (c *Client) CanReadBoard() bool { return c.cfg.BoardTokenFile != "" }

// CheckCredentials reads each configured bearer as a call would. Its errors
// name the file, never the content.
func (c *Client) CheckCredentials() error {
	for _, f := range []string{c.cfg.RegisterTokenFile, c.cfg.ReadTokenFile, c.cfg.BoardTokenFile} {
		if f == "" {
			continue
		}
		b, err := readCredential(f)
		if err != nil {
			return err
		}
		if !credentialShape.MatchString(b) {
			return fmt.Errorf("%s does not hold a peer credential (aimem_peer_ and 64 lowercase hex)", f)
		}
	}
	return nil
}

// Registration is team.register's answer.
type Registration struct {
	ProfileID    string `json:"profile_id"`
	TeamID       string `json:"team_id"`
	TeamName     string `json:"team_name"`
	Created      bool   `json:"created"`
	PreviousName string `json:"previous_name"`
}

// Register creates or renames the peer's profile for a team.
func (c *Client) Register(ctx context.Context, teamID, name string) (Registration, error) {
	if !c.CanRegister() {
		return Registration{}, &Error{Code: "no_credential", Reason: "register_credential"}
	}
	if !validID(teamID) {
		return Registration{}, &Error{Code: "invalid_request", Reason: "team_id"}
	}
	body, _ := json.Marshal(map[string]string{"team_name": name})
	var r Registration
	if err := c.call(ctx, http.MethodPut, "team-registrations/"+url.PathEscape(teamID), c.cfg.RegisterTokenFile, body, &r); err != nil {
		return Registration{}, err
	}
	if r.TeamID != teamID || r.TeamName != name || r.ProfileID == "" {
		return Registration{}, unavailable("answer")
	}
	return r, nil
}

// Repository is a granted project's repository, as the hub reports it.
type Repository struct {
	Kind   string `json:"kind"`
	URL    string `json:"url"`
	Host   string `json:"host"`
	Access string `json:"access"`
}

// ProcessPin is a granted project's selected process.
type ProcessPin struct {
	Repo     string `json:"repo"`
	Commit   string `json:"commit"`
	Manifest string `json:"manifest"`
}

// Project is one granted project.
type Project struct {
	Project    string      `json:"project"`
	Repository *Repository `json:"repository"`
	Process    *ProcessPin `json:"process"`
}

// Team is one team as team.read reports it: a disabled profile has no
// projects.
type Team struct {
	TeamID   string    `json:"team_id"`
	TeamName string    `json:"team_name"`
	Enabled  bool      `json:"enabled"`
	Projects []Project `json:"projects"`
}

// ReadTeam reads one of the peer's teams. An unknown team is the hub's
// not_found.
func (c *Client) ReadTeam(ctx context.Context, teamID string) (Team, error) {
	if !c.CanRead() {
		return Team{}, &Error{Code: "no_credential", Reason: "read_credential"}
	}
	if !validID(teamID) {
		return Team{}, &Error{Code: "invalid_request", Reason: "team_id"}
	}
	var t Team
	if err := c.call(ctx, http.MethodGet, "team-reads/"+url.PathEscape(teamID), c.cfg.ReadTokenFile, nil, &t); err != nil {
		return Team{}, err
	}
	if t.TeamID != teamID || !t.valid() {
		return Team{}, unavailable("answer")
	}
	return t, nil
}

// ReadTeams reads every team of the peer.
func (c *Client) ReadTeams(ctx context.Context) ([]Team, error) {
	if !c.CanRead() {
		return nil, &Error{Code: "no_credential", Reason: "read_credential"}
	}
	var all struct {
		Teams []Team `json:"teams"`
	}
	if err := c.call(ctx, http.MethodGet, "team-reads", c.cfg.ReadTokenFile, nil, &all); err != nil {
		return nil, err
	}
	for _, t := range all.Teams {
		if !t.valid() {
			return nil, unavailable("answer")
		}
	}
	return all.Teams, nil
}

// BoardChange is one entry of the board feed: the revision that set a
// task's state, the state before it ("" for the task's creation), the state
// it set, that revision's time and the task's required capability then.
type BoardChange struct {
	Project            string    `json:"project"`
	TaskID             string    `json:"task_id"`
	Revision           int64     `json:"revision"`
	From               string    `json:"from"`
	To                 string    `json:"to"`
	At                 time.Time `json:"at"`
	RequiredCapability string    `json:"required_capability"`
}

// BoardPage is one read of the board feed: its changes, the cursor to read
// on from, and whether a granted project has changes past it.
type BoardPage struct {
	Changes []BoardChange `json:"changes"`
	Cursor  string        `json:"cursor"`
	More    bool          `json:"more"`
}

// MaxBoardPage is the most changes one read asks for: the hub's own bound.
const MaxBoardPage = 500

// maxBoardField bounds each of an entry's text fields; the hub's required
// capability is at most 256 bytes, and a cursor is opaque but small.
const maxBoardField = 4096

// stateShape is a task state's name. A state aimem adds later still reads,
// so a new state cannot stop the feed.
var stateShape = regexp.MustCompile(`^[A-Z][A-Z_]{0,31}$`)

// ReadBoard reads the task state changes of the projects granted to the
// peer's enabled teams, from cursor ("" for each project's first change),
// at most limit of them. A cursor the hub cannot decode is invalid_cursor; a
// cursor past the feed, after the hub's state was restored, is cursor_ahead.
func (c *Client) ReadBoard(ctx context.Context, cursor string, limit int) (BoardPage, error) {
	if !c.CanReadBoard() {
		return BoardPage{}, &Error{Code: "no_credential", Reason: "board_credential"}
	}
	if limit < 1 || limit > MaxBoardPage || len(cursor) > maxBoardField {
		return BoardPage{}, &Error{Code: "invalid_request", Reason: "board_query"}
	}
	q := url.Values{"limit": {strconv.Itoa(limit)}}
	if cursor != "" {
		q.Set("cursor", cursor)
	}
	var p BoardPage
	if err := c.call(ctx, http.MethodGet, "board-changes?"+q.Encode(), c.cfg.BoardTokenFile, nil, &p); err != nil {
		return BoardPage{}, err
	}
	if !p.valid(limit) {
		return BoardPage{}, unavailable("answer")
	}
	return p, nil
}

// valid is a page the feed's contract allows: a cursor, at most limit
// entries, each naming its project, task, revision, the state it set and
// its time.
func (p BoardPage) valid(limit int) bool {
	if p.Cursor == "" || len(p.Cursor) > maxBoardField || p.Changes == nil || len(p.Changes) > limit {
		return false
	}
	for _, ch := range p.Changes {
		if !validID(ch.Project) || !validID(ch.TaskID) || ch.Revision < 1 || !stateShape.MatchString(ch.To) ||
			(ch.From != "" && !stateShape.MatchString(ch.From)) || ch.At.IsZero() || len(ch.RequiredCapability) > maxBoardField {
			return false
		}
	}
	return true
}

// valid is an answer the hub's contract allows: an ID, a name, projects
// with names, and none for a disabled profile.
func (t Team) valid() bool {
	if !validID(t.TeamID) || t.TeamName == "" || t.Projects == nil || (!t.Enabled && len(t.Projects) > 0) {
		return false
	}
	for _, p := range t.Projects {
		if p.Project == "" || (p.Repository != nil && (p.Repository.URL == "" || p.Repository.Kind == "")) {
			return false
		}
	}
	return true
}

// call is one exchange under this peer's path.
func (c *Client) call(ctx context.Context, method, path, tokenFile string, body []byte, out any) error {
	bearer, err := readCredential(tokenFile)
	if err != nil {
		e := unavailable("credential_file")
		e.cause = err
		return e
	}
	callCtx, cancel := context.WithTimeout(ctx, c.budget)
	defer cancel()
	endpoint := c.origin + "/v1/identity/peers/" + url.PathEscape(c.cfg.ServiceID) + "/" + path
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	hreq, err := http.NewRequestWithContext(callCtx, method, endpoint, rd)
	if err != nil {
		return unavailable("request")
	}
	hreq.Header.Set("Authorization", "Bearer "+bearer)
	hreq.Header.Set(VersionHeader, "1")
	hreq.Header.Set("Accept", "application/json")
	if body != nil {
		hreq.Header.Set("Content-Type", "application/json")
	}
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
	if err := dec.Decode(out); err != nil {
		return unavailable("shape")
	}
	if dec.Decode(&struct{}{}) != io.EOF {
		return unavailable("trailing_data")
	}
	return nil
}

// httpClient is one call's client: no proxy, no redirect, no connection
// reuse.
func (c *Client) httpClient() *http.Client {
	return &http.Client{
		Transport: &http.Transport{
			Proxy: nil, TLSClientConfig: c.tls, DisableKeepAlives: true,
			ForceAttemptHTTP2: false, MaxResponseHeaderBytes: 16384,
		},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
}

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

// refusal maps a non-200 reply; a redirect or an unknown answer is
// unavailable.
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

// readCredential returns the bearer in path, a private file holding one
// line. Errors never quote the content.
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
