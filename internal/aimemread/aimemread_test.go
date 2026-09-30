package aimemread

import (
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
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/BlackVS/aicrew/internal/privatefile"
	"github.com/BlackVS/aicrew/internal/privatefile/privatefiletest"
	"github.com/BlackVS/aicrew/internal/store"
)

// The shared coordination.v1 fixture, vendored for the coordination route's
// tests; see its PROVENANCE.md.
const fixturePath = "../server/testdata/coordination-v1/examples.json"

const fixtureService = "aicrew-example"

type exchange struct {
	Case string `json:"case"`
	HTTP struct {
		Method  string            `json:"method"`
		Path    string            `json:"path"`
		Headers map[string]string `json:"headers"`
	} `json:"http"`
	Response struct {
		Status int             `json:"status"`
		Body   json.RawMessage `json:"body"`
	} `json:"response"`
}

type fixture struct {
	SampleSecrets map[string]string `json:"sample_secrets"`
	ReadScope     struct {
		Exchanges []exchange `json:"exchanges"`
		Refusals  []struct {
			Case       string `json:"case"`
			Code       string `json:"code"`
			HTTPStatus int    `json:"http_status"`
			Retryable  bool   `json:"retryable"`
		} `json:"refusals"`
	} `json:"read_scope"`
}

func loadFixture(t *testing.T) fixture {
	t.Helper()
	raw, err := os.ReadFile(fixturePath)
	if err != nil {
		t.Fatal(err)
	}
	var fx fixture
	if err := json.Unmarshal(raw, &fx); err != nil {
		t.Fatal(err)
	}
	if len(fx.ReadScope.Exchanges) < 10 || len(fx.ReadScope.Refusals) < 7 || fx.SampleSecrets["read_credential"] == "" {
		t.Fatalf("the fixture's read scope is incomplete: %d exchanges, %d refusals", len(fx.ReadScope.Exchanges), len(fx.ReadScope.Refusals))
	}
	return fx
}

func newCert(t *testing.T) tls.Certificate {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "aimem-test"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		IPAddresses: []net.IP{net.ParseIP("127.0.0.1")},
		KeyUsage:    x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}, BasicConstraintsValid: true, IsCA: true,
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

// stub is a local TLS server standing in for aimem's read scope.
type stub struct {
	srv  *httptest.Server
	cert tls.Certificate
}

func newStub(t *testing.T, h http.HandlerFunc) *stub {
	t.Helper()
	s := &stub{cert: newCert(t)}
	s.srv = httptest.NewUnstartedServer(h)
	s.srv.TLS = &tls.Config{Certificates: []tls.Certificate{s.cert}}
	s.srv.StartTLS()
	t.Cleanup(s.srv.Close)
	return s
}

func (s *stub) pin() string {
	sum := sha256.Sum256(s.cert.Leaf.RawSubjectPublicKeyInfo)
	return "sha256-" + base64.StdEncoding.EncodeToString(sum[:])
}

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

// client is a pinned client of s with the fixture's read credential, and a
// separate redemption credential.
func (s *stub) client(t *testing.T, fx fixture) *Client {
	t.Helper()
	dir := t.TempDir()
	read, redeem := filepath.Join(dir, "read.token"), filepath.Join(dir, "redemption.token")
	writeBearer(t, read, fx.SampleSecrets["read_credential"])
	writeBearer(t, redeem, "sample-redemption-credential")
	c, err := New(Config{BaseURL: s.srv.URL, ServiceID: fixtureService, TLSMode: "spki_sha256", TLSValue: s.pin(),
		TokenFile: read, RedemptionTokenFile: redeem})
	if err != nil {
		t.Fatal(err)
	}
	if err := c.CheckCredential(); err != nil {
		t.Fatal(err)
	}
	return c
}

// call asks the scope the question an exchange's path encodes.
func call(c *Client, path string) (any, error) {
	rest := strings.TrimPrefix(path, "/v1/identity/peers/"+fixtureService+"/")
	parts := strings.Split(rest, "/")
	ctx := context.Background()
	switch {
	case parts[0] == "reservation-receipts":
		return c.ReceiptByProof(ctx, parts[1])
	case len(parts) == 5 && parts[2] == "receipts":
		return c.ReceiptByKey(ctx, store.TaskRef{TaskID: parts[1]}, store.ReservationOp(parts[3]), parts[4])
	case len(parts) == 2:
		return c.HoldStatus(ctx, store.TaskRef{TaskID: parts[1]})
	}
	return nil, errors.New("unknown path " + path)
}

