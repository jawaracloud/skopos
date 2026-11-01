package httpapi

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jawaracloud/skopos/internal/auth"
)

type fakePinger struct {
	err   error
	delay time.Duration
}

func (f fakePinger) Ping(ctx context.Context) error {
	if f.delay > 0 {
		select {
		case <-time.After(f.delay):
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return f.err
}

func newTestServer(t *testing.T, p Pinger, opts ...func(*Config)) *Server {
	t.Helper()
	cfg := Config{
		Logger:           slog.New(slog.NewJSONHandler(&bytes.Buffer{}, nil)),
		Pinger:           p,
		Version:          "test-version",
		ReadinessTimeout: 100 * time.Millisecond,
	}
	for _, opt := range opts {
		opt(&cfg)
	}
	s, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return s
}

func doRequest(t *testing.T, h http.Handler, method, path string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func decodeBody(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode body %q: %v", rec.Body.String(), err)
	}
	return body
}

func TestHealthz(t *testing.T) {
	s := newTestServer(t, fakePinger{})
	rec := doRequest(t, s.Handler(), http.MethodGet, "/healthz")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if got := decodeBody(t, rec)["status"]; got != "ok" {
		t.Fatalf("status body = %v", got)
	}
	if got := rec.Header().Get("Content-Type"); !strings.HasPrefix(got, "application/json") {
		t.Fatalf("content type = %q", got)
	}
	if rec.Header().Get("X-Content-Type-Options") != "nosniff" {
		t.Fatal("missing nosniff header")
	}
}

func TestReadyzOK(t *testing.T) {
	s := newTestServer(t, fakePinger{})
	rec := doRequest(t, s.Handler(), http.MethodGet, "/readyz")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body)
	}
	if got := decodeBody(t, rec)["status"]; got != "ready" {
		t.Fatalf("status body = %v", got)
	}
}

func TestReadyzUnavailable(t *testing.T) {
	s := newTestServer(t, fakePinger{err: errors.New("clickhouse down: password=secret")})
	rec := doRequest(t, s.Handler(), http.MethodGet, "/readyz")
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body)
	}
	body := decodeBody(t, rec)
	if body["status"] != "unavailable" {
		t.Fatalf("status body = %v", body)
	}
	if strings.Contains(rec.Body.String(), "secret") {
		t.Fatalf("readiness response leaked an internal error: %s", rec.Body)
	}
}

func TestReadyzTimeout(t *testing.T) {
	s := newTestServer(t, fakePinger{delay: time.Second})
	start := time.Now()
	rec := doRequest(t, s.Handler(), http.MethodGet, "/readyz")
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d", rec.Code)
	}
	if elapsed := time.Since(start); elapsed > 500*time.Millisecond {
		t.Fatalf("readiness took too long: %s", elapsed)
	}
}

func TestVersion(t *testing.T) {
	s := newTestServer(t, fakePinger{})
	rec := doRequest(t, s.Handler(), http.MethodGet, "/version")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if got := decodeBody(t, rec)["version"]; got != "test-version" {
		t.Fatalf("version = %v", got)
	}
}

func TestNotFoundAndMethodNotAllowed(t *testing.T) {
	s := newTestServer(t, fakePinger{})
	h := s.Handler()

	rec := doRequest(t, h, http.MethodGet, "/nope")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("not found status = %d", rec.Code)
	}
	if got := decodeBody(t, rec)["error"]; got != "not found" {
		t.Fatalf("not found body = %v", got)
	}

	rec = doRequest(t, h, http.MethodPost, "/healthz")
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("method status = %d", rec.Code)
	}
	if got := decodeBody(t, rec)["error"]; got != "method not allowed" {
		t.Fatalf("method body = %v", got)
	}

	rec = doRequest(t, h, http.MethodHead, "/healthz")
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("head status = %d", rec.Code)
	}
}

func TestBasicAuth(t *testing.T) {
	guard, err := auth.New("viewer", "secret")
	if err != nil {
		t.Fatal(err)
	}
	s := newTestServer(t, fakePinger{}, func(cfg *Config) { cfg.Auth = guard })
	h := s.Handler()

	for _, path := range []string{"/healthz", "/readyz"} {
		rec := doRequest(t, h, http.MethodGet, path)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s bypass status = %d", path, rec.Code)
		}
	}

	rec := doRequest(t, h, http.MethodGet, "/version")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("version without auth status = %d", rec.Code)
	}
	if rec.Header().Get("WWW-Authenticate") == "" {
		t.Fatal("missing WWW-Authenticate")
	}

	rec = doRequest(t, h, http.MethodGet, "/api/v1/query")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("query without auth status = %d", rec.Code)
	}

	req := httptest.NewRequest(http.MethodGet, "/version", nil)
	req.Header.Set("Authorization", "Basic "+base64.StdEncoding.EncodeToString([]byte("viewer:secret")))
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("version with auth status = %d", rec.Code)
	}
}

func TestRecovery(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, nil))
	h := recoverMiddleware(logger, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		panic("boom")
	}))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/panic", nil))
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "internal server error") {
		t.Fatalf("body = %q", rec.Body.String())
	}
	if !strings.Contains(buf.String(), "panic") {
		t.Fatalf("log = %q", buf.String())
	}
}

func TestLogging(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, nil))
	s := newTestServer(t, fakePinger{}, func(cfg *Config) { cfg.Logger = logger })
	doRequest(t, s.Handler(), http.MethodGet, "/healthz")
	log := buf.String()
	for _, want := range []string{`"method":"GET"`, `"path":"/healthz"`, `"status":200`} {
		if !strings.Contains(log, want) {
			t.Fatalf("log %q does not contain %q", log, want)
		}
	}
}

func TestNewRejectsInvalidConfig(t *testing.T) {
	tests := []struct {
		name string
		cfg  Config
	}{
		{"no pinger", Config{}},
		{"negative readiness timeout", Config{Pinger: fakePinger{}, ReadinessTimeout: -time.Second}},
		{"negative max body", Config{Pinger: fakePinger{}, MaxRequestBytes: -1}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := New(tc.cfg); err == nil {
				t.Fatal("expected error")
			}
		})
	}
}
