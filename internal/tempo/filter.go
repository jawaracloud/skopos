package tempo

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Filter is a TraceQL subset or a tag-search filter.
type Filter interface {
	Match(Span) bool
}

type andFilter []Filter
type orFilter []Filter

func (f andFilter) Match(span Span) bool {
	for _, child := range f {
		if !child.Match(span) {
			return false
		}
	}
	return true
}

func (f orFilter) Match(span Span) bool {
	for _, child := range f {
		if child.Match(span) {
			return true
		}
	}
	return false
}

type condition struct {
	scope    string
	key      string
	op       string
	value    string
	number   *float64
	duration *time.Duration
	regex    *regexp.Regexp
}

func (c condition) Match(span Span) bool {
	if c.key == "duration" || c.key == "span.duration" || c.key == "traceDuration" || c.key == "trace:duration" {
		return matchNumber(float64(span.Duration), c)
	}
	actual, ok := SpanTag(span, c.key)
	if !ok {
		return false
	}
	if c.key == "status" || c.key == "span.status" || c.key == "kind" || c.key == "span.kind" {
		actual = strings.ToLower(actual)
		c.value = strings.ToLower(c.value)
	}
	switch c.op {
	case "=", "contains":
		if c.op == "contains" {
			return strings.Contains(strings.ToLower(actual), strings.ToLower(c.value))
		}
		return actual == c.value
	case "!=":
		return actual != c.value
	case "=~":
		return c.regex != nil && c.regex.MatchString(actual)
	case "!~":
		return c.regex == nil || !c.regex.MatchString(actual)
	case ">", "<", ">=", "<=":
		actualNumber, err := strconv.ParseFloat(actual, 64)
		if err != nil {
			return false
		}
		return matchNumber(actualNumber, c)
	}
	return false
}

func matchNumber(actual float64, c condition) bool {
	want := 0.0
	switch {
	case c.duration != nil:
		want = float64(*c.duration)
	case c.number != nil:
		want = *c.number
	default:
		return false
	}
	switch c.op {
	case "=":
		return actual == want
	case "!=":
		return actual != want
	case ">":
		return actual > want
	case "<":
		return actual < want
	case ">=":
		return actual >= want
	case "<=":
		return actual <= want
	}
	return false
}

// ParseTraceQL parses the alpha TraceQL subset. It supports a single spanset
// with &&/|| and the operators =, !=, =~, !~, >, <, >=, <=. Structural
// operators, pipelines, and trace-level intrinsics are rejected.
func ParseTraceQL(query string) (Filter, error) {
	query = strings.TrimSpace(query)
	if query == "" {
		return nil, nil
	}
	if !strings.HasPrefix(query, "{") || !strings.HasSuffix(query, "}") {
		return nil, fmt.Errorf("tempo: TraceQL must be a single { ... } spanset")
	}
	body := strings.TrimSpace(query[1 : len(query)-1])
	if body == "" {
		return nil, nil
	}
	if strings.ContainsAny(body, "{}") {
		return nil, fmt.Errorf("tempo: multiple spansets are not supported")
	}
	orParts := splitOutside(body, "||")
	orFilters := make(orFilter, 0, len(orParts))
	for _, orPart := range orParts {
		andParts := splitOutside(orPart, "&&")
		andFilters := make(andFilter, 0, len(andParts))
		for _, part := range andParts {
			condition, err := parseCondition(part)
			if err != nil {
				return nil, err
			}
			andFilters = append(andFilters, condition)
		}
		orFilters = append(orFilters, andFilters)
	}
	if len(orFilters) == 1 {
		return orFilters[0], nil
	}
	return orFilters, nil
}

// ParseTags parses Tempo's logfmt-like tags search. Values are matched as
// case-insensitive substrings.
func ParseTags(tags string) (Filter, error) {
	tags = strings.TrimSpace(tags)
	if tags == "" {
		return nil, nil
	}
	parts, err := splitSpaces(tags)
	if err != nil {
		return nil, err
	}
	filters := make(andFilter, 0, len(parts))
	for _, part := range parts {
		idx := strings.IndexByte(part, '=')
		if idx <= 0 {
			return nil, fmt.Errorf("tempo: invalid tags value %q", part)
		}
		key := strings.TrimSpace(part[:idx])
		value := strings.TrimSpace(part[idx+1:])
		if unquoted, err := unquote(value); err == nil {
			value = unquoted
		}
		scope, tag := tagScope(key)
		filters = append(filters, condition{scope: scope, key: tagWithScope(scope, tag), op: "contains", value: value})
	}
	return filters, nil
}

