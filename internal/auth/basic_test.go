package auth

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestNewRejectsEmptyCredentials(t *testing.T) {
	if _, err := New("", "pass"); err == nil {
		t.Fatal("expected empty username to fail")
	}
	if _, err := New("user", ""); err == nil {
		t.Fatal("expected empty password to fail")
	}
	if _, err := New("user", "pass"); err != nil {
		t.Fatalf("valid credentials: %v", err)
	}
}

func TestWrap(t *testing.T) {
	guard, err := New("alice", "correct horse battery staple")
	if err != nil {
		t.Fatal(err)
	}
	next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	handler := guard.Wrap(next, "/healthz", "/readyz")

	tests := []struct {
		name       string
		method     string
		path       string
		user, pass string
		wantStatus int
	}{
		{name: "protected without header", method: http.MethodGet, path: "/api/v1/query", wantStatus: http.StatusUnauthorized},
		{name: "protected wrong user", method: http.MethodGet, path: "/api/v1/query", user: "bob", pass: "correct horse battery staple", wantStatus: http.StatusUnauthorized},
		{name: "protected wrong password", method: http.MethodGet, path: "/api/v1/query", user: "alice", pass: "wrong", wantStatus: http.StatusUnauthorized},
		{name: "protected correct", method: http.MethodPost, path: "/api/v1/query", user: "alice", pass: "correct horse battery staple", wantStatus: http.StatusNoContent},
		{name: "version protected", method: http.MethodGet, path: "/version", wantStatus: http.StatusUnauthorized},
		{name: "health bypass", method: http.MethodGet, path: "/healthz", wantStatus: http.StatusNoContent},
		{name: "ready bypass", method: http.MethodGet, path: "/readyz", wantStatus: http.StatusNoContent},
		{name: "non-get health reaches handler", method: http.MethodPost, path: "/healthz", wantStatus: http.StatusNoContent},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(tc.method, tc.path, nil)
			if tc.user != "" || tc.pass != "" {
				req.Header.Set("Authorization", "Basic "+base64.StdEncoding.EncodeToString([]byte(tc.user+":"+tc.pass)))
			}
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)
			if rec.Code != tc.wantStatus {
				t.Fatalf("status = %d, want %d", rec.Code, tc.wantStatus)
			}
			if tc.wantStatus == http.StatusUnauthorized {
				if got := rec.Header().Get("WWW-Authenticate"); got == "" {
					t.Fatal("401 response is missing WWW-Authenticate")
				}
				var body map[string]string
				if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
					t.Fatalf("401 body is not JSON: %v", err)
				}
				if body["error"] != "unauthorized" {
					t.Fatalf("401 body = %v", body)
				}
			}
		})
	}
}

func TestWrapMalformedAuthorization(t *testing.T) {
	guard, err := New("alice", "pass")
	if err != nil {
		t.Fatal(err)
	}
	handler := guard.Wrap(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	for _, header := range []string{"Basic !!!", "Bearer token", "Basic " + base64.StdEncoding.EncodeToString([]byte("no-colon"))} {
		req := httptest.NewRequest(http.MethodGet, "/api/v1/query", nil)
		req.Header.Set("Authorization", header)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("header %q: status = %d, want 401", header, rec.Code)
		}
	}
}
