package tempo

import (
	"fmt"
	"strings"
)

// traceQLSupportedSubset is appended to every unsupported-syntax error so a
// caller can tell which query shapes the gateway does implement.
const traceQLSupportedSubset = "supported subset: one spanset { ... } with &&, || and comparison operators =, !=, =~, !~, >, <, >=, <="

// traceQLMetricsFunctions are Tempo's TraceQL metrics functions. They are
// separated from search pipelines only to produce a more precise error.
var traceQLMetricsFunctions = map[string]bool{
	"rate":                true,
	"count_over_time":     true,
	"quantile_over_time":  true,
	"histogram_over_time": true,
	"compare":             true,
}

// splitTraceQLSpanset returns the body of the first spanset and any text after
// its closing brace. It honours quoted strings so braces in values do not
// affect the scan.
func splitTraceQLSpanset(query string) (body, tail string, err error) {
	if !strings.HasPrefix(query, "{") {
		return "", "", traceQLQueryShapeError(query)
	}
	inQuote := byte(0)
	depth := 0
	for i := 0; i < len(query); i++ {
		ch := query[i]
		if inQuote != 0 {
			if ch == '\\' {
				i++
				continue
			}
			if ch == inQuote {
				inQuote = 0
			}
			continue
		}
		switch ch {
		case '"', '\'':
			inQuote = ch
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return strings.TrimSpace(query[1:i]), strings.TrimSpace(query[i+1:]), nil
			}
			if depth < 0 {
				return "", "", fmt.Errorf("tempo: TraceQL has an unmatched '}'; %s", traceQLSupportedSubset)
			}
		}
	}
	if inQuote != 0 {
		return "", "", fmt.Errorf("tempo: TraceQL spanset has an unterminated string literal; %s", traceQLSupportedSubset)
	}
	return "", "", fmt.Errorf("tempo: TraceQL spanset is not closed: missing '}'; %s", traceQLSupportedSubset)
}

// checkTraceQLSpansetBody rejects unsupported syntax that can appear inside a
// single spanset: nested braces, grouping/functions, and pipelines. The
// logical operators && and || are supported inside the spanset.
func checkTraceQLSpansetBody(body string) error {
	if body == "" {
		return nil
	}
	inQuote := byte(0)
	for i := 0; i < len(body); i++ {
		ch := body[i]
		if inQuote != 0 {
			if ch == '\\' {
				i++
				continue
			}
			if ch == inQuote {
				inQuote = 0
			}
			continue
		}
		switch ch {
		case '"', '\'':
			inQuote = ch
		case '{', '}':
			return fmt.Errorf("tempo: TraceQL nested spansets are not supported; %s", traceQLSupportedSubset)
		case '(', ')':
			return traceQLGroupingError(string(ch))
		case '|':
			if i+1 < len(body) && body[i+1] == '|' {
				i++
				continue
			}
			detail, name := traceQLPipelineDetail(body, i)
			return traceQLPipelineOrMetricsError(detail, name)
		case '!':
			// Longest first: !>> and !<< must win over !> and !<.
			for _, op := range []string{"!>>", "!<<", "!>", "!<"} {
				if strings.HasPrefix(body[i:], op) {
					return traceQLStructuralOperatorError(op)
				}
			}
			// != and !~ are supported comparison operators.
			if i+1 < len(body) && (body[i+1] == '=' || body[i+1] == '~') {
				i++
			}
		case '>':
			if i+1 < len(body) && body[i+1] == '>' {
				return traceQLStructuralOperatorError(">>")
			}
		case '<':
			if i+1 < len(body) && body[i+1] == '<' {
				return traceQLStructuralOperatorError("<<")
			}
		}
	}
	if inQuote != 0 {
		return fmt.Errorf("tempo: TraceQL query has an unterminated string literal; %s", traceQLSupportedSubset)
	}
	return nil
}

