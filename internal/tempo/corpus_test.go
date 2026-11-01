package tempo

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	tracev1 "go.opentelemetry.io/proto/otlp/trace/v1"
	"google.golang.org/protobuf/proto"

	corpus "github.com/mrofisr/skopos/test/corpus/traceql"
)

func loadCorpusSpans(t *testing.T) (*corpus.Corpus, []Span) {
	t.Helper()
	c, err := corpus.Load()
	if err != nil {
		t.Fatalf("load corpus: %v", err)
	}
	base := c.BaseTime()
	spans := make([]Span, 0)
	for _, trace := range c.Traces {
		for _, source := range trace.Spans {
			span := Span{
				Timestamp:          corpus.SpanStart(base, source),
				TraceID:            trace.TraceID,
				SpanID:             source.SpanID,
				ParentSpanID:       source.ParentSpanID,
				SpanName:           source.Name,
				SpanKind:           source.Kind,
				ServiceName:        source.Service,
				ResourceAttributes: corpus.StringAttributes(source.ResourceAttributes),
				ScopeName:          source.ScopeName,
				ScopeVersion:       source.ScopeVersion,
				SpanAttributes:     corpus.StringAttributes(source.Attributes),
				Duration:           uint64(source.DurationNanos),
				StatusCode:         source.Status,
				StatusMessage:      source.StatusMessage,
			}
			if span.ScopeName == "" {
				span.ScopeName = "corpus"
			}
			if span.ScopeVersion == "" {
				span.ScopeVersion = "1.0"
			}
			for _, event := range source.Events {
				span.Events = append(span.Events, Event{
					Timestamp:  span.Timestamp.Add(time.Duration(event.OffsetNanos)),
					Name:       event.Name,
					Attributes: corpus.StringAttributes(event.Attributes),
				})
			}
			for _, link := range source.Links {
				span.Links = append(span.Links, Link{
					TraceID:    link.TraceID,
					SpanID:     link.SpanID,
					Attributes: corpus.StringAttributes(link.Attributes),
				})
			}
			spans = append(spans, span)
		}
	}
	return c, spans
}

func corpusHandler(t *testing.T, spans []Span) (http.Handler, *corpus.Corpus) {
	t.Helper()
	c, _ := loadCorpusSpans(t)
	api := NewAPI(fakeFetcher{spans: spans}, "corpus")
	api.Now = func() time.Time { return c.BaseTime().Add(10 * time.Minute) }
	mux := http.NewServeMux()
	api.Register(mux)
	return mux, c
}

func getJSON(t *testing.T, h http.Handler, path string, out any) *httptest.ResponseRecorder {
	t.Helper()
	rec := request(t, h, path)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET %s = %d %s", path, rec.Code, rec.Body)
	}
	if err := json.Unmarshal(rec.Body.Bytes(), out); err != nil {
		t.Fatalf("decode %s: %v (%s)", path, err, rec.Body)
	}
	return rec
}

func sortedSet(values []string) []string {
	out := append([]string(nil), values...)
	sort.Strings(out)
	return out
}

