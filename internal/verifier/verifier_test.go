package verifier

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/BlackVS/aicrew/internal/privatefile"
	"github.com/BlackVS/aicrew/internal/privatefile/privatefiletest"
	"github.com/BlackVS/aicrew/internal/store"
)

// The vendored identity.v1 fixture (internal/server/testdata/identity-v1,
// see its PROVENANCE.md). Its placeholders are replaced with sample_secrets.
type fixture struct {
	SampleSecrets      map[string]string `json:"sample_secrets"`
	RequestKeyEncoding struct {
		Cases []struct {
			Case           string `json:"case"`
			RawRequestKey  string `json:"raw_request_key"`
			IdempotencyKey string `json:"idempotency_key"`
		} `json:"cases"`
	} `json:"request_key_encoding"`
	Exchanges []struct {
		Case      string `json:"case"`
		Operation string `json:"operation"`
		HTTP      struct {
			Method  string            `json:"method"`
			Path    string            `json:"path"`
			Headers map[string]string `json:"headers"`
		} `json:"http"`
		Request  map[string]any `json:"request"`
		Response struct {
			Status int             `json:"status"`
			Body   json.RawMessage `json:"body"`
		} `json:"response"`
	} `json:"exchanges"`
	Refusals []struct {
		Case       string `json:"case"`
		Operation  string `json:"operation"`
		Code       string `json:"code"`
		HTTPStatus int    `json:"http_status"`
		Retryable  bool   `json:"retryable"`
		NextAction string `json:"next_action"`
	} `json:"refusals"`
}

func loadFixture(t *testing.T) fixture {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "server", "testdata", "identity-v1", "examples.json"))
	if err != nil {
		t.Fatal(err)
	}
	var f fixture
	if err := json.Unmarshal(raw, &f); err != nil {
		t.Fatal(err)
	}
	return f
}

func (f fixture) fill(s string) string {
	for k, v := range f.SampleSecrets {
		s = strings.ReplaceAll(s, "{"+k+"}", v)
	}
	return s
}

const (
	fixtureService   = "aicrew-example"
	fixtureHub       = "hub-example"
	fixtureChallenge = "01a0dbee-0000-7000-8000-00000000c001"
	fixtureKey       = "redeem:01a0dbee-0000-7000-8000-00000000c001:complete-1"
)

// newCert makes a self-signed certificate for 127.0.0.1 only.
func newCert(t *testing.T) tls.Certificate {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "aimem-test"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		IPAddresses:           []net.IP{net.ParseIP("127.0.0.1")},
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	leaf, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key, Leaf: leaf}
}

// fakeAimem is a local TLS server standing in for aimem.
type fakeAimem struct {
	srv   *httptest.Server
	cert  tls.Certificate
	calls atomic.Int32
}

func newFake(t *testing.T, h http.HandlerFunc) *fakeAimem {
	t.Helper()
	f := &fakeAimem{cert: newCert(t)}
	f.srv = httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.calls.Add(1)
		h(w, r)
	}))
	f.srv.TLS = &tls.Config{Certificates: []tls.Certificate{f.cert}}
	f.srv.StartTLS()
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeAimem) pin() string {
	sum := sha256.Sum256(f.cert.Leaf.RawSubjectPublicKeyInfo)
	return "sha256-" + base64.StdEncoding.EncodeToString(sum[:])
}

func (f *fakeAimem) roots() *x509.CertPool {
	p := x509.NewCertPool()
	p.AddCert(f.cert.Leaf)
	return p
}

// writeBearer writes a new private bearer file, replacing any old one.
func writeBearer(t *testing.T, path, bearer string) {
	t.Helper()
	os.Remove(path)
	f, err := privatefile.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(bearer + "\n"); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
}

