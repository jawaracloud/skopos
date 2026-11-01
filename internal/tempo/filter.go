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

// traceFilter is implemented by filters that need trace-level intrinsics.
// The exported Filter interface is unchanged so existing callers keep working.
type traceFilter interface {
	matchTrace(Span, TraceInfo) bool
}

type andFilter []Filter
type orFilter []Filter

func (f andFilter) Match(span Span) bool {
	return f.matchTrace(span, TraceInfoFor(span.TraceID, []Span{span}))
}

func (f andFilter) matchTrace(span Span, info TraceInfo) bool {
	for _, child := range f {
		if !matchTrace(child, span, info) {
			return false
		}
	}
	return true
}

func (f orFilter) Match(span Span) bool {
	return f.matchTrace(span, TraceInfoFor(span.TraceID, []Span{span}))
}

func (f orFilter) matchTrace(span Span, info TraceInfo) bool {
	for _, child := range f {
		if matchTrace(child, span, info) {
			return true
		}
	}
	return false
}

func matchTrace(filter Filter, span Span, info TraceInfo) bool {
	if tf, ok := filter.(traceFilter); ok {
		return tf.matchTrace(span, info)
	}
	return filter.Match(span)
}

type condition struct {
	// scope is "", "resource", "span", "event", "link", "trace",
	// "instrumentation", or "intrinsic".
	scope string
	key   string
	// intrinsic distinguishes span:name (intrinsic) from span.name (attribute).
	intrinsic bool
	// explicit marks an unscoped attribute written as ".fx" (no intrinsic
	// fallback).
	explicit bool
	// enum marks span:status and span:kind comparisons. Values are compared
	// case-insensitively as Tempo enums.
	enum     bool
	op       string
	value    string
	number   *float64
	duration *time.Duration
	regex    *regexp.Regexp
	// quoted records a string literal (including quoted numeric strings), which
	// enables Tempo's lexicographic comparison for ">" and "<".
	quoted   bool
	nilValue bool
}

func (c condition) Match(span Span) bool {
	return c.matchTrace(span, TraceInfoFor(span.TraceID, []Span{span}))
}

func (c condition) matchTrace(span Span, info TraceInfo) bool {
	switch c.scope {
	case "event":
		return c.matchEvents(span)
	case "link":
		return c.matchLinks(span)
	}
	actual, ok := c.actual(span, info)
	if !ok {
		// Tempo evaluates a condition on a missing attribute as false for every
		// operator, including != and !~. The nil extension makes absence
		// explicit with "= nil" and "!= nil".
		if c.nilValue {
			return c.op == "="
		}
		return false
	}
	if c.nilValue {
		return c.op == "!="
	}
	return c.compare(actual)
}

func (c condition) matchEvents(span Span) bool {
	values := eventValues(span, c)
	if len(values) == 0 {
		if c.nilValue {
			return c.op == "="
		}
		return false
	}
	for _, value := range values {
		if c.nilValue {
			return c.op == "!="
		}
		if c.compare(value) {
			return true
		}
	}
	return false
}

func (c condition) matchLinks(span Span) bool {
	values := linkValues(span, c)
	if len(values) == 0 {
		if c.nilValue {
			return c.op == "="
		}
		return false
	}
	for _, value := range values {
		if c.nilValue {
			return c.op == "!="
		}
		if c.compare(value) {
			return true
		}
	}
	return false
}

func eventValues(span Span, c condition) []value {
	out := make([]value, 0, len(span.Events))
	for _, event := range span.Events {
		switch {
		case c.intrinsic && c.key == "name":
			out = append(out, textValue(event.Name))
		case c.intrinsic && c.key == "timeSinceStart":
			out = append(out, numberValue(float64(event.Timestamp.Sub(span.Timestamp))))
		default:
			if attr, ok := event.Attributes[c.key]; ok {
				out = append(out, textAttributeValue(attr))
			}
		}
	}
	return out
}

