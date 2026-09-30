package server

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/BlackVS/aicrew/internal/aimemread"
	"github.com/BlackVS/aicrew/internal/privatefile"
	"github.com/BlackVS/aicrew/internal/privatefile/privatefiletest"
	"github.com/BlackVS/aicrew/internal/reconcile"
	"github.com/BlackVS/aicrew/internal/store"
)

const testReadCredential = "aimem_peer_0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f"

// readOverHTTPS, while set, gives setupCoordination's service the real read
// scope client, speaking HTTPS to the fake aimem's scope instead of calling
// it in process.
var readOverHTTPS bool

func writePrivate(t *testing.T, path, content string) {
	t.Helper()
	os.Remove(path)
	f, err := privatefile.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(content + "\n"); err != nil {
		t.Fatal(err)
	}
	f.Close()
}

// httpsReadScope serves the fake aimem's read scope over TLS, on exactly the
// scope's routes and headers, and returns the production client pinned to
// it.
func httpsReadScope(t *testing.T, aimem *fakeAimem) store.ReservationReader {
	t.Helper()
	prefix := "/v1/identity/peers/" + coordService + "/"
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.Header.Get("Authorization") != "Bearer "+testReadCredential ||
			r.Header.Get(aimemread.VersionHeader) != "1" || !strings.HasPrefix(r.URL.Path, prefix) {
			w.WriteHeader(http.StatusUnauthorized)
			json.NewEncoder(w).Encode(map[string]string{"code": "peer_unauthenticated"})
			return
		}
		parts := strings.Split(strings.TrimPrefix(r.URL.Path, prefix), "/")
		ctx := context.Background()
		var out any
		var err error
		switch {
		case len(parts) == 2 && parts[0] == "reservation-receipts":
			out, err = aimem.ReceiptByProof(ctx, parts[1])
		case len(parts) == 5 && parts[0] == "reservations" && parts[2] == "receipts":
			out, err = aimem.ReceiptByKey(ctx, store.TaskRef{TaskID: parts[1]}, store.ReservationOp(parts[3]), parts[4])
		case len(parts) == 2 && parts[0] == "reservations":
			out, err = aimem.HoldStatus(ctx, store.TaskRef{TaskID: parts[1]})
		default:
			w.WriteHeader(http.StatusNotFound)
			return
		}
		if err != nil {
			t.Error(err)
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(out)
	}))
	t.Cleanup(srv.Close)
	sum := sha256.Sum256(srv.Certificate().RawSubjectPublicKeyInfo)
	dir := t.TempDir()
	read, redeem := filepath.Join(dir, "read.token"), filepath.Join(dir, "redemption.token")
	writePrivate(t, read, testReadCredential)
	writePrivate(t, redeem, "sample-redemption-credential")
	c, err := aimemread.New(aimemread.Config{BaseURL: srv.URL, ServiceID: coordService, TLSMode: "spki_sha256",
		TLSValue: "sha256-" + base64.StdEncoding.EncodeToString(sum[:]), TokenFile: read, RedemptionTokenFile: redeem})
	if err != nil {
		t.Fatal(err)
	}
	if err := c.CheckCredential(); err != nil {
		t.Fatal(err)
	}
	return c
}

// The member-driven step flows settle through the production read scope
// client over HTTPS: offers, claims and stops, reviewed and confirmed
// finalizes, the complete loop, and proofless updates and their supersede.
func TestStepRoutesOverTheReadScope(t *testing.T) {
	readOverHTTPS = true
	t.Cleanup(func() { readOverHTTPS = false })
	for name, run := range map[string]func(*testing.T){
		"offer family":                TestStepRoutesEndToEnd,
		"a refused step waits":        TestStepRoutesRefusedStepWaits,
		"claim and stop":              TestClaimAndStopRoutesEndToEnd,
		"review, delivery, finalize":  TestReviewDeliveryFinalizeEndToEnd,
		"the complete loop":           TestCompleteLoopThroughTheRoutes,
		"updates and their supersede": TestSupersedeThroughTheRoutes,
	} {
		t.Run(name, run)
	}
}

// The read_token_file is optional, and is never the redemption file.
func TestConfigReadToken(t *testing.T) {
	withRead := strings.Replace(validAimem, "}", `,"read_token_file":"/etc/aicrew/read.token"}`, 1)
	c, err := ParseConfig([]byte(withAimem(withRead)))
	if err != nil || c.Aimem.ReadTokenFile != "/etc/aicrew/read.token" {
		t.Fatalf("read_token_file = %+v, %v", c.Aimem, err)
	}
	same := strings.Replace(validAimem, "}", `,"read_token_file":"/etc/aicrew/redemption.token"}`, 1)
	if _, err := ParseConfig([]byte(withAimem(same))); err == nil {
		t.Fatal("the redemption file as the read credential: accepted")
	}
}

