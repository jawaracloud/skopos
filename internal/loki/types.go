// Package loki implements the Loki-compatible log query head (M5).
//
// It supports stream selectors and line filters for log queries. LogQL metric
// queries and parser stages are rejected explicitly; see docs/logql-support.md.
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

// LineFilter is one |=, !=, |~, or !~ filter.
type LineFilter struct {
	Op    string
	Value string
	re    *regexp.Regexp
}

// Query is the supported LogQL subset.
type Query struct {
	Matchers []Matcher
	Filters  []LineFilter
}

// Match reports whether entry matches the selector and line filters.
func (q Query) Match(entry LogEntry) bool {
	for _, m := range q.Matchers {
		value := entry.Labels[m.Name]
		switch m.Op {
		case "=":
			if value != m.Value {
				return false
			}
		case "!=":
			if value == m.Value {
				return false
			}
		case "=~":
			if m.re == nil || !m.re.MatchString(value) {
				return false
			}
		case "!~":
			if m.re != nil && m.re.MatchString(value) {
				return false
			}
		}
	}
	for _, f := range q.Filters {
		switch f.Op {
		case "|=":
			if !strings.Contains(entry.Body, f.Value) {
				return false
			}
		case "!=":
			if strings.Contains(entry.Body, f.Value) {
				return false
			}
		case "|~":
			if f.re == nil || !f.re.MatchString(entry.Body) {
				return false
			}
		case "!~":
			if f.re != nil && f.re.MatchString(entry.Body) {
				return false
			}
		}
	}
	return true
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