// client returns a pinned client of f with the fixture's redemption bearer.
func (f *fakeAimem) client(t *testing.T, fx fixture) *Client {
	t.Helper()
	token := filepath.Join(t.TempDir(), "redemption.token")
	writeBearer(t, token, fx.SampleSecrets["redemption_credential"])
	c, err := New(Config{BaseURL: f.srv.URL, ServiceID: fixtureService, TLSMode: TrustSPKI, TLSValue: f.pin(), TokenFile: token})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func redeemReq(fx fixture) store.RedeemRequest {
	return store.RedeemRequest{
		ChallengeID: fixtureChallenge, HubID: fixtureHub,
		Receipt: store.NewSecret(fx.SampleSecrets["receipt"]), RequestKey: fixtureKey,
	}
}

// refusedAs checks err's code and retryability, and that it carries no secret.
func refusedAs(t *testing.T, fx fixture, err error, code string, retryable bool) *Error {
	t.Helper()
	var e *Error
	if !errors.As(err, &e) {
		t.Fatalf("error %v is not an *Error", err)
	}
	if e.Code != code || e.Retryable != retryable {
		t.Fatalf("got %s retryable=%v (%s), want %s retryable=%v", e.Code, e.Retryable, e.Reason, code, retryable)
	}
	noSecret(t, fx, err.Error())
	return e
}

func noSecret(t *testing.T, fx fixture, s string) {
	t.Helper()
	for name, v := range fx.SampleSecrets {
		if strings.Contains(s, v) {
			t.Fatalf("%q contains the %s", s, name)
		}
	}
}

// The client sends exactly the fixture's request and accepts its reply,
// including an identical replay.
func TestFixtureExchanges(t *testing.T) {
	fx := loadFixture(t)
	n := 0
	for _, ex := range fx.Exchanges {
		if ex.Operation != "redeem" {
			continue
		}
		n++
		t.Run(ex.Case, func(t *testing.T) {
			want := map[string]string{}
			for k, v := range ex.HTTP.Headers {
				want[k] = fx.fill(v)
			}
			wantBody := map[string]any{}
			for k, v := range ex.Request {
				wantBody[k] = fx.fill(v.(string))
			}
			f := newFake(t, func(w http.ResponseWriter, r *http.Request) {
				if r.Method != ex.HTTP.Method || r.RequestURI != ex.HTTP.Path {
					t.Errorf("request %s %s, want %s %s", r.Method, r.RequestURI, ex.HTTP.Method, ex.HTTP.Path)
				}
				for k, v := range want {
					if got := r.Header.Values(k); len(got) != 1 || got[0] != v {
						t.Errorf("header %s = %q, want %q", k, got, v)
					}
				}
				raw, _ := io.ReadAll(r.Body)
				var got map[string]any
				if err := json.Unmarshal(raw, &got); err != nil || len(got) != len(wantBody) {
					t.Errorf("body %s, want %v", raw, wantBody)
				}
				for k, v := range wantBody {
					if got[k] != v {
						t.Errorf("body field %s = %v, want %v", k, got[k], v)
					}
				}
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(ex.Response.Status)
				w.Write(ex.Response.Body)
			})
			id, err := f.client(t, fx).Redeem(context.Background(), redeemReq(fx))
			if err != nil {
				t.Fatal(err)
			}
			if id != (store.VerifiedIdentity{HubID: fixtureHub, UserID: "user-example", TokenID: "token-example-1"}) {
				t.Fatalf("identity %+v", id)
			}
		})
	}
	if n != 2 {
		t.Fatalf("the fixture has %d redemption exchanges, want 2", n)
	}
}

// Every redemption refusal in the fixture keeps its code and retryability.
func TestFixtureRefusals(t *testing.T) {
	fx := loadFixture(t)
	n := 0
	for _, rf := range fx.Refusals {
		if rf.Operation != "redeem" {
			continue
		}
		n++
		t.Run(rf.Case, func(t *testing.T) {
			f := newFake(t, func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(rf.HTTPStatus)
				json.NewEncoder(w).Encode(map[string]any{
					"code": rf.Code, "message": "refused", "retryable": rf.Retryable,
					"next_action": rf.NextAction, "correlation_id": "corr-test",
				})
			})
			_, err := f.client(t, fx).Redeem(context.Background(), redeemReq(fx))
			refusedAs(t, fx, err, rf.Code, rf.Retryable)
		})
	}
	if n != 14 {
		t.Fatalf("the fixture has %d redemption refusals, want 14", n)
	}
}