// The service starts with the read scope only from a private file holding
// a peer credential that is not the redemption credential, and says what is
// wrong without the content; a good one gives the service the client.
func TestNewChecksReadCredential(t *testing.T) {
	certFile, keyFile, _ := testCert(t)
	st, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "aicrew.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	dir := t.TempDir()
	redeem, read := filepath.Join(dir, "redemption.token"), filepath.Join(dir, "read.token")
	writePrivate(t, redeem, "sample-redemption-credential")
	cfg := Config{ListenAddr: "127.0.0.1:0", TLSCertFile: certFile, TLSKeyFile: keyFile, ServiceID: "aicrew-test",
		ShutdownTimeout: Duration(time.Second), Aimem: &AimemConfig{BaseURL: "https://hub.example",
			TLSTrustMode: "ca_dns", TLSTrustValue: "hub.example", RedemptionTokenFile: redeem, ReadTokenFile: read}}
	log := slogDiscard()
	refused := func(name string) {
		t.Helper()
		_, err := New(cfg, st, log)
		if err == nil || !strings.Contains(err.Error(), "read_token_file") || strings.Contains(err.Error(), testReadCredential) {
			t.Fatalf("%s: %v", name, err)
		}
	}
	refused("a missing file")
	writePrivate(t, read, testReadCredential)
	if err := privatefiletest.Expose(read); err != nil {
		t.Fatal(err)
	}
	refused("a file readable by others")
	writePrivate(t, read, "not-a-peer-credential")
	refused("a malformed credential")
	writePrivate(t, read, testReadCredential)
	writePrivate(t, redeem, testReadCredential)
	refused("the redemption credential's secret")
	writePrivate(t, redeem, "sample-redemption-credential")
	s, err := New(cfg, st, log)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := s.reader.(*aimemread.Client); !ok || s.loop == nil {
		t.Fatalf("the service's read scope is %T, loop %v", s.reader, s.loop != nil)
	}
	// Serving starts the reconciliation loop, and stopping stops it.
	var logs syncBuffer
	s.log = slog.New(slog.NewTextHandler(&logs, nil))
	s.loop.Log = s.log
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	sctx, stop := context.WithCancel(context.Background())
	served := make(chan error, 1)
	go func() { served <- s.Serve(sctx, ln) }()
	deadline := time.Now().Add(5 * time.Second)
	for !strings.Contains(logs.String(), "reconcile: started") {
		if time.Now().After(deadline) {
			t.Fatalf("the loop did not start: %s", logs.String())
		}
		time.Sleep(10 * time.Millisecond)
	}
	stop()
	if err := <-served; err != nil {
		t.Fatalf("serve: %v", err)
	}
	// Without a read credential, the service has no read scope.
	cfg.Aimem.ReadTokenFile = ""
	if s, err = New(cfg, st, log); err != nil || s.reader != nil || s.loop != nil {
		t.Fatalf("no read credential: reader %T, %v", s.reader, err)
	}
}

// With no member online, aicrewd's reconciler settles a member's claim and
// update that aimem committed but the member never settled, and closes as
// recovered an attempt whose reservation aimem closed outside aicrew: through
// the production read scope client over HTTPS.
func TestReconcilerSettlesCrashedSteps(t *testing.T) {
	readOverHTTPS = true
	t.Cleanup(func() { readOverHTTPS = false })
	ctx := context.Background()
	e := setupCoordination(t)
	loop := reconcile.New(e.store, e.srv.reader, slogDiscard())

	got := e.call(t, e.indep.token, ClaimPath, "claim-1", e.claimBody("task-1"))
	claim, id := stepOf(t, got), attemptOf(t, got)
	if code := e.aimem.send(t, e.task("task-1"), claim); code != "" {
		t.Fatalf("aimem refused the claim: %s", code)
	}
	// The member crashes: it never settles.
	if settled, _ := loop.Round(ctx); settled != 1 {
		t.Fatalf("the claim's round settled %d", settled)
	}
	if a, _ := e.store.GetAttempt(ctx, id); a.State != store.AttemptRunning {
		t.Fatalf("after the round: %+v", a)
	}

	upd := updateStepOf(t, e.call(t, e.indep.token, AttemptsPath+"/"+id+"/work", "block-1",
		map[string]string{"intent": "block", "detail": "waiting on design"}))
	if code := e.aimem.send(t, e.task("task-1"), upd); code != "" {
		t.Fatalf("aimem refused the update: %s", code)
	}
	if settled, _ := loop.Round(ctx); settled != 1 {
		t.Fatalf("the update's round settled %d", settled)
	}
	if a, _ := e.store.GetAttempt(ctx, id); a.Phase != store.PhaseBlocked || a.PendingKey != "" {
		t.Fatalf("after the update's round: %+v", a)
	}

	// An admin recovery at aimem closes the hold outside aicrew.
	a, _ := e.store.GetAttempt(ctx, id)
	e.aimem.mu.Lock()
	h := e.aimem.holds["task-1"]
	h.active, h.closedBy, h.fence = false, "recovery_release", h.fence+1
	e.aimem.mu.Unlock()
	if _, closed := loop.Round(ctx); closed != 1 {
		t.Fatalf("the recovery's round closed %d", closed)
	}
	if r, _ := e.store.GetAttempt(ctx, id); r.State != store.AttemptClosed || r.CloseReason != "recovered" ||
		r.RecoveredBy != "recovery_release" || r.ReservationID != "" || a.ReservationID == "" {
		t.Fatalf("the recovered attempt: %+v", r)
	}
}

// A claim replayed until it has more proofs than the loop reads in a
// minute, and committed under the newest, is settled by one round: the
// newest proof is read first.
func TestReconcilerReachesTheNewestOfManyProofs(t *testing.T) {
	readOverHTTPS = true
	t.Cleanup(func() { readOverHTTPS = false })
	ctx := context.Background()
	e := setupCoordination(t)
	loop := reconcile.New(e.store, e.srv.reader, slogDiscard())
	var claim store.Step
	var id string
	for i := 0; i < 31; i++ {
		got := e.call(t, e.indep.token, ClaimPath, "claim-1", e.claimBody("task-1"))
		claim, id = stepOf(t, got), attemptOf(t, got)
	}
	if code := e.aimem.send(t, e.task("task-1"), claim); code != "" {
		t.Fatalf("aimem refused the newest proof: %s", code)
	}
	if settled, _ := loop.Round(ctx); settled != 1 {
		t.Fatalf("the round settled %d", settled)
	}
	if a, _ := e.store.GetAttempt(ctx, id); a.State != store.AttemptRunning {
		t.Fatalf("after the round: %+v", a)
	}
}