func linkValues(span Span, c condition) []value {
	out := make([]value, 0, len(span.Links))
	for _, link := range span.Links {
		switch {
		case c.intrinsic && c.key == "traceID":
			out = append(out, identifierTagValue(link.TraceID))
		case c.intrinsic && c.key == "spanID":
			out = append(out, identifierTagValue(link.SpanID))
		case c.intrinsic && c.key == "traceState":
			out = append(out, textValue(link.TraceState))
		default:
			if attr, ok := link.Attributes[c.key]; ok {
				out = append(out, textAttributeValue(attr))
			}
		}
	}
	return out
}

// value is one candidate value for a condition. Numeric intrinsics set numeric;
// attributes are strings but are coerced when compared against a number.
type value struct {
	text    string
	number  float64
	numeric bool
	enum    bool
}

func textValue(s string) value    { return value{text: s} }
func numberValue(n float64) value { return value{text: formatNumber(n), number: n, numeric: true} }
func enumValue(s string) value    { return value{text: s, enum: true} }
func identifierTagValue(s string) value {
	return value{text: strings.ToLower(s), enum: true}
}
func textAttributeValue(s string) value { return value{text: s} }

func (v value) asNumber() (float64, bool) {
	if v.numeric {
		return v.number, true
	}
	n, err := strconv.ParseFloat(v.text, 64)
	if err != nil {
		return 0, false
	}
	return n, true
}

func (c condition) compare(actual value) bool {
	if c.duration != nil {
		got, ok := actual.asNumber()
		if !ok {
			return false
		}
		return compareNumbers(got, float64(*c.duration), c.op)
	}
	if c.number != nil {
		got, ok := actual.asNumber()
		if !ok {
			return false
		}
		return compareNumbers(got, *c.number, c.op)
	}
	switch c.op {
	case "=", "contains":
		if c.op == "contains" {
			return strings.Contains(strings.ToLower(actual.text), strings.ToLower(c.value))
		}
		return equalText(actual, c.value)
	case "!=":
		return !equalText(actual, c.value)
	case "=~":
		return c.regex != nil && c.regex.MatchString(actual.text)
	case "!~":
		return c.regex != nil && !c.regex.MatchString(actual.text)
	case ">", "<", ">=", "<=":
		if c.quoted || actual.enum {
			return compareStrings(actual.text, c.value, c.op)
		}
		if got, ok := actual.asNumber(); ok {
			if want, err := strconv.ParseFloat(c.value, 64); err == nil {
				return compareNumbers(got, want, c.op)
			}
		}
		return compareStrings(actual.text, c.value, c.op)
	}
	return false
}

func equalText(actual value, want string) bool {
	if actual.enum {
		return strings.EqualFold(actual.text, want)
	}
	return actual.text == want
}

func compareNumbers(a, b float64, op string) bool {
	switch op {
	case "=":
		return a == b
	case "!=":
		return a != b
	case ">":
		return a > b
	case "<":
		return a < b
	case ">=":
		return a >= b
	case "<=":
		return a <= b
	}
	return false
}

func compareStrings(a, b, op string) bool {
	switch op {
	case "=":
		return a == b
	case "!=":
		return a != b
	case ">":
		return a > b
	case "<":
		return a < b
	case ">=":
		return a >= b
	case "<=":
		return a <= b
	}
	return false
}

func formatNumber(n float64) string {
	return strconv.FormatFloat(n, 'g', -1, 64)
}

// actual resolves the condition's key against a span. Trace-level keys use the
// trace context computed by Search. The second return value reports presence;
// a missing attribute is not a match for any operator.
func (c condition) actual(span Span, info TraceInfo) (value, bool) {
	switch c.scope {
	case "resource":
		return resourceValue(span, c.key)
	case "span":
		if c.intrinsic {
			return spanIntrinsic(span, c.key)
		}
		if v, ok := span.SpanAttributes[c.key]; ok {
			return textAttributeValue(v), true
		}
		// Older alpha builds accepted span.name/span.duration/span.status/
		// span.kind as intrinsic aliases. Keep that working when no attribute
		// with the same name exists.
		if v, ok := spanIntrinsic(span, c.key); ok {
			return v, true
		}
		return value{}, false
	case "trace":
		return traceIntrinsic(info, c.key)
	case "instrumentation":
		return instrumentationIntrinsic(span, c.key)
	case "intrinsic":
		return c.actualIntrinsic(span, info, c.key)
	case "event", "link":
		return value{}, false
	default:
		if !c.explicit {
			if v, ok := c.actualIntrinsic(span, info, c.key); ok {
				return v, true
			}
			if c.key == "service.name" {
				return resourceValue(span, "service.name")
			}
		}
		if v, ok := span.SpanAttributes[c.key]; ok {
			return textAttributeValue(v), true
		}
		if v, ok := span.ResourceAttributes[c.key]; ok {
			return textAttributeValue(v), true
		}
		if c.key == "service.name" {
			return resourceValue(span, "service.name")
		}
		return value{}, false
	}
}

