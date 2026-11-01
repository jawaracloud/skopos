package loki

import (
	"fmt"
	"math"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Parse parses the supported LogQL subset: a stream selector with optional
// pipeline stages, or a count_over_time/rate metric query with an optional
// sum aggregation.
func Parse(query string) (Query, error) {
	query = strings.TrimSpace(query)
	if query == "" {
		return Query{}, fmt.Errorf("loki: query is required")
	}
	if strings.HasPrefix(query, "{") {
		return parseLogQuery(query)
	}
	return parseMetricQuery(query)
}

func parseLogQuery(query string) (Query, error) {
	query = strings.TrimSpace(query)
	open := strings.IndexByte(query, '{')
	if open != 0 {
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
	if err := parsePipeline(strings.TrimSpace(query[close+1:]), &out); err != nil {
		return Query{}, err
	}
	return out, nil
}

var unsupportedParserStages = map[string]struct{}{
	"pattern": {}, "regexp": {}, "unpack": {}, "ip": {}, "pack": {},
	"json_parser": {}, "logfmt_parser": {},
}

var unsupportedPipelineStages = map[string]struct{}{
	"line_format": {}, "label_format": {}, "drop": {}, "keep": {},
	"decolorize": {}, "unwrap": {}, "label_filter": {}, "parser": {},
	"match": {}, "limit": {},
}

func parsePipeline(tail string, out *Query) error {
	for {
		tail = strings.TrimSpace(tail)
		if tail == "" {
			return nil
		}
		switch {
		case strings.HasPrefix(tail, "|="), strings.HasPrefix(tail, "|~"):
			op := tail[:2]
			filter, rest, err := parseLineFilter(tail, op)
			if err != nil {
				return err
			}
			out.Stages = append(out.Stages, filter)
			tail = rest
		case strings.HasPrefix(tail, "!="), strings.HasPrefix(tail, "!~"):
			op := tail[:2]
			filter, rest, err := parseLineFilter(tail, op)
			if err != nil {
				return err
			}
			out.Stages = append(out.Stages, filter)
			tail = rest
		case strings.HasPrefix(tail, "|"):
			stageText := strings.TrimSpace(tail[1:])
			name, rest := readIdentifier(stageText)
			if name == "" {
				return fmt.Errorf("loki: pipeline stage has no name")
			}
			switch name {
			case "json":
				stage, after, err := parseJSONStage(rest)
				if err != nil {
					return err
				}
				out.Stages = append(out.Stages, stage)
				tail = after
			case "logfmt":
				after := strings.TrimSpace(rest)
				if after != "" && !strings.HasPrefix(after, "|") {
					if strings.HasPrefix(after, "--") {
						return fmt.Errorf("loki: | logfmt flags are not supported")
					}
					return fmt.Errorf("loki: | logfmt takes no arguments")
				}
				out.Stages = append(out.Stages, ParserStage{Kind: "logfmt"})
				tail = rest
			default:
				if _, ok := unsupportedParserStages[name]; ok {
					return fmt.Errorf("loki: unsupported parser stage %q", name)
				}
				if _, ok := unsupportedPipelineStages[name]; ok {
					return fmt.Errorf("loki: unsupported pipeline stage %q", name)
				}
				filter, after, err := parseLabelFilter(name, rest)
				if err != nil {
					return err
				}
				out.Stages = append(out.Stages, filter)
				tail = after
			}
		default:
			return fmt.Errorf("loki: unexpected query suffix %q", tail)
		}
	}
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
		re, err := regexp.Compile("^(?:" + unquoted + ")$")
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

func parseJSONStage(rest string) (ParserStage, string, error) {
	rest = strings.TrimSpace(rest)
	stage := ParserStage{Kind: "json"}
	if rest == "" || strings.HasPrefix(rest, "|") {
		return stage, rest, nil
	}
	if strings.HasPrefix(rest, "--") {
		return ParserStage{}, "", fmt.Errorf("loki: | json flags are not supported")
	}
	for {
		rest = strings.TrimSpace(rest)
		if rest == "" || strings.HasPrefix(rest, "|") {
			return stage, rest, nil
		}
		label, after := readIdentifier(rest)
		if label == "" {
			return ParserStage{}, "", fmt.Errorf("loki: expected a label name in | json")
		}
		if !validLabelName(label) {
			return ParserStage{}, "", fmt.Errorf("loki: invalid label name %q in | json", label)
		}
		after = strings.TrimSpace(after)
		if after == "" || after[0] != '=' {
			return ParserStage{}, "", fmt.Errorf("loki: | json label %q must be followed by =\"path\"", label)
		}
		after = strings.TrimSpace(after[1:])
		if after == "" || (after[0] != '"' && after[0] != '\'') {
			return ParserStage{}, "", fmt.Errorf("loki: | json label %q must have a quoted path", label)
		}
		path, consumed, err := readQuoted(after)
		if err != nil {
			return ParserStage{}, "", err
		}
		segments, err := parseJSONPath(path)
		if err != nil {
			return ParserStage{}, "", err
		}
		stage.Extractions = append(stage.Extractions, Extraction{Label: label, Path: segments})
		rest = strings.TrimSpace(after[consumed:])
		if rest == "" || strings.HasPrefix(rest, "|") {
			return stage, rest, nil
		}
		if rest[0] != ',' {
			return ParserStage{}, "", fmt.Errorf("loki: unexpected text %q after | json extraction", rest)
		}
		rest = rest[1:]
	}
}

func parseJSONPath(path string) ([]string, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return nil, fmt.Errorf("loki: | json extraction path is empty")
	}
	if strings.ContainsAny(path, "[]*?") {
		return nil, fmt.Errorf("loki: unsupported | json path %q", path)
	}
	parts := strings.Split(path, ".")
	for _, part := range parts {
		if part == "" {
			return nil, fmt.Errorf("loki: invalid | json path %q", path)
		}
	}
	return parts, nil
}

func parseLabelFilter(name, rest string) (LabelFilter, string, error) {
	if !validLabelName(name) {
		return LabelFilter{}, "", fmt.Errorf("loki: invalid label filter name %q", name)
	}
	rest = strings.TrimSpace(rest)
	op, index := findLabelOperator(rest)
	if index != 0 {
		return LabelFilter{}, "", fmt.Errorf("loki: label filter %q has no comparison operator", name)
	}
	after := strings.TrimSpace(rest[len(op):])
	if after == "" {
		return LabelFilter{}, "", fmt.Errorf("loki: label filter %q has no value", name)
	}
	var value string
	var consumed int
	if after[0] == '"' || after[0] == '\'' {
		if op == ">" || op == ">=" || op == "<" || op == "<=" {
			return LabelFilter{}, "", fmt.Errorf("loki: label filter %q requires an unquoted number", name)
		}
		var err error
		value, consumed, err = readQuoted(after)
		if err != nil {
			return LabelFilter{}, "", err
		}
	} else {
		start := 0
		for start < len(after) && !isSpace(after[start]) && after[start] != '|' {
			start++
		}
		value = after[:start]
		consumed = start
		if op == "=~" || op == "!~" {
			return LabelFilter{}, "", fmt.Errorf("loki: label filter %q must compare against a quoted string", name)
		}
		if _, err := strconv.ParseFloat(value, 64); err != nil {
			return LabelFilter{}, "", fmt.Errorf("loki: label filter %q requires a number or quoted string, got %q", name, value)
		}
	}
	filter, err := newLabelFilter(name, op, value)
	if err != nil {
		return LabelFilter{}, "", err
	}
	return filter, strings.TrimSpace(after[consumed:]), nil
}

func newLabelFilter(name, op, value string) (LabelFilter, error) {
	filter := LabelFilter{Name: name, Op: op, Value: value}
	switch op {
	case "=~", "!~":
		re, err := regexp.Compile("^(?:" + value + ")$")
		if err != nil {
			return LabelFilter{}, fmt.Errorf("loki: invalid label filter regex %q: %w", value, err)
		}
		filter.re = re
	case ">", ">=", "<", "<=":
		number, err := strconv.ParseFloat(strings.TrimSpace(value), 64)
		if err != nil || math.IsNaN(number) || math.IsInf(number, 0) {
			return LabelFilter{}, fmt.Errorf("loki: label filter %q requires a numeric value, got %q", name, value)
		}
		filter.numeric = true
		filter.number = number
	case "=", "!=":
	default:
		return LabelFilter{}, fmt.Errorf("loki: unsupported label filter operator %q", op)
	}
	return filter, nil
}

func parseLabelNumber(value string) (float64, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return 0, fmt.Errorf("empty label value")
	}
	number, err := strconv.ParseFloat(value, 64)
	if err != nil || math.IsNaN(number) || math.IsInf(number, 0) {
		return 0, fmt.Errorf("non-numeric label value %q", value)
	}
	return number, nil
}

func findLabelOperator(s string) (string, int) {
	operators := []string{"=~", "!~", "!=", ">=", "<=", "=", ">", "<"}
	for _, op := range operators {
		if strings.HasPrefix(s, op) {
			return op, 0
		}
	}
	return "", -1
}

func parseMetricQuery(text string) (Query, error) {
	text = strings.TrimSpace(text)
	name, rest := readIdentifier(text)
	if name == "" {
		return Query{}, fmt.Errorf("loki: query must start with a stream selector or a metric function")
	}
	switch name {
	case "count_over_time", "rate":
		query, after, err := parseMetricCall(name, rest)
		if err != nil {
			return Query{}, err
		}
		if after = strings.TrimSpace(after); after != "" {
			return Query{}, fmt.Errorf("loki: unexpected query suffix %q", after)
		}
		return query, nil
	case "sum":
		return parseSumMetric(rest)
	default:
		return Query{}, fmt.Errorf("loki: unsupported metric function %q", name)
	}
}

func parseSumMetric(rest string) (Query, error) {
	rest = strings.TrimSpace(rest)
	var groupBy []string
	if hasWordPrefix(rest, "by") {
		rest = strings.TrimSpace(rest[2:])
		if rest == "" || rest[0] != '(' {
			return Query{}, fmt.Errorf("loki: sum by must be followed by a label list")
		}
		close, err := matchingParen(rest, 0)
		if err != nil {
			return Query{}, err
		}
		labels := strings.TrimSpace(rest[1:close])
		groupBy = make([]string, 0)
		if labels != "" {
			seen := make(map[string]struct{})
			for _, label := range splitOutside(labels, ",") {
				label = strings.TrimSpace(label)
				if !validLabelName(label) {
					return Query{}, fmt.Errorf("loki: invalid grouping label %q", label)
				}
				if _, ok := seen[label]; ok {
					return Query{}, fmt.Errorf("loki: duplicate grouping label %q", label)
				}
				seen[label] = struct{}{}
				groupBy = append(groupBy, label)
			}
		}
		rest = strings.TrimSpace(rest[close+1:])
	}
	if rest == "" || rest[0] != '(' {
		return Query{}, fmt.Errorf("loki: sum must be followed by a metric expression")
	}
	close, err := matchingParen(rest, 0)
	if err != nil {
		return Query{}, err
	}
	inner := strings.TrimSpace(rest[1:close])
	innerName, innerRest := readIdentifier(inner)
	if innerName != "count_over_time" && innerName != "rate" {
		return Query{}, fmt.Errorf("loki: unsupported aggregate expression %q inside sum", innerName)
	}
	query, after, err := parseMetricCall(innerName, innerRest)
	if err != nil {
		return Query{}, err
	}
	if after = strings.TrimSpace(after); after != "" {
		return Query{}, fmt.Errorf("loki: unexpected text %q inside sum", after)
	}
	query.Metric.Aggregate = true
	query.Metric.GroupBy = groupBy
	if trailing := strings.TrimSpace(rest[close+1:]); trailing != "" {
		return Query{}, fmt.Errorf("loki: unexpected query suffix %q", trailing)
	}
	return query, nil
}

func parseMetricCall(name, rest string) (Query, string, error) {
	rest = strings.TrimSpace(rest)
	if rest == "" || rest[0] != '(' {
		return Query{}, "", fmt.Errorf("loki: %s must be followed by a stream selector and range", name)
	}
	close, err := matchingParen(rest, 0)
	if err != nil {
		return Query{}, "", err
	}
	inner := strings.TrimSpace(rest[1:close])
	source, rangeText, err := splitRangeSelector(inner)
	if err != nil {
		return Query{}, "", err
	}
	rng, err := parseRangeDuration(rangeText)
	if err != nil {
		return Query{}, "", fmt.Errorf("loki: invalid range [%s]: %w", rangeText, err)
	}
	query, err := parseLogQuery(source)
	if err != nil {
		return Query{}, "", err
	}
	query.Metric = &MetricQuery{Function: name, Range: rng}
	return query, rest[close+1:], nil
}

func splitRangeSelector(inner string) (string, string, error) {
	inQuote := byte(0)
	open := -1
	for i := 0; i < len(inner); i++ {
		if inQuote != 0 {
			if inner[i] == '\\' {
				i++
				continue
			}
			if inner[i] == inQuote {
				inQuote = 0
			}
			continue
		}
		if inner[i] == '"' || inner[i] == '\'' {
			inQuote = inner[i]
			continue
		}
		if inner[i] == '[' {
			open = i
		}
	}
	if open < 0 {
		return "", "", fmt.Errorf("loki: range selector [duration] is required")
	}
	source := strings.TrimSpace(inner[:open])
	rest := strings.TrimSpace(inner[open+1:])
	if !strings.HasSuffix(rest, "]") {
		return "", "", fmt.Errorf("loki: unterminated range selector")
	}
	rangeText := strings.TrimSpace(rest[:len(rest)-1])
	if source == "" {
		return "", "", fmt.Errorf("loki: metric query has no stream selector")
	}
	if rangeText == "" {
		return "", "", fmt.Errorf("loki: range selector is empty")
	}
	if strings.ContainsAny(rangeText, "[]") {
		return "", "", fmt.Errorf("loki: invalid range selector [%s]", rangeText)
	}
	return source, rangeText, nil
}

func parseRangeDuration(value string) (time.Duration, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return 0, fmt.Errorf("duration is empty")
	}
	var total time.Duration
	for i := 0; i < len(value); {
		start := i
		for i < len(value) && value[i] >= '0' && value[i] <= '9' {
			i++
		}
		if start == i {
			return 0, fmt.Errorf("expected a number at %q", value[start:])
		}
		n, err := strconv.ParseInt(value[start:i], 10, 64)
		if err != nil {
			return 0, fmt.Errorf("invalid number %q", value[start:i])
		}
		var unit time.Duration
		switch {
		case strings.HasPrefix(value[i:], "ms"):
			unit = time.Millisecond
			i += 2
		case i < len(value):
			switch value[i] {
			case 's':
				unit = time.Second
			case 'm':
				unit = time.Minute
			case 'h':
				unit = time.Hour
			case 'd':
				unit = 24 * time.Hour
			case 'w':
				unit = 7 * 24 * time.Hour
			case 'y':
				unit = 365 * 24 * time.Hour
			default:
				return 0, fmt.Errorf("unknown duration unit %q", string(value[i]))
			}
			i++
		default:
			return 0, fmt.Errorf("duration %q has no unit", value)
		}
		if n > int64(math.MaxInt64)/int64(unit) {
			return 0, fmt.Errorf("duration %q overflows", value)
		}
		total += time.Duration(n) * unit
	}
	if total <= 0 {
		return 0, fmt.Errorf("duration must be positive")
	}
	return total, nil
}