func equalStrings(a, b []string) bool {
	a, b = sortedSet(a), sortedSet(b)
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

type corpusSearchResponse struct {
	Traces []struct {
		TraceID  string `json:"traceID"`
		SpanSets []struct {
			Spans []struct {
				SpanID string `json:"spanID"`
			} `json:"spans"`
			Matched int `json:"matched"`
		} `json:"spanSets"`
	} `json:"traces"`
}

func TestCorpusSearchAPI(t *testing.T) {
	_, spans := loadCorpusSpans(t)
	h, c := corpusHandler(t, spans)
	base := c.BaseTime()
	start := base.Add(-time.Minute).Unix()
	end := base.Add(time.Hour).Unix()

	for _, test := range c.Queries {
		t.Run(test.Name, func(t *testing.T) {
			values := url.Values{}
			values.Set("q", test.Query)
			values.Set("start", strconv.FormatInt(start, 10))
			values.Set("end", strconv.FormatInt(end, 10))
			values.Set("limit", "100")
			values.Set("spss", "100")
			rec := request(t, h, "/api/search?"+values.Encode())
			if test.ExpectError {
				if rec.Code != http.StatusBadRequest {
					t.Fatalf("status = %d, want 400: %s", rec.Code, rec.Body)
				}
				var body map[string]string
				if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil || body["error"] == "" {
					t.Fatalf("error body = %s", rec.Body)
				}
				return
			}
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d: %s", rec.Code, rec.Body)
			}
			var body corpusSearchResponse
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
				t.Fatalf("decode: %v", err)
			}
			ids := make([]string, 0, len(body.Traces))
			matched := make(map[string][]string, len(body.Traces))
			for _, trace := range body.Traces {
				ids = append(ids, trace.TraceID)
				if len(trace.SpanSets) == 0 {
					t.Fatalf("trace %s has no spanSets", trace.TraceID)
				}
				if trace.SpanSets[0].Matched != len(trace.SpanSets[0].Spans) {
					t.Fatalf("matched = %d, spans = %d with spss=100", trace.SpanSets[0].Matched, len(trace.SpanSets[0].Spans))
				}
				for _, span := range trace.SpanSets[0].Spans {
					matched[trace.TraceID] = append(matched[trace.TraceID], span.SpanID)
				}
			}
			if !equalStrings(ids, test.ExpectTraceIDs) {
				t.Fatalf("trace IDs = %v, want %v", sortedSet(ids), sortedSet(test.ExpectTraceIDs))
			}
			for id, want := range test.ExpectMatchedSpans {
				if !equalStrings(matched[id], want) {
					t.Fatalf("matched spans for %s = %v, want %v", id, sortedSet(matched[id]), sortedSet(want))
				}
			}
		})
	}
}

func TestCorpusTagNamesAPI(t *testing.T) {
	_, spans := loadCorpusSpans(t)
	h, c := corpusHandler(t, spans)
	base := c.BaseTime()
	query := url.Values{}
	query.Set("start", strconv.FormatInt(base.Add(-time.Minute).Unix(), 10))
	query.Set("end", strconv.FormatInt(base.Add(time.Hour).Unix(), 10))

	var v2 struct {
		Scopes []struct {
			Name string   `json:"name"`
			Tags []string `json:"tags"`
		} `json:"scopes"`
	}
	getJSON(t, h, "/api/v2/search/tags?"+query.Encode(), &v2)
	gotScopes := make(map[string][]string, len(v2.Scopes))
	for _, scope := range v2.Scopes {
		gotScopes[scope.Name] = scope.Tags
	}
	wantOrder := []string{"span", "event", "link", "resource", "intrinsic"}
	if len(v2.Scopes) != len(wantOrder) {
		t.Fatalf("scopes = %v", v2.Scopes)
	}
	for i, name := range wantOrder {
		if v2.Scopes[i].Name != name {
			t.Fatalf("scope order = %v", v2.Scopes)
		}
	}
	union := make([]string, 0)
	for _, test := range c.TagNameTests {
		if !equalStrings(gotScopes[test.Scope], test.Expect) {
			t.Fatalf("scope %s = %v, want %v", test.Scope, gotScopes[test.Scope], test.Expect)
		}
		if test.Scope == "intrinsic" {
			continue
		}
		union = append(union, test.Expect...)
	}
	var v1 struct {
		TagNames []string `json:"tagNames"`
	}
	getJSON(t, h, "/api/search/tags?"+query.Encode(), &v1)
	if !equalStrings(v1.TagNames, union) {
		t.Fatalf("v1 tagNames = %v, want %v", sortedSet(v1.TagNames), sortedSet(union))
	}
	for _, test := range c.TagNameTests {
		values := url.Values{}
		values.Set("scope", test.Scope)
		values.Set("start", query.Get("start"))
		values.Set("end", query.Get("end"))
		var scoped struct {
			TagNames []string `json:"tagNames"`
		}
		getJSON(t, h, "/api/search/tags?"+values.Encode(), &scoped)
		if !equalStrings(scoped.TagNames, test.Expect) {
			t.Fatalf("v1 scope %s = %v, want %v", test.Scope, scoped.TagNames, test.Expect)
		}
	}
}

