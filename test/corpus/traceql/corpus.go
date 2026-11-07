// Package traceqlcorpus loads the deterministic TraceQL corpus used by the
// gateway unit tests, the ClickHouse integration test, and the Tempo
// differential harness. The expected results in corpus.json were captured from
// Grafana Tempo 2.7.2; see README.md in this directory.
package traceqlcorpus

import (
	"embed"
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strconv"
	"time"
)

//go:embed corpus.json
var files embed.FS

// Corpus is the on-disk trace and query corpus.
type Corpus struct {
	Version          int         `json:"version"`
	BaseTimeUnixNano string      `json:"baseTimeUnixNano"`
	Traces           []Trace     `json:"traces"`
	Queries          []Query     `json:"queries"`
	TagNameTests     []TagTest   `json:"tagNameTests"`
	TagValueTests    []TagValue  `json:"tagValueTests"`
	TraceTests       []TraceTest `json:"traceTests"`
}

// Trace is one trace with its spans.
type Trace struct {
	TraceID string `json:"traceID"`
	Spans   []Span `json:"spans"`
}

// Span is one corpus span. Times are offsets from the corpus base time.
type Span struct {
	SpanID             string         `json:"spanID"`
	ParentSpanID       string         `json:"parentSpanID,omitempty"`
	Name               string         `json:"name"`
	Service            string         `json:"service"`
	ScopeName          string         `json:"scopeName,omitempty"`
	ScopeVersion       string         `json:"scopeVersion,omitempty"`
	Kind               string         `json:"kind"`
	Status             string         `json:"status"`
	StatusMessage      string         `json:"statusMessage,omitempty"`
	StartOffsetNanos   int64          `json:"startOffsetNanos"`
	DurationNanos      int64          `json:"durationNanos"`
	ResourceAttributes map[string]any `json:"resourceAttributes,omitempty"`
	Attributes         map[string]any `json:"attributes,omitempty"`
	Events             []Event        `json:"events,omitempty"`
	Links              []Link         `json:"links,omitempty"`
}

// Event is a span event.
type Event struct {
	OffsetNanos int64          `json:"offsetNanos"`
	Name        string         `json:"name"`
	Attributes  map[string]any `json:"attributes,omitempty"`
}

// Link is a span link.
type Link struct {
	TraceID    string         `json:"traceID"`
	SpanID     string         `json:"spanID"`
	Attributes map[string]any `json:"attributes,omitempty"`
}

// Query is one TraceQL search query with its reference result.
type Query struct {
	Name               string              `json:"name"`
	Query              string              `json:"query"`
	ExpectTraceIDs     []string            `json:"expectTraceIDs,omitempty"`
	ExpectMatchedSpans map[string][]string `json:"expectMatchedSpans,omitempty"`
	ExpectError        bool                `json:"expectError,omitempty"`
	// ExpectErrorContains is checked against the HTTP 400 error body when
	// ExpectError is true. It documents which unsupported feature the query
	// exercises without pinning the full message in the corpus.
	ExpectErrorContains string `json:"expectErrorContains,omitempty"`
	// SkipTempo marks gateway behavior that Tempo 2.7 does not implement (for
	// example the nil extension). The harness still checks the gateway.
	SkipTempo bool   `json:"skipTempo,omitempty"`
	Note      string `json:"note,omitempty"`
}

// TagTest checks the tag names returned for one scope.
type TagTest struct {
	Name   string   `json:"name"`
	Scope  string   `json:"scope"`
	Expect []string `json:"expect"`
}

// TagValue checks one /search/tag/{tag}/values query.
type TagValue struct {
	Name      string   `json:"name"`
	Tag       string   `json:"tag"`
	Expect    []string `json:"expect"`
	SkipTempo bool     `json:"skipTempo,omitempty"`
	Note      string   `json:"note,omitempty"`
}

// TraceTest checks one trace-by-id response.
type TraceTest struct {
	TraceID      string   `json:"traceID"`
	SpanCount    int      `json:"spanCount"`
	SpanIDs      []string `json:"spanIDs"`
	Names        []string `json:"names"`
	RootSpanID   string   `json:"rootSpanID"`
	EventNames   []string `json:"eventNames,omitempty"`
	LinkTraceIDs []string `json:"linkTraceIDs,omitempty"`
}

