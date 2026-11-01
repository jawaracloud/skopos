package frontend

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func newTestServer(t *testing.T, backends ...string) *Server {
	t.Helper()
	s, err := New(Config{
		Backends:         backends,
		Version:          "test-version",
		ReadinessTimeout: time.Second,
		ProxyTimeout:     time.Second,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return s
}

func doRequest(t *testing.T, h http.Handler, method, target string, body io.Reader) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, target, body)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestNewValidation(t *testing.T) {
	tests := []struct {
		name string
		cfg  Config
	}{
		{"no backends", Config{}},
		{"relative backend", Config{Backends: []string{"/query"}}},
		{"bad scheme", Config{Backends: []string{"ftp://query:8080"}}},
		{"no host", Config{Backends: []string{"http://"}}},
		{"negative readiness timeout", Config{Backends: []string{"http://query:8080"}, ReadinessTimeout: -time.Second}},
		{"negative proxy timeout", Config{Backends: []string{"http://query:8080"}, ProxyTimeout: -time.Second}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := New(tc.cfg); err == nil {
				t.Fatal("expected error")
			}
		})
	}
}

func TestLocalEndpointsAreNotProxied(t *testing.T) {
	var calls atomic.Int32
	var paths atomic.Value
	paths.Store([]string{})
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		paths.Store(append(paths.Load().([]string), r.URL.Path))
		w.WriteHeader(http.StatusOK)
	}))
	defer backend.Close()

	s := newTestServer(t, backend.URL)
	h := s.Handler()

	if rec := doRequest(t, h, http.MethodGet, "http://frontend/healthz", nil); rec.Code != http.StatusOK {
		t.Fatalf("/healthz status = %d", rec.Code)
	}
	if rec := doRequest(t, h, http.MethodGet, "http://frontend/version", nil); rec.Code != http.StatusOK ||
		!strings.Contains(rec.Body.String(), "test-version") {
		t.Fatalf("/version = %d %q", rec.Code, rec.Body.String())
	}
	if rec := doRequest(t, h, http.MethodGet, "http://frontend/readyz", nil); rec.Code != http.StatusOK {
		t.Fatalf("/readyz status = %d", rec.Code)
	}

	got := paths.Load().([]string)
	if len(got) != 1 || got[0] != "/readyz" {
		t.Fatalf("backend paths = %#v, want only /readyz", got)
	}
	if calls.Load() != 1 {
		t.Fatalf("backend calls = %d, want 1", calls.Load())
	}
}

func TestProxyPreservesMethodPathQueryHeadersAndBody(t *testing.T) {
	type observedRequest struct {
		method   string
		path     string
		rawQuery string
		header   string
		body     string
	}
	observed := make(chan observedRequest, 1)
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		observed <- observedRequest{
			method:   r.Method,
			path:     r.URL.Path,
			rawQuery: r.URL.RawQuery,
			header:   r.Header.Get("X-Test"),
			body:     string(body),
		}
		w.Header().Set("X-Backend", "yes")
		w.WriteHeader(http.StatusCreated)
		_, _ = io.WriteString(w, "proxied response")
	}))
	defer backend.Close()

	s := newTestServer(t, backend.URL)
	h := s.Handler()
	req := httptest.NewRequest(http.MethodPost, "http://frontend/api/v1/query_range?start=1&end=2&x=a%2Fb", strings.NewReader("request body"))
	req.Header.Set("X-Test", "header value")
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, body = %q", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("X-Backend"); got != "yes" {
		t.Fatalf("response header X-Backend = %q", got)
	}
	if got := rec.Body.String(); got != "proxied response" {
		t.Fatalf("response body = %q", got)
	}

	got := <-observed
	if got.method != http.MethodPost || got.path != "/api/v1/query_range" ||
		got.rawQuery != "start=1&end=2&x=a%2Fb" || got.header != "header value" || got.body != "request body" {
		t.Fatalf("backend observed %#v", got)
	}
}

func TestProxyPreservesEscapedPath(t *testing.T) {
	type result struct{ path, rawPath string }
	results := make(chan result, 1)
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		results <- result{path: r.URL.Path, rawPath: r.URL.RawPath}
		w.WriteHeader(http.StatusOK)
	}))
	defer backend.Close()

	s := newTestServer(t, backend.URL+"/base/")
	h := s.Handler()
	if rec := doRequest(t, h, http.MethodGet, "http://frontend/api/name%2Fwith%2Fslash", nil); rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	got := <-results
	if got.path != "/base/api/name/with/slash" || got.rawPath != "/base/api/name%2Fwith%2Fslash" {
		t.Fatalf("backend path = %q raw = %q", got.path, got.rawPath)
	}
}