func TestIdempotencyKeyVectors(t *testing.T) {
	fx := loadFixture(t)
	if len(fx.RequestKeyEncoding.Cases) != 5 {
		t.Fatalf("%d vectors, want 5", len(fx.RequestKeyEncoding.Cases))
	}
	for _, c := range fx.RequestKeyEncoding.Cases {
		if got := IdempotencyKey(c.RawRequestKey); got != c.IdempotencyKey || len(got) != 46 {
			t.Errorf("%s: %s, want %s", c.Case, got, c.IdempotencyKey)
		}
	}
}

// A reply that does not answer this request is refused, never partly used.
func TestReplyVerification(t *testing.T) {
	fx := loadFixture(t)
	var good map[string]any
	for _, ex := range fx.Exchanges {
		if ex.Case == "redemption" {
			json.Unmarshal(ex.Response.Body, &good)
		}
	}
	edit := func(f func(m map[string]any)) []byte {
		raw, _ := json.Marshal(good)
		var m map[string]any
		json.Unmarshal(raw, &m)
		f(m)
		out, _ := json.Marshal(m)
		return out
	}
	identity := func(m map[string]any) map[string]any { return m["identity"].(map[string]any) }
	cases := map[string][]byte{
		"request_key":        edit(func(m map[string]any) { m["request_key"] = IdempotencyKey("other") }),
		"peer_service_id":    edit(func(m map[string]any) { m["peer_service_id"] = "aicrew-other" }),
		"challenge_id":       edit(func(m map[string]any) { m["challenge_id"] = "01a0dbee-0000-7000-8000-00000000c009" }),
		"hub_id":             edit(func(m map[string]any) { identity(m)["hub_id"] = "hub-other" }),
		"token_state":        edit(func(m map[string]any) { m["token_state"] = "revoked" }),
		"missing_token_id":   edit(func(m map[string]any) { delete(identity(m), "token_id") }),
		"empty_user_id":      edit(func(m map[string]any) { identity(m)["user_id"] = "" }),
		"bad_user_id":        edit(func(m map[string]any) { identity(m)["user_id"] = "user example" }),
		"unknown_field":      edit(func(m map[string]any) { m["grant"] = "all" }),
		"missing_replayed":   edit(func(m map[string]any) { delete(m, "replayed") }),
		"bad_redeemed_at":    edit(func(m map[string]any) { m["redeemed_at"] = "yesterday" }),
		"null_identity":      edit(func(m map[string]any) { m["identity"] = nil }),
		"missing_redemption": edit(func(m map[string]any) { delete(m, "redemption_id") }),
		"trailing_data":      append(edit(func(map[string]any) {}), []byte(` {}`)...),
		"not_json":           []byte(`ok`),
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			f := newFake(t, func(w http.ResponseWriter, r *http.Request) { w.Write(body) })
			_, err := f.client(t, fx).Redeem(context.Background(), redeemReq(fx))
			refusedAs(t, fx, err, CodeInvalidReply, false)
		})
	}
}

// Replies without a recognized refusal leave the outcome unknown.
func TestUnknownOutcomes(t *testing.T) {
	fx := loadFixture(t)
	cases := map[string]struct {
		status int
		body   string
		reason string
	}{
		"server_error": {500, "internal error", "status"},
		"empty_4xx":    {404, "", "status"},
		"unknown_code": {403, `{"code":"brand_new"}`, "unknown_code"},
		"redirect":     {307, "", "redirect"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			var followed atomic.Bool
			f := newFake(t, func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/elsewhere" {
					followed.Store(true)
				}
				if c.status == 307 {
					w.Header().Set("Location", "/elsewhere")
				}
				w.WriteHeader(c.status)
				io.WriteString(w, c.body)
			})
			_, err := f.client(t, fx).Redeem(context.Background(), redeemReq(fx))
			if e := refusedAs(t, fx, err, CodeUnavailable, true); e.Reason != c.reason {
				t.Fatalf("reason %s, want %s", e.Reason, c.reason)
			}
			if followed.Load() {
				t.Fatal("followed a redirect")
			}
		})
	}
}