// Every read-scope exchange of the pinned fixture is asked on exactly its
// path with its headers, and its answer decodes to exactly its body.
func TestFixtureExchanges(t *testing.T) {
	fx := loadFixture(t)
	bearer := fx.SampleSecrets["read_credential"]
	for _, ex := range fx.ReadScope.Exchanges {
		t.Run(ex.Case, func(t *testing.T) {
			s := newStub(t, func(w http.ResponseWriter, r *http.Request) {
				if r.Method != ex.HTTP.Method || r.URL.EscapedPath() != ex.HTTP.Path || r.URL.RawQuery != "" {
					t.Errorf("asked %s %s", r.Method, r.URL.RequestURI())
				}
				if r.Header.Get("Authorization") != "Bearer "+bearer || r.Header.Get(VersionHeader) != "1" {
					t.Errorf("headers: version %q, bearer ok %v", r.Header.Get(VersionHeader),
						r.Header.Get("Authorization") == "Bearer "+bearer)
				}
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(ex.Response.Status)
				w.Write(ex.Response.Body)
			})
			got, err := call(s.client(t, fx), ex.HTTP.Path)
			if err != nil {
				t.Fatal(err)
			}
			var want, have any
			json.Unmarshal(ex.Response.Body, &want)
			b, _ := json.Marshal(got)
			json.Unmarshal(b, &have)
			if !reflect.DeepEqual(want, have) {
				t.Fatalf("decoded %s, want %s", b, ex.Response.Body)
			}
		})
	}
}

// Every refusal of the fixture is an error with its code, never "none"; the
// retryable ones carry aimem's Retry-After. Nothing carries the bearer.
func TestFixtureRefusals(t *testing.T) {
	fx := loadFixture(t)
	bearer := fx.SampleSecrets["read_credential"]
	path := fx.ReadScope.Exchanges[0].HTTP.Path
	for _, rf := range fx.ReadScope.Refusals {
		t.Run(rf.Case, func(t *testing.T) {
			s := newStub(t, func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				if rf.Retryable {
					w.Header().Set("Retry-After", "7")
				}
				w.WriteHeader(rf.HTTPStatus)
				json.NewEncoder(w).Encode(map[string]any{"code": rf.Code, "message": "refused", "retryable": rf.Retryable,
					"next_action": "Ask the operator.", "correlation_id": "c-1"})
			})
			got, err := call(s.client(t, fx), path)
			var e *Error
			if !errors.As(err, &e) || e.Code != rf.Code || e.Retryable != rf.Retryable {
				t.Fatalf("answer %+v, error %v", got, err)
			}
			if rf.Retryable != (e.RetryAfter == 7*time.Second) {
				t.Fatalf("Retry-After %v", e.RetryAfter)
			}
			if strings.Contains(err.Error(), bearer) {
				t.Fatal("the error carries the bearer")
			}
		})
	}
}

