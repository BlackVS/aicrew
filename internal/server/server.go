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
	mux   *http.ServeMux
	http  *http.Server
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
		cfg: cfg, store: st, log: log, mux: http.NewServeMux(),
		tls: &tls.Config{
			MinVersion:   tls.VersionTLS12,
			Certificates: []tls.Certificate{cert},
			NextProtos:   []string{"http/1.1"},
		},
	}
	s.mux.HandleFunc("GET /healthz", s.health)
	s.http = &http.Server{
		Handler:           s.logged(limitBody(s.mux)),
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
// runs, and caps what a handler can read of an undeclared one.
func limitBody(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.ContentLength > MaxBodyBytes {
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
		route := r.Pattern
		if route == "" {
			route = "unmatched"
		}
		s.log.Info("request", "method", r.Method, "route", route, "status", rec.status,
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
