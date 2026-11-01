package tempo

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	tracev1 "go.opentelemetry.io/proto/otlp/trace/v1"
	"google.golang.org/protobuf/proto"
)

type fakeFetcher struct {
	spans []Span
}

func (f fakeFetcher) GetTrace(_ context.Context, traceID string, _ *time.Time, _ *time.Time) ([]Span, error) {
	out := make([]Span, 0, len(f.spans))
	for _, span := range f.spans {
		if span.TraceID == traceID {
			out = append(out, span)
		}
	}
	return out, nil
}

func (f fakeFetcher) FetchRange(context.Context, time.Time, time.Time) ([]Span, error) {
	return f.spans, nil
}

func testAPI(t *testing.T) (http.Handler, Span) {
	t.Helper()
	span := testSpan()
	fetcher := fakeFetcher{spans: []Span{span}}
	api := NewAPI(fetcher, "test-version")
	api.Now = func() time.Time { return time.Date(2026, 11, 1, 0, 10, 0, 0, time.UTC) }
	mux := http.NewServeMux()
	api.Register(mux)
	return mux, span
}

func request(t *testing.T, h http.Handler, path string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
	return rec
}

func TestEchoAndBuildInfo(t *testing.T) {
	h, _ := testAPI(t)
	rec := request(t, h, "/api/echo")
	if rec.Code != http.StatusOK || rec.Body.String() != "echo" {
		t.Fatalf("echo = %d %q", rec.Code, rec.Body.String())
	}
	rec = request(t, h, "/api/status/buildinfo")
	if rec.Code != http.StatusOK {
		t.Fatalf("buildinfo = %d", rec.Code)
	}
	var body map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	if body["version"] != "test-version" {
		t.Fatalf("buildinfo = %#v", body)
	}
}

func TestSearch(t *testing.T) {
	h, span := testAPI(t)
	rec := request(t, h, "/api/search?q="+url.QueryEscape(`{ service.name = "checkout" }`)+"&start=1793490000&end=1793491200")
	if rec.Code != http.StatusOK {
		t.Fatalf("search = %d %s", rec.Code, rec.Body)
	}
	var body map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	traces := body["traces"].([]any)
	if len(traces) != 1 {
		t.Fatalf("traces = %#v", traces)
	}
	trace := traces[0].(map[string]any)
	if trace["traceID"] != span.TraceID || trace["rootServiceName"] != "checkout" {
		t.Fatalf("trace = %#v", trace)
	}
	if trace["durationMs"].(float64) != 2000 {
		t.Fatalf("durationMs = %#v", trace["durationMs"])
	}
}

func TestTraceOTLPJSON(t *testing.T) {
	h, span := testAPI(t)
	rec := request(t, h, "/api/traces/"+span.TraceID+"?start=1793490000&end=1793491200")
	if rec.Code != http.StatusOK {
		t.Fatalf("trace = %d %s", rec.Code, rec.Body)
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	resourceSpans := body["resourceSpans"].([]any)
	if len(resourceSpans) != 1 {
		t.Fatalf("resourceSpans = %#v", resourceSpans)
	}
	scopeSpans := resourceSpans[0].(map[string]any)["scopeSpans"].([]any)
	spans := scopeSpans[0].(map[string]any)["spans"].([]any)
	spanJSON := spans[0].(map[string]any)
	if spanJSON["traceId"] != span.TraceID {
		t.Fatalf("traceId = %#v", spanJSON["traceId"])
	}
	if spanJSON["status"].(map[string]any)["code"].(float64) != 2 {
		t.Fatalf("status = %#v", spanJSON["status"])
	}
}

func TestTraceOTLPProtobuf(t *testing.T) {
	h, span := testAPI(t)
	req := httptest.NewRequest(http.MethodGet, "/api/traces/"+span.TraceID, nil)
	req.Header.Set("Accept", "application/protobuf")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("trace = %d %s", rec.Code, rec.Body)
	}
	var data tracev1.TracesData
	if err := proto.Unmarshal(rec.Body.Bytes(), &data); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(data.ResourceSpans) != 1 || len(data.ResourceSpans[0].ScopeSpans) != 1 {
		t.Fatalf("data = %#v", &data)
	}
	if got := data.ResourceSpans[0].ScopeSpans[0].Spans[0].Name; got != "GET /checkout" {
		t.Fatalf("span name = %q", got)
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func TestTraceV2Envelope(t *testing.T) {
	h, span := testAPI(t)
	req := httptest.NewRequest(http.MethodGet, "/api/v2/traces/"+span.TraceID, nil)
	req.Header.Set("Accept", "application/protobuf")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("v2 trace = %d %s", rec.Code, rec.Body)
	}
	body := rec.Body.Bytes()
	if len(body) < 3 || body[0] != 0x0a {
		t.Fatalf("v2 envelope missing field 1: %x", body[:min(8, len(body))])
	}
	length, n := binary.Uvarint(body[1:])
	if n <= 0 || int(length) > len(body)-1-n {
		t.Fatalf("v2 envelope length invalid: %d %d", length, n)
	}
	var data tracev1.TracesData
	if err := proto.Unmarshal(body[1+n:1+n+int(length)], &data); err != nil {
		t.Fatalf("unmarshal v2 trace: %v", err)
	}
	if len(data.ResourceSpans) != 1 {
		t.Fatalf("v2 trace = %#v", &data)
	}

	rec = request(t, h, "/api/v2/traces/"+span.TraceID)
	var jsonBody map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &jsonBody); err != nil {
		t.Fatalf("decode v2 json: %v", err)
	}
	if _, ok := jsonBody["trace"].(map[string]any); !ok {
		t.Fatalf("v2 json missing trace envelope: %#v", jsonBody)
	}
}

func TestTagsAndTagValues(t *testing.T) {
	h, _ := testAPI(t)
	rec := request(t, h, "/api/search/tags?start=1793490000&end=1793491200")
	var body map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	names := body["tagNames"].([]any)
	if len(names) == 0 {
		t.Fatal("no tags")
	}

	rec = request(t, h, "/api/v2/search/tags?start=1793490000&end=1793491200")
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	if _, ok := body["scopes"].([]any); !ok {
		t.Fatalf("v2 tags = %#v", body)
	}

	rec = request(t, h, "/api/search/tag/service.name/values?start=1793490000&end=1793491200")
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	values := body["tagValues"].([]any)
	if len(values) != 1 || values[0] != "checkout" {
		t.Fatalf("tag values = %#v", values)
	}

	rec = request(t, h, "/api/v2/search/tag/service.name/values?start=1793490000&end=1793491200")
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	v2 := body["tagValues"].([]any)[0].(map[string]any)
	if v2["type"] != "string" || v2["value"] != "checkout" {
		t.Fatalf("v2 tag values = %#v", v2)
	}
}

func TestNotFoundAndBadRequests(t *testing.T) {
	h, span := testAPI(t)
	rec := request(t, h, "/api/traces/00000000000000000000000000000000")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("missing trace = %d %s", rec.Code, rec.Body)
	}
	rec = request(t, h, "/api/traces/nothex")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("invalid trace = %d", rec.Code)
	}
	rec = request(t, h, "/api/search?q="+url.QueryEscape(`{ } | rate()`))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("invalid q = %d %s", rec.Code, rec.Body)
	}
	_ = span
}
