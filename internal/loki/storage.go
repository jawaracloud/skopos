package loki

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/jawaracloud/skopos/internal/chclient"
	"github.com/jawaracloud/skopos/internal/plan"
	"github.com/jawaracloud/skopos/internal/sqlgen"
)

// ClickHouseFetcher reads logs from the exporter's otel_logs table.
type ClickHouseFetcher struct {
	Client  *chclient.Client
	Schema  *plan.Schema
	Emitter *sqlgen.Emitter
	Names   plan.TableNames
	MaxRows int
}

// NewClickHouseFetcher returns a log fetcher.
func NewClickHouseFetcher(client *chclient.Client, schema *plan.Schema, names plan.TableNames, maxRows int) (*ClickHouseFetcher, error) {
	if client == nil || schema == nil {
		return nil, fmt.Errorf("loki: client and schema are required")
	}
	if maxRows <= 0 {
		return nil, fmt.Errorf("loki: max rows must be positive")
	}
	if _, ok := schema.Table(names.Logs); !ok {
		return nil, fmt.Errorf("loki: schema has no table %q", names.Logs)
	}
	// The API enforces a 7-day query range. A metric query additionally fetches
	// one range-selector lookback before its first step and may be step-aligned,
	// so the storage emitter allows a wider hard ceiling than the API.
	emitter, err := sqlgen.New(schema, sqlgen.Limits{MaxLimit: maxRows, MaxRange: 30 * 24 * time.Hour})
	if err != nil {
		return nil, err
	}
	return &ClickHouseFetcher{Client: client, Schema: schema, Emitter: emitter, Names: names, MaxRows: maxRows}, nil
}

// FetchRange returns log entries in [start, end] without pushdown.
func (f *ClickHouseFetcher) FetchRange(ctx context.Context, start, end time.Time) ([]LogEntry, error) {
	return f.FetchQueries(ctx, nil, start, end)
}

// FetchQueries returns the rows that may match any of queries in [start, end].
// It pushes stream selectors on known columns and body line filters into SQL.
// The returned rows are a superset of the matches: the caller must still run
// Query.Evaluate, which remains the final authority.
func (f *ClickHouseFetcher) FetchQueries(ctx context.Context, queries []Query, start, end time.Time) ([]LogEntry, error) {
	where := buildPushdownPredicate(f.Schema, f.Names.Logs, queries)
	entries, err := f.fetch(ctx, where, start, end)
	if err == nil || where == nil || !isRegexCompileError(err) {
		return entries, err
	}
	// Go and ClickHouse both use RE2, but if ClickHouse rejects a pattern the
	// Go parser accepted, fall back to the unfiltered fetch rather than
	// turning a working query into an error.
	return f.fetch(ctx, nil, start, end)
}

func (f *ClickHouseFetcher) fetch(ctx context.Context, where plan.Expr, start, end time.Time) ([]LogEntry, error) {
	query := f.logPlan(where, start, end)
	emitted, err := f.Emitter.Emit(query)
	if err != nil {
		return nil, fmt.Errorf("loki: build log query: %w", err)
	}
	result, err := f.Client.Query(ctx, emitted.SQL, emitted.Params)
	if err != nil {
		return nil, fmt.Errorf("loki: query logs: %w", err)
	}
	if result.Len() >= f.MaxRows {
		return nil, fmt.Errorf("loki: query reached the %d-row limit", f.MaxRows)
	}
	out := make([]LogEntry, 0, result.Len())
	for i := 0; i < result.Len(); i++ {
		ts, ok := result.String(i, "Timestamp")
		if !ok {
			continue
		}
		timestamp, err := parseCHTime(ts)
		if err != nil {
			return nil, err
		}
		severity, _ := result.Float64(i, "SeverityNumber")
		entry := LogEntry{
			Timestamp:      timestamp,
			Body:           stringValue(result, i, "Body"),
			ServiceName:    stringValue(result, i, "ServiceName"),
			SeverityText:   stringValue(result, i, "SeverityText"),
			SeverityNumber: uint8(severity),
			TraceID:        stringValue(result, i, "TraceId"),
			SpanID:         stringValue(result, i, "SpanId"),
			EventName:      stringValue(result, i, "EventName"),
			Labels:         make(map[string]string),
		}
		if entry.ServiceName != "" {
			entry.Labels["service_name"] = entry.ServiceName
		}
		if entry.SeverityText != "" {
			entry.Labels["severity_text"] = entry.SeverityText
		}
		if severity > 0 {
			entry.Labels["severity_number"] = strconv.Itoa(int(severity))
		}
		if entry.TraceID != "" {
			entry.Labels["trace_id"] = entry.TraceID
		}
		if entry.SpanID != "" {
			entry.Labels["span_id"] = entry.SpanID
		}
		if entry.EventName != "" {
			entry.Labels["event_name"] = entry.EventName
		}
		addAttributes(entry.Labels, anyAttributes(result.Rows[i]["ResourceAttributes"]))
		addAttributes(entry.Labels, anyAttributes(result.Rows[i]["LogAttributes"]))
		out = append(out, entry)
	}
	return out, nil
}

func (f *ClickHouseFetcher) logPlan(where plan.Expr, start, end time.Time) plan.Query {
	return plan.Query{
		Signal: plan.SignalLogs,
		Table:  f.Names.Logs,
		Select: []plan.SelectItem{
			{Expr: plan.Col("Timestamp")},
			{Expr: plan.Col("Body")},
			{Expr: plan.Col("ServiceName")},
			{Expr: plan.Col("SeverityText")},
			{Expr: plan.Col("SeverityNumber")},
			{Expr: plan.Col("TraceId")},
			{Expr: plan.Col("SpanId")},
			{Expr: plan.Col("EventName")},
			{Expr: plan.Col("LogAttributes")},
			{Expr: plan.Col("ResourceAttributes")},
		},
		Where:      where,
		TimeColumn: "Timestamp",
		TimeRange:  &plan.TimeRange{Start: start, End: end, EndInclusive: true},
		Limit:      f.MaxRows,
	}
}

// isRegexCompileError reports whether ClickHouse rejected an RE2 pattern. Go
// and ClickHouse share the same regexp family, but the fallback keeps a query
// working if a construction accepted by one engine is rejected by the other.
func isRegexCompileError(err error) bool {
	var clickhouseError *chclient.Error
	return errors.As(err, &clickhouseError) && clickhouseError.Code == "427"
}

func addAttributes(labels map[string]string, attrs map[string]string) {
	for key, value := range attrs {
		name := NormalizeLabelName(key)
		if _, exists := labels[name]; exists {
			continue
		}
		labels[name] = value
	}
}

func stringValue(result *chclient.Result, row int, column string) string {
	v, _ := result.String(row, column)
	return v
}

func anyAttributes(v any) map[string]string {
	raw, ok := v.(map[string]any)
	if !ok {
		return nil
	}
	out := make(map[string]string, len(raw))
	for k, value := range raw {
		if s, ok := value.(string); ok {
			out[k] = s
		}
	}
	return out
}

func parseCHTime(value string) (time.Time, error) {
	for _, layout := range []string{"2006-01-02 15:04:05.999999999", "2006-01-02 15:04:05", time.RFC3339Nano} {
		if t, err := time.ParseInLocation(layout, value, time.UTC); err == nil {
			return t, nil
		}
	}
	return time.Time{}, fmt.Errorf("loki: invalid timestamp %q", value)
}
