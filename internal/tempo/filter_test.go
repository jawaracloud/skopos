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
