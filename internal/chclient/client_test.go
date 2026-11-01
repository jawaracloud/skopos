package chclient

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/obi-wrapper/skopos/internal/plan"
)

func newTestClient(t *testing.T, h http.Handler, mutate func(*Config)) *Client {
	t.Helper()
	cfg := Config{
		BaseURL:          "",
		Database:         "otel",
		User:             "gateway",
		Password:         "secret",
		Timeout:          2 * time.Second,
		MaxExecutionTime: 30 * time.Second,
		MaxMemoryUsage:   1 << 30,
		MaxRowsToRead:    1_000_000,
		MaxResultRows:    10_000,
		MaxResponseBytes: 1 << 20,
	}
	if mutate != nil {
		mutate(&cfg)
	}
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	cfg.BaseURL = srv.URL
	if cfg.HTTPClient == nil {
		cfg.HTTPClient = srv.Client()
	}
	c, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return c
}

func TestQuerySendsParametersAndParsesJSON(t *testing.T) {
	start := time.Date(2026, 11, 1, 0, 0, 0, 123456789, time.UTC)
	end := start.Add(time.Hour)

	var gotQuery string
	var gotBody string
	var gotAuthUser, gotAuthPass string
	var gotAuthOK bool

	c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("method = %s, want POST", r.Method)
		}
		if r.URL.Path != "/" {
			t.Errorf("path = %s, want /", r.URL.Path)
		}
		gotQuery = r.URL.RawQuery
		body, _ := io.ReadAll(r.Body)
		gotBody = string(body)
		gotAuthUser, gotAuthPass, gotAuthOK = r.BasicAuth()
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"meta":[{"name":"MetricName","type":"String"},{"name":"Value","type":"Float64"}],"data":[{"MetricName":"http_requests_total","Value":1.5}],"rows":1}`)
	}), func(cfg *Config) {
		cfg.User = "gw"
		cfg.Password = "pw"
	})

	got, err := c.Query(context.Background(),
		"SELECT MetricName, Value FROM otel.metrics WHERE MetricName = {metric:String} AND TimeUnix >= {start:DateTime64(9)} AND TimeUnix < {end:DateTime64(9)} AND has({services:Array(String)}, ServiceName) LIMIT {limit:UInt64}",
		[]plan.Param{
			plan.String("metric", "http_requests_total"),
			plan.Time("start", start),
			plan.Time("end", end),
			plan.Strings("services", []string{"checkout", "cart"}),
			plan.Uint("limit", 100),
		})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}

	if gotAuthOK != true || gotAuthUser != "gw" || gotAuthPass != "pw" {
		t.Fatalf("basic auth = %q/%q ok=%v", gotAuthUser, gotAuthPass, gotAuthOK)
	}
	if !strings.HasPrefix(gotBody, "SELECT MetricName") {
		t.Fatalf("body does not start with SQL: %q", gotBody)
	}
	if !strings.HasSuffix(gotBody, "FORMAT JSON\n") {
		t.Fatalf("body does not end with FORMAT JSON: %q", gotBody)
	}
	for _, want := range []string{
		"database=otel",
		"max_execution_time=30",
		"max_memory_usage=1073741824",
		"max_rows_to_read=1000000",
		"max_result_rows=10000",
		"param_metric=http_requests_total",
		"param_services=",
		"param_limit=100",
	} {
		if !strings.Contains(gotQuery, want) {
			t.Errorf("query %q does not contain %q", gotQuery, want)
		}
	}
	if !strings.Contains(gotQuery, "param_start=2026-11-01+00%3A00%3A00.123456789") {
		t.Errorf("start parameter missing or not encoded: %q", gotQuery)
	}
	if len(got.Columns) != 2 || got.Columns[0].Name != "MetricName" {
		t.Fatalf("columns = %#v", got.Columns)
	}
	if got.Len() != 1 {
		t.Fatalf("rows = %d, want 1", got.Len())
	}
	name, ok := got.String(0, "MetricName")
	if !ok || name != "http_requests_total" {
		t.Fatalf("MetricName = %q ok=%v", name, ok)
	}
	value, ok := got.Float64(0, "Value")
	if !ok || value != 1.5 {
		t.Fatalf("Value = %v ok=%v", value, ok)
	}
}

func TestQueryRejectsWriteStatements(t *testing.T) {
	var called atomic.Bool
	c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called.Store(true)
		w.WriteHeader(http.StatusOK)
	}), nil)

	for _, sql := range []string{
		"INSERT INTO otel.otel_traces VALUES (1)",
		"ALTER TABLE otel.otel_logs DELETE WHERE 1",
		"DROP TABLE otel.otel_traces",
		"CREATE TABLE x (a UInt8) ENGINE = Memory",
		"SYSTEM FLUSH LOGS",
		"TRUNCATE TABLE otel.otel_logs",
		"WITH x AS (SELECT 1) INSERT INTO otel.otel_logs SELECT * FROM x",
		"WITH x AS (SELECT 1) DELETE FROM otel.otel_logs WHERE 1",
		"",
	} {
		if _, err := c.Query(context.Background(), sql, nil); err == nil {
			t.Errorf("Query(%q) succeeded, want error", sql)
		}
	}
	if called.Load() {
		t.Fatal("server was called for a rejected statement")
	}
}

func TestQueryAcceptsReadStatementsWithComments(t *testing.T) {
	c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"meta":[],"data":[]}`)
	}), nil)

	for _, sql := range []string{
		"SELECT 1",
		"  \n\tSELECT 1",
		"/* comment */ SELECT 1",
		"-- comment\nSELECT 1",
		"WITH x AS (SELECT 1) SELECT * FROM x",
		"SELECT 'insert into x' AS s",
		"SELECT `insert` FROM system.tables LIMIT 1",
		"SELECT 1 /* DROP TABLE x */",
		"SELECT 1 WHERE x = {insert:String}",
	} {
		if _, err := c.Query(context.Background(), sql, nil); err != nil {
			t.Errorf("Query(%q): %v", sql, err)
		}
	}
}

