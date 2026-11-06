package main

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jawaracloud/skopos/internal/checkpoint"
)

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func newTestServer(t *testing.T, cfg checkpoint.Config, maxRequestBytes int64) (*checkpoint.Checkpoint, *httptest.Server) {
	t.Helper()
	cfg.Logger = testLogger()
	cp, err := checkpoint.Open(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := cp.Close(); err != nil {
			t.Errorf("close checkpoint: %v", err)
		}
	})
	s := &server{checkpoint: cp, logger: testLogger(), maxRequestBytes: maxRequestBytes}
	ts := httptest.NewServer(s.routes())
	t.Cleanup(ts.Close)
	return cp, ts
}

func testCheckpointConfig(t *testing.T) checkpoint.Config {
	t.Helper()
	return checkpoint.Config{
		Dir:           t.TempDir(),
		SealBytes:     1 << 20,
		SealInterval:  time.Hour,
		MaxLocalBytes: 1 << 20,
		FlushTimeout:  time.Second,
	}
}

type httpResult struct {
	Status int
	Header http.Header
	Body   []byte
}

func doRequest(t *testing.T, method, url string, body []byte, headers map[string]string) httpResult {
	t.Helper()
	req, err := http.NewRequest(method, url, bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	for key, value := range headers {
		req.Header.Set(key, value)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return httpResult{Status: resp.StatusCode, Header: resp.Header, Body: data}
}

func readSegments(t *testing.T, dir string) []byte {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var out []byte
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasPrefix(entry.Name(), "seg-") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(dir, entry.Name()))
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, data...)
	}
	return out
}

func TestOTLPReceiverStoresPayload(t *testing.T) {
	cfg := testCheckpointConfig(t)
	cp, server := newTestServer(t, cfg, checkpoint.MaxRecordBytes)
	payload := []byte("protobuf trace payload")

	result := doRequest(t, http.MethodPost, server.URL+"/v1/traces", payload, map[string]string{
		"Content-Type": "application/x-protobuf",
	})
	if result.Status != http.StatusOK {
		t.Fatalf("status = %d, body = %s", result.Status, result.Body)
	}
	if got := result.Header.Get("Content-Type"); got != "application/x-protobuf" {
		t.Fatalf("response Content-Type = %q", got)
	}
	if err := cp.Flush(context.Background()); err != nil {
		t.Fatalf("Flush: %v", err)
	}
	if data := readSegments(t, cfg.Dir); !bytes.Contains(data, payload) {
		t.Fatal("stored segment does not contain the OTLP payload")
	}

	stats := cp.Stats()
	if stats.RecordsAppended != 1 || stats.PayloadBytesAppended != uint64(len(payload)) {
		t.Fatalf("stats = %+v", stats)
	}
	if stats.SegmentsSealed != 1 {
		t.Fatalf("SegmentsSealed = %d, want 1", stats.SegmentsSealed)
	}
}

func TestOTLPReceiverDecompressesGzip(t *testing.T) {
	cfg := testCheckpointConfig(t)
	cp, server := newTestServer(t, cfg, checkpoint.MaxRecordBytes)
	payload := []byte("gzip trace payload")

	var compressed bytes.Buffer
	zw := gzip.NewWriter(&compressed)
	if _, err := zw.Write(payload); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}

	result := doRequest(t, http.MethodPost, server.URL+"/v1/traces", compressed.Bytes(), map[string]string{
		"Content-Type":     "application/x-protobuf",
		"Content-Encoding": "gzip",
	})
	if result.Status != http.StatusOK {
		t.Fatalf("status = %d, body = %s", result.Status, result.Body)
	}
	if err := cp.Flush(context.Background()); err != nil {
		t.Fatalf("Flush: %v", err)
	}
	stored := readSegments(t, cfg.Dir)
	if !bytes.Contains(stored, payload) {
		t.Fatal("stored segment does not contain the decompressed payload")
	}
	if bytes.HasPrefix(stored, []byte{0x1f, 0x8b}) {
		t.Fatal("stored segment is still gzip-compressed")
	}
}

