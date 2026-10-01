package server

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/BlackVS/aicrew/internal/aimemread"
	"github.com/BlackVS/aicrew/internal/reconcile"
	"github.com/BlackVS/aicrew/internal/store"
	"github.com/BlackVS/aicrew/internal/verifier"
)

// Bounds of every request.
const (
	// MaxBodyBytes caps a request body. A declared larger body is refused
	// before any handler runs; reading past the cap fails.
	MaxBodyBytes   = 64 << 10
	MaxHeaderBytes = 16 << 10

	readHeaderTimeout = 5 * time.Second
	readTimeout       = 15 * time.Second
	writeTimeout      = 15 * time.Second
	idleTimeout       = 60 * time.Second
)

// Server is aicrew's HTTPS service.
type Server struct {
	cfg   Config
	store *store.Store
	log   *slog.Logger
	tls   *tls.Config
	// routes maps an exact path, then an exact method, to its route.
	routes map[string]map[string]route
	http   *http.Server
	// verifier redeems aimem proofs at session entry; nil when no aimem
	// hub is configured.
	verifier store.Verifier
	// reader is aimem's read scope for settling member-driven steps; nil
	// when no read credential is configured, so those steps stay pending.
	reader store.ReservationReader
	// loop is aicrewd's reconciliation loop over the configured read scope
	// (crew-execution b3b); nil without one, and never for a test's reader.
	loop *reconcile.Loop
	// Per-address limits on the unauthenticated routes, and per-session on
	// handle refresh.
	challengeLimit, tokenLimit, refreshLimit *limiter
	redemption                               redemptionState // redemption.go
}

// Option adjusts a Server before it serves.
type Option func(*Server)

// WithVerifier replaces the aimem verifier the configuration would build. It
// exists for tests of the service's clients, which stand in for aimem.
func WithVerifier(v store.Verifier) Option { return func(s *Server) { s.verifier = v } }

// WithReader replaces the read scope the configuration would build. It
// exists for tests, which stand in for aimem.
func WithReader(r store.ReservationReader) Option { return func(s *Server) { s.reader = r } }

// New builds the service over an open store. It loads the certificate and
// key now, so a bad pair fails at start rather than at the first handshake.
func New(cfg Config, st *store.Store, log *slog.Logger, opts ...Option) (*Server, error) {
	if st == nil || log == nil {
		return nil, errors.New("server: a store and a logger are required")
	}
	cert, err := tls.LoadX509KeyPair(cfg.TLSCertFile, cfg.TLSKeyFile)
	if err != nil {
		return nil, fmt.Errorf("load tls_cert_file and tls_key_file: %w", err)
	}
	s := &Server{
		cfg: cfg, store: st, log: log, routes: map[string]map[string]route{},
		tls: &tls.Config{
			MinVersion:   tls.VersionTLS12,
			Certificates: []tls.Certificate{cert},
			NextProtos:   []string{"http/1.1"},
		},
	}
	if cfg.Aimem != nil {
		v, err := verifier.New(cfg.Aimem.verifierConfig(cfg.ServiceID))
		if err != nil {
			return nil, fmt.Errorf("aimem: %w", err)
		}
		if err := v.CheckCredential(); err != nil {
			return nil, fmt.Errorf("aimem.redemption_token_file: %w", err)
		}
		s.verifier = v
		if cfg.Aimem.ReadTokenFile != "" {
			r, err := aimemread.New(cfg.Aimem.readerConfig(cfg.ServiceID))
			if err != nil {
				return nil, fmt.Errorf("aimem: %w", err)
			}
			if err := r.CheckCredential(); err != nil {
				return nil, fmt.Errorf("aimem.read_token_file: %w", err)
			}
			s.reader = r
			s.loop = reconcile.New(st, r, log)
		}
	}
	for _, o := range opts {
		o(s)
	}
	s.challengeLimit = newLimiter(ChallengesPerMinute, time.Minute)
	s.tokenLimit = newLimiter(ExchangesPerMinute, time.Minute)
	s.refreshLimit = newLimiter(RefreshesPerMinute, time.Minute)
	s.handle(http.MethodGet, "/healthz", s.health)
	s.handleOwnBody(http.MethodPost, IntrospectPath, s.introspect)
	s.handleOwnBody(http.MethodPost, CoordinationPath, s.coordination)
	s.handleOwnBody(http.MethodPost, ChallengesPath, s.challenge)
	s.handleOwnBody(http.MethodPost, TokenPath, s.token)
	s.handle(http.MethodGet, SessionPath, s.sessionStatus)
	s.handleOwnBody(http.MethodPost, LeavePath, s.leave)
	s.registerAttempts()
	s.registerRedemption()
	s.http = &http.Server{
		Handler:           s.logged(s.limitBody(http.HandlerFunc(s.dispatch))),
		ReadHeaderTimeout: readHeaderTimeout,
		ReadTimeout:       readTimeout,
		WriteTimeout:      writeTimeout,
		IdleTimeout:       idleTimeout,
		MaxHeaderBytes:    MaxHeaderBytes,
		ErrorLog:          slog.NewLogLogger(log.Handler(), slog.LevelWarn),
	}
	return s, nil
}

// ListenAndServe listens on the configured address and serves until ctx
// ends; see Serve.
func (s *Server) ListenAndServe(ctx context.Context) error {
	ln, err := net.Listen("tcp", s.cfg.ListenAddr)
	if err != nil {
		return fmt.Errorf("listen on listen_addr: %w", err)
	}
	return s.Serve(ctx, ln)
}

