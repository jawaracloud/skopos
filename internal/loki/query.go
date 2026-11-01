package loki

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// Parse parses a stream selector with optional line filters.
func Parse(query string) (Query, error) {
	query = strings.TrimSpace(query)
	if query == "" {
		return Query{}, fmt.Errorf("loki: query is required")
	}
	open := strings.IndexByte(query, '{')
	if open < 0 {
		return Query{}, fmt.Errorf("loki: query must start with a stream selector")
	}
	close, err := matchingBrace(query, open)
	if err != nil {
		return Query{}, err
	}
	selector := strings.TrimSpace(query[open+1 : close])
	out := Query{}
	if selector != "" {
		for _, raw := range splitOutside(selector, ",") {
			matcher, err := parseMatcher(raw)
			if err != nil {
				return Query{}, err
			}
			out.Matchers = append(out.Matchers, matcher)
		}
	}
	tail := strings.TrimSpace(query[close+1:])
	for tail != "" {
		switch {
		case strings.HasPrefix(tail, "|="):
			filter, rest, err := parseLineFilter(tail, "|=")
			if err != nil {
				return Query{}, err
			}
			out.Filters = append(out.Filters, filter)
			tail = rest
		case strings.HasPrefix(tail, "!="):
			filter, rest, err := parseLineFilter(tail, "!=")
			if err != nil {
				return Query{}, err
			}
			out.Filters = append(out.Filters, filter)
			tail = rest
		case strings.HasPrefix(tail, "|~"):
			filter, rest, err := parseLineFilter(tail, "|~")
			if err != nil {
				return Query{}, err
			}
			out.Filters = append(out.Filters, filter)
			tail = rest
		case strings.HasPrefix(tail, "!~"):
			filter, rest, err := parseLineFilter(tail, "!~")
			if err != nil {
				return Query{}, err
			}
			out.Filters = append(out.Filters, filter)
			tail = rest
		case strings.HasPrefix(tail, "|"):
			return Query{}, fmt.Errorf("loki: parser stages and metric LogQL queries are not supported")
		default:
			return Query{}, fmt.Errorf("loki: unexpected query suffix %q", tail)
		}
	}
	return out, nil
}

func parseMatcher(raw string) (Matcher, error) {
	raw = strings.TrimSpace(raw)
	op, index := findOperator(raw)
	if index <= 0 {
		return Matcher{}, fmt.Errorf("loki: invalid stream matcher %q", raw)
	}
	name := strings.TrimSpace(raw[:index])
	value := strings.TrimSpace(raw[index+len(op):])
	if name == "" {
		return Matcher{}, fmt.Errorf("loki: stream matcher has no label name")
	}
	unquoted, err := unquote(value)
	if err != nil {
		return Matcher{}, fmt.Errorf("loki: matcher value %q: %w", value, err)
	}
	m := Matcher{Name: NormalizeLabelName(name), Op: op, Value: unquoted}
	if op == "=~" || op == "!~" {
		re, err := regexp.Compile(unquoted)
		if err != nil {
			return Matcher{}, fmt.Errorf("loki: invalid regex %q: %w", unquoted, err)
		}
		m.re = re
	}
	return m, nil
}

func parseLineFilter(tail, op string) (LineFilter, string, error) {
	rest := strings.TrimSpace(tail[len(op):])
	if rest == "" || (rest[0] != '"' && rest[0] != '\'') {
		return LineFilter{}, "", fmt.Errorf("loki: %s must be followed by a quoted string", op)
	}
	value, consumed, err := readQuoted(rest)
	if err != nil {
		return LineFilter{}, "", err
	}
	filter := LineFilter{Op: op, Value: value}
	if op == "|~" || op == "!~" {
		re, err := regexp.Compile(value)
		if err != nil {
			return LineFilter{}, "", fmt.Errorf("loki: invalid regex %q: %w", value, err)
		}
		filter.re = re
	}
	return filter, strings.TrimSpace(rest[consumed:]), nil
}

func findOperator(s string) (string, int) {
	operators := []string{"=~", "!~", "!=", "="}
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

func matchingBrace(s string, open int) (int, error) {
	inQuote := byte(0)
	for i := open + 1; i < len(s); i++ {
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
		if s[i] == '}' {
			return i, nil
		}
	}
	return 0, fmt.Errorf("loki: unterminated stream selector")
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

func readQuoted(s string) (string, int, error) {
	quote := s[0]
	for i := 1; i < len(s); i++ {
		if s[i] == '\\' {
			i++
			continue
		}
		if s[i] == quote {
			value, err := unquote(s[:i+1])
			return value, i + 1, err
		}
	}
	return "", 0, fmt.Errorf("loki: unterminated quoted string")
}
