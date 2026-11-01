package main

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/jawaracloud/skopos/internal/prom"
)

type fakeMetadataFetcher struct {
	values map[string]prom.Metadata
	err    error
	calls  int
}

func (f *fakeMetadataFetcher) Metadata(context.Context, string) (map[string]prom.Metadata, error) {
	f.calls++
	return f.values, f.err
}

func TestContextObserverNormalizesAndCaches(t *testing.T) {
	fetcher := &fakeMetadataFetcher{values: map[string]prom.Metadata{
		"http_server_request_duration_seconds": {
			Type: "histogram", Unit: "s", Help: "HTTP duration",
		},
	}}
	observer := newContextObserver(fetcher, slog.New(slog.NewJSONHandler(io.Discard, nil)))
	now := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	observer.now = func() time.Time { return now }

	item, ok, err := observer.Observe(context.Background(), "http.server.request.duration.seconds")
	if err != nil || !ok {
		t.Fatalf("Observe = %#v, %v, %v", item, ok, err)
	}
	if item.Name != "http_server_request_duration_seconds" || item.Type != "histogram" || item.Unit != "s" {
		t.Fatalf("item = %#v", item)
	}
	_, _, _ = observer.Observe(context.Background(), "another_metric")
	if fetcher.calls != 1 {
		t.Fatalf("fetcher calls = %d, want cached 1", fetcher.calls)
	}
	now = now.Add(observedMetadataTTL + time.Second)
	_, _ = observer.ListObserved(context.Background())
	if fetcher.calls != 2 {
		t.Fatalf("fetcher calls after TTL = %d, want 2", fetcher.calls)
	}
}

func TestContextObserverError(t *testing.T) {
	observer := newContextObserver(
		&fakeMetadataFetcher{err: errors.New("clickhouse down")},
		slog.New(slog.NewJSONHandler(io.Discard, nil)),
	)
	if _, _, err := observer.Observe(context.Background(), "metric"); err == nil {
		t.Fatal("expected error")
	}
	if _, err := observer.ListObserved(context.Background()); err == nil {
		t.Fatal("expected list error")
	}
}