func (c condition) actualIntrinsic(span Span, info TraceInfo, key string) (value, bool) {
	switch key {
	case "name", "span:name":
		return textValue(span.SpanName), true
	case "duration", "span:duration":
		return numberValue(float64(span.Duration)), true
	case "status", "span:status":
		return enumValue(normalizeStatus(span.StatusCode)), true
	case "statusMessage", "span:statusMessage":
		return textValue(span.StatusMessage), true
	case "kind", "span:kind":
		return enumValue(normalizeKind(span.SpanKind)), true
	case "span:id":
		return identifierTagValue(span.SpanID), true
	case "span:parentID":
		return identifierTagValue(span.ParentSpanID), true
	case "trace:id":
		return identifierTagValue(info.TraceID), true
	case "trace:duration", "traceDuration":
		return numberValue(float64(info.Duration())), true
	case "trace:rootName", "rootName":
		if !info.HasRoot {
			return value{}, false
		}
		return textValue(info.RootName), true
	case "trace:rootService", "trace:rootServiceName", "rootService", "rootServiceName":
		if !info.HasRoot {
			return value{}, false
		}
		return textValue(info.RootServiceName), true
	case "event:name", "event:timeSinceStart", "link:traceID", "link:spanID", "link:traceState":
		return value{}, false
	}
	return value{}, false
}

func resourceValue(span Span, key string) (value, bool) {
	if key == "service.name" {
		if span.ServiceName != "" {
			return textValue(span.ServiceName), true
		}
	}
	v, ok := span.ResourceAttributes[key]
	if !ok {
		return value{}, false
	}
	return textAttributeValue(v), true
}

func spanIntrinsic(span Span, key string) (value, bool) {
	switch key {
	case "name":
		return textValue(span.SpanName), true
	case "duration":
		return numberValue(float64(span.Duration)), true
	case "status":
		return enumValue(normalizeStatus(span.StatusCode)), true
	case "statusMessage":
		return textValue(span.StatusMessage), true
	case "kind":
		return enumValue(normalizeKind(span.SpanKind)), true
	case "id":
		return identifierTagValue(span.SpanID), true
	case "parentID":
		return identifierTagValue(span.ParentSpanID), true
	case "traceID":
		return identifierTagValue(span.TraceID), true
	}
	return value{}, false
}

func traceIntrinsic(info TraceInfo, key string) (value, bool) {
	switch key {
	case "duration":
		return numberValue(float64(info.Duration())), true
	case "rootName":
		if !info.HasRoot {
			return value{}, false
		}
		return textValue(info.RootName), true
	case "rootService", "rootServiceName":
		if !info.HasRoot {
			return value{}, false
		}
		return textValue(info.RootServiceName), true
	case "id":
		return identifierTagValue(info.TraceID), true
	}
	return value{}, false
}

func instrumentationIntrinsic(span Span, key string) (value, bool) {
	switch key {
	case "name":
		return textValue(span.ScopeName), true
	case "version":
		return textValue(span.ScopeVersion), true
	}
	return value{}, false
}

func normalizeStatus(status string) string {
	status = strings.ToLower(strings.TrimSpace(status))
	status = strings.TrimPrefix(status, "status_code_")
	if status == "" {
		return "unset"
	}
	return status
}

