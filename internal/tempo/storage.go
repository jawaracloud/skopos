package tempo

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/mrofisr/skopos/internal/chclient"
	"github.com/mrofisr/skopos/internal/plan"
	"github.com/mrofisr/skopos/internal/sqlgen"
)

// Span is one trace span decoded from ClickHouse.
type Span struct {
	Timestamp          time.Time
	TraceID            string
	SpanID             string
	ParentSpanID       string
	TraceState         string
	SpanName           string
	SpanKind           string
	ServiceName        string
	ResourceAttributes map[string]string
	ScopeName          string
	ScopeVersion       string
	SpanAttributes     map[string]string
	Duration           uint64
	StatusCode         string
	StatusMessage      string
	Events             []Event
	Links              []Link
}

// Event is a span event.
type Event struct {
	Timestamp  time.Time
	Name       string
	Attributes map[string]string
}

// Link is a span link.
type Link struct {
	TraceID    string
	SpanID     string
	TraceState string
	Attributes map[string]string
}

// TraceSummary is a Tempo search result.
type TraceSummary struct {
	TraceID         string
	RootServiceName string
	RootTraceName   string
	Start           time.Time
	Duration        time.Duration
	// MatchedSpans are the spans that matched the query, in trace order.
	MatchedSpans []Span
	// AllSpans are every span fetched for the trace. They are used for
	// serviceStats and for the full-trace root/duration summary.
	AllSpans []Span
}

// ClickHouseFetcher reads spans from the exporter's otel_traces table.
type ClickHouseFetcher struct {
	Client  *chclient.Client
	Schema  *plan.Schema
	Emitter *sqlgen.Emitter
	Names   plan.TableNames
	MaxRows int
}

// NewClickHouseFetcher returns a trace fetcher.
func NewClickHouseFetcher(client *chclient.Client, schema *plan.Schema, names plan.TableNames, maxRows int) (*ClickHouseFetcher, error) {
	if client == nil || schema == nil {
		return nil, fmt.Errorf("tempo: client and schema are required")
	}
	if maxRows <= 0 {
		return nil, fmt.Errorf("tempo: max rows must be positive")
	}
	if _, ok := schema.Table(names.Traces); !ok {
		return nil, fmt.Errorf("tempo: schema has no table %q", names.Traces)
	}
	emitter, err := sqlgen.New(schema, sqlgen.Limits{MaxLimit: maxRows, MaxRange: 7 * 24 * time.Hour})
	if err != nil {
		return nil, err
	}
	return &ClickHouseFetcher{Client: client, Schema: schema, Emitter: emitter, Names: names, MaxRows: maxRows}, nil
}

// GetTrace loads all spans for one trace ID. start/end are optional.
func (f *ClickHouseFetcher) GetTrace(ctx context.Context, traceID string, start, end *time.Time) ([]Span, error) {
	query := plan.Query{
		Signal: plan.SignalTraces,
		Table:  f.Names.Traces,
		Select: spanSelects(),
		Where:  plan.Bin("=", plan.Col("TraceId"), plan.String("trace_id", traceID)),
		Limit:  f.MaxRows,
	}
	if start != nil && end != nil {
		query.TimeColumn = "Timestamp"
		query.TimeRange = &plan.TimeRange{Start: *start, End: *end, EndInclusive: true}
	}
	return f.fetch(ctx, query)
}

// FetchRange loads spans in a time range. Callers apply TraceQL filters.
func (f *ClickHouseFetcher) FetchRange(ctx context.Context, start, end time.Time) ([]Span, error) {
	return f.fetch(ctx, plan.Query{
		Signal:     plan.SignalTraces,
		Table:      f.Names.Traces,
		Select:     spanSelects(),
		TimeColumn: "Timestamp",
		TimeRange:  &plan.TimeRange{Start: start, End: end, EndInclusive: true},
		Limit:      f.MaxRows,
	})
}

func (f *ClickHouseFetcher) fetch(ctx context.Context, query plan.Query) ([]Span, error) {
	emitted, err := f.Emitter.Emit(query)
	if err != nil {
		return nil, fmt.Errorf("tempo: build trace query: %w", err)
	}
	result, err := f.Client.Query(ctx, emitted.SQL, emitted.Params)
	if err != nil {
		return nil, fmt.Errorf("tempo: query traces: %w", err)
	}
	if result.Len() >= f.MaxRows {
		return nil, fmt.Errorf("tempo: query reached the %d-row limit", f.MaxRows)
	}
	spans := make([]Span, 0, result.Len())
	for i := 0; i < result.Len(); i++ {
		span, err := spanFromRow(result, i)
		if err != nil {
			return nil, err
		}
		spans = append(spans, span)
	}
	return spans, nil
}