// Load returns the embedded corpus.
func Load() (*Corpus, error) {
	data, err := files.ReadFile("corpus.json")
	if err != nil {
		return nil, err
	}
	var corpus Corpus
	if err := json.Unmarshal(data, &corpus); err != nil {
		return nil, fmt.Errorf("decode traceql corpus: %w", err)
	}
	if corpus.Version != 1 {
		return nil, fmt.Errorf("unsupported traceql corpus version %d", corpus.Version)
	}
	return &corpus, nil
}

// BaseTime returns the corpus base time.
func (c *Corpus) BaseTime() time.Time {
	ns, err := strconv.ParseInt(c.BaseTimeUnixNano, 10, 64)
	if err != nil {
		return time.Time{}
	}
	return time.Unix(0, ns).UTC()
}

// SpanStart returns the absolute start time of a span.
func SpanStart(base time.Time, span Span) time.Time {
	return base.Add(time.Duration(span.StartOffsetNanos))
}

// SpanEnd returns the absolute end time of a span.
func SpanEnd(base time.Time, span Span) time.Time {
	return SpanStart(base, span).Add(time.Duration(span.DurationNanos))
}

// FindTrace returns a trace by ID.
func (c *Corpus) FindTrace(id string) (Trace, bool) {
	for _, trace := range c.Traces {
		if trace.TraceID == id {
			return trace, true
		}
	}
	return Trace{}, false
}

type otlpResourceSpans struct {
	Resource   otlpResource     `json:"resource"`
	ScopeSpans []otlpScopeSpans `json:"scopeSpans"`
}

type otlpResource struct {
	Attributes []otlpKeyValue `json:"attributes"`
}

type otlpScopeSpans struct {
	Scope otlpScope  `json:"scope"`
	Spans []otlpSpan `json:"spans"`
}

type otlpScope struct {
	Name    string `json:"name"`
	Version string `json:"version,omitempty"`
}

type otlpSpan struct {
	TraceID           string         `json:"traceId"`
	SpanID            string         `json:"spanId"`
	ParentSpanID      string         `json:"parentSpanId,omitempty"`
	Name              string         `json:"name"`
	Kind              int            `json:"kind"`
	StartTimeUnixNano string         `json:"startTimeUnixNano"`
	EndTimeUnixNano   string         `json:"endTimeUnixNano"`
	Attributes        []otlpKeyValue `json:"attributes,omitempty"`
	Status            otlpStatus     `json:"status"`
	Events            []otlpEvent    `json:"events,omitempty"`
	Links             []otlpLink     `json:"links,omitempty"`
}

type otlpStatus struct {
	Code    int    `json:"code"`
	Message string `json:"message,omitempty"`
}

type otlpEvent struct {
	TimeUnixNano string         `json:"timeUnixNano"`
	Name         string         `json:"name"`
	Attributes   []otlpKeyValue `json:"attributes,omitempty"`
}

type otlpLink struct {
	TraceID    string         `json:"traceId"`
	SpanID     string         `json:"spanId"`
	Attributes []otlpKeyValue `json:"attributes,omitempty"`
}

type otlpKeyValue struct {
	Key   string         `json:"key"`
	Value map[string]any `json:"value"`
}

