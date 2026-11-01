package tempo

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"

	commonv1 "go.opentelemetry.io/proto/otlp/common/v1"
	resourcev1 "go.opentelemetry.io/proto/otlp/resource/v1"
	tracev1 "go.opentelemetry.io/proto/otlp/trace/v1"
	"google.golang.org/protobuf/proto"
)

// TraceFetcher is the storage surface used by the Tempo API.
type TraceFetcher interface {
	GetTrace(ctx context.Context, traceID string, start, end *time.Time) ([]Span, error)
	FetchRange(ctx context.Context, start, end time.Time) ([]Span, error)
}

// API serves the Tempo-compatible endpoints used by Grafana.
type API struct {
	Fetcher  TraceFetcher
	Version  string
	MaxRange time.Duration
	Now      func() time.Time
}

// NewAPI returns a Tempo-compatible API.
func NewAPI(fetcher TraceFetcher, version string) *API {
	return &API{Fetcher: fetcher, Version: version, MaxRange: 7 * 24 * time.Hour, Now: time.Now}
}

// Register attaches the Tempo routes to mux.
func (a *API) Register(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/echo", a.handleEcho)
	mux.HandleFunc("GET /api/status/buildinfo", a.handleBuildInfo)
	mux.HandleFunc("GET /api/search", a.handleSearch)
	mux.HandleFunc("GET /api/traces/{traceID}", a.handleTrace)
	mux.HandleFunc("GET /api/v2/traces/{traceID}", a.handleTrace)
	mux.HandleFunc("GET /api/search/tags", a.handleTagsV1)
	mux.HandleFunc("GET /api/v2/search/tags", a.handleTagsV2)
	mux.HandleFunc("GET /api/search/tag/{tag}/values", a.handleTagValuesV1)
	mux.HandleFunc("GET /api/v2/search/tag/{tag}/values", a.handleTagValuesV2)
}

func (a *API) now() time.Time {
	if a.Now != nil {
		return a.Now()
	}
	return time.Now()
}

func (a *API) handleEcho(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("echo"))
}

func (a *API) handleBuildInfo(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{
		"version":   a.Version,
		"revision":  "",
		"branch":    "",
		"buildDate": "",
		"buildUser": "",
		"goVersion": runtime.Version(),
	})
}