func normalizeKind(kind string) string {
	kind = strings.ToLower(strings.TrimSpace(kind))
	kind = strings.TrimPrefix(kind, "span_kind_")
	if kind == "" {
		return "unspecified"
	}
	return kind
}

// ParseTraceQL parses the alpha TraceQL subset: one spanset with &&/|| and the
// operators =, !=, =~, !~, >, <, >=, <=. Structural operators, pipelines,
// metrics queries, multiple spansets, and aggregation are rejected with an
// explicit error.
func ParseTraceQL(query string) (Filter, error) {
	query = strings.TrimSpace(query)
	if query == "" {
		return nil, nil
	}
	if !strings.HasPrefix(query, "{") || !strings.HasSuffix(query, "}") {
		if strings.Contains(query, "|") {
			return nil, fmt.Errorf("tempo: TraceQL pipelines and metrics queries are not supported")
		}
		return nil, fmt.Errorf("tempo: TraceQL must be a single { ... } spanset")
	}
	body := strings.TrimSpace(query[1 : len(query)-1])
	if body == "" {
		return nil, nil
	}
	if strings.ContainsAny(body, "{}") {
		return nil, fmt.Errorf("tempo: multiple spansets and structural operators are not supported")
	}
	if hasPipeline(body) {
		return nil, fmt.Errorf("tempo: TraceQL pipelines are not supported")
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
		ref, err := parseKey(key)
		if err != nil {
			return nil, err
		}
		filters = append(filters, condition{
			scope:     ref.scope,
			key:       ref.key,
			intrinsic: ref.intrinsic,
			explicit:  ref.explicit,
			enum:      ref.enum,
			op:        "contains",
			value:     value,
		})
	}
	return filters, nil
}

