package checkpoint

import (
	"bytes"
	"compress/gzip"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
)

func gzipRecords(t *testing.T, records ...Record) []byte {
	t.Helper()
	var raw []byte
	for _, record := range records {
		framed, err := encodeRecord(record.Signal, record.Payload)
		if err != nil {
			t.Fatal(err)
		}
		raw = append(raw, framed...)
	}
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	if _, err := zw.Write(raw); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

type replayRequest struct {
	Method      string
	Path        string
	ContentType string
	Body        []byte
}

type replayCollector struct {
	*httptest.Server
	mu       sync.Mutex
	requests []replayRequest
	status   int
}

func newReplayCollector(t *testing.T, status int) *replayCollector {
	t.Helper()
	collector := &replayCollector{status: status}
	collector.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		collector.mu.Lock()
		collector.requests = append(collector.requests, replayRequest{
			Method:      r.Method,
			Path:        r.URL.Path,
			ContentType: r.Header.Get("Content-Type"),
			Body:        body,
		})
		status := collector.status
		collector.mu.Unlock()
		w.WriteHeader(status)
	}))
	t.Cleanup(collector.Close)
	return collector
}

func (c *replayCollector) captured() []replayRequest {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]replayRequest(nil), c.requests...)
}

func TestReplayReinjectsOTLP(t *testing.T) {
	server := newFakeS3(t)
	tracePayload := []byte("trace payload")
	logPayload := []byte("log payload")
	server.store.put("prefix/2026/01/01/seg-100.wal.gz", gzipRecords(t,
		Record{Signal: SignalTraces, Payload: tracePayload},
		Record{Signal: SignalLogs, Payload: logPayload},
	))
	server.store.put("prefix/notes.txt", []byte("not a segment"))

	collector := newReplayCollector(t, http.StatusOK)
	s3 := testS3Client(t, server.URL)

	stats, err := Replay(context.Background(), ReplayConfig{
		S3:         s3,
		Prefix:     "prefix",
		ForwardURL: collector.URL + "/otlp/",
	})
	if err != nil {
		t.Fatalf("Replay: %v", err)
	}
	if stats.Objects != 1 || stats.Records != 2 {
		t.Fatalf("stats = %+v, want 1 object and 2 records", stats)
	}
	if stats.SkippedObjects != 1 {
		t.Fatalf("SkippedObjects = %d, want 1", stats.SkippedObjects)
	}
	if want := int64(len(tracePayload) + len(logPayload)); stats.PayloadBytes != want {
		t.Fatalf("PayloadBytes = %d, want %d", stats.PayloadBytes, want)
	}

	requests := collector.captured()
	if len(requests) != 2 {
		t.Fatalf("collector received %d requests, want 2", len(requests))
	}
	if requests[0].Method != http.MethodPost || requests[0].Path != "/otlp/v1/traces" {
		t.Fatalf("first request = %+v", requests[0])
	}
	if requests[1].Path != "/otlp/v1/logs" {
		t.Fatalf("second request path = %q", requests[1].Path)
	}
	if !bytes.Equal(requests[0].Body, tracePayload) || !bytes.Equal(requests[1].Body, logPayload) {
		t.Fatal("replayed payloads do not match the checkpoint")
	}
	for _, request := range requests {
		if request.ContentType != "application/x-protobuf" {
			t.Errorf("Content-Type = %q", request.ContentType)
		}
	}

	if _, ok := server.store.get("prefix/2026/01/01/seg-100.wal.gz"); !ok {
		t.Fatal("replay deleted the object without DeleteAfter")
	}

	second, err := Replay(context.Background(), ReplayConfig{
		S3:          s3,
		Prefix:      "prefix",
		ForwardURL:  collector.URL + "/otlp",
		DeleteAfter: true,
	})
	if err != nil {
		t.Fatalf("second Replay: %v", err)
	}
	if second.DeletedObjects != 1 {
		t.Fatalf("DeletedObjects = %d, want 1", second.DeletedObjects)
	}
	if _, ok := server.store.get("prefix/2026/01/01/seg-100.wal.gz"); ok {
		t.Fatal("replayed object still exists")
	}
	if _, ok := server.store.get("prefix/notes.txt"); !ok {
		t.Fatal("replay deleted a non-segment object")
	}
}

func TestReplayPropagatesCollectorError(t *testing.T) {
	server := newFakeS3(t)
	server.store.put("prefix/2026/01/01/seg-200.wal.gz", gzipRecords(t,
		Record{Signal: SignalMetrics, Payload: []byte("metrics")},
	))
	collector := newReplayCollector(t, http.StatusServiceUnavailable)
	s3 := testS3Client(t, server.URL)

	_, err := Replay(context.Background(), ReplayConfig{
		S3:         s3,
		Prefix:     "prefix",
		ForwardURL: collector.URL,
	})
	if err == nil {
		t.Fatal("expected replay error")
	}
	if _, ok := server.store.get("prefix/2026/01/01/seg-200.wal.gz"); !ok {
		t.Fatal("failed replay deleted the object")
	}
}

func TestReplayCorruptSegment(t *testing.T) {
	server := newFakeS3(t)
	server.store.put("prefix/2026/01/01/seg-300.wal.gz", []byte("not gzip"))
	collector := newReplayCollector(t, http.StatusOK)
	s3 := testS3Client(t, server.URL)

	_, err := Replay(context.Background(), ReplayConfig{
		S3:         s3,
		Prefix:     "prefix",
		ForwardURL: collector.URL,
	})
	if err == nil {
		t.Fatal("expected corrupt-segment error")
	}
	if len(collector.captured()) != 0 {
		t.Fatal("corrupt segment was forwarded")
	}
}

func TestReplayValidation(t *testing.T) {
	server := newFakeS3(t)
	s3 := testS3Client(t, server.URL)
	tests := []ReplayConfig{
		{ForwardURL: "http://collector"},
		{S3: s3},
		{S3: s3, ForwardURL: "ftp://collector"},
		{S3: s3, ForwardURL: "http://"},
		{S3: s3, ForwardURL: "http://collector?x=1"},
	}
	for _, cfg := range tests {
		if _, err := Replay(context.Background(), cfg); err == nil {
			t.Errorf("Replay(%+v) succeeded, want error", cfg)
		}
	}
}
