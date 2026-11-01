package plan

import "fmt"

// ColumnType is the small amount of type information the emitter needs to
// reject map access on a non-map column. It is not a full ClickHouse type
// system.
type ColumnType int

const (
	ColumnString ColumnType = iota
	ColumnDateTime
	ColumnNumber
	ColumnBool
	ColumnMap
	ColumnArray
	ColumnNested
)

// Table is a whitelisted ClickHouse table and its columns.
type Table struct {
	Name    string
	Signal  Signal
	Columns map[string]ColumnType
}

// TableNames are the physical table names configured by the chart. The
// gateway and the init job use the same names.
type TableNames struct {
	Traces                      string
	Logs                        string
	MetricsGauge                string
	MetricsSum                  string
	MetricsHistogram            string
	MetricsExponentialHistogram string
	MetricsSummary              string
}

// DefaultTableNames returns the exporter's default names.
func DefaultTableNames() TableNames {
	return TableNames{
		Traces:                      "otel_traces",
		Logs:                        "otel_logs",
		MetricsGauge:                "otel_metrics_gauge",
		MetricsSum:                  "otel_metrics_sum",
		MetricsHistogram:            "otel_metrics_histogram",
		MetricsExponentialHistogram: "otel_metrics_exponential_histogram",
		MetricsSummary:              "otel_metrics_summary",
	}
}

// Schema is the set of tables the emitter may reference. It is built from
// trusted configuration, never from a query.
type Schema struct {
	Database string
	Tables   map[string]Table
}

// Table returns a table by physical name.
func (s *Schema) Table(name string) (Table, bool) {
	if s == nil {
		return Table{}, false
	}
	t, ok := s.Tables[name]
	return t, ok
}

// HasColumn reports whether table has the given column.
func (s *Schema) HasColumn(table, column string) bool {
	t, ok := s.Table(table)
	if !ok {
		return false
	}
	_, ok = t.Columns[column]
	return ok
}

// ColumnType returns the type of a column.
func (s *Schema) ColumnType(table, column string) (ColumnType, bool) {
	t, ok := s.Table(table)
	if !ok {
		return 0, false
	}
	ct, ok := t.Columns[column]
	return ct, ok
}

