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

	"github.com/BlackVS/aicrew/internal/store"
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
}

// New builds the service over an open store. It loads the certificate and
// key now, so a bad pair fails at start rather than at the first handshake.
func New(cfg Config, st *store.Store, log *slog.Logger) (*Server, error) {
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
	s.handle(http.MethodGet, "/healthz", s.health)
	s.handleOwnBody(http.MethodPost, IntrospectPath, s.introspect)
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

// dispatch matches the request path and method exactly. There is no
// pattern matching, no path cleaning, no decoding and no redirect: any other
// path is 404, and any other method on a known path, HEAD included, is 405.
func (s *Server) dispatch(w http.ResponseWriter, r *http.Request) {
	methods, ok := s.routes[requestPath(r)]
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
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"code": "method_not_allowed"})
		return
	}
	rt.h(w, r)
}

// routeOf names the route a request matches, for the log: the method and
// the registered path, or "unmatched". It never returns request text that
// no route registered.
func (s *Server) routeOf(r *http.Request) string {
	path := requestPath(r)
	if _, ok := s.routes[path][r.Method]; ok {
		return r.Method + " " + path
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
		if r.ContentLength > MaxBodyBytes && !s.routes[requestPath(r)][r.Method].ownsBody {
			// Closing the connection keeps the HTTP server from reading
			// the refused body to reuse the connection.
			w.Header().Set("Connection", "close")
			writeJSON(w, http.StatusRequestEntityTooLarge, map[string]string{"code": "request_too_large"})
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
