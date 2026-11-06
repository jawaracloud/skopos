//go:build integration

package loki

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jawaracloud/skopos/internal/chclient"
	"github.com/jawaracloud/skopos/internal/plan"
)

// noPushdownFetcher intentionally exposes only LogFetcher so the API takes the
// old unfiltered time-range path. It is the reference for every comparison.
type noPushdownFetcher struct {
	fetcher *ClickHouseFetcher
}

func (f noPushdownFetcher) FetchRange(ctx context.Context, start, end time.Time) ([]LogEntry, error) {
	return f.fetcher.FetchRange(ctx, start, end)
}

func pushdownLogRow(ts time.Time, body, service, severity, traceID, spanID, event string, logAttrs, resourceAttrs map[string]string) string {
	row := map[string]any{
		"Timestamp":          ts.Format("2006-01-02 15:04:05.000000000"),
		"Body":               body,
		"ServiceName":        service,
		"SeverityText":       severity,
		"SeverityNumber":     9,
		"TraceId":            traceID,
		"SpanId":             spanID,
		"EventName":          event,
		"LogAttributes":      logAttrs,
		"ResourceAttributes": resourceAttrs,
	}
	if severity == "ERROR" {
		row["SeverityNumber"] = 17
	}
	if severity == "WARN" {
		row["SeverityNumber"] = 13
	}
	data, _ := json.Marshal(row)
	return string(data)
}

func pushdownCorpus(base time.Time) []string {
	return []string{
		pushdownLogRow(base, "request timeout after 30s", "checkout", "ERROR", "4bf92f3577b34da6a3ce929d0e0e4736", "00f067aa0ba902b7", "", map[string]string{"request_id": "abc"}, map[string]string{"k8s.pod.name": "checkout-abc"}),
		pushdownLogRow(base.Add(time.Second), "request ok", "checkout", "INFO", "4bf92f3577b34da6a3ce929d0e0e4736", "00f067aa0ba902b8", "", nil, map[string]string{"k8s.pod.name": "checkout-abc"}),
		pushdownLogRow(base.Add(2*time.Second), "database connection reset", "checkout", "ERROR", "", "", "", nil, nil),
		pushdownLogRow(base.Add(3*time.Second), "healthcheck ok", "cart", "WARN", "", "", "", nil, nil),
		pushdownLogRow(base.Add(4*time.Second), "timeout deadline exceeded", "cart", "ERROR", "", "", "", nil, nil),
		pushdownLogRow(base.Add(5*time.Second), `{"level":"error","status":500,"msg":"json failure"}`, "structured", "ERROR", "", "", "", nil, nil),
		pushdownLogRow(base.Add(6*time.Second), `{"level":"info","status":200,"msg":"json ok"}`, "structured", "INFO", "", "", "", nil, nil),
		pushdownLogRow(base.Add(7*time.Second), `level=warn status=429 msg="slow down"`, "structured", "WARN", "", "", "", nil, nil),
		// Column empty: the label is filled from ResourceAttributes. A strict
		// column-only predicate would drop this row.
		pushdownLogRow(base.Add(8*time.Second), "ghost timeout", "", "ERROR", "", "", "", nil, map[string]string{"service_name": "ghost"}),
		// Column wins over a normalized resource attribute with a different
		// value, so {service_name="ghost"} must not match this row.
		pushdownLogRow(base.Add(9*time.Second), "real column wins", "real", "INFO", "", "", "", nil, map[string]string{"service.name": "ghost"}),
		// Column empty, LogAttributes fill the label.
		pushdownLogRow(base.Add(10*time.Second), "log attribute fallback", "", "INFO", "", "", "", map[string]string{"service_name": "logattr"}, nil),
		pushdownLogRow(base.Add(11*time.Second), "REQUEST TIMEOUT uppercase", "checkout", "INFO", "", "", "", nil, nil),
		pushdownLogRow(base.Add(12*time.Second), "error code: 42", "checkout", "ERROR", "", "", "", nil, nil),
		pushdownLogRow(base.Add(13*time.Second), "deployment started", "deployer", "INFO", "", "", "deploy", nil, nil),
		pushdownLogRow(base.Add(14*time.Second), "traced line", "tracer", "INFO", "abc123", "span456", "", nil, nil),
		pushdownLogRow(base.Add(15*time.Second), `{"level":"error","status":503,"msg":"checkout json failure"}`, "checkout", "ERROR", "", "", "", nil, nil),
		pushdownLogRow(base.Add(16*time.Second), "deadline exceeded for checkout", "checkout", "ERROR", "", "", "", nil, nil),
		pushdownLogRow(base.Add(17*time.Second), "debug trace", "checkout", "INFO", "", "", "", nil, nil),
		pushdownLogRow(base.Add(18*time.Second), "line one\nline two\tend", "checkout", "INFO", "", "", "", nil, nil),
		pushdownLogRow(base.Add(19*time.Second), "trailing newline\n", "checkout", "INFO", "", "", "", nil, nil),
	}
}

