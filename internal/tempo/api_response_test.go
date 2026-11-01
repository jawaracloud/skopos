package tempo

import (
	"encoding/binary"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	tracev1 "go.opentelemetry.io/proto/otlp/trace/v1"
	"google.golang.org/protobuf/proto"
)

func TestV1ProtobufIsBareOTLP(t *testing.T) {
	h, span := testAPI(t)
	req := httptest.NewRequest(http.MethodGet, "/api/traces/"+span.TraceID, nil)
	req.Header.Set("Accept", "application/protobuf")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("v1 protobuf = %d %s", rec.Code, rec.Body)
	}
	if got := rec.Header().Get("Content-Type"); got != "application/protobuf" {
		t.Fatalf("content type = %q", got)
	}
	var data tracev1.TracesData
	if err := proto.Unmarshal(rec.Body.Bytes(), &data); err != nil {
		t.Fatalf("v1 protobuf must be a bare OTLP TracesData: %v", err)
	}
	spans := otlpSpans(&data)
	if len(spans) != 1 || spans[0].Name != span.SpanName {
		t.Fatalf("v1 spans = %#v", spans)
	}
}

func TestV2ProtobufEnvelopeContents(t *testing.T) {
	h, span := testAPI(t)
	req := httptest.NewRequest(http.MethodGet, "/api/v2/traces/"+span.TraceID, nil)
	req.Header.Set("Accept", "application/protobuf")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("v2 protobuf = %d %s", rec.Code, rec.Body)
	}
	if got := rec.Header().Get("Content-Type"); got != "application/protobuf" {
		t.Fatalf("content type = %q", got)
	}
	body := rec.Body.Bytes()
	if len(body) < 3 || body[0] != 0x0a {
		t.Fatalf("v2 envelope missing field 1: %x", body)
	}
	length, n := binary.Uvarint(body[1:])
	if n <= 0 || int(length) != len(body)-1-n {
		t.Fatalf("v2 envelope length = %d/%d with %d payload bytes", length, n, len(body)-1-n)
	}
	var data tracev1.TracesData
	if err := proto.Unmarshal(body[1+n:], &data); err != nil {
		t.Fatalf("v2 envelope payload: %v", err)
	}
	spans := otlpSpans(&data)
	if len(spans) != 1 {
		t.Fatalf("v2 spans = %#v", spans)
	}
	got := spans[0]
	if got.TraceId == nil || got.SpanId == nil || got.Name != span.SpanName {
		t.Fatalf("v2 span = %#v", got)
	}
	if got.Status.GetCode() != tracev1.Status_STATUS_CODE_ERROR {
		t.Fatalf("v2 status = %#v", got.Status)
	}
	if len(got.Events) != 0 {
		t.Fatalf("unexpected events = %#v", got.Events)
	}
}

func otlpSpans(data *tracev1.TracesData) []*tracev1.Span {
	out := make([]*tracev1.Span, 0)
	for _, resource := range data.ResourceSpans {
		for _, scope := range resource.ScopeSpans {
			out = append(out, scope.Spans...)
		}
	}
	return out
}

func TestV1AndV2JSONEnvelopes(t *testing.T) {
	h, span := testAPI(t)
	v1 := request(t, h, "/api/traces/"+span.TraceID)
	if v1.Code != http.StatusOK {
		t.Fatalf("v1 json = %d", v1.Code)
	}
	var v1Body map[string]json.RawMessage
	if err := json.Unmarshal(v1.Body.Bytes(), &v1Body); err != nil {
		t.Fatalf("decode v1: %v", err)
	}
	if _, ok := v1Body["resourceSpans"]; !ok {
		t.Fatalf("v1 JSON missing resourceSpans")
	}
	if _, ok := v1Body["trace"]; ok {
		t.Fatalf("v1 JSON must not be wrapped in a trace envelope")
	}

	v2 := request(t, h, "/api/v2/traces/"+span.TraceID)
	if v2.Code != http.StatusOK {
		t.Fatalf("v2 json = %d", v2.Code)
	}
	var v2Body map[string]json.RawMessage
	if err := json.Unmarshal(v2.Body.Bytes(), &v2Body); err != nil {
		t.Fatalf("decode v2: %v", err)
	}
	if _, ok := v2Body["trace"]; !ok {
		t.Fatalf("v2 JSON missing trace envelope")
	}
}

func TestTraceNotFoundV2(t *testing.T) {
	h, _ := testAPI(t)
	rec := request(t, h, "/api/v2/traces/00000000000000000000000000000000")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("v2 missing trace = %d %s", rec.Code, rec.Body)
	}
	req := httptest.NewRequest(http.MethodGet, "/api/v2/traces/00000000000000000000000000000000", nil)
	req.Header.Set("Accept", "application/protobuf")
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound || !strings.Contains(rec.Body.String(), "trace not found") {
		t.Fatalf("v2 missing protobuf trace = %d %s", rec.Code, rec.Body)
	}
}

func TestAcceptHeaderWithQualityValues(t *testing.T) {
	h, span := testAPI(t)
	req := httptest.NewRequest(http.MethodGet, "/api/v2/traces/"+span.TraceID, nil)
	req.Header.Set("Accept", "application/json;q=0.5, application/protobuf;q=1.0")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("trace = %d", rec.Code)
	}
	if got := rec.Header().Get("Content-Type"); got != "application/protobuf" {
		t.Fatalf("content type = %q", got)
	}
}