// Search groups spans by trace, evaluates trace-level and span-level
// intrinsics, and returns one summary per matching trace. The root service,
// root name, start, and duration are always computed from every fetched span
// in the trace, not only from the spans that matched the filter.
func Search(spans []Span, filter Filter, minDuration, maxDuration time.Duration, limit int) []TraceSummary {
	groups := make(map[string][]Span)
	order := make([]string, 0)
	for _, span := range spans {
		id := strings.ToLower(span.TraceID)
		if _, ok := groups[id]; !ok {
			order = append(order, id)
		}
		groups[id] = append(groups[id], span)
	}
	out := make([]TraceSummary, 0, len(groups))
	for _, id := range order {
		traceSpans := groups[id]
		info := TraceInfoFor(id, traceSpans)
		matched := make([]Span, 0, len(traceSpans))
		for _, span := range traceSpans {
			if filter == nil || matchTrace(filter, span, info) {
				matched = append(matched, span)
			}
		}
		if len(matched) == 0 {
			continue
		}
		duration := info.Duration()
		if minDuration > 0 && duration < minDuration {
			continue
		}
		if maxDuration > 0 && duration > maxDuration {
			continue
		}
		rootService := info.RootServiceName
		rootName := info.RootName
		if !info.HasRoot {
			// Display a best-effort root for orphan-only traces, but trace:
			// root intrinsics stay absent for these traces.
			sortSpans(traceSpans)
			rootService = rootServiceName(traceSpans[0])
			rootName = traceSpans[0].SpanName
		}
		sortSpans(matched)
		sortSpans(traceSpans)
		out = append(out, TraceSummary{
			TraceID:         id,
			RootServiceName: rootService,
			RootTraceName:   rootName,
			Start:           info.Start,
			Duration:        duration,
			MatchedSpans:    matched,
			AllSpans:        traceSpans,
		})
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Start.Equal(out[j].Start) {
			return out[i].TraceID < out[j].TraceID
		}
		return out[i].Start.After(out[j].Start)
	})
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out
}

func spanSelects() []plan.SelectItem {
	columns := []string{
		"Timestamp", "TraceId", "SpanId", "ParentSpanId", "TraceState", "SpanName", "SpanKind",
		"ServiceName", "ResourceAttributes", "ScopeName", "ScopeVersion", "SpanAttributes",
		"Duration", "StatusCode", "StatusMessage", "Events.Timestamp", "Events.Name",
		"Events.Attributes", "Links.TraceId", "Links.SpanId", "Links.TraceState", "Links.Attributes",
	}
	out := make([]plan.SelectItem, 0, len(columns))
	for _, column := range columns {
		out = append(out, plan.SelectItem{Expr: plan.Col(column)})
	}
	return out
}

func spanFromRow(result *chclient.Result, row int) (Span, error) {
	ts, ok := result.String(row, "Timestamp")
	if !ok {
		return Span{}, fmt.Errorf("tempo: missing Timestamp")
	}
	timestamp, err := parseCHTime(ts)
	if err != nil {
		return Span{}, err
	}
	duration, _ := result.Float64(row, "Duration")
	span := Span{
		Timestamp:          timestamp,
		TraceID:            stringValue(result, row, "TraceId"),
		SpanID:             stringValue(result, row, "SpanId"),
		ParentSpanID:       stringValue(result, row, "ParentSpanId"),
		TraceState:         stringValue(result, row, "TraceState"),
		SpanName:           stringValue(result, row, "SpanName"),
		SpanKind:           stringValue(result, row, "SpanKind"),
		ServiceName:        stringValue(result, row, "ServiceName"),
		ResourceAttributes: attributes(result, row, "ResourceAttributes"),
		ScopeName:          stringValue(result, row, "ScopeName"),
		ScopeVersion:       stringValue(result, row, "ScopeVersion"),
		SpanAttributes:     attributes(result, row, "SpanAttributes"),
		Duration:           uint64(duration),
		StatusCode:         stringValue(result, row, "StatusCode"),
		StatusMessage:      stringValue(result, row, "StatusMessage"),
	}
	eventTimes, _ := timeSlice(result.Rows[row]["Events.Timestamp"])
	eventNames, _ := stringSlice(result.Rows[row]["Events.Name"])
	eventAttrs, _ := mapSlice(result.Rows[row]["Events.Attributes"])
	for i := 0; i < len(eventTimes) && i < len(eventNames); i++ {
		span.Events = append(span.Events, Event{
			Timestamp:  eventTimes[i],
			Name:       eventNames[i],
			Attributes: mapAt(eventAttrs, i),
		})
	}
	linkTraceIDs, _ := stringSlice(result.Rows[row]["Links.TraceId"])
	linkSpanIDs, _ := stringSlice(result.Rows[row]["Links.SpanId"])
	linkStates, _ := stringSlice(result.Rows[row]["Links.TraceState"])
	linkAttrs, _ := mapSlice(result.Rows[row]["Links.Attributes"])
	for i := 0; i < len(linkTraceIDs) && i < len(linkSpanIDs); i++ {
		span.Links = append(span.Links, Link{
			TraceID:    linkTraceIDs[i],
			SpanID:     linkSpanIDs[i],
			TraceState: stringAt(linkStates, i),
			Attributes: mapAt(linkAttrs, i),
		})
	}
	return span, nil
}