// checkTraceQLTail rejects structural operators, multiple spansets, and
// pipelines after the closing brace of the first spanset.
func checkTraceQLTail(tail string) error {
	switch {
	case strings.HasPrefix(tail, "&&"):
		return traceQLMultipleSpansetsError("&&")
	case strings.HasPrefix(tail, "||"):
		return traceQLMultipleSpansetsError("||")
	case strings.HasPrefix(tail, "{"):
		return traceQLMultipleSpansetsError("")
	case strings.HasPrefix(tail, "|"):
		detail, name := traceQLPipelineDetail(tail, 0)
		return traceQLPipelineOrMetricsError(detail, name)
	}
	for _, op := range []string{"!>>", "!<<", ">>", "<<", "!>", "!<", "~", ">", "<"} {
		if strings.HasPrefix(tail, op) {
			return traceQLStructuralOperatorError(op)
		}
	}
	if strings.HasPrefix(tail, "&") {
		return traceQLMultipleSpansetsError("&")
	}
	if strings.HasPrefix(tail, "(") {
		return traceQLGroupingError("(")
	}
	return fmt.Errorf("tempo: TraceQL unexpected text after the spanset: %q; %s", tail, traceQLSupportedSubset)
}

func traceQLQueryShapeError(query string) error {
	if name := traceQLLeadingFunction(query); name != "" {
		detail := "| " + name + "()"
		if traceQLMetricsFunctions[name] {
			return traceQLMetricsError(detail)
		}
		return traceQLPipelineError(detail)
	}
	if strings.HasPrefix(query, "(") {
		return traceQLGroupingError("(")
	}
	return fmt.Errorf("tempo: TraceQL query must start with a single { ... } spanset; %s", traceQLSupportedSubset)
}

// traceQLLeadingFunction returns the identifier in a bare function call such
// as rate() or count_over_time(...), or "" when the query does not start with
// one.
func traceQLLeadingFunction(query string) string {
	i := 0
	for i < len(query) && isTraceQLIdentifierByte(query[i]) {
		i++
	}
	if i == 0 || i >= len(query) || query[i] != '(' {
		return ""
	}
	return query[:i]
}

func isTraceQLIdentifierByte(ch byte) bool {
	return ch == '_' || ch >= 'a' && ch <= 'z' || ch >= 'A' && ch <= 'Z' || ch >= '0' && ch <= '9'
}

// traceQLPipelineDetail describes the pipeline beginning at pipe for an error
// message and returns its function name, when there is one.
func traceQLPipelineDetail(s string, pipe int) (detail, name string) {
	rest := strings.TrimSpace(s[pipe+1:])
	if rest == "" {
		return "|", ""
	}
	i := 0
	for i < len(rest) && isTraceQLIdentifierByte(rest[i]) {
		i++
	}
	if i == 0 {
		return "| " + firstTraceQLToken(rest), ""
	}
	name = rest[:i]
	detail = "| " + name
	if i < len(rest) && rest[i] == '(' {
		detail += "()"
	}
	return detail, name
}

func firstTraceQLToken(s string) string {
	for i := 0; i < len(s); i++ {
		if s[i] == ' ' || s[i] == '\t' || s[i] == '(' {
			return s[:i]
		}
	}
	return s
}

func traceQLPipelineOrMetricsError(detail, name string) error {
	if traceQLMetricsFunctions[name] {
		return traceQLMetricsError(detail)
	}
	return traceQLPipelineError(detail)
}

func traceQLStructuralOperatorError(op string) error {
	return fmt.Errorf("tempo: TraceQL structural operator %q is not supported; %s", op, traceQLSupportedSubset)
}

func traceQLMultipleSpansetsError(operator string) error {
	if operator == "" {
		return fmt.Errorf("tempo: TraceQL multiple spansets are not supported; %s", traceQLSupportedSubset)
	}
	return fmt.Errorf("tempo: TraceQL multiple spansets are not supported (found %q between spansets); %s", operator, traceQLSupportedSubset)
}

func traceQLPipelineError(detail string) error {
	return fmt.Errorf("tempo: TraceQL pipeline %q is not supported; %s", detail, traceQLSupportedSubset)
}

func traceQLMetricsError(detail string) error {
	return fmt.Errorf("tempo: TraceQL metrics query %q is not supported; %s", detail, traceQLSupportedSubset)
}

func traceQLGroupingError(token string) error {
	return fmt.Errorf("tempo: TraceQL grouping, functions, and aggregations are not supported (found %q); %s", token, traceQLSupportedSubset)
}
