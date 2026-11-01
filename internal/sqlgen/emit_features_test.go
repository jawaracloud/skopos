package sqlgen

import (
	"strings"
	"testing"
	"time"

	"github.com/obi-wrapper/skopos/internal/plan"
)

func rangeOneHour() *plan.TimeRange {
	return &plan.TimeRange{
		Start: time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC),
		End:   time.Date(2026, 11, 1, 1, 0, 0, 0, time.UTC),
	}
}

func TestEmitMetricsFiltered(t *testing.T) {
	q := plan.Query{
		Signal: plan.SignalMetrics,
		Table:  "otel_metrics_gauge",
		Select: []plan.SelectItem{
			{Expr: plan.Col("MetricName")},
			{Expr: plan.Col("TimeUnix")},
			{Expr: plan.Col("Value")},
		},
		Where: plan.Bin("AND",
			plan.Bin("=", plan.Col("MetricName"), plan.String("metric", "http_requests_total")),
			plan.Bin("=",
				plan.Map(plan.Col("Attributes"), plan.String("service_label", "service.name")),
				plan.String("service", "checkout"),
			),
		),
		TimeColumn: "TimeUnix",
		TimeRange:  rangeOneHour(),
		OrderBy:    []plan.Order{{By: "TimeUnix"}},
		Limit:      500,
	}

	got, err := testEmitter(t).Emit(q)
	if err != nil {
		t.Fatalf("Emit: %v", err)
	}
	checkGolden(t, "metrics_filtered", got)
}

func TestEmitLogsRegex(t *testing.T) {
	q := plan.Query{
		Signal: plan.SignalLogs,
		Table:  "otel_logs",
		Select: []plan.SelectItem{
			{Expr: plan.Col("Timestamp")},
			{Expr: plan.Col("Body")},
			{Expr: plan.Col("SeverityText")},
		},
		Where: plan.Bin("AND",
			plan.Bin("=", plan.Col("ServiceName"), plan.String("service", "checkout")),
			plan.F("match", plan.Col("Body"), plan.String("pattern", "timeout|error")),
		),
		TimeColumn: "Timestamp",
		TimeRange:  rangeOneHour(),
		OrderBy:    []plan.Order{{By: "Timestamp", Desc: true}},
		Limit:      200,
	}

	got, err := testEmitter(t).Emit(q)
	if err != nil {
		t.Fatalf("Emit: %v", err)
	}
	checkGolden(t, "logs_regex", got)
}

func TestEmitMetricsAggregation(t *testing.T) {
	q := plan.Query{
		Signal: plan.SignalMetrics,
		Table:  "otel_metrics_gauge",
		Select: []plan.SelectItem{
			{
				Expr:  plan.F("toStartOfInterval", plan.Col("TimeUnix"), plan.IntInterval(60, "SECOND")),
				Alias: "ts",
			},
			{Expr: plan.Agg("avg", plan.Col("Value")), Alias: "value"},
			{
				Expr:  plan.Map(plan.Col("Attributes"), plan.String("service_label", "service.name")),
				Alias: "service",
			},
		},
		Where:      plan.Bin("=", plan.Col("MetricName"), plan.String("metric", "http_requests_total")),
		TimeColumn: "TimeUnix",
		TimeRange:  rangeOneHour(),
		GroupBy:    []string{"ts", "service"},
		Having:     plan.Bin(">", plan.Agg("avg", plan.Col("Value")), plan.Float("threshold", 0.5)),
		OrderBy:    []plan.Order{{By: "ts"}},
		Limit:      1000,
	}

	got, err := testEmitter(t).Emit(q)
	if err != nil {
		t.Fatalf("Emit: %v", err)
	}
	checkGolden(t, "metrics_aggregation", got)
}

func TestEmitTracesArrayParam(t *testing.T) {
	q := plan.Query{
		Signal: plan.SignalTraces,
		Table:  "otel_traces",
		Select: []plan.SelectItem{
			{Expr: plan.Col("TraceId")},
			{Expr: plan.Col("SpanName")},
			{Expr: plan.Col("Duration")},
		},
		Where: plan.Bin("AND",
			plan.F("has", plan.Strings("services", []string{"checkout", "cart"}), plan.Col("ServiceName")),
			plan.Bin("=",
				plan.Map(plan.Col("SpanAttributes"), plan.String("attr_key", "http.method")),
				plan.String("method", "GET"),
			),
		),
		TimeColumn: "Timestamp",
		TimeRange:  rangeOneHour(),
		OrderBy:    []plan.Order{{By: "Duration", Desc: true}},
		Limit:      250,
	}

	got, err := testEmitter(t).Emit(q)
	if err != nil {
		t.Fatalf("Emit: %v", err)
	}
	checkGolden(t, "traces_array", got)
}

