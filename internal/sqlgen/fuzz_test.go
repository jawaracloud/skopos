package sqlgen

import (
	"encoding/base64"
	"strings"
	"testing"
	"time"

	"github.com/obi-wrapper/skopos/internal/plan"
)

// FuzzEmitNoUserValueLeak asserts that user-controlled values (label values,
// label keys, regex patterns, and array elements) never appear in the emitted
// SQL. They must only appear in the parameter list.
func FuzzEmitNoUserValueLeak(f *testing.F) {
	f.Add("checkout", "service.name", "timeout|error")
	f.Add("", "", "")
	f.Add("'; DROP TABLE otel_traces; --", "a`b", ".*(?:")
	f.Add("line\nbreak\ttab", "ключ", "\\")
	f.Add(strings.Repeat("x", 300), strings.Repeat("y", 300), strings.Repeat("z", 300))

	schema := plan.DefaultSchema("otel", plan.DefaultTableNames())
	emitter, err := New(schema, Limits{MaxLimit: 1000, MaxRange: 24 * time.Hour})
	if err != nil {
		f.Fatalf("New: %v", err)
	}
	start := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	end := start.Add(time.Hour)

	f.Fuzz(func(t *testing.T, rawValue, rawKey, rawPattern string) {
		// Bound the input so a fuzz case cannot make the assertion itself
		// pathological. The property still covers binary/control characters.
		rawValue = truncate(rawValue, 512)
		rawKey = truncate(rawKey, 512)
		rawPattern = truncate(rawPattern, 512)

		marker := "SKOPOS_LEAK_" + base64.RawURLEncoding.EncodeToString([]byte(rawValue+rawKey+rawPattern))
		value := marker + ":" + rawValue
		key := marker + ":" + rawKey
		pattern := marker + ":" + rawPattern

		q := plan.Query{
			Signal: plan.SignalMetrics,
			Table:  "otel_metrics_gauge",
			Select: []plan.SelectItem{
				{Expr: plan.Col("MetricName")},
				{Expr: plan.Col("Value")},
			},
			Where: plan.Bin("AND",
				plan.Bin("=", plan.Col("MetricName"), plan.String("metric", value)),
				plan.Bin("AND",
					plan.Bin("=",
						plan.Map(plan.Col("Attributes"), plan.String("label_key", key)),
						plan.String("label_value", value),
					),
					plan.F("match", plan.Col("ServiceName"), plan.String("pattern", pattern)),
				),
			),
			TimeColumn: "TimeUnix",
			TimeRange:  &plan.TimeRange{Start: start, End: end},
			Limit:      10,
		}

		got, err := emitter.Emit(q)
		if err != nil {
			t.Fatalf("valid plan failed: %v", err)
		}
		if strings.Contains(got.SQL, marker) {
			t.Fatalf("user marker leaked into SQL:\n%s", got.SQL)
		}
		if !strings.Contains(got.SQL, "{metric:String}") ||
			!strings.Contains(got.SQL, "{label_key:String}") ||
			!strings.Contains(got.SQL, "{pattern:String}") {
			t.Fatalf("expected parameter placeholders were not emitted:\n%s", got.SQL)
		}
		found := false
		for _, p := range got.Params {
			if s, ok := p.Value.(string); ok && strings.Contains(s, marker) {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("user value was not preserved in the parameter list: %#v", got.Params)
		}
	})
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}
