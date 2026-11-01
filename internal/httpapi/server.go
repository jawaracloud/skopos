// Package httpapi implements the gateway's HTTP surface. In M2 it serves only
// health, readiness, and version endpoints; the Prometheus, Loki, and Tempo
// heads are added in later milestones.
package httpapi

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"runtime/debug"
	"time"

	"github.com/jawaracloud/skopos/internal/auth"
)

// Pinger checks backend readiness. The ClickHouse client implements it.
type Pinger interface {
	Ping(ctx context.Context) error
}

// Config configures the HTTP server.
type Config struct {
	Logger           *slog.Logger
	Pinger           Pinger
	Version          string
	ReadinessTimeout time.Duration
	MaxRequestBytes  int64
	// Auth optionally protects every route except /healthz and /readyz.
	Auth *auth.Basic
	// Register adds the query-head routes to the server mux. It is called
	// before the JSON 404 catch-all.
	Register func(*http.ServeMux)
}

// Server owns the gateway handlers.
type Server struct {
	logger           *slog.Logger
	pinger           Pinger
	version          string
	readinessTimeout time.Duration
	maxRequestBytes  int64
	auth             *auth.Basic
	register         func(*http.ServeMux)
}

// New validates cfg and returns a server.
func New(cfg Config) (*Server, error) {
	if cfg.Pinger == nil {
		return nil, errors.New("httpapi: pinger is required")
	}
	if cfg.ReadinessTimeout < 0 {
		return nil, errors.New("httpapi: readiness timeout must not be negative")
	}
	if cfg.MaxRequestBytes < 0 {
		return nil, errors.New("httpapi: max request bytes must not be negative")
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.New(slog.NewJSONHandler(io.Discard, nil))
	}
	if cfg.Version == "" {
		cfg.Version = "dev"
	}
	if cfg.ReadinessTimeout == 0 {
		cfg.ReadinessTimeout = 2 * time.Second
	}
	if cfg.MaxRequestBytes == 0 {
		cfg.MaxRequestBytes = 1 << 20
	}
	return &Server{
		logger:           cfg.Logger,
		pinger:           cfg.Pinger,
		version:          cfg.Version,
		readinessTimeout: cfg.ReadinessTimeout,
		maxRequestBytes:  cfg.MaxRequestBytes,
		auth:             cfg.Auth,
		register:         cfg.Register,
	}, nil
}

// Handler returns the fully wrapped HTTP handler.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", s.healthz)
	mux.HandleFunc("GET /readyz", s.readyz)
	mux.HandleFunc("GET /version", s.versionHandler)
	if s.register != nil {
		s.register(mux)
	}
	mux.HandleFunc("/", s.notFound)

	known := map[string]struct{}{
		"/healthz": {},
		"/readyz":  {},
		"/version": {},
	}
	router := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			if _, ok := known[r.URL.Path]; ok {
				s.methodNotAllowed(w, r)
				return
			}
		}
		mux.ServeHTTP(w, r)
	})

	var h http.Handler = router
	if s.auth != nil {
		h = s.auth.Wrap(h, "/healthz", "/readyz")
	}
	h = s.limitBody(h)
	h = s.securityHeaders(h)
	h = recoverMiddleware(s.logger, h)
	h = s.logging(h)
	return h
}

func (s *Server) healthz(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (s *Server) readyz(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), s.readinessTimeout)
	defer cancel()
	if err := s.pinger.Ping(ctx); err != nil {
		s.logger.Warn("readiness check failed", "error", err)
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"status": "unavailable"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ready"})
}

func (s *Server) versionHandler(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"version": s.version})
}

func (s *Server) notFound(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusNotFound, map[string]string{"error": "not found"})
}

func (s *Server) methodNotAllowed(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
}

func (s *Server) limitBody(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Body != nil {
			r.Body = http.MaxBytesReader(w, r.Body, s.maxRequestBytes)
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Referrer-Policy", "no-referrer")
		next.ServeHTTP(w, r)
	})
}

type statusRecorder struct {
	http.ResponseWriter
	status int
	bytes  int
}

func (r *statusRecorder) WriteHeader(status int) {
	if r.status != 0 {
		return
	}
	r.status = status
	r.ResponseWriter.WriteHeader(status)
}

func (r *statusRecorder) Write(p []byte) (int, error) {
	if r.status == 0 {
		r.WriteHeader(http.StatusOK)
	}
	n, err := r.ResponseWriter.Write(p)
	r.bytes += n
	return n, err
}

func (s *Server) logging(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		requestID := r.Header.Get("X-Request-ID")
		if requestID == "" {
			requestID = newRequestID()
		}
		w.Header().Set("X-Request-ID", requestID)
		rec := &statusRecorder{ResponseWriter: w}
		next.ServeHTTP(rec, r)
		if rec.status == 0 {
			rec.status = http.StatusOK
		}
		attrs := []any{
			"method", r.Method,
			"path", r.URL.Path,
			"status", rec.status,
			"duration_ms", float64(time.Since(start).Microseconds()) / 1000,
			"remote", r.RemoteAddr,
			"request_id", requestID,
			"response_bytes", rec.bytes,
		}
		if rec.status >= http.StatusInternalServerError {
			s.logger.Error("request", attrs...)
		} else {
			s.logger.Info("request", attrs...)
		}
	})
}

type recoveryWriter struct {
	http.ResponseWriter
	wroteHeader bool
}

func (w *recoveryWriter) WriteHeader(status int) {
	w.wroteHeader = true
	w.ResponseWriter.WriteHeader(status)
}

func (w *recoveryWriter) Write(p []byte) (int, error) {
	if !w.wroteHeader {
		w.WriteHeader(http.StatusOK)
	}
	return w.ResponseWriter.Write(p)
}

func recoverMiddleware(logger *slog.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rw := &recoveryWriter{ResponseWriter: w}
		defer func() {
			if recovered := recover(); recovered != nil {
				logger.Error("panic recovered", "panic", recovered, "stack", string(debug.Stack()))
				if !rw.wroteHeader {
					writeJSON(rw, http.StatusInternalServerError, map[string]string{"error": "internal server error"})
				}
			}
		}()
		next.ServeHTTP(rw, r)
	})
}

func newRequestID() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return ""
	}
	return hex.EncodeToString(b[:])
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	_ = enc.Encode(value)
}