func (a *API) handleSearch(w http.ResponseWriter, r *http.Request) {
	start, end, err := a.timeRange(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	filter, err := a.searchFilter(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	minDuration, err := parseDurationParam(r.FormValue("minDuration"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "minDuration: "+err.Error())
		return
	}
	maxDuration, err := parseDurationParam(r.FormValue("maxDuration"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "maxDuration: "+err.Error())
		return
	}
	limit := parseLimit(r.FormValue("limit"), 20, 1000)
	spans, err := a.Fetcher.FetchRange(r.Context(), start, end)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	summaries := Search(spans, filter, minDuration, maxDuration, limit)
	traces := make([]map[string]any, 0, len(summaries))
	for _, summary := range summaries {
		traces = append(traces, map[string]any{
			"traceID":           summary.TraceID,
			"rootServiceName":   summary.RootServiceName,
			"rootTraceName":     summary.RootTraceName,
			"startTimeUnixNano": strconv.FormatInt(summary.Start.UnixNano(), 10),
			"durationMs":        summary.Duration.Milliseconds(),
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"traces": traces, "metrics": map[string]any{}})
}

func (a *API) handleTrace(w http.ResponseWriter, r *http.Request) {
	traceID := strings.ToLower(r.PathValue("traceID"))
	if len(traceID) != 32 {
		writeError(w, http.StatusBadRequest, "invalid trace ID")
		return
	}
	if _, err := hex.DecodeString(traceID); err != nil {
		writeError(w, http.StatusBadRequest, "invalid trace ID")
		return
	}
	var start, end *time.Time
	if r.FormValue("start") != "" || r.FormValue("end") != "" {
		s, err := parseOptionalTime(r.FormValue("start"))
		if err != nil {
			writeError(w, http.StatusBadRequest, "start: "+err.Error())
			return
		}
		e, err := parseOptionalTime(r.FormValue("end"))
		if err != nil {
			writeError(w, http.StatusBadRequest, "end: "+err.Error())
			return
		}
		start, end = s, e
	}
	spans, err := a.Fetcher.GetTrace(r.Context(), traceID, start, end)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if len(spans) == 0 {
		writeError(w, http.StatusNotFound, "trace not found")
		return
	}
	isV2 := strings.HasPrefix(r.URL.Path, "/api/v2/")
	if strings.Contains(r.Header.Get("Accept"), "application/protobuf") {
		data, err := otlpProto(spans)
		if err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		if isV2 {
			data = wrapTraceByIDResponse(data)
		}
		w.Header().Set("Content-Type", "application/protobuf")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(data)
		return
	}
	data, err := otlpJSON(spans)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if isV2 {
		data, err = json.Marshal(map[string]json.RawMessage{"trace": data})
		if err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(data)
}

func (a *API) handleTagsV1(w http.ResponseWriter, r *http.Request) {
	tags, err := a.tags(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	scope := r.FormValue("scope")
	if scope != "" {
		writeJSON(w, http.StatusOK, map[string]any{"tagNames": tags[scope], "metrics": map[string]any{}})
		return
	}
	all := make(map[string]struct{})
	for _, values := range tags {
		for _, value := range values {
			all[value] = struct{}{}
		}
	}
	names := make([]string, 0, len(all))
	for name := range all {
		names = append(names, name)
	}
	sort.Strings(names)
	writeJSON(w, http.StatusOK, map[string]any{"tagNames": names, "metrics": map[string]any{}})
}

func (a *API) handleTagsV2(w http.ResponseWriter, r *http.Request) {
	tags, err := a.tags(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	scopes := make([]map[string]any, 0, len(tags))
	for _, name := range []string{"resource", "span", "intrinsic"} {
		scopes = append(scopes, map[string]any{"name": name, "tags": tags[name]})
	}
	writeJSON(w, http.StatusOK, map[string]any{"scopes": scopes, "metrics": map[string]any{}})
}

func (a *API) tags(r *http.Request) (map[string][]string, error) {
	start, end, err := a.timeRange(r)
	if err != nil {
		return nil, err
	}
	filter, err := a.searchFilter(r)
	if err != nil {
		return nil, err
	}
	spans, err := a.Fetcher.FetchRange(r.Context(), start, end)
	if err != nil {
		return nil, err
	}
	names := TagNames(nil)
	for _, span := range spans {
		if filter != nil && !filter.Match(span) {
			continue
		}
		for key := range span.ResourceAttributes {
			names["resource"][key] = struct{}{}
		}
		if span.ServiceName != "" {
			names["resource"]["service.name"] = struct{}{}
		}
		for key := range span.SpanAttributes {
			names["span"][key] = struct{}{}
		}
	}
	out := make(map[string][]string, len(names))
	limit := parseLimit(r.FormValue("limit"), 1000, 10000)
	for scope, set := range names {
		values := make([]string, 0, len(set))
		for value := range set {
			values = append(values, value)
		}
		sort.Strings(values)
		if len(values) > limit {
			values = values[:limit]
		}
		out[scope] = values
	}
	return out, nil
}

func (a *API) handleTagValuesV1(w http.ResponseWriter, r *http.Request) {
	values, err := a.tagValues(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"tagValues": values, "metrics": map[string]any{}})
}

func (a *API) handleTagValuesV2(w http.ResponseWriter, r *http.Request) {
	values, err := a.tagValues(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	out := make([]map[string]string, 0, len(values))
	for _, value := range values {
		out = append(out, map[string]string{"type": "string", "value": value})
	}
	writeJSON(w, http.StatusOK, map[string]any{"tagValues": out, "metrics": map[string]any{}})
}

func (a *API) tagValues(r *http.Request) ([]string, error) {
	tag := r.PathValue("tag")
	if tag == "" {
		return nil, fmt.Errorf("tag is required")
	}
	start, end, err := a.timeRange(r)
	if err != nil {
		return nil, err
	}
	filter, err := a.searchFilter(r)
	if err != nil {
		return nil, err
	}
	spans, err := a.Fetcher.FetchRange(r.Context(), start, end)
	if err != nil {
		return nil, err
	}
	values := make(map[string]struct{})
	for _, span := range spans {
		if filter != nil && !filter.Match(span) {
			continue
		}
		if value, ok := SpanTag(span, tag); ok && value != "" {
			values[value] = struct{}{}
		}
	}
	out := make([]string, 0, len(values))
	for value := range values {
		out = append(out, value)
	}
	sort.Strings(out)
	limit := parseLimit(r.FormValue("limit"), 1000, 10000)
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

func (a *API) searchFilter(r *http.Request) (Filter, error) {
	if q := r.FormValue("q"); q != "" {
		return ParseTraceQL(q)
	}
	if tags := r.FormValue("tags"); tags != "" {
		return ParseTags(tags)
	}
	return nil, nil
}

func (a *API) timeRange(r *http.Request) (time.Time, time.Time, error) {
	now := a.now()
	start, err := parseTime(r.FormValue("start"), now.Add(-time.Hour))
	if err != nil {
		return time.Time{}, time.Time{}, fmt.Errorf("start: %w", err)
	}
	end, err := parseTime(r.FormValue("end"), now)
	if err != nil {
		return time.Time{}, time.Time{}, fmt.Errorf("end: %w", err)
	}
	if end.Before(start) {
		return time.Time{}, time.Time{}, fmt.Errorf("end is before start")
	}
	if a.MaxRange > 0 && end.Sub(start) > a.MaxRange {
		return time.Time{}, time.Time{}, fmt.Errorf("query range exceeds %s", a.MaxRange)
	}
	return start, end, nil
}

func parseTime(value string, fallback time.Time) (time.Time, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return fallback, nil
	}
	if t, err := time.Parse(time.RFC3339Nano, value); err == nil {
		return t, nil
	}
	seconds, err := strconv.ParseFloat(value, 64)
	if err != nil || math.IsNaN(seconds) || math.IsInf(seconds, 0) {
		return time.Time{}, fmt.Errorf("invalid timestamp %q", value)
	}
	whole, frac := math.Modf(seconds)
	return time.Unix(int64(whole), int64(frac*1e9)).UTC(), nil
}

func parseOptionalTime(value string) (*time.Time, error) {
	if value == "" {
		return nil, nil
	}
	t, err := parseTime(value, time.Time{})
	if err != nil {
		return nil, err
	}
	return &t, nil
}

func parseDurationParam(value string) (time.Duration, error) {
	if value == "" {
		return 0, nil
	}
	return time.ParseDuration(value)
}

func parseLimit(value string, fallback, max int) int {
	if value == "" {
		return fallback
	}
	n, err := strconv.Atoi(value)
	if err != nil || n <= 0 {
		return fallback
	}
	if n > max {
		return max
	}
	return n
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func writeError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]string{"error": message})
}

// --- OTLP conversion ---

type jsonResourceSpans struct {
	Resource   jsonResource     `json:"resource"`
	ScopeSpans []jsonScopeSpans `json:"scopeSpans"`
}

type jsonResource struct {
	Attributes []jsonKeyValue `json:"attributes"`
}

type jsonScopeSpans struct {
	Scope jsonScope  `json:"scope"`
	Spans []jsonSpan `json:"spans"`
}

type jsonScope struct {
	Name    string `json:"name"`
	Version string `json:"version,omitempty"`
}

type jsonSpan struct {
	TraceID           string         `json:"traceId,omitempty"`
	SpanID            string         `json:"spanId,omitempty"`
	ParentSpanID      string         `json:"parentSpanId,omitempty"`
	TraceState        string         `json:"traceState,omitempty"`
	Name              string         `json:"name,omitempty"`
	Kind              int32          `json:"kind"`
	StartTimeUnixNano string         `json:"startTimeUnixNano"`
	EndTimeUnixNano   string         `json:"endTimeUnixNano"`
	Attributes        []jsonKeyValue `json:"attributes,omitempty"`
	Status            *jsonStatus    `json:"status,omitempty"`
	Events            []jsonEvent    `json:"events,omitempty"`
	Links             []jsonLink     `json:"links,omitempty"`
}

type jsonStatus struct {
	Code    int32  `json:"code"`
	Message string `json:"message,omitempty"`
}

type jsonEvent struct {
	TimeUnixNano string         `json:"timeUnixNano"`
	Name         string         `json:"name,omitempty"`
	Attributes   []jsonKeyValue `json:"attributes,omitempty"`
}

type jsonLink struct {
	TraceID    string         `json:"traceId,omitempty"`
	SpanID     string         `json:"spanId,omitempty"`
	TraceState string         `json:"traceState,omitempty"`
	Attributes []jsonKeyValue `json:"attributes,omitempty"`
}

type jsonKeyValue struct {
	Key   string         `json:"key"`
	Value map[string]any `json:"value"`
}

func otlpJSON(spans []Span) ([]byte, error) {
	groups := groupSpans(spans)
	resourceSpans := make([]jsonResourceSpans, 0, len(groups))
	for _, group := range groups {
		scopeSpans := make([]jsonScopeSpans, 0, len(group.scopes))
		for _, skey := range group.order {
			scope := group.scopes[skey]
			jsonSpans := make([]jsonSpan, 0, len(scope.spans))
			for _, span := range scope.spans {
				jsonSpans = append(jsonSpans, jsonSpanFrom(span))
			}
			scopeSpans = append(scopeSpans, jsonScopeSpans{
				Scope: jsonScope{Name: scope.name, Version: scope.version},
				Spans: jsonSpans,
			})
		}
		resourceSpans = append(resourceSpans, jsonResourceSpans{
			Resource:   jsonResource{Attributes: jsonKeyValues(group.attrs)},
			ScopeSpans: scopeSpans,
		})
	}
	return json.Marshal(map[string]any{"resourceSpans": resourceSpans})
}

func jsonSpanFrom(span Span) jsonSpan {
	return jsonSpan{
		TraceID:           span.TraceID,
		SpanID:            span.SpanID,
		ParentSpanID:      span.ParentSpanID,
		TraceState:        span.TraceState,
		Name:              span.SpanName,
		Kind:              spanKind(span.SpanKind),
		StartTimeUnixNano: strconv.FormatInt(span.Timestamp.UnixNano(), 10),
		EndTimeUnixNano:   strconv.FormatInt(span.Timestamp.Add(time.Duration(span.Duration)).UnixNano(), 10),
		Attributes:        jsonKeyValues(span.SpanAttributes),
		Status:            &jsonStatus{Code: statusCode(span.StatusCode), Message: span.StatusMessage},
		Events:            jsonEvents(span.Events),
		Links:             jsonLinks(span.Links),
	}
}

func jsonEvents(events []Event) []jsonEvent {
	out := make([]jsonEvent, 0, len(events))
	for _, event := range events {
		out = append(out, jsonEvent{
			TimeUnixNano: strconv.FormatInt(event.Timestamp.UnixNano(), 10),
			Name:         event.Name,
			Attributes:   jsonKeyValues(event.Attributes),
		})
	}
	return out
}

func jsonLinks(links []Link) []jsonLink {
	out := make([]jsonLink, 0, len(links))
	for _, link := range links {
		out = append(out, jsonLink{
			TraceID:    link.TraceID,
			SpanID:     link.SpanID,
			TraceState: link.TraceState,
			Attributes: jsonKeyValues(link.Attributes),
		})
	}
	return out
}

func jsonKeyValues(attrs map[string]string) []jsonKeyValue {
	keys := make([]string, 0, len(attrs))
	for k := range attrs {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]jsonKeyValue, 0, len(keys))
	for _, key := range keys {
		out = append(out, jsonKeyValue{Key: key, Value: map[string]any{"stringValue": attrs[key]}})
	}
	return out
}

type resourceGroup struct {
	key    string
	attrs  map[string]string
	scopes map[string]*scopeGroup
	order  []string
}

type scopeGroup struct {
	name    string
	version string
	spans   []Span
}

func groupSpans(spans []Span) []*resourceGroup {
	groups := make(map[string]*resourceGroup)
	order := make([]string, 0)
	for _, span := range spans {
		attrs := cloneAttrs(span.ResourceAttributes)
		if span.ServiceName != "" {
			if _, ok := attrs["service.name"]; !ok {
				attrs["service.name"] = span.ServiceName
			}
		}
		rkey := attrsKey(attrs)
		group := groups[rkey]
		if group == nil {
			group = &resourceGroup{key: rkey, attrs: attrs, scopes: make(map[string]*scopeGroup)}
			groups[rkey] = group
			order = append(order, rkey)
		}
		skey := span.ScopeName + "\x00" + span.ScopeVersion
		scope := group.scopes[skey]
		if scope == nil {
			scope = &scopeGroup{name: span.ScopeName, version: span.ScopeVersion}
			group.scopes[skey] = scope
		}
		scope.spans = append(scope.spans, span)
	}
	out := make([]*resourceGroup, 0, len(order))
	for _, key := range order {
		group := groups[key]
		scopeKeys := make([]string, 0, len(group.scopes))
		for skey := range group.scopes {
			scopeKeys = append(scopeKeys, skey)
		}
		sort.Strings(scopeKeys)
		group.order = scopeKeys
		out = append(out, group)
	}
	return out
}

func attrsKey(attrs map[string]string) string {
	keys := make([]string, 0, len(attrs))
	for k := range attrs {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	for _, k := range keys {
		b.WriteString(k)
		b.WriteByte('=')
		b.WriteString(attrs[k])
		b.WriteByte(0)
	}
	return b.String()
}

func cloneAttrs(in map[string]string) map[string]string {
	out := make(map[string]string, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

func otlpProto(spans []Span) ([]byte, error) {
	groups := groupSpans(spans)
	data := &tracev1.TracesData{}
	for _, group := range groups {
		rs := &tracev1.ResourceSpans{
			Resource: &resourcev1.Resource{Attributes: keyValues(group.attrs)},
		}
		for _, skey := range group.order {
			scope := group.scopes[skey]
			ss := &tracev1.ScopeSpans{
				Scope: &commonv1.InstrumentationScope{Name: scope.name, Version: scope.version},
			}
			for _, span := range scope.spans {
				ss.Spans = append(ss.Spans, protoSpan(span))
			}
			rs.ScopeSpans = append(rs.ScopeSpans, ss)
		}
		data.ResourceSpans = append(data.ResourceSpans, rs)
	}
	return proto.Marshal(data)
}

func keyValues(attrs map[string]string) []*commonv1.KeyValue {
	keys := make([]string, 0, len(attrs))
	for k := range attrs {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]*commonv1.KeyValue, 0, len(keys))
	for _, key := range keys {
		out = append(out, &commonv1.KeyValue{
			Key:   key,
			Value: &commonv1.AnyValue{Value: &commonv1.AnyValue_StringValue{StringValue: attrs[key]}},
		})
	}
	return out
}

func protoSpan(span Span) *tracev1.Span {
	out := &tracev1.Span{
		TraceId:           decodeHex(span.TraceID),
		SpanId:            decodeHex(span.SpanID),
		ParentSpanId:      decodeHex(span.ParentSpanID),
		TraceState:        span.TraceState,
		Name:              span.SpanName,
		Kind:              tracev1.Span_SpanKind(spanKind(span.SpanKind)),
		StartTimeUnixNano: uint64(span.Timestamp.UnixNano()),
		EndTimeUnixNano:   uint64(span.Timestamp.Add(time.Duration(span.Duration)).UnixNano()),
		Attributes:        keyValues(span.SpanAttributes),
		Status:            &tracev1.Status{Code: tracev1.Status_StatusCode(statusCode(span.StatusCode)), Message: span.StatusMessage},
	}
	for _, event := range span.Events {
		out.Events = append(out.Events, &tracev1.Span_Event{
			TimeUnixNano: uint64(event.Timestamp.UnixNano()),
			Name:         event.Name,
			Attributes:   keyValues(event.Attributes),
		})
	}
	for _, link := range span.Links {
		out.Links = append(out.Links, &tracev1.Span_Link{
			TraceId:    decodeHex(link.TraceID),
			SpanId:     decodeHex(link.SpanID),
			TraceState: link.TraceState,
			Attributes: keyValues(link.Attributes),
		})
	}
	return out
}

// wrapTraceByIDResponse wraps an OTLP TracesData message in the Tempo v2
// TraceByIDResponse envelope. The envelope's field 1 is a Trace message whose
// field 1 is the repeated ResourceSpans, which is exactly TracesData's body.
// We encode it by hand to avoid importing Tempo's AGPL-licensed proto module.
func wrapTraceByIDResponse(trace []byte) []byte {
	out := []byte{0x0a}
	out = append(out, encodeVarint(uint64(len(trace)))...)
	out = append(out, trace...)
	return out
}

func encodeVarint(v uint64) []byte {
	var buf [10]byte
	n := 0
	for v >= 0x80 {
		buf[n] = byte(v) | 0x80
		v >>= 7
		n++
	}
	buf[n] = byte(v)
	n++
	return buf[:n]
}

func decodeHex(value string) []byte {
	out, _ := hex.DecodeString(value)
	return out
}

func spanKind(value string) int32 {
	switch strings.ToLower(strings.TrimPrefix(value, "span_kind_")) {
	case "internal":
		return int32(tracev1.Span_SPAN_KIND_INTERNAL)
	case "server":
		return int32(tracev1.Span_SPAN_KIND_SERVER)
	case "client":
		return int32(tracev1.Span_SPAN_KIND_CLIENT)
	case "producer":
		return int32(tracev1.Span_SPAN_KIND_PRODUCER)
	case "consumer":
		return int32(tracev1.Span_SPAN_KIND_CONSUMER)
	default:
		return 0
	}
}

func statusCode(value string) int32 {
	switch strings.ToLower(strings.TrimPrefix(value, "status_code_")) {
	case "ok":
		return int32(tracev1.Status_STATUS_CODE_OK)
	case "error":
		return int32(tracev1.Status_STATUS_CODE_ERROR)
	default:
		return 0
	}
}
