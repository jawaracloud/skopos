// Package loki implements the Loki-compatible log query head (M5).
//
// It supports stream selectors, line filters, the json and logfmt parser
// stages, label filters, and the count_over_time and rate log metric queries.
// Unsupported LogQL features are rejected at parse time; see
// docs/logql-support.md.
package loki

import (
	"regexp"
	"strings"
	"time"
)

// LogEntry is one log line with its stream labels.
type LogEntry struct {
	Timestamp      time.Time
	Body           string
	ServiceName    string
	SeverityText   string
	SeverityNumber uint8
	TraceID        string
	SpanID         string
	EventName      string
	Labels         map[string]string
}

// Matcher is one stream-selector matcher.
type Matcher struct {
	Name  string
	Op    string
	Value string
	re    *regexp.Regexp
}

// LineFilter is one |=, !=, |~, or !~ line filter.
type LineFilter struct {
	Op    string
	Value string
	re    *regexp.Regexp
}

// Stage is one pipeline stage. Stages run in query order after the stream
// selector and may add extracted labels for later stages and metric grouping.
type Stage interface {
	apply(body string, labels map[string]string) bool
}

// ParserStage extracts labels from the log body with | json or | logfmt.
type ParserStage struct {
	Kind        string
	Extractions []Extraction
}

// Extraction maps one JSON field path to a label.
type Extraction struct {
	Label string
	Path  []string
}

// LabelFilter is one label filter such as | level="error".
type LabelFilter struct {
	Name    string
	Op      string
	Value   string
	re      *regexp.Regexp
	numeric bool
	number  float64
}

// MetricQuery is the supported LogQL metric subset: count_over_time, rate,
// and an optional sum or sum by (...) aggregation.
type MetricQuery struct {
	Function  string
	Range     time.Duration
	Aggregate bool
	GroupBy   []string
}

// Query is a parsed LogQL query. When Metric is nil this is a log query.
type Query struct {
	Matchers []Matcher
	Stages   []Stage
	Metric   *MetricQuery
}

// Match reports whether entry matches the selector, pipeline, and filters.
func (q Query) Match(entry LogEntry) bool {
	_, ok := q.Evaluate(entry)
	return ok
}

// Evaluate reports whether entry matches and returns the effective label set
// after parser stages. Extracted labels are copied; entry.Labels is not
// modified.
func (q Query) Evaluate(entry LogEntry) (map[string]string, bool) {
	for _, m := range q.Matchers {
		if !matchSelector(m, entry.Labels[m.Name]) {
			return nil, false
		}
	}
	labels := entry.Labels
	if len(q.Stages) == 0 {
		return labels, true
	}
	labels = cloneLabels(entry.Labels)
	for _, stage := range q.Stages {
		if !stage.apply(entry.Body, labels) {
			return nil, false
		}
	}
	return labels, true
}

func matchSelector(m Matcher, value string) bool {
	switch m.Op {
	case "=":
		return value == m.Value
	case "!=":
		return value != m.Value
	case "=~":
		return m.re != nil && m.re.MatchString(value)
	case "!~":
		return m.re == nil || !m.re.MatchString(value)
	default:
		return false
	}
}

func (f LineFilter) apply(body string, _ map[string]string) bool {
	switch f.Op {
	case "|=":
		return strings.Contains(body, f.Value)
	case "!=":
		return !strings.Contains(body, f.Value)
	case "|~":
		return f.re != nil && f.re.MatchString(body)
	case "!~":
		return f.re == nil || !f.re.MatchString(body)
	default:
		return false
	}
}

func (f LabelFilter) apply(_ string, labels map[string]string) bool {
	value := labels[f.Name]
	if f.numeric {
		number, err := parseLabelNumber(value)
		if err != nil {
			return false
		}
		switch f.Op {
		case ">":
			return number > f.number
		case ">=":
			return number >= f.number
		case "<":
			return number < f.number
		case "<=":
			return number <= f.number
		default:
			return false
		}
	}
	switch f.Op {
	case "=":
		return value == f.Value
	case "!=":
		return value != f.Value
	case "=~":
		return f.re != nil && f.re.MatchString(value)
	case "!~":
		return f.re == nil || !f.re.MatchString(value)
	default:
		return false
	}
}

func cloneLabels(in map[string]string) map[string]string {
	out := make(map[string]string, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

// NormalizeLabelName maps an OpenTelemetry attribute key to a Loki label name.
func NormalizeLabelName(name string) string {
	var b strings.Builder
	for i, r := range name {
		ok := (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '_'
		if !ok {
			b.WriteByte('_')
			continue
		}
		if i == 0 && r >= '0' && r <= '9' {
			b.WriteByte('_')
		}
		b.WriteRune(r)
	}
	if b.Len() == 0 {
		return "_"
	}
	return b.String()
}