func TestCorpusTagValuesAPI(t *testing.T) {
	_, spans := loadCorpusSpans(t)
	h, c := corpusHandler(t, spans)
	base := c.BaseTime()
	query := url.Values{}
	query.Set("start", strconv.FormatInt(base.Add(-time.Minute).Unix(), 10))
	query.Set("end", strconv.FormatInt(base.Add(time.Hour).Unix(), 10))

	for _, test := range c.TagValueTests {
		t.Run(test.Name, func(t *testing.T) {
			path := "/api/v2/search/tag/" + url.PathEscape(test.Tag) + "/values?" + query.Encode()
			var body struct {
				TagValues []struct {
					Type  string `json:"type"`
					Value string `json:"value"`
				} `json:"tagValues"`
			}
			getJSON(t, h, path, &body)
			values := make([]string, 0, len(body.TagValues))
			for _, value := range body.TagValues {
				values = append(values, value.Value)
			}
			if !equalStrings(values, test.Expect) {
				t.Fatalf("values = %v, want %v", sortedSet(values), sortedSet(test.Expect))
			}
			var v1 struct {
				TagValues []string `json:"tagValues"`
			}
			getJSON(t, h, "/api/search/tag/"+url.PathEscape(test.Tag)+"/values?"+query.Encode(), &v1)
			if !equalStrings(v1.TagValues, test.Expect) {
				t.Fatalf("v1 values = %v, want %v", sortedSet(v1.TagValues), sortedSet(test.Expect))
			}
		})
	}
}

type flattenedOTLP struct {
	TraceID      string
	SpanID       string
	ParentSpanID string
	Name         string
	Kind         int
	Status       int
	EventNames   []string
	LinkTraceIDs []string
}

func flattenOTLP(t *testing.T, data *tracev1.TracesData) []flattenedOTLP {
	t.Helper()
	out := make([]flattenedOTLP, 0)
	for _, resource := range data.ResourceSpans {
		for _, scope := range resource.ScopeSpans {
			for _, span := range scope.Spans {
				flat := flattenedOTLP{
					TraceID:      hexID(span.TraceId),
					SpanID:       hexID(span.SpanId),
					ParentSpanID: hexID(span.ParentSpanId),
					Name:         span.Name,
					Kind:         int(span.Kind),
					Status:       int(span.Status.GetCode()),
				}
				for _, event := range span.Events {
					flat.EventNames = append(flat.EventNames, event.Name)
				}
				for _, link := range span.Links {
					flat.LinkTraceIDs = append(flat.LinkTraceIDs, hexID(link.TraceId))
				}
				out = append(out, flat)
			}
		}
	}
	return out
}

func hexID(id []byte) string {
	const digits = "0123456789abcdef"
	out := make([]byte, len(id)*2)
	for i, b := range id {
		out[i*2] = digits[b>>4]
		out[i*2+1] = digits[b&0x0f]
	}
	return string(out)
}

func decodeV2Envelope(t *testing.T, body []byte) *tracev1.TracesData {
	t.Helper()
	if len(body) < 3 || body[0] != 0x0a {
		t.Fatalf("v2 envelope field 1 missing: %x", body)
	}
	length, n := binaryUvarint(body[1:])
	if n <= 0 || int(length) > len(body)-1-n {
		t.Fatalf("v2 envelope length invalid: %d %d", length, n)
	}
	var data tracev1.TracesData
	if err := proto.Unmarshal(body[1+n:1+n+int(length)], &data); err != nil {
		t.Fatalf("unmarshal v2 trace: %v", err)
	}
	return &data
}

func binaryUvarint(buf []byte) (uint64, int) {
	var value uint64
	for i, b := range buf {
		if i >= 10 {
			return 0, -1
		}
		value |= uint64(b&0x7f) << (7 * i)
		if b < 0x80 {
			return value, i + 1
		}
	}
	return 0, 0
}