func TestOTLPReceiverRejectsBadRequests(t *testing.T) {
	cfg := testCheckpointConfig(t)
	_, server := newTestServer(t, cfg, 8)

	tests := []struct {
		name    string
		method  string
		path    string
		body    []byte
		headers map[string]string
		status  int
	}{
		{
			name:    "content type",
			method:  http.MethodPost,
			path:    "/v1/logs",
			body:    []byte("x"),
			headers: map[string]string{"Content-Type": "application/json"},
			status:  http.StatusUnsupportedMediaType,
		},
		{
			name:    "content encoding",
			method:  http.MethodPost,
			path:    "/v1/logs",
			body:    []byte("x"),
			headers: map[string]string{"Content-Type": "application/x-protobuf", "Content-Encoding": "br"},
			status:  http.StatusUnsupportedMediaType,
		},
		{
			name:    "empty body",
			method:  http.MethodPost,
			path:    "/v1/logs",
			body:    nil,
			headers: map[string]string{"Content-Type": "application/x-protobuf"},
			status:  http.StatusBadRequest,
		},
		{
			name:   "too large",
			method: http.MethodPost,
			path:   "/v1/logs",
			body:   []byte("0123456789"),
			headers: map[string]string{
				"Content-Type": "application/x-protobuf",
			},
			status: http.StatusRequestEntityTooLarge,
		},
		{
			name:   "wrong method",
			method: http.MethodGet,
			path:   "/v1/traces",
			status: http.StatusMethodNotAllowed,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			result := doRequest(t, test.method, server.URL+test.path, test.body, test.headers)
			if result.Status != test.status {
				t.Fatalf("status = %d, want %d (body %s)", result.Status, test.status, result.Body)
			}
		})
	}
}

func TestOTLPReceiverBackpressure(t *testing.T) {
	cfg := testCheckpointConfig(t)
	cfg.SealBytes = 20
	cfg.MaxLocalBytes = 30
	cp, server := newTestServer(t, cfg, checkpoint.MaxRecordBytes)

	if err := cp.Append(checkpoint.SignalLogs, bytes.Repeat([]byte{0x01}, 10)); err != nil {
		t.Fatalf("prefill Append: %v", err)
	}
	result := doRequest(t, http.MethodPost, server.URL+"/v1/logs", bytes.Repeat([]byte{0x02}, 10), map[string]string{
		"Content-Type": "application/x-protobuf",
	})
	if result.Status != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503 (body %s)", result.Status, result.Body)
	}
	if got := result.Header.Get("Retry-After"); got != "1" {
		t.Fatalf("Retry-After = %q", got)
	}
}

func TestHealthReadyStats(t *testing.T) {
	cfg := testCheckpointConfig(t)
	_, server := newTestServer(t, cfg, checkpoint.MaxRecordBytes)

	health := doRequest(t, http.MethodGet, server.URL+"/healthz", nil, nil)
	if health.Status != http.StatusOK || strings.TrimSpace(string(health.Body)) != "ok" {
		t.Fatalf("healthz = %d %q", health.Status, health.Body)
	}
	ready := doRequest(t, http.MethodGet, server.URL+"/readyz", nil, nil)
	if ready.Status != http.StatusOK || strings.TrimSpace(string(ready.Body)) != "ready" {
		t.Fatalf("readyz = %d %q", ready.Status, ready.Body)
	}
	stats := doRequest(t, http.MethodGet, server.URL+"/stats", nil, nil)
	if stats.Status != http.StatusOK {
		t.Fatalf("stats status = %d", stats.Status)
	}
	var decoded checkpoint.Stats
	if err := json.Unmarshal(stats.Body, &decoded); err != nil {
		t.Fatalf("stats JSON: %v", err)
	}
	if decoded.RecordsAppended != 0 || decoded.Closed {
		t.Fatalf("stats = %+v", decoded)
	}
}
