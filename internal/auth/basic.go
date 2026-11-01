// Package auth provides the optional HTTP Basic authentication guard used by
// the public gateway surface. It is deliberately small: one operator-managed
// credential, constant-time comparison, and a fixed bypass list for Kubernetes
// health probes.
package auth

import (
	"crypto/subtle"
	"encoding/json"
	"errors"
	"net/http"
)

const realm = "skopos"

// Basic validates one username/password pair. The credential is held in
// memory so Basic auth can compare it; it is supplied by the operator through
// a Secret or environment variable and is never logged. Basic auth is only
// safe over TLS.
type Basic struct {
	username string
	password string
}

// New validates and constructs a Basic guard. Both values must be non-empty;
// an operator who enables auth with a blank credential is configuration error,
// not an open gateway.
func New(username, password string) (*Basic, error) {
	if username == "" {
		return nil, errors.New("auth: username must not be empty")
	}
	if password == "" {
		return nil, errors.New("auth: password must not be empty")
	}
	return &Basic{username: username, password: password}, nil
}

// Wrap returns a handler that requires Basic auth except for the exact bypass
// paths. Bypass paths are intended for /healthz and /readyz only; do not add
// query paths to this list.
func (b *Basic) Wrap(next http.Handler, bypassPaths ...string) http.Handler {
	bypass := make(map[string]struct{}, len(bypassPaths))
	for _, path := range bypassPaths {
		bypass[path] = struct{}{}
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, ok := bypass[r.URL.Path]; ok {
			next.ServeHTTP(w, r)
			return
		}
		if !b.authorized(r) {
			w.Header().Set("WWW-Authenticate", `Basic realm="`+realm+`", charset="UTF-8"`)
			writeUnauthorized(w)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (b *Basic) authorized(r *http.Request) bool {
	username, password, ok := r.BasicAuth()
	if !ok {
		return false
	}
	userOK := subtle.ConstantTimeCompare([]byte(b.username), []byte(username))
	passOK := subtle.ConstantTimeCompare([]byte(b.password), []byte(password))
	return userOK&passOK == 1
}

func writeUnauthorized(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusUnauthorized)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": "unauthorized"})
}
