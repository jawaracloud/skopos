package loki

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jawaracloud/skopos/internal/chclient"
	"github.com/jawaracloud/skopos/internal/plan"
	"github.com/jawaracloud/skopos/internal/sqlgen"
)

func emitQueries(t *testing.T, queries []Query, mutate func(*plan.Schema)) sqlgen.Result {
	t.Helper()
	names := plan.DefaultTableNames()
	schema := plan.DefaultSchema("otel", names)
	if mutate != nil {
		mutate(schema)
	}
	fetcher := &ClickHouseFetcher{Schema: schema, Names: names, MaxRows: 1000}
	emitter, err := sqlgen.New(schema, sqlgen.Limits{MaxLimit: 1000, MaxRange: 30 * 24 * time.Hour})
	if err != nil {
		t.Fatalf("sqlgen.New: %v", err)
	}
	start := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	end := start.Add(time.Hour)
	where := buildPushdownPredicate(schema, names.Logs, queries)
	got, err := emitter.Emit(fetcher.logPlan(where, start, end))
	if err != nil {
		t.Fatalf("Emit: %v", err)
	}
	return got
}

func mustParse(t *testing.T, text string) Query {
	t.Helper()
	query, err := Parse(text)
	if err != nil {
		t.Fatalf("Parse(%q): %v", text, err)
	}
	return query
}

func paramValues(params []plan.Param) map[string]string {
	out := make(map[string]string)
	for _, p := range params {
		if value, ok := p.Value.(string); ok {
			out[p.Name] = value
		}
	}
	return out
}

func hasParamValue(params []plan.Param, want string) bool {
	for _, p := range params {
		if value, ok := p.Value.(string); ok && value == want {
			return true
		}
	}
	return false
}

// whereClause returns the predicate portion of an emitted log query so tests do
// not confuse selected columns with pushed predicates.
func whereClause(sql string) string {
	index := strings.Index(sql, "WHERE ")
	if index < 0 {
		return ""
	}
	rest := sql[index+len("WHERE "):]
	if end := strings.Index(rest, "\nLIMIT "); end >= 0 {
		rest = rest[:end]
	}
	return rest
}

func TestBuildPushdownPredicate(t *testing.T) {
	tests := []struct {
		name      string
		query     string
		want      []string
		notWant   []string
		wantValue string
		noWhere   bool
	}{
		{
			name:      "selector equality",
			query:     `{service_name="checkout"}`,
			want:      []string{"`ServiceName` = {", " OR ", "`ServiceName` = {"},
			wantValue: "checkout",
		},
		{
			name:      "selector inequality",
			query:     `{service_name!="checkout"}`,
			want:      []string{"`ServiceName` != {", " OR "},
			wantValue: "checkout",
		},
		{
			name:      "selector regex",
			query:     `{service_name=~"check.*"}`,
			want:      []string{"match(`ServiceName`, {", " OR ", "isValidUTF8(`ServiceName`)"},
			wantValue: "^(?:check.*)$",
		},
		{
			name:      "selector negative regex",
			query:     `{service_name!~"check.*"}`,
			want:      []string{"NOT match(`ServiceName`, {", " OR ", "isValidUTF8(`ServiceName`)"},
			wantValue: "^(?:check.*)$",
		},
		{
			name:      "severity column",
			query:     `{severity_text="ERROR"}`,
			want:      []string{"`SeverityText` = {"},
			wantValue: "ERROR",
		},
		{
			name:      "trace column",
			query:     `{trace_id="4bf92f"}`,
			want:      []string{"`TraceId` = {"},
			wantValue: "4bf92f",
		},
		{
			name:      "span column",
			query:     `{span_id="00f067"}`,
			want:      []string{"`SpanId` = {"},
			wantValue: "00f067",
		},
		{
			name:      "event column",
			query:     `{event_name="deploy"}`,
			want:      []string{"`EventName` = {"},
			wantValue: "deploy",
		},
		{
			name:      "multiple matchers are ANDed",
			query:     `{service_name="checkout", severity_text="ERROR"}`,
			want:      []string{"`ServiceName` = {", " AND ", "`SeverityText` = {"},
			wantValue: "checkout",
		},
		{
			name:      "known and unknown matchers",
			query:     `{service_name="checkout", missing="x"}`,
			want:      []string{"`ServiceName` = {"},
			notWant:   []string{"missing"},
			wantValue: "checkout",
		},
		{
			name:    "unknown selector label is not pushed",
			query:   `{pod_name="checkout-abc"}`,
			notWant: []string{"`ServiceName`", "match(", "position("},
			noWhere: true,
		},
		{
			name:    "attribute selector is not pushed",
			query:   `{k8s_pod_name="checkout-abc"}`,
			notWant: []string{"ResourceAttributes", "LogAttributes"},
			noWhere: true,
		},
		{
			name:    "parser stages and label filters are not pushed",
			query:   `{service_name="checkout"} | json | level="error" | status >= 500`,
			want:    []string{"`ServiceName` = {"},
			notWant: []string{"`level`", "`status`", "match("},
		},
		{
			name:      "contains line filter",
			query:     `{service_name="checkout"} |= "timeout"`,
			want:      []string{"position(`Body`, {", ") > 0"},
			wantValue: "timeout",
		},
		{
			name:      "not contains line filter",
			query:     `{service_name="checkout"} != "debug"`,
			want:      []string{"position(`Body`, {", ") = 0"},
			wantValue: "debug",
		},
		{
			name:      "regex line filter",
			query:     `{service_name="checkout"} |~ "timeout|deadline"`,
			want:      []string{"match(`Body`, {", "NOT isValidUTF8(`Body`)"},
			wantValue: "timeout|deadline",
		},
		{
			name:      "negative regex line filter",
			query:     `{service_name="checkout"} !~ "healthcheck"`,
			want:      []string{"NOT match(`Body`, {", "NOT isValidUTF8(`Body`)"},
			wantValue: "healthcheck",
		},
		{
			name:  "combined line filters",
			query: `{service_name="checkout"} |= "timeout" != "debug" |~ "request|deadline" !~ "healthcheck"`,
			want: []string{
				"position(`Body`, {", ") > 0", ") = 0", "match(`Body`, {", "NOT match(`Body`, {",
			},
			wantValue: "timeout",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := emitQueries(t, []Query{mustParse(t, tc.query)}, nil)
			where := whereClause(got.SQL)
			for _, want := range tc.want {
				if !strings.Contains(where, want) {
					t.Fatalf("predicate does not contain %q:\n%s", want, got.SQL)
				}
			}
			for _, notWant := range tc.notWant {
				if strings.Contains(where, notWant) {
					t.Fatalf("predicate unexpectedly contains %q:\n%s", notWant, got.SQL)
				}
			}
			if tc.noWhere {
				// Only the time range parameters remain; no user selector.
				if len(got.Params) != 3 {
					t.Fatalf("params = %#v, want only time and limit", got.Params)
				}
				return
			}
			if tc.wantValue != "" && !hasParamValue(got.Params, tc.wantValue) {
				t.Fatalf("params %#v do not contain %q", got.Params, tc.wantValue)
			}
		})
	}
}