// DefaultSchema builds the schema that mirrors the ClickHouse exporter at the
// pinned version. The table names are supplied by the chart.
func DefaultSchema(database string, names TableNames) *Schema {
	tables := map[string]Table{
		names.Traces: {
			Name: names.Traces,
			Columns: map[string]ColumnType{
				"Timestamp":          ColumnDateTime,
				"TraceId":            ColumnString,
				"SpanId":             ColumnString,
				"ParentSpanId":       ColumnString,
				"TraceState":         ColumnString,
				"SpanName":           ColumnString,
				"SpanKind":           ColumnString,
				"ServiceName":        ColumnString,
				"ResourceAttributes": ColumnMap,
				"ScopeName":          ColumnString,
				"ScopeVersion":       ColumnString,
				"SpanAttributes":     ColumnMap,
				"Duration":           ColumnNumber,
				"StatusCode":         ColumnString,
				"StatusMessage":      ColumnString,
				"Events.Timestamp":   ColumnArray,
				"Events.Name":        ColumnArray,
				"Events.Attributes":  ColumnArray,
				"Links.TraceId":      ColumnArray,
				"Links.SpanId":       ColumnArray,
				"Links.TraceState":   ColumnArray,
				"Links.Attributes":   ColumnArray,
			},
		},
		names.Traces + "_trace_id_ts": {
			Name: names.Traces + "_trace_id_ts",
			Columns: map[string]ColumnType{
				"TraceId": ColumnString,
				"Start":   ColumnDateTime,
				"End":     ColumnDateTime,
			},
		},
		names.Logs: {
			Name: names.Logs,
			Columns: map[string]ColumnType{
				"Timestamp":                              ColumnDateTime,
				"TraceId":                                ColumnString,
				"SpanId":                                 ColumnString,
				"TraceFlags":                             ColumnNumber,
				"SeverityText":                           ColumnString,
				"SeverityNumber":                         ColumnNumber,
				"ServiceName":                            ColumnString,
				"Body":                                   ColumnString,
				"ResourceSchemaUrl":                      ColumnString,
				"ResourceAttributes":                     ColumnMap,
				"ScopeSchemaUrl":                         ColumnString,
				"ScopeName":                              ColumnString,
				"ScopeVersion":                           ColumnString,
				"ScopeAttributes":                        ColumnMap,
				"LogAttributes":                          ColumnMap,
				"EventName":                              ColumnString,
				"__otel_materialized_k8s.cluster.name":   ColumnString,
				"__otel_materialized_k8s.container.name": ColumnString,
				"__otel_materialized_k8s.deployment.name":         ColumnString,
				"__otel_materialized_k8s.namespace.name":          ColumnString,
				"__otel_materialized_k8s.node.name":               ColumnString,
				"__otel_materialized_k8s.pod.name":                ColumnString,
				"__otel_materialized_k8s.pod.uid":                 ColumnString,
				"__otel_materialized_deployment.environment.name": ColumnString,
			},
		},
		names.MetricsGauge: {
			Name:    names.MetricsGauge,
			Columns: metricColumns(map[string]ColumnType{"Value": ColumnNumber}, true),
		},
		names.MetricsSum: {
			Name: names.MetricsSum,
			Columns: metricColumns(map[string]ColumnType{
				"Value":                  ColumnNumber,
				"AggregationTemporality": ColumnNumber,
				"IsMonotonic":            ColumnBool,
			}, true),
		},
		names.MetricsHistogram: {
			Name: names.MetricsHistogram,
			Columns: metricColumns(map[string]ColumnType{
				"Count":                  ColumnNumber,
				"Sum":                    ColumnNumber,
				"BucketCounts":           ColumnArray,
				"ExplicitBounds":         ColumnArray,
				"Min":                    ColumnNumber,
				"Max":                    ColumnNumber,
				"AggregationTemporality": ColumnNumber,
			}, true),
		},
		names.MetricsExponentialHistogram: {
			Name: names.MetricsExponentialHistogram,
			Columns: metricColumns(map[string]ColumnType{
				"Count":                  ColumnNumber,
				"Sum":                    ColumnNumber,
				"Scale":                  ColumnNumber,
				"ZeroCount":              ColumnNumber,
				"PositiveOffset":         ColumnNumber,
				"PositiveBucketCounts":   ColumnArray,
				"NegativeOffset":         ColumnNumber,
				"NegativeBucketCounts":   ColumnArray,
				"Min":                    ColumnNumber,
				"Max":                    ColumnNumber,
				"AggregationTemporality": ColumnNumber,
			}, true),
		},
		names.MetricsSummary: {
			Name: names.MetricsSummary,
			Columns: metricColumns(map[string]ColumnType{
				"Count":                     ColumnNumber,
				"Sum":                       ColumnNumber,
				"ValueAtQuantiles.Quantile": ColumnArray,
				"ValueAtQuantiles.Value":    ColumnArray,
			}, false),
		},
	}
	for name, signal := range map[string]Signal{
		names.Traces:                      SignalTraces,
		names.Traces + "_trace_id_ts":     SignalTraces,
		names.Logs:                        SignalLogs,
		names.MetricsGauge:                SignalMetrics,
		names.MetricsSum:                  SignalMetrics,
		names.MetricsHistogram:            SignalMetrics,
		names.MetricsExponentialHistogram: SignalMetrics,
		names.MetricsSummary:              SignalMetrics,
	} {
		t := tables[name]
		t.Signal = signal
		tables[name] = t
	}
	return &Schema{Database: database, Tables: tables}
}

func metricColumns(extra map[string]ColumnType, exemplars bool) map[string]ColumnType {
	cols := map[string]ColumnType{
		"ResourceAttributes":    ColumnMap,
		"ResourceSchemaUrl":     ColumnString,
		"ScopeName":             ColumnString,
		"ScopeVersion":          ColumnString,
		"ScopeAttributes":       ColumnMap,
		"ScopeDroppedAttrCount": ColumnNumber,
		"ScopeSchemaUrl":        ColumnString,
		"ServiceName":           ColumnString,
		"MetricName":            ColumnString,
		"MetricDescription":     ColumnString,
		"MetricUnit":            ColumnString,
		"Attributes":            ColumnMap,
		"StartTimeUnix":         ColumnDateTime,
		"TimeUnix":              ColumnDateTime,
		"Flags":                 ColumnNumber,
	}
	if exemplars {
		cols["Exemplars.FilteredAttributes"] = ColumnArray
		cols["Exemplars.TimeUnix"] = ColumnArray
		cols["Exemplars.Value"] = ColumnArray
		cols["Exemplars.SpanId"] = ColumnArray
		cols["Exemplars.TraceId"] = ColumnArray
	}
	for k, v := range extra {
		cols[k] = v
	}
	return cols
}

// ValidateIdentifier checks a database or table name is a safe SQL identifier.
func ValidateIdentifier(name string) error {
	if !ValidIdent(name) {
		return fmt.Errorf("invalid SQL identifier %q", name)
	}
	return nil
}