func TestProxyPrefixedBackendPath(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/base/api/v1/query" || r.URL.RawQuery != "q=1" {
			t.Errorf("backend got path %q query %q", r.URL.Path, r.URL.RawQuery)
		}
		_, _ = io.WriteString(w, "ok")
	}))
	defer backend.Close()

	s := newTestServer(t, backend.URL+"/base/")
	rec := doRequest(t, s.Handler(), http.MethodGet, "http://frontend/api/v1/query?q=1", nil)
	if rec.Code != http.StatusOK || rec.Body.String() != "ok" {
		t.Fatalf("status = %d body = %q", rec.Code, rec.Body.String())
	}
}

func TestRoundRobin(t *testing.T) {
	var first, second atomic.Int32
	firstBackend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		first.Add(1)
		_, _ = io.WriteString(w, "first")
	}))
	defer firstBackend.Close()
	secondBackend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		second.Add(1)
		_, _ = io.WriteString(w, "second")
	}))
	defer secondBackend.Close()

	s := newTestServer(t, firstBackend.URL, secondBackend.URL)
	h := s.Handler()
	want := []string{"first", "second", "first", "second", "first", "second"}
	for i, expected := range want {
		rec := doRequest(t, h, http.MethodGet, "http://frontend/api/v1/query", nil)
		if rec.Code != http.StatusOK || rec.Body.String() != expected {
			t.Fatalf("request %d = %d %q, want %q", i, rec.Code, rec.Body.String(), expected)
		}
	}
	if first.Load() != 3 || second.Load() != 3 {
		t.Fatalf("backend counts = %d/%d", first.Load(), second.Load())
	}
}

func TestProxySkipsUnreadyBackend(t *testing.T) {
	unready := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/readyz" {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		_, _ = io.WriteString(w, "unready")
	}))
	defer unready.Close()
	ready := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/readyz" {
			w.WriteHeader(http.StatusOK)
			return
		}
		_, _ = io.WriteString(w, "ready")
	}))
	defer ready.Close()

	s := newTestServer(t, unready.URL, ready.URL)
	h := s.Handler()
	rec := doRequest(t, h, http.MethodGet, "http://frontend/readyz", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("/readyz status = %d body = %q", rec.Code, rec.Body.String())
	}
	for i := 0; i < 4; i++ {
		rec = doRequest(t, h, http.MethodGet, "http://frontend/api/v1/query", nil)
		if rec.Code != http.StatusOK || rec.Body.String() != "ready" {
			t.Fatalf("proxy request %d = %d %q", i, rec.Code, rec.Body.String())
		}
	}
}

func TestReadyzRequiresAReadyBackend(t *testing.T) {
	unready := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer unready.Close()

	s := newTestServer(t, unready.URL)
	h := s.Handler()
	rec := doRequest(t, h, http.MethodGet, "http://frontend/readyz", nil)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("/readyz status = %d, want 503", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), `"ready_backends":0`) {
		t.Fatalf("/readyz body = %q", rec.Body.String())
	}

	rec = doRequest(t, h, http.MethodGet, "http://frontend/api/v1/query", nil)
	if rec.Code != http.StatusServiceUnavailable || !strings.Contains(rec.Body.String(), "no ready query backends") {
		t.Fatalf("proxy with no ready backends = %d %q", rec.Code, rec.Body.String())
	}
}

func TestBackendFailureReturnsBadGatewayThenServiceUnavailable(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	backendURL := backend.URL
	backend.Close()

	s := newTestServer(t, backendURL)
	h := s.Handler()
	rec := doRequest(t, h, http.MethodGet, "http://frontend/api/v1/query", nil)
	if rec.Code != http.StatusBadGateway || !strings.Contains(rec.Body.String(), "query backend request failed") {
		t.Fatalf("first proxy = %d %q, want 502", rec.Code, rec.Body.String())
	}

	rec = doRequest(t, h, http.MethodGet, "http://frontend/api/v1/query", nil)
	if rec.Code != http.StatusServiceUnavailable || !strings.Contains(rec.Body.String(), "no ready query backends") {
		t.Fatalf("second proxy = %d %q, want 503", rec.Code, rec.Body.String())
	}
}

func TestBackendTimeoutReturnsBadGateway(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}))
	defer backend.Close()

	s, err := New(Config{
		Backends:     []string{backend.URL},
		ProxyTimeout: 25 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	h := s.Handler()
	start := time.Now()
	rec := doRequest(t, h, http.MethodGet, "http://frontend/api/v1/query", nil)
	if rec.Code != http.StatusBadGateway || !strings.Contains(rec.Body.String(), "timed out") {
		t.Fatalf("proxy = %d %q, want 502 timeout", rec.Code, rec.Body.String())
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("proxy timeout took %s", elapsed)
	}
}

func TestLocalEndpointsRejectNonGet(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer backend.Close()

	s := newTestServer(t, backend.URL)
	rec := doRequest(t, s.Handler(), http.MethodPost, "http://frontend/healthz", strings.NewReader(""))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST /healthz status = %d", rec.Code)
	}
}