func TestPushdownSkipsColumnMissingFromSchema(t *testing.T) {
	names := plan.DefaultTableNames()
	schema := plan.DefaultSchema("otel", names)
	delete(schema.Tables[names.Logs].Columns, "EventName")
	where := buildPushdownPredicate(schema, names.Logs, []Query{mustParse(t, `{event_name="deploy"}`)})
	if where != nil {
		t.Fatalf("predicate references a missing column: %#v", where)
	}
}

func TestPushdownMultipleSelectors(t *testing.T) {
	t.Run("known columns OR", func(t *testing.T) {
		got := emitQueries(t, []Query{
			mustParse(t, `{service_name="checkout"}`),
			mustParse(t, `{severity_text="ERROR"}`),
		}, nil)
		where := whereClause(got.SQL)
		if !strings.Contains(where, "`ServiceName` = {") || !strings.Contains(where, " OR ") ||
			!strings.Contains(where, "`SeverityText` = {") {
			t.Fatalf("predicate does not combine selectors with OR:\n%s", got.SQL)
		}
	})

	t.Run("one unconstrained selector disables pushdown", func(t *testing.T) {
		got := emitQueries(t, []Query{
			mustParse(t, `{service_name="checkout"}`),
			mustParse(t, `{pod_name="checkout-abc"}`),
		}, nil)
		if strings.Contains(whereClause(got.SQL), "`ServiceName`") {
			t.Fatalf("predicate pushes when one selector is unconstrained:\n%s", got.SQL)
		}
		if len(got.Params) != 3 {
			t.Fatalf("params = %#v, want only time and limit", got.Params)
		}
	})

	t.Run("no selectors disables pushdown", func(t *testing.T) {
		got := emitQueries(t, nil, nil)
		if len(got.Params) != 3 {
			t.Fatalf("params = %#v, want only time and limit", got.Params)
		}
	})
}