func TestReplySizeCap(t *testing.T) {
	fx := loadFixture(t)
	for _, n := range []int{MaxReply, MaxReply + 1} {
		f := newFake(t, func(w http.ResponseWriter, r *http.Request) { w.Write(bytes.Repeat([]byte(" "), n)) })
		_, err := f.client(t, fx).Redeem(context.Background(), redeemReq(fx))
		if n == MaxReply {
			refusedAs(t, fx, err, CodeInvalidReply, false) // read in full, then refused as malformed
		} else if e := refusedAs(t, fx, err, CodeUnavailable, true); e.Reason != "oversize" {
			t.Fatalf("%d bytes: reason %s", n, e.Reason)
		}
	}
}

func TestBudgetAndCancellation(t *testing.T) {
	if Budget != 10*time.Second {
		t.Fatalf("Budget = %v, the contract's bound is 10 s", Budget)
	}
	fx := loadFixture(t)
	release := make(chan struct{})
	f := newFake(t, func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
		}
	})
	// Registered after the server's Close, so it runs first and frees the
	// handlers Close waits for.
	t.Cleanup(func() { close(release) })
	c := f.client(t, fx)
	c.budget = 200 * time.Millisecond

	// A longer caller deadline does not extend the budget.
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	start := time.Now()
	_, err := c.Redeem(ctx, redeemReq(fx))
	if e := refusedAs(t, fx, err, CodeUnavailable, true); e.Reason != "timeout" {
		t.Fatalf("reason %s", e.Reason)
	}
	if d := time.Since(start); d > 5*time.Second {
		t.Fatalf("took %v", d)
	}

	// The caller's cancellation ends the call and stays visible.
	c.budget = Budget
	ctx, cancel = context.WithCancel(context.Background())
	time.AfterFunc(100*time.Millisecond, cancel)
	_, err = c.Redeem(ctx, redeemReq(fx))
	if e := refusedAs(t, fx, err, CodeUnavailable, true); e.Reason != "cancelled" || !errors.Is(err, context.Canceled) {
		t.Fatalf("reason %s, err %v", e.Reason, err)
	}
}

func TestTLSTrust(t *testing.T) {
	fx := loadFixture(t)
	ok := func(w http.ResponseWriter, r *http.Request) {
		for _, ex := range fx.Exchanges {
			if ex.Case == "redemption" {
				w.Write(ex.Response.Body)
			}
		}
	}
	f := newFake(t, ok)
	other := newFake(t, ok)
	token := filepath.Join(t.TempDir(), "redemption.token")
	writeBearer(t, token, fx.SampleSecrets["redemption_credential"])
	host := "127.0.0.1"
	localhostURL := strings.Replace(f.srv.URL, host, "localhost", 1)

	cases := []struct {
		name  string
		cfg   Config
		roots *x509.CertPool
		ok    bool
	}{
		{"ca_dns", Config{BaseURL: f.srv.URL, TLSMode: TrustCADNS, TLSValue: host}, f.roots(), true},
		{"ca_dns_untrusted_ca", Config{BaseURL: f.srv.URL, TLSMode: TrustCADNS, TLSValue: host}, other.roots(), false},
		{"ca_dns_system_roots", Config{BaseURL: f.srv.URL, TLSMode: TrustCADNS, TLSValue: host}, nil, false},
		{"ca_dns_wrong_host", Config{BaseURL: localhostURL, TLSMode: TrustCADNS, TLSValue: "localhost"}, f.roots(), false},
		{"spki", Config{BaseURL: f.srv.URL, TLSMode: TrustSPKI, TLSValue: f.pin()}, nil, true},
		{"spki_other_key", Config{BaseURL: f.srv.URL, TLSMode: TrustSPKI, TLSValue: other.pin()}, nil, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			c.cfg.ServiceID, c.cfg.TokenFile = fixtureService, token
			cl, err := newClient(c.cfg, c.roots)
			if err != nil {
				t.Fatal(err)
			}
			_, err = cl.Redeem(context.Background(), redeemReq(fx))
			if c.ok {
				if err != nil {
					t.Fatal(err)
				}
				return
			}
			if e := refusedAs(t, fx, err, CodeUnavailable, true); e.Reason != "tls_untrusted" {
				t.Fatalf("reason %s", e.Reason)
			}
		})
	}
}

