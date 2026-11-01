package loki

import (
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/jawaracloud/skopos/internal/plan"
)

// pushdownColumns maps stream labels whose values come from a dedicated
// otel_logs column. Attribute labels are deliberately absent: their
// normalization and resource/log precedence are implemented in Go and are not
// reproduced by this pushdown.
var pushdownColumns = map[string]string{
	"service_name":  "ServiceName",
	"severity_text": "SeverityText",
	"trace_id":      "TraceId",
	"span_id":       "SpanId",
	"event_name":    "EventName",
}

// predicateBuilder assigns a unique parameter name to every user-controlled
// value it renders.
type predicateBuilder struct {
	next int
}

func (b *predicateBuilder) value(prefix, value string) plan.Param {
	p := plan.String(fmt.Sprintf("%s_%d", prefix, b.next), encodeStringParam(value))
	b.next++
	return p
}

// encodeStringParam escapes a value for ClickHouse's HTTP String parameter
// parser, which interprets backslash escapes and rejects raw tabs and newlines.
// The encoded parameter decodes back to the original bytes, so the SQL
// predicate sees exactly the LogQL value. Iterating bytes keeps invalid UTF-8
// unchanged.
func encodeStringParam(value string) string {
	if !strings.ContainsAny(value, "\\\t\n") {
		return value
	}
	var b strings.Builder
	b.Grow(len(value) + 8)
	for i := 0; i < len(value); i++ {
		switch value[i] {
		case '\\':
			b.WriteString(`\\`)
		case '\t':
			b.WriteString(`\t`)
		case '\n':
			b.WriteString(`\n`)
		default:
			b.WriteByte(value[i])
		}
	}
	return b.String()
}

// buildPushdownPredicate returns a WHERE expression that every row matching any
// of queries must also satisfy in ClickHouse. It is only a pre-filter: the Go
// evaluator still applies the full selector and pipeline to every fetched row.
//
// The expression is a conjunction of the stream selectors and line filters a
// query exposes. Parser stages and label filters are skipped because they
// depend on extracted labels. When multiple selectors are supplied (labels,
// label values, and series endpoints), the result is their OR. A selector with
// no pushable condition makes the OR trivially true, so no predicate is pushed
// at all.
func buildPushdownPredicate(schema *plan.Schema, table string, queries []Query) plan.Expr {
	b := &predicateBuilder{}
	var combined plan.Expr
	for i := range queries {
		predicate := b.queryPredicate(schema, table, queries[i])
		if predicate == nil {
			return nil
		}
		if combined == nil {
			combined = predicate
		} else {
			combined = plan.Bin("OR", combined, predicate)
		}
	}
	return combined
}

func (b *predicateBuilder) queryPredicate(schema *plan.Schema, table string, query Query) plan.Expr {
	parts := make([]plan.Expr, 0, len(query.Matchers)+len(query.Stages))
	for _, matcher := range query.Matchers {
		column, ok := pushdownColumns[matcher.Name]
		if !ok {
			continue
		}
		if schema != nil && !schema.HasColumn(table, column) {
			continue
		}
		if predicate := b.matcherPredicate(column, matcher); predicate != nil {
			parts = append(parts, predicate)
		}
	}
	for _, stage := range query.Stages {
		filter, ok := stage.(LineFilter)
		if !ok {
			continue
		}
		if predicate := b.lineFilterPredicate(filter); predicate != nil {
			parts = append(parts, predicate)
		}
	}
	return conjunction(parts)
}

// matcherPredicate renders a stream-selector matcher for a known column.
//
// A non-empty column value is also the label value, so the column predicate is
// necessary. An empty column may be filled from ResourceAttributes or
// LogAttributes by addAttributes; the extra empty-column comparison keeps those rows
// in the candidate set for the Go evaluator. The predicate is therefore never
// stricter than the Go evaluator, at the cost of scanning rows with an empty
// column.
func (b *predicateBuilder) matcherPredicate(column string, matcher Matcher) plan.Expr {
	col := plan.Col(column)
	var exact plan.Expr
	switch matcher.Op {
	case "=":
		exact = plan.Bin("=", col, b.value("label", matcher.Value))
	case "!=":
		exact = plan.Bin("!=", col, b.value("label", matcher.Value))
	case "=~":
		if matcher.re == nil {
			return nil
		}
		// The parser anchored the regex with ^(?:...)$; passing the compiled
		// source to ClickHouse keeps the same anchoring and uses the same RE2
		// family for the match.
		exact = plan.F("match", col, b.value("label", matcher.re.String()))
	case "!~":
		if matcher.re == nil {
			return nil
		}
		exact = plan.Un("NOT", plan.F("match", col, b.value("label", matcher.re.String())))
	default:
		return nil
	}
	// ClickHouse matches raw bytes; Go evaluates the JSON-decoded label, where
	// invalid UTF-8 becomes U+FFFD. Include invalid values so the Go evaluator
	// makes the final decision instead of the SQL predicate dropping them.
	if matcher.Op == "=~" || matcher.Op == "!~" || strings.ContainsRune(matcher.Value, utf8.RuneError) {
		exact = plan.Bin("OR", exact, invalidUTF8(col))
	}
	empty := plan.Bin("=", col, b.value("empty", ""))
	return plan.Bin("OR", exact, empty)
}

// lineFilterPredicate renders a body line filter. LogQL line filters are
// case-sensitive and are applied to the raw body, independent of parser
// stages, so they can always be folded into WHERE.
func (b *predicateBuilder) lineFilterPredicate(filter LineFilter) plan.Expr {
	body := plan.Col("Body")
	var exact plan.Expr
	guardInvalidUTF8 := false
	switch filter.Op {
	case "|=":
		exact = plan.Bin(">", plan.F("position", body, b.value("line", filter.Value)), plan.Integer(0))
		guardInvalidUTF8 = strings.ContainsRune(filter.Value, utf8.RuneError)
	case "!=":
		exact = plan.Bin("=", plan.F("position", body, b.value("line", filter.Value)), plan.Integer(0))
		guardInvalidUTF8 = strings.ContainsRune(filter.Value, utf8.RuneError)
	case "|~":
		if filter.re == nil {
			return nil
		}
		exact = plan.F("match", body, b.value("line", filter.re.String()))
		guardInvalidUTF8 = true
	case "!~":
		if filter.re == nil {
			return nil
		}
		exact = plan.Un("NOT", plan.F("match", body, b.value("line", filter.re.String())))
		guardInvalidUTF8 = true
	default:
		return nil
	}
	if guardInvalidUTF8 {
		// The Go evaluator sees the JSON-decoded body. Invalid bytes become
		// U+FFFD there, which can change a regex or a U+FFFD substring result.
		// Keep invalid bodies in the candidate set and let Go decide.
		return plan.Bin("OR", exact, invalidUTF8(body))
	}
	return exact
}

func invalidUTF8(expr plan.Expr) plan.Expr {
	return plan.Un("NOT", plan.F("isValidUTF8", expr))
}

func conjunction(parts []plan.Expr) plan.Expr {
	if len(parts) == 0 {
		return nil
	}
	out := parts[0]
	for _, part := range parts[1:] {
		out = plan.Bin("AND", out, part)
	}
	return out
}