func TestPushdownParameterizesUserValues(t *testing.T) {
	queries := []Query{
		mustParse(t, `{service_name="a' OR 1=1 --"}`),
		mustParse(t, `{severity_text=~"(?i)err'or.*"}`),
		mustParse(t, `{service_name="checkout"} |= "'; DROP TABLE otel_logs; --"`),
		mustParse(t, `{service_name="checkout"} |~ "(?i)drop.*table"`),
	}
	got := emitQueries(t, queries, nil)
	userValues := []string{
		`a' OR 1=1 --`,
		`^(?:(?i)err'or.*)$`,
		`'; DROP TABLE otel_logs; --`,
		`(?i)drop.*table`,
	}
	for _, value := range userValues {
		if strings.Contains(got.SQL, value) {
			t.Fatalf("SQL interpolates user value %q:\n%s", value, got.SQL)
		}
	}
	params := paramValues(got.Params)
	for _, value := range userValues {
		found := false
		for _, param := range params {
			if param == value {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("params %#v do not preserve user value %q", got.Params, value)
		}
	}
	for _, param := range got.Params {
		if _, ok := param.Value.(string); !ok {
			continue
		}
		if !strings.Contains(got.SQL, "{"+param.Name+":"+param.Type+"}") {
			t.Fatalf("param %q is not a placeholder in SQL:\n%s", param.Name, got.SQL)
		}
	}
	if strings.Contains(got.SQL, "'") {
		t.Fatalf("SQL contains a string literal:\n%s", got.SQL)
	}
}

func TestEncodeStringParam(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{name: "plain", in: "checkout", want: "checkout"},
		{name: "backslash", in: `\brequest\b`, want: `\\brequest\\b`},
		{name: "literal backslash", in: `a\\b`, want: `a\\\\b`},
		{name: "newline", in: "a\nb", want: `a\nb`},
		{name: "tab", in: "a\tb", want: `a\tb`},
		{name: "carriage return remains raw", in: "a\rb", want: "a\rb"},
		{name: "quote remains raw", in: "a'b", want: "a'b"},
		{name: "nul remains raw", in: "a\x00b", want: "a\x00b"},
		{name: "invalid utf8 remains raw", in: "a\xffb", want: "a\xffb"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := encodeStringParam(tc.in); got != tc.want {
				t.Fatalf("encodeStringParam(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestPushdownEncodesClickHouseParameterEscapes(t *testing.T) {
	got := emitQueries(t, []Query{mustParse(t, `{service_name=~"\\bcheck.*"} |= "line\nbreak"`)}, nil)
	if !hasParamValue(got.Params, `^(?:\\bcheck.*)$`) {
		t.Fatalf("selector regex was not escaped for the ClickHouse parameter parser: %#v", got.Params)
	}
	if !hasParamValue(got.Params, `line\nbreak`) {
		t.Fatalf("line filter newline was not escaped for the ClickHouse parameter parser: %#v", got.Params)
	}
}

func TestIsRegexCompileError(t *testing.T) {
	if !isRegexCompileError(&chclient.Error{Code: "427"}) {
		t.Fatal("regex compile error was not recognized")
	}
	if !isRegexCompileError(fmt.Errorf("loki: query logs: %w", &chclient.Error{Code: "427"})) {
		t.Fatal("wrapped regex compile error was not recognized")
	}
	if isRegexCompileError(&chclient.Error{Code: "457"}) {
		t.Fatal("unrelated ClickHouse error was treated as a regex error")
	}
	if isRegexCompileError(fmt.Errorf("other")) {
		t.Fatal("non-ClickHouse error was treated as a regex error")
	}
}

func TestPushdownGuardsInvalidUTF8(t *testing.T) {
	t.Run("regex line filter", func(t *testing.T) {
		got := emitQueries(t, []Query{mustParse(t, `{service_name="checkout"} |~ "."`)}, nil)
		if !strings.Contains(whereClause(got.SQL), "NOT isValidUTF8(`Body`)") {
			t.Fatalf("regex filter is not guarded against invalid UTF-8:\n%s", got.SQL)
		}
	})
	t.Run("replacement character substring", func(t *testing.T) {
		got := emitQueries(t, []Query{mustParse(t, `{service_name="checkout"} |= "\ufffd"`)}, nil)
		if !strings.Contains(whereClause(got.SQL), "NOT isValidUTF8(`Body`)") {
			t.Fatalf("U+FFFD substring is not guarded against invalid UTF-8:\n%s", got.SQL)
		}
	})
	t.Run("replacement character selector", func(t *testing.T) {
		got := emitQueries(t, []Query{mustParse(t, `{service_name="\ufffd"}`)}, nil)
		if !strings.Contains(whereClause(got.SQL), "NOT isValidUTF8(`ServiceName`)") {
			t.Fatalf("U+FFFD selector is not guarded against invalid UTF-8:\n%s", got.SQL)
		}
	})
	t.Run("plain substring has no guard", func(t *testing.T) {
		got := emitQueries(t, []Query{mustParse(t, `{service_name="checkout"} |= "timeout"`)}, nil)
		if strings.Contains(whereClause(got.SQL), "isValidUTF8") {
			t.Fatalf("plain substring carries an unnecessary UTF-8 guard:\n%s", got.SQL)
		}
	})
}

func TestPushdownAnchorsSelectorRegexOnly(t *testing.T) {
	selector := emitQueries(t, []Query{mustParse(t, `{service_name=~"check.*"}`)}, nil)
	if !hasParamValue(selector.Params, "^(?:check.*)$") {
		t.Fatalf("selector regex is not anchored: %#v", selector.Params)
	}
	line := emitQueries(t, []Query{mustParse(t, `{service_name="checkout"} |~ "check.*"`)}, nil)
	if !hasParamValue(line.Params, "check.*") || hasParamValue(line.Params, "^(?:check.*)$") {
		t.Fatalf("line filter regex was anchored: %#v", line.Params)
	}
}