// An answer that is not the scope's exact answer to the request is an
// error, never "none" and never a receipt.
func TestInvalidReplies(t *testing.T) {
	fx := loadFixture(t)
	byProof := "/v1/identity/peers/" + fixtureService + "/reservation-receipts/p1_nzF7rp0dEZsGKnCn70VnvSoxlGXk0MAFo1SrGi89VGU"
	byKey := "/v1/identity/peers/" + fixtureService + "/reservations/01a0e39c-0000-7000-8000-00000000c501/receipts/update/k1_9svAO90Pfq8X2wdd-Oe5mAzACzlWX0HgNAoKiTInH44"
	hold := "/v1/identity/peers/" + fixtureService + "/reservations/01a0e39c-0000-7000-8000-00000000c501"
	receipt := `{"id":"r-1","operation":"update","task_id":"01a0e39c-0000-7000-8000-00000000c501","request_key_digest":"k1_9svAO90Pfq8X2wdd-Oe5mAzACzlWX0HgNAoKiTInH44","reservation_id":"res-1","fence":"2","task_revision":4,"member_user_id":"u","verified_mode":"team","committed_at":"2026-09-27T18:05:40Z"}`
	for name, c := range map[string]struct {
		path, body string
		status     int
		code       string
	}{
		"an unknown field":              {byProof, `{"state":"none","extra":1}`, 200, CodeUnavailable},
		"a duplicate state, none last":  {byProof, `{"state":"committed","state":"none"}`, 200, CodeUnavailable},
		"a duplicate state, none first": {byProof, `{"state":"none","receipt":` + receipt + `,"state":"committed"}`, 200, CodeUnavailable},
		"a duplicate receipt":           {byProof, `{"state":"committed","receipt":` + receipt + `,"receipt":` + receipt + `}`, 200, CodeUnavailable},
		"a duplicate key in the receipt": {byKey, `{"state":"committed","receipt":` +
			strings.Replace(receipt, `"fence":"2"`, `"fence":"2","fence":"3"`, 1) + `}`, 200, CodeUnavailable},
		"none with a null receipt":      {byProof, `{"state":"none","receipt":null}`, 200, CodeUnavailable},
		"committed with a null receipt": {byProof, `{"state":"committed","receipt":null}`, 200, CodeUnavailable},
		"a receipt with a null field": {byKey, `{"state":"committed","receipt":` +
			strings.Replace(receipt, `"member_user_id":"u"`, `"member_user_id":null`, 1) + `}`, 200, CodeUnavailable},
		"a receipt of another verified mode": {byKey, `{"state":"committed","receipt":` +
			strings.Replace(receipt, `"verified_mode":"team"`, `"verified_mode":"personal"`, 1) + `}`, 200, CodeUnavailable},
		"a duplicate key in a hold":   {hold, `{"state":"held","reservation_id":"r-1","reservation_id":"r-2","fence":"2","holder_mode":"external","task_revision":3}`, 200, CodeUnavailable},
		"held of another holder mode": {hold, `{"state":"held","reservation_id":"r-1","fence":"2","holder_mode":"personal","task_revision":3}`, 200, CodeUnavailable},
		"held without a holder mode":  {hold, `{"state":"held","reservation_id":"r-1","fence":"2","task_revision":3}`, 200, CodeUnavailable},
		"held with a closed_by":       {hold, `{"state":"held","reservation_id":"r-1","fence":"2","holder_mode":"external","task_revision":3,"closed_by":"holder_release"}`, 200, CodeUnavailable},
		"closed with a null time":     {hold, `{"state":"closed","reservation_id":"r-1","closing_fence":"3","closed_by":"holder_release","closed_at":null,"task_revision":5}`, 200, CodeUnavailable},
		"a JSON array":                {hold, `[{"state":"none"}]`, 200, CodeUnavailable},
		"a proof's receipt with a malformed fence": {byProof, `{"state":"committed","receipt":` +
			strings.Replace(receipt, `"fence":"2"`, `"fence":"x"`, 1) + `}`, 200, CodeUnavailable},
		"trailing data":               {byProof, `{"state":"none"} {}`, 200, CodeUnavailable},
		"an unknown state":            {byProof, `{"state":"maybe"}`, 200, CodeUnavailable},
		"committed without a receipt": {byProof, `{"state":"committed"}`, 200, CodeUnavailable},
		"none with a receipt":         {byProof, `{"state":"none","receipt":` + receipt + `}`, 200, CodeUnavailable},
		"a receipt for another key": {byKey, `{"state":"committed","receipt":` +
			strings.Replace(receipt, "k1_9svAO90Pfq8X2wdd", "k1_0svAO90Pfq8X2wdd", 1) + `}`, 200, CodeUnavailable},
		"a receipt for another task": {byKey, `{"state":"committed","receipt":` +
			strings.Replace(receipt, "c501", "c502", 1) + `}`, 200, CodeUnavailable},
		"a receipt of another operation": {byKey, `{"state":"committed","receipt":` +
			strings.Replace(receipt, `"update"`, `"claim"`, 1) + `}`, 200, CodeUnavailable},
		"a receipt without a fence": {byKey, `{"state":"committed","receipt":` +
			strings.Replace(receipt, `"fence":"2"`, `"fence":""`, 1) + `}`, 200, CodeUnavailable},
		"held without a fence":         {hold, `{"state":"held","reservation_id":"r-1","task_revision":3}`, 200, CodeUnavailable},
		"held with a closing fence":    {hold, `{"state":"held","reservation_id":"r-1","fence":"2","closing_fence":"3","task_revision":3}`, 200, CodeUnavailable},
		"closed without closed_by":     {hold, `{"state":"closed","reservation_id":"r-1","closing_fence":"3","closed_at":"2026-09-28T04:10:00Z","task_revision":5}`, 200, CodeUnavailable},
		"closed by an unknown kind":    {hold, `{"state":"closed","reservation_id":"r-1","closing_fence":"3","closed_by":"magic","closed_at":"2026-09-28T04:10:00Z","task_revision":5}`, 200, CodeUnavailable},
		"closed with a live fence":     {hold, `{"state":"closed","reservation_id":"r-1","fence":"2","closing_fence":"3","closed_by":"holder_release","closed_at":"2026-09-28T04:10:00Z","task_revision":5}`, 200, CodeUnavailable},
		"closed without a time":        {hold, `{"state":"closed","reservation_id":"r-1","closing_fence":"3","closed_by":"holder_release","task_revision":5}`, 200, CodeUnavailable},
		"none with fields":             {hold, `{"state":"none","reservation_id":"r-1"}`, 200, CodeUnavailable},
		"not JSON":                     {hold, `<html>`, 200, CodeUnavailable},
		"an oversized body":            {hold, `{"state":"none","pad":"` + strings.Repeat("x", MaxReply) + `"}`, 200, CodeUnavailable},
		"a redirect":                   {hold, ``, 302, CodeUnavailable},
		"a status without an envelope": {hold, `oops`, 500, CodeUnavailable},
		"an unknown refusal code":      {hold, `{"code":"surprise"}`, 409, CodeUnavailable},
	} {
		t.Run(name, func(t *testing.T) {
			s := newStub(t, func(w http.ResponseWriter, r *http.Request) {
				if c.status == 302 {
					w.Header().Set("Location", "https://elsewhere.example/")
				}
				w.WriteHeader(c.status)
				w.Write([]byte(c.body))
			})
			got, err := call(s.client(t, fx), c.path)
			var e *Error
			if !errors.As(err, &e) || e.Code != c.code || !e.Retryable {
				t.Fatalf("answer %+v, error %v; want a retryable %s", got, err, c.code)
			}
			// A refused answer is never also returned.
			if b, _ := json.Marshal(got); string(b) != `{"state":""}` {
				t.Fatalf("an answer came back with the error: %s", b)
			}
		})
	}
}