func TestCorpusTraceByIDAPI(t *testing.T) {
	_, spans := loadCorpusSpans(t)
	h, c := corpusHandler(t, spans)

	for _, test := range c.TraceTests {
		t.Run(test.TraceID, func(t *testing.T) {
			var v1 map[string]any
			rec := getJSON(t, h, "/api/traces/"+test.TraceID, &v1)
			if _, wrapped := v1["trace"]; wrapped {
				t.Fatalf("v1 JSON must not carry the v2 trace envelope")
			}
			if rec.Header().Get("Content-Type") != "application/json; charset=utf-8" {
				t.Fatalf("v1 content type = %q", rec.Header().Get("Content-Type"))
			}

			var v2 struct {
				Trace json.RawMessage `json:"trace"`
			}
			rec = getJSON(t, h, "/api/v2/traces/"+test.TraceID, &v2)
			if len(v2.Trace) == 0 {
				t.Fatalf("v2 JSON missing trace envelope")
			}
			var otlp map[string]json.RawMessage
			if err := json.Unmarshal(v2.Trace, &otlp); err != nil {
				t.Fatalf("decode v2 OTLP JSON: %v", err)
			}
			if _, ok := otlp["resourceSpans"]; !ok {
				t.Fatalf("v2 OTLP JSON missing resourceSpans: %s", v2.Trace)
			}

			req := httptest.NewRequest(http.MethodGet, "/api/v2/traces/"+test.TraceID, nil)
			req.Header.Set("Accept", "application/protobuf")
			rec = httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			if rec.Code != http.StatusOK {
				t.Fatalf("v2 protobuf = %d %s", rec.Code, rec.Body)
			}
			if rec.Header().Get("Content-Type") != "application/protobuf" {
				t.Fatalf("v2 protobuf content type = %q", rec.Header().Get("Content-Type"))
			}
			flat := flattenOTLP(t, decodeV2Envelope(t, rec.Body.Bytes()))
			checkTraceTest(t, test, flat)

			req = httptest.NewRequest(http.MethodGet, "/api/traces/"+test.TraceID, nil)
			req.Header.Set("Accept", "application/protobuf")
			rec = httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			if rec.Code != http.StatusOK {
				t.Fatalf("v1 protobuf = %d %s", rec.Code, rec.Body)
			}
			var v1data tracev1.TracesData
			if err := proto.Unmarshal(rec.Body.Bytes(), &v1data); err != nil {
				t.Fatalf("v1 protobuf is not a bare OTLP trace: %v", err)
			}
			checkTraceTest(t, test, flattenOTLP(t, &v1data))
		})
	}
}

func checkTraceTest(t *testing.T, test corpus.TraceTest, spans []flattenedOTLP) {
	t.Helper()
	if len(spans) != test.SpanCount {
		t.Fatalf("span count = %d, want %d", len(spans), test.SpanCount)
	}
	ids := make([]string, 0, len(spans))
	names := make([]string, 0, len(spans))
	events := make([]string, 0)
	links := make([]string, 0)
	for _, span := range spans {
		ids = append(ids, span.SpanID)
		names = append(names, span.Name)
		events = append(events, span.EventNames...)
		links = append(links, span.LinkTraceIDs...)
		if span.SpanID == test.RootSpanID && span.ParentSpanID != "" {
			t.Fatalf("root span %s parent = %q", span.SpanID, span.ParentSpanID)
		}
	}
	if !equalStrings(ids, test.SpanIDs) {
		t.Fatalf("span IDs = %v, want %v", sortedSet(ids), sortedSet(test.SpanIDs))
	}
	if !equalStrings(names, test.Names) {
		t.Fatalf("names = %v, want %v", sortedSet(names), sortedSet(test.Names))
	}
	if !equalStrings(events, test.EventNames) {
		t.Fatalf("events = %v, want %v", sortedSet(events), sortedSet(test.EventNames))
	}
	if !equalStrings(links, test.LinkTraceIDs) {
		t.Fatalf("links = %v, want %v", sortedSet(links), sortedSet(test.LinkTraceIDs))
	}
}

