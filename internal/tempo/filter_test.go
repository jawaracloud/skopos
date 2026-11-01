package tempo

import (
	"testing"
	"time"
)

func testSpan() Span {
	return Span{
		Timestamp:          time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC),
		TraceID:            "4bf92f3577b34da6a3ce929d0e0e4736",
		SpanID:             "00f067aa0ba902b7",
		SpanName:           "GET /checkout",
		ServiceName:        "checkout",
		SpanKind:           "Server",
		Duration:           uint64(2 * time.Second),
		StatusCode:         "Error",
		ResourceAttributes: map[string]string{"service.name": "checkout", "deployment.environment.name": "prod"},
		SpanAttributes:     map[string]string{"http.method": "GET", "http.status_code": "500"},
	}
}

func TestParseTraceQLAnd(t *testing.T) {
	filter, err := ParseTraceQL(`{ resource.service.name = "checkout" && span.http.method = "GET" }`)
	if err != nil {
		t.Fatalf("ParseTraceQL: %v", err)
	}
	if !filter.Match(testSpan()) {
		t.Fatal("filter did not match")
	}
	if filter.Match(Span{ServiceName: "cart"}) {
		t.Fatal("filter matched wrong service")
	}
}

func TestTraceQLStatusDurationAndRegex(t *testing.T) {
	tests := []struct {
		query string
		match bool
	}{
		{`{ status = error }`, true},
		{`{ status = ok }`, false},
		{`{ duration > 1s }`, true},
		{`{ duration > 3s }`, false},
		{`{ span.http.status_code =~ "5.." }`, true},
		{`{ span.http.status_code !~ "4.." }`, true},
		{`{ service.name = "checkout" }`, true},
		{`{ span:name = "GET /checkout" }`, true},
		{`{ span:duration > 1s }`, true},
		{`{ resource.environment = ~"prod" }`, false},
		{`{ span.missing != "x" }`, false},
	}
	for _, tc := range tests {
		filter, err := ParseTraceQL(tc.query)
		if err != nil {
			t.Fatalf("%s: %v", tc.query, err)
		}
		if got := filter.Match(testSpan()); got != tc.match {
			t.Errorf("%s match = %v, want %v (%#v)", tc.query, got, tc.match, filter)
		}
	}
}

func TestParseTags(t *testing.T) {
	filter, err := ParseTags(`service.name=checkout http.method=GET`)
	if err != nil {
		t.Fatalf("ParseTags: %v", err)
	}
	if !filter.Match(testSpan()) {
		t.Fatal("tags filter did not match")
	}
	if filter.Match(Span{ServiceName: "cart"}) {
		t.Fatal("tags filter matched wrong service")
	}
}

func TestTraceQLRegexIsAnchored(t *testing.T) {
	filter, err := ParseTraceQL(`{ resource.service.name =~ "check" }`)
	if err != nil {
		t.Fatalf("ParseTraceQL: %v", err)
	}
	if filter.Match(testSpan()) {
		t.Fatal("unanchored regex matched checkout")
	}
}

func TestTraceQLRejectsUnsupported(t *testing.T) {
	for _, query := range []string{
		`{ } | rate()`,
		`{ span.foo = "bar" } && { span.baz = "qux" }`,
		`{ span.foo }`,
	} {
		if _, err := ParseTraceQL(query); err == nil {
			t.Errorf("ParseTraceQL(%q) succeeded, want error", query)
		}
	}
}

func TestSearchGroupsTraces(t *testing.T) {
	spanA := testSpan()
	spanB := testSpan()
	spanB.TraceID = "5bf92f3577b34da6a3ce929d0e0e4736"
	spanB.ParentSpanID = spanA.SpanID
	spanB.Timestamp = spanA.Timestamp.Add(time.Second)
	spanB.Duration = uint64(time.Second)
	filter, _ := ParseTraceQL(`{ service.name = "checkout" }`)
	result := Search([]Span{spanA, spanB}, filter, 0, 0, 10)
	if len(result) != 2 {
		t.Fatalf("search results = %#v", result)
	}
	for _, trace := range result {
		if trace.RootServiceName != "checkout" || trace.Duration <= 0 {
			t.Fatalf("trace = %#v", trace)
		}
	}
	if result[1].TraceID != spanA.TraceID {
		t.Fatalf("results not sorted by start: %#v", result)
	}
}

func TestTraceQLMissingAttributeSemantics(t *testing.T) {
	span := testSpan()
	tests := []struct {
		query string
		match bool
	}{
		{`{ span.missing = "x" }`, false},
		{`{ span.missing != "x" }`, false},
		{`{ span.missing =~ ".*" }`, false},
		{`{ span.missing !~ ".*" }`, false},
		{`{ span.missing = nil }`, true},
		{`{ span.missing != nil }`, false},
		{`{ span.http.method = nil }`, false},
		{`{ span.http.method != nil }`, true},
		{`{ resource.missing = nil }`, true},
	}
	for _, tc := range tests {
		filter, err := ParseTraceQL(tc.query)
		if err != nil {
			t.Fatalf("%s: %v", tc.query, err)
		}
		if got := filter.Match(span); got != tc.match {
			t.Errorf("%s match = %v, want %v", tc.query, got, tc.match)
		}
	}
}

