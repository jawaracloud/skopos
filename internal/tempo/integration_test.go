//go:build integration

package tempo

import (
	"encoding/binary"
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

	tracev1 "go.opentelemetry.io/proto/otlp/trace/v1"
	"google.golang.org/protobuf/proto"

	"github.com/mrofisr/skopos/internal/chclient"
	"github.com/mrofisr/skopos/internal/plan"
)

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func TestIntegrationTempoAPI(t *testing.T) {
	baseURL := os.Getenv("CLICKHOUSE_URL")
	if baseURL == "" {
		t.Fatal("CLICKHOUSE_URL is required for integration tests")
	}
	suffix := fmt.Sprintf("%d", time.Now().UnixNano())
	database := "skopos_tempo_" + suffix
	admin := &http.Client{Timeout: 15 * time.Second}
	exec := func(query string) {
		t.Helper()
		req, err := http.NewRequest(http.MethodPost, baseURL, strings.NewReader(query))
		if err != nil {
			t.Fatalf("build request: %v", err)
		}
		resp, err := admin.Do(req)
		if err != nil {
			t.Fatalf("request: %v", err)
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("query failed: status %d: %s\nquery: %s", resp.StatusCode, body, query)
		}
	}
	exec("CREATE DATABASE `" + database + "`")
	t.Cleanup(func() { exec("DROP DATABASE IF EXISTS `" + database + "` SYNC") })
	exec("CREATE TABLE `" + database + "`.`otel_traces` (" +
		"Timestamp DateTime64(9), TraceId String, SpanId String, ParentSpanId String, TraceState String, " +
		"SpanName LowCardinality(String), SpanKind LowCardinality(String), ServiceName LowCardinality(String), " +
		"ResourceAttributes Map(String, String), ScopeName String, ScopeVersion String, SpanAttributes Map(String, String), " +
		"Duration UInt64, StatusCode LowCardinality(String), StatusMessage String, " +
		"Events Nested (Timestamp DateTime64(9), Name LowCardinality(String), Attributes Map(String, String)), " +
		"Links Nested (TraceId String, SpanId String, TraceState String, Attributes Map(String, String))" +
		") ENGINE = MergeTree ORDER BY (ServiceName, SpanName, Timestamp)")

	traceID := "4bf92f3577b34da6a3ce929d0e0e4736"
	rootID := "00f067aa0ba902b7"
	childID := "10f067aa0ba902b8"
	rows := []string{
		`{"Timestamp":"2026-11-01 00:00:00.000000000","TraceId":"` + traceID + `","SpanId":"` + rootID + `","ParentSpanId":"","TraceState":"","SpanName":"GET /checkout","SpanKind":"Server","ServiceName":"checkout","ResourceAttributes":{"service.name":"checkout","deployment.environment.name":"prod"},"ScopeName":"test","ScopeVersion":"1.0","SpanAttributes":{"http.method":"GET","http.status_code":"500"},"Duration":2000000000,"StatusCode":"Error","StatusMessage":"boom","Events.Timestamp":[],"Events.Name":[],"Events.Attributes":[],"Links.TraceId":[],"Links.SpanId":[],"Links.TraceState":[],"Links.Attributes":[]}`,
		`{"Timestamp":"2026-11-01 00:00:00.500000000","TraceId":"` + traceID + `","SpanId":"` + childID + `","ParentSpanId":"` + rootID + `","TraceState":"","SpanName":"SELECT orders","SpanKind":"Client","ServiceName":"checkout","ResourceAttributes":{"service.name":"checkout","deployment.environment.name":"prod"},"ScopeName":"test","ScopeVersion":"1.0","SpanAttributes":{"db.system":"postgresql"},"Duration":500000000,"StatusCode":"Ok","StatusMessage":"","Events.Timestamp":[],"Events.Name":[],"Events.Attributes":[],"Links.TraceId":[],"Links.SpanId":[],"Links.TraceState":[],"Links.Attributes":[]}`,
	}
	exec("INSERT INTO `" + database + "`.`otel_traces` FORMAT JSONEachRow\n" + strings.Join(rows, "\n"))

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
	api := NewAPI(fetcher, "test-version")
	api.Now = func() time.Time { return time.Date(2026, 11, 1, 0, 10, 0, 0, time.UTC) }
	mux := http.NewServeMux()
	api.Register(mux)
	server := httptest.NewServer(mux)
	defer server.Close()

	get := func(path string) *http.Response {
		t.Helper()
		resp, err := http.Get(server.URL + path)
		if err != nil {
			t.Fatalf("GET %s: %v", path, err)
		}
		return resp
	}

	resp := get("/api/search?q=" + url.QueryEscape(`{ service.name = "checkout" && status = error }`) + "&start=1793488800&end=1793491200")
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("search = %d %s", resp.StatusCode, body)
	}
	var search map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&search)
	resp.Body.Close()
	traces := search["traces"].([]any)
	if len(traces) != 1 || traces[0].(map[string]any)["traceID"] != traceID {
		t.Fatalf("search traces = %#v", traces)
	}

	resp = get("/api/traces/" + traceID)
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("trace = %d %s", resp.StatusCode, body)
	}
	var otlp map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&otlp)
	resp.Body.Close()
	resourceSpans := otlp["resourceSpans"].([]any)
	scopeSpans := resourceSpans[0].(map[string]any)["scopeSpans"].([]any)
	spans := scopeSpans[0].(map[string]any)["spans"].([]any)
	if len(spans) != 2 {
		t.Fatalf("otlp spans = %#v", spans)
	}

	req, _ := http.NewRequest(http.MethodGet, server.URL+"/api/traces/"+traceID, nil)
	req.Header.Set("Accept", "application/protobuf")
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("protobuf request: %v", err)
	}
	data, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	var trace tracev1.TracesData
	if err := proto.Unmarshal(data, &trace); err != nil {
		t.Fatalf("unmarshal protobuf: %v", err)
	}
	if len(trace.ResourceSpans) == 0 || len(trace.ResourceSpans[0].ScopeSpans[0].Spans) != 2 {
		t.Fatalf("protobuf trace = %#v", &trace)
	}

	req, _ = http.NewRequest(http.MethodGet, server.URL+"/api/v2/traces/"+traceID, nil)
	req.Header.Set("Accept", "application/protobuf")
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("v2 protobuf request: %v", err)
	}
	envelope, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if len(envelope) < 2 || envelope[0] != 0x0a {
		t.Fatalf("v2 envelope = %x", envelope[:minInt(8, len(envelope))])
	}
	length, n := binary.Uvarint(envelope[1:])
	if n <= 0 || int(length) > len(envelope)-1-n {
		t.Fatalf("v2 envelope length invalid")
	}
	var v2 tracev1.TracesData
	if err := proto.Unmarshal(envelope[1+n:1+n+int(length)], &v2); err != nil {
		t.Fatalf("unmarshal v2 trace: %v", err)
	}
	if len(v2.ResourceSpans) == 0 {
		t.Fatalf("v2 trace = %#v", &v2)
	}

	resp = get("/api/search/tag/service.name/values?start=1793488800&end=1793491200")
	var values map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&values)
	resp.Body.Close()
	tagValues := values["tagValues"].([]any)
	if len(tagValues) != 1 || tagValues[0] != "checkout" {
		t.Fatalf("tag values = %#v", tagValues)
	}
}