func TestEmitEndInclusive(t *testing.T) {
	q := plan.Query{
		Signal:     plan.SignalMetrics,
		Table:      "otel_metrics_gauge",
		Select:     []plan.SelectItem{{Expr: plan.Col("Value")}},
		TimeColumn: "TimeUnix",
		TimeRange: &plan.TimeRange{
			Start:        time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC),
			End:          time.Date(2026, 11, 1, 1, 0, 0, 0, time.UTC),
			EndInclusive: true,
		},
		Limit: 1,
	}

	got, err := testEmitter(t).Emit(q)
	if err != nil {
		t.Fatalf("Emit: %v", err)
	}
	if !strings.Contains(got.SQL, "`TimeUnix` <= {__end:DateTime64(9)}") {
		t.Fatalf("end boundary is not inclusive:\n%s", got.SQL)
	}
}

func TestEmitRejectsInvalidPlans(t *testing.T) {
	base := func() plan.Query {
		return plan.Query{
			Signal:     plan.SignalMetrics,
			Table:      "otel_metrics_gauge",
			Select:     []plan.SelectItem{{Expr: plan.Col("Value")}},
			TimeColumn: "TimeUnix",
			TimeRange:  rangeOneHour(),
			Limit:      10,
		}
	}

	tests := []struct {
		name   string
		mutate func(*plan.Query)
		want   string
	}{
		{"unknown table", func(q *plan.Query) { q.Table = "otel_secrets" }, "unknown table"},
		{"unknown column", func(q *plan.Query) { q.Select[0].Expr = plan.Col("Password") }, "unknown column"},
		{"empty select", func(q *plan.Query) { q.Select = nil }, "select list is empty"},
		{"duplicate alias", func(q *plan.Query) {
			q.Select = []plan.SelectItem{
				{Expr: plan.Col("Value"), Alias: "v"},
				{Expr: plan.Col("MetricName"), Alias: "v"},
			}
		}, "duplicate alias"},
		{"invalid alias", func(q *plan.Query) { q.Select[0].Alias = "value; DROP TABLE" }, "invalid alias"},
		{"unknown function", func(q *plan.Query) {
			q.Select[0].Expr = plan.F("systemCommand", plan.Col("Value"))
		}, "unsupported function"},
		{"unknown aggregate", func(q *plan.Query) {
			q.Select[0].Expr = plan.Agg("secretAgg", plan.Col("Value"))
		}, "unsupported aggregate"},
		{"unknown operator", func(q *plan.Query) {
			q.Where = plan.Bin("OR 1=1 --", plan.Col("Value"), plan.Integer(1))
		}, "unsupported binary operator"},
		{"duplicate param", func(q *plan.Query) {
			q.Where = plan.Bin("AND",
				plan.Bin("=", plan.Col("MetricName"), plan.String("x", "a")),
				plan.Bin("=", plan.Col("ServiceName"), plan.String("x", "b")),
			)
		}, "duplicate parameter"},
		{"map on scalar", func(q *plan.Query) {
			q.Where = plan.Bin("=",
				plan.Map(plan.Col("ServiceName"), plan.String("key", "x")),
				plan.String("value", "y"),
			)
		}, "not a map"},
		{"time range too large", func(q *plan.Query) {
			q.TimeRange = &plan.TimeRange{
				Start: time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC),
				End:   time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC),
			}
		}, "exceeds maximum"},
		{"offset without limit", func(q *plan.Query) { q.Limit = 0; q.Offset = 10 }, "offset requires a limit"},
		{"limit too large", func(q *plan.Query) { q.Limit = 10001 }, "exceeds maximum"},
		{"missing time column", func(q *plan.Query) { q.TimeColumn = "" }, "time column is required"},
		{"unknown time column", func(q *plan.Query) { q.TimeColumn = "Timestamp" }, "unknown time column"},
		{"empty in", func(q *plan.Query) {
			q.Where = plan.In{Expr: plan.Col("MetricName")}
		}, "IN list is empty"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			q := base()
			tc.mutate(&q)
			_, err := testEmitter(t).Emit(q)
			if err == nil {
				t.Fatalf("expected error containing %q, got nil", tc.want)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error %q does not contain %q", err, tc.want)
			}
		})
	}
}