func TestCorpusSearchPagination(t *testing.T) {
	_, spans := loadCorpusSpans(t)
	h, c := corpusHandler(t, spans)
	base := c.BaseTime()
	values := url.Values{}
	values.Set("q", "{ }")
	values.Set("start", strconv.FormatInt(base.Add(-time.Minute).Unix(), 10))
	values.Set("end", strconv.FormatInt(base.Add(time.Hour).Unix(), 10))
	values.Set("limit", "2")
	values.Set("spss", "1")
	var body corpusSearchResponse
	getJSON(t, h, "/api/search?"+values.Encode(), &body)
	if len(body.Traces) != 2 {
		t.Fatalf("limit=2 returned %d traces", len(body.Traces))
	}
	for _, trace := range body.Traces {
		if len(trace.SpanSets) != 1 {
			t.Fatalf("%s spanSets = %d", trace.TraceID, len(trace.SpanSets))
		}
		if len(trace.SpanSets[0].Spans) != 1 {
			t.Fatalf("%s spss=1 returned %d spans", trace.TraceID, len(trace.SpanSets[0].Spans))
		}
		if trace.SpanSets[0].Matched <= 1 {
			t.Fatalf("%s matched = %d, want > 1", trace.TraceID, trace.SpanSets[0].Matched)
		}
	}
}

func TestParseKeyScopes(t *testing.T) {
	tests := []struct {
		key       string
		scope     string
		name      string
		intrinsic bool
		explicit  bool
	}{
		{"span:name", "span", "name", true, false},
		{"span.name", "span", "name", false, false},
		{"trace:duration", "trace", "duration", true, false},
		{"traceDuration", "trace", "duration", true, false},
		{"rootServiceName", "trace", "rootService", true, false},
		{"event:name", "event", "name", true, false},
		{"event.exception.type", "event", "exception.type", false, false},
		{"link:traceID", "link", "traceID", true, false},
		{"link.rel", "link", "rel", false, false},
		{".unscoped", "", "unscoped", false, true},
		{"resource.service.name", "resource", "service.name", false, false},
		{"instrumentation:name", "instrumentation", "name", true, false},
	}
	for _, test := range tests {
		ref, err := parseKey(test.key)
		if err != nil {
			t.Fatalf("parseKey(%q): %v", test.key, err)
		}
		if ref.scope != test.scope || ref.key != test.name || ref.intrinsic != test.intrinsic || ref.explicit != test.explicit {
			t.Errorf("parseKey(%q) = %#v", test.key, ref)
		}
	}
}

func TestParseKeyRejectsUnsupported(t *testing.T) {
	for _, key := range []string{"trace:foo", "span:childCount", "event:duration", "link:foo", "instrumentation:language", "instrumentation.language", "unknown:key"} {
		if _, err := parseKey(key); err == nil {
			t.Errorf("parseKey(%q) succeeded", key)
		}
	}
}

func TestTraceInfoRootSelection(t *testing.T) {
	base := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	spans := []Span{
		{TraceID: "aa", SpanID: "02", ParentSpanID: "01", Timestamp: base.Add(time.Second), Duration: uint64(time.Second)},
		{TraceID: "aa", SpanID: "01", ParentSpanID: "", Timestamp: base, Duration: uint64(2 * time.Second), SpanName: "root", ServiceName: "svc"},
	}
	info := TraceInfoFor("aa", spans)
	if !info.HasRoot || info.RootSpanID != "01" || info.RootName != "root" || info.RootServiceName != "svc" {
		t.Fatalf("info = %#v", info)
	}
	if info.Duration() != 2*time.Second {
		t.Fatalf("duration = %s", info.Duration())
	}
}

func TestServiceStats(t *testing.T) {
	stats := serviceStats([]Span{
		{ServiceName: "checkout", StatusCode: "Error"},
		{ServiceName: "checkout", StatusCode: "Ok"},
		{ServiceName: "cart", StatusCode: "Ok"},
	})
	if stats["checkout"].SpanCount != 2 || stats["checkout"].ErrorCount != 1 {
		t.Fatalf("checkout = %#v", stats["checkout"])
	}
	if stats["cart"].SpanCount != 1 || stats["cart"].ErrorCount != 0 {
		t.Fatalf("cart = %#v", stats["cart"])
	}
}