func hasWordPrefix(s, word string) bool {
	if !strings.HasPrefix(s, word) {
		return false
	}
	if len(s) == len(word) {
		return true
	}
	switch s[len(word)] {
	case ' ', '\t', '\r', '\n', '(':
		return true
	default:
		return false
	}
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

func matchingParen(s string, open int) (int, error) {
	if open < 0 || open >= len(s) || s[open] != '(' {
		return 0, fmt.Errorf("loki: expected (")
	}
	depth := 0
	inQuote := byte(0)
	for i := open; i < len(s); i++ {
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
		switch s[i] {
		case '(':
			depth++
		case ')':
			depth--
			if depth == 0 {
				return i, nil
			}
		}
	}
	return 0, fmt.Errorf("loki: unterminated parenthesized expression")
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

func readIdentifier(s string) (string, string) {
	i := 0
	for i < len(s) {
		c := s[i]
		if (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || c == '_' || (i > 0 && c >= '0' && c <= '9') {
			i++
			continue
		}
		break
	}
	return s[:i], s[i:]
}

func validLabelName(name string) bool {
	if name == "" {
		return false
	}
	for i, c := range name {
		if (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || c == '_' {
			continue
		}
		if i > 0 && c >= '0' && c <= '9' {
			continue
		}
		return false
	}
	return true
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