func TestSearchSpanSetsAndServiceStats(t *testing.T) {
	base := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	root := testSpan()
	root.Timestamp = base
	root.Duration = uint64(2 * time.Second)
	root.SpanID = "root"
	root.ParentSpanID = ""
	child := testSpan()
	child.Timestamp = base.Add(time.Second)
	child.Duration = uint64(500 * time.Millisecond)
	child.SpanID = "child"
	child.ParentSpanID = "root"
	child.SpanName = "child"
	child.ServiceName = "payments"
	child.StatusCode = "Ok"
	fetcher := fakeFetcher{spans: []Span{root, child}}
	api := NewAPI(fetcher, "test")
	api.Now = func() time.Time { return base.Add(10 * time.Minute) }
	mux := http.NewServeMux()
	api.Register(mux)

	values := url.Values{}
	values.Set("q", `{ span:name = "child" }`)
	values.Set("start", "1793488800")
	values.Set("end", "1793494800")
	values.Set("spss", "1")
	rec := request(t, mux, "/api/search?"+values.Encode())
	if rec.Code != http.StatusOK {
		t.Fatalf("search = %d %s", rec.Code, rec.Body)
	}
	var body struct {
		Traces []struct {
			RootServiceName string `json:"rootServiceName"`
			RootTraceName   string `json:"rootTraceName"`
			DurationMs      int64  `json:"durationMs"`
			SpanSet         struct {
				Spans []struct {
					SpanID string `json:"spanID"`
				} `json:"spans"`
				Matched int `json:"matched"`
			} `json:"spanSet"`
			SpanSets []struct {
				Spans []struct {
					SpanID string `json:"spanID"`
				} `json:"spans"`
				Matched int `json:"matched"`
			} `json:"spanSets"`
			ServiceStats map[string]struct {
				SpanCount  int `json:"spanCount"`
				ErrorCount int `json:"errorCount"`
			} `json:"serviceStats"`
		} `json:"traces"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(body.Traces) != 1 {
		t.Fatalf("traces = %#v", body.Traces)
	}
	trace := body.Traces[0]
	if trace.RootServiceName != "checkout" || trace.RootTraceName != "GET /checkout" {
		t.Fatalf("root summary = %#v", trace)
	}
	if trace.DurationMs != 2000 {
		t.Fatalf("durationMs = %d", trace.DurationMs)
	}
	if len(trace.SpanSets) != 1 || trace.SpanSets[0].Matched != 1 || len(trace.SpanSets[0].Spans) != 1 {
		t.Fatalf("spanSets = %#v", trace.SpanSets)
	}
	if trace.SpanSet.Matched != 1 || len(trace.SpanSet.Spans) != 1 {
		t.Fatalf("spanSet = %#v", trace.SpanSet)
	}
	if trace.SpanSets[0].Spans[0].SpanID != "child" {
		t.Fatalf("matched span = %#v", trace.SpanSets[0].Spans)
	}
	if trace.ServiceStats["checkout"].SpanCount != 1 || trace.ServiceStats["checkout"].ErrorCount != 1 {
		t.Fatalf("checkout stats = %#v", trace.ServiceStats["checkout"])
	}
	if trace.ServiceStats["payments"].SpanCount != 1 {
		t.Fatalf("payments stats = %#v", trace.ServiceStats["payments"])
	}
}

func TestTagsV2ScopesAndEventLink(t *testing.T) {
	span := testSpan()
	span.Events = []Event{{Name: "exception", Attributes: map[string]string{"exception.type": "BadGateway"}}}
	span.Links = []Link{{TraceID: "aabb", SpanID: "ccdd", Attributes: map[string]string{"rel": "retry"}}}
	api := NewAPI(fakeFetcher{spans: []Span{span}}, "test")
	api.Now = func() time.Time { return time.Date(2026, 11, 1, 0, 10, 0, 0, time.UTC) }
	mux := http.NewServeMux()
	api.Register(mux)
	rec := request(t, mux, "/api/v2/search/tags?start=1793490000&end=1793491200")
	var body struct {
		Scopes []struct {
			Name string   `json:"name"`
			Tags []string `json:"tags"`
		} `json:"scopes"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	got := make(map[string]bool)
	for _, scope := range body.Scopes {
		for _, tag := range scope.Tags {
			got[scope.Name+"."+tag] = true
		}
	}
	for _, want := range []string{"event.exception.type", "link.rel", "span.http.method", "resource.service.name", "intrinsic.trace:duration"} {
		if !got[want] {
			t.Errorf("missing %s in %v", want, got)
		}
	}

	values := request(t, mux, "/api/v2/search/tag/event.exception.type/values?start=1793490000&end=1793491200")
	var valuesBody struct {
		TagValues []struct {
			Type  string `json:"type"`
			Value string `json:"value"`
		} `json:"tagValues"`
	}
	if err := json.Unmarshal(values.Body.Bytes(), &valuesBody); err != nil {
		t.Fatalf("decode values: %v", err)
	}
	if len(valuesBody.TagValues) != 1 || valuesBody.TagValues[0].Value != "BadGateway" {
		t.Fatalf("event values = %#v", valuesBody.TagValues)
	}
}
