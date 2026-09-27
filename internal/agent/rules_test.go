package agent

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"sync"
	"testing"
	"time"
)

// stubCrew scripts aicrewd's answers and records the keys it was sent.
type stubCrew struct {
	mu          sync.Mutex
	challenges  int
	enterKeys   []string
	enterErrs   []error
	refreshKeys []string
	refreshErrs []error
	leaveKeys   []string
	leaveErrs   []error
	order       *[]string
}

func pop(errs *[]error) error {
	if len(*errs) == 0 {
		return nil
	}
	err := (*errs)[0]
	*errs = (*errs)[1:]
	return err
}

func (s *stubCrew) Challenge(context.Context, string, string) (Challenge, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.challenges++
	return Challenge{ID: "ch", HubID: "hub-test", ServiceID: "svc", ExpiresAt: time.Now().Add(5 * time.Minute)}, nil
}

func (s *stubCrew) Enter(_ context.Context, key, _, _, _, _, _ string) (Entry, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.enterKeys = append(s.enterKeys, key)
	if err := pop(&s.enterErrs); err != nil {
		return Entry{}, err
	}
	return Entry{Session: Session{ID: "sess", TeamID: "team", Generation: "1"}, Token: "ast1_token",
		TokenExpires: time.Now().Add(8 * time.Hour), Handle: "acs1_handle", HandleExpires: time.Now().Add(15 * time.Minute)}, nil
}

func (s *stubCrew) Refresh(_ context.Context, key, _, _ string) (Handle, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.refreshKeys = append(s.refreshKeys, key)
	if err := pop(&s.refreshErrs); err != nil {
		return Handle{}, err
	}
	return Handle{Value: "acs1_new", Expires: time.Now().Add(15 * time.Minute)}, nil
}

func (s *stubCrew) Leave(_ context.Context, key, _ string) (Session, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.leaveKeys = append(s.leaveKeys, key)
	if err := pop(&s.leaveErrs); err != nil {
		return Session{}, err
	}
	if s.order != nil {
		*s.order = append(*s.order, "leave")
	}
	return Session{ID: "sess", State: "left"}, nil
}

// stubAimem records lifecycle calls; gate, when set, holds Refresh until it
// is closed, after signalling inside.
type stubAimem struct {
	mu     sync.Mutex
	events []string
	open   bool
	gate   chan struct{}
	inside chan struct{}
	order  *[]string
}

func (a *stubAimem) log(ev string) {
	a.mu.Lock()
	a.events = append(a.events, ev)
	if a.order != nil {
		*a.order = append(*a.order, ev)
	}
	a.mu.Unlock()
}

func (a *stubAimem) Proof(context.Context, string, string, string) (string, error) {
	return "amr1_receipt", nil
}

func (a *stubAimem) Open(context.Context, string, string, string, string) (string, error) {
	a.log("open")
	a.open = true
	return "/aimem/aicrew-sessions/sess.json", nil
}

func (a *stubAimem) Refresh(context.Context, string, string) error {
	a.log("refresh-start")
	if a.gate != nil {
		close(a.inside)
		<-a.gate
	}
	a.log("refresh-end")
	return nil
}

func (a *stubAimem) Close(_ context.Context, session string) error {
	a.log("close " + session)
	return nil
}

func (a *stubAimem) Status(context.Context, string) (string, bool, error) {
	return "/aimem/aicrew-sessions/sess.json", a.open, nil
}

func stubEngine(t *testing.T, crew *stubCrew, aimem *stubAimem) *Engine {
	t.Helper()
	e := NewEngine(Config{Home: t.TempDir(), AgentID: "agent", TeamID: "team"}, crew, aimem,
		slog.New(slog.NewTextHandler(new(bytes.Buffer), nil)))
	e.Sleep = func(ctx context.Context, _ time.Duration) error { return ctx.Err() }
	return e
}

func unreachable() error { return &TransportError{Err: errors.New("timeout")} }

func refused(code string, retryable bool) error {
	return &Refusal{Status: 400, Code: code, NextAction: "do the next thing", Retryable: retryable}
}

// D-2a: an entry whose outcome is unknown, or that aicrewd reports still in
// progress, is retried with the same key after the earlier attempt was
// abandoned; one challenge serves all of them.
func TestEntryRetriesKeepTheKey(t *testing.T) {
	crew := &stubCrew{enterErrs: []error{unreachable(), refused("request_in_progress", true), unreachable()}}
	e := stubEngine(t, crew, &stubAimem{})
	if err := e.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(crew.enterKeys) != 4 || crew.challenges != 1 {
		t.Fatalf("%d entry attempts, %d challenges", len(crew.enterKeys), crew.challenges)
	}
	for _, k := range crew.enterKeys {
		if k != crew.enterKeys[0] {
			t.Fatalf("the key changed across retries: %v", crew.enterKeys)
		}
	}
	if e.token != "ast1_token" {
		t.Fatal("the engine kept no token")
	}
}

