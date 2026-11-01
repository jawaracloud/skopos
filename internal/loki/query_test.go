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

func TestRejectsUnsupportedLogQL(t *testing.T) {
	for _, query := range []string{
		`{service_name="checkout"} | json`,
		`{service_name="checkout"} |= "x" | logfmt`,
		`rate({service_name="checkout"}[5m])`,
		`{service_name="checkout"`,
		`{service_name=checkout}`,
	} {
		if _, err := Parse(query); err == nil {
			t.Errorf("Parse(%q) succeeded, want error", query)
		}
	}
}
