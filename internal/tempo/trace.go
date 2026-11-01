package tempo

import (
	"sort"
	"strings"
	"time"
)

// TraceInfo is the trace-level context used by trace: intrinsics and by the
// search summary. It is computed from every span of a trace that the gateway
// fetched, not only from the spans that matched a filter.
type TraceInfo struct {
	TraceID         string
	Start           time.Time
	End             time.Time
	HasRoot         bool
	RootSpanID      string
	RootServiceName string
	RootName        string
}

// Duration returns max(end) - min(start) across the fetched trace spans.
func (t TraceInfo) Duration() time.Duration {
	if t.Start.IsZero() || t.End.Before(t.Start) {
		return 0
	}
	return t.End.Sub(t.Start)
}

// TraceInfoFor computes trace-level intrinsics for one trace.
func TraceInfoFor(traceID string, spans []Span) TraceInfo {
	info := TraceInfo{TraceID: strings.ToLower(traceID)}
	if len(spans) == 0 {
		return info
	}
	info.Start = spans[0].Timestamp
	info.End = spanEnd(spans[0])

	var root *Span
	for i := range spans {
		span := &spans[i]
		if span.Timestamp.Before(info.Start) {
			info.Start = span.Timestamp
		}
		if end := spanEnd(*span); end.After(info.End) {
			info.End = end
		}
		if !isRootSpan(*span) {
			continue
		}
		if root == nil || span.Timestamp.Before(root.Timestamp) ||
			(span.Timestamp.Equal(root.Timestamp) && span.SpanID < root.SpanID) {
			root = span
		}
	}
	if root != nil {
		info.HasRoot = true
		info.RootSpanID = strings.ToLower(root.SpanID)
		info.RootServiceName = rootServiceName(*root)
		info.RootName = root.SpanName
	}
	return info
}

// TraceInfoMap groups spans and returns one TraceInfo per trace ID. Group
// iteration order matches the first span seen for each trace.
func TraceInfoMap(spans []Span) map[string]TraceInfo {
	groups := make(map[string][]Span)
	order := make([]string, 0)
	for _, span := range spans {
		id := strings.ToLower(span.TraceID)
		if _, ok := groups[id]; !ok {
			order = append(order, id)
		}
		groups[id] = append(groups[id], span)
	}
	out := make(map[string]TraceInfo, len(groups))
	for _, id := range order {
		out[id] = TraceInfoFor(id, groups[id])
	}
	return out
}

func spanEnd(span Span) time.Time {
	return span.Timestamp.Add(time.Duration(span.Duration))
}

func isRootSpan(span Span) bool {
	parent := strings.ToLower(strings.TrimSpace(span.ParentSpanID))
	if parent == "" {
		return true
	}
	return strings.Trim(parent, "0") == ""
}

func rootServiceName(span Span) string {
	if span.ServiceName != "" {
		return span.ServiceName
	}
	return span.ResourceAttributes["service.name"]
}

func sortSpans(spans []Span) {
	sort.SliceStable(spans, func(i, j int) bool {
		if spans[i].Timestamp.Equal(spans[j].Timestamp) {
			return spans[i].SpanID < spans[j].SpanID
		}
		return spans[i].Timestamp.Before(spans[j].Timestamp)
	})
}