// A receipt missing any required field, or a closed hold missing any of
// its fields, is unavailable and retryable, never none.
func TestRequiredFields(t *testing.T) {
	fx := loadFixture(t)
	for _, ex := range fx.ReadScope.Exchanges {
		var body map[string]json.RawMessage
		if json.Unmarshal(ex.Response.Body, &body) != nil {
			t.Fatal(ex.Case)
		}
		var drop []string
		inner := ""
		if raw, ok := body["receipt"]; ok {
			var r map[string]json.RawMessage
			json.Unmarshal(raw, &r)
			for k := range r {
				drop = append(drop, k)
			}
			inner = "receipt"
		} else {
			for k := range body {
				if k != "state" && k != "own_work_ref" {
					drop = append(drop, k)
				}
			}
		}
		for _, field := range drop {
			t.Run(ex.Case+" without "+field, func(t *testing.T) {
				var m map[string]json.RawMessage
				json.Unmarshal(ex.Response.Body, &m)
				if inner != "" {
					var r map[string]json.RawMessage
					json.Unmarshal(m[inner], &r)
					delete(r, field)
					m[inner], _ = json.Marshal(r)
				} else {
					delete(m, field)
				}
				b, _ := json.Marshal(m)
				s := newStub(t, func(w http.ResponseWriter, r *http.Request) { w.Write(b) })
				got, err := call(s.client(t, fx), ex.HTTP.Path)
				var e *Error
				if !errors.As(err, &e) || e.Code != CodeUnavailable || !e.Retryable {
					t.Fatalf("answer %+v, error %v", got, err)
				}
			})
		}
	}
}