// Plain HTTP, trust on first use and malformed bindings cannot be configured.
func TestConfigRefused(t *testing.T) {
	good := Config{BaseURL: "https://hub.example:8443", ServiceID: fixtureService,
		TLSMode: TrustCADNS, TLSValue: "hub.example", TokenFile: "redemption.token"}
	if _, err := New(good); err != nil {
		t.Fatalf("good config: %v", err)
	}
	pin := "sha256-" + base64.StdEncoding.EncodeToString(make([]byte, 32))
	cases := map[string]func(c *Config){
		"plain_http":    func(c *Config) { c.BaseURL = "http://hub.example:8443" },
		"no_host":       func(c *Config) { c.BaseURL = "https:///x" },
		"path":          func(c *Config) { c.BaseURL = "https://hub.example/aimem" },
		"query":         func(c *Config) { c.BaseURL = "https://hub.example?x=1" },
		"empty_query":   func(c *Config) { c.BaseURL = "https://hub.example?" },
		"fragment":      func(c *Config) { c.BaseURL = "https://hub.example#x" },
		"userinfo":      func(c *Config) { c.BaseURL = "https://u:p@hub.example" },
		"no_trust_mode": func(c *Config) { c.TLSMode = "" },
		"unknown_mode":  func(c *Config) { c.TLSMode = "tofu" },
		"ca_dns_other":  func(c *Config) { c.TLSValue = "other.example" },
		"pin_no_prefix": func(c *Config) { c.TLSMode, c.TLSValue = TrustSPKI, strings.TrimPrefix(pin, "sha256-") },
		"pin_short": func(c *Config) {
			c.TLSMode, c.TLSValue = TrustSPKI, "sha256-"+base64.StdEncoding.EncodeToString(make([]byte, 31))
		},
		"pin_not_base64": func(c *Config) { c.TLSMode, c.TLSValue = TrustSPKI, "sha256-!!!" },
		"service_empty":  func(c *Config) { c.ServiceID = "" },
		"service_dotdot": func(c *Config) { c.ServiceID = ".." },
		"service_slash":  func(c *Config) { c.ServiceID = "a/b" },
		"no_token_file":  func(c *Config) { c.TokenFile = "" },
	}
	for name, mutate := range cases {
		c := good
		mutate(&c)
		if _, err := New(c); err == nil {
			t.Errorf("%s: accepted %+v", name, c)
		}
	}
	c := good
	c.TLSMode, c.TLSValue = TrustSPKI, pin
	if _, err := New(c); err != nil {
		t.Fatalf("spki config: %v", err)
	}
}