func TestSpanSetLimit(t *testing.T) {
	spans := []Span{{SpanID: "a"}, {SpanID: "b"}, {SpanID: "c"}}
	set := buildSpanSet(spans, 2)
	if set.Matched != 3 || len(set.Spans) != 2 {
		t.Fatalf("span set = %#v", set)
	}
}

func TestSearchSummaryUsesWholeTrace(t *testing.T) {
	base := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	spans := []Span{
		{TraceID: "aa", SpanID: "01", ParentSpanID: "", SpanName: "root", ServiceName: "checkout", Timestamp: base, Duration: uint64(2 * time.Second)},
		{TraceID: "aa", SpanID: "02", ParentSpanID: "01", SpanName: "child", ServiceName: "payments", Timestamp: base.Add(3 * time.Second), Duration: uint64(time.Second)},
	}
	filter, err := ParseTraceQL(`{ span:name = "child" }`)
	if err != nil {
		t.Fatal(err)
	}
	result := Search(spans, filter, 0, 0, 10)
	if len(result) != 1 {
		t.Fatalf("results = %#v", result)
	}
	if result[0].RootTraceName != "root" || result[0].RootServiceName != "checkout" {
		t.Fatalf("summary = %#v", result[0])
	}
	if result[0].Duration != 4*time.Second {
		t.Fatalf("duration = %s", result[0].Duration)
	}
	if len(result[0].MatchedSpans) != 1 || result[0].MatchedSpans[0].SpanID != "02" {
		t.Fatalf("matched = %#v", result[0].MatchedSpans)
	}
}

func TestTagNamesIncludeEventAndLink(t *testing.T) {
	span := testSpan()
	span.Events = []Event{{Name: "exception", Attributes: map[string]string{"exception.type": "BadGateway"}}}
	span.Links = []Link{{TraceID: "abc", SpanID: "def", Attributes: map[string]string{"rel": "retry"}}}
	names := TagNames([]Span{span})
	if _, ok := names["event"]["exception.type"]; !ok {
		t.Fatalf("event tags = %v", names["event"])
	}
	if _, ok := names["link"]["rel"]; !ok {
		t.Fatalf("link tags = %v", names["link"])
	}
}

func TestTagValuesEventAndLink(t *testing.T) {
	span := testSpan()
	span.Events = []Event{
		{Name: "exception", Attributes: map[string]string{"exception.type": "BadGateway"}},
		{Name: "log", Attributes: map[string]string{"level": "warn"}},
	}
	span.Links = []Link{{TraceID: "AABB", SpanID: "CCDD", Attributes: map[string]string{"rel": "retry"}}}
	info := TraceInfoFor(span.TraceID, []Span{span})
	if got := TagValues(span, "event:name", info); !equalStrings(got, []string{"exception", "log"}) {
		t.Fatalf("event names = %v", got)
	}
	if got := TagValues(span, "event.exception.type", info); !equalStrings(got, []string{"BadGateway"}) {
		t.Fatalf("event attr = %v", got)
	}
	if got := TagValues(span, "link:traceID", info); !equalStrings(got, []string{"aabb"}) {
		t.Fatalf("link trace id = %v", got)
	}
	if got := TagValues(span, "link.rel", info); !equalStrings(got, []string{"retry"}) {
		t.Fatalf("link attr = %v", got)
	}
}

func TestTraceQLErrorsExplicit(t *testing.T) {
	for _, query := range []string{
		`{ } | rate()`,
		`{ span:name = "x" } >> { span:name = "y" }`,
		`{ span:name = "x" } && { span:name = "y" }`,
		`{ trace:foo = "bar" }`,
		`{ span:count() > 1 }`,
		`{ instrumentation.language = "go" }`,
		`{ span.foo =~ nil }`,
	} {
		filter, err := ParseTraceQL(query)
		if err == nil {
			t.Errorf("ParseTraceQL(%q) = %#v, want error", query, filter)
			continue
		}
		if !strings.Contains(err.Error(), "tempo:") {
			t.Errorf("ParseTraceQL(%q) error = %v", query, err)
		}
	}
}