// A challenge that aicrewd no longer accepts is replaced by a new challenge,
// a new proof and a new key; a refusal that cannot succeed stops.
func TestEntryNewChallengeOrStop(t *testing.T) {
	crew := &stubCrew{enterErrs: []error{refused("challenge_invalid", false)}}
	e := stubEngine(t, crew, &stubAimem{})
	if err := e.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	if crew.challenges != 2 || crew.enterKeys[0] == crew.enterKeys[1] {
		t.Fatalf("%d challenges, keys %v", crew.challenges, crew.enterKeys)
	}
	crew = &stubCrew{enterErrs: []error{refused("identity_mismatch", false)}}
	e = stubEngine(t, crew, &stubAimem{})
	if err := e.Start(context.Background()); codeOf(err) != "identity_mismatch" || len(crew.enterKeys) != 1 {
		t.Fatalf("got %v after %d attempts", err, len(crew.enterKeys))
	}
}

// A refresh that fails is retried with a new key each time.
func TestRefreshRetriesWithNewKeys(t *testing.T) {
	crew := &stubCrew{refreshErrs: []error{unreachable(), refused("rate_limited", true)}}
	e := stubEngine(t, crew, &stubAimem{})
	if err := e.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := e.refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	k := crew.refreshKeys
	if len(k) != 3 || k[0] == k[1] || k[1] == k[2] || k[0] == k[2] {
		t.Fatalf("refresh keys %v", k)
	}
}

// D-3g: outstanding work keeps the session and never closes aimem's
// binding; a leave whose reply is lost is retried with the same key; aicrew
// is left before aimem is closed.
func TestLeaveRules(t *testing.T) {
	crew := &stubCrew{leaveErrs: []error{&Refusal{Status: 409, Code: "work_outstanding", NextAction: "reconcile it"}}}
	aimem := &stubAimem{}
	e := stubEngine(t, crew, aimem)
	if err := e.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	var kept *WorkOutstanding
	if err := e.Leave(context.Background()); !errors.As(err, &kept) || kept.NextAction != "reconcile it" {
		t.Fatalf("leave with open work: %v", err)
	}
	for _, ev := range aimem.events {
		if ev == "close sess" {
			t.Fatal("aimem was closed although the session was kept")
		}
	}
	if _, ok, _ := LoadState(e.Cfg.Home); !ok {
		t.Fatal("the kept session's record was dropped")
	}

	var order []string
	crew = &stubCrew{leaveErrs: []error{unreachable()}, order: &order}
	aimem = &stubAimem{order: &order}
	e = stubEngine(t, crew, aimem)
	if err := e.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	order = order[:0]
	if err := e.Leave(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(crew.leaveKeys) != 2 || crew.leaveKeys[0] != crew.leaveKeys[1] {
		t.Fatalf("leave keys %v", crew.leaveKeys)
	}
	if len(order) != 2 || order[0] != "leave" || order[1] != "close sess" {
		t.Fatalf("order %v, want leave then close", order)
	}
	if _, ok, _ := LoadState(e.Cfg.Home); ok {
		t.Fatal("the record outlived the session")
	}
}

// The operator's condition: aimem's lifecycle commands never overlap for one
// session, across clients of the same agent home. Two independent wrappers
// stand for two processes: a close and a status issued through the second
// while the first holds a refresh run only after the refresh finishes;
// another session is not held up.
func TestCloseWaitsForRefresh(t *testing.T) {
	stub := &stubAimem{gate: make(chan struct{}), inside: make(chan struct{})}
	dir := t.TempDir()
	first, second := Serialize(stub, dir), Aimem(&serialAimem{Aimem: stub, dir: dir})
	ctx := context.Background()
	refreshed := make(chan struct{})
	go func() {
		first.Refresh(ctx, "sess", "acs1_x")
		close(refreshed)
	}()
	<-stub.inside
	closed, statused := make(chan struct{}), make(chan struct{})
	go func() {
		second.Close(ctx, "sess")
		close(closed)
	}()
	go func() {
		second.Status(ctx, "sess")
		close(statused)
	}()
	if err := second.Close(ctx, "other"); err != nil {
		t.Fatal(err)
	}
	select {
	case <-closed:
		t.Fatal("close ran while the refresh was in flight")
	case <-statused:
		t.Fatal("status ran while the refresh was in flight")
	case <-time.After(200 * time.Millisecond):
	}
	close(stub.gate)
	<-refreshed
	<-closed
	<-statused
	stub.mu.Lock()
	defer stub.mu.Unlock()
	end, closeAt := -1, -1
	for i, ev := range stub.events {
		switch ev {
		case "refresh-end":
			end = i
		case "close sess":
			closeAt = i
		}
	}
	if end < 0 || closeAt < end {
		t.Fatalf("events %v: the close did not follow the refresh", stub.events)
	}
}

// The engine always serializes aimem's lifecycle commands, whatever runner
// it is given.
func TestEngineSerializes(t *testing.T) {
	e := stubEngine(t, &stubCrew{}, &stubAimem{})
	if _, ok := e.Aimem.(*serialAimem); !ok {
		t.Fatalf("the engine's aimem runner is %T, not serialized", e.Aimem)
	}
	if Serialize(e.Aimem, t.TempDir()) != e.Aimem {
		t.Fatal("serializing twice wrapped again")
	}
}
