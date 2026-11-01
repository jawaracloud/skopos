package plan

import "testing"

func TestNestedColumnsAreArrays(t *testing.T) {
	s := DefaultSchema("otel", DefaultTableNames())
	checks := []struct{ table, column string }{
		{"otel_traces", "Events.Timestamp"},
		{"otel_traces", "Events.Name"},
		{"otel_traces", "Events.Attributes"},
		{"otel_traces", "Links.TraceId"},
		{"otel_traces", "Links.SpanId"},
		{"otel_traces", "Links.TraceState"},
		{"otel_traces", "Links.Attributes"},
		{"otel_metrics_gauge", "Exemplars.FilteredAttributes"},
		{"otel_metrics_gauge", "Exemplars.TimeUnix"},
		{"otel_metrics_gauge", "Exemplars.Value"},
		{"otel_metrics_gauge", "Exemplars.SpanId"},
		{"otel_metrics_gauge", "Exemplars.TraceId"},
		{"otel_metrics_summary", "ValueAtQuantiles.Quantile"},
		{"otel_metrics_summary", "ValueAtQuantiles.Value"},
	}
	for _, check := range checks {
		got, ok := s.ColumnType(check.table, check.column)
		if !ok {
			t.Errorf("%s.%s is missing", check.table, check.column)
			continue
		}
		if got != ColumnArray {
			t.Errorf("%s.%s type = %v, want ColumnArray", check.table, check.column, got)
		}
	}
}

func TestSummaryHasNoExemplars(t *testing.T) {
	s := DefaultSchema("otel", DefaultTableNames())
	for _, column := range []string{
		"Exemplars.FilteredAttributes",
		"Exemplars.TimeUnix",
		"Exemplars.Value",
		"Exemplars.SpanId",
		"Exemplars.TraceId",
	} {
		if s.HasColumn("otel_metrics_summary", column) {
			t.Errorf("summary unexpectedly has %s", column)
		}
	}
}

func TestTableSignals(t *testing.T) {
	s := DefaultSchema("otel", DefaultTableNames())
	checks := map[string]Signal{
		"otel_traces":                        SignalTraces,
		"otel_traces_trace_id_ts":            SignalTraces,
		"otel_logs":                          SignalLogs,
		"otel_metrics_gauge":                 SignalMetrics,
		"otel_metrics_sum":                   SignalMetrics,
		"otel_metrics_histogram":             SignalMetrics,
		"otel_metrics_exponential_histogram": SignalMetrics,
		"otel_metrics_summary":               SignalMetrics,
	}
	for table, want := range checks {
		got, ok := s.Table(table)
		if !ok {
			t.Errorf("%s is missing", table)
			continue
		}
		if got.Signal != want {
			t.Errorf("%s signal = %q, want %q", table, got.Signal, want)
		}
	}
}