type keyRef struct {
	scope     string
	key       string
	intrinsic bool
	explicit  bool
	enum      bool
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
	ref, err := parseKey(key)
	if err != nil {
		return condition{}, err
	}
	c := condition{
		scope:     ref.scope,
		key:       ref.key,
		intrinsic: ref.intrinsic,
		explicit:  ref.explicit,
		enum:      ref.enum,
		op:        op,
	}
	if value == "nil" {
		if op != "=" && op != "!=" {
			return condition{}, fmt.Errorf("tempo: nil only supports = and !=")
		}
		c.nilValue = true
		return c, nil
	}
	if op == "=~" || op == "!~" {
		pattern := value
		if unquoted, err := unquote(value); err == nil {
			pattern = unquoted
			c.quoted = true
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
		c.quoted = true
		return c, nil
	}
	if d, err := parseTraceQLDuration(value); err == nil {
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

func parseTraceQLDuration(value string) (time.Duration, error) {
	d, err := time.ParseDuration(value)
	if err != nil {
		return 0, err
	}
	return d, nil
}

func parseKey(raw string) (keyRef, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return keyRef{}, fmt.Errorf("tempo: empty TraceQL key")
	}
	if strings.HasPrefix(raw, ".") {
		key, err := unquoteKey(strings.TrimPrefix(raw, "."))
		if err != nil {
			return keyRef{}, err
		}
		return keyRef{key: key, explicit: true}, nil
	}
	if idx := strings.IndexByte(raw, ':'); idx >= 0 && !insideQuotes(raw, idx) {
		scope := strings.ToLower(raw[:idx])
		key, err := unquoteKey(raw[idx+1:])
		if err != nil {
			return keyRef{}, err
		}
		if key == "" {
			return keyRef{}, fmt.Errorf("tempo: empty intrinsic %q", raw)
		}
		ref := keyRef{scope: scope, key: key, intrinsic: true}
		switch scope {
		case "span":
			switch key {
			case "name", "duration", "status", "statusMessage", "kind", "id", "parentID", "traceID":
			default:
				return keyRef{}, fmt.Errorf("tempo: unsupported span intrinsic %q", key)
			}
			ref.enum = key == "status" || key == "kind"
		case "trace":
			switch key {
			case "duration", "rootName", "rootService", "rootServiceName", "id":
			default:
				return keyRef{}, fmt.Errorf("tempo: unsupported trace intrinsic %q", key)
			}
		case "event":
			switch key {
			case "name", "timeSinceStart":
			default:
				return keyRef{}, fmt.Errorf("tempo: unsupported event intrinsic %q", key)
			}
		case "link":
			key = canonicalLinkKey(key)
			switch key {
			case "traceID", "spanID", "traceState":
			default:
				return keyRef{}, fmt.Errorf("tempo: unsupported link intrinsic %q", key)
			}
		case "instrumentation":
			switch key {
			case "name", "version":
			default:
				return keyRef{}, fmt.Errorf("tempo: unsupported instrumentation intrinsic %q", key)
			}
		case "intrinsic":
			return parseIntrinsicAlias(key)
		default:
			return keyRef{}, fmt.Errorf("tempo: unsupported TraceQL scope %q", scope)
		}
		return ref, nil
	}

	lower := strings.ToLower(raw)
	for _, prefix := range []string{"resource.", "span.", "event.", "link.", "instrumentation."} {
		if strings.HasPrefix(lower, prefix) {
			key, err := unquoteKey(raw[len(prefix):])
			if err != nil {
				return keyRef{}, err
			}
			if key == "" {
				return keyRef{}, fmt.Errorf("tempo: empty attribute name in %q", raw)
			}
			scope := strings.TrimSuffix(prefix, ".")
			if scope == "instrumentation" {
				return keyRef{}, fmt.Errorf("tempo: instrumentation attributes are not supported")
			}
			return keyRef{scope: scope, key: key}, nil
		}
	}
	if strings.HasPrefix(lower, "intrinsic.") {
		key, err := unquoteKey(raw[len("intrinsic."):])
		if err != nil {
			return keyRef{}, err
		}
		return parseIntrinsicAlias(key)
	}
	if strings.Contains(raw, ":") {
		return keyRef{}, fmt.Errorf("tempo: unsupported TraceQL scope in %q", raw)
	}
	key, err := unquoteKey(raw)
	if err != nil {
		return keyRef{}, err
	}
	return parseIntrinsicAlias(key)
}

func parseIntrinsicAlias(key string) (keyRef, error) {
	switch strings.ToLower(key) {
	case "name":
		return keyRef{scope: "span", key: "name", intrinsic: true}, nil
	case "duration":
		return keyRef{scope: "span", key: "duration", intrinsic: true}, nil
	case "status":
		return keyRef{scope: "span", key: "status", intrinsic: true, enum: true}, nil
	case "statusmessage":
		return keyRef{scope: "span", key: "statusMessage", intrinsic: true}, nil
	case "kind":
		return keyRef{scope: "span", key: "kind", intrinsic: true, enum: true}, nil
	case "rootname":
		return keyRef{scope: "trace", key: "rootName", intrinsic: true}, nil
	case "rootservice", "rootservicename":
		return keyRef{scope: "trace", key: "rootService", intrinsic: true}, nil
	case "traceduration":
		return keyRef{scope: "trace", key: "duration", intrinsic: true}, nil
	case "traceid":
		return keyRef{scope: "trace", key: "id", intrinsic: true}, nil
	case "service.name":
		return keyRef{scope: "resource", key: "service.name"}, nil
	}
	return keyRef{key: key}, nil
}

func canonicalLinkKey(key string) string {
	switch strings.ToLower(key) {
	case "traceid":
		return "traceID"
	case "spanid":
		return "spanID"
	case "tracestate":
		return "traceState"
	default:
		return key
	}
}

func unquoteKey(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", nil
	}
	if unquoted, err := unquote(raw); err == nil {
		return unquoted, nil
	}
	if strings.ContainsAny(raw, "\"' \t") {
		return "", fmt.Errorf("tempo: invalid attribute name %q", raw)
	}
	return raw, nil
}

func insideQuotes(s string, pos int) bool {
	inQuote := byte(0)
	for i := 0; i < pos && i < len(s); i++ {
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
		}
	}
	return inQuote != 0
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

func hasPipeline(s string) bool {
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
		if s[i] == '|' && (i == 0 || s[i-1] != '|') && (i+1 >= len(s) || s[i+1] != '|') {
			return true
		}
	}
	return false
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