func chPost(t *testing.T, baseURL, sql string, params []plan.Param) string {
	t.Helper()
	values := url.Values{}
	values.Set("query", sql)
	for _, param := range params {
		encoded, err := chclient.EncodeParam(param)
		if err != nil {
			t.Fatalf("encode param %q: %v", param.Name, err)
		}
		values.Set("param_"+param.Name, encoded)
	}
	req, err := http.NewRequest(http.MethodPost, baseURL+"?"+values.Encode(), nil)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	resp, err := (&http.Client{Timeout: 30 * time.Second}).Do(req)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("query failed: status %d: %s\nquery: %s", resp.StatusCode, body, sql)
	}
	return string(body)
}

func chPostBody(t *testing.T, baseURL, sql string) string {
	t.Helper()
	resp, err := (&http.Client{Timeout: 30 * time.Second}).Post(baseURL, "text/plain", strings.NewReader(sql))
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("query failed: status %d: %s\nquery: %s", resp.StatusCode, body, sql)
	}
	return string(body)
}

func TestIntegrationPushdownEquivalence(t *testing.T) {
	baseURL := os.Getenv("CLICKHOUSE_URL")
	if baseURL == "" {
		t.Fatal("CLICKHOUSE_URL is required for integration tests")
	}
	suffix := fmt.Sprintf("%d", time.Now().UnixNano())
	database := "skopos_pushdown_" + suffix
	chPostBody(t, baseURL, "CREATE DATABASE `"+database+"`")
	t.Cleanup(func() {
		chPostBody(t, baseURL, "DROP DATABASE IF EXISTS `"+database+"` SYNC")
	})
	chPostBody(t, baseURL, "CREATE TABLE `"+database+"`.`otel_logs` ("+
		"Timestamp DateTime64(9), Body String, ServiceName LowCardinality(String), "+
		"SeverityText LowCardinality(String), SeverityNumber UInt8, TraceId String, SpanId String, "+
		"EventName String, LogAttributes Map(String, String), ResourceAttributes Map(String, String), "+
		"INDEX idx_lower_body lower(Body) TYPE text(tokenizer = 'splitByNonAlpha'), "+
		"INDEX idx_body Body TYPE text(tokenizer = 'splitByNonAlpha'), "+
		"INDEX idx_event_name EventName TYPE bloom_filter(0.01) GRANULARITY 1"+
		") ENGINE = ReplacingMergeTree PARTITION BY toDate(Timestamp) ORDER BY (toStartOfFiveMinutes(Timestamp), ServiceName, Timestamp, cityHash64(Body, LogAttributes, ResourceAttributes))")

	base := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	rows := pushdownCorpus(base)
	// Several inserts produce several parts so EXPLAIN can show index effects.
	partSize := 4
	for start := 0; start < len(rows); start += partSize {
		end := start + partSize
		if end > len(rows) {
			end = len(rows)
		}
		chPostBody(t, baseURL, "INSERT INTO `"+database+"`.`otel_logs` FORMAT JSONEachRow\n"+strings.Join(rows[start:end], "\n"))
	}
	// JSONEachRow cannot carry invalid UTF-8. Insert it with SQL escapes so the
	// JSON response (and therefore the Go evaluator) sees U+FFFD while the
	// ClickHouse column and predicate see the raw bytes.
	chPostBody(t, baseURL, "INSERT INTO `"+database+"`.`otel_logs` "+
		"(Timestamp, Body, ServiceName, SeverityText, SeverityNumber, TraceId, SpanId, EventName, LogAttributes, ResourceAttributes) VALUES "+
		"('2026-11-01 00:00:20.000000000', 'invalid \\xFF byte', 'checkout', 'INFO', 9, '', '', '', map(), map()), "+
		"('2026-11-01 00:00:21.000000000', 'invalid service column', unhex('FF'), 'INFO', 9, '', '', '', map(), map())")

	client, err := chclient.New(chclient.Config{
		BaseURL:          baseURL,
		Database:         database,
		Timeout:          10 * time.Second,
		MaxExecutionTime: 10 * time.Second,
		MaxMemoryUsage:   256 << 20,
		MaxRowsToRead:    1_000_000,
		MaxResultRows:    100_000,
	})
	if err != nil {
		t.Fatalf("chclient.New: %v", err)
	}
	names := plan.DefaultTableNames()
	schema := plan.DefaultSchema(database, names)
	fetcher, err := NewClickHouseFetcher(client, schema, names, 100_000)
	if err != nil {
		t.Fatalf("NewClickHouseFetcher: %v", err)
	}

	now := base.Add(10 * time.Minute)
	pushAPI := NewAPI(fetcher, "pushdown")
	pushAPI.Now = func() time.Time { return now }
	referenceAPI := NewAPI(noPushdownFetcher{fetcher: fetcher}, "reference")
	referenceAPI.Now = func() time.Time { return now }

	pushMux := http.NewServeMux()
	pushAPI.Register(pushMux)
	pushServer := httptest.NewServer(pushMux)
	defer pushServer.Close()
	referenceMux := http.NewServeMux()
	referenceAPI.Register(referenceMux)
	referenceServer := httptest.NewServer(referenceMux)
	defer referenceServer.Close()

	window := "start=" + fmt.Sprintf("%d", base.UnixNano()) + "&end=" + fmt.Sprintf("%d", base.Add(time.Minute).UnixNano())
	metricWindow := "start=" + fmt.Sprintf("%d", base.Unix()) + "&end=" + fmt.Sprintf("%d", base.Add(5*time.Minute).Unix()) + "&step=60"

	get := func(server *httptest.Server, path string) (int, string) {
		t.Helper()
		resp, err := http.Get(server.URL + path)
		if err != nil {
			t.Fatalf("GET %s: %v", path, err)
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(body)
	}

	compare := func(t *testing.T, path string) {
		t.Helper()
		pushStatus, pushBody := get(pushServer, path)
		referenceStatus, referenceBody := get(referenceServer, path)
		if pushStatus != referenceStatus || pushBody != referenceBody {
			t.Fatalf("pushdown differs from unfiltered path\npath: %s\npushdown %d: %s\nreference %d: %s",
				path, pushStatus, pushBody, referenceStatus, referenceBody)
		}
		if pushStatus != http.StatusOK {
			t.Fatalf("query failed: %d %s", pushStatus, pushBody)
		}
	}

	logQueries := []string{
		`{service_name="checkout"}`,
		`{service_name!="checkout"}`,
		`{service_name=~"check.*"}`,
		`{service_name!~"check.*"}`,
		`{service_name=""}`,
		`{service_name!=""}`,
		`{service_name=~".*"}`,
		`{service_name!~".*"}`,
		`{severity_text="ERROR"}`,
		`{severity_text!="ERROR"}`,
		`{trace_id="abc123"}`,
		`{trace_id!="abc123"}`,
		`{span_id="span456"}`,
		`{event_name="deploy"}`,
		`{service_name="ghost"}`,
		`{service_name="logattr"}`,
		`{service_name="real"}`,
		`{service_name="real", severity_text="INFO"}`,
		`{missing=""}`,
		`{missing!="x"}`,
		`{service_name="checkout"} |= "timeout"`,
		`{service_name="checkout"} != "timeout"`,
		`{service_name="checkout"} |~ "timeout|deadline"`,
		`{service_name="checkout"} !~ "timeout"`,
		`{service_name="checkout"} |= "REQUEST"`,
		`{service_name="checkout"} |~ "^request timeout.*30s$"`,
		`{service_name="checkout"} |~ "code: [0-9]+"`,
		`{service_name="checkout"} |~ "(?i)REQUEST"`,
		`{service_name="checkout"} |~ "\\brequest\\b"`,
		`{service_name="checkout"} |~ "[[:alpha:]]+ timeout"`,
		`{service_name="checkout"} |~ "\\p{L}+ timeout"`,
		`{service_name="checkout"} |~ "\\Qrequest timeout\\E"`,
		`{service_name="checkout"} |~ "(?m)^request"`,
		`{service_name="checkout"} |~ "timeout\\z"`,
		`{service_name="checkout"} !~ "(?i)timeout"`,
		`{service_name="checkout"} |~ "连接"`,
		`{service_name=~"(?i)CHECK.*"}`,
		`{service_name=~"check|cart"}`,
		`{severity_text=~"ERR.*"} |= "timeout" != "deadline" |~ "request|database" !~ "reset"`,
		`{service_name="structured"} | json | level="error" | status >= 500`,
		`{service_name="structured"} | json |= "json" | level="info"`,
		`{service_name="structured"} | logfmt | status >= 400`,
		`{service_name="checkout"} | json | level="error"`,
		`{service_name="checkout"} |= ""`,
		`{service_name="checkout"} != ""`,
		`{service_name="checkout"} |~ ""`,
		`{service_name="checkout"} !~ ""`,
		`{service_name="checkout"} |= "one\nline"`,
		`{service_name="checkout"} |= "one\\nline"`,
		`{service_name="checkout"} |~ "two\tend"`,
		`{service_name="checkout"} |~ "two\\tend"`,
		`{service_name="checkout"} |~ "trailing newline$"`,
		`{service_name="checkout"} |~ "trailing newline\\z"`,
		`{service_name="checkout"} |~ "(?m)newline$"`,
		`{service_name="checkout"} |~ "(?m)^line two"`,
		// The API decodes invalid UTF-8 as U+FFFD; raw ClickHouse predicates
		// must not drop those rows before Go sees them.
		`{service_name="\ufffd"}`,
		`{service_name=~"\ufffd"}`,
		`{service_name!~"\ufffd"}`,
		`{service_name="checkout"} |~ "."`,
		`{service_name="checkout"} |~ "invalid . byte"`,
		`{service_name="checkout"} |~ "invalid \\p{Any} byte"`,
		`{service_name="checkout"} |~ "[^x]"`,
		`{service_name="checkout"} !~ "nomatch"`,
		`{service_name="checkout"} |= "\ufffd"`,
		`{service_name="checkout"} != "\ufffd"`,
		`{service_name="checkout"} |= "invalid \xFF byte"`,
		`{service_name="checkout"} |= "invalid "`,
	}
	for _, query := range logQueries {
		t.Run("log/"+query, func(t *testing.T) {
			compare(t, "/loki/api/v1/query_range?query="+url.QueryEscape(query)+"&"+window+"&limit=500")
		})
	}

	metricQueries := []string{
		`count_over_time({service_name="checkout"}[5m])`,
		`sum by (severity_text) (count_over_time({service_name=~"check.*"} |= "timeout" [10m]))`,
		`rate({service_name="checkout", severity_text="ERROR"}[5m])`,
		`sum(count_over_time({service_name!="checkout"} |~ "timeout|deadline" [5m]))`,
	}
	for _, query := range metricQueries {
		t.Run("metric/"+query, func(t *testing.T) {
			compare(t, "/loki/api/v1/query_range?query="+url.QueryEscape(query)+"&"+metricWindow)
		})
	}

	metadataPaths := []string{
		"/loki/api/v1/labels?" + window,
		"/loki/api/v1/labels?query=" + url.QueryEscape(`{service_name="checkout"}`) + "&" + window,
		"/loki/api/v1/labels?query=" + url.QueryEscape(`{service_name="structured"} | json`) + "&" + window,
		"/loki/api/v1/label/service_name/values?" + window,
		"/loki/api/v1/label/service_name/values?match[]=" + url.QueryEscape(`{severity_text="ERROR"}`) + "&" + window,
		"/loki/api/v1/label/severity_text/values?match[]=" + url.QueryEscape(`{service_name=~"check.*"}`) + "&" + window,
		"/loki/api/v1/series?match[]=" + url.QueryEscape(`{service_name="checkout"}`) + "&" + window,
		"/loki/api/v1/series?match[]=" + url.QueryEscape(`{service_name="checkout"}`) + "&match[]=" + url.QueryEscape(`{severity_text="ERROR"}`) + "&" + window,
		"/loki/api/v1/series?match[]=" + url.QueryEscape(`{service_name="checkout"}`) + "&match[]=" + url.QueryEscape(`{missing="x"}`) + "&" + window,
		"/loki/api/v1/series?query=" + url.QueryEscape(`{service_name="structured"} | json`) + "&" + window,
		"/loki/api/v1/series?match[]=" + url.QueryEscape(`{service_name="ghost"}`) + "&" + window,
		"/loki/api/v1/series?match[]=" + url.QueryEscape(`{service_name="logattr"}`) + "&" + window,
	}
	for _, path := range metadataPaths {
		t.Run("metadata/"+path, func(t *testing.T) {
			compare(t, path)
		})
	}

	t.Run("invalid UTF-8 corpus is reachable", func(t *testing.T) {
		_, body := get(referenceServer, "/loki/api/v1/query_range?query="+url.QueryEscape(`{service_name="\ufffd"}`)+"&"+window)
		if !strings.Contains(body, "invalid service column") {
			t.Fatalf("reference path does not expose the invalid service label: %s", body)
		}
		_, body = get(referenceServer, "/loki/api/v1/query_range?query="+url.QueryEscape(`{service_name="checkout"} |= "\ufffd"`)+"&"+window)
		if !strings.Contains(body, "invalid \ufffd byte") {
			t.Fatalf("reference path does not expose the invalid body as U+FFFD: %s", body)
		}
	})

	t.Run("explain", func(t *testing.T) {
		explain := func(queryText string) string {
			where := buildPushdownPredicate(schema, names.Logs, []Query{mustParse(t, queryText)})
			emitted, err := fetcher.Emitter.Emit(fetcher.logPlan(where, base, base.Add(time.Minute)))
			if err != nil {
				t.Fatalf("Emit(%q): %v", queryText, err)
			}
			if strings.Contains(emitted.SQL, "timeout'") || strings.Contains(emitted.SQL, "checkout'") {
				t.Fatalf("SQL contains a literal user value:\n%s", emitted.SQL)
			}
			return chPost(t, baseURL, "EXPLAIN indexes=1 "+emitted.SQL, emitted.Params)
		}

		textIndex := explain(`{service_name="checkout"} |~ "request timeout after 30s"`)
		if !strings.Contains(textIndex, "idx_body") {
			t.Errorf("EXPLAIN does not use idx_body for a tokenizable match:\n%s", textIndex)
		}
		t.Logf("EXPLAIN match(Body):\n%s", textIndex)

		substring := explain(`{service_name="checkout"} |= "timeout"`)
		if strings.Contains(substring, "idx_body") {
			t.Errorf("EXPLAIN unexpectedly claims idx_body supports position():\n%s", substring)
		}
		t.Logf("EXPLAIN position(Body):\n%s", substring)

		selector := explain(`{service_name="checkout"}`)
		if !strings.Contains(selector, "PrimaryKey") || !strings.Contains(selector, "ServiceName") {
			t.Errorf("EXPLAIN does not use the primary key for ServiceName:\n%s", selector)
		}
		t.Logf("EXPLAIN selector:\n%s", selector)
	})
}