func TestTraceQLScopesAndAliases(t *testing.T) {
	base := testSpan().Timestamp
	root := testSpan()
	root.SpanID = "root"
	root.ParentSpanID = ""
	root.SpanName = "root span"
	root.ServiceName = "checkout"
	root.Timestamp = base
	root.Duration = uint64(3 * time.Second)
	child := testSpan()
	child.SpanID = "child"
	child.ParentSpanID = "root"
	child.SpanName = "child span"
	child.ServiceName = "payments"
	child.Timestamp = base.Add(time.Second)
	child.Duration = uint64(time.Second)
	child.SpanAttributes = map[string]string{"db.system": "postgresql", "http.method": "GET"}
	child.Events = []Event{{Timestamp: base.Add(1500 * time.Millisecond), Name: "exception", Attributes: map[string]string{"exception.type": "BadGateway"}}}
	child.Links = []Link{{TraceID: "aabb", SpanID: "ccdd", Attributes: map[string]string{"rel": "retry"}}}
	spans := []Span{root, child}
	info := TraceInfoFor(root.TraceID, spans)

	tests := []struct {
		query string
		span  Span
		want  bool
	}{
		{`{ span:name = "root span" }`, root, true},
		{`{ span.name = "root span" }`, root, true}, // dot reads the attribute, then falls back to the intrinsic
		{`{ span.name = "attribute" }`, Span{SpanAttributes: map[string]string{"name": "attribute"}}, true},
		{`{ name = "child span" }`, child, true},
		{`{ trace:duration >= 3s }`, child, true},
		{`{ traceDuration >= 3s }`, child, true},
		{`{ trace:rootName = "root span" }`, child, true},
		{`{ rootName = "root span" }`, child, true},
		{`{ trace:rootService = "checkout" }`, child, true},
		{`{ rootServiceName = "checkout" }`, child, true},
		{`{ trace:id = "4bf92f3577b34da6a3ce929d0e0e4736" }`, child, true},
		{`{ span:id = "child" }`, child, true},
		{`{ event:name = "exception" }`, child, true},
		{`{ event.exception.type = "BadGateway" }`, child, true},
		{`{ event.exception.type != "Other" }`, child, true},
		{`{ event.missing != "x" }`, child, false},
		{`{ event:timeSinceStart = 500ms }`, child, true},
		{`{ link:traceID = "AABB" }`, child, true},
		{`{ link:spanID = "ccdd" }`, child, true},
		{`{ link.rel = "retry" }`, child, true},
		{`{ link.missing !~ ".*" }`, child, false},
		{`{ instrumentation:name = "test" }`, Span{ScopeName: "test"}, true},
		{`{ instrumentation:version = "1.0" }`, Span{ScopeVersion: "1.0"}, true},
		{`{ span:status = ERROR }`, root, true},
		{`{ span:kind = SERVER }`, root, true},
		{`{ .http.method = "GET" }`, child, true},
	}
	for _, tc := range tests {
		filter, err := ParseTraceQL(tc.query)
		if err != nil {
			t.Fatalf("%s: %v", tc.query, err)
		}
		if got := matchTrace(filter, tc.span, info); got != tc.want {
			t.Errorf("%s match = %v, want %v", tc.query, got, tc.want)
		}
	}
}

func TestTraceQLRootIntrinsicsAbsentWithoutRoot(t *testing.T) {
	span := testSpan()
	span.ParentSpanID = "not-root"
	info := TraceInfoFor(span.TraceID, []Span{span})
	for _, query := range []string{`{ trace:rootName = "GET /checkout" }`, `{ rootServiceName = "checkout" }`} {
		filter, err := ParseTraceQL(query)
		if err != nil {
			t.Fatalf("%s: %v", query, err)
		}
		if matchTrace(filter, span, info) {
			t.Errorf("%s matched a trace without a root", query)
		}
	}
	if info.HasRoot {
		t.Fatalf("HasRoot = true for an orphan-only trace: %#v", info)
	}
}

func TestTraceQLStatusAndKindNormalization(t *testing.T) {
	span := testSpan()
	span.StatusCode = "STATUS_CODE_ERROR"
	span.SpanKind = "SPAN_KIND_SERVER"
	for _, query := range []string{`{ span:status = error }`, `{ status = ERROR }`, `{ span:kind = server }`, `{ kind = Server }`} {
		filter, err := ParseTraceQL(query)
		if err != nil {
			t.Fatalf("%s: %v", query, err)
		}
		if !filter.Match(span) {
			t.Errorf("%s did not match", query)
		}
	}
}

func TestTraceQLNumericAndLexicographicComparisons(t *testing.T) {
	span := testSpan()
	span.SpanAttributes = map[string]string{"http.status_code": "500", "version": "v2"}
	tests := []struct {
		query string
		want  bool
	}{
		{`{ span.http.status_code = 500 }`, true},
		{`{ span.http.status_code = "500" }`, true},
		{`{ span.http.status_code > 499 }`, true},
		{`{ span.http.status_code < 501 }`, true},
		{`{ span.version > "v1" }`, true},
		{`{ span.version < "v3" }`, true},
		{`{ span.http.status_code = 400 }`, false},
	}
	for _, tc := range tests {
		filter, err := ParseTraceQL(tc.query)
		if err != nil {
			t.Fatalf("%s: %v", tc.query, err)
		}
		if got := filter.Match(span); got != tc.want {
			t.Errorf("%s match = %v, want %v", tc.query, got, tc.want)
		}
	}
}

func TestTraceQLQuotedAttributeName(t *testing.T) {
	span := testSpan()
	span.SpanAttributes["attribute name with space"] = "value"
	for _, query := range []string{
		`{ span."attribute name with space" = "value" }`,
		`{ ."attribute name with space" = "value" }`,
	} {
		filter, err := ParseTraceQL(query)
		if err != nil {
			t.Fatalf("%s: %v", query, err)
		}
		if !filter.Match(span) {
			t.Errorf("%s did not match", query)
		}
	}
}