// OTLPJSON renders the corpus as an OTLP/HTTP JSON request body (the format
// Tempo's receiver accepts) with all timestamps shifted to base.
func (c *Corpus) OTLPJSON(base time.Time) ([]byte, error) {
	resourceSpans := make([]otlpResourceSpans, 0, len(c.Traces))
	for _, trace := range c.Traces {
		type resourceGroup struct {
			key    string
			attrs  map[string]string
			scopes map[string][]Span
			order  []string
		}
		groups := make(map[string]*resourceGroup)
		order := make([]string, 0)
		for _, span := range trace.Spans {
			attrs := cloneStrings(span.ResourceAttributes)
			if span.Service != "" {
				attrs["service.name"] = span.Service
			}
			key := attrsKey(attrs)
			group := groups[key]
			if group == nil {
				group = &resourceGroup{key: key, attrs: attrs, scopes: make(map[string][]Span)}
				groups[key] = group
				order = append(order, key)
			}
			scopeKey := span.ScopeName + "\x00" + span.ScopeVersion
			if _, ok := group.scopes[scopeKey]; !ok {
				group.order = append(group.order, scopeKey)
			}
			group.scopes[scopeKey] = append(group.scopes[scopeKey], span)
		}
		for _, key := range order {
			group := groups[key]
			rs := otlpResourceSpans{Resource: otlpResource{Attributes: otlpStringAttributes(group.attrs)}}
			for _, scopeKey := range group.order {
				scopeSpans := group.scopes[scopeKey]
				name, version := "corpus", "1.0"
				if len(scopeSpans) > 0 {
					if scopeSpans[0].ScopeName != "" {
						name = scopeSpans[0].ScopeName
					}
					if scopeSpans[0].ScopeVersion != "" {
						version = scopeSpans[0].ScopeVersion
					}
				}
				ss := otlpScopeSpans{Scope: otlpScope{Name: name, Version: version}}
				for _, span := range scopeSpans {
					start := SpanStart(base, span)
					end := SpanEnd(base, span)
					js := otlpSpan{
						TraceID:           trace.TraceID,
						SpanID:            span.SpanID,
						ParentSpanID:      span.ParentSpanID,
						Name:              span.Name,
						Kind:              otlpKind(span.Kind),
						StartTimeUnixNano: strconv.FormatInt(start.UnixNano(), 10),
						EndTimeUnixNano:   strconv.FormatInt(end.UnixNano(), 10),
						Attributes:        otlpAttributes(span.Attributes),
						Status:            otlpStatus{Code: statusCode(span.Status), Message: span.StatusMessage},
					}
					for _, event := range span.Events {
						js.Events = append(js.Events, otlpEvent{
							TimeUnixNano: strconv.FormatInt(start.Add(time.Duration(event.OffsetNanos)).UnixNano(), 10),
							Name:         event.Name,
							Attributes:   otlpAttributes(event.Attributes),
						})
					}
					for _, link := range span.Links {
						js.Links = append(js.Links, otlpLink{
							TraceID:    link.TraceID,
							SpanID:     link.SpanID,
							Attributes: otlpAttributes(link.Attributes),
						})
					}
					ss.Spans = append(ss.Spans, js)
				}
				rs.ScopeSpans = append(rs.ScopeSpans, ss)
			}
			resourceSpans = append(resourceSpans, rs)
		}
	}
	return json.Marshal(map[string]any{"resourceSpans": resourceSpans})
}

// ClickHouseRows renders the corpus as JSONEachRow documents for the exporter's
// otel_traces table.
func (c *Corpus) ClickHouseRows(base time.Time) ([]string, error) {
	rows := make([]string, 0)
	for _, trace := range c.Traces {
		for _, span := range trace.Spans {
			attrs := cloneStrings(span.ResourceAttributes)
			if span.Service != "" {
				attrs["service.name"] = span.Service
			}
			row := map[string]any{
				"Timestamp":          SpanStart(base, span).UTC().Format("2006-01-02 15:04:05.000000000"),
				"TraceId":            trace.TraceID,
				"SpanId":             span.SpanID,
				"ParentSpanId":       span.ParentSpanID,
				"TraceState":         "",
				"SpanName":           span.Name,
				"SpanKind":           chKind(span.Kind),
				"ServiceName":        span.Service,
				"ResourceAttributes": attrs,
				"ScopeName":          defaultString(span.ScopeName, "corpus"),
				"ScopeVersion":       defaultString(span.ScopeVersion, "1.0"),
				"SpanAttributes":     stringMap(span.Attributes),
				"Duration":           span.DurationNanos,
				"StatusCode":         chStatus(span.Status),
				"StatusMessage":      span.StatusMessage,
				"Events.Timestamp":   []string{},
				"Events.Name":        []string{},
				"Events.Attributes":  []map[string]string{},
				"Links.TraceId":      []string{},
				"Links.SpanId":       []string{},
				"Links.TraceState":   []string{},
				"Links.Attributes":   []map[string]string{},
			}
			for _, event := range span.Events {
				start := SpanStart(base, span)
				row["Events.Timestamp"] = append(row["Events.Timestamp"].([]string), start.Add(time.Duration(event.OffsetNanos)).UTC().Format("2006-01-02 15:04:05.000000000"))
				row["Events.Name"] = append(row["Events.Name"].([]string), event.Name)
				row["Events.Attributes"] = append(row["Events.Attributes"].([]map[string]string), stringMap(event.Attributes))
			}
			for _, link := range span.Links {
				row["Links.TraceId"] = append(row["Links.TraceId"].([]string), link.TraceID)
				row["Links.SpanId"] = append(row["Links.SpanId"].([]string), link.SpanID)
				row["Links.TraceState"] = append(row["Links.TraceState"].([]string), "")
				row["Links.Attributes"] = append(row["Links.Attributes"].([]map[string]string), stringMap(link.Attributes))
			}
			data, err := json.Marshal(row)
			if err != nil {
				return nil, err
			}
			rows = append(rows, string(data))
		}
	}
	return rows, nil
}

