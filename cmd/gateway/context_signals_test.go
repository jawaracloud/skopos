package main

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/jawaracloud/skopos/internal/chclient"
	"github.com/jawaracloud/skopos/internal/plan"
)

type fakeQueryer struct {
	queries []string
	err     error
}

func (f *fakeQueryer) Query(_ context.Context, sql string, _ []plan.Param) (*chclient.Result, error) {
	f.queries = append(f.queries, sql)
	if f.err != nil {
		return nil, f.err
	}
	if strings.Contains(sql, "otel_traces") {
		return &chclient.Result{Rows: []map[string]any{{
			"ServiceName": "checkout", "SpanName": "GET /checkout", "SpanKind": "server", "count": int64(7),
		}}}, nil
	}
	return &chclient.Result{Rows: []map[string]any{{
		"ServiceName": "checkout", "SeverityText": "ERROR", "EventName": "exception", "count": int64(3),
	}}}, nil
}

func TestObservedSignals(t *testing.T) {
	queryer := &fakeQueryer{}
	signals := newObservedSignals(queryer, "otel", plan.DefaultTableNames(),
		slog.New(slog.NewJSONHandler(io.Discard, nil)))
	now := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	signals.now = func() time.Time { return now }

	logs, err := signals.ListObservedLogs(context.Background())
	if err != nil || len(logs) != 1 || logs[0].Event != "exception" || logs[0].Count != 3 {
		t.Fatalf("logs = %#v, %v", logs, err)
	}
	traces, err := signals.ListObservedTraces(context.Background())
	if err != nil || len(traces) != 1 || traces[0].Operation != "GET /checkout" || traces[0].Count != 7 {
		t.Fatalf("traces = %#v, %v", traces, err)
	}
	if len(queryer.queries) != 2 {
		t.Fatalf("queries = %d, want 2", len(queryer.queries))
	}
	for _, query := range queryer.queries {
		if !strings.Contains(query, "`otel`") || !strings.Contains(query, "INTERVAL 7 DAY") {
			t.Fatalf("query = %s", query)
		}
		if !strings.Contains(query, " FINAL ") {
			t.Fatalf("query does not deduplicate replay rows: %s", query)
		}
	}

	// Cache is independent for logs and traces.
	_, _ = signals.ListObservedLogs(context.Background())
	_, _ = signals.ListObservedTraces(context.Background())
	if len(queryer.queries) != 2 {
		t.Fatalf("cached queries = %d", len(queryer.queries))
	}
	now = now.Add(observedSignalsTTL + time.Second)
	_, _ = signals.ListObservedLogs(context.Background())
	if len(queryer.queries) != 3 {
		t.Fatalf("expired log queries = %d", len(queryer.queries))
	}
}

func TestObservedSignalsError(t *testing.T) {
	signals := newObservedSignals(&fakeQueryer{err: errors.New("clickhouse down")}, "otel",
		plan.DefaultTableNames(), slog.New(slog.NewJSONHandler(io.Discard, nil)))
	if _, err := signals.ListObservedLogs(context.Background()); err == nil {
		t.Fatal("expected log error")
	}
	if _, err := signals.ListObservedTraces(context.Background()); err == nil {
		t.Fatal("expected trace error")
	}
}