// Serve accepts TLS connections on ln until ctx ends. It then stops
// accepting, lets requests in flight finish within the shutdown timeout, and
// returns. The caller closes the store afterwards. Every connection is TLS:
// a plain-HTTP request gets the HTTP server's own refusal and never reaches
// a handler.
func (s *Server) Serve(ctx context.Context, ln net.Listener) error {
	s.log.Info("listening", "addr", ln.Addr().String())
	if s.loop != nil {
		loopCtx, stopLoop := context.WithCancel(ctx)
		looped := make(chan struct{})
		go func() { s.loop.Run(loopCtx); close(looped) }()
		defer func() { stopLoop(); <-looped }()
	}
	served := make(chan error, 1)
	go func() { served <- s.http.Serve(tls.NewListener(ln, s.tls)) }()
	select {
	case err := <-served:
		return err
	case <-ctx.Done():
	}
	s.log.Info("shutting down")
	sctx, cancel := context.WithTimeout(context.Background(), time.Duration(s.cfg.ShutdownTimeout))
	defer cancel()
	err := s.http.Shutdown(sctx)
	if serr := <-served; err == nil && serr != nil && !errors.Is(serr, http.ErrServerClosed) {
		err = serr
	}
	return err
}

// route is one registered handler. A route that owns its body refuses an
// oversized one by its own contract instead of the generic 413, and must do
// so after its own authentication.
type route struct {
	h        http.HandlerFunc
	ownsBody bool
}

// handle registers h for exactly one method on exactly one path.
func (s *Server) handle(method, path string, h http.HandlerFunc) {
	s.register(method, path, route{h: h})
}

// handleOwnBody registers a route that refuses an oversized body itself.
func (s *Server) handleOwnBody(method, path string, h http.HandlerFunc) {
	s.register(method, path, route{h: h, ownsBody: true})
}

func (s *Server) register(method, path string, rt route) {
	if s.routes[path] == nil {
		s.routes[path] = map[string]route{}
	}
	s.routes[path][method] = rt
}

// dispatch matches the request path and method exactly. The only pattern is
// an attempt's ID in the step routes, which must have the ID's shape. There
// is no path cleaning, no decoding and no redirect: any other path is 404,
// and any other method on a known path, HEAD included, is 405.
func (s *Server) dispatch(w http.ResponseWriter, r *http.Request) {
	methods, ok := s.routes[s.routeKey(r)]
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]string{"code": "not_found"})
		return
	}
	rt, ok := methods[r.Method]
	if !ok {
		allowed := make([]string, 0, len(methods))
		for m := range methods {
			allowed = append(allowed, m)
		}
		sort.Strings(allowed)
		w.Header().Set("Allow", strings.Join(allowed, ", "))
		s.refuseShared(w, r, http.StatusMethodNotAllowed, "method_not_allowed")
		return
	}
	rt.h(w, r)
}

// refuseShared answers a refusal made before any handler runs. The client
// session routes get their envelope, with RFC 6749's members on the token
// endpoint; every other path keeps the bare code.
func (s *Server) refuseShared(w http.ResponseWriter, r *http.Request, status int, code string) {
	switch key := s.routeKey(r); {
	case key == ChallengesPath, key == TokenPath, key == SessionPath, key == LeavePath,
		key == AttemptsPath, strings.HasPrefix(key, AttemptsPath+"/"):
		s.refuseSession(w, r, code, key == TokenPath, 0)
	case key == InvitationBeginPath, key == InvitationCompletePath:
		s.refuseRedemption(w, r, code, 0) // redemption.go: counted, with the envelope
	default:
		writeJSON(w, status, map[string]string{"code": code})
	}
}

// routeKey is the registered path a request's path matches: the path itself,
// or an attempt step's route template. It is "" when nothing matches.
func (s *Server) routeKey(r *http.Request) string {
	path := requestPath(r)
	if _, ok := s.routes[path]; ok {
		return path
	}
	if _, route, ok := attemptPath(path); ok {
		return route
	}
	return ""
}

// routeOf names the route a request matches, for the log: the method and
// the registered path, or "unmatched". It never returns request text that
// no route registered.
func (s *Server) routeOf(r *http.Request) string {
	key := s.routeKey(r)
	if _, ok := s.routes[key][r.Method]; ok {
		return r.Method + " " + key
	}
	return "unmatched"
}

// requestPath is the path exactly as the client sent it in the request
// target, before the query. URL.Path is not used: it is percent-decoded, so
// "/%68ealthz" would read as "/healthz".
func requestPath(r *http.Request) string {
	path, _, _ := strings.Cut(r.RequestURI, "?")
	return path
}

func (s *Server) health(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// limitBody refuses a declared body over MaxBodyBytes before any handler
// runs, unless the matched route owns its body, and caps what any handler can
// read of a body.
func (s *Server) limitBody(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.ContentLength > MaxBodyBytes && !s.routes[s.routeKey(r)][r.Method].ownsBody {
			// Closing the connection keeps the HTTP server from reading
			// the refused body to reuse the connection.
			w.Header().Set("Connection", "close")
			s.refuseShared(w, r, http.StatusRequestEntityTooLarge, "request_too_large")
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, MaxBodyBytes)
		next.ServeHTTP(w, r)
	})
}

// logged records each request's method, matched route, status and duration.
// It never records headers, bodies, the query string or the raw path, so a
// secret sent in any of them cannot reach the log.
func (s *Server) logged(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rec, r)
		s.log.Info("request", "method", r.Method, "route", s.routeOf(r), "status", rec.status,
			"duration_ms", time.Since(start).Milliseconds())
	})
}

type statusRecorder struct {
	http.ResponseWriter
	status int
	wrote  bool
}

func (r *statusRecorder) WriteHeader(code int) {
	if !r.wrote {
		r.status, r.wrote = code, true
	}
	r.ResponseWriter.WriteHeader(code)
}

func (r *statusRecorder) Write(b []byte) (int, error) {
	r.wrote = true
	return r.ResponseWriter.Write(b)
}