func TestQueryClickHouseError(t *testing.T) {
	c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = io.WriteString(w, `{"exception":"Code: 60. DB::Exception: Table otel.otel_traces does not exist. (UNKNOWN_TABLE)"}`)
	}), nil)

	_, err := c.Query(context.Background(), "SELECT * FROM otel.otel_traces", nil)
	if err == nil {
		t.Fatal("expected error")
	}
	var che *Error
	if !errors.As(err, &che) {
		t.Fatalf("error type = %T, want *Error", err)
	}
	if che.StatusCode != http.StatusInternalServerError || che.Code != "60" {
		t.Fatalf("error = %#v", che)
	}
	if !strings.Contains(che.Message, "UNKNOWN_TABLE") {
		t.Fatalf("message = %q", che.Message)
	}
}

func TestQueryResponseLimit(t *testing.T) {
	c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"meta":[],"data":[`+strings.Repeat(`{"x":1},`, 100)+`{}]}`)
	}), func(cfg *Config) { cfg.MaxResponseBytes = 32 })

	_, err := c.Query(context.Background(), "SELECT 1", nil)
	if err == nil || !strings.Contains(err.Error(), "response exceeds") {
		t.Fatalf("error = %v, want response limit error", err)
	}
}

func TestQueryAcceptsGeneratedReservedParameters(t *testing.T) {
	c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.Query().Get("param___start"); got != "2026-11-01 00:00:00.000000000" {
			t.Errorf("param___start = %q", got)
		}
		_, _ = io.WriteString(w, `{"meta":[],"data":[]}`)
	}), nil)
	_, err := c.Query(context.Background(), "SELECT 1 WHERE x = {__start:DateTime64(9)}", []plan.Param{
		plan.Time("__start", time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)),
	})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
}

func TestQueryRejectsDuplicateParameters(t *testing.T) {
	c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"meta":[],"data":[]}`)
	}), nil)
	_, err := c.Query(context.Background(), "SELECT 1 WHERE x = {x:String}", []plan.Param{
		plan.String("x", "a"), plan.String("x", "b"),
	})
	if err == nil || !strings.Contains(err.Error(), "duplicate") {
		t.Fatalf("error = %v, want duplicate parameter error", err)
	}
}