func parseCondition(raw string) (condition, error) {
	raw = strings.TrimSpace(raw)
	op, index := findOperator(raw)
	if index < 0 {
		return condition{}, fmt.Errorf("tempo: TraceQL condition %q has no operator", raw)
	}
	key := strings.TrimSpace(raw[:index])
	value := strings.TrimSpace(raw[index+len(op):])
	if key == "" || value == "" {
		return condition{}, fmt.Errorf("tempo: incomplete TraceQL condition %q", raw)
	}
	scope, key := tagScope(key)
	c := condition{scope: scope, key: tagWithScope(scope, key), op: op}
	if op == "=~" || op == "!~" {
		pattern := value
		if unquoted, err := unquote(value); err == nil {
			pattern = unquoted
		}
		re, err := regexp.Compile("^(?:" + pattern + ")$")
		if err != nil {
			return condition{}, fmt.Errorf("tempo: invalid regex %q: %w", pattern, err)
		}
		c.value = pattern
		c.regex = re
		return c, nil
	}
	if unquoted, err := unquote(value); err == nil {
		c.value = unquoted
		return c, nil
	}
	if d, err := time.ParseDuration(value); err == nil && (key == "duration" || key == "span.duration" || op == ">" || op == "<" || op == ">=" || op == "<=") {
		c.duration = &d
		c.value = value
		return c, nil
	}
	if n, err := strconv.ParseFloat(value, 64); err == nil {
		c.number = &n
		c.value = value
		return c, nil
	}
	c.value = value
	return c, nil
}

func tagWithScope(scope, key string) string {
	switch scope {
	case "resource":
		return "resource." + key
	case "span":
		return "span." + key
	case "intrinsic":
		return key
	default:
		return key
	}
}

func findOperator(s string) (string, int) {
	operators := []string{"=~", "!~", "!=", ">=", "<=", "=", ">", "<"}
	inQuote := byte(0)
	for i := 0; i < len(s); i++ {
		if inQuote != 0 {
			if s[i] == '\\' {
				i++
				continue
			}
			if s[i] == inQuote {
				inQuote = 0
			}
			continue
		}
		if s[i] == '"' || s[i] == '\'' {
			inQuote = s[i]
			continue
		}
		for _, op := range operators {
			if strings.HasPrefix(s[i:], op) {
				return op, i
			}
		}
	}
	return "", -1
}

func splitOutside(s, separator string) []string {
	var out []string
	start := 0
	inQuote := byte(0)
	for i := 0; i < len(s); i++ {
		if inQuote != 0 {
			if s[i] == '\\' {
				i++
				continue
			}
			if s[i] == inQuote {
				inQuote = 0
			}
			continue
		}
		if s[i] == '"' || s[i] == '\'' {
			inQuote = s[i]
			continue
		}
		if strings.HasPrefix(s[i:], separator) {
			out = append(out, strings.TrimSpace(s[start:i]))
			i += len(separator) - 1
			start = i + 1
		}
	}
	out = append(out, strings.TrimSpace(s[start:]))
	return out
}

func splitSpaces(s string) ([]string, error) {
	var out []string
	start := -1
	inQuote := byte(0)
	for i := 0; i < len(s); i++ {
		if inQuote != 0 {
			if s[i] == '\\' {
				i++
				continue
			}
			if s[i] == inQuote {
				inQuote = 0
			}
			continue
		}
		if s[i] == '"' || s[i] == '\'' {
			if start < 0 {
				start = i
			}
			inQuote = s[i]
			continue
		}
		if s[i] == ' ' || s[i] == '\t' {
			if start >= 0 {
				out = append(out, s[start:i])
				start = -1
			}
			continue
		}
		if start < 0 {
			start = i
		}
	}
	if inQuote != 0 {
		return nil, fmt.Errorf("tempo: unterminated quote in tags")
	}
	if start >= 0 {
		out = append(out, s[start:])
	}
	return out, nil
}

func unquote(s string) (string, error) {
	if len(s) < 2 {
		return "", fmt.Errorf("not quoted")
	}
	quote := s[0]
	if (quote != '"' && quote != '\'') || s[len(s)-1] != quote {
		return "", fmt.Errorf("not quoted")
	}
	if quote == '"' {
		return strconv.Unquote(s)
	}
	return strings.ReplaceAll(s[1:len(s)-1], `\'`, `'`), nil
}
