package loki

import (
	"testing"
	"time"
)

func entry() LogEntry {
	return LogEntry{
		Timestamp: time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC),
		Body:      "request timeout after 30s",
		Labels: map[string]string{
			"service_name":          "checkout",
			"severity_text":         "ERROR",
			"resource_k8s_pod_name": "checkout-abc",
		},
	}
}

func TestParseSelectorAndLineFilter(t *testing.T) {
	query, err := Parse(`{service_name="checkout", severity_text=~"ERR.*"} |= "timeout"`)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if !query.Match(entry()) {
		t.Fatal("query did not match")
	}
	if query.Match(LogEntry{Body: "ok", Labels: map[string]string{"service_name": "checkout", "severity_text": "INFO"}}) {
		t.Fatal("query matched wrong entry")
	}
}

func TestLineFilters(t *testing.T) {
	tests := []struct {
		query string
		match bool
	}{
		{`{service_name="checkout"} != "timeout"`, false},
		{`{service_name="checkout"} |~ "timed? ?out"`, true},
		{`{service_name="checkout"} !~ "database"`, true},
	}
	for _, tc := range tests {
		query, err := Parse(tc.query)
		if err != nil {
			t.Fatalf("%s: %v", tc.query, err)
		}
		if got := query.Match(entry()); got != tc.match {
			t.Errorf("%s match = %v, want %v", tc.query, got, tc.match)
		}
	}
}

func TestNormalizesAttributeLabels(t *testing.T) {
	query, err := Parse(`{resource.k8s.pod.name="checkout-abc"}`)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if !query.Match(entry()) {
		t.Fatal("normalized attribute label did not match")
	}
}

func TestMatcherRegexIsAnchored(t *testing.T) {
	query, err := Parse(`{service_name=~"check"}`)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if query.Match(entry()) {
		t.Fatal("unanchored regex matched checkout")
	}
}

func TestJSONParserStage(t *testing.T) {
	body := LogEntry{
		Body: `{"level":"error","status":500,"nested":{"code":"E1"},"arr":[1,2],"severity_text":"BODY","message":"boom"}`,
		Labels: map[string]string{
			"service_name":  "checkout",
			"severity_text": "STREAM",
		},
	}
	query, err := Parse(`{service_name="checkout"} | json | level="error" | status >= 500`)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	labels, ok := query.Evaluate(body)
	if !ok {
		t.Fatal("json query did not match")
	}
	want := map[string]string{
		"service_name":            "checkout",
		"severity_text":           "STREAM",
		"severity_text_extracted": "BODY",
		"level":                   "error",
		"status":                  "500",
		"nested_code":             "E1",
		"message":                 "boom",
	}
	for name, value := range want {
		if labels[name] != value {
			t.Errorf("label %q = %q, want %q (labels %#v)", name, labels[name], value, labels)
		}
	}
	if _, ok := labels["arr"]; ok {
		t.Errorf("array label was extracted: %#v", labels)
	}
	if query.Match(LogEntry{Body: `{"level":"info","status":200}`, Labels: map[string]string{"service_name": "checkout"}}) {
		t.Fatal("json query matched a non-error line")
	}
}

func TestJSONExplicitExtraction(t *testing.T) {
	body := LogEntry{
		Body:   `{"level":"error","status":500,"nested":{"code":"E1"}}`,
		Labels: map[string]string{"service_name": "checkout"},
	}
	query, err := Parse(`{service_name="checkout"} | json status="status", code="nested.code"`)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	labels, ok := query.Evaluate(body)
	if !ok {
		t.Fatal("json query did not match")
	}
	if labels["status"] != "500" || labels["code"] != "E1" {
		t.Fatalf("explicit extraction = %#v", labels)
	}
	if _, ok := labels["level"]; ok {
		t.Fatalf("explicit extraction also extracted level: %#v", labels)
	}
}

func TestParserErrorsKeepLines(t *testing.T) {
	for _, queryText := range []string{
		`{service_name="checkout"} | json`,
		`{service_name="checkout"} | logfmt`,
	} {
		query, err := Parse(queryText)
		if err != nil {
			t.Fatalf("%s: %v", queryText, err)
		}
		if !query.Match(LogEntry{Body: "not json or logfmt", Labels: map[string]string{"service_name": "checkout"}}) {
			t.Errorf("%s dropped an unparseable line", queryText)
		}
	}
}

