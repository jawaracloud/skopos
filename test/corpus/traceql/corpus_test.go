package traceqlcorpus

import (
	"encoding/hex"
	"encoding/json"
	"testing"
)

func TestCorpusLoads(t *testing.T) {
	c, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(c.Traces) != 3 {
		t.Fatalf("traces = %d", len(c.Traces))
	}
	if c.BaseTime().IsZero() {
		t.Fatal("base time is zero")
	}
	spanIDs := make(map[string]bool)
	traceIDs := make(map[string]bool)
	for _, trace := range c.Traces {
		if len(trace.TraceID) != 32 {
			t.Fatalf("trace ID %q is not 32 hex characters", trace.TraceID)
		}
		if _, err := hex.DecodeString(trace.TraceID); err != nil {
			t.Fatalf("trace ID %q: %v", trace.TraceID, err)
		}
		if traceIDs[trace.TraceID] {
			t.Fatalf("duplicate trace ID %s", trace.TraceID)
		}
		traceIDs[trace.TraceID] = true
		for _, span := range trace.Spans {
			if len(span.SpanID) != 16 {
				t.Fatalf("span ID %q is not 16 hex characters", span.SpanID)
			}
			if _, err := hex.DecodeString(span.SpanID); err != nil {
				t.Fatalf("span ID %q: %v", span.SpanID, err)
			}
			if span.Service == "" || span.Name == "" {
				t.Fatalf("incomplete span %#v", span)
			}
			if spanIDs[span.SpanID] {
				t.Fatalf("duplicate span ID %s", span.SpanID)
			}
			spanIDs[span.SpanID] = true
		}
	}
	for _, query := range c.Queries {
		for _, id := range query.ExpectTraceIDs {
			if !traceIDs[id] {
				t.Fatalf("query %q expects unknown trace %s", query.Name, id)
			}
		}
		for id, ids := range query.ExpectMatchedSpans {
			if !traceIDs[id] {
				t.Fatalf("query %q expects spans for unknown trace %s", query.Name, id)
			}
			for _, spanID := range ids {
				if !spanIDs[spanID] {
					t.Fatalf("query %q expects unknown span %s", query.Name, spanID)
				}
			}
		}
	}
	for _, test := range c.TraceTests {
		if !traceIDs[test.TraceID] {
			t.Fatalf("trace test expects unknown trace %s", test.TraceID)
		}
	}
}

func TestCorpusRendersOTLPAndClickHouse(t *testing.T) {
	c, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	base := c.BaseTime()
	data, err := c.OTLPJSON(base)
	if err != nil {
		t.Fatalf("OTLPJSON: %v", err)
	}
	var otlp struct {
		ResourceSpans []struct {
			ScopeSpans []struct {
				Spans []struct {
					TraceID string `json:"traceId"`
					SpanID  string `json:"spanId"`
					Name    string `json:"name"`
				} `json:"spans"`
			} `json:"scopeSpans"`
		} `json:"resourceSpans"`
	}
	if err := json.Unmarshal(data, &otlp); err != nil {
		t.Fatalf("decode OTLP JSON: %v", err)
	}
	spans := 0
	for _, resource := range otlp.ResourceSpans {
		for _, scope := range resource.ScopeSpans {
			spans += len(scope.Spans)
			for _, span := range scope.Spans {
				if len(span.TraceID) != 32 || len(span.SpanID) != 16 || span.Name == "" {
					t.Fatalf("invalid OTLP span %#v", span)
				}
			}
		}
	}
	if spans == 0 {
		t.Fatal("OTLP JSON has no spans")
	}
	rows, err := c.ClickHouseRows(base)
	if err != nil {
		t.Fatalf("ClickHouseRows: %v", err)
	}
	if len(rows) != spans {
		t.Fatalf("ClickHouse rows = %d, OTLP spans = %d", len(rows), spans)
	}
	for _, row := range rows {
		var value map[string]any
		if err := json.Unmarshal([]byte(row), &value); err != nil {
			t.Fatalf("decode ClickHouse row: %v", err)
		}
		if value["TraceId"] == "" || value["SpanId"] == "" {
			t.Fatalf("incomplete ClickHouse row %s", row)
		}
	}
}