// Malformed input is refused before anything is sent.
func TestLocalValidation(t *testing.T) {
	fx := loadFixture(t)
	f := newFake(t, func(w http.ResponseWriter, r *http.Request) {})
	c := f.client(t, fx)
	cases := map[string]struct {
		mutate func(r *store.RedeemRequest)
		code   string
	}{
		"receipt_shape": {func(r *store.RedeemRequest) { r.Receipt = store.NewSecret("amr1_short") }, "proof_invalid"},
		"receipt_prefix": {func(r *store.RedeemRequest) {
			r.Receipt = store.NewSecret(strings.Replace(r.Receipt.Reveal(), "amr1_", "acs1_", 1))
		}, "proof_invalid"},
		"receipt_empty":   {func(r *store.RedeemRequest) { r.Receipt = store.NewSecret("") }, "proof_invalid"},
		"challenge_shape": {func(r *store.RedeemRequest) { r.ChallengeID = "a b" }, "invalid_request"},
		"hub_empty":       {func(r *store.RedeemRequest) { r.HubID = "" }, "invalid_request"},
		"key_empty":       {func(r *store.RedeemRequest) { r.RequestKey = "" }, "invalid_request"},
		"key_not_utf8":    {func(r *store.RedeemRequest) { r.RequestKey = "\xff" }, "invalid_request"},
	}
	for name, tc := range cases {
		r := redeemReq(fx)
		tc.mutate(&r)
		_, err := c.Redeem(context.Background(), r)
		t.Run(name, func(t *testing.T) { refusedAs(t, fx, err, tc.code, false) })
	}
	if n := f.calls.Load(); n != 0 {
		t.Fatalf("%d requests reached aimem", n)
	}
}

// The bearer comes from a private file, read on every call.
func TestBearerFile(t *testing.T) {
	fx := loadFixture(t)
	var seen atomic.Value
	f := newFake(t, func(w http.ResponseWriter, r *http.Request) {
		seen.Store(r.Header.Get("Authorization"))
		w.WriteHeader(http.StatusForbidden)
		io.WriteString(w, `{"code":"proof_invalid"}`)
	})
	token := filepath.Join(t.TempDir(), "redemption.token")
	c, err := New(Config{BaseURL: f.srv.URL, ServiceID: fixtureService, TLSMode: TrustSPKI, TLSValue: f.pin(), TokenFile: token})
	if err != nil {
		t.Fatal(err)
	}

	// Missing file.
	_, err = c.Redeem(context.Background(), redeemReq(fx))
	refusedAs(t, fx, err, CodeUnavailable, true)

	// Rotation takes effect on the next call.
	for _, bearer := range []string{"bearer-one", "bearer-two"} {
		writeBearer(t, token, bearer)
		c.Redeem(context.Background(), redeemReq(fx))
		if got := seen.Load(); got != "Bearer "+bearer {
			t.Fatalf("sent %v, want the %s", got, bearer)
		}
	}

	// Content that is not one line of visible ASCII is refused unsent.
	calls := f.calls.Load()
	for name, content := range map[string]string{
		"empty": "", "two_lines": "a\nb\n", "space": "a b", "oversize": strings.Repeat("a", maxCredential+1),
	} {
		os.Remove(token)
		pf, err := privatefile.Create(token)
		if err != nil {
			t.Fatal(err)
		}
		pf.WriteString(content)
		pf.Close()
		_, err = c.Redeem(context.Background(), redeemReq(fx))
		if e := refusedAs(t, fx, err, CodeUnavailable, true); e.Reason != "credential_file" {
			t.Fatalf("%s: reason %s", name, e.Reason)
		}
		if cause := errors.Unwrap(err); cause == nil || (content != "" && strings.Contains(cause.Error(), content)) {
			t.Fatalf("%s: cause %v", name, cause)
		}
	}

	// A file other accounts can read is refused unsent.
	writeBearer(t, token, "bearer-three")
	if err := privatefiletest.Expose(token); err != nil {
		t.Fatal(err)
	}
	_, err = c.Redeem(context.Background(), redeemReq(fx))
	if e := refusedAs(t, fx, err, CodeUnavailable, true); e.Reason != "credential_file" {
		t.Fatalf("exposed file: reason %s", e.Reason)
	}
	if cause := errors.Unwrap(err); cause == nil || strings.Contains(cause.Error(), "bearer-three") {
		t.Fatalf("exposed file: cause %v", cause)
	}
	if f.calls.Load() != calls {
		t.Fatal("a refused credential file still sent a request")
	}
}