func TestLogfmtParserAndNumericLabelFilter(t *testing.T) {
	body := LogEntry{
		Body:   `level=error status=500 msg="bad thing" escaped="a \"quote\""`,
		Labels: map[string]string{"service_name": "checkout"},
	}
	query, err := Parse(`{service_name="checkout"} | logfmt | status >= 500 | level="error"`)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	labels, ok := query.Evaluate(body)
	if !ok {
		t.Fatal("logfmt query did not match")
	}
	if labels["msg"] != "bad thing" || labels["escaped"] != `a "quote"` {
		t.Fatalf("logfmt labels = %#v", labels)
	}
	if query.Match(LogEntry{Body: "level=info status=200", Labels: map[string]string{"service_name": "checkout"}}) {
		t.Fatal("numeric label filter matched status 200")
	}
	if query.Match(LogEntry{Body: "level=error status=abc", Labels: map[string]string{"service_name": "checkout"}}) {
		t.Fatal("numeric label filter matched a non-numeric value")
	}
}

func TestLabelFilterMissingLabelSemantics(t *testing.T) {
	tests := []struct {
		query string
		match bool
	}{
		{`{service_name="checkout"} | json | missing=""`, true},
		{`{service_name="checkout"} | json | missing!="x"`, true},
		{`{service_name="checkout"} | json | missing="x"`, false},
		{`{service_name="checkout"} | json | missing=~".*"`, true},
		{`{service_name="checkout"} | json | missing!~".*"`, false},
		{`{service_name="checkout"} | json | missing="x"`, false},
	}
	body := LogEntry{Body: `{"level":"info"}`, Labels: map[string]string{"service_name": "checkout"}}
	for _, tc := range tests {
		query, err := Parse(tc.query)
		if err != nil {
			t.Fatalf("%s: %v", tc.query, err)
		}
		if got := query.Match(body); got != tc.match {
			t.Errorf("%s match = %v, want %v", tc.query, got, tc.match)
		}
	}
}

func TestLabelFilterRegexIsAnchored(t *testing.T) {
	query, err := Parse(`{service_name="checkout"} | json | level=~"err"`)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if query.Match(LogEntry{Body: `{"level":"error"}`, Labels: map[string]string{"service_name": "checkout"}}) {
		t.Fatal("unanchored label filter regex matched error")
	}
}

func TestParseMetricQueries(t *testing.T) {
	tests := []struct {
		query     string
		function  string
		rng       time.Duration
		aggregate bool
		groupBy   []string
	}{
		{`count_over_time({service_name="checkout"}[5m])`, "count_over_time", 5 * time.Minute, false, nil},
		{`rate({service_name="checkout"} |= "timeout" [1h30m])`, "rate", 90 * time.Minute, false, nil},
		{`sum by (severity_text) (count_over_time({service_name="checkout"}[5m]))`, "count_over_time", 5 * time.Minute, true, []string{"severity_text"}},
		{`sum(rate({service_name="checkout"} | json | level="error" [5m]))`, "rate", 5 * time.Minute, true, nil},
	}
	for _, tc := range tests {
		query, err := Parse(tc.query)
		if err != nil {
			t.Fatalf("%s: %v", tc.query, err)
		}
		if query.Metric == nil {
			t.Fatalf("%s: not a metric query", tc.query)
		}
		if query.Metric.Function != tc.function || query.Metric.Range != tc.rng {
			t.Errorf("%s metric = %#v", tc.query, query.Metric)
		}
		if query.Metric.Aggregate != tc.aggregate {
			t.Errorf("%s aggregate = %v, want %v", tc.query, query.Metric.Aggregate, tc.aggregate)
		}
		if len(query.Metric.GroupBy) != len(tc.groupBy) {
			t.Errorf("%s groupBy = %#v, want %#v", tc.query, query.Metric.GroupBy, tc.groupBy)
		}
	}
}

func TestRejectsUnsupportedLogQL(t *testing.T) {
	for _, query := range []string{
		`{service_name="checkout"} | pattern "<_> <_>"`,
		`{service_name="checkout"} | regexp "(?P<level>.*)"`,
		`{service_name="checkout"} | unwrap duration`,
		`{service_name="checkout"} | line_format "{{.msg}}"`,
		`{service_name="checkout"} | label_format foo=bar`,
		`{service_name="checkout"} | drop level`,
		`{service_name="checkout"} | json --flatten`,
		`avg(count_over_time({service_name="checkout"}[5m]))`,
		`count_over_time({service_name="checkout"})`,
		`count_over_time({service_name="checkout"}[0m])`,
		`sum by (x) (avg(count_over_time({service_name="checkout"}[5m])))`,
		`sum by (x) (sum(count_over_time({service_name="checkout"}[5m])))`,
		`{service_name="checkout"`,
		`{service_name=checkout}`,
	} {
		if _, err := Parse(query); err == nil {
			t.Errorf("Parse(%q) succeeded, want error", query)
		}
	}
}