// aimem's TLS identity must match the binding; the call's budget and the
// caller's cancellation end a read that hangs.
func TestTrustAndBudget(t *testing.T) {
	fx := loadFixture(t)
	path := fx.ReadScope.Exchanges[1].HTTP.Path
	release := make(chan struct{})
	s := newStub(t, func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
		}
	})
	defer close(release)
	c := s.client(t, fx)
	c.cfg.TLSValue = "sha256-" + base64.StdEncoding.EncodeToString(make([]byte, 32))
	other, err := New(c.cfg)
	if err != nil {
		t.Fatal(err)
	}
	var e *Error
	if _, err := call(other, path); !errors.As(err, &e) || e.Reason != "tls_untrusted" || !e.Retryable {
		t.Fatalf("a wrong pin: %v", err)
	}
	c = s.client(t, fx)
	c.budget = 200 * time.Millisecond
	if _, err := call(c, path); !errors.As(err, &e) || e.Reason != "timeout" {
		t.Fatalf("a hanging read: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := c.HoldStatus(ctx, store.TaskRef{TaskID: "t-1"}); !errors.Is(err, context.Canceled) {
		t.Fatalf("a cancelled read: %v", err)
	}
}

// The read credential is its own private file holding a peer credential;
// the check never quotes it.
func TestCredentialChecks(t *testing.T) {
	fx := loadFixture(t)
	secret := fx.SampleSecrets["read_credential"]
	s := newStub(t, func(http.ResponseWriter, *http.Request) {})
	c := s.client(t, fx)
	read, redeem := c.cfg.TokenFile, c.cfg.RedemptionTokenFile
	refused := func(name string) {
		t.Helper()
		err := c.CheckCredential()
		if err == nil || strings.Contains(err.Error(), secret) {
			t.Fatalf("%s: %v", name, err)
		}
	}
	os.Remove(read)
	refused("a missing file")
	writeBearer(t, read, secret)
	if err := privatefiletest.Expose(read); err != nil {
		t.Fatal(err)
	}
	refused("a file readable by others")
	writeBearer(t, read, "not-a-peer-credential")
	refused("a malformed credential")
	writeBearer(t, read, secret)
	writeBearer(t, redeem, secret)
	refused("the redemption credential's secret")
	writeBearer(t, redeem, "sample-redemption-credential")
	if err := c.CheckCredential(); err != nil {
		t.Fatalf("a good file: %v", err)
	}
	// A read with a refused file asks nothing.
	os.Remove(read)
	var e *Error
	if _, err := c.HoldStatus(context.Background(), store.TaskRef{TaskID: "t-1"}); !errors.As(err, &e) || e.Reason != "credential_file" {
		t.Fatalf("a read without its credential: %v", err)
	}
}

// The configuration is refused offline for anything the scope cannot be
// read with safely.
func TestConfigRefused(t *testing.T) {
	good := Config{BaseURL: "https://hub.example:8443", ServiceID: fixtureService, TLSMode: "ca_dns",
		TLSValue: "hub.example", TokenFile: "/etc/aicrew/read.token", RedemptionTokenFile: "/etc/aicrew/redemption.token"}
	if _, err := New(good); err != nil {
		t.Fatal(err)
	}
	for name, mutate := range map[string]func(*Config){
		"plain http":           func(c *Config) { c.BaseURL = "http://hub.example" },
		"a path":               func(c *Config) { c.BaseURL = "https://hub.example/aimem" },
		"a bad service ID":     func(c *Config) { c.ServiceID = "a/b" },
		"no token file":        func(c *Config) { c.TokenFile = "" },
		"the redemption file":  func(c *Config) { c.TokenFile = c.RedemptionTokenFile },
		"another trusted host": func(c *Config) { c.TLSValue = "other.example" },
	} {
		c := good
		mutate(&c)
		if _, err := New(c); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

// The client refuses to ask a malformed question.
func TestLocalValidation(t *testing.T) {
	fx := loadFixture(t)
	s := newStub(t, func(http.ResponseWriter, *http.Request) { t.Error("a malformed question reached aimem") })
	c := s.client(t, fx)
	ctx := context.Background()
	if _, err := c.ReceiptByProof(ctx, "k1_9svAO90Pfq8X2wdd-Oe5mAzACzlWX0HgNAoKiTInH44"); err == nil {
		t.Error("a key digest as a proof digest")
	}
	if _, err := c.ReceiptByKey(ctx, store.TaskRef{TaskID: "t/1"}, store.ReservationUpdate, "k1_9svAO90Pfq8X2wdd-Oe5mAzACzlWX0HgNAoKiTInH44"); err == nil {
		t.Error("a task ID with a slash")
	}
	if _, err := c.HoldStatus(ctx, store.TaskRef{TaskID: ".."}); err == nil {
		t.Error("a dot-dot task ID")
	}
}