func TestPing(t *testing.T) {
	c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if !strings.Contains(string(body), "SELECT 1") {
			t.Errorf("ping body = %q", body)
		}
		_, _ = io.WriteString(w, `{"meta":[{"name":"1","type":"UInt8"}],"data":[{"1":1}]}`)
	}), nil)
	if err := c.Ping(context.Background()); err != nil {
		t.Fatalf("Ping: %v", err)
	}
}

func TestNewRejectsInvalidConfig(t *testing.T) {
	tests := []struct {
		name string
		cfg  Config
	}{
		{"missing URL", Config{}},
		{"bad scheme", Config{BaseURL: "ftp://example.com"}},
		{"no host", Config{BaseURL: "http:///path"}},
		{"negative timeout", Config{BaseURL: "http://example.com", Timeout: -time.Second}},
		{"negative response limit", Config{BaseURL: "http://example.com", MaxResponseBytes: -1}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := New(tc.cfg); err == nil {
				t.Fatal("expected error")
			}
		})
	}
}

func TestEncodeParam(t *testing.T) {
	start := time.Date(2026, 11, 1, 0, 0, 0, 123456789, time.UTC)
	tests := []struct {
		name string
		p    plan.Param
		want string
	}{
		{"string", plan.String("s", "hello"), "hello"},
		{"string quote", plan.String("s", `a'b\c`), `a'b\c`},
		{"array", plan.Strings("a", []string{"checkout", "cart"}), `['checkout','cart']`},
		{"array escapes", plan.Strings("a", []string{`a'b\c`, "line\nbreak\ttab"}), `['a\'b\\c','line\nbreak\ttab']`},
		{"time", plan.Time("t", start), "2026-11-01 00:00:00.123456789"},
		{"uint", plan.Uint("u", 42), "42"},
		{"int", plan.Int("i", -42), "-42"},
		{"float", plan.Float("f", 1.25), "1.25"},
		{"bool true", plan.Bool("b", true), "1"},
		{"bool false", plan.Bool("b", false), "0"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := EncodeParam(tc.p)
			if err != nil {
				t.Fatalf("EncodeParam: %v", err)
			}
			if got != tc.want {
				t.Fatalf("EncodeParam() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestEncodeParamRejectsInvalidValues(t *testing.T) {
	tests := []plan.Param{
		{Name: "x", Type: plan.TypeString, Value: 1},
		{Name: "x", Type: plan.TypeStrings, Value: "not-a-slice"},
		{Name: "x", Type: plan.TypeDateTime64, Value: "not-a-time"},
		{Name: "x", Type: plan.TypeUInt64, Value: -1},
		{Name: "x", Type: plan.TypeFloat64, Value: mathNaN()},
		{Name: "x", Type: "Unsupported", Value: "x"},
	}
	for _, p := range tests {
		if _, err := EncodeParam(p); err == nil {
			t.Errorf("EncodeParam(%#v) succeeded, want error", p)
		}
	}
}

func TestResultHelpers(t *testing.T) {
	r := &Result{
		Columns: []Column{{Name: "s", Type: "String"}, {Name: "f", Type: "Float64"}, {Name: "i", Type: "Int64"}},
		Rows: []map[string]any{
			{"s": "v", "f": json.Number("1.5"), "i": json.Number("7")},
		},
	}
	if v, ok := r.String(0, "s"); !ok || v != "v" {
		t.Fatalf("String = %q %v", v, ok)
	}
	if v, ok := r.Float64(0, "f"); !ok || v != 1.5 {
		t.Fatalf("Float64 = %v %v", v, ok)
	}
	if v, ok := r.Int64(0, "i"); !ok || v != 7 {
		t.Fatalf("Int64 = %v %v", v, ok)
	}
	if _, ok := r.String(1, "s"); ok {
		t.Fatal("out-of-range row returned ok")
	}
	if _, ok := r.String(0, "missing"); ok {
		t.Fatal("missing column returned ok")
	}
}

func mathNaN() float64 {
	var z float64
	return z / z
}