// StringAttributes converts a corpus attribute map to the string map used by
// the ClickHouse exporter (and by the gateway's in-memory Span type).
func StringAttributes(attrs map[string]any) map[string]string {
	if len(attrs) == 0 {
		return nil
	}
	return stringMap(attrs)
}

func defaultString(value, fallback string) string {
	if value == "" {
		return fallback
	}
	return value
}

func cloneStrings(in map[string]any) map[string]string {
	out := make(map[string]string, len(in)+1)
	for key, value := range in {
		out[key] = attrString(value)
	}
	return out
}

func stringMap(in map[string]any) map[string]string {
	out := make(map[string]string, len(in))
	for key, value := range in {
		out[key] = attrString(value)
	}
	return out
}

func attrString(value any) string {
	switch v := value.(type) {
	case string:
		return v
	case bool:
		return strconv.FormatBool(v)
	case float64:
		if v == math.Trunc(v) {
			return strconv.FormatInt(int64(v), 10)
		}
		return strconv.FormatFloat(v, 'g', -1, 64)
	case json.Number:
		return v.String()
	default:
		return fmt.Sprint(value)
	}
}

func attrsKey(attrs map[string]string) string {
	keys := make([]string, 0, len(attrs))
	for key := range attrs {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	var b []byte
	for _, key := range keys {
		b = append(b, key...)
		b = append(b, '=')
		b = append(b, attrs[key]...)
		b = append(b, 0)
	}
	return string(b)
}

func otlpStringAttributes(attrs map[string]string) []otlpKeyValue {
	if len(attrs) == 0 {
		return nil
	}
	keys := make([]string, 0, len(attrs))
	for key := range attrs {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	out := make([]otlpKeyValue, 0, len(keys))
	for _, key := range keys {
		out = append(out, otlpKeyValue{Key: key, Value: map[string]any{"stringValue": attrs[key]}})
	}
	return out
}

func otlpAttributes(attrs map[string]any) []otlpKeyValue {
	if len(attrs) == 0 {
		return nil
	}
	keys := make([]string, 0, len(attrs))
	for key := range attrs {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	out := make([]otlpKeyValue, 0, len(keys))
	for _, key := range keys {
		out = append(out, otlpKeyValue{Key: key, Value: otlpValue(attrs[key])})
	}
	return out
}

func otlpValue(value any) map[string]any {
	switch v := value.(type) {
	case string:
		return map[string]any{"stringValue": v}
	case bool:
		return map[string]any{"boolValue": v}
	case float64:
		if v == math.Trunc(v) {
			return map[string]any{"intValue": strconv.FormatInt(int64(v), 10)}
		}
		return map[string]any{"doubleValue": v}
	case json.Number:
		return map[string]any{"intValue": v.String()}
	default:
		return map[string]any{"stringValue": fmt.Sprint(value)}
	}
}

func otlpKind(kind string) int {
	switch kind {
	case "internal":
		return 1
	case "server":
		return 2
	case "client":
		return 3
	case "producer":
		return 4
	case "consumer":
		return 5
	default:
		return 0
	}
}

func statusCode(status string) int {
	switch status {
	case "ok":
		return 1
	case "error":
		return 2
	default:
		return 0
	}
}

func chKind(kind string) string {
	switch kind {
	case "internal":
		return "Internal"
	case "server":
		return "Server"
	case "client":
		return "Client"
	case "producer":
		return "Producer"
	case "consumer":
		return "Consumer"
	default:
		return "Unspecified"
	}
}

func chStatus(status string) string {
	switch status {
	case "ok":
		return "Ok"
	case "error":
		return "Error"
	default:
		return "Unset"
	}
}