func stringValue(result *chclient.Result, row int, column string) string {
	v, _ := result.String(row, column)
	return v
}

func parseCHTime(value string) (time.Time, error) {
	for _, layout := range []string{"2006-01-02 15:04:05.999999999", "2006-01-02 15:04:05", time.RFC3339Nano} {
		if t, err := time.ParseInLocation(layout, value, time.UTC); err == nil {
			return t, nil
		}
	}
	return time.Time{}, fmt.Errorf("tempo: invalid timestamp %q", value)
}

func attributes(result *chclient.Result, row int, column string) map[string]string {
	return anyAttributes(result.Rows[row][column])
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

func stringSlice(v any) ([]string, error) {
	values, ok := v.([]any)
	if !ok {
		if values, ok := v.([]string); ok {
			return values, nil
		}
		return nil, fmt.Errorf("tempo: unexpected string array %T", v)
	}
	out := make([]string, 0, len(values))
	for _, value := range values {
		s, ok := value.(string)
		if !ok {
			return nil, fmt.Errorf("tempo: unexpected string array element %T", value)
		}
		out = append(out, s)
	}
	return out, nil
}

func timeSlice(v any) ([]time.Time, error) {
	values, err := stringSlice(v)
	if err != nil {
		return nil, err
	}
	out := make([]time.Time, 0, len(values))
	for _, value := range values {
		t, err := parseCHTime(value)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, nil
}

func mapSlice(v any) ([]map[string]string, error) {
	values, ok := v.([]any)
	if !ok {
		return nil, fmt.Errorf("tempo: unexpected map array %T", v)
	}
	out := make([]map[string]string, 0, len(values))
	for _, value := range values {
		out = append(out, anyAttributes(value))
	}
	return out, nil
}

func mapAt(values []map[string]string, i int) map[string]string {
	if i < len(values) {
		return values[i]
	}
	return nil
}

func stringAt(values []string, i int) string {
	if i < len(values) {
		return values[i]
	}
	return ""
}

// SpanTag returns the first value for a tag on a span. It is kept for callers
// that only need a single value; event and link scopes can produce several.
func SpanTag(span Span, tag string) (string, bool) {
	values := TagValues(span, tag, TraceInfoFor(span.TraceID, []Span{span}))
	if len(values) == 0 {
		return "", false
	}
	return values[0], true
}

// TagValues returns every value for a tag. Tag names use the same scopes as
// TraceQL: resource.*, span.*, event.*, link.*, and the colon intrinsics such
// as span:name and trace:rootService.
func TagValues(span Span, tag string, info TraceInfo) []string {
	ref, err := parseKey(tag)
	if err != nil {
		return nil
	}
	c := condition{
		scope:     ref.scope,
		key:       ref.key,
		intrinsic: ref.intrinsic,
		explicit:  ref.explicit,
		enum:      ref.enum,
	}
	switch ref.scope {
	case "event":
		values := eventValues(span, c)
		out := make([]string, 0, len(values))
		for _, value := range values {
			out = append(out, value.text)
		}
		return out
	case "link":
		values := linkValues(span, c)
		out := make([]string, 0, len(values))
		for _, value := range values {
			out = append(out, value.text)
		}
		return out
	}
	actual, ok := c.actual(span, info)
	if !ok {
		return nil
	}
	return []string{actual.text}
}

// TagNames returns all tag names grouped by Tempo scope. Intrinsic names match
// the list Tempo exposes through /api/v2/search/tags.
func TagNames(spans []Span) map[string]map[string]struct{} {
	out := map[string]map[string]struct{}{
		"resource":  {},
		"span":      {},
		"event":     {},
		"link":      {},
		"intrinsic": {},
	}
	for _, name := range []string{
		"duration", "event:name", "event:timeSinceStart",
		"instrumentation:name", "instrumentation:version", "kind", "name",
		"rootName", "rootServiceName", "span:duration", "span:kind", "span:name",
		"span:status", "span:statusMessage", "status", "statusMessage",
		"trace:duration", "trace:rootName", "trace:rootService", "traceDuration",
	} {
		out["intrinsic"][name] = struct{}{}
	}
	for _, span := range spans {
		for key := range span.ResourceAttributes {
			out["resource"][key] = struct{}{}
		}
		if span.ServiceName != "" {
			out["resource"]["service.name"] = struct{}{}
		}
		for key := range span.SpanAttributes {
			out["span"][key] = struct{}{}
		}
		for _, event := range span.Events {
			for key := range event.Attributes {
				out["event"][key] = struct{}{}
			}
		}
		for _, link := range span.Links {
			for key := range link.Attributes {
				out["link"][key] = struct{}{}
			}
		}
	}
	return out
}
